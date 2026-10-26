package syncplugin

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/core"
)

// recordIDPattern is the PocketBase default record id format (protocol §3).
var recordIDPattern = regexp.MustCompile(`^[a-z0-9]{15}$`)

// Mutation statuses (protocol §6.5).
const (
	statusOK        = "ok"
	statusDuplicate = "duplicate"
	statusConflict  = "conflict"
	statusRejected  = "rejected"
)

// Per-mutation codes (protocol §12.2).
const (
	codeCollectionNotSynced  = "collection_not_synced"
	codeInvalidOp            = "invalid_op"
	codeInvalidID            = "invalid_id"
	codeInvalidBaseRev       = "invalid_base_rev"
	codeUnknownField         = "unknown_field"
	codeForbiddenField       = "forbidden_field"
	codeFileFieldUnsupported = "file_field_unsupported"
	codeValidationFailed     = "validation_failed"
	codeUniqueViolation      = "unique_violation"
	codeIDUnavailable        = "id_unavailable"
	codeNotFound             = "not_found"
	codeDependencyFailed     = "dependency_failed"
	codeMutationIDReuse      = "mutation_id_reuse"
	codeFieldConflict        = "field_conflict"
	codeDeleted              = "deleted"
	codeExists               = "exists"
	codeEditedSince          = "edited_since"
)

// pushRequest is the POST /api/sync/push body (protocol §6.1). client is
// informational only.
type pushRequest struct {
	Client    json.RawMessage `json:"client"`
	Mutations []pushMutation  `json:"mutations"`
}

type pushMutation struct {
	ID         string                     `json:"id"`
	Collection string                     `json:"collection"`
	RecordID   string                     `json:"recordId"`
	Op         string                     `json:"op"`
	BaseRev    *int                       `json:"baseRev"`
	Fields     map[string]json.RawMessage `json:"fields"`
}

// canonicalMutation is the deterministic payload used for the idempotency hash
// (protocol §9). Go marshals map keys in sorted order, so equal payloads hash
// equal.
type canonicalMutation struct {
	Collection string                     `json:"collection"`
	RecordID   string                     `json:"recordId"`
	Op         string                     `json:"op"`
	BaseRev    *int                       `json:"baseRev"`
	Fields     map[string]json.RawMessage `json:"fields"`
}

// outcome is the per-mutation result plus its effective status for dependency
// tracking (a duplicate carries its original status, protocol §6.2 rule 3).
type outcome struct {
	res       map[string]any
	effective string
}

// handlePush serves POST /api/sync/push (protocol §6, §8, §9).
func handlePush(e *core.RequestEvent, cfg Config) error {
	if !protocolOK(e) {
		return nil
	}

	req, ok := readPushRequest(e, cfg)
	if !ok {
		return nil
	}

	uid := e.Auth.Id
	results := make([]map[string]any, 0, len(req.Mutations))
	// failed tracks records whose earlier mutation in this request did not
	// succeed, so later same-record mutations are dependency_failed (§6.2.3).
	failed := make(map[string]bool)

	for _, m := range req.Mutations {
		out, err := runMutation(e.App, cfg, uid, m, failed[m.Collection+"\x00"+m.RecordID])
		if err != nil {
			// A transient failure: earlier mutations are already committed and
			// the client resends the batch with the same IDs (§6.6, §9). No
			// result is stored for the failing mutation.
			return writeError(e, http.StatusInternalServerError, "server_error", "Mutation could not be applied.", nil)
		}
		if out.effective != statusOK {
			failed[m.Collection+"\x00"+m.RecordID] = true
		}
		results = append(results, out.res)
	}

	return e.JSON(http.StatusOK, map[string]any{"results": results})
}

