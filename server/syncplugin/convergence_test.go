package syncplugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
)

// Convergence harness (PDD §11.3 — the key correctness proof).
//
// A reference client model implements the protocol §11 rules (local outbox with
// §11.1 coalescing, push-result handling §6.5, apply-by-rev pull §11.3, 410 →
// full resync §11.5) and is driven over the real handlers. Multiple *devices of
// the same user* (the actual multi-device scenario) edit concurrently under a
// seeded random schedule; at quiescence every device must equal the server.
//
// It proves convergence and, in the targeted tests, that acknowledged writes are
// not lost. It does NOT exercise the TypeScript engine or a real network.

// ---- reference device model ----

type devRec struct {
	rev   int
	title string
	body  string
}

type devMut struct {
	id      string
	op      string
	recID   string
	baseRev *int
	fields  map[string]any
}

type device struct {
	t          *testing.T
	name       string
	token      string
	uid        string
	local      map[string]*devRec
	pending    []devMut
	cursor     string
	seq        int
	needResync bool
}

func (d *device) nextMutID() string {
	d.seq++
	return fmt.Sprintf("%s-%d", d.name, d.seq)
}

// coalesce implements protocol §11.1: update+update merge (union of fields,
// earliest baseRev); create+update collapses to create; create+delete cancels.
func (d *device) coalesce(m devMut) {
	idx := -1
	for i := len(d.pending) - 1; i >= 0; i-- {
		if d.pending[i].recID == m.recID {
			idx = i
			break
		}
	}
	if idx == -1 {
		d.pending = append(d.pending, m)
		return
	}
	last := &d.pending[idx]
	if last.op == "create" && m.op == "update" {
		for k, v := range m.fields {
			last.fields[k] = v
		}
		return
	}
	if last.op == "create" && m.op == "delete" {
		d.pending = append(d.pending[:idx], d.pending[idx+1:]...)
		return
	}
	if last.op == "update" && m.op == "update" {
		for k, v := range m.fields {
			last.fields[k] = v
		}
		if m.baseRev != nil && (last.baseRev == nil || *m.baseRev < *last.baseRev) {
			last.baseRev = m.baseRev
		}
		return
	}
	d.pending = append(d.pending, m)
}

func (d *device) create(recID, title, body string) {
	d.local[recID] = &devRec{rev: 0, title: title, body: body}
	d.coalesce(devMut{id: d.nextMutID(), op: "create", recID: recID, fields: map[string]any{"title": title, "body": body}})
}

func (d *device) update(recID, field, val string) {
	r, ok := d.local[recID]
	if !ok {
		return
	}
	if field == "title" {
		r.title = val
	} else {
		r.body = val
	}
	b := r.rev
	d.coalesce(devMut{id: d.nextMutID(), op: "update", recID: recID, baseRev: &b, fields: map[string]any{field: val}})
}

func (d *device) remove(recID string) {
	r, ok := d.local[recID]
	if !ok {
		return
	}
	b := r.rev
	delete(d.local, recID)
	d.coalesce(devMut{id: d.nextMutID(), op: "delete", recID: recID, baseRev: &b})
}

func (d *device) sync(env *testEnv) {
	d.pushAll(env)
	d.pullAll(env)
}

func (d *device) pushAll(env *testEnv) {
	for len(d.pending) > 0 {
		n := len(d.pending)
		if n > 50 {
			n = 50
		}
		batch := append([]devMut(nil), d.pending[:n]...)
		code, resp := postPush(env, d.token, batch)
		if code != http.StatusOK {
			d.t.Fatalf("%s: push HTTP %d: %v", d.name, code, resp)
		}
		results := resultList(d.t, resp)
		if len(results) != len(batch) {
			d.t.Fatalf("%s: %d results for %d mutations: %v", d.name, len(results), len(batch), resp)
		}
		byID := map[string]devMut{}
		for _, m := range batch {
			byID[m.id] = m
		}
		for _, r := range results {
			id, _ := r["id"].(string)
			m, ok := byID[id]
			if !ok {
				d.t.Fatalf("%s: result for unknown mutation %q", d.name, id)
			}
			d.applyPushResult(m, r)
		}
		d.pending = d.pending[n:]
	}
}

