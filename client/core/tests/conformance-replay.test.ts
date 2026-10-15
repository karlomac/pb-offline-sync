import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { SyncEngine } from "../src/engine";
import { MemoryStorageAdapter } from "../src/memory";
import type { Transport } from "../src/interfaces";
import type { Mutation, PullResponse, PushResult } from "../src/types";

/**
 * C-CONF-2 / E2E-2: replay `conformance/fixtures/*.json` against a small
 * in-test mock server that implements the protocol semantics these fixtures
 * use, asserting each step's `expect` with the §15.1 subset matcher.
 *
 * The fixtures are read-only; see conformance/README.md for the format.
 */

// ── fixture shape ──────────────────────────────────────────────────────────

interface FieldCfg {
  name: string;
  type: string;
  writable: boolean;
}
interface CollectionCfg {
  ownerField: string;
  conflictPolicy: "serverWins" | "clientWins" | "reject";
  deletePolicy: "deleteWins" | "editWins";
  fields: FieldCfg[];
}
interface FixtureConfig {
  collections: Record<string, CollectionCfg>;
}
interface FixtureMutation {
  id: string;
  collection: string;
  recordId: string;
  op: "create" | "update" | "delete";
  baseRev?: number;
  fields?: Record<string, unknown>;
}
interface FixtureStep {
  as: string;
  push?: { mutations: FixtureMutation[] };
  pull?: { cursor?: string; limit?: number };
  expect: unknown;
}
interface Fixture {
  name: string;
  description: string;
  config: FixtureConfig;
  users: string[];
  steps: FixtureStep[];
}

// ── subset matcher (§15.1) ─────────────────────────────────────────────────

function matchExpect(exp: unknown, act: unknown, path = "$"): string | null {
  if (exp === "$any") return null;
  if (exp === "$string") return typeof act === "string" ? null : `${path}: expected $string`;
  if (exp === "$int") {
    return typeof act === "number" && Number.isInteger(act) ? null : `${path}: expected $int`;
  }
  if (Array.isArray(exp)) {
    if (!Array.isArray(act)) return `${path}: expected array`;
    if (exp.length !== act.length) {
      return `${path}: expected ${exp.length} elements, got ${act.length}`;
    }
    for (let i = 0; i < exp.length; i++) {
      const err = matchExpect(exp[i], act[i], `${path}[${i}]`);
      if (err) return err;
    }
    return null;
  }
  if (exp && typeof exp === "object") {
    if (!act || typeof act !== "object") return `${path}: expected object`;
    const record = act as Record<string, unknown>;
    for (const [k, v] of Object.entries(exp as Record<string, unknown>)) {
      if (!(k in record)) return `${path}.${k}: missing`;
      const err = matchExpect(v, record[k], `${path}.${k}`);
      if (err) return err;
    }
    return null;
  }
  return exp === act ? null : `${path}: expected ${JSON.stringify(exp)}, got ${JSON.stringify(act)}`;
}

// ── mock server ────────────────────────────────────────────────────────────

const STALE = "$stale";
const SERVER_FIELDS = new Set(["id", "owner", "created", "updated", "collectionId", "expand"]);

class RequestFailure extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
  ) {
    super(code);
  }
}

interface LogEntry {
  seq: number;
  collection: string;
  recordId: string;
  owner: string;
  op: "create" | "update" | "delete";
  rev: number;
  changed: string[];
}

function hashMutation(m: FixtureMutation): string {
  const fields = Object.fromEntries(
    Object.keys(m.fields ?? {})
      .sort()
      .map((k) => [k, m.fields?.[k]]),
  );
  return JSON.stringify([m.id, m.collection, m.recordId, m.op, m.baseRev ?? null, fields]);
}

/** A server-equivalent mock: ownership, revs, change log, idempotency, policies. */
class MockServer {
  private readonly store = new MemoryStorageAdapter();
  private readonly log: LogEntry[] = [];
  private readonly tombstones = new Set<string>();
  private readonly idOwner = new Map<string, string>();
  private readonly processed = new Map<string, { hash: string; result: PushResult }>();
  private head = 0;

  constructor(private readonly config: FixtureConfig) {}

  private key(collection: string, recordId: string): string {
    return `${collection}\u0000${recordId}`;
  }
  private ns(owner: string, collection: string): string {
    return `\u0000${owner}\u0000${collection}`;
  }

