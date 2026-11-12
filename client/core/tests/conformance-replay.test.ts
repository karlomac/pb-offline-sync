import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { SyncEngine } from "../src/engine";
import { MemoryStorageAdapter } from "../src/memory";
import type { Transport } from "../src/interfaces";
import type { MediaCommitResponse, MediaInitResponse } from "../src/media";
import type { Mutation, PullResponse, PushResult } from "../src/types";

/**
 * C-CONF-2 / E2E-2: replay `conformance/fixtures/*.json` against a small
 * in-test mock server that implements the protocol semantics these fixtures
 * use, asserting each step's `expect` with the §15.1 subset matcher.
 *
 * The fixtures are read-only; see conformance/README.md for the format.
 */

// ── fixture shape ──────────────────────────────────────────────────────────

interface FieldCfg {
  name: string;
  type: string;
  writable: boolean;
}
interface CollectionCfg {
  ownerField: string;
  conflictPolicy: "serverWins" | "clientWins" | "reject";
  deletePolicy: "deleteWins" | "editWins";
  fields: FieldCfg[];
}
interface FixtureConfig {
  collections?: Record<string, CollectionCfg>;
  /** §18 media config; presence enables the media endpoints. */
  media?: { collection: string; maxFileSize?: number; perUserQuota?: number };
}
interface FixtureMutation {
  id: string;
  collection: string;
  recordId: string;
  op: "create" | "update" | "delete";
  baseRev?: number;
  fields?: Record<string, unknown>;
}
interface FixtureMediaStep {
  op: "init" | "upload" | "offset" | "commit" | "abort";
  request?: Record<string, unknown>;
  blob?: string;
  slice?: number[];
}
interface FixtureStep {
  as: string;
  push?: { mutations: FixtureMutation[] };
  pull?: { cursor?: string; limit?: number };
  media?: FixtureMediaStep;
  expect: unknown;
}
interface Fixture {
  name: string;
  description: string;
  config: FixtureConfig;
  users: string[];
  /** Named blobs; "$sha:<b>"/"$size:<b>" sentinels refer to these (never real hashes in the file). */
  blobs?: Record<string, string>;
  steps: FixtureStep[];
}

// ── subset matcher (§15.1) ─────────────────────────────────────────────────

function matchExpect(exp: unknown, act: unknown, path = "$"): string | null {
  if (exp === "$any") return null;
  if (exp === "$string") return typeof act === "string" ? null : `${path}: expected $string`;
  if (exp === "$int") {
    return typeof act === "number" && Number.isInteger(act) ? null : `${path}: expected $int`;
  }
  if (Array.isArray(exp)) {
    if (!Array.isArray(act)) return `${path}: expected array`;
    if (exp.length !== act.length) {
      return `${path}: expected ${exp.length} elements, got ${act.length}`;
    }
    for (let i = 0; i < exp.length; i++) {
      const err = matchExpect(exp[i], act[i], `${path}[${i}]`);
      if (err) return err;
    }
    return null;
  }
  if (exp && typeof exp === "object") {
    if (!act || typeof act !== "object") return `${path}: expected object`;
    const record = act as Record<string, unknown>;
    for (const [k, v] of Object.entries(exp as Record<string, unknown>)) {
      if (!(k in record)) return `${path}.${k}: missing`;
      const err = matchExpect(v, record[k], `${path}.${k}`);
      if (err) return err;
    }
    return null;
  }
  return exp === act ? null : `${path}: expected ${JSON.stringify(exp)}, got ${JSON.stringify(act)}`;
}

// ── mock server ────────────────────────────────────────────────────────────

const STALE = "$stale";
const SERVER_FIELDS = new Set(["id", "owner", "created", "updated", "collectionId", "expand"]);

class RequestFailure extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
  ) {
    super(code);
  }
}

interface LogEntry {
  seq: number;
  collection: string;
  recordId: string;
  owner: string;
  op: "create" | "update" | "delete";
  rev: number;
  changed: string[];
}

/** §18 media error codes → HTTP statuses (subset used by fixtures). */
const MEDIA_STATUS: Record<string, number> = {
  unsupported_media: 400,
  hash_mismatch: 400,
  quota_exceeded: 403,
  upload_not_found: 404,
  upload_expired: 410,
  upload_too_large: 413,
  mime_not_allowed: 415,
};

