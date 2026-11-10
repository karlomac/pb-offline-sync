# PocketBase upgrade guide

**Audience:** maintainers of this repository and anyone maintaining a fork.
**Scope:** how PocketBase and its Go toolchain are pinned here, what an upgrade
touches, how to verify it, and how to roll back.

> **Current pin:** `github.com/pocketbase/pocketbase v0.40.6` (`server/go.mod`),
> Go 1.27.x. See `AGENTS.md` → "Pinned versions".

---

## 1. TL;DR

| Change | Risk | Do |
|---|---|---|
| Dependency CVE (e.g. `golang.org/x/net`) | low | bump the module, run `govulncheck` + suites, release a patch |
| Go toolchain security patch | low | bump the digest-pinned `golang:1.27-alpine` image (and CI `1.27.x` follows), rebuild |
| PocketBase **patch** release (`0.40.x`) | low–medium | bump `go.mod`, run everything, update the CI matrix |
| PocketBase **minor** release (`0.41.0`) | **high** | expect API breaks; upgrade on a branch, fix, test against a copy of staging data, staged rollout |
| Runtime `./pocketbase update` | — | **do not use.** Images are immutable; upgrades are rebuilds |

PocketBase is `v0.x`. **A minor bump may break compilation** — there is no
compatibility guarantee. Our integration surface is listed in §6.

---

## 2. How the version is pinned

| File | Controls | Notes |
|---|---|---|
| `server/go.mod` | the actual module version + `go` directive | source of truth |
| `.github/workflows/ci.yml` | `matrix.pocketbase` | a guard runs `go get …@<matrix>` and fails if `go.mod`/`go.sum` change — the pin cannot drift silently |
| `.github/workflows/ci.yml` (`pocketbase-latest`) | `@latest` compatibility canary | informational (`continue-on-error`); surfaces a new release early |
| `Dockerfile` | `ARG PB_VERSION` (empty by default) | when set, the build runs `go get …@${PB_VERSION}`; **leave empty for production** |
| `docker-compose.yml` | forwards `PB_VERSION` as a build arg | same warning: an override ships an untested module graph |
| `Dockerfile` base image | `golang:1.27-alpine@sha256:…` | digest-pinned, so Go **patch** releases do not arrive automatically |
| `.github/workflows/release.yml` | builds the image from the committed pin | no `PB_VERSION` override |

The released binary embeds the pinned PocketBase. There is **no self-update
path** in the image or entrypoint; `docker-entrypoint.sh` only bootstraps the
superuser and starts `serve`.

### Why exact pins

The plugin depends on internal PocketBase Go APIs (§6), not just the HTTP API.
"Any 0.40.x" is not a supported contract; the suite is verified against one
version. Forks may widen the range at their own risk, but the CI pin guard and
the boot validations (`validateMediaCollection`, auth setup checks) are what
catch incompatibilities — keep them.

---

## 3. Runbook A — dependency/security patch (e.g. `golang.org/x/net`)

