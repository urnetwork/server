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

// Builds a child environment without leaking stale test-environment selectors
// into script regressions that need to control each input explicitly.
func testCommandEnvironment(overrideNameValues map[string]string, unsetNames ...string) []string {
	blockedNames := map[string]bool{}
	for _, name := range []string{
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
		"WARP_TEST_ENV_TEST_HOSTS_FILE",
		"WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR",
	} {
		blockedNames[name] = true
	}
	for name := range overrideNameValues {
		blockedNames[name] = true
	}
	for _, name := range unsetNames {
		blockedNames[name] = true
	}
	environment := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !blockedNames[name] {
			environment = append(environment, entry)
		}
	}
	for name, value := range overrideNameValues {
		environment = append(environment, name+"="+value)
	}
	return environment
}

// Writes a complete launcher state under temporary paths so shell-preflight
// tests exercise ownership without reading or changing the host's live state.
func writeTestEnvironmentLauncherState(
	t *testing.T,
	postgresHost string,
	postgresPort string,
	redisHost string,
	redisPort string,
) (string, string) {
	t.Helper()
	stateDir := t.TempDir()
	lockDir := filepath.Join(stateDir, "run-local.lock")
	hostsPath := filepath.Join(stateDir, "hosts")
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "owner"), []byte("test-owner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attestation := "format=urnetwork-server-run-local-ready-v1\n" +
		"owner_token=test-owner\n" +
		"host_ip=" + localDedicatedAddress + "\n" +
		"postgres_host=" + postgresHost + "\n" +
		"postgres_port=" + postgresPort + "\n" +
		"redis_host=" + redisHost + "\n" +
		"redis_port=" + redisPort + "\n"
	if err := os.WriteFile(filepath.Join(lockDir, "ready"), []byte(attestation), 0o600); err != nil {
		t.Fatal(err)
	}
	hosts := "127.0.0.1 localhost\n" + localHostsMarkerBegin + "\n" +
		localDedicatedAddress + " " + postgresHost + "\n" +
		localDedicatedAddress + " " + redisHost + "\n" + localHostsMarkerEnd + "\n"
	if err := os.WriteFile(hostsPath, []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	return lockDir, hostsPath
}

// Test directory discovery must retain local unit and integration packages
// while leaving every acceptance-owned subtree to its separate harness.
func TestLocalTestDirectoryDiscoveryExcludesAcceptance(t *testing.T) {
	output, err := exec.Command("./test-dirs.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("discover local test directories: %v\n%s", err, output)
	}
	directories := strings.Fields(string(output))
	for _, directory := range directories {
		if strings.Contains(strings.ToLower(directory), "acceptance") {
			t.Errorf("local test discovery included acceptance-owned directory %q", directory)
		}
	}
	for _, requiredDirectory := range []string{".", "./connect/perfvar", "./grafana", "./proxy"} {
		if !slices.Contains(directories, requiredDirectory) {
			t.Errorf("local test discovery omitted %q", requiredDirectory)
		}
	}
}

// The discovery entrypoint uses bash available on ordinary release hosts and
// diagnoses its actual utilities before attempting discovery.
func TestLocalTestDirectoryDiscoveryReportsMissingTool(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.Symlink(bashPath, filepath.Join(binDir, "bash")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("./test-dirs.sh")
	cmd.Env = []string{"PATH=" + binDir}
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("discovery without find unexpectedly passed:\n%s", output)
	}
	if !strings.Contains(string(output), "missing prerequisite: find") {
		t.Fatalf("discovery failure did not identify find:\n%s", output)
	}
}

// Every local runner uses the shell its pipeline-status and array syntax were
// written for, so a zsh installation is not an undeclared release dependency.
func TestLocalTestRunnersUseBash(t *testing.T) {
	scriptPaths := []string{
		"test-env.sh",
		"test-dirs.sh",
		"test.sh",
		"connect/test.sh",
		"proxy/test.sh",
		"task/test.sh",
		"connect/sim-latency/tests.sh",
	}
	for _, scriptPath := range scriptPaths {
		content, err := os.ReadFile(scriptPath)
		if err != nil {
			t.Errorf("read %s: %v", scriptPath, err)
			continue
		}
		firstLine, _, _ := strings.Cut(string(content), "\n")
		if firstLine != "#!/usr/bin/env bash" {
			t.Errorf("%s interpreter = %q; want bash", scriptPath, firstLine)
		}
	}
}

// The release preflight falls back to checked-in, non-secret local resources
// when an isolated server checkout has no sibling vault or config repositories.
func TestTestEnvironmentScriptUsesPortableResources(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
	probePath := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(
		probePath,
		[]byte("#!/bin/sh\nprintf '%s %s %s\\n' \"$1\" \"$2\" \"$3\" >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(
		"bash",
		"-c",
		`source ./test-env.sh && printf '%s\n%s\n%s\n' "$WARP_ENV" "$WARP_VAULT_HOME" "$WARP_CONFIG_HOME"`,
	)
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"WARP_HOME": t.TempDir(),
			"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1",
			"WARP_TEST_ENV_PROBE_RECORD":                      probeRecordPath,
			"WARP_TEST_ENV_TCP_PROBE":                         probePath,
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES":            "1",
			"BRINGYOUR_POSTGRES_HOSTNAME":                     "local-pg.bringyour.com",
			"BRINGYOUR_REDIS_HOSTNAME":                        "local-redis.bringyour.com",
		},
		"WARP_ENV",
		"WARP_VAULT_HOME",
		"WARP_CONFIG_HOME",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("portable test environment preflight: %v\n%s", err, output)
	}
	lines := strings.Fields(string(output))
	expectedLines := []string{
		"local",
		filepath.Join(workingDirectory, "local", "testdata", "vault"),
		filepath.Join(workingDirectory, "local", "testdata", "config"),
	}
	if !slices.Equal(lines, expectedLines) {
		t.Fatalf("configured environment = %q; want %q", lines, expectedLines)
	}
	probeRecord, err := os.ReadFile(probeRecordPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedProbeRecord := "postgres local-pg.bringyour.com 5432\nredis local-redis.bringyour.com 6379\n"
	if string(probeRecord) != expectedProbeRecord {
		t.Fatalf("service probes = %q; want %q", probeRecord, expectedProbeRecord)
	}
}

// Legacy aliases and a launcher still starting have no complete readiness
// proof, so both stop before even a synthetic service probe can run.
func TestTestEnvironmentScriptRejectsUnattestedLocalTopology(t *testing.T) {
	tests := []struct {
		name        string
		halfStarted bool
		errorText   string
	}{
		{
			name:        "legacy unmanaged aliases",
			halfStarted: false,
			errorText:   "lock has no readable owner",
		},
		{
			name:        "launcher has not published readiness",
			halfStarted: true,
			errorText:   "readiness attestation is missing",
		},
	}

	for _, test := range tests {
		stateDir := t.TempDir()
		lockDir := filepath.Join(stateDir, "run-local.lock")
		hostsPath := filepath.Join(stateDir, "hosts")
		probeRecordPath := filepath.Join(stateDir, "probe-record")
		probePath := filepath.Join(stateDir, "probe")
		hosts := "10.211.55.3 local-pg.bringyour.com\n" +
			"10.211.55.3 local-redis.bringyour.com\n"
		if test.halfStarted {
			hosts = "127.0.0.1 localhost\n" + localHostsMarkerBegin + "\n" +
				localDedicatedAddress + " " + localPostgresHost + "\n" +
				localDedicatedAddress + " " + localRedisHost + "\n" + localHostsMarkerEnd + "\n"
			if err := os.Mkdir(lockDir, 0o700); err != nil {
				t.Fatalf("%s: create lock: %v", test.name, err)
			}
			if err := os.WriteFile(filepath.Join(lockDir, "owner"), []byte("test-owner\n"), 0o600); err != nil {
				t.Fatalf("%s: write owner: %v", test.name, err)
			}
			if err := os.WriteFile(filepath.Join(lockDir, "ready.pending"), []byte("partial\n"), 0o600); err != nil {
				t.Fatalf("%s: write pending readiness: %v", test.name, err)
			}
		}
		if err := os.WriteFile(hostsPath, []byte(hosts), 0o600); err != nil {
			t.Fatalf("%s: write hosts: %v", test.name, err)
		}
		if err := os.WriteFile(
			probePath,
			[]byte("#!/bin/sh\nprintf 'called\\n' >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"),
			0o700,
		); err != nil {
			t.Fatalf("%s: write probe: %v", test.name, err)
		}

		cmd := exec.Command("bash", "./test-env.sh")
		cmd.Env = testCommandEnvironment(
			map[string]string{
				"WARP_TEST_ENV_TEST_HOSTS_FILE":         hostsPath,
				"WARP_TEST_ENV_PROBE_RECORD":            probeRecordPath,
				"WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR": lockDir,
				"WARP_TEST_ENV_TCP_PROBE":               probePath,
				"WARP_TEST_ENV_USE_PORTABLE_RESOURCES":  "1",
				"BRINGYOUR_POSTGRES_HOSTNAME":           localPostgresHost,
				"BRINGYOUR_REDIS_HOSTNAME":              localRedisHost,
			},
			"WARP_ENV",
			"WARP_VAULT_HOME",
			"WARP_CONFIG_HOME",
		)
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), test.errorText) ||
			!strings.Contains(string(output), "launcher-managed local services are not ready") {
			t.Errorf("%s: preflight = %v, %q; want early ownership failure", test.name, err, output)
		}
		if _, err := os.Stat(probeRecordPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: ownership failure reached service probe: %v", test.name, err)
		}
	}
}

