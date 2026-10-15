package syncplugin

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

// normalizedResult marshals a push result with the caller-chosen mutation id
// removed. The mutation id is supplied by the caller and says nothing about the
// target record, so two responses equal after this normalization are the "no
// existence oracle" property required by protocol §14.5 / threat-model.
func normalizedResult(t *testing.T, r map[string]any) string {
	t.Helper()
	delete(r, "id")
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return string(raw)
}

func requireField(t *testing.T, r map[string]any, key string) {
	t.Helper()
	if _, ok := r[key]; ok {
		t.Fatalf("result must not carry %q: %#v", key, r)
	}
}

// TestSecurityUpdateNoExistenceOracle: updating another user's record is
// byte-for-byte the same response as updating a nonexistent one, and the other
// user's record is untouched.
func TestSecurityUpdateNoExistenceOracle(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")
	aliceTok := env.token(alice)

	bobRec := env.saveNoteID(bob, "notesrecord0002", "bob-original", "b")

	_, respCross := env.pushMutations(aliceTok,
		updateMut("x1", "notes", "notesrecord0002", 1, map[string]any{"title": "hacked"}))
	_, respMissing := env.pushMutations(aliceTok,
		updateMut("x2", "notes", "notesrecord0003", 1, map[string]any{"title": "hacked"}))

	cross, missing := resultAt(t, respCross, 0), resultAt(t, respMissing, 0)
	wantStatus(t, cross, statusRejected)
	wantCode(t, cross, codeNotFound)
	wantStatus(t, missing, statusRejected)
	wantCode(t, missing, codeNotFound)
	requireField(t, cross, "record")
	requireField(t, cross, "rev")

	if a, b := normalizedResult(t, cross), normalizedResult(t, missing); a != b {
		t.Fatalf("cross-user and missing update responses differ:\n %s\n %s", a, b)
	}

	rec, err := env.app.FindRecordById("notes", bobRec.Id)
	if err != nil {
		t.Fatalf("bob's record disappeared: %v", err)
	}
	if rec.GetString("title") != "bob-original" || ownerOf(rec, "owner") != bob.Id {
		t.Fatalf("bob's record changed: title=%q owner=%q", rec.GetString("title"), ownerOf(rec, "owner"))
	}
	if got := metaRev(t, env.app, "notes", "notesrecord0002"); got != 1 {
		t.Fatalf("bob's record rev changed to %d", got)
	}
}

// TestSecurityDeleteNoExistenceOracle: deleting another user's record returns
// the same ok as deleting a missing one, and the record is not deleted.
func TestSecurityDeleteNoExistenceOracle(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")
	aliceTok := env.token(alice)

	bobRec := env.saveNoteID(bob, "notesrecord0002", "bob-original", "b")

	_, respCross := env.pushMutations(aliceTok, deleteMut("y1", "notes", "notesrecord0002", 1))
	_, respMissing := env.pushMutations(aliceTok, deleteMut("y2", "notes", "notesrecord0003", 1))

	cross, missing := resultAt(t, respCross, 0), resultAt(t, respMissing, 0)
	wantStatus(t, cross, statusOK)
	wantStatus(t, missing, statusOK)
	requireField(t, cross, "record")
	requireField(t, cross, "rev")

	if a, b := normalizedResult(t, cross), normalizedResult(t, missing); a != b {
		t.Fatalf("cross-user and missing delete responses differ:\n %s\n %s", a, b)
	}

	rec, err := env.app.FindRecordById("notes", bobRec.Id)
	if err != nil {
		t.Fatalf("bob's record was deleted: %v", err)
	}
	if rec.GetString("title") != "bob-original" || ownerOf(rec, "owner") != bob.Id {
		t.Fatalf("bob's record changed: title=%q owner=%q", rec.GetString("title"), ownerOf(rec, "owner"))
	}
}

// TestSecurityCreateIDSquattingIndistinguishable: creating with another user's
// live ID responds exactly like creating with a previously-deleted ID, and the
// other user's record is untouched (protocol §6.3, §14.5).
func TestSecurityCreateIDSquattingIndistinguishable(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")
	aliceTok := env.token(alice)
	bobTok := env.token(bob)

	// Bob's live record.
	env.pushMutations(bobTok, createMut("b1", "notes", "notesrecord0002", map[string]any{"title": "bob"}))
	// An ID Alice created and then deleted.
	env.pushMutations(aliceTok, createMut("a1", "notes", "notesrecord0001", map[string]any{"title": "gone"}))
	env.pushMutations(aliceTok, deleteMut("a2", "notes", "notesrecord0001", 1))

	_, respLive := env.pushMutations(aliceTok, createMut("s1", "notes", "notesrecord0002", map[string]any{"title": "squat"}))
	_, respDeleted := env.pushMutations(aliceTok, createMut("s2", "notes", "notesrecord0001", map[string]any{"title": "squat"}))

	live, deleted := resultAt(t, respLive, 0), resultAt(t, respDeleted, 0)
	wantStatus(t, live, statusRejected)
	wantCode(t, live, codeIDUnavailable)
	wantStatus(t, deleted, statusRejected)
	wantCode(t, deleted, codeIDUnavailable)
	requireField(t, live, "record")
	requireField(t, live, "rev")

	if a, b := normalizedResult(t, live), normalizedResult(t, deleted); a != b {
		t.Fatalf("live-ID and deleted-ID create responses differ:\n %s\n %s", a, b)
	}

	rec, err := env.app.FindRecordById("notes", "notesrecord0002")
	if err != nil {
		t.Fatalf("bob's record disappeared: %v", err)
	}
	if rec.GetString("title") != "bob" || ownerOf(rec, "owner") != bob.Id {
		t.Fatalf("bob's record changed: title=%q owner=%q", rec.GetString("title"), ownerOf(rec, "owner"))
	}
}

