package syncplugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

// ---- S-HOOK-3: no-op update (§8.1 "MAY skip") ----

// TestSrvScenNoOpUpdate saves a record with identical client field values and
// asserts the update is treated as a no-op: no rev bump and no new log row.
// The autodate `updated` field always changes on save, so the changed-field set
// must exclude server-managed system fields for this to hold (§8.1 "MAY skip").
func TestSrvScenNoOpUpdate(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")

	rec := env.saveNote(alice, "t1", "b1")
	revBefore := metaRev(t, env.app, "notes", rec.Id)
	countBefore := countChanges(t, env.app)

	fetched, err := env.app.FindRecordById("notes", rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	// Identical client field values; only the autodate `updated` differs.
	if err := env.app.Save(fetched); err != nil {
		t.Fatal(err)
	}

	if got := metaRev(t, env.app, "notes", rec.Id); got != revBefore {
		t.Fatalf("no-op update bumped rev: got %d want %d", got, revBefore)
	}
	if got := countChanges(t, env.app); got != countBefore {
		t.Fatalf("no-op update wrote a change row: got %d want %d", got, countBefore)
	}
	// A real field change after the no-op still bumps the rev and logs.
	fetched, err = env.app.FindRecordById("notes", rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	fetched.Set("title", "t2")
	if err := env.app.Save(fetched); err != nil {
		t.Fatal(err)
	}
	if got := metaRev(t, env.app, "notes", rec.Id); got != revBefore+1 {
		t.Fatalf("real update rev = %d, want %d", got, revBefore+1)
	}
}

// ---- S-MIG-3 / S-PRUNE-4: upgrade path for sync_changes.created ----

// TestSrvScenUpgradeAddChangeLogCreated builds a pre-M3 sync_changes (no created
// column) with rows, runs addChangeLogCreated directly, and asserts the column
// is added, every row backfilled with a non-empty timestamp, a second run is a
// no-op, and a row inserted with created = ” is repaired by a later run
// (protocol §13).
func TestSrvScenUpgradeAddChangeLogCreated(t *testing.T) {
	env := newTestEnv(t, Config{})

	// Replace the fresh table with a pre-M3 one that lacks the created column.
	if _, err := env.app.DB().NewQuery("DROP TABLE sync_changes").Execute(); err != nil {
		t.Fatalf("drop sync_changes: %v", err)
	}
	if _, err := env.app.DB().NewQuery(`CREATE TABLE sync_changes (
		seq            INTEGER PRIMARY KEY AUTOINCREMENT,
		collection     TEXT NOT NULL,
		record_id      TEXT NOT NULL,
		op             TEXT NOT NULL,
		owner          TEXT NOT NULL,
		rev            INTEGER NOT NULL,
		changed_fields TEXT NOT NULL DEFAULT '[]'
	)`).Execute(); err != nil {
		t.Fatalf("create pre-M3 sync_changes: %v", err)
	}
	if _, err := env.app.DB().NewQuery(`INSERT INTO sync_changes
		(collection, record_id, op, owner, rev, changed_fields)
		VALUES ('notes', 'notesrecord0001', 'create', 'user1', 1, '["title"]')`).Execute(); err != nil {
		t.Fatalf("insert pre-M3 row: %v", err)
	}

	if srvScenHasColumn(t, env.app, "created") {
		t.Fatal("precondition: created column should be absent")
	}

	if err := addChangeLogCreated(env.app); err != nil {
		t.Fatalf("addChangeLogCreated: %v", err)
	}
	if !srvScenHasColumn(t, env.app, "created") {
		t.Fatal("created column was not added")
	}
	if n := srvScenCountEmptyCreated(t, env.app); n != 0 {
		t.Fatalf("rows left with empty created after backfill: %d", n)
	}

	// A second run must be a no-op: no error and no value change.
	before := srvScenCreatedSnapshot(t, env.app)
	if err := addChangeLogCreated(env.app); err != nil {
		t.Fatalf("second addChangeLogCreated: %v", err)
	}
	after := srvScenCreatedSnapshot(t, env.app)
	if !srvScenSameStringMap(before, after) {
		t.Fatalf("second run was not a no-op: before=%v after=%v", before, after)
	}

	// A row written with the '' default (e.g. an old build inserting without
	// created) is repaired by a later run rather than being deleted by Prune.
	if _, err := env.app.DB().NewQuery(`INSERT INTO sync_changes
		(collection, record_id, op, owner, rev, changed_fields, created)
		VALUES ('notes', 'notesrecord0002', 'update', 'user1', 2, '["body"]', '')`).Execute(); err != nil {
		t.Fatalf("insert empty-created row: %v", err)
	}
	if n := srvScenCountEmptyCreated(t, env.app); n != 1 {
		t.Fatalf("expected 1 empty created row, got %d", n)
	}
	if err := addChangeLogCreated(env.app); err != nil {
		t.Fatalf("repair run: %v", err)
	}
	if n := srvScenCountEmptyCreated(t, env.app); n != 0 {
		t.Fatalf("empty created row was not repaired: %d remain", n)
	}
}

func srvScenHasColumn(t *testing.T, app core.App, name string) bool {
	t.Helper()
	cols, err := app.TableColumns("sync_changes")
	if err != nil {
		t.Fatalf("TableColumns: %v", err)
	}
	for _, c := range cols {
		if c == name {
			return true
		}
	}
	return false
}

func srvScenCountEmptyCreated(t *testing.T, app core.App) int {
	t.Helper()
	var n int
	if err := app.DB().NewQuery("SELECT COUNT(*) FROM sync_changes WHERE created = '' OR created IS NULL").Row(&n); err != nil {
		t.Fatalf("count empty created: %v", err)
	}
	return n
}

// srvScenCreatedSnapshot maps rowid -> created for the no-op comparison.
func srvScenCreatedSnapshot(t *testing.T, app core.App) map[string]string {
	t.Helper()
	var rows []struct {
		Rowid   string `db:"rowid"`
		Created string `db:"created"`
	}
	if err := app.DB().NewQuery("SELECT rowid, created FROM sync_changes").All(&rows); err != nil {
		t.Fatalf("snapshot created: %v", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Rowid] = r.Created
	}
	return out
}

func srvScenSameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ---- S-PULL-6: snapshot stability while records mutate mid-snapshot ----

// srvScenPullState is the minimal client-side view of one record used to apply
// pulled changes by rev (protocol §11.3).
type srvScenPullState struct {
	rev     int64
	deleted bool
}

// TestSrvScenSnapshotStabilityMidSnapshot starts a snapshot with a small page
// limit, mutates data between pages (create/edit/delete, all seq > H), drains
// the snapshot, then continues incrementally to head, and asserts the final
// delivered set equals the server state for that user (no record skipped, none
// delivered stale, deletes delivered) — protocol §7.3.2.
func TestSrvScenSnapshotStabilityMidSnapshot(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	base := []string{
		"srvscen00000001", "srvscen00000002", "srvscen00000003",
		"srvscen00000004", "srvscen00000005",
	}
	for _, id := range base {
		env.saveNoteID(alice, id, "t-"+id, "b")
	}

	state := map[string]srvScenPullState{}
	deletedDelivered := map[string]bool{}
	sawSnapshotHasMore := false
	mutated := false

	apply := func(ch []map[string]any) {
		for _, c := range ch {
			id, _ := c["recordId"].(string)
			rev, _ := c["rev"].(float64)
			local, known := state[id]
			if known && int64(rev) <= local.rev {
				continue // stale/duplicate
			}
			switch c["op"] {
			case "delete":
				state[id] = srvScenPullState{rev: int64(rev), deleted: true}
				deletedDelivered[id] = true
			default:
				state[id] = srvScenPullState{rev: int64(rev)}
			}
		}
	}

	cursor := ""
	for {
		pageURL := "/api/sync/pull?limit=2"
		if cursor != "" {
			pageURL = "/api/sync/pull?limit=2&cursor=" + url.QueryEscape(cursor)
		}
		status, body := env.getJSON(pageURL, tok, nil)
		if status != http.StatusOK {
			t.Fatalf("pull status = %d body = %v", status, body)
		}
		if body["mode"] == "snapshot" && body["hasMore"] == true {
			sawSnapshotHasMore = true
		}
		apply(changes(t, body))
		cursor = body["cursor"].(string)

		if body["hasMore"] == true {
			if !mutated {
				// Mid-snapshot mutations: all get seq > H and must be
				// redelivered incrementally. The new record's id sorts first,
				// so it cannot appear in the snapshot keyset at all.
				env.saveNoteID(alice, "srvscen00000000", "new", "n")
				if rec, err := env.app.FindRecordById("notes", "srvscen00000001"); err == nil {
					rec.Set("title", "edited")
					if err := env.app.Save(rec); err != nil {
						t.Fatal(err)
					}
				} else {
					t.Fatalf("find record to edit: %v", err)
				}
				if rec, err := env.app.FindRecordById("notes", "srvscen00000002"); err == nil {
					if err := env.app.Delete(rec); err != nil {
						t.Fatal(err)
					}
				} else {
					t.Fatalf("find record to delete: %v", err)
				}
				mutated = true
			}
			continue
		}
		break
	}

	if !sawSnapshotHasMore {
		t.Fatal("snapshot never reported hasMore; the mid-snapshot path was not exercised")
	}

	// Continue incrementally until head.
	for {
		status, body := env.getJSON("/api/sync/pull?cursor="+url.QueryEscape(cursor), tok, nil)
		if status != http.StatusOK {
			t.Fatalf("incremental status = %d body = %v", status, body)
		}
		apply(changes(t, body))
		cursor = body["cursor"].(string)
		if body["hasMore"] != true {
			break
		}
	}

	// Final local state must equal the server state for alice.
	serverRev := func(id string) (int, bool) {
		rec, err := env.app.FindRecordById("notes", id)
		if err != nil {
			return 0, false
		}
		if ownerOf(rec, "owner") != alice.Id {
			return 0, false
		}
		return metaRev(t, env.app, "notes", id), true
	}

	expected := []string{"srvscen00000000", "srvscen00000001", "srvscen00000003", "srvscen00000004", "srvscen00000005"}
	for _, id := range expected {
		rev, ok := serverRev(id)
		if !ok {
			t.Fatalf("server unexpectedly missing %s", id)
		}
		local, known := state[id]
		if !known || local.deleted {
			t.Fatalf("record %s not delivered: %#v", id, local)
		}
		if local.rev != int64(rev) {
			t.Fatalf("record %s delivered stale: local rev %d, server rev %d", id, local.rev, rev)
		}
	}

	if _, ok := serverRev("srvscen00000002"); ok {
		t.Fatal("server still has the deleted record")
	}
	if local := state["srvscen00000002"]; !local.deleted {
		t.Fatalf("delete not delivered: local state %#v", local)
	}
	if !deletedDelivered["srvscen00000002"] {
		t.Fatal("delete change was not delivered as an op=delete entry")
	}

	// No local record may exist that the server no longer has.
	for id, local := range state {
		if local.deleted {
			continue
		}
		if _, ok := serverRev(id); !ok {
			t.Fatalf("local record %s absent on server (extra/stale)", id)
		}
	}
}

// ---- S-INFO-4: protocol header enforced on /push and /pull ----

// TestSrvScenProtocolHeaderEnforced checks a missing or invalid
// X-Sync-Protocol on POST /api/sync/push and GET /api/sync/pull returns
// 400 protocol_unsupported with the supported versions (protocol §2).
func TestSrvScenProtocolHeaderEnforced(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	pushBody, _ := json.Marshal(map[string]any{
		"mutations": []any{deleteMut("m1", "notes", "notesrecord0001", 1)},
	})

	for _, header := range []string{"", "2"} {
		status, body := env.getJSON("/api/sync/pull", tok, map[string]string{"X-Sync-Protocol": header})
		if status != http.StatusBadRequest || body["code"] != "protocol_unsupported" {
			t.Fatalf("pull header %q: status=%d body=%#v", header, status, body)
		}
		srvScenCheckSupported(t, body)

		status, body = env.pushRaw(tok, pushBody, map[string]string{"X-Sync-Protocol": header})
		if status != http.StatusBadRequest || body["code"] != "protocol_unsupported" {
			t.Fatalf("push header %q: status=%d body=%#v", header, status, body)
		}
		srvScenCheckSupported(t, body)
	}
}

func srvScenCheckSupported(t *testing.T, body map[string]any) {
	t.Helper()
	supported, ok := body["supported"].([]any)
	if !ok || len(supported) != 1 || supported[0].(float64) != ProtocolVersion {
		t.Fatalf("supported = %#v, want [%d]", body["supported"], ProtocolVersion)
	}
}
