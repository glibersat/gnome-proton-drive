# ROADMAP

## Overview

Three independent tracks that converge into a full GNOME Proton Drive integration.
Each track can be developed in parallel; the dependency arrows show what must land
before the final assembly.

```
Track A: Credentials       Track B: Go helper        Track C: GVfs backend
─────────────────────      ──────────────────────     ──────────────────────
A1. libsecret storage   →
A2. setup wizard        →  B3. write operations   →  C1. backend skeleton
                        →                         →  C2. volume monitor
A3. GOA provider        →                         →     (future upgrade)
                                                   →  C3. two-way sync
```

**M1 strategy:** A1 + A2 feed directly into C2 via libsecret. No GOA dependency
for the first working mount. A3 is a later UX upgrade that slots in without
changing the backend architecture.

---

## Track A — Credential management

### A1. libsecret storage (prerequisite for everything)

Store and retrieve Proton credentials independently of GOA so the helper and
backend have a working auth path from day one.

- Use `libsecret` to store `{username, session_token}` in the user's keyring
  under a well-known schema: `org.gnome.proton.drive / username=<email>`.
- Store the session token (returned by `go-proton-api` after SRP login), not
  the raw password — the password never leaves the setup wizard process.
- The Go helper receives the account ID via argv and fetches its token from the
  keyring at startup using `secret-tool` or a small C shim.
- **Why not store credentials in the helper itself:** the keyring survives
  helper restarts and is unlocked by PAM at login, matching GNOME conventions.

### A2. Setup wizard (self-contained, no upstream dependency)

A minimal GTK4 dialog (`gnome-proton-setup`) that:

1. Prompts for email + password.
2. Delegates the SRP challenge/response to `proton-drive-helper` over the RPC
   socket — the GTK layer never holds the plaintext password beyond one RPC
   round-trip.
3. On success, writes the returned session token to libsecret (A1).
4. Emits a signal on our own D-Bus service (`org.gnome.ProtonDrive`) to notify
   the volume monitor that a new account is available.

**Why our own D-Bus service, not GOA's:**
`goa-daemon` owns `org.gnome.OnlineAccounts` and its `Manager.AddAccount()`
rejects any `provider_type` not compiled into the daemon. We cannot register
objects on a bus name we don't own, and writing `Provider=proton` directly into
`~/.config/goa-1.0/accounts.conf` is equally rejected at daemon reload. Our
volume monitor (C2) watches our own D-Bus service and libsecret directly,
making GOA a non-dependency for M1–M3.

This gives a fully working end-to-end flow shipping entirely within this
package.

### A3. GOA provider (long-term, proper GNOME integration)

Adding Proton as a first-class entry in GNOME Settings → Online Accounts.
**Not required for M1–M3.** When it lands, C2 gains a second credential source
(GOA) alongside libsecret, with no other backend changes.

**Hard constraint:** GOA providers are compiled into `gnome-online-accounts`.
There is no external plugin mechanism. The options are:

#### Option A3-a — Contribute upstream (recommended)

Submit a `GoaProtonProvider` to the GOA project:

- Subclass `GoaProvider` (not `GoaOAuth2Provider` — Proton uses SRP).
- Implement `build_object()` to attach `GoaFiles` (and optionally `GoaMail`)
  service interfaces.
- Implement `add_account()` / `refresh_account()` dialogs delegating SRP to a
  helper subprocess — no Go in the GOA daemon.
- Store the session token via GOA's standard libsecret helpers.

Prerequisites: stable Proton API, no new non-GNOME build deps in the GOA tree.
Multi-month process requiring upstream maintainer review.

#### Option A3-b — Patched GOA package (distribution bridge)

Ship a downstream `gnome-online-accounts` with the Proton provider patched in.
Viable for distro packages while A3-a is in review; maintenance burden on every
GOA release.

---

## Testing

Tests must be written alongside every piece of code — never deferred.

- **Go helper (`helper/`):** `_test.go` files in the same package. Cover RPC
  serialisation, path resolution logic, and session helpers with unit tests.
  Use the `go-proton-api` dev server (in `server/`) for integration tests that
  need a live Proton API.
- **C backend (`backend/`):** `tests/` directory built with meson. Unit-test
  the RPC client (`proton-rpc.c`) by spinning a mock Unix socket server in the
  test process. Integration tests mount against a running `proton-drive-helper`
  pointed at the dev server.
- CI must run both suites on every commit.

---

## Track B — Go helper completion

### B1. Path resolution cache ✅

`resolvePath` short-circuits on already-resolved paths and warms all cache
layers on every tree walk:

- ✅ **MetaCache warmed during traversal** — each `client.ListChildren` result
  is stored via `meta.SetList`; `statUncached` routes through `s.ListChildren`
  rather than the raw client call.
- ✅ **path→linkID forward map** — `Session.pathLinks` caches the full resolved
  path → linkID mapping. `resolvePath` short-circuits entirely on a hit,
  skipping the tree walk and all per-segment name-decryption crypto. Each
  resolved segment is stored so intermediate paths are also cached. Invalidation
  (`Session.InvalidatePath` / `invalidateLinkID` / `invalidateAll`) clears
  `pathLinks` alongside MetaCache, keeping all layers consistent.

