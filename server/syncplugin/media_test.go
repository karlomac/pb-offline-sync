package syncplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// mediaTestConfig is a plugin config with media enabled and small limits so
// quota/oversize paths are cheap to exercise.
func mediaTestConfig(overrides func(*MediaConfig)) Config {
	mc := &MediaConfig{MaxFileSize: 1 << 20}
	if overrides != nil {
		overrides(mc)
	}
	return Config{
		Collections: map[string]CollectionConfig{
			"notes": {OwnerField: "owner", Conflict: ServerWins, DeletePolicy: DeleteWins},
		},
		Media: mc,
	}
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// uploadAndCommit drives init → PATCH chunk(s) → commit and returns the commit
// status/body plus the uploadId (empty on a dedup hit).
func uploadAndCommit(t *testing.T, env *testEnv, tok string, mc *MediaConfig, sha, mime, filename string, data []byte) (int, map[string]any, string) {
	t.Helper()

	status, _, initBody := env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":%d,"mime":%q,"filename":%q}`,
			sha, len(data), mime, filename)))
	if status != http.StatusOK {
		return status, initBody, ""
	}
	if s, _ := initBody["status"].(string); s == "exists" {
		return status, initBody, ""
	}
	uploadID, _ := initBody["uploadId"].(string)
	if uploadID == "" {
		t.Fatalf("init response missing uploadId: %#v", initBody)
	}

	offset := int64(0)
	for offset < int64(len(data)) {
		end := offset + int64(mc.ChunkSize)
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		headers := map[string]string{
			"Upload-Offset": strconv.FormatInt(offset, 10),
			"Content-Type":  tusCTType,
		}
		st, _, body := env.doJSON(http.MethodPatch, "/api/sync/media/upload/"+uploadID, tok, headers, data[offset:end])
		if st != http.StatusNoContent {
			return st, body, uploadID
		}
		offset = end
	}

	status, _, body := env.doJSON(http.MethodPost, "/api/sync/media/commit", tok, nil,
		[]byte(fmt.Sprintf(`{"uploadId":%q}`, uploadID)))
	return status, body, uploadID
}

func TestMediaUploadEndToEnd(t *testing.T) {
	cfg := mediaTestConfig(func(mc *MediaConfig) { mc.ChunkSize = 4 })
	env := newTestEnv(t, cfg)
	u := env.newUser("alice@example.com")
	tok := env.token(u)

	data := []byte("payload bytes for a photo used in tests")
	status, body, uploadID := uploadAndCommit(t, env, tok, cfg.Media, hashBytes(data), "image/jpeg", "p.jpg", data)
	if status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("commit failed: %d %s", status, body)
	}
	mediaID, _ := body["mediaId"].(string)
	if mediaID == "" {
		t.Fatalf("commit missing mediaId: %#v", body)
	}

	// The record exists with server-set owner and the declared meta fields.
	rec, err := env.app.FindRecordById("media", mediaID)
	if err != nil {
		t.Fatalf("media record missing: %v", err)
	}
	if ownerOf(rec, mediaOwnerField) != u.Id {
		t.Fatalf("owner %q, want %q", ownerOf(rec, mediaOwnerField), u.Id)
	}
	if got := rec.GetString(mediaSHAField); got != hashBytes(data) {
		t.Fatalf("sha256 %q", got)
	}
	if got := rec.GetInt(mediaSizeField); got != len(data) {
		t.Fatalf("size %d, want %d", got, len(data))
	}
	if rec.GetString(mediaFileField) == "" {
		t.Fatal("file field empty")
	}

	// The write went through the normal save path: rev 1 and a change-log row
	// (protocol §18.4, §8.5).
	if r := metaRev(t, env.app, "media", mediaID); r != 1 {
		t.Fatalf("rev %d, want 1", r)
	}
	if cr := lastChange(t, env.app, "media", mediaID); cr.Op != opCreate || cr.Rev != 1 {
		t.Fatalf("change row %#v", cr)
	}

	// Pull reflects alice's own media record (§7.2, §14.3).
	st, pullBody := env.getJSON("/api/sync/pull", tok, nil)
	if st != http.StatusOK {
		t.Fatalf("pull: %d %s", st, pullBody)
	}
	chs := changes(t, pullBody)
	if len(chs) != 1 || chs[0]["collection"] != "media" || chs[0]["recordId"] != mediaID {
		t.Fatalf("pull changes: %#v", chs)
	}

	// Dedup: a re-init is a hit without bytes (§18.2)...
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":%d,"mime":"image/jpeg","filename":"p.jpg"}`, hashBytes(data), len(data))))
	if st != http.StatusOK || body["status"] != "exists" || body["mediaId"] != mediaID {
		t.Fatalf("dedup init: %d %#v", st, body)
	}
	// ...and re-committing the consumed upload returns the stored result (§18.4).
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/commit", tok, nil,
		[]byte(fmt.Sprintf(`{"uploadId":%q}`, uploadID)))
	if st != http.StatusOK || body["status"] != "ok" || body["mediaId"] != mediaID {
		t.Fatalf("re-commit not idempotent: %d %#v", st, body)
	}
}

