# pb-offline-sync Protocol Specification

**Protocol version:** 1 · **Status:** Frozen (v1.0) · **Companion to:** `pb-offline-sync-PDD.md`

This document is the source of truth for the wire protocol between a sync **client** (local database + outbox) and a sync **server** (PocketBase with the pb-offline-sync plugin). Server and client implementations MUST be able to interoperate using only this document and the fixtures in `conformance/`.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY** are to be interpreted as in RFC 2119. This document was frozen at protocol **v1.0**; the former `[OPEN]` items are resolved in §17. **v1.1** adds media sync as a purely additive extension (§18); the wire version stays `1`.

---

## 1. Scope and terminology

**In scope (v1):** two-way sync of per-user owned records, field-level patches, optimistic concurrency via `rev`, idempotent push, checkpointed pull, conflict reporting, deletes.

**Out of scope (v1):** shared/multi-user records, partial/filtered sync, file content sync, operation mutations (counters), CRDTs.

| Term | Meaning |
|---|---|
| **Synced collection** | A PocketBase collection enabled for sync in server config. |
| **Owner** | The authenticated user that owns a record, stored in the collection's configured owner field. |
| **Record** | A row in a synced collection. |
| **rev** | Server-maintained positive integer on each record, incremented on every committed write. |
| **baseRev** | The `rev` of the record state a client's edit was based on. |
| **Mutation** | A single create/update/delete request from a client, identified by a client-generated mutation ID. |
| **Change log** | Server-side ordered log of committed changes, each with a monotonically increasing sequence number (`seq`). |
| **Cursor** | An opaque token representing a client's position in the change log. |
| **Outbox** | Client-side ordered queue of pending mutations. |

## 2. Transport, encoding, versioning

- All endpoints are served over HTTPS under the base path `/api/sync`. (Plain HTTP MAY be used for local development only.)
- Request and response bodies are JSON, UTF-8, `Content-Type: application/json`.
- Authentication uses a standard PocketBase auth token in the `Authorization` header. All sync endpoints require authentication.
- Every request MUST include the header `X-Sync-Protocol: 1`. A server that does not support the requested version MUST respond `400` with error code `protocol_unsupported` and SHOULD list supported versions in `supported` (an array of integers).
- **Extensibility:** receivers MUST ignore unknown JSON properties. Additive changes (new optional properties, new error codes, new capabilities) do not bump the protocol version. Changes to existing semantics or required properties do.
- A client that receives an unknown mutation `status` MUST treat the mutation as failed (not applied), keep it in the outbox flagged as errored, and surface it to the application. It MUST NOT silently discard it.

## 3. Identifiers

