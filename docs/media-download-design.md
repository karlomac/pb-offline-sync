# Media download & on-device storage — design

**Status: proposal. Not implemented.** The repository is under a code freeze while
the current `staging` state is end-to-end tested, promoted to production and
validated by the production app. Implementation starts only after those steps
(see §13).

Related: `docs/protocol.md` §18 (media wire contract), `docs/media-sync-plan.md`
MED-4 ("other device pulls + downloads + verifies"), `docs/threat-model.md`,
`docs/e2e-replication-test-plan.md`.

---

## 1. Problem

Today media **records** replicate; the **bytes** do not.

| Exists today | Missing |
|---|---|
| Upload channel: `mediaInit/mediaOffset/mediaAppend/mediaAbort/mediaCommit` | Any download API in the transport |
| Media record sync (`sha256`, `size`, `mime`, `filename`, `file`, `poster`) via push/pull | A local byte store; `StorageAdapter` has records/outbox/conflicts/state only |
| Server-side dedup by `(owner, sha256)`, quotas, GC, staging sweep | Client-side cache, dedup by sha, eviction, pins |
| Protected file fields + owner-scoped rules + `POST /api/files/token` | File-token handling, ranged download, hash verification, resume |

Scenario to satisfy: an iPhone uploads a photo (bytes land in S3 via PocketBase),
a second device on the same account pulls the record, downloads the bytes, and
can **display/play them with no network**.

Without a downloader the second device has metadata only — it can build a URL,
but offline it has nothing to render.

---

## 2. Goals and non-goals

### Goals

1. Download media bytes from PocketBase and store them on the device.
2. Serve them offline from local storage (image/video), including after an app
   restart and while the network is down.
3. Deduplicate by content: one blob per `sha256`, referenced by many records.
4. Resumable, verified, retryable downloads with bounded disk use.
5. No protocol/wire change for v1; no change to what a media record contains.

### Non-goals (explicit)

- **No PouchDB/CouchDB wire protocol, MVCC, rev trees or attachment stubs.** We
  borrow only the general principles noted in §12.
- No server-side filtering or download endpoints; PocketBase's file serving is
  the download path.
- No inline/base64 media in JSON, and no change to the media record schema.
- No transcoding, streaming (HLS) or video editing in v1.
- No direct-to-S3 presigned downloads in v1 (possible later; see §8).
- No E2EE (the server can always read the bytes, as today).

---

## 3. Requirements

| ID | Requirement |
|---|---|
| R1 | A media record pulled from the server can be resolved to a local file after download. |
| R2 | Downloads resume after interruption (app kill, network loss, OS suspend) without restarting from byte 0. |
| R3 | Downloaded bytes are verified against the record's `sha256` before being exposed to the app. |
| R4 | Identical content is stored once per account, even if referenced by multiple records. |
| R5 | Storage is bounded by a configurable byte cap with LRU eviction; pinned items are exempt. |
| R6 | A policy decides what to download: on view, thumbnails + on view, or all under a cap. |
| R7 | Downloads require the owner's auth token + a short-lived file token; nothing is public. |
| R8 | Deleting/evicting a record removes its local file when no other record references the blob. |
| R9 | The app can tell whether a record is available offline (and show progress/failed state). |
| R10 | The design works with PocketBase's S3 backend without exposing bucket credentials. |

---

## 4. Architecture

```
pull (records)                     download lane                     local disk
┌──────────────┐   media record   ┌──────────────────┐  GET /api/files  ┌──────────────┐
│ SyncEngine   │ ───────────────► │ MediaDownloader  │ ───────────────► │ MediaStore   │
│ (existing)   │  {sha256,size,   │ queue, resume,   │  Range + token   │ sha256-keyed │
│              │   filename}      │ verify, retry    │                  │ files + index│
└──────────────┘                  └──────────────────┘                  └──────────────┘
                                            │                                   │
                                            └── POST /api/files/token ──────────┘
                                                (owner auth, short TTL)
```

Three new pieces, all client-side:

1. **Transport addition** — obtain a file token and perform a ranged download.
2. **Core addition** — `MediaStore` seam + `MediaDownloader` queue/policy.
3. **Adapter addition** — file storage + cache index for the platform
   (`adapter-expo-sqlite` first; web later).

