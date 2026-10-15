# pb-offline-sync — Product Design Document

> Working name. Check availability on GitHub, Go modules, and npm before committing. Do not imply official PocketBase endorsement.

**Status:** Draft v0.1 · **License:** MIT · **Target:** open-source plugin for PocketBase users building offline-first apps

---

## 1. Summary

pb-offline-sync is a two-way sync layer between a **local database on a mobile/web client** and a **server-side PocketBase**. It consists of:

1. A **Go plugin** for PocketBase that adds sync endpoints, a change log, and conflict handling.
2. A **TypeScript client engine** that keeps an outbox of local mutations, pushes them, pulls server changes, and merges both.
3. A **written protocol spec** and **conformance test suite** so either side can be reimplemented independently (Swift, Kotlin, Dart clients; other servers).

It is not a CouchDB/PouchDB-compatible replicator. It borrows CouchDB's ideas (change feed, checkpoints, optimistic concurrency, preserved conflicts) but maps them onto PocketBase's typed relational records.

## 2. Problem and motivation

- PocketBase has no offline-first sync. Realtime subscriptions require a live connection and do not queue local writes.
- PouchDB/CouchDB-style sync is poorly maintained and does not fit PocketBase's data model.
- Existing sync engines (PowerSync, ElectricSQL, RxDB) are built around other backends or have licensing/maintenance tradeoffs.
- The goal is a small, correct, debuggable option that PocketBase users can add without forking PocketBase.

## 3. Goals and non-goals

### Goals (v0.1)

- Two-way sync of **per-user owned records** (each record has an owner field).
- Offline-first client: the UI reads and writes only local data; sync happens in the background.
- **Idempotent, retry-safe** push over flaky mobile networks.
- **Field-level patches** with `rev` checks so edits to different fields on different devices merge automatically.
- **Deterministic conflict resolution** with a conflict log (nothing silently lost when configured so).
- Deletes propagate to all devices.
- Realtime "pull now" nudge so updates arrive quickly without polling.
- Works as a **plugin on top of PocketBase** (no fork required).
- Documented protocol and a conformance suite.

### Non-goals (v0.1) — listed as roadmap in the README

- Shared / multi-user records (requires per-record rule evaluation on pull).
- Partial sync / filtered sync / sync subscriptions.
- File sync (documented as a separate upload queue; roadmap).
- CRDTs / collaborative text editing.
- Server-side custom merge hooks.
- Compatibility with the CouchDB replication protocol.
- Running PocketBase itself on the device.

## 4. Target users and use cases

- Solo developers and small teams building Expo/React Native (and later web) apps on PocketBase.
- Typical apps: notes, trackers, field-data capture, POS/inventory companions, personal tools that must work without connectivity.

## 5. Design principles

1. **Protocol first.** The spec is the source of truth, code follows.
2. **Narrow and correct over broad and leaky.** Ship less, test more.
3. **Server is authoritative** for ordering, timestamps, rev numbers, and ownership. Device clocks are never trusted.
4. **Security by default.** Custom routes bypass collection API rules unless explicitly enforced, so enforcement is built into the plugin and tested.
5. **No hidden dependencies.** Client core has no hard dependency on a specific storage engine, transport, or UI framework.
6. **Observable.** Sync state, errors, and conflicts are inspectable by the app.

## 6. Architecture overview

```
┌────────────────────────── Client ──────────────────────────┐
│ App UI ──reads/writes──► Local DB (SQLite)                  │
│                            │ tables: <collection>, _outbox, │
│                            │ _sync_state, _conflicts        │
│ Sync Engine ◄──► StorageAdapter                             │
│      │                                                      │
│      └──► Transport (HTTP + SSE nudge)                      │
└──────────────────────────┬──────────────────────────────────┘
                           │ push / pull / nudge
┌──────────────────────────▼──────────────────────────────────┐
│ PocketBase (+ pb-offline-sync plugin)                       │
│  routes: /api/sync/push, /api/sync/pull, /api/sync/info     │
│  hooks:  record create/update/delete → sync_changes + rev   │
│  tables: sync_meta, sync_changes, sync_processed,           │
│          sync_conflicts                                     │
└─────────────────────────────────────────────────────────────┘
```

