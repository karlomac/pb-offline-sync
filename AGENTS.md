# AGENTS.md

Conventions for humans and coding agents working in this repository.

## What this is

`pb-offline-sync` — a two-way, offline-first sync layer between a client-local
database and a server-side PocketBase. **Protocol first:** `docs/protocol.md` is
the frozen v1.0 contract and the source of truth; code follows it.

## Branches and environments

- **`main` is production.** A live app runs from it. Do **not** merge `staging`
  into `main`, do not target `main` with feature PRs, and do not force-push or
  rewrite it. Promotion to production is a deliberate maintainer decision.
- **`staging` is the testbed** and a mirror of production plus features under
  test (media sync, auth signup gate). Feature/fix PRs target `staging`; it may
  be deployed to a test environment only.
- There is currently **no planned `staging → main` merge**. Do not assume a
  release train, and do not describe testbed fixes as "live" or "in
  production".
- CI runs on both branches; tags/releases remain a maintainer action.

## Read before changing anything

- `docs/protocol.md` — the wire contract (frozen v1.0).
- `docs/pb-offline-sync-PDD.md` — scope, non-goals, roadmap.
- `docs/architecture.md` — server design and the resolved Gate 0 decisions.
- `docs/threat-model.md` — security requirements.
- `docs/test-scenarios.md` — the scenario catalog for both sides, with coverage
  status and the prioritised gaps.
- `docs/media-sync-plan.md` — photo/video sync. Shipped on `staging` as the
  additive protocol v1.1 extension (`docs/protocol.md` §18); still under test,
  not in production.

## Repository layout

```
docs/            protocol.md (contract), PDD, architecture, threat-model, ecosystem-scan
conformance/     language-neutral JSON fixtures + format; both implementations consume them
server/          Go module: the PocketBase plugin (package syncplugin)
client/core/     TypeScript engine (no platform deps)
client/adapter-expo-sqlite/   first storage adapter
examples/        example apps (later)
```

## Build and test commands

- Server (Go, from `server/`): `go build ./...` · `go vet ./...` · `go test ./...`
- Client (Node/pnpm, one self-contained package per directory under `client/`:
  `core`, `transport-pocketbase`, `adapter-expo-sqlite`): from each package
  directory run `pnpm install` · `pnpm run typecheck` · `pnpm test`.
- Conformance fixtures are validated by both test suites; a green run means both
  sides agree with `docs/protocol.md`. Run every touched package's tests before
  declaring a task complete.

**A task is not complete until its tests pass.** Run the commands above for the
packages you touched.

## Pinned versions

- **PocketBase:** `v0.40.5` (verify a supported range before widening; check the
  upstream changelog before using any API outside it).
- **Go:** 1.27.x (module `github.com/karlomac/pb-offline-sync/server`).
- **Node:** 22.x.

The project name and Go module path are settled: `pb-offline-sync` and
`github.com/karlomac/pb-offline-sync/server` (see `docs/ecosystem-scan.md`).

## Non-negotiable rules

1. **Never bypass ownership checks.** Owner is set by the server from the
   authenticated user and is never trusted from client input.
2. **Never add server-trusted fields from client input.** `id`, owner, `created`,
   `updated`, `collectionId`, `expand` are server-authoritative.
3. **Any behaviour not in `docs/protocol.md` requires a spec change first.**
   `docs/protocol.md` and `conformance/` are read-only contracts for feature
   work; changes to them are separate, deliberate commits that all worktrees
   rebase onto.
4. **No device clocks** for ordering or conflict resolution — the server assigns
   `rev`, `seq`, and timestamps.
5. **Change-log writes are atomic** with the data write, performed from the
   in-transaction `OnRecord*Execute` hooks — never from `*AfterSuccess` hooks
   (see `docs/architecture.md` and `server/syncplugin/spike_test.go`).
6. **Logging:** do not log record contents at default log levels.

## Every task ends with

- Tests added (unit/conformance as appropriate) and passing.
- Conformance fixtures updated if the change is protocol-affecting.
- A short note on decisions made.

## Highest-risk areas (review these manually)

Conflict logic, ownership checks, and hook/transaction atomicity.

## Bug tracking (audits, reviews, builds)

A bug is not finished until it is tracked in this repository:

1. **Open a GitHub issue** as soon as an audit, code review, CI run or build/test
   failure identifies a bug or security finding. Describe severity, exact
   location (`file:line`, endpoint or collection), impact, and the evidence or
   reproduction. Group by audited change-set or bug class — not one issue per
   commit.
2. **When a fix lands, comment on that issue** with: what changed, the tests
   that now cover it, the commit(s), and a link to the PR. Put `Fixes #<n>` in
   the PR body so the issue closes on merge.
3. Keep the trail in one place: the issue carries problem → resolution history;
   the PR carries the code.

This applies to security audits, review findings, CI failures, and any
build-time error that required a code change.
