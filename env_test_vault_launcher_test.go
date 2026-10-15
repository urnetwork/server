package server

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Synthetic launcher profile values; none is a live identity or secret.
const (
	testLauncherPostgresHost = "pg.launcher.example"
	testLauncherRedisHost    = "redis.launcher.example"
	testLauncherPassword     = "launcher-test-not-a-secret"
	testLauncherOwnerToken   = "v2:4242:0123456789abcdef0123456789abcdef"
	testLauncherMainPassword = "stand-in-for-a-main-secret"
)

// The launcher's readiness record for the synthetic profile, with the named
// fields replaced.
func testLauncherReadiness(replacedFieldNameValues map[string]string) string {
	record := "format=urnetwork-server-run-local-ready-v1\n"
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "owner_token", value: testLauncherOwnerToken},
		{name: "host_ip", value: "192.0.2.10"},
		{name: "postgres_host", value: testLauncherPostgresHost},
		{name: "postgres_port", value: "15432"},
		{name: "redis_host", value: testLauncherRedisHost},
		{name: "redis_port", value: "16379"},
	} {
		value, ok := replacedFieldNameValues[field.name]
		if !ok {
			value = field.value
		}
		record += field.name + "=" + value + "\n"
	}
	return record
}

// The launcher vault's local directory with its symlinks resolved, as the
// launcher scope reads it (Darwin's temp directory is below the /var link).
func testLauncherLocalDir(t *testing.T, vaultHome string) string {
	t.Helper()
	localDir, err := filepath.EvalSymlinks(filepath.Join(vaultHome, "local"))
	if err != nil {
		t.Fatal(err)
	}
	return localDir
}

// The endpoints that test-env.sh would attest for the synthetic launcher: its
// dedicated address with each service's port.
const testLauncherAttestedAddrs = "192.0.2.10:15432,192.0.2.10:16379"

// A launcher's local profile in a vault outside every fixture root, beside
// stand-ins for the live secrets such a vault also holds, and the launcher's
// lock with its owner and readiness. Selects both with the environment that
// test-env.sh exports in managed-local mode, the lock through its test-only
// local-state paths, and installs the dial guard that a process started from
// that environment would have chosen. Returns the vault and the lock directory.
func writeTestLauncherVault(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", "")
	t.Setenv(TestRealVaultEnv, "")
	t.Setenv("GOTMPDIR", "")
	t.Setenv("BRINGYOUR_POSTGRES_HOSTNAME", testLauncherPostgresHost)
	t.Setenv("BRINGYOUR_REDIS_HOSTNAME", testLauncherRedisHost)
	vaultHome := filepath.Join(t.TempDir(), "vault")
	writeResolverFixture(t, vaultHome, map[string]string{
		"local/pg.yml": "authority: \"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432\"\n" +
			"user: launcher_test\npassword: " + testLauncherPassword + "\ndb: launcher_test\n",
		"local/redis.yml": "authority: \"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379\"\n" +
			"password: \"\"\ndb: 0\ncluster: false\n",
		// stand-ins for what a test must never read from such a vault
		"local/stripe.yml": "secret_key: stand-in-for-a-live-secret\n",
		"pg.yml":           "authority: pg.root.example:5432\nuser: root\npassword: stand-in-root\ndb: root\n",
		"all/client.yml":   "client_ip_hash_pepper: stand-in-for-a-live-pepper\n",
		"main/pg.yml": "authority: pg.main.example:5432\n" +
			"user: main\npassword: " + testLauncherMainPassword + "\ndb: main\n",
	})
	lockDir := filepath.Join(t.TempDir(), "run-local.lock")
	writeResolverFixture(t, lockDir, map[string]string{
		"owner": testLauncherOwnerToken + "\n",
		"ready": testLauncherReadiness(nil),
	})
	t.Setenv("WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR", lockDir)
	t.Setenv("WARP_TEST_ENV_TEST_HOSTS_FILE", filepath.Join(lockDir, "unused-hosts"))
	t.Setenv("WARP_TEST_ENV_TCP_PROBE", filepath.Join(lockDir, "unused-probe"))
	t.Setenv("WARP_VAULT_HOME", vaultHome)
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(syntheticTestResourceRoot, "config"))
	t.Setenv("WARP_TEST_ENV_LAUNCHER_VAULT_HOME", vaultHome)
	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvManagedEnvironment(testLauncherAttestedAddrs), testEnvUnusedResolver(t)))
	// the vault is no longer below the temp directory, so it is not a fixture
	t.Setenv("TMPDIR", t.TempDir())
	if err := refuseNonFixtureVault(vaultHome, true); err == nil {
		t.Fatal("the launcher vault is still a fixture")
	}
	return vaultHome, lockDir
}