Changes made through **any** path (sync push, normal PocketBase API, admin UI, other hooks) are captured by hooks and delivered to all devices.

## 7. Protocol specification (summary)

The full spec lives in `docs/protocol.md`. This section defines intent; the spec must be written first and treated as the contract.

### 7.1 General

- Header `X-Sync-Protocol: 1` on all requests. Server rejects unsupported versions with `400` + code `protocol_unsupported`.
- Auth: standard PocketBase auth token. All sync routes require auth.
- `GET /api/sync/info` returns the protocol version and supported versions, the plugin version, `serverTime`, the configured collections (each with `ownerField`, `conflictPolicy`, `deletePolicy`, and a `fields` list of `{name, type, writable}` where file fields are `writable: false`), and `limits` (`maxBatch`, `maxPage`, `defaultPage`, `maxBodyBytes`, `idempotencyRetentionDays`). See protocol §5 for the exact shape.
- All timestamps in responses are server-generated UTC ISO strings.

### 7.2 Record identity

- Record IDs are **generated on the client**, in PocketBase's default format (15 chars, `[a-z0-9]`), so the server accepts them.
- Each synced record has a server-maintained integer **`rev`**, incremented on every write. New records start at `rev = 1`.

### 7.3 Push — `POST /api/sync/push`

Request:

```json
{
  "mutations": [
    {
      "id": "mut_<uuid>",
      "collection": "notes",
      "recordId": "abc123def456ghi",
      "op": "create | update | delete",
      "baseRev": 3,
      "fields": { "title": "New title" }
    }
  ]
}
```

Rules:

- `create`: `fields` is the full set of client-writable fields; `baseRev` omitted.
- `update`: `fields` contains **only changed fields**; `baseRev` required.
- `delete`: `baseRev` required.
- Mutations are applied **in order**, each in its **own transaction**, so one failure does not block the batch.
- If a mutation fails, later mutations on the **same record** in the batch return `rejected` with code `dependency_failed`.
- Server-authoritative fields (`id`, `owner`, `created`, `updated`, system fields) in `fields` are rejected or ignored per spec. A client-supplied `owner` is never trusted.

Response:

```json
{
  "results": [
    {
      "id": "mut_<uuid>",
      "status": "ok | duplicate | conflict | rejected",
      "rev": 4,
      "merged": false,
      "record": { "...": "current server state (on conflict, or when fields were merged)" },
      "code": "machine_readable_code",
      "message": "human readable",
      "conflictId": "optional sync_conflicts reference"
    }
  ]
}
```

- `duplicate`: mutation ID already processed; the stored original result is returned.
- `merged`: `true` on an `ok` update that was applied from a stale `baseRev` via a non-overlapping field merge (protocol §6.5, §8.2).
- `conflict`: see §7.5.
- `rejected`: validation failure, rule violation, unique constraint, unknown collection, forbidden field, etc. Not retryable without change.

### 7.4 Pull — `GET /api/sync/pull?cursor=<opaque>&limit=<n>`

- Cursor is an **opaque string** to clients (encodes `seq` of `sync_changes`). Empty cursor = initial sync.
- Server reads up to `limit` change rows with `seq > cursor` **for the authenticated owner**, dedupes by `(collection, recordId)` keeping the latest, loads current state for upserts.

Response:

```json
{
  "mode": "incremental",
  "changes": [
    { "collection": "notes", "recordId": "abc...", "op": "upsert", "rev": 5, "record": { } },
    { "collection": "notes", "recordId": "def...", "op": "delete", "rev": 7 }
  ],
  "cursor": "<next opaque>",
  "hasMore": true,
  "serverTime": "2026-10-15T08:31:00.000Z"
}
```

- `mode` is `"snapshot"` while the initial snapshot is being delivered, `"incremental"` otherwise; clients use it for progress only, never for correctness. The response also carries `serverTime`. (Protocol §7.1 is authoritative where this summary differs.)

- Client loops until `hasMore` is false, persisting the cursor after each page is applied atomically.
- If the cursor is older than the retained log, server returns `410` + code `cursor_too_old`; client performs a **full resync** (§9.7).
- Initial sync for an empty cursor may stream a snapshot of current records (spec decision, see §15) instead of replaying the log from zero.

### 7.5 Conflict model

