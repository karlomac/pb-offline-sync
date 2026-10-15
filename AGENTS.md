# AGENTS.md

Conventions for humans and coding agents working in this repository.

## What this is

`pb-offline-sync` — a two-way, offline-first sync layer between a client-local
database and a server-side PocketBase. **Protocol first:** `docs/protocol.md` is
the frozen v1.0 contract and the source of truth; code follows it.

## Read before changing anything

- `docs/protocol.md` — the wire contract (frozen v1.0).
- `docs/pb-offline-sync-PDD.md` — scope, non-goals, roadmap.
- `docs/architecture.md` — server design and the resolved Gate 0 decisions.
- `docs/threat-model.md` — security requirements.
- `docs/test-scenarios.md` — the scenario catalog for both sides, with coverage
  status and the prioritised gaps.

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
- **Go:** 1.27.x (module `github.com/pb-offline-sync/server`).
- **Node:** 22.x.

The Go module path and project name are provisional pending confirmation of the
name/org (see `docs/ecosystem-scan.md`).

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
