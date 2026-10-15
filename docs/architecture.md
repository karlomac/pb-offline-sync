# Architecture

Companion to `protocol.md` (the wire contract, source of truth) and
`pb-offline-sync-PDD.md` (scope and roadmap). This document records the concrete
server design decisions and the evidence behind them. Where this document and
`protocol.md` disagree, the protocol wins.

## Components

```
Client app ──► local DB (SQLite)                   Server: PocketBase + plugin
   │            tables: <collection>, _outbox,        routes: /api/sync/{info,push,pull}
   │            _sync_state, _conflicts               hooks:  record *Execute → sync_changes + rev
   └─► Sync engine ──► Transport (HTTP + SSE nudge)   tables: sync_meta, sync_changes,
                                                              sync_processed, sync_conflicts
```

- **Server (Go plugin, `server/syncplugin`)**: registers routes, runs idempotent
  migrations, and hooks configured collections so *every* write path (sync push,
  REST API, admin UI, other hooks) is captured.
- **Client core (TypeScript, `client/core`)**: platform-independent engine —
  outbox, sync loop, rebase, conflict handling — over `StorageAdapter` and
  `Transport` interfaces. No storage/transport/UI dependency.
- **Adapters (`client/adapter-expo-sqlite`)**: first storage adapter; the
  PocketBase transport lives with the client.

## Server data model

| Table | Purpose |
|---|---|
| `sync_meta` | `(collection, record_id)` → `rev`, `owner`. Side table (protocol §17.1.1); rows for deleted records are retained for the retained-history window so a tombstone `rev` can be answered. |
| `sync_changes` | `seq` (AUTOINCREMENT = commit order), `collection`, `record_id`, `op`, `owner`, `rev`, `changed_fields` (JSON), `created` (SQLite clock, used only for retention, never ordering). Index on `(owner, seq)`. |
| `sync_processed` | `(user, mutation_id)` → canonical payload hash + stored result JSON + `created`. Pruned after `idempotencyRetentionDays`. |
| `sync_conflicts` | collection, record, mutation ID, losing/overwritten fields, winning `rev`, `created`, `resolved`. |

`seq` (not `updated`) orders changes: timestamps tie and a transaction can commit
after a client has already pulled past its time. SQLite has a single writer, so
autoincrement order equals commit order.

## Retention

`Prune(app, cfg)` (explicit/injectable — the plugin starts no goroutine) deletes
`sync_changes` rows older than `Config.ChangeLogRetention` and
`sync_processed` rows older than `IdempotencyRetentionDays`, in bounded batches.
Pruning the oldest changes raises `MIN(seq)`, so a cursor pointing below it
returns `410 cursor_too_old` and the client resyncs (§7.4). Pruned history also
makes a stale-base changed-field lookup report incomplete coverage, so the merge
falls back to all fields rather than "no changes" (§8.2). `sync_meta` is never
pruned, so tombstone `rev`s are preserved. Unresolved conflicts are inspected via
`ListUnresolvedConflicts`.

## Hook transaction scope (Gate 0 G.4 — resolved)

**Evidence:** `server/syncplugin/spike_test.go`, run against PocketBase
**v0.40.5** (Go 1.27.2). Both tests pass.

- `On*AfterCreate/Update/DeleteSuccess` is **deferred until after the outer
  transaction commits** (`core/db.go` — `App.txInfo.OnComplete`). On rollback it
  is never called; on success its own writes are a separate post-commit
  transaction. It therefore **MUST NOT** be used for change-log writes.
- The **atomic** pattern is the in-transaction `OnRecordCreate/Update/DeleteExecute`
  hooks, appending `rev` + the change-log row through the event's tx-scoped
  `e.App` DB. Test proof: a forced rollback leaves neither the record nor the log
  row (0/0); a commit leaves both (1/1).

Consequence for the server: bind `*Execute` hooks for logging so §8.5's
requirement ("in the same transaction as the write; a rolled-back write MUST NOT
leave a log entry") holds.

## Request flow

**Push** (`POST /api/sync/push`): authenticate → for each mutation in order,
inside its own transaction: validate → idempotency lookup (`sync_processed`) →
ownership check / force owner on create → conflict engine (protocol §8) → apply
via the standard PocketBase save/delete path (so validation, unique constraints,
and `*Execute` hooks run) → store the final result → commit. `dependency_failed`
for later same-record mutations after a non-`ok` result.

**Pull** (`GET /api/sync/pull`): authenticate → decode cursor (stale/ahead →
`410`) → select `seq > cursor AND owner = user` ordered by `seq`, deduped to the
latest state per `(collection, record_id)` → load current records for upserts
(emit `delete` if gone) → return page + next cursor. Initial sync uses the
snapshot + head-cursor strategy (protocol §7.3).

## Determinism

Outcomes depend only on committed log order and data — never on device clocks or
request timing beyond arrival order (protocol §8.3, §11.6). The server assigns
`rev`, `seq`, `serverTime`, and ownership; client input cannot set them.

## Related documents

- `protocol.md` — wire contract (frozen v1.0).
- `threat-model.md` — security assets, threats, mitigations.
- `ecosystem-scan.md` — prior art and name-availability check.
- `../conformance/` — language-neutral fixtures both implementations must pass.