// readPushRequest enforces the body-size and batch limits (protocol §6.1) and
// decodes the request. On failure it writes the error response and returns ok
// false.
//
// Resource limits (protocol §14.7): maxBatch and maxBodyBytes are enforced here,
// maxPage/defaultPage in pull.go. Rate limiting is RECOMMENDED, not implemented
// in-plugin: put the /api/sync/* routes behind PocketBase's rate limiter or a
// reverse proxy and answer with 429 rate_limited + Retry-After (protocol §12.1).
func readPushRequest(e *core.RequestEvent, cfg Config) (pushRequest, bool) {
	var req pushRequest

	raw, err := io.ReadAll(http.MaxBytesReader(e.Response, e.Request.Body, cfg.MaxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			_ = writeError(e, http.StatusRequestEntityTooLarge, "batch_too_large",
				"Request body exceeds maxBodyBytes.", nil)
			return req, false
		}
		_ = writeError(e, http.StatusBadRequest, "invalid_request", "Could not read request body.", nil)
		return req, false
	}

	if len(raw) == 0 || json.Unmarshal(raw, &req) != nil {
		_ = writeError(e, http.StatusBadRequest, "invalid_request", "Malformed or empty JSON body.", nil)
		return req, false
	}
	if len(req.Mutations) == 0 {
		_ = writeError(e, http.StatusBadRequest, "batch_too_large", "mutations must be non-empty.", nil)
		return req, false
	}
	if len(req.Mutations) > cfg.MaxBatch {
		_ = writeError(e, http.StatusRequestEntityTooLarge, "batch_too_large", "Batch exceeds maxBatch.", nil)
		return req, false
	}

	return req, true
}

// runMutation applies one mutation in its own transaction, storing any final
// result in the same transaction (protocol §6.2.2, §9). The returned error is
// always transient.
func runMutation(app core.App, cfg Config, uid string, m pushMutation, depFailed bool) (outcome, error) {
	var out outcome
	err := app.RunInTransaction(func(tx core.App) error {
		o, err := applyMutation(tx, cfg, uid, m, depFailed)
		if err != nil {
			return err
		}
		// Only final outcomes are stored; a duplicate re-reads its stored
		// original, and mutation_id_reuse must not overwrite it (protocol §9).
		if o.res["status"] != statusDuplicate && o.res["code"] != codeMutationIDReuse {
			hash, herr := hashMutation(m)
			if herr != nil {
				return herr
			}
			if serr := storeProcessed(tx, uid, m.ID, hash, o.res); serr != nil {
				return serr
			}
		}
		out = o
		return nil
	})
	return out, err
}

// applyMutation validates and applies a single mutation, returning a final
// outcome. A non-nil error is a transient failure (nothing stored, tx rolled
// back).
func applyMutation(app core.App, cfg Config, uid string, m pushMutation, depFailed bool) (outcome, error) {
	// Idempotency is checked before validation so a reused mutation ID with a
	// different payload is always mutation_id_reuse (protocol §9).
	hash, err := hashMutation(m)
	if err != nil {
		return outcome{}, err
	}
	prevHash, prevResult, found, err := lookupProcessed(app, uid, m.ID)
	if err != nil {
		return outcome{}, err
	}
	if found {
		if prevHash == hash {
			return duplicateOutcome(m.ID, prevResult), nil
		}
		return rejectedOutcome(m.ID, codeMutationIDReuse), nil
	}

	if depFailed {
		return rejectedOutcome(m.ID, codeDependencyFailed), nil
	}

	cc, ok := cfg.Collections[m.Collection]
	if !ok {
		return rejectedOutcome(m.ID, codeCollectionNotSynced), nil
	}
	// Media records are immutable and server-created (protocol §18.1): push
	// never creates, updates or deletes them.
	if cfg.Media != nil && m.Collection == cfg.Media.Collection {
		return rejectedOutcome(m.ID, codeMediaImmutable), nil
	}
	col, err := app.FindCollectionByNameOrId(m.Collection)
	if err != nil {
		return rejectedOutcome(m.ID, codeCollectionNotSynced), nil
	}

	switch m.Op {
	case opCreate, opUpdate, opDelete:
	default:
		return rejectedOutcome(m.ID, codeInvalidOp), nil
	}

	if !recordIDPattern.MatchString(m.RecordID) {
		return rejectedOutcome(m.ID, codeInvalidID), nil
	}

	if m.Op == opCreate {
		if m.BaseRev != nil {
			return rejectedOutcome(m.ID, codeInvalidBaseRev), nil
		}
	} else if m.BaseRev == nil || *m.BaseRev < 1 {
		return rejectedOutcome(m.ID, codeInvalidBaseRev), nil
	}

	if code := validateFields(col, cc, m.Fields); code != "" {
		return rejectedOutcome(m.ID, code), nil
	}
	fields, err := decodeFields(m.Fields)
	if err != nil {
		return rejectedOutcome(m.ID, codeValidationFailed), nil
	}

	switch m.Op {
	case opCreate:
		return applyCreate(app, col, cc, uid, m, fields)
	case opUpdate:
		return applyUpdate(app, col, cc, uid, m, fields)
	default:
		return applyDelete(app, cc, uid, m)
	}
}

