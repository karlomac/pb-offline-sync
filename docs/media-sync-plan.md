# Media sync plan (photos & video)

**Status: accepted — shipped as `docs/protocol.md` §18 (v1.1, additive; wire
version stays `1`).** Executed 2026-10-22: MED-1 (spec + 10 media conformance
fixtures), MED-2 (server, `server/syncplugin/media.go`), MED-4 (relation
workflow fixture), MED-5 (GC/sweep/`/info`), and the protocol-level client
media uploader (`client/core/src/media.ts`, `transport-pocketbase` methods).
Deferred: the Expo-native slice of MED-3 (hash cache keyed on asset mtime,
two-lane background policy, poster/thumb generation, media-library scan — they
belong in the future Expo adapter/an app host), and MED-6 (presigned direct
upload).

Executed simplifications vs. this proposal: the dedup-hit `init` response omits
`file.url` and `commit` omits `thumbs` (clients build PB file URLs themselves —
`/api/files/{collection}/{id}/{file}?thumb=…` with a file token).

The historical proposal text below is kept for rationale; where it disagrees
with `docs/protocol.md` §18, §18 wins.

## 1. Why media can't ride `push`/`pull`

The existing sync is optimised for exactly what it does: small JSON records,
field-level patches, one transaction per mutation, an append-only change log,
and a cursor. Media breaks every one of those assumptions:

| Assumption in `push`/`pull` | Reality for media |
|---|---|
| Payload is small JSON | Files are MB–GB; base64/JSON is wasteful |
| Mutation is atomic and fast | A transfer takes minutes; the network drops mid-flight |
| Retry re-sends the whole mutation | Re-sending 2 GB is unacceptable — needs **resume** |
| Server assigns `rev` on write | Bytes are immutable; identity is the **content hash** |
| Every change is a small log row | Storing bytes in the change log is absurd |
| Device clocks irrelevant | Still true — ordering stays `rev`/`seq` |

So: **keep metadata on the existing protocol, add a separate binary channel.**
The record that *owns* a photo is still synced by `push`/`pull` exactly as today;
only the bytes use new endpoints. That is the central decision of this plan.

## 2. Goals / non-goals

**Goals**
- Upload photos/video from mobile reliably over flaky networks (resume, retry,
  background-friendly), without a per-upload size cliff.
- **Deduplicate**: the same bytes across devices/records transfer **once**.
- Keep the existing guarantees: ownership from the token, no device clocks,
  idempotent retries, no lost acknowledged writes.
- Play to PocketBase's strengths (file storage incl. S3-compatible backends,
  on-demand thumbnails, authenticated file serving) rather than re-implementing
  them.
- Bounded server cost: quotas, GC, TTL on abandoned uploads.

**Non-goals (this plan)**
- End-to-end encryption — **decided: not required.** The server hashes, generates
  thumbnails and dedups on plaintext, and can serve files directly.
- Camera-roll auto-backup / album mirroring (Immich-style) — **decided: out of
  scope.** Media is attached to business records, not the whole photo library.
  (Deferred; it was MED-7.)
- Collaborative libraries, face/ML indexing, map/timeline features (Immich
  territory).
- Editing/transcoding pipelines beyond thumbnails/poster frames.
- P2P/off-LAN transfer between devices (Syncthing territory).

## 3. Architecture

Two layers, deliberately separate:

```
  business record (e.g. "inspection")        ← existing push/pull, unchanged
        │  relation field: media[]  (a normal, writable relation)
        ▼
  media record  { owner, sha256, size, mime, filename, file, poster, ... }
        │  file field                              ← PocketBase-managed storage
        ▼
  bytes  ── uploaded via the NEW media channel ──►  PB storage (local or S3)
```

- **`media` is a normal synced collection.** Its records flow through the
  existing change log, so all devices learn "media M exists, hash H, name N"
  with no protocol change. Bytes are fetched separately.
- **Media records are immutable.** The content *is* the hash; you never update
  bytes in place. Editing a photo = a new media record. This kills a whole class
  of conflicts and makes dedup trivial.
- **Business records reference media by relation** (single or multi). Today's
  `file_field_unsupported` rule stays: clients never push a file field; they push
  a relation value, which is an ordinary field.
- **Dedup key is the content hash**, and it is stored *per user* (see §10 for why
  global dedup leaks a content-existence oracle).

### Why content-addressed + reference

This is what makes dedup, resume, and GC tractable:

- Upload identity = `sha256` (+ `size`), so a retry or a re-send from another
  device is a no-op once the server has it.
