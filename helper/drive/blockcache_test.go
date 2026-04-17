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

func TestBlockCacheInvalidateLinkMissing(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	bc.InvalidateLink("nonexistent") // must not panic or return an error
}

func TestBlockCacheInvalidateLink(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	if err := bc.PutBlock("link1", "rev1", 0, []byte("data")); err != nil {
		t.Fatal(err)
	}

	bc.InvalidateLink("link1")

	_, ok := bc.GetBlock("link1", "rev1", 0)
	if ok {
		t.Fatal("expected cache miss after InvalidateLink")
	}
}

func TestBlockCacheEviction(t *testing.T) {
	// maxSize = 30 bytes; insert 3 × 10-byte per-block entries, then a 4th.
	// The oldest two should be evicted to bring total to ≤ 27 bytes (90 %).
	bc := newTestBlockCache(t, 30)

	put := func(link, rev string, delay bool) {
		if delay {
			// Ensure distinguishable mtime ordering.
			time.Sleep(5 * time.Millisecond)
		}
		if err := bc.PutBlock(link, rev, 0, []byte("0123456789")); err != nil { // 10 bytes each
			t.Fatalf("PutBlock %s/%s: %v", link, rev, err)
		}
	}

	put("l1", "r1", false)
	put("l2", "r2", true)
	put("l3", "r3", true) // total = 30, right at limit

	// Adding a 4th entry (40 total) must trigger eviction.
	put("l4", "r4", true)

	// l1 is the oldest; it must be gone.
	if _, ok := bc.GetBlock("l1", "r1", 0); ok {
		t.Error("expected l1/r1 to be evicted")
	}
	// l4 (newest) must still be present.
	if _, ok := bc.GetBlock("l4", "r4", 0); !ok {
		t.Error("expected l4/r4 to be present after eviction")
	}
}

func TestBlockCacheGetBlockMiss(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	data, ok := bc.GetBlock("link1", "rev1", 0)
	if ok || data != nil {
		t.Fatal("expected cache miss on empty cache")
	}
}

func TestBlockCachePutBlockAndGet(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	want := []byte("block zero data")

	if err := bc.PutBlock("link1", "rev1", 0, want); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	got, ok := bc.GetBlock("link1", "rev1", 0)
	if !ok {
		t.Fatal("expected cache hit after PutBlock")
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBlockCacheGetBlockTouchesMtime(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	if err := bc.PutBlock("link1", "rev1", 0, []byte("data")); err != nil {
		t.Fatal(err)
	}

	p := bc.blockPath("link1", "rev1", 0)
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)
	bc.GetBlock("link1", "rev1", 0)

	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().After(before.ModTime()) {
		t.Error("GetBlock should have updated mtime")
	}
}

func TestBlockCacheGetBlockMissesOtherIndex(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	if err := bc.PutBlock("link1", "rev1", 0, []byte("block0")); err != nil {
		t.Fatal(err)
	}
	if _, ok := bc.GetBlock("link1", "rev1", 1); ok {
		t.Error("expected cache miss for index 1 when only index 0 is stored")
	}
}

func TestBlockCacheInvalidateLinkRemovesPerBlocks(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	if err := bc.PutBlock("link1", "rev1", 0, []byte("b0")); err != nil {
		t.Fatal(err)
	}
	if err := bc.PutBlock("link1", "rev1", 1, []byte("b1")); err != nil {
		t.Fatal(err)
	}

	bc.InvalidateLink("link1")

	if _, ok := bc.GetBlock("link1", "rev1", 0); ok {
		t.Error("expected block 0 to be gone after InvalidateLink")
	}
	if _, ok := bc.GetBlock("link1", "rev1", 1); ok {
		t.Error("expected block 1 to be gone after InvalidateLink")
	}
}

func TestBlockCachePutBlockMigration(t *testing.T) {
	bc := newTestBlockCache(t, 0)
	// Simulate a whole-file cache entry from the old format: a plain file at
	// <root>/link1/rev1 (no sub-directory).
	linkDir := filepath.Join(bc.root, "link1")
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(linkDir, "rev1")
	if err := os.WriteFile(oldPath, []byte("whole file"), 0o600); err != nil {
		t.Fatal(err)
	}

	// PutBlock must silently remove the old file and write the per-block entry.
	if err := bc.PutBlock("link1", "rev1", 0, []byte("block0")); err != nil {
		t.Fatalf("PutBlock after old whole-file entry: %v", err)
	}
	got, ok := bc.GetBlock("link1", "rev1", 0)
	if !ok {
		t.Fatal("expected per-block cache hit after migration")
	}
	if string(got) != "block0" {
		t.Fatalf("got %q, want %q", got, "block0")
	}
	// Old whole-file file must be gone (now a directory at that path).
	info, err := os.Stat(oldPath)
	if err != nil {
		t.Fatalf("stat rev1: %v", err)
	}
	if !info.IsDir() {
		t.Error("rev1 path should now be a directory, not a file")
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
