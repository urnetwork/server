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

// The guard runs before the resolution cache: a resolution cached while a test
// opted in to a non-fixture vault is refused once the opt-in is withdrawn, and
// a refusal is never cached.
func TestTestBinaryVaultGuardPrecedesTheResolutionCache(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", "")
	vaultHome := filepath.Join(t.TempDir(), "vault")
	writeResolverFixture(t, vaultHome, map[string]string{"local/fixture.yml": "key: value\n"})
	// the vault is no longer below the temp directory or GOTMPDIR, so it is not
	// a fixture
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("GOTMPDIR", "")
	t.Setenv("WARP_VAULT_HOME", vaultHome)
	if err := refuseNonFixtureVault(vaultHome, true); err == nil {
		t.Fatal("the vault is still a fixture")
	}
	resolver := newSettledResolver(MOUNT_TYPE_VAULT)

	t.Setenv(TestRealVaultEnv, "1")
	version := resolver.resourceVersion("fixture.yml")
	paths, err := resolver.ResourcePaths("fixture.yml")
	if err != nil || len(paths) == 0 || paths[0] != filepath.Join(vaultHome, "local", "fixture.yml") {
		t.Fatalf("opted-in read %v, %v", paths, err)
	}
	if resolver.resourceVersion("fixture.yml") != version {
		t.Fatal("the opted-in resolution was not cached")
	}

	t.Setenv(TestRealVaultEnv, "")
	for lookup := 0; lookup < 2; lookup += 1 {
		_, err := resolver.ResourcePaths("fixture.yml")
		if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), TestRealVaultEnv) {
			t.Fatalf("lookup %d after the opt-in was withdrawn read %v, want the refusal naming %s", lookup, err, TestRealVaultEnv)
		}
	}
	if resolver.resourceVersion("fixture.yml") == resolver.resourceVersion("fixture.yml") {
		t.Fatal("a refused resolution was reused")
	}
}

// A vault below a GOTMPDIR kept apart from TMPDIR, as the release gate's child
// keeps them, with neither the release gate's fixture root nor the opt-in set.
// Returns GOTMPDIR, which the caller sets, and the vault, named by
// WARP_VAULT_HOME.
func gotmpdirVaultFixture(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", "")
	t.Setenv(TestRealVaultEnv, "")
	root := t.TempDir()
	temp := filepath.Join(root, "tmp")
	if err := os.Mkdir(temp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temp)
	gotmpdir := filepath.Join(root, "gotmp")
	vaultHome := filepath.Join(gotmpdir, "vault")
	writeResolverFixture(t, vaultHome, map[string]string{"local/fixture.yml": "key: value\n"})
	t.Setenv("WARP_VAULT_HOME", vaultHome)
	return gotmpdir, vaultHome
}

// Go's t.TempDir creates its directory below GOTMPDIR when GOTMPDIR is set, so
// a vault a test writes there is a fixture although it is not below TMPDIR.
func TestTestBinaryReadsAVaultBelowGOTMPDIR(t *testing.T) {
	gotmpdir, vaultHome := gotmpdirVaultFixture(t)
	t.Setenv("GOTMPDIR", gotmpdir)
	if err := refuseNonFixtureVault(vaultHome, true); err != nil {
		t.Fatalf("a vault below GOTMPDIR was refused: %v", err)
	}
	resource, err := Vault.SimpleResource("fixture.yml")
	if err != nil || resource.RequireString("key") != "value" {
		t.Fatalf("GOTMPDIR vault: %v", err)
	}
}

// The same vault is not a fixture when GOTMPDIR is unset or empty, or names
// another directory: TMPDIR, a sibling, or one below the vault.
func TestTestBinaryRefusesAVaultBelowAnUnsetOrOtherGOTMPDIR(t *testing.T) {
	gotmpdir, vaultHome := gotmpdirVaultFixture(t)
	requireRefused := func(name string) {
		t.Helper()
		if err := refuseNonFixtureVault(vaultHome, true); err == nil {
			t.Fatalf("%s: the vault was accepted", name)
		}
		_, err := Vault.SimpleResource("fixture.yml")
		if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), TestRealVaultEnv) {
			t.Fatalf("%s: the vault read %v, want a not-found refusal naming %s", name, err, TestRealVaultEnv)
		}
	}
	t.Setenv("GOTMPDIR", "")
	requireRefused("GOTMPDIR empty")
	if err := os.Unsetenv("GOTMPDIR"); err != nil {
		t.Fatal(err)
	}
	requireRefused("GOTMPDIR unset")
	for _, elsewhere := range []string{os.Getenv("TMPDIR"), gotmpdir + "-sibling", filepath.Join(vaultHome, "local")} {
		t.Setenv("GOTMPDIR", elsewhere)
		requireRefused("GOTMPDIR " + elsewhere)
	}
}

// GOTMPDIR admits only what is below it: with it set, a vault outside every
// fixture root is still refused, whether it is named explicitly or derived from
// WARP_HOME.
func TestTestBinaryRefusesANonFixtureVaultWithGOTMPDIRSet(t *testing.T) {
	gotmpdir, _ := gotmpdirVaultFixture(t)
	t.Setenv("GOTMPDIR", gotmpdir)
	outside := filepath.Join(string(filepath.Separator), "nonexistent-urnetwork-deployment")
	requireRefused := func(name string) {
		t.Helper()
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
	if err := refuseNonFixtureVault(filepath.Join(outside, "vault"), true); err == nil {
		t.Fatal("a test binary accepted a vault outside every fixture root")
	}
}

// The explicit opt-in reads every vault as before, whatever GOTMPDIR names.
func TestTestBinaryOptInReadsAVaultWhateverGOTMPDIR(t *testing.T) {
	gotmpdir, vaultHome := gotmpdirVaultFixture(t)
	outside := filepath.Join(string(filepath.Separator), "nonexistent-urnetwork-deployment", "vault")
	t.Setenv(TestRealVaultEnv, "1")
	for _, value := range []string{gotmpdir, "", gotmpdir + "-sibling"} {
		t.Setenv("GOTMPDIR", value)
		for _, root := range []string{vaultHome, outside} {
			if err := refuseNonFixtureVault(root, true); err != nil {
				t.Fatalf("GOTMPDIR %q: the opted-in test refused %s: %v", value, root, err)
			}
		}
		t.Setenv("WARP_VAULT_HOME", vaultHome)
		resource, err := Vault.SimpleResource("fixture.yml")
		if err != nil || resource.RequireString("key") != "value" {
			t.Fatalf("GOTMPDIR %q: opted-in read %v", value, err)
		}
		t.Setenv("WARP_VAULT_HOME", outside)
		if _, err := Vault.SimpleResource("pg.yml"); !errors.Is(err, ErrResourceNotFound) || strings.Contains(err.Error(), TestRealVaultEnv) {
			t.Fatalf("GOTMPDIR %q: opted-in read %v, want the plain lookup of an absent vault", value, err)
		}
	}
}