This is the recent case (GO-2026-6860, issue #6):

```bash
cd server
go get golang.org/x/net@v0.62.0
go mod tidy
go build ./... && go vet ./... && go test ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

Then bump the CI matrix if the advisory is in PocketBase itself, and rebuild the
image. Verify with the live rig (§8). Patch releases of PocketBase are the same
shape: `go get github.com/pocketbase/pocketbase@v0.40.7`.

---

## 4. Runbook B — Go toolchain security patch

The Docker build uses a **digest-pinned** Go image, so a Go security release is
an explicit bump:

1. Resolve the new digest for `golang:1.27-alpine` (registry API or
   `docker buildx imagetools inspect`).
2. Update both `FROM golang:1.27-alpine@sha256:…` lines in `Dockerfile`
   (build stage) — keep the comment with the resolved tag.
3. CI already uses `go-version: 1.27.x`, so it picks up the newest patch; if the
   `go` directive in `server/go.mod` is older, leave it unless a dependency
   requires more.
4. Rebuild, run the suites + live rig, release.

> Why this matters: PocketBase 0.40.6's notes required Go 1.27.3 for a Go CA fix.
> A digest-pinned image gives reproducibility at the cost of not auto-patching —
> schedule these bumps like dependency updates.

---

## 5. Runbook C — PocketBase patch / minor upgrade

```bash
git checkout staging
git pull
git checkout -b karlomac/pb-<version>

cd server
go get github.com/pocketbase/pocketbase@v<version>
go mod tidy
go build ./... && go vet ./...
go test ./... -count=1
go test -race ./syncplugin/ -count=1
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

Then:

1. **Read the upstream changelog** for breaking changes, required Go version and
   data migrations.
2. Fix compile/runtime breaks (see §6 for the likely ones).
3. Update `.github/workflows/ci.yml` `matrix.pocketbase` (and `go` version if
   required); update the `PB_VERSION` example comment in `Dockerfile`.
4. If the `go` directive must move, update `server/go.mod` and the Docker base
   image together.
5. Run the **live** rig against a **copy** of staging data (§8) — unit tests use
   `tests.NewTestApp`, which is not the same as an existing database.
6. Rebuild the image, deploy to the test environment, run the E2E plan, then
   promote. Rollback is the previous image tag (§9).

Before starting, confirm the release is actually fetchable (§7).

---

## 6. Compatibility surface (where an upgrade breaks)

Everything imported from PocketBase in `server/`:

```bash
grep -rhoE 'pocketbase/(apis|core|mails|tools|cmd)[a-zA-Z0-9_/.-]*' server --include=*.go \
  | grep -v _test | sort -u
```

Current surface:

| Area | Used for | Files |
|---|---|---|
| `core` app + hooks | `OnServe`, `OnRecordCreateExecute`, `OnRecordCreateRequest`, `OnRecordAfterCreate*`, `OnRecordUpdate/Delete*`, `RunInTransaction`, `FindRecordsByFilter`, `FindRecordById`, `FindCollectionByNameOrId`, `Save`, `Delete`, `Cron()` | `server/syncplugin/*.go`, `server/authplugin/plugin.go` |
| `core` models | `Collection`, `Record`, `TextField`, `NumberField`, `FileField` (incl. `Protected`), `RelationField`, `AutodateField` | provisioning, validation, media, fixtures |
| `apis` | `RequireAuth`, `NewRouter` (tests), request-info contexts (`OAuth2`, `Batch`) | routes, auth gate, tests |
| `mails` | verification email | `server/authplugin/plugin.go` |
| `tools/cron` | prune/media jobs | `server/cmd/pb-offline-sync/main.go` |
| `tools/filesystem` | `NewFileFromPath` (commit), `Serve`/Range semantics (downloads) | `server/syncplugin/media.go` |
| `tools/router` | custom routes | `server/syncplugin/*.go` |
| `tests` | `TestApp` for unit/integration suites | all `*_test.go` |

Likely breakpoints on a minor release: hook names/signatures, `RequestInfo`
context constants, field-model fields (`Protected`), collection rule pointers,
`core.App` method set, mailer helpers, and `tests` fixture shape. The HTTP
surface we rely on is narrower and more stable: `/api/health`,
`/api/collections/{c}/records`, `/api/collections/{c}/auth-with-password`,
`/api/collections/_superusers/auth-with-password`, `/api/files/token`,
`/api/files/...` (Range), `/api/settings`, and `/api/realtime` (nudge default).

Our own protocol routes (`/api/sync/*`) are ours; they do not depend on PB
versioning beyond the above.

---

## 7. Distribution changes (GitHub → new host)

PocketBase 0.40.6's release notes state it is **likely the last release on
GitHub**; `./pocketbase update` will fetch from `https://ganigeorgiev.com`.
That does **not** directly affect our build (we use Go modules and never
self-update), but it may affect availability of future module versions.

Check before an upgrade:

```bash
# Which versions does the module proxy know about?
go list -m -versions github.com/pocketbase/pocketbase | tr ' ' '\n' | tail -10

# Is a specific version resolvable?
GOPROXY=https://proxy.golang.org go list -m -json github.com/pocketbase/pocketbase@v0.41.0
```

If GitHub tags stop and the new host does not publish Go modules:

- **Mirror/vendor** (documented, maintainable path):
  ```bash
  go mod vendor            # vendor the whole dependency graph
  # build with -mod=vendor; commit vendor/
  ```
  or keep a maintained fork/mirror and add a `replace` directive pointing at it.
  Both are repository changes that forks must adopt deliberately; record the
  decision in this file.
- **Canary blindness:** the `pocketbase-latest` CI job resolves `@latest` from the
  module proxy. If releases move off GitHub, it can stay at the last GitHub tag
  and stop warning you. Add a release-announcement check (RSS/changelog) to the
  maintenance calendar until the module source is settled.

---

## 8. Verification gates (all must pass)

```bash
# 1. Static + unit
cd server && go build ./... && go vet ./... && go test ./... -count=1
go test -race ./syncplugin/ -count=1
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# 2. Client packages
for p in core transport-pocketbase adapter-expo-sqlite; do
  (cd "client/$p" && pnpm install --frozen-lockfile && pnpm run typecheck && pnpm test)
done

# 3. Live rig on a COPY of staging data (or a fresh disposable dir)
./e2e/bootstrap.sh --skip-install --skip-load
# then with load:
./e2e/bootstrap.sh --skip-install --load-duration 2m --soak-seconds 60
```

The live rig provisions its own schema and exercises replication, conflicts,
media (upload/resume/dedup/access control) and the auth gate. For an **existing
database**, additionally: copy `pb_data` from staging to a disposable host,
start the new binary against the copy, verify boot (migrations + collection
validation), run a full `pull` from a test client, and inspect
`sync_changes`/`sync_meta` counts before/after.

---

## 9. Rollout and rollback

- `main` is **production**; `staging` is the testbed. Upgrades are PRs to
  `staging`, validated there, then promoted deliberately.
- Build and tag an image for the new version; deploy to the test environment
  first; run the E2E plan; only then promote.
- **Rollback:** redeploy the previous image tag. PocketBase does **not** support
  database downgrades; if the upgrade ran new migrations, restore the pre-upgrade
  backup instead.
- Always take a backup of `pb_data` (or the S3 bucket + settings) before the
  first boot of a new version.

---

## 10. Fork-maintainer notes

- Keep an **exact pin** and the CI guard; that is what makes upgrades explicit.
- Keep the `pocketbase-latest` canary, but treat it as a hint — verify release
  availability yourself (§7).
- Do not expose `PB_VERSION` in production builds. If you need a one-off image,
  commit the `go.mod` bump instead so the artifact is reproducible.
- If you use non-default collection names: the media collection must have
  `Protected` file fields and owner-scoped list/view rules, and the auth
  collection's `authRule` must require verification for password signup. The
  server fails fast at boot when these are missing — see
  `docs/protocol.md` §18 and the README "Media sync" section.
- If you change the pinned PocketBase version, record it in `AGENTS.md` and this
  file's "Current pin" line in the same PR.

---

## 11. Upgrade checklist (copy into the PR)

```markdown
- [ ] Upstream changelog read; breaking changes and migrations noted
- [ ] `go get github.com/pocketbase/pocketbase@vX.Y.Z` + `go mod tidy`
- [ ] `go build/vet/test`, `-race`, `govulncheck` green
- [ ] CI matrix (`matrix.pocketbase`, Go version) updated
- [ ] Dockerfile base digest / `PB_VERSION` comment updated
- [ ] `AGENTS.md` + this file "Current pin" updated
- [ ] Live rig green (sync, media, auth); load run recorded
- [ ] Boot verified against a copy of staging `pb_data`; backup taken
- [ ] Image built, tested in the test environment, rollback tag noted
- [ ] Issue/PR cross-linked; `Fixes #…` where applicable
```
