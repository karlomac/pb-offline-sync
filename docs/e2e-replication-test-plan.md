# End-to-end replication test plan

**Version:** 1.0 · **Date:** 2026-11-07 · **Target branch:** `staging` (testbed)
**Never run load or destructive scenarios against `main`/production.**

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

## Appendix A — live replication harness (add as `client/transport-pocketbase/tests/e2e.replication.test.ts`)

```ts
/**
 * Opt-in live E2E suite. Skipped unless E2E_BASE_URL is set.
 * Prereq: e2e/up.sh RC-B (provisioned server on 127.0.0.1:8090).
 */
import { describe, expect, it } from "vitest";
import { MemoryStorageAdapter, SyncEngine } from "@pb-offline-sync/core";
import { PocketBaseTransport } from "../src/transport";

const BASE = process.env.E2E_BASE_URL;
const live = BASE ? describe : describe.skip;

const PASSWORD = "e2e-password-123";

function rid(n: number): string {
  return "r" + String(n).padStart(14, "0"); // 15 chars, [a-z0-9]
}

async function login(email: string): Promise<string> {
  const res = await fetch(`${BASE}/api/collections/users/auth-with-password`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ identity: email, password: PASSWORD }),
  });
  expect(res.status, `login ${email}`).toBe(200);
  return ((await res.json()) as { token: string }).token;
}

function makeEngine(token: string) {
  const storage = new MemoryStorageAdapter();
  const transport = new PocketBaseTransport({
    baseUrl: BASE!,
    token: { getToken: () => token },
  });
  const engine = new SyncEngine({ storage, transport });
  return { engine, storage, transport };
}

async function settle(engine: SyncEngine) {
  await engine.syncNow();
  const status = await engine.status();
  // Conflicts and rejected mutations legitimately set lastError; the run must
  // always finish idle instead of looping or staying offline.
  expect(status.state).toBe("idle");
  return status;
}

async function get(storage: MemoryStorageAdapter, collection: string, recordId: string) {
  return storage.transaction((tx) => tx.get(collection, recordId));
}

live("live replication", () => {
  it("R1 create replicates to a second device", async () => {
    const a = makeEngine(await login("alice@e2e.test"));
    const b = makeEngine(await login("bob@e2e.test"));
    const id = rid(1);
    await a.engine.applyLocal("create", "notes", id, { title: "hello" });
    await settle(a.engine);
    await settle(b.engine);
    expect((await get(b.storage, "notes", id))?.fields.title).toBe("hello");
  });

  it("R4 identical mutation is idempotent", async () => {
    const token = await login("alice@e2e.test");
    const mutation = {
      id: `replay-${Date.now()}`,
      collection: "notes",
      recordId: rid(2),
      op: "create",
      fields: { title: "once" },
    };
    const push = () =>
      fetch(`${BASE}/api/sync/push`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-Sync-Protocol": "1",
          Authorization: token,
        },
        body: JSON.stringify({ mutations: [mutation] }),
      }).then((r) => r.json() as Promise<{ results: Array<{ status: string; rev: number }> }>);

    const first = await push();
    const second = await push();
    expect(first.results[0].status).toBe("ok");
    expect(second.results[0].status).toBe("duplicate");
    expect(second.results[0].rev).toBe(first.results[0].rev);
  });

  it("R10 cross-user relation target is rejected", async () => {
    const alice = makeEngine(await login("alice@e2e.test"));
    const bobToken = await login("bob@e2e.test");
    const ownerRecord = rid(3);
    await alice.engine.applyLocal("create", "notes", ownerRecord, { title: "alice" });
    await settle(alice.engine);

    // Bob knows Alice's record id (ids are not secrets) and points at it.
    const res = await fetch(`${BASE}/api/sync/push`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Sync-Protocol": "1",
        Authorization: bobToken,
      },
      body: JSON.stringify({
        mutations: [{
          id: `rel-${Date.now()}`,
          collection: "notes",
          recordId: rid(4),
          op: "create",
          fields: { title: "bob", attachment: ownerRecord },
        }],
      }),
    }).then((r) => r.json() as Promise<{ results: Array<{ status: string; code?: string }> }>);

    expect(res.results[0].status).toBe("rejected");
    expect(res.results[0].code).toBe("validation_failed");
  });

  it("M1 upload, commit and read the media record", async () => {
    const token = await login("alice@e2e.test");
    const t = new PocketBaseTransport({ baseUrl: BASE!, token: { getToken: () => token } });
    const bytes = new Uint8Array(1024 * 1024).fill(7);
    const { createHash } = await import("node:crypto");
    const sha256 = createHash("sha256").update(bytes).digest("hex");
    const source = {
      collection: "media",
      sha256,
      size: bytes.byteLength,
      mime: "image/png",
      filename: "e2e.png",
      read: async (offset: number, length: number) => bytes.slice(offset, offset + length),
    };
    const { uploadMedia } = await import("@pb-offline-sync/core");
    const result = await uploadMedia(t, source);
    // A rerun of the same bytes legitimately returns the dedup hit.
    expect(["ok", "exists"]).toContain(result.status);
  });
});
```

