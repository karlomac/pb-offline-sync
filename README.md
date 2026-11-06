# pb-offline-sync

Two-way, offline-first sync between a local client database and a server-side
[PocketBase](https://pocketbase.io). Built as a Go plugin for PocketBase plus a
TypeScript client, joined by a written protocol and a language-neutral
conformance suite.

The app reads and writes **only local data** and stays fully usable offline; a
background engine reconciles it with PocketBase when connectivity returns.

> **Status: pre-release.** The project is **`pb-offline-sync`** and the Go module
> is `github.com/karlomac/pb-offline-sync/server`. Nothing is published yet (not on
> npm or the Go module proxy), so install from source (below); the npm scope
> `@pb-offline-sync/*` is reserved for the client packages. Do not imply official
> PocketBase endorsement.

---

## Contents

- [What it does](#what-it-does)
- [How it works](#how-it-works)
- [Repository layout](#repository-layout)
- [Part 1 — Server: the PocketBase build](#part-1--server-the-pocketbase-build)
- [Part 2 — PocketBase configuration](#part-2--pocketbase-configuration)
- [Part 3 — Deploying on Dokploy](#part-3--deploying-on-dokploy)
- [Part 4 — Bare metal: Linux or Windows](#part-4--bare-metal-linux-or-windows)
- [Part 5 — Client: use it in a SvelteKit or React project](#part-5--client-use-it-in-a-sveltekit-or-react-project)
- [Testing](#testing)
- [Releases](#releases)
- [Compatibility](#compatibility)
- [Non-goals](#non-goals)
- [Roadmap](#roadmap)
- [Contributing](#contributing)
- [License](#license)

---

## What it does

- **Two-way sync** of per-user owned records; the UI talks to the local DB only.
- **Idempotent, retry-safe push** over flaky networks (every mutation has a
  stable id; the server dedupes replays).
- **Field-level patches** with a server-assigned `rev`, so edits to *different*
  fields on *different* devices merge automatically instead of clobbering.
- **Deterministic conflicts**: overlapping edits are resolved by a per-collection
  policy (`serverWins` / `clientWins` / `reject`) and recorded for the app.
- **Deletes propagate** with both `deleteWins` and `editWins` policies.
- **Realtime nudge**: a PocketBase realtime subscription signals "pull now", so
  updates arrive quickly without polling. The nudge payload is never
  authoritative — correctness only depends on `pull`.

## How it works

```
Client app ─► local DB (SQLite)                  Server: PocketBase + plugin
   │            tables: <collection>, _outbox,        routes: /api/sync/{info,push,pull}
   │            _sync_state, _conflicts               hooks:  record *Execute → sync_changes + rev
   └─► SyncEngine ─► Transport (HTTP + realtime nudge)  tables: sync_meta, sync_changes,
                                                                 sync_processed, sync_conflicts
```

- The **server plugin** hooks every write path (sync push, REST API, admin UI,
  your own hooks) in the same transaction as the data write, assigning a `rev`
  and appending to a change log. That log is what `pull` streams.
- The **client engine** is platform-independent (no storage or transport
  dependency). It keeps an outbox, coalesces pending edits, applies incoming
  changes by `rev`, rebases pending edits over pulls, and does a full resync when
  the server says the cursor is too old.

The wire contract is frozen at protocol **v1.0**: `docs/protocol.md` is the
source of truth. See `docs/architecture.md` for the server design and the
resolved decisions, and `docs/threat-model.md` for the security model.

## Repository layout

```
docs/protocol.md          the frozen v1.0 wire contract (source of truth)
docs/test-scenarios.md    scenario catalog for both sides + coverage gaps
docs/media-sync-plan.md   design proposal for photo/video (media) sync
docs/release-plan.md      versioning scheme + the tag-driven release pipeline
.github/workflows/        CI and release automation
conformance/              language-neutral JSON fixtures both sides must pass
server/                   Go module: the PocketBase plugin (package syncplugin)
server/cmd/pb-offline-sync/   the default PocketBase build (env-configured)
Dockerfile                production image for the bundled build
docker-entrypoint.sh      container entrypoint: admin bootstrap + serve
docker-compose.yml        local / Dokploy Compose deployment
.env.example              deployment env template (never commit the real .env)
client/core/              TypeScript sync engine (no platform dependencies)
client/transport-pocketbase/  HTTP transport (auth + realtime nudge)
client/adapter-expo-sqlite/   first storage adapter + schema/type mapping
examples/expo-notes/      example app (roadmap)
```

---

## Part 1 — Server: the PocketBase build

The server is PocketBase with the sync plugin compiled in. This repo ships that
app at `server/cmd/pb-offline-sync`, configured entirely by environment
variables, so **one binary and one image serve every app** — it is the default
PocketBase build.

### Option A — use the bundled build (recommended)

Build it directly, or let Dokploy build the shipped `Dockerfile` (see Part 3):

```bash
cd server
go build -o pb ./cmd/pb-offline-sync

# no collections listed => plain PocketBase (plugin off)
./pb serve --http=0.0.0.0:8090

# enable sync for one or more collections (they must already exist)
SYNC_COLLECTIONS=notes ./pb serve --http=0.0.0.0:8090

# create the first superuser, then open http://localhost:8090/_/
./pb superuser upsert you@example.com 'a-strong-password'
```

Every collection in `SYNC_COLLECTIONS` must already exist and have the owner
field as a relation to an auth collection, or the server **refuses to start**
(config is validated at boot). Configuration:

| Environment variable | Default | Meaning |
|---|---|---|
| `SYNC_COLLECTIONS` | *(empty)* | comma-separated collections to sync; empty keeps the plugin off |
| `SYNC_OWNER_FIELD` | `owner` | the owner relation field |
| `SYNC_CONFLICT` | `serverWins` | `serverWins` \| `clientWins` \| `reject` |
| `SYNC_DELETE_POLICY` | `deleteWins` | `deleteWins` \| `editWins` |
| `SYNC_MAX_BATCH` | `100` | max mutations per push |
| `SYNC_MAX_PAGE` | `500` | max changes per pull page |
| `SYNC_DEFAULT_PAGE` | `200` | pull page size when the client omits one |
| `SYNC_MAX_BODY_BYTES` | `1048576` | max push body size |
| `SYNC_IDEMPOTENCY_RETENTION_DAYS` | `14` | idempotency-record retention |
| `SYNC_CHANGELOG_RETENTION_DAYS` | `30` | change-log retention (server-side) |
| `SYNC_PRUNE_CRON` | `0 3 * * *` | cron for retention pruning; `none` disables |
| `SYNC_MEDIA_COLLECTION` | *(empty)* | media collection name; empty keeps media sync off |
| `SYNC_MEDIA_MAX_FILE_BYTES` | `0` | max media file size (0 = unlimited; set it in production) |
| `SYNC_MEDIA_PER_USER_QUOTA` | `0` | per-user media quota in bytes (0 = unlimited) |
| `SYNC_MEDIA_MIME_ALLOWLIST` | `image/,video/,audio/` | exact MIME types or `type/` prefixes |

The container image's entrypoint also understands `PB_ADMIN_EMAIL` /
`PB_ADMIN_PASSWORD` (superuser bootstrap), `PB_DATA_DIR` (default
`/pb/pb_data`, must stay under `/pb`), `PB_HTTP_ADDR` (default
`0.0.0.0:8090`) and `PB_ENCRYPTION_KEY` (when set, PocketBase settings such as
SMTP/OAuth secrets are encrypted at rest via `--encryptionEnv`).

### Signup gating (optional auth plugin)

`server/authplugin` gates new-account signup, off unless `AUTH_SIGNUP_MODE` is
set. It only gates **creation**; sign-in, OTP and admin flows are untouched.

| Environment variable | Default | Meaning |
|---|---|---|
| `AUTH_SIGNUP_MODE` | *(empty)* | `oauth` \| `password` \| `both`; empty disables the plugin |
| `AUTH_COLLECTION` | `users` | auth collection to gate (validated at boot) |
| `AUTH_TURNSTILE_SECRET` | — | Cloudflare Turnstile secret (required for `password`/`both`) |
| `AUTH_TURNSTILE_HOSTNAMES` | *(empty)* | allowed Turnstile hostnames (comma-separated; recommended) |
| `AUTH_TURNSTILE_VERIFY_URL` | Cloudflare siteverify | verifier URL; must be https (http only for loopback) |
| `AUTH_ALLOWED_EMAIL_DOMAINS` | *(empty)* | allowlist, exact domain match (`@` optional) |
| `AUTH_DISPOSABLE_DOMAINS` | *(empty)* | blocklist, exact domain **or subdomain** match |

The server refuses to start when `AUTH_COLLECTION` is missing/not an auth
collection (a renamed collection would otherwise silently disable the gate), or
in `password`/`both` mode when the collection's **auth rule** does not require
verification. Set it to `verified = true` so a Turnstile-gated account is
unusable until the verification email is confirmed; the plugin sends that email
on password signup. Disposable-domain blocking covers subdomains, so
`x.mailinator.com` is blocked by `mailinator.com`.

The Turnstile verifier is called over TLS, refuses redirects (so the secret can
never be replayed to another host), and is bounded to 5 s / 1 MiB. Signup has no
built-in rate limit: put `/api/collections/<auth>/records` behind PocketBase's
rate limiter or a proxy limit.

### Option B — embed the plugin in your own PocketBase app

Use this if you already have a PocketBase app, or want to configure it in code
rather than by environment.

```bash
mkdir pb-sync-server && cd pb-sync-server
go mod init example.com/pb-sync-server
```

Because the plugin is not published yet, point Go at a checkout of this repo with
a `replace` directive. `go.mod`:

```
module example.com/pb-sync-server

go 1.27

require (
	github.com/karlomac/pb-offline-sync/server v0.0.0
	github.com/pocketbase/pocketbase v0.40.6
)

replace github.com/karlomac/pb-offline-sync/server => ../pb-offline-sync/server
```

#### `main.go`

```go
package main

import (
	"log"
	"time"

	"github.com/pocketbase/pocketbase"

	"github.com/karlomac/pb-offline-sync/server/syncplugin"
)

func main() {
	app := pocketbase.New()

	syncplugin.MustRegister(app, syncplugin.Config{
		Collections: map[string]syncplugin.CollectionConfig{
			// key = the PocketBase collection name you want to sync
			"notes": {
				OwnerField:   "owner",               // a relation field → your auth collection
				Conflict:     syncplugin.ServerWins, // ServerWins | ClientWins | Reject
				DeletePolicy: syncplugin.DeleteWins, // DeleteWins | EditWins
			},
		},

		// Optional limits (defaults shown). Advertised via GET /api/sync/info.
		MaxBatch:                 100,
		MaxPage:                  500,
		DefaultPage:              200,
		MaxBodyBytes:             1048576,
		IdempotencyRetentionDays: 14,
		ChangeLogRetention:       30 * 24 * time.Hour, // server-side only
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
```

`MustRegister` validates your config against the real schemas at startup and
**fails fast** (the app refuses to serve) if a collection is missing, is a view,
or its owner field is absent or is not a relation to an auth collection. It also
registers the plugin's migrations, which run automatically on first boot and
create the `sync_*` tables.

#### Build and run

```bash
go mod tidy
go build -o pb .

# first run creates pb_data/ and the schema
./pb serve --http=0.0.0.0:8090

# create the first superuser (admin), then open http://localhost:8090/_/
./pb superuser upsert you@example.com 'a-strong-password'
```

The sync endpoints are then live and auth-gated:

| Endpoint | Purpose |
|---|---|
| `GET /api/sync/info` | protocol version, configured collections, limits |
| `POST /api/sync/push` | send a batch of local mutations |
| `GET /api/sync/pull` | fetch changes since an opaque cursor |

All three require the `X-Sync-Protocol: 1` header and a valid PocketBase auth
token (`Authorization: <token>`).

---

## Part 2 — PocketBase configuration

### Collections

For **each** collection you list in `Config.Collections`:

1. It must be a **base collection** (not a view).
2. It needs an **owner field** — a `relation` field, `MaxSelect: 1`, pointing at
   an **auth collection** (PocketBase's default `users`, or your own). In this
   doc the field is called `owner`.
3. The example uses a `notes` collection with a required `title` text field (and
   optionally an `editor`/`body` field).

Create it in the admin UI (`/_/`) or programmatically. `id`, `created`,
`updated`, `collectionId` and the owner field are **server-authoritative** —
clients may never send them; the server rejects them with `forbidden_field`.

### API rules

The plugin's routes enforce ownership itself: the owner is taken from the
authenticated user and is never trusted from the request, and `pull` filters by
owner in the data layer. The plugin's routes are **separate from** PocketBase's
collection API rules; if your app also calls PocketBase directly, set those
collection rules to what you want (typically owner-only, or deny and force all
access through sync).

File fields are **not** synced in v1: clients receive file field values on pull
but will not push changes to them (`file_field_unsupported`).

### Media sync (protocol §18)

Media is **off** unless `SYNC_MEDIA_COLLECTION` is set. Media records are
server-created and immutable; clients upload bytes through the media channel and
reference the resulting record from ordinary relation fields.

Provision the media collection *before* enabling it, either with
`syncplugin.EnsureMediaCollection(app, "media", usersCollection.Id, maxFileSize,
posterMaxBytes)` in a migration, or by hand with the fixed fields from
`docs/protocol.md` §18.1. Three requirements are security-critical and are
**validated at boot** (the server refuses to start otherwise):

1. the `file` and `poster` file fields must be **Protected** — an unprotected
   field is served by PocketBase to anyone with the URL, no auth (H-1);
2. the collection needs owner-scoped **list/view** rules
   (`owner = @request.auth.id`) or owners cannot download their own media
   (with a nil rule only superusers can);
3. **create/update/delete** rules must stay locked (`null`): the plugin rejects
   media mutations on the sync path, but PocketBase's own record API is governed
   by these rules, so an open write rule would bypass the immutability model
   (M-2).

`EnsureMediaCollection` sets all three and converges collections created by
older versions.

The env-configured binary schedules the maintenance jobs itself whenever
`SYNC_MEDIA_COLLECTION` is set: `SweepMediaStaging` every 30 minutes and
`MediaGC` nightly. If you embed the plugin in your own app instead, you must
schedule them yourself — they are explicit (no goroutines in the library):

```go
app.Cron().MustAdd("media-sweep", "*/30 * * * *", func() {
	_, _ = syncplugin.SweepMediaStaging(app, cfg, time.Now().UTC())
})
app.Cron().MustAdd("media-gc", "15 3 * * *", func() {
	_, _ = syncplugin.MediaGC(app, cfg, time.Now().UTC())
})
```

Set a finite `SYNC_MEDIA_MAX_FILE_BYTES` / `SYNC_MEDIA_PER_USER_QUOTA` in
production: `0` means unlimited, which leaves disk exhaustion as the only
stopping condition. Rate-limit `/api/sync/media/init` at the proxy or with
PocketBase's rate limiter; each `init` allocates a staging directory.

The MIME allowlist is **advisory**: the declared MIME is checked at `init`, but
bytes are not sniffed. That is safe for XSS because PocketBase serves only
images/audio/video/PDF inline and forces `Content-Disposition: attachment` plus
a sandbox CSP for everything else — do not re-serve media from your own handler
based on the declared `mime` field.

`sync_meta` keeps one tombstone row per record ever created (by design, so
deleted ids are never reused); `Prune` does not remove it. Plan storage for it,
and note that `Prune` does remove change-log, idempotency and conflict-diagnostic
rows on their retention schedules.

### Retention / pruning

Change-log and idempotency rows are pruned by `syncplugin.Prune`, which is
**explicit** — the plugin starts no background goroutine. Schedule it yourself,
e.g. daily:

```go
app.Cron().MustAdd("sync-prune", "0 3 * * *", func() {
	if err := syncplugin.Prune(app, cfg); err != nil {
		app.Logger().Error("sync prune failed", "error", err)
	}
})
```

(Keep `cfg` in a variable you pass to `MustRegister`.)

### Deploying PocketBase data

PocketBase stores everything (SQLite DB, uploads) in its `pb_data` directory.
**Back it up and mount it on a persistent volume in any container deployment** —
see below.

---

## Part 3 — Deploying on Dokploy

[Dokploy](https://dokploy.com) is a self-hosted PaaS: it builds your repo into a
container and handles domains, TLS and volumes. Deploy this as a normal Docker
application. (Menu labels vary slightly by Dokploy version — consult the Dokploy
docs if a step differs.)

### 1. Use the shipped `Dockerfile`

This repo already contains a production `Dockerfile` at the root. It builds
`server/cmd/pb-offline-sync`, runs as a non-root user, and exposes a
`/api/health` check — there is nothing to write. Point Dokploy at the **repo
root**.

```bash
# optional local sanity check (plain Docker on a host, not Dokploy)
docker build -t pb-offline-sync .
docker run -p 8090:8090 \
  -v /etc/dokploy/storage/pb-sync/pb_data:/pb/pb_data \
  -e PB_ADMIN_EMAIL=you@example.com -e PB_ADMIN_PASSWORD='YOUR_STRONG_PASSWORD' \
  -e SYNC_COLLECTIONS=notes pb-offline-sync
```

On a Dokploy host, **do not** publish `-p 8090:8090` / a compose `ports:` entry —
Dokploy's Traefik reaches the container over the internal network. See step 4.

#### Environment

This build follows the usual PocketBase-on-Dokploy `.env` (template:
`.env.example`):

| Key | Meaning |
|---|---|
| `PB_ADMIN_EMAIL` / `PB_ADMIN_PASSWORD` | superuser created/refreshed idempotently on every boot |
| `PB_VERSION` | optional build-time PocketBase version. **Blank means the pinned version** (`v0.40.6`), not "latest" — see the note below |
| `PB_DATA_PATH` | absolute host path bind-mounted to `/pb/pb_data` |
| `SYNC_COLLECTIONS`, `SYNC_*` | sync plugin config (Part 1) |

> `PB_VERSION` deliberately differs from the upstream "blank = latest" habit.
> The plugin's change-log/hook behaviour is only verified against the pinned
> PocketBase version (protocol §17.1.4), so a different version is untested; the
> build accepts an override but CI only guarantees the pin.

> Building your **own** PocketBase app instead (Option B in Part 1)? Then the
> plugin is pulled in via a `replace`, and the build context must contain the
> plugin at the replaced path — this repo's root `Dockerfile` already does that
> for the bundled command, so it is only relevant if you fork the app.

### 2. Create the Dokploy application

1. **Create → Application** and connect this repo; leave the **build path** at the
   repo root.
2. **Build type:** `Dockerfile` (the root `Dockerfile`).
3. **Environment:** add the keys from `.env.example` in Dokploy's Environment
   editor (Dokploy has no `env_file`), or use the Compose stack below.
4. **Ports / domain:** map the domain to **container port 8090** and leave the
   host port unpublished — the shipped `docker-compose.yml` uses
   `expose: ["8090"]`, not `ports:`. Publishing `8090:8090` makes a *second* app
   on the same host fail with
   `Bind for 0.0.0.0:8090 failed: port is already allocated`.
5. **Volumes (important):** bind-mount an absolute host path to
   **`/pb/pb_data`** — use `PB_DATA_PATH`, unique per app (e.g.
   `/etc/dokploy/storage/<app>/pb_data`). Without it, your database and uploads
   are lost on redeploy. The image starts as root just long enough to `chown`
   that mount to its `pb` user, then runs the server unprivileged, so a
   root-owned host directory (the usual Dokploy case) works without a manual
   `chown`. If your platform forces a non-root user instead, make the host path
   writable by that user.
6. **Domains:** add your domain and let Dokploy provision TLS; point the client's
   `baseUrl` at `https://your-domain`.

### 3. First boot

On first start the container creates the data dir, runs the plugin migrations
(`sync_meta`, `sync_changes`, `sync_processed`, `sync_conflicts`) and the core
schema, and — because `PB_ADMIN_EMAIL` / `PB_ADMIN_PASSWORD` are set — creates
the superuser automatically (idempotently, on every boot). To do it manually
instead:

```bash
# from the Dokploy container terminal, or `docker exec -it <container> ...`
/pb/pb superuser upsert you@example.com 'a-strong-password' --dir=/pb/pb_data
```

Then open `https://your-domain/_/` to create the auth collection user(s) and the
synced collection(s) from Part 2. Health probe: `GET /api/health`.

> **Order matters on a fresh volume.** The plugin validates `SYNC_COLLECTIONS` at
> boot and refuses to start if a listed collection is missing, so on a brand-new
> `pb_data` deploy **first with `SYNC_COLLECTIONS` unset** (plain PocketBase),
> create the collections in `/_/`, then set `SYNC_COLLECTIONS` and redeploy.
> `/api/sync/*` routes only exist once the plugin is enabled.

### Docker Compose alternative

A ready `docker-compose.yml` is included at the repo root — set
`SYNC_COLLECTIONS` there and run:

```bash
docker compose up -d --build
```

---

## Part 4 — Bare metal: Linux or Windows

Docker is only packaging. The server is a **single static Go binary**, so you can
run it directly on a VM, a workstation, or a Raspberry Pi. Nothing in the sync
layer needs a container.

### Build

```bash
cd server
CGO_ENABLED=0 go build -trimpath -o pb ./cmd/pb-offline-sync
```

Cross-compile elsewhere without a C toolchain (PocketBase's SQLite driver is pure
Go):

```bash
# Windows
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o pb.exe ./cmd/pb-offline-sync
# Linux arm64 (e.g. a Pi)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o pb ./cmd/pb-offline-sync
```

### What changes vs Docker

| | Container | Bare metal |
|---|---|---|
| Process user | starts root, `chown`s the mount, `su-exec pb` | run as a user that owns the data dir — no `chown` dance |
| Admin bootstrap | `PB_ADMIN_*` read by `docker-entrypoint.sh` | `PB_ADMIN_*` do **nothing**; run `superuser upsert` once |
| Env | compose `env_file` / Dokploy editor | systemd `EnvironmentFile` or the shell |
| TLS | Dokploy/Traefik | Caddy/nginx or a Cloudflare Tunnel — PocketBase does not terminate TLS |
| Health | Docker `HEALTHCHECK` | same `GET /api/health`, from your monitoring |

The `SYNC_*` variables and the `media`/`pb_data` layout are identical.

### Linux (systemd)

```bash
sudo useradd --system --home /var/lib/pb-offline-sync pb
sudo install -d -o pb -g pb /var/lib/pb-offline-sync
sudo install -o pb -g pb -m 0755 pb /usr/local/bin/pb-offline-sync

# create the superuser once (or use the installer at /_/)
sudo -u pb SYNC_COLLECTIONS=notes \
  /usr/local/bin/pb-offline-sync superuser upsert you@example.com 'a-strong-password' \
  --dir=/var/lib/pb-offline-sync
```

`/etc/pb-offline-sync.env`:

```dotenv
SYNC_COLLECTIONS=notes
SYNC_OWNER_FIELD=owner
# SYNC_PRUNE_CRON=0 3 * * *
```

`/etc/systemd/system/pb-offline-sync.service`:

```ini
[Unit]
Description=pb-offline-sync (PocketBase + sync plugin)
After=network-online.target
Wants=network-online.target

[Service]
User=pb
Group=pb
EnvironmentFile=/etc/pb-offline-sync.env
ExecStart=/usr/local/bin/pb-offline-sync serve --http=127.0.0.1:8090 --dir=/var/lib/pb-offline-sync
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now pb-offline-sync
curl -s http://127.0.0.1:8090/api/health
```

Listening on a privileged port? Use `AmbientCapabilities=CAP_NET_BIND_SERVICE`
rather than running as root. Put Caddy/nginx in front for TLS.

### Windows

```powershell
mkdir C:\pb-offline-sync
$env:SYNC_COLLECTIONS = "notes"

.\pb.exe superuser upsert you@example.com 'a-strong-password' --dir=C:\pb-offline-sync
.\pb.exe serve --http=0.0.0.0:8090 --dir=C:\pb-offline-sync

New-NetFirewallRule -DisplayName "pb-offline-sync" -Direction Inbound `
  -LocalPort 8090 -Protocol TCP -Action Allow
```

For a real service, use [NSSM](https://nssm.cc/) (sets the environment and
restarts on failure) or Task Scheduler with "Run whether user is logged on or
not":

```powershell
nssm install pb-offline-sync C:\pb-offline-sync\pb.exe serve --http=0.0.0.0:8090 --dir=C:\pb-offline-sync
# NSSM → Environment tab: SYNC_COLLECTIONS=notes
nssm start pb-offline-sync
```

S3-compatible storage is configured the same way in both cases: Admin UI →
**Settings → Files storage** (it lives in the app settings, not in an env var).
Backups are still `pb_data` **plus** the bucket (versioning/replication), as
noted in Part 3.

---

## Part 5 — Client: use it in a SvelteKit or React project

The client is three packages:

| Package | Role | Runs on |
|---|---|---|
| `@pb-offline-sync/core` | the engine, wire types, platform seams | any JS runtime |
| `@pb-offline-sync/transport-pocketbase` | HTTP transport + realtime nudge | any runtime with `fetch` |
| `@pb-offline-sync/adapter-expo-sqlite` | SQLite `StorageAdapter` + type mapping | **Expo / React Native** |

### Install (from source, unpublished)

The packages are not on npm. Add them as workspace or path dependencies, e.g.
`package.json`:

```json
{
  "dependencies": {
    "@pb-offline-sync/core": "file:../pb-offline-sync/client/core",
    "@pb-offline-sync/transport-pocketbase": "file:../pb-offline-sync/client/transport-pocketbase",
    "@pb-offline-sync/adapter-expo-sqlite": "file:../pb-offline-sync/client/adapter-expo-sqlite"
  }
}
```

With a pnpm workspace (`pnpm-workspace.yaml` → `packages: ["client/*"]`) you can
reference them by name and share one lockfile.

### React Native / Expo (fully supported today)

```ts
import PocketBase from "pocketbase";
import { SyncEngine } from "@pb-offline-sync/core";
import { PocketBaseTransport } from "@pb-offline-sync/transport-pocketbase";
import {
  SqliteStorageAdapter,
  openExpoDriver,
  type CollectionSchema,
} from "@pb-offline-sync/adapter-expo-sqlite";

const pb = new PocketBase("https://your-domain");

// 1. Auth (any PocketBase auth flow). The transport reads the token per request.
const transport = new PocketBaseTransport({
  baseUrl: "https://your-domain",
  token: { getToken: () => pb.authStore.token },
});

// 2. Learn the schema/limits from the server (protocol §5), then build the
//    storage schema. `/info` gives each field's name/type/writable; add
//    `multiple: true` for multi-select / multi-relation fields yourself.
const info = await transport.info();
const schema: CollectionSchema = {
  notes: [
    { name: "title", type: "text" },
    { name: "body", type: "editor" },
  ],
};

// 3. Storage + engine.
const storage = new SqliteStorageAdapter({
  driver: await openExpoDriver("app.db"),
  schema,
});
const engine = new SyncEngine({ storage, transport, maxBatch: info.limits.maxBatch, maxPage: info.limits.maxPage });

// 4. Local writes go through the engine (single transaction: data + outbox).
const recordId = "notesmap0q1w2e3"; // 15-char PocketBase-format id you generate
await engine.applyLocal("create", "notes", recordId, { title: "First note" });
await engine.applyLocal("update", "notes", recordId, { title: "Edited offline" });

// 5. Sync. Call it on start, on foreground, on connectivity change, and on the
//    realtime nudge. All calls are single-flight.
await engine.syncNow();

// 6. Optional: realtime nudge (never authoritative — just triggers a pull).
const unsub = transport.subscribe(() => void engine.syncNow());

// Observe state for UI:
console.log(await engine.status()); // { state, pending, lastError, conflicts }
```

Reads are plain local reads through the same adapter:

```ts
const rows = await storage.transaction((tx) => tx.list("notes"));
```

**Cursors are opaque.** Never parse, edit or construct them: they are the
server's bookkeeping, and a hand-edited cursor can silently skip records.

**Lifecycle:** call `engine.syncNow()` (or the convenience hooks
`onForeground()` / `onConnectivity()` / `onNudge()`) whenever the app resumes,
connectivity returns, or the nudge fires. Conflicts surfaced in `status()` come
from `_conflicts`, and can be dismissed with `tx.conflictDelete(id)`.

### SvelteKit / React on the **web** (bring your own storage adapter)

`@pb-offline-sync/core` and `@pb-offline-sync/transport-pocketbase` are
platform-independent and run in the browser — the engine's only requirement is a
`StorageAdapter`. **The web storage adapter is not shipped yet** (only the Expo
SQLite adapter exists), so for SvelteKit or React-on-web you provide one, e.g.
over IndexedDB (Dexie, `idb`, …).

Wiring is identical to the Expo example, minus `openExpoDriver`.

> **Token storage:** the transport only reads what your `getToken` returns. If
> you use `localStorage`, any script injected into the page can read a
> long-lived token that survives reload. Prefer an in-memory token refreshed on
> load, or an httpOnly cookie held by a server-side proxy. The engine never
> persists the token itself.

```ts
import { SyncEngine, type StorageAdapter, type Tx } from "@pb-offline-sync/core";
import { PocketBaseTransport } from "@pb-offline-sync/transport-pocketbase";
import { MyIndexedDbAdapter } from "./my-indexeddb-adapter"; // you implement this

const transport = new PocketBaseTransport({
  baseUrl: "https://your-domain",
  token: { getToken: () => localStorage.getItem("pb_token") },
});

const storage: StorageAdapter = new MyIndexedDbAdapter();
const engine = new SyncEngine({ storage, transport });

await engine.applyLocal("create", "notes", "notesmap0q1w2e3", { title: "hi" });
await engine.syncNow();
```

An adapter must implement exactly this surface (all writes happen inside one
transaction; a throw rolls everything back):

```ts
interface StorageAdapter {
  transaction<T>(fn: (tx: Tx) => Promise<T>): Promise<T>;
}

interface Tx {
  get(collection: string, recordId: string): Promise<StoredRecord | null>;
  put(collection: string, recordId: string, record: StoredRecord): Promise<void>;
  delete(collection: string, recordId: string): Promise<void>;
  list(collection: string): Promise<Array<{ recordId: string; record: StoredRecord }>>;
  collections(): Promise<string[]>;

  outboxList(): Promise<OutboxEntry[]>;
  outboxPut(entry: OutboxEntry): Promise<void>;
  outboxDelete(mutationId: string): Promise<void>;

  syncStateGet(): Promise<SyncState>;
  syncStatePut(state: SyncState): Promise<void>;

  conflictAdd(conflict: ConflictRecord): Promise<void>;
  conflictList(): Promise<ConflictRecord[]>;
  conflictDelete(id: string): Promise<void>;
}
```

`StoredRecord` is `{ rev: number; fields: Record<string, unknown> }` (`rev: 0`
means "created locally, not yet acknowledged"). The required local tables and the
per-field-type mapping are specified in `docs/pb-offline-sync-PDD.md` §9.3–§9.4;
`@pb-offline-sync/adapter-expo-sqlite` is the reference implementation to copy
(its `mapping.ts` shows the type rules, and its `driver.ts` shows the small
driver interface you'd implement for your store).

---

## Testing

```bash
# server (Go)
cd server && go build ./... && go vet ./... && go test ./...

# each client package (Node 22 + pnpm)
cd client/core                 && pnpm install && pnpm run typecheck && pnpm test
cd client/transport-pocketbase && pnpm install && pnpm run typecheck && pnpm test
cd client/adapter-expo-sqlite  && pnpm install && pnpm run typecheck && pnpm test
```

Both sides validate the same `conformance/` fixtures; a green run on both means
they agree with `docs/protocol.md`. The server also has a randomized
multi-device convergence harness: `go test ./syncplugin/ -run Convergence -count=5`.
See `docs/test-scenarios.md` for the full scenario catalog and known gaps.

## Releases

Tag-driven, fully automated — see `docs/release-plan.md`:

```bash
# move the [Unreleased] section in CHANGELOG.md to:  ## [0.1.0] - 2026-10-22
git commit -am "Release v0.1.0" && git push
git tag -a v0.1.0 -m v0.1.0 && git push origin v0.1.0
```

The release workflow refuses to run without a matching changelog section, re-runs
the Go suite, then publishes the GHCR image (`ghcr.io/karlomac/pb-offline-sync`),
cross-compiled binaries with checksums, a GitHub Release, and the `server/vX.Y.Z`
tag that `go get` requires. Stable tags also move `latest`; prereleases
(`v0.2.0-rc.1`) do not.

## Compatibility

| | Supported |
|---|---|
| PocketBase | `v0.40.6` (pinned; range not yet widened) |
| Go | 1.27.x |
| Node | 22.x |
| Protocol | 1 |

## Non-goals (v0.1)

Shared/multi-user records, partial/filtered sync, file **content** sync,
operation mutations (counters), CRDTs, server-side merge hooks,
CouchDB-protocol compatibility, running PocketBase on-device. Full list and
rationale in `docs/pb-offline-sync-PDD.md` §3.

## Roadmap

M0 contract + fixtures + scaffold · M1–M3 server · M4–M5 client · M6 convergence
harness (all landed) → **M7 example app + v0.1 release**. See
`docs/pb-offline-sync-PDD.md` §13.

## Contributing

Read `AGENTS.md` first — it lists the non-negotiable rules (ownership is never
trusted from clients, no device clocks for ordering, change-log writes are
atomic with the data write, `docs/protocol.md` and `conformance/` are read-only
contracts). A task is not complete until the touched packages' tests pass.

## License

MIT — see `LICENSE`.