// A DB test on a machine whose launcher runs with a real vault's local
// credentials loads its PostgreSQL and Redis settings from that vault's local
// directory, once test-env.sh has named the vault and while the launcher's
// attestation names those endpoints. Before, every lookup in a vault outside
// the fixture roots was refused, so no DB test could reach the launcher's
// services. Every other resource comes from the checked-in fixture, and
// nothing else of the launcher's vault is read.
func TestTestBinaryLoadsDbSettingsFromTheAttestedLauncherVault(t *testing.T) {
	vaultHome, _ := writeTestLauncherVault(t)

	configuration, err := loadTestEnvironmentConfiguration()
	if err != nil {
		t.Fatalf("a DB test could not load the launcher's settings: %v", err)
	}
	if configuration.postgresAuthority != testLauncherPostgresHost+":15432" ||
		configuration.postgresUser != "launcher_test" ||
		configuration.postgresPassword != testLauncherPassword ||
		configuration.postgresDatabase != "launcher_test" ||
		configuration.redisAuthority != testLauncherRedisHost+":16379" {
		t.Fatalf(
			"the DB settings are not the launcher's: postgres %s user %s db %s, redis %s",
			configuration.postgresAuthority,
			configuration.postgresUser,
			configuration.postgresDatabase,
			configuration.redisAuthority,
		)
	}
	for _, name := range []string{"pg.yml", "redis.yml"} {
		path, err := Vault.ResourcePath(name)
		if err != nil || path != filepath.Join(testLauncherLocalDir(t, vaultHome), name) {
			t.Fatalf("%s resolved to %q, %v; want the launcher's local file", name, path, err)
		}
	}
	// the launcher's local directory has no maintenance resource, and no
	// PostgreSQL resource comes from the fixture
	if _, err := Vault.SimpleResource(MaintenancePgVaultResourceName); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("%s read %v; want not found", MaintenancePgVaultResourceName, err)
	}
	fixtureVault := filepath.Join(syntheticTestResourceRoot, "vault")
	for _, name := range []string{"client.yml", "password.yml", "jwt.yml"} {
		path, err := Vault.ResourcePath(name)
		if err != nil || !pathWithin(path, fixtureVault) {
			t.Fatalf("%s resolved to %q, %v; want the checked-in fixture's", name, path, err)
		}
	}
	if _, err := Vault.SimpleResource("stripe.yml"); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("stripe.yml read %v; want not found, since only the launcher's resources are read from its vault", err)
	}
}