const MEDIA_MIME_DENY = new Set(["text/html", "image/svg+xml"]);
const MEDIA_MIME_DEFAULT = ["image/", "video/", "audio/"];

// ── media driver state (mirror of the Go runner's drvMediaState) ───────────

/** Tracks the active media session/offset/committed record for "$..." sentinels. */
class MediaState {
  uploadId: string | null = null;
  offset = 0;
  mediaId: string | null = null;

  substitute(value: unknown): unknown {
    if (value === "$mediaId") return this.mediaId;
    if (value === "$uploadId") return this.uploadId;
    return value;
  }
}

async function sha256Hex(data: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", data);
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** Replaces "$sha:<b>"/"$size:<b>"/"$uploadId"/"$mediaId" sentinels in a request object. */
async function substituteMediaRequest(
  req: Record<string, unknown> | undefined,
  blobs: Record<string, string> | undefined,
  ms: MediaState,
): Promise<Record<string, unknown>> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(req ?? {})) {
    if (typeof v === "string" && v.startsWith("$sha:")) {
      out[k] = await sha256Hex(new TextEncoder().encode(blobs?.[v.slice(5)] ?? ""));
    } else if (typeof v === "string" && v.startsWith("$size:")) {
      out[k] = new TextEncoder().encode(blobs?.[v.slice(6)] ?? "").byteLength;
    } else {
      out[k] = ms.substitute(v);
    }
  }
  return out;
}

function hashMutation(m: FixtureMutation): string {
  const fields = Object.fromEntries(
    Object.keys(m.fields ?? {})
      .sort()
      .map((k) => [k, m.fields?.[k]]),
  );
  return JSON.stringify([m.id, m.collection, m.recordId, m.op, m.baseRev ?? null, fields]);
}

/** A server-equivalent mock: ownership, revs, change log, idempotency, policies. */
class MockServer {
  private readonly store = new MemoryStorageAdapter();
  private readonly config: FixtureConfig;
  private readonly log: LogEntry[] = [];
  private readonly tombstones = new Set<string>();
  private readonly idOwner = new Map<string, string>();
  private readonly processed = new Map<string, { hash: string; result: PushResult }>();
  private head = 0;

  // §18 media channel state.
  private readonly mediaName: string | null;
  private readonly mediaCfg: NonNullable<FixtureConfig["media"]>;
  private readonly sessions = new Map<
    string,
    { owner: string; sha: string; size: number; mime: string; filename: string; offset: number; bytes: Uint8Array }
  >();
  private readonly done = new Map<string, { status: string; mediaId: string; rev: number }>();
  private mediaSeq = 0;

  constructor(config: FixtureConfig) {
    const collections: Record<string, CollectionCfg> = { ...(config.collections ?? {}) };
    this.mediaName = config.media?.collection ?? null;
    this.mediaCfg = config.media ?? { collection: "media" };
    if (this.mediaName && !collections[this.mediaName]) {
      // The media collection is a fixed-schema synced collection (§18.1).
      collections[this.mediaName] = {
        ownerField: "owner",
        conflictPolicy: "serverWins",
        deletePolicy: "deleteWins",
        fields: [],
      };
    }
    this.config = { ...config, collections };
  }

  private mimeAllowed(mime: string | undefined): boolean {
    const m = (mime ?? "").toLowerCase();
    if (m === "" || MEDIA_MIME_DENY.has(m)) return false;
    // Fixtures never configure a custom allowlist; the §18.6 default applies.
    return MEDIA_MIME_DEFAULT.some((p) => m.startsWith(p));
  }

  private mediaFail(code: string): never {
    throw new RequestFailure(MEDIA_STATUS[code] ?? 400, code);
  }

  private readonly consumed = new Set<string>();

