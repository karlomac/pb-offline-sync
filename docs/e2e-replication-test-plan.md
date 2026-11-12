# End-to-end replication test plan

**Version:** 1.0 · **Date:** 2026-11-07 · **Target branch:** `staging` (testbed)
**Never run load or destructive scenarios against `main`/production.**

## 0. Quick start on a blank Ubuntu 22.04 VPS

```bash
git clone -b staging https://github.com/karlomac/pb-offline-sync.git
cd pb-offline-sync
sudo ./e2e/bootstrap.sh
```

The bootstrap installs Go 1.27.3, Node 22.23.4, pnpm 12, k6 v2.3.0 and sqlite3,
builds the server binary, provisions the schema, then runs L0, L1, the live
sync/media suite, the auth-gate suite and the load/soak phase. It binds
`127.0.0.1` only, uses `/tmp/pb-e2e`, writes `artifacts/<timestamp>/`, and exits
non-zero if any phase fails. Useful flags: `--skip-install` (reuse the
toolchain), `--skip-load`, `--skip-vuln`, `--race`, `--load-duration 2m`,
`--soak-seconds 3600`, `--load-users 50`, `--keep`, `--dev` (verbose PocketBase
dev mode — off by default so the run matches production behaviour).

---

## 1. Purpose and scope

This plan defines how to prove, repeatedly and from a clean checkout, that the
sync layer **replicates correctly end to end** between multiple client engines
and a real PocketBase server, under normal operation, failure, and load.

It covers:

- the deterministic suites that already exist in the repository (fast, run on
  every change);
- a reproducible **live two/three-device replication harness** against the real
  server binary over HTTP;
- the media upload channel (resume, dedup, quota, access control);
- the auth signup gate;
- failure injection (restarts, dropped requests, pruned cursors, disk pressure);
- load and soak testing with explicit budgets;
- security regression checks derived from `docs/security-audit-2026-10-29.md`.

Out of scope: multi-node/clustered PocketBase (unsupported by the data model),
a real OAuth2 provider, the Expo-native client capture flow, and the deferred
presigned-upload (MED-6) design.

### Definitions

- **Replication**: convergence of every device's local state to the server
  truth, with no lost writes and no duplicate effects under retries.
- **Device**: one `SyncEngine` + one `StorageAdapter` + one auth identity.
- **Server truth**: a fresh `pull` snapshot (mode `snapshot`) for the user.
- **Pass**: every assertion in the scenario holds and the run artifact is saved.

---

## 2. System under test

| Component | Location | Notes |
|---|---|---|
| Server binary | `server/cmd/pb-offline-sync` | PocketBase v0.40.6 + sync/media/auth plugins |
| Sync plugin | `server/syncplugin` | routes `/api/sync/{info,push,pull}` and media channel |
| Auth plugin | `server/authplugin` | optional signup gate |
| Client engine | `client/core` | `SyncEngine`, `MemoryStorageAdapter` |
| Transport | `client/transport-pocketbase` | HTTP + media channel |
| Conformance fixtures | `conformance/fixtures/*.json` | executable protocol contract |

Pinned toolchain: Go 1.27.x, Node 22.x, pnpm 12.x. `sqlite3` is needed only for
the cursor-expiry scenario and for collecting metrics.

---

## 3. Test levels

| Level | What it proves | Runs in | Runtime |
|---|---|---|---|
| **L0** | Compile, vet, race, dependency scan | CI / local | 2–4 min |
| **L1** | Unit + transactional + protocol conformance (34 fixtures executed) | CI / local | ~1 min |
| **L2** | Live single-server API smoke (unauth, protocol errors, one push/pull) | this plan | 1 min |
| **L3** | Live multi-device replication + media + auth | this plan | 5–10 min |
| **L4** | Load, soak, failure injection | this plan | 30–90 min |

A release candidate is *tested* only when L0–L3 pass and L4 has a recorded run
with budgets met or deviations accepted in writing.

### 3.1 L0/L1 commands (existing, no setup)