> Media access control, quota, session cap and the remaining R/M/A scenarios are
> listed in §6–§8 as executable steps; extend this file with one `it()` per row.
> The deterministic Go tests (`go test ./syncplugin`) already cover the same
> invariants, so a failing live scenario should be reproduced there first.

## Appendix B — provisioning script (`e2e/provision.mjs`)

Run against the server in **plain** mode (no `SYNC_*`), before the RC-A/B/C start.

```js
// e2e/provision.mjs
const BASE = process.env.E2E_BASE_URL ?? "http://127.0.0.1:8090";
const ADMIN = {
  identity: process.env.E2E_ADMIN_EMAIL ?? "e2e-admin@example.com",
  password: process.env.E2E_ADMIN_PASSWORD ?? "e2e-admin-password",
};
const MAX_FILE = Number(process.env.E2E_MAX_FILE_BYTES ?? 52428800);
const LOAD_USERS = Number(process.env.E2E_LOAD_USERS ?? 0);
const PASSWORD = "e2e-password-123";

async function api(method, path, body, token) {
  const res = await fetch(BASE + path, {
    method,
    headers: { "Content-Type": "application/json", ...(token ? { Authorization: token } : {}) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${path} -> ${res.status}: ${text}`);
  return text ? JSON.parse(text) : null;
}

const su = (await api("POST", "/api/collections/_superusers/auth-with-password", ADMIN)).token;
const users = await api("GET", "/api/collections/users", undefined, su);

const autodate = [
  { name: "created", type: "autodate", onCreate: true },
  { name: "updated", type: "autodate", onCreate: true, onUpdate: true },
];

async function ensure(payload) {
  try {
    return await api("GET", `/api/collections/${payload.name}`, undefined, su);
  } catch {
    return await api("POST", "/api/collections", payload, su);
  }
}

const media = await ensure({
  name: "media", type: "base",
  fields: [
    ...autodate,
    { name: "sha256", type: "text" },
    { name: "size", type: "number", onlyInt: true },
    { name: "mime", type: "text" },
    { name: "filename", type: "text" },
    { name: "file", type: "file", maxSelect: 1, maxSize: MAX_FILE, protected: true },
    { name: "poster", type: "file", maxSelect: 1, maxSize: 8388608, protected: true },
    { name: "owner", type: "relation", collectionId: users.id, maxSelect: 1, cascadeDelete: false },
  ],
  listRule: "owner = @request.auth.id",
  viewRule: "owner = @request.auth.id",
  createRule: null, updateRule: null, deleteRule: null,
});

await ensure({
  name: "notes", type: "base",
  fields: [
    ...autodate,
    { name: "title", type: "text" },
    { name: "body", type: "text" },
    { name: "attachment", type: "relation", collectionId: media.id, maxSelect: 1, cascadeDelete: false },
    { name: "owner", type: "relation", collectionId: users.id, maxSelect: 1, cascadeDelete: false },
  ],
  listRule: null, viewRule: null, createRule: null, updateRule: null, deleteRule: null,
});

// Signup-enabled, verification-required users collection (auth scenarios).
const usersFull = await api("GET", `/api/collections/${users.id}`, undefined, su);
usersFull.createRule = "";
usersFull.authRule = "verified = true";
await api("PATCH", `/api/collections/${users.id}`, usersFull, su);