// A complete attestation tied to the live lock, selected endpoints, and sole
// managed aliases admits the existing first-attempt service probes.
func TestTestEnvironmentScriptAcceptsLauncherManagedServices(t *testing.T) {
	lockDir, hostsPath := writeTestEnvironmentLauncherState(
		t,
		localPostgresHost,
		"5432",
		localRedisHost,
		"6379",
	)
	probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
	probePath := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(
		probePath,
		[]byte("#!/bin/sh\nprintf '%s %s %s\\n' \"$1\" \"$2\" \"$3\" >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"WARP_TEST_ENV_TEST_HOSTS_FILE":         hostsPath,
			"WARP_TEST_ENV_PROBE_RECORD":            probeRecordPath,
			"WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR": lockDir,
			"WARP_TEST_ENV_TCP_PROBE":               probePath,
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES":  "1",
			"BRINGYOUR_POSTGRES_HOSTNAME":           localPostgresHost,
			"BRINGYOUR_REDIS_HOSTNAME":              localRedisHost,
		},
		"WARP_ENV",
		"WARP_VAULT_HOME",
		"WARP_CONFIG_HOME",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("managed service preflight: %v\n%s", err, output)
	}
	probeRecord, err := os.ReadFile(probeRecordPath)
	if err != nil {
		t.Fatal(err)
	}
	expected := "postgres " + localPostgresHost + " 5432\n" +
		"redis " + localRedisHost + " 6379\n"
	if string(probeRecord) != expected {
		t.Fatalf("managed service probes = %q; want %q", probeRecord, expected)
	}
}

