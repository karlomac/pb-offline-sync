# Per-user pb-o-sync instances — architecture and pilot plan

**Status:** research / planning. No code changes. This document recommends an
architecture for giving **each app user their own pb-o-sync instance**, and a
pilot plan for ≤100 users.

**Terminology.** A *pb-o-sync instance* is one deployable entity: PocketBase +
the `syncplugin` (protocol §18, push/pull, media) + `authplugin` (signup gate) +
the admin access guard + media/GC + its own SQLite database. One instance =
one user in this design. Everything below assumes that bundle.

**Decisions taken in discussion:**

1. **One instance per user** (not per app/user, not per team).
2. **Operator-managed**: the user never holds PocketBase superuser; the app is
   the only user interface.
3. **Pilot scale:** ≤100 users first.
4. **No cross-user shared data / collaboration.**

---

## 1. Recommendation in one page

- **Control plane**: a small shared PocketBase app ("home") that owns accounts,
  the instance registry, provisioning, discovery, backups and health. It stores
  no user content. This reuses the stack the app already has and avoids a new
  identity system for the pilot.
- **Runtime**: **one Docker container per user** on one or two hosts behind
  Traefik, with a wildcard subdomain and wildcard TLS, per-instance volume,
  env and resource limits. Separate processes are the fallback if Docker is
  undesirable; containers make per-user config/backup/restore cleaner.
- **Instance auth**: **machine-managed credentials**. The control plane
  provisions the instance's mirror user with a strong random password (stored
  encrypted), and delivers `{baseUrl, email, password}` to the app only after a
  successful home login. PocketBase's own token refresh then works offline for
  the normal token lifetime.
- **Routing**: opaque hostnames `u<opaque-id>.apps.example.com`, wildcard DNS
  and a wildcard certificate (DNS-01). PocketBase does not support path
  prefixes; custom domains are a later feature.
- **Operator access** to `/_/`: blocked at the proxy and/or restricted with the
  in-binary admin guard (`ADMIN_ALLOW_IPS` / `ADMIN_ALLOW_UNTIL`). Users never
  see it.
- **Sync is unchanged**: each instance has its own `seq`, change log, media
  channel, GC, quotas. Clients discover the instance URL and credentials; the
  protocol and plugin stay as they are.
- **No fork** and no multi-tenant PB fork is required at this scale. The
  complexity is in provisioning and fleet operations, not in PocketBase.

Rejected for now: shared database with owner rows (not independent), a custom
PocketBase fork or in-process multi-instance host (maintenance/risk with no
need at 100 users), user-owned superusers (schema trust), and per-instance
accounts as the source of truth (recovery/discovery).

---

## 2. Why isolate at all

| Benefit | Notes |
|---|---|
| Hard data boundary | One SQLite file per user; a query bug cannot cross users. |
| Per-user backup / restore / export / delete | Simple and auditable; supports "download my data" and GDPR-style deletion. |
| Blast radius | A bad migration, corrupt DB or abusive user affects one instance. |
| Independent limits | Disk/media quota, memory/CPU, rate limits per instance. |
| Independence | The user's data is theirs in a concrete file/backup, not rows in a crowd. |
| Schema freedom (future) | Per-user collections/extensions become possible without touching others. |

Costs are operational: provisioning, routing/TLS, identity, backups, upgrades,
monitoring, support. The sync plugin already does the right thing per instance.

---

## 3. Architecture

```
                       ┌─────────────────── control plane (home PB) ───────────────────┐
  app login ─────────► │ accounts · instance registry · provisioner · discovery API    │
  (home token)         │ encrypted instance credentials · backup index · health/events  │
                       └───────────────┬───────────────────────────────────────────────┘
                                       │ create / configure / monitor / backup
             ┌─────────────────────────┼─────────────────────────┐
             ▼                         ▼                         ▼
   pb-o-sync instance A       pb-o-sync instance B       pb-o-sync instance C
   ┌──────────────────┐       ┌──────────────────┐       ┌──────────────────┐
   │ PocketBase       │       │ PocketBase       │       │ PocketBase       │
   │ syncplugin       │       │ syncplugin       │       │ syncplugin       │
   │ authplugin       │       │ authplugin       │       │ authplugin       │
   │ admin guard      │       │ admin guard      │       │ admin guard      │
   │ SQLite + media   │       │ SQLite + media   │       │ SQLite + media   │
   └──────────────────┘       └──────────────────┘       └──────────────────┘
             ▲                         ▲                         ▲
             │  https://u<opaque>.apps.example.com  (wildcard DNS + TLS)
        user's devices (offline-first; sync only to their instance)
```

