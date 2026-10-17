package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Writes a probe that records each service probe instead of connecting.
func writeTestEnvironmentRecordingProbe(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	probePath := filepath.Join(directory, "probe")
	if err := os.WriteFile(
		probePath,
		[]byte("#!/bin/sh\nprintf '%s %s %s\\n' \"$1\" \"$2\" \"$3\" >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	return probePath, filepath.Join(directory, "probe-record")
}

// Writes the launcher's local profile into a vault that stands in for a
// developer's real one, selected through WARP_HOME. Returns WARP_HOME.
func writeTestEnvironmentLauncherWarpHome(t *testing.T) string {
	t.Helper()
	warpHome := t.TempDir()
	writeResolverFixture(t, filepath.Join(warpHome, "vault"), map[string]string{
		"local/pg.yml": "authority: \"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:5432\"\n" +
			"user: launcher_test\npassword: launcher-test-not-a-secret\ndb: launcher_test\n",
		"local/redis.yml": "authority: \"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:6379\"\n" +
			"password: \"\"\ndb: 0\ncluster: false\n",
	})
	return warpHome
}

// In managed-local mode a successful preflight names the vault it checked
// against the launcher, the one the launcher reads its local credentials from,
// for the Go test vault guard (env_test_vault.go), replacing a stale name.
func TestTestEnvironmentScriptNamesTheLauncherVault(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	lockDir, hostsPath := writeTestEnvironmentLauncherState(t, localPostgresHost, "5432", localRedisHost, "6379")
	probePath, probeRecordPath := writeTestEnvironmentRecordingProbe(t)
	resolverPath, _ := writeTestEnvironmentHostResolver(t, map[string][]string{
		localPostgresHost: {localDedicatedAddress},
		localRedisHost:    {localDedicatedAddress},
	})
	warpHome := writeTestEnvironmentLauncherWarpHome(t)
	cmd := exec.Command(
		"bash",
		"-c",
		`export WARP_TEST_ENV_LAUNCHER_VAULT_HOME=/stale-launcher-vault
source ./test-env.sh && printf '%s\n%s\n' "$WARP_VAULT_HOME" "$WARP_TEST_ENV_LAUNCHER_VAULT_HOME"`,
	)
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"WARP_HOME":                             warpHome,
			"WARP_CONFIG_HOME":                      filepath.Join(workingDirectory, "local", "testdata", "config"),
			"WARP_TEST_ENV_TEST_HOSTS_FILE":         hostsPath,
			"WARP_TEST_ENV_HOST_RESOLVER":           resolverPath,
			"WARP_TEST_ENV_PROBE_RECORD":            probeRecordPath,
			"WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR": lockDir,
			"WARP_TEST_ENV_TCP_PROBE":               probePath,
			"BRINGYOUR_POSTGRES_HOSTNAME":           localPostgresHost,
			"BRINGYOUR_REDIS_HOSTNAME":              localRedisHost,
		},
		"WARP_ENV",
		"WARP_VAULT_HOME",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("managed service preflight: %v\n%s", err, output)
	}
	vaultHome := filepath.Join(warpHome, "vault")
	if expected := vaultHome + "\n" + vaultHome + "\n"; string(output) != expected {
		t.Fatalf("selected and launcher vaults = %q; want %q", output, expected)
	}
}

