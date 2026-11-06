package authplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// prepareUsers makes the "users" collection look like a real signup-enabled app:
// public create, and an auth rule that requires verification (A-2).
func prepareUsers(t *testing.T, app *tests.TestApp) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(usersCollection)
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	open := ""
	verified := "verified = true"
	col.CreateRule = &open
	col.AuthRule = &verified
	if err := app.Save(col); err != nil {
		t.Fatalf("prepare users: %v", err)
	}
}

// serveWithAuth registers the plugin, runs migrations and triggers OnServe so
// the boot validation and the tagged hooks are live.
func serveWithAuth(t *testing.T, app *tests.TestApp, cfg Config) (*httptest.ResponseRecorder, http.Handler) {
	t.Helper()
	if err := Register(app, cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("migrations: %v", err)
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
	return httptest.NewRecorder(), mux
}

func triggerOnServe(t *testing.T, app *tests.TestApp) error {
	t.Helper()
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	serve := &core.ServeEvent{App: app, Router: router}
	return app.OnServe().Trigger(serve, func(e *core.ServeEvent) error { return e.Next() })
}

func TestValidateSetupRejectsMissingOrWrongCollection(t *testing.T) {
	// Missing collection: a renamed auth collection must not silently disable
	// the gate (A-1).
	app := newTestApp(t)
	cfg := passwordCfg()
	cfg.Collection = "members"
	if err := Register(app, cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := triggerOnServe(t, app); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want missing-collection error, got %v", err)
	}

	// Existing but non-auth collection.
	app2 := newTestApp(t)
	base := core.NewBaseCollection("members")
	if err := app2.Save(base); err != nil {
		t.Fatal(err)
	}
	cfg2 := passwordCfg()
	cfg2.Collection = "members"
	if err := Register(app2, cfg2); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := triggerOnServe(t, app2); err == nil || !strings.Contains(err.Error(), "not an auth collection") {
		t.Fatalf("want non-auth error, got %v", err)
	}

	// A correctly named auth collection passes.
	app3 := newTestApp(t)
	prepareUsers(t, app3)
	if err := Register(app3, passwordCfg()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := triggerOnServe(t, app3); err != nil {
		t.Fatalf("valid setup rejected: %v", err)
	}
}

func TestValidateSetupRequiresVerificationRule(t *testing.T) {
	app := newTestApp(t)
	// The users collection has no auth rule in the test fixtures.
	if err := Register(app, passwordCfg()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	err := triggerOnServe(t, app)
	if err == nil || !strings.Contains(err.Error(), "authRule") {
		t.Fatalf("want authRule error, got %v", err)
	}

	// OAuth-only mode does not require it (the plugin sends no verification
	// email on that path).
	app2 := newTestApp(t)
	cfg := passwordCfg()
	cfg.Mode = OAuth
	if err := Register(app2, cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := triggerOnServe(t, app2); err != nil {
		t.Fatalf("oauth mode rejected: %v", err)
	}
}

func TestConfigFromEnvCollectionAndVerifyURL(t *testing.T) {
	base := map[string]string{
		"AUTH_SIGNUP_MODE":        "password",
		"AUTH_TURNSTILE_SECRET":   "s",
		"AUTH_TURNSTILE_VERIFY_URL": "https://challenges.cloudflare.com/turnstile/v0/siteverify",
	}
	cfg, ok, err := ConfigFromEnv(envMap(base))
	if err != nil || !ok {
		t.Fatalf("enabled=%v err=%v", ok, err)
	}
	if cfg.collection() != "users" {
		t.Fatalf("default collection = %q", cfg.collection())
	}

	withCollection := map[string]string{}
	for k, v := range base {
		withCollection[k] = v
	}
	withCollection["AUTH_COLLECTION"] = "members"
	cfg, _, err = ConfigFromEnv(envMap(withCollection))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.collection() != "members" {
		t.Fatalf("collection = %q", cfg.collection())
	}

	// Plain HTTP to a remote host would send the secret in cleartext (A-3).
	httpRemote := map[string]string{}
	for k, v := range base {
		httpRemote[k] = v
	}
	httpRemote["AUTH_TURNSTILE_VERIFY_URL"] = "http://verify.example/siteverify"
	if _, _, err := ConfigFromEnv(envMap(httpRemote)); err == nil {
		t.Fatal("expected http remote verify URL to be rejected")
	}

	// Loopback http stays available for development/tests.
	httpLocal := map[string]string{}
	for k, v := range base {
		httpLocal[k] = v
	}
	httpLocal["AUTH_TURNSTILE_VERIFY_URL"] = "http://127.0.0.1:8080/siteverify"
	if _, _, err := ConfigFromEnv(envMap(httpLocal)); err != nil {
		t.Fatalf("loopback http rejected: %v", err)
	}
}

func TestVerifyTurnstileRefusesRedirects(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		_, _ = w.Write([]byte(`{"success":true,"hostname":"evil.example"}`))
	}))
	defer target.Close()

	verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer verifier.Close()

	cfg := Config{TurnstileSecret: "s3cret", TurnstileVerifyURL: verifier.URL}
	if _, err := verifyTurnstileHTTP(context.Background(), cfg, "tok", ""); err == nil {
		t.Fatal("expected a redirect to fail verification")
	}
	if redirected {
		t.Fatal("redirect target was contacted; the secret could have leaked")
	}
}

func TestDisposableSubdomainsBlocked(t *testing.T) {
	cfg := passwordCfg()
	cfg.DisposableDomains = []string{"mailinator.com"}
	app := newTestApp(t)
	prepareUsers(t, app)
	if err := Register(app, cfg); err != nil {
		t.Fatal(err)
	}
	if err := triggerOnServe(t, app); err != nil {
		t.Fatal(err)
	}

	_, err := triggerCreate(t, app, createRequest{body: map[string]any{
		"email": "user@rotating.mailinator.com", "turnstileToken": "good",
	}})
	wantBadRequest(t, err, "Disposable")

	_, err = triggerCreate(t, app, createRequest{body: map[string]any{
		"email": "user@notmailinator.com", "turnstileToken": "good",
	}})
	if err != nil {
		t.Fatalf("near-miss domain rejected: %v", err)
	}
	// Leave the pending marker for the test app's lifecycle; no email is sent
	// because the after-create hook is never reached in this unit test.
}

// TestSignupThroughRouter is the A-5 regression: the gate must fire through the
// real, tag-filtered hook chain (the unit tests trigger the untagged hook, which
// cannot prove the collection tag is right).
func TestSignupThroughRouter(t *testing.T) {
	app := newTestApp(t)
	prepareUsers(t, app)

	cfg := passwordCfg()
	cfg.Collection = usersCollection
	_, mux := serveWithAuth(t, app, cfg)

	post := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/collections/users/records", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.9:1234"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// No Turnstile token: rejected by the gate (this only works if the tagged
	// hook actually fired).
	rec := post(map[string]any{
		"email": "new@example.com", "password": "1234567890", "passwordConfirm": "1234567890",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tokenless signup status = %d, body %s", rec.Code, rec.Body.String())
	}

	// With a valid token: created and a verification email is dispatched.
	rec = post(map[string]any{
		"email": "new@example.com", "password": "1234567890", "passwordConfirm": "1234567890",
		"turnstileToken": "good",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("gated signup status = %d, body %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for app.TestMailer.TotalSend() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := app.TestMailer.TotalSend(); got != 1 {
		t.Fatalf("verification emails = %d, want 1", got)
	}
}
