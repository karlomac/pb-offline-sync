package syncplugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// Fixture execution driver (protocol §15, E2E-1 / S-CONV-8).
//
// conformance_test.go only validates that the 24 fixtures are well-formed. This
// file replays each fixture's steps against a live test app: a fresh server per
// fixture, configured from the fixture's own `config`, then the push/pull steps
// in order with a subset match on `expect`. The TS client suite drives the same
// fixtures; a green run on both sides means both agree with docs/protocol.md.

const drvUsersCollection = "syncusers"

type drvConfig struct {
	Collections map[string]drvCollection `json:"collections"`
}

type drvCollection struct {
	OwnerField     string     `json:"ownerField"`
	ConflictPolicy string     `json:"conflictPolicy"`
	DeletePolicy   string     `json:"deletePolicy"`
	Fields         []drvField `json:"fields"`
}

type drvField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Writable bool   `json:"writable"`
}

type drvPull struct {
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

// TestFixtureDriverExecutesAllFixtures walks every conformance fixture and runs
// it. Each fixture is its own subtest so one failure does not hide the rest.
func TestFixtureDriverExecutesAllFixtures(t *testing.T) {
	dir := filepath.Join("..", "..", "conformance", "fixtures")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixtures dir %q: %v", dir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("no fixtures found in %q", dir)
	}

	executed := 0
	for _, file := range files {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Errorf("%s: read: %v", file, err)
			continue
		}
		var f fixture
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Errorf("%s: invalid JSON: %v", file, err)
			continue
		}
		if !t.Run(f.Name, func(t *testing.T) { drvRunFixture(t, f) }) {
			continue
		}
		executed++
		t.Logf("fixture %s: PASS", f.Name)
	}
	t.Logf("fixture driver executed %d/%d fixtures", executed, len(files))
}

// drvRunFixture builds a server for one fixture and replays its steps in order.
func drvRunFixture(t *testing.T, f fixture) {
	t.Helper()

	var fc drvConfig
	if err := json.Unmarshal(f.Config, &fc); err != nil {
		t.Fatalf("config: %v", err)
	}
	env := drvNewEnv(t, fc)

	tokens := make(map[string]string, len(f.Users))
	for _, name := range f.Users {
		tokens[name] = env.token(env.newUser(name + "@example.com"))
	}

	prevCursor := ""
	for i, step := range f.Steps {
		tok, ok := tokens[step.As]
		if !ok {
			t.Fatalf("step %d: unknown user %q", i, step.As)
		}
		drvRunStep(t, env, fmt.Sprintf("step %d", i), step, tok, &prevCursor)
	}
}

