/**
 * Platform seams (PDD §9.2). The engine only knows these; concrete SQLite/web
 * adapters and the PocketBase transport (M5) implement them.
 */
import type { Mutation, PushResponse, PullResponse } from "./types";

/**
 * A synced-collection row. `rev` is the last server rev; `0` means the row was
 * created locally and not yet acknowledged (protocol §8.1 revs start at 1).
 *
 * PDD §9.3 also lists a `_dirty` column; it is intentionally omitted here
 * because "has pending outbox mutations" is derived from the outbox (single
 * source of truth) and duplicating it can drift.
 */
export interface StoredRecord {
  rev: number;
  fields: Record<string, unknown>;
}

export type OutboxStatus = "pending" | "sent" | "errored";

/**
 * `_outbox` row (PDD §9.3). `created_at` is omitted: it is not needed for
 * ordering (outbox array order is authoritative) and §11.6 forbids device
 * clocks for ordering/conflict resolution.
 */
export interface OutboxEntry {
  mutationId: string;
  collection: string;
  recordId: string;
  op: import("./types").MutationOp;
  baseRev: number | null;
  fields: Record<string, unknown>;
  status: OutboxStatus;
  attempts: number;
}

/** `_sync_state` row. */
export interface SyncState {
  cursor: string | null;
  lastSyncAt: number | null;
  lastError: string | null;
}

/** `_conflicts` row surfaced to the application (§11.4). */
export interface ConflictRecord {
  id: string;
  collection: string;
  recordId: string;
  code: string;
  fields: Record<string, unknown>;
  message?: string;
}

/** All reads/writes run inside one transaction scope (PDD §9.2). */
export interface Tx {
  get(collection: string, recordId: string): Promise<StoredRecord | null>;
  put(collection: string, recordId: string, record: StoredRecord): Promise<void>;
  delete(collection: string, recordId: string): Promise<void>;
  list(collection: string): Promise<Array<{ recordId: string; record: StoredRecord }>>;
  collections(): Promise<string[]>;

  outboxList(): Promise<OutboxEntry[]>;
  outboxPut(entry: OutboxEntry): Promise<void>;
  outboxDelete(mutationId: string): Promise<void>;

  syncStateGet(): Promise<SyncState>;
  syncStatePut(state: SyncState): Promise<void>;

  conflictAdd(conflict: ConflictRecord): Promise<void>;
  conflictList(): Promise<ConflictRecord[]>;
  conflictDelete(id: string): Promise<void>;
}

export interface StorageAdapter {
  /** Runs `fn` atomically: any throw rolls back every write made through `tx`. */
  transaction<T>(fn: (tx: Tx) => Promise<T>): Promise<T>;
}

export interface Transport {
  push(mutations: Mutation[]): Promise<PushResponse>;
  pull(cursor: string | null, limit: number): Promise<PullResponse>;
  /** §10 nudge subscription; returns an unsubscribe function. */
  subscribe(onNudge: () => void): () => void;
}
