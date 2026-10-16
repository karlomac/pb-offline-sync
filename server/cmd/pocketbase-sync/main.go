// Command pocketbase-sync is the default PocketBase build for pb-offline-sync:
// a stock PocketBase server with the sync plugin wired in, configured entirely
// by environment variables so one binary (and one container image) can serve
// any app.
//
// Environment:
//
//	SYNC_COLLECTIONS                 comma-separated collection names to sync.
//	                                 Empty/absent => plain PocketBase (plugin off).
//	SYNC_OWNER_FIELD                 relation field naming the owner (default "owner")
//	SYNC_CONFLICT                    serverWins|clientWins|reject (default serverWins)
//	SYNC_DELETE_POLICY               deleteWins|editWins (default deleteWins)
//	SYNC_MAX_BATCH                   max mutations per push (default 100)
//	SYNC_MAX_PAGE                    max changes per pull page (default 500)
//	SYNC_DEFAULT_PAGE                pull page size when unset (default 200)
//	SYNC_MAX_BODY_BYTES              max push body size (default 1048576)
//	SYNC_IDEMPOTENCY_RETENTION_DAYS  idempotency retention (default 14)
//	SYNC_CHANGELOG_RETENTION_DAYS    change-log retention (default 30)
//	SYNC_PRUNE_CRON                  cron expr for retention pruning
//	                                 (default "0 3 * * *"; empty disables)
//
// Every collection named in SYNC_COLLECTIONS must exist and have the owner
// field as a relation to an auth collection, or the server refuses to start
// (the plugin validates at boot — see syncplugin.MustRegister).
package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase"

	"github.com/pb-offline-sync/server/syncplugin"
)

func main() {
	app := pocketbase.New()

	cfg, enabled, err := configFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("pb-offline-sync: %v", err)
	}

	if enabled {
		syncplugin.MustRegister(app, cfg)

		// The plugin never prunes on its own; schedule it here so the
		// configured retention is actually applied.
		if expr := pruneCron(); expr != "" {
			app.Cron().MustAdd("pb-sync-prune", expr, func() {
				if err := syncplugin.Prune(app, cfg); err != nil {
					app.Logger().Error("pb-offline-sync: prune failed", "error", err)
				}
			})
		}
	}

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// pruneCron returns the configured cron expression, defaulting to nightly and
// treating an explicit empty value as "do not schedule".
func pruneCron() string {
	if v, ok := os.LookupEnv("SYNC_PRUNE_CRON"); ok {
		return strings.TrimSpace(v)
	}
	return "0 3 * * *"
}

// configFromEnv builds the plugin config from environment variables. The second
// result is false when SYNC_COLLECTIONS is empty, meaning the plugin should not
// be registered at all.
func configFromEnv(get func(string) string) (syncplugin.Config, bool, error) {
	names := splitList(get("SYNC_COLLECTIONS"))
	if len(names) == 0 {
		return syncplugin.Config{}, false, nil
	}

	owner := strings.TrimSpace(get("SYNC_OWNER_FIELD"))
	if owner == "" {
		owner = "owner"
	}

	conflict, err := parseConflict(get("SYNC_CONFLICT"))
	if err != nil {
		return syncplugin.Config{}, false, err
	}
	delPolicy, err := parseDeletePolicy(get("SYNC_DELETE_POLICY"))
	if err != nil {
		return syncplugin.Config{}, false, err
	}

	collections := make(map[string]syncplugin.CollectionConfig, len(names))
	for _, name := range names {
		collections[name] = syncplugin.CollectionConfig{
			OwnerField:   owner,
			Conflict:     conflict,
			DeletePolicy: delPolicy,
		}
	}

	cfg := syncplugin.Config{Collections: collections}

	if cfg.MaxBatch, err = envInt(get, "SYNC_MAX_BATCH"); err != nil {
		return syncplugin.Config{}, false, err
	}
	if cfg.MaxPage, err = envInt(get, "SYNC_MAX_PAGE"); err != nil {
		return syncplugin.Config{}, false, err
	}
	if cfg.DefaultPage, err = envInt(get, "SYNC_DEFAULT_PAGE"); err != nil {
		return syncplugin.Config{}, false, err
	}
	if cfg.MaxBodyBytes, err = envInt64(get, "SYNC_MAX_BODY_BYTES"); err != nil {
		return syncplugin.Config{}, false, err
	}
	if cfg.IdempotencyRetentionDays, err = envInt(get, "SYNC_IDEMPOTENCY_RETENTION_DAYS"); err != nil {
		return syncplugin.Config{}, false, err
	}

	days, err := envInt(get, "SYNC_CHANGELOG_RETENTION_DAYS")
	if err != nil {
		return syncplugin.Config{}, false, err
	}
	if days > 0 {
		cfg.ChangeLogRetention = time.Duration(days) * 24 * time.Hour
	}

	return cfg, true, nil
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseConflict(v string) (syncplugin.ConflictPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "serverwins":
		return syncplugin.ServerWins, nil
	case "clientwins":
		return syncplugin.ClientWins, nil
	case "reject":
		return syncplugin.Reject, nil
	default:
		return "", fmt.Errorf("SYNC_CONFLICT: unknown policy %q (want serverWins|clientWins|reject)", v)
	}
}

func parseDeletePolicy(v string) (syncplugin.DeletePolicy, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "deletewins":
		return syncplugin.DeleteWins, nil
	case "editwins":
		return syncplugin.EditWins, nil
	default:
		return "", fmt.Errorf("SYNC_DELETE_POLICY: unknown policy %q (want deleteWins|editWins)", v)
	}
}

func envInt(get func(string) string, key string) (int, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: %q is not a non-negative integer", key, v)
	}
	return n, nil
}

func envInt64(get func(string) string, key string) (int64, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: %q is not a non-negative integer", key, v)
	}
	return n, nil
}
