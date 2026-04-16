# gnome-proton

A native GNOME integration for Proton Drive, exposing it as a mounted volume with two-way sync.

## Architecture

```
Nautilus / GTK file choosers
        ↕ GVfs VFS ops
gvfsd-proton  (C, GVfs backend)
        ↕ Unix socket — line-delimited JSON-RPC
proton-drive-helper  (Go binary)
        ↕ HTTPS + E2E encryption
Proton Drive API
```

The Go helper owns authentication, key management, and all Proton Drive API calls using [go-proton-api](https://github.com/ProtonMail/go-proton-api). `gvfsd-proton` translates GVfs operations into RPC calls, making the drive appear as a native volume. The backend spawns the helper automatically on mount.

## Building

### Prerequisites

- Go 1.22+
- GLib/GIO 2.76+, json-glib 1.0 (headers + dev packages)
- GVfs 1.57 (runtime libraries: `libgvfsdaemon.so`, `libgvfscommon.so`)
- Meson 1.0+ and Ninja

### Go helper

```sh
cd helper
go build -o proton-drive-helper .
```

### C backend

```sh
cd backend
meson setup build --prefix=/usr/local
ninja -C build
meson test -C build    # runs the RPC unit tests
```

## Installation

```sh
# helper binary
sudo cp helper/proton-drive-helper /usr/local/libexec/

# backend binary + mount descriptor
cd backend && sudo meson install -C build
# installs: /usr/local/libexec/gvfsd-proton
#           /usr/local/share/gvfs/mounts/proton.mount
```

After installing, restart `gvfsd` so it picks up the new mount type:

```sh
pkill gvfsd; gvfsd &    # or log out and back in
```

## First mount (read-only)

**1. Store your credentials in the GNOME keyring:**

```sh
secret-tool store --label="Proton Drive" \
  schema org.gnome.proton.drive \
  username you@proton.me
# enter your Proton password at the prompt
```

**2. Mount the drive:**

```sh
gvfs-mount "proton:///?account=you@proton.me&username=you@proton.me"
```

`gvfsd-proton` will spawn `proton-drive-helper`, wait for the socket, authenticate, and the volume will appear in Nautilus as **Proton Drive**. Opening files and browsing directories works read-only at this stage.

**To unmount:**

```sh
gvfs-mount -u "proton:///?account=you@proton.me&username=you@proton.me"
```

## RPC protocol reference

Line-delimited JSON over a Unix socket (`/run/user/<uid>/proton-drive-<account>.sock`). Each request and response is one JSON object terminated by `\n`.

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

## Status

| Feature | Status |
|---|---|
| Authentication + key unlock | ✅ |
| List directory | ✅ |
| Stat file/directory | ✅ |
| Read file (decrypted) | ✅ |
| Delete / trash | ✅ |
| GVfs C backend (read-only) | ✅ |
| Create directory | ⏳ Pending crypto helpers in go-proton-api |
| Move / rename | ⏳ `MoveLink` not yet in go-proton-api |
| Write file | ⏳ Block upload + revision creation |
| GNOME volume monitor | 🔲 Not started |
| Two-way sync / event polling | 🔲 Not started |
| Credential setup wizard (GTK) | 🔲 Not started |
| GNOME Online Accounts integration | 🔲 Future (post-M3) |

## Known limitations

- **Read-only.** Write operations (`Mkdir`, `Move`, `WriteFile`) are stubbed pending `go-proton-api` additions: key generation for new nodes, `MoveLink`, and block upload helpers.
- **Manual credential setup.** Credentials must be stored with `secret-tool` for now. A GTK setup wizard is planned (track A2 in ROADMAP.md).
- **Path resolution is uncached.** The helper walks the tree on every call. A production implementation should keep a linkID cache invalidated by Drive events.
- **`ReadFile` buffers in memory.** The entire block set is decrypted before slicing to the requested window. Streaming decryption is needed for large files.
