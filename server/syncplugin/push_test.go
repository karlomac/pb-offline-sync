package syncplugin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

// ---- push test helpers ----

// pushRaw posts a body to /api/sync/push.
func (env *testEnv) pushRaw(token string, body []byte, headers map[string]string) (int, map[string]any) {
	env.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sync/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sync-Protocol", "1")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	res := rec.Result()
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return res.StatusCode, out
}

func (env *testEnv) push(token string, body map[string]any) (int, map[string]any) {
	env.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		env.t.Fatalf("marshal push body: %v", err)
	}
	return env.pushRaw(token, raw, nil)
}

func (env *testEnv) pushMutations(token string, muts ...map[string]any) (int, map[string]any) {
	env.t.Helper()
	list := make([]any, len(muts))
	for i, m := range muts {
		list[i] = m
	}
	return env.push(token, map[string]any{"mutations": list})
}

func createMut(id, coll, recID string, fields map[string]any) map[string]any {
	return map[string]any{"id": id, "collection": coll, "recordId": recID, "op": "create", "fields": fields}
}

func updateMut(id, coll, recID string, baseRev int, fields map[string]any) map[string]any {
	return map[string]any{"id": id, "collection": coll, "recordId": recID, "op": "update", "baseRev": baseRev, "fields": fields}
}

func deleteMut(id, coll, recID string, baseRev int) map[string]any {
	return map[string]any{"id": id, "collection": coll, "recordId": recID, "op": "delete", "baseRev": baseRev}
}

func resultAt(t *testing.T, resp map[string]any, i int) map[string]any {
	t.Helper()
	raw, ok := resp["results"].([]any)
	if !ok {
		t.Fatalf("response has no results array: %#v", resp)
	}
	if i >= len(raw) {
		t.Fatalf("results has %d entries, want index %d: %#v", len(raw), i, raw)
	}
	m, ok := raw[i].(map[string]any)
	if !ok {
		t.Fatalf("result %d is not an object: %#v", i, raw[i])
	}
	return m
}

func wantStatus(t *testing.T, r map[string]any, status string) {
	t.Helper()
	if r["status"] != status {
		t.Fatalf("status = %v, want %q (result %#v)", r["status"], status, r)
	}
}

func wantCode(t *testing.T, r map[string]any, code string) {
	t.Helper()
	if r["code"] != code {
		t.Fatalf("code = %v, want %q (result %#v)", r["code"], code, r)
	}
}

func wantRev(t *testing.T, r map[string]any, rev int) {
	t.Helper()
	got, ok := r["rev"].(float64)
	if !ok || int(got) != rev {
		t.Fatalf("rev = %v, want %d (result %#v)", r["rev"], rev, r)
	}
}

func wantMerged(t *testing.T, r map[string]any, merged bool) {
	t.Helper()
	if r["merged"] != merged {
		t.Fatalf("merged = %v, want %v (result %#v)", r["merged"], merged, r)
	}
}

func wantRecordField(t *testing.T, r map[string]any, key, val string) {
	t.Helper()
	rec, ok := r["record"].(map[string]any)
	if !ok {
		t.Fatalf("result has no record object: %#v", r)
	}
	if rec[key] != val {
		t.Fatalf("record.%s = %v, want %q (record %#v)", key, rec[key], val, rec)
	}
}

func countConflicts(t *testing.T, env *testEnv) int {
	t.Helper()
	var n int
	if err := env.app.DB().NewQuery("SELECT COUNT(*) FROM sync_conflicts").Row(&n); err != nil {
		t.Fatalf("count sync_conflicts: %v", err)
	}
	return n
}

// ---- happy path / rev ----

