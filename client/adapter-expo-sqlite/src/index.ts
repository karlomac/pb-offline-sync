/**
 * @pb-offline-sync/adapter-expo-sqlite
 *
 * SQLite `StorageAdapter` (PDD §9.2–§9.3), the PocketBase schema/type mapping
 * (§9.4), an `expo-sqlite` driver binding, and a `node:sqlite` in-memory driver
 * for Node tests.
 */
export { SqliteStorageAdapter } from "./adapter";
export type { CollectionSchema, SqliteStorageAdapterOptions } from "./adapter";
export { MemorySQLiteDriver } from "./memory-driver";
export { openExpoDriver } from "./expo";
export { decodeField, encodeField, isJsonField, localColumnType } from "./mapping";
export type { PbFieldType, SchemaField, SQLiteColumnType } from "./mapping";
export type { SQLiteDriver, SQLiteTx, SQLiteValue } from "./driver";