// The launcher's resources are read only while its readiness record is
// complete and names its lock's v2 owner, a dedicated address and the endpoint
// that the resource names. Each defect refuses the resources it affects, and
// no refusal names a credential.
func TestTestBinaryRefusesLauncherResourcesWithoutAValidAttestation(t *testing.T) {
	writeFile := func(t *testing.T, path string, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name         string
		mutate       func(t *testing.T, lockDir string)
		refusedNames []string
		errorText    string
	}{
		{
			name: "no launcher lock",
			mutate: func(t *testing.T, lockDir string) {
				t.Setenv("WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR", filepath.Join(lockDir, "absent"))
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "has no readable owner",
		},
		{
			name: "withdrawn readiness",
			mutate: func(t *testing.T, lockDir string) {
				if err := os.Remove(filepath.Join(lockDir, "ready")); err != nil {
					t.Fatal(err)
				}
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "has published no readiness",
		},
		{
			name: "another owner holds the lock",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "owner"), "v2:4243:fedcba9876543210fedcba9876543210\n")
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "does not name its lock's owner",
		},
		{
			name: "owner without the live-process challenge",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "owner"), "test-owner\n")
				writeFile(t, filepath.Join(lockDir, "ready"), testLauncherReadiness(map[string]string{"owner_token": "test-owner"}))
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "has no v2 owner",
		},
		{
			name: "two owners",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "owner"), testLauncherOwnerToken+"\n"+testLauncherOwnerToken+"\n")
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "has no v2 owner",
		},
		{
			name: "truncated record",
			mutate: func(t *testing.T, lockDir string) {
				record := testLauncherReadiness(nil)
				writeFile(t, filepath.Join(lockDir, "ready"), record[:strings.LastIndex(record, "redis_port=")])
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "is malformed",
		},
		{
			name: "another record format",
			mutate: func(t *testing.T, lockDir string) {
				record := strings.Replace(testLauncherReadiness(nil), "-ready-v1", "-ready-v0", 1)
				writeFile(t, filepath.Join(lockDir, "ready"), record)
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "is malformed",
		},
		{
			name: "empty value",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "ready"), testLauncherReadiness(map[string]string{"postgres_host": ""}))
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "is malformed",
		},
		{
			name: "value with a space",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "ready"), testLauncherReadiness(map[string]string{"redis_host": testLauncherRedisHost + " other.example"}))
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "is malformed",
		},
		{
			name: "loopback address",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "ready"), testLauncherReadiness(map[string]string{"host_ip": "127.0.0.1"}))
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "not a dedicated non-loopback address",
		},
		{
			name: "mapped loopback address",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "ready"), testLauncherReadiness(map[string]string{"host_ip": "::ffff:127.0.0.1"}))
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "not a dedicated non-loopback address",
		},
		{
			name: "unspecified address",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "ready"), testLauncherReadiness(map[string]string{"host_ip": "0.0.0.0"}))
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "not a dedicated non-loopback address",
		},
		{
			name: "mapped unspecified address",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "ready"), testLauncherReadiness(map[string]string{"host_ip": "::ffff:0.0.0.0"}))
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "not a dedicated non-loopback address",
		},
		{
			name: "invalid port",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "ready"), testLauncherReadiness(map[string]string{"postgres_port": "015432"}))
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "has an invalid postgres_port",
		},
		{
			name: "another PostgreSQL endpoint",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "ready"), testLauncherReadiness(map[string]string{"postgres_port": "25432"}))
				// the dial guard attests the moved port too, so only the file's
				// authority differs
				useTestEnvProcessDialGuard(t, newTestEnvDialGuard(
					testEnvManagedEnvironment("192.0.2.10:25432,192.0.2.10:16379"), testEnvUnusedResolver(t)))
			},
			refusedNames: []string{"pg.yml"},
			errorText:    "does not name the attested endpoint " + testLauncherPostgresHost + ":25432",
		},
		{
			name: "another Redis endpoint",
			mutate: func(t *testing.T, lockDir string) {
				writeFile(t, filepath.Join(lockDir, "ready"), testLauncherReadiness(map[string]string{"redis_host": "redis.other.example"}))
			},
			refusedNames: []string{"redis.yml"},
			errorText:    "does not name the attested endpoint redis.other.example:16379",
		},
		{
			name: "incomplete test-only lock paths",
			mutate: func(t *testing.T, lockDir string) {
				t.Setenv("WARP_TEST_ENV_TCP_PROBE", "")
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "test-only local-state paths require each other",
		},
	}
	for _, c := range cases {
		_, lockDir := writeTestLauncherVault(t)
		c.mutate(t, lockDir)
		for _, name := range []string{"pg.yml", "redis.yml"} {
			_, err := Vault.SimpleResource(name)
			if !slices.Contains(c.refusedNames, name) {
				if err != nil {
					t.Errorf("%s: %s read %v; want the launcher's", c.name, name, err)
				}
				continue
			}
			if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), c.errorText) {
				t.Errorf("%s: %s read %v; want a refusal naming %q", c.name, name, err, c.errorText)
			} else if strings.Contains(err.Error(), testLauncherPassword) || strings.Contains(err.Error(), testLauncherOwnerToken) {
				t.Errorf("%s: the %s refusal names a credential or the owner token", c.name, name)
			}
		}
		if _, err := loadTestEnvironmentConfiguration(); err == nil {
			t.Errorf("%s: a DB test loaded the launcher's settings", c.name)
		}
	}
}