- The same photo attached to three records uploads once.
- Deleting an attachment doesn't delete bytes until the last reference goes
  (reference counting → GC).

## 4. Wire protocol proposal (v1.1, `X-Sync-Protocol: 1` → `2` on breaking change)

New endpoints under `/api/sync/media/`, all authenticated, owner from the token.

### 4.1 `POST /api/sync/media/init`

```jsonc
// request
{ "collection": "media",      // target synced collection
  "sha256": "<64 hex>",       // client-computed over the full file
  "size": 84213769,           // bytes
  "mime": "image/heic",
  "filename": "IMG_4821.heic" }
```

```jsonc
// dedup hit — no bytes transferred
{ "status": "exists", "mediaId": "abc123…", "file": { "url": "/api/files/…" } }
// need bytes — returns a resumable session
{ "status": "upload", "uploadId": "u_9f…", "chunkSize": 8388608,
  "expiresIn": 86400 }
```

### 4.2 Upload the bytes (resumable, tus-compatible core)

Mount a minimal **tus 1.0 core** at `/api/sync/media/upload/{uploadId}`:

| Request | Meaning |
|---|---|
| `HEAD` | `Upload-Offset: <n>` — where to resume from |
| `PATCH` | append bytes at the current offset (`application/offset+octet-stream`) |
| `DELETE` | abort |

Clients resume by `HEAD`-ing the offset and `PATCH`-ing the remainder. Chunks are
**8 MiB** by default — comfortably under the ~100 MB request caps that reverse
proxies (Cloudflare) impose, and they keep the server's memory flat because we
stream to a staging file (`io.Copy`), never buffering the whole body.

### 4.3 `POST /api/sync/media/commit`

```jsonc
{ "uploadId": "u_9f…" }
```

Server: verifies assembled `size`, computes `sha256` over the staged file,
rejects on mismatch, then creates the `media` record with the file attached (or
returns the existing `mediaId` if a row for that hash already exists — commit is
**idempotent**).

```jsonc
{ "status": "ok", "mediaId": "abc123…", "rev": 1,
  "file": { "name": "IMG_4821.heic", "size": 84213769 },
  "thumbs": { "256x256": "/api/files/…?thumb=256x256" } }
```

### 4.4 New error codes

`upload_too_large`, `hash_mismatch`, `upload_expired`, `upload_not_found`,
`quota_exceeded`, `mime_not_allowed`, `unsupported_media`, `media_not_found`.

### 4.5 Download

Prefer PocketBase's own serving (`/api/files/{collection}/{record}/{file}?thumb=…`,
with a short-lived file token) so we inherit access rules, range requests and
thumbnails. A plugin route is only needed if we later add E2EE or custom expiry.

## 5. Upload flow (client)

```
scan → hash (cached) → init → [exists? done] → upload (resume) → commit → relate → sync
```

1. **Hash first, cache the result.** Hashing a 4 GB video repeatedly is the main
   mobile CPU cost, so cache by `(assetId, size, modificationTime)`. Immich and
   muleline both dedup by checksum *before* transferring bytes, and the round-trip
   saving is worth the local hash.
2. **`init`** — if `exists`, skip straight to step 5; no bytes cross the network.
3. **Upload** with resume. Persist `uploadId` + offset locally so a killed app
   continues where it stopped.
4. **`commit`** — idempotent; safe to retry blindly.
5. **Relate** the returned `mediaId` into the business record and let the normal
   `syncNow()` publish it.

Media stays **out of the outbox**: it has its own queue with its own retry/backoff,
because a 2 GB transfer and a 200-byte field patch have nothing in common
operationally.

## 6. Android / iOS / Expo realities

This is where most naive designs fail. The current client targets Expo.

- **`expo-file-system`** can upload with `sessionType: 'background'` — on iOS that
  is a URLSession background transfer that *continues when the app is suspended*;
  **Android ignores it**. That is a single-shot upload, not a chunk loop.
- **`expo-background-task`** (BGTaskScheduler / WorkManager) schedules *when* to
  run (Android min ~15 min, iOS decides from battery/usage), not a long transfer
  inside one call.
- Community native modules exist for long background uploads
  (`react-native-background-upload`, `expo-background-upload`, upload managers),
  and iOS 26 adds `BGContinuedProcessingTask` for user-visible long tasks.

**Pragmatic policy (two lanes):**

