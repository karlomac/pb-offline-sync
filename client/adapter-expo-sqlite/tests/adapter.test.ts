import { describe, expect, it } from "vitest";
import { MockTransport, SyncEngine } from "@pb-offline-sync/core";
import type { StorageAdapter, SyncState, Tx } from "@pb-offline-sync/core";
import { MemorySQLiteDriver } from "../src/memory-driver";
import { SqliteStorageAdapter } from "../src/adapter";
import type { CollectionSchema } from "../src/adapter";

const A = "aaaaaaaaaaaaaaa";
const B = "bbbbbbbbbbbbbbb";

const schema: CollectionSchema = {
  notes: [
    { name: "title", type: "text" },
    { name: "body", type: "editor" },
    { name: "done", type: "bool" },
    { name: "count", type: "number" },
    { name: "tags", type: "select", multiple: true },
    { name: "meta", type: "json" },
  ],
};

function adapter(): SqliteStorageAdapter {
  return new SqliteStorageAdapter({ driver: new MemorySQLiteDriver(), schema });
}

describe("SqliteStorageAdapter CRUD", () => {
  it("round-trips a record with every column type and undeclared keys", async () => {
    const s = adapter();
    await s.transaction((tx) =>
      tx.put("notes", A, {
        rev: 1,
        fields: {
          id: A,
          title: "hi",
          body: "<p>x</p>",
          done: true,
          count: 3.5,
          tags: ["x", "y"],
          meta: { a: 1 },
          created: "2026-10-15 08:30:00.000Z",
        },
      }),
    );

    const rec = await s.transaction((tx) => tx.get("notes", A));
    expect(rec).toEqual({
      rev: 1,
      fields: {
        id: A,
        created: "2026-10-15 08:30:00.000Z",
        title: "hi",
        body: "<p>x</p>",
        done: true,
        count: 3.5,
        tags: ["x", "y"],
        meta: { a: 1 },
      },
    });
  });

  it("delete removes the record; list and collections reflect stored rows", async () => {
    const s = adapter();
    await s.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await s.transaction((tx) => tx.put("notes", B, { rev: 1, fields: { title: "b" } }));

    expect(await s.transaction((tx) => tx.collections())).toEqual(["notes"]);
    const list = await s.transaction((tx) => tx.list("notes"));
    expect(list.map((r) => r.recordId).sort()).toEqual([A, B]);

    await s.transaction((tx) => tx.delete("notes", A));
    expect(await s.transaction((tx) => tx.get("notes", A))).toBeNull();
    expect((await s.transaction((tx) => tx.list("notes"))).map((r) => r.recordId)).toEqual([B]);
  });

  it("rejects a collection that was not configured for sync", async () => {
    const s = adapter();
    await expect(s.transaction((tx) => tx.get("nope", A))).rejects.toThrow("collection not configured");
  });
});

describe("put passthrough rules (§4.1)", () => {
  it("a key absent from fields is left unchanged", async () => {
    const s = adapter();
    await s.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await s.transaction((tx) => tx.put("notes", A, { rev: 2, fields: { body: "b" } }));

    const rec = await s.transaction((tx) => tx.get("notes", A));
    expect(rec?.fields).toMatchObject({ title: "a", body: "b" });
  });

  it("a key present with null sets the field empty", async () => {
    const s = adapter();
    await s.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await s.transaction((tx) => tx.put("notes", A, { rev: 2, fields: { title: null } }));

    const rec = await s.transaction((tx) => tx.get("notes", A));
    expect(rec?.fields).toHaveProperty("title", null);
  });
});

