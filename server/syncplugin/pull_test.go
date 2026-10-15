package syncplugin

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

func TestPullOwnerIsolation(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")

	env.saveNoteID(alice, "notesrecord0001", "alice's note", "a")
	env.saveNoteID(bob, "notesrecord0002", "bob's note", "b")

	status, body := env.getJSON("/api/sync/pull", env.token(alice), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	ch := changes(t, body)
	if len(ch) != 1 {
		t.Fatalf("alice saw %d changes, want 1: %#v", len(ch), ch)
	}
	if ch[0]["recordId"] != "notesrecord0001" || ch[0]["op"] != "upsert" {
		t.Fatalf("alice pull leaked/wrong entry: %#v", ch[0])
	}
}

func TestPullSnapshotPagingHasMoreAndTransition(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	for _, id := range []string{"notesrecord0001", "notesrecord0002", "notesrecord0003"} {
		env.saveNoteID(alice, id, "t-"+id, "b")
	}

	status, body := env.getJSON("/api/sync/pull?limit=2", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("page1 status = %d", status)
	}
	if body["mode"] != "snapshot" || body["hasMore"] != true {
		t.Fatalf("page1 mode/hasMore = %v/%v", body["mode"], body["hasMore"])
	}
	ch := changes(t, body)
	if len(ch) != 2 || ch[0]["recordId"] != "notesrecord0001" || ch[1]["recordId"] != "notesrecord0002" {
		t.Fatalf("page1 entries = %#v", ch)
	}
	cursor1 := body["cursor"].(string)

	status, body = env.getJSON("/api/sync/pull?cursor="+url.QueryEscape(cursor1)+"&limit=2", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("page2 status = %d", status)
	}
	if body["mode"] != "snapshot" || body["hasMore"] != false {
		t.Fatalf("page2 mode/hasMore = %v/%v", body["mode"], body["hasMore"])
	}
	ch = changes(t, body)
	if len(ch) != 1 || ch[0]["recordId"] != "notesrecord0003" {
		t.Fatalf("page2 entries = %#v", ch)
	}
	cursor2 := body["cursor"].(string)

	// The snapshot is exhausted: the next request is incremental (protocol §7.3.3).
	status, body = env.getJSON("/api/sync/pull?cursor="+url.QueryEscape(cursor2), tok, nil)
	if status != http.StatusOK {
		t.Fatalf("page3 status = %d", status)
	}
	if body["mode"] != "incremental" || body["hasMore"] != false || len(changes(t, body)) != 0 {
		t.Fatalf("page3 = mode %v hasMore %v changes %#v", body["mode"], body["hasMore"], changes(t, body))
	}
}

func TestPullIncrementalDedupeAndDelete(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	env.saveNoteID(alice, "notesrecord0001", "one", "b")
	env.saveNoteID(alice, "notesrecord0002", "two", "b")

	// Initial snapshot establishes the incremental head cursor.
	_, body := env.getJSON("/api/sync/pull", tok, nil)
	if len(changes(t, body)) != 2 {
		t.Fatalf("snapshot changes = %#v", changes(t, body))
	}
	cursor := body["cursor"].(string)

	// Two updates to rec1 (dedupe to latest) and a delete of rec2.
	update := func(id, field, val string) {
		rec, err := env.app.FindRecordById("notes", id)
		if err != nil {
			t.Fatal(err)
		}
		rec.Set(field, val)
		if err := env.app.Save(rec); err != nil {
			t.Fatal(err)
		}
	}
	update("notesrecord0001", "title", "one-b")
	update("notesrecord0001", "body", "b2")
	del, err := env.app.FindRecordById("notes", "notesrecord0002")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.app.Delete(del); err != nil {
		t.Fatal(err)
	}

	status, body := env.getJSON("/api/sync/pull?cursor="+url.QueryEscape(cursor), tok, nil)
	if status != http.StatusOK || body["mode"] != "incremental" {
		t.Fatalf("incremental status/mode = %d/%v", status, body["mode"])
	}
	ch := changes(t, body)
	if len(ch) != 2 {
		t.Fatalf("incremental changes = %#v", ch)
	}
	if ch[0]["recordId"] != "notesrecord0001" || ch[0]["op"] != "upsert" || int(ch[0]["rev"].(float64)) != 3 {
		t.Fatalf("rec1 entry = %#v", ch[0])
	}
	rec := ch[0]["record"].(map[string]any)
	if rec["title"] != "one-b" || rec["body"] != "b2" {
		t.Fatalf("rec1 latest state = %#v", rec)
	}
	if ch[1]["recordId"] != "notesrecord0002" || ch[1]["op"] != "delete" || int(ch[1]["rev"].(float64)) != 2 {
		t.Fatalf("rec2 entry = %#v", ch[1])
	}
	if _, ok := ch[1]["record"]; ok {
		t.Fatalf("delete entry must not carry a record: %#v", ch[1])
	}
}

// TestPullNormalSavePath checks a record changed via the normal PocketBase save
// path (no sync handler) appears in pull with its rev and changed fields.
func TestPullNormalSavePath(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	rec := env.saveNoteID(alice, "notesrecord0001", "one", "b")
	_, body := env.getJSON("/api/sync/pull", tok, nil)
	cursor := body["cursor"].(string)

	fetched, err := env.app.FindRecordById("notes", rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	fetched.Set("title", "one2") // normal save path, not a sync endpoint
	if err := env.app.Save(fetched); err != nil {
		t.Fatal(err)
	}

	_, body = env.getJSON("/api/sync/pull?cursor="+url.QueryEscape(cursor), tok, nil)
	ch := changes(t, body)
	if len(ch) != 1 || int(ch[0]["rev"].(float64)) != 2 {
		t.Fatalf("pull after normal save = %#v", ch)
	}
	if ch[0]["record"].(map[string]any)["title"] != "one2" {
		t.Fatalf("record state = %#v", ch[0]["record"])
	}
	if got := decodeChanged(t, lastChange(t, env.app, "notes", rec.Id).ChangedFields); !reflect.DeepEqual(got, []string{"title"}) {
		t.Fatalf("changed_fields = %v, want [title]", got)
	}
}

func TestPullCursorErrors(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	t.Run("malformed", func(t *testing.T) {
		status, body := env.getJSON("/api/sync/pull?cursor="+url.QueryEscape("not a cursor!"), tok, nil)
		if status != http.StatusBadRequest || body["code"] != "cursor_invalid" {
			t.Fatalf("status=%d code=%v", status, body["code"])
		}
	})

	t.Run("ahead of head", func(t *testing.T) {
		cur := encodeCursor(cursor{Kind: cursorKindIncremental, Seq: 1000})
		status, body := env.getJSON("/api/sync/pull?cursor="+url.QueryEscape(cur), tok, nil)
		if status != http.StatusGone || body["code"] != "cursor_too_old" {
			t.Fatalf("status=%d code=%v", status, body["code"])
		}
	})

	t.Run("older than retained history", func(t *testing.T) {
		for _, id := range []string{"notesrecord0001", "notesrecord0002", "notesrecord0003"} {
			env.saveNoteID(alice, id, "t-"+id, "b")
		}
		// Simulate log pruning: drop the oldest rows so seq 1 is no longer retained.
		if _, err := env.app.DB().NewQuery("DELETE FROM sync_changes WHERE seq <= 2").Execute(); err != nil {
			t.Fatal(err)
		}
		cur := encodeCursor(cursor{Kind: cursorKindIncremental, Seq: 1})
		status, body := env.getJSON("/api/sync/pull?cursor="+url.QueryEscape(cur), tok, nil)
		if status != http.StatusGone || body["code"] != "cursor_too_old" {
			t.Fatalf("status=%d code=%v", status, body["code"])
		}
	})
}

func TestPullEnforcesMaxPage(t *testing.T) {
	env := newTestEnv(t, Config{MaxPage: 2, DefaultPage: 10})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	for _, id := range []string{"notesrecord0001", "notesrecord0002", "notesrecord0003"} {
		env.saveNoteID(alice, id, "t-"+id, "b")
	}

	_, body := env.getJSON("/api/sync/pull", tok, nil)
	if got := len(changes(t, body)); got != 2 {
		t.Fatalf("page size = %d, want 2 (maxPage)", got)
	}
	if body["hasMore"] != true {
		t.Fatalf("hasMore = %v, want true", body["hasMore"])
	}
}

// TestPullUsesDefaultPage checks the default page size applies when no/invalid
// limit is given (protocol §7).
func TestPullUsesDefaultPage(t *testing.T) {
	env := newTestEnv(t, Config{DefaultPage: 1, MaxPage: 5})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	for _, id := range []string{"notesrecord0001", "notesrecord0002", "notesrecord0003"} {
		env.saveNoteID(alice, id, "t-"+id, "b")
	}

	for _, q := range []string{"", "?limit=0", "?limit=-3", "?limit=notanumber"} {
		_, body := env.getJSON("/api/sync/pull"+q, tok, nil)
		if got := len(changes(t, body)); got != 1 {
			t.Fatalf("pull%s page size = %d, want defaultPage 1", q, got)
		}
	}
}