  async mediaInit(user: string, req: Record<string, unknown>): Promise<MediaInitResponse> {
    if (!this.mediaName || req.collection !== this.mediaName) this.mediaFail("unsupported_media");
    const sha = String(req.sha256 ?? "");
    const size = Number(req.size ?? 0);
    if (!/^[0-9a-f]{64}$/.test(sha)) throw new RequestFailure(400, "invalid_request");
    const max = this.mediaCfg.maxFileSize ?? 0;
    if (!(size > 0) || (max > 0 && size > max)) this.mediaFail("upload_too_large");
    if (!this.mimeAllowed(String(req.mime ?? ""))) this.mediaFail("mime_not_allowed");
    const rows = await this.mediaRows(user);
    const existing = rows.find((r) => r.fields.sha256 === sha);
    if (existing) return { status: "exists", mediaId: existing.recordId, rev: existing.rev };
    // Quota after dedup: a hit adds no bytes (§18.2). Committed plus fully
    // staged active sessions (mirror of the Go server's accounting).
    const quota = this.mediaCfg.perUserQuota ?? 0;
    if (quota > 0) {
      let used = rows.reduce((n: number, row: { fields: Record<string, unknown> }) => n + Number(row.fields.size ?? 0), 0);
      for (const [sid, s] of this.sessions.entries()) {
        if (s.owner === user && !this.consumed.has(sid) && s.offset === s.size) used += s.size;
      }
      if (used + size > quota) this.mediaFail("quota_exceeded");
    }
    const uploadId = `u_${(this.mediaSeq++).toString(16).padStart(32, "0")}`;
    this.sessions.set(uploadId, {
      owner: user,
      sha,
      size,
      mime: String(req.mime ?? ""),
      filename: String(req.filename ?? ""),
      offset: 0,
      bytes: new Uint8Array(size),
    });
    return { status: "upload", uploadId, chunkSize: 8 << 20, expiresIn: 86400 };
  }

  mediaOffset(user: string, uploadId: string): number {
    return this.mediaSession(user, uploadId).offset;
  }

  /** §18.3 HEADUpload-Length: the declared size. */
  mediaUploadLength(user: string, uploadId: string): number {
    return this.mediaSession(user, uploadId).size;
  }

  mediaAppend(user: string, uploadId: string, offset: number, bytes: Uint8Array): void {
    const s = this.mediaSession(user, uploadId);
    if (offset !== s.offset) throw new RequestFailure(400, "invalid_request");
    if (s.offset + bytes.byteLength > s.size) this.mediaFail("upload_too_large");
    s.bytes.set(bytes, s.offset);
    s.offset += bytes.byteLength;
  }

  mediaAbort(user: string, uploadId: string): void {
    this.mediaSession(user, uploadId);
    this.sessions.delete(uploadId);
  }

  async mediaCommit(user: string, req: Record<string, unknown>): Promise<MediaInitResponse | MediaCommitResponse> {
    const uploadId = String(req.uploadId ?? "");
    const s = this.sessions.get(uploadId);
    if (!s || s.owner !== user) this.mediaFail("upload_not_found");
    // Consumed: return the stored original result (§18.4).
    if (this.consumed.has(uploadId)) {
      const d = this.done.get(uploadId);
      if (d) return { status: "ok", mediaId: d.mediaId, rev: d.rev, file: { name: s.filename, size: s.size } };
      this.mediaFail("upload_not_found");
    }
    if (s.offset !== s.size) throw new RequestFailure(400, "invalid_request");

    let poster: NonNullable<typeof s> | null = null;
    if (req.posterUploadId) {
      if (req.posterUploadId === uploadId) throw new RequestFailure(400, "invalid_request");
      const p = this.sessions.get(String(req.posterUploadId));
      if (!p || p.owner !== user) this.mediaFail("upload_not_found");
      // A consumed poster session's bytes are gone (Go removes data.bin).
      if (this.consumed.has(String(req.posterUploadId)) || p.offset !== p.size || p.mime !== "image/jpeg" || p.size > 8 << 20) {
        throw new RequestFailure(400, "invalid_request");
      }
      poster = p;
    }

    // The client hash is a claim; recompute (§18.4).
    const actual = await sha256Hex(s.bytes.slice(0, s.size));
    if (actual !== s.sha) {
      this.sessions.delete(uploadId);
      this.mediaFail("hash_mismatch");
    }
    const byHash = this.done.get(`${user}:${s.sha}`);
    const recordId = byHash?.mediaId ?? `m${(this.mediaSeq++).toString(16).padStart(14, "0")}`;
    if (!byHash) {
      const fields: Record<string, unknown> = {
        id: recordId,
        sha256: s.sha,
        size: s.size,
        mime: s.mime,
        filename: s.filename,
        file: s.filename,
      };
      if (poster) fields.poster = poster.filename;
      await this.commit(user, this.mediaName!, recordId, "create", 1, fields, Object.keys(fields));
    }
    this.done.set(`${user}:${s.sha}`, { status: "ok", mediaId: recordId, rev: 1 });
    this.done.set(uploadId, { status: "ok", mediaId: recordId, rev: 1 });
    // Consumed: bytes are gone server-side (offset stays `size`, like Go's meta).
    this.consumed.add(uploadId);
    if (poster) this.consumed.add(String(req.posterUploadId));
    s.bytes = new Uint8Array(0);
    return { status: "ok", mediaId: recordId, rev: 1, file: { name: s.filename, size: s.size } };
  }