func TestMediaCommitHashMismatchDiscardsStaging(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	u := env.newUser("alice@example.com")
	tok := env.token(u)

	data := []byte("actual bytes")
	declared := hashBytes([]byte("other bytes"))

	st, _, initBody := env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":%d,"mime":"image/png","filename":"p.png"}`, declared, len(data))))
	if st != http.StatusOK {
		t.Fatalf("init: %d %s", st, initBody)
	}
	uploadID, _ := initBody["uploadId"].(string)
	st, _, body := env.doJSON(http.MethodPatch, "/api/sync/media/upload/"+uploadID, tok,
		map[string]string{"Upload-Offset": "0", "Content-Type": tusCTType}, data)
	if st != http.StatusNoContent {
		t.Fatalf("patch: %d %s", st, body)
	}

	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/commit", tok, nil,
		[]byte(fmt.Sprintf(`{"uploadId":%q}`, uploadID)))
	if st != http.StatusBadRequest || body["code"] != codeHashMismatch {
		t.Fatalf("commit: %d %#v", st, body)
	}

	// Staged bytes are discarded, so a retry cannot resurrect them (§18.4).
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/commit", tok, nil,
		[]byte(fmt.Sprintf(`{"uploadId":%q}`, uploadID)))
	if st != http.StatusNotFound || body["code"] != codeUploadNotFound {
		t.Fatalf("retry after mismatch: %d %#v", st, body)
	}
	recs, err := env.app.FindRecordsByFilter("media", "id != ''", "", 10, 0)
	if err != nil || len(recs) != 0 {
		t.Fatalf("media records exist after mismatch (%d, err %v)", len(recs), err)
	}
}

func TestMediaUploadOffsetAndOversize(t *testing.T) {
	cfg := mediaTestConfig(func(mc *MediaConfig) { mc.ChunkSize = 4 })
	env := newTestEnv(t, cfg)
	u := env.newUser("alice@example.com")
	tok := env.token(u)

	data := []byte("0123456789")
	st, _, initBody := env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":%d,"mime":"image/png","filename":"n.png"}`, hashBytes(data), len(data))))
	if st != http.StatusOK {
		t.Fatalf("init: %d %s", st, initBody)
	}
	if st, _, body := env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil, []byte(`{"collection":"media","sha256":"zz","size":1,"mime":"image/png","filename":"x"}`)); st != http.StatusBadRequest {
		t.Fatalf("bad sha init: %d %s", st, body)
	}

	uploadID, _ := initBody["uploadId"].(string)

	// A wrong offset is rejected (§18.3).
	st, _, body := env.doJSON(http.MethodPatch, "/api/sync/media/upload/"+uploadID, tok,
		map[string]string{"Upload-Offset": "4", "Content-Type": tusCTType}, []byte("abcd"))
	if st != http.StatusBadRequest {
		t.Fatalf("offset mismatch: %d %s", st, body)
	}

	// A patch pushing past the declared size is a hard reject, truncated back.
	st, _, body = env.doJSON(http.MethodPatch, "/api/sync/media/upload/"+uploadID, tok,
		map[string]string{"Upload-Offset": "0", "Content-Type": tusCTType}, data)
	if st != http.StatusNoContent {
		t.Fatalf("patch full: %d %s", st, body)
	}
	st, _, body = env.doJSON(http.MethodPatch, "/api/sync/media/upload/"+uploadID, tok,
		map[string]string{"Upload-Offset": "10", "Content-Type": tusCTType}, []byte("extra!"))
	if st != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize patch: %d %s", st, body)
	}

	// Abort clears the session (§18.3).
	st, _, body = env.doJSON(http.MethodDelete, "/api/sync/media/upload/"+uploadID, tok, nil, nil)
	if st != http.StatusNoContent {
		t.Fatalf("abort: %d %s", st, body)
	}
	st, _, body = env.doJSON(http.MethodHead, "/api/sync/media/upload/"+uploadID, tok, nil, nil)
	if st != http.StatusNotFound || body["code"] != codeUploadNotFound {
		t.Fatalf("head after abort: %d %#v", st, body)
	}
}

