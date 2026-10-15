import { describe, expect, it } from "vitest";
import { SyncEngine } from "../src/engine";
import { MemoryStorageAdapter, MockTransport } from "../src/memory";
import type { Change, PullResponse, StoredRecord } from "../src/index";

function page(changes: Change[]): PullResponse {
  return { mode: "incremental", changes, cursor: "c1", hasMore: false, serverTime: "" };
}

async function seed(storage: MemoryStorageAdapter, recordId: string, record: StoredRecord) {
  await storage.transaction((tx) => tx.put("notes", recordId, record));
}

describe("apply-by-rev idempotency (§11.3)", () => {
  it("ignores stale and duplicate upserts, applies a newer one", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });
    await seed(storage, "aaaaaaaaaaaaaaa", { rev: 3, fields: { title: "local" } });

    transport.pullHandler = () =>
      page([
        { collection: "notes", recordId: "aaaaaaaaaaaaaaa", op: "upsert", rev: 2, record: { title: "stale" } },
        { collection: "notes", recordId: "aaaaaaaaaaaaaaa", op: "upsert", rev: 3, record: { title: "dup" } },
        { collection: "notes", recordId: "aaaaaaaaaaaaaaa", op: "upsert", rev: 4, record: { title: "new" } },
      ]);

    await engine.pullNow();

    const rec = await storage.transaction((tx) => tx.get("notes", "aaaaaaaaaaaaaaa"));
    expect(rec).toEqual({ rev: 4, fields: { title: "new" } });
  });

  it("applies a delete of an unknown record", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    transport.pullHandler = () =>
      page([{ collection: "notes", recordId: "bbbbbbbbbbbbbbb", op: "delete", rev: 5 }]);

    await engine.pullNow();

    const rec = await storage.transaction((tx) => tx.get("notes", "bbbbbbbbbbbbbbb"));
    expect(rec).toBeNull();
    const state = await storage.transaction((tx) => tx.syncStateGet());
    expect(state.cursor).toBe("c1");
  });

  it("ignores a delete older than the local rev", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });
    await seed(storage, "aaaaaaaaaaaaaaa", { rev: 5, fields: { title: "keep" } });

    transport.pullHandler = () =>
      page([{ collection: "notes", recordId: "aaaaaaaaaaaaaaa", op: "delete", rev: 4 }]);

    await engine.pullNow();

    const rec = await storage.transaction((tx) => tx.get("notes", "aaaaaaaaaaaaaaa"));
    expect(rec?.fields).toEqual({ title: "keep" });
  });
});
