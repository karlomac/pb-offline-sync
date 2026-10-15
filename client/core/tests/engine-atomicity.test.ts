import { describe, expect, it } from "vitest";
import { SyncEngine } from "../src/engine";
import { MemoryStorageAdapter, MockTransport } from "../src/memory";
import type { StorageAdapter, SyncState, Tx } from "../src/index";

/** Storage wrapper that fails while persisting the cursor, after applying changes. */
class FailingCursorStorage implements StorageAdapter {
  constructor(private inner: StorageAdapter) {}

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

describe("page apply + cursor persist atomicity (§11.3)", () => {
  it("does not advance the cursor or apply the page when persistence fails", async () => {
    const inner = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage: new FailingCursorStorage(inner), transport });

    await inner.transaction((tx) => tx.put("notes", "aaaaaaaaaaaaaaa", { rev: 1, fields: { title: "local" } }));

    transport.pullHandler = () => ({
      mode: "incremental",
      changes: [
        { collection: "notes", recordId: "aaaaaaaaaaaaaaa", op: "upsert", rev: 2, record: { title: "server" } },
        { collection: "notes", recordId: "bbbbbbbbbbbbbbb", op: "upsert", rev: 1, record: { title: "new" } },
      ],
      cursor: "c2",
      hasMore: false,
      serverTime: "",
    });

    await expect(engine.pullNow()).rejects.toThrow("boom");

    const state = await inner.transaction((tx) => tx.syncStateGet());
    expect(state.cursor).toBeNull(); // cursor did not advance

    // The whole page rolled back with the failed cursor write.
    const a = await inner.transaction((tx) => tx.get("notes", "aaaaaaaaaaaaaaa"));
    expect(a).toEqual({ rev: 1, fields: { title: "local" } });
    const b = await inner.transaction((tx) => tx.get("notes", "bbbbbbbbbbbbbbb"));
    expect(b).toBeNull();
  });
});
