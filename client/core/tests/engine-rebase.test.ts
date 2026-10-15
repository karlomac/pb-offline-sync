import { describe, expect, it } from "vitest";
import { SyncEngine } from "../src/engine";
import { MemoryStorageAdapter, MockTransport } from "../src/memory";

describe("rebase-on-pull (§11.3)", () => {
  it("preserves a pending local edit over a pulled server change and updates baseRev", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    // Seed a server-known row, then make a local edit that is still pending.
    await storage.transaction((tx) => tx.put("notes", "aaaaaaaaaaaaaaa", { rev: 1, fields: { title: "a", body: "b" } }));
    await engine.applyLocal("update", "notes", "aaaaaaaaaaaaaaa", { body: "B" });

    // Server delivers a newer state written elsewhere.
    transport.pullHandler = () => ({
      mode: "incremental",
      changes: [
        {
          collection: "notes",
          recordId: "aaaaaaaaaaaaaaa",
          op: "upsert",
          rev: 2,
          record: { title: "server", body: "b" },
        },
      ],
      cursor: "c1",
      hasMore: false,
      serverTime: "",
    });

    await engine.pullNow();

    // The user's pending body edit is preserved; the server's title is applied.
    const rec = await storage.transaction((tx) => tx.get("notes", "aaaaaaaaaaaaaaa"));
    expect(rec).toEqual({ rev: 2, fields: { title: "server", body: "B" } });

    // The pending mutation's baseRev moved up to the newly applied rev.
    const outbox = await storage.transaction((tx) => tx.outboxList());
    expect(outbox).toHaveLength(1);
    expect(outbox[0].baseRev).toBe(2);
    expect(outbox[0].fields).toEqual({ body: "B" });
  });
});
