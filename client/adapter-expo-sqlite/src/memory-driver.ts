/**
 * In-memory `SQLiteDriver` for Node tests/CI. It is a real in-memory SQLite
 * database via Node's built-in `node:sqlite` — no dependency — so the adapter's
 * SQL, transactions and rollback semantics are exercised exactly as on device.
 *
 * Not for production: `node:sqlite` is not available in React Native. On device
 * use `openExpoDriver` (`expo.ts`).
 */
import { createRequire } from "node:module";
import type { SQLiteDriver, SQLiteTx, SQLiteValue } from "./driver";

// Node's `builtinModules` omits `sqlite`, so Vite 5 does not treat `node:sqlite`
// as external and tries to bundle it. Load it via createRequire to keep it out
// of the bundler graph.
// shortcut: replace with a plain `import` once Vite recognises `node:sqlite`.
const { DatabaseSync } = createRequire(import.meta.url)("node:sqlite") as typeof import("node:sqlite");

export class MemorySQLiteDriver implements SQLiteDriver {
  private readonly db = new DatabaseSync(":memory:");
  private queue: Promise<unknown> = Promise.resolve();
  private closed = false;

  transaction<T>(fn: (tx: SQLiteTx) => Promise<T>): Promise<T> {
    const run = (): Promise<T> => {
      if (this.closed) return Promise.reject(new Error("driver closed"));
      this.db.exec("BEGIN IMMEDIATE");
      return Promise.resolve()
        .then(() => fn(this.tx()))
        .then(
          (result) => {
            this.db.exec("COMMIT");
            return result;
          },
          (err: unknown) => {
            this.db.exec("ROLLBACK");
            throw err;
          },
        );
    };
    // Serialize: a single in-memory connection cannot nest transactions.
    const result = this.queue.then(run, run);
    this.queue = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  }

  private tx(): SQLiteTx {
    return {
      run: async (sql, params) => {
        this.db.prepare(sql).run(...(params ?? []));
      },
      all: async (sql, params) =>
        this.db.prepare(sql).all(...(params ?? [])) as unknown as Array<Record<string, SQLiteValue>>,
    };
  }

  async close(): Promise<void> {
    this.closed = true;
    this.db.close();
  }
}