  private mediaSession(user: string, uploadId: string) {
    // Consumed sessions are upload_not_found on the upload channel (§18.5) —
    // only mediaCommit may consume them (via the done map above).
    const s = this.sessions.get(uploadId);
    if (!s || s.owner !== user || this.consumed.has(uploadId)) this.mediaFail("upload_not_found");
    return s;
  }

  private async mediaRows(user: string): Promise<Array<{ recordId: string; rev: number; fields: Record<string, unknown> }>> {
    const rows = await this.store.transaction((tx) => tx.list(this.ns(user, this.mediaName!)));
    return rows.map((r) => ({ recordId: r.recordId, rev: r.record.rev, fields: r.record.fields }));
  }

  private key(collection: string, recordId: string): string {
    return `${collection}\u0000${recordId}`;
  }
  private ns(owner: string, collection: string): string {
    return `\u0000${owner}\u0000${collection}`;
  }

  async push(user: string, mutations: FixtureMutation[]): Promise<PushResult[]> {
    const results: PushResult[] = [];
    const failed = new Set<string>();
    for (const m of mutations) {
      const recordKey = this.key(m.collection, m.recordId);
      const processedKey = `${user}\u0000${m.id}`;
      const stored = this.processed.get(processedKey);
      if (stored) {
        // §9: same payload -> duplicate carrying the original result; different
        // payload -> mutation_id_reuse.
        results.push(
          stored.hash === hashMutation(m)
            ? {
                id: m.id,
                status: "duplicate",
                rev: stored.result.rev,
                record: stored.result.record,
                code: stored.result.code,
              }
            : { id: m.id, status: "rejected", code: "mutation_id_reuse" },
        );
        continue;
      }
      if (failed.has(recordKey)) {
        results.push({ id: m.id, status: "rejected", code: "dependency_failed" });
        continue;
      }
      const result = await this.apply(user, m);
      if (result.status !== "ok") failed.add(recordKey);
      if (result.status !== "duplicate") {
        this.processed.set(processedKey, { hash: hashMutation(m), result });
      }
      results.push(result);
    }
    return results;
  }

  private async apply(user: string, m: FixtureMutation): Promise<PushResult> {
    const cfg = this.config.collections![m.collection];
    if (!cfg) return { id: m.id, status: "rejected", code: "collection_not_synced" };
    // Media records are immutable and server-created; push never touches them (§18.1).
    if (this.mediaName && m.collection === this.mediaName) {
      return { id: m.id, status: "rejected", code: "media_immutable" };
    }
    if (m.op === "create") {
      if (m.baseRev != null) return { id: m.id, status: "rejected", code: "invalid_base_rev" };
    } else if (m.op === "update" || m.op === "delete") {
      if (m.baseRev == null || m.baseRev < 1) {
        return { id: m.id, status: "rejected", code: "invalid_base_rev" };
      }
    } else {
      return { id: m.id, status: "rejected", code: "invalid_op" };
    }

    if (m.op !== "delete") {
      for (const name of Object.keys(m.fields ?? {})) {
        if (SERVER_FIELDS.has(name)) return { id: m.id, status: "rejected", code: "forbidden_field" };
        const field = cfg.fields.find((f) => f.name === name);
        if (!field) return { id: m.id, status: "rejected", code: "unknown_field" };
        if (field.type === "file") {
          return { id: m.id, status: "rejected", code: "file_field_unsupported" };
        }
      }
    }

    if (m.op === "create") return this.create(user, m);
    if (m.op === "update") return this.update(user, m, cfg);
    return this.delete(user, m, cfg);
  }