// drvNewEnv is newTestEnv parameterised by the fixture's own collection schema
// (the hardcoded notes schema in helpers_test.go cannot be reused).
func drvNewEnv(t *testing.T, fc drvConfig) *testEnv {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(app.Cleanup)

	users := core.NewAuthCollection(drvUsersCollection)
	if err := app.Save(users); err != nil {
		t.Fatalf("save users collection: %v", err)
	}

	cfg := Config{Collections: make(map[string]CollectionConfig, len(fc.Collections))}
	for name, c := range fc.Collections {
		col := core.NewBaseCollection(name)
		for _, f := range c.Fields {
			col.Fields.Add(drvBuildField(t, f))
		}
		col.Fields.Add(&core.RelationField{Name: c.OwnerField, CollectionId: users.Id, MaxSelect: 1})
		if err := app.Save(col); err != nil {
			t.Fatalf("save collection %q: %v", name, err)
		}
		cfg.Collections[name] = CollectionConfig{
			OwnerField:   c.OwnerField,
			Conflict:     ConflictPolicy(c.ConflictPolicy),
			DeletePolicy: DeletePolicy(c.DeletePolicy),
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

// drvBuildField maps a fixture field descriptor to a PocketBase field. The
// fixtures use text/editor/file only; anything else fails loudly rather than
// silently changing the schema the fixture was written against.
func drvBuildField(t *testing.T, f drvField) core.Field {
	t.Helper()
	switch f.Type {
	case "text":
		return &core.TextField{Name: f.Name}
	case "editor":
		return &core.EditorField{Name: f.Name}
	case "file":
		return &core.FileField{Name: f.Name, MaxSelect: 1}
	default:
		t.Fatalf("fixture driver: unsupported field type %q for %q", f.Type, f.Name)
		return nil
	}
}

func drvRunStep(t *testing.T, env *testEnv, ctx string, step fixtureStep, tok string, prevCursor *string) {
	t.Helper()

	if isSet(step.Push) {
		status, body := env.pushRaw(tok, []byte(step.Push), nil)
		drvCheck(t, ctx, step.Expect, body, status)
		return
	}

	var p drvPull
	if err := json.Unmarshal(step.Pull, &p); err != nil {
		t.Fatalf("%s: invalid pull: %v", ctx, err)
	}
	cur := p.Cursor
	switch cur {
	case "", "none":
		cur = ""
	case "$prev":
		cur = *prevCursor
	case "$stale":
		// Decodable but ahead of head: built with the server's own encoder (§7.4).
		cur = encodeCursor(cursor{Kind: cursorKindIncremental, Seq: 1 << 62})
	}

	path := "/api/sync/pull"
	sep := "?"
	if p.Limit > 0 {
		path += fmt.Sprintf("?limit=%d", p.Limit)
		sep = "&"
	}
	if cur != "" {
		path += sep + "cursor=" + url.QueryEscape(cur)
	}

	status, body := env.getJSON(path, tok, nil)
	if status == http.StatusOK {
		if c, ok := body["cursor"].(string); ok {
			*prevCursor = c
		}
	}
	drvCheck(t, ctx, step.Expect, body, status)
}

// drvCheck asserts a step's `expect` against the response.
func drvCheck(t *testing.T, ctx string, expect json.RawMessage, body map[string]any, status int) {
	t.Helper()

	var want any
	if err := json.Unmarshal(expect, &want); err != nil {
		t.Fatalf("%s: invalid expect: %v", ctx, err)
	}

	// A request-level expect {status, code} also pins the HTTP status.
	if wm, ok := want.(map[string]any); ok {
		if ws, present := wm["status"]; present {
			if wi, ok := drvInt(ws); !ok || wi != status {
				t.Fatalf("%s: HTTP status %d, want %v", ctx, status, ws)
			}
		}
	}

	drvMatch(t, ctx, want, body)
}

// drvMatch implements the subset-match rule in conformance/README.md: every
// listed key must match recursively; arrays are ordered and equal-length; the
// wildcards $any/$string/$int match any/any-string/any-integer.
func drvMatch(t *testing.T, path string, want, got any) {
	t.Helper()

	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("%s: want an object, got %T (%v)", path, got, got)
		}
		for k, wv := range w {
			gv, present := g[k]
			if !present {
				t.Fatalf("%s: missing key %q (got %v)", path, k, g)
			}
			drvMatch(t, path+"."+k, wv, gv)
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			t.Fatalf("%s: want an array, got %T (%v)", path, got, got)
		}
		if len(g) != len(w) {
			t.Fatalf("%s: array length %d, want %d (%v)", path, len(g), len(w), g)
		}
		for i := range w {
			drvMatch(t, fmt.Sprintf("%s[%d]", path, i), w[i], g[i])
		}
	case string:
		switch w {
		case "$any":
			return
		case "$string":
			if _, ok := got.(string); !ok {
				t.Fatalf("%s: want any string, got %T (%v)", path, got, got)
			}
			return
		case "$int":
			if _, ok := drvInt(got); !ok {
				t.Fatalf("%s: want any integer, got %T (%v)", path, got, got)
			}
			return
		}
		g, ok := got.(string)
		if !ok || g != w {
			t.Fatalf("%s: got %v, want %q", path, got, w)
		}
	case float64:
		wi, ok := drvInt(w)
		if !ok {
			t.Fatalf("%s: non-integer expected number %v", path, w)
		}
		gi, ok := drvInt(got)
		if !ok || gi != wi {
			t.Fatalf("%s: got %v, want %v", path, got, w)
		}
	case bool:
		g, ok := got.(bool)
		if !ok || g != w {
			t.Fatalf("%s: got %v, want %v", path, got, w)
		}
	case nil:
		if got != nil {
			t.Fatalf("%s: got %v, want null", path, got)
		}
	default:
		t.Fatalf("%s: unsupported expected type %T", path, want)
	}
}

func drvInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}
