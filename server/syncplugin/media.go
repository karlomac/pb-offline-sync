package syncplugin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/router"
)

// Media error codes (protocol §18.5).
const (
	codeUnsupportedMedia = "unsupported_media"
	codeHashMismatch     = "hash_mismatch"
	codeQuotaExceeded    = "quota_exceeded"
	codeUploadNotFound   = "upload_not_found"
	codeMediaNotFound    = "media_not_found"
	codeUploadExpired    = "upload_expired"
	codeUploadTooLarge   = "upload_too_large"
	codeMimeNotAllowed   = "mime_not_allowed"
	codeMediaImmutable   = "media_immutable"
)

// Fixed media collection field names (protocol §18.1).
const (
	mediaOwnerField  = "owner"
	mediaSHAField    = "sha256"
	mediaSizeField   = "size"
	mediaMimeField   = "mime"
	mediaNameField   = "filename"
	mediaFileField   = "file"
	mediaPosterField = "poster"
)

// posterMaxBytes caps the optional client-generated JPEG poster (§18.4).
const posterMaxBytes = 8 << 20

// mediaMaxOpenSessions bounds live upload sessions per user so a client cannot
// fill staging with abandoned partials (§14.7, §18.6). Over the limit, init
// answers 429 rate_limited with Retry-After.
const mediaMaxOpenSessions = 16

// mediaMimeDeny is always denied regardless of the allowlist (§18.6).
var mediaMimeDeny = map[string]bool{"text/html": true, "image/svg+xml": true}

// defaultMediaMimePrefixes apply when MediaConfig.MimeAllowlist is empty.
var defaultMediaMimePrefixes = []string{"image/", "video/", "audio/"}

var (
	shaRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uploadRe  = regexp.MustCompile(`^u_[0-9a-f]{32}$`)
	identRe   = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	tusCTType = "application/offset+octet-stream"
)

// mediaViewRule scopes media records (and therefore their files) to the owner.
// PocketBase serves a file only when the field is Protected and the record
// passes the collection's ViewRule (§18.6), so this rule is required for owners
// to download their own media and for everyone else to be refused.
const mediaViewRule = mediaOwnerField + " = @request.auth.id"

// EnsureMediaCollection provisions or converges the media collection with the
// fixed field set of protocol §18.1. It is idempotent and safe to call on every
// boot: an existing collection gets the security-relevant parts converged
// (file fields marked Protected, owner-scoped list/view rules when unset) while
// operator-set custom rules are left alone (validateMediaCollection then
// rejects anything unsafe).
//
// A non-positive fileMax/posterMax means "no explicit cap" (1 TiB) for the file
// field; posterMax should otherwise be MediaConfig.PosterMaxBytes.
func EnsureMediaCollection(app core.App, name, authCollectionOrID string, maxFileSize, posterMax int64) (*core.Collection, error) {
	col, err := app.FindCollectionByNameOrId(name)
	if err == nil {
		return convergeMediaCollection(app, col)
	}

	col = core.NewBaseCollection(name)
	// created/updated are not automatic in PB; MediaGC orders on `updated`.
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true, OnUpdate: false})
	col.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	col.Fields.Add(&core.TextField{Name: mediaSHAField})
	col.Fields.Add(&core.NumberField{Name: mediaSizeField, OnlyInt: true})
	col.Fields.Add(&core.TextField{Name: mediaMimeField})
	col.Fields.Add(&core.TextField{Name: mediaNameField})
	fileMax := maxFileSize
	if fileMax <= 0 {
		fileMax = 1 << 40
	}
	col.Fields.Add(&core.FileField{Name: mediaFileField, MaxSelect: 1, MaxSize: fileMax, Protected: true})
	if posterMax <= 0 {
		posterMax = posterMaxBytes
	}
	col.Fields.Add(&core.FileField{Name: mediaPosterField, MaxSelect: 1, MaxSize: posterMax, Protected: true})
	col.Fields.Add(&core.RelationField{Name: mediaOwnerField, CollectionId: authCollectionOrID, MaxSelect: 1})
	// Write rules stay locked (nil = superuser only); records are server-created
	// (§18.1) and push mutations are rejected with media_immutable.
	viewRule := mediaViewRule
	col.ListRule = &viewRule
	col.ViewRule = &viewRule
	if err := app.Save(col); err != nil {
		return nil, err
	}
	return app.FindCollectionByNameOrId(name)
}

