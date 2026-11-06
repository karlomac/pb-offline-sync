package authplugin

import (
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/karlomac/pb-offline-sync/server/syncplugin"
)

// TestSyncedCollectionWritesUnaffected registers both plugins and proves an
// ordinary synced-collection create still writes sync_meta and the change log.
func TestSyncedCollectionWritesUnaffected(t *testing.T) {
	app := newTestApp(t)

	users, err := app.FindCollectionByNameOrId(usersCollection)
	if err != nil {
		t.Fatalf("find users: %v", err)
	}

	notes := core.NewBaseCollection("notes")
	notes.Fields.Add(&core.TextField{Name: "title", Required: true})
	notes.Fields.Add(&core.RelationField{Name: "owner", CollectionId: users.Id, MaxSelect: 1})
	if err := app.Save(notes); err != nil {
		t.Fatalf("save notes collection: %v", err)
	}

	syncplugin.MustRegister(app, syncplugin.Config{
		Collections: map[string]syncplugin.CollectionConfig{
			"notes": {OwnerField: "owner"},
		},
	})
	if err := Register(app, passwordCfg()); err != nil {
		t.Fatalf("Register auth plugin: %v", err)
	}
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	serve := &core.ServeEvent{App: app, Router: router}
	if err := app.OnServe().Trigger(serve, func(e *core.ServeEvent) error { return e.Next() }); err != nil {
		t.Fatalf("OnServe: %v", err)
	}

	alice := core.NewRecord(users)
	alice.Set("email", "alice@example.com")
	alice.Set("password", "1234567890")
	alice.Set("passwordConfirm", "1234567890")
	alice.Set("verified", true)
	if err := app.Save(alice); err != nil {
		t.Fatalf("save user: %v", err)
	}

	note := core.NewRecord(notes)
	note.Set("title", "hello")
	note.Set("owner", alice.Id)
	if err := app.Save(note); err != nil {
		t.Fatalf("save note: %v", err)
	}

	var rev int
	if err := app.DB().NewQuery("SELECT rev FROM sync_meta WHERE collection = {:c} AND record_id = {:r}").
		Bind(map[string]any{"c": "notes", "r": note.Id}).Row(&rev); err != nil {
		t.Fatalf("read sync_meta: %v", err)
	}
	if rev != 1 {
		t.Fatalf("sync_meta rev = %d, want 1", rev)
	}

	var changes int
	if err := app.DB().NewQuery("SELECT COUNT(*) FROM sync_changes WHERE collection = 'notes'").Row(&changes); err != nil {
		t.Fatalf("count sync_changes: %v", err)
	}
	if changes != 1 {
		t.Fatalf("sync_changes rows = %d, want 1", changes)
	}
}
