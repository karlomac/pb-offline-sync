# Security Audit — pb-offline-sync (2026-10-29)

## Scope

- **Target(s) audited:** this repository's source only — the Go PocketBase
  plugin (`server/syncplugin`, `server/cmd/pb-offline-sync`), the TypeScript
  client (`client/core`, `client/transport-pocketbase`,
  `client/adapter-expo-sqlite`), the conformance fixtures, and the
  Docker/CI/release/deploy configuration (commit `07b1d41`, the media-sync
  branch state).
- **Access level:** full source, white-box, local. **No live or staging
  instance was audited** (none was provided), and testing was non-destructive:
  no scanning, no load testing, no exploitation attempts.
- **Out of scope / not tested:** the PocketBase server configuration as
  actually deployed (collection API rules, admin-UI exposure, rate-limiter
  settings, TLS/reverse proxy, backups, superuser IP allowlist,
  `--encryptionEnv`); upstream PocketBase core 0-day research beyond a version
  comparison; the third-party reverse proxy (Dokploy/Traefik); the deferred
  Expo-native modules (they do not exist in this repo yet);
  `examples/expo-notes` (README placeholder only).
- **Testing window:** 2026-10-29.

Method notes: findings were verified against the pinned library source in the
module cache (`pocketbase@v0.40.5`: `tools/filesystem/file.go`,
`tools/filesystem/filesystem.go`, `apis/file.go`, `core/record_query.go`,
`core/field_relation.go`, `core/collection_model.go`). `pnpm audit --prod`
reported no known vulnerabilities for the client packages. `govulncheck` could
not run in the audit sandbox (killed twice by the memory limit) — see
"Not tested".

## Executive summary

The **core sync engine is carefully built**: I found no SQL injection, no path
traversal, no stored-XSS path, and no cross-user record access — ownership is
taken from the token, `pull` filters by owner, and create/update/delete all
behave identically for "not mine" and "does not exist". The problems cluster in
the **newer media feature**, the **container defaults**, and the **client's
trust in server responses**:

1. The helper that provisions the media collection creates its file fields as
   *not* `Protected`, which in PocketBase means uploaded photos/videos are
   served **to anyone who has the URL, with no authentication** — this
   contradicts the protocol's stated "short-lived file token" model. One flag
   plus a documented rule fixes it.
2. The default container ships a **guessable superuser password** (`change-me`)
   and passes the real password on the command line.
3. The TypeScript client can be **wedged into infinite loops** (or unbounded
   memory use) by a malicious or buggy server — relevant because the client
   treats the server as authoritative.
4. The release pipeline uses **mutable action tags with write permissions**, and
   the pinned PocketBase `v0.40.5` is one patch behind `v0.40.6`, which adds a
   **thumbnail memory-limit** fix directly on the path media enables.

No Critical finding (nothing gives an unauthenticated attacker blanket access
to all data). One High, several Medium. Highest priority: **H-1** and **M-1**
(together they are the media-privacy story), then **M-3** (PocketBase bump) and
**M-7/M-8** (client robustness).

## Findings summary

| # | Severity | Title | Area |
|---|---|---|---|
| H-1 | High | Media file fields are not `Protected`: uploaded media is public by URL | Server / media |
| M-1 | Medium | Media security requirements are undocumented and media cannot be enabled in the shipped image | Server / docs / deploy |
| M-2 | Medium | Media immutability is bypassable through PocketBase's REST API unless collection rules are locked | Server / deploy |
| M-3 | Medium | PocketBase pinned at v0.40.5; v0.40.6 fixes thumbnail OOM and fileblob escape checks | Dependencies |
| M-4 | Medium | Relation fields accept cross-user targets: weak existence oracle + cross-user GC pinning | Server / push |
| M-5 | Medium | Quota is bypassable: partially staged sessions count 0 bytes; size unlimited by default | Server / media |
| M-6 | Medium | No cap on open upload sessions and no rate limiting; unbounded `sync_meta`/`sync_conflicts` growth | Server / ops |
| M-7 | Medium | Client infinite loops driven by server responses (zero `chunkSize`, non-advancing cursor, unmatched push results) | Client |
| M-8 | Medium | Client adopts unvalidated server offsets/revisions/envelopes | Client |
| M-9 | Medium | Release pipeline: mutable action refs with `contents: write` + `packages: write` | Supply chain |
| M-10 | Medium | Container ships a guessable admin password; password exposed via argv/env; no settings encryption | Deploy |
| L-1 | Low | Mid-chunk disconnect corrupts the staged stream (no truncate on error) | Server / media |
| L-2 | Low | Expiry/sweep of a consumed session destroys the stored idempotency result | Server / media |
| L-3 | Low | `MediaGC` starves when a full first page is entirely referenced | Server / media |
| L-4 | Low | SSE nudge: unbounded buffer growth, no reconnect, loose `data:` matching | Client |
| L-5 | Low | Base images not digest-pinned; CI lacks a top-level `permissions` block and any vuln/secret scan | Supply chain / CI |
| L-6 | Low | `docker-compose.yml` `env_file` breaks Dokploy builds; `chown -R` breadth; admin email logged | Deploy |
| I-1 | Informational | Cursor is client-tamperable (self-affecting only) | Server / pull |
| I-2 | Informational | MIME allowlist is advisory only (safe: PocketBase forces attachment for non-inline types) | Server / media |
| I-3 | Informational | README recommends `localStorage` for the auth token | Client docs |

## Detailed findings

### [High] H-1 — Media file fields are not `Protected`: uploaded media is public by URL

**Area:** Server / media
**Location:** `server/syncplugin/media.go:89,93` (`EnsureMediaCollection`), PocketBase
`apis/file.go:106` and `core/record_query.go:599`