func TestMediaForeignAndExpiredSessions(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")
	aliceTok := env.token(alice)
	bobTok := env.token(bob)

	data := []byte("private bytes")
	st, _, initBody := env.doJSON(http.MethodPost, "/api/sync/media/init", aliceTok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":%d,"mime":"image/png","filename":"p.png"}`, hashBytes(data), len(data))))
	if st != http.StatusOK {
		t.Fatalf("init: %d %s", st, initBody)
	}
	uploadID, _ := initBody["uploadId"].(string)

	// Bob sees alice's session as missing (no existence oracle, §14.5).
	for _, method := range []string{http.MethodHead, http.MethodPatch, http.MethodDelete} {
		st, _, body := env.doJSON(method, "/api/sync/media/upload/"+uploadID, bobTok,
			map[string]string{"Upload-Offset": "0", "Content-Type": tusCTType}, []byte("x"))
		if st != http.StatusNotFound || body["code"] != codeUploadNotFound {
			t.Fatalf("%s foreign session: %d %#v", method, st, body)
		}
	}
	if st, _, body := env.doJSON(http.MethodPost, "/api/sync/media/commit", bobTok, nil,
		[]byte(fmt.Sprintf(`{"uploadId":%q}`, uploadID))); st != http.StatusNotFound || body["code"] != codeUploadNotFound {
		t.Fatalf("foreign commit: %d %#v", st, body)
	}

	// An expired session is 410 upload_expired (§18.2).
	dir := cfg.Media.sessionDir(env.app, uploadID)
	s, found, err := loadSession(dir)
	if err != nil || !found {
		t.Fatalf("load session: %v %v", found, err)
	}
	s.Expires = time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	if err := saveTestSession(dir, s); err != nil {
		t.Fatal(err)
	}
	st, _, body := env.doJSON(http.MethodHead, "/api/sync/media/upload/"+uploadID, aliceTok, nil, nil)
	if st != http.StatusGone || body["code"] != codeUploadExpired {
		t.Fatalf("expired head: %d %#v", st, body)
	}
}

func TestMediaQuotaAndOversizeInit(t *testing.T) {
	cfg := mediaTestConfig(func(mc *MediaConfig) {
		mc.ChunkSize = 4
		mc.PerUserQuota = 16
		mc.MaxFileSize = 16
	})
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	bob := env.newUser("bob@example.com")
	aliceTok := env.token(alice)
	bobTok := env.token(bob)

	// Over the configured max file size: rejected before bytes move (§18.2).
	st, _, body := env.doJSON(http.MethodPost, "/api/sync/media/init", aliceTok, nil,
		[]byte(`{"collection":"media","sha256":"`+strings.Repeat("a", 64)+`","size":17,"mime":"image/png","filename":"big.png"}`))
	if st != http.StatusRequestEntityTooLarge || body["code"] != codeUploadTooLarge {
		t.Fatalf("oversize init: %d %#v", st, body)
	}

	// Commit two 8-byte files: exactly fills the 16-byte per-user quota.
	for i := range 2 {
		data := []byte(fmt.Sprintf("8byte00%d", i))
		st, body, _ := uploadAndCommit(t, env, aliceTok, cfg.Media, hashBytes(data), "image/png", "f.png", data)
		if st != http.StatusOK {
			t.Fatalf("upload %d: %d %s", i, st, body)
		}
	}
	if used, err := mediaBytesUsed(env.app, cfg.Media, "media", alice.Id); err != nil || used != 16 {
		t.Fatalf("bytes used %d err %v", used, err)
	}

	// A third 8 byte file would exceed the quota: rejected at init (§18.2).
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/init", aliceTok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":8,"mime":"image/png","filename":"f3.png"}`, hashBytes([]byte("moredata")))))
	if st != http.StatusForbidden || body["code"] != codeQuotaExceeded {
		t.Fatalf("quota init: %d %#v", st, body)
	}

	// Bob is not affected by alice's usage (§18.2).
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/init", bobTok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":8,"mime":"image/png","filename":"b.png"}`, hashBytes([]byte("bobdata!")))))
	if st != http.StatusOK {
		t.Fatalf("bob init: %d %s", st, body)
	}
}

