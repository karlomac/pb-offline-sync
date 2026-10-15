package syncplugin

import (
	"sync"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

// registerMigrations registers the plugin schema migration exactly once per
// process (the migration list is global and keyed by file name).
var registerMigrationsOnce sync.Once

func registerMigrations() {
	registerMigrationsOnce.Do(func() {
		migrations.Register(createSyncSchema, dropSyncSchema)
		// Second, distinct migration (its own file key) that adds sync_changes.created
		// to databases created by an earlier M1/M2 build. Named so it sorts after
		// migrate.go, i.e. after createSyncSchema on a fresh database.
		migrations.Register(addChangeLogCreated, nil, "sync_changelog_created.go")
		// Third migration (own file key, sorts after the created column exists)
		// adding indexes the change-log queries rely on. IF NOT EXISTS, so it is
		// safe on both fresh and upgraded databases.
		migrations.Register(addSyncIndexes, dropSyncIndexes, "sync_indexes.go")
	})
}

// syncMetaTable: (collection, record_id) -> rev, owner. Rows for deleted
// records are retained as tombstones so a tombstone rev can be answered
// (protocol §17.1.1).
const syncMetaTable = `CREATE TABLE IF NOT EXISTS sync_meta (
	collection TEXT NOT NULL,
	record_id  TEXT NOT NULL,
	rev        INTEGER NOT NULL,
	owner      TEXT NOT NULL,
	PRIMARY KEY (collection, record_id)
)`

// syncChangesTable: append-only, seq = commit order (SQLite single writer).
// created is the server clock used only for retention pruning; it is not part
// of the wire format and is never used for ordering (seq is).
const syncChangesTable = `CREATE TABLE IF NOT EXISTS sync_changes (
	seq            INTEGER PRIMARY KEY AUTOINCREMENT,
	collection     TEXT NOT NULL,
	record_id      TEXT NOT NULL,
	op             TEXT NOT NULL,
	owner          TEXT NOT NULL,
	rev            INTEGER NOT NULL,
	changed_fields TEXT NOT NULL DEFAULT '[]',
	created        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%fZ'))
)`

const syncChangesOwnerSeqIndex = `CREATE INDEX IF NOT EXISTS idx_sync_changes_owner_seq ON sync_changes (owner, seq)`

const syncProcessedTable = `CREATE TABLE IF NOT EXISTS sync_processed (
	user         TEXT NOT NULL,
	mutation_id  TEXT NOT NULL,
	payload_hash TEXT NOT NULL DEFAULT '',
	result       TEXT NOT NULL DEFAULT '',
	created      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%fZ')),
	PRIMARY KEY (user, mutation_id)
)`

const syncConflictsTable = `CREATE TABLE IF NOT EXISTS sync_conflicts (
	id            TEXT PRIMARY KEY DEFAULT ('r'||lower(hex(randomblob(7)))),
	collection    TEXT NOT NULL,
	record_id     TEXT NOT NULL,
	mutation_id   TEXT NOT NULL DEFAULT '',
	losing_fields TEXT NOT NULL DEFAULT '{}',
	winning_rev   INTEGER NOT NULL DEFAULT 0,
	created       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%fZ')),
	resolved      BOOLEAN NOT NULL DEFAULT FALSE
)`

var syncSchemaStatements = []string{
	syncMetaTable,
	syncChangesTable,
	syncChangesOwnerSeqIndex,
	syncProcessedTable,
	syncConflictsTable,
}

func createSyncSchema(app core.App) error {
	for _, q := range syncSchemaStatements {
		if _, err := app.DB().NewQuery(q).Execute(); err != nil {
			return err
		}
	}
	return nil
}

func dropSyncSchema(app core.App) error {
	for _, t := range []string{"sync_conflicts", "sync_processed", "sync_changes", "sync_meta"} {
		if _, err := app.DB().DropTable(t).Execute(); err != nil {
			return err
		}
	}
	return nil
}

// Indexes added after the base schema: the stale-base changed-field lookup
// (collection, record_id, rev), and retention scans by created. Without
// idx_sync_changes_coll_rec_rev, changedUnion and revFor's log fallback
// full-scan sync_changes; without the created indexes, Prune re-scans per batch.
var syncIndexStatements = []string{
	`CREATE INDEX IF NOT EXISTS idx_sync_changes_coll_rec_rev ON sync_changes (collection, record_id, rev)`,
	`CREATE INDEX IF NOT EXISTS idx_sync_changes_created ON sync_changes (created)`,
	`CREATE INDEX IF NOT EXISTS idx_sync_processed_created ON sync_processed (created)`,
}

var syncIndexNames = []string{
	"idx_sync_changes_coll_rec_rev",
	"idx_sync_changes_created",
	"idx_sync_processed_created",
}

func addSyncIndexes(app core.App) error {
	for _, q := range syncIndexStatements {
		if _, err := app.DB().NewQuery(q).Execute(); err != nil {
			return err
		}
	}
	return nil
}

func dropSyncIndexes(app core.App) error {
	for _, n := range syncIndexNames {
		if _, err := app.DB().NewQuery("DROP INDEX IF EXISTS " + n).Execute(); err != nil {
			return err
		}
	}
	return nil
}
