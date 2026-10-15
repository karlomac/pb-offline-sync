# @pb-offline-sync/adapter-expo-sqlite

First storage adapter for `@pb-offline-sync/core`: an Expo SQLite implementation
of the `StorageAdapter` interface plus the PocketBase `Transport` (auth + SSE
nudge). Implements the local table layout and schema/type mapping in
`docs/pb-offline-sync-PDD.md` §9.3–§9.4.

Lands in **M5**. Depends on M4 (`@pb-offline-sync/core`).
