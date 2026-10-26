import { describe, expect, it } from "vitest";
import type {
  MediaCommitResponse,
  MediaInitResponse,
  MediaTransport,
  MediaUploadState,
} from "../src/media";
import { uploadMedia } from "../src/media";
import { TransportError } from "../src/types";

const SOURCE = {
  collection: "media",
  sha256: "a".repeat(64),
  size: 10,
  mime: "image/png",
  filename: "p.png",
};

/** Spies over a staged-bytes model so chunk boundaries are assertable. */
function mockTransport(initRes: MediaInitResponse, opts?: { stagedOffsets?: Record<string, number> }) {
  const staged = new Map<string, number>(Object.entries(opts?.stagedOffsets ?? {}).map(([k, v]) => [k, v]));
  const appends: Array<{ id: string; offset: number; bytes: number }> = [];
  let committed: { id: string; req: { uploadId: string; posterUploadId?: string } } | null = null;

  const t: MediaTransport = {
    async mediaInit(req) {
      if ("uploadId" in initRes) {
        // Init with a known-active session (resume) must not reset staged bytes.
        if (!staged.has(initRes.uploadId)) staged.set(initRes.uploadId, 0);
      }
      return initRes;
    },
    async mediaOffset(uploadId) {
      return staged.get(uploadId) ?? 0;
    },
    async mediaAppend(uploadId, offset, chunk) {
      appends.push({ id: uploadId, offset, bytes: chunk.byteLength });
      staged.set(uploadId, offset + chunk.byteLength);
    },
    async mediaAbort(uploadId) {
      staged.delete(uploadId);
    },
    async mediaCommit(req) {
      const body: MediaCommitResponse = {
        status: "ok",
        mediaId: "m1",
        rev: 1,
        file: { name: SOURCE.filename, size: SOURCE.size },
      };
      committed = { id: req.uploadId, req };
      return body;
    },
  };
  return {
    t,
    appends,
    commit: () => committed,
    stagedFor: (id: string) => staged.get(id),
  };
}

/** In-memory state store; the host would back this with local storage. */
function memStore(initial: MediaUploadState | null = null) {
  let state = initial;
  return {
    get: async () => state,
    put: async (s: MediaUploadState) => {
      state = s;
    },
    current: () => state,
  };
}

function sourceBytes(data: string) {
  const enc = new TextEncoder().encode(data);
  return { ...SOURCE, size: enc.length, read: async (o: number, l: number) => enc.slice(o, o + l) };
}