func (d *device) applyPushResult(m devMut, r map[string]any) {
	status, _ := r["status"].(string)
	switch status {
	case "ok", "duplicate":
		if m.op == "delete" {
			delete(d.local, m.recID)
			return
		}
		rec, ok := r["record"].(map[string]any)
		if !ok {
			d.t.Fatalf("%s: %s %s returned %q without a record: %v", d.name, m.op, m.recID, status, r)
		}
		d.local[m.recID] = &devRec{rev: jsonInt(r["rev"]), title: str(rec["title"]), body: str(rec["body"])}
	case "conflict":
		code, _ := r["code"].(string)
		if code == "deleted" {
			delete(d.local, m.recID)
			return
		}
		rec, ok := r["record"].(map[string]any)
		if !ok {
			d.t.Fatalf("%s: conflict on %s without a record: %v", d.name, m.recID, r)
		}
		d.local[m.recID] = &devRec{rev: jsonInt(r["rev"]), title: str(rec["title"]), body: str(rec["body"])}
	case "rejected":
		code, _ := r["code"].(string)
		if code != "dependency_failed" {
			// The harness generates only valid, own-record, fresh-id mutations.
			d.t.Fatalf("%s: unexpected %q for %s %s: %v", d.name, status, m.op, m.recID, r)
		}
		// §6.2.3: a later same-record mutation after a non-ok result is not
		// applied. Drop it and resync (§11.5) to reconcile local state.
		d.needResync = true
	default:
		d.t.Fatalf("%s: unexpected push status %q for %s %s: %v", d.name, status, m.op, m.recID, r)
	}
}

func (d *device) pullAll(env *testEnv) {
	if d.needResync {
		d.needResync = false
		d.resyncPull(env)
		return
	}
	for i := 0; i < 10000; i++ {
		code, resp := getPull(env, d.token, d.cursor, 200)
		if code == http.StatusGone {
			d.resyncPull(env) // protocol §11.5: full resync
			return
		}
		if code != http.StatusOK {
			d.t.Fatalf("%s: pull HTTP %d: %v", d.name, code, resp)
		}
		for _, ch := range changes(d.t, resp) {
			d.applyChange(ch)
		}
		if c, ok := resp["cursor"].(string); ok {
			d.cursor = c
		}
		if more, _ := resp["hasMore"].(bool); !more {
			return
		}
	}
	d.t.Fatalf("%s: pull did not terminate", d.name)
}

// resyncPull performs protocol §11.5: discard the cursor, replay the snapshot,
// then drop local synced rows that were not delivered and have no pending write.
func (d *device) resyncPull(env *testEnv) {
	delivered := map[string]bool{}
	cursor := ""
	for i := 0; i < 10000; i++ {
		code, resp := getPull(env, d.token, cursor, 200)
		if code == http.StatusGone {
			cursor = ""
			continue
		}
		if code != http.StatusOK {
			d.t.Fatalf("%s: resync pull HTTP %d: %v", d.name, code, resp)
		}
		for _, ch := range changes(d.t, resp) {
			delivered[str(ch["recordId"])] = true
			d.applyChange(ch)
		}
		if c, ok := resp["cursor"].(string); ok {
			cursor = c
		}
		if more, _ := resp["hasMore"].(bool); !more {
			break
		}
	}
	pending := map[string]bool{}
	for _, m := range d.pending {
		pending[m.recID] = true
	}
	for id := range d.local {
		if !delivered[id] && !pending[id] {
			delete(d.local, id)
		}
	}
	d.cursor = cursor
}

func (d *device) applyChange(ch map[string]any) {
	recID, _ := ch["recordId"].(string)
	op, _ := ch["op"].(string)
	rev := jsonInt(ch["rev"])
	if op == "delete" {
		if cur, ok := d.local[recID]; ok && cur.rev <= rev {
			delete(d.local, recID)
		}
		return
	}
	rec, _ := ch["record"].(map[string]any)
	if rec == nil {
		return
	}
	if cur, ok := d.local[recID]; ok && cur.rev >= rev {
		return // stale or duplicate (protocol §11.3)
	}
	d.local[recID] = &devRec{rev: rev, title: str(rec["title"]), body: str(rec["body"])}
}

func (d *device) randomLive(rng *rand.Rand) string {
	if len(d.local) == 0 {
		return ""
	}
	ids := make([]string, 0, len(d.local))
	for id := range d.local {
		ids = append(ids, id)
	}
	return ids[rng.Intn(len(ids))]
}