// The launcher's credentials load only into a test process whose dial guard,
// chosen at startup, attests the launcher's address with both of its ports.
// Setting WARP_ENV=local and the launcher vault by hand, or after startup,
// leaves a process without a guard, whose pools dial by name and can fall
// through public DNS to 127.0.0.1, where a production data tunnel listens. That
// process, one whose guard has no attestation, and one whose guard attests
// other endpoints read none of the launcher's credentials.
func TestTestBinaryReadsLauncherResourcesOnlyUnderItsDialGuard(t *testing.T) {
	cases := []struct {
		name      string
		guard     func(t *testing.T) *testEnvDialGuard
		errorText string
	}{
		{
			name: "no dial guard",
			guard: func(t *testing.T) *testEnvDialGuard {
				return nil
			},
			errorText: "only in a process whose dial guard attests the launcher's endpoints",
		},
		{
			name: "a guard without an attestation",
			guard: func(t *testing.T) *testEnvDialGuard {
				return newTestEnvDialGuard(testEnvManagedEnvironment(""), testEnvUnusedResolver(t))
			},
			errorText: "only in a process whose dial guard attests the launcher's endpoints",
		},
		{
			name: "a guard for another address",
			guard: func(t *testing.T) *testEnvDialGuard {
				return newTestEnvDialGuard(testEnvManagedEnvironment("192.0.2.11:15432,192.0.2.11:16379"), testEnvUnusedResolver(t))
			},
			errorText: "attests the launcher's 192.0.2.10:15432",
		},
		{
			name: "a guard for the PostgreSQL endpoint only",
			guard: func(t *testing.T) *testEnvDialGuard {
				return newTestEnvDialGuard(testEnvManagedEnvironment("192.0.2.10:15432"), testEnvUnusedResolver(t))
			},
			errorText: "attests the launcher's 192.0.2.10:16379",
		},
	}
	for _, c := range cases {
		writeTestLauncherVault(t)
		useTestEnvProcessDialGuard(t, c.guard(t))
		for _, name := range []string{"pg.yml", "redis.yml"} {
			_, err := Vault.SimpleResource(name)
			if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), c.errorText) {
				t.Errorf("%s: %s read %v; want a refusal naming %q", c.name, name, err, c.errorText)
			}
		}
		if _, err := loadTestEnvironmentConfiguration(); err == nil {
			t.Errorf("%s: a DB test loaded the launcher's settings", c.name)
		}
	}
}