async function user(email, verified = true) {
  const filter = encodeURIComponent(`email="${email}"`);
  const found = await api("GET", `/api/collections/users/records?filter=${filter}`, undefined, su);
  if (found.items.length > 0) return found.items[0];
  return api("POST", "/api/collections/users/records", {
    email, password: PASSWORD, passwordConfirm: PASSWORD, verified,
  }, su);
}

await user("alice@e2e.test");
await user("bob@e2e.test");
for (let i = 0; i < LOAD_USERS; i++) await user(`load${i}@e2e.test`);

console.log(JSON.stringify({ ok: true, usersId: users.id, mediaId: media.id, loadUsers: LOAD_USERS }));
```

## Appendix C — Turnstile stub (`e2e/turnstile-stub.mjs`)

```js
// e2e/turnstile-stub.mjs — listens on 127.0.0.1:8787
import { createServer } from "node:http";

let mode = "ok"; // ok | fail | hostname
const hostname = "localhost";

createServer((req, res) => {
  if (req.url?.startsWith("/mode?value=")) {
    mode = new URL(req.url, "http://x").searchParams.get("value") ?? "ok";
    res.end("mode=" + mode);
    return;
  }
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const ok = mode !== "fail";
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({
      success: ok,
      hostname: mode === "hostname" ? "evil.example" : hostname,
      "error-codes": ok ? [] : ["invalid-input-response"],
    }));
  });
}).listen(8787, "127.0.0.1", () => console.log("turnstile stub on 127.0.0.1:8787"));
```

Set the stub mode with `curl 'http://127.0.0.1:8787/mode?value=hostname'`.

## Appendix D — k6 load script (`e2e/load.k6.js`)

```js
import http from "k6/http";
import { check } from "k6";

const BASE = __ENV.E2E_BASE_URL ?? "http://127.0.0.1:8090";
const USERS = Number(__ENV.E2E_LOAD_USERS ?? 10);
const PASSWORD = "e2e-password-123";

export const options = {
  scenarios: {
    push: { executor: "constant-vus", vus: 25, duration: "2m", exec: "pushLoop" },
    pull: { executor: "constant-vus", vus: 25, duration: "2m", exec: "pullLoop" },
  },
  thresholds: {
    http_req_failed: ["rate<0.001"],
    "http_req_duration{scenario:push}": ["p(95)<500"],
    "http_req_duration{scenario:pull}": ["p(95)<800"],
  },
};

export function setup() {
  const tokens = [];
  for (let i = 0; i < USERS; i++) {
    const res = http.post(
      `${BASE}/api/collections/users/auth-with-password`,
      JSON.stringify({ identity: `load${i}@e2e.test`, password: PASSWORD }),
      { headers: { "Content-Type": "application/json" } },
    );
    tokens.push(res.json("token"));
  }
  return { tokens };
}

const headers = (token) => ({
  "Content-Type": "application/json",
  "X-Sync-Protocol": "1",
  Authorization: token,
});

const rid = (n) => "l" + String(n).padStart(14, "0");

export function pushLoop(data) {
  const token = data.tokens[(__VU - 1) % data.tokens.length];
  const mutations = [];
  for (let i = 0; i < 10; i++) {
    mutations.push({
      id: `k6-${__VU}-${__ITER}-${i}`,
      collection: "notes",
      recordId: rid(__VU * 100000 + __ITER * 10 + i),
      op: "create",
      fields: { title: `vu${__VU}-iter${__ITER}-${i}` },
    });
  }
  const res = http.post(`${BASE}/api/sync/push`, JSON.stringify({ mutations }), {
    headers: headers(token),
  });
  check(res, {
    "push 200": (r) => r.status === 200,
    "push results": (r) => (r.json("results") ?? []).every((x) => x.status === "ok" || x.status === "duplicate"),
  });
}

const cursors = {}; // per-VU cursor: first call snapshots, then incremental