func TestMediaInitValidation(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	tok := env.token(env.newUser("alice@example.com"))

	// Unknown collection name → unsupported_media (§18.2).
	st, _, body := env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(`{"collection":"other","sha256":"`+strings.Repeat("a", 64)+`","size":1,"mime":"image/png","filename":"x"}`))
	if st != http.StatusBadRequest || body["code"] != codeUnsupportedMedia {
		t.Fatalf("wrong collection: %d %#v", st, body)
	}

	// Deny-listed MIME fails even when the operator allowlists broadly (§18.6).
	cfg2 := mediaTestConfig(func(mc *MediaConfig) { mc.MimeAllowlist = []string{"image/svg+xml", "text/"} })
	env2 := newTestEnv(t, cfg2)
	tok2 := env2.token(env2.newUser("alice@example.com"))
	for _, mime := range []string{"image/svg+xml", "text/html"} {
		st, _, body = env2.doJSON(http.MethodPost, "/api/sync/media/init", tok2, nil,
			[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":1,"mime":%q,"filename":"x"}`, strings.Repeat("a", 64), mime)))
		if st != http.StatusUnsupportedMediaType || body["code"] != codeMimeNotAllowed {
			t.Fatalf("deny mime %q: %d %#v", mime, st, body)
		}
	}
	// A custom allowlist prefix permits M MIME types.
	st, _, body = env2.doJSON(http.MethodPost, "/api/sync/media/init", tok2, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":1,"mime":"text/csv","filename":"x"}`, strings.Repeat("b", 64))))
	if st != http.StatusOK {
		t.Fatalf("custom allowlist: %d %s", st, body)
	}
}

func TestMediaCommitWithPoster(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	// Each staged upload uses unique bytes: media dedup would otherwise
	// short-circuit the session (§18.2).
	// Happy path: the poster session is attached as the poster file (§18.4).
	data := []byte("heic original bytes-1")
	poster := []byte("jpeg poster bytes-1")
	mainID := stageAndUpload(t, env, tok, hashBytes(data), data, "image/heic")
	posterID := stageAndUpload(t, env, tok, hashBytes(poster), poster, "image/jpeg")
	st, _, body := env.doJSON(http.MethodPost, "/api/sync/media/commit", tok, nil,
		[]byte(fmt.Sprintf(`{"uploadId":%q,"posterUploadId":%q}`, mainID, posterID)))
	if st != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("commit with poster: %d %#v", st, body)
	}
	var mediaID string
	if m, ok := body["mediaId"].(string); ok {
		mediaID = m
	}
	rec, err := env.app.FindRecordById("media", mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.GetString(mediaPosterField) == "" {
		t.Fatal("poster field not set")
	}
	if rec.GetString(mediaFileField) == "" {
		t.Fatal("main file field not set")
	}

	// A foreign poster session exists for its uploader only (§18.4).
	data2 := []byte("heic original bytes-2")
	main2 := stageAndUpload(t, env, tok, hashBytes(data2), data2, "image/heic")
	foreignPoster := []byte("jpeg poster bytes-2")
	foreignID := stageAndUpload(t, env, env.token(env.newUser("bob@example.com")), hashBytes(foreignPoster), foreignPoster, "image/jpeg")
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/commit", tok, nil,
		[]byte(fmt.Sprintf(`{"uploadId":%q,"posterUploadId":%q}`, main2, foreignID)))
	if st != http.StatusNotFound || body["code"] != codeUploadNotFound {
		t.Fatalf("foreign poster: %d %#v", st, body)
	}

	// A non-JPEG poster → invalid_request (§18.4).
	data3 := []byte("heic original bytes-3")
	badPoster := []byte("png poster bytes-3")
	main3 := stageAndUpload(t, env, tok, hashBytes(data3), data3, "image/heic")
	badID := stageAndUpload(t, env, tok, hashBytes(badPoster), badPoster, "image/png")
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/commit", tok, nil,
		[]byte(fmt.Sprintf(`{"uploadId":%q,"posterUploadId":%q}`, main3, badID)))
	if st != http.StatusBadRequest || body["code"] != "invalid_request" {
		t.Fatalf("non-jpeg poster: %d %#v", st, body)
	}
}

