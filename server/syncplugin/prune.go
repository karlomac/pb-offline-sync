package syncplugin

import (
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// changeLogTimeFormat matches SQLite's strftime('%Y-%m-%d %H:%M:%fZ', ...) used
// for sync_changes.created / sync_processed.created, so string comparison is
// chronological.
const changeLogTimeFormat = "2006-01-02 15:04:05.000Z"

// pruneBatch bounds how many rows a single DELETE removes, so pruning never
// holds a long write lock.
const pruneBatch = 500

// addChangeLogCreated adds sync_changes.created on databases created before M3
// (SQLite cannot add a column with a non-constant default). It then backfills
// any empty created values in bounded batches, so it both fills the column on an
// upgrade and repairs rows written with the column's ” default on any later run
// (an empty value sorts before every timestamp and would otherwise be deleted by
// the next Prune). Idempotent.
func addChangeLogCreated(app core.App) error {
	cols, err := app.TableColumns("sync_changes")
	if err != nil {
		return err
	}
	has := false
	for _, c := range cols {
		if c == "created" {
			has = true
			break
		}
	}
	if !has {
		if _, err := app.DB().NewQuery(
			"ALTER TABLE sync_changes ADD COLUMN created TEXT NOT NULL DEFAULT ''",
		).Execute(); err != nil {
			return err
		}
	}

	for {
		res, err := app.DB().NewQuery(`UPDATE sync_changes
			SET created = strftime('%Y-%m-%d %H:%M:%fZ')
			WHERE rowid IN (SELECT rowid FROM sync_changes WHERE created = '' LIMIT {:n})`).
			Bind(map[string]any{"n": pruneBatch}).Execute()
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n < int64(pruneBatch) {
			return nil
		}
	}
}

// Prune deletes sync_changes rows older than Config.ChangeLogRetention and
// sync_processed rows older than Config.IdempotencyRetentionDays, in bounded
// batches. It is idempotent and safe to call from a cron/hook or CLI; it starts
// no goroutine of its own.
//
// Pruning the oldest changes raises MIN(seq), so the pull handler's
// cursor<oldest check (pull.go handlePull) returns 410 cursor_too_old for any
// cursor that pointed at them (protocol §13). Pruned history also makes
// changedUnion report incomplete coverage, so stale-base merges fall back to
// "all fields" rather than "no changes" (§8.2.3.2).
func Prune(app core.App, cfg Config) error {
	cfg.applyDefaults()
	now := time.Now().UTC()

	if _, err := deleteOlderThan(app, "sync_changes", now.Add(-cfg.ChangeLogRetention)); err != nil {
		return err
	}
	if _, err := deleteOlderThan(app, "sync_processed", now.AddDate(0, 0, -cfg.IdempotencyRetentionDays)); err != nil {
		return err
	}
	return nil
}

// deleteOlderThan removes rows with created < cutoff in pruneBatch-sized
// batches by rowid (supported by all SQLite builds, unlike DELETE ... LIMIT).
func deleteOlderThan(app core.App, table string, cutoff time.Time) (int64, error) {
	cutoffStr := cutoff.UTC().Format(changeLogTimeFormat)
	var total int64
	for {
		res, err := app.DB().NewQuery(fmt.Sprintf(
			"DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE created < {:cutoff} LIMIT {:n})",
			table, table,
		)).Bind(map[string]any{"cutoff": cutoffStr, "n": pruneBatch}).Execute()
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < pruneBatch {
			return total, nil
		}
	}
}
