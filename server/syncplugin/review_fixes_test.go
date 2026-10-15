package syncplugin

import "testing"

// Regression tests for the uncommitted-review fixes (server side).

// TestLegacyRecordFirstWriteDoesNotReuseRev guards finding 3: a record that
// pre-dates the plugin has no sync history, so the snapshot serves it at rev 1
// (revFor's fallback). Its first tracked write must therefore be rev 2, not a
// reuse of rev 1 that clients would ignore (§8.1).
func TestLegacyRecordFirstWriteDoesNotReuseRev(t *testing.T) {
	env := newTestEnv(t, Config{})
	owner := env.newUser("legacy@example.com")
	rec := env.saveNoteID(owner, "legacyrecord001", "v1", "")

	// Simulate a pre-plugin record: strip its sync history entirely.
	for _, q := range []string{
		"DELETE FROM sync_meta WHERE collection='notes' AND record_id={:r}",
		"DELETE FROM sync_changes WHERE collection='notes' AND record_id={:r}",
	} {
		if _, err := env.app.DB().NewQuery(q).Bind(map[string]any{"r": rec.Id}).Execute(); err != nil {
			t.Fatalf("clear history: %v", err)
		}
	}

	// A snapshot would now advertise this record at rev 1 (revFor fallback).
	found, err := env.app.FindRecordById("notes", rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	found.Set("title", "v2")
	if err := env.app.Save(found); err != nil {
		t.Fatalf("update legacy record: %v", err)
	}

	if got := metaRev(t, env.app, "notes", rec.Id); got != 2 {
		t.Fatalf("first tracked write reused a rev: sync_meta rev = %d, want 2", got)
	}
	if got := lastChange(t, env.app, "notes", rec.Id).Rev; got != 2 {
		t.Fatalf("logged rev = %d, want 2", got)
	}
}
