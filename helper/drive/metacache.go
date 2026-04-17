// Package drive implements the Proton Drive session layer for the GVfs helper.
//
// # Caching architecture
//
// Two complementary caches live inside each Session:
//
//   - MetaCache (this file): an in-process, TTL-based store for directory
//     listings (ListChildren) and file/folder metadata (Stat).  Reduces
//     round-trips for repeated Nautilus re-stats during enumeration and
//     drag-and-drop.  Survives only for the lifetime of the mount.
//
//   - BlockCache (blockcache.go): a persistent on-disk store for fully
//     decrypted file content, keyed by (linkID, revisionID).  Allows
//     repeated reads of the same file without re-fetching from the API and
//     enables offline reads for previously accessed files.
//
// # Invalidation
//
// Both caches expose explicit invalidation hooks designed for the future B4
// event-polling integration (ROADMAP §B4).  When that lands, the event loop
// calls:
//
//	session.meta.InvalidatePath(event.Path)
//	session.blocks.InvalidateLink(event.LinkID)
//
// No other code changes are required for invalidation to work end-to-end.
package drive

import (
	"path"
	"sync"
	"time"

	proton "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

const defaultMetaTTL = 30 * time.Second

// metaStatEntry is a single cached Stat result.
type metaStatEntry struct {
	link     proton.Link
	parentKR *crypto.KeyRing
	cachedAt time.Time
}

// metaListEntry is a single cached ListChildren result.
type metaListEntry struct {
	links    []proton.Link
	parentKR *crypto.KeyRing
	cachedAt time.Time
}

// MetaCache is a thread-safe, in-memory TTL cache for Stat and ListDir
// results.  The zero value is not usable; allocate with NewMetaCache.
type MetaCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	stats map[string]metaStatEntry
	lists map[string]metaListEntry
}

// NewMetaCache returns a MetaCache with the given TTL.  A zero ttl uses the
// 30-second default.
func NewMetaCache(ttl time.Duration) *MetaCache {
	if ttl == 0 {
		ttl = defaultMetaTTL
	}
	return &MetaCache{
		ttl:   ttl,
		stats: make(map[string]metaStatEntry),
		lists: make(map[string]metaListEntry),
	}
}

// GetStat returns a fresh cached Stat result for path, or false on a miss or
// TTL expiry.
func (c *MetaCache) GetStat(p string) (proton.Link, *crypto.KeyRing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.stats[p]
	if !ok || time.Since(e.cachedAt) >= c.ttl {
		return proton.Link{}, nil, false
	}
	return e.link, e.parentKR, true
}

// SetStat stores a Stat result for path.
func (c *MetaCache) SetStat(p string, link proton.Link, kr *crypto.KeyRing) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats[p] = metaStatEntry{link: link, parentKR: kr, cachedAt: time.Now()}
}

// GetStatStale returns any cached Stat result regardless of TTL.  Used as an
// offline fallback when the API is unreachable.
func (c *MetaCache) GetStatStale(p string) (proton.Link, *crypto.KeyRing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.stats[p]
	if !ok {
		return proton.Link{}, nil, false
	}
	return e.link, e.parentKR, true
}

// GetList returns a fresh cached ListChildren result for dirPath, or false on
// a miss or TTL expiry.
func (c *MetaCache) GetList(dirPath string) ([]proton.Link, *crypto.KeyRing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.lists[dirPath]
	if !ok || time.Since(e.cachedAt) >= c.ttl {
		return nil, nil, false
	}
	return e.links, e.parentKR, true
}

// SetList stores a ListChildren result for dirPath.
func (c *MetaCache) SetList(dirPath string, links []proton.Link, kr *crypto.KeyRing) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists[dirPath] = metaListEntry{links: links, parentKR: kr, cachedAt: time.Now()}
}

// GetListStale returns any cached ListChildren result regardless of TTL.
// Used as an offline fallback when the API is unreachable.
func (c *MetaCache) GetListStale(dirPath string) ([]proton.Link, *crypto.KeyRing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.lists[dirPath]
	if !ok {
		return nil, nil, false
	}
	return e.links, e.parentKR, true
}

// InvalidatePath removes cached metadata for p and for its parent directory
// listing.  This is the B4 event hook: the event poller calls this for every
// received LinkEvent, and no other code changes are needed.
func (c *MetaCache) InvalidatePath(p string) {
	if p == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.stats, p)
	delete(c.lists, p)
	delete(c.lists, path.Dir(p))
}

// invalidateLinkID scans the stat cache for any entry whose LinkID matches
// and removes it along with its parent list entry.  Used when a path is not
// in the reverse map (linkID → path) but we know the linkID from an event.
func (c *MetaCache) invalidateLinkID(linkID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p, e := range c.stats {
		if e.link.LinkID == linkID {
			delete(c.stats, p)
			delete(c.lists, p)
			delete(c.lists, path.Dir(p))
			return
		}
	}
}

// invalidateAll clears every entry.  Called when the server signals a full
// refresh (DriveEvent.Refresh == true).
func (c *MetaCache) invalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats = make(map[string]metaStatEntry)
	c.lists = make(map[string]metaListEntry)
}