func TestInfoAdvertisesMedia(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	tok := env.token(env.newUser("alice@example.com"))

	st, body := env.getJSON("/api/sync/info", tok, nil)
	if st != http.StatusOK {
		t.Fatalf("info: %d", st)
	}
	m, ok := body["media"].(map[string]any)
	if !ok {
		t.Fatalf("info has no media object: %#v", body)
	}
	if m["collection"] != "media" || m["maxFileSize"] != float64(1<<20) {
		t.Fatalf("media info: %#v", m)
	}
	if _, ok := m["chunkSize"].(float64); !ok {
		t.Fatalf("chunkSize missing: %#v", m)
	}

	// Without media config, /info has no media object (additive, §18.6).
	env2 := newTestEnv(t, Config{})
	tok2 := env2.token(env2.newUser("alice@example.com"))
	_, body2 := env2.getJSON("/api/sync/info", tok2, nil)
	if _, present := body2["media"]; present {
		t.Fatal("media advertised without config")
	}
}

func TestMediaStagingSweep(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	data := []byte("kept bytes")
	st, body, aID := uploadAndCommit(t, env, tok, cfg.Media, hashBytes(data), "image/png", "a.png", data)
	if st != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("commit: %d %#v", st, body)
	}
	mediaID, _ := body["mediaId"].(string)
	// An abandoned fresh session and an expired stale session.
	freshID := stageAndUpload(t, env, tok, hashBytes([]byte("fresh bytes")), []byte("fresh bytes"), "image/png")
	oldID := stageAndUpload(t, env, tok, hashBytes([]byte("stale bytes")), []byte("stale bytes"), "image/png")
	oldDir := cfg.Media.sessionDir(env.app, oldID)
	if s, found, _ := loadSession(oldDir); found {
		s.Expires = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
		if err := saveTestSession(oldDir, s); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := SweepMediaStaging(env.app, cfg, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("swept %d sessions, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(cfg.Media.stagingRoot(env.app), oldID)); !os.IsNotExist(err) {
		t.Fatal("expired session dir still present")
	}
	for _, keep := range []string{freshID, aID} {
		if _, err := os.Stat(filepath.Join(cfg.Media.stagingRoot(env.app), keep)); err != nil {
			t.Fatalf("kept session dir removed: %v", err)
		}
	}
	// The committed session's stored result survives, keeping re-commit
	// idempotent (§18.4).
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/commit", tok, nil,
		[]byte(fmt.Sprintf(`{"uploadId":%q}`, aID)))
	if st != http.StatusOK || body["mediaId"] != mediaID {
		t.Fatalf("re-commit after sweep: %d %#v", st, body)
	}
}

func TestMediaStagingSweepMalformedSidecar(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)

	// A corrupted sidecar cannot classify its session; the sweep must remove it
	// by directory age so staged bytes never leak forever (§18.6).
	badDir := filepath.Join(cfg.Media.stagingRoot(env.app), "u_"+strings.Repeat("0", 32))
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "data.bin"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "meta.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-2 * cfg.Media.StagingTTL)
	if err := os.Chtimes(badDir, old, old); err != nil {
		t.Fatal(err)
	}

	removed, err := SweepMediaStaging(env.app, cfg, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("swept %d, want 1", removed)
	}
	if _, err := os.Stat(badDir); !os.IsNotExist(err) {
		t.Fatal("malformed dir not swept")
	}
}

