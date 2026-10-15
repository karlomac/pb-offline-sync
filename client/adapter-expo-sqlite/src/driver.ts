/**
 * The only seam through which SQLite is touched. `expo.ts` binds it to
 * `expo-sqlite`; `memory-driver.ts` backs it with Node's built-in `node:sqlite`
 * for tests. The adapter emits plain SQL with `?` placeholders.
 */

export type SQLiteValue = string | number | null | Uint8Array;

export interface SQLiteTx {
  run(sql: string, params?: SQLiteValue[]): Promise<void>;
  all(sql: string, params?: SQLiteValue[]): Promise<Array<Record<string, SQLiteValue>>>;
}

export interface SQLiteDriver {
  /** Runs `fn` atomically in one SQLite transaction; a throw rolls back. */
  transaction<T>(fn: (tx: SQLiteTx) => Promise<T>): Promise<T>;
  close(): Promise<void>;
}
