/**
 * Outbox enqueue + coalescing (protocol §11.1, PDD §9.5).
 */
import type { OutboxEntry, Tx } from "./interfaces";
import type { MutationOp } from "./types";

let fallbackSeq = 0;

/** §3 mutation ID: UUID where available, else a random token. Never a clock. */
export function newMutationId(): string {
  const g = globalThis as { crypto?: { randomUUID?: () => string } };
  const uuid = g.crypto?.randomUUID?.();
  if (uuid) return uuid;
  fallbackSeq += 1;
  return `m${fallbackSeq.toString(36)}${Math.random().toString(36).slice(2, 12)}`;
}

export interface EnqueueInput {
  collection: string;
  recordId: string;
  op: MutationOp;
  baseRev?: number | null;
  fields?: Record<string, unknown>;
}

function recordKey(collection: string, recordId: string): string {
  return `${collection}\u0000${recordId}`;
}

function earliest(a: number | null, b: number | null): number | null {
  if (a == null) return b;
  if (b == null) return a;
  return Math.min(a, b);
}

export function makeEntry(input: EnqueueInput, mutationId: string): OutboxEntry {
  return {
    mutationId,
    collection: input.collection,
    recordId: input.recordId,
    op: input.op,
    baseRev: input.op === "create" ? null : (input.baseRev ?? null),
    fields: input.fields ? { ...input.fields } : {},
    status: "pending",
    attempts: 0,
  };
}

/**
 * Pure coalescing. Returns the resulting outbox array. Only `pending`
 * mutations coalesce; once a mutation is `sent` (in flight or possibly
 * applied) or `errored`, it is a barrier (§11.1).
 */
export function coalesce(outbox: OutboxEntry[], incoming: OutboxEntry): OutboxEntry[] {
  const key = recordKey(incoming.collection, incoming.recordId);
  let lastIndex = -1;
  for (let i = outbox.length - 1; i >= 0; i--) {
    if (recordKey(outbox[i].collection, outbox[i].recordId) === key) {
      lastIndex = i;
      break;
    }
  }
  if (lastIndex === -1) return [...outbox, incoming];

  const last = outbox[lastIndex];
  if (last.status !== "pending") return [...outbox, incoming];

  if (last.op === "create") {
    if (incoming.op === "update") {
      return replaceAt(outbox, lastIndex, { ...last, fields: { ...last.fields, ...incoming.fields } });
    }
    if (incoming.op === "delete") {
      return outbox.filter((_, i) => i !== lastIndex); // create + delete cancels entirely
    }
    return [...outbox, incoming];
  }

  if (last.op === "update" && incoming.op === "update") {
    return replaceAt(outbox, lastIndex, {
      ...last,
      baseRev: earliest(last.baseRev, incoming.baseRev),
      fields: { ...last.fields, ...incoming.fields },
    });
  }

  // update+delete, delete+anything: §11.1 does not define coalescing here, so
  // keep them as separate mutations rather than invent behaviour.
  return [...outbox, incoming];
}

function replaceAt(outbox: OutboxEntry[], index: number, entry: OutboxEntry): OutboxEntry[] {
  const next = outbox.slice();
  next[index] = entry;
  return next;
}

/**
 * Applies a local write's mutation to the outbox inside `tx`, coalescing per
 * §11.1. Mutation IDs are assigned here and never change on later merges.
 */
export async function enqueue(
  tx: Tx,
  input: EnqueueInput,
  newId: () => string = newMutationId,
): Promise<void> {
  const outbox = await tx.outboxList();
  const next = coalesce(outbox, makeEntry(input, newId()));
  const keep = new Set(next.map((e) => e.mutationId));
  for (const e of outbox) {
    if (!keep.has(e.mutationId)) await tx.outboxDelete(e.mutationId);
  }
  for (const e of next) await tx.outboxPut(e);
}