// TestSecurityForgedServerFieldsNeverApplied: forged owner/id/created/updated
// (and collectionId) are rejected and never written.
func TestSecurityForgedServerFieldsNeverApplied(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")
	aliceTok := env.token(alice)

	cases := []struct {
		name   string
		fields map[string]any
	}{
		{"owner", map[string]any{"title": "t", "owner": bob.Id}},
		{"id", map[string]any{"title": "t", "id": "notesrecord0007"}},
		{"created", map[string]any{"title": "t", "created": "2020-01-01 00:00:00.000Z"}},
		{"updated", map[string]any{"title": "t", "updated": "2020-01-01 00:00:00.000Z"}},
		{"collectionId", map[string]any{"title": "t", "collectionId": "somecollectionid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, resp := env.pushMutations(aliceTok, createMut("f-"+tc.name, "notes", "notesrecord0009", tc.fields))
			r := resultAt(t, resp, 0)
			wantStatus(t, r, statusRejected)
			wantCode(t, r, codeForbiddenField)

			if _, err := env.app.FindRecordById("notes", "notesrecord0009"); err == nil {
				t.Fatal("forged-field create was applied")
			}
			var n int
			if err := env.app.DB().NewQuery(
				"SELECT COUNT(*) FROM sync_meta WHERE collection='notes' AND record_id='notesrecord0009'",
			).Row(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("rejected create left a sync_meta row: %d", n)
			}
		})
	}

	// A legitimate create is owned by the authenticated user, never by input.
	_, resp := env.pushMutations(aliceTok, createMut("ok1", "notes", "notesrecord0001", map[string]any{"title": "t"}))
	rec, ok := resultAt(t, resp, 0)["record"].(map[string]any)
	if !ok || rec["owner"] != alice.Id {
		t.Fatalf("create owner = %#v, want %s", rec["owner"], alice.Id)
	}
	var owner string
	if err := env.app.DB().NewQuery("SELECT owner FROM sync_meta WHERE collection='notes' AND record_id='notesrecord0001'").Row(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != alice.Id {
		t.Fatalf("stored owner = %q, want %s", owner, alice.Id)
	}
}

// TestSecurityPullIsolationWithBothUsers: pull never returns another user's
// records, in snapshot or incremental mode, even when both users have changes.
func TestSecurityPullIsolationWithBothUsers(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")
	aliceTok := env.token(alice)

	aliceIDs := map[string]bool{"notesrecord0001": true, "notesrecord0002": true}
	env.saveNoteID(alice, "notesrecord0001", "a1", "b")
	env.saveNoteID(alice, "notesrecord0002", "a2", "b")
	env.saveNoteID(bob, "notesrecord0003", "b1", "b")
	env.saveNoteID(bob, "notesrecord0004", "b2", "b")

	_, body := env.getJSON("/api/sync/pull", aliceTok, nil)
	ch := changes(t, body)
	if len(ch) != 2 {
		t.Fatalf("alice snapshot changes = %d, want 2: %#v", len(ch), ch)
	}
	for _, c := range ch {
		if !aliceIDs[c["recordId"].(string)] {
			t.Fatalf("alice pull leaked another owner's record: %#v", c)
		}
	}
	cursor := body["cursor"].(string)

	// Bob keeps changing after Alice's cursor; Alice must not see any of it.
	bobRec, err := env.app.FindRecordById("notes", "notesrecord0003")
	if err != nil {
		t.Fatal(err)
	}
	bobRec.Set("title", "b1-edited")
	if err := env.app.Save(bobRec); err != nil {
		t.Fatal(err)
	}
	env.saveNoteID(bob, "notesrecord0005", "b3", "b")

	_, body = env.getJSON("/api/sync/pull?cursor="+url.QueryEscape(cursor), aliceTok, nil)
	if leaked := changes(t, body); len(leaked) != 0 {
		t.Fatalf("alice incremental pull leaked %d changes: %#v", len(leaked), leaked)
	}
}

// TestSecurityUniqueAndValidationCodes: the standard PocketBase errors map to the
// protocol codes on the sync path.
func TestSecurityUniqueAndValidationCodes(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	col, err := env.app.FindCollectionByNameOrId("notes")
	if err != nil {
		t.Fatal(err)
	}
	col.Fields.Add(&core.TextField{Name: "code"})
	col.AddIndex("idx_notes_sec_code", true, "code", "code != ''")
	if err := env.app.Save(col); err != nil {
		t.Fatalf("add unique field: %v", err)
	}

	env.pushMutations(tok, createMut("u1", "notes", "notesrecord0001", map[string]any{"title": "a", "code": "X"}))
	_, resp := env.pushMutations(tok, createMut("u2", "notes", "notesrecord0002", map[string]any{"title": "b", "code": "X"}))
	wantCode(t, resultAt(t, resp, 0), codeUniqueViolation)

	_, resp = env.pushMutations(tok, createMut("v1", "notes", "notesrecord0003", map[string]any{"body": "no title"}))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusRejected)
	wantCode(t, r, codeValidationFailed)
}
