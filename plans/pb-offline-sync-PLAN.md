# pb-offline-sync — Implementation Plan (v0.1)

Derived from `pb-offline-sync-PDD.md` (design) and `protocol.md` (wire contract).
No code yet. This is the plan of record until M0 freezes the protocol.

Plan file location: `plans/pb-offline-sync-PLAN.md` (the earlier copy at
`.orca/drops/pb-offline-sync-PLAN.md` is superseded).

---

## 0. Evaluation of the two documents

**Verdict:** the PDD and `protocol.md` are consistent and complementary. The
protocol is the stronger artifact (RFC 2119 language, error-code registry,
conformance fixture format). The PDD is a good scope/roadmap doc. Usable as-is
once the drift items below are fixed.

**Spec drift to reconcile before freezing** (protocol is authoritative where
they differ):

1. Push response: `merged` boolean exists in protocol §6.5 but is missing from
   the PDD §7.3 example — adopt `merged` in the PDD.
2. `deletePolicy` (`deleteWins`/`editWins`) is first-class in protocol §5/§8.4
   but absent from the PDD config example §8.1 — add it.
3. Idempotency payload-hash + `mutation_id_reuse`: protocol §9/§12.2, not in the
   PDD §8.4 — add to the PDD.
4. Deleted IDs are not reusable and are rejected `id_unavailable` (protocol
   §3/§6.3). The PDD §9.5 client ID generator does not state this rule; the
   client must persist a never-reuse tombstone set or rely on the server. Add.
5. PDD §7.3 pull response omits `mode` and `serverTime` present in protocol §7.1
   — protocol supersedes; note in the PDD.
6. `/info` field list (`collections[].fields[].writable`, file fields) is
   specified in protocol §5 but only vaguely in PDD §7.1 — align the wording.
7. Tombstone `rev`: protocol §17.1 requires the server to answer `rev` for
   deleted records within retained history. The PDD §8.2 `sync_meta` design does
   not say what happens to the row on delete. Decide: retain `sync_meta` rows
   (or serve tombstone `rev` from `sync_changes`).

**Highest-risk unknowns** (both docs already flag them — resolve, do not guess):

- **Hook transaction scope** (PDD §15.4 / protocol §17.4): whether change-log
  writes commit atomically with the data write in the targeted PocketBase
  versions. Gates the whole server design (§8.5). Must be proven by a spike.
- **`rev` storage** (PDD §15.1 / protocol §17.1): side table vs. field on each
  collection — affects joins, delete tombstones, and the "other write paths" hook.
- **Collection rules** (PDD §15.2 / protocol §17.2): ownership-only vs.
  evaluating PocketBase create/update/delete rules per mutation (`rule_denied`).
- **Initial-sync snapshot** (PDD §15.3 / protocol §7.3): snapshot + head cursor
  vs. log replay.
- **Unique constraints / relations / no-op rev** (PDD §15.5–15.6, protocol §17.5–17.7).

**Environment:** working dir is a git worktree of `/data/orca-home/orca/projects/pb-sync`
(`huchen` branch; sibling `spookfish` worktree exists — matches PDD §14
one-worktree-per-package). Node v22 present; **the Go toolchain is not installed
on PATH**, which blocks M1–M3. PocketBase version is not yet pinned.

---

## 1. Gate 0 — decisions before any implementation

These must be settled and written into `docs/protocol.md` / PDD. Everything else
rebases onto them (PDD §14: protocol + conformance are read-only contracts).

| # | Decision | Owner artifact |
|---|---|---|
| G.1 | Confirm project name availability (GitHub, Go module, npm) and no PocketBase endorsement implied | README |
| G.2 | Pre-build ecosystem scan of existing PocketBase sync plugins; re-check RxDB license | PDD §15.8 |
| G.3 | Pin supported PocketBase version range and confirm hook/router API | AGENTS.md, `go.mod` |
| G.4 | Hook transaction/atomicity spike result | note in protocol §8.5 |
| G.5 | `rev` storage choice (side table vs. field) | protocol §17.1 |
| G.6 | Collection-rule evaluation choice | protocol §17.2 |
| G.7 | Initial-sync strategy | protocol §7.3 |
| G.8 | Resolve §0 drift items 1–7 | PDD + protocol |

---

## 2. Milestones (from PDD §13, made concrete)

Contract-first: **M0 produces the frozen protocol + fixtures**; server and client
then run in parallel worktrees and only meet at M6.

### M0 — Docs relocation, protocol spec, conformance fixtures, repo scaffold, CI
- **Relocate docs first.** Create repo-root `docs/` and move every doc out of
  `.orca/drops/` into it:
  - `.orca/drops/protocol.md` → `docs/protocol.md`
  - `.orca/drops/pb-offline-sync-PDD.md` → `docs/pb-offline-sync-PDD.md`

  `protocol.md` becomes the frozen contract at `docs/protocol.md` (matches PDD
  §12). The plan file stays at `plans/pb-offline-sync-PLAN.md`.
- Apply Gate 0 resolutions to `docs/protocol.md`; add
  `docs/architecture.md`, `docs/threat-model.md`.
