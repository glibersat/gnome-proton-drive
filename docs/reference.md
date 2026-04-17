# Windows Client Reference

Notes extracted from the official Windows Proton Drive client
(`windows-drive/`) as a cross-reference for the Linux implementation.
All file paths are relative to `windows-drive/src/`.

---

## Project layout

| Project | Role |
|---|---|
| `ProtonDrive.Client` | HTTP API client, crypto primitives, event polling |
| `ProtonDrive.Sync.Adapter` | Bridges API events → sync engine; update detection |
| `ProtonDrive.Sync.Engine` | Reconciliation, conflict resolution, propagation |
| `ProtonDrive.App` | Application services: mapping, volumes, auth |

---

## Event polling (ROADMAP §B4)

### API surface

Two endpoints exist. The Windows client uses the **volume-level** endpoint
for its main drive and falls back to the **share-level** endpoint for older
mounts.

```
GET /drive/volumes/{volumeId}/events/{anchorId}   # preferred
GET /drive/shares/{shareId}/events/{anchorId}     # legacy
```

Both return `EventListResponse`:

```csharp
// ProtonDrive.Client/Contracts/EventListResponse.cs
public sealed record EventListResponse : ApiResponse {
    [JsonPropertyName("EventID")]
    public string? AnchorId { get; init; }   // new cursor to pass next call
    public IImmutableList<EventListItem> Events { get; init; }
    [JsonPropertyName("More")]
    public bool HasMoreData { get; init; }   // paginate immediately if true
    [JsonPropertyName("Refresh")]
    public bool RequiresRefresh { get; init; } // full tree rescan needed
}
```

**Our Go implementation** uses `GetShareEvent` (share-level). We should
migrate to the volume-level endpoint (`GetShareEvent` →
`GetVolumeEvent` or equivalent) once go-proton-api exposes it.

### Resume token / anchor

`DriveEventResumeToken` wraps the `anchorId` string plus two flags:

```csharp
// ProtonDrive.Client/Events/DriveEventResumeToken.cs
class DriveEventResumeToken {
    string? AnchorId;      // EventID returned by last successful call
    bool HasMoreData;      // caller must immediately page for more
    bool IsRefreshRequired;// full resync triggered; send Skipped entry
}
```

On first start (empty persisted anchor): call `GetLatestEvent` to obtain
the current head, store it, and mark `IsRefreshRequired = true`. This
is exactly what our `GetLatestShareEventID` anchor fetch does in
`events.go:Start()`.

When `anchorId` is lost or invalid (API returns `InvalidEncryptedIdFormat`),
fall back to `GetLatestEvent` again. We should add this fallback — our
poller currently just logs and retains the stale anchor.

### Polling loop

`RemoteEventLogClient` (`ProtonDrive.Client/RemoteEventLogClient.cs`) is
the canonical implementation:

1. Timer fires at `pollInterval` (configurable; default varies by feature flag).
2. If within `throttleInterval` of last poll, skip.
3. Call `GetEvents` → `ProcessEventsAsync`.
4. If `HasMoreData`, loop immediately (drain the queue in one shot).
5. Persist anchor via `IRepository<string>` after each successful batch.
6. Emit `LogEntriesReceived` event with a commit callback — callers only
   advance the anchor when they have processed the entries.

**Key difference from our implementation:** the Windows client persists the
anchor to disk so polling resumes from the correct position after restart.
Our `EventPoller` fetches the anchor from scratch on every `Start()` call,
which means we may miss events that occurred while the helper was down.
Consider persisting the anchor to `~/.cache/proton-drive/<account>/anchor`.

### Event types

```csharp
// ProtonDrive.Client/Contracts/EventType.cs
enum EventType {
    Delete = 0,         // link garbage-collected or moved out of share
    Create = 1,         // link created, or first revision committed
    Update = 2,         // file content updated (new revision)
    UpdateMetadata = 3, // name, parent, shares, state (active/trashed) changed
}
```

`UpdateMetadata` covers **renames and moves** — the link's `ParentLinkID`
and encrypted `Name` change. We currently map both `Update` and
`UpdateMetadata` to `EventChanged`, which is correct for cache
invalidation but loses the move semantics for future write support (B3).

### Per-event processing

For each `EventListItem`:

- **Delete** (`EventType.Delete`): only `Link.Id` is reliable; emit
  `Deleted` with the linkID. The Windows client does the same:
  ```csharp
  entries.Add(new EventLogEntry<string>(EventLogChangeType.Deleted) { Id = item.Link.Id });
  ```
