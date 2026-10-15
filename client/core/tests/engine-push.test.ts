import { describe, expect, it } from "vitest";
import { SyncEngine } from "../src/engine";
import { MemoryStorageAdapter, MockTransport } from "../src/memory";

const A = "aaaaaaaaaaaaaaa";
const B = "bbbbbbbbbbbbbbb";

describe("push result handling (§11.2)", () => {
  it("ok removes the mutation and stores the returned record+rev", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await engine.applyLocal("create", "notes", A, { title: "a" });
    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) => ({
        id: m.id,
        status: "ok" as const,
        rev: 1,
        record: { id: A, title: "a" },
      })),
    });

    await engine.syncNow();

    expect(await storage.transaction((tx) => tx.outboxList())).toEqual([]);
    expect(await storage.transaction((tx) => tx.get("notes", A))).toEqual({ rev: 1, fields: { id: A, title: "a" } });
  });

  it("conflict replaces local state, drops the mutation and records it", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await storage.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "local" } }));
    await engine.applyLocal("update", "notes", A, { title: "mine" });

    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) => ({
        id: m.id,
        status: "conflict" as const,
        code: "field_conflict",
        rev: 2,
        record: { title: "server" },
        message: "overlap",
      })),
    });

    await engine.syncNow();

    expect(await storage.transaction((tx) => tx.get("notes", A))).toEqual({ rev: 2, fields: { title: "server" } });
    expect(await storage.transaction((tx) => tx.outboxList())).toEqual([]);
    const conflicts = await storage.transaction((tx) => tx.conflictList());
    expect(conflicts).toHaveLength(1);
    expect(conflicts[0]).toMatchObject({ code: "field_conflict", fields: { title: "mine" }, message: "overlap" });
  });

  it("rejected marks the mutation errored and holds later mutations for the record", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await storage.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await storage.transaction((tx) => tx.put("notes", B, { rev: 1, fields: { title: "b" } }));
    await engine.applyLocal("update", "notes", A, { title: "bad" });
    await engine.applyLocal("update", "notes", B, { title: "fine" });

    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) =>
        m.recordId === A
          ? { id: m.id, status: "rejected" as const, code: "validation_failed" }
          : { id: m.id, status: "ok" as const, rev: 2, record: { title: "fine" } },
      ),
    });

    await engine.syncNow();

    let outbox = await storage.transaction((tx) => tx.outboxList());
    expect(outbox).toHaveLength(1);
    expect(outbox[0]).toMatchObject({ recordId: A, status: "errored" });

    // A later mutation for the same (errored) record is appended, not coalesced.
    await engine.applyLocal("update", "notes", A, { title: "again" });
    outbox = await storage.transaction((tx) => tx.outboxList());
    expect(outbox).toHaveLength(2);
    expect(outbox[1]).toMatchObject({ recordId: A, status: "pending", fields: { title: "again" } });

    // The held mutation must not be pushed.
    const callsBefore = transport.pushed.length;
    await engine.syncNow();
    expect(transport.pushed.length).toBe(callsBefore);
  });
});
