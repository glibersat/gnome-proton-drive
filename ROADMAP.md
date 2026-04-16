# ROADMAP

## Overview

Three independent tracks that converge into a full GNOME Proton Drive integration.
Each track can be developed in parallel; the dependency arrows show what must land
before the final assembly.

```
Track A: Credentials       Track B: Go helper        Track C: GVfs backend
─────────────────────      ──────────────────────     ──────────────────────
A1. libsecret storage   →
A2. credential UI       →  B3. write operations   →  C1. backend skeleton
A3. GOA provider        →                         →  C2. volume monitor
                                                   →  C3. two-way sync
```

---

## Track A — Credential management

### A1. libsecret storage (prerequisite for everything)

Store and retrieve Proton credentials independently of GOA so the helper and
backend have a working auth path from day one.

- Use `libsecret` to store `{username, password}` (or a session token once
  Proton exposes one) in the user's keyring under a well-known schema:
  `org.gnome.proton.drive / username=<email>`
- The Go helper reads the socket path and account ID from argv; it fetches
  the credential from the keyring at startup via a small C shim or via a
  `secret-tool`-style subprocess call.
- **Why not store credentials in the helper itself:** the keyring survives
  helper restarts and is unlocked by PAM at login, matching GNOME conventions.

### A2. Standalone credential UI (short-term, no upstream dependency)

A minimal GTK4 dialog (`gnome-proton-auth`) that:

1. Prompts for email + password (or opens a Proton web-auth flow in a
   `WebKitWebView` if Proton ever exposes OAuth2).
2. Writes the credential to libsecret via the schema above.
3. Signals the GVfs volume monitor (via D-Bus or a well-known flag file) that
   a new account is available, triggering an auto-mount.

This gives a working end-to-end flow without any upstream changes and can ship
as part of this package.

**Open question:** Proton's authentication uses SRP (Secure Remote Password),
not a standard OAuth2 flow. The login challenge/response must go through the
Go helper (which already uses `go-proton-api` for SRP). The GTK dialog should
therefore delegate authentication to the helper over the RPC socket rather than
handling the password directly, so the GTK layer never holds the plaintext
password longer than one RPC round-trip.

### A3. GOA provider (long-term, proper GNOME integration)

Adding Proton as a first-class entry in GNOME Settings → Online Accounts.

**Hard constraint:** GOA providers are compiled into `gnome-online-accounts`.
There is no external plugin mechanism (`libpeas` is not used). The options are:

#### Option A3-a — Contribute upstream (recommended long-term)

Submit a `GoaProtonProvider` to the GOA project. The provider would:

- Subclass `GoaProvider` (not `GoaOAuth2Provider` — Proton uses SRP, not
  OAuth2, so the base class is more appropriate).
- Implement `build_object()` to attach `GoaFiles` (and optionally `GoaMail`,
  `GoaCalendar`) service interfaces to the account object.
- Implement `add_account()` and `refresh_account()` UI dialogs using GTK4,
  delegating the SRP challenge to a helper subprocess to avoid pulling Go
  into the GOA daemon.
- Store the session token in libsecret via GOA's standard keyring helpers.

Prerequisites for upstream acceptance:
- Proton must provide a stable, documented API (the current `go-proton-api` is
  "maintained but not actively seeking contributors" and has no write-path
  stability guarantees yet).
- The provider must not introduce new non-GNOME build dependencies into the
  GOA tree — the SRP/crypto work must stay out-of-process.

This is a multi-month process requiring upstream GNOME maintainer review.

#### Option A3-b — Patched GOA package (distribution path)

Ship a downstream `gnome-online-accounts` package with the Proton provider
patched in. Works for Flatpak-distributed settings panels or distro packages,
but creates a maintenance burden on every GOA release.

Only recommended as a bridge until Option A3-a lands.

#### Option A3-c — GOA D-Bus impersonation (not recommended)

Register a D-Bus service that mimics the GOA account D-Bus interface for a
Proton account, making it appear to GVfs as a GOA-managed account without
touching the GOA source. Technically possible but fragile: any GOA API change
breaks it silently, and it violates the intended contract.

**Decision:** Implement A2 first for a self-contained working product. Pursue
A3-a in parallel as a separate upstream contribution once the integration is
stable.

---

## Track B — Go helper completion

### B1. Path resolution cache ✅ (partial — in-memory only)

Current state: `resolvePath` walks the tree on every call. Add an in-memory
`linkID` cache keyed by path, invalidated by Drive events (see B4).

### B2. Streaming block reads

Current `ReadFile` buffers the entire file before applying offset/length.
Replace with a streaming approach: fetch and decrypt blocks on demand, seeking
by skipping blocks whose byte range falls entirely before `offset`.

