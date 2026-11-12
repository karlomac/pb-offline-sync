package authplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/mails"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/router"
)

func newTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(app.Cleanup)
	return app
}

func passwordCfg() Config {
	return Config{
		Mode:               Password,
		TurnstileSecret:    "secret",
		TurnstileVerifyURL: "http://127.0.0.1:1/siteverify",
		verifyTurnstile: func(context.Context, Config, string, string) (string, error) {
			return "app.example", nil
		},
	}
}

type createRequest struct {
	body map[string]any
	ctx  string
	auth *core.Record
	next func(*core.RecordRequestEvent) error
}

// triggerCreate runs the OnRecordCreateRequest hook chain directly. next stands
// in for PocketBase's default handler (form submission).
func triggerCreate(t *testing.T, app core.App, req createRequest) (*core.RecordRequestEvent, error) {
	t.Helper()

	users, err := app.FindCollectionByNameOrId(usersCollection)
	if err != nil {
		t.Fatalf("find users collection: %v", err)
	}

	raw, err := json.Marshal(req.body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/api/collections/users/records", bytes.NewReader(raw))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.RemoteAddr = "203.0.113.7:4321"

	requestEvent := &core.RequestEvent{App: app, Auth: req.auth}
	requestEvent.Request = httpReq
	requestEvent.Response = httptest.NewRecorder()
	if req.ctx != "" {
		requestEvent.Set(core.RequestEventKeyInfoContext, req.ctx)
	}

	event := &core.RecordRequestEvent{
		RequestEvent: requestEvent,
		Collection:   users,
		Record:       core.NewRecord(users),
	}

	next := req.next
	if next == nil {
		next = func(*core.RecordRequestEvent) error { return nil }
	}

	return event, app.OnRecordCreateRequest().Trigger(event, next)
}

func wantBadRequest(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a 400 error, got nil")
	}
	var apiErr *router.ApiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *router.ApiError, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%v)", apiErr.Status, err)
	}
	if contains != "" && !strings.Contains(apiErr.Message, contains) {
		t.Fatalf("message %q does not contain %q", apiErr.Message, contains)
	}
}

func TestDisabledPluginBindsNoHooks(t *testing.T) {
	app := newTestApp(t)
	if err := Register(app, Config{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Without hooks this body would be rejected by the password policy.
	if _, err := triggerCreate(t, app, createRequest{body: map[string]any{
		"email": "user@example.com",
	}}); err != nil {
		t.Fatalf("disabled plugin affected create: %v", err)
	}
}

func TestPasswordSignupRejections(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		ctx       string
		body      map[string]any
		want      string
	}{
		{
			name: "missing token",
			body: map[string]any{"email": "user@example.com"},
			want: "Turnstile verification is required",
		},
		{
			name: "blank token",
			body: map[string]any{"email": "user@example.com", "turnstileToken": "   "},
			want: "Turnstile verification is required",
		},
		{
			name: "honeypot filled",
			body: map[string]any{"email": "user@example.com", "turnstileToken": "good", "website": "https://spam.example"},
			want: "Signup rejected",
		},
		{
			name: "verifier failure",
			configure: func(cfg *Config) {
				cfg.verifyTurnstile = func(context.Context, Config, string, string) (string, error) {
					return "", errors.New("invalid-input-response")
				}
			},
			body: map[string]any{"email": "user@example.com", "turnstileToken": "bad"},
			want: "Turnstile verification failed",
		},
		{
			name: "hostname mismatch",
			configure: func(cfg *Config) {
				cfg.TurnstileHostnames = []string{"app.example"}
				cfg.verifyTurnstile = func(context.Context, Config, string, string) (string, error) {
					return "evil.example", nil
				}
			},
			body: map[string]any{"email": "user@example.com", "turnstileToken": "good"},
			want: "Turnstile verification failed",
		},
		{
			name: "disposable domain",
			configure: func(cfg *Config) {
				cfg.DisposableDomains = []string{"mailinator.com"}
			},
			body: map[string]any{"email": "user@mailinator.com", "turnstileToken": "good"},
			want: "Disposable email addresses are not allowed",
		},
		{
			name: "domain not allowed",
			configure: func(cfg *Config) {
				cfg.AllowedEmailDomains = []string{"example.com"}
			},
			body: map[string]any{"email": "user@other.example", "turnstileToken": "good"},
			want: "Email domain is not allowed",
		},
		{
			name: "invalid email",
			body: map[string]any{"email": "not-an-email", "turnstileToken": "good"},
			want: "A valid email address is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(t)
			cfg := passwordCfg()
			if tt.configure != nil {
				tt.configure(&cfg)
			}
			if err := Register(app, cfg); err != nil {
				t.Fatalf("Register: %v", err)
			}

			_, err := triggerCreate(t, app, createRequest{ctx: tt.ctx, body: tt.body})
			wantBadRequest(t, err, tt.want)
		})
	}
}

