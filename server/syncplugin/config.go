package syncplugin

import (
	"errors"
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// ProtocolVersion is the wire protocol version this plugin implements (protocol.md §2, §16).
const ProtocolVersion = 1

// PluginVersion is the plugin's own semver, independent of the protocol version
// (§16). Release builds override it:
//
//	-ldflags "-X github.com/karlomac/pb-offline-sync/server/syncplugin.PluginVersion=0.2.0"
var PluginVersion = "0.1.0"

// ConflictPolicy selects the server action for overlapping fields on a stale base (§8.3).
type ConflictPolicy string

const (
	ServerWins ConflictPolicy = "serverWins"
	ClientWins ConflictPolicy = "clientWins"
	Reject     ConflictPolicy = "reject"
)

// DeletePolicy selects delete behaviour on a stale base (§8.4).
type DeletePolicy string

const (
	DeleteWins DeletePolicy = "deleteWins"
	EditWins   DeletePolicy = "editWins"
)

// CollectionConfig configures one synced collection.
type CollectionConfig struct {
	// OwnerField is the collection field naming the owning auth record.
	OwnerField string

	// Conflict is the stale-base field-conflict policy (default ServerWins).
	Conflict ConflictPolicy

	// DeletePolicy is the delete-vs-edit policy (default DeleteWins).
	DeletePolicy DeletePolicy
}

// Config configures the sync plugin. Limit fields must not exceed the advertised
// values (protocol §5); zero values are replaced with the defaults below.
type Config struct {
	Collections map[string]CollectionConfig

	MaxBatch                 int
	MaxPage                  int
	DefaultPage              int
	MaxBodyBytes             int64
	IdempotencyRetentionDays int

	// ChangeLogRetention is how long sync_changes rows are kept before Prune
	// may delete them. It is deliberately server-side only: protocol §13
	// requires advertising idempotencyRetentionDays but defines no wire field
	// for change-log retention (§5), so this is NOT exposed in /info.
	ChangeLogRetention time.Duration
}

const (
	defaultMaxBatch                 = 100
	defaultMaxPage                  = 500
	defaultDefaultPage              = 200
	defaultMaxBodyBytes             = 1048576
	defaultIdempotencyRetentionDays = 14
	defaultChangeLogRetention       = 30 * 24 * time.Hour
)

func (cfg *Config) applyDefaults() {
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = defaultMaxBatch
	}
	if cfg.MaxPage <= 0 {
		cfg.MaxPage = defaultMaxPage
	}
	if cfg.DefaultPage <= 0 {
		cfg.DefaultPage = defaultDefaultPage
	}
	if cfg.DefaultPage > cfg.MaxPage {
		cfg.DefaultPage = cfg.MaxPage
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.IdempotencyRetentionDays <= 0 {
		cfg.IdempotencyRetentionDays = defaultIdempotencyRetentionDays
	}
	if cfg.ChangeLogRetention <= 0 {
		cfg.ChangeLogRetention = defaultChangeLogRetention
	}
	for name, cc := range cfg.Collections {
		if cc.Conflict == "" {
			cc.Conflict = ServerWins
		}
		if cc.DeletePolicy == "" {
			cc.DeletePolicy = DeleteWins
		}
		cfg.Collections[name] = cc
	}
}

// validate checks the config against the real collection schemas and fails fast.
func (cfg Config) validate(app core.App) error {
	if len(cfg.Collections) == 0 {
		return errors.New("syncplugin: no collections configured")
	}

	for name, cc := range cfg.Collections {
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return fmt.Errorf("syncplugin: collection %q: %w", name, err)
		}
		if col.IsView() {
			return fmt.Errorf("syncplugin: collection %q is a view collection and cannot be synced", name)
		}
		if cc.OwnerField == "" {
			return fmt.Errorf("syncplugin: collection %q: OwnerField is required", name)
		}

		f := col.Fields.GetByName(cc.OwnerField)
		if f == nil {
			return fmt.Errorf("syncplugin: collection %q: owner field %q does not exist", name, cc.OwnerField)
		}
		rf, ok := f.(*core.RelationField)
		if !ok {
			return fmt.Errorf("syncplugin: collection %q: owner field %q must be a relation field", name, cc.OwnerField)
		}
		target, err := app.FindCollectionByNameOrId(rf.CollectionId)
		if err != nil || !target.IsAuth() {
			return fmt.Errorf("syncplugin: collection %q: owner field %q must relate to an auth collection", name, cc.OwnerField)
		}

		switch cc.Conflict {
		case ServerWins, ClientWins, Reject:
		default:
			return fmt.Errorf("syncplugin: collection %q: invalid conflict policy %q", name, cc.Conflict)
		}
		switch cc.DeletePolicy {
		case DeleteWins, EditWins:
		default:
			return fmt.Errorf("syncplugin: collection %q: invalid delete policy %q", name, cc.DeletePolicy)
		}
	}

	if cfg.MaxBatch <= 0 || cfg.MaxPage <= 0 || cfg.DefaultPage <= 0 || cfg.DefaultPage > cfg.MaxPage {
		return errors.New("syncplugin: invalid page/batch limits")
	}
	if cfg.MaxBodyBytes <= 0 || cfg.IdempotencyRetentionDays <= 0 || cfg.ChangeLogRetention <= 0 {
		return errors.New("syncplugin: limit values must be positive")
	}

	return nil
}

// MustRegister registers migrations, hooks and routes for the plugin. Config is
// validated against the real collection schemas when the app serves (collections
// are only loaded then); an invalid config fails fast by returning an error from
// the OnServe hook, which aborts app startup.
func MustRegister(app core.App, cfg Config) {
	cfg.applyDefaults()
	registerMigrations()

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		if err := cfg.validate(e.App); err != nil {
			return err
		}
		bindHooks(e.App, cfg)
		bindRoutes(e.App, e.Router, cfg)
		return e.Next()
	})
}
