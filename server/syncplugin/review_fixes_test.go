package syncplugin

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

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

// ---- audit follow-up (2026-11-06) regression tests ----

func TestMediaQuotaCountsPartialSessions(t *testing.T) {
	cfg := mediaTestConfig(func(mc *MediaConfig) { mc.PerUserQuota = 8 })
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	// Stage 6 of 8 bytes: the session is partial, so the old accounting
	// (fully-staged only) treated it as zero (M-5).
	data := []byte("8byte000")
	st, _, body := env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(`{"collection":"media","sha256":"`+hashBytes(data)+`","size":8,"mime":"image/png","filename":"a.png"}`))
	if st != http.StatusOK {
		t.Fatalf("init: %d %s", st, body)
	}
	uploadID := body["uploadId"].(string)
	st, _, body = env.doJSON(http.MethodPatch, "/api/sync/media/upload/"+uploadID, tok,
		map[string]string{"Upload-Offset": "0", "Content-Type": tusCTType}, data[:6])
	if st != http.StatusNoContent {
		t.Fatalf("patch: %d %s", st, body)
	}

	// Committed 0 + staged 8 (the declared size, partial or not) + 1 > 8.
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(`{"collection":"media","sha256":"`+hashBytes([]byte("x"))+`","size":1,"mime":"image/png","filename":"b.png"}`))
	if st != http.StatusForbidden || body["code"] != codeQuotaExceeded {
		t.Fatalf("partial session not counted: %d %#v", st, body)
	}
}

func TestMediaInitCapsOpenSessions(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	tok := env.token(env.newUser("alice@example.com"))

	for i := 0; i < mediaMaxOpenSessions; i++ {
		st, _, body := env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
			[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":4,"mime":"image/png","filename":"f%d"}`, hashBytes([]byte(fmt.Sprintf("blob%d", i))), i)))
		if st != http.StatusOK {
			t.Fatalf("init %d: %d %s", i, st, body)
		}
	}
	st, _, body := env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(`{"collection":"media","sha256":"`+hashBytes([]byte("extra"))+`","size":4,"mime":"image/png","filename":"x"}`))
	if st != http.StatusTooManyRequests || body["code"] != "rate_limited" {
		t.Fatalf("session cap not enforced: %d %#v", st, body)
	}
}

func TestPushRejectsCrossUserRelationTargets(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")
	aliceTok := env.token(alice)
	bobTok := env.token(bob)

	// Alice owns a note; bob tries to point his note at it (M-4).
	aliceNote := env.saveNoteID(alice, "alicenote000001", "alice", "")
	notes, err := env.app.FindCollectionByNameOrId("notes")
	if err != nil {
		t.Fatal(err)
	}
	notes.Fields.Add(&core.RelationField{Name: "parent", CollectionId: notes.Id, MaxSelect: 1})
	if err := env.app.Save(notes); err != nil {
		t.Fatal(err)
	}

	status, body := env.pushRaw(bobTok, []byte(fmt.Sprintf(`{"mutations":[
		{"id":"m2","collection":"notes","recordId":"bobnote00000001","op":"create","fields":{"title":"bob","parent":"%s"}}]}`, aliceNote.Id)), nil)
	if status != http.StatusOK {
		t.Fatalf("push: %d", status)
	}
	results := body["results"].([]any)
	res := results[0].(map[string]any)
	if res["status"] != "rejected" || res["code"] != codeValidationFailed {
		t.Fatalf("cross-user relation accepted: %#v", res)
	}

	// The same shape pointing at bob's own record is accepted.
	bobNote := env.saveNoteID(bob, "bobnote00000001", "bob", "")
	status, body = env.pushRaw(bobTok, []byte(fmt.Sprintf(`{"mutations":[
		{"id":"m3","collection":"notes","recordId":"bobnote00000002","op":"create","fields":{"title":"bob2","parent":"%s"}}]}`, bobNote.Id)), nil)
	if status != http.StatusOK {
		t.Fatalf("push own relation: %d", status)
	}
	res = body["results"].([]any)[0].(map[string]any)
	if res["status"] != "ok" {
		t.Fatalf("own relation rejected: %#v", res)
	}

	// And alice's own record works too (regression guard).
	status, body = env.pushRaw(aliceTok, []byte(fmt.Sprintf(`{"mutations":[
		{"id":"m4","collection":"notes","recordId":"alicenote000002","op":"create","fields":{"title":"a2","parent":"%s"}}]}`, aliceNote.Id)), nil)
	if status != http.StatusOK {
		t.Fatalf("alice push: %d", status)
	}
	if res = body["results"].([]any)[0].(map[string]any); res["status"] != "ok" {
		t.Fatalf("own relation rejected: %#v", res)
	}
}

func TestPruneRemovesOldConflicts(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")

	// Seed an old conflict row (the writer sets `created` via SQLite default).
	if _, err := env.app.DB().NewQuery(`INSERT INTO sync_conflicts (collection, record_id, mutation_id, losing_fields, winning_rev)
		VALUES ('notes', 'legacyrecord002', 'm1', '{"title":"x"}', 2)`).Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := env.app.DB().NewQuery(`UPDATE sync_conflicts SET created = '2020-01-01 00:00:00.000Z'`).Execute(); err != nil {
		t.Fatal(err)
	}
	_ = alice

	if err := Prune(env.app, Config{}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	var n int
	if err := env.app.DB().NewQuery(`SELECT COUNT(*) FROM sync_conflicts`).Row(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("old conflict rows not pruned: %d", n)
	}
}

func TestMediaGCPaginatesPastReferencedPage(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	// Fill more than one GC page with records, all old enough; reference the
	// first page's worth so the old loop would stop (L-3).
	total := gcBatch + 3
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		data := []byte(fmt.Sprintf("blob-%04d", i))
		st, body, _ := uploadAndCommit(t, env, tok, cfg.Media, hashBytes(data), "image/png", "f.png", data)
		if st != http.StatusOK || body["status"] != "ok" {
			t.Fatalf("upload %d: %d %s", i, st, body)
		}
		ids = append(ids, body["mediaId"].(string))
	}
	if _, err := env.app.DB().NewQuery(`UPDATE "media" SET updated = '2020-01-01 00:00:00.000Z'`).Execute(); err != nil {
		t.Fatal(err)
	}

	// Reference the 200 lowest ids (gcBatch) from notes, leaving the rest free.
	notes, err := env.app.FindCollectionByNameOrId("notes")
	if err != nil {
		t.Fatal(err)
	}
	mediaCol, err := env.app.FindCollectionByNameOrId("media")
	if err != nil {
		t.Fatal(err)
	}
	notes.Fields.Add(&core.RelationField{Name: "attachment", CollectionId: mediaCol.Id, MaxSelect: 1})
	if err := env.app.Save(notes); err != nil {
		t.Fatal(err)
	}
	sorted := append([]string(nil), ids...)
	for i := 0; i < gcBatch && i < len(sorted); i++ {
		note := core.NewRecord(notes)
		note.Set("title", "n")
		note.Set("attachment", sorted[i])
		note.Set("owner", alice.Id)
		if err := env.app.Save(note); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := MediaGC(env.app, cfg, time.Now().UTC())
	if err != nil {
		t.Fatalf("MediaGC: %v", err)
	}
	if want := total - gcBatch; deleted != want {
		t.Fatalf("deleted %d, want %d (must paginate past the referenced page)", deleted, want)
	}
}