Nothing in `server/` changes for v1.

---

## 5. Transport design

PocketBase already provides everything needed:

- `POST /api/files/token` (owner auth) → `{ "token": "…" }`, short-lived.
- `GET /api/files/{collection}/{recordId}/{filename}?token=…` streams the file.
  With S3 configured, PocketBase **proxies** the object from the bucket
  (`filesystem.System.Serve`); no bucket URL or credential reaches the client.
- Range requests are supported (`http.ServeContent`): `Range: bytes=<from>-`,
  responses `206` + `Content-Range`, or `416` when out of bounds.
- `?thumb=WxH` returns a generated thumbnail (stored back in the bucket on first
  request) — useful for cheap list prefetch.

Add to the transport (names indicative):

```ts
interface MediaTransport {
  // …existing upload methods…

  /** Short-lived file token for the current user. */
  mediaFileToken(): Promise<{ token: string; expiresIn?: number }>;

  /**
   * Download bytes [from, to) — or the whole file when omitted.
   * Resolves with a stream/reader; the caller owns hashing and persistence.
   */
  mediaDownload(
    collection: string,
    recordId: string,
    filename: string,
    opts?: { from?: number; to?: number; thumb?: string },
  ): Promise<ReadableStream<Uint8Array>>;
}
```

Implementation notes:

- One `POST /api/files/token` per batch of downloads (refresh on `401`, not per
  file).
- `filename` is the record's `file` field value (already synced); never derive it
  from user input.
- `416` → reset the resume offset to the current file size and retry once.
- `403`/`404` → the record is gone or no longer owned: drop the queue item and
  the cache entry.
- No retries on `4xx` except `401` (token refresh) and `416` (offset reset).

---

## 6. Core design

### 6.1 `MediaStore` seam

Content-addressed by `sha256`; the record is only a reference.

```ts
export interface LocalMedia {
  sha256: string;
  uri: string;        // file:// URI for the platform
  size: number;
  mime: string;
}

export interface MediaStore {
  has(sha256: string): Promise<boolean>;
  open(sha256: string): Promise<LocalMedia | null>;
  /** Temp path for a partially downloaded blob. */
  stagingPath(sha256: string): string;
  /** Atomically move a verified temp file into the store. */
  commit(sha256: string, tempPath: string, size: number, mime: string): Promise<LocalMedia>;
  remove(sha256: string): Promise<void>;
  pin(sha256: string, pinned: boolean): Promise<void>;
  usage(): Promise<{ bytes: number; count: number }>;
  /** Evict least-recently-used unpinned blobs until usage <= maxBytes. */
  evictToLimit(maxBytes: number): Promise<number>;
}
```

### 6.2 `MediaDownloader`

A persisted, single-flight-per-blob queue. It does **not** live in the outbox:
bytes have their own retry/resume semantics.

```ts
export type DownloadPolicy =
  | { mode: "on-view" }
  | { mode: "prefetch"; scope: "thumbs" | "images" | "all"; maxBytes?: number; wifiOnly?: boolean }
  | { mode: "all"; maxBytes: number; wifiOnly: true };

export interface MediaDownloader {
  /** Called for every pulled media record (idempotent). */
  enqueue(item: { collection: string; recordId: string; sha256: string; size: number; mime: string }): Promise<void>;
  pump(): Promise<void>;          // process until idle/offline/paused
  pause(): void;
  resume(): void;
  status(): Promise<{ queued: number; active: number; failed: number; bytes: number }>;
}
```

Algorithm per blob:

1. If `store.has(sha256)` → mark the record as available, done (no network).
2. Ensure a valid file token; `GET` with `Range: bytes=<offset>-`.
3. Stream to `stagingPath(sha256)`, hashing incrementally; persist `offset`
   every ~1 MiB (crash-safe).
4. On completion: compare `sha256` and `size` with the record.
   - match → `commit()` (atomic rename), write the cache row, mark available;
   - mismatch → delete temp, retry once from 0; second failure → mark `failed`
     and surface the error (do not loop).
5. `416` → reset offset; `401` → refresh token; `403/404` → drop;
   network error/offline → exponential backoff (1 s → 5 min) with a persisted
   `next_at`.
6. Evict to the configured cap after commits (never evict pinned or actively
   referenced blobs).