**Description:** The provisioning helper adds the `file` and `poster` fields
without setting `Protected`, so they default to `false`. In PocketBase, the
file-serving handler only evaluates the collection's `ViewRule` **when
`fileField.Protected` is true**; for an unprotected field the file is served
with no authentication and no rule check at all. Media bytes therefore become
readable by anyone who has the URL
(`/api/files/media/{recordId}/{filename}`), where `recordId` is a random
15-char id and the stored filename carries a 10-char random suffix
(`normalizeName` in `tools/filesystem/file.go`). This is a capability URL, not
an enumerable one — but record ids are handed to clients in `pull` responses,
can appear in client logs, crash reports or screenshots, and are exactly the
thing the protocol's "short-lived file token" model (§18.4) was meant to
protect.

**Evidence:** `media.go:89,93` create `&core.FileField{...}` with no
`Protected: true`. `apis/file.go:106` reads
`if fileField.Protected { ... CanAccessRecord(...) }` — i.e. all checks are
skipped when false. `core/record_query.go:603-607` confirms a nil rule means
superuser-only, and a rule is only consulted inside the `Protected` branch.
`tools/filesystem/file.go:47-58` shows the stored name is
`<snakecase>_<10 random chars><ext>`. Not independently verified against a live
server (no deployment provided) — inferred from the pinned source, which is
deterministic here.

**Impact:** Private photos, videos and attachments are disclosed to any party
that obtains a file URL — no account needed. For a personal media-sync product
this is the most serious finding: the main asset the feature exists to protect
is readable over the public internet.

**Remediation:** Set `Protected: true` on both file fields in
`EnsureMediaCollection`, and document the accompanying rules: the media
collection needs `listRule`/`viewRule` = `owner = @request.auth.id` (so owners
and their file tokens pass), while `createRule`/`updateRule`/`deleteRule`
should stay locked (`null`, the PB default) since records are server-created
(see M-2). With `Protected: true` and a locked rule set, media is reachable
only via the owner's session or PocketBase's short-lived file token — matching
protocol §18.4, which should also state the `Protected` requirement explicitly.

**References:** PocketBase `apis/file.go` (file serving), `core/record_query.go`
(`CanAccessRecord`), protocol §18.4/§18.6, checklist §4 (file storage).

### [Medium] M-1 — Media security requirements are undocumented and media cannot be enabled in the shipped image

**Area:** Server / docs / deploy
**Location:** `server/cmd/pb-offline-sync/main.go` (`configFromEnv`), `README.md:262-272`, `docs/protocol.md` §18

