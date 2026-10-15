// Shell regressions for local/local-env.sh, which attests a local process that
// is not a test to the running launcher, and for the run-local.sh entry points
// that source it. Like the test-env.sh cases in test_harness_test.go, every run
// uses temporary launcher state, a scripted resolver, a recording TCP probe and
// a recording go command, so nothing reads or changes the host's launcher lock,
// hosts file, resolver or services, and no process is started.
package server

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The local entry points that are not tests and the identity each exports.
var localRunScriptIdentities = []struct {
	path    string
	service string
	block   string
	args    []string
}{
	{path: "cli/api/run-local.sh", service: "api", block: "local"},
	{path: "cli/connect/run-local.sh", service: "connect", block: "local"},
	{path: "cli/gossip/run-local.sh", service: "gossip", block: "local"},
	{path: "cli/mcp/run-local.sh", service: "api", block: "local"},
	{path: "cli/proxy/run-local.sh", service: "proxy", block: "local"},
	{path: "cli/taskworker/run-local.sh", service: "taskworker", block: "local"},
	{path: "bringyourctl/run-local.sh", service: "bringyourctl", block: "local", args: []string{"db-version"}},
	{path: "connect/sim-latency/run-local.sh", service: "sim", block: "sim", args: []string{"reset"}},
}