### B3. Write operations

Three sub-tasks, each blocked on `go-proton-api` upstream:

| Sub-task | Blocker |
|---|---|
| `Mkdir` | Need crypto helpers to generate `NodeKey` + `NodePassphrase` for a new folder node. `go-proton-api` exposes `CreateFolder` but not the key-generation step. |
| `WriteFile` | Need block encryption + `RequestBlockUpload` + `UpdateRevision` flow. The API calls exist; the crypto wrapper (generate session key, encrypt blocks, sign manifest) does not yet exist in the library. |
| `Move` / `Rename` | `go-proton-api` has no `MoveLink` call at all. Needs either an upstream addition or a raw HTTP implementation against the `/drive/shares/{id}/files/{id}/move` endpoint. |

Track these against `go-proton-api` releases. When unblocked, implement
in `drive/session.go` and register the `WriteFile` handler in `main.go`.

### B4. Event polling for two-way sync

Proton Drive exposes a `/drive/shares/{id}/events/{eventID}` endpoint. Poll
it on a configurable interval (default 30 s) and emit invalidation signals
over a second Unix socket or over the same RPC connection as server-push
events:

```json
{"event": "changed", "path": "/Documents/file.txt"}
{"event": "deleted", "path": "/old-name.txt"}
{"event": "created", "path": "/new-folder"}
```

The GVfs backend consumes these to call `g_vfs_monitor_emit_event()`,
triggering Nautilus refreshes without polling from the C side.

---

## Track C — GVfs backend

### C1. Backend skeleton

Implement `gvfsd-proton` in C, following the pattern of `gvfsd-sftp` or
`gvfsd-google`:

- Subclass `GVfsBackend`.
- On `mount`, spawn `proton-drive-helper` with a per-mount socket path,
  send `Auth` with credentials fetched from libsecret (A1), and verify the
  connection.
- Implement the mandatory VFS ops by translating them to RPC calls:
  `open_for_read`, `read`, `close_read`, `query_info`, `enumerate`.
- Stub write ops (`open_for_write`, `write`, `close_write`, `make_directory`,
  `set_display_name`, `delete`) with `G_IO_ERROR_NOT_SUPPORTED` until B3 lands.

### C2. Volume monitor

A small `gvfsd-proton-volume-monitor` daemon (or integrated into the backend):

- Watches libsecret for Proton credentials (A1) or GOA account additions (A3).
- Calls `g_volume_monitor_adopt_orphan_mount()` / emits `volume-added` on
  `GVfsVolumeMonitor` to make the drive appear in Nautilus.
- Provides icon (`proton-drive` symbolic), display name, and UUID derived from
  the Proton account email.

### C3. Two-way sync

- **Remote → local:** consume events from B4; call
  `g_file_monitor_emit_event()` on affected paths so Nautilus and open file
  handles see changes without re-stating.
- **Local → remote:** all VFS write ops (C1 write stubs, once B3 is done) go
  directly to the API via the helper — there is no separate sync step for
  writes. The GVfs backend is the sync layer.
- **Conflict policy:** last-write-wins for now (matches Proton Drive web
  client behaviour). Document as a known limitation; revisit if revision
  history API becomes accessible.

---

## Milestones

| Milestone | Tracks | Deliverable |
|---|---|---|
| **M1 — Read-only mount** | A1 + A2 + B1 + C1 + C2 | Proton Drive visible in Nautilus; files openable read-only |
| **M2 — Live updates** | B4 + C3 (remote→local) | Nautilus refreshes when remote changes |
| **M3 — Full read/write** | B3 + C1 (writes) + C3 | Create, edit, move, delete from Nautilus |
| **M4 — Settings panel** | A3-a | Proton Drive in GNOME Settings → Online Accounts |

---

## Open questions

1. **SRP vs OAuth2 in GOA:** GOA's credential refresh machinery is designed
   around OAuth2 token refresh. Proton's SRP-based auth does not map cleanly
   onto this. The session token returned by `go-proton-api` after login has an
   unknown TTL. Need to determine: does the token expire, and if so, what is
   the re-auth UX?

2. **Flatpak sandboxing:** `gvfsd` backends must run outside the Flatpak
   sandbox. Packaging strategy (system package vs. Flatpak portal) is TBD.

3. **Multi-account:** The volume monitor and helper should support multiple
   Proton accounts simultaneously (one helper process per account). Design the
   socket path as `/run/user/{uid}/proton-drive-{account-hash}.sock`.

4. **go-proton-api stability:** The library is at a pre-1.0 pseudo-version
   pinned to a specific commit. Establish a policy for tracking upstream
   (pin + periodic manual upgrade, or contribute a `go.mod` tag request).