func TestMediaGCRefcount(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	// Commit two media records; relate only the first from a note.
	a := []byte("kept bytes!!")
	b := []byte("freed bytes!")
	st1, body1, _ := uploadAndCommit(t, env, tok, cfg.Media, hashBytes(a), "image/png", "a.png", a)
	st2, body2, _ := uploadAndCommit(t, env, tok, cfg.Media, hashBytes(b), "image/png", "b.png", b)
	if st1 != http.StatusOK || st2 != http.StatusOK || body1["status"] != "ok" || body2["status"] != "ok" {
		t.Fatalf("commits: %d %d %s %s", st1, st2, body1, body2)
	}
	aID, _ := body1["mediaId"].(string)
	bID, _ := body2["mediaId"].(string)

	// A relation field on notes targeting media (that's workflow MED-4).
	notes, err := env.app.FindCollectionByNameOrId("notes")
	if err != nil {
		t.Fatal(err)
	}
	mediaCol, err := env.app.FindCollectionByNameOrId("media")
	if err != nil {
		t.Fatal(err)
	}
	notes.Fields.Add(&core.RelationField{Name: "attachment", CollectionId: mediaCol.Id, MaxSelect: 1})
	if err := env.app.Save(notes); err != nil {
		t.Fatalf("add relation field to notes: %v", err)
	}
	note := core.NewRecord(notes)
	note.Set("title", "n")
	note.Set("attachment", aID)
	note.Set("owner", alice.Id)
	if err := env.app.Save(note); err != nil {
		t.Fatalf("save note: %v", err)
	}

	// Age both media records past the grace window.
	for _, id := range []string{aID, bID} {
		if _, err := env.app.DB().NewQuery(
			`UPDATE "media" SET updated = '2020-01-01 00:00:00.000Z' WHERE id = {:id}`).
			Bind(map[string]any{"id": id}).Execute(); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := MediaGC(env.app, cfg, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted %d, want 1 (the unreferenced record)", deleted)
	}
	if _, err := env.app.FindRecordById("media", aID); err != nil {
		t.Fatal("referenced media was deleted")
	}
	if _, err := env.app.FindRecordById("media", bID); err == nil {
		t.Fatal("unreferenced media survived")
	}
	// The GC delete is a normal delete: tombstone rev + change row (§8.1).
	if cr := lastChange(t, env.app, "media", bID); cr.Op != opDelete {
		t.Fatalf("no tombstone change row: %#v", cr)
	}
}

// ---- small media test helpers ----

func TestMediaDedupHitBeatsQuota(t *testing.T) {
	cfg := mediaTestConfig(func(mc *MediaConfig) { mc.PerUserQuota = 8 })
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	// Fill the quota exactly.
	data := []byte("8byte000")
	st, body, _ := uploadAndCommit(t, env, tok, cfg.Media, hashBytes(data), "image/png", "f.png", data)
	if st != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("commit: %d %s", st, body)
	}

	// A dedup hit adds no bytes: it must return `exists`, not quota_exceeded.
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":%d,"mime":"image/png","filename":"f.png"}`, hashBytes(data), len(data))))
	if st != http.StatusOK || body["status"] != "exists" {
		t.Fatalf("dedup at quota: %d %#v", st, body)
	}

	// A different blob still trips the quota.
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":1,"mime":"image/png","filename":"g.png"}`, hashBytes([]byte("x")))))
	if st != http.StatusForbidden || body["code"] != codeQuotaExceeded {
		t.Fatalf("other blob at quota: %d %#v", st, body)
	}
}

func TestMediaQuotaCountsStagedSessions(t *testing.T) {
	cfg := mediaTestConfig(func(mc *MediaConfig) { mc.PerUserQuota = 17 })
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	// Commit 8 bytes; consume that session (its bytes are gone).
	data := []byte("8byte000")
	st, body, _ := uploadAndCommit(t, env, tok, cfg.Media, hashBytes(data), "image/png", "f.png", data)
	if st != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("commit: %d %s", st, body)
	}

	// Stage a second live session of 8 bytes (not committed).
	staged := []byte("staged00")
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":%d,"mime":"image/png","filename":"s.png"}`, hashBytes(staged), len(staged))))
	if st != http.StatusOK {
		t.Fatalf("stage init: %d %s", st, body)
	}
	sID, _ := body["uploadId"].(string)
	if st, _, body := env.doJSON(http.MethodPatch, "/api/sync/media/upload/"+sID, tok,
		map[string]string{"Upload-Offset": "0", "Content-Type": tusCTType}, staged); st != http.StatusNoContent {
		t.Fatalf("stage patch: %d %s", st, body)
	}

	// Init a third small file: committed 8 + staged 8 + 2 = 18 > 17. Accounting
	// that ignored staged sessions would wrongly pass (8 + 2 = 10).
	st, _, body = env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":2,"mime":"image/png","filename":"t.png"}`, hashBytes([]byte("tw")))))
	if st != http.StatusForbidden || body["code"] != codeQuotaExceeded {
		t.Fatalf("staged session not counted: %d %#v", st, body)
	}
}