// Records the endpoints a preflight probes, one "service host port" line each.
func writeLocalEnvironmentRecordingProbe(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	recordPath := filepath.Join(directory, "probe-record")
	probePath := filepath.Join(directory, "probe")
	if err := os.WriteFile(
		probePath,
		[]byte("#!/bin/sh\nprintf '%s %s %s\\n' \"$1\" \"$2\" \"$3\" >> '"+recordPath+"'\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	return probePath, recordPath
}

// Reads a record file, or nil when nothing was ever recorded.
func readLocalEnvironmentRecord(t *testing.T, recordPath string) []string {
	t.Helper()
	record, err := os.ReadFile(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(record), "\n"), "\n")
}

// Points local-env.sh at temporary launcher state and synthetic probes.
func localEnvironmentScriptOverrides(lockDir string, hostsPath string, resolverPath string, probePath string) map[string]string {
	return map[string]string{
		"WARP_TEST_ENV_TEST_HOSTS_FILE":         hostsPath,
		"WARP_TEST_ENV_HOST_RESOLVER":           resolverPath,
		"WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR": lockDir,
		"WARP_TEST_ENV_TCP_PROBE":               probePath,
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES":  "1",
		"BRINGYOUR_POSTGRES_HOSTNAME":           localPostgresHost,
		"BRINGYOUR_REDIS_HOSTNAME":              localRedisHost,
	}
}

// Runs a bash script in the server directory with the given overrides and
// without any inherited process identity or resource selection.
func runLocalEnvironmentScript(t *testing.T, script string, overrides map[string]string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = testCommandEnvironment(
		overrides,
		"WARP_ENV", "WARP_SERVICE", "WARP_BLOCK", "WARP_VERSION", "WARP_DOMAIN", "WARP_VAULT_HOME", "WARP_CONFIG_HOME",
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// A service started through local-env.sh gets the endpoints the launcher
// attests, after the same launcher, resolution and probe checks test-env.sh
// runs, and keeps its own identity: the test identity test-env.sh exports does
// not leak into it.
func TestLocalEnvironmentScriptAttestsServiceProcessToLauncher(t *testing.T) {
	lockDir, hostsPath := writeTestEnvironmentLauncherState(t, localPostgresHost, "5432", localRedisHost, "6379")
	probePath, probeRecordPath := writeLocalEnvironmentRecordingProbe(t)
	resolverPath, resolverRecordPath := writeTestEnvironmentHostResolver(t, map[string][]string{
		localPostgresHost: {localDedicatedAddress},
		localRedisHost:    {localDedicatedAddress},
	})
	output, err := runLocalEnvironmentScript(t, `export WARP_ENV=local WARP_SERVICE=api WARP_BLOCK=local WARP_VERSION=0.0.0-local
source ./local/local-env.sh || exit
printf '%s\n' "$WARP_TEST_ENV_ATTESTED_ADDRS" "$WARP_ENV" "$WARP_SERVICE" "$WARP_BLOCK" "$WARP_VERSION" "${WARP_DOMAIN-unset}"`,
		localEnvironmentScriptOverrides(lockDir, hostsPath, resolverPath, probePath),
	)
	if err != nil {
		t.Fatalf("local attestation: %v\n%s", err, output)
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	expected := []string{
		localDedicatedAddress + ":5432," + localDedicatedAddress + ":6379",
		"local",
		"api",
		"local",
		"0.0.0-local",
		"unset",
	}
	if !slices.Equal(lines, expected) {
		t.Fatalf("attested environment = %q; want %q", lines, expected)
	}
	expectedProbes := []string{"postgres " + localDedicatedAddress + " 5432", "redis " + localDedicatedAddress + " 6379"}
	if probes := readLocalEnvironmentRecord(t, probeRecordPath); !slices.Equal(probes, expectedProbes) {
		t.Fatalf("service probes = %q; want %q", probes, expectedProbes)
	}
	if queried := readTestEnvironmentResolverRecord(t, resolverRecordPath); !slices.Equal(queried, []string{localPostgresHost, localRedisHost}) {
		t.Fatalf("resolved names = %q; want both managed names checked once", queried)
	}
}

// With the launcher down there is nothing to attest: local-env.sh fails, clears
// a stale attestation, restores the caller's identity and tells the user to
// start the launcher. The same holds when the launcher looks up but a service
// name falls through to loopback, where a production data tunnel can listen.
func TestLocalEnvironmentScriptRefusesWithoutLauncherOrOnLoopbackFallThrough(t *testing.T) {
	cases := []struct {
		name      string
		launcher  bool
		answers   map[string][]string
		errorText string
	}{
		{
			name:      "launcher down",
			answers:   map[string][]string{localPostgresHost: {"127.0.0.1"}, localRedisHost: {"127.0.0.1"}},
			errorText: "launcher-managed local services are not ready",
		},
		{
			name:      "postgres name falls through to loopback",
			launcher:  true,
			answers:   map[string][]string{localPostgresHost: {"127.0.0.1"}, localRedisHost: {localDedicatedAddress}},
			errorText: "postgres host " + localPostgresHost + " (port 5432) resolves to loopback 127.0.0.1",
		},
		{
			name:      "redis name falls through to loopback",
			launcher:  true,
			answers:   map[string][]string{localPostgresHost: {localDedicatedAddress}, localRedisHost: {"::1"}},
			errorText: "redis host " + localRedisHost + " (port 6379) resolves to loopback ::1",
		},
	}
	for _, c := range cases {
		lockDir := filepath.Join(t.TempDir(), "run-local.lock")
		hostsPath := filepath.Join(t.TempDir(), "hosts")
		if err := os.WriteFile(hostsPath, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if c.launcher {
			lockDir, hostsPath = writeTestEnvironmentLauncherState(t, localPostgresHost, "5432", localRedisHost, "6379")
		}
		probePath, probeRecordPath := writeLocalEnvironmentRecordingProbe(t)
		resolverPath, _ := writeTestEnvironmentHostResolver(t, c.answers)
		output, err := runLocalEnvironmentScript(t, `export WARP_ENV=local WARP_SERVICE=api WARP_BLOCK=local WARP_VERSION=0.0.0-local
export WARP_TEST_ENV_ATTESTED_ADDRS=192.0.2.99:1
source ./local/local-env.sh
status=$?
printf 'status=%s attestation=%s identity=%s/%s/%s/%s\n' "$status" "${WARP_TEST_ENV_ATTESTED_ADDRS-unset}" \
    "$WARP_SERVICE" "$WARP_BLOCK" "$WARP_VERSION" "${WARP_DOMAIN-unset}"`,
			localEnvironmentScriptOverrides(lockDir, hostsPath, resolverPath, probePath),
		)
		if err != nil {
			t.Errorf("%s: script: %v\n%s", c.name, err, output)
			continue
		}
		if !strings.Contains(output, c.errorText) || !strings.Contains(output, "start ./local/run-local.sh") {
			t.Errorf("%s: output %q; want %q and the launcher instruction", c.name, output, c.errorText)
		}
		if !strings.Contains(output, "status=1 attestation=unset identity=api/local/0.0.0-local/unset") {
			t.Errorf("%s: output %q; want a failure, no attestation and the caller's identity", c.name, output)
		}
		if probes := readLocalEnvironmentRecord(t, probeRecordPath); probes != nil {
			t.Errorf("%s: a refused attestation probed %q", c.name, probes)
		}
	}
}

// Exports from an executed script cannot reach the caller, so local-env.sh
// refuses to run unless it is sourced.
func TestLocalEnvironmentScriptMustBeSourced(t *testing.T) {
	output, err := runLocalEnvironmentScript(t, `bash ./local/local-env.sh`, map[string]string{})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(output, "must be sourced") {
		t.Fatalf("executed local-env.sh = %v, %q; want exit 2 asking to source it", err, output)
	}
}

// Writes a go command that records the attestation and identity it was started
// with, in place of compiling and running the entry point.
func writeLocalRunRecordingGo(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	recordPath := filepath.Join(directory, "go-record")
	script := "#!/bin/sh\nprintf '%s|%s|%s|%s|%s\\n' \"${WARP_TEST_ENV_ATTESTED_ADDRS-unset}\" \"$WARP_ENV\" " +
		"\"$WARP_SERVICE\" \"$WARP_BLOCK\" \"$*\" >> '" + recordPath + "'\n"
	if err := os.WriteFile(filepath.Join(directory, "go"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return directory, recordPath
}

// Every local entry point that is not a test starts its process only after
// local-env.sh attested the launcher, and the process inherits that attestation
// with the entry point's own identity.
func TestLocalRunScriptsStartProcessesWithLauncherAttestation(t *testing.T) {
	for _, script := range localRunScriptIdentities {
		lockDir, hostsPath := writeTestEnvironmentLauncherState(t, localPostgresHost, "5432", localRedisHost, "6379")
		probePath, _ := writeLocalEnvironmentRecordingProbe(t)
		resolverPath, _ := writeTestEnvironmentHostResolver(t, map[string][]string{
			localPostgresHost: {localDedicatedAddress},
			localRedisHost:    {localDedicatedAddress},
		})
		goDirectory, goRecordPath := writeLocalRunRecordingGo(t)
		overrides := localEnvironmentScriptOverrides(lockDir, hostsPath, resolverPath, probePath)
		overrides["PATH"] = goDirectory + string(os.PathListSeparator) + os.Getenv("PATH")
		cmd := exec.Command("bash", append([]string{filepath.Base(script.path)}, script.args...)...)
		cmd.Dir = filepath.Dir(script.path)
		cmd.Env = testCommandEnvironment(overrides, "WARP_ENV", "WARP_SERVICE", "WARP_BLOCK", "WARP_VERSION", "WARP_DOMAIN", "WARP_VAULT_HOME", "WARP_CONFIG_HOME")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s", script.path, err, output)
			continue
		}
		expected := []string{
			localDedicatedAddress + ":5432," + localDedicatedAddress + ":6379|local|" + script.service + "|" + script.block + "|" +
				strings.Join(append([]string{"run", "."}, script.args...), " "),
		}
		if started := readLocalEnvironmentRecord(t, goRecordPath); !slices.Equal(started, expected) {
			t.Errorf("%s: started %q; want %q", script.path, started, expected)
		}
	}
}

// With the launcher down, no entry point starts its process: each exits after
// local-env.sh refuses, so nothing can resolve a local name to loopback.
// sim-latency's file-only commands still run, without an attestation.
func TestLocalRunScriptsRefuseToStartWithoutLauncher(t *testing.T) {
	for _, script := range localRunScriptIdentities {
		hostsPath := filepath.Join(t.TempDir(), "hosts")
		if err := os.WriteFile(hostsPath, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		probePath, probeRecordPath := writeLocalEnvironmentRecordingProbe(t)
		resolverPath, _ := writeTestEnvironmentHostResolver(t, map[string][]string{
			localPostgresHost: {"127.0.0.1"},
			localRedisHost:    {"127.0.0.1"},
		})
		goDirectory, goRecordPath := writeLocalRunRecordingGo(t)
		overrides := localEnvironmentScriptOverrides(filepath.Join(t.TempDir(), "run-local.lock"), hostsPath, resolverPath, probePath)
		overrides["PATH"] = goDirectory + string(os.PathListSeparator) + os.Getenv("PATH")
		run := func(args []string) (string, error) {
			cmd := exec.Command("bash", append([]string{filepath.Base(script.path)}, args...)...)
			cmd.Dir = filepath.Dir(script.path)
			cmd.Env = testCommandEnvironment(overrides, "WARP_ENV", "WARP_SERVICE", "WARP_BLOCK", "WARP_VERSION", "WARP_DOMAIN", "WARP_VAULT_HOME", "WARP_CONFIG_HOME")
			output, err := cmd.CombinedOutput()
			return string(output), err
		}
		output, err := run(script.args)
		if err == nil || !strings.Contains(output, "start ./local/run-local.sh") {
			t.Errorf("%s without a launcher = %v, %q; want a refusal", script.path, err, output)
		}
		if started := readLocalEnvironmentRecord(t, goRecordPath); started != nil {
			t.Errorf("%s without a launcher started %q", script.path, started)
		}
		if probes := readLocalEnvironmentRecord(t, probeRecordPath); probes != nil {
			t.Errorf("%s without a launcher probed %q", script.path, probes)
		}
		if script.service != "sim" {
			continue
		}
		if output, err := run([]string{"init", "--out", "providers.yml"}); err != nil {
			t.Errorf("%s init without a launcher: %v\n%s", script.path, err, output)
		}
		if started := readLocalEnvironmentRecord(t, goRecordPath); !slices.Equal(started, []string{"unset|local|sim|sim|run . init --out providers.yml"}) {
			t.Errorf("%s init started %q; want the file-only command without an attestation", script.path, started)
		}
	}
}

// sim-latency's wrapper attests the launcher only for the commands that reach
// PostgreSQL and Redis. Without a launcher those refuse before starting the
// simulator, while the file-only commands, baseline with --runs included,
// start without an attestation.
func TestSimLatencyRunLocalAttestsOnlyDatabaseCommands(t *testing.T) {
	hostsPath := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hostsPath, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probePath, probeRecordPath := writeLocalEnvironmentRecordingProbe(t)
	resolverPath, _ := writeTestEnvironmentHostResolver(t, map[string][]string{
		localPostgresHost: {"127.0.0.1"},
		localRedisHost:    {"127.0.0.1"},
	})
	overrides := localEnvironmentScriptOverrides(filepath.Join(t.TempDir(), "run-local.lock"), hostsPath, resolverPath, probePath)
	run := func(args []string) ([]string, string, error) {
		goDirectory, goRecordPath := writeLocalRunRecordingGo(t)
		overrides["PATH"] = goDirectory + string(os.PathListSeparator) + os.Getenv("PATH")
		cmd := exec.Command("bash", append([]string{"run-local.sh"}, args...)...)
		cmd.Dir = filepath.Join("connect", "sim-latency")
		cmd.Env = testCommandEnvironment(overrides, "WARP_ENV", "WARP_SERVICE", "WARP_BLOCK", "WARP_VERSION", "WARP_DOMAIN", "WARP_VAULT_HOME", "WARP_CONFIG_HOME")
		output, err := cmd.CombinedOutput()
		return readLocalEnvironmentRecord(t, goRecordPath), string(output), err
	}
	for _, args := range [][]string{
		{"run", "--epoch", "0"},
		{"fleet", "--epoch", "0"},
		{"reset"},
		{"baseline", "--epoch", "0"},
		{"baseline", "--epoch", "0", "--replicates", "2"},
	} {
		started, output, err := run(args)
		if err == nil || started != nil || !strings.Contains(output, "start ./local/run-local.sh") {
			t.Errorf("%q without a launcher = %v, started %q, %q; want a refusal before the simulator starts", args, err, started, output)
		}
	}
	for _, args := range [][]string{
		{"init", "--out", "providers.yml"},
		{"analyze", "--run", "results.csv"},
		{"baseline", "--epoch", "0", "--runs=a.csv,b.csv"},
		{"baseline", "--epoch", "0", "--runs", "a.csv,b.csv"},
	} {
		started, output, err := run(args)
		expected := []string{"unset|local|sim|sim|" + strings.Join(append([]string{"run", "."}, args...), " ")}
		if err != nil || !slices.Equal(started, expected) {
			t.Errorf("%q without a launcher = %v, started %q, %q; want the file-only command started without an attestation", args, err, started, output)
		}
	}
	if probes := readLocalEnvironmentRecord(t, probeRecordPath); probes != nil {
		t.Errorf("a command without a launcher probed %q", probes)
	}
}