**Description:** `configFromEnv` builds `Config` from `SYNC_*` variables and
never sets `Config.Media`, so the shipped binary and container **cannot enable
media sync at all**. The only way to turn it on is a custom build/migration
calling `syncplugin.MustRegister` with `Media` set — the intended path per the
docs ("host app calls `EnsureMediaCollection` in migrations"). That path has no
security checklist anywhere: nothing tells the operator that file fields must
be `Protected` (H-1), that the collection's rules must be owner-scoped for
downloads to work, or that write rules must stay locked (M-2). The README's
"API rules" section actively suggests the opposite ("set those collection rules
to what you want"), which is precisely how an operator ends up either with
public files or with a wide-open media collection.

**Evidence:** `main.go` `configFromEnv` sets `Collections`, limits, retention —
and never `cfg.Media` (env-int parsing covers `SYNC_MAX_*`,
`SYNC_IDEMPOTENCY_RETENTION_DAYS`, `SYNC_CHANGELOG_RETENTION_DAYS` only).
`README.md:262-268` discusses rules generically and makes no mention of the
media collection. `grep` for `Protected`/`token` in `README.md` and `docs/`
returns only protocol prose, no operational requirement.

**Impact:** A security control (H-1's fix, M-2's rule set) is invisible to the
only person who can apply it, and the feature is unreachable through the
documented deployment path — so it will be enabled by a custom build with no
guidance, shipping a known-weak default.

**Remediation:** Add a `SYNC_MEDIA_COLLECTION` (plus limit/quota/MIME)
env wiring to `configFromEnv`, and a "Media" subsection to the README module
docs listing the required collection shape: fixed fields, `Protected: true` on
`file`/`poster`, `listRule`/`viewRule = owner = @request.auth.id`, and locked
write rules. Mirror the requirement in protocol §18.1/§18.6 so it is part of
the contract rather than a footnote.

**References:** protocol §18, checklist §1 and §4.

### [Medium] M-2 — Media immutability is bypassable through PocketBase's REST API unless collection rules are locked

**Area:** Server / deploy
**Location:** `server/syncplugin/media.go:66-100` (no rules set), `server/syncplugin/push.go:212-216`, `README.md:262-272`

**Description:** The plugin enforces media immutability only on its own push
path (`media_immutable`). PocketBase's own record API for the same collection
is governed solely by that collection's rules. `core.NewBaseCollection` leaves
all five rules `null` (superuser-only), which is safe by default — but the
helper does not set them explicitly, and the README invites operators to set
rules "to what they want". If an operator widens `listRule`/`viewRule` (the
natural move when trying to make files work), the same rules file usually
covers writes: an unauthenticated or any authenticated caller could then create
media records with attacker-chosen `sha256`/`size`/`filename` (and even set
`owner` themselves), or update/delete other users' media records — bypassing
§18.1 entirely and corrupting dedup and GC accounting.

**Evidence:** `EnsureMediaCollection` sets no rules and no `Protected` flag
(`media.go:66-100`); `core/collection_model.go:407` (`NewBaseCollection`) does
not initialise rules, so they stay `null` until an operator sets them.
`push.go:214-216` is the only place `media_immutable` is enforced. Not
independently verified against a live instance.

**Impact:** Data-integrity and privacy breach of the media model
(forged/ambiguous media, deletion of other users' photos, dedup poisoning) in
any deployment whose media rules were relaxed.

**Remediation:** In `EnsureMediaCollection`, set the rules explicitly:
`ListRule`/`ViewRule` = `owner = @request.auth.id`, `CreateRule`/
`UpdateRule`/`DeleteRule` = `nil`, and assert this in `validateMediaCollection`
(warn or fail when a configured media collection has a non-nil write rule).
Document it in the same README/protocol place as M-1.

**References:** protocol §18.1, §14.6; checklist §1 ("createRule blocks
privilege/ownership fields").

### [Medium] M-3 — PocketBase pinned at v0.40.5; v0.40.6 fixes thumbnail OOM and fileblob escape checks

**Area:** Dependencies
**Location:** `server/go.mod:8`

**Description:** The module pins `github.com/pocketbase/pocketbase v0.40.5`.
Upstream released **v0.40.6** (2026-10-23) whose notes include
"**Additional fileblob bucket escape checks**" and "a global restriction for the
max `WxH` pixels size when generating a thumb **to reduce the risk of excessive
memory usage**", explicitly describing thumbnails as "one of the main culprit
for potential OOMs in PocketBase". Thumbnail generation is exactly the code path
the media feature puts on the request path (`?thumb=…` on the media file
fields); a small file declaring enormous pixel dimensions can therefore drive
unbounded memory use on the pinned version and take the server down.

**Evidence:** `server/go.mod:8` pins v0.40.5. GitHub releases API for
`pocketbase/pocketbase` latest = `v0.40.6`, published 2026-10-23, with the notes
quoted above (thumbnail pixel cap, `PB_THUMBS_MAX_SOURCE_PIXELS`). In v0.40.5
the only thumbnail guard is a *concurrency* semaphore
(`apis/file.go:24-37`: `PB_THUMBS_MAX_WORKERS`, default `NumCPU+2`) — it limits
parallel work, not the memory a single crafted image can allocate during decode.

**Impact:** Authenticated-but-any-user availability loss (decompression-bomb
thumbnail), on a component the media feature depends on. Upstream's fix is one
patch away.

**Remediation:** Bump the pin to `v0.40.6` (and the `PB_VERSION` build arg in
`Dockerfile`), rebuild, and re-run the conformance suite; additionally consider
setting `PB_THUMBS_MAX_SOURCE_PIXELS` to a value appropriate for the app, and
serving thumbnails only from the client-generated `poster` rather than from
arbitrary uploads.

**References:** PocketBase v0.40.6 release notes; protocol §18.4.

### [Medium] M-4 — Relation fields accept cross-user targets: weak existence oracle + cross-user GC pinning

**Area:** Server / push
**Location:** `server/syncplugin/push.go:260-280` (`validateFields`),
PocketBase `core/field_relation.go:198-240`

**Description:** `validateFields` rejects server-authoritative, unknown, system
and file fields but does not constrain relation *values*. `rec.Load(fields)` +
`app.Save` then accept any relation id that exists, because PocketBase's
relation validation checks only counts and existence — not that the target
belongs to the caller. So user A can set a relation field to a record id owned
by user B (including a media record). Two consequences: (a) a weak cross-user
existence oracle — `ok` vs `validation_failed` distinguishes "id exists" from
"id does not exist", and §14.5 claims no cross-user existence oracle exists;
practically weak because ids are 15 random chars; (b) `MediaGC` counts
references globally (`media_gc.go` scans every collection's relation columns
without an owner filter), so a cross-user reference **pins another user's media
against deletion** — and, symmetrically, an attacker can pin their own media by
referencing it from records they own, keeping storage forever.

**Evidence:** `push.go:260-280` validates names/op/owner-field but not relation
targets; `core/field_relation.go:198-240` validates min/max select and existence
only. `media_gc.go` builds `SELECT "<field>" FROM "<collection>" WHERE
"<ownerField>" IS NOT NULL` (no owner predicate), so references from any user
count.

**Impact:** Storage-exhaustion leverage against the operator (pinned media is
never collected), a partial violation of the documented "no existence oracle"
guarantee, and integrity gaps for app-level relations that cross ownership
boundaries.

**Remediation:** In `validateFields` (or a post-decode check), resolve relation
values and require each target record's owner to equal the authenticated user —
or restrict relation fields to same-collection targets explicitly. Document the
decision in §14.6. As a cheaper partial fix, have `MediaGC` only honour
references from records owned by the same user as the media record.

**References:** protocol §14.5, §14.6, §18.1; checklist §5 (hooks bypass the
record-rule model).

### [Medium] M-5 — Quota is bypassable: partially staged sessions count 0 bytes; size unlimited by default

**Area:** Server / media
**Location:** `server/syncplugin/media.go:247` (`stagedBytes`), `server/syncplugin/config.go` (`MaxFileSize`/`PerUserQuota` defaults 0)

**Description:** `stagedBytes` skips any session whose `Offset != Size`, so
partially-uploaded sessions contribute nothing to the quota accounting at
`init`, while the commit-time re-check counts only *fully* staged sessions. A
client can therefore open many sessions and stream up to
`maxFileSize × sessions` real bytes into staging before any check trips (and
with the default `MaxFileSize = 0` there is no per-file ceiling either). The
`size` field is a client-declared `int64` with no upper bound, and the staged
sum is plain `int64` arithmetic, so extreme declared sizes risk overflow in the
accounting rather than rejection.

**Evidence:** `media.go:247` — `if !found || s.Owner != uid || s.Offset != s.Size
{ continue }` inside `stagedBytes`; the doc comment above `mediaBytesUsed`
claims active staged sessions are counted, which this line contradicts.
`config.go` defaults `MaxFileSize`/`PerUserQuota` to 0 (unlimited) when unset.

**Impact:** Authenticated disk-fill (resource exhaustion) on a deployment that
enabled media with the documented defaults; the "checked at init, before bytes
move" guarantee in §18.2 does not hold for parallel sessions.

**Remediation:** Count every active (unconsumed, unexpired) session's declared
`Size` in `stagedBytes`; cap concurrent open sessions per user; require a
non-zero `MaxFileSize` when media is enabled (or document the risk loudly);
reject/limit `size` above `math.MaxInt64/2` to keep the arithmetic safe.

**References:** protocol §18.2, §18.6, §14.7.

### [Medium] M-6 — No cap on open upload sessions and no rate limiting; unbounded `sync_meta`/`sync_conflicts` growth

**Area:** Server / ops
**Location:** `server/syncplugin/media.go:343` (`newUploadID` at init),
`server/syncplugin/prune.go:73-84` (`Prune`), `server/syncplugin/push.go:642-660` (`writeConflict`), `server/syncplugin/hooks.go:69-77` (`sync_meta` upsert)

**Description:** Three unbounded-growth paths share one root cause — no
retention for tables the plugin itself writes, and no throttling:

- **Upload sessions:** `init` creates a staging directory per call with no
  per-user cap and no rate limiting (§18.6 recommends rate-limiting `init` but
  nothing implements it). Each session costs a directory + sidecar until the
  TTL sweep runs; the sweep is only run if the host wires it.
- **`sync_meta`:** one row per record ever created, never deleted — by design
  (§8.1/§17.1.1 tombstone/reuse protection) — but that means an authenticated
  client can create and delete records in a loop and the table only grows.
- **`sync_conflicts`:** every stale-base overlap writes a row containing the
  losing *field values* (`writeConflict`); `Prune` never touches this table, and
  nothing marks rows resolved except an external caller using the Go API, so
  conflict rows accumulate forever — including editor-sized payloads.

**Evidence:** `media.go:343` creates a session with no counter/limit;
`prune.go:77-83` prunes only `sync_changes` and `sync_processed`;
`push.go:650-655` inserts into `sync_conflicts` on each overlapping stale
update; there is no `resolved` update path in `server/` outside the
`ListUnresolvedConflicts` reader (`conflicts.go`).

**Impact:** Authenticated storage-exhaustion DoS with no cleanup path; the
cheapest lever is conflict rows (large free-text fields) or a
create/delete loop inflating `sync_meta`.

**Remediation:** Add retention to `Prune` for `sync_conflicts` (and document
the intentional `sync_meta` growth, or bound it per user with a tombstone-hash
scheme), cap concurrent sessions per user, and enable PocketBase's rate limiter
(or a proxy limit) on `/api/sync/*` — the protocol's §12.1 `429 rate_limited`
path already exists for that.

**References:** protocol §9, §13, §14.7, §18.6; checklist §6 (rate limiting),
§7 (operational hygiene).

### [Medium] M-7 — Client infinite loops driven by server responses

**Area:** Client
**Location:** `client/core/src/media.ts:116,136`, `client/core/src/engine.ts:186,326,169`

**Description:** Four loops advance only on server-provided values, with no
progress guard:

- `media.ts:116,136`: `chunkSize = opts?.chunkSize ?? init.chunkSize` — a
  server `chunkSize` of `0` or `null` makes `Math.min(...)` 0, the read returns
  0 bytes, the `byteLength !== length` guard passes, `offset += 0`, and the
  `while (offset < size)` loop issues 0-byte PATCHes forever.
- `engine.ts:326`: `pullAll` loops on `page.hasMore`; a server that always
  reports `hasMore: true` with a non-advancing cursor spins forever.
- `engine.ts:186-205`: the push loop re-sends `sent` entries; a `results` array
  whose ids match no batch entry applies nothing, so the outbox never drains and
  the same batch is pushed indefinitely.
- `engine.ts:169-179`: a server that always answers 410 causes
  resync → pull → 410 → `scheduleRetry` indefinitely (rate-limited, not
  bounded).

**Evidence:** the loops above, read in full; no iteration cap or
"cursor must advance" check exists in `engine.ts` or `media.ts`.

**Impact:** A malicious or compromised server (a realistic threat for an
offline-first client that must distrust the network), or a buggy proxy, wedges
the app: battery drain, unbounded requests, quota burn, no user-visible failure.

**Remediation:** Reject non-positive/non-integer `chunkSize` after `init`;
break the pull loop when the cursor does not advance or `changes` is empty; cap
consecutive no-progress push passes; cap consecutive resyncs and surface a
terminal error state.

**References:** protocol §6.6, §7.4, §11, §18.2.

### [Medium] M-8 — Client adopts unvalidated server offsets/revisions/envelopes

**Area:** Client
**Location:** `client/transport-pocketbase/src/transport.ts:233-239`, `client/core/src/media.ts:122-124`, `client/core/src/engine.ts:352,268,286,362`

**Description:** `parseOffset` accepts any finite number (`-1`, `1e308`, and
`""` → 0 all pass), and that value is adopted verbatim as the resume truth, then
used to slice the source (`source.read(state.offset, …)`) — a negative offset
becomes an illegal-range read against the host's file handle. `change.rev` is
used in ordering comparisons with no integer check (`engine.ts:352`), and
response envelopes (`results`, `changes`, `record`) are cast rather than
validated before being indexed, so a huge or malformed `changes` array is
consumed (memory) and arbitrary `record` shapes are written into local storage.

**Evidence:** `transport.ts:233-239` (`Number(raw)` +
`Number.isFinite` only); `media.ts:122-124` (offset adopted, passed to `read`);
`engine.ts:352` (`change.rev <= localRev`); `transport.ts:156-162`
(`res.json()` cast to the response type in every caller).

**Impact:** Client-side memory/heap exhaustion and wrong local state (skipped or
force-applied changes) from a hostile server; the offset case can also throw
out of the uploader mid-file.

**Remediation:** Require `Number.isInteger(n) && n >= 0 && n <= size` for
offsets; validate `rev` as a non-negative integer; validate the envelope shape
(array-ness, required per-change fields) in one place before use.

**References:** protocol §7.2, §18.3.

### [Medium] M-9 — Release pipeline: mutable action refs with write permissions

**Area:** Supply chain
**Location:** `.github/workflows/release.yml:14-16` (`permissions`),
`.github/workflows/ci.yml:22` and other `uses:` lines

**Description:** Every `uses:` is a floating major tag
(`actions/checkout@v4`, `docker/build-push-action@v6`, …). `release.yml` grants
`contents: write` + `packages: write`, so a retagged or compromised action runs
with enough privilege to push tags and overwrite the published GHCR image — the
artefact every deployment pulls.

**Evidence:** workflow files read in full; `release.yml:14-16` is the
permissions block; no SHA-pinned `uses:` anywhere.

**Impact:** Supply-chain compromise of the shipped image (and therefore all
users) via one upstream action.

**Remediation:** Pin every action to a full commit SHA (keep the version in a
comment); consider `permissions: contents: read` at the workflow top level with
step-level elevation only where needed; add a Dependabot/renovate rule to keep
SHAs fresh.

**References:** checklist §7 (operational hygiene), §5-infra (secrets/CI).

### [Medium] M-10 — Container ships a guessable admin password; password exposed via argv/env; no settings encryption

**Area:** Deploy
**Location:** `docker-entrypoint.sh:37-42`, `.env.example:9`, `README.md:317`, `Dockerfile`/entrypoint (no `--encryptionEnv`)

**Description:** `.env.example` sets `PB_ADMIN_PASSWORD=change-me` and the README
run example copies it verbatim, while the entrypoint creates/refreshes that
superuser on every boot: a copy-paste deployment has a guessable superuser with
full `/_/` access. The real password is also passed as a command-line argument
(`su-exec pb:pb /pb/pb superuser upsert "$EMAIL" "$PASSWORD"`), readable by any
process in the same namespace via `/proc/<pid>/cmdline`, and arrives as an env
var (visible to `docker inspect`). Settings encryption is never enabled
(`--encryptionEnv`), so SMTP/OAuth secrets and other settings sit unencrypted in
the SQLite database, and no superuser IP allowlist is configured.

**Evidence:** `.env.example:9`; `README.md:317`; `docker-entrypoint.sh:37-42`
(argv upsert); no `--encryptionEnv` in the entrypoint, `Dockerfile` or `main.go`.

**Impact:** An exposed/unprotected admin UI with weak credentials is the single
most common real-world PocketBase compromise; the argv/env exposure widens the
blast radius of any host access, and unencrypted settings leak secrets at rest
(backups included).

**Remediation:** Blank the example password with a "required — set a strong
value" comment and drop the working default from the README example; feed the
password over stdin (or a 0600 temp file) rather than argv; support
`PB_ENCRYPTION_KEY` → `--encryptionEnv`; document `superuser ips` and keep the
admin UI behind the proxy allowlist (the compose file already prefers `expose:`
over `ports:` — keep that).

**References:** checklist §2 (auth/superuser), §3 (admin UI), §7 (encryptionEnv,
backups).

### [Low] L-1 — Mid-chunk disconnect corrupts the staged stream

**Area:** Server / media
**Location:** `server/syncplugin/media.go:439`

**Description:** `data.bin` is opened `O_APPEND`; if `io.Copy` fails mid-PATCH
(client disconnect), the appended bytes stay on disk but `s.Offset` is not
advanced. HEAD then reports the old offset, the retry appends after the
orphans, and commit fails `hash_mismatch` — the session is unusable except by
re-init. Only the offset-0 path truncates first.

**Evidence:** `media.go:437-446` (append + `LimitReader` + no truncate on
error).

**Impact:** Wasted bytes and an unrecoverable session on flaky networks
(reliability; no cross-user effect thanks to the commit-time hash check).

**Remediation:** Truncate back to `s.Offset` on any `io.Copy` error (or seek
rather than append and truncate on failure).

### [Low] L-2 — Expiry/sweep of a consumed session destroys the stored idempotency result

**Area:** Server / media
**Location:** `server/syncplugin/media.go:386,487,542`, `server/syncplugin/media_gc.go:36-55`

**Description:** The expiry branch runs before the `done.json` (consumed) check
and calls `RemoveAll`, and the sweep also removes consumed session directories
at TTL — so a commit retry arriving after the staging TTL gets `404` instead of
the stored result the protocol promises. Clients recover via a re-`init` dedup
hit, so the practical impact is small, but §18.4's "returns the stored original
result" is time-bounded in a way the spec does not say.

**Evidence:** the three `RemoveAll` sites and the sweep loop; no `done.json`
guard.

**Impact:** Lost idempotency window (client-visible 404 on a retry after a long
outage); no data exposure.

**Remediation:** Check for `done.json` before the expiry branch/sweep and return
`404` without deleting; or state the TTL bound explicitly in §18.4.

### [Low] L-3 — `MediaGC` starves when a full first page is entirely referenced

**Area:** Server / media
**Location:** `server/syncplugin/media_gc.go:132`

**Description:** The batch loop re-queries the same first page (no keyset) and
returns as soon as a page yields no deletion (`!progressed`). If the oldest 200
eligible records are all referenced, unreferenced records deeper in the set are
never collected.

**Evidence:** `media_gc.go:120-137`.

**Impact:** Slow storage growth that GC is supposed to bound (§18.1/§18.6).

**Remediation:** Keyset-paginate on `updated`/`id`, continuing until a page
returns fewer than `gcBatch` rows.

### [Low] L-4 — SSE nudge: unbounded buffer growth, no reconnect, loose `data:` matching

**Area:** Client
**Location:** `client/transport-pocketbase/src/nudge.ts:30-44`

**Description:** The reader appends to `buf` and only trims at `"\n\n"`, so a
stream that never emits the delimiter grows memory without bound; when the
stream ends the loop exits with no reconnect (nudges silently stop); and
`block.includes("data:")` matches any occurrence, not just a line-start field.

**Evidence:** `nudge.ts:33-39`.

**Impact:** Client memory exhaustion from a hostile endpoint; silently dead
nudge channel otherwise (availability, not confidentiality).

**Remediation:** Cap the buffer (drop + reconnect past a limit), add bounded
reconnect with backoff, and match `data:` at line start.

### [Low] L-5 — Base images not digest-pinned; CI lacks permissions and vuln/secret scanning

**Area:** Supply chain / CI
**Location:** `Dockerfile:15,36`, `.github/workflows/ci.yml:1-5,21-60`

**Description:** `FROM golang:1.27-alpine` / `alpine:3.20` use mutable tags on an
image that is built and pushed to GHCR; `ci.yml` declares no top-level
`permissions:` (so it inherits the repo default token scope while executing
`pnpm install`/`go` fetches of third-party code), and CI runs no
`govulncheck`, `pnpm audit` or secret scan.

**Evidence:** `Dockerfile:15,36`; `ci.yml` (no `permissions:`, no scan steps);
`release.yml:14-16` shows the correct pattern.

**Impact:** Non-reproducible builds and a supply-chain vector for the published
image; no automated detection of dependency CVEs (relevant to M-3).

**Remediation:** Pin base images by digest; add `permissions: contents: read` to
`ci.yml`; add `govulncheck ./...`, `pnpm audit`, and a secret scan to CI.

### [Low] L-6 — Compose/entrypoint operational issues

**Area:** Deploy
**Location:** `docker-compose.yml:18`, `docker-entrypoint.sh:32,38`

**Description:** `env_file: .env` references a gitignored file that will be
absent on a Dokploy git build (Dokploy injects env variables instead); the
entrypoint `chown -R`s an env-controlled path
(`"${PB_DATA_DIR:-/pb/pb_data}"`) with no guard, so a mistyped `PB_DATA_DIR`
recursively changes ownership of an arbitrary directory (the expansion is
quoted — no shell injection); and the admin **email** is echoed to container
logs on every boot (identity leak into log aggregation).

**Evidence:** `docker-compose.yml:18-19`; `docker-entrypoint.sh:18,30-32,38`.

**Impact:** Failed or surprising deployments; log-borne reconnaissance;
unbounded chown in a misconfiguration.

**Remediation:** Declare each variable as `${VAR}` under `environment:` and drop
`env_file`; validate `PB_DATA_DIR` is under `/pb` before recursing; drop the
email from the log line.

### [Informational] I-1 — Cursor is client-tamperable (self-affecting only)

**Area:** Server / pull
**Location:** `server/syncplugin/pull.go:38-54,306-309`

**Description:** `decodeCursor` validates version and kind but not the payload
ranges: a client can set `Head`/`AfterID` to arbitrary values. Everything is
bound as a query parameter and `Seq`/`Head` are range-checked against the log
bounds, so tampering only misdirects the *caller's own* sync position (possibly
skipping records). No injection and no cross-user effect — recorded for
completeness, and worth an item in the client docs ("cursors are opaque; do not
edit them").

### [Informational] I-2 — MIME allowlist is advisory only (safe as deployed)

**Area:** Server / media
**Location:** `server/syncplugin/media.go:101-124` (`mimeAllowed`)

**Description:** The allowlist gates the *declared* MIME at `init`; uploaded
bytes are never sniffed server-side, and PocketBase re-derives the served
Content-Type from content. I verified this is **not** an XSS vector: PB serves
only `image/*`, `video/*`, `audio/*` and PDF inline and forces
`Content-Disposition: attachment` plus a `default-src 'none'; sandbox` CSP for
everything else (`tools/filesystem/filesystem.go:513-580`), with
`X-Content-Type-Options: nosniff` added by the API middleware. Noted so the
advisory nature of the check is a conscious decision: if a host ever serves
media through its own handler instead of PocketBase's, this becomes exploitable.

### [Informational] I-3 — README recommends `localStorage` for the auth token

**Area:** Client docs
**Location:** `README.md:626` (and `:561`)

**Description:** The web wiring example stores the PocketBase token in
`localStorage`, where any injected script can read it and it survives reload.
The transport itself only consumes the injected provider
(`client/transport-pocketbase/src/transport.ts:140-144`, a plain `Authorization`
header) — the guidance, not the code, is the weak point. Suggest documenting the trade-off and preferring in-memory tokens
with refresh, or an httpOnly cookie via a server-side proxy.

## Controls verified (no findings)

- **SQL injection:** every user-supplied value is bound with dbx parameters.
  The only interpolated identifiers are operator-config values validated at boot
  to exist as PocketBase fields (`config.go:174-214`), `identRe`-guarded names
  in `mediaBytesUsed`/`MediaGC`, and compile-time constants in `Prune`. No
  string-built query accepts request data.
- **Filter-expression injection:** the pull/snapshot filters use a boot-validated
  owner field name plus bound `{:owner}`/`{:after}` params.
- **Path traversal:** `uploadId` is regex-gated (`^u_[0-9a-f]{32}$`) before any
  path join; client `filename` is never used as a path — PocketBase normalises
  its own stored name and always appends randomness; staging dirs are `0700` with
  `0600` files; record ids are pattern-validated.
- **Stored XSS via uploads:** neutralised by PocketBase's inline-type allowlist,
  forced attachment disposition and sandbox CSP (see I-2).
- **Ownership / IDOR:** push create/update/delete are owner-checked with
  identical behaviour for "not mine" and "missing"; `pull` filters by owner in
  the data layer and re-checks before emitting; media sessions are owner-bound
  with uniform `404`s (no existence oracle); `id` reuse is blocked by retained
  `sync_meta` rows.
- **Change-log atomicity:** `sync_meta`/`sync_changes` are written from the
  in-transaction `OnRecord*Execute` hooks (`hooks.go:24-63`), never
  `*AfterSuccess`.
- **Client JS hazards:** no `eval`, `new Function`, `innerHTML`,
  `document.write` or dynamic `import(variable)` in the runtime client; the
  token is only sent in the `Authorization` header (SSE uses `fetch`, not
  `EventSource`, so there is no query-string token leak); identifiers are
  URL-encoded; no token/record logging; no prototype pollution (object spread
  creates own properties; the SQLite adapter uses `hasOwn`).
- **SQLite adapter:** identifiers are quoted with `""`-doubling, column types
  come from a local static schema, and values are parameterised
  (`client/adapter-expo-sqlite/src/adapter.ts:86,169,301`).
- **Repo secret scan:** no committed credentials, keys or tokens in tracked
  files.
- **Client dependencies:** `pnpm audit --prod` → no known vulnerabilities; the
  only runtime dependency is the workspace-local core package.

## Not tested

- **No live/staging instance was provided**, so nothing was verified against a
  running PocketBase: collection API rules as actually set, admin-UI exposure,
  rate-limiter settings, TLS/reverse-proxy behaviour, backup configuration,
  superuser IP allowlist and `--encryptionEnv`. Findings touching those are
  stated as requirements or as consequences of the shipped defaults.
- **`govulncheck` did not complete** in the audit sandbox (SIGKILL from the
  memory limit, twice). Go dependency CVE status is therefore unverified; the
  PocketBase version comparison (M-3) is from the upstream release notes rather
  than a scanner. Add `govulncheck` to CI (L-5).
- **No destructive or load testing** was performed (per the non-destructive
  posture): DoS findings (M-5, M-6, M-7) are reasoned from code, not measured.
- **Thumbnail memory behaviour (M-3)** was not reproduced; it is taken from the
  upstream release notes that describe the fix.
- **Expo-native modules** do not exist yet, so the deferred MED-3 client work
  (hash cache, background lanes, poster generation) is unaudited.
- The reverse proxy layer (Dokploy/Traefik) is not represented in the repo
  beyond `docker-compose.yml`, so header/TLS/rate-limit behaviour there is
  unaudited.

## Recommended fix order

1. **H-1** + **M-1** + **M-2** together — one code change (set `Protected: true`,
   set/validate the media rules) plus the documented requirement. This is the
   media-privacy story and should land before media is enabled anywhere.
2. **M-3** — bump PocketBase to v0.40.6 before enabling media (thumbnail OOM).
3. **M-10** — remove the default password from examples and stop passing it on
   argv; enable `--encryptionEnv`.
4. **M-7** + **M-8** — client progress guards and response validation.
5. **M-5** + **M-6** — quota/accounting fixes and retention for
   `sync_conflicts`; enable rate limiting.
6. **M-9** + **L-5** — pin actions/base images, add CI vulnerability scanning.
7. **M-4** — relation-target ownership check (or an explicit documented
   decision).
8. Low items as time allows.

---

# Follow-up review — 2026-11-06 (PR #3: auth signup gating)

## What changed since the audit

- `karlomac/sync-media` (PR #2) is **unchanged** at `07b1d41`, the exact commit
  this audit snapshotted. **Every finding below therefore remains open** — none
  of H-1, M-1…M-10, L-1…L-6 or I-1…I-3 was addressed.
- New work: **PR #3 `karlomac/auth-signup` @ `6f4d16c`** (base `staging`),
  adding `server/authplugin` (~1,100 lines) + `main.go` wiring: a
  Turnstile-gated password signup with optional email-domain controls, OAuth2
  signup enablement, and a verification email on password signups.
- Verified on the branch: `go build ./...`, `go vet ./...`, `go test ./...`
  all green (authplugin, cmd, syncplugin).
- The audit document itself is untracked in this worktree; commit it under
  `docs/` so PR reviewers can see both waves.

## Audit findings status

| ID | Status |
|---|---|
| H-1 (media files public by URL) | **Open** — media branch unchanged |
| M-1 (media not enableable/undocumented) | **Open** |
| M-2 (media rules bypass) | **Open** |
| M-3 (PocketBase v0.40.5 → v0.40.6) | **Open** — `go.mod` still `v0.40.5` |
| M-4 (cross-user relation targets) | **Open** |
| M-5 (quota bypass) | **Open** |
| M-6 (unbounded growth / no rate limit) | **Open** — and now also applies to signup (A-7) |
| M-7 / M-8 (client loops / unvalidated responses) | **Open** |
| M-9 (mutable action refs) | **Open** — workflows untouched |
| M-10 (container defaults / argv password / no encryptionEnv) | **Open** — and auth adds another env secret (A-9) |
| L-1…L-6, I-1…I-3 | **Open** |

## New findings in PR #3 (auth)

### [Medium] A-1 — Hardcoded `users` collection, no validation → silent fail-open

`plugin.go:29` binds `OnRecordCreateRequest(usersCollection)` with
`usersCollection = "users"` (`config.go:17`). PocketBase tag-filtered hooks
simply never fire when no collection has that name
(`core/base.go:1210` returns a `TaggedHook`), so an app whose auth collection is
named anything else (or was renamed) has **password signup completely
ungated** while the operator believes it is Turnstile-protected. `Register`
performs no existence/type check.

Fix: resolve and validate the collection at `OnServe` (exists, `IsAuth()`),
fatal otherwise; or make the name configurable (`AUTH_COLLECTION`).

### [Medium] A-2 — Verification email is not enforced

The plugin sends `mails.SendRecordVerification` but never sets or validates the
collection's `authRule`, so unless the operator separately configures
"verified required", accounts created through the gated signup are immediately
usable and the email is advisory. Fix: document the required `authRule`, and
validate it at boot (or expose a flag that applies it).

### [Low] A-3 — Turnstile verifier: redirects + scheme

`turnstile.go:45-51` uses `http.DefaultClient` (which follows redirects; on
301/302 Go drops the body, but 307/308 **replays it, including the secret**) and
`AUTH_TURNSTILE_VERIFY_URL` may be `http://` (`url.ParseRequestURI` is
scheme-agnostic). Operator-controlled, so not directly exploitable, but a
mistyped/compromised endpoint can receive the secret or be used as an SSRF hop.
Fix: dedicated client with `CheckRedirect` returning `ErrUseLastResponse`,
require `https` unless an explicit dev override, keep the 5 s timeout.

### [Low] A-4 — Disposable-domain check is exact-match

`inList` (`plugin.go:121`) compares whole domains, so `anything.mailinator.com`
passes when `mailinator.com` is blocked. Fix: suffix-match disposable entries
(or document that only exact hosts are blocked).

### [Low] A-5 — Tests cannot detect the fail-open

Unit tests trigger `app.OnRecordCreateRequest()` **untagged**
(`plugin_test.go:87`), which bypasses the tag filter the production hook relies
on; there is no signup test through the real router/tagged path (contrast the
sync plugin's fixture driver). Add a through-the-mux test plus a
non-`users`-collection case.

### [Informational] A-6 — Signup has no rate limit

Each attempt costs one bounded outbound Cloudflare call; PB's built-in rate
limiter (or a proxy limit) must be configured. Same posture as M-6.

### [Informational] A-7 — OAuth2 auto-linking by email

OAuth2 signup is intentionally not bot-gated. PB links OAuth identities by
email, so the operator's provider trust assumptions matter (checklist §2); not a
defect in this code, worth a docs note.

### [Informational] A-8 — New env secret is undocumented

`AUTH_TURNSTILE_SECRET` is another secret delivered via container env (see
M-10) and none of the `AUTH_*` variables appear in `.env.example`, the README,
or the compose file — the same documentation gap class as M-1.

## Positive

The gate itself held up under review: it fails **closed** on missing/blank
tokens, verifier errors, hostname mismatch, honeypot, and disallowed domains; it
requires a Turnstile token for batch-created users
(`RequestInfoContextBatch` falls through to the password branch); OAuth2
creates go through `processInternalRequest(..., RequestInfoContextOAuth2, ...)`
inside the OAuth transaction (`apis/record_auth_with_oauth2.go:259,411`) so a
rejection rolls back cleanly with no orphaned rows; the superuser bypass is
explicit and intended; the verification email is replay-safe
(`LoadAndDelete`) and the record-pointer identity does hold across the real
save (`submitSignup` saves the same pointer); no secret or token is logged; the
verifier bounds body size and time.

**Verdict:** PR #3 is sound but needs A-1, A-2, A-3 and A-5 addressed before
merge; no bypass of the Turnstile gate was found on the default (`users`)
configuration.


---

# Remediation status — 2026-11-06

Applied on the PR branches (not yet pushed at the time of writing):

## PR #2 (`karlomac/sync-media`)

| Finding | Status | Commit | Evidence |
|---|---|---|---|
| H-1 media files public by URL | **Fixed** | `4633087` | file/poster fields `Protected`, owner-scoped list/view rule, boot validation rejects unprotected fields; `TestEnsureMediaCollectionProtectsFilesAndScopesRules`, `TestValidateMediaCollectionRejectsInsecureShapes`, `TestEnsureMediaCollectionConvergesUnprotectedCollection` |
| M-1 media not enableable / undocumented | **Fixed** | `4633087` | `SYNC_MEDIA_*` env wiring + README/.env.example requirements; `TestConfigFromEnvMediaWiring` |
| M-2 media rules bypass | **Fixed** | `4633087` | write rules required to stay `nil`; boot validation + helper |
| M-3 PocketBase pin | **Fixed** | `4633087` | `v0.40.6`; full suite green on the new pin |
| M-4 cross-user relation targets | **Fixed** | `bf11d55` | `validateRelationOwners` in push; `TestPushRejectsCrossUserRelationTargets` |
| M-5 quota bypass | **Fixed** | `bf11d55` | partial sessions counted, per-user session cap, overflow guard; `TestMediaQuotaCountsPartialSessions`, `TestMediaInitCapsOpenSessions` |
| M-6 unbounded growth | **Fixed** | `bf11d55` | `Prune` now removes `sync_conflicts`; session cap; README documents `sync_meta` growth + rate limiting; `TestPruneRemovesOldConflicts` |
| M-7 client loops | **Fixed** | `675b078` | chunkSize/offset validation, cursor-advance, no-progress, page/resync caps; engine/media tests |
| M-8 unvalidated server data | **Fixed** | `675b078` | offset/rev/envelope validation; `parseOffset` integer+non-negative |
| M-9 mutable action refs | **Fixed** | `57afc0c` | all `uses:` pinned to commit SHAs |
| M-10 container defaults | **Partially fixed** | `57afc0c` | default password removed, email no longer logged, `PB_ENCRYPTION_KEY` wired, data-dir validated. Residual: PocketBase's CLI only accepts the superuser password as argv — documented; a one-shot init job is the mitigation. |
| L-1 mid-chunk corruption | **Fixed** | `bf11d55` | truncate back to the acknowledged offset on read error |
| L-2 expired consumed session | **Fixed** | `bf11d55` | stored result / 404 checked before the expiry branch |
| L-3 GC starvation | **Fixed** | `bf11d55` | keyset pagination; `TestMediaGCPaginatesPastReferencedPage` |
| L-4 nudge hardening | **Fixed** | `675b078` | buffer cap, capped reconnect, line-start `data:` match; `tests/nudge.test.ts` |
| L-5 CI/images | **Fixed** | `57afc0c` | digest-pinned base images, least-privilege permissions, govulncheck + `pnpm audit` |
| L-6 compose/entrypoint | **Fixed** | `57afc0c` | explicit `environment:`, `/pb` data-dir validation, email log removed |
| I-1 opaque cursors | **Documented** | `57afc0c` | README |
| I-2 MIME advisory | **Documented** | `57afc0c` | README (incl. why PB serving neutralises it) |
| I-3 token storage | **Documented** | `57afc0c` | README warning + alternatives |

Not done: CI secret scanning (needs a pinned scanner/action — flagged as a follow-up).

## PR #3 (`karlomac/auth-signup`)

| Finding | Status | Commit | Evidence |
|---|---|---|---|
| A-1 silent fail-open on collection name | **Fixed** | `6435940` | configurable `AUTH_COLLECTION`, boot validation; `TestValidateSetupRejectsMissingOrWrongCollection` |
| A-2 verification not enforced | **Fixed** | `6435940` | password/both mode requires a non-empty `authRule`, actionable error |
| A-3 verifier redirects / cleartext | **Fixed** | `6435940` | no-redirect client, https required except loopback; `TestVerifyTurnstileRefusesRedirects`, config tests |
| A-4 disposable exact-match | **Fixed** | `6435940` | suffix matching; `TestDisposableSubdomainsBlocked` |
| A-5 tests cannot see the tag path | **Fixed** | `6435940` | through-the-router signup test; `TestSignupThroughRouter` |

## Merge note

Both PRs touch `README.md`, `.env.example`, `server/cmd/.../main.go`, the compose
file and the workflows. Merge PR #2 first and rebase PR #3 onto it; then run
both suites once more together (PR #3 was verified on the v0.40.5 base, PR #2
moves the pin to v0.40.6 — the combined tree is the only one CI will see).