**Remaining:**

- **Parallel name decryption in `resolvePath`** *(medium effort, helps wide dirs)* —
  At each path segment all child names are decrypted serially to find the match.
  Fanning out `GetName` into goroutines (first-match via channel) cuts crypto
  time proportionally to directory width.

### B2. Streaming block reads

Current `ReadFile` buffers the entire file before applying offset/length.
Replace with a streaming approach: fetch and decrypt blocks on demand, seeking
by skipping blocks whose byte range falls entirely before `offset`.

### B3. Write operations

The API calls exist and their parameters are fully documented from the Windows
client (see `docs/reference.md`). Three sub-tasks remain blocked on
`go-proton-api` not wrapping the required crypto steps:

| Sub-task | API | Blocker |
|---|---|---|
| `Mkdir` | `POST /shares/{id}/folders` | Must generate `NodeKey` (Ed25519+X25519), random `NodePassphrase` (32 bytes, base64), encrypt passphrase with parent `NodeKey`, compute `NameHash = HMAC-SHA256(hashKey, name)`. go-proton-api exposes the endpoint but not the key-generation helpers. |
| `WriteFile` | `POST /shares/{id}/files` → `POST /shares/{id}/files/{id}/revisions` → `POST /blocks` → commit | Blocks are 4 MiB, encrypted with a per-file `SessionKey` (PGP data packet), signed, SHA-256 hashed. Upload pipeline: batch-request URLs, then PUT multipart (3 concurrent). Crypto wrapper missing from go-proton-api. |
| `Move` / `Rename` | `PUT /shares/{id}/links/{id}/move` | Passphrase must be re-encrypted under the new parent's `NodeKey`; `NameHash` recomputed. `MoveLink` call missing from go-proton-api. |

Track against `go-proton-api` releases. When unblocked, implement in
`drive/session.go` and register the handlers in `main.go`.

### B4. Event polling ✅

Polls `/drive/volumes/{id}/events/{anchorID}` every 30 s via `EventPoller`
(`helper/drive/events.go`). The C backend drains the queue every 5 s via
`GetEvents` RPC and emits `GVfsMonitor` notifications.

**What works:**
- Volume-level event endpoint used (covers all shares in one poll).
- Anchor persisted to `~/.cache/proton-drive/<account>/anchor` after each
  successful batch; resumed on restart so no events are missed.
- Stale anchor recovered automatically: on empty `EventID` response the poller
  re-anchors at the current head and requests a full metadata refresh.
- `MetaCache` and `BlockCache` invalidated immediately on receipt.
- Diff-based change detection: for `Create` and `UpdateMetadata` events the
  poller captures the old directory listing from the cache before invalidation,
  fetches the new listing from the API, and emits precise `CREATED` / `DELETED`
  events with the child path — which is what Nautilus requires to add/remove
  entries without a full re-enumeration.
- Trash detection: when a `UpdateMetadata` event arrives for a known path, the
  helper fetches the link state; `LinkStateTrashed` and `LinkStateDeleted` both
  emit `EventDeleted` so Nautilus removes the entry immediately.
- GVfs monitors triggered with `CREATED` / `DELETED` / `CHANGED` on the
  specific child path, not just the parent directory.

**Known gaps:**

| Gap | Impact | Fix |
|---|---|---|
| `HasMoreData` not drained immediately | Burst of events takes many 30 s ticks to process | Loop until `HasMoreData == false` before waiting for next tick |
| `UpdateMetadata` not distinguished from `EventChanged` for renames/moves | Renames appear as delete+create instead of move | Add `EventMoved` type for future C3 move propagation (B3 dependency) |

---

## Track C — GVfs backend

### C1. Backend skeleton ✅

Implements `gvfsd-proton` in C (`backend/`):

- Subclasses `GVfsBackend`.
- On `mount`: reads credentials from libsecret via `secret-tool`, spawns
  `proton-drive-helper`, calls `ResumeSession`.
- Read-only VFS ops via RPC: `open_for_read`, `read`, `close_read`,
  `query_info`, `enumerate`.
- Write ops return `G_IO_ERROR_NOT_SUPPORTED` until B3 lands.

### C2. Volume monitor

A `gvfsd-proton-volume-monitor` daemon that makes the drive appear in Nautilus:

- **Primary source (M1–M3):** watch libsecret for entries matching the
  `org.gnome.proton.drive` schema, and listen on `org.gnome.ProtonDrive` D-Bus
  for account-added/removed signals emitted by the setup wizard (A2).
- **Secondary source (M4+):** additionally watch `GoaClient` for GOA accounts
  with `provider_type=proton` once A3 lands.
- On account detected: emit `volume-added` on `GVfsVolumeMonitor`, providing
  icon (`proton-drive` symbolic), display name, and UUID from the account email.
- On account removed: unmount and emit `volume-removed`.

### C3. Two-way sync

