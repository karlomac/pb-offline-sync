# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/). The sync **protocol version**
(`X-Sync-Protocol`) is tracked separately and changes only on breaking wire
changes.

## [Unreleased]

_Nothing yet._

## [0.1.0-rc.1] - 2026-10-22

### Added

- **Automated releases** (`docs/release-plan.md`,
  `.github/workflows/release.yml`): a tag-driven pipeline that validates the tag
  against this changelog, re-runs the Go suite, publishes the GHCR image,
  cross-compiled binaries with checksums, the `server/vX.Y.Z` Go module tag, and
  a GitHub Release with notes taken from this file.
- **Bare-metal deployment docs** (README Part 4): Linux systemd and Windows/NSSM
  services, cross-compilation, and the practical differences from the container
  (no entrypoint `chown`, `PB_ADMIN_*` unused outside it, TLS via a reverse proxy).
- **Media sync plan** (`docs/media-sync-plan.md`): design proposal for photo/video
  sync — a separate binary channel, content-hash dedup, resumable chunk uploads,
  immutable media records referenced by business records, S3-compatible storage.
- Deployable **default PocketBase build**: `server/cmd/pb-offline-sync` (stock
  PocketBase + the sync plugin, configured by `SYNC_*` environment variables)
  with a root `Dockerfile` and `docker-compose.yml` for Dokploy. The image
  follows the usual PocketBase-on-Dokploy `.env`: `PB_ADMIN_EMAIL` /
  `PB_ADMIN_PASSWORD` bootstrap the superuser on boot (`docker-entrypoint.sh`),
  `PB_DATA_PATH` bind-mounts the data dir, and `PB_VERSION` overrides the build
  (blank keeps the pinned, tested version). `.env.example` added; `.env` is
  gitignored.

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

### Changed

- Renamed to **`pb-offline-sync`**: GitHub repo `karlomac/pb-offline-sync`, Go
  module `github.com/karlomac/pb-offline-sync/server`, command
  `server/cmd/pb-offline-sync`, image `ghcr.io/karlomac/pb-offline-sync`. The npm
  scope stays `@pb-offline-sync/*`; the wire protocol (`X-Sync-Protocol`) and the
  `sync_*` tables are unchanged.
- `syncplugin.PluginVersion` is now a variable so release builds stamp the exact
  version with `-ldflags`; `/api/sync/info` reports it.

### Fixed

- Deployment: the shipped `docker-compose.yml` no longer publishes host port 8090
  (it uses `expose`, so Dokploy's Traefik reaches the container internally).
  Publishing it made every second app on the same host fail to start with
  `Bind for 0.0.0.0:8090 failed: port is already allocated`.

- Deployment: the entrypoint now repairs ownership of a root-owned bind-mounted
  data directory before dropping to the unprivileged `pb` user. Fixes the
  `unable to open database file (14)` crash-loop on Dokploy (bind mounts are
  created root-owned, but the server previously ran only as `pb`).

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

- Naming is settled: the project is `pb-offline-sync`, the Go module is
  `github.com/karlomac/pb-offline-sync/server` (`docs/ecosystem-scan.md`).