// A refused preflight leaves no launcher vault named, so a stale name from an
// earlier run cannot admit a vault the launcher no longer attests.
func TestTestEnvironmentScriptClearsTheLauncherVaultWhenRefused(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	lockDir, hostsPath := writeTestEnvironmentLauncherState(t, localPostgresHost, "5432", localRedisHost, "6379")
	if err := os.Remove(filepath.Join(lockDir, "ready")); err != nil {
		t.Fatal(err)
	}
	probePath, probeRecordPath := writeTestEnvironmentRecordingProbe(t)
	resolverPath, _ := writeTestEnvironmentHostResolver(t, map[string][]string{
		localPostgresHost: {localDedicatedAddress},
		localRedisHost:    {localDedicatedAddress},
	})
	cmd := exec.Command(
		"bash",
		"-c",
		`export WARP_TEST_ENV_LAUNCHER_VAULT_HOME=/stale-launcher-vault
source ./test-env.sh
status=$?
printf 'launcher_vault=%s\n' "${WARP_TEST_ENV_LAUNCHER_VAULT_HOME-unset}"
exit "$status"`,
	)
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"WARP_HOME":                             writeTestEnvironmentLauncherWarpHome(t),
			"WARP_CONFIG_HOME":                      filepath.Join(workingDirectory, "local", "testdata", "config"),
			"WARP_TEST_ENV_TEST_HOSTS_FILE":         hostsPath,
			"WARP_TEST_ENV_HOST_RESOLVER":           resolverPath,
			"WARP_TEST_ENV_PROBE_RECORD":            probeRecordPath,
			"WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR": lockDir,
			"WARP_TEST_ENV_TCP_PROBE":               probePath,
			"BRINGYOUR_POSTGRES_HOSTNAME":           localPostgresHost,
			"BRINGYOUR_REDIS_HOSTNAME":              localRedisHost,
		},
		"WARP_ENV",
		"WARP_VAULT_HOME",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a preflight without launcher readiness passed: %s", output)
	}
	if !testOutputHasLine(output, "launcher_vault=unset") {
		t.Fatalf("a refused preflight left a launcher vault named: %q", output)
	}
}

// Without a launcher there is no launcher vault: the unmanaged portable
// services and the suite proxy run against roots they name themselves.
func TestTestEnvironmentScriptNamesNoLauncherVaultWithoutALauncher(t *testing.T) {
	probePath, probeRecordPath := writeTestEnvironmentRecordingProbe(t)
	resolverPath, _ := writeTestEnvironmentHostResolver(t, map[string][]string{
		localPostgresHost: {"192.0.2.61"},
		localRedisHost:    {"192.0.2.62"},
	})
	script := `export WARP_TEST_ENV_LAUNCHER_VAULT_HOME=/stale-launcher-vault
source ./test-env.sh || exit $?
printf 'launcher_vault=%s\n' "${WARP_TEST_ENV_LAUNCHER_VAULT_HOME-unset}"`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"WARP_HOME": t.TempDir(),
			"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1",
			"WARP_TEST_ENV_HOST_RESOLVER":                     resolverPath,
			"WARP_TEST_ENV_PROBE_RECORD":                      probeRecordPath,
			"WARP_TEST_ENV_TCP_PROBE":                         probePath,
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES":            "1",
			"BRINGYOUR_POSTGRES_HOSTNAME":                     localPostgresHost,
			"BRINGYOUR_REDIS_HOSTNAME":                        localRedisHost,
		},
		"WARP_ENV",
		"WARP_VAULT_HOME",
		"WARP_CONFIG_HOME",
	)
	output, err := cmd.CombinedOutput()
	if err != nil || !testOutputHasLine(output, "launcher_vault=unset") {
		t.Fatalf("unmanaged portable preflight = %v, %q; want no launcher vault", err, output)
	}

	stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
	vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
		t,
		"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
		"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
	)
	binDir := writeTestEnvironmentSuiteProxyTools(t)
	suiteProbePath, suiteProbeRecordPath := writeTestEnvironmentRecordingProbe(t)
	cmd = exec.Command("bash", "-c", script)
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH":                                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"SUITE_TEST_HOST_IP":                  suiteProxyTestAddress,
			"SUITE_TEST_OWNER_TOKEN":              suiteProxyTestToken,
			"WARP_CONFIG_HOME":                    configDir,
			"WARP_TEST_ENV_PROBE_RECORD":          suiteProbeRecordPath,
			"WARP_TEST_ENV_SUITE_PROXY_STATE_DIR": stateDir,
			"WARP_TEST_ENV_TCP_PROBE":             suiteProbePath,
			"WARP_VAULT_HOME":                     vaultDir,
		},
		"BRINGYOUR_POSTGRES_HOSTNAME",
		"BRINGYOUR_REDIS_HOSTNAME",
		"WARP_ENV",
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err = cmd.CombinedOutput()
	if err != nil || !testOutputHasLine(output, "launcher_vault=unset") {
		t.Fatalf("suite proxy preflight = %v, %q; want no launcher vault", err, output)
	}
}

// Whether output holds line as one complete line.
func testOutputHasLine(output []byte, line string) bool {
	return slices.Contains(strings.Split(string(output), "\n"), line)
}
