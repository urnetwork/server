package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A test binary reads a vault outside every fixture root as empty, whether it
// is named explicitly or derived from WARP_HOME, and reads it after the
// explicit opt-in.
func TestTestBinaryReadsOnlyAFixtureVault(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", "")
	t.Setenv(TestRealVaultEnv, "")
	outside := filepath.Join(string(filepath.Separator), "nonexistent-urnetwork-deployment")

	requireRefused := func(name string) {
		_, err := Vault.SimpleResource("pg.yml")
		if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), TestRealVaultEnv) {
			t.Fatalf("%s: the vault read %v, want a not-found refusal naming %s", name, err, TestRealVaultEnv)
		}
	}
	t.Setenv("WARP_VAULT_HOME", filepath.Join(outside, "vault"))
	requireRefused("explicit vault")
	t.Setenv("WARP_VAULT_HOME", "")
	t.Setenv("WARP_HOME", outside)
	requireRefused("vault derived from WARP_HOME")

	t.Setenv(TestRealVaultEnv, "1")
	_, err := Vault.SimpleResource("pg.yml")
	if !errors.Is(err, ErrResourceNotFound) || strings.Contains(err.Error(), TestRealVaultEnv) {
		t.Fatalf("opted-in read %v, want the plain lookup of an absent vault", err)
	}
	t.Setenv(TestRealVaultEnv, "")

	// a production binary reads its deployment vault as before
	if err := refuseNonFixtureVault(filepath.Join(outside, "vault"), false); err != nil {
		t.Fatalf("a non-test binary refused its vault: %v", err)
	}
	if err := refuseNonFixtureVault(filepath.Join(outside, "vault"), true); err == nil {
		t.Fatal("a test binary accepted a vault outside every fixture root")
	}
}

// The release gate's private fixture, the repository's synthetic testdata and
// a vault a test writes below the temp directory stay readable.
func TestTestBinaryReadsFixtureVaults(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	t.Setenv(TestRealVaultEnv, "")

	portableRoot := filepath.Join(string(filepath.Separator), "nonexistent-release-gate-fixture")
	t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", portableRoot)
	if err := refuseNonFixtureVault(filepath.Join(portableRoot, "vault"), true); err != nil {
		t.Fatalf("release gate fixture vault refused: %v", err)
	}
	if err := refuseNonFixtureVault(portableRoot+"-sibling", true); err == nil {
		t.Fatal("a sibling of the release gate fixture was accepted")
	}
	t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", "")

	t.Setenv("WARP_VAULT_HOME", filepath.Join(syntheticTestResourceRoot, "vault"))
	if _, err := Vault.SimpleResource("pg.yml"); err != nil {
		t.Fatalf("synthetic local/testdata vault: %v", err)
	}

	vaultHome := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(filepath.Join(vaultHome, "local"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vaultHome, "local", "fixture.yml"), []byte("key: value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_VAULT_HOME", vaultHome)
	resource, err := Vault.SimpleResource("fixture.yml")
	if err != nil || resource.RequireString("key") != "value" {
		t.Fatalf("temp directory vault: %v", err)
	}
}