// Every ownership dimension is part of readiness; a stale token, endpoint, or
// duplicate alias is rejected before reachability can disguise the mismatch.
func TestTestEnvironmentScriptRejectsMismatchedLauncherReadiness(t *testing.T) {
	tests := []struct {
		name      string
		mutation  string
		errorText string
	}{
		{
			name:      "lock token",
			mutation:  "lock",
			errorText: "readiness owner does not match its lock",
		},
		{
			name:      "resource endpoint",
			mutation:  "endpoint",
			errorText: "readiness endpoints do not match",
		},
		{
			name:      "duplicate alias",
			mutation:  "hosts",
			errorText: "aliases are not unique launcher-managed mappings",
		},
	}

	for _, test := range tests {
		lockDir, hostsPath := writeTestEnvironmentLauncherState(
			t,
			localPostgresHost,
			"5432",
			localRedisHost,
			"6379",
		)
		switch test.mutation {
		case "lock":
			if err := os.WriteFile(filepath.Join(lockDir, "owner"), []byte("other-owner\n"), 0o600); err != nil {
				t.Fatalf("%s: change lock owner: %v", test.name, err)
			}
		case "endpoint":
			attestationPath := filepath.Join(lockDir, "ready")
			contents, err := os.ReadFile(attestationPath)
			if err != nil {
				t.Fatalf("%s: read readiness: %v", test.name, err)
			}
			changed := strings.Replace(string(contents), "postgres_port=5432", "postgres_port=15432", 1)
			if err := os.WriteFile(attestationPath, []byte(changed), 0o600); err != nil {
				t.Fatalf("%s: change endpoint: %v", test.name, err)
			}
		case "hosts":
			file, err := os.OpenFile(hostsPath, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("%s: open hosts: %v", test.name, err)
			}
			if _, err := file.WriteString("198.51.100.90 " + localPostgresHost + "\n"); err != nil {
				_ = file.Close()
				t.Fatalf("%s: duplicate alias: %v", test.name, err)
			}
			if err := file.Close(); err != nil {
				t.Fatalf("%s: close hosts: %v", test.name, err)
			}
		default:
			t.Fatalf("%s: unknown mutation %q", test.name, test.mutation)
		}

		probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
		probePath := filepath.Join(t.TempDir(), "probe")
		if err := os.WriteFile(
			probePath,
			[]byte("#!/bin/sh\nprintf 'called\\n' >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"),
			0o700,
		); err != nil {
			t.Fatalf("%s: write probe: %v", test.name, err)
		}
		cmd := exec.Command("bash", "./test-env.sh")
		cmd.Env = testCommandEnvironment(
			map[string]string{
				"WARP_TEST_ENV_TEST_HOSTS_FILE":         hostsPath,
				"WARP_TEST_ENV_PROBE_RECORD":            probeRecordPath,
				"WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR": lockDir,
				"WARP_TEST_ENV_TCP_PROBE":               probePath,
				"WARP_TEST_ENV_USE_PORTABLE_RESOURCES":  "1",
				"BRINGYOUR_POSTGRES_HOSTNAME":           localPostgresHost,
				"BRINGYOUR_REDIS_HOSTNAME":              localRedisHost,
			},
			"WARP_ENV",
			"WARP_VAULT_HOME",
			"WARP_CONFIG_HOME",
		)
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), test.errorText) {
			t.Errorf("%s: preflight = %v, %q; want readiness mismatch", test.name, err, output)
		}
		if _, err := os.Stat(probeRecordPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: readiness mismatch reached service probe: %v", test.name, err)
		}
	}
}