func TestMediaConsumedSessionOnUploadChannel(t *testing.T) {
	cfg := mediaTestConfig(nil)
	env := newTestEnv(t, cfg)
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	data := []byte("committed bytes")
	_, body, uploadID := uploadAndCommit(t, env, tok, cfg.Media, hashBytes(data), "image/png", "f.png", data)
	if body["status"] != "ok" {
		t.Fatal("commit failed")
	}

	// HEAD/PATCH/DELETE on a consumed session → 404 upload_not_found; its stored
	// result must survive (it backs idempotent re-commits, §18.4).
	headers := map[string]string{"Upload-Offset": "0", "Content-Type": tusCTType}
	checks := []struct {
		method string
		hdrs   map[string]string
	}{
		{http.MethodHead, nil},
		{http.MethodPatch, headers},
		{http.MethodDelete, nil},
	}
	for _, c := range checks {
		st, _, got := env.doJSON(c.method, "/api/sync/media/upload/"+uploadID, tok, c.hdrs, []byte("x"))
		if st != http.StatusNotFound || got["code"] != codeUploadNotFound {
			t.Fatalf("%s consumed session: %d %#v", c.method, st, got)
		}
	}
	st, _, got := env.doJSON(http.MethodPost, "/api/sync/media/commit", tok, nil,
		[]byte(fmt.Sprintf(`{"uploadId":%q}`, uploadID)))
	if st != http.StatusOK || got["status"] != "ok" {
		t.Fatalf("re-commit after upload-channel 404s: %d %#v", st, got)
	}
}

// stageAndUpload starts a session and uploads all bytes, returning the uploadId.
func stageAndUpload(t *testing.T, env *testEnv, tok, sha string, data []byte, mime string) string {
	t.Helper()
	st, _, body := env.doJSON(http.MethodPost, "/api/sync/media/init", tok, nil,
		[]byte(fmt.Sprintf(`{"collection":"media","sha256":%q,"size":%d,"mime":%q,"filename":"f"}`, sha, len(data), mime)))
	if st != http.StatusOK {
		t.Fatalf("init: %d %s", st, body)
	}
	id, _ := body["uploadId"].(string)
	if id == "" {
		t.Fatalf("no uploadId: %#v", body)
	}
	st, _, body = env.doJSON(http.MethodPatch, "/api/sync/media/upload/"+id, tok,
		map[string]string{"Upload-Offset": "0", "Content-Type": tusCTType}, data)
	if st != http.StatusNoContent {
		t.Fatalf("patch: %d %s", st, body)
	}
	return id
}

// saveTestSession overwrites a staging sidecar (for expiry simulation).
func saveTestSession(dir string, s mediaSession) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), raw, 0o600)
}

func TestEnsureMediaCollectionProtectsFilesAndScopesRules(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(app.Cleanup)

	users := core.NewAuthCollection("syncusers")
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureMediaCollection(app, "media", users.Id, 0, 0); err != nil {
		t.Fatalf("EnsureMediaCollection: %v", err)
	}

	col, err := app.FindCollectionByNameOrId("media")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{mediaFileField, mediaPosterField} {
		f, ok := col.Fields.GetByName(name).(*core.FileField)
		if !ok || !f.Protected {
			t.Fatalf("field %q must be Protected so files need a token/view rule (H-1)", name)
		}
	}
	if col.ListRule == nil || *col.ListRule != mediaViewRule {
		t.Fatalf("list rule = %v, want %q", col.ListRule, mediaViewRule)
	}
	if col.ViewRule == nil || *col.ViewRule != mediaViewRule {
		t.Fatalf("view rule = %v, want %q", col.ViewRule, mediaViewRule)
	}
	for name, rule := range map[string]*string{
		"create": col.CreateRule, "update": col.UpdateRule, "delete": col.DeleteRule,
	} {
		if rule != nil {
			t.Fatalf("%s rule must stay locked (nil), got %q", name, *rule)
		}
	}

	// validateMediaCollection accepts the provisioned shape.
	if err := validateMediaCollection(app, MediaConfig{Collection: "media"}); err != nil {
		t.Fatalf("validate provisioned collection: %v", err)
	}
}