// TestPushCreateUpdateDeleteRevIncrements mirrors conformance fixture
// create-update-delete-rev-increments.
func TestPushCreateUpdateDeleteRevIncrements(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	status, resp := env.pushMutations(tok,
		createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "first", "body": "hello"}),
	)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusOK)
	wantRev(t, r, 1)
	wantRecordField(t, r, "id", "notesrecord0001")
	wantRecordField(t, r, "title", "first")
	wantRecordField(t, r, "body", "hello")
	if rec := r["record"].(map[string]any); rec["collectionName"] != "notes" {
		t.Fatalf("record missing server-authoritative fields: %#v", rec)
	}

	status, resp = env.pushMutations(tok,
		updateMut("m2", "notes", "notesrecord0001", 1, map[string]any{"title": "second"}),
	)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	r = resultAt(t, resp, 0)
	wantStatus(t, r, statusOK)
	wantRev(t, r, 2)
	wantMerged(t, r, false)
	wantRecordField(t, r, "title", "second")

	_, resp = env.pushMutations(tok,
		updateMut("m3", "notes", "notesrecord0001", 2, map[string]any{"body": "world"}),
	)
	r = resultAt(t, resp, 0)
	wantStatus(t, r, statusOK)
	wantRev(t, r, 3)
	wantMerged(t, r, false)
	wantRecordField(t, r, "body", "world")

	_, resp = env.pushMutations(tok,
		deleteMut("m4", "notes", "notesrecord0001", 3),
	)
	r = resultAt(t, resp, 0)
	wantStatus(t, r, statusOK)
	if _, ok := r["record"]; ok {
		t.Fatalf("delete ok must not carry a record: %#v", r)
	}
	if _, err := env.app.FindRecordById("notes", "notesrecord0001"); err == nil {
		t.Fatal("record still exists after delete")
	}
	if got := metaRev(t, env.app, "notes", "notesrecord0001"); got != 4 {
		t.Fatalf("tombstone rev = %d, want 4", got)
	}
}

// ---- idempotency ----

// TestPushDuplicateMutationIDSamePayload mirrors
// duplicate-mutation-id-same-payload.
func TestPushDuplicateMutationIDSamePayload(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	m := createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "first", "body": "hello"})
	_, resp := env.pushMutations(tok, m)
	if r := resultAt(t, resp, 0); r["status"] != statusOK {
		t.Fatalf("first push = %#v", r)
	}
	changesBefore := countChanges(t, env.app)

	_, resp = env.pushMutations(tok, m)
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusDuplicate)
	wantRev(t, r, 1)
	wantRecordField(t, r, "id", "notesrecord0001")
	wantRecordField(t, r, "title", "first")

	if got := countChanges(t, env.app); got != changesBefore {
		t.Fatalf("duplicate re-applied: changes %d -> %d", changesBefore, got)
	}
}

// TestPushMutationIDReuseDifferentPayload mirrors
// mutation-id-reuse-different-payload and proves the original stored result is
// not overwritten.
func TestPushMutationIDReuseDifferentPayload(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "first", "body": "hello"}))

	_, resp := env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "different", "body": "hello"}))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusRejected)
	wantCode(t, r, codeMutationIDReuse)

	// The original payload still resolves to the original result (reuse did not
	// overwrite sync_processed).
	_, resp = env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "first", "body": "hello"}))
	r = resultAt(t, resp, 0)
	wantStatus(t, r, statusDuplicate)
	wantRev(t, r, 1)
}

// TestPushRejectedResultIsStored proves final rejected outcomes are stored and
// replayed as duplicates (protocol §9).
func TestPushRejectedResultIsStored(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	m := updateMut("m1", "notes", "notesrecord0001", 1, map[string]any{"title": "x"})
	_, resp := env.pushMutations(tok, m)
	if r := resultAt(t, resp, 0); r["code"] != codeNotFound {
		t.Fatalf("first = %#v", r)
	}
	_, resp = env.pushMutations(tok, m)
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusDuplicate)
	wantCode(t, r, codeNotFound)
}

// ---- stale base / merge / conflict policies ----

func newStaleBaseEnv(t *testing.T, conflict ConflictPolicy) (*testEnv, string) {
	t.Helper()
	env := newTestEnv(t, Config{Collections: map[string]CollectionConfig{
		"notes": {OwnerField: "owner", Conflict: conflict, DeletePolicy: DeleteWins},
	}})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)
	env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "t", "body": "b"}))
	env.pushMutations(tok, updateMut("m2", "notes", "notesrecord0001", 1, map[string]any{"title": "t2"}))
	return env, tok
}

// TestPushStaleBaseNonOverlappingMerge mirrors
// stale-base-nonoverlapping-merge.
func TestPushStaleBaseNonOverlappingMerge(t *testing.T) {
	env, tok := newStaleBaseEnv(t, ServerWins)

	_, resp := env.pushMutations(tok,
		updateMut("m3", "notes", "notesrecord0001", 1, map[string]any{"body": "b2"}),
	)
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusOK)
	wantRev(t, r, 3)
	wantMerged(t, r, true)
	wantRecordField(t, r, "title", "t2")
	wantRecordField(t, r, "body", "b2")
}