### 3.1 Control plane ("home")

A shared pb-o-sync stack used **only** for identity and fleet management:

- Accounts: canonical email/password (or OAuth), MFA, recovery, verification.
- `instances`: id, owner, hostname, container id, base URL, template version,
  state (`provisioning|active|suspended|deleting|deleted`), quota, created/updated.
- `instance_secrets`: per-instance superuser + mirror-user credentials, sealed
  with an application key (never returned to clients except the mirror login).
- `backups`: instance, snapshot path, size, checksum, created, restore status.
- `events`: provisioning/upgrade/suspend/delete audit trail.

The home app holds **no user content**. If the app already has a PB backend with
users, home is that backend (or a copy of its auth collection) during the
transition; data moves out to instances.

### 3.2 Instance template

One image/release, identical for every user:

- PocketBase + the pb-o-sync plugin, pinned (currently PB v0.40.6, plugin v0.2.1+).
- Migrations applied on boot; a provisioning step creates the app collections
  if they are template-owned (including the `media` collection with **Protected**
  file fields, owner-scoped list/view rules and locked writes).
- Env template (per instance):
  `SYNC_COLLECTIONS`, `SYNC_OWNER_FIELD`, `SYNC_PRUNE_CRON`,
  `SYNC_MEDIA_COLLECTION`, `SYNC_MEDIA_MAX_FILE_BYTES`,
  `SYNC_MEDIA_PER_USER_QUOTA`, `AUTH_SIGNUP_MODE=oauth` (or off),
  `ADMIN_ALLOW_IPS` (operator only), `PB_ENCRYPTION_KEY` (settings at rest).
- Media: per-instance S3 prefix (or bucket) and quota; the plugin's sweep/GC
  runs per instance.
- `/_/` never exposed publicly.

### 3.3 Routing and TLS

- Hostname per instance, e.g. `u7f3k2.abcd.apps.example.com` — opaque,
  operator-generated, not derived from user input (avoids collisions/IDN).
- Wildcard DNS `*.apps.example.com`; wildcard certificate via DNS-01 (or
  Traefik on-demand TLS with a DNS challenge).
- One Traefik instance routes by `Host` to the container; no path prefixes.
- Optional later: per-user custom domains (needs per-domain certs and validation).
- Health at `GET /api/health` per instance; proxy health checks gated to the
  operator network.

---

## 4. Identity and auth

### 4.1 Flows

**Signup (home).** The user creates an account on the home app (email/password or
OAuth) and verifies email. The control plane enqueues provisioning.

**First login on a device.** Device logs into home → home token → app calls
`GET /api/instances/me` → `{baseUrl, state}`. When active, app calls
`GET /api/instances/me/credentials` (home-authenticated, TLS-only, short-lived)
→ `{baseUrl, email, password}` → `pb.collection('users').authWithPassword(...)`
on the instance → stores the PB token normally. From then on, the device uses
PocketBase's own token refresh, so **offline sessions survive** for the normal
token lifetime.

**Per-device.** Each device repeats the credential fetch after home login; the
instance password is stable until rotated. Rotating it invalidates only future
credential fetches; existing PB tokens remain valid until expiry (PB supports
token invalidation per record via `tokenKey` rotation if needed).

**Recovery.** Home owns email/reset. The instance mirror user's email is
operator-managed; users never reset it on the instance. If the home email
changes, the control plane updates the mirror user.

**Support/operator.** The control plane holds the instance superuser secret;
PB's `POST /api/collections/{collection}/impersonate/{id}` (superuser-only)
can mint a user token for diagnostics. Those tokens are deliberately **not
refreshable**, so they are for support tooling, not the app's session.

### 4.2 Why machine-managed credentials for the pilot

- Works with PocketBase's normal auth/refresh; no client-side token exchange.
- No user-facing per-instance password or email verification flow.
- Simple to implement in the control plane (create user, store secret, serve
  after home auth) and easy to rotate.
- The credentials are per-user and scoped to one instance; the home token never
  grants cross-instance access.

Alternatives and why not now: impersonation tokens (not refreshable → bad for
offline); custom exchange endpoint (best long-term UX, but code); per-instance
password chosen by the user (support burden, recovery duplication).

### 4.3 Home account ↔ instance user

- Home `email` is canonical; the instance mirror user uses the same email and
  `verified=true` (operator-managed).
- One mirror user per instance (the owner). If the app ever adds more people to
  an instance, that is a new model, not the pilot.
