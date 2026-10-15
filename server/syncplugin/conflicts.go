package syncplugin

import (
	"encoding/json"

	"github.com/pocketbase/pocketbase/core"
)

// Conflict is one unresolved sync_conflicts row (PDD §8.7 diagnostics).
type Conflict struct {
	ID         string
	Collection string
	RecordID   string
	MutationID string
	WinningRev int
	Created    string
	// Losing is the decoded losing_fields map (nil if it could not be decoded).
	Losing map[string]any
}

// ListUnresolvedConflicts returns the unresolved sync_conflicts rows for a
// collection/record, ordered oldest first. An empty collection or recordID is a
// wildcard, so it can serve a per-record or per-collection report (PDD §8.7).
func ListUnresolvedConflicts(app core.App, collection, recordID string) ([]Conflict, error) {
	q := `SELECT id, collection, record_id, mutation_id, winning_rev, created, losing_fields
		FROM sync_conflicts WHERE resolved = FALSE`
	params := map[string]any{}
	if collection != "" {
		q += " AND collection = {:c}"
		params["c"] = collection
	}
	if recordID != "" {
		q += " AND record_id = {:r}"
		params["r"] = recordID
	}
	q += " ORDER BY created ASC, id ASC"

	var rows []struct {
		ID           string `db:"id"`
		Collection   string `db:"collection"`
		RecordID     string `db:"record_id"`
		MutationID   string `db:"mutation_id"`
		WinningRev   int    `db:"winning_rev"`
		Created      string `db:"created"`
		LosingFields string `db:"losing_fields"`
	}
	if err := app.DB().NewQuery(q).Bind(params).All(&rows); err != nil {
		return nil, err
	}

	out := make([]Conflict, 0, len(rows))
	for _, r := range rows {
		c := Conflict{
			ID:         r.ID,
			Collection: r.Collection,
			RecordID:   r.RecordID,
			MutationID: r.MutationID,
			WinningRev: r.WinningRev,
			Created:    r.Created,
		}
		_ = json.Unmarshal([]byte(r.LosingFields), &c.Losing)
		out = append(out, c)
	}
	return out, nil
}