- **Record ID:** MUST match `^[a-z0-9]{15}$` (PocketBase's default ID format). Clients generate IDs for new records.
- **Mutation ID:** a string matching `^[A-Za-z0-9_-]{1,64}$`. Clients SHOULD use UUIDs. A mutation ID MUST be unique per authenticated user for the lifetime of the server's idempotency retention window (§9).
- **Collection name:** the PocketBase collection name.
- **Deleted IDs are not reusable.** Clients MUST NOT create a record with an ID that was previously deleted. Servers SHOULD reject such a create with `id_unavailable` for as long as their change history still shows the deletion.

## 4. Record wire format

Records on the wire use the **same JSON representation as PocketBase's REST API** for that collection (so date strings, JSON fields, multi-select arrays, relation IDs, etc. match what PocketBase itself returns). This spec does not redefine field serialization. Clients MUST accept PocketBase's native formats.

A wire record is a JSON object containing at minimum:

- `id`: the record ID
- all collection fields, including the owner field, `created`, and `updated`

`rev` is **not** inside the record object. It is a sibling property of the record in results and changes (§5, §7, §8).

### 4.1 Field categories

| Category | Fields | Client may send in `fields`? |
|---|---|---|
| **Server-authoritative** | `id`, owner field, `created`, `updated`, `collectionId`, `collectionName`, `expand` | **No** → `rejected` / `forbidden_field` |
| **Client-writable** | All other non-file fields declared in `/info` with `writable: true` | Yes |
| **File fields** | Fields of type `file` | **No** in v1 → `rejected` / `file_field_unsupported`. File content sync is a non-goal of v1. Clients receive file field values (filenames) on pull but MUST NOT send them. |

- A key **absent** from `fields` means "unchanged". A key **present** with `null` sets the field to its empty value according to PocketBase semantics for that type.
- A key not declared for the collection → `rejected` / `unknown_field`.

## 5. Endpoint: `GET /api/sync/info`

Returns server capabilities and configuration. Clients SHOULD call it at startup and after protocol or config errors, and MUST respect the advertised limits.

Response `200`:

```json
{
  "protocol": 1,
  "supported": [1],
  "pluginVersion": "0.1.0",
  "serverTime": "2026-10-15T08:30:00.000Z",
  "limits": {
    "maxBatch": 100,
    "maxPage": 500,
    "defaultPage": 200,
    "maxBodyBytes": 1048576,
    "idempotencyRetentionDays": 14
  },
  "collections": {
    "notes": {
      "ownerField": "owner",
      "conflictPolicy": "serverWins",
      "deletePolicy": "deleteWins",
      "fields": [
        { "name": "title", "type": "text", "writable": true },
        { "name": "body", "type": "editor", "writable": true },
        { "name": "attachments", "type": "file", "writable": false }
      ]
    }
  }
}
```

- `conflictPolicy`: `serverWins` | `clientWins` | `reject` (§8.3).
- `deletePolicy`: `deleteWins` | `editWins` (§8.4).
- `fields[].type` uses PocketBase field type names.

## 6. Push: `POST /api/sync/push`

### 6.1 Request

```json
{
  "client": { "id": "device-uuid", "name": "optional label" },
  "mutations": [
    {
      "id": "6f1c0a52-3c2d-4a5e-9a39-0d2c2f5d9b11",
      "collection": "notes",
      "recordId": "abc123def456ghi",
      "op": "update",
      "baseRev": 3,
      "fields": { "title": "New title" }
    }
  ]
}
```

- `client` is optional and informational (diagnostics only; no protocol semantics).
- `mutations` MUST be non-empty and MUST NOT exceed `limits.maxBatch`, else `413`/`400` `batch_too_large`.
- Request body size MUST NOT exceed `limits.maxBodyBytes`.

### 6.2 Mutation semantics

| `op` | `baseRev` | `fields` | Behavior |
|---|---|---|---|
| `create` | MUST be absent | Full set of client-writable fields (required ones included) | Creates the record owned by the authenticated user with `rev = 1`. |
| `update` | REQUIRED, integer ≥ 1 | Only the **changed** fields, non-empty | Applies a field-level patch subject to §8. |
| `delete` | REQUIRED, integer ≥ 1 | MUST be absent | Deletes the record subject to §8.4. |

Processing rules:

1. The server MUST process mutations **in array order**.
2. Each mutation MUST be applied in its **own transaction**. A failed mutation MUST NOT roll back earlier successful ones or prevent later ones from being attempted (except per rule 3).
3. If a mutation for record R does not result in `ok` (or `duplicate` of an earlier `ok`), every later mutation in the **same request** for the same `(collection, recordId)` MUST return `rejected` / `dependency_failed` without being applied.
4. The server MAY stop early (e.g., time limit) and return results for only a **prefix** of the mutations. It MUST NOT skip or reorder: results are positional and cover mutations `0..k-1`. The client MUST resend the unprocessed remainder.
5. Applying a mutation MUST go through PocketBase's normal save/delete path so validation, unique constraints, and hooks run (and so the change log is written, §8.5).
6. The owner field is set by the server from the authenticated user. It MUST NOT be taken from the request (§4.1).

### 6.3 Create details

- ID not existing, not previously deleted → create; result `ok`.
- ID exists and is owned by the authenticated user → `conflict` / `exists`, with the current record and `rev`. (A client retrying a create under a **new** mutation ID after losing the first response will hit this; the idempotency mechanism in §9 normally prevents it.)
- ID exists and is owned by **another** user, or was previously deleted → `rejected` / `id_unavailable`. The server MUST NOT reveal which case applies.

### 6.4 Update / delete details on missing records

- `update` and the record does not exist:
  - if the change log shows a delete of it for this user → `conflict` / `deleted`;
  - otherwise → `rejected` / `not_found`. Clients SHOULD treat `not_found` on update like `deleted`.
- `delete` and the record does not exist → `ok` (deletes are idempotent). `rev` and `record` are omitted if unknown.
- For records owned by another user, the server MUST respond exactly as if the record did not exist.

### 6.5 Response

`200` with one result per processed mutation, in order:

```json
{
  "results": [
    {
      "id": "6f1c0a52-3c2d-4a5e-9a39-0d2c2f5d9b11",
      "status": "ok",
      "rev": 4,
      "merged": false,
      "record": { "id": "abc123def456ghi", "title": "New title", "updated": "2026-10-15 08:30:01.123Z" }
    }
  ]
}
```

Result properties:

| Property | When | Meaning |
|---|---|---|
| `id` | always | The mutation ID. |
| `status` | always | `ok` \| `duplicate` \| `conflict` \| `rejected` |
| `rev` | `ok` (create/update), `conflict` where the record exists | Current server `rev` after this operation. |
| `record` | `ok` (create/update), `conflict` where the record exists | Current full server record (so the client receives server-authoritative fields). |
| `merged` | `ok` on update | `true` if the update was based on a stale `baseRev` but was applied via a non-overlapping field merge (§8.2). The returned `record` is the merged state. |
| `code` | `conflict`, `rejected` | Machine-readable code (§12). |
| `message` | optional | Human-readable detail. Not for programmatic use. |
| `conflictId` | `conflict`/`ok` where an entry was written to `sync_conflicts` | Server-side conflict log reference (diagnostic). |

`status` meanings:

- **`ok`** – applied (or, for delete of a missing record, already satisfied). Client removes the mutation from the outbox and applies `record`/`rev` locally.
- **`duplicate`** – this mutation ID was already processed; the response carries the **original stored result** (including its original `status`, `rev`, `record`, `code`). Client treats it exactly as that original result.
- **`conflict`** – not applied because of a concurrency conflict (§8). The client resolves it locally (§11).
- **`rejected`** – not applied; retrying unchanged will not succeed (validation, forbidden field, unique constraint, unknown collection, dependency failure, etc.).

### 6.6 Request-level errors

If the whole request fails, the server responds with a non-200 status and an error body (§12.1). No mutation in a failed request is applied unless a result was returned for it (which implies a `200`). On network failure or `5xx` the client MUST assume nothing was applied or everything may have been, and **resend the same mutations with the same IDs** (safe due to §9).

## 7. Pull: `GET /api/sync/pull`

Query parameters:

- `cursor` (optional): an opaque string previously returned by the server. Omitted/empty → initial sync.
- `limit` (optional): max number of change entries; default `limits.defaultPage`, maximum `limits.maxPage`.

### 7.1 Response

```json
{
  "mode": "incremental",
  "changes": [
    { "collection": "notes", "recordId": "abc123def456ghi", "op": "upsert", "rev": 5, "record": { } },
    { "collection": "notes", "recordId": "def456ghi789jkl", "op": "delete", "rev": 7 }
  ],
  "cursor": "opaque-next-cursor",
  "hasMore": false,
  "serverTime": "2026-10-15T08:31:00.000Z"
}
```

- `mode`: `"snapshot"` while delivering the initial snapshot (§7.3), `"incremental"` otherwise.
- `changes[].op`: `upsert` (with full `record`) or `delete` (no `record`).
- `cursor`: the value the client MUST send on its next pull. The client MUST persist it only **after** successfully applying the page (§11.3). An empty page still returns a cursor.
- `hasMore`: `true` if more data is immediately available; the client MUST keep pulling until `false`.

### 7.2 Incremental guarantees

For an authenticated user, given a cursor C, the server MUST ensure the sequence of pages starting at C delivers, for every change to a record owned by that user committed after C and before the end of paging, **at least the latest state** of that record (as `upsert` or `delete`). Specifically:

1. **Owner-scoped:** only records owned by the authenticated user are ever returned. This is non-negotiable (§14).
2. **Latest-state, deduplicated:** within one response, there is at most one entry per `(collection, recordId)`, reflecting the latest state known when the page is read. Entries are ordered by ascending change `seq`.
3. **At-least-once:** a record may appear again in later pages or responses (including after the client's own pushes). Clients MUST apply entries idempotently (§11.3).
4. **No gaps:** the cursor semantics guarantee no committed change is skipped, including changes committing while the client is mid-paging.
5. A record that has been deleted by the time the page is read MUST be emitted as `delete` (never as `upsert` with stale data).

### 7.3 Initial sync (snapshot)

When `cursor` is absent, the server starts a **snapshot** (this strategy is confirmed; see §17):

1. The server captures the current change-log head `H`.
2. It returns the user's current records page by page (`mode: "snapshot"`, all entries `op: "upsert"`, ordered by `(collection, recordId)`), using cursors that encode both the snapshot position and `H`.
3. When the snapshot is exhausted, the final page of the snapshot returns a cursor equal to the incremental position `H` (`mode` may be `"snapshot"` on that page; the **next** request uses `mode: "incremental"` semantics starting at `H`).
4. Because records changed during the snapshot have `seq > H`, they are redelivered incrementally. The client's rev-based idempotent apply (§11.3) makes this safe.

Servers MAY implement initial sync by replaying the log from `seq 0` instead, if the log is complete, but MUST then behave identically from the client's perspective. Clients MUST NOT assume which strategy is used and MUST rely on `mode` only for UI purposes (e.g., progress), not correctness.

### 7.4 Cursor rules

- Cursors are **opaque**; clients MUST NOT parse, construct, or compare them.
- A cursor the server cannot decode is **malformed** → `400` / `cursor_invalid`.
- A cursor the server **can** decode but that is older than its retained history **or** ahead of its current head (e.g., after restoring an older backup) is **stale** → `410` / `cursor_too_old`. The client MUST perform a **full resync** (§11.5) rather than retry.
- Because cursors are opaque, conformance fixtures never construct one literally; they request a stale cursor with the runner sentinel `"$stale"` (§15.1).
- Servers SHOULD make cursors valid across restarts and across server upgrades within a protocol version.

## 8. Concurrency and conflict resolution

### 8.1 rev semantics

- A new record starts at `rev = 1`.
- `rev` MUST increase by exactly 1 for every committed write to that record, regardless of the path that caused it (sync push, PocketBase REST API, admin UI, server hooks).
- A delete is a write: it produces a final `rev` (tombstone rev) retained in the change history.
- `rev` is never decreased or reused for the same `(collection, recordId)`.
- A server MAY treat an `update` whose resulting state is identical to the current state as a no-op (no `rev` increment), returning `ok` with the current `rev`.

### 8.2 Update algorithm

Let `R` be the record's current `rev`, `B` the mutation's `baseRev`, and `F` the set of field names in `fields`.

1. If `B > R` → `rejected` / `invalid_base_rev`.
2. If `B == R` → apply; `ok`, `merged: false`.
3. If `B < R` (stale base):
   1. Let `S` be the union of the **changed-field sets** of all committed changes to this record with `rev` in `(B, R]`.
   2. If the server's history does not fully cover `(B, R]` (pruned or unavailable), let `S` = **all fields**.
   3. If `F ∩ S = ∅` → apply as a **merge**; `ok`, `merged: true`, with the merged `record` and new `rev`.
   4. Otherwise (fields overlap) → apply `conflictPolicy` (§8.3).

The changed-field set of a create is all fields. The changed-field set of an update is the set of fields whose stored values differ before vs. after.

### 8.3 Conflict policies (overlapping fields on a stale base)

| Policy | Server action | Result |
|---|---|---|
| `serverWins` (default) | Do not apply. Record the losing mutation (fields/values) in `sync_conflicts`. | `conflict` / `field_conflict`, with current `record` and `rev` |
| `clientWins` | Apply the mutation. Record the overwritten server values in `sync_conflicts`. | `ok`, `merged: false`, `conflictId` set |
| `reject` | Do not apply. Do **not** write to `sync_conflicts`. | `conflict` / `field_conflict`, with current `record` and `rev` |

The policy is configured per collection and advertised in `/info`. It MUST be deterministic: the outcome depends only on the log order and the data, never on device clocks or request timing beyond arrival order.

Notes:

- v1 provides no operation-style mutations. Fields such as counters/balances where "last value wins" is unsafe SHOULD be in collections using `serverWins` or `reject`, with the application resolving conflicts itself.

### 8.4 Delete rules

- `delete` with `B == R` → apply.
- `delete` with `B < R` and `deletePolicy = deleteWins` (default) → apply; if overlapping edits are discarded this way, the server MAY record them in `sync_conflicts`.
- `delete` with `B < R` and `deletePolicy = editWins` → do not apply; `conflict` / `edited_since`, with current `record` and `rev`.
- `delete` with `B > R` → `rejected` / `invalid_base_rev`.
- `update` where the record was deleted → `conflict` / `deleted` (§6.4).

### 8.5 Change log requirements (server)

For each committed write to a synced collection the server MUST, **in the same transaction** as the write, append a change-log entry containing at least: `seq` (monotonic, commit-ordered), `collection`, `recordId`, `owner`, `op` (`create|update|delete`), `rev`, and the changed-field set. A rolled-back write MUST NOT leave a log entry. Changes by any path (not just sync push) MUST be logged.

## 9. Idempotency

- The server MUST persist, per `(user, mutationId)`, the **result** of every mutation it has processed, together with a hash of the mutation's canonical payload.
- On receiving a mutation whose ID is already stored:
  - same payload hash → return `status: "duplicate"` carrying the original stored result (original `status`, `rev`, `record`, `code`);
  - different payload hash → `rejected` / `mutation_id_reuse`.
- Stored results MUST be retained for at least `limits.idempotencyRetentionDays` (advertised in `/info`). Clients MUST NOT retry a mutation after being offline longer than this window without first treating it as potentially applied; if a client's outbox contains mutations older than the window, it SHOULD perform a pull before pushing and rely on conflict handling (`exists`, rev checks) for correctness.
- Transient failures (e.g., database busy, timeout) MUST NOT be stored as results. Only final outcomes (`ok`, `conflict`, `rejected`) are stored.

## 10. Realtime nudge (non-normative in v1)

Servers built on PocketBase SHOULD allow clients to use PocketBase realtime subscriptions on synced collections as a hint that changes are available. The event payload is **not authoritative**; a client receiving any such event SHOULD trigger a pull (debounced). Correctness MUST NOT depend on delivery of nudges. Clients MUST also sync on foreground, connectivity regained, and after local writes.

## 11. Client processing requirements

### 11.1 Outbox

- Each local write MUST be applied to local storage and appended to the outbox in a **single local transaction**.
- Mutations are sent in outbox order.
- The client MAY coalesce pending mutations for the same record before sending: consecutive updates merge (union of fields, latest values, **earliest** `baseRev`); create + updates collapse to one create; create + delete cancel entirely. It MUST NOT coalesce mutations that have already been sent (in flight or possibly applied).
- Mutation IDs are assigned when a mutation is enqueued (or coalesced) and MUST NOT change across retries of that mutation.

### 11.2 Handling push results

| Result | Client action |
|---|---|
| `ok` | Remove from outbox; store returned `record` and `rev` locally (subject to §11.3 rebase for any later pending edits). |
| `duplicate` | Treat as the original stored result. |
| `conflict` | Apply local resolution (§11.4); record in local `_conflicts`; do not retry unchanged. |
| `rejected` | Mark mutation errored, keep in outbox (or move to a failed queue), surface to the application. Later outbox mutations for the same record MUST be held. Do not retry unchanged. |
| HTTP `5xx`, network failure, `429` | Retry the whole batch with the same IDs, with exponential backoff and jitter; honor `Retry-After`. |
| HTTP `401` | Pause syncing, request re-authentication from the host app. MUST NOT drop the outbox. |

### 11.3 Applying pulled changes

For each change entry in order:

- Let `localRev` be the stored `rev` of the record (or none).
- `upsert`: apply only if there is no local record or `change.rev > localRev`. Otherwise ignore (stale or duplicate).
- `delete`: apply only if `change.rev > localRev` or the record is unknown. Deleting a record that has pending local edits triggers conflict handling (§11.4).
- **Rebase:** if the record has pending outbox mutations, the client MUST apply the server state, then re-apply its pending field patches on top, and update each pending mutation's `baseRev` to the newly applied `rev`. The UI MUST NOT observe the user's pending edits reverting.
- Applying a page and persisting the new cursor MUST happen in a **single local transaction**. A crash between them MUST NOT lose the page or skip it.

### 11.4 Local conflict resolution (defaults)

- `conflict` / `field_conflict`: replace local state with the returned server `record`; drop the conflicting mutation; keep the dropped values in `_conflicts` for the app to surface/restore.
- `conflict` / `deleted` (or `rejected` / `not_found` on update): remove the local record; keep the dropped edit in `_conflicts`.
- `conflict` / `exists` (create): treat the returned record as server truth; convert the local intent into an `update` against the returned `rev` if the app opts in, otherwise log in `_conflicts`.
- `conflict` / `edited_since` (delete under `editWins`): restore the returned server record locally; log in `_conflicts`.

Applications MAY override these with their own resolution callbacks.

### 11.5 Full resync

On `410` / `cursor_too_old` the client MUST:

1. Preserve the outbox.
2. Discard its stored cursor and begin a snapshot pull.
3. After the snapshot is applied, remove local synced records that were not delivered **and** have no pending mutations (they were deleted server-side); keep records with pending creates.
4. Rebase pending mutations onto the fresh state (§11.3) and continue normally.

### 11.6 Misc

- Clients MUST respect `limits` from `/info`.
- Clients MUST run at most one sync loop at a time per user/database.
- Clients MUST NOT use device clocks for ordering or conflict resolution.
- On logout, clients MUST NOT silently clear unsynced outbox data (an explicit wipe API is the host app's decision).

## 12. Errors and codes

### 12.1 Request-level errors

Non-200 responses have the body:

```json
{ "status": 400, "code": "protocol_unsupported", "message": "…", "supported": [1] }
```

| HTTP | `code` | Meaning |
|---|---|---|
| 400 | `protocol_unsupported` | Version header missing/unsupported. |
| 400 | `invalid_request` | Malformed JSON / schema violation. |
| 400 | `cursor_invalid` | Malformed cursor. |
| 400/413 | `batch_too_large` | Over `maxBatch` or `maxBodyBytes`. |
| 401 | `unauthorized` | Missing/invalid/expired token. |
| 403 | `forbidden` | Authenticated but not permitted to use sync. |
| 410 | `cursor_too_old` | Stale or ahead-of-head cursor → full resync. |
| 429 | `rate_limited` | Honor `Retry-After`. |
| 5xx | `server_error` | Transient; retry with the same mutation IDs. |

Authorization failures MUST use generic messages and MUST NOT reveal whether a given record or ID exists for another user.

### 12.2 Per-mutation `code` registry

**`conflict`:** `field_conflict`, `deleted`, `exists`, `edited_since`

**`rejected`:**

| Code | Meaning |
|---|---|
| `collection_not_synced` | Unknown or non-synced collection. |
| `invalid_op` | Unsupported `op`. |
| `invalid_id` | Record ID format invalid. |
| `invalid_base_rev` | `baseRev` missing/invalid or greater than current `rev`. |
| `unknown_field` | Field not defined on the collection. |
| `forbidden_field` | Server-authoritative field supplied. |
| `file_field_unsupported` | File field supplied (v1). |
| `validation_failed` | PocketBase validation failed; `details` MAY carry per-field errors. |
| `unique_violation` | Unique constraint violated. |
| `rule_denied` | A collection rule denied the write. |
| `id_unavailable` | ID belongs to another user or was previously deleted. |
| `not_found` | Update target missing (no deletion evidence). |
| `dependency_failed` | An earlier mutation for the same record in this request did not succeed. |
| `mutation_id_reuse` | Mutation ID previously used with a different payload. |

New codes MAY be added without a version bump; clients MUST treat unknown codes by their `status` class.

## 13. Retention and operations (server requirements)

- Servers MUST advertise `idempotencyRetentionDays` in `/info`.
- Servers MAY prune the change log; if they do, they MUST track the oldest retained position and answer older cursors with `410` / `cursor_too_old`.
- Servers MUST answer a cursor that is ahead of their head with `410` / `cursor_too_old` (covers restore-from-backup and resets).
- Pruning change-history that is needed to evaluate `(B, R]` MUST be treated as incomplete history (§8.2 step 3.2), never as "no changes".

## 14. Security requirements (normative)

1. **Authentication required** on all endpoints.
2. **Ownership enforcement on every mutation:** the server MUST verify the record's owner equals the authenticated user for update/delete, and MUST set the owner itself on create. The owner value is never trusted from the client.
3. **Pull isolation:** pull queries MUST filter by the authenticated owner at the data layer; a bug in post-filtering is not acceptable.
4. **Authoritative fields:** server-authoritative fields in client input MUST be rejected (§4.1).
5. **No existence oracle:** responses for other users' records MUST be indistinguishable from nonexistent ones (§6.3, §6.4, §12.1).
6. **Rules:** the server MUST apply PocketBase validation and unique constraints. In v1, **ownership is the only authorization rule** evaluated per mutation beyond that; the collection's own create/update/delete rules are **not** evaluated on the sync path (§17). The `rule_denied` code is reserved for a future opt-in mode and MUST NOT be emitted by a v1 server.
7. **Resource limits:** batch size, page size, and request body size limits MUST be enforced. Rate limiting is RECOMMENDED.
8. **Transport:** TLS in production.
9. **Logging:** servers SHOULD NOT log record contents at default log levels.

## 15. Conformance

A server or client implementation conforms to protocol v1 if it passes every fixture in `conformance/` that applies to its role.

### 15.1 Fixture format

Fixtures are JSON files describing deterministic scenarios:

```json
{
  "name": "update-stale-base-nonoverlapping-merges",
  "description": "A stale baseRev with non-overlapping fields is merged.",
  "config": { "collections": { "notes": { "ownerField": "owner", "conflictPolicy": "serverWins", "deletePolicy": "deleteWins" } } },
  "users": ["alice", "bob"],
  "steps": [
    { "as": "alice", "push": { "mutations": [
      { "id": "m1", "collection": "notes", "recordId": "aaaaaaaaaaaaaaa", "op": "create", "fields": { "title": "t", "body": "b" } }
    ] }, "expect": { "results": [ { "id": "m1", "status": "ok", "rev": 1 } ] } },
    { "as": "alice", "push": { "mutations": [
      { "id": "m2", "collection": "notes", "recordId": "aaaaaaaaaaaaaaa", "op": "update", "baseRev": 1, "fields": { "title": "t2" } }
    ] }, "expect": { "results": [ { "id": "m2", "status": "ok", "rev": 2 } ] } },
    { "as": "alice", "push": { "mutations": [
      { "id": "m3", "collection": "notes", "recordId": "aaaaaaaaaaaaaaa", "op": "update", "baseRev": 1, "fields": { "body": "b2" } }
    ] }, "expect": { "results": [ { "id": "m3", "status": "ok", "rev": 3, "merged": true } ] } }
  ]
}
```

- Steps run sequentially against a fresh server (or client + mock server).
- `expect` is a **subset match**: listed properties must match; unlisted properties (e.g., timestamps, opaque cursors) are ignored.
- Wildcards: `"$any"` (any value), `"$string"`, `"$int"`.
- Cursors are passed between steps by reference: `{ "pull": { "cursor": "$prev" } }` (the cursor returned by the most recent pull). To exercise the stale-cursor path use `{ "pull": { "cursor": "$stale" } }`: the runner supplies a cursor that its server decodes but that is older than retained history or ahead of head, so the expected result is `410`. Cursors are never written literally (§7.4).

### 15.2 Required fixture categories (initial list)

1. **Create / update / delete** happy paths and `rev` increments.
2. **Idempotency:** duplicate mutation ID (same payload → `duplicate`; different payload → `mutation_id_reuse`).
3. **Stale-base merge:** non-overlapping fields merge; overlapping fields per each of the three policies.
4. **Delete rules:** `deleteWins` vs `editWins`; update-after-delete; delete of missing record.
5. **Create edge cases:** `exists`, `id_unavailable` (other user's ID, deleted ID).
6. **Rejections:** forbidden/unknown/file fields, invalid `baseRev`, unique violation, dependency failure.
7. **Batch behavior:** per-mutation isolation, ordering, partial prefix results.
8. **Pull:** paging, `hasMore`, dedupe, delete delivery, empty cursor snapshot with concurrent writes, owner isolation across users.
9. **Cursor errors:** malformed (`400`), stale and ahead-of-head (`410`).
10. **Security:** cross-user update/delete/create attempts, forged owner, forged server fields, no existence oracle.
11. **Changes via other paths:** a record changed via the PocketBase REST API appears in pull with correct `rev` and changed fields for merge evaluation.
12. **Client-side fixtures:** outbox coalescing, rebase-on-pull, apply-by-rev idempotency, crash between page-apply and cursor-persist (atomicity), full resync with pending mutations.

## 16. Versioning and compatibility

- The protocol version is a single integer (`X-Sync-Protocol`). It changes only for breaking changes to this document.
- Plugin/client package versions follow semver independently; each release states the protocol versions it supports.
- This document is frozen at **v1.0**. A breaking change requires a new protocol version and is recorded in the changelog below.

## 17. Frozen decisions and deferred items

Items formerly marked `[OPEN]` are resolved here. Decisions 17.1.4 and orphan items 5–9 are non-wire concerns; they do not change the protocol and are recorded for implementers.

### 17.1 Resolved at Gate 0 (protocol v1.0)

1. **`rev` storage.** Servers keep a side table `sync_meta(collection, record_id)` → `rev`, `owner` (it does not modify user collection schemas). Deleted records retain their `sync_meta` row for the retained-history window so a tombstone `rev` can be answered; where that row has been pruned, the tombstone `rev` is served from the latest `sync_changes` entry for the record. Servers MUST NOT reuse or decrease a `rev` (§8.1).
2. **Collection rules.** v1 evaluates **ownership only** (§14.6). `rule_denied` is reserved for a future opt-in mode and MUST NOT be emitted by a v1 server.
3. **Initial sync.** The snapshot-plus-head-cursor design in §7.3 is confirmed. Records changed during the snapshot have `seq > H` and are redelivered incrementally; the client's rev-based idempotent apply makes this safe. Log-replay from `seq 0` remains an allowed alternative implementation (§7.3) with identical client-visible behavior.
4. **Hook transaction scope.** Proven for PocketBase v0.40.5: `On*AfterCreate/Update/DeleteSuccess` runs *after* the outer transaction commits, so change-log writes MUST NOT go there. The atomic pattern is the in-transaction `OnRecordCreate/Update/DeleteExecute` hooks, appending `rev` + change-log via the event's tx-scoped `e.App` DB. Evidence: `server/syncplugin/spike_test.go` (a forced rollback leaves neither the record nor the log row; a commit leaves both).

### 17.2 Deferred to v0.2 (do not implement in v1)

5. **Unique constraints across devices:** a server MAY return `details` naming the conflicting record ID on `unique_violation`, but v1 defines no structured client resolution flow.
6. **Relations and ordering:** only same-record dependencies are tracked in v1 (§6.2 rule 3, `dependency_failed`); cross-record relation ordering is out of scope.
7. **No-op updates:** servers MAY skip the `rev` increment for an identical update (§8.1); this stays MAY.
8. **Idempotency window vs. long offline periods:** clients SHOULD pull before pushing when their outbox is older than the advertised window (§9); no extra protocol mechanism in v1.
9. **Dedicated nudge channel:** v1 reuses PocketBase collection subscriptions (§10); a dedicated SSE topic is roadmap.

## 18. Media sync (v1.1, additive)

Added in **v1.1** without a version bump: new endpoints under `/api/sync/media/`, new error codes, and an optional `media` object in `/info` are additive changes (§2). The header stays `X-Sync-Protocol: 1`; clients not using media are unaffected. Rationale and design background: `docs/media-sync-plan.md`.

### 18.1 Model

- A dedicated **synced collection** (default name `media`) holds one record per unique blob, with the fixed fields `owner` (owner field), `sha256` (text), `size` (number), `mime` (text), `filename` (text), `file` (file field: the bytes), and `poster` (file field, optional: client-generated JPEG so HEIC/RAW/video always have a thumbnail source).
- **Media records are immutable and server-created.** They are created only by the server at `commit`. Clients MUST NOT create, update, or delete media records via push; the server MUST reject such mutations with `rejected` / `media_immutable`. Unreferenced media is removed by server-side GC (§13-style retention; not a wire concern).
- Business records reference media with **ordinary relation fields**, synced by `push`/`pull` as today. File fields remain non-writable on push (§4.1).
- The dedup key is **(owner, sha256)**: dedup scope is per user (§14.5 — a global index would leak a cross-user content-existence oracle).
- The identity of the bytes is the content hash, not `rev`. `rev` orders the media *record* changes only.

### 18.2 `POST /api/sync/media/init`

Request:

```json
{ "collection": "media", "sha256": "<64 lowercase hex>", "size": 84213769,
  "mime": "image/heic", "filename": "IMG_4821.heic" }
```

- `collection` MUST be the server's configured media collection, else `400` / `unsupported_media`.
- `sha256` MUST match `^[0-9a-f]{64}$`; `size` MUST be a positive integer ≤ the advertised `media.maxFileSize`, else `413` / `upload_too_large`.
- `mime` MUST pass the server's allowlist, else `415` / `mime_not_allowed`.
- If the per-user quota would be exceeded, `403` / `quota_exceeded` — checked at `init`, before bytes move, counting committed bytes and active staged sessions; a dedup hit is checked first and takes precedence (it adds no bytes).

Response `200`, either a dedup hit (no bytes transferred):

```json
{ "status": "exists", "mediaId": "abc123def456ghi", "rev": 1 }
```

or a resumable upload session:

```json
{ "status": "upload", "uploadId": "u_9f…", "chunkSize": 8388608, "expiresIn": 86400 }
```

- `uploadId` is an opaque token; `chunkSize` is the server's recommended chunk size in bytes; `expiresIn` is the session TTL in seconds. A session that expires or is aborted yields `410` / `upload_expired` and `404` / `upload_not_found` respectively on later use.

### 18.3 Upload channel (resumable, tus 1.0 core)

| Request | Meaning |
|---|---|
| `HEAD /api/sync/media/upload/{uploadId}` | `200` with `Upload-Offset: <n>` and `Upload-Length: <size>` headers — where to resume from. |
| `PATCH /api/sync/media/upload/{uploadId}` | Headers `Upload-Offset: <n>` (MUST equal the current offset, else `400` / `invalid_request`) and `Content-Type: application/offset+octet-stream`. Appends the body at the current offset. Responds `204` with the new `Upload-Offset` header. |
| `DELETE /api/sync/media/upload/{uploadId}` | `204`; aborts the session and discards staged bytes. |

- All three require authentication and the `X-Sync-Protocol` header. An unknown **or foreign** `uploadId` is `404` / `upload_not_found` (no existence oracle, §14.5); this includes consumed sessions, so `DELETE` cannot destroy the stored idempotency result of §18.4.
- A `PATCH` that would push the staged bytes past the declared `size` is `413` / `upload_too_large`.
- Servers MUST stream chunks to staging storage and MUST NOT require the whole body in memory.

### 18.4 `POST /api/sync/media/commit`

Request: `{ "uploadId": "u_9f…", "posterUploadId": "u_aa…" }` (`posterUploadId` optional).

Server behavior: verifies the staged size equals the declared `size` and recomputes `sha256` over the staged bytes (the client's hash is a claim, not proof) → mismatch is `400` / `hash_mismatch`. If the optional `posterUploadId` is given, its session must be a fully staged JPEG (≤ the advertised poster cap; otherwise `400` / `invalid_request`). Then, in one transaction, it creates the `media` record (owner from the token, file fields attached), so the change log is written atomically (§8.5).

Response `200`:

```json
{ "status": "ok", "mediaId": "abc123def456ghi", "rev": 1,
  "file": { "name": "IMG_4821.heic", "size": 84213769 } }
```

- **Idempotent:** if a media record for (owner, sha256) already exists — including a commit retry after a lost response — the server returns the existing record with the same response shape. A commit for an already-committed `uploadId` returns the stored original result.
- Bytes are served through PocketBase's own file serving (`/api/files/{collection}/{recordId}/{file}[?thumb=…]`) with a short-lived file token; this protocol defines no download endpoint.

### 18.5 Error codes

New request-level codes (§12.1 semantics; bodies use `{status, code, message}`):

| HTTP | `code` | Meaning |
|---|---|---|
| 400 | `unsupported_media` | `collection` is not the configured media collection. |
| 400 | `hash_mismatch` | Committed bytes do not match the declared `sha256`. |
| 403 | `quota_exceeded` | Per-user byte quota would be exceeded. |
| 404 | `upload_not_found` | Unknown, foreign, or already-consumed `uploadId`. |
| 404 | `media_not_found` | A referenced media record does not exist (reserved for future media-lookup endpoints). |
| 410 | `upload_expired` | The upload session's TTL has passed. |
| 413 | `upload_too_large` | Declared or staged size exceeds `media.maxFileSize`. |
| 415 | `mime_not_allowed` | MIME type fails the server allowlist. |

A new per-mutation code is registered: `media_immutable` under `rejected` (§12.2).

### 18.6 `/info` addition and security requirements

When media sync is configured, `/info` MAY include an additive `media` object:

```json
"media": { "collection": "media", "chunkSize": 8388608, "maxFileSize": 0,
           "perUserQuota": 0, "posterMaxBytes": 8388608, "stagingTtlHours": 24 }
```

(`0` = unlimited; `maxFileSize`/`perUserQuota` are bytes.) Clients MUST respect these limits.

Security (extends §14): upload sessions and media records are bound to the token's user — a session cannot be resumed or committed by another user; `sha256` is always recomputed server-side at commit; MIME allowlists MUST deny `text/html` and `image/svg+xml`; `init` SHOULD be rate-limited; abandoned staging MUST be swept by TTL so partial uploads cannot fill the disk; file tokens handed to clients MUST be short-lived.

## 19. Changelog

- **v1.1** — Media sync added as a purely additive extension (§18): `init`/`commit` endpoints, a tus-1.0-core resumable upload channel, per-user content-hash dedup, the immutable server-created `media` collection (`media_immutable` rejection on push), new error codes, and an optional `media` object in `/info`. The protocol version header remains `1`; §16's "additive changes do not bump the version" rule applies.

- **v1.0 (frozen)** — Gate 0 resolutions applied: `rev` storage in a `sync_meta` side table with retained tombstone revs (§17.1.1), ownership-only rules in v1 (§14.6, §17.1.2), snapshot+head-cursor initial sync confirmed (§7.3, §17.1.3), and hook transaction-scope resolved to the `*Execute` hooks with spike evidence (§8.5, §17.1.4). Former `[OPEN]` markers removed; non-wire items deferred to v0.2 (§17.2).
- **Draft 0** — Initial draft: endpoints (`info`, `push`, `pull`), rev/baseRev concurrency, field-level merge, conflict policies, idempotency, snapshot initial sync, error registry, security requirements, conformance fixture format.