  async push(user: string, mutations: FixtureMutation[]): Promise<PushResult[]> {
    const results: PushResult[] = [];
    const failed = new Set<string>();
    for (const m of mutations) {
      const recordKey = this.key(m.collection, m.recordId);
      const processedKey = `${user}\u0000${m.id}`;
      const stored = this.processed.get(processedKey);
      if (stored) {
        // §9: same payload -> duplicate carrying the original result; different
        // payload -> mutation_id_reuse.
        results.push(
          stored.hash === hashMutation(m)
            ? {
                id: m.id,
                status: "duplicate",
                rev: stored.result.rev,
                record: stored.result.record,
                code: stored.result.code,
              }
            : { id: m.id, status: "rejected", code: "mutation_id_reuse" },
        );
        continue;
      }
      if (failed.has(recordKey)) {
        results.push({ id: m.id, status: "rejected", code: "dependency_failed" });
        continue;
      }
      const result = await this.apply(user, m);
      if (result.status !== "ok") failed.add(recordKey);
      if (result.status !== "duplicate") {
        this.processed.set(processedKey, { hash: hashMutation(m), result });
      }
      results.push(result);
    }
    return results;
  }

  private async apply(user: string, m: FixtureMutation): Promise<PushResult> {
    const cfg = this.config.collections[m.collection];
    if (!cfg) return { id: m.id, status: "rejected", code: "collection_not_synced" };
    if (m.op === "create") {
      if (m.baseRev != null) return { id: m.id, status: "rejected", code: "invalid_base_rev" };
    } else if (m.op === "update" || m.op === "delete") {
      if (m.baseRev == null || m.baseRev < 1) {
        return { id: m.id, status: "rejected", code: "invalid_base_rev" };
      }
    } else {
      return { id: m.id, status: "rejected", code: "invalid_op" };
    }

    if (m.op !== "delete") {
      for (const name of Object.keys(m.fields ?? {})) {
        if (SERVER_FIELDS.has(name)) return { id: m.id, status: "rejected", code: "forbidden_field" };
        const field = cfg.fields.find((f) => f.name === name);
        if (!field) return { id: m.id, status: "rejected", code: "unknown_field" };
        if (field.type === "file") {
          return { id: m.id, status: "rejected", code: "file_field_unsupported" };
        }
      }
    }

    if (m.op === "create") return this.create(user, m);
    if (m.op === "update") return this.update(user, m, cfg);
    return this.delete(user, m, cfg);
  }

  private async create(user: string, m: FixtureMutation): Promise<PushResult> {
    const live = await this.store.transaction((tx) =>
      tx.get(this.ns(user, m.collection), m.recordId),
    );
    if (live) {
      return {
        id: m.id,
        status: "conflict",
        code: "exists",
        rev: live.rev,
        record: { id: m.recordId, ...live.fields },
      };
    }
    const key = this.key(m.collection, m.recordId);
    if (this.idOwner.has(key) || this.tombstones.has(key)) {
      return { id: m.id, status: "rejected", code: "id_unavailable" };
    }
    const fields = { id: m.recordId, ...(m.fields ?? {}) };
    await this.commit(user, m.collection, m.recordId, "create", 1, fields, Object.keys(m.fields ?? {}));
    this.idOwner.set(key, user);
    return { id: m.id, status: "ok", rev: 1, merged: false, record: fields };
  }

  private async update(
    user: string,
    m: FixtureMutation,
    cfg: CollectionCfg,
  ): Promise<PushResult> {
    const live = await this.store.transaction((tx) =>
      tx.get(this.ns(user, m.collection), m.recordId),
    );
    if (!live) {
      return this.tombstones.has(this.key(m.collection, m.recordId))
        ? { id: m.id, status: "conflict", code: "deleted" }
        : { id: m.id, status: "rejected", code: "not_found" };
    }
    const R = live.rev;
    const B = m.baseRev as number;
    if (B > R) return { id: m.id, status: "rejected", code: "invalid_base_rev" };

    const F = Object.keys(m.fields ?? {});
    let merged = false;
    let overlap = false;
    if (B < R) {
      const changed = this.changedSince(user, m.collection, m.recordId, B, R);
      overlap = F.some((f) => changed.has(f));
      if (overlap && cfg.conflictPolicy !== "clientWins") {
        return {
          id: m.id,
          status: "conflict",
          code: "field_conflict",
          rev: R,
          record: { id: m.recordId, ...live.fields },
        };
      }
      if (!overlap) merged = true;
    }

    const fields = { ...live.fields, ...(m.fields ?? {}) };
    const rev = R + 1;
    const changedFields = F.filter((f) => live.fields[f] !== m.fields?.[f]);
    await this.commit(user, m.collection, m.recordId, "update", rev, fields, changedFields);
    const result: PushResult = { id: m.id, status: "ok", rev, merged, record: fields };
    if (overlap) result.conflictId = `conflict-${rev}`;
    return result;
  }