// ---- randomized convergence ----

func TestConvergenceRandomized(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 4, 5, 6, 7, 8} {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) { runRandomConvergence(t, seed, ServerWins) })
	}
}

// TestConvergenceRandomizedClientWins runs the same randomized multi-device
// harness under the clientWins conflict policy (S-CONV-6): a stale overlapping
// edit is applied rather than rejected, and every device must still converge.
func TestConvergenceRandomizedClientWins(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 4, 5, 6, 7, 8} {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) { runRandomConvergence(t, seed, ClientWins) })
	}
}

func runRandomConvergence(t *testing.T, seed int64, conflict ConflictPolicy) {
	rng := rand.New(rand.NewSource(seed))
	env := newTestEnv(t, Config{Collections: map[string]CollectionConfig{
		"notes": {OwnerField: "owner", Conflict: conflict, DeletePolicy: DeleteWins},
	}})

	// Two users, three devices each: concurrency is within one user's devices,
	// which is the real multi-device case (records are per-owner).
	var devices []*device
	for _, u := range []string{"alice", "bob"} {
		usr := env.newUser(u + "@example.com")
		tok := env.token(usr)
		for i := 0; i < 3; i++ {
			devices = append(devices, &device{
				t:     t,
				name:  fmt.Sprintf("%s-d%d", u, i),
				token: tok,
				uid:   usr.Id,
				local: map[string]*devRec{},
			})
		}
	}

	for r := 0; r < 80; r++ {
		d := devices[rng.Intn(len(devices))]
		for o := rng.Intn(4); o > 0; o-- {
			switch rng.Intn(3) {
			case 0:
				d.create(randomRecordID(rng), fmt.Sprintf("t%d", r), fmt.Sprintf("b%d", r))
			case 1:
				if id := d.randomLive(rng); id != "" {
					if rng.Intn(2) == 0 {
						d.update(id, "title", fmt.Sprintf("T%d", r))
					} else {
						d.update(id, "body", fmt.Sprintf("B%d", r))
					}
				}
			case 2:
				if id := d.randomLive(rng); id != "" {
					d.remove(id)
				}
			}
		}
		// Sometimes buffer offline (skip the sync) to exercise outbox + resync.
		if rng.Intn(4) != 0 {
			d.sync(env)
		}
	}

	for pass := 0; pass < 3; pass++ {
		for _, d := range devices {
			d.sync(env)
		}
	}

	for _, d := range devices {
		want := serverTruth(t, env, d.uid)
		if len(d.local) != len(want) {
			t.Fatalf("seed %d: %s has %d records, server has %d", seed, d.name, len(d.local), len(want))
		}
		for id, wr := range want {
			got, ok := d.local[id]
			if !ok {
				t.Fatalf("seed %d: %s is missing record %s", seed, d.name, id)
			}
			if got.rev != wr.rev || got.title != wr.title || got.body != wr.body {
				t.Fatalf("seed %d: %s diverged on %s: got %+v want %+v", seed, d.name, id, got, wr)
			}
		}
	}
}

// ---- targeted properties ----

// TestNoLostAcknowledgedWrite: a write the server acknowledged must reach the
// user's other devices.
func TestNoLostAcknowledgedWrite(t *testing.T) {
	env := newTestEnv(t, Config{Collections: map[string]CollectionConfig{
		"notes": {OwnerField: "owner", Conflict: ServerWins, DeletePolicy: DeleteWins},
	}})
	usr := env.newUser("alice@example.com")
	tok := env.token(usr)
	d1 := &device{t: t, name: "d1", token: tok, uid: usr.Id, local: map[string]*devRec{}}
	d2 := &device{t: t, name: "d2", token: tok, uid: usr.Id, local: map[string]*devRec{}}

	const id = "notesconverge01"
	d1.create(id, "hello", "world")
	d1.sync(env)

	d2.sync(env)
	if r, ok := d2.local[id]; !ok || r.title != "hello" || r.body != "world" {
		t.Fatalf("d2 did not converge on the acknowledged create: %+v", d2.local[id])
	}

	d1.update(id, "title", "hello2")
	d1.sync(env)
	d2.sync(env)
	if r, ok := d2.local[id]; !ok || r.title != "hello2" || r.rev != 2 {
		t.Fatalf("d2 did not converge on the acknowledged update: %+v", d2.local[id])
	}
}

