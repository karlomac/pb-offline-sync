package syncplugin

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// backdateChanges moves sync_changes rows matching where exactly offset into the
// past, simulating an aged log.
func backdateChanges(t *testing.T, env *testEnv, where string, offset string) {
	t.Helper()
	if _, err := env.app.DB().NewQuery(
		"UPDATE sync_changes SET created = strftime('%Y-%m-%d %H:%M:%fZ','now',{:off}) WHERE " + where,
	).Bind(map[string]any{"off": offset}).Execute(); err != nil {
		t.Fatalf("backdate changes: %v", err)
	}
}

// TestPruneMakesOldCursor410 proves the protocol §13 contract: a cursor that was
// valid before a prune returns 410 cursor_too_old afterwards, because pruning
// the oldest rows raises MIN(seq).
func TestPruneMakesOldCursor410(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	for _, id := range []string{"notesrecord0001", "notesrecord0002", "notesrecord0003"} {
		env.saveNoteID(alice, id, "t-"+id, "b")
	}

	cur := url.QueryEscape(encodeCursor(cursor{Kind: cursorKindIncremental, Seq: 1}))

	// Valid before pruning: seq 1 is still within retained history.
	status, body := env.getJSON("/api/sync/pull?cursor="+cur, tok, nil)
	if status != http.StatusOK || body["mode"] != "incremental" {
		t.Fatalf("pre-prune cursor: status=%d mode=%v", status, body["mode"])
	}
	if len(changes(t, body)) != 2 {
		t.Fatalf("pre-prune changes = %#v, want 2", changes(t, body))
	}

	// Age the two oldest changes so the 24h retention prunes them.
	backdateChanges(t, env, "seq <= 2", "-30 days")
	if err := Prune(env.app, Config{ChangeLogRetention: 24 * time.Hour}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := countChanges(t, env.app); got != 1 {
		t.Fatalf("changes after prune = %d, want 1", got)
	}

	// The same cursor is now below the oldest retained seq -> 410.
	status, body = env.getJSON("/api/sync/pull?cursor="+cur, tok, nil)
	if status != http.StatusGone || body["code"] != "cursor_too_old" {
		t.Fatalf("post-prune cursor: status=%d code=%v", status, body["code"])
	}

	// A cursor at the retained head is still served.
	headCur := url.QueryEscape(encodeCursor(cursor{Kind: cursorKindIncremental, Seq: 3}))
	status, body = env.getJSON("/api/sync/pull?cursor="+headCur, tok, nil)
	if status != http.StatusOK || body["mode"] != "incremental" {
		t.Fatalf("head cursor after prune: status=%d mode=%v", status, body["mode"])
	}
}

// TestPruneIdempotentKeepsRecent checks pruning is idempotent and never removes
// changes inside the retention window.
func TestPruneIdempotentKeepsRecent(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	for _, id := range []string{"notesrecord0001", "notesrecord0002"} {
		env.saveNoteID(alice, id, "t-"+id, "b")
	}
	backdateChanges(t, env, "seq = 1", "-30 days")

	cfg := Config{ChangeLogRetention: 24 * time.Hour}
	if err := Prune(env.app, cfg); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := countChanges(t, env.app); got != 1 {
		t.Fatalf("changes after first prune = %d, want 1", got)
	}

	// Idempotent: a second run deletes nothing.
	if err := Prune(env.app, cfg); err != nil {
		t.Fatalf("second Prune: %v", err)
	}
	if got := countChanges(t, env.app); got != 1 {
		t.Fatalf("changes after second prune = %d, want 1", got)
	}
	if got := metaRev(t, env.app, "notes", "notesrecord0002"); got != 1 {
		t.Fatalf("prune changed a retained rev: got %d", got)
	}
}

// TestPruneProcessedRespectsIdempotencyWindow checks sync_processed is pruned by
// age and recent rows survive.
func TestPruneProcessedRespectsIdempotencyWindow(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "one"}))
	env.pushMutations(tok, createMut("m2", "notes", "notesrecord0002", map[string]any{"title": "two"}))

	if _, err := env.app.DB().NewQuery(
		"UPDATE sync_processed SET created = strftime('%Y-%m-%d %H:%M:%fZ','now','-30 days') WHERE mutation_id = 'm1'",
	).Execute(); err != nil {
		t.Fatalf("backdate processed: %v", err)
	}

	if err := Prune(env.app, Config{IdempotencyRetentionDays: 14}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var n int
	if err := env.app.DB().NewQuery("SELECT COUNT(*) FROM sync_processed").Row(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sync_processed rows = %d, want 1 (only the recent one)", n)
	}
	var m string
	if err := env.app.DB().NewQuery("SELECT mutation_id FROM sync_processed").Row(&m); err != nil {
		t.Fatal(err)
	}
	if m != "m2" {
		t.Fatalf("surviving processed row = %q, want m2", m)
	}
}
