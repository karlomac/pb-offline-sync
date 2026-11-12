/**
 * Sync engine (PDD §9.6, protocol §11). State machine
 * idle → pushing → pulling → idle, plus offline/backoff/error. Single-flight.
 */
import { enqueue, newMutationId } from "./outbox";
import type { EnqueueInput } from "./outbox";
import { TransportError, isConflictCode } from "./types";
import type {
  Change,
  Mutation,
  MutationOp,
  PullResponse,
  PushResult,
} from "./types";
import type {
  ConflictRecord,
  OutboxEntry,
  StorageAdapter,
  SyncState,
  Transport,
  Tx,
} from "./interfaces";

/** Bounded retry/pagination guards so a hostile or buggy server cannot wedge a sync run (audit M-7). */
const MAX_NO_PROGRESS = 3;
const MAX_PULL_PAGES = 10_000;
const MAX_RESYNCS = 3;

export type EngineState = "idle" | "pushing" | "pulling" | "offline" | "error" | "backoff";

export interface SyncStatus {
  state: EngineState;
  pending: number;
  lastError: string | null;
  conflicts: ConflictRecord[];
}

export interface EngineOptions {
  storage: StorageAdapter;
  transport: Transport;
  maxBatch?: number;
  maxPage?: number;
  newId?: () => string;
  /** Wall clock for `lastSyncAt` only — never used for ordering/conflict (§11.6). */
  now?: () => number;
  /** Optional backoff scheduler; omitted means "no automatic retry" (M5 wires it). */
  schedule?: (fn: () => void, delayMs: number) => void;
}

function recordKey(collection: string, recordId: string): string {
  return `${collection}\u0000${recordId}`;
}

export class SyncEngine {
  private readonly storage: StorageAdapter;
  private readonly transport: Transport;
  private readonly maxBatch: number;
  private readonly maxPage: number;
  private readonly newId: () => string;
  private readonly now: () => number;
  private readonly schedule?: (fn: () => void, delayMs: number) => void;

  private state: EngineState = "idle";
  private lastError: string | null = null;
  private inflight: Promise<void> | null = null;
  private retryAttempt = 0;
  private resyncAttempt = 0;

  constructor(opts: EngineOptions) {
    this.storage = opts.storage;
    this.transport = opts.transport;
    this.maxBatch = opts.maxBatch ?? 100;
    this.maxPage = opts.maxPage ?? 200;
    this.newId = opts.newId ?? newMutationId;
    this.now = opts.now ?? (() => Date.now());
    this.schedule = opts.schedule;
  }

  /**
   * Local write path (§11.1): apply to local storage and enqueue the mutation
   * in a single transaction.
   */
  async applyLocal(
    op: MutationOp,
    collection: string,
    recordId: string,
    fields?: Record<string, unknown>,
  ): Promise<void> {
    await this.storage.transaction(async (tx) => {
      let baseRev: number | null = null;
      if (op === "delete") {
        baseRev = (await tx.get(collection, recordId))?.rev ?? 0;
        await tx.delete(collection, recordId);
      } else {
        const cur = await tx.get(collection, recordId);
        await tx.put(collection, recordId, {
          rev: cur?.rev ?? 0,
          fields: { ...(cur?.fields ?? {}), ...(fields ?? {}) },
        });
        // A pending create has rev 0; the update coalesces into it so this
        // baseRev never reaches the wire. (If the create was already sent it
        // is resolved via idempotent resend, §9.)
        baseRev = op === "create" ? null : (cur?.rev ?? 0);
      }
      const input: EnqueueInput = { collection, recordId, op, baseRev, fields };
      await enqueue(tx, input, this.newId);
    });
  }

  /** Single-flight (§11.6): concurrent calls share one loop. */
  syncNow(): Promise<void> {
    if (this.inflight) return this.inflight;
    const p = this.run().finally(() => {
      this.inflight = null;
    });
    this.inflight = p;
    return p;
  }

  /** Pull only (hosts/tests); the full loop is `syncNow`. */
  async pullNow(): Promise<void> {
    await this.pullAll();
  }

  /** Full resync (§11.5): discard cursor, snapshot, drop unreferenced rows, rebase. */
  async resync(): Promise<void> {
    await this.setCursor(null);
    await this.pullAll();
  }