// convergeMediaCollection upgrades a pre-existing media collection to the
// §18.1 security requirements without touching custom rules.
func convergeMediaCollection(app core.App, col *core.Collection) (*core.Collection, error) {
	changed := false
	for _, fieldName := range []string{mediaFileField, mediaPosterField} {
		if f, ok := col.Fields.GetByName(fieldName).(*core.FileField); ok && !f.Protected {
			f.Protected = true
			changed = true
		}
	}
	if col.ListRule == nil {
		viewRule := mediaViewRule
		col.ListRule = &viewRule
		changed = true
	}
	if col.ViewRule == nil {
		viewRule := mediaViewRule
		col.ViewRule = &viewRule
		changed = true
	}
	if !changed {
		return col, nil
	}
	if err := app.Save(col); err != nil {
		return nil, err
	}
	return app.FindCollectionByNameOrId(col.Name)
}

func (mc *MediaConfig) mimeAllowed(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	if m == "" || mediaMimeDeny[m] {
		return false
	}
	list := mc.MimeAllowlist
	if len(list) == 0 {
		list = defaultMediaMimePrefixes
	}
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if strings.HasSuffix(e, "/") {
			if strings.HasPrefix(m, e) {
				return true
			}
		} else if m == e {
			return true
		}
	}
	return false
}