```bash
# Server
cd server
go build ./...
go vet ./...
go test ./... -count=1                 # includes 34/34 conformance fixtures
go test -race ./syncplugin/ -count=1   # concurrency-sensitive code
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Clients (each package is self-contained)
for p in core transport-pocketbase adapter-expo-sqlite; do
  (cd "client/$p" && pnpm install --frozen-lockfile && pnpm run typecheck && pnpm test)
done
```

Exit criteria L0/L1: all commands exit 0; the fixture driver prints
`executed 34/34`.

---

## 4. Test rig (L2–L4)

```
        ┌──────────────┐   HTTP    ┌─────────────────────────────┐
        │ engine A     │──────────►│  pb-offline-sync (PocketBase│
        │ engine B     │◄──────────│  + sync/media/auth plugins) │
        │ engine C     │           │  SQLite: pb_data/data.db    │
        └──────────────┘           └─────────────────────────────┘
             Node/vitest                    127.0.0.1:8090
```

- **Isolation**: a disposable data dir (`/tmp/pb-e2e`), a dedicated port, and a
  loopback bind. Never reuse a real `pb_data` and never bind `0.0.0.0`.
- **Determinism**: record ids are fixed per test (`r00000000000001` …); only the
  server assigns `rev`/`seq`; the client clock is never used for ordering.
- **Evidence**: every run writes `artifacts/<run-id>/` with the server log, the
  k6 summary, DB/WAL sizes and the client status dump.

### 4.1 Environment variables used by the server

| Variable | Value used in this plan |
|---|---|
| `SYNC_COLLECTIONS` | `notes` (comma-separated; each must exist with an `owner` relation) |
| `SYNC_OWNER_FIELD` | `owner` |
| `SYNC_MEDIA_COLLECTION` | `media` (optional; leave empty to test sync only) |
| `SYNC_MEDIA_MAX_FILE_BYTES` | `52428800` (50 MiB) |
| `SYNC_MEDIA_PER_USER_QUOTA` | `104857600` (100 MiB); `1048576` for the quota scenario |
| `SYNC_MEDIA_MIME_ALLOWLIST` | default (`image/`, `video/`, `audio/`) |
| `SYNC_PRUNE_CRON` | `none` (so maintenance never races a scenario) |
| `AUTH_SIGNUP_MODE` | empty for replication runs; `password` for auth runs |
| `AUTH_COLLECTION` | `users` |
| `AUTH_TURNSTILE_SECRET` | `e2e-secret` |
| `AUTH_TURNSTILE_HOSTNAMES` | `localhost` |
| `AUTH_TURNSTILE_VERIFY_URL` | `http://127.0.0.1:8787/siteverify` (local stub) |
| `AUTH_ALLOWED_EMAIL_DOMAINS` | `example.com` (auth run only) |
| `AUTH_DISPOSABLE_DOMAINS` | `mailinator.com` (auth run only) |

The binary takes `--http` and `--dir`; `PB_DATA_DIR`/`PB_ADMIN_*` are read by the
container entrypoint, not by the binary. The superuser is created with the
PocketBase CLI (`superuser upsert`).

### 4.2 Run configurations

The plugin validates collections at boot, so provisioning happens **before** the
sync-enabled start. Three configs cover every scenario:

| Config | Extra env | Scenarios |
|---|---|---|
| **RC-A** sync-only | `SYNC_COLLECTIONS=notes` | R1–R12, load push/pull |
| **RC-B** sync+media | `SYNC_COLLECTIONS=notes`, `SYNC_MEDIA_COLLECTION=media` | M1–M9, media load |
| **RC-C** sync+media+auth | RC-B + `AUTH_SIGNUP_MODE=password`, domain lists | A1–A6 |

Bring-up (two-phase, because the admin API needs a running server):

```bash
# e2e/up.sh — see Appendix E for the full script
e2e/up.sh plain        # start plain PocketBase, provision, stop (writes /tmp/pb-e2e/provisioned)
e2e/up.sh RC-B         # start with RC-B env, wait for /api/health
```

Reset: `e2e/down.sh --purge` stops the process and deletes `/tmp/pb-e2e`.

### 4.3 Remote run against a deployed instance (e.g. Dokploy)

