package drive

import (
	"context"
	"log"
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
	s        *Session
	interval time.Duration

	mu     sync.Mutex
	queue  []DriveEvent // bounded ring; oldest dropped when full

	cancel context.CancelFunc
	done   chan struct{}
}

func newEventPoller(s *Session, interval time.Duration) *EventPoller {
	if interval == 0 {
		interval = defaultPollInterval
	}
	return &EventPoller{
		s:        s,
		interval: interval,
		done:     make(chan struct{}),
	}
}

// Start fetches the initial event anchor and launches the polling goroutine.
// It is a no-op if the session has no client (useful in tests).
func (p *EventPoller) Start(ctx context.Context) {
	eventID, err := p.s.client.GetLatestShareEventID(ctx, p.s.shareID)
	if err != nil {
		log.Printf("events: could not get initial event anchor: %v — polling disabled", err)
		close(p.done)
		return
	}
	log.Printf("events: starting poller (interval %s, anchor %s)", p.interval, eventID)

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
	ev, err := p.s.client.GetShareEvent(ctx, p.s.shareID, eventID)
	if err != nil {
		if !isOfflineError(err) {
			log.Printf("events: poll error: %v", err)
		}
		return eventID // retain anchor; retry next tick
	}

	if ev.Refresh {
		log.Printf("events: server requested full refresh")
		p.s.meta.invalidateAll()
		p.enqueue(DriveEvent{Type: EventChanged, Path: "/"})
		return ev.EventID
	}

	for _, le := range ev.Events {
		p.handle(le)
	}

	return ev.EventID
}

func (p *EventPoller) handle(le proton.LinkEvent) {
	linkID := le.Link.LinkID
	parentID := le.Link.ParentLinkID

	// Resolve paths from the session's reverse map before invalidating.
	path := p.s.linkPath(linkID)
	parentPath := p.s.linkPath(parentID)

	var et EventType
	switch le.EventType {
	case proton.LinkEventDelete:
		et = EventDeleted
		p.s.meta.InvalidatePath(path)
		if path == "" {
			p.s.meta.invalidateLinkID(parentID)
		}
		p.s.blocks.InvalidateLink(linkID)
	case proton.LinkEventCreate:
		et = EventCreated
		// Parent directory listing is now stale.
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

	log.Printf("events: %s linkID=%s path=%q", et, linkID, path)
	p.enqueue(DriveEvent{Type: et, LinkID: linkID, Path: path})
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