// TestPushStaleBaseOverlapServerWins mirrors stale-base-overlap-server-wins.
func TestPushStaleBaseOverlapServerWins(t *testing.T) {
	env, tok := newStaleBaseEnv(t, ServerWins)

	_, resp := env.pushMutations(tok,
		updateMut("m3", "notes", "notesrecord0001", 1, map[string]any{"title": "t3"}),
	)
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusConflict)
	wantCode(t, r, codeFieldConflict)
	wantRev(t, r, 2)
	wantRecordField(t, r, "title", "t2")

	if got := countConflicts(t, env); got != 1 {
		t.Fatalf("sync_conflicts rows = %d, want 1", got)
	}
	rec, err := env.app.FindRecordById("notes", "notesrecord0001")
	if err != nil {
		t.Fatal(err)
	}
	if rec.GetString("title") != "t2" {
		t.Fatalf("serverWins applied the mutation: title=%q", rec.GetString("title"))
	}
}

// TestPushStaleBaseOverlapClientWins mirrors stale-base-overlap-client-wins.
func TestPushStaleBaseOverlapClientWins(t *testing.T) {
	env, tok := newStaleBaseEnv(t, ClientWins)

	_, resp := env.pushMutations(tok,
		updateMut("m3", "notes", "notesrecord0001", 1, map[string]any{"title": "t3"}),
	)
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusOK)
	wantRev(t, r, 3)
	wantMerged(t, r, false)
	wantRecordField(t, r, "title", "t3")
	if cid, _ := r["conflictId"].(string); cid == "" {
		t.Fatalf("clientWins result missing conflictId: %#v", r)
	}
	if got := countConflicts(t, env); got != 1 {
		t.Fatalf("sync_conflicts rows = %d, want 1", got)
	}
}

// TestPushStaleBaseOverlapReject mirrors stale-base-overlap-reject.
func TestPushStaleBaseOverlapReject(t *testing.T) {
	env, tok := newStaleBaseEnv(t, Reject)

	_, resp := env.pushMutations(tok,
		updateMut("m3", "notes", "notesrecord0001", 1, map[string]any{"title": "t3"}),
	)
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusConflict)
	wantCode(t, r, codeFieldConflict)
	wantRev(t, r, 2)
	wantRecordField(t, r, "title", "t2")

	if got := countConflicts(t, env); got != 0 {
		t.Fatalf("reject must not write sync_conflicts: rows=%d", got)
	}
}

// ---- delete policies ----

// TestPushDeleteWinsStaleBase mirrors delete-wins-applies-on-stale-base.
func TestPushDeleteWinsStaleBase(t *testing.T) {
	env, tok := newStaleBaseEnv(t, ServerWins)

	_, resp := env.pushMutations(tok,
		deleteMut("m3", "notes", "notesrecord0001", 1),
	)
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusOK)
	if _, err := env.app.FindRecordById("notes", "notesrecord0001"); err == nil {
		t.Fatal("record still exists after deleteWins delete")
	}
}

// TestPushEditWinsStaleBase mirrors
// edit-wins-stale-base-conflict-edited-since.
func TestPushEditWinsStaleBase(t *testing.T) {
	env := newTestEnv(t, Config{Collections: map[string]CollectionConfig{
		"notes": {OwnerField: "owner", Conflict: ServerWins, DeletePolicy: EditWins},
	}})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)
	env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "t", "body": "b"}))
	env.pushMutations(tok, updateMut("m2", "notes", "notesrecord0001", 1, map[string]any{"title": "t2"}))

	_, resp := env.pushMutations(tok, deleteMut("m3", "notes", "notesrecord0001", 1))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusConflict)
	wantCode(t, r, codeEditedSince)
	wantRev(t, r, 2)
	wantRecordField(t, r, "title", "t2")
	if _, err := env.app.FindRecordById("notes", "notesrecord0001"); err != nil {
		t.Fatalf("editWins deleted the record: %v", err)
	}
}

// TestPushUpdateAfterDeleteConflictDeleted mirrors
// update-after-delete-conflict-deleted.
func TestPushUpdateAfterDeleteConflictDeleted(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "t", "body": "b"}))
	env.pushMutations(tok, deleteMut("m2", "notes", "notesrecord0001", 1))

	_, resp := env.pushMutations(tok, updateMut("m3", "notes", "notesrecord0001", 1, map[string]any{"title": "t3"}))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusConflict)
	wantCode(t, r, codeDeleted)
}

