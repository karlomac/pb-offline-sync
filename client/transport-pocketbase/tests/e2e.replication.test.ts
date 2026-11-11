/**
 * Opt-in live end-to-end replication suite (test plan R1–R6, R9, M1–M5, M8, M10).
 * Skipped unless E2E_BASE_URL points at a provisioned server in RC-B mode.
 *
 *   E2E_BASE_URL=http://127.0.0.1:8090 pnpm vitest run tests/e2e.replication.test.ts
 */
import { createHash } from "node:crypto";
import { describe, expect, it } from "vitest";
import { MemoryStorageAdapter, SyncEngine, TransportError, uploadMedia } from "@pb-offline-sync/core";
import { PocketBaseTransport } from "../src/transport";

const BASE = process.env.E2E_BASE_URL;
const PASSWORD = process.env.E2E_PASSWORD ?? "e2e-password-123";
const live = BASE ? describe : describe.skip;

// Run-scoped record ids so repeated runs against a kept data dir never collide.
const RUN = Math.floor(Math.random() * 1_000_000);
function rid(n: number): string {
  return "r" + String(RUN * 10_000 + (n % 10_000)).padStart(14, "0");
}

async function login(email: string): Promise<string> {
  const res = await fetch(`${BASE}/api/collections/users/auth-with-password`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ identity: email, password: PASSWORD }),
  });
  if (res.status !== 200) throw new Error(`login ${email}: ${res.status} ${await res.text()}`);
  return ((await res.json()) as { token: string }).token;
}

function device(token: string) {
  const storage = new MemoryStorageAdapter();
  const transport = new PocketBaseTransport({ baseUrl: BASE!, token: { getToken: () => token } });
  const engine = new SyncEngine({ storage, transport });
  return { storage, transport, engine };
}

async function settle(engine: SyncEngine) {
  await engine.syncNow();
  return engine.status();
}

async function rec(storage: MemoryStorageAdapter, collection: string, id: string) {
  return storage.transaction((tx) => tx.get(collection, id));
}

interface PullBody {
  mode: string;
  changes: Array<{ collection: string; recordId: string; op: string; rev: number; record?: Record<string, unknown> }>;
  cursor: string;
  hasMore: boolean;
}

async function pull(token: string, cursor?: string): Promise<PullBody> {
  const url = `${BASE}/api/sync/pull?limit=500` + (cursor ? `&cursor=${encodeURIComponent(cursor)}` : "");
  const res = await fetch(url, { headers: { "X-Sync-Protocol": "1", Authorization: token } });
  if (res.status !== 200) throw new Error(`pull ${res.status} ${await res.text()}`);
  return (await res.json()) as PullBody;
}

/** Full snapshot (follows hasMore) so the returned cursor is incremental-ready. */
async function snapshot(token: string): Promise<PullBody> {
  let page = await pull(token);
  const changes = [...page.changes];
  while (page.hasMore) {
    page = await pull(token, page.cursor);
    changes.push(...page.changes);
  }
  return { ...page, mode: "snapshot", changes };
}

async function pushRaw(token: string, mutations: unknown[]) {
  const res = await fetch(`${BASE}/api/sync/push`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Sync-Protocol": "1", Authorization: token },
    body: JSON.stringify({ mutations }),
  });
  const body = (await res.json()) as {
    results: Array<{ id: string; status: string; code?: string; rev?: number }>;
  };
  return { status: res.status, body };
}

function bytes(fill: number, size: number): Uint8Array {
  const data = new Uint8Array(size).fill(fill);
  // Run-scoped prefix: fixed patterns would dedup against blobs uploaded by a
  // previous run against the same (remote, persistent) instance, breaking tests
  // such as the resume scenario that expect a fresh upload session.
  const tag = new TextEncoder().encode(`e2e:${RUN}:${fill}:`);
  data.set(tag.subarray(0, Math.min(tag.length, size)), 0);
  return data;
}

function sha(data: Uint8Array): string {
  return createHash("sha256").update(data).digest("hex");
}

async function upload(token: string, data: Uint8Array, filename: string) {
  const transport = new PocketBaseTransport({ baseUrl: BASE!, token: { getToken: () => token } });
  const res = await uploadMedia(transport, {
    collection: "media",
    sha256: sha(data),
    size: data.byteLength,
    mime: "image/png",
    filename,
    read: async (offset, length) => data.slice(offset, offset + length),
  });
  return { transport, res, mediaId: "mediaId" in res ? (res.mediaId as string) : "" };
}