```bash
E2E_ADMIN_EMAIL=... E2E_ADMIN_PASSWORD=... \
  ./e2e/bootstrap.sh --remote https://test-app.example
```

- Skips install/local build/L0/L1; provisions the remote schema (unless
  `--no-provision`), then runs the live sync/media and auth suites against the
  deployed instance.
- Provisioning **mutates** the target (creates `notes`/`media`, sets
  `users.createRule`/`authRule`). Use a test instance only. For the first
  provision, deploy with `SYNC_*`/`AUTH_*` unset, run the remote command with
  `--no-provision` after the plugin-enabled redeploy, or provision once with the
  plain instance.
- The auth suite needs a Turnstile verifier the **server** can reach over
  **https** (plain `http` is allowed only for loopback); deploy the stub behind a
  Dokploy domain, or pass `--skip-auth`.
- Load run from the operator's machine measures the WAN (e.g. Germany → Singapore
  ≈ 200 ms RTT) — use `--skip-load` for correctness-only runs, or run k6
  co-located with the app for capacity numbers.
- Both suites are rerun-safe against a persistent instance (run-scoped record ids
  and media content).
- Configure S3/R2 in PocketBase settings before the run and keep the bucket
  private.

Operational note: PocketBase has no switch to disable the admin UI. Restrict it
with `superuserIps` (Settings → superusers IPs) and/or a proxy rule that blocks
`/_/` on test deployments.

---

## 5. L2 — live smoke

| ID | Action | Expected |
|---|---|---|
| S1 | `GET /api/health` | `200` |
| S2 | `GET /api/sync/info` without token | `401` |
| S3 | `GET /api/sync/info` with a user token | `200`, `protocol=1`, `limits` present, `collections.notes` |
| S4 | `POST /api/sync/push` with `X-Sync-Protocol: 2` | `400 protocol_unsupported` |
| S5 | Push create → pull snapshot | mutation `ok` with `rev=1`; pull returns the record |
| S6 | `POST /api/sync/push` with a file field | `rejected` / `file_field_unsupported` |
| S7 | `POST /api/sync/push` setting `owner`/`id`/`created` | `rejected` / `forbidden_field` |

Record ids are 15 lowercase alphanumerics. Every request needs
`X-Sync-Protocol: 1` and `Authorization: <token>`.

---

## 6. L3 — deterministic scenario catalogue (RC-A)

Run with the opt-in vitest harness (Appendix A):

```bash
cd client/transport-pocketbase
E2E_BASE_URL=http://127.0.0.1:8090 pnpm vitest run tests/e2e.replication.test.ts
```

| ID | Scenario | Steps | Pass criteria |
|---|---|---|---|
| R1 | Create and replicate | A creates `r…01`, syncs; B syncs | B stores the record with the same `rev` and fields |
| R2 | Server-wins conflict | A create+sync; B sync; A update (rev 2); B update from stale rev 1 | Server keeps A's value; B records a conflict; B converges to A's value |
| R3 | Delete-wins | A create+sync; B sync; A delete+sync; B updates the stale record | Server keeps the tombstone; B records a conflict and removes the record |
| R4 | Idempotent replay | Repeat the same mutation id | First `ok`, second `duplicate` (same rev); no second change row |
| R5 | Cursor semantics | Fresh pull → pull with returned cursor → tampered cursor | `snapshot` → `incremental`; tampered cursor `400 cursor_invalid` |
| R6 | Three-device convergence | A/B/C each apply 20 mixed ops, sync in random rounds ×2 | All three storages equal the server snapshot (ids, revs, fields) |
| R7 | Batch and page limits | Push 101 mutations; pull `limit=1000` | `101` → `400 batch_too_large`; pull clamps to `maxPage` |
| R8 | Protocol errors | Bad JSON, unknown collection, unknown op, missing `baseRev` | `400 invalid_request` / per-mutation `rejected` codes |
| R9 | Ownership isolation | B pulls after A writes; B pushes an update to A's record | B sees nothing; mutation is `rejected` with a not-found-style code |
| R10 | Relation ownership | B creates a record whose relation points at A's record | `rejected` / `validation_failed` |
| R11 | Rate limiting | Enable a rate-limit rule for `POST /api/sync/push` (30/min, `@auth`), burst 40 | Excess requests `429` with `Retry-After`; client treats it as backoff. If the PB middleware does not cover the plugin route, enforce it at the proxy and record that deviation |
| R12 | Restart durability | `kill -9` after push returns, restart, pull | Record present exactly once; change log intact |