**Remote → local ✅ (partial)**

- B4 event polling drives `GVfsMonitor` notifications via a 5 s GLib timer
  in `gvfsbackendproton.c`.
- `do_create_dir_monitor` and `do_create_file_monitor` vtable slots implemented.
- When the specific changed file path is unknown, `CHANGED` is emitted on the
  parent directory monitor so the file manager re-enumerates.
- Known gap: monitor notifications fail silently when the GVfs client has
  disconnected (D-Bus name gone). The monitor entry remains in `self->monitors`
  until its `GVfsMonitor` GObject is finalised.

**Local → remote:** VFS write ops (once B3 is done) go directly to the API
via the helper — no separate sync step.

**Conflict policy (from Windows client reference):** remote wins on edit-edit
conflicts; the local file is backed up with a `_conflict` suffix before
overwrite. Revisit once revision history API is accessible.

### C4. Caching and offline access

**Metadata cache (in-process, Track B) ✅**

- `ListDir` and `Stat` results cached in the helper (`helper/drive/metacache.go`).
- Currently TTL-based (30 s); planned replacement with an entry-count LRU — the
  B4 event poller already calls `InvalidatePath` on every remote change, so the
  TTL exists only as a fallback for missed events. With volume-level events and
  anchor persistence, correctness is guaranteed by invalidation rather than expiry;
  a pure LRU (no TTL) reduces unnecessary API round-trips for hot directories.
- Stale entries served when the API is unreachable (offline fallback).
- `InvalidatePath(path)` wired to B4 events.
- Eliminates redundant API round-trips when Nautilus re-stats files during
  enumeration and drag-and-drop.

**Block cache / offline store (persistent, read-only tier) ✅ (partial)**

- Decrypted file content stored on disk under
  `~/.cache/proton-drive/<account>/blocks/<linkID>/<revisionID>`
  (`helper/drive/blockcache.go`).
- *Read-through* tier implemented: blocks fetched on demand, cached on first
  read, served from disk on subsequent reads.
- 2 GiB LRU eviction; mtime updated on each cache hit for accurate ordering.
- **Offline reads** work for previously-opened files (`rpc.ErrOffline = -32005`
  returned to the C backend when offline with no cached data).
- `InvalidateLink(linkID)` wired to B4 events.

**Remaining for full C4:**

- *Pinned tier:* user-marked files synced proactively and available without
  network access.
- *Write queue:* queue local writes offline and flush on reconnect (blocked on
  B3 write operations).
- *Conflict detection* on reconnect: compare local mtime against revision seen
  in event stream.
- *Block re-encryption* for local storage (currently stored as plaintext —
  relies on OS full-disk encryption).

**Open questions for C4:**
- Re-encrypt blocks for local storage (avoids storing plaintext on disk) or
  rely on the OS keyring / full-disk encryption?
- Pinning UI: extend the volume monitor with a D-Bus method the file manager
  can call, or ship a separate `proton-drive-pin` CLI?

---

## Milestones

| Milestone | Tracks | Deliverable | Status |
|---|---|---|---|
| **M1 — Read-only mount** | A1 + A2 + B1 + C1 + C2 | Proton Drive visible in Nautilus; files openable read-only | In progress |
| **M2 — Live updates** | B4 + C3 (remote→local) | Nautilus refreshes when remote changes | ✅ Core complete — volume-level polling, anchor persistence, diff-based CREATED/DELETED, trash detection all working. `HasMoreData` paging and move/rename distinction pending |
| **M3 — Full read/write** | B3 + C1 (writes) + C3 | Create, edit, move, delete from Nautilus | Blocked on go-proton-api |
| **M4 — Settings panel** | A3-a or A3-b | Proton Drive in GNOME Settings → Online Accounts | Not started |
| **M5 — Cache + offline** | C4 | Fast repeated access; reads served from disk when offline — *read-only tier done; pinning and write-queue pending* | Partial |

---

## Open questions

1. **Session token TTL:** The session token returned by `go-proton-api` after
   SRP login has an unknown expiry. Need to determine re-auth UX: silent
   background re-auth via stored password, or a re-prompt dialog?

2. **Flatpak sandboxing:** `gvfsd` backends must run outside the Flatpak
   sandbox. Packaging strategy (system package vs. Flatpak portal) is TBD.

3. **Multi-account:** One helper process per account. Socket path:
   `/run/user/{uid}/proton-drive-{sha256(email)[:8]}.sock`.

4. **go-proton-api stability:** Pinned to a pre-1.0 pseudo-version at a
   specific commit. Policy needed: periodic manual upgrade or request a
   semver tag from upstream.

5. **MetaCache TTL vs. LRU:** The current 30 s TTL is a correctness safety net
   for missed events. Now that volume-level polling with anchor persistence is
   in place, event-driven invalidation (`InvalidatePath`) is the primary
   freshness mechanism. Replace the TTL map with a pure entry-count LRU so hot
   directories are never unnecessarily re-fetched; the offline stale fallback
   (`GetListStale` / `GetStatStale`) continues to work unchanged.