- Scaffold the PDD §12 layout: `server/`, `client/core/`,
  `client/adapter-expo-sqlite/`, `examples/`, `conformance/`, `AGENTS.md`,
  `LICENSE` (MIT), `SECURITY.md`, `README.md`, `CHANGELOG.md`.
- Define the conformance fixture format (protocol §15.1) and a runner skeleton
  that both Go and TS test suites consume.
- CI skeleton (`.github/workflows/`): lint/typecheck/test jobs, plus the
  PocketBase-version compatibility matrix job from the start.
- Write `AGENTS.md` with the PDD §14 rules (test commands, server-authority,
  never bypass ownership, spec-change-first).
- **Done when:** docs live under `docs/`; `protocol.md` reviewed; fixtures cover
  push/pull/conflict/duplicate/410; CI runs green on the empty scaffold.

### M1 — Server core: config, migrations, hooks, pull, info
- `syncplugin.MustRegister(app, Config)` (PDD §8.1): config validation against
  real schemas, fail fast.
- Idempotent migrations for `sync_meta`, `sync_changes`, `sync_processed`,
  `sync_conflicts` (PDD §8.2), per the G.5 decision.
- Hooks on configured collections: rev increment + changed-field diff + change
  log append **in the data transaction** (PDD §8.3).
- `GET /api/sync/info` (protocol §5) and `GET /api/sync/pull` (protocol §7:
  owner-scoped, dedupe, `hasMore`, `410`).
- **Done when:** pull conformance passes and the hook-rollback atomicity test
  passes (no log row on rollback).

### M2 — Server push: validation, ownership, idempotency, conflicts
- Push handler per PDD §8.4 / protocol §6 : ordering, per-mutation transaction,
  `dependency_failed`, ownership, forced owner on create, `sync_processed`
  idempotency with payload hash.
- Update/delete conflict engine (protocol §8): stale-base merge, three conflict
  policies, delete policies, writes to `sync_conflicts`.
- **Done when:** push + conflict + idempotency conformance passes.

### M3 — Server hardening: security, pruning, limits, 410
- Cross-user access suite (PDD §10 / protocol §14): forged owner, forged server
  fields, id squatting, no existence oracle.
- Pruning + oldest-retained tracking + ahead-of-head → `410` (protocol §13).
- Enforce `maxBatch` / `maxPage` / `maxBodyBytes`; rate-limit guidance.
- `SECURITY.md` disclosure process.
- **Done when:** security suite green.

### M4 — Client core (TS): interfaces, in-memory adapter, engine
- `StorageAdapter` / `Transport` interfaces (PDD §9.2); in-memory adapter + mock
  transport for fast tests.
- Local tables + schema mapping (PDD §9.3–9.4) with round-trip tests.
- Outbox with coalescing rules (PDD §9.5 / protocol §11.1); sync loop state
  machine, single-flight, backoff (PDD §9.6); rebase-on-pull (§11.3); full
  resync (§11.5); auth handling (§9.8).
- **Done when:** passes conformance against the mock server; client unit tests
  green.

### M5 — Expo SQLite adapter + PocketBase transport + schema mapping
- `@pb-offline-sync/adapter-expo-sqlite`; transport with auth + realtime nudge
  (payload ignored, protocol §10).
- **Done when:** mapping round-trip tests pass and it runs on device/simulator.

### M6 — Convergence/fuzz harness
- N simulated clients, random edits, offline periods, duplicated/reordered/
  dropped requests, concurrent server-side edits.
- **Done when:** all clients + server converge; no acknowledged write lost except
  as recorded in `sync_conflicts` (PDD §11.3 — the key correctness proof).

### M7 — Example app, docs, compatibility matrix, v0.1 release
- `examples/expo-notes`, README with scope/non-goals up front, compatibility
  matrix, release v0.1.
- **Done when:** fresh-clone quickstart works end to end against a real
  PocketBase binary.

---

## 3. Parallelization
- After M0: **M1→M2→M3** (server worktree) and **M4→M5** (client worktree) are
  independent. M6 needs M3 + M4.
- Protocol + conformance fixtures are read-only contracts; changes are separate
  deliberate commits that all worktrees rebase onto (PDD §14).

## 4. Cross-cutting (apply in every milestone)
- Manual review focus: conflict logic, ownership checks, hook/transaction
  atomicity (PDD §14).
- Every task ends with tests added, conformance fixtures updated if
  protocol-affecting, and a short decisions note.
- Client and server must never trust device clocks (PDD §5.3 / protocol §11.6).

## 5. Out of scope for v0.1 (do not build)
Shared/multi-user records, partial/filtered sync, file content sync, operation
mutations (counters), CRDTs, server-side merge hooks, CouchDB-protocol compat,
PocketBase-on-device (PDD §3 / protocol §1).

## 6. Immediate next steps
1. **Move docs:** `mkdir -p docs && git mv .orca/drops/protocol.md
   .orca/drops/pb-offline-sync-PDD.md docs/` (requires an implementation-capable
   agent; Plan Mode cannot move files).
2. Resolve Gate 0 G.1–G.3 (naming, ecosystem scan, PocketBase pin) and install
   the Go toolchain in the build environment.
3. Run the hook-atomicity spike (G.4) — it is the only unknown that can force a
   server redesign.
4. Apply §0 drift fixes, then execute M0.