// Nothing of the launcher's vault is read outside the managed-local profile:
// not with another WARP_ENV such as main, not when the vault home is another
// directory than the one test-env.sh named, such as the vault's main directory,
// and not when its local resource names an endpoint the launcher did not
// attest, as main's credentials do.
func TestTestBinaryReadsNoMainOrNonLocalResourcesThroughTheLauncher(t *testing.T) {
	cases := []struct {
		name                  string
		setup                 func(t *testing.T, vaultHome string)
		refusedNameErrorTexts map[string]string
	}{
		{
			name: "WARP_ENV main",
			setup: func(t *testing.T, vaultHome string) {
				t.Setenv("WARP_ENV", "main")
			},
			refusedNameErrorTexts: map[string]string{
				"pg.yml":     "only with WARP_ENV=local",
				"redis.yml":  "only with WARP_ENV=local",
				"client.yml": "only with WARP_ENV=local",
			},
		},
		{
			name: "vault home is the main directory",
			setup: func(t *testing.T, vaultHome string) {
				t.Setenv("WARP_VAULT_HOME", filepath.Join(vaultHome, "main"))
			},
			refusedNameErrorTexts: map[string]string{
				"pg.yml":     TestRealVaultEnv,
				"redis.yml":  TestRealVaultEnv,
				"client.yml": TestRealVaultEnv,
			},
		},
		{
			name: "the launcher vault is another directory",
			setup: func(t *testing.T, vaultHome string) {
				t.Setenv("WARP_TEST_ENV_LAUNCHER_VAULT_HOME", filepath.Join(filepath.Dir(vaultHome), "other-vault"))
			},
			refusedNameErrorTexts: map[string]string{
				"pg.yml":     TestRealVaultEnv,
				"redis.yml":  TestRealVaultEnv,
				"client.yml": TestRealVaultEnv,
			},
		},
		{
			name: "a relative launcher vault",
			setup: func(t *testing.T, vaultHome string) {
				t.Setenv("WARP_TEST_ENV_LAUNCHER_VAULT_HOME", filepath.Base(vaultHome))
			},
			refusedNameErrorTexts: map[string]string{
				"pg.yml":     TestRealVaultEnv,
				"redis.yml":  TestRealVaultEnv,
				"client.yml": TestRealVaultEnv,
			},
		},
		{
			name: "no launcher vault, as in a plain go test with WARP_HOME exported",
			setup: func(t *testing.T, vaultHome string) {
				t.Setenv("WARP_TEST_ENV_LAUNCHER_VAULT_HOME", "")
				t.Setenv("WARP_VAULT_HOME", "")
				t.Setenv("WARP_HOME", filepath.Dir(vaultHome))
			},
			refusedNameErrorTexts: map[string]string{
				"pg.yml":     TestRealVaultEnv,
				"redis.yml":  TestRealVaultEnv,
				"client.yml": TestRealVaultEnv,
			},
		},
		{
			name: "the local credentials name main's endpoint",
			setup: func(t *testing.T, vaultHome string) {
				pg := "authority: pg.main.example:5432\nuser: main\npassword: " + testLauncherMainPassword + "\ndb: main\n"
				if err := os.WriteFile(filepath.Join(vaultHome, "local", "pg.yml"), []byte(pg), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			refusedNameErrorTexts: map[string]string{
				"pg.yml": "does not name the attested endpoint " + testLauncherPostgresHost + ":15432",
			},
		},
		{
			name: "the local directory is main's",
			setup: func(t *testing.T, vaultHome string) {
				if err := os.Rename(filepath.Join(vaultHome, "local"), filepath.Join(vaultHome, "local-original")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("main", filepath.Join(vaultHome, "local")); err != nil {
					t.Fatal(err)
				}
			},
			refusedNameErrorTexts: map[string]string{
				"pg.yml":    "only from a real local directory",
				"redis.yml": "only from a real local directory",
			},
		},
	}
	for _, c := range cases {
		vaultHome, _ := writeTestLauncherVault(t)
		c.setup(t, vaultHome)
		for _, name := range []string{"pg.yml", "redis.yml", "client.yml"} {
			resource, err := Vault.SimpleResource(name)
			errorText, refused := c.refusedNameErrorTexts[name]
			if !refused {
				if err != nil {
					t.Errorf("%s: %s read %v; want it read", c.name, name, err)
				} else if strings.Contains(string(resource.Bytes()), testLauncherMainPassword) {
					t.Errorf("%s: %s read main's credentials", c.name, name)
				}
				continue
			}
			if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), errorText) {
				t.Errorf("%s: %s read %v; want a refusal naming %q", c.name, name, err, errorText)
			} else if strings.Contains(err.Error(), testLauncherMainPassword) || strings.Contains(err.Error(), "pg.main.example") {
				t.Errorf("%s: the %s refusal repeats a value of main's file", c.name, name)
			}
		}
	}
}

// The resolution of a launcher resource is cached like any other, but the
// attestation and the selected file's endpoint are checked at every lookup:
// withdrawing readiness refuses the next lookup of the cached resolution,
// publishing it again restores it, and a file rewritten to name another
// endpoint is refused.
func TestTestBinaryChecksTheLauncherAttestationAtEveryLookup(t *testing.T) {
	vaultHome, lockDir := writeTestLauncherVault(t)
	resolver := newSettledResolver(MOUNT_TYPE_VAULT)
	requireRead := func(phase string) {
		t.Helper()
		paths, err := resolver.ResourcePaths("pg.yml")
		if err != nil || len(paths) == 0 || paths[0] != filepath.Join(testLauncherLocalDir(t, vaultHome), "pg.yml") {
			t.Fatalf("%s: pg.yml resolved to %q, %v; want the launcher's", phase, paths, err)
		}
	}
	requireRefused := func(phase string, errorText string) {
		t.Helper()
		_, err := resolver.ResourcePaths("pg.yml")
		if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), errorText) {
			t.Fatalf("%s: pg.yml read %v; want a refusal naming %q", phase, err, errorText)
		}
	}

	requireRead("published")
	version := resolver.resourceVersion("pg.yml")
	if resolver.resourceVersion("pg.yml") != version {
		t.Fatal("the launcher's resolution was not cached")
	}

	readyPath := filepath.Join(lockDir, "ready")
	ready, err := os.ReadFile(readyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(readyPath); err != nil {
		t.Fatal(err)
	}
	requireRefused("withdrawn", "has published no readiness")
	if err := os.WriteFile(readyPath, ready, 0o600); err != nil {
		t.Fatal(err)
	}
	requireRead("published again")

	pg := "authority: pg.main.example:5432\nuser: main\npassword: " + testLauncherMainPassword + "\ndb: main\n"
	if err := os.WriteFile(filepath.Join(vaultHome, "local", "pg.yml"), []byte(pg), 0o600); err != nil {
		t.Fatal(err)
	}
	requireRefused("rewritten", "does not name the attested endpoint")
}

