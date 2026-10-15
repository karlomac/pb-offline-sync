/**
 * SQLite `StorageAdapter` (PDD §9.2–§9.4).
 *
 * Layout per synced collection: one table with the mapped columns plus
 * `_rev`, `_dirty` and `_extra` (`_extra` holds keys the schema does not
 * declare — e.g. server-authoritative `id`/`created`/`updated` — so records
 * round-trip verbatim). Internal tables: `_outbox`, `_sync_state`, `_conflicts`
 * and `_sync_collections`.
 *
 * `put` implements the §4.1 passthrough rules: a key absent from `fields` is
 * left unchanged, a key present with `null` sets the field empty.
 */
import type {
  ConflictRecord,
  MutationOp,
  OutboxEntry,
  OutboxStatus,
  StorageAdapter,
  StoredRecord,
  SyncState,
  Tx,
} from "@pb-offline-sync/core";
import type { SQLiteDriver, SQLiteTx, SQLiteValue } from "./driver";
import { decodeField, encodeField, localColumnType } from "./mapping";
import type { SchemaField } from "./mapping";

/** Collection name → its storage fields. The host builds this from `/info` (§5). */
export type CollectionSchema = Record<string, SchemaField[]>;

export interface SqliteStorageAdapterOptions {
  driver: SQLiteDriver;
  schema: CollectionSchema;
}

export class SqliteStorageAdapter implements StorageAdapter {
  private readonly driver: SQLiteDriver;
  private readonly schema: CollectionSchema;
  private readyPromise: Promise<void> | null = null;

  constructor(opts: SqliteStorageAdapterOptions) {
    this.driver = opts.driver;
    this.schema = opts.schema;
  }

  transaction<T>(fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.ensureReady().then(() => this.driver.transaction((db) => fn(this.makeTx(db))));
  }

  close(): Promise<void> {
    return this.driver.close();
  }

  // ── schema setup (committed once, never inside a caller transaction) ──────

  private ensureReady(): Promise<void> {
    this.readyPromise ??= this.driver.transaction((db) => this.migrate(db));
    return this.readyPromise;
  }

  private async migrate(db: SQLiteTx): Promise<void> {
    await db.run(
      `CREATE TABLE IF NOT EXISTS _sync_state(_id INTEGER PRIMARY KEY CHECK(_id=1), cursor TEXT, last_sync_at INTEGER, last_error TEXT)`,
    );
    await db.run(
      `CREATE TABLE IF NOT EXISTS _outbox(mutation_id TEXT PRIMARY KEY, collection TEXT NOT NULL, record_id TEXT NOT NULL, ` +
        `op TEXT NOT NULL, base_rev INTEGER, fields_json TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER NOT NULL, ` +
        `created_at INTEGER, _ord INTEGER NOT NULL)`,
    );
    await db.run(
      `CREATE TABLE IF NOT EXISTS _conflicts(id TEXT PRIMARY KEY, collection TEXT NOT NULL, record_id TEXT NOT NULL, ` +
        `code TEXT NOT NULL, fields_json TEXT NOT NULL, message TEXT)`,
    );
    await db.run(`CREATE TABLE IF NOT EXISTS _sync_collections(name TEXT PRIMARY KEY)`);
    await db.run(`INSERT OR IGNORE INTO _sync_state(_id, cursor, last_sync_at, last_error) VALUES(1, NULL, NULL, NULL)`);
    for (const [name, fields] of Object.entries(this.schema)) await this.createCollection(db, name, fields);
  }

  private async createCollection(db: SQLiteTx, name: string, fields: SchemaField[]): Promise<void> {
    const body = [
      "_id TEXT PRIMARY KEY",
      "_rev INTEGER NOT NULL DEFAULT 0",
      "_dirty INTEGER NOT NULL DEFAULT 0",
      "_extra TEXT NOT NULL DEFAULT '{}'",
      ...fields.map((f) => `${q(f.name)} ${localColumnType(f)}`),
    ];
    await db.run(`CREATE TABLE IF NOT EXISTS ${q(name)}(${body.join(", ")})`);
    await db.run(`INSERT OR IGNORE INTO _sync_collections(name) VALUES(?)`, [name]);
  }

  // ── Tx ────────────────────────────────────────────────────────────────────

  private makeTx(db: SQLiteTx): Tx {
    return {
      get: (collection, recordId) => this.get(db, collection, recordId),
      put: (collection, recordId, record) => this.put(db, collection, recordId, record),
      delete: (collection, recordId) => this.delete(db, collection, recordId),
      list: (collection) => this.list(db, collection),
      collections: () => this.collections(db),
      outboxList: () => this.outboxList(db),
      outboxPut: (entry) => this.outboxPut(db, entry),
      outboxDelete: (mutationId) => this.outboxDelete(db, mutationId),
      syncStateGet: () => this.syncStateGet(db),
      syncStatePut: (state) => this.syncStatePut(db, state),
      conflictAdd: (conflict) => this.conflictAdd(db, conflict),
      conflictList: () => this.conflictList(db),
      conflictDelete: (id) => this.conflictDelete(db, id),
    };
  }