// TestPushDeleteMissingRecordOK mirrors delete-missing-record-ok.
func TestPushDeleteMissingRecordOK(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	_, resp := env.pushMutations(tok, deleteMut("m1", "notes", "notesrecord0001", 1))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusOK)
	if _, ok := r["rev"]; ok {
		t.Fatalf("delete of missing record must omit rev: %#v", r)
	}
	if _, ok := r["record"]; ok {
		t.Fatalf("delete of missing record must omit record: %#v", r)
	}
}

// TestPushUpdateNeverExistingNotFound checks update of a never-existing record.
func TestPushUpdateNeverExistingNotFound(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	_, resp := env.pushMutations(tok, updateMut("m1", "notes", "notesrecord0001", 1, map[string]any{"title": "x"}))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusRejected)
	wantCode(t, r, codeNotFound)
}

// ---- create edge cases ----

// TestPushCreateExistsSameUser mirrors create-exists-owned-by-same-user.
func TestPushCreateExistsSameUser(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "first", "body": "hello"}))
	_, resp := env.pushMutations(tok, createMut("m2", "notes", "notesrecord0001", map[string]any{"title": "second", "body": "hello"}))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusConflict)
	wantCode(t, r, codeExists)
	wantRev(t, r, 1)
	wantRecordField(t, r, "title", "first")
}

// TestPushCreateIDUnavailable covers both an ID owned by another user and a
// previously deleted ID: the responses are indistinguishable (protocol §6.3).
func TestPushCreateIDUnavailable(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")
	aliceTok := env.token(alice)
	bobTok := env.token(bob)

	// Other user's live ID.
	env.pushMutations(bobTok, createMut("b1", "notes", "notesrecord0002", map[string]any{"title": "bob", "body": "b"}))
	_, resp := env.pushMutations(aliceTok, createMut("a1", "notes", "notesrecord0002", map[string]any{"title": "alice", "body": "a"}))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusRejected)
	wantCode(t, r, codeIDUnavailable)

	// Previously deleted ID.
	env.pushMutations(aliceTok, createMut("a2", "notes", "notesrecord0001", map[string]any{"title": "x", "body": "y"}))
	env.pushMutations(aliceTok, deleteMut("a3", "notes", "notesrecord0001", 1))
	_, resp = env.pushMutations(aliceTok, createMut("a4", "notes", "notesrecord0001", map[string]any{"title": "again", "body": "y"}))
	r = resultAt(t, resp, 0)
	wantStatus(t, r, statusRejected)
	wantCode(t, r, codeIDUnavailable)
}

// ---- field validation ----

// TestPushFieldRejections mirrors forbidden-field-server-authoritative,
// unknown-field-not-declared and file-field-unsupported, plus a forged owner.
func TestPushFieldRejections(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
		code   string
	}{
		{"created", map[string]any{"created": "2026-10-15 08:30:00.000Z"}, codeForbiddenField},
		{"updated", map[string]any{"updated": "2026-10-15 08:30:00.000Z"}, codeForbiddenField},
		{"id", map[string]any{"id": "notesrecord0002"}, codeForbiddenField},
		{"owner", map[string]any{"owner": "someotherid0000"}, codeForbiddenField},
		{"unknown", map[string]any{"notacolumn": "x"}, codeUnknownField},
		{"file", map[string]any{"attachments": "photo.png"}, codeFileFieldUnsupported},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, Config{})
			alice := env.newUser("alice@example.com")
			tok := env.token(alice)
			env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "t", "body": "b"}))

			_, resp := env.pushMutations(tok, updateMut("m2", "notes", "notesrecord0001", 1, tc.fields))
			r := resultAt(t, resp, 0)
			wantStatus(t, r, statusRejected)
			wantCode(t, r, tc.code)
		})
	}
}

// TestPushRequiredValidationFailed checks PocketBase validation surfaces as
// validation_failed.
func TestPushRequiredValidationFailed(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	_, resp := env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"body": "no title"}))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusRejected)
	wantCode(t, r, codeValidationFailed)
}

// TestPushUniqueViolation adds a unique index and checks unique_violation.
func TestPushUniqueViolation(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	col, err := env.app.FindCollectionByNameOrId("notes")
	if err != nil {
		t.Fatal(err)
	}
	col.Fields.Add(&core.TextField{Name: "code"})
	col.AddIndex("idx_notes_code", true, "code", "code != ''")
	if err := env.app.Save(col); err != nil {
		t.Fatalf("add unique field: %v", err)
	}

	_, resp := env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "a", "code": "X"}))
	if r := resultAt(t, resp, 0); r["status"] != statusOK {
		t.Fatalf("first create = %#v", r)
	}

	_, resp = env.pushMutations(tok, createMut("m2", "notes", "notesrecord0002", map[string]any{"title": "b", "code": "X"}))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusRejected)
	wantCode(t, r, codeUniqueViolation)
}

