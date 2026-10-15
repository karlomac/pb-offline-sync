package syncplugin

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// cursor is the opaque pull position. Clients MUST NOT parse it (protocol §7.4).
type cursor struct {
	V         int    `json:"v"`
	Kind      string `json:"k"`           // "snapshot" | "inc"
	Head      int64  `json:"h,omitempty"` // snapshot head H (§7.3)
	AfterColl string `json:"c,omitempty"` // snapshot keyset: last delivered collection
	AfterID   string `json:"i,omitempty"` // snapshot keyset: last delivered record id
	Seq       int64  `json:"s,omitempty"` // incremental position: last delivered seq
}

const (
	cursorKindSnapshot    = "snapshot"
	cursorKindIncremental = "inc"
)

func encodeCursor(c cursor) string {
	c.V = 1
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (cursor, error) {
	var c cursor
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.V != 1 {
		return c, errors.New("unsupported cursor version")
	}
	if c.Kind != cursorKindSnapshot && c.Kind != cursorKindIncremental {
		return c, errors.New("invalid cursor kind")
	}
	return c, nil
}

// handlePull serves GET /api/sync/pull (protocol §7).
func handlePull(e *core.RequestEvent, cfg Config) error {
	if !protocolOK(e) {
		return nil
	}

	uid := e.Auth.Id

	limit := cfg.DefaultPage
	if v := e.Request.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > cfg.MaxPage {
		limit = cfg.MaxPage
	}

	raw := e.Request.URL.Query().Get("cursor")
	if raw == "" {
		// Head is a sentinel until the snapshot captures H on its first page.
		return pullSnapshot(e, cfg, uid, cursor{Kind: cursorKindSnapshot, Head: -1}, limit)
	}

	cur, err := decodeCursor(raw)
	if err != nil {
		return writeError(e, http.StatusBadRequest, "cursor_invalid", "Malformed cursor.", nil)
	}

	head, oldest, err := logBounds(e.App)
	if err != nil {
		return err
	}

	if cur.Kind == cursorKindSnapshot {
		if cur.Head > head {
			return writeError(e, http.StatusGone, "cursor_too_old", "Cursor is ahead of the server head.", nil)
		}
		return pullSnapshot(e, cfg, uid, cur, limit)
	}

	// Incremental: a cursor older than retained history or ahead of head must
	// force a full resync (protocol §7.4, §13).
	if cur.Seq > head || cur.Seq < oldest {
		return writeError(e, http.StatusGone, "cursor_too_old", "Cursor is stale or ahead of the server head.", nil)
	}
	return pullIncremental(e, cfg, uid, cur.Seq, limit)
}

// logBounds returns the current change-log head and oldest retained seq (0 when
// the log is empty).
func logBounds(app core.App) (head, oldest int64, err error) {
	var row struct {
		Head   int64 `db:"head"`
		Oldest int64 `db:"oldest"`
	}
	err = app.DB().NewQuery(
		"SELECT COALESCE(MAX(seq), 0) AS head, COALESCE(MIN(seq), 0) AS oldest FROM sync_changes",
	).One(&row)
	return row.Head, row.Oldest, err
}

// pullIncremental delivers the latest state of each record owned by uid whose
// latest change seq is greater than after (protocol §7.2). Revs and records are
// loaded one query per collection, not one query per row.
func pullIncremental(e *core.RequestEvent, cfg Config, uid string, after int64, limit int) error {
	var rows []struct {
		Collection string `db:"collection"`
		RecordID   string `db:"record_id"`
		Seq        int64  `db:"seq"`
	}
	err := e.App.DB().NewQuery(`
		SELECT collection, record_id, MAX(seq) AS seq
		FROM sync_changes
		WHERE owner = {:owner} AND seq > {:after}
		GROUP BY collection, record_id
		ORDER BY seq ASC
		LIMIT {:limit}`).
		Bind(map[string]any{"owner": uid, "after": after, "limit": limit + 1}).
		All(&rows)
	if err != nil {
		return err
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	colls := make([]string, len(rows))
	ids := make([]string, len(rows))
	for i, r := range rows {
		colls[i] = r.Collection
		ids[i] = r.RecordID
	}
	revsByColl, recsByColl, err := loadPage(e.App, cfg, colls, ids)
	if err != nil {
		return err
	}

	changes := make([]map[string]any, 0, len(rows))
	next := after
	for _, r := range rows {
		next = r.Seq
		change, err := pageChange(e.App, cfg, uid, r.Collection, r.RecordID, revsByColl, recsByColl)
		if err != nil {
			return err
		}
		if change != nil {
			changes = append(changes, change)
		}
	}

	return e.JSON(http.StatusOK, map[string]any{
		"mode":       "incremental",
		"changes":    changes,
		"cursor":     encodeCursor(cursor{Kind: cursorKindIncremental, Seq: next}),
		"hasMore":    hasMore,
		"serverTime": serverTime(),
	})
}

// loadPage batches the sync_meta revs and the records for a page of
// (collection, recordId) pairs, keyed by collection.
func loadPage(app core.App, cfg Config, colls, ids []string) (map[string]map[string]int64, map[string]map[string]*core.Record, error) {
	idByColl := map[string][]string{}
	for i := range colls {
		if _, ok := cfg.Collections[colls[i]]; !ok {
			continue
		}
		idByColl[colls[i]] = append(idByColl[colls[i]], ids[i])
	}
	revs := make(map[string]map[string]int64, len(idByColl))
	recs := make(map[string]map[string]*core.Record, len(idByColl))
	for coll, list := range idByColl {
		r, err := revsFor(app, coll, list)
		if err != nil {
			return nil, nil, err
		}
		revs[coll] = r
		found, err := app.FindRecordsByIds(coll, list)
		if err != nil {
			return nil, nil, err
		}
		m := make(map[string]*core.Record, len(found))
		for _, rec := range found {
			m[rec.Id] = rec
		}
		recs[coll] = m
	}
	return revs, recs, nil
}

// pageChange emits the current state for one (collection, record): upsert when
// the record exists, delete when it no longer does (protocol §7.2.5).
func pageChange(app core.App, cfg Config, uid, collection, recordID string, revs map[string]map[string]int64, recs map[string]map[string]*core.Record) (map[string]any, error) {
	cc, ok := cfg.Collections[collection]
	if !ok {
		return nil, nil
	}
	rev, ok := revs[collection][recordID]
	if !ok {
		r, err := revFor(app, collection, recordID)
		if err != nil {
			return nil, err
		}
		rev = r
	}
	rec := recs[collection][recordID]
	if rec == nil {
		return map[string]any{
			"collection": collection, "recordId": recordID, "op": "delete", "rev": rev,
		}, nil
	}
	// The log said uid owns this change; never return another owner's data.
	if ownerOf(rec, cc.OwnerField) != uid {
		return nil, nil
	}
	return map[string]any{
		"collection": collection, "recordId": recordID, "op": "upsert", "rev": rev, "record": rec,
	}, nil
}

// revsFor returns the sync_meta rev for each id, in one query.
func revsFor(app core.App, collection string, ids []string) (map[string]int64, error) {
	out := make(map[string]int64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	params := map[string]any{"c": collection}
	for i, id := range ids {
		key := "id" + strconv.Itoa(i)
		placeholders[i] = "{:" + key + "}"
		params[key] = id
	}
	var rows []struct {
		RecordID string `db:"record_id"`
		Rev      int64  `db:"rev"`
	}
	err := app.DB().NewQuery(
		"SELECT record_id, rev FROM sync_meta WHERE collection = {:c} AND record_id IN (" +
			strings.Join(placeholders, ",") + ")",
	).Bind(params).All(&rows)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.RecordID] = r.Rev
	}
	return out, nil
}

// revFor returns the current rev, falling back to the latest logged rev, then 1
// for a pre-existing record with no sync history (protocol §17.1.1).
func revFor(app core.App, collection, recordID string) (int64, error) {
	var row struct {
		Rev int64 `db:"rev"`
	}
	err := app.DB().NewQuery("SELECT rev FROM sync_meta WHERE collection = {:c} AND record_id = {:r}").
		Bind(map[string]any{"c": collection, "r": recordID}).
		One(&row)
	if err == nil {
		return row.Rev, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	err = app.DB().NewQuery(
		"SELECT rev FROM sync_changes WHERE collection = {:c} AND record_id = {:r} ORDER BY seq DESC LIMIT 1",
	).Bind(map[string]any{"c": collection, "r": recordID}).One(&row)
	if err == nil {
		return row.Rev, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return 1, nil
}

// pullSnapshot implements the snapshot + head-cursor strategy (protocol §7.3).
func pullSnapshot(e *core.RequestEvent, cfg Config, uid string, cur cursor, limit int) error {
	head, _, err := logBounds(e.App)
	if err != nil {
		return err
	}

	// Capture H on the first page (Head == -1); later pages carry it in the
	// cursor. A captured H of 0 is legitimate for an empty log.
	snapshotHead := cur.Head
	if snapshotHead < 0 {
		snapshotHead = head
	}

	type item struct {
		collection string
		record     *core.Record
	}
	var items []item

	for _, name := range sortedCollectionNames(cfg) {
		if cur.AfterColl != "" && name < cur.AfterColl {
			continue
		}

		cc := cfg.Collections[name]
		filter := cc.OwnerField + " = {:owner}"
		params := dbx.Params{"owner": uid}
		if name == cur.AfterColl && cur.AfterID != "" {
			filter += " && id > {:after}"
			params["after"] = cur.AfterID
		}

		records, err := e.App.FindRecordsByFilter(name, filter, "id", limit+1, 0, params)
		if err != nil {
			return err
		}
		for _, r := range records {
			items = append(items, item{name, r})
			if len(items) > limit {
				break
			}
		}
		if len(items) > limit {
			break
		}
	}

	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}

	changes := make([]map[string]any, 0, len(items))
	colls := make([]string, len(items))
	ids := make([]string, len(items))
	for i, it := range items {
		colls[i] = it.collection
		ids[i] = it.record.Id
	}
	revsByColl, _, err := loadPage(e.App, cfg, colls, ids)
	if err != nil {
		return err
	}
	for _, it := range items {
		rev, ok := revsByColl[it.collection][it.record.Id]
		if !ok {
			rev, err = revFor(e.App, it.collection, it.record.Id)
			if err != nil {
				return err
			}
		}
		changes = append(changes, map[string]any{
			"collection": it.collection,
			"recordId":   it.record.Id,
			"op":         "upsert",
			"rev":        rev,
			"record":     it.record,
		})
	}

	next := cursor{Kind: cursorKindSnapshot, Head: snapshotHead}
	if hasMore {
		last := items[len(items)-1]
		next.AfterColl = last.collection
		next.AfterID = last.record.Id
	} else {
		// Snapshot exhausted: the next request is incremental from H (§7.3.3).
		next = cursor{Kind: cursorKindIncremental, Seq: snapshotHead}
	}

	return e.JSON(http.StatusOK, map[string]any{
		"mode":       "snapshot",
		"changes":    changes,
		"cursor":     encodeCursor(next),
		"hasMore":    hasMore,
		"serverTime": serverTime(),
	})
}

func sortedCollectionNames(cfg Config) []string {
	names := make([]string, 0, len(cfg.Collections))
	for name := range cfg.Collections {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