// mediaSession is the staging sidecar (media_staging/<uploadId>/meta.json).
type mediaSession struct {
	Owner    string `json:"owner"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Mime     string `json:"mime"`
	Filename string `json:"filename"`
	Offset   int64  `json:"offset"`
	Expires  string `json:"expires"`
}

func (s *mediaSession) expired(now time.Time) bool {
	t, err := time.Parse(time.RFC3339, s.Expires)
	return err == nil && now.After(t)
}

func (mc *MediaConfig) stagingRoot(app core.App) string {
	return filepath.Join(app.DataDir(), "media_staging")
}

func newUploadID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return "u_" + hex.EncodeToString(b)
}

func (mc *MediaConfig) sessionDir(app core.App, id string) string {
	return filepath.Join(mc.stagingRoot(app), id)
}

// loadSession reads the sidecar. found is false when the directory or meta is
// missing (mapped to upload_not_found).
// loadSession reads the sidecar. found is false when the directory or meta is
// missing or malformed (missing maps to upload_not_found, malformed to a
// sweepable dir in SweepMediaStaging). The error is always nil; kept for
// call-site future-proofing.
func loadSession(dir string) (mediaSession, bool, error) {
	var s mediaSession
	raw, rerr := os.ReadFile(filepath.Join(dir, "meta.json"))
	if rerr != nil {
		return s, false, nil
	}
	if json.Unmarshal(raw, &s) != nil {
		return s, false, nil
	}
	return s, true, nil
}

func (mc *MediaConfig) saveSession(dir string, s mediaSession) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Temp + rename: concurrent readers must never see a half-written sidecar.
	tmp := filepath.Join(dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "meta.json"))
}

// findMediaByHash is the per-user dedup lookup (protocol §18.1).
func findMediaByHash(app core.App, collection, sha, uid string) (*core.Record, error) {
	recs, err := app.FindRecordsByFilter(collection,
		mediaSHAField+" = {:sha} && "+mediaOwnerField+" = {:owner}", "", 1, 0,
		dbx.Params{"sha": sha, "owner": uid})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return recs[0], nil
}

// mediaBytesUsed sums the user's committed media sizes plus the declared sizes
// of active staging sessions (committed-only accounting lets parallel sessions
// oversubscribe the quota). Consumed sessions (done.json) add nothing.
func mediaBytesUsed(app core.App, mc *MediaConfig, collection, uid string) (int64, error) {
	if !identRe.MatchString(collection) {
		return 0, errors.New("syncplugin: unsafe collection name for media accounting")
	}
	var row struct {
		Used int64 `db:"used"`
	}
	err := app.DB().NewQuery("SELECT COALESCE(SUM(\"" + mediaSizeField + "\"), 0) AS used FROM \"" +
		collection + "\" WHERE \"" + mediaOwnerField + "\" = {:o}").
		Bind(dbx.Params{"o": uid}).One(&row)
	if err != nil {
		return 0, err
	}
	staged, err := mc.stagedBytes(app, uid)
	if err != nil {
		return 0, err
	}
	return row.Used + staged, nil
}

// stagedBytes walks the staging root and sums the declared sizes of the user's
// active sessions. shortcut: per-dir meta read (O(sessions)) — fine at
// personal-app scale; a heavier deployment should index staging elsewhere.
func (mc *MediaConfig) stagedBytes(app core.App, uid string) (int64, error) {
	entries, err := os.ReadDir(mc.stagingRoot(app))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		s, found, _ := loadSession(filepath.Join(mc.stagingRoot(app), entry.Name()))
		// Any live session of this user reserves its declared size, including
		// partially uploaded ones: otherwise parallel sessions oversubscribe the
		// quota before the commit-time check sees them (§18.2).
		if !found || s.Owner != uid || s.expired(time.Now().UTC()) {
			continue
		}
		dir := filepath.Join(mc.stagingRoot(app), entry.Name())
		if _, err := os.Stat(filepath.Join(dir, "done.json")); err == nil {
			continue // consumed: bytes are gone and the record counts instead
		}
		if s.Size > 0 {
			total += s.Size
		}
	}
	return total, nil
}

// openSessions counts the user's live, unconsumed upload sessions.
func (mc *MediaConfig) openSessions(app core.App, uid string) (int, error) {
	entries, err := os.ReadDir(mc.stagingRoot(app))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	now := time.Now().UTC()
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(mc.stagingRoot(app), entry.Name())
		s, found, _ := loadSession(dir)
		if !found || s.Owner != uid || s.expired(now) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "done.json")); err == nil {
			continue
		}
		count++
	}
	return count, nil
}

// bindMediaRoutes registers the media endpoints (protocol §18). Only called
// when cfg.Media is configured, so an unconfigured server 404s them naturally.
func bindMediaRoutes(app core.App, r *router.Router[*core.RequestEvent], cfg Config) {
	r.POST("/api/sync/media/init", func(e *core.RequestEvent) error {
		return handleMediaInit(e, cfg)
	}).Bind(apis.RequireAuth())

	r.POST("/api/sync/media/commit", func(e *core.RequestEvent) error {
		return handleMediaCommit(e, cfg)
	}).Bind(apis.RequireAuth())

	upH := func(e *core.RequestEvent) error { return handleMediaUpload(e, cfg) }
	r.HEAD("/api/sync/media/upload/{uploadId}", upH).Bind(apis.RequireAuth())
	r.PATCH("/api/sync/media/upload/{uploadId}", upH).Bind(apis.RequireAuth())
	r.DELETE("/api/sync/media/upload/{uploadId}", upH).Bind(apis.RequireAuth())
}

// handleMediaInit serves POST /api/sync/media/init (protocol §18.2).
func handleMediaInit(e *core.RequestEvent, cfg Config) error {
	if !protocolOK(e) {
		return nil
	}
	mc := cfg.Media
	uid := e.Auth.Id

	var req struct {
		Collection string `json:"collection"`
		SHA256     string `json:"sha256"`
		Size       int64  `json:"size"`
		Mime       string `json:"mime"`
		Filename   string `json:"filename"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(e.Response, e.Request.Body, cfg.MaxBodyBytes))
	if err != nil || json.Unmarshal(body, &req) != nil {
		return writeError(e, http.StatusBadRequest, "invalid_request", "Malformed JSON body.", nil)
	}

	if req.Collection != mc.Collection {
		return writeError(e, http.StatusBadRequest, codeUnsupportedMedia,
			"Unknown media collection.", nil)
	}
	if !shaRe.MatchString(req.SHA256) {
		return writeError(e, http.StatusBadRequest, "invalid_request",
			"sha256 must be 64 lowercase hex characters.", nil)
	}
	if req.Size <= 0 || (mc.MaxFileSize > 0 && req.Size > mc.MaxFileSize) {
		return writeError(e, http.StatusRequestEntityTooLarge, codeUploadTooLarge,
			"Declared size exceeds the maximum file size.", nil)
	}
	if !mc.mimeAllowed(req.Mime) {
		return writeError(e, http.StatusUnsupportedMediaType, codeMimeNotAllowed,
			"MIME type not allowed.", nil)
	}

	// Dedup hit: no bytes transferred (§18.2) — takes precedence over the quota
	// check, because committed media adds nothing new.
	existing, err := findMediaByHash(e.App, mc.Collection, req.SHA256, uid)
	if err != nil {
		return err
	}
	if existing != nil {
		rev, err := revFor(e.App, mc.Collection, existing.Id)
		if err != nil {
			return err
		}
		return e.JSON(http.StatusOK, map[string]any{
			"status": "exists", "mediaId": existing.Id, "rev": int(rev),
		})
	}

	// Quota and open-session limits are enforced before any bytes move (§18.2,
	// §14.7). Declared sizes for active sessions include partial uploads, and
	// absurd sizes are rejected so the byte sums cannot overflow.
	if req.Size > math.MaxInt64/4 {
		return writeError(e, http.StatusRequestEntityTooLarge, codeUploadTooLarge,
			"Declared size is not supported.", nil)
	}
	if open, err := mc.openSessions(e.App, uid); err != nil {
		return err
	} else if open >= mediaMaxOpenSessions {
		e.Response.Header().Set("Retry-After", "60")
		return writeError(e, http.StatusTooManyRequests, "rate_limited",
			"Too many open upload sessions; retry later.", nil)
	}
	if mc.PerUserQuota > 0 {
		used, err := mediaBytesUsed(e.App, mc, mc.Collection, uid)
		if err != nil {
			return err
		}
		if used+req.Size > mc.PerUserQuota {
			return writeError(e, http.StatusForbidden, codeQuotaExceeded,
				"Per-user storage quota exceeded.", nil)
		}
	}

	now := time.Now().UTC()
	id := newUploadID()
	dir := mc.sessionDir(e.App, id)
	if err := os.RemoveAll(dir); err != nil { // defensive: id collision
		return err
	}
	s := mediaSession{
		Owner: uid, SHA256: req.SHA256, Size: req.Size,
		Mime: req.Mime, Filename: req.Filename,
		Expires: now.Add(mc.StagingTTL).Format(time.RFC3339),
	}
	if err := mc.saveSession(dir, s); err != nil {
		return err
	}

	return e.JSON(http.StatusOK, map[string]any{
		"status":    "upload",
		"uploadId":  id,
		"chunkSize": mc.ChunkSize,
		"expiresIn": int(mc.StagingTTL.Seconds()),
	})
}

