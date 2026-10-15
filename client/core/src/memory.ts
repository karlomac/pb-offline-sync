/**
 * In-memory StorageAdapter and a mock Transport for fast, dependency-free tests
 * (PDD §9.2). Not for production.
 */
import type {
  ConflictRecord,
  OutboxEntry,
  StorageAdapter,
  StoredRecord,
  SyncState,
  Transport,
  Tx,
} from "./interfaces";
import type { Mutation, PullResponse, PushResponse } from "./types";

interface MemoryData {
  collections: Map<string, Map<string, StoredRecord>>;
  outbox: OutboxEntry[];
  syncState: SyncState;
  conflicts: ConflictRecord[];
}

export class MemoryStorageAdapter implements StorageAdapter {
  private data: MemoryData = {
    collections: new Map(),
    outbox: [],
    syncState: { cursor: null, lastSyncAt: null, lastError: null },
    conflicts: [],
  };

  // Serializes transactions so a snapshot is never taken mid-transaction.
  private queue: Promise<unknown> = Promise.resolve();

  transaction<T>(fn: (tx: Tx) => Promise<T>): Promise<T> {
    const run = async (): Promise<T> => {
      const snapshot = structuredClone(this.data);
      try {
        return await fn(this.tx());
      } catch (err) {
        this.data = snapshot;
        throw err;
      }
    };
    const result = this.queue.then(run, run);
    this.queue = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  }

  private tx(): Tx {
    const data = this.data;
    return {
      async get(collection, recordId) {
        const rec = data.collections.get(collection)?.get(recordId);
        return rec ? structuredClone(rec) : null;
      },
      async put(collection, recordId, record) {
        let col = data.collections.get(collection);
        if (!col) {
          col = new Map();
          data.collections.set(collection, col);
        }
        col.set(recordId, structuredClone(record));
      },
      async delete(collection, recordId) {
        data.collections.get(collection)?.delete(recordId);
      },
      async list(collection) {
        const col = data.collections.get(collection);
        if (!col) return [];
        return [...col.entries()].map(([recordId, record]) => ({
          recordId,
          record: structuredClone(record),
        }));
      },
      async collections() {
        return [...data.collections.keys()];
      },
      async outboxList() {
        return structuredClone(data.outbox);
      },
      async outboxPut(entry) {
        const i = data.outbox.findIndex((e) => e.mutationId === entry.mutationId);
        const copy = structuredClone(entry);
        if (i >= 0) data.outbox[i] = copy;
        else data.outbox.push(copy);
      },
      async outboxDelete(mutationId) {
        const i = data.outbox.findIndex((e) => e.mutationId === mutationId);
        if (i >= 0) data.outbox.splice(i, 1);
      },
      async syncStateGet() {
        return structuredClone(data.syncState);
      },
      async syncStatePut(state) {
        data.syncState = structuredClone(state);
      },
      async conflictAdd(conflict) {
        data.conflicts.push(structuredClone(conflict));
      },
      async conflictList() {
        return structuredClone(data.conflicts);
      },
      async conflictDelete(id) {
        const i = data.conflicts.findIndex((c) => c.id === id);
        if (i >= 0) data.conflicts.splice(i, 1);
      },
    };
  }
}

export type PushHandler = (mutations: Mutation[]) => PushResponse | Promise<PushResponse>;
export type PullHandler = (cursor: string | null, limit: number) => PullResponse | Promise<PullResponse>;

export class MockTransport implements Transport {
  pushHandler: PushHandler | null = null;
  pullHandler: PullHandler | null = null;
  readonly pushed: Mutation[][] = [];
  private nudges = new Set<() => void>();

  async push(mutations: Mutation[]): Promise<PushResponse> {
    this.pushed.push(structuredClone(mutations));
    if (this.pushHandler) return this.pushHandler(mutations);
    return {
      results: mutations.map((m) => ({
        id: m.id,
        status: "ok" as const,
        rev: 1,
        record: { id: m.recordId, ...(m.fields ?? {}) },
      })),
    };
  }

  async pull(cursor: string | null, _limit: number): Promise<PullResponse> {
    if (this.pullHandler) return this.pullHandler(cursor, _limit);
    return {
      mode: "incremental",
      changes: [],
      cursor: cursor ?? "cursor-0",
      hasMore: false,
      serverTime: "",
    };
  }

  subscribe(onNudge: () => void): () => void {
    this.nudges.add(onNudge);
    return () => {
      this.nudges.delete(onNudge);
    };
  }

  nudge(): void {
    for (const fn of this.nudges) fn();
  }
}
