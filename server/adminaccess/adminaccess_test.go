package adminaccess

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestParseConfigDisabled(t *testing.T) {
	cfg, err := ParseConfig("", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled() {
		t.Fatal("empty config must be disabled")
	}
}

func TestParseConfigAndAllow(t *testing.T) {
	cfg, err := ParseConfig(" 203.0.113.5 , 10.0.0.0/8 , ::1 ", false, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, ip := range []string{"203.0.113.5", "10.20.30.40", "::1", "::ffff:10.0.0.1"} {
		if !cfg.Allows(ip, now) {
			t.Fatalf("%s should be allowed", ip)
		}
	}
	for _, ip := range []string{"203.0.113.6", "192.168.1.1", "2001:db8::1"} {
		if cfg.Allows(ip, now) {
			t.Fatalf("%s should be denied", ip)
		}
	}
}

func TestParseConfigErrors(t *testing.T) {
	if _, err := ParseConfig("not-an-ip", false, ""); err == nil {
		t.Fatal("expected an error for a bad IP")
	}
	if _, err := ParseConfig("", false, "tomorrow"); err == nil {
		t.Fatal("expected an error for a bad ADMIN_ALLOW_UNTIL")
	}
}

func TestTemporaryWindow(t *testing.T) {
	until := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	cfg, err := ParseConfig("", false, until.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled() {
		t.Fatal("a window alone must enable the guard")
	}
	if !cfg.Allows("198.51.100.7", until.Add(-time.Minute)) {
		t.Fatal("inside the window every IP is allowed")
	}
	// After the window with no allowlist: fail closed.
	if cfg.Allows("198.51.100.7", until.Add(time.Minute)) {
		t.Fatal("after the window with no allowlist the admin surfaces must be closed")
	}
}

func TestAllowAll(t *testing.T) {
	cfg, err := ParseConfig("", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Allows("198.51.100.7", time.Now()) {
		t.Fatal("ADMIN_ALLOW_ALL must allow every IP")
	}
}

func TestIsAdminPath(t *testing.T) {
	admin := []string{
		"/_", "/_/", "/_/index.html",
		"/api/collections/_superusers/auth-with-password",
		"/api/settings", "/api/backups", "/api/logs", "/api/crons",
	}
	for _, p := range admin {
		if !IsAdminPath(p) {
			t.Fatalf("%s should be an admin path", p)
		}
	}
	notAdmin := []string{
		"/api/health", "/api/sync/info", "/api/sync/push",
		"/api/collections/users/records", "/api/files/token",
	}
	for _, p := range notAdmin {
		if IsAdminPath(p) {
			t.Fatalf("%s must not be guarded", p)
		}
	}
}

func TestGuardMiddleware(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(app.Cleanup)

	cfg, err := ParseConfig("203.0.113.5", false, "")
	if err != nil {
		t.Fatal(err)
	}
	Register(app, cfg)

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

	do := func(path, ip string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = ip + ":4321"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := do("/_/", "203.0.113.5"); got == http.StatusForbidden {
		t.Fatalf("allowed IP got 403 for the admin UI")
	}
	if got := do("/_/", "198.51.100.9"); got != http.StatusForbidden {
		t.Fatalf("denied IP got %d for the admin UI, want 403", got)
	}
	if got := do("/api/settings", "198.51.100.9"); got != http.StatusForbidden {
		t.Fatalf("denied IP got %d for /api/settings, want 403", got)
	}
	if got := do("/api/health", "198.51.100.9"); got != http.StatusOK {
		t.Fatalf("health must stay reachable for a denied IP, got %d", got)
	}
}
