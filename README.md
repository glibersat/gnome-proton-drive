# gnome-proton

A native GNOME integration for Proton Drive, exposing it as a mounted volume with two-way sync.

## Architecture

```
gvfsd-proton (C, GVfs backend)       ← to be implemented
    ↕ Unix socket (line-delimited JSON-RPC)
proton-drive-helper (Go binary)       ← this repo
    ↕ HTTPS + E2E encryption
Proton Drive API
```

The Go helper owns authentication, key management, and all Proton Drive API calls using [go-proton-api](https://github.com/ProtonMail/go-proton-api). The C GVfs backend (not yet implemented) will translate GVfs VFS operations into RPC calls to the helper, making Proton Drive appear as a native volume in Nautilus and all GTK file choosers.

## Helper binary (`helper/`)

### RPC protocol

Line-delimited JSON over a Unix socket. Each request and response is a single JSON object terminated by `\n`.

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
| `Mkdir` | `{path}` | Create a directory *(not yet implemented)* |
| `Delete` | `{path, trash}` | Delete or trash a file/directory |
| `Move` | `{src, dst}` | Move or rename *(not yet implemented)* |

### Error codes

| Code | Meaning |
|---|---|
| `-32603` | Internal error |
| `-32602` | Invalid arguments |
| `-32001` | Not found |
| `-32002` | Authentication failed |
| `-32003` | Not authenticated |

### Build

Requires Go 1.26+.

```sh
cd helper
go build -o proton-drive-helper .
```

### Run

```sh
./proton-drive-helper --socket /run/user/1000/proton-drive.sock
```

Then send an `Auth` request before any other method.

## Status

| Feature | Status |
|---|---|
| Authentication + key unlock | ✅ |
| List directory | ✅ |
| Stat file/directory | ✅ |
| Read file (decrypted) | ✅ |
| Delete / trash | ✅ |
| Create directory | ⏳ Pending key generation helpers in go-proton-api |
| Move / rename | ⏳ MoveLink not yet in go-proton-api |
| Write file | ⏳ Block upload + revision creation |
| GVfs C backend | 🔲 Not started |
| GNOME volume monitor | 🔲 Not started |
| Two-way sync / event polling | 🔲 Not started |

## Known limitations

- `Mkdir` and `Move` are stubbed — `go-proton-api` does not yet expose the crypto helpers needed to generate a new node's `NodeKey`/`NodePassphrase` pair, nor a `MoveLink` endpoint.
- Path resolution walks the tree on every call (no persistent cache). A production implementation should maintain a linkID cache invalidated by Drive events.
- `ReadFile` buffers the entire file in memory before applying the offset/length window. Streaming block decryption is needed for large files.