// The unmanaged-service escape is coupled to checked-in portable resources;
// it cannot silently bypass launcher ownership for an ordinary local run.
func TestTestEnvironmentScriptRestrictsUnmanagedServiceEscape(t *testing.T) {
	cmd := exec.Command("bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1",
		},
		"WARP_ENV",
		"WARP_VAULT_HOME",
		"WARP_CONFIG_HOME",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("unmanaged service escape without portable resources passed:\n%s", output)
	}
	if !strings.Contains(
		string(output),
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES=1 requires WARP_TEST_ENV_USE_PORTABLE_RESOURCES=1",
	) {
		t.Fatalf("unmanaged service escape failure was not explicit:\n%s", output)
	}
}

// The shell preflight reads the same authority shapes as the YAML resource
// loader without acquiring a Python/yq dependency. Exercise that observable
// boundary through both services and keep config resources from being mistaken
// for their same-named vault peers.
func TestTestEnvironmentScriptParsesYamlAuthorityScalars(t *testing.T) {
	tests := []struct {
		name             string
		postgresScalar   string
		redisScalar      string
		postgresHostname string
		redisHostname    string
		postgresPort     string
		redisPort        string
	}{
		{
			name:             "plain values with comments",
			postgresScalar:   "{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432 # postgres comment",
			redisScalar:      "{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379 # redis comment",
			postgresHostname: "pg-shell-test.example",
			redisHostname:    "redis-shell-test.example",
			postgresPort:     "15432",
			redisPort:        "16379",
		},
		{
			name:             "single and double quoted values with comments",
			postgresScalar:   "'{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:25432' # postgres comment",
			redisScalar:      "\"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:26379\" # redis comment",
			postgresHostname: "pg-quoted-test.example",
			redisHostname:    "redis-quoted-test.example",
			postgresPort:     "25432",
			redisPort:        "26379",
		},
		{
			name:             "bracketed IPv6 values",
			postgresScalar:   "\"[{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}]:35432\" # postgres comment",
			redisScalar:      "'[{{ env:BRINGYOUR_REDIS_HOSTNAME }}]:36379' # redis comment",
			postgresHostname: "2001:db8:1::10",
			redisHostname:    "2001:db8:2::20",
			postgresPort:     "35432",
			redisPort:        "36379",
		},
	}

	for _, test := range tests {
		vaultDir := t.TempDir()
		configDir := t.TempDir()
		lockDir, hostsPath := writeTestEnvironmentLauncherState(
			t,
			test.postgresHostname,
			test.postgresPort,
			test.redisHostname,
			test.redisPort,
		)
		for path, content := range map[string]string{
			filepath.Join(vaultDir, "pg.yml"):     "authority: " + test.postgresScalar + "\n",
			filepath.Join(vaultDir, "redis.yml"):  "authority: " + test.redisScalar + "\n",
			filepath.Join(configDir, "db.yml"):    "authority: decoy-pg.example:1\n",
			filepath.Join(configDir, "redis.yml"): "authority: decoy-redis.example:2\n",
		} {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("%s: write %s: %v", test.name, filepath.Base(path), err)
			}
		}

		probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
		probePath := filepath.Join(t.TempDir(), "probe")
		if err := os.WriteFile(
			probePath,
			[]byte("#!/bin/sh\nprintf '%s %s %s\\n' \"$1\" \"$2\" \"$3\" >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"),
			0o700,
		); err != nil {
			t.Fatalf("%s: write probe: %v", test.name, err)
		}

		cmd := exec.Command("bash", "./test-env.sh")
		cmd.Env = testCommandEnvironment(
			map[string]string{
				"WARP_ENV":                              "local",
				"WARP_VAULT_HOME":                       vaultDir,
				"WARP_CONFIG_HOME":                      configDir,
				"WARP_TEST_ENV_TEST_HOSTS_FILE":         hostsPath,
				"WARP_TEST_ENV_PROBE_RECORD":            probeRecordPath,
				"WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR": lockDir,
				"WARP_TEST_ENV_TCP_PROBE":               probePath,
				"BRINGYOUR_POSTGRES_HOSTNAME":           test.postgresHostname,
				"BRINGYOUR_REDIS_HOSTNAME":              test.redisHostname,
			},
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s: test environment preflight: %v\n%s", test.name, err, output)
			continue
		}

		probeRecord, err := os.ReadFile(probeRecordPath)
		if err != nil {
			t.Errorf("%s: read probe record: %v", test.name, err)
			continue
		}
		expectedProbeRecord := "postgres " + test.postgresHostname + " " + test.postgresPort + "\n" +
			"redis " + test.redisHostname + " " + test.redisPort + "\n"
		if string(probeRecord) != expectedProbeRecord {
			t.Errorf("%s: service probes = %q; want %q", test.name, probeRecord, expectedProbeRecord)
		}
	}
}