The server keeps, per change-log row, the **set of fields changed**. When an `update` arrives with `baseRev` older than the current `rev`:

1. Compute the union of fields changed on the server since `baseRev`.
2. If the mutation's fields **do not intersect** that set → apply as a **merge**, return `ok` with the merged record.
3. If they intersect → apply the collection's configured policy:
   - `serverWins` (default): do not apply; log to `sync_conflicts`; return `conflict` + current server record.
   - `clientWins`: apply; log overwritten values to `sync_conflicts`; return `ok`.
   - `reject`: do not apply or log; return `conflict`; client decides.
4. If `baseRev` predates the retained change history, treat **all fields as conflicting** (conservative).

Other cases:

- **Update vs. server-side delete:** return `conflict` code `deleted`; default client behavior: drop the local edit, record it in `_conflicts`.
- **Delete vs. newer server edit:** default is **delete wins** (configurable per collection).
- **Counters/balances:** field-level merge is not enough. Roadmap: operation mutations (`{ "inc": 1 }`). In v0.1, document that such fields should use `serverWins` or `reject`.
- Determinism: ties are broken by server arrival order (sequence), never by device time.

## 8. Server design (Go plugin)

### 8.1 Integration

```go
// main.go in the user's own PocketBase app
app := pocketbase.New()
syncplugin.MustRegister(app, syncplugin.Config{
    Collections: map[string]syncplugin.CollectionConfig{
        "notes": {OwnerField: "owner", Conflict: syncplugin.ServerWins, DeletePolicy: syncplugin.DeleteWins},
    },
    MaxBatch: 100,
    MaxPage:  500,
    ChangeLogRetention: 30 * 24 * time.Hour,
})
app.Start()
```

- Targets PocketBase's current Go-framework API (v0.23+ hook/router style). **Verify against the latest release at build time** and pin a supported range.
- On register, the plugin runs its own **migrations** (idempotent) and validates config against the actual collection schemas (owner field exists, is a relation to the auth collection, etc.). Fail fast on misconfiguration.

### 8.2 Tables (created by migration, in the main data DB)

| Table | Purpose |
|---|---|
| `sync_meta` | `(collection, record_id)` → `rev`, `owner`. Avoids modifying user collection schemas. **Decided (protocol §17.1): a side table, not a per-collection `rev` field.** Deleted records keep their row for the retained-history window so a tombstone `rev` can be answered. |
| `sync_changes` | `seq` (AUTOINCREMENT), `collection`, `record_id`, `op`, `owner`, `rev`, `changed_fields` (JSON). Index on `(owner, seq)`. |
| `sync_processed` | `mutation_id` (unique per user), stored result JSON, `created`. Pruned after N days. |
| `sync_conflicts` | Collection, record, mutation ID, losing fields/values, winning rev, `created`, `resolved` flag. |

Rationale for `seq` over `updated`: timestamps can tie and transactions can commit after a client has already pulled past their time. SQLite has a single writer, so autoincrement order equals commit order.

### 8.3 Hooks

- Hook the record **create/update/delete `*Execute`** events (the in-transaction hooks) for configured collections — **not** the `*AfterSuccess` hooks, which are deferred until after the outer transaction commits (proven in the M0 spike, protocol §17.1.4).
- In the **same transaction** as the data change, using the event's tx-scoped `e.App` DB: increment `rev` in `sync_meta`, compute the changed-field set (original vs. new state), append to `sync_changes`.
- **Covered by a test** (`server/syncplugin/spike_test.go`): a forced rollback leaves neither the record nor the log row; a commit leaves both.
- Push uses the normal record save path, so hooks fire exactly once per applied mutation and there is a single code path for logging.

### 8.4 Push handler algorithm

For each mutation, in order:

1. Authenticate; resolve user.
2. Validate: collection configured, op valid, fields allowed (no server-authoritative or unknown fields), ID format.
3. Begin transaction.
4. Look up `sync_processed` for `(user, mutation_id)`; if found, compare the stored **canonical payload hash**: equal → return `duplicate` carrying the stored original result; different → `rejected` / `mutation_id_reuse` (protocol §9). Only final outcomes are stored; transient failures never are.
5. Load existing record (if any); verify **ownership** (`owner == user`); for `create`, force `owner = user` and reject if the ID already exists for another owner.
6. Run conflict logic (§7.5) for update/delete.
7. Apply via the standard save/delete path, which enforces schema validation and hooks.
8. Store result in `sync_processed`; commit.
9. On error, roll back, and (for non-transient errors) store a `rejected` result so retries are stable.

