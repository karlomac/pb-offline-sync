package syncplugin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// testEnv is a booted test app with the plugin registered, the users/notes
// collections created and the routes built into an http.Handler.
type testEnv struct {
	t   *testing.T
	app *tests.TestApp
	mux http.Handler
}

func newTestEnv(t *testing.T, cfg Config) *testEnv {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(app.Cleanup)

	users := core.NewAuthCollection("syncusers")
	if err := app.Save(users); err != nil {
		t.Fatalf("save users collection: %v", err)
	}

	notes := core.NewBaseCollection("notes")
	notes.Fields.Add(&core.TextField{Name: "title", Required: true})
	notes.Fields.Add(&core.EditorField{Name: "body"})
	notes.Fields.Add(&core.FileField{Name: "attachments", MaxSelect: 3})
	notes.Fields.Add(&core.RelationField{Name: "owner", CollectionId: users.Id, MaxSelect: 1})
	if err := app.Save(notes); err != nil {
		t.Fatalf("save notes collection: %v", err)
	}

	if cfg.Collections == nil {
		cfg.Collections = map[string]CollectionConfig{
			"notes": {OwnerField: "owner", Conflict: ServerWins, DeletePolicy: DeleteWins},
		}
	}

	if cfg.Media != nil {
		name := cfg.Media.Collection
		if name == "" {
			name = defaultMediaCollection
		}
		if _, err := EnsureMediaCollection(app, name, users.Id, cfg.Media.MaxFileSize, cfg.Media.PosterMaxBytes); err != nil {
			t.Fatalf("ensure media collection: %v", err)
		}
	}

	MustRegister(app, cfg)
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	serve := &core.ServeEvent{App: app, Router: router}
	if err := app.OnServe().Trigger(serve, func(e *core.ServeEvent) error { return e.Next() }); err != nil {
		t.Fatalf("OnServe: %v", err)
	}
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatalf("BuildMux: %v", err)
	}

	return &testEnv{t: t, app: app, mux: mux}
}

func (env *testEnv) newUser(email string) *core.Record {
	env.t.Helper()
	col, err := env.app.FindCollectionByNameOrId("syncusers")
	if err != nil {
		env.t.Fatalf("find users: %v", err)
	}
	u := core.NewRecord(col)
	u.Set("email", email)
	u.Set("password", "1234567890")
	u.Set("passwordConfirm", "1234567890")
	u.Set("verified", true)
	if err := env.app.Save(u); err != nil {
		env.t.Fatalf("save user %s: %v", email, err)
	}
	return u
}

func (env *testEnv) token(u *core.Record) string {
	env.t.Helper()
	tok, err := u.NewAuthToken()
	if err != nil {
		env.t.Fatalf("auth token: %v", err)
	}
	return tok
}

// saveNote writes a record through the normal PocketBase save path (hooks fire).
func (env *testEnv) saveNote(owner *core.Record, title, body string) *core.Record {
	env.t.Helper()
	return env.saveNoteID(owner, "", title, body)
}

// saveNoteID is saveNote with an explicit PocketBase-format record id.
func (env *testEnv) saveNoteID(owner *core.Record, id, title, body string) *core.Record {
	env.t.Helper()
	col, err := env.app.FindCollectionByNameOrId("notes")
	if err != nil {
		env.t.Fatalf("find notes: %v", err)
	}
	rec := core.NewRecord(col)
	if id != "" {
		rec.Id = id
	}
	rec.Set("title", title)
	if body != "" {
		rec.Set("body", body)
	}
	if owner != nil {
		rec.Set("owner", owner.Id)
	}
	if err := env.app.Save(rec); err != nil {
		env.t.Fatalf("save note: %v", err)
	}
	return rec
}

func (env *testEnv) request(method, path, token string, headers map[string]string) (*http.Response, []byte) {
	return env.doRequest(method, path, token, headers, nil)
}

// doRequest is request with an optional request body (media upload channel).
func (env *testEnv) doRequest(method, path, token string, headers map[string]string, body []byte) (*http.Response, []byte) {
	env.t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if _, ok := headers["X-Sync-Protocol"]; !ok {
		req.Header.Set("X-Sync-Protocol", "1")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	res := rec.Result()
	body, _ = io.ReadAll(res.Body)
	return res, body
}

// doJSON runs a request and decodes the response body as a JSON object.
func (env *testEnv) doJSON(method, path, token string, headers map[string]string, body []byte) (int, http.Header, map[string]any) {
	env.t.Helper()
	res, raw := env.doRequest(method, path, token, headers, body)
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			out = map[string]any{}
		}
	}
	return res.StatusCode, res.Header, out
}

func (env *testEnv) getJSON(path, token string, headers map[string]string) (int, map[string]any) {
	env.t.Helper()
	res, body := env.request(http.MethodGet, path, token, headers)
	out := map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			env.t.Fatalf("decode response %s: %v\n%s", path, err, body)
		}
	}
	return res.StatusCode, out
}

// changes extracts the changes array from a pull response.
func changes(t *testing.T, resp map[string]any) []map[string]any {
	t.Helper()
	raw, ok := resp["changes"].([]any)
	if !ok {
		t.Fatalf("response has no changes array: %#v", resp)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		m, ok := c.(map[string]any)
		if !ok {
			t.Fatalf("change entry is not an object: %#v", c)
		}
		out = append(out, m)
	}
	return out
}

// metaRev reads the current sync_meta rev for a record.
func metaRev(t *testing.T, app core.App, collection, recordID string) int {
	t.Helper()
	var row struct{ Rev int }
	if err := app.DB().NewQuery("SELECT rev FROM sync_meta WHERE collection = {:c} AND record_id = {:r}").
		Bind(map[string]any{"c": collection, "r": recordID}).One(&row); err != nil {
		t.Fatalf("read sync_meta rev: %v", err)
	}
	return row.Rev
}

// changeRow is the subset of sync_changes needed by tests.
type changeRow struct {
	Seq           int64  `db:"seq"`
	Op            string `db:"op"`
	Rev           int    `db:"rev"`
	Owner         string `db:"owner"`
	ChangedFields string `db:"changed_fields"`
}

func lastChange(t *testing.T, app core.App, collection, recordID string) changeRow {
	t.Helper()
	var row changeRow
	if err := app.DB().NewQuery(
		"SELECT seq, op, rev, owner, changed_fields FROM sync_changes WHERE collection = {:c} AND record_id = {:r} ORDER BY seq DESC LIMIT 1",
	).Bind(map[string]any{"c": collection, "r": recordID}).One(&row); err != nil {
		t.Fatalf("read sync_changes: %v", err)
	}
	return row
}

func countChanges(t *testing.T, app core.App) int {
	t.Helper()
	var n int
	if err := app.DB().NewQuery("SELECT COUNT(*) FROM sync_changes").Row(&n); err != nil {
		t.Fatalf("count sync_changes: %v", err)
	}
	return n
}
