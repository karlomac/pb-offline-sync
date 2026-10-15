import { describe, expect, it } from "vitest";
import { SyncEngine } from "../src/engine";
import { MemoryStorageAdapter, MockTransport } from "../src/memory";
import { TransportError } from "../src/types";

const A = "aaaaaaaaaaaaaaa";
const C = "ccccccccccccccc";

const emptyPage = {
  mode: "incremental" as const,
  changes: [],
  cursor: "c1",
  hasMore: false,
  serverTime: "",
};

// C-LIFE-1: concurrent syncNow() calls share one loop (§11.6 single-flight).
describe("single-flight (C-LIFE-1, §11.6)", () => {
  it("runs one push/pull sequence for concurrent syncNow calls", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await engine.applyLocal("create", "notes", A, { title: "a" });

    let pulls = 0;
    transport.pullHandler = () => {
      pulls += 1;
      return emptyPage;
    };

    await Promise.all([engine.syncNow(), engine.syncNow(), engine.syncNow()]);

    expect(transport.pushed).toHaveLength(1);
    expect(pulls).toBe(1);
  });
});

// C-LIFE-2: a 410 during the normal loop triggers an automatic resync (§11.5).
describe("automatic resync on 410 (C-LIFE-2, §11.5)", () => {
  it("recovers by snapshotting with a null cursor and preserves the outbox", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await storage.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await storage.transaction((tx) =>
      tx.syncStatePut({ cursor: "old", lastSyncAt: null, lastError: null }),
    );
    // A mutation that will not be acknowledged (rejected -> stays errored) so we
    // can prove the resync did not discard the outbox.
    await engine.applyLocal("create", "notes", C, { title: "c" });

    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) => ({
        id: m.id,
        status: "rejected" as const,
        code: "validation_failed",
      })),
    });
    transport.pullHandler = (cursor) => {
      if (cursor === "old") {
        throw new TransportError("stale cursor", { status: 410, code: "cursor_too_old" });
      }
      if (cursor !== null) throw new Error(`expected a null cursor after resync, got ${cursor}`);
      return {
        mode: "snapshot",
        changes: [
          { collection: "notes", recordId: A, op: "upsert", rev: 1, record: { title: "a" } },
        ],
        cursor: "fresh",
        hasMore: false,
        serverTime: "",
      };
    };

    await engine.syncNow();

    expect((await storage.transaction((tx) => tx.syncStateGet())).cursor).toBe("fresh");
    const outbox = await storage.transaction((tx) => tx.outboxList());
    expect(outbox).toHaveLength(1);
    expect(outbox[0]).toMatchObject({ recordId: C, status: "errored" });
    expect(await storage.transaction((tx) => tx.get("notes", A))).toEqual({
      rev: 1,
      fields: { title: "a" },
    });
    expect((await engine.status()).state).toBe("idle");
  });
});

// C-LIFE-3: a 401 pauses sync but must not drop the outbox (§11.2).
describe("401 pauses without dropping the outbox (C-LIFE-3, §11.2)", () => {
  it("keeps the outbox and surfaces the error", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await engine.applyLocal("create", "notes", A, { title: "a" });

    transport.pushHandler = () => {
      throw new TransportError("unauthorized", { status: 401, code: "unauthorized" });
    };

    await engine.syncNow();

    const outbox = await storage.transaction((tx) => tx.outboxList());
    expect(outbox).toHaveLength(1);
    const status = await engine.status();
    expect(status.pending).toBe(1);
    expect(status.lastError).toBeTruthy();
  });
});

// C-LIFE-4: exponential backoff with jitter, capped (§11.2).
describe("backoff (C-LIFE-4, §11.2)", () => {
  it("grows the exponent, stays within [exp/2, exp] and caps at 60s", () => {
    const engine = new SyncEngine({
      storage: new MemoryStorageAdapter(),
      transport: new MockTransport(),
    });
    const base = 1000;
    const cap = 60_000;

    let prevExp = 0;
    for (let attempt = 1; attempt <= 8; attempt++) {
      const exp = Math.min(cap, base * 2 ** (attempt - 1));
      expect(exp).toBeGreaterThanOrEqual(prevExp); // monotone non-decreasing
      prevExp = exp;
      for (let i = 0; i < 100; i++) {
        const d = engine.backoffDelay(attempt);
        expect(d).toBeGreaterThanOrEqual(Math.floor(exp / 2));
        expect(d).toBeLessThanOrEqual(exp);
        expect(Number.isInteger(d)).toBe(true);
      }
    }

    // Far past the cap the delay stays pinned to the 60s window.
    for (let i = 0; i < 100; i++) {
      const d = engine.backoffDelay(50);
      expect(d).toBeGreaterThanOrEqual(cap / 2);
      expect(d).toBeLessThanOrEqual(cap);
    }
  });
});

// C-LIFE-5: lifecycle triggers funnel into syncNow (§9.6/§10).
describe("lifecycle triggers (C-LIFE-5, §10)", () => {
  for (const trigger of ["onForeground", "onConnectivity", "onNudge"] as const) {
    it(`${trigger} runs a sync`, async () => {
      const storage = new MemoryStorageAdapter();
      const transport = new MockTransport();
      const engine = new SyncEngine({ storage, transport });

      await engine.applyLocal("create", "notes", A, { title: "a" });
      let pulls = 0;
      transport.pullHandler = () => {
        pulls += 1;
        return emptyPage;
      };

      await engine[trigger]();

      expect(transport.pushed).toHaveLength(1);
      expect(pulls).toBe(1);
    });
  }
});

// C-LIFE-6: status() reports state, pending, lastError and conflicts.
describe("status (C-LIFE-6)", () => {
  it("reports state, pending count, lastError and conflicts", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await engine.applyLocal("create", "notes", A, { title: "a" });
    expect(await engine.status()).toEqual({
      state: "idle",
      pending: 1,
      lastError: null,
      conflicts: [],
    });

    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) => ({
        id: m.id,
        status: "rejected" as const,
        code: "validation_failed",
      })),
    });
    await engine.syncNow();

    const failed = await engine.status();
    expect(failed.state).toBe("idle");
    expect(failed.pending).toBe(1); // errored entry stays in the outbox
    expect(failed.lastError).toBeTruthy();
  });

  it("lists recorded conflicts", async () => {
    const storage = new MemoryStorageAdapter();
    const transport = new MockTransport();
    const engine = new SyncEngine({ storage, transport });

    await storage.transaction((tx) => tx.put("notes", A, { rev: 1, fields: { title: "a" } }));
    await engine.applyLocal("update", "notes", A, { title: "mine" });

    transport.pushHandler = (mutations) => ({
      results: mutations.map((m) => ({
        id: m.id,
        status: "conflict" as const,
        code: "field_conflict",
        rev: 2,
        record: { title: "server" },
      })),
    });
    await engine.syncNow();

    const status = await engine.status();
    expect(status.pending).toBe(0);
    expect(status.conflicts).toHaveLength(1);
    expect(status.conflicts[0]).toMatchObject({ recordId: A, code: "field_conflict" });
  });
});