  private async create(user: string, m: FixtureMutation): Promise<PushResult> {
    const live = await this.store.transaction((tx) =>
      tx.get(this.ns(user, m.collection), m.recordId),
    );
    if (live) {
      return {
        id: m.id,
        status: "conflict",
        code: "exists",
        rev: live.rev,
        record: { id: m.recordId, ...live.fields },
      };
    }
    const key = this.key(m.collection, m.recordId);
    if (this.idOwner.has(key) || this.tombstones.has(key)) {
      return { id: m.id, status: "rejected", code: "id_unavailable" };
    }
    const fields = { id: m.recordId, ...(m.fields ?? {}) };
    await this.commit(user, m.collection, m.recordId, "create", 1, fields, Object.keys(m.fields ?? {}));
    this.idOwner.set(key, user);
    return { id: m.id, status: "ok", rev: 1, merged: false, record: fields };
  }

  private async update(
    user: string,
    m: FixtureMutation,
    cfg: CollectionCfg,
  ): Promise<PushResult> {
    const live = await this.store.transaction((tx) =>
      tx.get(this.ns(user, m.collection), m.recordId),
    );
    if (!live) {
      return this.tombstones.has(this.key(m.collection, m.recordId))
        ? { id: m.id, status: "conflict", code: "deleted" }
        : { id: m.id, status: "rejected", code: "not_found" };
    }
    const R = live.rev;
    const B = m.baseRev as number;
    if (B > R) return { id: m.id, status: "rejected", code: "invalid_base_rev" };

    const F = Object.keys(m.fields ?? {});
    let merged = false;
    let overlap = false;
    if (B < R) {
      const changed = this.changedSince(user, m.collection, m.recordId, B, R);
      overlap = F.some((f) => changed.has(f));
      if (overlap && cfg.conflictPolicy !== "clientWins") {
        return {
          id: m.id,
          status: "conflict",
          code: "field_conflict",
          rev: R,
          record: { id: m.recordId, ...live.fields },
        };
      }
      if (!overlap) merged = true;
    }

    const fields = { ...live.fields, ...(m.fields ?? {}) };
    const rev = R + 1;
    const changedFields = F.filter((f) => live.fields[f] !== m.fields?.[f]);
    await this.commit(user, m.collection, m.recordId, "update", rev, fields, changedFields);
    const result: PushResult = { id: m.id, status: "ok", rev, merged, record: fields };
    if (overlap) result.conflictId = `conflict-${rev}`;
    return result;
  }

  private async delete(
    user: string,
    m: FixtureMutation,
    cfg: CollectionCfg,
  ): Promise<PushResult> {
    const live = await this.store.transaction((tx) =>
      tx.get(this.ns(user, m.collection), m.recordId),
    );
    if (!live) return { id: m.id, status: "ok" };
    const R = live.rev;
    const B = m.baseRev as number;
    if (B > R) return { id: m.id, status: "rejected", code: "invalid_base_rev" };
    if (B < R && cfg.deletePolicy === "editWins") {
      return {
        id: m.id,
        status: "conflict",
        code: "edited_since",
        rev: R,
        record: { id: m.recordId, ...live.fields },
      };
    }
    const rev = R + 1;
    await this.commitDelete(user, m.collection, m.recordId, rev);
    return { id: m.id, status: "ok", rev };
  }

  private async commit(
    owner: string,
    collection: string,
    recordId: string,
    op: "create" | "update",
    rev: number,
    fields: Record<string, unknown>,
    changed: string[],
  ): Promise<void> {
    await this.store.transaction((tx) =>
      tx.put(this.ns(owner, collection), recordId, { rev, fields }),
    );
    this.head += 1;
    this.log.push({ seq: this.head, collection, recordId, owner, op, rev, changed });
  }

  private async commitDelete(
    owner: string,
    collection: string,
    recordId: string,
    rev: number,
  ): Promise<void> {
    await this.store.transaction((tx) => tx.delete(this.ns(owner, collection), recordId));
    this.tombstones.add(this.key(collection, recordId));
    this.head += 1;
    this.log.push({ seq: this.head, collection, recordId, owner, op: "delete", rev, changed: [] });
  }

  private changedSince(
    owner: string,
    collection: string,
    recordId: string,
    baseRev: number,
    rev: number,
  ): Set<string> {
    const out = new Set<string>();
    for (const e of this.log) {
      if (
        e.owner === owner &&
        e.collection === collection &&
        e.recordId === recordId &&
        e.rev > baseRev &&
        e.rev <= rev
      ) {
        for (const f of e.changed) out.add(f);
      }
    }
    return out;
  }

