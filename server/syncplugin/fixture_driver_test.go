package syncplugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// Fixture execution driver (protocol §15, E2E-1 / S-CONV-8).
//
// conformance_test.go only validates that every fixture is well-formed. This
// file replays each fixture's steps against a live test app: a fresh server per
// fixture, configured from the fixture's own `config`, then the push/pull/media
// steps in order with a subset match on `expect`. The TS client suite drives
// the same fixtures; a green run on both sides means both agree with
// docs/protocol.md.

const drvUsersCollection = "syncusers"

type drvConfig struct {
	Collections map[string]drvCollection `json:"collections"`
	Media       *drvMedia                `json:"media"`
}

type drvMedia struct {
	Collection   string `json:"collection"`
	MaxFileSize  int64  `json:"maxFileSize"`
	PerUserQuota int64  `json:"perUserQuota"`
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
	ms := drvMediaState{}
	for i, step := range f.Steps {
		tok, ok := tokens[step.As]
		if !ok {
			t.Fatalf("step %d: unknown user %q", i, step.As)
		}
		drvRunStep(t, env, fmt.Sprintf("step %d", i), step, tok, &prevCursor, f, &ms)
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
	if fc.Media != nil {
		name := fc.Media.Collection
		if name == "" {
			name = defaultMediaCollection
		}
		cfg.Media = &MediaConfig{
			Collection:   name,
			MaxFileSize:  fc.Media.MaxFileSize,
			PerUserQuota: fc.Media.PerUserQuota,
		}
		if _, err := EnsureMediaCollection(app, name, users.Id, fc.Media.MaxFileSize, cfg.Media.PosterMaxBytes); err != nil {
			t.Fatalf("create media collection: %v", err)
		}
	}
	for name, c := range fc.Collections {
		col := core.NewBaseCollection(name)
		for _, f := range c.Fields {
			col.Fields.Add(drvBuildField(t, f, cfg.Media, app))
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
// fixtures use text/editor/file/relation only; anything else fails loudly
// rather than silently changing the schema the fixture was written against.
// A "relation" field may only target the media collection.
func drvBuildField(t *testing.T, f drvField, mc *MediaConfig, app core.App) core.Field {
	t.Helper()
	switch f.Type {
	case "text":
		return &core.TextField{Name: f.Name}
	case "editor":
		return &core.EditorField{Name: f.Name}
	case "file":
		return &core.FileField{Name: f.Name, MaxSelect: 1}
	case "relation":
		if mc == nil {
			t.Fatalf("fixture driver: relation field %q requires a media config", f.Name)
		}
		mediaCol, err := app.FindCollectionByNameOrId(mc.Collection)
		if err != nil {
			t.Fatalf("fixture driver: relation field %q: find media collection: %v", f.Name, err)
		}
		return &core.RelationField{Name: f.Name, CollectionId: mediaCol.Id, MaxSelect: 1}
	default:
		t.Fatalf("fixture driver: unsupported field type %q for %q", f.Type, f.Name)
		return nil
	}
}

// drvMediaState is the runner's per-fixture media-channel state: the active
// session, its bytes offset (mirrored to the server's Upload-Offset), and the
// last committed media record for the "$mediaId" sentinel.
type drvMediaState struct {
	uploadID string
	offset   int64
	mediaID  string
}

// drvSentinels substitutes blob-derived values in a media step's request body:
// "$sha:<blob>" (hex sha256), "$size:<blob>" (byte length), "$uploadId" and
// "$mediaId" (runner state). Fixture files never hardcode hashes or ids.
func drvSentinels(raw []byte, blobs map[string]string, ms *drvMediaState) []byte {
	for name, content := range blobs {
		sum := sha256.Sum256([]byte(content))
		raw = bytes.ReplaceAll(raw, []byte(`"$sha:`+name+`"`), []byte(`"`+hex.EncodeToString(sum[:])+`"`))
		raw = bytes.ReplaceAll(raw, []byte(`"$size:`+name+`"`), []byte(strconv.Itoa(len(content))))
	}
	if ms.uploadID != "" {
		raw = bytes.ReplaceAll(raw, []byte(`"$uploadId"`), []byte(`"`+ms.uploadID+`"`))
	}
	if ms.mediaID != "" {
		raw = bytes.ReplaceAll(raw, []byte(`"$mediaId"`), []byte(`"`+ms.mediaID+`"`))
	}
	return raw
}

func drvRunMediaStep(t *testing.T, env *testEnv, ctx string, step fixtureStep, tok string, f fixture, ms *drvMediaState) {
	t.Helper()

	var m fixtureMediaStep
	if err := json.Unmarshal(step.Media, &m); err != nil {
		t.Fatalf("%s: invalid media step: %v", ctx, err)
	}

	switch m.Op {
	case "init":
		req := drvSentinels(m.Request, f.Blobs, ms)
		status, _, body := env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil, req)
		if status == http.StatusOK {
			if id, ok := body["uploadId"].(string); ok {
				ms.uploadID = id
				ms.offset = 0
			}
			if id, ok := body["mediaId"].(string); ok {
				ms.mediaID = id
			}
		}
		drvCheck(t, ctx, step.Expect, body, status)

	case "upload":
		content := []byte(f.Blobs[m.Blob])
		if len(m.Slice) == 2 {
			content = content[m.Slice[0]:m.Slice[1]]
		}
		headers := map[string]string{
			"Upload-Offset": strconv.FormatInt(ms.offset, 10),
			"Content-Type":  tusCTType,
		}
		status, _, body := env.doJSON(http.MethodPatch, "/api/sync/media/upload/"+ms.uploadID, tok, headers, content)
		if status == http.StatusNoContent {
			ms.offset += int64(len(content))
		}
		// The successful upload channel answers with headers and no body.
		if len(body) == 0 {
			body = map[string]any{"status": status}
		}
		drvCheck(t, ctx, step.Expect, body, status)

	case "offset":
		status, hdr, body := env.doJSON(http.MethodHead, "/api/sync/media/upload/"+ms.uploadID, tok, nil, nil)
		if len(body) == 0 {
			body = map[string]any{"status": status}
			if n, perr := strconv.ParseInt(hdr.Get("Upload-Offset"), 10, 64); perr == nil {
				body["uploadOffset"] = n
			}
			if n, perr := strconv.ParseInt(hdr.Get("Upload-Length"), 10, 64); perr == nil {
				body["uploadLength"] = n
			}
		}
		drvCheck(t, ctx, step.Expect, body, status)

	case "commit":
		req := drvSentinels(m.Request, f.Blobs, ms)
		status, _, body := env.doJSON(http.MethodPost, "/api/sync/media/commit", tok, nil, req)
		if status == http.StatusOK {
			if id, ok := body["mediaId"].(string); ok {
				ms.mediaID = id
			}
		}
		drvCheck(t, ctx, step.Expect, body, status)

	case "abort":
		status, _, body := env.doJSON(http.MethodDelete, "/api/sync/media/upload/"+ms.uploadID, tok, nil, nil)
		if status == http.StatusNoContent {
			ms.uploadID = ""
			ms.offset = 0
		}
		if len(body) == 0 {
			body = map[string]any{"status": status}
		}
		drvCheck(t, ctx, step.Expect, body, status)
	}
}

func drvRunStep(t *testing.T, env *testEnv, ctx string, step fixtureStep, tok string, prevCursor *string, f fixture, ms *drvMediaState) {
	t.Helper()

	if isSet(step.Media) {
		drvRunMediaStep(t, env, ctx, step, tok, f, ms)
		return
	}

	if isSet(step.Push) {
		push := []byte(step.Push)
		if ms.mediaID != "" {
			push = drvSentinels(push, nil, &drvMediaState{mediaID: ms.mediaID})
		}
		status, body := env.pushRaw(tok, push, nil)
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

	// A request-level expect {status, code} also pins the HTTP status. Only a
	// numeric status pins it: the media init/commit responses carry a *body*
	// status string ("upload"/"ok", protocol §18.2/§18.4).
	if wm, ok := want.(map[string]any); ok {
		if ws, present := wm["status"]; present {
			if wi, ok := drvInt(ws); ok && wi != status {
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
