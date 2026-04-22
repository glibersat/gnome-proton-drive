# RPC protocol reference

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

## Methods

| Method | Params | Description |
|---|---|---|
| `Auth` | `{username, password}` | Authenticate and unlock the Drive keyring |
| `ResumeSession` | `{username?, uid, refresh_token, salted_passphrase}` | Restore session from stored credentials |
| `ListDir` | `{path}` | List active children of a directory |
| `Stat` | `{path}` | Get metadata for a file or directory |
| `ReadFile` | `{path, offset, length}` | Read file content (decrypted) |
| `FetchThumbnail` | `{link_id, revision_id}` | Fetch and cache a server-side thumbnail; returns local path |
| `Mkdir` | `{path}` | Create a directory *(blocked — see limitations)* |
| `Delete` | `{path, trash}` | Delete or trash a file/directory |
| `Move` | `{src, dst}` | Move or rename *(blocked — see limitations)* |

## Error codes

| Code | Meaning |
|---|---|
| `-32603` | Internal error |
| `-32602` | Invalid arguments |
| `-32001` | Not found |
| `-32002` | Authentication failed |
| `-32003` | Not authenticated |
| `-32004` | Human verification required (CAPTCHA) |
| `-32005` | Offline — network unreachable and no cached data available |
