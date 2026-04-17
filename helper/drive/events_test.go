package drive

import (
	"testing"
	"time"

	proton "github.com/ProtonMail/go-proton-api"
)

func TestEventPollerDrainEmpty(t *testing.T) {
	p := &EventPoller{done: make(chan struct{})}
	close(p.done)
	if got := p.Drain(); got != nil {
		t.Fatalf("expected nil drain on empty poller, got %v", got)
	}
}

func TestEventPollerEnqueueAndDrain(t *testing.T) {
	p := &EventPoller{done: make(chan struct{})}
	close(p.done)

	p.enqueue(DriveEvent{Type: EventChanged, LinkID: "l1", Path: "/a"})
	p.enqueue(DriveEvent{Type: EventDeleted, LinkID: "l2", Path: "/b"})

	got := p.Drain()
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0].Type != EventChanged || got[0].LinkID != "l1" {
		t.Errorf("unexpected first event: %+v", got[0])
	}
	if got[1].Type != EventDeleted || got[1].Path != "/b" {
		t.Errorf("unexpected second event: %+v", got[1])
	}
}

func TestEventPollerDrainResetsQueue(t *testing.T) {
	p := &EventPoller{done: make(chan struct{})}
	close(p.done)

	p.enqueue(DriveEvent{Type: EventCreated, LinkID: "l1"})
	p.Drain()

	if got := p.Drain(); got != nil {
		t.Fatalf("expected nil on second drain, got %v", got)
	}
}

func TestEventPollerBufferCap(t *testing.T) {
	p := &EventPoller{done: make(chan struct{})}
	close(p.done)

	// Fill past the buffer cap — oldest should be dropped.
	for i := 0; i < eventBufferSize+5; i++ {
		p.enqueue(DriveEvent{Type: EventChanged, LinkID: "x"})
	}

	got := p.Drain()
	if len(got) != eventBufferSize {
		t.Fatalf("expected buffer capped at %d, got %d", eventBufferSize, len(got))
	}
}

func TestMetaCacheInvalidateAll(t *testing.T) {
	c := NewMetaCache(time.Hour)
	c.SetStat("/a", proton.Link{LinkID: "la"}, nil)
	c.SetList("/b", nil, nil)

	c.invalidateAll()

	if _, _, ok := c.GetStat("/a"); ok {
		t.Error("stat /a should be gone after invalidateAll")
	}
	if _, _, ok := c.GetList("/b"); ok {
		t.Error("list /b should be gone after invalidateAll")
	}
}

func TestMetaCacheInvalidateLinkID(t *testing.T) {
	c := NewMetaCache(time.Hour)
	link := proton.Link{LinkID: "target-link"}
	c.SetStat("/doc/file.txt", link, nil)

	c.invalidateLinkID("target-link")

	if _, _, ok := c.GetStat("/doc/file.txt"); ok {
		t.Error("stat should be evicted by linkID")
	}
}

func TestMetaCacheInvalidateLinkIDMissing(t *testing.T) {
	c := NewMetaCache(time.Hour)
	c.invalidateLinkID("nonexistent") // must not panic
}
