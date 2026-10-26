/**
 * Media upload (protocol §18). Metadata flows through init/commit; the bytes
 * use a tus-1.0-core resumable channel. Media stays out of the outbox: this
 * module has its own resume state, persisted through `MediaStateStore` so a
 * killed uploader continues at the server's offset.
 */
import { TransportError } from "./types";


/** §18.2 init request. sha256 is lowercase hex over the full file. */
export interface MediaInitRequest {
  collection: string;
  sha256: string;
  size: number;
  mime: string;
  filename: string;
}

/** §18.2 init response: a dedup hit or a resumable session. */
export type MediaInitResponse =
  | { status: "exists"; mediaId: string; rev?: number }
  | { status: "upload"; uploadId: string; chunkSize: number; expiresIn: number };

/** §18.4 commit request; `posterUploadId` optionally names a staged JPEG session. */
export interface MediaCommitRequest {
  uploadId: string;
  posterUploadId?: string;
}

/** §18.4 commit response. */
export interface MediaCommitResponse {
  status: "ok";
  mediaId: string;
  rev: number;
  file: { name: string; size: number };
}

/** The media side of the transport seam (§18). Binary-aware counterpart of `Transport`. */
export interface MediaTransport {
  mediaInit(req: MediaInitRequest): Promise<MediaInitResponse>;
  /** §18.3 HEAD: the server's current offset — the resume truth. */
  mediaOffset(uploadId: string): Promise<number>;
  /** §18.3 PATCH: appends `chunk` at `offset`; throws on a mismatched offset. */
  mediaAppend(uploadId: string, offset: number, chunk: Uint8Array): Promise<void>;
  mediaAbort(uploadId: string): Promise<void>;
  mediaCommit(req: MediaCommitRequest): Promise<MediaCommitResponse>;
}

/** Where the bytes come from (file handle, blob, expo asset, …). */
export interface MediaSource {
  collection: string;
  sha256: string;
  size: number;
  mime: string;
  filename: string;
  /** Returns the bytes in [offset, offset + length). */
  read(offset: number, length: number): Promise<Uint8Array>;
}

/** Resume state (§18.2: persist uploadId + offset so a killed app continues). */
export interface MediaUploadState {
  uploadId: string | null;
  offset: number;
}

export interface MediaStateStore {
  get(): Promise<MediaUploadState | null>;
  put(state: MediaUploadState): Promise<void>;
}

export type MediaUploadResult = MediaInitResponse | MediaCommitResponse;

/** Fallback chunk size for resumed runs (§18.6 advertises the server's). */
export const DEFAULT_CHUNK_SIZE = 8 << 20;

/** True when a resume 404/410 means the session is gone, not transient. */
function isUploadGone(err: unknown): boolean {
  return (
    err instanceof TransportError &&
    (err.status === 410 || (err.status === 404 && err.code === "upload_not_found"))
  );
}

/**
 * §18.2–18.4 upload flow: hashing is the caller's (cached by the host);
 * init → [exists? done] → resume-aware chunk loop → commit (idempotent).
 * On any throw the persisted state lets a later call resume; the chunk loop
 * should be run from a resumable context by the host (background-task window).
 * A stored session the server no longer has (expired, or its bytes discarded
 * after `hash_mismatch`) transparently re-inits once. Poster upload (§18.4)
 * is host-driven: stage a second session and call `mediaCommit` directly.
 */
export async function uploadMedia(
  transport: MediaTransport,
  source: MediaSource,
  store?: MediaStateStore,
  opts?: { chunkSize?: number },
): Promise<MediaUploadResult> {
  for (let attempt = 0; ; attempt++) {
    let state: MediaUploadState = (await store?.get()) ?? { uploadId: null, offset: 0 };
    let chunkSize = opts?.chunkSize ?? 0; // 0 = take it from init

    if (state.uploadId == null) {
      const init = await transport.mediaInit({
        collection: source.collection,
        sha256: source.sha256,
        size: source.size,
        mime: source.mime,
        filename: source.filename,
      });
      if (init.status === "exists") {
        await store?.put({ uploadId: null, offset: 0 });
        return init;
      }
      state = { uploadId: init.uploadId, offset: 0 };
      chunkSize = opts?.chunkSize ?? init.chunkSize;
    } else {
      chunkSize = opts?.chunkSize ?? DEFAULT_CHUNK_SIZE;
      try {
        // The server's offset is the resume truth (§18.3); a stale local
        // offset is dropped.
        const serverOffset = await transport.mediaOffset(state.uploadId!);
        if (serverOffset !== state.offset) {
          state = { uploadId: state.uploadId, offset: serverOffset };
        }
      } catch (err) {
        if (attempt === 0 && isUploadGone(err)) {
          await store?.put({ uploadId: null, offset: 0 });
          continue;
        }
        throw err;
      }
    }
    await store?.put(state);

    while (state.offset < source.size) {
      const length = Math.min(chunkSize, source.size - state.offset);
      const chunk = await source.read(state.offset, length);
      if (chunk.byteLength !== length) {
        throw new Error(`media source truncated: read ${chunk.byteLength} of ${length}`);
      }
      await transport.mediaAppend(state.uploadId!, state.offset, chunk);
      state = { uploadId: state.uploadId, offset: state.offset + chunk.byteLength };
      await store?.put(state);
    }

    const res = await transport.mediaCommit({ uploadId: state.uploadId! });
    await store?.put({ uploadId: null, offset: 0 });
    return res;
  }
}