Additional checks executed by the harness:

- **R5b cursor expiry (410)**: age the change log and prune; the old cursor must
  return `410 cursor_too_large`/`cursor_too_old` and the engine must resync via a
  snapshot without data loss. Requires `sqlite3` and the prune helper
  (library-level test `TestResyncOn410AfterPrune` covers the same behaviour
  deterministically; the live variant is optional).
- **R13 maintenance**: `go test ./syncplugin -run 'TestMediaGC|TestMediaStaging|TestPrune'`
  is the deterministic reference; live cron runs on the binary's schedule
  (30 min sweep, nightly GC) and is not part of the timed run.

---

## 7. L3 — media scenarios (RC-B)

Upload bytes are generated in-process from a seeded buffer; `sha256` is computed
with `node:crypto`. The harness uses the transport's media methods
(`mediaInit` → `mediaAppend` → `mediaCommit`) so it exercises the real client.

| ID | Scenario | Steps | Pass criteria |
|---|---|---|---|
| M1 | Upload + commit | 1 MiB PNG-like blob, init → chunks of `chunkSize` → commit | `ok`, `mediaId`, `rev=1`; record has `sha256/size/mime/filename/file` |
| M2 | Dedup | Init the same content again | `status:"exists"` with the same `mediaId`; no new record |
| M3 | Resume | Init, upload half, `HEAD`, upload rest, commit | `Upload-Offset` equals bytes sent; commit `ok` |
| M4 | Hash mismatch | Init with a wrong `sha256`, upload, commit | `400 hash_mismatch`; session discarded |
| M5 | Incomplete commit | Init, upload half, commit | `400 invalid_request` |
| M6 | Quota | Restart with `SYNC_MEDIA_PER_USER_QUOTA=1048576`; upload 2 MiB | `403 quota_exceeded` at `init` |
| M7 | Session cap | 17 inits with distinct content | First 16 accepted; 17th `429 rate_limited`; abort all sessions |
| M8 | Access control | Owner fetches the file via `/api/files/token`; stranger fetches without a token | Owner `200`; stranger `403/404`, never `200` |
| M9 | Browse isolation | A uploads; B lists `/api/collections/media/records` and pulls | B sees none of A's media records or files |
| M10 | Push immutability | Push create/update/delete on `media` | `rejected` / `media_immutable` |

Media file URL: `GET /api/files/media/{recordId}/{filename}`; a short-lived token
comes from `POST /api/files/token` and is passed as `?token=`. File fields must
be `protected` and the collection's list/view rules owner-scoped (the
provisioning script does both); with those in place a stranger gets a non-2xx.

---

## 8. L3 — auth signup gate (RC-C)

The Turnstile verifier is stubbed on loopback (Appendix C). The stub can be put
in `fail` or `hostname` mode through `/mode?value=`.

| ID | Scenario | Steps | Pass criteria |
|---|---|---|---|
| A1 | Gated signup | `POST /api/collections/users/records` without a token | `400` |
| A2 | Successful signup | Same body + `turnstileToken:"ok"` | `200`, record created |
| A3 | Unverified cannot sign in | `auth-with-password` before verification | non-2xx (auth rule `verified = true`) |
| A4 | Verification unblocks | Admin sets `verified=true`, retry login | `200` + token |
| A5 | Domain allowlist / disposable | `@example.com` vs `@mailinator.com` vs `@x.mailinator.com` | allowed; blocked; blocked (subdomain match) |
| A6 | Wrong hostname | Stub returns `hostname:"evil.example"` | `400` (hostnames are configured) |
| A7 | Misconfiguration fails closed | Restart with `AUTH_COLLECTION=members` (does not exist) | process exits non-zero with an actionable message |

OAuth2 live signup is provider-dependent and therefore **not** in this plan; the
gate's OAuth context is covered by `TestSignupThroughRouter` and the unit suite.