// validateFields rejects server-authoritative, unknown and file fields
// (protocol §4.1). Returns "" when every key is client-writable.
func validateFields(col *core.Collection, cc CollectionConfig, raw map[string]json.RawMessage) string {
	for name := range raw {
		switch name {
		// Server-authoritative fields (protocol §4.1) are rejected even when the
		// collection does not declare them (e.g. created/updated collectionId).
		case "id", "collectionId", "collectionName", "expand", "created", "updated", cc.OwnerField:
			return codeForbiddenField
		}
		f := col.Fields.GetByName(name)
		if f == nil {
			return codeUnknownField
		}
		if f.Type() == fieldTypeFile {
			return codeFileFieldUnsupported
		}
		if f.GetSystem() {
			return codeForbiddenField
		}
	}
	return ""
}

func applyCreate(app core.App, col *core.Collection, cc CollectionConfig, uid string, m pushMutation, fields map[string]any) (outcome, error) {
	existing, err := app.FindRecordById(m.Collection, m.RecordID)
	switch {
	case err == nil:
		if ownerOf(existing, cc.OwnerField) == uid {
			// Owned by the caller: conflict/exists with the current state (§6.3).
			rev, rerr := revFor(app, m.Collection, m.RecordID)
			if rerr != nil {
				return outcome{}, rerr
			}
			r := int(rev)
			return outcome{
				res:       conflictResult(m.ID, codeExists, existing, r, ""),
				effective: statusConflict,
			}, nil
		}
		// Another user's ID: indistinguishable from a deleted one (§6.3, §14.5).
		return rejectedOutcome(m.ID, codeIDUnavailable), nil
	case !errors.Is(err, sql.ErrNoRows):
		return outcome{}, err
	}

	// The record is gone; a retained sync_meta row means the ID was deleted and
	// must not be reused (protocol §3).
	used, err := hasMeta(app, m.Collection, m.RecordID)
	if err != nil {
		return outcome{}, err
	}
	if used {
		return rejectedOutcome(m.ID, codeIDUnavailable), nil
	}

	rec := core.NewRecord(col)
	rec.Id = m.RecordID
	rec.Set(cc.OwnerField, uid)
	rec.Load(fields)
	if err := app.Save(rec); err != nil {
		return saveErrorOutcome(m.ID, err)
	}

	rev, err := revFor(app, m.Collection, m.RecordID)
	if err != nil {
		return outcome{}, err
	}
	r := int(rev)
	return outcome{
		res:       okResult(m.ID, r, rec, nil, ""),
		effective: statusOK,
	}, nil
}

func applyUpdate(app core.App, col *core.Collection, cc CollectionConfig, uid string, m pushMutation, fields map[string]any) (outcome, error) {
	rec, err := app.FindRecordById(m.Collection, m.RecordID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return outcome{}, err
		}
		return missingUpdateOutcome(app, cc, uid, m)
	}
	if ownerOf(rec, cc.OwnerField) != uid {
		// No existence oracle: behave exactly as if it were missing (§6.4, §14.5).
		return missingUpdateOutcome(app, cc, uid, m)
	}

	curRev, err := revFor(app, m.Collection, m.RecordID)
	if err != nil {
		return outcome{}, err
	}
	R := int(curRev)
	B := *m.BaseRev
	if B > R {
		return rejectedOutcome(m.ID, codeInvalidBaseRev), nil
	}

	if B < R {
		S, complete, cerr := changedUnion(app, m.Collection, m.RecordID, B, R)
		if cerr != nil {
			return outcome{}, cerr
		}
		if !complete {
			S = allFieldNames(col) // incomplete history -> all fields (§8.2.3.2)
		}
		if overlap := intersect(fieldKeys(m.Fields), S); len(overlap) > 0 {
			return applyOverlap(app, cc, rec, m, fields, overlap, R)
		}
	}

	rec.Load(fields)
	if err := app.Save(rec); err != nil {
		return saveErrorOutcome(m.ID, err)
	}
	newRev, err := revFor(app, m.Collection, m.RecordID)
	if err != nil {
		return outcome{}, err
	}
	nr := int(newRev)
	merged := B < R
	return outcome{
		res:       okResult(m.ID, nr, rec, &merged, ""),
		effective: statusOK,
	}, nil
}