export function pullLoop(data) {
  const token = data.tokens[(__VU - 1) % data.tokens.length];
  const cursor = cursors[__VU];
  const url = `${BASE}/api/sync/pull?limit=200` + (cursor ? `&cursor=${encodeURIComponent(cursor)}` : "");
  const res = http.get(url, { headers: headers(token) });
  if (res.status === 200) cursors[__VU] = res.json("cursor");
  check(res, {
    "pull 200": (r) => r.status === 200,
    "pull shape": (r) => Array.isArray(r.json("changes")) && typeof r.json("hasMore") === "boolean",
  });
}
```

> k6 must see `E2E_LOAD_USERS` at run time; it is read in `setup()`. Keep rate
> limiting **disabled** for this run (see §4.1) or the 429s will dominate.

## Appendix E — bring-up/teardown (`e2e/up.sh`, `e2e/down.sh`)

```bash
#!/usr/bin/env bash
# e2e/up.sh <plain|RC-A|RC-B|RC-C>
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-8090}"
BASE="http://127.0.0.1:${PORT}"
WORK="${E2E_WORK_DIR:-/tmp/pb-e2e}"
DATA="$WORK/pb_data"
BIN="$WORK/pb-offline-sync"
MODE="${1:-RC-B}"
# Never inherit a developer's sync/auth environment into the rig.
unset $(env | grep -oE '^(SYNC|AUTH)_[A-Z0-9_]+' || true) 2>/dev/null || true

export PATH="$HOME/tools/go/bin:$PATH"
if [ "$MODE" = "plain" ]; then rm -rf "$WORK"; fi
mkdir -p "$DATA"

if [ ! -x "$BIN" ]; then
  (cd "$ROOT/server" && go build -o "$BIN" ./cmd/pb-offline-sync)
fi
"$BIN" superuser upsert e2e-admin@example.com e2e-admin-password --dir="$DATA" >/dev/null

start() { "$BIN" serve --http="127.0.0.1:${PORT}" --dir="$DATA" --dev >>"$WORK/server.log" 2>&1 & echo $! > "$WORK/server.pid"; }
wait_health() { for _ in $(seq 1 60); do curl -fsS "$BASE/api/health" >/dev/null 2>&1 && return 0; sleep 0.5; done; echo "server did not become healthy"; exit 1; }

if [ "$MODE" = "plain" ]; then
  start; wait_health
  E2E_BASE_URL="$BASE" E2E_LOAD_USERS="${E2E_LOAD_USERS:-0}" node "$ROOT/e2e/provision.mjs"
  kill "$(cat "$WORK/server.pid")"; wait "$(cat "$WORK/server.pid")" 2>/dev/null || true
  echo "provisioned"; exit 0
fi

export SYNC_COLLECTIONS=notes SYNC_OWNER_FIELD=owner SYNC_PRUNE_CRON=none
if [ "$MODE" = "RC-B" ] || [ "$MODE" = "RC-C" ]; then
  export SYNC_MEDIA_COLLECTION=media
  export SYNC_MEDIA_MAX_FILE_BYTES="${SYNC_MEDIA_MAX_FILE_BYTES:-52428800}"
  export SYNC_MEDIA_PER_USER_QUOTA="${SYNC_MEDIA_PER_USER_QUOTA:-104857600}"
fi
if [ "$MODE" = "RC-C" ]; then
  export AUTH_SIGNUP_MODE=password AUTH_COLLECTION=users
  export AUTH_TURNSTILE_SECRET=e2e-secret AUTH_TURNSTILE_HOSTNAMES=localhost
  export AUTH_TURNSTILE_VERIFY_URL=http://127.0.0.1:8787/siteverify
  export AUTH_ALLOWED_EMAIL_DOMAINS=example.com AUTH_DISPOSABLE_DOMAINS=mailinator.com
fi
start; wait_health; echo "server up: $BASE ($MODE)"
```

```bash
#!/usr/bin/env bash
# e2e/down.sh [--purge]
set -euo pipefail
WORK="${E2E_WORK_DIR:-/tmp/pb-e2e}"
[ -f "$WORK/server.pid" ] && kill "$(cat "$WORK/server.pid")" 2>/dev/null || true
[ "${1:-}" = "--purge" ] && rm -rf "$WORK"
echo "stopped"
```