// ---- baseRev / structural validation ----

func TestPushInvalidBaseRev(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)
	env.pushMutations(tok, createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "t", "body": "b"}))

	t.Run("greater than current", func(t *testing.T) {
		_, resp := env.pushMutations(tok, updateMut("m2", "notes", "notesrecord0001", 5, map[string]any{"title": "x"}))
		r := resultAt(t, resp, 0)
		wantStatus(t, r, statusRejected)
		wantCode(t, r, codeInvalidBaseRev)
	})

	t.Run("missing on update", func(t *testing.T) {
		_, resp := env.pushMutations(tok, map[string]any{
			"id": "m3", "collection": "notes", "recordId": "notesrecord0001", "op": "update",
			"fields": map[string]any{"title": "x"},
		})
		r := resultAt(t, resp, 0)
		wantCode(t, r, codeInvalidBaseRev)
	})

	t.Run("zero on delete", func(t *testing.T) {
		_, resp := env.pushMutations(tok, deleteMut("m4", "notes", "notesrecord0001", 0))
		r := resultAt(t, resp, 0)
		wantCode(t, r, codeInvalidBaseRev)
	})

	t.Run("present on create", func(t *testing.T) {
		_, resp := env.pushMutations(tok, map[string]any{
			"id": "m5", "collection": "notes", "recordId": "notesrecord0002", "op": "create",
			"baseRev": 1, "fields": map[string]any{"title": "x"},
		})
		r := resultAt(t, resp, 0)
		wantCode(t, r, codeInvalidBaseRev)
	})
}

func TestPushStructuralValidation(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	t.Run("collection_not_synced", func(t *testing.T) {
		_, resp := env.pushMutations(tok, createMut("m1", "nosuch", "notesrecord0001", map[string]any{"title": "x"}))
		r := resultAt(t, resp, 0)
		wantStatus(t, r, statusRejected)
		wantCode(t, r, codeCollectionNotSynced)
	})

	t.Run("invalid_op", func(t *testing.T) {
		_, resp := env.pushMutations(tok, map[string]any{
			"id": "m2", "collection": "notes", "recordId": "notesrecord0001", "op": "frobnicate",
		})
		r := resultAt(t, resp, 0)
		wantCode(t, r, codeInvalidOp)
	})

	t.Run("invalid_id", func(t *testing.T) {
		_, resp := env.pushMutations(tok, createMut("m3", "notes", "BAD_ID", map[string]any{"title": "x"}))
		r := resultAt(t, resp, 0)
		wantCode(t, r, codeInvalidID)
	})
}

// ---- batch ordering / isolation / dependency ----

// TestPushDependencyFailedSameRecord mirrors dependency-failed-same-record.
func TestPushDependencyFailedSameRecord(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	_, resp := env.pushMutations(tok,
		updateMut("m1", "notes", "notesrecord0001", 1, map[string]any{"title": "x"}),
		deleteMut("m2", "notes", "notesrecord0001", 1),
	)
	wantStatus(t, resultAt(t, resp, 0), statusRejected)
	wantCode(t, resultAt(t, resp, 0), codeNotFound)
	wantStatus(t, resultAt(t, resp, 1), statusRejected)
	wantCode(t, resultAt(t, resp, 1), codeDependencyFailed)
}

// TestPushBatchOrderingAndIsolation mirrors
// batch-ordering-and-per-mutation-isolation: a rejected middle mutation does
// not roll back the earlier one nor stop a later different-record mutation.
func TestPushBatchOrderingAndIsolation(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	_, resp := env.pushMutations(tok,
		createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "a", "body": "b"}),
		updateMut("m2", "notes", "notesrecord0002", 1, map[string]any{"title": "x"}),
		createMut("m3", "notes", "notesrecord0003", map[string]any{"title": "c", "body": "d"}),
	)
	wantStatus(t, resultAt(t, resp, 0), statusOK)
	wantRev(t, resultAt(t, resp, 0), 1)
	wantStatus(t, resultAt(t, resp, 1), statusRejected)
	wantCode(t, resultAt(t, resp, 1), codeNotFound)
	wantStatus(t, resultAt(t, resp, 2), statusOK)
	wantRev(t, resultAt(t, resp, 2), 1)

	// The two successful creates are committed and visible to pull.
	status, body := env.getJSON("/api/sync/pull", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("pull status = %d", status)
	}
	ch := changes(t, body)
	got := map[string]bool{}
	for _, c := range ch {
		got[c["recordId"].(string)] = true
	}
	if len(ch) != 2 || !got["notesrecord0001"] || !got["notesrecord0003"] {
		t.Fatalf("pull after batch = %#v", ch)
	}
}