// applyOverlap resolves a stale-base update whose fields overlap committed
// changes, per the collection's conflict policy (protocol §8.3).
func applyOverlap(app core.App, cc CollectionConfig, rec *core.Record, m pushMutation, fields map[string]any, overlap []string, R int) (outcome, error) {
	switch cc.Conflict {
	case Reject:
		return outcome{
			res:       conflictResult(m.ID, codeFieldConflict, rec, R, ""),
			effective: statusConflict,
		}, nil

	case ClientWins:
		losing := serverValues(rec, overlap)
		rec.Load(fields)
		if err := app.Save(rec); err != nil {
			return saveErrorOutcome(m.ID, err)
		}
		newRev, err := revFor(app, m.Collection, m.RecordID)
		if err != nil {
			return outcome{}, err
		}
		nr := int(newRev)
		cid, err := writeConflict(app, m.Collection, m.RecordID, m.ID, losing, nr)
		if err != nil {
			return outcome{}, err
		}
		merged := false
		return outcome{
			res:       okResult(m.ID, nr, rec, &merged, cid),
			effective: statusOK,
		}, nil

	default: // ServerWins
		losing := losingValues(fields, overlap)
		cid, err := writeConflict(app, m.Collection, m.RecordID, m.ID, losing, R)
		if err != nil {
			return outcome{}, err
		}
		return outcome{
			res:       conflictResult(m.ID, codeFieldConflict, rec, R, cid),
			effective: statusConflict,
		}, nil
	}
}

// missingUpdateOutcome handles an update whose target is absent or owned by
// another user: conflict/deleted when this user's log shows a delete, else
// rejected/not_found (protocol §6.4).
func missingUpdateOutcome(app core.App, cc CollectionConfig, uid string, m pushMutation) (outcome, error) {
	deleted, err := logShowsDelete(app, m.Collection, m.RecordID, uid)
	if err != nil {
		return outcome{}, err
	}
	if deleted {
		return outcome{
			res:       conflictResult(m.ID, codeDeleted, nil, 0, ""),
			effective: statusConflict,
		}, nil
	}
	return rejectedOutcome(m.ID, codeNotFound), nil
}

func applyDelete(app core.App, cc CollectionConfig, uid string, m pushMutation) (outcome, error) {
	rec, err := app.FindRecordById(m.Collection, m.RecordID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return outcome{}, err
		}
		// Deleting a missing record is idempotent: ok with no rev/record (§6.4).
		return outcome{res: okResult(m.ID, 0, nil, nil, ""), effective: statusOK}, nil
	}
	if ownerOf(rec, cc.OwnerField) != uid {
		// Another user's record: behave as if missing (§14.5).
		return outcome{res: okResult(m.ID, 0, nil, nil, ""), effective: statusOK}, nil
	}

	curRev, err := revFor(app, m.Collection, m.RecordID)
	if err != nil {
		return outcome{}, err
	}
	R := int(curRev)
	B := *m.BaseRev
	if B > R {
		return rejectedOutcome(m.ID, codeInvalidBaseRev), nil
	}
	if B < R && cc.DeletePolicy == EditWins {
		return outcome{
			res:       conflictResult(m.ID, codeEditedSince, rec, R, ""),
			effective: statusConflict,
		}, nil
	}

	// deleteWins (default) with a stale base may discard edits; record them for
	// diagnostics (protocol §8.4).
	if B < R {
		S, complete, cerr := changedUnion(app, m.Collection, m.RecordID, B, R)
		if cerr != nil {
			return outcome{}, cerr
		}
		if !complete {
			S = allFieldNames(rec.Collection())
		}
		if len(S) > 0 {
			if _, werr := writeConflict(app, m.Collection, m.RecordID, m.ID, serverValues(rec, S), R+1); werr != nil {
				return outcome{}, werr
			}
		}
	}

	if err := app.Delete(rec); err != nil {
		return saveErrorOutcome(m.ID, err)
	}
	return outcome{res: okResult(m.ID, 0, nil, nil, ""), effective: statusOK}, nil
}

