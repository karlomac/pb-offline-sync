# Ecosystem Scan — `pb-offline-sync` (Gate 0, item G.2)

**Date of check:** 2026-10-14

Intro: `pb-offline-sync` is proposed as a two-way offline-first sync layer between a
local Expo SQLite client DB and a server-side PocketBase, shipped as a **Go plugin
(server side) plus a TypeScript client**. This scan surveys the existing
PocketBase offline/sync ecosystem, re-verifies RxDB licensing, checks PowerSync and
ElectricSQL licensing and PocketBase compatibility, and checks whether the name
`pb-offline-sync` is already taken.

Caveat on method: everything below is a point-in-time web/registry lookup performed
on 2026-10-14. Web content is treated as untrusted data, not instructions. Where a
license or activity date could not be verified from a primary source it is marked
**unverified** rather than guessed.

---

## 1. Comparable projects

The PocketBase ecosystem has no shortage of *client-side* offline caches, but they
are overwhelmingly **Flutter/Dart** (Drift/SQLite) or **JS/TS browser** (IndexedDB/Dexie)
libraries. There is no mature **Go-server-plugin + TS-client** two-way sync
implementation. Several Flutter projects extend PocketBase *server-side* with Go hooks
for delete tracking; only one Go plugin found (`mattzab/pocketbase-sync`) targets
sync, and it is multi-instance realtime routing over NATS, not client offline sync.