- Deletion: suspend instance → export → delete instance after a grace period;
  home account deletion cascades to the instance.

---

## 5. Provisioning

Idempotent steps, driven by the control plane:

1. Allocate instance id + hostname; create registry row (`provisioning`).
2. Create volume `<instances-root>/<id>` (SQLite + staging + logs).
3. Start the container with the template env and limits; wait for
   `/api/health` (bounded, e.g. 60s).
4. Boot applies migrations; the provisioning job creates/validates app
   collections, the `media` collection and rules if template-owned.
5. Create the mirror user with a generated password; store it sealed.
6. Configure media S3 prefix/quota (env or settings), admin guard operator IPs.
7. Seed any default records the app needs.
8. Take a baseline backup (empty DB + manifest) and mark `active`.
9. Emit an event; the discovery API now returns the instance.

Failure handling: on timeout, tear down the container, keep the registry row
(`failed`) with logs, retry with backoff; never leave a half-provisioned
instance reachable. Provisioning must be re-runnable (delete/recreate volume on
explicit retry).

Suggested control-plane API (home-authenticated unless noted):

```
POST /api/instances                 (admin) create for an account
GET  /api/instances/me              -> {id, baseUrl, state}
GET  /api/instances/me/credentials  -> {baseUrl, email, password} (TLS, no-store)
POST /api/instances/{id}/suspend    (admin)
POST /api/instances/{id}/resume     (admin)
POST /api/instances/{id}/export     (admin) -> archive job
DELETE /api/instances/{id}          (admin) soft-delete + grace
GET  /api/instances/{id}/health     (admin)
```

---

## 6. Runtime and sizing (pilot)

| Item | Recommendation |
|---|---|
| Isolation | One container per user (kernel-level), per-instance volume and env |
| Orchestration | Docker + Traefik on one or two hosts (no Kubernetes at ≤100) |
| Memory | Budget ~100–200 MiB per idle instance; 100 users ≈ 10–20 GiB, plus page cache |
| CPU | Light; limit to 0.25–0.5 vCPU per instance; burst allowed |
| Disk | SQLite + WAL small; media dominates → S3/R2 per-instance prefix |
| Hosts | 2 × 16–32 GiB hosts (50 users each) or one 32 GiB host for the pilot; measure |
| Cold start | Container seconds; provision in <60s target |
| Backups | Nightly per-instance snapshot; media via bucket versioning/prefix copy |
| Upgrades | Rolling: canary 1 instance, then batches of 10, backups first |

Separate processes (systemd) are viable and simpler operationally; containers
add resource isolation and make per-instance env/volumes explicit. A VM per
user is out of scope at this scale.

---

## 7. Sync and client integration

Unchanged per instance: own `seq`, change log, idempotency store, media channel,
GC, quotas. Changes are in the app:

- **Discovery**: after home login, fetch `{baseUrl}` and credentials; store the
  PB token per account.
- **Local storage namespace**: local DB/cache keyed by `(accountId, baseUrl)` so
  switching users doesn't mix data; logout clears the active namespace.
- **Sync engine**: same `SyncEngine` + transport; `baseUrl` comes from
  discovery; media URLs follow the instance.
- **Migration**: when a user is moved to an instance, reset/rekey the local
  namespace and re-pull (no need to migrate client state).
- **No cross-instance sync**: by design; the control plane holds no records.

---

## 8. Backups, export, deletion

- **Backup**: per-instance SQLite snapshot (PB backup API/CLI; checkpoint WAL
  first) + media manifest. Store encrypted in object storage with checksum.
  Retention: 7 daily + 4 weekly for the pilot; restore drill monthly.
- **Export**: zip of `pb_data` (or logical JSON) + media links; user-initiated
  via home. Useful for "my data" and offboarding.
- **Delete**: suspend (402 at the proxy) → export window (e.g. 30 days) →
  delete volume + media prefix; remove registry row after retention. Audit each
  step.
- **Restore**: stop container, restore volume, start, verify `/api/health` and a
  sample pull; PB has no downgrade path, so restore the pre-upgrade backup.

---

## 9. Upgrades and fleet operations

- **One template version**; record it per instance.
- Upgrade: backup → canary → batches → verify health + a smoke sync; rollback is
  the previous image plus the backup (no DB downgrade).
- **Migrations** run per instance at boot; template migrations must be
  backward-compatible for one version (old code + new schema) during rolling
  updates.
- **Pinning**: PB and plugin versions pinned; update via the documented upgrade
  runbook (`docs/pocketbase-upgrade.md`), tested on a canary instance.