func TestValidPasswordSignupPasses(t *testing.T) {
	app := newTestApp(t)
	cfg := passwordCfg()

	var gotToken, gotIP string
	cfg.verifyTurnstile = func(_ context.Context, _ Config, token, remoteIP string) (string, error) {
		gotToken, gotIP = token, remoteIP
		return "app.example", nil
	}
	cfg.TurnstileHostnames = []string{"app.example"}

	if err := Register(app, cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	event, err := triggerCreate(t, app, createRequest{body: map[string]any{
		"email":          "user@example.com",
		"turnstileToken": "good-token",
	}})
	if err != nil {
		t.Fatalf("valid signup rejected: %v", err)
	}
	if gotToken != "good-token" {
		t.Fatalf("verifier token = %q, want %q", gotToken, "good-token")
	}
	if gotIP != "203.0.113.7" {
		t.Fatalf("verifier remote IP = %q, want %q", gotIP, "203.0.113.7")
	}

	// This test does not submit the form, so drop the pending marker it left.
	pendingVerification.Delete(event.Record)
}

func TestAllowedDomainPasses(t *testing.T) {
	app := newTestApp(t)
	cfg := passwordCfg()
	cfg.AllowedEmailDomains = []string{"example.com"}
	if err := Register(app, cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	event, err := triggerCreate(t, app, createRequest{body: map[string]any{
		"email":          "user@example.com",
		"turnstileToken": "good",
	}})
	if err != nil {
		t.Fatalf("allowed domain rejected: %v", err)
	}
	pendingVerification.Delete(event.Record)
}

// TestModeMatrix verifies that each mode gates both signup paths.
func TestModeMatrix(t *testing.T) {
	tests := []struct {
		mode       Mode
		passwordOK bool
		oauthOK    bool
	}{
		{mode: OAuth, passwordOK: false, oauthOK: true},
		{mode: Password, passwordOK: true, oauthOK: false},
		{mode: Both, passwordOK: true, oauthOK: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			app := newTestApp(t)
			cfg := passwordCfg()
			cfg.Mode = tt.mode
			if err := Register(app, cfg); err != nil {
				t.Fatalf("Register: %v", err)
			}

			event, err := triggerCreate(t, app, createRequest{body: map[string]any{
				"email":          "user@example.com",
				"turnstileToken": "good",
			}})
			if tt.passwordOK {
				if err != nil {
					t.Fatalf("password signup rejected in %s mode: %v", tt.mode, err)
				}
				pendingVerification.Delete(event.Record)
			} else {
				wantBadRequest(t, err, "Password signup is disabled")
			}

			_, err = triggerCreate(t, app, createRequest{
				ctx:  core.RequestInfoContextOAuth2,
				body: map[string]any{"email": "user@example.com"},
			})
			if tt.oauthOK {
				if err != nil {
					t.Fatalf("OAuth2 signup rejected in %s mode: %v", tt.mode, err)
				}
			} else {
				wantBadRequest(t, err, "OAuth2 signup is disabled")
			}
		})
	}
}

func newSuperuser(t *testing.T, app *tests.TestApp) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatalf("find superusers: %v", err)
	}
	su := core.NewRecord(col)
	su.Set("email", "root@example.com")
	su.Set("password", "1234567890")
	su.Set("passwordConfirm", "1234567890")
	if err := app.Save(su); err != nil {
		t.Fatalf("save superuser: %v", err)
	}
	return su
}

func TestSuperuserAlwaysAllowed(t *testing.T) {
	for _, mode := range []Mode{OAuth, Password, Both} {
		t.Run(string(mode), func(t *testing.T) {
			app := newTestApp(t)
			cfg := passwordCfg()
			cfg.Mode = mode
			if err := Register(app, cfg); err != nil {
				t.Fatalf("Register: %v", err)
			}
			su := newSuperuser(t, app)

			if _, err := triggerCreate(t, app, createRequest{
				auth: su,
				body: map[string]any{"email": "user@example.com"},
			}); err != nil {
				t.Fatalf("superuser create rejected in %s mode: %v", mode, err)
			}
		})
	}
}

