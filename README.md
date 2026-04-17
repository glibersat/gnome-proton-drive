# Gnome Proton Drive

> **Alpha — read-only.** This project is in early development. Only browsing
> and opening files is supported; no writes, moves, or deletes are exposed to
> the file manager yet. Expect rough edges and breaking changes between
> commits.

A native GNOME integration for Proton Drive, exposing it as a mounted volume
in Nautilus and GTK file choosers.

## Architecture

```
Nautilus / GTK file choosers
        ↕ GVfs VFS ops
gvfsd-proton  (C, GVfs backend)
        ↕ Unix socket — line-delimited JSON-RPC
proton-drive-helper  (Go binary)
        ├── EventPoller (volume events, 30 s poll)
        ├── MetaCache   (in-memory, event-invalidated)
        ├── BlockCache  (~/.cache/proton-drive/)
        └── ↕ HTTPS + E2E encryption
Proton Drive API
```

The Go helper owns authentication, key management, and all Proton Drive API
calls using [go-proton-api](https://github.com/ProtonMail/go-proton-api).
`gvfsd-proton` translates GVfs operations into RPC calls, making the drive
appear as a native volume. The backend spawns the helper automatically on
mount.

Directory listings and file metadata are cached in-process, invalidated by
the event poller when remote changes arrive. Decrypted file content is cached
on disk under `~/.cache/proton-drive/<account>/` so repeated reads and offline
access work without hitting the network. See `docs/caching.md` for details.

## Building

### Prerequisites

- Go 1.22+
- GLib/GIO 2.76+, json-glib 1.0 (headers + dev packages)
- GVfs 1.57 (runtime libraries: `libgvfsdaemon.so`, `libgvfscommon.so`)
- Meson 1.0+ and Ninja

### Build everything

```sh
make
```

### Build components individually

```sh
# Go helper only
make build-helper

# C backend only (configures Meson into _build/ on first run)
make build-backend
```

## Installation

```sh
sudo make install
```

This installs:
- `proton-drive-helper` → `/usr/local/libexec/`
- `gvfsd-proton` → GVfs backend directory (via `meson install`)
- `proton.mount` → GVfs mounts directory
- `proton-drive-setup` → `/usr/local/bin/`

Override `PREFIX` or `DESTDIR` as needed:

```sh
sudo make install PREFIX=/usr
make install DESTDIR=/tmp/pkg PREFIX=/usr
```

After installing, restart `gvfsd` so it picks up the new mount type:

```sh
pkill gvfsd; gvfsd &    # or log out and back in
```

## First mount

**1. Run the setup wizard:**

```sh
proton-drive-setup
```

This opens a GTK dialog asking for your email and password, performs SRP
login via `proton-drive-helper`, and stores the session tokens (`uid`,
`refresh_token`, `salted_passphrase`) in the GNOME keyring. The password is
never written to disk.

**2. Mount the drive:**

```sh
gio mount "proton://you%40proton.me/"
```

The `@` in the email must be percent-encoded as `%40` so GVfs passes it
through as the host field.

`gvfsd-proton` will spawn `proton-drive-helper`, wait for the socket,
authenticate, and the volume will appear in Nautilus as
**Proton Drive (you@proton.me)**. Browsing directories and opening files
works read-only at this stage.

**3. Unmount:**

```sh
gio mount -u "proton://you%40proton.me/"
```

## Testing

```sh
make test          # Go unit tests (with -race) + Meson backend tests
make test-helper   # Go only
make test-backend  # C backend only
```

## RPC protocol reference

Line-delimited JSON over a Unix socket
(`/run/user/<uid>/proton-drive-<account>.sock`). Each request and response is
one JSON object terminated by `\n`.

**Request:**
```json
{"id": 1, "method": "ListDir", "params": {"path": "/Documents"}}
```

**Response:**
```json
{"id": 1, "result": {"entries": [{"name": "file.txt", "is_dir": false, "size": 1234, "mtime": 1713200000}]}}
```

**Error response:**
```json
{"id": 1, "error": {"code": -32001, "message": "not found: /Documents/missing"}}
```

### Methods

| Method | Params | Description |
|---|---|---|
| `Auth` | `{username, password}` | Authenticate and unlock the Drive keyring |
| `ResumeSession` | `{username?, uid, refresh_token, salted_passphrase}` | Restore session from stored credentials |
| `ListDir` | `{path}` | List active children of a directory |
| `Stat` | `{path}` | Get metadata for a file or directory |
| `ReadFile` | `{path, offset, length}` | Read file content (decrypted) |
| `Mkdir` | `{path}` | Create a directory *(blocked — see limitations)* |
| `Delete` | `{path, trash}` | Delete or trash a file/directory |
| `Move` | `{src, dst}` | Move or rename *(blocked — see limitations)* |

### Error codes

| Code | Meaning |
|---|---|
| `-32603` | Internal error |
| `-32602` | Invalid arguments |
| `-32001` | Not found |
| `-32002` | Authentication failed |
| `-32003` | Not authenticated |
| `-32004` | Human verification required (CAPTCHA) |
| `-32005` | Offline — network unreachable and no cached data available |

## Status

| Feature | Status |
|---|---|
| Authentication + key unlock | ✅ |
| List directory | ✅ |
| Stat file/directory | ✅ |
| Read file (streaming, offset-aware, decrypted) | ✅ |
| Metadata cache (event-invalidated, offline fallback) | ✅ |
| Block cache (per-block, persistent, 2 GiB LRU, offline reads) | ✅ |
| GVfs C backend (read-only) | ✅ |
| Delete / trash | ✅ (helper only — not exposed via GVfs yet) |
| Create directory | ⏳ Pending crypto helpers in go-proton-api |
| Move / rename | ⏳ `MoveLink` not yet in go-proton-api |
| Write file | ⏳ Block upload + revision creation |
| GNOME volume monitor | 🔲 Not started |
| Event polling (remote → Nautilus) | ✅ Volume-level, anchor-persisted; `HasMoreData` paging pending |
| Block cache re-encryption | 🔲 Currently stored as plaintext |
| Pinned offline files | 🔲 Not started |
| GNOME Online Accounts integration | 🔲 Future (post-M3) |

## Known limitations

- **Read-only.** Write operations (`Mkdir`, `Move`, `WriteFile`) are stubbed
  pending `go-proton-api` additions: key generation for new nodes, `MoveLink`,
  and block upload helpers.
- **No volume monitor.** The drive must be mounted manually with `gio mount`.
  A GVfs volume monitor (making it appear automatically in Nautilus) is
  planned for M1.
- **Block cache stores plaintext.** Decrypted file content is written to
  `~/.cache/proton-drive/` without re-encryption. Rely on OS full-disk
  encryption until this is addressed.
- **Session token TTL unknown.** Re-authentication UX (silent refresh vs.
  re-prompt dialog) is not yet determined.
