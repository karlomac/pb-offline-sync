// Command pb-offline-sync is the default PocketBase build for pb-offline-sync:
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
//	                                 (default "0 3 * * *"; "none" disables)
//	SYNC_MEDIA_COLLECTION            media collection name; empty disables media
//	                                 (see docs/protocol.md §18 for the required
//	                                 Protected file fields and owner-scoped rules)
//	SYNC_MEDIA_MAX_FILE_BYTES        media max file size in bytes (default 0 = unlimited)
//	SYNC_MEDIA_PER_USER_QUOTA        media per-user quota in bytes (default 0 = unlimited)
//	SYNC_MEDIA_MIME_ALLOWLIST        comma-separated exact MIME types or "type/" prefixes
//	                                 (default: image/, video/, audio/; text/html and
//	                                 image/svg+xml are always denied)
//
//	                                           Auth plugin (off unless AUTH_SIGNUP_MODE is set):
//		AUTH_SIGNUP_MODE                 oauth|password|both
//		AUTH_TURNSTILE_SECRET            Turnstile secret (required for password/both)
//		AUTH_TURNSTILE_HOSTNAMES         comma-separated allowed Turnstile hostnames
//		AUTH_TURNSTILE_VERIFY_URL        siteverify URL (default Cloudflare)
//		AUTH_ALLOWED_EMAIL_DOMAINS       comma-separated allowed email domains
//		AUTH_DISPOSABLE_DOMAINS          comma-separated blocked disposable domains
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

	"github.com/karlomac/pb-offline-sync/server/authplugin"
	"github.com/karlomac/pb-offline-sync/server/syncplugin"
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

	authCfg, authEnabled, err := authplugin.ConfigFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("pb-offline-sync auth: %v", err)
	}
	if authEnabled {
		if err := authplugin.Register(app, authCfg); err != nil {
			log.Fatalf("pb-offline-sync auth: %v", err)
		}
	}

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// pruneCron returns the configured cron expression, defaulting to nightly.
// Empty means "unset" (compose/Dokploy inject optional variables as empty
// strings), so disabling is explicit: "none" or "off".
func pruneCron() string {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("SYNC_PRUNE_CRON"))); v {
	case "":
		return "0 3 * * *"
	case "none", "off":
		return ""
	default:
		return v
	}
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

	if cfg.Media, err = mediaFromEnv(get, cfg.Collections); err != nil {
		return syncplugin.Config{}, false, err
	}

	return cfg, true, nil
}

// mediaFromEnv builds the optional media config. An empty media collection name
// disables the feature; the collection itself is provisioned by the operator
// (or the host app via syncplugin.EnsureMediaCollection) and validated at boot.
func mediaFromEnv(get func(string) string, collections map[string]syncplugin.CollectionConfig) (*syncplugin.MediaConfig, error) {
	name := strings.TrimSpace(get("SYNC_MEDIA_COLLECTION"))
	if name == "" {
		return nil, nil
	}
	mc := &syncplugin.MediaConfig{
		Collection:    name,
		MimeAllowlist: splitList(get("SYNC_MEDIA_MIME_ALLOWLIST")),
	}
	maxFileSize, err := envInt64(get, "SYNC_MEDIA_MAX_FILE_BYTES")
	if err != nil {
		return nil, err
	}
	quota, err := envInt64(get, "SYNC_MEDIA_PER_USER_QUOTA")
	if err != nil {
		return nil, err
	}
	mc.MaxFileSize = maxFileSize
	mc.PerUserQuota = quota

	// The media collection is a synced collection (§18.1); main.go owns the
	// defaults wiring, so register it when the operator has not listed it.
	if _, ok := collections[name]; !ok {
		collections[name] = syncplugin.CollectionConfig{OwnerField: "owner"}
	}
	return mc, nil
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