// TestDuplicateDeliveryIsIdempotent: resending an acknowledged mutation returns
// duplicate and does not double-apply.
func TestDuplicateDeliveryIsIdempotent(t *testing.T) {
	env := newTestEnv(t, Config{Collections: map[string]CollectionConfig{
		"notes": {OwnerField: "owner", Conflict: ServerWins, DeletePolicy: DeleteWins},
	}})
	usr := env.newUser("alice@example.com")
	d := &device{t: t, name: "d", token: env.token(usr), uid: usr.Id, local: map[string]*devRec{}}

	const id = "duprecord000001"
	d.create(id, "a", "b")
	original := d.pending[0]
	d.sync(env)

	code, resp := postPush(env, d.token, []devMut{original})
	if code != http.StatusOK {
		t.Fatalf("replay HTTP %d: %v", code, resp)
	}
	results := resultList(t, resp)
	if len(results) != 1 || results[0]["status"] != "duplicate" {
		t.Fatalf("replay was not duplicate: %v", resp)
	}
	if got := serverTruth(t, env, usr.Id); len(got) != 1 || got[id].rev != 1 {
		t.Fatalf("replay changed server state: %+v", got)
	}
}

// TestResyncOn410AfterPrune: pruning the log makes an old cursor stale (410);
// the device resyncs via a snapshot and converges, preserving local records.
func TestResyncOn410AfterPrune(t *testing.T) {
	env := newTestEnv(t, Config{Collections: map[string]CollectionConfig{
		"notes": {OwnerField: "owner", Conflict: ServerWins, DeletePolicy: DeleteWins},
	}})
	usr := env.newUser("alice@example.com")
	d := &device{t: t, name: "d", token: env.token(usr), uid: usr.Id, local: map[string]*devRec{}}

	for i := 0; i < 5; i++ {
		d.create(randomRecordID(rand.New(rand.NewSource(int64(i)))), fmt.Sprintf("t%d", i), "b")
	}
	d.sync(env)
	oldCursor := d.cursor
	if oldCursor == "" {
		t.Fatal("expected a cursor after the first sync")
	}

	// Age the whole log out, then confirm the old cursor is now rejected.
	if err := Prune(env.app, Config{
		Collections:              map[string]CollectionConfig{"notes": {OwnerField: "owner"}},
		ChangeLogRetention:       time.Nanosecond,
		IdempotencyRetentionDays: 1,
	}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	code, _ := getPull(env, d.token, oldCursor, 200)
	if code != http.StatusGone {
		t.Fatalf("stale cursor after prune: got HTTP %d, want 410", code)
	}

	// The device resyncs (its pullAll treats 410 as "drop the cursor").
	d.sync(env)
	want := serverTruth(t, env, usr.Id)
	if len(d.local) != len(want) {
		t.Fatalf("after resync: %d local, %d server", len(d.local), len(want))
	}
	for id, wr := range want {
		if got, ok := d.local[id]; !ok || got.rev != wr.rev || got.title != wr.title {
			t.Fatalf("after resync, %s diverged: got %+v want %+v", id, d.local[id], wr)
		}
	}
}

// TestDeleteVsEditEditWins: under editWins a stale delete conflicts and leaves
// the edited record intact on every device.
func TestDeleteVsEditEditWins(t *testing.T) {
	env := newTestEnv(t, Config{Collections: map[string]CollectionConfig{
		"notes": {OwnerField: "owner", Conflict: ServerWins, DeletePolicy: EditWins},
	}})
	usr := env.newUser("alice@example.com")
	tok := env.token(usr)
	d1 := &device{t: t, name: "d1", token: tok, uid: usr.Id, local: map[string]*devRec{}}
	d2 := &device{t: t, name: "d2", token: tok, uid: usr.Id, local: map[string]*devRec{}}

	const id = "deleditrecord01"
	d1.create(id, "v1", "b")
	d1.sync(env)
	d2.sync(env)

	d1.update(id, "title", "v2") // server rev -> 2
	d1.sync(env)

	d2.remove(id) // baseRev 1, stale -> editWins -> conflict/edited_since
	d2.pushAll(env)
	d2.sync(env)

	want := serverTruth(t, env, usr.Id)
	if wr, ok := want[id]; !ok || wr.title != "v2" || wr.rev != 2 {
		t.Fatalf("server record changed by editWins delete: %+v", want)
	}
	if got, ok := d2.local[id]; !ok || got.title != "v2" {
		t.Fatalf("d2 lost the edited record: %+v", d2.local[id])
	}
}

// TestClientWinsConvergence: S-CONV-6. Two devices edit the same field from the
// same base rev under clientWins; the later push wins (applied, conflictId
// recorded) and both devices converge on the winning value.
func TestClientWinsConvergence(t *testing.T) {
	env := newTestEnv(t, Config{Collections: map[string]CollectionConfig{
		"notes": {OwnerField: "owner", Conflict: ClientWins, DeletePolicy: DeleteWins},
	}})
	usr := env.newUser("alice@example.com")
	tok := env.token(usr)
	d1 := &device{t: t, name: "d1", token: tok, uid: usr.Id, local: map[string]*devRec{}}
	d2 := &device{t: t, name: "d2", token: tok, uid: usr.Id, local: map[string]*devRec{}}

	const id = "clientwinsrec01"
	d1.create(id, "v1", "b")
	d1.sync(env)
	d2.sync(env)

	d1.update(id, "title", "d1") // server rev -> 2, title d1
	d1.sync(env)

	d2.update(id, "title", "d2") // baseRev 1, stale overlap -> clientWins applies
	d2.sync(env)

	if got := countConflicts(t, env); got != 1 {
		t.Fatalf("clientWins overlap recorded %d conflicts, want 1", got)
	}

	want := serverTruth(t, env, usr.Id)
	if wr, ok := want[id]; !ok || wr.title != "d2" || wr.rev != 3 {
		t.Fatalf("server did not apply the clientWins edit: %+v", want)
	}

	d1.sync(env)
	for _, d := range []*device{d1, d2} {
		got, ok := d.local[id]
		if !ok || got.rev != want[id].rev || got.title != "d2" || got.body != "b" {
			t.Fatalf("%s did not converge: got %+v want %+v", d.name, got, want[id])
		}
	}
}

// ---- helpers ----

type truthRec struct {
	rev   int
	title string
	body  string
}

func serverTruth(t *testing.T, env *testEnv, uid string) map[string]truthRec {
	t.Helper()
	recs, err := env.app.FindRecordsByFilter("notes", "owner = {:u}", "id", 0, 0, dbx.Params{"u": uid})
	if err != nil {
		t.Fatalf("serverTruth: %v", err)
	}
	out := map[string]truthRec{}
	for _, r := range recs {
		out[r.Id] = truthRec{
			rev:   metaRev(t, env.app, "notes", r.Id),
			title: r.GetString("title"),
			body:  r.GetString("body"),
		}
	}
	return out
}

func postPush(env *testEnv, token string, muts []devMut) (int, map[string]any) {
	arr := make([]map[string]any, 0, len(muts))
	for _, m := range muts {
		mm := map[string]any{"id": m.id, "collection": "notes", "recordId": m.recID, "op": m.op}
		if m.baseRev != nil {
			mm["baseRev"] = *m.baseRev
		}
		if m.fields != nil {
			mm["fields"] = m.fields
		}
		arr = append(arr, mm)
	}
	raw, err := json.Marshal(map[string]any{"mutations": arr})
	if err != nil {
		env.t.Fatalf("marshal push: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sync/push", bytes.NewReader(raw))
	req.Header.Set("X-Sync-Protocol", "1")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	out := map[string]any{}
	if body := rec.Body.Bytes(); len(body) > 0 {
		_ = json.Unmarshal(body, &out)
	}
	return rec.Code, out
}

func getPull(env *testEnv, token, cursor string, limit int) (int, map[string]any) {
	path := fmt.Sprintf("/api/sync/pull?limit=%d", limit)
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	return env.getJSON(path, token, nil)
}

func resultList(t *testing.T, resp map[string]any) []map[string]any {
	t.Helper()
	raw, ok := resp["results"].([]any)
	if !ok {
		t.Fatalf("push response has no results array: %#v", resp)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("push result is not an object: %#v", r)
		}
		out = append(out, m)
	}
	return out
}

func randomRecordID(rng *rand.Rand) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 15)
	for i := range b {
		b[i] = charset[rng.Intn(len(charset))]
	}
	return string(b)
}

func jsonInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