Concurrency: 2 downloads by default (3 on Wi-Fi); per-`sha256` single-flight so
two records sharing a blob do not download twice.

### 6.3 Local pointer (never synced)

Do **not** add a field to the synced record. The adapter exposes the mapping:

```ts
interface MediaCacheIndex {
  localFor(collection: string, recordId: string): Promise<LocalMedia | null>;
  recordsFor(sha256: string): Promise<Array<{ collection: string; recordId: string }>>;
}
```

The UI asks `localFor()`; a `null` result means "not downloaded" (show a
placeholder / download button when online).

---

## 7. Adapter design (`adapter-expo-sqlite`)

Files: `FileSystem.documentDirectory + "media/<accountScope>/<sha256>"`, temp
files under `media/.tmp/`. App-private storage only; never the shared media
library.

New tables (SQLite, alongside the existing sync tables):

```sql
CREATE TABLE media_cache (
  sha256      TEXT PRIMARY KEY,
  size        INTEGER NOT NULL,
  mime        TEXT NOT NULL,
  path        TEXT NOT NULL,
  downloaded_at INTEGER NOT NULL,
  last_access   INTEGER NOT NULL,
  pinned        INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE media_ref (
  collection TEXT NOT NULL,
  record_id  TEXT NOT NULL,
  sha256     TEXT NOT NULL,
  PRIMARY KEY (collection, record_id)
);
CREATE INDEX media_ref_sha ON media_ref (sha256);

CREATE TABLE media_queue (
  sha256     TEXT PRIMARY KEY,
  collection TEXT NOT NULL,
  record_id  TEXT NOT NULL,
  size       INTEGER NOT NULL,
  offset     INTEGER NOT NULL DEFAULT 0,
  attempts   INTEGER NOT NULL DEFAULT 0,
  next_at    INTEGER NOT NULL DEFAULT 0,
  state      TEXT NOT NULL           -- queued|active|failed|paused
);

CREATE TABLE media_checkpoint (
  server_url TEXT NOT NULL,
  user_id    TEXT NOT NULL,
  collection TEXT NOT NULL,
  policy     TEXT NOT NULL,
  cursor     TEXT,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (server_url, user_id, collection)
);
```

Key behaviours:

- `accountScope` is derived from `(serverUrl, userId)` so two accounts on one
  device never share cached bytes or queue state (`media_checkpoint` uses the
  same key).
- Files live in `Documents` (not `Caches`): the OS may purge caches. Our own LRU
  does the eviction.
- On logout: clear that account's refs/cache (policy decision; default: remove
  unpinned, keep pinned until confirmed).
- Web adapter (later): Cache Storage/IndexedDB, same index semantics.

---

## 8. Download policy and storage sizing

| Mode | Behaviour | Default for |
|---|---|---|
| `on-view` | Nothing prefetched; download when the UI asks for the item | phones with large libraries |
| `prefetch(thumbs)` | Thumbnails only, Wi-Fi | list/grid screens |
| `prefetch(images)` | All images under the cap, video on view | photo-centric apps |
| `all` | Everything under `maxBytes`, Wi-Fi only | small libraries / tablets |

Sizing examples: 500 photos × 5 MB ≈ 2.5 GB; 100 videos × 200 MB ≈ 20 GB. "All"
therefore requires an explicit user opt-in and a cap.

Recommended default: **thumbs prefetched + full resolution on view**, plus a
"keep offline" pin per item.

---

## 9. Platform and S3 notes

- **iOS**: background `URLSession` for large prefetch (foreground-only download
  is fine for on-view); files are app-private; enable
  `NSFileProtectionCompleteUntilFirstUserAuthentication` or stricter.
- **Android**: `WorkManager` for prefetch with network constraints; scoped
  storage is not needed (app-private files).
- **Playback**: video from a local `file://` URI; poster from the record's
  `poster` field (downloaded like any image) or `?thumb=`.
- **HEIC/RAW**: the original is stored and served; the UI uses the poster/thumb
  for lists (PocketBase does not transcode).
- **PocketBase + S3**: downloads traverse PocketBase (`Serve` → S3), so S3 egress
  applies per download; R2 (zero egress) is the recommended backend. Thumbnails
  are generated on first request and written back to the bucket.
