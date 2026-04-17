package drive

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	proton "github.com/ProtonMail/go-proton-api"
)

const (
	defaultPollInterval = 30 * time.Second
	// eventBufferSize is the maximum number of undelivered events queued
	// before the oldest are dropped.  64 covers any realistic burst between
	// two C-side GetEvents calls.
	eventBufferSize = 64
)

// EventType classifies a remote change for consumption by the C backend.
type EventType string

const (
	EventChanged EventType = "changed" // content or metadata updated
	EventDeleted EventType = "deleted"
	EventCreated EventType = "created"
)

// DriveEvent is the simplified event delivered to the C backend via GetEvents.
// Path is the absolute POSIX path resolved from the helper's linkID→path map;
// it may be empty when the link was never visited in this session.
type DriveEvent struct {
	Type   EventType `json:"type"`
	LinkID string    `json:"link_id"`
	Path   string    `json:"path,omitempty"`
}

// EventPoller polls the Proton Drive share event stream and maintains a
// bounded queue of DriveEvents for the C backend to consume.
//
// Start is called once by newSession; Stop is called by Session.Close.
// The queue is drained by Session.DrainEvents (backing the GetEvents RPC).
//
// As a side-effect of receiving events, InvalidatePath and InvalidateLink are
// called immediately so that the next Stat / ListChildren / ReadFileContent
// hits the API rather than a stale cache entry.
type EventPoller struct {
	s          *Session
	interval   time.Duration
	anchorPath string // path to the persisted anchor file

	mu    sync.Mutex
	queue []DriveEvent // bounded ring; oldest dropped when full

	cancel context.CancelFunc
	done   chan struct{}
}

func newEventPoller(s *Session, interval time.Duration) *EventPoller {
	if interval == 0 {
		interval = defaultPollInterval
	}
	p := &EventPoller{
		s:        s,
		interval: interval,
		done:     make(chan struct{}),
	}
	if base, err := blockCacheBase(s.account); err == nil {
		p.anchorPath = filepath.Join(base, "anchor")
	}
	return p
}

// Start fetches the initial event anchor and launches the polling goroutine.
// It is a no-op if the session has no client (useful in tests).
//
// The anchor is loaded from disk when available so that events are not missed
// across helper restarts.  If no persisted anchor exists (first run or cache
// missing), the latest volume event ID is fetched from the API.
func (p *EventPoller) Start(ctx context.Context) {
	eventID := p.loadAnchor()
	if eventID == "" {
		var err error
		eventID, err = p.s.client.GetLatestVolumeEventID(ctx, p.s.volumeID)
		if err != nil {
			log.Printf("events: could not get initial event anchor: %v — polling disabled", err)
			close(p.done)
			return
		}
		log.Printf("events: no persisted anchor — fetched latest volume anchor %s", eventID)
	} else {
		log.Printf("events: resumed from persisted anchor %s", eventID)
	}
	log.Printf("events: starting poller (interval %s)", p.interval)

	pollCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	go p.loop(pollCtx, eventID)
}

// Stop signals the polling goroutine and waits for it to exit.
func (p *EventPoller) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	<-p.done
}

// Drain returns all queued events and resets the queue.
func (p *EventPoller) Drain() []DriveEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return nil
	}
	out := p.queue
	p.queue = nil
	return out
}

func (p *EventPoller) loop(ctx context.Context, eventID string) {
	defer close(p.done)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			eventID = p.poll(ctx, eventID)
		}
	}
}