// handleMediaUpload serves the tus-core upload channel (protocol §18.3).
func handleMediaUpload(e *core.RequestEvent, cfg Config) error {
	if !protocolOK(e) {
		return nil
	}
	mc := cfg.Media
	id := e.Request.PathValue("uploadId")
	if !uploadRe.MatchString(id) {
		return writeError(e, http.StatusNotFound, codeUploadNotFound, "Unknown upload.", nil)
	}
	dir := mc.sessionDir(e.App, id)
	s, found, err := loadSession(dir)
	if err != nil || !found {
		return writeError(e, http.StatusNotFound, codeUploadNotFound, "Unknown upload.", nil)
	}
	// Sessions are bound to their owner; a foreign id is indistinguishable from
	// a missing one (protocol §18.3, §14.5).
	if s.Owner != e.Auth.Id {
		return writeError(e, http.StatusNotFound, codeUploadNotFound, "Unknown upload.", nil)
	}
	// A consumed session (stored result kept, bytes gone) is upload_not_found on
	// the upload channel too, so DELETE cannot destroy its stored result (§18.5)
	// — checked before expiry so an expired consumed session is never deleted
	// from this path (L-2). A fresh session (no bytes yet) is still live.
	dataPath := filepath.Join(dir, "data.bin")
	_, dataStatErr := os.Stat(dataPath)
	if dataStatErr != nil {
		if _, doneErr := os.Stat(filepath.Join(dir, "done.json")); doneErr == nil {
			return writeError(e, http.StatusNotFound, codeUploadNotFound, "Unknown upload.", nil)
		}
	}
	if s.expired(time.Now().UTC()) {
		_ = os.RemoveAll(dir)
		return writeError(e, http.StatusGone, codeUploadExpired, "Upload session expired.", nil)
	}

	switch e.Request.Method {
	case http.MethodHead:
		e.Response.Header().Set("Upload-Offset", strconv.FormatInt(s.Offset, 10))
		e.Response.Header().Set("Upload-Length", strconv.FormatInt(s.Size, 10))
		e.Response.Header().Set("Cache-Control", "no-store")
		return e.NoContent(http.StatusOK)

	case http.MethodDelete:
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return e.NoContent(http.StatusNoContent)

	case http.MethodPatch:
		reqOff, perr := strconv.ParseInt(e.Request.Header.Get("Upload-Offset"), 10, 64)
		if perr != nil || reqOff != s.Offset {
			return writeError(e, http.StatusBadRequest, "invalid_request",
				"Upload-Offset does not match the current offset.", nil)
		}
		if ct := e.Request.Header.Get("Content-Type"); !strings.HasPrefix(ct, tusCTType) {
			return writeError(e, http.StatusBadRequest, "invalid_request",
				"Content-Type must be application/offset+octet-stream.", nil)
		}

		if s.Offset == 0 {
			// A retried first chunk replaces any earlier partial bytes.
			if err := os.WriteFile(filepath.Join(dir, "data.bin"), nil, 0o600); err != nil {
				return err
			}
		}
		f, err := os.OpenFile(filepath.Join(dir, "data.bin"), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()

		// Stream to staging, never buffering the body (§18.3). One extra byte
		// is allowed through so an oversized body is detected, then truncated.
		remaining := s.Size - s.Offset
		n, err := io.Copy(f, io.LimitReader(e.Request.Body, remaining+1))
		if err != nil {
			// A dropped connection must not leave orphaned bytes behind the
			// acknowledged offset, or the next resume appends after them and the
			// commit fails hash_mismatch (L-1).
			_ = f.Truncate(s.Offset)
			return err
		}
		if n > remaining {
			_ = f.Truncate(s.Offset)
			return writeError(e, http.StatusRequestEntityTooLarge, codeUploadTooLarge,
				"Chunk exceeds the declared size.", nil)
		}

		s.Offset += n
		if err := mc.saveSession(dir, s); err != nil {
			return err
		}
		e.Response.Header().Set("Upload-Offset", strconv.FormatInt(s.Offset, 10))
		return e.NoContent(http.StatusNoContent)

	default:
		return writeError(e, http.StatusMethodNotAllowed, "invalid_request", "Method not allowed.", nil)
	}
}

// handleMediaCommit serves POST /api/sync/media/commit (protocol §18.4).
func handleMediaCommit(e *core.RequestEvent, cfg Config) error {
	if !protocolOK(e) {
		return nil
	}
	mc := cfg.Media
	uid := e.Auth.Id

	var req struct {
		UploadID       string `json:"uploadId"`
		PosterUploadID string `json:"posterUploadId"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(e.Response, e.Request.Body, cfg.MaxBodyBytes))
	if err != nil || json.Unmarshal(body, &req) != nil {
		return writeError(e, http.StatusBadRequest, "invalid_request", "Malformed JSON body.", nil)
	}
	if !uploadRe.MatchString(req.UploadID) {
		return writeError(e, http.StatusNotFound, codeUploadNotFound, "Unknown upload.", nil)
	}

	dir := mc.sessionDir(e.App, req.UploadID)
	s, found, err := loadSession(dir)
	if err != nil || !found || s.Owner != uid {
		return writeError(e, http.StatusNotFound, codeUploadNotFound, "Unknown upload.", nil)
	}
	dataPath := filepath.Join(dir, "data.bin")
	if _, statErr := os.Stat(dataPath); statErr != nil {
		// Already committed: return the stored original result before the expiry
		// branch can delete it (§18.4 idempotency; L-2).
		if doneRaw, derr := os.ReadFile(filepath.Join(dir, "done.json")); derr == nil {
			var done map[string]any
			if json.Unmarshal(doneRaw, &done) == nil && done != nil {
				return e.JSON(http.StatusOK, done)
			}
		}
		return writeError(e, http.StatusNotFound, codeUploadNotFound, "Unknown upload.", nil)
	}
	if s.expired(time.Now().UTC()) {
		_ = os.RemoveAll(dir)
		return writeError(e, http.StatusGone, codeUploadExpired, "Upload session expired.", nil)
	}
	if s.Offset != s.Size {
		return writeError(e, http.StatusBadRequest, "invalid_request",
			"Upload is incomplete.", nil)
	}

	var posterSession *mediaSession
	var posterDir string
	if req.PosterUploadID != "" {
		if req.PosterUploadID == req.UploadID || !uploadRe.MatchString(req.PosterUploadID) {
			return writeError(e, http.StatusBadRequest, "invalid_request",
				"Invalid posterUploadId.", nil)
		}
		posterDir = mc.sessionDir(e.App, req.PosterUploadID)
		p, pfound, perr := loadSession(posterDir)
		if perr != nil || !pfound {
			return writeError(e, http.StatusNotFound, codeUploadNotFound, "Unknown upload.", nil)
		}
		if p.Owner != uid {
			return writeError(e, http.StatusNotFound, codeUploadNotFound, "Unknown upload.", nil)
		}
		if p.expired(time.Now().UTC()) {
			return writeError(e, http.StatusGone, codeUploadExpired, "Upload session expired.", nil)
		}
		if _, perr2 := os.Stat(filepath.Join(posterDir, "data.bin")); perr2 != nil || p.Offset != p.Size {
			return writeError(e, http.StatusBadRequest, "invalid_request",
				"Poster upload is incomplete.", nil)
		}
		if p.Mime != "image/jpeg" || p.Size > mc.PosterMaxBytes {
			return writeError(e, http.StatusBadRequest, "invalid_request",
				"Poster must be a JPEG up to the advertised cap.", nil)
		}
		posterSession = &p
	}

	// The client's hash is a claim; recompute over the staged bytes (§18.4).
	sum, err := shaFile(dataPath)
	if err != nil {
		return err
	}
	if sum != s.SHA256 {
		_ = os.RemoveAll(dir)
		return writeError(e, http.StatusBadRequest, codeHashMismatch,
			"Bytes do not match the declared sha256.", nil)
	}

	res, err := mc.commitRecord(e.App, dir, dataPath, s, posterDir, posterSession, uid)
	if err != nil {
		if errors.Is(err, errQuotaExceeded) {
			return writeError(e, http.StatusForbidden, codeQuotaExceeded,
				"Per-user storage quota exceeded.", nil)
		}
		return err
	}

	// Keep meta + the stored result for idempotent re-commits; drop the bytes.
	doneRaw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "done.json"), doneRaw, 0o600); err != nil {
		// Without the stored result a retry could not be answered idempotently —
		// fail while the session is still intact and retryable (§18.4).
		return err
	}
	_ = os.Remove(dataPath)
	if posterSession != nil {
		_ = os.Remove(filepath.Join(posterDir, "data.bin"))
	}

	return e.JSON(http.StatusOK, res)
}

// errQuotaExceeded surfaces a final commit-time quota rejection (§18.5).
var errQuotaExceeded = errors.New("syncplugin: per-user media quota exceeded")

func shaFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// commitRecord creates (or dedups) the media record inside one transaction so
// the change log is written atomically with the record (§8.5).
func (mc *MediaConfig) commitRecord(app core.App, dir, dataPath string, s mediaSession, posterDir string, poster *mediaSession, uid string) (map[string]any, error) {
	var res map[string]any
	err := app.RunInTransaction(func(tx core.App) error {
		// Re-check dedup inside the transaction so concurrent commits of the
		// same content converge on one record (§18.4).
		existing, err := findMediaByHash(tx, mc.Collection, s.SHA256, uid)
		if err != nil {
			return err
		}
		if existing != nil {
			rev, rerr := revFor(tx, mc.Collection, existing.Id)
			if rerr != nil {
				return rerr
			}
			res = commitResult(existing.Id, int(rev), s.Filename, s.Size)
			return nil
		}

		// Belt-and-braces quota re-check inside the transaction: parallel init
		// sessions could race past the init-time check (§18.5 quota_exceeded).
		// used already contains this session's staged bytes; a dedup hit above
		// never reaches this branch.
		if mc.PerUserQuota > 0 {
			used, qerr := mediaBytesUsed(tx, mc, mc.Collection, uid)
			if qerr != nil {
				return qerr
			}
			if used > mc.PerUserQuota {
				return errQuotaExceeded
			}
		}

		col, err := tx.FindCollectionByNameOrId(mc.Collection)
		if err != nil {
			return err
		}
		rec := core.NewRecord(col)
		rec.Set(mediaOwnerField, uid)
		rec.Set(mediaSHAField, s.SHA256)
		rec.Set(mediaSizeField, s.Size)
		rec.Set(mediaMimeField, s.Mime)
		rec.Set(mediaNameField, s.Filename)
		file, err := filesystem.NewFileFromPath(dataPath)
		if err != nil {
			return err
		}
		rec.Set(mediaFileField, file)
		if poster != nil {
			pf, err := filesystem.NewFileFromPath(filepath.Join(posterDir, "data.bin"))
			if err != nil {
				return err
			}
			rec.Set(mediaPosterField, pf)
		}
		if err := tx.Save(rec); err != nil {
			return err
		}
		rev, err := revFor(tx, mc.Collection, rec.Id)
		if err != nil {
			return err
		}
		res = commitResult(rec.Id, int(rev), s.Filename, s.Size)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func commitResult(mediaID string, rev int, filename string, size int64) map[string]any {
	return map[string]any{
		"status":  "ok",
		"mediaId": mediaID,
		"rev":     rev,
		"file":    map[string]any{"name": filename, "size": size},
	}
}