// ---- result builders ----

func okResult(id string, rev int, rec *core.Record, merged *bool, conflictID string) map[string]any {
	m := map[string]any{"id": id, "status": statusOK}
	if rev > 0 {
		m["rev"] = rev
	}
	if rec != nil {
		m["record"] = rec
	}
	if merged != nil {
		m["merged"] = *merged
	}
	if conflictID != "" {
		m["conflictId"] = conflictID
	}
	return m
}

func conflictResult(id, code string, rec *core.Record, rev int, conflictID string) map[string]any {
	m := map[string]any{"id": id, "status": statusConflict, "code": code}
	if rev > 0 {
		m["rev"] = rev
	}
	if rec != nil {
		m["record"] = rec
	}
	if conflictID != "" {
		m["conflictId"] = conflictID
	}
	return m
}

func rejectedOutcome(id, code string) outcome {
	return outcome{
		res:       map[string]any{"id": id, "status": statusRejected, "code": code},
		effective: statusRejected,
	}
}

// duplicateOutcome returns the stored result with status duplicate while
// preserving the original status for dependency tracking (protocol §6.5, §9).
func duplicateOutcome(id, stored string) outcome {
	var m map[string]any
	if err := json.Unmarshal([]byte(stored), &m); err != nil || m == nil {
		return outcome{res: map[string]any{"id": id, "status": statusDuplicate}, effective: statusRejected}
	}
	effective, _ := m["status"].(string)
	if effective == "" {
		effective = statusRejected
	}
	m["id"] = id
	m["status"] = statusDuplicate
	return outcome{res: m, effective: effective}
}

// saveErrorOutcome classifies a save/delete error: validation and unique errors
// are final rejected outcomes, anything else is transient.
func saveErrorOutcome(id string, err error) (outcome, error) {
	switch classifySaveError(err) {
	case codeUniqueViolation:
		return rejectedOutcome(id, codeUniqueViolation), nil
	case codeValidationFailed:
		return rejectedOutcome(id, codeValidationFailed), nil
	default:
		return outcome{}, err
	}
}

func classifySaveError(err error) string {
	var verrs validation.Errors
	if !errors.As(err, &verrs) {
		return ""
	}
	if hasValidationCode(verrs, "validation_not_unique") {
		return codeUniqueViolation
	}
	return codeValidationFailed
}

func hasValidationCode(errs validation.Errors, code string) bool {
	for _, e := range errs {
		switch v := e.(type) {
		case validation.Errors:
			if hasValidationCode(v, code) {
				return true
			}
		case validation.Error:
			if v.Code() == code {
				return true
			}
		}
	}
	return false
}

// ---- persistence helpers ----

