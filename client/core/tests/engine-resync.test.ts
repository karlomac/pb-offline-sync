import { describe, expect, it } from "vitest";
import { SyncEngine } from "../src/engine";
import { MemoryStorageAdapter, MockTransport } from "../src/memory";

const A = "aaaaaaaaaaaaaaa";
const B = "bbbbbbbbbbbbbbb";
const C = "ccccccccccccccc";

describe("full resync with pending mutations (§11.5)", () => {
  it("preserves the outbox, keeps pending creates, drops unreferenced rows", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    // Known row still on the server (will be delivered by the snapshot).
    await storage.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    // Row dropped server-side; no pending mutation → must be removed.
    await storage.transaction((tx) => tx.put("notes", B, { rev: 1, fields: { title: "b" } }));
    // Row created locally, never pushed → pending create must survive.
    await engine.applyLocal("create", "notes", C, { title: "c" });
    // A stale cursor that triggers the resync.
    await storage.transaction((tx) => tx.syncStatePut({ cursor: "old", lastSyncAt: null, lastError: null }));

    transport.pullHandler = (cursor) => {
      if (cursor === "old") throw new Error("unreachable: resync clears the cursor first");
      return {
        mode: "snapshot",
        changes: [{ collection: "notes", recordId: A, op: "upsert", rev: 1, record: { title: "a" } }],
        cursor: "fresh",
        hasMore: false,
        serverTime: "",
      };
    };

    await engine.resync();

    const recs = await storage.transaction((tx) => tx.list("notes"));
    const ids = recs.map((r) => r.recordId).sort();
    expect(ids).toEqual([A, C].sort()); // B removed, C (pending create) kept

    const outbox = await storage.transaction((tx) => tx.outboxList());
    expect(outbox).toHaveLength(1);
    expect(outbox[0]).toMatchObject({ op: "create", recordId: C, status: "pending" });

    const state = await storage.transaction((tx) => tx.syncStateGet());
    expect(state.cursor).toBe("fresh");
  });
});
