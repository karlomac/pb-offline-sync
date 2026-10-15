package syncplugin

import (
	"encoding/json"
	"reflect"

	"github.com/pocketbase/pocketbase/core"
)

// Change-log op values (protocol §8.5).
const (
	opCreate = "create"
	opUpdate = "update"
	opDelete = "delete"
)

// bindHooks binds the in-transaction *Execute hooks for every configured
// collection. *AfterSuccess hooks are post-commit and MUST NOT be used for the
// change log (protocol §17.1.4; evidence server/syncplugin/spike_test.go).
func bindHooks(app core.App, cfg Config) {
	for name, cc := range cfg.Collections {
		name, cc := name, cc

		app.OnRecordCreateExecute(name).BindFunc(func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			// A create starts at rev 1 and its changed set is all fields (§8.2).
			return writeChange(e.App, name, e.Record.Id, opCreate,
				ownerOf(e.Record, cc.OwnerField), 1, allFieldNames(e.Record.Collection()))
		})

		app.OnRecordUpdateExecute(name).BindFunc(func(e *core.RecordEvent) error {
			changed := diffFields(e.Record, e.Record.Original())
			if err := e.Next(); err != nil {
				return err
			}
			if len(changed) == 0 {
				// Identical resulting state: a no-op may skip the increment (§8.1).
				return nil
			}
			rev, err := nextRev(e.App, name, e.Record.Id)
			if err != nil {
				return err
			}
			return writeChange(e.App, name, e.Record.Id, opUpdate,
				ownerOf(e.Record, cc.OwnerField), rev, changed)
		})

		app.OnRecordDeleteExecute(name).BindFunc(func(e *core.RecordEvent) error {
			owner := ownerOf(e.Record, cc.OwnerField)
			if err := e.Next(); err != nil {
				return err
			}
			// A delete is a write producing a final rev; the meta row is retained
			// as a tombstone rather than deleted (protocol §8.1, §17.1.1).
			rev, err := nextRev(e.App, name, e.Record.Id)
			if err != nil {
				return err
			}
			return writeChange(e.App, name, e.Record.Id, opDelete, owner, rev, []string{})
		})
	}
}

// writeChange upserts sync_meta to rev/owner and appends the change row, both
// through the event's tx-scoped DB so they roll back with the data write
// (protocol §8.5).
func writeChange(app core.App, collection, recordID, op, owner string, rev int, changed []string) error {
	_, err := app.DB().NewQuery(`INSERT INTO sync_meta (collection, record_id, rev, owner)
		VALUES ({:c}, {:r}, {:rev}, {:o})
		ON CONFLICT (collection, record_id) DO UPDATE SET rev = {:rev}, owner = {:o}`).
		Bind(map[string]any{"c": collection, "r": recordID, "rev": rev, "o": owner}).
		Execute()
	if err != nil {
		return err
	}

	raw, err := json.Marshal(changed)
	if err != nil {
		return err
	}

	return appendChange(app, collection, recordID, op, owner, rev, string(raw))
}

// appendChange writes one sync_changes row. created is set from SQLite's clock
// so it is correct even on a table altered to add the column with an empty
// default (see prune.go); it is only used for retention, never for ordering.
func appendChange(app core.App, collection, recordID, op, owner string, rev int, changedJSON string) error {
	_, err := app.DB().NewQuery(`INSERT INTO sync_changes (collection, record_id, op, owner, rev, changed_fields, created)
		VALUES ({:c}, {:r}, {:op}, {:o}, {:rev}, {:cf}, strftime('%Y-%m-%d %H:%M:%fZ'))`).
		Bind(map[string]any{
			"c": collection, "r": recordID, "op": op, "o": owner, "rev": rev, "cf": changedJSON,
		}).
		Execute()
	return err
}

// nextRev returns the current rev + 1. It shares revFor's derivation so read
// and write paths can never disagree: a record with no sync_meta row but with
// change-log history must continue from its logged rev, and a record with no
// history at all is at rev 1, so its first write is rev 2 — not a reuse of the
// rev 1 the snapshot already advertised (protocol §8.1).
func nextRev(app core.App, collection, recordID string) (int, error) {
	rev, err := revFor(app, collection, recordID)
	if err != nil {
		return 0, err
	}
	return int(rev) + 1, nil
}

func ownerOf(record *core.Record, ownerField string) string {
	v, _ := record.Get(ownerField).(string)
	return v
}

func allFieldNames(col *core.Collection) []string {
	names := make([]string, 0, len(col.Fields))
	for _, f := range col.Fields {
		names = append(names, f.GetName())
	}
	return names
}

// diffFields returns the names of collection fields whose persisted values
// differ between cur and orig (protocol §8.2). Values are compared through the
// field's DB representation so equivalent typed values compare equal.
func diffFields(cur, orig *core.Record) []string {
	var changed []string
	for _, f := range cur.Collection().Fields {
		if !reflect.DeepEqual(driverValue(f, cur), driverValue(f, orig)) {
			changed = append(changed, f.GetName())
		}
	}
	return changed
}

func driverValue(f core.Field, record *core.Record) any {
	if dv, ok := f.(core.DriverValuer); ok {
		if v, err := dv.DriverValue(record); err == nil {
			return v
		}
	}
	return record.GetRaw(f.GetName())
}