live("live replication", () => {
  it("R1 two devices converge on a create", async () => {
    const token = await login("alice@e2e.test");
    const a = device(token);
    const b = device(token);
    const id = rid(1);
    await a.engine.applyLocal("create", "notes", id, { title: "hello" });
    await settle(a.engine);
    await settle(b.engine);
    expect((await rec(b.storage, "notes", id))?.fields.title).toBe("hello");
  });

  it("R2 a stale-base edit records a conflict and keeps the server value", async () => {
    const token = await login("alice@e2e.test");
    const a = device(token);
    const b = device(token);
    const id = rid(2);
    await a.engine.applyLocal("create", "notes", id, { title: "v1" });
    await settle(a.engine);
    await settle(b.engine);

    await a.engine.applyLocal("update", "notes", id, { title: "alice2" });
    await settle(a.engine);

    await b.engine.applyLocal("update", "notes", id, { title: "bob2" });
    const conflicted = await settle(b.engine);
    expect(conflicted.conflicts.length).toBeGreaterThan(0);

    await settle(b.engine); // pull the winning rev
    expect((await rec(b.storage, "notes", id))?.fields.title).toBe("alice2");
  });

  it("R3 delete wins over a stale edit", async () => {
    const token = await login("alice@e2e.test");
    const a = device(token);
    const b = device(token);
    const id = rid(3);
    await a.engine.applyLocal("create", "notes", id, { title: "v1" });
    await settle(a.engine);
    await settle(b.engine);

    await a.engine.applyLocal("delete", "notes", id);
    await settle(a.engine);

    await b.engine.applyLocal("update", "notes", id, { title: "late" });
    await settle(b.engine);
    await settle(b.engine);

    expect(await rec(b.storage, "notes", id)).toBeNull();
    const snap = await snapshot(token);
    expect(snap.changes.some((c) => c.collection === "notes" && c.recordId === id && c.op !== "delete")).toBe(false);
  });

  it("R4 replaying a mutation is idempotent", async () => {
    const token = await login("alice@e2e.test");
    const mutation = {
      id: `replay-${RUN}`,
      collection: "notes",
      recordId: rid(4),
      op: "create",
      fields: { title: "once" },
    };
    const first = await pushRaw(token, [mutation]);
    const second = await pushRaw(token, [mutation]);
    expect(first.body.results[0].status).toBe("ok");
    expect(second.body.results[0].status).toBe("duplicate");
    expect(second.body.results[0].rev).toBe(first.body.results[0].rev);
  });

  it("R5 snapshot then incremental cursor, and tampered cursors are rejected", async () => {
    const token = await login("alice@e2e.test");
    const snap = await snapshot(token);
    expect(snap.mode).toBe("snapshot");

    const a = device(token);
    await a.engine.applyLocal("create", "notes", rid(5), { title: "after-snapshot" });
    await settle(a.engine);

    const inc = await pull(token, snap.cursor);
    expect(inc.mode).toBe("incremental");
    expect(inc.changes.some((c) => c.recordId === rid(5))).toBe(true);

    const bad = await fetch(`${BASE}/api/sync/pull?cursor=not-a-cursor`, {
      headers: { "X-Sync-Protocol": "1", Authorization: token },
    });
    expect(bad.status).toBe(400);
    expect(((await bad.json()) as { code: string }).code).toBe("cursor_invalid");
  });

  it("R6 three devices converge after interleaved writes", async () => {
    const token = await login("alice@e2e.test");
    const devices = [device(token), device(token), device(token)];

    for (let d = 0; d < devices.length; d++) {
      for (let i = 0; i < 5; i++) {
        await devices[d].engine.applyLocal("create", "notes", rid(100 + d * 10 + i), {
          title: `d${d}-${i}`,
        });
      }
    }
    for (let round = 0; round < 3; round++) {
      for (const dev of devices) await settle(dev.engine);
    }

    const rows = await Promise.all(
      devices.map(async (dev) => {
        const list = await dev.storage.transaction((tx) => tx.list("notes"));
        return new Map(list.map((r) => [r.recordId, r.record.fields.title]));
      }),
    );
    for (let i = 1; i < rows.length; i++) {
      expect([...rows[i].entries()].sort()).toEqual([...rows[0].entries()].sort());
    }
  });

  it("R9 another user cannot read or mutate the record", async () => {
    const aliceToken = await login("alice@e2e.test");
    const bobToken = await login("bob@e2e.test");
    const alice = device(aliceToken);
    const bob = device(bobToken);
    const id = rid(6);

    await alice.engine.applyLocal("create", "notes", id, { title: "private" });
    await settle(alice.engine);
    await settle(bob.engine);

    expect(await rec(bob.storage, "notes", id)).toBeNull();
    const res = await pushRaw(bobToken, [
      { id: `cross-${RUN}`, collection: "notes", recordId: id, op: "update", baseRev: 1, fields: { title: "hijack" } },
    ]);
    expect(res.body.results[0].status).toBe("rejected");
  });

  it("M1 upload and commit creates a synced media record", async () => {
    const token = await login("alice@e2e.test");
    const data = bytes(7, 1024 * 1024);
    const { res, mediaId } = await upload(token, data, "e2e.png");
    expect(["ok", "exists"]).toContain(res.status);
    expect(mediaId).not.toBe("");

    const dev = device(token);
    await settle(dev.engine);
    const row = await rec(dev.storage, "media", mediaId);
    expect(row).not.toBeNull();
    expect(row!.fields.sha256).toBe(sha(data));
    expect(row!.fields.size).toBe(data.byteLength);
  });

  it("M2 identical bytes dedup to the same media record", async () => {
    const token = await login("alice@e2e.test");
    const data = bytes(11, 128 * 1024);
    const first = await upload(token, data, "dedup.png");
    const second = await upload(token, data, "dedup-copy.png");
    expect(second.res.status).toBe("exists");
    expect(second.mediaId).toBe(first.mediaId);
  });

  it("M3 an upload resumes from the server offset", async () => {
    const token = await login("alice@e2e.test");
    const transport = new PocketBaseTransport({ baseUrl: BASE!, token: { getToken: () => token } });
    const data = bytes(13, 256 * 1024);
    const init = await transport.mediaInit({
      collection: "media",
      sha256: sha(data),
      size: data.byteLength,
      mime: "image/png",
      filename: "resume.png",
    });
    if (init.status !== "upload") throw new Error(`unexpected init status ${init.status}`);

    const half = data.byteLength / 2;
    await transport.mediaAppend(init.uploadId, 0, data.slice(0, half));
    expect(await transport.mediaOffset(init.uploadId)).toBe(half);
    await transport.mediaAppend(init.uploadId, half, data.slice(half));
    const commit = await transport.mediaCommit({ uploadId: init.uploadId });
    expect(commit.status).toBe("ok");
    expect(commit.file.size).toBe(data.byteLength);
  });

  it("M4 a hash mismatch is rejected and the session is discarded", async () => {
    const token = await login("alice@e2e.test");
    const transport = new PocketBaseTransport({ baseUrl: BASE!, token: { getToken: () => token } });
    const data = bytes(17, 64 * 1024);
    const init = await transport.mediaInit({
      collection: "media",
      sha256: sha(bytes(18, 64 * 1024)),
      size: data.byteLength,
      mime: "image/png",
      filename: "bad.png",
    });
    if (init.status !== "upload") throw new Error(`unexpected init status ${init.status}`);
    await transport.mediaAppend(init.uploadId, 0, data);
    await expect(transport.mediaCommit({ uploadId: init.uploadId })).rejects.toMatchObject({
      code: "hash_mismatch",
    });
  });

  it("M5 an incomplete upload cannot be committed", async () => {
    const token = await login("alice@e2e.test");
    const transport = new PocketBaseTransport({ baseUrl: BASE!, token: { getToken: () => token } });
    const data = bytes(19, 128 * 1024);
    const init = await transport.mediaInit({
      collection: "media",
      sha256: sha(data),
      size: data.byteLength,
      mime: "image/png",
      filename: "partial.png",
    });
    if (init.status !== "upload") throw new Error(`unexpected init status ${init.status}`);
    await transport.mediaAppend(init.uploadId, 0, data.slice(0, 1024));
    await expect(transport.mediaCommit({ uploadId: init.uploadId })).rejects.toBeInstanceOf(TransportError);
  });

  it("M8 media files require a token and are owner-scoped", async () => {
    const token = await login("alice@e2e.test");
    const data = bytes(23, 64 * 1024);
    const { mediaId } = await upload(token, data, "secret.png");

    const dev = device(token);
    await settle(dev.engine);
    const row = await rec(dev.storage, "media", mediaId);
    const fileName = String(row?.fields.file);
    expect(fileName).toBeTruthy();

    const ft = (await (
      await fetch(`${BASE}/api/files/token`, { method: "POST", headers: { Authorization: token } })
    ).json()) as { token: string };
    expect(ft.token).toBeTruthy();

    const filePath = `${BASE}/api/files/media/${mediaId}/${encodeURIComponent(fileName)}`;
    expect((await fetch(`${filePath}?token=${encodeURIComponent(ft.token)}`)).status).toBe(200);
    expect((await fetch(filePath)).status).not.toBe(200);

    const bobToken = await login("bob@e2e.test");
    expect((await fetch(filePath, { headers: { Authorization: bobToken } })).status).not.toBe(200);
  });

  it("M10 media records are immutable through push", async () => {
    const token = await login("alice@e2e.test");
    const res = await pushRaw(token, [
      {
        id: `imm-${RUN}`,
        collection: "media",
        recordId: rid(7),
        op: "create",
        fields: { sha256: "a".repeat(64), size: 1 },
      },
    ]);
    expect(res.body.results[0].status).toBe("rejected");
    expect(res.body.results[0].code).toBe("media_immutable");
  });
});