  async pull(user: string, cursor: string | null, limit: number): Promise<PullResponse> {
    if (cursor === STALE) throw new RequestFailure(410, "cursor_too_old");
    if (cursor == null || cursor === "") return this.snapshot(user, 0, this.head, limit);
    if (cursor.startsWith("snap:")) {
      const [, index, head] = cursor.split(":");
      return this.snapshot(user, Number(index), Number(head), limit);
    }
    if (cursor.startsWith("inc:")) {
      const seq = Number(cursor.slice(4));
      if (seq > this.head) throw new RequestFailure(410, "cursor_too_old");
      return this.incremental(user, seq, limit);
    }
    throw new RequestFailure(400, "cursor_invalid");
  }

  private async snapshot(
    user: string,
    index: number,
    head: number,
    limit: number,
  ): Promise<PullResponse> {
    const records: Array<{
      collection: string;
      recordId: string;
      rev: number;
      record: Record<string, unknown>;
    }> = [];
    for (const collection of Object.keys(this.config.collections ?? {})) {
      const rows = await this.store.transaction((tx) => tx.list(this.ns(user, collection)));
      for (const row of rows) {
        records.push({
          collection,
          recordId: row.recordId,
          rev: row.record.rev,
          record: row.record.fields,
        });
      }
    }
    records.sort((a, b) =>
      a.collection !== b.collection
        ? a.collection < b.collection
          ? -1
          : 1
        : a.recordId < b.recordId
          ? -1
          : a.recordId > b.recordId
            ? 1
            : 0,
    );
    const page = records.slice(index, index + limit);
    const hasMore = index + limit < records.length;
    return {
      mode: "snapshot",
      changes: page.map((r) => ({
        collection: r.collection,
        recordId: r.recordId,
        op: "upsert" as const,
        rev: r.rev,
        record: r.record,
      })),
      cursor: hasMore ? `snap:${index + limit}:${head}` : `inc:${head}`,
      hasMore,
      serverTime: "",
    };
  }

  private async incremental(
    user: string,
    fromSeq: number,
    limit: number,
  ): Promise<PullResponse> {
    // §7.2: latest state per (collection, recordId), ascending seq.
    const latest = new Map<string, LogEntry>();
    for (const e of this.log) {
      if (e.owner !== user || e.seq <= fromSeq) continue;
      const key = this.key(e.collection, e.recordId);
      const prev = latest.get(key);
      if (!prev || e.seq > prev.seq) latest.set(key, e);
    }
    const entries = [...latest.values()].sort((a, b) => a.seq - b.seq);
    const page = entries.slice(0, limit);
    const changes: PullResponse["changes"] = [];
    for (const e of page) {
      if (e.op === "delete") {
        changes.push({ collection: e.collection, recordId: e.recordId, op: "delete", rev: e.rev });
      } else {
        const rec = await this.store.transaction((tx) =>
          tx.get(this.ns(user, e.collection), e.recordId),
        );
        if (rec) {
          changes.push({
            collection: e.collection,
            recordId: e.recordId,
            op: "upsert",
            rev: e.rev,
            record: rec.fields,
          });
        }
      }
    }
    const lastSeq = page.length > 0 ? page[page.length - 1].seq : fromSeq;
    return {
      mode: "incremental",
      changes,
      cursor: `inc:${lastSeq}`,
      hasMore: entries.length > page.length,
      serverTime: "",
    };
  }

  /** A Transport view of this server for one user, used to drive the engine. */
  transportFor(user: string): Transport {
    return {
      push: async (mutations: Mutation[]) => ({
        results: mutations.map((m) => ({ id: m.id, status: "ok" as const, rev: 1, record: {} })),
      }),
      pull: (cursor: string | null, limit: number) => this.pull(user, cursor, limit),
      subscribe: () => () => undefined,
    };
  }

  async liveRecordIds(user: string, collection: string): Promise<string[]> {
    return (await this.store.transaction((tx) => tx.list(this.ns(user, collection)))).map(
      (r) => r.recordId,
    );
  }
}

// ── driver ─────────────────────────────────────────────────────────────────

const here = dirname(fileURLToPath(import.meta.url));
const fixturesDir = resolve(here, "../../../conformance/fixtures");

function loadFixtures(): Fixture[] {
  return readdirSync(fixturesDir)
    .filter((name) => name.endsWith(".json"))
    .map((name) => JSON.parse(readFileSync(join(fixturesDir, name), "utf8")) as Fixture)
    .sort((a, b) => (a.name < b.name ? -1 : 1));
}

