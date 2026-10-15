package syncplugin

import (
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

const fieldTypeFile = "file"

// bindRoutes registers the sync endpoints, all behind auth (protocol §2, §14.1).
func bindRoutes(app core.App, r *router.Router[*core.RequestEvent], cfg Config) {
	r.GET("/api/sync/info", func(e *core.RequestEvent) error {
		return handleInfo(e, cfg)
	}).Bind(apis.RequireAuth())

	r.GET("/api/sync/pull", func(e *core.RequestEvent) error {
		return handlePull(e, cfg)
	}).Bind(apis.RequireAuth())

	r.POST("/api/sync/push", func(e *core.RequestEvent) error {
		return handlePush(e, cfg)
	}).Bind(apis.RequireAuth())
}

// handleInfo serves GET /api/sync/info (protocol §5).
func handleInfo(e *core.RequestEvent, cfg Config) error {
	if !protocolOK(e) {
		return nil
	}

	collections := make(map[string]any, len(cfg.Collections))
	for name, cc := range cfg.Collections {
		col, err := e.App.FindCollectionByNameOrId(name)
		if err != nil {
			return err
		}

		fields := make([]map[string]any, 0, len(col.Fields))
		for _, f := range col.Fields {
			if f.GetSystem() || f.GetName() == cc.OwnerField {
				continue // server-authoritative fields are not advertised (§4.1)
			}
			fields = append(fields, map[string]any{
				"name":     f.GetName(),
				"type":     f.Type(),
				"writable": f.Type() != fieldTypeFile,
			})
		}

		collections[name] = map[string]any{
			"ownerField":     cc.OwnerField,
			"conflictPolicy": string(cc.Conflict),
			"deletePolicy":   string(cc.DeletePolicy),
			"fields":         fields,
		}
	}

	return e.JSON(http.StatusOK, map[string]any{
		"protocol":      ProtocolVersion,
		"supported":     []int{ProtocolVersion},
		"pluginVersion": PluginVersion,
		"serverTime":    serverTime(),
		"limits":        limitsMap(cfg),
		"collections":   collections,
	})
}

// limitsMap is the /info `limits` object (protocol §5). ChangeLogRetention is
// intentionally absent: the protocol advertises only idempotencyRetentionDays,
// so config.ChangeLogRetention stays server-side.
func limitsMap(cfg Config) map[string]any {
	return map[string]any{
		"maxBatch":                 cfg.MaxBatch,
		"maxPage":                  cfg.MaxPage,
		"defaultPage":              cfg.DefaultPage,
		"maxBodyBytes":             cfg.MaxBodyBytes,
		"idempotencyRetentionDays": cfg.IdempotencyRetentionDays,
	}
}

// protocolOK enforces the mandatory X-Sync-Protocol header (protocol §2) and
// writes a 400 protocol_unsupported body when it is missing/unsupported.
func protocolOK(e *core.RequestEvent) bool {
	if e.Request.Header.Get("X-Sync-Protocol") == "1" {
		return true
	}
	_ = writeError(e, http.StatusBadRequest, "protocol_unsupported",
		"Missing or unsupported X-Sync-Protocol header.",
		map[string]any{"supported": []int{ProtocolVersion}})
	return false
}

func writeError(e *core.RequestEvent, status int, code, message string, extra map[string]any) error {
	body := map[string]any{"status": status, "code": code, "message": message}
	for k, v := range extra {
		body[k] = v
	}
	return e.JSON(status, body)
}

// serverTime returns a UTC ISO-8601 timestamp as used on the wire (protocol §7.1).
func serverTime() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