// Explicit resource roots remain fail-closed: a typo is reported rather than
// being replaced with a fixture from some other checkout.
func TestTestEnvironmentScriptReportsMissingFixture(t *testing.T) {
	probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
	cmd := exec.Command("bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"WARP_ENV":                   "local",
			"WARP_VAULT_HOME":            filepath.Join(t.TempDir(), "vault"),
			"WARP_CONFIG_HOME":           filepath.Join(t.TempDir(), "config"),
			"WARP_TEST_ENV_PROBE_RECORD": probeRecordPath,
			"WARP_TEST_ENV_TCP_PROBE":    filepath.Join(t.TempDir(), "unused-probe"),
		},
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("preflight without fixtures unexpectedly passed:\n%s", output)
	}
	if !strings.Contains(string(output), "required resource is missing") ||
		!strings.Contains(string(output), "pg.yml") {
		t.Fatalf("fixture failure did not identify pg.yml:\n%s", output)
	}
	if _, err := os.Stat(probeRecordPath); !os.IsNotExist(err) {
		t.Fatalf("missing fixture reached service probe: %v", err)
	}
}

// A release shell carrying a production selector must be rejected before any
// fixture resolution or service connection can occur.
func TestTestEnvironmentScriptRejectsNonLocalEnvironment(t *testing.T) {
	cmd := exec.Command("bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(map[string]string{"WARP_ENV": "main"})
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("preflight accepted WARP_ENV=main:\n%s", output)
	}
	if !strings.Contains(string(output), "refusing WARP_ENV=main") {
		t.Fatalf("non-local failure was not explicit:\n%s", output)
	}
}

