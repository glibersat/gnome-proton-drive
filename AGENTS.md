# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build and test commands

```sh
make                  # build Go helper + C backend
make install          # install all (usually needs sudo)
make test             # Go tests (-race) + Meson backend tests
make test-helper      # Go tests only
make test-backend     # C backend tests only

# Single Go test
go test -C helper -run TestBlockCacheEviction ./drive/...

# Go build / vet directly
go build -C helper ./...
go vet -C helper ./...
```

The Meson build is configured once into `_build/` on the first `make build-backend`; subsequent calls just run `ninja`.

## Architecture

Two processes collaborate over a Unix socket:

**`proton-drive-helper` (Go, `helper/`)** — owns all crypto and API work.
- `main.go` — registers JSON-RPC handlers and serves the socket.
- `rpc/` — wire protocol (line-delimited JSON, one object per line). `types.go` defines all params/results and error codes; `server.go` dispatches.
- `drive/session.go` — the core: resolves POSIX paths to Proton linkIDs, decrypts names and content with `go-proton-api` / `gopenpgp`. Wraps every network call with the two caches.
- `drive/metacache.go` — in-memory TTL cache (30 s) for `Stat` and `ListChildren` results. `InvalidatePath(path)` is the B4 event hook.
- `drive/blockcache.go` — persistent disk cache under `~/.cache/proton-drive/<account>/blocks/<linkID>/<revisionID>`. LRU eviction, mtime-touched on hit. `InvalidateLink(linkID)` is the B4 event hook.
- `drive/errors.go` — `isOfflineError()` and the `ErrOffline` sentinel.

**`gvfsd-proton` (C, `backend/`)** — thin GVfs backend, no caching.
- `proton-rpc.c/h` — JSON-RPC client over the Unix socket; thread-safe via mutex.
- `gvfsbackendproton.c` — GVfs vtable: `do_mount`, `do_open_for_read`, `do_read`, `do_query_info`, `do_enumerate`, `do_close_read`. On mount it reads credentials from libsecret via `secret-tool`, calls `ResumeSession`, and spawns the helper if the socket is absent.
- `daemon-main.c` / `daemon-main-generic.c` — GVfs daemon boilerplate.
- `backend/tests/test-proton-rpc.c` — unit tests for the RPC client using an in-process mock socket server.

**`proton-drive-setup` (Python)** — GTK wizard that runs SRP login through the helper and writes tokens to the GNOME keyring.

## Key invariants

- **Path resolution:** `session.Stat` and `session.ListChildren` in `session.go` cache results and fall back to stale data on `isOfflineError`. The underlying `resolvePath` walks the tree by decrypting each segment's name with the parent node keyring.
- **Crypto chain:** share keyring → root node keyring → child node keyring → session key → block decryption. Every keyring is cached in `session.nodeKRs` after first use.
- **Offline detection:** `isOfflineError` matches `net.Error` (covers `*url.Error` from resty) and `context.DeadlineExceeded`. Any other error is not treated as offline.
- **RPC error codes:** `-32001` not found, `-32003` not authenticated, `-32005` offline with no cache. See `rpc/types.go`.
- **B4 invalidation hooks:** `meta.InvalidatePath(path)` and `blocks.InvalidateLink(linkID)` are the only two calls needed when event polling (ROADMAP §B4) lands.

## Project state

**Alpha, read-only.** The GVfs backend exposes only read operations. Write stubs (`Mkdir`, `Move`, `WriteFile`) exist in the helper but return errors — they are blocked on missing `go-proton-api` primitives. The block cache stores plaintext (no re-encryption). There is no volume monitor; mounts are manual via `gio mount`. See `ROADMAP.md` for the milestone plan and `docs/caching.md` for the cache architecture.

## Testing notes

- Go tests for the `drive` package are in the same package (white-box). Run with `-race`.
- The C backend tests spin a mock Unix socket server in-process; no running helper needed.
- Integration tests that need a live API should use `go-proton-api`'s `server/` dev server (not yet wired up in this repo).
- Tests must be written alongside every new piece of code — never deferred.