Without `--dev` and without SMTP, the verification send fails asynchronously and
is logged (`failed to send verification email`); the signup itself still succeeds
and the suite verifies the account through the admin API, not the mailbox.

---

## 9. L4 — load and soak

### 9.1 Workload model

| Dimension | Value |
|---|---|
| Users | 50 (provisioned by `E2E_LOAD_USERS=50`) |
| Push VUs | 25, 10 mutations/batch, 2 min |
| Pull VUs | 25, page 200, 2 min |
| Media VUs (optional) | 10, 1 MiB × 5 chunks each |
| Record ids | deterministic 15-char ids per VU/iteration |
| Rate limiting | **disabled** for load; enabled only for R11 |

SQLite has a single writer, so push throughput is the expected ceiling; the
purpose of L4 is to measure it and prove no corruption/no loss, not to reach a
number.

### 9.2 k6 run

```bash
E2E_BASE_URL=http://127.0.0.1:8090 E2E_LOAD_USERS=50 \
  k6 run e2e/load.k6.js
# container:
docker run --rm --network host -i grafana/k6 run - < e2e/load.k6.js
```

Budgets (loopback, 4 vCPU class host):

| Metric | Budget |
|---|---|
| `http_req_failed` | < 0.1% |
| push duration p95 / p99 | < 500 ms / < 1500 ms |
| pull duration p95 / p99 | < 800 ms / < 2000 ms |
| server RSS after run | < 500 MiB |
| DB growth | linear with mutations; no runaway WAL |
| lost mutations | 0 (compare `sync_meta`/`sync_changes` counts vs. successful pushes) |

Any budget miss is recorded with the observed value; it blocks a release only if
it indicates incorrectness (errors, loss, corruption), not merely slowness.

### 9.3 Soak (correctness over time)

`E2E_SOAK_SECONDS=3600 pnpm vitest run tests/e2e.soak.test.ts` (Appendix A,
optional file): 10 engines, 10 ops/s total, periodic syncs, random restarts of
one engine, final convergence assertion. Pass: no lost writes, no duplicate
records, all engines converged, RSS growth < 20% over the hour.

### 9.4 Evidence to collect

```bash
ps -o rss=,pcpu= -p "$(pgrep -f pb-offline-sync)"
ls -lh /tmp/pb-e2e/pb_data/data.db*
sqlite3 /tmp/pb-e2e/pb_data/data.db \
  'select (select count(*) from sync_changes)  as changes,
          (select count(*) from sync_meta)     as meta,
          (select count(*) from sync_processed) as processed,
          (select count(*) from sync_conflicts) as conflicts;'
```

Save the k6 summary JSON, the server stdout/stderr, the DB sizes and the engine
status dumps under `artifacts/<run-id>/`.

---

## 10. Failure injection

| ID | Failure | Method | Expected |
|---|---|---|---|
| F1 | Server process dies mid-sync | `kill -9` during a push | Client retries the same mutation ids; after restart no loss/duplication (`duplicate`) |
| F2 | Dropped responses | Wrapper fetch that fails every 3rd request | Engine retries; final state converges; no duplicate records |
| F3 | Partition then heal | `iptables`/`tc netem` or wrapper with a 30 s outage | Engine reports `offline`, then converges |
| F4 | Cursor pruned during pull | Age rows + `Prune` with 1 h retention (RC-A) | `410`, engine resyncs via snapshot, converges |
| F5 | Disk pressure | Run `pb_data` on a small tmpfs | Push returns an error; no partially written record/change row |
| F6 | Concurrent edit | Two engines update the same record in the same second | Exactly one winner per policy; loser records a conflict; both converge |
| F7 | Duplicate mutation id | Same id, different payload | `rejected` / `mutation_id_reuse` |
| F8 | Clock skew | Client `now` set ±1 day | No effect on `rev`/`seq` ordering |

---

## 11. Security regression checks (from the 2026-10-29 audit)

