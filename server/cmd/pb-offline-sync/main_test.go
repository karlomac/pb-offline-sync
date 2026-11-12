package main

import (
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/cron"

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

func TestConfigFromEnvMediaDisabledByDefault(t *testing.T) {
	cfg, _, err := configFromEnv(getter(map[string]string{"SYNC_COLLECTIONS": "notes"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Media != nil {
		t.Fatalf("media must stay disabled without SYNC_MEDIA_COLLECTION: %+v", cfg.Media)
	}
}

func TestConfigFromEnvMediaWiring(t *testing.T) {
	cfg, enabled, err := configFromEnv(getter(map[string]string{
		"SYNC_COLLECTIONS":         "notes",
		"SYNC_MEDIA_COLLECTION":    "photos",
		"SYNC_MEDIA_MAX_FILE_BYTES": "1048576",
		"SYNC_MEDIA_PER_USER_QUOTA": "5242880",
	}))
	if err != nil || !enabled {
		t.Fatalf("enabled=%v err=%v", enabled, err)
	}
	if cfg.Media == nil {
		t.Fatal("media config not built")
	}
	if cfg.Media.Collection != "photos" || cfg.Media.MaxFileSize != 1048576 || cfg.Media.PerUserQuota != 5242880 {
		t.Fatalf("media config: %+v", cfg.Media)
	}
	// The media collection is auto-registered as synced (§18.1).
	if _, ok := cfg.Collections["photos"]; !ok {
		t.Fatal("media collection not added to the synced set")
	}

	// Invalid numeric values fail fast instead of silently disabling a limit.
	if _, _, err := configFromEnv(getter(map[string]string{
		"SYNC_COLLECTIONS":      "notes",
		"SYNC_MEDIA_COLLECTION": "photos",
		"SYNC_MEDIA_MAX_FILE_BYTES": "lots",
	})); err == nil {
		t.Fatal("expected an error for a non-numeric media limit")
	}
}

func TestPruneCronDefaultsAndExplicitDisable(t *testing.T) {
	// Compose/Dokploy inject optional vars as empty strings: that must mean
	// "default", not "disabled".
	t.Setenv("SYNC_PRUNE_CRON", "")
	if got := pruneCron(); got != "0 3 * * *" {
		t.Fatalf("empty = %q, want the nightly default", got)
	}
	t.Setenv("SYNC_PRUNE_CRON", "OFF")
	if got := pruneCron(); got != "" {
		t.Fatalf("off = %q, want disabled", got)
	}
	t.Setenv("SYNC_PRUNE_CRON", "*/15 * * * *")
	if got := pruneCron(); got != "*/15 * * * *" {
		t.Fatalf("explicit cron = %q", got)
	}
}

func cronIDs(t *testing.T, app interface{ Cron() *cron.Cron }) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, job := range app.Cron().Jobs() {
		ids[job.Id()] = true
	}
	return ids
}

func TestRegisterCronSchedulesMaintenance(t *testing.T) {
	mediaCfg := syncplugin.Config{
		Collections: map[string]syncplugin.CollectionConfig{"media": {OwnerField: "owner"}},
		Media:       &syncplugin.MediaConfig{Collection: "media"},
	}

	// PocketBase registers its own jobs on a test app, so assert by id.
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	defer app.Cleanup()
	t.Setenv("SYNC_PRUNE_CRON", "")
	registerCron(app, mediaCfg)
	ids := cronIDs(t, app)
	for _, want := range []string{"pb-sync-prune", "pb-media-sweep", "pb-media-gc"} {
		if !ids[want] {
			t.Fatalf("cron job %q not scheduled; got %v", want, ids)
		}
	}

	// Without media, only pruning is registered.
	app2, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	defer app2.Cleanup()
	registerCron(app2, syncplugin.Config{
		Collections: map[string]syncplugin.CollectionConfig{"notes": {OwnerField: "owner"}},
	})
	ids = cronIDs(t, app2)
	if !ids["pb-sync-prune"] || ids["pb-media-sweep"] || ids["pb-media-gc"] {
		t.Fatalf("unexpected jobs without media: %v", ids)
	}

	// SYNC_PRUNE_CRON=none disables pruning but keeps the media jobs.
	app3, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	defer app3.Cleanup()
	t.Setenv("SYNC_PRUNE_CRON", "none")
	registerCron(app3, mediaCfg)
	ids = cronIDs(t, app3)
	if ids["pb-sync-prune"] || !ids["pb-media-sweep"] || !ids["pb-media-gc"] {
		t.Fatalf("unexpected jobs with pruning disabled: %v", ids)
	}
}