/** Executes one media step against the mock server and returns the matched body. */
async function runMediaStep(
  server: MockServer,
  user: string,
  step: FixtureMediaStep,
  fixture: Fixture,
  ms: MediaState,
): Promise<Record<string, unknown>> {
  try {
    switch (step.op) {
      case "init": {
        const req = await substituteMediaRequest(step.request as Record<string, unknown> | undefined, fixture.blobs, ms);
        const res = await server.mediaInit(user, req);
        if ("uploadId" in res) {
          ms.uploadId = res.uploadId;
          ms.offset = 0;
        }
        if ("mediaId" in res) ms.mediaId = res.mediaId;
        return res as unknown as Record<string, unknown>;
      }
      case "upload": {
        const enc = new TextEncoder().encode(fixture.blobs?.[step.blob ?? ""] ?? "");
        const from = step.slice?.[0] ?? 0;
        const to = step.slice?.[1] ?? enc.byteLength;
        server.mediaAppend(user, ms.uploadId!, ms.offset, enc.slice(from, to));
        ms.offset += to - from;
        return { status: 204 };
      }
      case "offset":
        return {
          status: 200,
          uploadOffset: server.mediaOffset(user, ms.uploadId!),
          uploadLength: server.mediaUploadLength(user, ms.uploadId!),
        };
      case "commit": {
        const req = await substituteMediaRequest(step.request as Record<string, unknown> | undefined, fixture.blobs, ms);
        const res = await server.mediaCommit(user, req);
        if ("mediaId" in res) ms.mediaId = res.mediaId;
        return res as unknown as Record<string, unknown>;
      }
      case "abort":
        server.mediaAbort(user, ms.uploadId!);
        ms.uploadId = null;
        ms.offset = 0;
        return { status: 204 };
    }
  } catch (err) {
    if (!(err instanceof RequestFailure)) throw err;
    return { status: err.status, code: err.code };
  }
}

const fixtures = loadFixtures();

describe("conformance replay: mock server satisfies every fixture (C-CONF-2)", () => {
  for (const fixture of fixtures) {
    it(fixture.name, async () => {
      const server = new MockServer(fixture.config);
      let prevCursor: string | null = null;
      const ms = new MediaState();

      for (const [stepIndex, step] of fixture.steps.entries()) {
        const context = `step ${stepIndex} (${fixture.name})`;
        if (step.media) {
          const error = matchExpect(step.expect, await runMediaStep(server, step.as, step.media, fixture, ms), context);
          expect(error).toBeNull();
          continue;
        }
        if (step.push) {
          // The "$mediaId" sentinel injects the committed media id into relations.
          const mutations = step.push.mutations.map((m) => ({
            ...m,
            fields: m.fields == null ? m.fields : Object.fromEntries(Object.entries(m.fields).map(([k, v]) => [k, ms.substitute(v)])),
          }));
          const results = await server.push(step.as, mutations);
          const error = matchExpect(step.expect, { results }, context);
          expect(error).toBeNull();
          continue;
        }
        const pull = step.pull ?? {};
        const cursor = pull.cursor === "$prev" ? prevCursor : (pull.cursor ?? null);
        try {
          const response = await server.pull(step.as, cursor, pull.limit ?? 100);
          prevCursor = response.cursor;
          const error = matchExpect(step.expect, response, context);
          expect(error).toBeNull();
        } catch (err) {
          if (!(err instanceof RequestFailure)) throw err;
          const error = matchExpect(step.expect, { status: err.status, code: err.code }, context);
          expect(error).toBeNull();
        }
      }
    });
  }
});

describe("conformance replay: the TS engine converges with the mock server", () => {
  it("drives a pull for every fixture's first user and matches server state", async () => {
    for (const fixture of fixtures) {
      const server = new MockServer(fixture.config);
      for (const step of fixture.steps) {
        if (step.push) await server.push(step.as, step.push.mutations);
      }
      const user = fixture.users[0];
      const storage = new MemoryStorageAdapter();
      const engine = new SyncEngine({ storage, transport: server.transportFor(user) });

      await engine.pullNow();

      const local = (await storage.transaction((tx) => tx.list("notes")))
        .map((r) => r.recordId)
        .sort();
      const remote = (await server.liveRecordIds(user, "notes")).sort();
      expect({ fixture: fixture.name, local }).toEqual({ fixture: fixture.name, local: remote });
    }
  });
});