// A lookup in the launcher's vault reads only files that resolve inside its
// real local directory, and checks that before reading any of them. A
// credential linked to a file elsewhere, and a local directory that is itself a
// link, are refused even when what they lead to names the attested endpoints.
func TestTestBinaryReadsNoLauncherResourceLinkedOutOfItsLocalDirectory(t *testing.T) {
	elsewhereNameContents := map[string]string{
		"pg.yml": "authority: \"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432\"\n" +
			"user: elsewhere\npassword: " + testLauncherMainPassword + "\ndb: elsewhere\n",
		"redis.yml": "authority: \"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379\"\n" +
			"password: " + testLauncherMainPassword + "\ndb: 0\ncluster: false\n",
	}
	cases := []struct {
		name         string
		setup        func(t *testing.T, vaultHome string, elsewhere string)
		refusedNames []string
		errorText    string
	}{
		{
			name: "a credential linked out of the local directory",
			setup: func(t *testing.T, vaultHome string, elsewhere string) {
				local := filepath.Join(vaultHome, "local", "pg.yml")
				if err := os.Remove(local); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(elsewhere, "pg.yml"), local); err != nil {
					t.Fatal(err)
				}
			},
			refusedNames: []string{"pg.yml"},
			errorText:    "leads outside it",
		},
		{
			name: "a local directory that is a link",
			setup: func(t *testing.T, vaultHome string, elsewhere string) {
				if err := os.Rename(filepath.Join(vaultHome, "local"), filepath.Join(vaultHome, "local-original")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, filepath.Join(vaultHome, "local")); err != nil {
					t.Fatal(err)
				}
			},
			refusedNames: []string{"pg.yml", "redis.yml"},
			errorText:    "only from a real local directory",
		},
	}
	for _, c := range cases {
		vaultHome, _ := writeTestLauncherVault(t)
		// not below the temp directory, the vault's local directory or any
		// other fixture root
		elsewhere := filepath.Join(t.TempDir(), "elsewhere")
		writeResolverFixture(t, elsewhere, elsewhereNameContents)
		c.setup(t, vaultHome, elsewhere)
		for _, name := range c.refusedNames {
			resource, err := Vault.SimpleResource(name)
			if err == nil && strings.Contains(string(resource.Bytes()), testLauncherMainPassword) {
				t.Errorf("%s: %s read the credentials it links to", c.name, name)
			}
			if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), c.errorText) {
				t.Errorf("%s: %s read %v; want a refusal naming %q", c.name, name, err, c.errorText)
			}
		}
		if _, err := loadTestEnvironmentConfiguration(); err == nil {
			t.Errorf("%s: a DB test loaded the linked settings", c.name)
		}
	}
}

