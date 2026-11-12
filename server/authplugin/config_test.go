package authplugin

import (
	"slices"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfigFromEnvDisabledByDefault(t *testing.T) {
	cfg, enabled, err := ConfigFromEnv(envMap(nil))
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if enabled {
		t.Fatal("plugin enabled without AUTH_SIGNUP_MODE")
	}
	if cfg.enabled() {
		t.Fatalf("Config.enabled() = true for zero config: %#v", cfg)
	}
}

func TestConfigFromEnvModes(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantEnabled bool
		wantMode    Mode
		wantErr     string
	}{
		{
			name:        "oauth does not need a secret",
			env:         map[string]string{"AUTH_SIGNUP_MODE": "oauth"},
			wantEnabled: true,
			wantMode:    OAuth,
		},
		{
			name:    "password requires a secret",
			env:     map[string]string{"AUTH_SIGNUP_MODE": "password"},
			wantErr: "AUTH_TURNSTILE_SECRET",
		},
		{
			name:        "password",
			env:         map[string]string{"AUTH_SIGNUP_MODE": "password", "AUTH_TURNSTILE_SECRET": "s"},
			wantEnabled: true,
			wantMode:    Password,
		},
		{
			name:        "both is case insensitive",
			env:         map[string]string{"AUTH_SIGNUP_MODE": " BOTH ", "AUTH_TURNSTILE_SECRET": "s"},
			wantEnabled: true,
			wantMode:    Both,
		},
		{
			name:        "blank means disabled",
			env:         map[string]string{"AUTH_SIGNUP_MODE": "   "},
			wantEnabled: false,
		},
		{
			name:    "unknown mode",
			env:     map[string]string{"AUTH_SIGNUP_MODE": "invite"},
			wantErr: "unknown mode",
		},
		{
			name: "invalid verify URL",
			env: map[string]string{
				"AUTH_SIGNUP_MODE":          "oauth",
				"AUTH_TURNSTILE_VERIFY_URL": "://bad",
			},
			wantErr: "AUTH_TURNSTILE_VERIFY_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, enabled, err := ConfigFromEnv(envMap(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnv: %v", err)
			}
			if enabled != tt.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, tt.wantEnabled)
			}
			if cfg.Mode != tt.wantMode {
				t.Fatalf("mode = %q, want %q", cfg.Mode, tt.wantMode)
			}
			if enabled && cfg.TurnstileVerifyURL != defaultTurnstileVerifyURL {
				t.Fatalf("verify URL = %q, want default", cfg.TurnstileVerifyURL)
			}
		})
	}
}

func TestConfigFromEnvLists(t *testing.T) {
	cfg, enabled, err := ConfigFromEnv(envMap(map[string]string{
		"AUTH_SIGNUP_MODE":           "both",
		"AUTH_TURNSTILE_SECRET":      "secret",
		"AUTH_TURNSTILE_HOSTNAMES":   " App.Example.com , other.example ",
		"AUTH_ALLOWED_EMAIL_DOMAINS": "@Example.com, corp.example",
		"AUTH_DISPOSABLE_DOMAINS":    "mailinator.com, YOPMAIL.com,",
		"AUTH_TURNSTILE_VERIFY_URL":  "http://127.0.0.1:9999/siteverify",
	}))
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if !enabled {
		t.Fatal("plugin not enabled")
	}
	if got, want := cfg.TurnstileHostnames, []string{"app.example.com", "other.example"}; !slices.Equal(got, want) {
		t.Fatalf("hostnames = %v, want %v", got, want)
	}
	if got, want := cfg.AllowedEmailDomains, []string{"example.com", "corp.example"}; !slices.Equal(got, want) {
		t.Fatalf("allowed domains = %v, want %v", got, want)
	}
	if got, want := cfg.DisposableDomains, []string{"mailinator.com", "yopmail.com"}; !slices.Equal(got, want) {
		t.Fatalf("disposable domains = %v, want %v", got, want)
	}
	if cfg.TurnstileVerifyURL != "http://127.0.0.1:9999/siteverify" {
		t.Fatalf("verify URL = %q", cfg.TurnstileVerifyURL)
	}
}