// A missing runner binary is diagnosed before fixtures are probed or an
// expensive test command is launched.
func TestTestEnvironmentScriptReportsMissingGo(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bashPath, "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH":      t.TempDir(),
			"WARP_HOME": t.TempDir(),
		},
		"WARP_ENV",
		"WARP_VAULT_HOME",
		"WARP_CONFIG_HOME",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("preflight without go unexpectedly passed:\n%s", output)
	}
	if !strings.Contains(string(output), "missing prerequisite: go") {
		t.Fatalf("tool failure did not identify go:\n%s", output)
	}
}

// The default service probe must make bounded zero-I/O connections to both
// configured stores without relying on Bash's non-portable /dev/tcp handling.
func TestTestEnvironmentScriptUsesBoundedDefaultTcpProbe(t *testing.T) {
	probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
	binDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(binDir, "nc"),
		[]byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1",
			"WARP_TEST_ENV_PROBE_RECORD":                      probeRecordPath,
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES":            "1",
			"BRINGYOUR_POSTGRES_HOSTNAME":                     localPostgresHost,
			"BRINGYOUR_REDIS_HOSTNAME":                        localRedisHost,
		},
		"WARP_ENV",
		"WARP_VAULT_HOME",
		"WARP_CONFIG_HOME",
		"WARP_TEST_ENV_TCP_PROBE",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("default TCP service preflight: %v\n%s", err, output)
	}
	probeRecord, err := os.ReadFile(probeRecordPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedProbeRecord := "-z -w 3 -- " + localPostgresHost + " 5432\n" +
		"-z -w 3 -- " + localRedisHost + " 6379\n"
	if string(probeRecord) != expectedProbeRecord {
		t.Fatalf("default service probes = %q; want %q", probeRecord, expectedProbeRecord)
	}
}

// A host without the default TCP probe gets an actionable prerequisite error
// before the harness can misreport a configured service as unreachable.
func TestTestEnvironmentScriptReportsMissingDefaultTcpProbe(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	for _, commandName := range []string{"awk", "dirname", "find", "go", "grep", "sort"} {
		commandPath, err := exec.LookPath(commandName)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(commandPath, filepath.Join(binDir, commandName)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bashPath, "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH":                                 binDir,
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1",
		},
		"WARP_ENV",
		"WARP_VAULT_HOME",
		"WARP_CONFIG_HOME",
		"WARP_TEST_ENV_TCP_PROBE",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("preflight without nc unexpectedly passed:\n%s", output)
	}
	if !strings.Contains(string(output), "missing prerequisite: nc") {
		t.Fatalf("TCP probe failure did not identify nc:\n%s", output)
	}
}

// The local service launcher diagnoses its bounded-probe dependency before it
// asks for privileges or makes any host, kernel, or Docker mutation.
func TestRunLocalReportsMissingTcpProbe(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	for _, commandName := range []string{"dirname", "head", "sed", "uname"} {
		commandPath, err := exec.LookPath(commandName)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(commandPath, filepath.Join(binDir, commandName)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bashPath, "./local/run-local.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH":                                 binDir,
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1",
		},
		"WARP_ENV",
		"WARP_VAULT_HOME",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("local launcher without nc unexpectedly passed:\n%s", output)
	}
	if !strings.Contains(string(output), "nc not found on PATH") {
		t.Fatalf("local launcher failure did not identify nc:\n%s", output)
	}
}
