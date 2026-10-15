package syncplugin

import (
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// registerErr builds a minimal app, runs MustRegister with the config returned
// by build, and returns any error from the OnServe validation.
func registerErr(t *testing.T, build func(app *tests.TestApp) Config) error {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(app.Cleanup)

	users := core.NewAuthCollection("syncusers")
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}

	cfg := build(app)
	MustRegister(app, cfg)

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	se := &core.ServeEvent{App: app, Router: router}
	return app.OnServe().Trigger(se, func(e *core.ServeEvent) error { return e.Next() })
}

func TestConfigValidationFailsFast(t *testing.T) {
	t.Run("missing owner field", func(t *testing.T) {
		err := registerErr(t, func(app *tests.TestApp) Config {
			col := core.NewBaseCollection("notes")
			col.Fields.Add(&core.TextField{Name: "title"})
			if err := app.Save(col); err != nil {
				t.Fatal(err)
			}
			return Config{Collections: map[string]CollectionConfig{
				"notes": {OwnerField: "owner"},
			}}
		})
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("want missing-owner-field error, got %v", err)
		}
	})

	t.Run("owner field must be a relation", func(t *testing.T) {
		err := registerErr(t, func(app *tests.TestApp) Config {
			col := core.NewBaseCollection("notes")
			col.Fields.Add(&core.TextField{Name: "owner"})
			if err := app.Save(col); err != nil {
				t.Fatal(err)
			}
			return Config{Collections: map[string]CollectionConfig{
				"notes": {OwnerField: "owner"},
			}}
		})
		if err == nil || !strings.Contains(err.Error(), "relation field") {
			t.Fatalf("want relation-field error, got %v", err)
		}
	})

	t.Run("owner relation must target an auth collection", func(t *testing.T) {
		err := registerErr(t, func(app *tests.TestApp) Config {
			teams := core.NewBaseCollection("teams")
			if err := app.Save(teams); err != nil {
				t.Fatal(err)
			}
			col := core.NewBaseCollection("notes")
			col.Fields.Add(&core.RelationField{Name: "owner", CollectionId: teams.Id, MaxSelect: 1})
			if err := app.Save(col); err != nil {
				t.Fatal(err)
			}
			return Config{Collections: map[string]CollectionConfig{
				"notes": {OwnerField: "owner"},
			}}
		})
		if err == nil || !strings.Contains(err.Error(), "auth collection") {
			t.Fatalf("want auth-collection error, got %v", err)
		}
	})

	t.Run("unknown collection", func(t *testing.T) {
		err := registerErr(t, func(app *tests.TestApp) Config {
			return Config{Collections: map[string]CollectionConfig{
				"nope": {OwnerField: "owner"},
			}}
		})
		if err == nil {
			t.Fatal("want unknown-collection error, got nil")
		}
	})

	t.Run("valid config", func(t *testing.T) {
		err := registerErr(t, func(app *tests.TestApp) Config {
			users, err := app.FindCollectionByNameOrId("syncusers")
			if err != nil {
				t.Fatal(err)
			}
			col := core.NewBaseCollection("notes")
			col.Fields.Add(&core.TextField{Name: "title"})
			col.Fields.Add(&core.RelationField{Name: "owner", CollectionId: users.Id, MaxSelect: 1})
			if err := app.Save(col); err != nil {
				t.Fatal(err)
			}
			return Config{Collections: map[string]CollectionConfig{
				"notes": {OwnerField: "owner"},
			}}
		})
		if err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
	})
}