  private fields(collection: string): SchemaField[] {
    const fields = this.schema[collection];
    if (!fields) throw new Error(`collection not configured for sync: ${collection}`);
    return fields;
  }

  private async get(db: SQLiteTx, collection: string, recordId: string): Promise<StoredRecord | null> {
    const fields = this.fields(collection);
    const row = (await db.all(`SELECT * FROM ${q(collection)} WHERE _id = ?`, [recordId]))[0];
    if (!row) return null;
    return { rev: int(row._rev), fields: this.rowToFields(fields, row) };
  }

  private rowToFields(fields: SchemaField[], row: Record<string, SQLiteValue>): Record<string, unknown> {
    const out: Record<string, unknown> = { ...parseObject(row._extra) };
    for (const f of fields) out[f.name] = decodeField(f, row[f.name]);
    return out;
  }

  private async put(
    db: SQLiteTx,
    collection: string,
    recordId: string,
    record: StoredRecord,
  ): Promise<void> {
    const schema = this.fields(collection);
    const incoming = record.fields ?? {};
    const schemaNames = new Set(schema.map((f) => f.name));

    const colNames: string[] = [];
    const colVals: SQLiteValue[] = [];
    for (const f of schema) {
      if (hasOwn(incoming, f.name) && incoming[f.name] !== undefined) {
        colNames.push(f.name);
        colVals.push(encodeField(f, incoming[f.name]));
      }
    }
    const extraPatch: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(incoming)) {
      if (!schemaNames.has(key)) extraPatch[key] = value;
    }

    const existing = (await db.all(`SELECT _extra FROM ${q(collection)} WHERE _id = ?`, [recordId]))[0];
    const extra = { ...parseObject(existing?._extra), ...extraPatch };