describe("uploadMedia", () => {
  it("uploads in init chunkSize-sized chunks then commits", async () => {
    const init: MediaInitResponse = { status: "upload", uploadId: "u_1", chunkSize: 4, expiresIn: 60 };
    const m = mockTransport(init);
    const src = sourceBytes("0123456789");

    const res = (await uploadMedia(m.t, src, memStore())) as MediaCommitResponse;

    expect(res.status).toBe("ok");
    expect(m.appends).toEqual([
      { id: "u_1", offset: 0, bytes: 4 },
      { id: "u_1", offset: 4, bytes: 4 },
      { id: "u_1", offset: 8, bytes: 2 },
    ]);
    expect(m.commit()?.req).toEqual({ uploadId: "u_1" });
    // The state store is cleared after commit.
    expect(m.stagedFor("u_1")).toBe(10);
  });

  it("short-circuits on a dedup hit without touching bytes", async () => {
    const m = mockTransport({ status: "exists", mediaId: "m9", rev: 1 });
    const res = await uploadMedia(m.t, sourceBytes("0123456789"), memStore());
    expect(res).toEqual({ status: "exists", mediaId: "m9", rev: 1 });
    expect(m.appends).toHaveLength(0);
    expect(m.commit()).toBeNull();
  });

  it("resumes from the server offset when local state is stale", async () => {
    const init: MediaInitResponse = { status: "upload", uploadId: "u_2", chunkSize: 4, expiresIn: 60 };
    // The server says 6 bytes are staged (§18.3 HEAD) — its offset is the truth.
    const m = mockTransport(init, { stagedOffsets: { u_2: 6 } });
    const src = sourceBytes("0123456789");

    await uploadMedia(m.t, src, memStore({ uploadId: "u_2", offset: 0 }), { chunkSize: 4 });

    expect(m.appends).toEqual([{ id: "u_2", offset: 6, bytes: 4 }]);
  });

  it("keeps resume state across a mid-transfer crash", async () => {
    const init: MediaInitResponse = { status: "upload", uploadId: "u_3", chunkSize: 4, expiresIn: 60 };
    const store = memStore();
    const src = sourceBytes("0123456789");

    // First run dies mid-transfer; the chunk at offset 4 was never acknowledged.
    const m1 = mockTransport(init);
    Object.assign(m1.t, {
      async mediaAppend(uploadId: string, offset: number, chunk: Uint8Array) {
        m1.appends.push({ id: uploadId, offset, bytes: chunk.byteLength });
        if (offset >= 4) throw new Error("killed");
      },
    });
    await expect(uploadMedia(m1.t, src, store, { chunkSize: 4 })).rejects.toThrow("killed");

    // The persisted state is the last acknowledged append boundary (§18.2).
    const saved = store.current()!;
    expect(saved.uploadId).toBe("u_3");
    expect(saved.offset).toBe(4);

    // Second run: the server offset (0 — the mock lost the bytes) is the truth;
    // everything from there is re-sent and finishes (§18.3).
    const m2 = mockTransport(init);
    const res = (await uploadMedia(m2.t, src, store, { chunkSize: 4 })) as MediaCommitResponse;
    expect(res.status).toBe("ok");
    expect(res.file.size).toBe(10);
    expect(m2.appends).toEqual([
      { id: "u_3", offset: 0, bytes: 4 },
      { id: "u_3", offset: 4, bytes: 4 },
      { id: "u_3", offset: 8, bytes: 2 },
    ]);
  });

  it("rejects a truncated source instead of committing short bytes", async () => {
    const m = mockTransport({ status: "upload", uploadId: "u_4", chunkSize: 4, expiresIn: 60 });
    const bad = {
      ...SOURCE,
      read: async () => new Uint8Array(2),
    };
    await expect(uploadMedia(m.t, bad, memStore())).rejects.toThrow("truncated");
  });

  it("explicit chunkSize overrides init's recommendation on resume", async () => {
    const init: MediaInitResponse = { status: "upload", uploadId: "u_5", chunkSize: 4, expiresIn: 60 };
    // The server reports 2 staged bytes for the resumed session (§18.3 HEAD).
    const m = mockTransport(init, { stagedOffsets: { u_5: 2 } });
    const store = memStore({ uploadId: "u_5", offset: 2 });
    const src = sourceBytes("0123456789");

    await uploadMedia(m.t, src, store, { chunkSize: 3 });

    // Local offset was moved to the server truth (2), then 3-byte chunks.
    expect(m.appends).toEqual([
      { id: "u_5", offset: 2, bytes: 3 },
      { id: "u_5", offset: 5, bytes: 3 },
      { id: "u_5", offset: 8, bytes: 2 },
    ]);
  });

  it("clears the state store after a successful commit", async () => {
    const m = mockTransport({ status: "upload", uploadId: "u_6", chunkSize: 4, expiresIn: 60 });
    const store = memStore();
    const src = sourceBytes("0123456789");

    await uploadMedia(m.t, src, store, { chunkSize: 4 });

    // §18.4: after commit the session is consumed; a retry must re-init (dedup
    // hit) rather than resume a dead uploadId.
    expect(store.current()).toEqual({ uploadId: null, offset: 0 });
  });

  it("re-inits once when a stored session is gone server-side", async () => {
    const store = memStore({ uploadId: "u_dead", offset: 5 });
    let inited = 0;
    let offsetCalls = 0;
    const freshInit: MediaInitResponse = { status: "upload", uploadId: "u_new", chunkSize: 4, expiresIn: 60 };
    const freshBytes: string[] = [];
    const t: MediaTransport = {
      async mediaInit(req) {
        inited += 1;
        return freshInit;
      },
      async mediaOffset(uploadId) {
        offsetCalls += 1;
        if (uploadId === "u_dead") {
          throw new TransportError("gone", { status: 410, code: "upload_expired" });
        }
        // fresh, nothing staged yet
        return 0;
      },
      async mediaAppend(_uploadId, _offset, chunk) {
        freshBytes.push(new TextDecoder().decode(chunk));
      },
      async mediaAbort() {},
      async mediaCommit(req) {
        expect(req.uploadId).toBe("u_new");
        return { status: "ok", mediaId: "m_new", rev: 1, file: { name: SOURCE.filename, size: SOURCE.size } };
      },
    };

    const res = (await uploadMedia(t, sourceBytes("0123456789"), store)) as MediaCommitResponse;

    expect(res.status).toBe("ok");
    expect(inited).toBe(1);
    // Only the dead session is HEADed; the fresh one starts at offset 0 by
    // construction (init creates it).
    expect(offsetCalls).toBe(1);
    // All bytes re-sent on the fresh session.
    expect(freshBytes.join("")).toBe("0123456789");
  });
});