  private async delete(
    user: string,
    m: FixtureMutation,
    cfg: CollectionCfg,
  ): Promise<PushResult> {
    const live = await this.store.transaction((tx) =>
      tx.get(this.ns(user, m.collection), m.recordId),
    );
    if (!live) return { id: m.id, status: "ok" };
    const R = live.rev;
    const B = m.baseRev as number;
    if (B > R) return { id: m.id, status: "rejected", code: "invalid_base_rev" };
    if (B < R && cfg.deletePolicy === "editWins") {
      return {
        id: m.id,
        status: "conflict",
        code: "edited_since",
        rev: R,
        record: { id: m.recordId, ...live.fields },
      };
    }
    const rev = R + 1;
    await this.commitDelete(user, m.collection, m.recordId, rev);
    return { id: m.id, status: "ok", rev };
  }

  private async commit(
    owner: string,
    collection: string,
    recordId: string,
    op: "create" | "update",
    rev: number,
    fields: Record<string, unknown>,
    changed: string[],
  ): Promise<void> {
    await this.store.transaction((tx) =>
      tx.put(this.ns(owner, collection), recordId, { rev, fields }),
    );
    this.head += 1;
    this.log.push({ seq: this.head, collection, recordId, owner, op, rev, changed });
  }

  private async commitDelete(
    owner: string,
    collection: string,
    recordId: string,
    rev: number,
  ): Promise<void> {
    await this.store.transaction((tx) => tx.delete(this.ns(owner, collection), recordId));
    this.tombstones.add(this.key(collection, recordId));
    this.head += 1;
    this.log.push({ seq: this.head, collection, recordId, owner, op: "delete", rev, changed: [] });
  }

  private changedSince(
    owner: string,
    collection: string,
    recordId: string,
    baseRev: number,
    rev: number,
  ): Set<string> {
    const out = new Set<string>();
    for (const e of this.log) {
      if (
        e.owner === owner &&
        e.collection === collection &&
        e.recordId === recordId &&
        e.rev > baseRev &&
        e.rev <= rev
      ) {
        for (const f of e.changed) out.add(f);
      }
    }
    return out;
  }

  async pull(user: string, cursor: string | null, limit: number): Promise<PullResponse> {
    if (cursor === STALE) throw new RequestFailure(410, "cursor_too_old");
    if (cursor == null || cursor === "") return this.snapshot(user, 0, this.head, limit);
    if (cursor.startsWith("snap:")) {
      const [, index, head] = cursor.split(":");
      return this.snapshot(user, Number(index), Number(head), limit);
    }
    if (cursor.startsWith("inc:")) {
      const seq = Number(cursor.slice(4));
      if (seq > this.head) throw new RequestFailure(410, "cursor_too_old");
      return this.incremental(user, seq, limit);
    }
    throw new RequestFailure(400, "cursor_invalid");
  }

  private async snapshot(
    user: string,
    index: number,
    head: number,
    limit: number,
  ): Promise<PullResponse> {
    const records: Array<{
      collection: string;
      recordId: string;
      rev: number;
      record: Record<string, unknown>;
    }> = [];
    for (const collection of Object.keys(this.config.collections)) {
      const rows = await this.store.transaction((tx) => tx.list(this.ns(user, collection)));
      for (const row of rows) {
        records.push({
          collection,
          recordId: row.recordId,
          rev: row.record.rev,
          record: row.record.fields,
        });
      }
    }
    records.sort((a, b) =>
      a.collection !== b.collection
        ? a.collection < b.collection
          ? -1
          : 1
        : a.recordId < b.recordId
          ? -1
          : a.recordId > b.recordId
            ? 1
            : 0,
    );
    const page = records.slice(index, index + limit);
    const hasMore = index + limit < records.length;
    return {
      mode: "snapshot",
      changes: page.map((r) => ({
        collection: r.collection,
        recordId: r.recordId,
        op: "upsert" as const,
        rev: r.rev,
        record: r.record,
      })),
      cursor: hasMore ? `snap:${index + limit}:${head}` : `inc:${head}`,
      hasMore,
      serverTime: "",
    };
  }