func hashMutation(m pushMutation) (string, error) {
	raw, err := json.Marshal(canonicalMutation{
		Collection: m.Collection,
		RecordID:   m.RecordID,
		Op:         m.Op,
		BaseRev:    m.BaseRev,
		Fields:     m.Fields,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func lookupProcessed(app core.App, uid, mutationID string) (hash, result string, found bool, err error) {
	var row struct {
		Hash   string `db:"payload_hash"`
		Result string `db:"result"`
	}
	err = app.DB().NewQuery(
		"SELECT payload_hash, result FROM sync_processed WHERE user = {:u} AND mutation_id = {:m}",
	).Bind(map[string]any{"u": uid, "m": mutationID}).One(&row)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return row.Hash, row.Result, true, nil
}

func storeProcessed(app core.App, uid, mutationID, hash string, res map[string]any) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	_, err = app.DB().NewQuery(`INSERT INTO sync_processed (user, mutation_id, payload_hash, result)
		VALUES ({:u}, {:m}, {:h}, {:r})
		ON CONFLICT (user, mutation_id) DO UPDATE SET payload_hash = {:h}, result = {:r}`).
		Bind(map[string]any{"u": uid, "m": mutationID, "h": hash, "r": string(raw)}).
		Execute()
	return err
}

func writeConflict(app core.App, collection, recordID, mutationID string, losing map[string]any, winningRev int) (string, error) {
	raw, err := json.Marshal(losing)
	if err != nil {
		return "", err
	}
	var row struct {
		ID string `db:"id"`
	}
	err = app.DB().NewQuery(`INSERT INTO sync_conflicts (collection, record_id, mutation_id, losing_fields, winning_rev)
		VALUES ({:c}, {:r}, {:m}, {:lf}, {:wr}) RETURNING id`).
		Bind(map[string]any{
			"c": collection, "r": recordID, "m": mutationID, "lf": string(raw), "wr": winningRev,
		}).
		One(&row)
	if err != nil {
		return "", err
	}
	return row.ID, nil
}

// ---- change-log inspection ----

func hasMeta(app core.App, collection, recordID string) (bool, error) {
	var n int
	err := app.DB().NewQuery(
		"SELECT COUNT(*) FROM sync_meta WHERE collection = {:c} AND record_id = {:r}",
	).Bind(map[string]any{"c": collection, "r": recordID}).Row(&n)
	return n > 0, err
}

func logShowsDelete(app core.App, collection, recordID, owner string) (bool, error) {
	var row struct {
		Op string `db:"op"`
	}
	err := app.DB().NewQuery(`SELECT op FROM sync_changes
		WHERE collection = {:c} AND record_id = {:r} AND owner = {:o}
		ORDER BY seq DESC LIMIT 1`).
		Bind(map[string]any{"c": collection, "r": recordID, "o": owner}).One(&row)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return row.Op == opDelete, nil
}

// changedUnion returns the union of changed-field sets for rev in (baseRev,
// curRev] and whether the history fully covers that range (protocol §8.2.3).
func changedUnion(app core.App, collection, recordID string, baseRev, curRev int) ([]string, bool, error) {
	var rows []struct {
		Rev     int    `db:"rev"`
		Changed string `db:"changed_fields"`
	}
	err := app.DB().NewQuery(`SELECT rev, changed_fields FROM sync_changes
		WHERE collection = {:c} AND record_id = {:r} AND rev > {:b} AND rev <= {:cr}
		ORDER BY rev ASC`).
		Bind(map[string]any{"c": collection, "r": recordID, "b": baseRev, "cr": curRev}).
		All(&rows)
	if err != nil {
		return nil, false, err
	}

	seen := make(map[int]bool, len(rows))
	union := map[string]bool{}
	for _, row := range rows {
		seen[row.Rev] = true
		var names []string
		if json.Unmarshal([]byte(row.Changed), &names) == nil {
			for _, n := range names {
				union[n] = true
			}
		}
	}
	out := make([]string, 0, len(union))
	for n := range union {
		out = append(out, n)
	}
	return out, len(seen) == curRev-baseRev, nil
}

// ---- small helpers ----

func decodeFields(raw map[string]json.RawMessage) (map[string]any, error) {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			return nil, err
		}
		out[k] = val
	}
	return out, nil
}

func fieldKeys(raw map[string]json.RawMessage) []string {
	out := make([]string, 0, len(raw))
	for k := range raw {
		out = append(out, k)
	}
	return out
}

func intersect(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	var out []string
	for _, v := range a {
		if set[v] {
			out = append(out, v)
		}
	}
	return out
}

func losingValues(fields map[string]any, names []string) map[string]any {
	out := make(map[string]any, len(names))
	for _, n := range names {
		out[n] = fields[n]
	}
	return out
}

func serverValues(rec *core.Record, names []string) map[string]any {
	out := make(map[string]any, len(names))
	for _, n := range names {
		out[n] = rec.Get(n)
	}
	return out
}