    if (existing) {
      // Absent keys are left unchanged (§4.1).
      const assigns = ["_rev = ?", "_extra = ?"];
      const params: SQLiteValue[] = [record.rev, JSON.stringify(extra)];
      for (let i = 0; i < colNames.length; i++) {
        assigns.push(`${q(colNames[i])} = ?`);
        params.push(colVals[i]);
      }
      params.push(recordId);
      await db.run(`UPDATE ${q(collection)} SET ${assigns.join(", ")} WHERE _id = ?`, params);
    } else {
      const cols = ["_id", "_rev", "_extra", ...colNames];
      const vals: SQLiteValue[] = [recordId, record.rev, JSON.stringify(extra), ...colVals];
      await db.run(
        `INSERT INTO ${q(collection)}(${cols.map(q).join(", ")}) VALUES(${cols.map(() => "?").join(", ")})`,
        vals,
      );
    }
  }

  private async delete(db: SQLiteTx, collection: string, recordId: string): Promise<void> {
    this.fields(collection);
    await db.run(`DELETE FROM ${q(collection)} WHERE _id = ?`, [recordId]);
  }

  private async list(
    db: SQLiteTx,
    collection: string,
  ): Promise<Array<{ recordId: string; record: StoredRecord }>> {
    const schema = this.fields(collection);
    const rows = await db.all(`SELECT * FROM ${q(collection)}`);
    return rows.map((row) => ({
      recordId: String(row._id),
      record: { rev: int(row._rev), fields: this.rowToFields(schema, row) },
    }));
  }

  private async collections(db: SQLiteTx): Promise<string[]> {
    const rows = await db.all(`SELECT name FROM _sync_collections ORDER BY name`);
    return rows.map((row) => String(row.name));
  }

  // ── outbox (insertion order is authoritative; `_ord` preserves it) ────────

  private async outboxList(db: SQLiteTx): Promise<OutboxEntry[]> {
    const rows = await db.all(`SELECT * FROM _outbox ORDER BY _ord ASC`);
    return rows.map(rowToOutbox);
  }

  private async outboxPut(db: SQLiteTx, entry: OutboxEntry): Promise<void> {
    const existing = (await db.all(`SELECT _ord FROM _outbox WHERE mutation_id = ?`, [entry.mutationId]))[0];
    const params: SQLiteValue[] = [
      entry.collection,
      entry.recordId,
      entry.op,
      entry.baseRev,
      JSON.stringify(entry.fields ?? {}),
      entry.status,
      entry.attempts,
      entry.mutationId,
    ];
    if (existing) {
      await db.run(
        `UPDATE _outbox SET collection = ?, record_id = ?, op = ?, base_rev = ?, fields_json = ?, ` +
          `status = ?, attempts = ? WHERE mutation_id = ?`,
        params,
      );
    } else {
      await db.run(
        `INSERT INTO _outbox(mutation_id, collection, record_id, op, base_rev, fields_json, status, attempts, ` +
          `created_at, _ord) VALUES(?, ?, ?, ?, ?, ?, ?, ?, NULL, (SELECT COALESCE(MAX(_ord), 0) + 1 FROM _outbox))`,
        [entry.mutationId, ...params.slice(0, 7)],
      );
    }
    await this.refreshDirty(db, entry.collection, entry.recordId);
  }

  private async outboxDelete(db: SQLiteTx, mutationId: string): Promise<void> {
    const row = (await db.all(`SELECT collection, record_id FROM _outbox WHERE mutation_id = ?`, [mutationId]))[0];
    await db.run(`DELETE FROM _outbox WHERE mutation_id = ?`, [mutationId]);
    if (row) await this.refreshDirty(db, String(row.collection), String(row.record_id));
  }

  // ── sync state / conflicts ────────────────────────────────────────────────

  private async syncStateGet(db: SQLiteTx): Promise<SyncState> {
    const row = (await db.all(`SELECT cursor, last_sync_at, last_error FROM _sync_state WHERE _id = 1`))[0];
    return {
      cursor: textOrNull(row?.cursor),
      lastSyncAt: row?.last_sync_at == null ? null : int(row.last_sync_at),
      lastError: textOrNull(row?.last_error),
    };
  }

  private async syncStatePut(db: SQLiteTx, state: SyncState): Promise<void> {
    await db.run(`UPDATE _sync_state SET cursor = ?, last_sync_at = ?, last_error = ? WHERE _id = 1`, [
      state.cursor,
      state.lastSyncAt,
      state.lastError,
    ]);
  }

  private async conflictAdd(db: SQLiteTx, conflict: ConflictRecord): Promise<void> {
    await db.run(
      `INSERT OR REPLACE INTO _conflicts(id, collection, record_id, code, fields_json, message) VALUES(?, ?, ?, ?, ?, ?)`,
      [
        conflict.id,
        conflict.collection,
        conflict.recordId,
        conflict.code,
        JSON.stringify(conflict.fields ?? {}),
        conflict.message ?? null,
      ],
    );
  }

  private async conflictList(db: SQLiteTx): Promise<ConflictRecord[]> {
    const rows = await db.all(`SELECT * FROM _conflicts ORDER BY rowid ASC`);
    return rows.map((row) => ({
      id: String(row.id),
      collection: String(row.collection),
      recordId: String(row.record_id),
      code: String(row.code),
      fields: parseObject(row.fields_json),
      ...(row.message == null ? {} : { message: String(row.message) }),
    }));
  }

  private async conflictDelete(db: SQLiteTx, id: string): Promise<void> {
    await db.run(`DELETE FROM _conflicts WHERE id = ?`, [id]);
  }

  /**
   * `_dirty` (PDD §9.3) is denormalized: 1 while the record has any outbox
   * entry. Correctness never reads it — the outbox is the source of truth.
   */
  private async refreshDirty(db: SQLiteTx, collection: string, recordId: string): Promise<void> {
    const table = (await db.all(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, [collection]))[0];
    if (!table) return;
    const row = (
      await db.all(`SELECT COUNT(*) AS c FROM _outbox WHERE collection = ? AND record_id = ?`, [collection, recordId])
    )[0];
    await db.run(`UPDATE ${q(collection)} SET _dirty = ? WHERE _id = ?`, [int(row?.c) > 0 ? 1 : 0, recordId]);
  }
}

function q(identifier: string): string {
  return `"${identifier.replace(/"/g, '""')}"`;
}

function hasOwn(obj: Record<string, unknown>, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(obj, key);
}

function parseObject(value: unknown): Record<string, unknown> {
  if (typeof value !== "string" || value === "") return {};
  try {
    const parsed: unknown = JSON.parse(value);
    return parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

function textOrNull(value: unknown): string | null {
  return value == null ? null : String(value);
}

function int(value: unknown, fallback = 0): number {
  if (value == null) return fallback;
  const n = typeof value === "number" ? value : Number(value);
  return Number.isFinite(n) ? n : fallback;
}

function rowToOutbox(row: Record<string, SQLiteValue>): OutboxEntry {
  return {
    mutationId: String(row.mutation_id),
    collection: String(row.collection),
    recordId: String(row.record_id),
    op: String(row.op) as MutationOp,
    baseRev: row.base_rev == null ? null : int(row.base_rev),
    fields: parseObject(row.fields_json),
    status: String(row.status) as OutboxStatus,
    attempts: int(row.attempts),
  };
}
