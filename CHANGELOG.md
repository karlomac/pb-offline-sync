# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/). The sync **protocol version**
(`X-Sync-Protocol`) is tracked separately and changes only on breaking wire
changes.

## [Unreleased]

### Added

- Deployable **default PocketBase build**: `server/cmd/pocketbase-sync` (stock
  PocketBase + the sync plugin, configured by `SYNC_*` environment variables)
  with a root `Dockerfile` and `docker-compose.yml` for Dokploy — mount a volume
  at `/pb/pb_data`, expose 8090, set `SYNC_COLLECTIONS`.

- Validation pass over the full scenario catalog (`docs/test-scenarios.md`): a
  Go **fixture-execution driver** that runs all 24 conformance fixtures against a
  live server, a `clientWins` convergence scenario, and the client
  lifecycle/push-status/adapter-reopen/transport test suites. No product bugs
  found; no source changes required.

- `docs/protocol.md` frozen at protocol **v1.0**: endpoints (`info`, `push`,
  `pull`), `rev`/`baseRev` concurrency, field-level merge, conflict and delete
  policies, idempotency, snapshot initial sync, error registry, security
  requirements, and the conformance fixture format.
- Gate 0 decisions in protocol §17: `rev` in a `sync_meta` side table, ownership
  as the only v1 authorization rule, snapshot+head-cursor initial sync, and the
  hook atomicity resolution.
- `conformance/` fixture format + 24 fixtures covering protocol §15.2 categories
  1–10, with a `$stale` cursor sentinel; validated by both the Go and TS suites.
- Repository scaffold: `server/` (Go module, PocketBase `v0.40.5`),
  `client/core`, `client/transport-pocketbase`, `client/adapter-expo-sqlite`,
  `examples/`, CI workflow.
- Hook-atomicity spike (`server/syncplugin/spike_test.go`) proving change-log
  writes must use the in-transaction `OnRecord*Execute` hooks.
- **M1 server core**: config validation, idempotent migrations (`sync_meta`,
  `sync_changes`, `sync_processed`, `sync_conflicts`), in-transaction change-log
  hooks, `GET /api/sync/info`, `GET /api/sync/pull` (owner-scoped, dedupe,
  snapshot + incremental, cursors, limits).
- **M2 server push**: `POST /api/sync/push` with per-mutation transactions, the
  §8 conflict/rev engine (all three conflict policies, both delete policies),
  §9 idempotency (payload hash, `duplicate`/`mutation_id_reuse`), ownership
  enforcement and no existence oracle, and body/batch limits.
- **M4 client core**: `StorageAdapter`/`Transport` seams, in-memory adapter and
  mock transport, outbox coalescing, and the sync engine (single-flight, backoff,
  rebase-on-pull, apply-by-rev idempotency, full resync, push-result handling).
- **M5 client transport + adapter**: PocketBase HTTP transport (auth + realtime
  nudge, error mapping) and the Expo SQLite adapter with the PDD §9.4 schema/type
  mapping (round-trip tested).
- **M3 server hardening**: change-log/idempotency retention with explicit,
  injectable, batched `Prune` (raising `MIN(seq)` so stale cursors become `410`),
  a `sync_changes.created` migration, an unresolved-conflict inspection helper,
  and a cross-user security suite (no existence oracle, ID-squatting
  indistinguishability, forged server fields, pull isolation).
- **M6 convergence harness**: a reference multi-device client model driving the
  real handlers under seeded random schedules (with offline buffering and
  duplicate delivery), asserting every device converges to server state, plus
  targeted proofs that acknowledged writes are not lost, duplicate delivery is
  idempotent, a prune-induced `410` resyncs correctly, and `editWins` preserves
  an edited record.

### Fixed

- Client: snapshot cleanup no longer deletes previously delivered records when a
  snapshot is resumed from a persisted cursor (only a fresh snapshot cleans up).
- Client: a `sent` (unacknowledged) create now blocks later same-record edits, so
  an edit can no longer be pushed with `baseRev: 0` and rejected.
- Server: `nextRev` shares `revFor`'s derivation, so a pre-plugin record's first
  tracked write no longer reuses the rev the snapshot advertised.
- Server: added indexes for the stale-base changed-field lookup
  (`sync_changes(collection, record_id, rev)`) and retention scans
  (`sync_changes(created)`, `sync_processed(created)`).
- Server: the pull path batches rev and record lookups (one query per collection
  instead of per row); the `sync_changes.created` migration backfills in bounded
  batches and repairs empty values.
- CI: the server job now fails on drift between `go.mod` and the matrix PocketBase
  version instead of silently rewriting the module graph.

### Notes

- The project and Go module path are provisional pending a name/availability
  decision (`docs/ecosystem-scan.md`).