// A fixture vault reads only files that resolve inside it. A test's own vault
// below the temp directory whose credential links to a file outside every
// fixture root, such as a real vault's main/pg.yml, is refused, not read, and
// the vault's own files are still read.
func TestTestBinaryReadsNoFixtureFileLinkedOutOfItsVault(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_TEST_ENV_PORTABLE_ROOT", "")
	t.Setenv(TestRealVaultEnv, "")
	t.Setenv("GOTMPDIR", "")
	t.Setenv("WARP_TEST_ENV_LAUNCHER_VAULT_HOME", "")
	secretVault := filepath.Join(t.TempDir(), "secret-vault")
	writeResolverFixture(t, secretVault, map[string]string{
		"main/pg.yml": "authority: pg.main.example:5432\nuser: main\npassword: " + testLauncherMainPassword + "\ndb: main\n",
	})
	// the secret vault is no longer below the temp directory
	t.Setenv("TMPDIR", t.TempDir())
	if err := refuseNonFixtureVault(secretVault, true); err == nil {
		t.Fatal("the secret vault is still a fixture")
	}
	fixtureVault := filepath.Join(os.Getenv("TMPDIR"), "vault")
	writeResolverFixture(t, fixtureVault, map[string]string{"local/placeholder.yml": "key: value\n"})
	if err := os.Symlink(filepath.Join(secretVault, "main", "pg.yml"), filepath.Join(fixtureVault, "local", "pg.yml")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_VAULT_HOME", fixtureVault)

	resource, err := Vault.SimpleResource("pg.yml")
	if err == nil && strings.Contains(string(resource.Bytes()), testLauncherMainPassword) {
		t.Fatal("the fixture vault's linked pg.yml read the secret vault's credentials")
	}
	if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), "leads outside it") {
		t.Fatalf("the linked pg.yml read %v; want a refusal naming %q", err, "leads outside it")
	}
	if resource, err := Vault.SimpleResource("placeholder.yml"); err != nil || resource.RequireString("key") != "value" {
		t.Fatalf("the fixture vault's own placeholder.yml read %v", err)
	}
}

// The explicit opt-in still reads the vault as before, its root first,
// whatever test-env.sh named, and a fixture vault, such as one a test writes,
// is read as before without any launcher attestation.
func TestTestBinaryLauncherVaultLeavesTheOptInAndFixturesAsBefore(t *testing.T) {
	vaultHome, lockDir := writeTestLauncherVault(t)
	t.Setenv(TestRealVaultEnv, "1")
	if path, err := Vault.ResourcePath("pg.yml"); err != nil || path != filepath.Join(vaultHome, "pg.yml") {
		t.Fatalf("opted-in pg.yml resolved to %q, %v; want the vault's own", path, err)
	}
	t.Setenv(TestRealVaultEnv, "")

	if err := os.Remove(filepath.Join(lockDir, "ready")); err != nil {
		t.Fatal(err)
	}
	fixtureVault := filepath.Join(os.Getenv("TMPDIR"), "vault")
	writeResolverFixture(t, fixtureVault, map[string]string{
		"local/pg.yml": "authority: postgres.fixture.example:5432\nuser: fixture\npassword: fixture\ndb: fixture\n",
	})
	t.Setenv("WARP_VAULT_HOME", fixtureVault)
	if path, err := Vault.ResourcePath("pg.yml"); err != nil || path != filepath.Join(fixtureVault, "local", "pg.yml") {
		t.Fatalf("fixture pg.yml resolved to %q, %v; want the fixture's own", path, err)
	}
}
