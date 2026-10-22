package main

import (
	"testing"
	"time"

	"github.com/karlomac/pb-offline-sync/server/syncplugin"
)

func getter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfigFromEnvDisabledWithoutCollections(t *testing.T) {
	for _, env := range []map[string]string{nil, {"SYNC_COLLECTIONS": "  ,  "}} {
		cfg, enabled, err := configFromEnv(getter(env))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if enabled || cfg.Collections != nil {
			t.Fatalf("empty SYNC_COLLECTIONS must disable the plugin, got enabled=%v cfg=%+v", enabled, cfg)
		}
	}
}

func TestConfigFromEnvBuildsCollections(t *testing.T) {
	cfg, enabled, err := configFromEnv(getter(map[string]string{
		"SYNC_COLLECTIONS":              "notes, projects ,",
		"SYNC_OWNER_FIELD":              "user",
		"SYNC_CONFLICT":                 "ClientWins",
		"SYNC_DELETE_POLICY":            "editWins",
		"SYNC_MAX_BATCH":                "50",
		"SYNC_CHANGELOG_RETENTION_DAYS": "7",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected the plugin to be enabled")
	}
	if len(cfg.Collections) != 2 {
		t.Fatalf("collections = %d, want 2 (%v)", len(cfg.Collections), cfg.Collections)
	}
	notes, ok := cfg.Collections["notes"]
	if !ok {
		t.Fatalf("missing 'notes' collection: %v", cfg.Collections)
	}
	if notes.OwnerField != "user" || notes.Conflict != syncplugin.ClientWins || notes.DeletePolicy != syncplugin.EditWins {
		t.Fatalf("notes config = %+v", notes)
	}
	if cfg.MaxBatch != 50 {
		t.Fatalf("MaxBatch = %d, want 50", cfg.MaxBatch)
	}
	if cfg.ChangeLogRetention != 7*24*time.Hour {
		t.Fatalf("ChangeLogRetention = %v, want %v", cfg.ChangeLogRetention, 7*24*time.Hour)
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg, enabled, err := configFromEnv(getter(map[string]string{"SYNC_COLLECTIONS": "notes"}))
	if err != nil || !enabled {
		t.Fatalf("enabled=%v err=%v", enabled, err)
	}
	n := cfg.Collections["notes"]
	if n.OwnerField != "owner" || n.Conflict != syncplugin.ServerWins || n.DeletePolicy != syncplugin.DeleteWins {
		t.Fatalf("defaults not applied: %+v", n)
	}
	// Unset limits stay zero so the plugin applies its own defaults.
	if cfg.MaxBatch != 0 || cfg.MaxPage != 0 || cfg.ChangeLogRetention != 0 {
		t.Fatalf("limits should stay zero for the plugin to default: %+v", cfg)
	}
}

func TestConfigFromEnvRejectsBadValues(t *testing.T) {
	base := map[string]string{"SYNC_COLLECTIONS": "notes"}
	for _, tc := range []struct{ key, val string }{
		{"SYNC_CONFLICT", "nope"},
		{"SYNC_DELETE_POLICY", "nope"},
		{"SYNC_MAX_BATCH", "-1"},
		{"SYNC_MAX_BATCH", "abc"},
		{"SYNC_MAX_BODY_BYTES", "1.5"},
		{"SYNC_CHANGELOG_RETENTION_DAYS", "-3"},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env[tc.key] = tc.val
		if _, _, err := configFromEnv(getter(env)); err == nil {
			t.Errorf("%s=%q: expected an error", tc.key, tc.val)
		}
	}
}