  // Lifecycle triggers (§9.6/§10). All funnel into single-flight syncNow.
  onForeground(): Promise<void> {
    return this.syncNow();
  }
  onConnectivity(): Promise<void> {
    return this.syncNow();
  }
  onNudge(): Promise<void> {
    return this.syncNow();
  }
  stop(): void {
    this.state = "idle";
  }

  async status(): Promise<SyncStatus> {
    return this.storage.transaction(async (tx) => ({
      state: this.state,
      pending: (await tx.outboxList()).length,
      lastError: this.lastError,
      conflicts: await tx.conflictList(),
    }));
  }

  /** Exponential backoff with jitter (§11.2). Exposed for hosts/tests. */
  backoffDelay(attempt: number): number {
    const base = 1000;
    const cap = 60_000;
    const exp = Math.min(cap, base * 2 ** Math.max(0, attempt - 1));
    return Math.floor(exp / 2 + (Math.random() * exp) / 2);
  }

  // ── loop ────────────────────────────────────────────────────────────────

  private async run(): Promise<void> {
    this.lastError = null;
    try {
      this.state = "pushing";
      await this.pushAll();
      this.state = "pulling";
      await this.pullAll();
      this.state = "idle";
      this.retryAttempt = 0;
      await this.recordSyncTime();
    } catch (err) {
      if (isCursorTooOld(err)) {
        if (this.resyncAttempt >= MAX_RESYNCS) {
          this.lastError = describe(err);
          this.state = "error";
          return;
        }
        this.resyncAttempt++;
        try {
          await this.resync();
          this.resyncAttempt = 0;
          this.state = "idle";
          this.retryAttempt = 0;
          await this.recordSyncTime();
          return;
        } catch (err2) {
          err = err2;
        }
      }
      this.lastError = describe(err);
      this.state = isOffline(err) ? "offline" : "error";
      this.scheduleRetry();
    }
  }

  private async pushAll(): Promise<void> {
    let noProgress = 0;
    for (;;) {
      const batch = await this.storage.transaction(async (tx) => {
        const b = await this.nextBatch(tx);
        for (const e of b) await tx.outboxPut({ ...e, status: "sent", attempts: e.attempts + 1 });
        return b;
      });
      if (batch.length === 0) return;

      const response = await this.transport.push(batch.map(toMutation));
      if (!response.results || response.results.length === 0) {
        // Retried with the same IDs; the whole batch stays `sent` (§6.6).
        throw new TransportError("empty push response");
      }

      const applied = await this.storage.transaction(async (tx) => {
        let matched = 0;
        for (const result of response.results) {
          if (result == null || typeof result.id !== "string") continue;
          const entry = batch.find((e) => e.mutationId === result.id);
          if (entry) {
            matched++;
            await this.applyPushResult(tx, entry, result);
          }
        }
        return matched;
      });
      // Results that match no batch entry apply nothing: an identical batch
      // would be pushed forever (M-7).
      if (applied === 0) {
        if (++noProgress >= MAX_NO_PROGRESS) {
          throw new TransportError("push made no progress", { code: "invalid_response" });
        }
      } else {
        noProgress = 0;
      }
    }
  }

  private async nextBatch(tx: Tx): Promise<OutboxEntry[]> {
    const outbox = await tx.outboxList();
    const blocked = new Set<string>();
    const batch: OutboxEntry[] = [];
    for (const e of outbox) {
      const key = recordKey(e.collection, e.recordId);
      if (e.status === "errored") {
        blocked.add(key); // later mutations for this record are held (§11.2)
        continue;
      }
      if (blocked.has(key)) continue;
      if (batch.length >= this.maxBatch) break;
      batch.push(e);
      // A `sent` entry is re-sent (same mutation id, §6.6), but it also acts as
      // a barrier: a later pending edit for the same record must wait until its
      // result is applied and rebases the edit's baseRev, otherwise it could be
      // sent with baseRev 0 (a create not yet acknowledged) and be rejected.
      if (e.status === "sent") blocked.add(key);
    }
    return batch;
  }

  private async applyPushResult(tx: Tx, entry: OutboxEntry, result: PushResult): Promise<void> {
    switch (result.status) {
      case "ok":
        await tx.outboxDelete(entry.mutationId);
        await this.storeServerState(tx, entry, result);
        return;
      case "duplicate":
        // §6.5: carries the original result, surfaced with status "duplicate".
        if (isConflictCode(result.code)) await this.applyConflict(tx, entry, result);
        else if (result.code) await this.markErrored(tx, entry, result);
        else {
          await tx.outboxDelete(entry.mutationId);
          await this.storeServerState(tx, entry, result);
        }
        return;
      case "conflict":
        await this.applyConflict(tx, entry, result);
        return;
      case "rejected":
        await this.markErrored(tx, entry, result);
        return;
      default:
        // §2: unknown status → treat as failed, keep it flagged.
        await this.markErrored(tx, entry, { ...result, code: result.code ?? "unknown_status" });
        return;
    }
  }

