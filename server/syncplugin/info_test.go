package syncplugin

import (
	"net/http"
	"testing"
)

func TestInfoShape(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")

	status, body := env.getJSON("/api/sync/info", env.token(alice), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if body["protocol"].(float64) != ProtocolVersion {
		t.Fatalf("protocol = %v", body["protocol"])
	}
	if body["pluginVersion"] != PluginVersion {
		t.Fatalf("pluginVersion = %v", body["pluginVersion"])
	}
	if _, ok := body["serverTime"].(string); !ok {
		t.Fatalf("serverTime missing: %v", body["serverTime"])
	}

	limits := body["limits"].(map[string]any)
	for k, want := range map[string]float64{
		"maxBatch": 100, "maxPage": 500, "defaultPage": 200,
		"maxBodyBytes": 1048576, "idempotencyRetentionDays": 14,
	} {
		if limits[k].(float64) != want {
			t.Fatalf("limits[%s] = %v, want %v", k, limits[k], want)
		}
	}

	notes := body["collections"].(map[string]any)["notes"].(map[string]any)
	if notes["ownerField"] != "owner" {
		t.Fatalf("ownerField = %v", notes["ownerField"])
	}
	if notes["conflictPolicy"] != string(ServerWins) || notes["deletePolicy"] != string(DeleteWins) {
		t.Fatalf("policies = %v / %v", notes["conflictPolicy"], notes["deletePolicy"])
	}

	writable := map[string]bool{}
	for _, raw := range notes["fields"].([]any) {
		f := raw.(map[string]any)
		writable[f["name"].(string)] = f["writable"].(bool)
	}
	for name, want := range map[string]bool{"title": true, "body": true, "attachments": false} {
		got, ok := writable[name]
		if !ok {
			t.Fatalf("fields missing %q: %v", name, writable)
		}
		if got != want {
			t.Fatalf("field %q writable = %v, want %v", name, got, want)
		}
	}
	for _, name := range []string{"id", "owner", "created", "updated"} {
		if _, ok := writable[name]; ok {
			t.Fatalf("server-authoritative field %q advertised in /info", name)
		}
	}
}

func TestInfoProtocolHeader(t *testing.T) {
	env := newTestEnv(t, Config{})
	alice := env.newUser("alice@example.com")
	tok := env.token(alice)

	status, body := env.getJSON("/api/sync/info", tok, map[string]string{"X-Sync-Protocol": ""})
	if status != http.StatusBadRequest || body["code"] != "protocol_unsupported" {
		t.Fatalf("missing header: status=%d code=%v", status, body["code"])
	}
	supported, ok := body["supported"].([]any)
	if !ok || len(supported) != 1 || supported[0].(float64) != ProtocolVersion {
		t.Fatalf("supported = %v", body["supported"])
	}

	status, body = env.getJSON("/api/sync/info", tok, map[string]string{"X-Sync-Protocol": "2"})
	if status != http.StatusBadRequest || body["code"] != "protocol_unsupported" {
		t.Fatalf("unsupported header: status=%d code=%v", status, body["code"])
	}
}

func TestInfoRequiresAuth(t *testing.T) {
	env := newTestEnv(t, Config{})
	res, _ := env.request(http.MethodGet, "/api/sync/info", "", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated info status = %d, want 401", res.StatusCode)
	}
}