- **Create / Update / UpdateMetadata**: call
  `_remoteNodeService.GetRemoteNodeAsync(shareId, item.Link)` — this
  fetches and decrypts the full `Link` to get the plaintext name and
  parent chain. The decrypted name + parentId allows the sync engine to
  place the node in the tree.

  **Implication for us:** after receiving a Create event we call
  `meta.invalidateLinkID` / `meta.InvalidatePath` but do **not** yet know
  the plaintext name. We cannot populate `linkPaths` for the new node until
  it is fetched by a subsequent `resolvePath` call. This is acceptable for
  our read-only use case.

- **Crypto failure** during node fetch → emit `EventLogChangeType.Error`
  and stop processing the current batch (return false). Our poller should
  do the same rather than silently dropping the event.

---

## Crypto chain (ROADMAP §B3)

### Key hierarchy

```
User address key  (RSA/Ed25519, stored in account)
  └─ Share key passphrase  (encrypted with address key, signed)
       └─ NodeKey  (per link; Ed25519 + X25519)
            └─ NodePassphrase  (random 32 bytes, base64; encrypted with parent NodeKey, signed)
                 └─ SessionKey  (AES-256; encrypted into ContentKeyPacket with NodeKey)
                      └─ Block data packets  (PGP symmetrically encrypted with SessionKey)
```

All strings stored on the server are PGP-armored ciphertext. Signatures
are detached and stored separately (`NodePassphraseSignature`,
`ContentKeyPacketSignature`, per-block `Signature`).

### Creating a node (Mkdir / WriteFile)

`CryptographyService.GenerateShareOrNodeKey()` generates a fresh Ed25519 +
X25519 key pair. The node creation flow:

1. `GeneratePassphrase()` → 32 random bytes → base64 → `NodePassphrase` plaintext
2. Encrypt passphrase with parent's `NodeKey.PublicKey`, sign with address key
3. `NodeKey` private key is PGP-encrypted with the passphrase (self-encrypted)
4. Compute `NameHash = HMAC-SHA256(hashKey, name_utf8)` (hex) for server-side
   deduplication / collision detection

```csharp
// ProtonDrive.Client/Contracts/NodeCreationParameters.cs
class NodeCreationParameters {
    string Name;               // encrypted with parent NodeKey
    string ParentLinkId;
    string NameHash;           // HMAC-SHA256(hashKey, plaintext_name) hex
    string NodePassphrase;     // encrypted + signed
    string NodePassphraseSignature;
    string SignatureEmailAddress;
    string NodeKey;            // armored private key, self-encrypted
}
```

**go-proton-api gap (B3 blocker):** The library does not expose the key
generation step. The raw crypto operations above must be added to the Go
helper or contributed upstream before `Mkdir` can be implemented.

### Moving / renaming a link

`PUT /shares/{shareId}/links/{linkId}/move` with `MoveLinkParameters`:

```csharp
// ProtonDrive.Client/MoveLinkParameters.cs
class MoveLinkParameters {
    string ParentLinkId;       // new parent
    string NodePassphrase;     // re-encrypted with new parent's NodeKey
    string? NodePassphraseSignature; // required for anonymously-created nodes
    string Name;               // re-encrypted with new parent's NodeKey
    string NameHash;           // recomputed HMAC with new parent's hashKey
    string NameSignatureEmailAddress;
    string? OriginalHash;      // old hash — prevents race conditions
}
```

The passphrase must be re-encrypted under the new parent because the
NodePassphrase was originally encrypted with the old parent's NodeKey.
**go-proton-api gap (B3 blocker):** `MoveLink` is not implemented.

### Writing a file

1. `POST /shares/{shareId}/files` with `FileCreationParameters` (extends
   `NodeCreationParameters`, adds `ContentKeyPacket`, `MIMEType`,
   `ClientUID`).
2. `POST /shares/{shareId}/files/{linkId}/revisions` to open a revision.
3. For each block (default 4 MiB — `Constants.FileBlockSize`):
   a. Encrypt block with `SessionKey` (PGP data packet).
   b. Sign block content; attach signature.
   c. SHA-256 hash encrypted block for integrity.
   d. Batch `POST /blocks` to request upload URLs (up to 3 in parallel).
   e. `POST` encrypted block to the returned URL (multipart form).
