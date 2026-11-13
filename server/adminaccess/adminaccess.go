// Package adminaccess optionally restricts PocketBase's admin surfaces to a set
// of source addresses, with an emergency bypass and a time-boxed temporary
// window. It is defense-in-depth for deployments that expose the app publicly:
// PocketBase itself always serves the admin UI and cannot disable it.
package adminaccess

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// Config is the parsed guard configuration. The zero value is disabled.
type Config struct {
	// AllowIPs are the source addresses (single IPs or CIDR prefixes) allowed to
	// reach the admin surfaces.
	AllowIPs []netip.Prefix

	// AllowAll opens the admin surfaces to every source IP (emergency use; it is
	// logged loudly at boot).
	AllowAll bool

	// AllowUntil opens the admin surfaces to every source IP until this instant.
	// After it, only AllowIPs are accepted; with no allowlist the admin surfaces
	// stay closed (fail closed).
	AllowUntil time.Time
}

// Enabled reports whether the guard should be installed. An empty
// configuration keeps the historical behaviour (no restriction).
func (c Config) Enabled() bool {
	return len(c.AllowIPs) > 0 || c.AllowAll || !c.AllowUntil.IsZero()
}

// ParseConfig parses the ADMIN_ALLOW_IPS (comma-separated IPs/CIDRs),
// ADMIN_ALLOW_ALL (bool) and ADMIN_ALLOW_UNTIL (RFC 3339) values.
func ParseConfig(allowIPs string, allowAll bool, allowUntil string) (Config, error) {
	var c Config

	for _, raw := range strings.Split(allowIPs, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if p, err := netip.ParsePrefix(raw); err == nil {
			c.AllowIPs = append(c.AllowIPs, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return c, fmt.Errorf("ADMIN_ALLOW_IPS: %q is not an IP address or CIDR prefix", raw)
		}
		addr = addr.Unmap()
		c.AllowIPs = append(c.AllowIPs, netip.PrefixFrom(addr, addr.BitLen()))
	}

	c.AllowAll = allowAll

	if v := strings.TrimSpace(allowUntil); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return c, fmt.Errorf("ADMIN_ALLOW_UNTIL: %w", err)
		}
		c.AllowUntil = t.UTC()
	}

	return c, nil
}

// Allows reports whether the source IP may reach the admin surfaces at now.
func (c Config) Allows(ip string, now time.Time) bool {
	if c.AllowAll {
		return true
	}
	if !c.AllowUntil.IsZero() && now.UTC().Before(c.AllowUntil) {
		return true
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range c.AllowIPs {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// IsAdminPath reports whether path is part of the admin UI or a superuser-only
// API group. Record CRUD and the sync/media routes are intentionally excluded.
func IsAdminPath(path string) bool {
	if path == "/_" || strings.HasPrefix(path, "/_/") {
		return true
	}
	for _, prefix := range []string{
		"/api/collections/_superusers",
		"/api/settings",
		"/api/backups",
		"/api/logs",
		"/api/crons",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// Register installs the guard as a router middleware when enabled.
func Register(app core.App, cfg Config) {
	if !cfg.Enabled() {
		return
	}

	now := time.Now().UTC()
	switch {
	case cfg.AllowAll:
		app.Logger().Warn("adminaccess: emergency bypass enabled (ADMIN_ALLOW_ALL=true); admin surfaces are open to every source IP")
	case !cfg.AllowUntil.IsZero() && now.Before(cfg.AllowUntil):
		app.Logger().Warn("adminaccess: temporary admin bypass active", "until", cfg.AllowUntil.Format(time.RFC3339))
	}
	if len(cfg.AllowIPs) == 0 && !cfg.AllowAll && !cfg.AllowUntil.IsZero() {
		app.Logger().Warn("adminaccess: no IP allowlist configured; admin surfaces will be closed after the temporary window")
	}

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.Router.BindFunc(func(re *core.RequestEvent) error {
			if !IsAdminPath(re.Request.URL.Path) {
				return re.Next()
			}
			ip := re.RealIP()
			if cfg.Allows(ip, time.Now().UTC()) {
				return re.Next()
			}
			app.Logger().Warn("adminaccess: denied admin request", "ip", ip, "path", re.Request.URL.Path)
			return apis.NewApiError(http.StatusForbidden, "Admin access is restricted.", nil)
		})
		return e.Next()
	})
}