  /** Store the returned record/rev, rebasing any later pending edits (§11.2/§11.3). */
  private async storeServerState(tx: Tx, entry: OutboxEntry, result: PushResult): Promise<void> {
    if (entry.op === "delete") {
      await tx.delete(entry.collection, entry.recordId);
      return;
    }
    const current = await tx.get(entry.collection, entry.recordId);
    const rev = result.rev ?? current?.rev ?? 0;
    let fields = result.record ? { ...result.record } : { ...(current?.fields ?? {}) };
    for (const e of await this.visiblePending(tx, entry.collection, entry.recordId)) {
      if (e.op !== "delete") fields = { ...fields, ...e.fields };
      await tx.outboxPut({ ...e, baseRev: e.op === "create" ? null : rev });
    }
    await tx.put(entry.collection, entry.recordId, { rev, fields });
  }

  /** §11.4 default local conflict resolution. */
  private async applyConflict(tx: Tx, entry: OutboxEntry, result: PushResult): Promise<void> {
    await tx.outboxDelete(entry.mutationId);
    const code = result.code ?? "conflict";
    if (code === "deleted") {
      await tx.delete(entry.collection, entry.recordId);
    } else if (result.record) {
      const current = await tx.get(entry.collection, entry.recordId);
      await tx.put(entry.collection, entry.recordId, {
        rev: result.rev ?? current?.rev ?? 0,
        fields: { ...result.record },
      });
    }
    await this.addConflict(tx, {
      collection: entry.collection,
      recordId: entry.recordId,
      code,
      fields: { ...entry.fields },
      message: result.message,
    });
  }

  private async markErrored(tx: Tx, entry: OutboxEntry, result: PushResult): Promise<void> {
    await tx.outboxPut({ ...entry, status: "errored" });
    this.lastError =
      result.message ??
      `${entry.op} ${entry.collection}/${entry.recordId} rejected (${result.code ?? "unknown"})`;
  }

  private async visiblePending(tx: Tx, collection: string, recordId: string): Promise<OutboxEntry[]> {
    return (await tx.outboxList()).filter(
      (e) => e.collection === collection && e.recordId === recordId && e.status !== "errored",
    );
  }

  private async addConflict(tx: Tx, c: Omit<ConflictRecord, "id">): Promise<void> {
    await tx.conflictAdd({ id: this.newId(), ...c });
  }

  // ── pull ────────────────────────────────────────────────────────────────

  private async pullAll(): Promise<void> {
    const cursor0 = (await this.readState()).cursor;
    // Snapshot cleanup (§11.5) is only valid when this run started the snapshot
    // from the beginning: a resumed snapshot (persisted cursor, e.g. after the
    // app was killed mid-snapshot) has no in-memory record of the pages already
    // delivered, so cleaning up would delete records delivered before the resume.
    const startedSnapshotFresh = cursor0 === null;
    let cursor = cursor0;
    let delivered: Set<string> | null = null;
    let pages = 0;
    for (;;) {
      const page = await this.transport.pull(cursor, this.maxPage);
      if (page == null || !Array.isArray(page.changes)) {
        throw new TransportError("invalid pull response", { code: "invalid_response" });
      }
      if (page.mode === "snapshot") delivered ??= new Set();
      await this.applyPage(page, delivered);
      if (!page.hasMore) break;
      // A non-advancing or missing cursor with hasMore=true would loop forever
      // (M-7); a page count cap bounds any server that keeps advancing without
      // ever finishing.
      if (typeof page.cursor !== "string" || page.cursor === "" || page.cursor === cursor) {
        throw new TransportError("pull cursor did not advance", { code: "invalid_response" });
      }
      cursor = page.cursor;
      if (++pages > MAX_PULL_PAGES) {
        throw new TransportError("pull page limit exceeded", { code: "invalid_response" });
      }
    }
    if (delivered && startedSnapshotFresh) await this.cleanupSnapshot(delivered);
  }