| Asset | Strategy |
|---|---|
| small (≤ ~32 MiB) | single-shot background upload (iOS URLSession background); commit is idempotent |
| large / video | chunked resumable in the foreground/background-task window; the offset survives a kill, so it converges over several sessions |

Plus: **Wi-Fi-only by default** (Immich's default), pause on low battery /
metered connection, and an explicit "uploading N of M, resume" UI. The rare
worst case is a huge video that takes a few app launches — acceptable, and the
resumable offset is what makes it acceptable.

**Photo-library specifics**
- Enumerate with `expo-media-library`; track already-uploaded assets locally
  (asset id + modification time), because there is no change-observer in the Expo
  library — re-enumerate and diff.
- **HEIC/RAW**: PocketBase's Go decoders don't handle HEIC, so thumbnails will
  fail. Either transcode client-side or **always attach a client-generated JPEG
  poster/thumb** (`poster` field) for HEIC, RAW and video. Video posters come
  from a frame (e.g. `expo-video-thumbnails`).
- **Live Photos** = two assets (HEIC + MOV) sharing a group id.
- **Edits**: treat as a new asset (new hash) — media is immutable.

## 7. Server operations

- **Staging** lives under `pb_data/media_staging/<uploadId>` with a JSON sidecar
  (owner, sha, size, offset, expiry). Abandoned sessions are swept after a TTL
  (24 h) — a "partials" GC, run by the same explicit-cron model as `Prune`.
- **Final bytes** live in PocketBase storage via the `media` file field, so we
  inherit local-disk **or S3-compatible** backends and PB's `>5 MB` multipart
  uploads to S3 for free. Set the field's `MaxSize` realistically and keep chunk
  requests small so reverse proxies are never the limiting factor.
- **S3-compatible storage (the chosen backend)**: configure PocketBase's Files
  storage (Admin UI → Settings → Files storage, or programmatically) against
  R2/B2/MinIO. Consequences to design around:
  - Bytes still flow **client → plugin → S3**. The plugin stays in the data path
    in v1; the server→S3 hop is in-region and fast, and it keeps serving,
    thumbnails, and access rules on PocketBase. **MED-6** (presigned direct
    upload) would remove the hop, but PocketBase has no "adopt an existing object"
    API, so it means taking over storage *and* serving *and* thumbnails — only
    worth it if the hop measurably hurts.
  - **Backups**: files in a bucket are *not* part of PocketBase's instance
    backups. Protect them with bucket versioning/replication — a 3-2-1 strategy
    applies to the bucket, not `pb_data`.
  - **Egress**: the same photo served to several devices is egress per device.
    R2's zero-egress pricing is a real reason it's the recommended S3 target here.
- **GC**: a media record with zero inbound relations and older than a grace
  window is deletable; run it like `Prune` (explicit, injectable, batched).
- **Quotas**: per-user bytes and max file size, advertised in `/info`; reject at
  `init` (before bytes move) so the client fails fast and cheaply.
- **Storage accounting**: PocketBase records do **not** carry size/content-type
  for file fields, so `size`/`mime`/`sha256` must live on our own media record —
  which is exactly why the media record exists.

## 8. Security & threat-model additions

Add to `docs/threat-model.md` when this ships:

- **Ownership**: upload sessions and media records are bound to the token's user;
  a session cannot be resumed or committed by another user.
- **Integrity**: `sha256` is recomputed server-side at commit — the client's hash
  is a *claim*, not proof. Clients verify the hash after download too.
- **Content-type safety**: allowlist MIME types; never serve user content as
  `text/html`/`image/svg+xml` inline (`nosniff`, `Content-Disposition:
  attachment` for non-images) — stored XSS via upload is the classic bug here.
- **Dedup oracle (important)**: a *global* dedup index answers "does the server
  hold content with hash H?" to any user, which can confirm known content. Scope
  dedup **per user** (or per user + explicitly shared scope).
- **Abuse / cost**: rate-limit `init`; enforce per-user quota; TTL abandoned
  staging so a client can't fill the disk with partials; optional AV scan.
- **Serving**: PocketBase file tokens are short-lived; do not hand out
  long-lived public URLs for private media.

## 9. Testing plan

Follows the existing style (`docs/test-scenarios.md`):

- **Conformance fixtures** (new category): `init` dedup hit vs miss; resume from a
  non-zero offset; commit idempotency; hash mismatch → `hash_mismatch`; expired
  and foreign upload sessions; quota exceeded; MIME rejection; cross-user
  isolation (user B cannot resume/commit user A's session, cannot dedup against
  A's hash).
- **Server unit/integration**: assemble-from-chunks correctness; offset
  bookkeeping under duplicate `PATCH`; GC refcount (delete one of two refs);
  staging TTL sweep.
- **Client**: hash cache invalidation; queue resume after a simulated kill;
  respecting Wi-Fi-only; poster generation for video/HEIC.
- **Failure injection**: kill mid-`PATCH`, then resume and converge; orphaned
  upload then re-`init`; commit retry after a timeout (must not duplicate).

## 10. Milestones

| # | Deliverable | Depends on |
|---|---|---|
| **MED-1** | Protocol v1.1 spec text (this doc → `docs/protocol.md` §19) + conformance fixtures | decision in §12 |
| **MED-2** | Server: `media` collection, staged chunk upload (tus core), `init`/`commit`, hash verify, dedup index, quotas | MED-1 |
| **MED-3** | Client: media queue, hash cache, resume, two-lane background policy, poster generation | MED-2 |
| **MED-4** | Relations workflow end-to-end (attach media to a business record; other device pulls + downloads + verifies) | MED-3 |
| **MED-5** | Ops: GC/refcount, staging TTL, `/info` quota advertisement | MED-2 |
| **MED-6** (optional) | S3 **presigned multipart** direct upload, removing the plugin from the byte path. Caveat: PocketBase cannot adopt an externally-written object, so this means taking over storage **and** serving **and** thumbnails. Only if the client→plugin→S3 hop measurably hurts. | MED-2 |

Camera-roll auto-backup (previously MED-7) is **out of scope** per §12.

## 11. Prior art (what to copy, and licences)

We are **MIT**; do not copy AGPL code.

| Project | Licence | What we take |
|---|---|---|
| **tus** (`tus/tusd`, tus-js-client) | MIT | The resumable protocol + a Go reference. Supabase Storage runs TUS for 50 GB files. If a dependency is acceptable, use `tusd`'s handler library instead of hand-rolling. |
| **muleline** | MIT | Very close shape to this plan: TUS-inspired chunked uploads (5 MB chunks), **SHA-256 dedup checked before bytes move**, thumbnails, pluggable storage (S3/R2/MinIO/WebDAV/SFTP). Good reference implementation to read. |
| **Immich** | AGPLv3 | **Architecture only, no code.** Checksum dedup before upload, Wi-Fi-default backup, background backup via BGTaskScheduler/WorkManager, album mirroring, iCloud handling. Reference for the deferred camera-roll work. |
| **copyparty** (`up2k`) | check repo | Resumable multi-threaded uploads with no size limit, dedup, ffmpeg thumbnails. |
| **PocketBase** | MIT | Own the storage: file fields + S3-compatible backends + `>5 MB` multipart to S3 + on-demand thumbnails + authenticated serving. The maintainer **declined** built-in chunked uploads and suggested exactly the "dedicated Media/Storage collection" pattern this plan uses. |

## 12. Decisions

| # | Decision | Chosen |
|---|---|---|
| 1 | **Scope** | **Media attached to business records.** Camera-roll auto-backup is out of scope (deferred). |
| 2 | **End-to-end encryption** | **Not required.** Server-side hashing, thumbnails and dedup stay. |
| 3 | **Storage** | **S3-compatible** (R2 / B2 / MinIO) via PocketBase's Files storage. R2 recommended (zero egress). |
| 4 | **Dedup scope** | Per user (recommended — avoids a cross-user content-existence oracle). |
| 5 | **Originals** | Keep as-is (HEIC/RAW/video) and **always attach a client-generated JPEG poster** so thumbnails exist regardless. |
| 6 | **Limits** | To set: max file size, per-user quota, whether video is in v1. |

Items 4–6 follow the recommendation unless you say otherwise; 1–3 are settled.

## 13. Recommendation

Ship **media attached to records** (MED-1…MED-5), with the **tus core** for
resumability, **content-hash dedup per user**, **client-generated posters** so
HEIC and video still have thumbnails, and **S3-compatible storage** (R2
recommended) as the byte backend via PocketBase's Files storage. That covers the
mobile photo/video case the apps need, reuses PocketBase's storage/serving/
thumbnails, and touches the frozen protocol only by *adding* a section — no
changes to `push`/`pull`.

Keep the plugin in the byte path for v1 (client → plugin → S3): it is the only
option that keeps serving, access rules and thumbnails in PocketBase. Revisit
MED-6 only if that hop measurably hurts. Camera-roll auto-backup is deferred.