### 8.5 Pull handler algorithm

1. Authenticate; decode cursor; validate it against the oldest retained `seq` (else `410`).
2. Select change rows `seq > cursor AND owner = user ORDER BY seq LIMIT n+1` (n+1 to detect `hasMore`).
3. Dedupe by `(collection, record_id)`, keeping the highest `seq`.
4. For upserts, load the **current** record state and `rev`; if the record no longer exists, emit a delete.
5. Serialize records using the documented wire format (§9.4). Return next cursor.

### 8.6 Realtime nudge

- v0.1: clients subscribe to PocketBase realtime on synced collections and treat any event as "pull now" (payload ignored). Collection list/view rules must permit it, which the docs must state.
- Roadmap: a dedicated lightweight SSE topic carrying only "changes available".

### 8.7 Retention and operations

- Prune `sync_changes` older than the configured retention, `sync_processed` older than N days.
- Track the oldest retained `seq` to detect stale cursors.
- Provide a CLI/admin helper to inspect conflicts and the log.
- Document backup/restore implications (restoring an older backup invalidates client cursors → clients must full-resync; the server should detect "cursor ahead of head" and return `410` as well).

## 9. Client design (TypeScript)

### 9.1 Package structure

- `@pb-offline-sync/core` — engine, outbox, merge logic, interfaces. No platform dependencies.
- `@pb-offline-sync/adapter-expo-sqlite` — first storage adapter.
- Later: web adapter (wa-sqlite / IndexedDB), React hooks package.

### 9.2 Interfaces

```ts
interface StorageAdapter {
  transaction<T>(fn: (tx: Tx) => Promise<T>): Promise<T>;
  // CRUD for synced collections + internal tables, all within one transaction scope
}

interface Transport {
  push(mutations: Mutation[]): Promise<PushResponse>;
  pull(cursor: string | null, limit: number): Promise<PullResponse>;
  subscribe(onNudge: () => void): () => void;
}
```

Core ships with an **in-memory adapter and mock transport** for fast tests.

### 9.3 Local tables

- One table per synced collection, with the mapped columns plus `_rev` and `_dirty`.
- `_outbox`: `(mutation_id, collection, record_id, op, base_rev, fields_json, created_at, attempts, status)`.
- `_sync_state`: cursor, last sync time, last error.
- `_conflicts`: surfaced conflicts and their resolution state.

### 9.4 Schema / type mapping

| PocketBase field | Local SQLite | Notes |
|---|---|---|
| text, email, url, editor | TEXT | |
| number | REAL | |
| bool | INTEGER (0/1) | |
| date, autodate | TEXT (ISO) | `created`/`updated` are **server-authoritative**, never sent by the client |
| select (single) | TEXT | |
| select (multi) | TEXT (JSON array) | |
| json, geoPoint | TEXT (JSON) | |
| relation (single) | TEXT | No local FK enforcement in v0.1 |
| relation (multi) | TEXT (JSON array) | |
| file | TEXT (JSON array of filenames) | Content not synced in v0.1 |

This is where most real-world bugs will appear. Each mapping needs round-trip tests (server → local → server).

### 9.5 Local write path

- Single transaction: apply the change to the local table **and** append/coalesce into `_outbox`.
- **Coalescing:** consecutive pending updates to the same record merge into one mutation (union of fields, latest values, **earliest** `baseRev`). A create followed by updates collapses into one create. A create followed by a delete cancels out entirely.
- IDs generated on-device in PocketBase's format.
- **Deleted IDs are never reused** (protocol §3): the client persists a tombstone set of deleted record IDs and MUST NOT generate a new record with a previously used ID; the server rejects such a create with `id_unavailable`.

### 9.6 Sync loop

State machine: `idle → pushing → pulling → idle` (with `offline`, `error`, `backoff`).

