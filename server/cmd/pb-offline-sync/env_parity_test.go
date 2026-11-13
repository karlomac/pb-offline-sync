package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// envVarRe finds the env variable names the server actually reads.
var envVarRe = regexp.MustCompile(`\b((?:ADMIN|SYNC|AUTH|PB)_[A-Z0-9_]*[A-Z0-9])\b`)

// TestConfigSurfaceCoveredEverywhere guards the deployment contract: the
// variables the binary reads must be forwarded by docker-compose.yml (Dokploy
// injects only declared variables) and shown in .env.example. Drift here has
// already shipped once (AUTH_COLLECTION was unforwardable, issue #11).
func TestConfigSurfaceCoveredEverywhere(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	sources := []string{
		"main.go",
		filepath.Join("..", "..", "authplugin", "config.go"),
		filepath.Join(root, "docker-entrypoint.sh"),
	}

	required := map[string]bool{}
	for _, name := range sources {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range envVarRe.FindAllString(string(raw), -1) {
			// PB_VERSION is a build arg (compose `args:`), not a runtime env var.
			if m == "PB_VERSION" {
				continue
			}
			required[m] = true
		}
	}
	if len(required) < 20 {
		t.Fatalf("env extraction looks wrong, only found %d variables: %v", len(required), required)
	}

	compose, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	example, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	var missingCompose, missingExample []string
	for name := range required {
		// Compose forwards `NAME: ${NAME:-}` under environment:.
		if !regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(name) + `:`).Match(compose) {
			missingCompose = append(missingCompose, name)
		}
		// .env.example shows it either active or commented out.
		if !regexp.MustCompile(`(?m)^#?\s*` + regexp.QuoteMeta(name) + `=`).Match(example) {
			missingExample = append(missingExample, name)
		}
	}
	sort.Strings(missingCompose)
	sort.Strings(missingExample)
	if len(missingCompose) > 0 {
		t.Errorf("docker-compose.yml does not forward: %s", strings.Join(missingCompose, ", "))
	}
	if len(missingExample) > 0 {
		t.Errorf(".env.example does not document: %s", strings.Join(missingExample, ", "))
	}
}
