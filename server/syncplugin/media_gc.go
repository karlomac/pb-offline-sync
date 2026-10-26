package syncplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// Media operations (MED-5): explicit, injectable, batched — the same model as
// Prune. The plugin starts no goroutine; the host calls these on a cron.

// SweepMediaStaging removes upload sessions whose TTL has passed (protocol
// §18.6: abandoned partials must not fill the disk). Returns the removed count.
func SweepMediaStaging(app core.App, cfg Config, now time.Time) (int, error) {
	if cfg.Media == nil {
		return 0, nil
	}
	root := cfg.Media.stagingRoot(app)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		s, found, _ := loadSession(dir)
		deadline := time.Time{}
		if found {
			deadline, _ = time.Parse(time.RFC3339, s.Expires)
		} else {
			// A corrupted/missing sidecar can never be classified; sweep by
			// directory mtime so it cannot leak staged bytes forever (§18.6).
			info, serr := entry.Info()
			if serr != nil {
				continue
			}
			deadline = info.ModTime().Add(cfg.Media.StagingTTL)
		}
		if !deadline.IsZero() && now.After(deadline) {
			if err := os.RemoveAll(dir); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// MediaGC deletes unreferenced media records older than MediaConfig.GCGrace
// (protocol §18.1: bytes live until the last inbound relation goes). A record
// is referenced when any relation field of any other synced collection targets
// it. Deletes go through the standard delete path, so tombstone revs and the
// change log are written like any other deletion. Returns the deleted count.
func MediaGC(app core.App, cfg Config, now time.Time) (int, error) {
	if cfg.Media == nil {
		return 0, nil
	}
	mc := cfg.Media
	mediaCol, err := app.FindCollectionByNameOrId(mc.Collection)
	if err != nil {
		return 0, err
	}

	// Collect every referenced media ID across all synced collections' relation
	// fields that target the media collection. shortcut: full scans per relation
	// field — fine at personal-app scale; move to per-record refcounts if this
	// ever shows up in profiles.
	referenced := map[string]bool{}
	for name, cc := range cfg.Collections {
		if name == mc.Collection || !identRe.MatchString(name) {
			continue
		}
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return 0, err
		}
		for _, f := range col.Fields {
			rf, ok := f.(*core.RelationField)
			if !ok || rf.CollectionId != mediaCol.Id || !identRe.MatchString(rf.GetName()) {
				continue
			}
			var rows []struct {
				V *string `db:"v"`
			}
			q := fmt.Sprintf(`SELECT "%s" AS v FROM "%s" WHERE "%s" IS NOT NULL`,
				rf.GetName(), name, cc.OwnerField)
			if err := app.DB().NewQuery(q).All(&rows); err != nil {
				return 0, err
			}
			for _, row := range rows {
				if row.V == nil {
					continue
				}
				for _, id := range relationValueIDs(*row.V) {
					referenced[id] = true
				}
			}
		}
	}

	cutoff := now.Add(-mc.GCGrace).UTC().Format("2006-01-02 15:04:05.000Z")
	deleted := 0
	for {
		recs, err := app.FindRecordsByFilter(mc.Collection,
			"updated < {:cutoff}", "", gcBatch, 0,
			map[string]any{"cutoff": cutoff})
		if err != nil {
			return deleted, err
		}
		progressed := false
		for _, rec := range recs {
			if referenced[rec.Id] {
				continue
			}
			if err := app.Delete(rec); err != nil {
				return deleted, err
			}
			deleted++
			progressed = true
		}
		if !progressed || len(recs) < gcBatch {
			return deleted, nil
		}
	}
}

const gcBatch = 200

// relationValueIDs parses a relation column value: a single ID, or a JSON array
// of IDs (PocketBase stores multi-select relations as JSON).
func relationValueIDs(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if strings.HasPrefix(v, "[") {
		var ids []string
		if json.Unmarshal([]byte(v), &ids) == nil {
			return ids
		}
		return nil
	}
	return []string{v}
}
