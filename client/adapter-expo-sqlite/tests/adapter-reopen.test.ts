/**
 * Adapter reopen semantics (docs/test-scenarios.md §13):
 *  - C-AD-6: constructing/opening the adapter twice over the same database is
 *    safe (`CREATE TABLE IF NOT EXISTS`) and loses no data.
 *  - C-AD-7: `_outbox` insertion order survives a reopen, with no reliance on
 *    any clock (§11.6) — the `_ord` sequence column carries it.
 */
import { describe, expect, it } from "vitest";
import { MockTransport, SyncEngine } from "@pb-offline-sync/core";
import type { OutboxEntry } from "@pb-offline-sync/core";
import { MemorySQLiteDriver } from "../src/memory-driver";
import { SqliteStorageAdapter } from "../src/adapter";
import type { CollectionSchema } from "../src/adapter";

const A = "aaaaaaaaaaaaaaa";
const B = "bbbbbbbbbbbbbbb";
const C = "ccccccccccccccc";

const schema: CollectionSchema = {
  notes: [
    { name: "title", type: "text" },
    { name: "body", type: "editor" },
  ],
};

/** Reopen: a fresh adapter instance over the same driver/database. */
function reopen(driver: MemorySQLiteDriver): SqliteStorageAdapter {
  return new SqliteStorageAdapter({ driver, schema });
}

function entry(mutationId: string, recordId: string, op: OutboxEntry["op"] = "update"): OutboxEntry {
  return { mutationId, collection: "notes", recordId, op, baseRev: op === "create" ? null : 1, fields: { title: mutationId }, status: "pending", attempts: 0 };
}

describe("schema idempotency across reopens (C-AD-6)", () => {
  it("opening a second adapter over the same database does not error and keeps data", async () => {
    const driver = new MemorySQLiteDriver();
    const first = reopen(driver);
    await first.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));

    const second = reopen(driver);
    const rec = await second.transaction((tx) => tx.get("notes", A));
    expect(rec?.fields).toMatchObject({ title: "a" });
    expect(rec?.rev).toBe(1);
  });

  it("migrate re-runs cleanly a third time and both instances can write and read", async () => {
    const driver = new MemorySQLiteDriver();
    const first = reopen(driver);
    const second = reopen(driver);
    const third = reopen(driver);

    await first.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await second.transaction((tx) => tx.put("notes", B, { rev: 1, fields: { title: "b" } }));
    await third.transaction((tx) => tx.put("notes", C, { rev: 1, fields: { title: "c" } }));

    const rows = await first.transaction((tx) => tx.list("notes"));
    expect(rows.map((r) => r.recordId).sort()).toEqual([A, B, C]);
    expect(await third.transaction((tx) => tx.collections())).toEqual(["notes"]);
  });

  it("a reopen does not reset persisted sync state", async () => {
    const driver = new MemorySQLiteDriver();
    const first = reopen(driver);
    await first.transaction((tx) => tx.syncStatePut({ cursor: "c1", lastSyncAt: 123, lastError: null }));

    const second = reopen(driver);
    expect(await second.transaction((tx) => tx.syncStateGet())).toEqual({ cursor: "c1", lastSyncAt: 123, lastError: null });
  });
});

describe("_outbox ordering survives a reopen (C-AD-7)", () => {
  it("returns the same insertion order after reopening the database", async () => {
    const driver = new MemorySQLiteDriver();
    const first = reopen(driver);
    await first.transaction(async (tx) => {
      await tx.outboxPut(entry("m1", A));
      await tx.outboxPut(entry("m2", B));
      await tx.outboxPut(entry("m3", C));
    });

    const second = reopen(driver);
    const outbox = await second.transaction((tx) => tx.outboxList());
    expect(outbox.map((e) => e.mutationId)).toEqual(["m1", "m2", "m3"]);
  });

  it("appends entries enqueued after the reopen at the end of the order", async () => {
    const driver = new MemorySQLiteDriver();
    const first = reopen(driver);
    await first.transaction(async (tx) => {
      await tx.outboxPut(entry("m1", A));
      await tx.outboxPut(entry("m2", B));
    });

    const second = reopen(driver);
    await second.transaction((tx) => tx.outboxPut(entry("m3", C)));

    const third = reopen(driver);
    const outbox = await third.transaction((tx) => tx.outboxList());
    expect(outbox.map((e) => e.mutationId)).toEqual(["m1", "m2", "m3"]);
  });

  it("keeps insertion order for engine-enqueued mutations across a reopen", async () => {
    const driver = new MemorySQLiteDriver();
    const first = reopen(driver);
    const engine = new SyncEngine({ storage: first, transport: new MockTransport(), newId: idFactory() });

    await engine.applyLocal("update", "notes", A, { title: "one" });
    await engine.applyLocal("update", "notes", B, { title: "two" });
    await engine.applyLocal("update", "notes", C, { title: "three" });

    const second = reopen(driver);
    const outbox = await second.transaction((tx) => tx.outboxList());
    expect(outbox.map((e) => e.recordId)).toEqual([A, B, C]);
  });
});

function idFactory(): () => string {
  let n = 0;
  return () => `m${++n}`;
}
