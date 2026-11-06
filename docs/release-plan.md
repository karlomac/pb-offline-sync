# Release plan

**Status: automated.** Releases are cut by pushing a semver tag; the rest is
`.github/workflows/release.yml`. This document is the contract for that process.

## 1. Three independent versions

| Version | Lives in | Scheme | Changes when |
|---|---|---|---|
| **Protocol** | `docs/protocol.md`, the `X-Sync-Protocol` header | integer (`1`) | **breaking wire change only** — not every release |
| **Plugin / server** | git tag `vX.Y.Z` + `syncplugin.PluginVersion` | SemVer | every release |
| **Client packages** | `client/*/package.json` | SemVer | when they are published (currently `private`, `0.0.0`) |

`server/go.mod`'s PocketBase pin (`v0.40.6`) is a **dependency pin, not a product
version** — it does not move with a release unless the plugin is re-verified
against a new PocketBase (protocol §17.1.4).

Tagging the Go module: the module lives in `server/`, so Go requires a
**subdirectory tag** — `server/vX.Y.Z`. `go get .../server@vX.Y.Z` needs that tag;
the workflow creates it automatically from `vX.Y.Z`.

## 2. What one release produces

- **Go module** — `server/vX.Y.Z` → `go get github.com/karlomac/pb-offline-sync/server@vX.Y.Z`
- **Container image** — `ghcr.io/karlomac/pb-offline-sync:X.Y.Z`, plus `X.Y`,
  `sha-<short>`, and `latest` (stable releases only)
- **Binaries** — `pb-offline-sync-{linux-amd64,linux-arm64,darwin-arm64,windows-amd64}`
  with `SHA256SUMS`
- **GitHub Release** — notes from the matching `CHANGELOG.md` section, plus the
  protocol version and install lines

## 3. Cutting a release

```bash
# 1. Move [Unreleased] content in CHANGELOG.md into a versioned section:
#      ## [0.1.0] - 2026-10-22
#    (the workflow refuses to release a version the changelog does not document)

# 2. Commit and push, then tag:
git commit -am "Release v0.1.0"
git push
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

That is the whole manual surface. A `-` in the version (`v0.2.0-rc.1`) produces a
**prerelease** and does not move `latest`.

## 4. Workflow stages and guards

```
validate ─┬─ test ────────┬─ module-tag ─┐
          ├─ binaries ────┤              ├─ release
          └─ image ───────┘              ┘
```

- **validate** — the tag is SemVer; `CHANGELOG.md` has a `## [X.Y.Z]` section;
  derives the protocol version from `docs/protocol.md`. **Everything else depends
  on it**, so a malformed tag or missing notes fails before anything is published.
- **test** — `go build`/`vet`/`test` (the release re-tests rather than trusting
  branch state).
- **binaries** — cross-compiled, `CGO_ENABLED=0`, version stamped via
  `-X ...syncplugin.PluginVersion=…`, so `/api/sync/info` reports the exact build.
- **image** — buildx, pushed to GHCR with the tags above.
- **module-tag** — creates `server/vX.Y.Z` if absent (idempotent on re-runs).
- **release** — checksums, notes extracted from the changelog, `gh release create`.

## 5. Permissions

Only the workflow's built-in **`GITHUB_TOKEN`**, with `contents: write` (release
+ module tag) and `packages: write` (GHCR). **No PAT, no stored secrets** — this
is deliberate, given workflow files themselves needed a broader PAT scope.

## 6. Pre-release checklist

- [ ] `CHANGELOG.md` has the versioned section (the hard gate)
- [ ] protocol version stated, and bumped **only** if the wire contract changed
- [ ] CI green on `main`
- [ ] any new env vars / config documented in `README.md` and `AGENTS.md`
- [ ] migrations are idempotent (they run on every upgrade — see `docs/architecture.md`)
- [ ] Dokploy users know the new image tag (the push does **not** redeploy them)

## 7. Rollback

- **Bad release notes / assets** — edit or delete the GitHub Release; the tag stays.
- **Bad image** — pull the previous version tag; `latest` only moves on stable
  releases, so pinning `X.Y.Z` in Dokploy is the safest production choice.
- **Bad Go module version — cannot be unpublished.** `proxy.golang.org` caches a
  version **forever**, so a broken `server/vX.Y.Z` must be superseded by a new
  patch (`vX.Y.Z+1`), never rewritten. Prefer prereleases for anything uncertain.

## 8. Not automated (deliberate)

- **npm publish** — the client packages are `private: true`; publishing needs a
  decision, a scope that exists, and an `NPM_TOKEN` secret. Add an `npm-publish`
  job (gated on the same tag) when that is settled.
- **Dokploy redeploy** — pushing an image is not deploying. Either enable the
  app's auto-deploy, or pin the version tag in the compose/Dokploy config and
  redeploy deliberately.
- **Go proxy propagation** — automatic on tag push (typically minutes).

## 9. Relationship to CI

`.github/workflows/ci.yml` stays the gate for pushes and PRs. The release workflow
re-runs the Go suite itself, so a release cannot ship untested code even if branch
protection is off.

## 10. Alternatives considered

**GoReleaser** handles archives/changelog/releases well, but we need GHCR, notes
derived from *our* changelog format, and a submodule tag — a ~110-line workflow is
more transparent and adds no tooling or config file. Revisit if binary packaging
(brew taps, deb/rpm, notarisation) becomes a requirement.

## 11. Open decisions

1. `darwin/amd64` (Intel Macs) — currently omitted; add if anyone needs it.
2. Should `latest` also move for prereleases? Currently **no**.
3. Client package versioning and npm publishing (see §8).