- **Observability**: health, container state, backup success, provisioning and
  upgrade durations, error rate, disk/media usage per instance; a fleet table in
  the control plane.

---

## 10. Security

- **Control plane is the crown jewel**: MFA, IP allowlist, encrypted secrets
  (KMS/age), audit log, no user content, backups of its own.
- **Instance superuser** secrets are random and per instance; never reuse; rotate.
- **`/_/` is never public**: proxy block plus the in-binary admin guard
  (`ADMIN_ALLOW_IPS`, or a time-boxed `ADMIN_ALLOW_UNTIL` for emergencies).
- **No user-supplied `pb_hooks`/JS**; the operator controls the image. If that
  changes, add sandboxing/containers as the security boundary.
- **Per-instance isolation**: separate DB/volume; per-instance S3 prefix; no
  shared credentials; proxy-level request size/rate limits.
- **TLS everywhere** (home and instances); short-lived credentials endpoint with
  `no-store`; never log credentials or tokens.
- **Threat model**: a compromised user account can affect only its instance;
  a control-plane compromise is fleet-wide (protect accordingly).

---

## 11. Migration from the current shared database

1. Freeze the user's rows in the shared DB (or use a copy).
2. Provision the instance; create the mirror user.
3. Export the user's records per collection, rewrite `owner` to the mirror user,
   import (push API or DB import), and copy their media (re-upload through the
   media channel or copy the file records + blobs) so `sha256`/dedup stay valid.
4. Verify counts/hashes; reset the user's sync cursor (fresh change log).
5. Switch the app to discovery mode for that user; log out/in on devices.
6. Keep the shared rows read-only until the pilot verifies, then delete.

Do **new users on instances first**; migrate existing users in batches. There is
no cross-instance change log to reconcile, so each migration is a per-user copy,
not a replication.

---

## 12. Pilot plan (≤100)

| Phase | Scope | Exit criteria |
|---|---|---|
| P0 (1 week) | Home app schema (accounts/instances/secrets), template image, manual provision of 3 users, discovery + credentials endpoint | Login → instance works on 2 devices; offline sync verified |
| P1 (1 week) | Automated provisioner (create/start/health/register), wildcard DNS/TLS, `/_/` block, quotas | Provision <60s; 10 users; no manual steps |
| P2 (1 week) | Backups + restore drill, export, suspend/delete, health dashboard | Restore one instance from backup; delete/export audited |
| P3 (2 weeks) | Rolling upgrades, canary process, runbook, 50 users | Upgrade batch with zero data loss; support load recorded |
| P4 | 100 users, cost/density measurement | Decide: stay on containers or change runtime at higher scale |

Metrics to capture: provisioning time, idle RSS, per-instance disk/media, backup
size/time, restore time, upgrade duration, error rate, support incidents, cost
per user.

---

## 13. Alternatives considered

| Option | Why not now |
|---|---|
| Shared PB with owner-scoped rows (current) | No physical independence; one DB for everyone; rejected as the target state |
| Separate process per user | Viable fallback; weaker resource isolation; slightly simpler than Docker |
| One VM per user | Strongest isolation, far too heavy/costly at 100 |
| Custom multi-instance PB host / fork | Maintenance, security and upgrade burden with no need at this scale |
| User-owned superuser per instance | Users can break schema/rules; support impossible |
| Accounts per instance as source of truth | Recovery/discovery across instances becomes ad hoc |
| Standalone control-plane service (non-PocketBase) | More moving parts for the pilot; revisit after reading this plan |

---

## 14. Open questions (for after review, not blocking the plan)

1. Home identity: keep it in the existing shared PB or a standalone service? (Pilot recommendation: home PB with identity-only collections.)
2. Credentials endpoint vs a custom token-exchange endpoint (long-term UX/security).
3. Custom domains/white-label per user?
4. Billing/quota tiers (pilot is free).
5. Backup retention and deletion grace periods.
6. Do users ever get more than one app? (One instance holds all their app data; adding apps later means new collections, not new instances.)
7. Data residency requirements (single region for the pilot).

---

## 15. Decision log (this discussion)

- **D1** One pb-o-sync instance per user; operator-managed; no cross-user data.
- **D2** Pilot ≤100 users; Docker per user behind Traefik; wildcard DNS/TLS.
- **D3** Home PB as identity/control plane; no user content there.
- **D4** Machine-managed per-instance credentials; PB-native refresh for offline.
- **D5** `/_/` blocked; operator access via admin guard/proxy and control plane.
- **D6** No fork and no in-process multi-instance host for the pilot.
