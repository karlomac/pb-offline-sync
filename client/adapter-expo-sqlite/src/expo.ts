/**
 * `expo-sqlite` binding — the only module in this package that names the Expo
 * API. `expo-sqlite` is an optional peer dependency, so it is imported
 * dynamically through a non-literal specifier: this package typechecks and its
 * tests run under Node without it installed.
 */
import type { SQLiteDriver, SQLiteTx, SQLiteValue } from "./driver";

interface ExpoTransaction {
  runAsync(sql: string, ...params: SQLiteValue[]): Promise<unknown>;
  getAllAsync(sql: string, ...params: SQLiteValue[]): Promise<Array<Record<string, SQLiteValue>>>;
}

interface ExpoDatabase {
  withExclusiveTransactionAsync(fn: (tx: ExpoTransaction) => Promise<unknown>): Promise<unknown>;
  closeAsync(): Promise<void>;
}

interface ExpoSQLiteModule {
  openDatabaseAsync(name: string): Promise<ExpoDatabase>;
}

/** Opens (or creates) an Expo SQLite database and adapts it to `SQLiteDriver`. */
export async function openExpoDriver(name: string): Promise<SQLiteDriver> {
  const specifier: string = "expo-sqlite";
  const SQLite = (await import(specifier)) as ExpoSQLiteModule;
  const db = await SQLite.openDatabaseAsync(name);

  return {
    async transaction<T>(fn: (tx: SQLiteTx) => Promise<T>): Promise<T> {
      return db.withExclusiveTransactionAsync(async (txn) => {
        const tx: SQLiteTx = {
          run: async (sql, params) => {
            await txn.runAsync(sql, ...(params ?? []));
          },
          all: async (sql, params) => txn.getAllAsync(sql, ...(params ?? [])),
        };
        return fn(tx);
      }) as Promise<T>;
    },
    async close() {
      await db.closeAsync();
    },
  };
}
