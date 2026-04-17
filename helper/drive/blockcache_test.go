package drive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestBlockCache(t *testing.T, maxSize int64) *BlockCache {
	t.Helper()
	bc, err := NewBlockCache(t.TempDir(), maxSize)
	if err != nil {
		t.Fatalf("NewBlockCache: %v", err)
	}
	return bc
}

func TestBlockCacheGetMiss(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	data, ok := bc.Get("link1", "rev1")
	if ok || data != nil {
		t.Fatal("expected cache miss on empty cache")
	}
}

func TestBlockCachePutAndGet(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	want := []byte("hello, proton")

	if err := bc.Put("link1", "rev1", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := bc.Get("link1", "rev1")
	if !ok {
		t.Fatal("expected cache hit after Put")
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBlockCacheGetTouchesMtime(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	if err := bc.Put("link1", "rev1", []byte("data")); err != nil {
		t.Fatal(err)
	}

	p := bc.path("link1", "rev1")
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	// Ensure at least 1ms passes so mtime can differ.
	time.Sleep(5 * time.Millisecond)
	bc.Get("link1", "rev1")

	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().After(before.ModTime()) {
		t.Error("Get should have updated mtime")
	}
}

func TestBlockCacheInvalidateLink(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	if err := bc.Put("link1", "rev1", []byte("data")); err != nil {
		t.Fatal(err)
	}

	bc.InvalidateLink("link1")

	_, ok := bc.Get("link1", "rev1")
	if ok {
		t.Fatal("expected cache miss after InvalidateLink")
	}
}

func TestBlockCacheInvalidateLinkMissing(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	bc.InvalidateLink("nonexistent") // must not panic or return an error
}

func TestBlockCacheEviction(t *testing.T) {
	// maxSize = 30 bytes; we'll insert 3 × 10-byte entries, then a 4th.
	// The oldest two should be evicted to bring total to ≤ 27 bytes (90 %).
	bc := newTestBlockCache(t, 30)

	put := func(link, rev string, delay bool) {
		if delay {
			// Ensure distinguishable mtime ordering.
			time.Sleep(5 * time.Millisecond)
		}
		if err := bc.Put(link, rev, []byte("0123456789")); err != nil { // 10 bytes each
			t.Fatalf("Put %s/%s: %v", link, rev, err)
		}
	}

	put("l1", "r1", false)
	put("l2", "r2", true)
	put("l3", "r3", true) // total = 30, right at limit

	// Adding a 4th entry (40 total) must trigger eviction.
	put("l4", "r4", true)

	// l1 is the oldest; it must be gone.
	if _, ok := bc.Get("l1", "r1"); ok {
		t.Error("expected l1/r1 to be evicted")
	}
	// l4 (newest) must still be present.
	if _, ok := bc.Get("l4", "r4"); !ok {
		t.Error("expected l4/r4 to be present after eviction")
	}
}

func TestBlockCacheDirCreated(t *testing.T) {
	base := t.TempDir()
	bc, err := NewBlockCache(base, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bc.root)); err != nil {
		t.Fatalf("blocks dir not created: %v", err)
	}
}