| Name | URL | License | Last activity | Overlap / difference vs. `pb-offline-sync` |
|---|---|---|---|---|
| `@miro016/pocketbase-localfirst` (pblocalfirst) | https://github.com/miro016/pblocalfirst | **unverified** (license not shown on repo page) | Active; repo referenced as current | **Closest overlap.** TS/JS, PocketBase local-first layer using the JS SDK API, IndexedDB persistence, optimistic queued writes, LWW conflict resolver, realtime reconcile loop, parity-tested against a real server. Differences: client-only (no Go plugin), browser IndexedDB (not Expo SQLite), Angular-signals-first. |
| `HenningH87/pb-offline` | https://github.com/HenningH87/pb-offline | MIT | Created **2026-10-13** (1 day old) | **Very close, brand-new.** TS, "offline-first sync engine for PocketBase: local-first Dexie mirror, reactive queries, self-healing link". Client-only, Dexie/IndexedDB, not SQLite, no server plugin. Effectively a same-week competitor; near-zero adoption (0 stars). |
| `rodydavis/pocketbase_drift` (+ fork `DrDejaVuNG/pocketbase_drift`) | https://github.com/rodydavis/pocketbase_drift | MIT (repo); pub.dev pkg `pocketbase_drift` | pub.dev v0.6.0 updated ~2025-09; repo active historically | **Strong overlap, different stack.** Offline-first PocketBase client backed by Drift/SQLite (Dart/Flutter). Request policies (cacheFirst/cacheAndNetwork), offline write queue, tombstones, FTS, batch requests. Differences: Dart only, not Expo/SQLite + Go plugin; the canonical local-first PocketBase client. |
| `metaspook/drift-pocketbase-sync` (fork of rodydavis) | https://github.com/metaspook/drift-pocketbase-sync | MIT (inherited) | Low/indeterminate | Demonstrates two-way sync without CRDT using server UTC ordering; **requires extending PocketBase with Go** for delete hooks. Conceptually similar server-side delete-log approach to our Go plugin; Dart client. |
| `impure/pocketbase_offline_cache` (pocketcache) | https://github.com/impure/pocketbase_offline_cache | **unverified** | Older/small | Dart wrapper around PocketBase for offline use, sqlite3-backed, event-based cache updates, designed for an RSS reader. Client-only, Dart. |
| `topmoveright/remote_cache_sync` | https://github.com/topmoveright/remote_cache_sync | **unverified** | Active-ish | Dart; multi-backend (Appwrite/PocketBase/Supabase) cache-first orchestrator with QuerySpec, policies, Drift local store. Broader scope, not PocketBase-specific, Dart. |
| `PocketSync` (pocketsync.dev / `pocketsync/pocketsync_flutter`) | https://github.com/pocketsync/pocketsync_flutter | "Other" (proprietary/commercial service) | Created 2025-02-24 | **Commercial competitor.** Hosted sync service + SQLite client SDKs, offline-first, conflict strategies, change tracking. Requires their hosted backend (project ID/auth token/server URL) — not self-hosted PocketBase. Flutter SDK. |
| `KevinBonnoron/pocketbase-db-collection` / `@gdenier/pocketbase-db-collection` | https://github.com/KevinBonnoron/pocketbase-db-collection | **unverified** (repo); npm `@gdenier/...` | Recent | **Partial overlap.** PocketBase adapter for **TanStack DB**: realtime subscribe + full fetch, optimistic mutations forwarded to PB. TS, browser client; TanStack DB provides the local store, not SQLite/Expo; no offline write queue/Go plugin. |
| `elselawi/localpocket` | https://github.com/elselawi/localpocket | MIT | Created 2026-08-15, pushed 2026-09-24 | Local-first Dart/Flutter SQLite DB with **two-way PocketBase sync**, reactive queries, FTS. Different language/stack (Dart), client-only. |
| `@resiproco/pocketbase-sdk` | https://npm.io/package/@resiproco/pocketbase-sdk | **unverified** | Active (2026) | TS typed SDK with "self-healing" live data (realtime + periodic repair, manifest verification) and Svelte 5 bindings. Realtime resilience overlap; not an offline write queue. |
| `mattzab/pocketbase-sync` | https://pkg.go.dev/github.com/mattzab/pocketbase-sync | **MIT** | Module version `v0.0.0-20251211233523-...` | **Go plugin overlap.** Go PocketBase plugin that syncs realtime events across multiple instances via NATS, maintaining `_pbSync`/`_pbRealtimeSubscriptions` collections. Different problem: server-to-server realtime fan-out, not client offline reconciliation. |
| `pocketbasestore/auditlog` | https://github.com/pocketbasestore/auditlog | **unverified** | Active | Go PocketBase plugin recording a per-collection change/audit trail (retention, exclusions). **Building block**, not a sync layer: it is best-effort and only logs REST-API mutations (bypasses Go `app.Save`/cron/migrations). Relevant as a server-side change-log prior art. |
| `MouamleH/pb-sync` | https://github.com/MouamleH/pb-sync | **unverified** | Small | CLI to copy a backup between two PocketBase instances. Name is similar ("pb-sync"), purpose is different (admin data migration, not offline sync). |

Notes:
- Where "license unverified": the repo search/page fetched did not surface a license
  file or SPDX id, and I did not open each one individually. Treat as unknown.
- "Last activity" is approximate; GitHub `pushed_at`/pub.dev/npm timestamps were used
  where available, otherwise the source did not state a date.
