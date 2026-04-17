package drive

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	defaultBlockCacheSize = 2 << 30 // 2 GiB
	// evictTargetFraction is the fraction of maxSize we evict down to.
	// Evicting in a batch avoids per-write eviction overhead.
	evictTargetFraction = 0.9
)

// BlockCache is a persistent, on-disk store for individually cached file blocks.
//
// Blocks are keyed by (linkID, revisionID, blockIndex) and stored under:
//
//	<root>/<linkID>/<revisionID>/<blockIndex>
//
// where <root> is the blocks sub-directory of the account cache directory
// (typically ~/.cache/proton-drive/<account>/blocks/) and blockIndex is
// 0-based.
//
// Reading a cached block updates its mtime so that LRU eviction removes the
// least-recently-used blocks first.
//
// # Invalidation
//
// InvalidateLink removes all cached blocks for a linkID.  This is the B4
// hook: when the event poller (ROADMAP §B4) detects that a file has changed,
// it calls InvalidateLink with the affected linkID.  The next read of that
// file will fetch and re-cache the new revision from the API.
//
// # Thread safety
//
// GetBlock is lock-free (concurrent reads are safe and benign).  PutBlock
// serialises the post-write eviction pass under a mutex, but file I/O itself
// is outside the lock.  InvalidateLink holds no lock; os.RemoveAll is safe to
// call concurrently with reads on Linux.
type BlockCache struct {
	root    string
	maxSize int64
	mu      sync.Mutex
}

// NewBlockCache creates a BlockCache whose files live under baseDir/blocks.
// A maxSize of 0 uses the 2 GiB default.
// Returns an error if the directory cannot be created.
func NewBlockCache(baseDir string, maxSize int64) (*BlockCache, error) {
	if maxSize == 0 {
		maxSize = defaultBlockCacheSize
	}
	root := filepath.Join(baseDir, "blocks")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &BlockCache{root: root, maxSize: maxSize}, nil
}

// InvalidateLink removes all cached blocks for linkID.
// This is the B4 event hook: call this when the event poller reports that
// a file's content has changed.  The removal is best-effort; errors are
// silently ignored because a stale cache entry will simply be overwritten on
// the next PutBlock.
func (bc *BlockCache) InvalidateLink(linkID string) {
	_ = os.RemoveAll(filepath.Join(bc.root, linkID))
}

// blockPath returns the filesystem path for a per-block cache entry.
// index is 0-based (block 0 = first block of the file).
func (bc *BlockCache) blockPath(linkID, revisionID string, index int) string {
	return filepath.Join(bc.root, linkID, revisionID, strconv.Itoa(index))
}

// GetBlock returns the cached decrypted bytes for one block identified by
// (linkID, revisionID, index), where index is 0-based.
// Returns (nil, false) on a cache miss.
// On a hit the block file's mtime is updated for LRU ordering.
func (bc *BlockCache) GetBlock(linkID, revisionID string, index int) ([]byte, bool) {
	p := bc.blockPath(linkID, revisionID, index)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return data, true
}

// PutBlock stores decrypted bytes for one block and runs LRU eviction if
// needed.  index is 0-based.
//
// If a whole-file cache entry already exists at the revision path (written by
// the old Get/Put API), it is removed before creating the per-block directory
// so the two formats do not collide.
func (bc *BlockCache) PutBlock(linkID, revisionID string, index int, data []byte) error {
	revDir := filepath.Join(bc.root, linkID, revisionID)
	if info, err := os.Stat(revDir); err == nil && !info.IsDir() {
		if err := os.Remove(revDir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(revDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(bc.blockPath(linkID, revisionID, index), data, 0o600); err != nil {
		return err
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.evictIfNeeded()
	return nil
}

// cacheFile holds metadata for one cached block during eviction.
type cacheFile struct {
	path  string
	mtime time.Time
	size  int64
}

// evictIfNeeded removes the least-recently-used files until the total cache
// size falls to 90 % of the limit.  Must be called with bc.mu held.
func (bc *BlockCache) evictIfNeeded() {
	var files []cacheFile
	var total int64

	_ = filepath.WalkDir(bc.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, cacheFile{path: p, mtime: info.ModTime(), size: info.Size()})
		total += info.Size()
		return nil
	})

	if total <= bc.maxSize {
		return
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].mtime.Before(files[j].mtime)
	})

	target := int64(float64(bc.maxSize) * evictTargetFraction)
	for _, f := range files {
		if total <= target {
			break
		}
		if err := os.Remove(f.path); err != nil {
			log.Printf("blockcache evict %s: %v", f.path, err)
			continue
		}
		total -= f.size
		_ = os.Remove(filepath.Dir(f.path)) // remove parent dir if now empty
	}
}
