package syncplugin

import (
	"errors"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// This file is the Gate 0 G.4 spike (protocol §17.4 / PDD §8.3).
//
// It proves the transaction semantics of PocketBase v0.40.5 hooks that the
// server design depends on:
//
//  1. A change-log row written inside OnRecordCreateExecute (in-transaction)
//     using the event's tx-scoped App DB rolls back with the data write.
//  2. OnRecordAfterCreateSuccess is deferred until AFTER the outer transaction
//     commits (core/db.go: App.txInfo.OnComplete). It therefore must NOT be used
//     for atomic logging: on rollback it is never called, and on success its
//     writes are a separate post-commit transaction.
//
// Conclusion: the plugin MUST append the change log from the *Execute hooks
// (in-transaction), not from the *AfterSuccess hooks.

func newSpikeApp(t *testing.T) *tests.TestApp {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(app.Cleanup)

	col := core.NewBaseCollection("notes")
	col.Fields.Add(&core.TextField{Name: "title", Required: true})
	if err := app.Save(col); err != nil {
		t.Fatalf("save collection: %v", err)
	}

	if _, err := app.DB().NewQuery(`CREATE TABLE IF NOT EXISTS spike_changes (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		record_id TEXT NOT NULL,
		op TEXT NOT NULL
	)`).Execute(); err != nil {
		t.Fatalf("create spike_changes: %v", err)
	}

	return app
}

func countLogRows(t *testing.T, app core.App) int {
	t.Helper()
	var row struct{ C int }
	if err := app.DB().NewQuery("SELECT COUNT(*) AS c FROM spike_changes").One(&row); err != nil {
		t.Fatalf("count spike_changes: %v", err)
	}
	return row.C
}

func notesCount(t *testing.T, app core.App) int {
	t.Helper()
	n, err := app.CountRecords("notes")
	if err != nil {
		t.Fatalf("count notes: %v", err)
	}
	return int(n)
}

// TestInTransactionHookLogRollsBack proves the atomic logging pattern: a log row
// appended in OnRecordCreateExecute using the tx-scoped DB rolls back with the
// data write.
func TestInTransactionHookLogRollsBack(t *testing.T) {
	app := newSpikeApp(t)

	app.OnRecordCreateExecute("notes").BindFunc(func(e *core.RecordEvent) error {
		if err := e.Next(); err != nil { // runs the actual INSERT
			return err
		}
		_, err := e.App.DB().
			NewQuery("INSERT INTO spike_changes (record_id, op) VALUES ({:id}, 'create')").
			Bind(map[string]any{"id": e.Record.Id}).
			Execute()
		return err
	})

	col, err := app.FindCollectionByNameOrId("notes")
	if err != nil {
		t.Fatal(err)
	}

	// Rollback case: neither the record nor the log row may survive.
	err = app.RunInTransaction(func(txApp core.App) error {
		rec := core.NewRecord(col)
		rec.Set("title", "boom")
		if err := txApp.Save(rec); err != nil {
			return err
		}
		return errors.New("force rollback")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}
	if got := notesCount(t, app); got != 0 {
		t.Fatalf("rolled-back record survived: notes=%d", got)
	}
	if got := countLogRows(t, app); got != 0 {
		t.Fatalf("rolled-back change-log row survived: spike_changes=%d", got)
	}

	// Commit case: both the record and exactly one log row must persist.
	if err := app.RunInTransaction(func(txApp core.App) error {
		rec := core.NewRecord(col)
		rec.Set("title", "ok")
		return txApp.Save(rec)
	}); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
	if got := notesCount(t, app); got != 1 {
		t.Fatalf("committed record missing: notes=%d", got)
	}
	if got := countLogRows(t, app); got != 1 {
		t.Fatalf("committed change-log row missing: spike_changes=%d", got)
	}
}

// TestAfterSuccessHookIsPostCommit proves OnRecordAfterCreateSuccess is
// deferred until after the transaction commits, so it is unsuitable for atomic
// change-log writes.
func TestAfterSuccessHookIsPostCommit(t *testing.T) {
	app := newSpikeApp(t)

	var successCalls int
	app.OnRecordAfterCreateSuccess("notes").BindFunc(func(e *core.RecordEvent) error {
		successCalls++
		// A log write here would be a separate post-commit transaction.
		_, err := e.App.DB().
			NewQuery("INSERT INTO spike_changes (record_id, op) VALUES ({:id}, 'create')").
			Bind(map[string]any{"id": e.Record.Id}).
			Execute()
		return err
	})

	col, err := app.FindCollectionByNameOrId("notes")
	if err != nil {
		t.Fatal(err)
	}

	// Rollback: the success hook must never fire (only the error hook would).
	err = app.RunInTransaction(func(txApp core.App) error {
		rec := core.NewRecord(col)
		rec.Set("title", "boom")
		if err := txApp.Save(rec); err != nil {
			return err
		}
		return errors.New("force rollback")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}
	if successCalls != 0 {
		t.Fatalf("OnRecordAfterCreateSuccess fired inside a rolled-back transaction: calls=%d", successCalls)
	}
	if got := countLogRows(t, app); got != 0 {
		t.Fatalf("log row written from after-success hook survived rollback: %d", got)
	}

	// Commit: the success hook fires once, after commit.
	if err := app.RunInTransaction(func(txApp core.App) error {
		rec := core.NewRecord(col)
		rec.Set("title", "ok")
		return txApp.Save(rec)
	}); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
	if successCalls != 1 {
		t.Fatalf("OnRecordAfterCreateSuccess calls = %d, want 1", successCalls)
	}
}
