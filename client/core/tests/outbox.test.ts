import { describe, expect, it } from "vitest";
import type { OutboxEntry } from "../src/interfaces";
import { MemoryStorageAdapter } from "../src/memory";
import { coalesce, enqueue, makeEntry } from "../src/outbox";

function entry(over: Partial<OutboxEntry> & Pick<OutboxEntry, "op">): OutboxEntry {
  return makeEntry(
    {
      collection: "notes",
      recordId: "aaaaaaaaaaaaaaa",
      op: over.op,
      baseRev: over.baseRev,
      fields: over.fields,
    },
    over.mutationId ?? "m1",
  );
}

describe("outbox coalescing (§11.1)", () => {
  it("merges consecutive updates: union of fields, earliest baseRev, same id", () => {
    const outbox = [entry({ op: "update", baseRev: 3, fields: { title: "a" }, mutationId: "m1" })];
    const merged = coalesce(outbox, entry({ op: "update", baseRev: 5, fields: { body: "b" }, mutationId: "m2" }));

    expect(merged).toHaveLength(1);
    expect(merged[0].mutationId).toBe("m1"); // id assigned at first enqueue, never changes
    expect(merged[0].fields).toEqual({ title: "a", body: "b" });
    expect(merged[0].baseRev).toBe(3); // earliest
  });

  it("collapses create + update into one create (no baseRev)", () => {
    const outbox = [entry({ op: "create", fields: { title: "a" }, mutationId: "m1" })];
    const merged = coalesce(outbox, entry({ op: "update", baseRev: 1, fields: { body: "b" }, mutationId: "m2" }));

    expect(merged).toHaveLength(1);
    expect(merged[0].op).toBe("create");
    expect(merged[0].baseRev).toBeNull();
    expect(merged[0].fields).toEqual({ title: "a", body: "b" });
  });

  it("cancels create + delete entirely", () => {
    const outbox = [entry({ op: "create", fields: { title: "a" }, mutationId: "m1" })];
    expect(coalesce(outbox, entry({ op: "delete", baseRev: 1, mutationId: "m2" }))).toEqual([]);
  });

  it("does not coalesce a mutation that was already sent", () => {
    const sent = entry({ op: "update", baseRev: 3, fields: { title: "a" }, mutationId: "m1" });
    sent.status = "sent";
    const merged = coalesce([sent], entry({ op: "update", baseRev: 4, fields: { body: "b" }, mutationId: "m2" }));

    expect(merged).toHaveLength(2);
    expect(merged[0].status).toBe("sent");
    expect(merged[0].fields).toEqual({ title: "a" });
    expect(merged[1].fields).toEqual({ body: "b" });
  });
});

describe("enqueue", () => {
  it("persists coalesced result through a transaction", async () => {
    const storage = new MemoryStorageAdapter();
    let n = 0;
    const id = () => `m${++n}`;

    await storage.transaction((tx) =>
      enqueue(tx, { collection: "notes", recordId: "aaaaaaaaaaaaaaa", op: "create", fields: { title: "a" } }, id),
    );
    await storage.transaction((tx) =>
      enqueue(tx, { collection: "notes", recordId: "aaaaaaaaaaaaaaa", op: "update", baseRev: 1, fields: { body: "b" } }, id),
    );

    const outbox = await storage.transaction((tx) => tx.outboxList());
    expect(outbox).toHaveLength(1);
    expect(outbox[0]).toMatchObject({ mutationId: "m1", op: "create", fields: { title: "a", body: "b" } });
  });
});
