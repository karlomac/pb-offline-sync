import { describe, expect, it } from "vitest";
import { SyncEngine } from "../src/engine";
import { MemoryStorageAdapter, MockTransport } from "../src/memory";

const A = "aaaaaaaaaaaaaaa";
const B = "bbbbbbbbbbbbbbb";

// Regression tests for the uncommitted-review fixes (client side).

describe("snapshot cleanup only runs for a fresh snapshot", () => {
  it("a resumed snapshot does not delete previously delivered records", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    // A was delivered by an earlier snapshot page, then the app was killed: the
    // persisted cursor is a snapshot cursor, and A is not re-delivered.
    await storage.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await storage.transaction((tx) =>
      tx.syncStatePut({ cursor: "snapshot-cursor", lastSyncAt: null, lastError: null }),
    );

    transport.pullHandler = () => ({
      mode: "snapshot",
      changes: [{ collection: "notes", recordId: B, op: "upsert", rev: 1, record: { title: "b" } }],
      cursor: "inc-cursor",
      hasMore: false,
      serverTime: "",
    });

    await engine.pullNow();

    const ids = (await storage.transaction((tx) => tx.list("notes"))).map((r) => r.recordId).sort();
    expect(ids).toEqual([A, B].sort()); // A must survive the resumed snapshot
  });
});

describe("a sent create blocks later same-record edits", () => {
  it("never pushes an update with baseRev 0 after an unacknowledged create", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await engine.applyLocal("create", "notes", A, { title: "a" });

    // First sync fails at the transport, so the create stays `sent`.
    transport.pushHandler = () => {
      throw new Error("offline");
    };
    await engine.syncNow();

    // The user edits the still-unacknowledged record.
    await engine.applyLocal("update", "notes", A, { title: "v2" });

    // Network returns: the create must go first, then the rebased edit.
    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) =>
        m.op === "create"
          ? { id: m.id, status: "ok" as const, rev: 1, record: { title: "a" } }
          : { id: m.id, status: "ok" as const, rev: 2, record: { title: "v2" } },
      ),
    });
    transport.pullHandler = () => ({
      mode: "incremental",
      changes: [],
      cursor: "c1",
      hasMore: false,
      serverTime: "",
    });

    await engine.syncNow();

    for (const batch of transport.pushed) {
      for (const m of batch) {
        expect(m.baseRev === null || m.baseRev === undefined || m.baseRev >= 1).toBe(true);
      }
    }
    expect(await storage.transaction((tx) => tx.outboxList())).toEqual([]);
    expect(await storage.transaction((tx) => tx.get("notes", A))).toEqual({
      rev: 2,
      fields: { title: "v2" },
    });
  });
});