// poll fetches new events since eventID, processes them, and returns the
// updated anchor for the next call.
func (p *EventPoller) poll(ctx context.Context, eventID string) string {
	ev, err := p.s.client.GetVolumeEvent(ctx, p.s.volumeID, eventID)
	if err != nil {
		if !isOfflineError(err) {
			log.Printf("events: poll error: %v", err)
		}
		return eventID // retain anchor; retry next tick
	}

	// Empty EventID means the server has forgotten this anchor (e.g. after a
	// long outage).  Re-anchor at the current head and request a full refresh.
	if ev.EventID == "" {
		log.Printf("events: anchor %s lost — re-anchoring at current head", eventID)
		newID, err := p.s.client.GetLatestVolumeEventID(ctx, p.s.volumeID)
		if err != nil {
			log.Printf("events: re-anchor failed: %v", err)
			return eventID
		}
		p.s.meta.invalidateAll()
		p.enqueue(DriveEvent{Type: EventChanged, Path: "/"})
		p.saveAnchor(newID)
		return newID
	}

	if ev.Refresh {
		log.Printf("events: server requested full refresh")
		p.s.meta.invalidateAll()
		p.enqueue(DriveEvent{Type: EventChanged, Path: "/"})
		p.saveAnchor(ev.EventID)
		return ev.EventID
	}

	for _, le := range ev.Events {
		p.handle(le)
	}

	p.saveAnchor(ev.EventID)
	return ev.EventID
}

// loadAnchor reads the persisted event anchor from disk.
// Returns an empty string when no anchor file exists or on any read error.
func (p *EventPoller) loadAnchor() string {
	if p.anchorPath == "" {
		return ""
	}
	data, err := os.ReadFile(p.anchorPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveAnchor writes the event anchor to disk so polling can resume after a
// helper restart without missing events.
func (p *EventPoller) saveAnchor(eventID string) {
	if p.anchorPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.anchorPath), 0o700); err != nil {
		log.Printf("events: save anchor mkdir: %v", err)
		return
	}
	if err := os.WriteFile(p.anchorPath, []byte(eventID), 0o600); err != nil {
		log.Printf("events: save anchor: %v", err)
	}
}

func (p *EventPoller) handle(le proton.LinkEvent) {
	linkID := le.Link.LinkID
	parentID := le.Link.ParentLinkID

	// Resolve paths from the session's reverse map before invalidating.
	path := p.s.linkPath(linkID)
	parentPath := p.s.linkPath(parentID)

	// eventPath is the most specific path the C backend can act on.
	// For creates the new file's path is unknown; use the parent directory so
	// the C backend can signal the right directory monitor.
	// For deletes/updates fall back to parentPath when the file is unknown.
	eventPath := path
	if eventPath == "" {
		eventPath = parentPath
	}

	var et EventType
	switch le.EventType {
	case proton.LinkEventDelete:
		et = EventDeleted
		p.s.meta.InvalidatePath(path)
		if path == "" {
			p.s.meta.invalidateLinkID(parentID)
		}
		p.s.blocks.InvalidateLink(linkID)
		if eventPath == path {
			break // known path: emit DELETED for the file itself
		}
		// Unknown path: fall through to CHANGED for the parent directory.
		et = EventChanged
	case proton.LinkEventCreate:
		// The new file's path is always unknown at event time; use the parent
		// directory path and emit CHANGED so the C backend re-enumerates it.
		et = EventChanged
		if parentPath != "" {
			p.s.meta.InvalidatePath(parentPath)
		} else {
			p.s.meta.invalidateLinkID(parentID)
		}
	default: // LinkEventUpdate, LinkEventUpdateMetadata
		et = EventChanged
		p.s.meta.InvalidatePath(path)
		if path == "" {
			p.s.meta.invalidateLinkID(parentID)
		}
		p.s.blocks.InvalidateLink(linkID)
	}

	log.Printf("events: %s linkID=%s path=%q", et, linkID, eventPath)
	p.enqueue(DriveEvent{Type: et, LinkID: linkID, Path: eventPath})
}

func (p *EventPoller) enqueue(ev DriveEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) >= eventBufferSize {
		// Drop the oldest event to make room.
		p.queue = p.queue[1:]
		log.Printf("events: buffer full, oldest event dropped")
	}
	p.queue = append(p.queue, ev)
}