1. Push outbox in order, in batches ≤ server `MaxBatch`.
2. Process per-mutation results: `ok` → remove from outbox, update local `_rev`; `duplicate` → same; `conflict` → apply policy (§7.5), store in `_conflicts`; `rejected` → mark failed and surface to the app, block dependents.
3. Pull from the stored cursor, loop until `hasMore` is false.
4. **Apply pulled changes** in one transaction per page, with the cursor update in the same transaction.
5. **Rebase pending edits:** if a pulled record has pending outbox mutations, apply the server state, then re-apply the pending patch on top and update that mutation's `baseRev` to the pulled rev. The UI must never see a pulled change revert the user's just-typed value.

Triggers: app foreground, connectivity regained, after local writes (debounced), realtime nudge, manual `syncNow()`. Exponential backoff with jitter; single-flight (no concurrent loops).

Engine exposes: sync status, pending count, last error, conflict list, and events/observables for UIs.

### 9.7 Full resync

On `cursor_too_old` (or a server reset): fetch a full snapshot, **preserve unsynced outbox** mutations, replace synced rows, rebase pending edits onto the fresh state, then continue normally.

### 9.8 Auth handling

- Token refresh before/within long syncs; on 401 pause and request re-auth from the host app without dropping the outbox.
- Outbox is **never** cleared on logout unless the host app explicitly requests a wipe (avoid silent data loss; also provide an explicit wipe API for account switching).

## 10. Security and threat model

Assets: per-user records; integrity of other users' records; server availability.