| Check | Expected | Scenario |
|---|---|---|
| Cross-user record read | New user pulls only own records | R9 |
| Cross-user relation target | `validation_failed` | R10 |
| Media file without token | Not `200` | M8 |
| Media mutation via push | `media_immutable` | M10 |
| Media field protection | `file`/`poster` are `protected`; rules locked | provisioning + M8 |
| Signup gate bypass (batch) | Batch create without a token is rejected | A1 variant |
| Renamed auth collection | Server refuses to start | A7 |
| Dependency advisory | `govulncheck` clean | L0 |

---

## 12. Traceability

| Protocol | Scenarios |
|---|---|
| §4 record/field rules | S6–S7, R8 |
| §6 push (batch, idempotency) | R1, R4, R7, F1, F7 |
| §7 pull (snapshot/incremental/cursor) | R1, R5, R12 |
| §8 revs and change log | R2, R3, R12 |
| §9 idempotency retention | R4, F4 |
| §10 nudge | L1 (unit), L4 (client soak) |
| §11 engine semantics | R2, R3, R6, F2, F3 |
| §14 security | R9, R10, M8, M10, A1–A7 |
| §18 media | M1–M10 |

---

## 13. Reporting template

```markdown
## Run <id> — <date>

- Commit / branch: <sha> / <branch>
- Config: RC-A | RC-B | RC-C        Host: <cpu/mem/os>
- L0/L1: pass/fail (attach output)
- L2: <n>/7
- L3: <n>/<n>, failures: R?, M?, A?
- L4: k6 summary (p95/p99/errors), RSS, DB/WAL, soak convergence
- Deviations: <none | list with observed numbers and why accepted>
- Artifacts: artifacts/<run-id>/
- Verdict: PASS | PASS WITH DEVIATIONS | FAIL
```

---

## 14. Known limitations

- The harness uses `MemoryStorageAdapter`; SQLite-specific behaviour of
  `client/adapter-expo-sqlite` is covered by its own suite, not live E2E.
- Media TTL/GC grace are 24 h (not env-configurable); live time-based
  maintenance is validated by the library tests, not by waiting.
- OAuth2 and email delivery need external providers/SMTP; the gate logic is
  covered by unit + router tests.
- Single-node only. PocketBase's SQLite design does not support horizontal
  scaling, so no cluster scenario exists.

---

> **Appendices A–E are the executables this plan requires.** They are provided
> verbatim so a run can be reproduced from this document; adding them does not
> change production code (the test file is opt-in and skipped unless
> `E2E_BASE_URL` is set).

## Appendix — files in this repository

The executables referenced above are committed; run them via the bootstrap or
directly:

| Path | Purpose |
|---|---|
| `e2e/bootstrap.sh` | installs the toolchain and runs every phase; the entry point |
| `e2e/provision.mjs` | creates `notes` (locked) and `media` (protected file fields, owner-scoped rules, locked writes), test users, and the signup-enabled/verification-required `users` collection |
| `e2e/turnstile-stub.mjs` | loopback verified stub for the auth gate (`/mode?value=ok\|fail\|hostname`) |
| `e2e/load.k6.js` | k6 push/pull workload with per-VU incremental cursors |
| `client/transport-pocketbase/tests/e2e.replication.test.ts` | live R1–R6, R9, M1–M5, M8, M10 (opt-in via `E2E_BASE_URL`) |
| `client/transport-pocketbase/tests/e2e.auth.test.ts` | live A1–A6 against RC-C |

Coverage note: R7, R8, R11–R13 and R5b are covered deterministically by
`go test ./syncplugin/` (limits, protocol errors, cursor expiry) or require a
privileged/proxy runner (rate limiting), so they are not part of the default
bootstrap run. F3 (real partition) and F5 (disk full) need root and are listed
as manual/privileged; F1/F2/F4/F6/F7/F8 are covered by the crash/retry and
conflict scenarios plus the engine unit suite.

Measured baseline on a shared 2-vCPU sandbox (loopback, 5 users, 25 push + 25
pull VUs, 10 s): 30,385 requests, 0 errors, pull p95 ≈ 37 ms, **push p95 ≈ 5 s**
— SQLite has a single writer, so push throughput is the expected ceiling. Treat
the §9.2 latency budgets as targets for a dedicated runner, not as a gate on a
noisy host.
