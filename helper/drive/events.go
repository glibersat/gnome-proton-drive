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
	"github.com/ProtonMail/gopenpgp/v2/crypto"
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

	log.Printf("events: raw eventType=%d linkID=%s parentID=%s", le.EventType, linkID, parentID)

	path := p.s.linkPath(linkID)
	parentPath := p.s.linkPath(parentID)

	switch le.EventType {
	case proton.LinkEventDelete:
		p.s.meta.InvalidatePath(path)
		if path == "" {
			p.s.meta.invalidateLinkID(parentID)
		}
		p.s.blocks.InvalidateLink(linkID)

		if path != "" {
			log.Printf("events: deleted linkID=%s path=%q", linkID, path)
			p.enqueue(DriveEvent{Type: EventDeleted, LinkID: linkID, Path: path})
		} else {
			// Path unknown — signal the parent directory so Nautilus re-enumerates.
			log.Printf("events: delete: path unknown linkID=%s; changed parent %q", linkID, parentPath)
			if parentPath != "" {
				p.enqueue(DriveEvent{Type: EventChanged, LinkID: linkID, Path: parentPath})
			}
		}

	case proton.LinkEventCreate:
		// Capture old parent listing BEFORE invalidation for the diff fallback.
		oldLinks, _, hasOld := p.s.meta.GetListStale(parentPath)
		if parentPath != "" {
			p.s.meta.InvalidatePath(parentPath)
		} else {
			p.s.meta.invalidateLinkID(parentID)
		}

		// Fast path: decrypt child name directly from event payload.
		if parentPath != "" {
			if parentKR, ok := p.s.nodeKRs[parentID]; ok {
				name, err := le.Link.GetName(parentKR, p.s.addrKR)
				if err != nil {
					// Volume events may omit the encrypted Name; fetch the full link.
					if fullLink, ferr := p.s.client.GetLink(context.Background(), p.s.shareID, linkID); ferr == nil {
						name, err = fullLink.GetName(parentKR, p.s.addrKR)
					}
				}
				if err == nil && name != "" {
					childPath := strings.TrimRight(parentPath, "/") + "/" + name
					p.s.linkPaths[linkID] = childPath
					log.Printf("events: created linkID=%s path=%q", linkID, childPath)
					p.enqueue(DriveEvent{Type: EventCreated, LinkID: linkID, Path: childPath})
					return
				}
				log.Printf("events: create: name resolve failed linkID=%s: %v", linkID, err)
			} else {
				log.Printf("events: create: parent KR not cached parentID=%s", parentID)
			}
		}

		// Diff fallback: re-list the parent and emit CREATED for the new entry.
		if parentPath != "" && hasOld {
			if parentKR, ok := p.s.nodeKRs[parentID]; ok {
				if p.diffAndEmit(context.Background(), parentPath, parentID, parentKR, oldLinks) {
					return
				}
			}
		}

		// Last resort: tell Nautilus the parent directory changed.
		if parentPath != "" {
			log.Printf("events: create: fallback changed %q", parentPath)
			p.enqueue(DriveEvent{Type: EventChanged, LinkID: linkID, Path: parentPath})
		}

	default: // LinkEventUpdate, LinkEventUpdateMetadata
		// Capture old listing BEFORE invalidation so we can diff.
		oldLinks, _, hasOld := p.s.meta.GetListStale(path)

		p.s.meta.InvalidatePath(path)
		if path == "" {
			p.s.meta.invalidateLinkID(parentID)
		}
		p.s.blocks.InvalidateLink(linkID)

		// Diff-based detection: when a directory's metadata changes, something
		// in it was added, removed, or renamed. Re-list and emit specific events.
		if path != "" && hasOld {
			if dirKR, ok := p.s.nodeKRs[linkID]; ok {
				if p.diffAndEmit(context.Background(), path, linkID, dirKR, oldLinks) {
					return
				}
			}
		}

		// Fallback: emit a CHANGED for the best-known path.
		eventPath := path
		if eventPath == "" {
			eventPath = parentPath
		}
		if eventPath != "" {
			log.Printf("events: changed linkID=%s path=%q", linkID, eventPath)
			p.enqueue(DriveEvent{Type: EventChanged, LinkID: linkID, Path: eventPath})
		}
	}
}

// diffAndEmit compares oldLinks against the current API listing for dirPath.
// For each entry that appeared it emits EventCreated; for each that disappeared
// it emits EventDeleted. linkPaths is updated for newly discovered entries and
// the meta cache is refreshed with the new listing.
// Returns true when the diff ran successfully (no API error), even if there are
// no differences (prevents a spurious generic EventChanged from being queued).
func (p *EventPoller) diffAndEmit(ctx context.Context, dirPath, dirLinkID string, dirKR *crypto.KeyRing, oldLinks []proton.Link) bool {
	// Decrypt old names → name→linkID map.
	oldNames := make(map[string]string)
	for _, l := range oldLinks {
		name, err := l.GetName(dirKR, p.s.addrKR)
		if err == nil {
			oldNames[name] = l.LinkID
		}
	}

	// Fetch current children from the API (cache was already invalidated).
	newLinks, err := p.s.client.ListChildren(ctx, p.s.shareID, dirLinkID, false)
	if err != nil {
		log.Printf("events: diff: list failed for %s: %v", dirPath, err)
		return false
	}

	// Decrypt new names, update linkPaths and meta cache.
	newNames := make(map[string]string)
	for _, l := range newLinks {
		name, err := l.GetName(dirKR, p.s.addrKR)
		if err != nil {
			continue
		}
		childPath := strings.TrimRight(dirPath, "/") + "/" + name
		p.s.linkPaths[l.LinkID] = childPath
		newNames[name] = l.LinkID
	}
	p.s.meta.SetList(dirPath, newLinks, dirKR)

	// Emit CREATED for entries that appeared.
	for name, lid := range newNames {
		if _, existed := oldNames[name]; !existed {
			childPath := strings.TrimRight(dirPath, "/") + "/" + name
			log.Printf("events: diff created %s linkID=%s", childPath, lid)
			p.enqueue(DriveEvent{Type: EventCreated, LinkID: lid, Path: childPath})
		}
	}

	// Emit DELETED for entries that disappeared.
	for name, lid := range oldNames {
		if _, stillExists := newNames[name]; !stillExists {
			childPath := strings.TrimRight(dirPath, "/") + "/" + name
			log.Printf("events: diff deleted %s linkID=%s", childPath, lid)
			p.enqueue(DriveEvent{Type: EventDeleted, LinkID: lid, Path: childPath})
		}
	}

	return true
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
