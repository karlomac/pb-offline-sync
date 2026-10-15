package syncplugin

import "testing"

// TestListUnresolvedConflicts records a serverWins field conflict and checks the
// inspection helper returns it, filters by record, and supports wildcards.
func TestListUnresolvedConflicts(t *testing.T) {
	env, tok := newStaleBaseEnv(t, ServerWins)

	_, resp := env.pushMutations(tok,
		updateMut("m3", "notes", "notesrecord0001", 1, map[string]any{"title": "t3"}),
	)
	if r := resultAt(t, resp, 0); r["status"] != statusConflict {
		t.Fatalf("expected a recorded conflict, got %#v", r)
	}

	got, err := ListUnresolvedConflicts(env.app, "notes", "notesrecord0001")
	if err != nil {
		t.Fatalf("ListUnresolvedConflicts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("conflicts for record = %d, want 1", len(got))
	}
	c := got[0]
	if c.Collection != "notes" || c.RecordID != "notesrecord0001" || c.MutationID != "m3" || c.WinningRev != 2 {
		t.Fatalf("conflict = %#v", c)
	}
	if c.Losing["title"] != "t3" {
		t.Fatalf("losing fields = %#v, want title=t3", c.Losing)
	}

	if other, err := ListUnresolvedConflicts(env.app, "notes", "notesrecord0002"); err != nil || len(other) != 0 {
		t.Fatalf("other record conflicts = %#v err=%v, want none", other, err)
	}

	all, err := ListUnresolvedConflicts(env.app, "", "")
	if err != nil || len(all) != 1 {
		t.Fatalf("wildcard conflicts = %d err=%v, want 1", len(all), err)
	}
}