  private async incremental(
    user: string,
    fromSeq: number,
    limit: number,
  ): Promise<PullResponse> {
    // §7.2: latest state per (collection, recordId), ascending seq.
    const latest = new Map<string, LogEntry>();
    for (const e of this.log) {
      if (e.owner !== user || e.seq <= fromSeq) continue;
      const key = this.key(e.collection, e.recordId);
      const prev = latest.get(key);
      if (!prev || e.seq > prev.seq) latest.set(key, e);
    }
    const entries = [...latest.values()].sort((a, b) => a.seq - b.seq);
    const page = entries.slice(0, limit);
    const changes: PullResponse["changes"] = [];
    for (const e of page) {
      if (e.op === "delete") {
        changes.push({ collection: e.collection, recordId: e.recordId, op: "delete", rev: e.rev });
      } else {
        const rec = await this.store.transaction((tx) =>
          tx.get(this.ns(user, e.collection), e.recordId),
        );
        if (rec) {
          changes.push({
            collection: e.collection,
            recordId: e.recordId,
            op: "upsert",
            rev: e.rev,
            record: rec.fields,
          });
        }
      }
    }
    const lastSeq = page.length > 0 ? page[page.length - 1].seq : fromSeq;
    return {
      mode: "incremental",
      changes,
      cursor: `inc:${lastSeq}`,
      hasMore: entries.length > page.length,
      serverTime: "",
    };
  }

  /** A Transport view of this server for one user, used to drive the engine. */
  transportFor(user: string): Transport {
    return {
      push: async (mutations: Mutation[]) => ({
        results: mutations.map((m) => ({ id: m.id, status: "ok" as const, rev: 1, record: {} })),
      }),
      pull: (cursor: string | null, limit: number) => this.pull(user, cursor, limit),
      subscribe: () => () => undefined,
    };
  }

  async liveRecordIds(user: string, collection: string): Promise<string[]> {
    return (await this.store.transaction((tx) => tx.list(this.ns(user, collection)))).map(
      (r) => r.recordId,
    );
  }
}

// ── driver ─────────────────────────────────────────────────────────────────

const here = dirname(fileURLToPath(import.meta.url));
const fixturesDir = resolve(here, "../../../conformance/fixtures");

function loadFixtures(): Fixture[] {
  return readdirSync(fixturesDir)
    .filter((name) => name.endsWith(".json"))
    .map((name) => JSON.parse(readFileSync(join(fixturesDir, name), "utf8")) as Fixture)
    .sort((a, b) => (a.name < b.name ? -1 : 1));
}

const fixtures = loadFixtures();

describe("conformance replay: mock server satisfies every fixture (C-CONF-2)", () => {
  for (const fixture of fixtures) {
    it(fixture.name, async () => {
      const server = new MockServer(fixture.config);
      let prevCursor: string | null = null;

      for (const [stepIndex, step] of fixture.steps.entries()) {
        const context = `step ${stepIndex} (${fixture.name})`;
        if (step.push) {
          const results = await server.push(step.as, step.push.mutations);
          const error = matchExpect(step.expect, { results }, context);
          expect(error).toBeNull();
          continue;
        }
        const pull = step.pull ?? {};
        const cursor = pull.cursor === "$prev" ? prevCursor : (pull.cursor ?? null);
        try {
          const response = await server.pull(step.as, cursor, pull.limit ?? 100);
          prevCursor = response.cursor;
          const error = matchExpect(step.expect, response, context);
          expect(error).toBeNull();
        } catch (err) {
          if (!(err instanceof RequestFailure)) throw err;
          const error = matchExpect(step.expect, { status: err.status, code: err.code }, context);
          expect(error).toBeNull();
        }
      }
    });
  }
});

describe("conformance replay: the TS engine converges with the mock server", () => {
  it("drives a pull for every fixture's first user and matches server state", async () => {
    for (const fixture of fixtures) {
      const server = new MockServer(fixture.config);
      for (const step of fixture.steps) {
        if (step.push) await server.push(step.as, step.push.mutations);
      }
      const user = fixture.users[0];
      const storage = new MemoryStorageAdapter();
      const engine = new SyncEngine({ storage, transport: server.transportFor(user) });

      await engine.pullNow();

      const local = (await storage.transaction((tx) => tx.list("notes")))
        .map((r) => r.recordId)
        .sort();
      const remote = (await server.liveRecordIds(user, "notes")).sort();
      expect({ fixture: fixture.name, local }).toEqual({ fixture: fixture.name, local: remote });
    }
  });
});
