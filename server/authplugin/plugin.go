package authplugin

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/mails"
	"github.com/pocketbase/pocketbase/tools/routine"
)

// pendingVerification marks records created through the password signup path.
// OnRecordAfterCreateSuccess is a model hook and carries no request info, so
// the decision made in the create-request hook is carried over by record
// pointer. LoadAndDelete means a repeated event cannot send a second email.
var pendingVerification sync.Map // map[*core.Record]struct{}

// Register binds the auth hooks. It is a no-op when cfg is disabled, so the
// shared image behaves like stock PocketBase unless AUTH_SIGNUP_MODE is set.
func Register(app core.App, cfg Config) error {
	if app == nil {
		return fmt.Errorf("authplugin: nil app")
	}
	if !cfg.enabled() {
		return nil
	}

	app.OnRecordCreateRequest(usersCollection).BindFunc(func(e *core.RecordRequestEvent) error {
		needsVerification, err := authorizeCreate(e, cfg)
		if err != nil {
			return err
		}
		if needsVerification {
			pendingVerification.Store(e.Record, struct{}{})
		}

		if err := e.Next(); err != nil {
			if needsVerification {
				pendingVerification.Delete(e.Record)
			}
			return err
		}
		return nil
	})

	app.OnRecordAfterCreateError(usersCollection).BindFunc(func(e *core.RecordErrorEvent) error {
		pendingVerification.Delete(e.Record)
		return e.Next()
	})

	app.OnRecordAfterCreateSuccess(usersCollection).BindFunc(func(e *core.RecordEvent) error {
		if err := e.Next(); err != nil {
			return err
		}

		if _, ok := pendingVerification.LoadAndDelete(e.Record); !ok {
			return nil
		}
		if e.Record.Verified() {
			return nil
		}

		send := cfg.sendVerification
		if send == nil {
			send = mails.SendRecordVerification
		}
		app, record := e.App, e.Record
		routine.FireAndForget(func() {
			if err := send(app, record); err != nil {
				app.Logger().Error("authplugin: failed to send verification email",
					"record", record.Id, "error", err)
			}
		})
		return nil
	})

	return nil
}

// authorizeCreate enforces the password-signup policy. It returns true when
// the newly created record should receive a verification email.
func authorizeCreate(e *core.RecordRequestEvent, cfg Config) (bool, error) {
	info, err := e.RequestInfo()
	if err != nil {
		return false, e.BadRequestError("Failed to read signup data.", err)
	}

	// Superuser requests bypass collection rules and are already trusted.
	if info.HasSuperuserAuth() {
		return false, nil
	}

	// OAuth2 creates are new signups too, so the mode must include oauth.
	if info.Context == core.RequestInfoContextOAuth2 {
		if !cfg.allowsOAuth() {
			return false, e.BadRequestError("OAuth2 signup is disabled.", nil)
		}
		return false, nil
	}

	if !cfg.allowsPassword() {
		return false, e.BadRequestError("Password signup is disabled.", nil)
	}

	body := info.Body
	if strings.TrimSpace(stringValue(body["website"])) != "" {
		return false, e.BadRequestError("Signup rejected.", nil)
	}

	token := strings.TrimSpace(stringValue(body["turnstileToken"]))
	if token == "" {
		return false, e.BadRequestError("Turnstile verification is required.", nil)
	}

	email := strings.TrimSpace(stringValue(body["email"]))
	domain, ok := emailDomain(email)
	if !ok {
		return false, e.BadRequestError("A valid email address is required.", nil)
	}
	if inList(cfg.DisposableDomains, domain) {
		return false, e.BadRequestError("Disposable email addresses are not allowed.", nil)
	}
	if len(cfg.AllowedEmailDomains) > 0 && !inList(cfg.AllowedEmailDomains, domain) {
		return false, e.BadRequestError("Email domain is not allowed.", nil)
	}

	hostname, err := cfg.verifyToken(e.Request.Context(), token, e.RealIP())
	if err != nil {
		e.App.Logger().Warn("authplugin: turnstile verification failed", "error", err, "ip", e.RealIP())
		return false, e.BadRequestError("Turnstile verification failed.", nil)
	}
	if len(cfg.TurnstileHostnames) > 0 && !inList(cfg.TurnstileHostnames, hostname) {
		e.App.Logger().Warn("authplugin: turnstile hostname mismatch", "hostname", hostname)
		return false, e.BadRequestError("Turnstile verification failed.", nil)
	}

	return true, nil
}

func stringValue(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func emailDomain(email string) (string, bool) {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return "", false
	}
	domain := strings.ToLower(email[at+1:])
	if strings.ContainsAny(domain, " \t\r\n") {
		return "", false
	}
	return domain, true
}

func inList(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}
