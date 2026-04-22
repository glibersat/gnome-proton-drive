# Conflict handling for write operations

## Context

This project is a **GVfs filesystem backend**, not a sync engine. There is no
local mirror of the remote tree and no reconciliation loop. Write operations
are user-initiated (via Nautilus or other GIO clients) and are applied directly
to the Proton Drive API. This fundamentally limits the conflict surface compared
to the Windows client.

---

## Name clash on directory creation (Mkdir)

When the user creates a directory whose name already exists in the parent, the
API returns `AlreadyExists` (Proton API code 2500). The helper maps this to RPC
error `-32006 ErrAlreadyExists`, which the C backend translates to
`G_IO_ERROR_EXISTS`. The file manager receives the error and is responsible for
any user-facing resolution (e.g. prompting to rename).

**No silent rename.** The Windows client renames the losing node to
`"name (1)"` etc., but that is a sync engine concern — the Windows client owns
a local file tree that must stay consistent. Our backend does not have local
state to rename; renaming is the file manager's job.

### Race condition: remote creates same name while local Mkdir is in flight

Between the point where the user (or helper) resolves the parent path and the
`POST /shares/{id}/folders` API call landing, a remote event could create a
link with the same name. The API still returns `AlreadyExists` in this case.
The helper returns the error unchanged; the file manager retries or reports it
to the user.

After any `AlreadyExists` error the helper invalidates the parent path in
`MetaCache` so the next `ListDir` reflects the remote state.

---

## Name clash on file creation (WriteFile)

Same as Mkdir: API returns `AlreadyExists` → `ErrAlreadyExists` → `G_IO_ERROR_EXISTS`.

---

## Write after stale cache

The helper may believe a name does not exist because the MetaCache has a stale
listing (e.g. the event poller missed a remote create during an outage). The
`POST` will still fail with `AlreadyExists`. The error path invalidates the
parent and surfaces the error to GVfs — no silent data loss.

---

## Concurrent edits to the same file (EditEdit)

Not yet applicable — `WriteFile` is not implemented. When it is: the Proton
API supports creating a new revision on an existing link. If two writers race,
both revisions are created; the server's `ActiveRevision` is the last one
committed. This matches the Windows client "remote wins" policy. No conflict
backup is needed at the GVfs layer because the backend does not hold a local
copy of the file content.

---

## Move / rename conflicts

`MoveLink` is not yet in `go-proton-api` (B3 blocker). When implemented:

- **MoveMoveDest** (two nodes moved to same destination): API `AlreadyExists` →
  `ErrAlreadyExists` → `G_IO_ERROR_EXISTS`.
- **MoveMoveSource** (same node moved from two places): second `MoveLink` call
  will fail with `NotFound` (source already moved) → `ErrNotFound` →
  `G_IO_ERROR_NOT_FOUND`.
- No local state to reconcile in either case.

---

## Summary table

| Scenario | API response | RPC error | GVfs error |
|---|---|---|---|
| Name already exists (Mkdir / WriteFile) | `AlreadyExists` (2500) | `-32006 ErrAlreadyExists` | `G_IO_ERROR_EXISTS` |
| Parent not found | `NotFound` (2501) | `-32001 ErrNotFound` | `G_IO_ERROR_NOT_FOUND` |
| Network outage during write | timeout / offline | `-32005 ErrOffline` | `G_IO_ERROR_HOST_UNREACHABLE` |
| Any other API error | varies | `-32603 ErrInternal` | `G_IO_ERROR_FAILED` |

Post-error, the helper always invalidates the affected path(s) in `MetaCache`
so subsequent reads reflect the true remote state.