// ---- ownership / no existence oracle ----

// TestPushCrossUserNoOracle checks update/delete of another user's record behave
// exactly as if it did not exist (protocol §6.4, §14.5).
func TestPushCrossUserNoOracle(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")
	aliceTok := env.token(alice)

	bobRec := env.saveNoteID(bob, "notesrecord0002", "bob", "b")

	_, resp := env.pushMutations(aliceTok, updateMut("m1", "notes", "notesrecord0002", 1, map[string]any{"title": "hacked"}))
	r := resultAt(t, resp, 0)
	wantStatus(t, r, statusRejected)
	wantCode(t, r, codeNotFound)

	_, resp = env.pushMutations(aliceTok, deleteMut("m2", "notes", "notesrecord0002", 1))
	r = resultAt(t, resp, 0)
	wantStatus(t, r, statusOK)

	// Bob's record is untouched.
	rec, err := env.app.FindRecordById("notes", bobRec.Id)
	if err != nil {
		t.Fatalf("bob's record disappeared: %v", err)
	}
	if rec.GetString("title") != "bob" {
		t.Fatalf("bob's record was modified: title=%q", rec.GetString("title"))
	}
	if ownerOf(rec, "owner") != bob.Id {
		t.Fatalf("bob's record changed owner")
	}
}

// ---- request-level errors ----

func TestPushRequestErrors(t *testing.T) {
	env := newTestEnv(t, Config{MaxBatch: 2})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	t.Run("missing protocol header", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"mutations": []any{deleteMut("m1", "notes", "notesrecord0001", 1)}})
		status, resp := env.pushRaw(tok, body, map[string]string{"X-Sync-Protocol": ""})
		if status != http.StatusBadRequest || resp["code"] != "protocol_unsupported" {
			t.Fatalf("status=%d resp=%#v", status, resp)
		}
	})

	t.Run("unauthorized", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"mutations": []any{deleteMut("m1", "notes", "notesrecord0001", 1)}})
		status, _ := env.pushRaw("", body, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		status, resp := env.pushRaw(tok, []byte("{not json"), nil)
		if status != http.StatusBadRequest || resp["code"] != "invalid_request" {
			t.Fatalf("status=%d resp=%#v", status, resp)
		}
	})

	t.Run("empty batch", func(t *testing.T) {
		status, resp := env.push(tok, map[string]any{"mutations": []any{}})
		if status != http.StatusBadRequest || resp["code"] != "batch_too_large" {
			t.Fatalf("status=%d resp=%#v", status, resp)
		}
	})

	t.Run("batch too large", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"mutations": []any{
			deleteMut("m1", "notes", "notesrecord0001", 1),
			deleteMut("m2", "notes", "notesrecord0002", 1),
			deleteMut("m3", "notes", "notesrecord0003", 1),
		}})
		status, resp := env.pushRaw(tok, body, nil)
		if status != http.StatusRequestEntityTooLarge || resp["code"] != "batch_too_large" {
			t.Fatalf("status=%d resp=%#v", status, resp)
		}
	})

	t.Run("batch at limit accepted", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"mutations": []any{
			deleteMut("m4", "notes", "notesrecord0004", 1),
			deleteMut("m5", "notes", "notesrecord0005", 1),
		}})
		status, resp := env.pushRaw(tok, body, nil)
		if status != http.StatusOK {
			t.Fatalf("batch exactly maxBatch rejected: status=%d resp=%#v", status, resp)
		}
		if len(resp["results"].([]any)) != 2 {
			t.Fatalf("results = %#v", resp["results"])
		}
	})

	t.Run("body too large", func(t *testing.T) {
		small := newTestEnv(t, Config{MaxBodyBytes: 60})
		u := small.newUser("carol@example.com")
		big := map[string]any{"mutations": []any{
			createMut("m1", "notes", "notesrecord0001", map[string]any{"title": "this title is comfortably longer than sixty bytes"}),
		}}
		status, resp := small.push(small.token(u), big)
		if status != http.StatusRequestEntityTooLarge || resp["code"] != "batch_too_large" {
			t.Fatalf("status=%d resp=%#v", status, resp)
		}
	})
}