describe("outbox / conflicts / sync state", () => {
  it("preserves outbox insertion order and coalesces pending updates", async () => {
    const s = adapter();
    const engine = new SyncEngine({ storage: s, transport: new MockTransport(), newId: idFactory() });

    await engine.applyLocal("update", "notes", A, { title: "one" });
    await engine.applyLocal("update", "notes", A, { body: "two" });
    await engine.applyLocal("update", "notes", B, { title: "other" });

    const outbox = await s.transaction((tx) => tx.outboxList());
    expect(outbox.map((e) => e.recordId)).toEqual([A, B]);
    expect(outbox[0]).toMatchObject({ op: "update", fields: { title: "one", body: "two" } });
  });

  it("stores conflicts and sync state", async () => {
    const s = adapter();
    expect(await s.transaction((tx) => tx.syncStateGet())).toEqual({ cursor: null, lastSyncAt: null, lastError: null });

    await s.transaction((tx) =>
      tx.syncStatePut({ cursor: "c1", lastSyncAt: 123, lastError: "e" }),
    );
    expect(await s.transaction((tx) => tx.syncStateGet())).toEqual({ cursor: "c1", lastSyncAt: 123, lastError: "e" });

    await s.transaction((tx) =>
      tx.conflictAdd({ id: "c1", collection: "notes", recordId: A, code: "field_conflict", fields: { title: "x" }, message: "m" }),
    );
    expect(await s.transaction((tx) => tx.conflictList())).toEqual([
      { id: "c1", collection: "notes", recordId: A, code: "field_conflict", fields: { title: "x" }, message: "m" },
    ]);
    await s.transaction((tx) => tx.conflictDelete("c1"));
    expect(await s.transaction((tx) => tx.conflictList())).toEqual([]);
  });
});

describe("transaction atomicity", () => {
  it("rolls back every write when the transaction throws", async () => {
    const s = adapter();
    await expect(
      s.transaction(async (tx) => {
        await tx.put("notes", A, { rev: 1, fields: { title: "a" } });
        await tx.syncStatePut({ cursor: "c1", lastSyncAt: null, lastError: null });
        throw new Error("boom");
      }),
    ).rejects.toThrow("boom");

    expect(await s.transaction((tx) => tx.get("notes", A))).toBeNull();
    expect((await s.transaction((tx) => tx.syncStateGet())).cursor).toBeNull();
  });
});

describe("engine integration (§11)", () => {
  it("push ok stores the returned record and clears the outbox", async () => {
    const s = adapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage: s, transport });

    await engine.applyLocal("create", "notes", A, { title: "a" });
    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) => ({ id: m.id, status: "ok" as const, rev: 1, record: { id: A, title: "a" } })),
    });

    await engine.syncNow();

    expect(await s.transaction((tx) => tx.outboxList())).toEqual([]);
    const rec = await s.transaction((tx) => tx.get("notes", A));
    expect(rec?.rev).toBe(1);
    expect(rec?.fields).toMatchObject({ id: A, title: "a" });
  });

  it("rebases a pending edit onto a pulled change without reverting it", async () => {
    const s = adapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage: s, transport });

    await s.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "base" } }));
    await engine.applyLocal("update", "notes", A, { body: "mine" });

    transport.pullHandler = () => ({
      mode: "incremental",
      changes: [{ collection: "notes", recordId: A, op: "upsert", rev: 2, record: { id: A, title: "server" } }],
      cursor: "c1",
      hasMore: false,
      serverTime: "",
    });
    await engine.pullNow();

    const rec = await s.transaction((tx) => tx.get("notes", A));
    expect(rec?.rev).toBe(2);
    expect(rec?.fields).toMatchObject({ title: "server", body: "mine" });
    const [entry] = await s.transaction((tx) => tx.outboxList());
    expect(entry.baseRev).toBe(2);
  });

  it("applies a page and cursor atomically (rollback on cursor failure)", async () => {
    const inner = adapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage: new FailingCursorStorage(inner), transport });

    await inner.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "local" } }));
    transport.pullHandler = () => ({
      mode: "incremental",
      changes: [
        { collection: "notes", recordId: A, op: "upsert", rev: 2, record: { title: "server" } },
        { collection: "notes", recordId: B, op: "upsert", rev: 1, record: { title: "new" } },
      ],
      cursor: "c2",
      hasMore: false,
      serverTime: "",
    });

    await expect(engine.pullNow()).rejects.toThrow("boom");

    expect((await inner.transaction((tx) => tx.syncStateGet())).cursor).toBeNull();
    expect((await inner.transaction((tx) => tx.get("notes", A)))?.fields).toMatchObject({ title: "local" });
    expect(await inner.transaction((tx) => tx.get("notes", B))).toBeNull();
  });
});

/** Storage wrapper that fails while persisting the cursor, after applying changes. */
class FailingCursorStorage implements StorageAdapter {
  constructor(private inner: SqliteStorageAdapter) {}

  transaction<T>(fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.inner.transaction((tx) =>
      fn({
        ...tx,
        syncStatePut: async (_state: SyncState) => {
          throw new Error("boom: could not persist cursor");
        },
      }),
    );
  }
}

function idFactory(): () => string {
  let n = 0;
  return () => `m${++n}`;
}
