# pb-offline-sync

Two-way, offline-first sync between a local client database and a server-side
[PocketBase](https://pocketbase.io). Built as a Go plugin for PocketBase plus a
TypeScript client, joined by a written protocol and a language-neutral
conformance suite.

The app reads and writes **only local data** and stays fully usable offline; a
background engine reconciles it with PocketBase when connectivity returns.

> **Status: pre-release.** The name, Go module path (`github.com/pb-offline-sync/server`)
> and npm scope (`@pb-offline-sync/*`) are **provisional and unpublished** — see
> `docs/ecosystem-scan.md`. Nothing here is on npm or the Go module proxy yet, so
> install from source (below). Do not imply official PocketBase endorsement.

---

## Contents

- [What it does](#what-it-does)
- [How it works](#how-it-works)
- [Repository layout](#repository-layout)
- [Part 1 — Server: build the PocketBase plugin](#part-1--server-build-the-pocketbase-plugin)
- [Part 2 — PocketBase configuration](#part-2--pocketbase-configuration)
- [Part 3 — Deploying on Dokploy](#part-3--deploying-on-dokploy)
- [Part 4 — Client: use it in a SvelteKit or React project](#part-4--client-use-it-in-a-sveltekit-or-react-project)
- [Testing](#testing)
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
conformance/              language-neutral JSON fixtures both sides must pass
server/                   Go module: the PocketBase plugin (package syncplugin)
client/core/              TypeScript sync engine (no platform dependencies)
client/transport-pocketbase/  HTTP transport (auth + realtime nudge)
client/adapter-expo-sqlite/   first storage adapter + schema/type mapping
examples/expo-notes/      example app (roadmap)
```

---

## Part 1 — Server: build the PocketBase plugin

The server is a **library** you register into a PocketBase app, not a standalone
binary. You write a tiny `main.go`, add the plugin, and build your own PocketBase
binary.

### 1. Create your server app

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
	github.com/pb-offline-sync/server v0.0.0
	github.com/pocketbase/pocketbase v0.40.5
)

replace github.com/pb-offline-sync/server => ../pb-offline-sync/server
```

### 2. Wire the plugin in `main.go`

```go
package main

import (
	"log"
	"time"

	"github.com/pocketbase/pocketbase"
	_ "github.com/pocketbase/pocketbase/migrations" // core system migrations

	"github.com/pb-offline-sync/server/syncplugin"
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

### 3. Build and run

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

### 1. Add a `Dockerfile` to your server app

Multi-stage build of your PocketBase binary. Put this next to your `main.go`:

```dockerfile
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# if you depend on the local plugin via `replace`, copy it in and fix the path
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/pb .

FROM alpine:3.20
RUN adduser -D -h /pb pb
WORKDIR /pb
COPY --from=build /out/pb /pb/pb
RUN mkdir -p /pb/pb_data && chown -R pb:pb /pb
USER pb
VOLUME ["/pb/pb_data"]
EXPOSE 8090
ENTRYPOINT ["/pb/pb"]
CMD ["serve", "--http=0.0.0.0:8090"]
```

> If you use the `replace ... => ../pb-offline-sync/server` directive, the build
> context must include that directory (e.g. vendor it, or use a monorepo/multi-stage
> `COPY` that places the plugin at the replaced path). Publishing the module does
> not exist yet, so the `replace` is the supported path.

### 2. Create the Dokploy application

1. **Push** the server app to a Git repo Dokploy can reach.
2. In Dokploy, **Create → Application**, connect the repo, branch, and build path.
3. **Build type:** `Dockerfile` (Dokploy builds it and runs the container).
4. **Ports:** map container port **8090** (PocketBase's HTTP port).
5. **Volumes (important):** add a persistent volume, e.g. name `pb-data`, mounted
   at **`/pb/pb_data`**. Without this, your database and uploads are lost on
   redeploy.
6. **Domains:** add your domain and let Dokploy provision TLS. Point the client's
   `baseUrl` at `https://your-domain`.

### 3. First boot

On first start the container creates `pb_data/`, runs the plugin migrations
(`sync_meta`, `sync_changes`, `sync_processed`, `sync_conflicts`) and the core
schema. Create the first superuser once:

```bash
# from the Dokploy container terminal, or `docker exec -it <container> ...`
/pb/pb superuser upsert you@example.com 'a-strong-password'
```

Then open `https://your-domain/_/` to create the auth collection user(s) and the
synced collection(s) from Part 2. Health probe: `GET /api/health`.

### Docker Compose alternative

Dokploy also runs a Compose stack if you prefer:

```yaml
services:
  pb:
    build: .
    restart: unless-stopped
    ports:
      - "8090:8090"
    volumes:
      - pb-data:/pb/pb_data

volumes:
  pb-data:
```

---

## Part 4 — Client: use it in a SvelteKit or React project

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

Wiring is identical to the Expo example, minus `openExpoDriver`:

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

## Compatibility

| | Supported |
|---|---|
| PocketBase | `v0.40.5` (pinned; range not yet widened) |
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