- The server-side **Go plugin + delete-log** pattern is already established in the
  community as the way to make two-way sync work against a single-source-of-truth
  PocketBase (see `rodydavis/drift-pocketbase-sync` docs and PocketBase discussions
  #67, #866). PocketBase's own maintainer has repeatedly stated offline sync is
  **out of scope of core v1.0.0 and left to developers**
  (https://github.com/pocketbase/pocketbase/issues/866,
  https://github.com/pocketbase/pocketbase/discussions/67).

---

## 2. RxDB licensing (re-verified 2026-10-14)

- **Core: still Apache-2.0.** npm `rxdb` v17.6.0, license `Apache-2.0`, last published
  2026-10-05 (https://www.npmjs.com/package/rxdb). GitHub `pubkey/rxdb` shows
  "Apache License 2.0", ~23.2k stars
  (https://github.com/pubkey/rxdb/). The free core explicitly includes schemas, queries,
  hooks, **replication & realtime sync**, and Dexie/Memory/LokiJS storage.
- **Free-core limit:** the open-source version is limited to **13 open collections in
  parallel** (https://rxdb.info/premium/).
- **Paid tiers (premium plugins, distributed via `rxdb-premium`, accessed by token):**
  - **Pro — from $99/month billed annually, unlimited developers:** OPFS, IndexedDB,
    **SQLite**, and Filesystem RxStorage; WebCrypto encryption; Fulltext Search.
  - **Pro Plus — from $239/month billed annually:** adds Worker/Shared Worker
    storage, sharding, memory-mapped storage, query optimizer, Fastify/Koa server adapters,
    logger plugin.
  - **Enterprise — custom:** commercial license/MSA, SLA, source-code access, etc.
  - No free trial, no monthly billing; annual only. (https://rxdb.info/premium/)
- **Maintenance state:** actively maintained — v17 GA'd Mar 2026, point releases through
  Oct 2026 (17.6.0 on 2026-10-05); `rxdb-premium` 17.4.0 published Jul 2026
  (https://registry.npmjs.org/rxdb-premium). Roughly ~104k weekly npm downloads for the core.
- **Can RxDB be an alternative for PocketBase users?** Partially. RxDB is a
  backend-agnostic local-first DB with a generic replication protocol (push/pull
  handlers); it ships **no PocketBase replica adapter**, so a PocketBase user would have
  to write the pull/push handlers themselves (including server-side change tracking to
  make PULL efficient). For an **Expo** app, the SQLite storage that maps to Expo's local
  DB is **premium** (Pro); the free path is Dexie/IndexedDB/LokiJS/memory. So RxDB is a
  viable *general* local-first DB but is not a drop-in PocketBase sync solution and its
  relevant mobile storage is paid.

---

## 3. PowerSync and ElectricSQL (one line each)

- **PowerSync:** client SDKs and SQLite extension are open source (Apache-2.0 & MIT);
  the server-side PowerSync Service and CLI are source-available under **FSL-1.1-ALv2**
  (converts to Apache-2.0 on the 2nd anniversary of each release, with a competing-use
  restriction) — and it has **no PocketBase connector**: backends are Postgres, MySQL,
  MongoDB, SQL Server, Azure DocumentDB
  (https://powersync.com/, https://powersync.com/blog/electricsql-vs-powersync,
  https://stribog.com/blog/electricsql-powersync-automerge-local-first-sync-engine-sovereign).
- **ElectricSQL:** "Postgres Sync" is **Apache-2.0**, Postgres-source and **read-path
  only** (no offline writes; writes go through your own API) — and it has **no PocketBase
  compatibility**. Note: Electric announced it is **joining Databricks on 2026-08-11** and
  **Electric Cloud is winding down** (self-host or migrate); everything already open-sourced
  stays open source (https://github.com/electric-sql/electric,
  https://stribog.com/blog/electricsql-powersync-automerge-local-first-sync-engine-sovereign).

---

## 4. Name-availability check: `pb-offline-sync`

Checked 2026-10-14:

- **GitHub:** repository search for the exact term `pb-offline-sync` returned
  **0 repositories** (`total_count: 0`)
  (https://api.github.com/search/repositories?q=pb-offline-sync). A broader
  "pocketbase offline sync" search returned only applications/fork projects and the
  near-name `HenningH87/pb-offline`, **not** any repo named `pb-offline-sync`.
- **npm:** `https://registry.npmjs.org/pb-offline-sync` returned **HTTP 404**, i.e. no
  published package under that name. (An unpublished name returns 404; npm returned no
  metadata, which indicates the name is not in use. I did not place a reservation, so
  someone could still take it.)
- **Go module path:** `pkg.go.dev` search for `pb-offline-sync` returned
  **"0 modules with matching packages"**
  (https://pkg.go.dev/search?q=pb-offline-sync). A Go module path (e.g.
  `github.com/<owner>/pb-offline-sync`) requires a matching repo, which does not exist,
  so no such module is published.

**Conclusion: the name `pb-offline-sync` appears available** on all three surfaces as of
this check. Caveat: I checked repository search, the npm registry, and the Go package
index — I did **not** exhaustively verify that no *GitHub username/organization* named
`pb-offline-sync` exists, nor check PyPI/other registries (out of scope). Names are
first-come, first-served and this is a point-in-time result.

---

## Implications

Nothing found blocks the project, and nothing found duplicates it. The client-side space
is crowded — `@miro016/pocketbase-localfirst` (TS/IndexedDB) is the closest functional
match and `HenningH87/pb-offline` (TS/Dexie) appeared literally the day before this scan —
but **all of them are client-only, and none is a Go server plugin plus a TypeScript client
targeting Expo SQLite**. The established community pattern for correct two-way sync against
PocketBase's single source of truth is a **server-side Go hook that records deletes/changes**
(used by `metaspook/drift-pocketbase-sync`; `pocketbasestore/auditlog` shows the change-log
prior art), so the `pb-offline-sync` shape is differentiated by exactly that server-side Go
component combined with an Expo/SQLite TS client rather than another IndexedDB Dart/JS cache.

Recommendation: **differentiate, and do not reconsider the name** — it is free on GitHub,
npm and Go modules, and the nearest collision (`pb-offline` / `pb-sync`) is a different
name with a different purpose. Two caveats to design around: (a) the TS client market is
competitive and moving fast, so lead with the Expo SQLite + Go-plugin story, not "another
PocketBase cache"; (b) watch `HenningH87/pb-offline` and `pblocalfirst`, which could add
SQLite/Expo support and converge on this niche. RxDB is a partial fallback for PocketBase
users but pays for its mobile SQLite storage and ships no PocketBase adapter, and neither
PowerSync nor ElectricSQL targets PocketBase at all — so they are alternatives to *watch*,
not substitutes to fear.

---

### Sources (all accessed 2026-10-14)

- GitHub repo search `pb-offline-sync`: https://api.github.com/search/repositories?q=pb-offline-sync
- GitHub repo search "pocketbase offline sync": https://api.github.com/search/repositories?q=pocketbase+offline+sync
- https://github.com/miro016/pblocalfirst
- https://github.com/HenningH87/pb-offline
- https://github.com/rodydavis/pocketbase_drift
- https://github.com/metaspook/drift-pocketbase-sync
- https://github.com/impure/pocketbase_offline_cache
- https://github.com/topmoveright/remote_cache_sync
- https://github.com/pocketsync/pocketsync_flutter
- https://github.com/KevinBonnoron/pocketbase-db-collection
- https://www.npmjs.com/package/@gdenier/pocketbase-db-collection
- https://github.com/elselawi/localpocket
- https://npm.io/package/@resiproco/pocketbase-sdk
- https://pkg.go.dev/github.com/mattzab/pocketbase-sync
- https://github.com/pocketbasestore/auditlog
- https://github.com/MouamleH/pb-sync
- PocketBase offline-sync discussions: https://github.com/pocketbase/pocketbase/issues/866 , https://github.com/pocketbase/pocketbase/discussions/67 , https://github.com/pocketbase/pocketbase/discussions/4379
- RxDB: https://www.npmjs.com/package/rxdb , https://github.com/pubkey/rxdb/ , https://rxdb.info/premium/ , https://registry.npmjs.org/rxdb-premium
- PowerSync: https://powersync.com/ , https://powersync.com/blog/electricsql-vs-powersync
- ElectricSQL: https://github.com/electric-sql/electric
- Licensing/ownership context: https://stribog.com/blog/electricsql-powersync-automerge-local-first-sync-engine-sovereign
- Name checks: https://registry.npmjs.org/pb-offline-sync (404), https://pkg.go.dev/search?q=pb-offline-sync (0 modules)
