package syncplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Conformance runner skeleton (protocol §15).
//
// At M0 this validates that every fixture is well-formed and internally
// consistent with the protocol's wire rules. The execution driver — replaying
// each step against a server — is wired in at M1, once the server exists. The
// TS client suite (client/core) validates the same fixtures, so a green run on
// both sides means both agree with docs/protocol.md.

var (
	recordIDRe   = regexp.MustCompile(`^[a-z0-9]{15}$`)
	mutationIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type fixture struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Config      json.RawMessage `json:"config"`
	Users       []string        `json:"users"`
	Blobs       map[string]string `json:"blobs"`
	Steps       []fixtureStep   `json:"steps"`
}

type fixtureStep struct {
	As     string          `json:"as"`
	Push   json.RawMessage `json:"push"`
	Pull   json.RawMessage `json:"pull"`
	Media  json.RawMessage `json:"media"`
	Expect json.RawMessage `json:"expect"`
}

// fixtureMediaStep is a single media-channel interaction (protocol §18).
type fixtureMediaStep struct {
	Op      string          `json:"op"`
	Request json.RawMessage `json:"request"`
	Blob    string          `json:"blob"`
	Slice   []int           `json:"slice"`
}

type fixturePush struct {
	Mutations []fixtureMutation `json:"mutations"`
}

type fixtureMutation struct {
	ID         string                     `json:"id"`
	Collection string                     `json:"collection"`
	RecordID   string                     `json:"recordId"`
	Op         string                     `json:"op"`
	BaseRev    *int                       `json:"baseRev"`
	Fields     map[string]json.RawMessage `json:"fields"`
}

func isSet(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

func TestConformanceFixturesAreWellFormed(t *testing.T) {
	dir := filepath.Join("..", "..", "conformance", "fixtures")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixtures dir %q: %v", dir, err)
	}

	var count int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		count++
		file := e.Name()
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Errorf("%s: read: %v", file, err)
			continue
		}

		var f fixture
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Errorf("%s: invalid JSON: %v", file, err)
			continue
		}

		if want := strings.TrimSuffix(file, ".json"); f.Name != want {
			t.Errorf("%s: name %q must equal filename", file, f.Name)
		}
		if f.Description == "" {
			t.Errorf("%s: description is required", file)
		}
		if !isSet(f.Config) {
			t.Errorf("%s: config is required", file)
		}
		if len(f.Users) == 0 {
			t.Errorf("%s: users must be non-empty", file)
		}
		if len(f.Steps) == 0 {
			t.Errorf("%s: steps must be non-empty", file)
		}

		users := make(map[string]bool, len(f.Users))
		for _, u := range f.Users {
			users[u] = true
		}

		for i, s := range f.Steps {
			ctx := fmt.Sprintf("%s step %d", file, i)
			if s.As == "" || !users[s.As] {
				t.Errorf("%s: `as` %q must name one of the configured users", ctx, s.As)
			}
			hasPush, hasPull := isSet(s.Push), isSet(s.Pull)
			hasMedia := isSet(s.Media)
			if hasMedia {
				if hasPush || hasPull {
					t.Errorf("%s: exactly one of push/pull/media must be set", ctx)
				}
			} else if hasPush == hasPull {
				t.Errorf("%s: exactly one of push/pull/media must be set", ctx)
			}
			if !isSet(s.Expect) {
				t.Errorf("%s: expect is required", ctx)
			}
			if hasMedia {
				var m fixtureMediaStep
				if err := json.Unmarshal(s.Media, &m); err != nil {
					t.Errorf("%s: invalid media step: %v", ctx, err)
					continue
				}
				switch m.Op {
				case "init", "commit":
					if !isSet(m.Request) {
						t.Errorf("%s: media %s requires request", ctx, m.Op)
					}
				case "upload":
					if m.Blob == "" {
						t.Errorf("%s: media upload requires blob", ctx)
					} else if _, ok := f.Blobs[m.Blob]; !ok {
						t.Errorf("%s: media upload references unknown blob %q", ctx, m.Blob)
					}
					if len(m.Slice) != 0 && len(m.Slice) != 2 {
						t.Errorf("%s: media upload slice must be [from,to]", ctx)
					} else if len(m.Slice) == 2 {
						blob := f.Blobs[m.Blob]
						if m.Slice[0] < 0 || m.Slice[1] > len(blob) || m.Slice[0] > m.Slice[1] {
							t.Errorf("%s: media upload slice %v out of range for blob %q", ctx, m.Slice, m.Blob)
						}
					}
				case "offset", "abort":
				default:
					t.Errorf("%s: media op %q must be init|upload|offset|commit|abort", ctx, m.Op)
				}
				continue
			}
			if !hasPush {
				continue
			}

			var p fixturePush
			if err := json.Unmarshal(s.Push, &p); err != nil {
				t.Errorf("%s: invalid push: %v", ctx, err)
				continue
			}
			if len(p.Mutations) == 0 {
				t.Errorf("%s: push.mutations must be non-empty", ctx)
			}
			for j, m := range p.Mutations {
				mctx := fmt.Sprintf("%s mutation %d (%s)", ctx, j, m.ID)
				if !mutationIDRe.MatchString(m.ID) {
					t.Errorf("%s: mutation id must match %s", mctx, mutationIDRe)
				}
				if m.Collection == "" {
					t.Errorf("%s: collection is required", mctx)
				}
				if !recordIDRe.MatchString(m.RecordID) {
					t.Errorf("%s: recordId %q must match %s", mctx, m.RecordID, recordIDRe)
				}
				switch m.Op {
				case "create":
					if m.BaseRev != nil {
						t.Errorf("%s: create must omit baseRev", mctx)
					}
				case "update":
					if m.BaseRev == nil {
						t.Errorf("%s: update requires baseRev", mctx)
					}
					if len(m.Fields) == 0 {
						t.Errorf("%s: update requires non-empty fields", mctx)
					}
				case "delete":
					if m.BaseRev == nil {
						t.Errorf("%s: delete requires baseRev", mctx)
					}
					if len(m.Fields) != 0 {
						t.Errorf("%s: delete must omit fields", mctx)
					}
				default:
					t.Errorf("%s: op %q must be create|update|delete", mctx, m.Op)
				}
			}
		}
	}

	if count == 0 {
		t.Fatalf("no fixtures found in %q", dir)
	}
	t.Logf("validated %d conformance fixtures", count)
}
