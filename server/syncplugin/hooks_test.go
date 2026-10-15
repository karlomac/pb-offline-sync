package syncplugin

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func decodeChanged(t *testing.T, raw string) []string {
	t.Helper()
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode changed_fields %q: %v", raw, err)
	}
	return out
}

// TestRevIncrementsAndChangedFields checks create=1, update=2, update=3 and the
// changed-field diff against the record's original state (protocol §8.1, §8.2).
func TestRevIncrementsAndChangedFields(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")

	rec := env.saveNote(alice, "t1", "b1")

	meta := lastChange(t, env.app, "notes", rec.Id)
	if meta.Op != opCreate || meta.Rev != 1 {
		t.Fatalf("create change: op=%q rev=%d, want create/1", meta.Op, meta.Rev)
	}
	wantAll := []string{"id", "title", "body", "attachments", "owner"}
	if got := decodeChanged(t, meta.ChangedFields); !reflect.DeepEqual(got, wantAll) {
		t.Fatalf("create changed_fields = %v, want %v", got, wantAll)
	}
	if got := metaRev(t, env.app, "notes", rec.Id); got != 1 {
		t.Fatalf("rev after create = %d, want 1", got)
	}

	fetched, err := env.app.FindRecordById("notes", rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	fetched.Set("title", "t2")
	if err := env.app.Save(fetched); err != nil {
		t.Fatal(err)
	}
	ch := lastChange(t, env.app, "notes", rec.Id)
	if ch.Op != opUpdate || ch.Rev != 2 {
		t.Fatalf("update1 change: op=%q rev=%d, want update/2", ch.Op, ch.Rev)
	}
	if got := decodeChanged(t, ch.ChangedFields); !reflect.DeepEqual(got, []string{"title"}) {
		t.Fatalf("update1 changed_fields = %v, want [title]", got)
	}
	if got := metaRev(t, env.app, "notes", rec.Id); got != 2 {
		t.Fatalf("rev after update1 = %d, want 2", got)
	}

	fetched, err = env.app.FindRecordById("notes", rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	fetched.Set("body", "b2")
	if err := env.app.Save(fetched); err != nil {
		t.Fatal(err)
	}
	ch = lastChange(t, env.app, "notes", rec.Id)
	if ch.Op != opUpdate || ch.Rev != 3 {
		t.Fatalf("update2 change: op=%q rev=%d, want update/3", ch.Op, ch.Rev)
	}
	if got := decodeChanged(t, ch.ChangedFields); !reflect.DeepEqual(got, []string{"body"}) {
		t.Fatalf("update2 changed_fields = %v, want [body]", got)
	}
	if got := metaRev(t, env.app, "notes", rec.Id); got != 3 {
		t.Fatalf("rev after update2 = %d, want 3", got)
	}
}

// TestDeleteIncrementsAndRetainsTombstone checks a delete is a write with a final
// rev and that sync_meta is retained (protocol §8.1, §17.1.1).
func TestDeleteIncrementsAndRetainsTombstone(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	rec := env.saveNote(alice, "t1", "b1")

	fetched, err := env.app.FindRecordById("notes", rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.app.Delete(fetched); err != nil {
		t.Fatal(err)
	}

	ch := lastChange(t, env.app, "notes", rec.Id)
	if ch.Op != opDelete || ch.Rev != 2 {
		t.Fatalf("delete change: op=%q rev=%d, want delete/2", ch.Op, ch.Rev)
	}
	if got := metaRev(t, env.app, "notes", rec.Id); got != 2 {
		t.Fatalf("tombstone meta rev = %d, want 2", got)
	}
	if _, err := env.app.FindRecordById("notes", rec.Id); err == nil {
		t.Fatal("record still exists after delete")
	}
}

// TestRollbackAtomicityUpdateAndDelete proves no sync_meta/change-log write
// survives a forced rollback of an update or a delete (protocol §8.5).
func TestRollbackAtomicityUpdateAndDelete(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	rec := env.saveNote(alice, "original", "b1")

	revBefore := metaRev(t, env.app, "notes", rec.Id)
	countBefore := countChanges(t, env.app)

	// Update rollback.
	var insideRev, insideCount int
	err := env.app.RunInTransaction(func(txApp core.App) error {
		fetched, err := txApp.FindRecordById("notes", rec.Id)
		if err != nil {
			return err
		}
		fetched.Set("title", "boom")
		if err := txApp.Save(fetched); err != nil {
			return err
		}
		insideRev = metaRev(t, txApp, "notes", rec.Id)
		insideCount = countChanges(t, txApp)
		return errors.New("force rollback")
	})
	if err == nil {
		t.Fatal("expected update transaction to fail")
	}
	if insideRev != revBefore+1 || insideCount != countBefore+1 {
		t.Fatalf("hook did not run in-tx: rev=%d count=%d", insideRev, insideCount)
	}
	if got := metaRev(t, env.app, "notes", rec.Id); got != revBefore {
		t.Fatalf("update rollback changed meta rev: got %d want %d", got, revBefore)
	}
	if got := countChanges(t, env.app); got != countBefore {
		t.Fatalf("update rollback left a change row: got %d want %d", got, countBefore)
	}
	after, err := env.app.FindRecordById("notes", rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	if after.GetString("title") != "original" {
		t.Fatalf("update rollback changed the record: title=%q", after.GetString("title"))
	}

	// Delete rollback.
	err = env.app.RunInTransaction(func(txApp core.App) error {
		fetched, err := txApp.FindRecordById("notes", rec.Id)
		if err != nil {
			return err
		}
		if err := txApp.Delete(fetched); err != nil {
			return err
		}
		insideRev = metaRev(t, txApp, "notes", rec.Id)
		insideCount = countChanges(t, txApp)
		return errors.New("force rollback")
	})
	if err == nil {
		t.Fatal("expected delete transaction to fail")
	}
	if insideRev != revBefore+1 || insideCount != countBefore+1 {
		t.Fatalf("delete hook did not run in-tx: rev=%d count=%d", insideRev, insideCount)
	}
	if got := metaRev(t, env.app, "notes", rec.Id); got != revBefore {
		t.Fatalf("delete rollback changed meta rev: got %d want %d", got, revBefore)
	}
	if got := countChanges(t, env.app); got != countBefore {
		t.Fatalf("delete rollback left a change row: got %d want %d", got, countBefore)
	}
	if _, err := env.app.FindRecordById("notes", rec.Id); err != nil {
		t.Fatalf("delete rollback lost the record: %v", err)
	}
}
