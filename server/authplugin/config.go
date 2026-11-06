// Package authplugin gates signup for the shared PocketBase image. It is a
// complete no-op unless AUTH_SIGNUP_MODE is set, and it only ever binds hooks
// to the "users" collection so the sync plugin and its collections are
// untouched.
package authplugin

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

const (
	usersCollection           = "users"
	defaultTurnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
)

// Mode selects which signup paths are enabled.
type Mode string

const (
	// OAuth only permits provider signup (all OAuth2 creates pass through).
	OAuth Mode = "oauth"
	// Password only permits email/password signup with Turnstile.
	Password Mode = "password"
	// Both permits OAuth2 and email/password signup.
	Both Mode = "both"
)

// Config is the auth plugin configuration.
type Config struct {
	Mode Mode

	TurnstileSecret    string
	TurnstileHostnames []string
	TurnstileVerifyURL string

	AllowedEmailDomains []string
	DisposableDomains   []string

	// verifyTurnstile is overridden in unit tests so they never touch the
	// network. A nil value uses the Cloudflare HTTP verifier.
	verifyTurnstile func(context.Context, Config, string, string) (string, error)

	// sendVerification is overridden in unit tests so the background email
	// sender can be observed deterministically. A nil value uses
	// mails.SendRecordVerification.
	sendVerification func(core.App, *core.Record) error
}

func (cfg Config) enabled() bool { return cfg.Mode != "" }

func (cfg Config) allowsOAuth() bool {
	return cfg.Mode == OAuth || cfg.Mode == Both
}

func (cfg Config) allowsPassword() bool {
	return cfg.Mode == Password || cfg.Mode == Both
}

// ConfigFromEnv builds the plugin config from environment variables. The second
// result is false when AUTH_SIGNUP_MODE is empty, meaning the plugin should not
// be registered at all.
func ConfigFromEnv(get func(string) string) (Config, bool, error) {
	raw := strings.ToLower(strings.TrimSpace(get("AUTH_SIGNUP_MODE")))
	if raw == "" {
		return Config{}, false, nil
	}

	mode := Mode(raw)
	switch mode {
	case OAuth, Password, Both:
	default:
		return Config{}, false, fmt.Errorf("AUTH_SIGNUP_MODE: unknown mode %q (want oauth|password|both)", raw)
	}

	secret := strings.TrimSpace(get("AUTH_TURNSTILE_SECRET"))
	if mode != OAuth && secret == "" {
		return Config{}, false, fmt.Errorf("AUTH_TURNSTILE_SECRET is required when AUTH_SIGNUP_MODE=%s", mode)
	}

	verifyURL := strings.TrimSpace(get("AUTH_TURNSTILE_VERIFY_URL"))
	if verifyURL == "" {
		verifyURL = defaultTurnstileVerifyURL
	}
	if _, err := url.ParseRequestURI(verifyURL); err != nil {
		return Config{}, false, fmt.Errorf("AUTH_TURNSTILE_VERIFY_URL: %w", err)
	}

	return Config{
		Mode:                mode,
		TurnstileSecret:     secret,
		TurnstileHostnames:  list(get("AUTH_TURNSTILE_HOSTNAMES")),
		TurnstileVerifyURL:  verifyURL,
		AllowedEmailDomains: list(get("AUTH_ALLOWED_EMAIL_DOMAINS")),
		DisposableDomains:   list(get("AUTH_DISPOSABLE_DOMAINS")),
	}, true, nil
}

// list splits a comma-separated env value, lowercasing entries and tolerating
// an optional leading "@" so "@example.com" and "example.com" are equivalent.
func list(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		part = strings.TrimPrefix(part, "@")
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