  /** Apply a whole page and persist the new cursor in one transaction (§11.3). */
  private async applyPage(page: PullResponse, delivered: Set<string> | null): Promise<void> {
    await this.storage.transaction(async (tx) => {
      for (const change of page.changes) {
        if (change == null || typeof change.collection !== "string" || typeof change.recordId !== "string") continue;
        if (delivered) delivered.add(recordKey(change.collection, change.recordId));
        await this.applyChange(tx, change);
      }
      const state = await tx.syncStateGet();
      await tx.syncStatePut({ ...state, cursor: page.cursor });
    });
  }

  /** §11.3 apply-by-rev with rebase of pending edits. */
  private async applyChange(tx: Tx, change: Change): Promise<void> {
    // rev is server-authoritative; a non-integer would corrupt ordering (M-8).
    if (!Number.isSafeInteger(change.rev) || change.rev < 1) return;
    const local = await tx.get(change.collection, change.recordId);
    const localRev = local && local.rev > 0 ? local.rev : null;
    if (localRev !== null && change.rev <= localRev) return; // stale or duplicate
    const pending = await this.visiblePending(tx, change.collection, change.recordId);

    if (change.op === "upsert") {
      if (!change.record) return; // protocol guarantees a full record; ignore if absent
      if (pending.some((e) => e.op === "delete")) {
        // Local intent is deletion: don't resurrect, just move the delete's base.
        for (const e of pending) await tx.outboxPut({ ...e, baseRev: change.rev });
        return;
      }
      let fields = { ...change.record };
      for (const e of pending) {
        fields = { ...fields, ...e.fields };
        await tx.outboxPut({ ...e, baseRev: e.op === "create" ? null : change.rev });
      }
      await tx.put(change.collection, change.recordId, { rev: change.rev, fields });
      return;
    }

    // delete: apply; a record with pending edits triggers conflict handling (§11.4)
    for (const e of pending) {
      await this.addConflict(tx, {
        collection: change.collection,
        recordId: change.recordId,
        code: "deleted",
        fields: { ...e.fields },
      });
      await tx.outboxDelete(e.mutationId);
    }
    await tx.delete(change.collection, change.recordId);
  }

  /** §11.5 step 3: drop undelivered synced rows that have no pending mutation. */
  private async cleanupSnapshot(delivered: Set<string>): Promise<void> {
    await this.storage.transaction(async (tx) => {
      const referenced = new Set(
        (await tx.outboxList()).map((e) => recordKey(e.collection, e.recordId)),
      );
      for (const collection of await tx.collections()) {
        for (const { recordId } of await tx.list(collection)) {
          const key = recordKey(collection, recordId);
          if (delivered.has(key) || referenced.has(key)) continue;
          await tx.delete(collection, recordId);
        }
      }
    });
  }

  // ── state helpers ───────────────────────────────────────────────────────

  private readState(): Promise<SyncState> {
    return this.storage.transaction((tx) => tx.syncStateGet());
  }

  private async setCursor(cursor: string | null): Promise<void> {
    await this.storage.transaction(async (tx) => {
      const state = await tx.syncStateGet();
      await tx.syncStatePut({ ...state, cursor });
    });
  }

  private async recordSyncTime(): Promise<void> {
    await this.storage.transaction(async (tx) => {
      const state = await tx.syncStateGet();
      await tx.syncStatePut({ ...state, lastSyncAt: this.now() });
    });
  }

  private scheduleRetry(): void {
    this.retryAttempt += 1;
    const delay = this.backoffDelay(this.retryAttempt);
    this.schedule?.(() => void this.syncNow(), delay);
  }
}

function toMutation(entry: OutboxEntry): Mutation {
  const m: Mutation = {
    id: entry.mutationId,
    collection: entry.collection,
    recordId: entry.recordId,
    op: entry.op,
  };
  if (entry.op !== "create") m.baseRev = entry.baseRev ?? undefined;
  if (entry.op !== "delete") m.fields = { ...entry.fields };
  return m;
}

function isCursorTooOld(err: unknown): boolean {
  return err instanceof TransportError && (err.status === 410 || err.code === "cursor_too_old");
}

function isOffline(err: unknown): boolean {
  if (!(err instanceof TransportError)) return false;
  // A malformed/hostile response is a protocol violation, not a network loss:
  // reporting it as offline would retry it forever behind a friendly state.
  if (err.code === "invalid_response") return false;
  const s = err.status;
  return s === undefined || s >= 500 || s === 429;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