- **Auth**: `POST /api/files/token` needs a valid owner token; file fields are
  `Protected` and the collection rules are owner-scoped, so a leaked URL is not
  enough.
- **Presigned GET** (future): would remove PocketBase from the byte path but
  requires a custom hook minting signed URLs; not needed to satisfy R1–R10.

---

## 10. Observability and UX contract

Emit (host-supplied logger/events): `media.queued`, `media.downloaded`,
`media.verified`, `media.failed`, `media.evicted`, with `sha256`, bytes,
duration and reason. Expose `status()` for a settings screen (queued bytes,
cached bytes, cap). The UI must distinguish:

- available offline (`localFor` non-null),
- downloading (progress from `media_queue.offset`),
- not downloaded (needs network),
- failed (retryable).

---

## 11. Testing plan (additions to `docs/e2e-replication-test-plan.md`)

| ID | Scenario | Expected |
|---|---|---|
| D1 | Two devices, one upload | Device B downloads, verifies and renders offline after sync |
| D2 | Interrupted download | Kill the app at ~50%; resume continues at the offset, final hash matches |
| D3 | Duplicate content | Two records with the same `sha256` share one file; deleting one keeps the other |
| D4 | Hash mismatch | Corrupted bytes are discarded, retried once, then `failed` |
| D5 | Record deleted server-side | Queue item and cache entry are removed on the next pull |
| D6 | Token expiry | A mid-download `401` refreshes the token and continues |
| D7 | Storage cap | Eviction removes LRU unpinned blobs; pinned survive |
| D8 | Account switch | No cross-account bytes or checkpoints are visible |
| D9 | S3 backend | Same behaviour as local storage; Range works through PocketBase |
| D10 | Large video | Resumable download and local playback |

Unit tests (core + adapter): queue state machine, resume offsets, dedup/refcount,
eviction order, failed-item quarantine, account scoping.

---

## 12. Prior art (inspiration only)

PouchDB/CouchDB were reviewed for ideas, **not adopted as a protocol**:

- content-identity reuse (skip transfer when the digest already exists locally),
- batched "what do you lack?" checks (future batch `media/need` endpoint),
- checkpoint documents keyed by source + target + filters (our
  `media_checkpoint` key includes server + user + collection).

We deliberately do **not** copy rev trees, attachment stubs, multipart doc+
attachment transfer, or CouchDB's wire protocol.

---

## 13. Milestones (blocked by the freeze)

| Phase | Scope | Est. |
|---|---|---|
| P1 | Transport `mediaFileToken` + ranged `mediaDownload` (tested against the live rig), core `MediaStore` + in-memory store + downloader state machine | ~1 week |
| P2 | `adapter-expo-sqlite` file store, cache/queue/checkpoint tables, eviction/pins, delete/refcount | ~2–3 weeks |
| P3 | Policy wiring (on-view / thumbs / all + cap, Wi-Fi), offline status UI hooks, background transfer | ~1 week |
| P4 (optional) | Batch `POST /api/sync/media/need` (fewer upload round-trips), presigned GET evaluation | ~1 week |

**Entry criteria for P1:** current `staging` E2E run is green and signed off; the
change is promoted to the production side and validated by the production app;
this design is approved.

---

## 14. Risks

| Risk | Mitigation |
|---|---|
| Disk exhaustion from "download all" | Mandatory cap + LRU + user opt-in; surface usage |
| S3 egress cost | R2; thumbs-first policy; cap; avoid re-downloads via sha cache |
| Background transfer cancelled by the OS | `URLSession` background / WorkManager; resume offsets; on-view path does not depend on background |
| Token expiry mid-batch | Refresh once on `401`; per-batch tokens |
| Shared device / multiple accounts | Account-scoped cache directory and checkpoints; clear on logout |
| Cache corruption | sha256 verification before commit; quarantine + single retry |
| Video sizes | Store original; thumbs/poster first; on-view default |

## 15. Open questions

1. Default policy for new installs: thumbs + on-view, or ask the user during
   onboarding?
2. Should pinned media be excluded from logout purges?
3. Do we need encryption at rest beyond OS file protection?
4. Is a batch `media/need` endpoint worth a v1.2 protocol change for first sync?
5. Do we ever transcode video, or always store the original?