// submitSignup stands in for PocketBase's default handler: it copies the
// submitted fields onto the record and saves it, which fires
// OnRecordAfterCreateSuccess.
func submitSignup(e *core.RecordRequestEvent, body map[string]any) error {
	e.Record.Set("email", body["email"])
	e.Record.Set("password", body["password"])
	e.Record.Set("passwordConfirm", body["passwordConfirm"])
	return e.App.Save(e.Record)
}

func verificationSender(sent chan<- struct{}) func(core.App, *core.Record) error {
	return func(app core.App, record *core.Record) error {
		err := mails.SendRecordVerification(app, record)
		sent <- struct{}{}
		return err
	}
}

func waitForVerification(t *testing.T, sent <-chan struct{}) {
	t.Helper()
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for verification email")
	}
}

func assertNoVerification(t *testing.T, sent <-chan struct{}, app *tests.TestApp) {
	t.Helper()
	select {
	case <-sent:
		t.Fatal("unexpected verification email")
	case <-time.After(100 * time.Millisecond):
	}
	if got := app.TestMailer.TotalSend(); got != 0 {
		t.Fatalf("verification emails = %d, want 0", got)
	}
}

func TestVerificationEmailSentForPasswordSignup(t *testing.T) {
	app := newTestApp(t)
	cfg := passwordCfg()
	sent := make(chan struct{}, 2)
	cfg.sendVerification = verificationSender(sent)
	if err := Register(app, cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	body := map[string]any{
		"email":           "new@example.com",
		"password":        "1234567890",
		"passwordConfirm": "1234567890",
		"turnstileToken":  "good",
	}
	event, err := triggerCreate(t, app, createRequest{
		body: body,
		next: func(e *core.RecordRequestEvent) error { return submitSignup(e, body) },
	})
	if err != nil {
		t.Fatalf("signup failed: %v", err)
	}
	waitForVerification(t, sent)
	if got := app.TestMailer.TotalSend(); got != 1 {
		t.Fatalf("verification emails = %d, want 1", got)
	}

	// Replaying the same after-create event must not send again.
	after := new(core.RecordEvent)
	after.App = app
	after.Record = event.Record
	if err := app.OnRecordAfterCreateSuccess(usersCollection).Trigger(after, func(*core.RecordEvent) error {
		return nil
	}); err != nil {
		t.Fatalf("replay after-create: %v", err)
	}
	select {
	case <-sent:
		t.Fatal("verification email sent twice")
	case <-time.After(100 * time.Millisecond):
	}
	if got := app.TestMailer.TotalSend(); got != 1 {
		t.Fatalf("verification emails after replay = %d, want 1", got)
	}
}

func TestNoVerificationEmailForOAuth2(t *testing.T) {
	app := newTestApp(t)
	cfg := passwordCfg()
	cfg.Mode = Both
	sent := make(chan struct{}, 1)
	cfg.sendVerification = verificationSender(sent)
	if err := Register(app, cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	body := map[string]any{
		"email":           "oauth@example.com",
		"password":        "1234567890",
		"passwordConfirm": "1234567890",
	}
	// OAuth2 creates get a random password from PocketBase; an explicit one is
	// fine for this hook test.
	if _, err := triggerCreate(t, app, createRequest{
		ctx:  core.RequestInfoContextOAuth2,
		body: body,
		next: func(e *core.RecordRequestEvent) error { return submitSignup(e, body) },
	}); err != nil {
		t.Fatalf("OAuth2 signup failed: %v", err)
	}
	assertNoVerification(t, sent, app)
}

func TestNoVerificationEmailForSuperuser(t *testing.T) {
	app := newTestApp(t)
	cfg := passwordCfg()
	sent := make(chan struct{}, 1)
	cfg.sendVerification = verificationSender(sent)
	if err := Register(app, cfg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	su := newSuperuser(t, app)

	body := map[string]any{
		"email":           "created@example.com",
		"password":        "1234567890",
		"passwordConfirm": "1234567890",
	}
	if _, err := triggerCreate(t, app, createRequest{
		auth: su,
		body: body,
		next: func(e *core.RecordRequestEvent) error { return submitSignup(e, body) },
	}); err != nil {
		t.Fatalf("superuser create failed: %v", err)
	}
	assertNoVerification(t, sent, app)
}