4. Commit revision (update `ActiveRevision`).

The pipeline is implemented in
`ProtonDrive.Client/FileUploading/RemoteFileWriteStream.cs` as a
TPL Dataflow pipeline with three stages: encrypt → batch request URLs →
upload. Max parallelism: 3 blocks concurrent.

---

## Sync engine (ROADMAP §C3)

### Update detection: log-based vs. state-based

The Windows client uses two complementary strategies:

| Strategy | When used | How |
|---|---|---|
| **Log-based** | Normal operation | Event polling (`RemoteEventLogClient`) drives `LogBasedUpdateDetection` |
| **State-based** | After `Refresh=true` or on startup | Full tree enumeration; compares against persisted state |

`LogBasedUpdateDetection` (`ProtonDrive.Sync.Adapter/UpdateDetection/LogBased/LogBasedUpdateDetection.cs`):
- Subscribes to `IEventLogClient.LogEntriesReceived`.
- Enqueues batches in a `ConcurrentQueue`.
- Dispatches via `CoalescingAction` (merges redundant triggers).
- Feeds each entry into `IdentityBasedEventLogProcessingStep` which maps
  remote linkIDs to local adapter-tree nodes and marks them dirty.

### Conflict resolution

`EditConflictResolutionPipeline` (`ProtonDrive.Sync.Engine/ConflictResolution/EditConflictResolutionPipeline.cs`):

> **Remote replica always wins.** The local file is backed up with a suffix
> before being overwritten.

This matches the ROADMAP §C3 "last-write-wins" policy. The Windows client
creates a `_conflict` copy of the local file rather than silently
discarding it.

Other pipelines: `DeleteConflictResolutionPipeline`,
`MoveConflictResolutionPipeline`, `NameClashConflictResolutionPipeline`.

### EventLogChangeType mapping

```csharp
// RemoteEventLogClient.cs — ToChangeType()
EventType.Create         → EventLogChangeType.CreatedOrMovedTo
EventType.Update         → EventLogChangeType.Changed
EventType.UpdateMetadata → EventLogChangeType.ChangedOrMoved   // covers renames
EventType.Delete         → EventLogChangeType.DeletedOrMovedFrom
```

`UpdateMetadata` maps to `ChangedOrMoved` because a metadata change may
be a rename/move; the sync engine resolves which by comparing old vs. new
`ParentLinkID` and `Name`.

---

## API endpoints summary

| Endpoint | Method | Purpose |
|---|---|---|
| `/drive/volumes/{id}/events/{anchorId}` | GET | Volume-level event stream (preferred) |
| `/drive/shares/{id}/events/{anchorId}` | GET | Share-level event stream (legacy) |
| `/drive/shares/{id}/folders/{id}/children` | GET | Enumerate directory |
| `/drive/shares/{id}/folders` | POST | Create folder |
| `/drive/shares/{id}/files` | POST | Create file (opens draft) |
| `/drive/shares/{id}/files/{id}/revisions` | POST | Open new revision |
| `/blocks` | POST | Request block upload URLs |
| `/drive/shares/{id}/links/{id}/move` | PUT | Move or rename |
| `/drive/shares/{id}/links/{id}/rename` | PUT | Rename only |
| `/drive/shares/{id}/folders/{id}/trash_multiple` | POST | Trash children |

---

## Action items for the Linux implementation

| Item | ROADMAP | Notes |
|---|---|---|
| Migrate event polling to volume endpoint | B4 | Volume events include `ContextShareID` per item |
| Persist event anchor to disk | B4 | Survive helper restarts; path: `~/.cache/proton-drive/<account>/anchor` |
| Handle `HasMoreData` — drain immediately | B4 | Current poller waits for next 30 s tick |
| Fallback to `GetLatestEvent` on invalid anchor | B4 | Currently logs error and retains stale anchor |
| Implement `GenerateShareOrNodeKey` + passphrase encrypt | B3 | Prerequisite for Mkdir |
| Implement `MoveLink` / `RenameLinkParameters` | B3 | API exists; missing in go-proton-api |
| Block upload pipeline (4 MiB blocks, 3 concurrent) | B3 | See `RemoteFileWriteStream` |
| Conflict resolution: backup local file on edit-edit | C3 | Windows backs up with `_conflict` suffix |
| Map `UpdateMetadata` → move detection | C3 | Check `ParentLinkID` delta |
