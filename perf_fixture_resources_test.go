// A named-file consumer executes actual Server init and Vault resource selection.
// Its only resources are a generated private suite and a foreign synthetic tree.
package server_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	server "github.com/urnetwork/server"
)

// Resolves database and non-database resources before checking environment roots;
// a permitted managed launcher must not substitute for the owned private suite.
func TestPerfFixtureCompiledServerGuard(t *testing.T) {
	if os.Getenv("URNETWORK_PERF_RESOURCE_CHILD") != "1" {
		return
	}
	root := os.Getenv("URNETWORK_PERF_EXPECTED_FIXTURE")
	if root == "" {
		t.Fatal("private fixture witness missing")
	}
	vaultRoot, err := filepath.EvalSymlinks(filepath.Join(root, "vault"))
	if err != nil {
		t.Fatalf("resolve expected private Vault: %v", err)
	}
	for _, name := range []string{"pg.yml", "password.yml"} {
		selected, err := server.Vault.ResourcePath(name)
		if err != nil {
			t.Errorf("actual Server resource selection %s: %v", name, err)
			continue
		}
		resolved, err := filepath.EvalSymlinks(selected)
		if err != nil {
			t.Errorf("resolve selected Server resource %s: %v", name, err)
			continue
		}
		relative, err := filepath.Rel(vaultRoot, resolved)
		if err != nil || relative == "." || !filepath.IsLocal(relative) {
			t.Errorf("actual Server resource %s resolved outside private fixture", name)
			continue
		}
		if _, err := server.Vault.SimpleResource(name); err != nil {
			t.Errorf("actual private Vault read %s: %v", name, err)
		}
	}
	if t.Failed() {
		return
	}
	if os.Getenv("URNETWORK_PERF_AMBIENT_SITE") != "" || os.Getenv("URNETWORK_TEST_REAL_VAULT") != "" {
		t.Fatal("ambient site or real-vault opt-in reached Server initialization")
	}
	if os.Getenv("WARP_TEST_ENV_LAUNCHER_VAULT_HOME") != "" {
		t.Fatal("managed launcher vault reached private Server initialization")
	}
	for key, name := range map[string]string{"WARP_VAULT_HOME": "vault", "WARP_CONFIG_HOME": "config", "WARP_SITE_HOME": "site"} {
		if os.Getenv(key) != filepath.Join(root, name) {
			t.Errorf("wrong private resource root: %s", key)
		}
	}
	if _, err := server.Config.SimpleResource("db.yml"); err != nil {
		t.Fatalf("actual fixture Config read: %v", err)
	}
	fmt.Println("perf-private-fixture-server-ok")
}