| Threat | Mitigation |
|---|---|
| Client writes another user's record | Server verifies `owner == auth.id` on every mutation; owner never taken from the client |
| Client sets owner on create | Ignored/forced to auth user |
| Forging server fields (`rev`, `created`, `updated`) | Rejected/stripped |
| Reading others' changes via pull | Pull query always filtered by authenticated owner |
| ID collision/squatting (create with another user's record ID) | Reject; do not leak existence (generic error) |
| Replay of old mutations | `sync_processed` idempotency |
| Oversized batches / DoS | `MaxBatch`, `MaxPage`, body size limit, rate limiting guidance |
| Collection API rules bypassed by custom routes | Plugin must call the standard save/delete paths (so validation + hooks run) and **explicitly check** the collection's create/update/delete rules where they restrict beyond ownership (decision in §15) |
| Info leak through error messages | Generic messages for authz failures |

Required: a dedicated **cross-user access test suite** (see §11), and a short `SECURITY.md` with a disclosure process.

## 11. Testing strategy

1. **Conformance suite** (`conformance/`): language-neutral fixtures (request/response JSON) that any server or client implementation must pass. Generated/consumed by both Go and TS tests.
2. **Server unit/integration tests:** hooks (including rollback atomicity), push (each status), pull (paging, dedupe, deletes), conflict matrix, idempotency, pruning, `410` paths.
3. **Convergence/fuzz test:** N simulated clients doing random edits, offline periods, duplicated/reordered/dropped requests, and concurrent server-side edits; assert all clients and the server converge to identical state, and that no acknowledged write is lost (except as recorded in `sync_conflicts`). This is the key proof of correctness.
4. **Security tests:** cross-user read/write/delete attempts, forged owner, forged server fields, ID squatting.
5. **Client tests:** outbox coalescing, rebase-on-pull, resync with pending edits, backoff, single-flight, schema mapping round-trips.
6. **Compatibility matrix in CI:** several supported PocketBase versions (oldest supported → latest), Go versions, and Node versions.
7. **E2E:** the example Expo app against a real PocketBase binary (smoke test).

## 12. Repo layout

```
pb-offline-sync/
  AGENTS.md / CLAUDE.md    # build conventions for coding agents (see §14)
  docs/
    protocol.md            # source of truth
    architecture.md
    threat-model.md
  conformance/             # shared JSON fixtures
  server/                  # Go module (plugin)
  client/
    core/
    adapter-expo-sqlite/
  examples/
    expo-notes/
    web-minimal/           # later
  .github/workflows/
  LICENSE (MIT), SECURITY.md, README.md, CHANGELOG.md
```

Separate repo from the PocketBase fork. The plugin tracks upstream releases without merge conflicts.

## 13. Milestones and work packages

Designed so work packages can run in **parallel worktrees** once M0 lands, because the shared contract (`docs/protocol.md` + `conformance/`) decouples server and client.

| ID | Package | Depends on | Done when |
|---|---|---|---|
| **M0** | Protocol spec + conformance fixtures + repo scaffold + CI skeleton | — | `protocol.md` reviewed; fixtures cover push/pull/conflict/duplicate/410 |
| **M1** | Server core: config, migrations, hooks (rev + change log), pull endpoint, `info` endpoint | M0 | Pull conformance passes; hook atomicity test passes |
| **M2** | Server push: validation, ownership, idempotency, conflict engine, `sync_conflicts` | M1 | Push + conflict conformance passes |
| **M3** | Server hardening: security suite, pruning, `410` paths, limits | M2 | Security suite green |
| **M4** | Client core: interfaces, in-memory adapter, outbox + coalescing, sync engine, rebase, resync | M0 | Passes conformance against a mock server; client unit tests green |
| **M5** | Expo SQLite adapter, PocketBase transport (auth, SSE nudge), schema mapping | M4 | Mapping round-trip tests pass; runs on device/simulator |
| **M6** | Convergence/fuzz harness (server + client together) | M3, M4 | Convergence test passes over many seeds |
| **M7** | Example Expo app, docs, README (scope/non-goals), compatibility matrix, release v0.1 | M5, M6 | Fresh-clone quickstart works end to end |

## 14. Notes for building with coding agents (Orca / Claude harness)

- **One worktree per work package.** M1–M3 (server) and M4–M5 (client) are independent after M0. M6 needs both.
- Treat `docs/protocol.md` and `conformance/` as **read-only contracts** for implementation tasks. Changes to them are separate, deliberate commits that all packages must rebase onto.
- Put in `AGENTS.md` / `CLAUDE.md`:
  - Test commands for each package (`go test ./...`, `pnpm test`), and that tests must pass before a task is considered complete.
  - The principles in §5 (server authority, no device clocks, security by default).
  - Rule: never bypass ownership checks; never add server-trusted fields from client input.
  - Rule: any behavior not in the protocol spec requires a spec change first.
  - Pinned PocketBase version range; do not use APIs outside it without checking the changelog.
- Have each agent task end with: tests added, conformance fixtures updated (if protocol-affecting), and a short note on decisions made.
- Review conflict logic, ownership checks, and the transaction/hook atomicity manually; these are the highest-risk areas.

## 15. Open questions (resolve during M0/M1)

1. **`rev` storage:** **Resolved** — side table `sync_meta` (protocol §17.1.1), retaining rows for deleted records so tombstone revs can be answered.
2. **Collection rules:** **Resolved** — ownership only in v1; `rule_denied` reserved (protocol §14.6, §17.1.2).
3. **Initial sync:** **Resolved** — snapshot plus a starting head cursor (protocol §7.3, §17.1.3).
4. **Hook transaction scope:** **Resolved** by the M0 spike — log writes MUST use the in-transaction `*Execute` hooks, not `*AfterSuccess` (protocol §8.5, §17.1.4; evidence `server/syncplugin/spike_test.go`).
5. **Unique constraints:** how to report and let clients resolve uniqueness violations discovered at push time (e.g., two devices creating the same unique slug).
6. **Relations:** ordering/dependency handling when a mutation references a record created in an earlier (possibly failed) mutation.
7. **Realtime nudge:** acceptable to rely on collection subscriptions, or ship a dedicated topic in v0.1?
8. **Existing ecosystem:** search GitHub and PocketBase discussions for existing sync plugins before building, to differentiate or collaborate. Re-verify RxDB's current licensing/maintenance state if it remains an alternative.
9. **Naming:** final project name; confirm availability.

## 16. Roadmap beyond v0.1

- File sync (resumable upload queue, content-addressed handling).
- Operation mutations for counters/balances.
- Shared/multi-user collections with per-record rule evaluation.
- Partial sync / filtered subscriptions.
- Web adapter, React hooks package, additional native clients.
- Dedicated sync SSE topic, compression, and delta pulls.
- Pluggable server-side merge hooks.
- Admin UI panel for conflicts and sync health.

## 17. Release and governance

- Semver; protocol version tracked separately from package versions and bumped only on breaking wire changes.
- Compatibility matrix in the README (PocketBase versions × plugin versions).
- MIT license; `CONTRIBUTING.md`, issue templates, `SECURITY.md`.
- Honest README: v0.1 scope, non-goals, and known limitations stated up front.
