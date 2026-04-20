# Caching and Offline Access

This document describes the two-layer caching architecture introduced in
`feature/ro-caching`.  It is aimed at contributors working on the Go helper
(`helper/`) and anyone integrating the future B4 event-polling feature
(ROADMAP §B4).

---

## Overview

```
Nautilus
   │  GVfs read ops
   ▼
gvfsd-proton (C backend)
   │  JSON-RPC over Unix socket
   ▼
proton-drive-helper (Go)
   │
   ├── MetaCache (in-process, TTL 30 s)
   │     Stat / ListDir results
   │
   ├── BlockCache (~/.cache/proton-drive/<account>/blocks/)
   │     Decrypted file content, keyed by (linkID, revisionID)
   │
   ├── ThumbnailCache (~/.cache/proton-drive/<account>/thumbnails/)
   │     Server-generated thumbnails, keyed by (linkID, revisionID)
   │
   └── Proton API (network)
```

The Go helper owns both caches.  The C backend is cache-unaware and always
sends requests over RPC.

---

## Layer 1 — MetaCache

**File:** `helper/drive/metacache.go`

An in-memory, TTL-based cache for `Stat` and `ListChildren` results.

| Property | Value |
|----------|-------|
| Storage | `sync.Map`-equivalent (mutex + Go maps) |
| TTL | 30 s (configurable via `NewMetaCache(ttl)`) |
| Scope | Per-mount (Session lifetime) |
| Offline fallback | `GetStatStale` / `GetListStale` return any age |

### Cache flow

```
GetStat(path)
  ├── Fresh entry?  → return immediately (no network)
  ├── Miss / stale? → call API
  │     ├── Success → SetStat(path, …); return
  │     └── isOfflineError? → GetStatStale(path)
  │           ├── Stale entry exists? → return (offline served)
  │           └── No entry → propagate error
  └── (same pattern for GetList / SetList)
```

### B4 invalidation hook

```go
session.meta.InvalidatePath(event.Path)
```

Removes the `Stat` entry for the changed path, the `ListDir` entry for that
path, and the `ListDir` entry for the parent directory.  Called once per B4
event; no other code changes needed.

---

## Layer 2 — BlockCache

**File:** `helper/drive/blockcache.go`

A persistent, on-disk cache for fully decrypted file content.

| Property | Value |
|----------|-------|
| Storage | `~/.cache/proton-drive/<account>/blocks/<linkID>/<revisionID>` |
| `<account>` | URL-path-escaped email (e.g. `user%40proton.me`) |
| Max size | 2 GiB (configurable via `NewBlockCache(base, maxSize)`) |
| Eviction | LRU by mtime; batch-evict to 90 % of limit on each `Put` |
| Encryption | None (deferred; rely on OS full-disk encryption for now) |
| Scope | Persistent across mounts |

### Cache flow

```
ReadFileContent(ctx, link, parentKR)
  ├── blocks.Get(linkID, revID)?
  │     └── Hit → return bytes (no network, mtime touched for LRU)
  │     └── Miss → fetch from API
  │           ├── Success → blocks.Put(linkID, revID, data); return
  │           └── isOfflineError? → return drive.ErrOffline
  └── (ErrOffline mapped to rpc.ErrOffline = -32005 by main.go)
```

### LRU eviction

After each `Put`, `evictIfNeeded` walks the `blocks/` tree, sums file sizes,
and removes the oldest files (by mtime) until the total is ≤ 90 % of the
limit.  `Get` updates the file mtime so recently-read files are retained.

### B4 invalidation hook

```go
session.blocks.InvalidateLink(event.LinkID)
```

Removes the entire `blocks/<linkID>/` directory, so the next read fetches the
new revision from the API.  Called once per B4 changed/deleted event.

---

## Offline detection

**File:** `helper/drive/errors.go`

```go
func isOfflineError(err error) bool
```

Returns `true` when `err` is a `net.Error` (covers DNS failures,
connection-refused, and `*url.Error` wrappers from resty/go-proton-api) or
`context.DeadlineExceeded`.

---

## RPC error code

`rpc.ErrOffline = -32005` is returned to the C backend when a file read fails
offline with no cached data.  The backend maps this to
`G_IO_ERROR_NOT_CONNECTED` so Nautilus shows an appropriate message.

---

## B4 integration checklist

When the event-polling goroutine (ROADMAP §B4) lands, the integration is:

1. On a `changed` or `created` event for a path:
   ```go
   session.meta.InvalidatePath(event.Path)
   ```

2. On a `changed` event for a file (content change):
   ```go
   session.blocks.InvalidateLink(event.LinkID)
   ```

3. On a `deleted` event:
   ```go
   session.meta.InvalidatePath(event.Path)
   session.blocks.InvalidateLink(event.LinkID)
   ```

No other code changes are required.

---

## Cache directory layout

```
~/.cache/proton-drive/
└── user%40proton.me/           ← URL-path-escaped account email
    ├── blocks/
    │   └── <linkID>/
    │       └── <revisionID>    ← raw decrypted bytes
    └── thumbnails/
        └── <linkID>/
            └── <revisionID>    ← raw thumbnail bytes (JPEG/WebP)
```

Each file contains the complete decrypted content of one file revision.
Partial block caching (ROADMAP §B2) will replace this once streaming reads
are implemented.

---

## Open questions

- **Re-encrypt blocks for local storage** to avoid storing plaintext on disk,
  relying instead on the OS keyring.  See ROADMAP §C4 open questions.
- **Pinning UI**: a D-Bus method on the volume monitor vs. a `proton-drive-pin`
  CLI.  See ROADMAP §C4 open questions.
- **Metadata persistence**: the current `MetaCache` is in-process only.
  Persisting it across mounts would benefit slow connections but adds
  invalidation complexity.