func TestValidateMediaCollectionRejectsInsecureShapes(t *testing.T) {
	newApp := func(t *testing.T) *tests.TestApp {
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
		if _, err := EnsureMediaCollection(app, "media", users.Id, 0, 0); err != nil {
			t.Fatal(err)
		}
		return app
	}

	// Unprotected file field → reject (H-1).
	app := newApp(t)
	col, _ := app.FindCollectionByNameOrId("media")
	// Replace the protected field with an unprotected one of the same type.
	idx := col.Fields.GetByName(mediaFileField).GetId()
	col.Fields.RemoveById(idx)
	col.Fields.Add(&core.FileField{Name: mediaFileField, MaxSelect: 1, Protected: false})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	if err := validateMediaCollection(app, MediaConfig{Collection: "media"}); err == nil ||
		!strings.Contains(err.Error(), "Protected") {
		t.Fatalf("want Protected error, got %v", err)
	}

	// Open write rule → reject (M-2).
	app2 := newApp(t)
	col2, _ := app2.FindCollectionByNameOrId("media")
	open := ""
	col2.CreateRule = &open
	if err := app2.Save(col2); err != nil {
		t.Fatal(err)
	}
	if err := validateMediaCollection(app2, MediaConfig{Collection: "media"}); err == nil ||
		!strings.Contains(err.Error(), "locked") {
		t.Fatalf("want locked-rules error, got %v", err)
	}

	// Nil view rule → reject (files would 404 for owners).
	app3 := newApp(t)
	col3, _ := app3.FindCollectionByNameOrId("media")
	col3.ViewRule = nil
	if err := app3.Save(col3); err != nil {
		t.Fatal(err)
	}
	if err := validateMediaCollection(app3, MediaConfig{Collection: "media"}); err == nil ||
		!strings.Contains(err.Error(), "list/view rules") {
		t.Fatalf("want view-rule error, got %v", err)
	}
}

func TestEnsureMediaCollectionConvergesUnprotectedCollection(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(app.Cleanup)

	users := core.NewAuthCollection("syncusers")
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}
	// Simulate a collection created before the Protected/rule hardening.
	col := core.NewBaseCollection("media")
	col.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	col.Fields.Add(&core.TextField{Name: mediaSHAField})
	col.Fields.Add(&core.NumberField{Name: mediaSizeField, OnlyInt: true})
	col.Fields.Add(&core.TextField{Name: mediaMimeField})
	col.Fields.Add(&core.TextField{Name: mediaNameField})
	col.Fields.Add(&core.FileField{Name: mediaFileField, MaxSelect: 1})
	col.Fields.Add(&core.FileField{Name: mediaPosterField, MaxSelect: 1})
	col.Fields.Add(&core.RelationField{Name: mediaOwnerField, CollectionId: users.Id, MaxSelect: 1})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureMediaCollection(app, "media", users.Id, 0, 0); err != nil {
		t.Fatalf("converge: %v", err)
	}
	got, err := app.FindCollectionByNameOrId("media")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{mediaFileField, mediaPosterField} {
		if f, ok := got.Fields.GetByName(name).(*core.FileField); !ok || !f.Protected {
			t.Fatalf("field %q was not converged to Protected", name)
		}
	}
	if got.ViewRule == nil || *got.ViewRule != mediaViewRule {
		t.Fatalf("view rule was not converged: %v", got.ViewRule)
	}
	// A custom (non-nil) rule is preserved.
	custom := "owner = @request.auth.id && size > 0"
	got.ViewRule = &custom
	if err := app.Save(got); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureMediaCollection(app, "media", users.Id, 0, 0); err != nil {
		t.Fatalf("converge custom: %v", err)
	}
	again, _ := app.FindCollectionByNameOrId("media")
	if again.ViewRule == nil || *again.ViewRule != custom {
		t.Fatalf("custom view rule overwritten: %v", again.ViewRule)
	}
}
