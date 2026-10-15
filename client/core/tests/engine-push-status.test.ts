import { describe, expect, it } from "vitest";
import { SyncEngine } from "../src/engine";
import { MemoryStorageAdapter, MockTransport } from "../src/memory";
import type { PushResult } from "../src/types";

const A = "aaaaaaaaaaaaaaa";
const B = "bbbbbbbbbbbbbbb";
const C = "ccccccccccccccc";
const D = "ddddddddddddddd";
const E = "eeeeeeeeeeeeeee";

// C-PUSH-5: `duplicate` carries the original stored result (§6.5, §9); it must be
// routed by its code exactly like a non-duplicate result.
describe("duplicate result handling (C-PUSH-5, §6.5/§9)", () => {
  it("duplicate with a conflict code follows the conflict path", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await storage.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await engine.applyLocal("update", "notes", A, { title: "mine" });

    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) => ({
        id: m.id,
        status: "duplicate" as const,
        code: "field_conflict",
        rev: 2,
        record: { title: "server" },
        message: "overlap",
      })),
    });

    await engine.syncNow();

    expect(await storage.transaction((tx) => tx.get("notes", A))).toEqual({
      rev: 2,
      fields: { title: "server" },
    });
    expect(await storage.transaction((tx) => tx.outboxList())).toEqual([]);
    const conflicts = await storage.transaction((tx) => tx.conflictList());
    expect(conflicts).toHaveLength(1);
    expect(conflicts[0]).toMatchObject({ code: "field_conflict", fields: { title: "mine" } });
  });

  it("duplicate with a non-conflict code marks the entry errored", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await storage.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await engine.applyLocal("update", "notes", A, { title: "mine" });

    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) => ({
        id: m.id,
        status: "duplicate" as const,
        code: "validation_failed",
      })),
    });

    await engine.syncNow();

    const outbox = await storage.transaction((tx) => tx.outboxList());
    expect(outbox).toHaveLength(1);
    expect(outbox[0]).toMatchObject({ recordId: A, status: "errored" });
    // The local edit is kept (rejection does not roll back local state).
    expect(await storage.transaction((tx) => tx.get("notes", A))).toEqual({
      rev: 1,
      fields: { title: "mine" },
    });
  });

  it("duplicate with no code is treated as ok", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await engine.applyLocal("create", "notes", A, { title: "a" });

    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) => ({
        id: m.id,
        status: "duplicate" as const,
        rev: 1,
        record: { id: A, title: "a" },
      })),
    });

    await engine.syncNow();

    expect(await storage.transaction((tx) => tx.outboxList())).toEqual([]);
    expect(await storage.transaction((tx) => tx.get("notes", A))).toEqual({
      rev: 1,
      fields: { id: A, title: "a" },
    });
  });
});

// C-PUSH-6: an unrecognised status must be kept and flagged, never dropped or
// mis-applied (§2: clients treat unknown codes by status class).
describe("unknown mutation status (C-PUSH-6, §2)", () => {
  it("marks the entry errored without dropping or applying it", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await storage.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await engine.applyLocal("update", "notes", A, { title: "mine" });

    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) => ({
        id: m.id,
        status: "something-new" as unknown as PushResult["status"],
      })),
    });

    await engine.syncNow();

    const outbox = await storage.transaction((tx) => tx.outboxList());
    expect(outbox).toHaveLength(1);
    expect(outbox[0]).toMatchObject({ recordId: A, status: "errored" });
    // Local state is untouched by the flag: the mutation was neither applied by
    // the server nor discarded from the outbox.
    expect(await storage.transaction((tx) => tx.get("notes", A))).toEqual({
      rev: 1,
      fields: { title: "mine" },
    });
  });
});

// C-PUSH-7: an outbox larger than maxBatch is split and the remainder is sent
// on later batches until it drains (§11.6).
describe("maxBatch splitting (C-PUSH-7, §11.6)", () => {
  it("splits into maxBatch-sized batches and drains the outbox", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport, maxBatch: 2 });

    for (const [id, title] of [
      [A, "a"],
      [B, "b"],
      [C, "c"],
      [D, "d"],
      [E, "e"],
    ] as const) {
      await engine.applyLocal("create", "notes", id, { title });
    }

    await engine.syncNow();

    expect(transport.pushed.map((batch) => batch.length)).toEqual([2, 2, 1]);
    for (const batch of transport.pushed) expect(batch.length).toBeLessThanOrEqual(2);
    // Every entry was sent exactly once (default handler acks each).
    const sent = transport.pushed.flat();
    expect(sent.map((m) => m.recordId).sort()).toEqual([A, B, C, D, E].sort());
    expect(await storage.transaction((tx) => tx.outboxList())).toEqual([]);
  });
});

// C-PUSH-8: the pull loop follows `hasMore` to head.
describe("pull paging follows hasMore (C-PUSH-8, §11.3)", () => {
  it("applies every page and persists the final cursor", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    const seen: Array<string | null> = [];
    transport.pullHandler = (cursor) => {
      seen.push(cursor);
      if (cursor === null) {
        return {
          mode: "snapshot",
          changes: [
            { collection: "notes", recordId: A, op: "upsert", rev: 1, record: { title: "a" } },
          ],
          cursor: "p1",
          hasMore: true,
          serverTime: "",
        };
      }
      if (cursor === "p1") {
        return {
          mode: "snapshot",
          changes: [
            { collection: "notes", recordId: B, op: "upsert", rev: 1, record: { title: "b" } },
          ],
          cursor: "p2",
          hasMore: false,
          serverTime: "",
        };
      }
      throw new Error(`unexpected cursor ${String(cursor)}`);
    };

    await engine.pullNow();

    expect(seen).toEqual([null, "p1"]);
    expect(await storage.transaction((tx) => tx.get("notes", A))).toEqual({
      rev: 1,
      fields: { title: "a" },
    });
    expect(await storage.transaction((tx) => tx.get("notes", B))).toEqual({
      rev: 1,
      fields: { title: "b" },
    });
    expect((await storage.transaction((tx) => tx.syncStateGet())).cursor).toBe("p2");
  });
});
