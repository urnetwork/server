package server

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const testServerRunnerChildMode = "URNETWORK_TEST_SERVER_RUNNER_CHILD_MODE"

// These lists pin the checked-in manifest to the resources used by the local
// full-suite environment rather than deriving the fixture from its subject.
var (
	suiteProxyTestVaultResourceNames = []string{
		"auth.yml",
		"brevo.yml",
		"circle.yml",
		"client.yml",
		"coinbase.yml",
		"helius.yml",
		"ipinfo.yml",
		"jwt.yml",
		"jwt-local-evaluator.pem",
		"password.yml",
		"pg.yml",
		"proxy.yml",
		"redis.yml",
		"services.yml",
		"st.yml",
		"stripe.yml",
		"wireguard.yml",
		"x402.yml",
	}
	suiteProxyTestVaultTreeNames = []string{
		"tls",
	}
	suiteProxyTestLocalConfigResourceNames = []string{
		"brevo.yml",
		"db.yml",
		"email.yml",
		"redis.yml",
		"settings.yml",
		"subsidy.yml",
		"tls.yml",
	}
	suiteProxyTestAllConfigResourceNames = []string{
		"apple_roots.pem",
		"city-list.yml",
		"iso-country-list.yml",
		"pro.yml",
	}
)

// Builds a child environment without leaking stale test-environment selectors
// into script regressions that need to control each input explicitly.
func testCommandEnvironment(overrideNameValues map[string]string, unsetNames ...string) []string {
	blockedNames := map[string]bool{}
	for _, name := range []string{
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
		"WARP_TEST_ENV_SUITE_PROXY_STATE_DIR",
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

// Writes the fixed owner/readiness layout consumed by suite-mode preflight.
func writeTestEnvironmentSuiteProxyState(
	t *testing.T,
	host string,
	postgresPort string,
	redisPort string,
) string {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "suite-proxy.state")
	readyDir := filepath.Join(stateDir, "ready")
	if err := os.MkdirAll(filepath.Join(readyDir, "published"), 0o700); err != nil {
		t.Fatal(err)
	}
	ownerPid := os.Getpid()
	owner := "format=urnetwork-server-suite-proxy-owner-v1\n" +
		"owner_pid=" + strconv.Itoa(ownerPid) + "\n" +
		"owner_start_identity=" + suiteProxyTestStart + "\n" +
		"owner_token=" + suiteProxyTestToken + "\n" +
		"generation=" + suiteProxyTestGeneration + "\n"
	if err := os.WriteFile(filepath.Join(stateDir, "owner"), []byte(owner), 0o600); err != nil {
		t.Fatal(err)
	}
	attestation := "format=urnetwork-server-suite-proxy-ready-v1\n" +
		"owner_pid=" + strconv.Itoa(ownerPid) + "\n" +
		"owner_start_identity=" + suiteProxyTestStart + "\n" +
		"owner_token=" + suiteProxyTestToken + "\n" +
		"generation=" + suiteProxyTestGeneration + "\n" +
		"host_ip=" + host + "\n" +
		"postgres_host=" + host + "\n" +
		"postgres_port=" + postgresPort + "\n" +
		"redis_host=" + host + "\n" +
		"redis_port=" + redisPort + "\n" +
		"postgres_upstream_id=" + strings.Repeat("a", 64) + "\n" +
		"redis_upstream_id=" + strings.Repeat("b", 64) + "\n" +
		"proxy_image_id=sha256:" + strings.Repeat("c", 64) + "\n" +
		"postgres_proxy_name=urnetwork-suite-proxy-pg\n" +
		"postgres_proxy_id=" + strings.Repeat("d", 64) + "\n" +
		"redis_proxy_name=urnetwork-suite-proxy-redis\n" +
		"redis_proxy_id=" + strings.Repeat("e", 64) + "\n"
	if err := os.WriteFile(filepath.Join(readyDir, "record"), []byte(attestation), 0o600); err != nil {
		t.Fatal(err)
	}
	return stateDir
}

// Writes the explicit root/local/all resource boundary required before
// destructive full-suite database tests begin.
func writeTestEnvironmentSuiteProxyResources(
	t *testing.T,
	postgresAuthority string,
	redisAuthority string,
) (string, string) {
	t.Helper()
	vaultDir := t.TempDir()
	configDir := t.TempDir()
	vaultLocalDir := filepath.Join(vaultDir, "local")
	vaultAllDir := filepath.Join(vaultDir, "all")
	configLocalDir := filepath.Join(configDir, "local")
	configAllDir := filepath.Join(configDir, "all")
	for _, path := range []string{vaultLocalDir, vaultAllDir, configLocalDir, configAllDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, resourceName := range suiteProxyTestVaultTreeNames {
		resourceTreeDir := filepath.Join(vaultAllDir, resourceName, "2026.9.2")
		for _, hostName := range []string{
			"ur.network",
			"bringyour.com",
			"main-connect.ur.network",
			"main-connect.bringyour.com",
		} {
			hostDir := filepath.Join(resourceTreeDir, hostName)
			if err := os.MkdirAll(hostDir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, extension := range []string{"crt", "key"} {
				resourcePath := filepath.Join(hostDir, hostName+"."+extension)
				if err := os.WriteFile(resourcePath, []byte("synthetic\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, resourceName := range suiteProxyTestVaultResourceNames {
		content := "{}\n"
		resourceDir := vaultLocalDir
		switch resourceName {
		case "auth.yml":
			resourceDir = vaultDir
		case "password.yml":
			content = "password:\n  pepper: test-suite-pepper\n"
		case "pg.yml":
			content = "authority: " + postgresAuthority + "\n"
		case "redis.yml":
			content = "authority: " + redisAuthority + "\n"
		}
		if err := os.WriteFile(filepath.Join(resourceDir, resourceName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, resourceName := range suiteProxyTestLocalConfigResourceNames {
		if err := os.WriteFile(filepath.Join(configLocalDir, resourceName), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, resourceName := range suiteProxyTestAllConfigResourceNames {
		if err := os.WriteFile(filepath.Join(configAllDir, resourceName), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return vaultDir, configDir
}

// Provides deterministic, read-only process and assigned-address observations.
func writeTestEnvironmentSuiteProxyTools(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	tools := map[string]string{
		"ip":       "#!/bin/sh\nexit 1\n",
		"ifconfig": "#!/bin/sh\nprintf 'en0: flags=8863<UP>\\n\\tinet %s netmask 0xffffff00\\n' \"$SUITE_TEST_HOST_IP\"\n",
		"ps": `#!/bin/sh
case " $* " in
  *" lstart= "*) printf 'Fri Sep 4 12:34:56 2026\n' ;;
  *" command= "*) printf 'bash suite-owner --urnetwork-suite-proxy-owner-token=%s\n' "$SUITE_TEST_OWNER_TOKEN" ;;
  *) exit 1 ;;
esac
`,
	}
	for name, content := range tools {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return binDir
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
// while leaving every acceptance-owned subtree and inert external fixture out.
func TestLocalTestDirectoryDiscoveryPreservesPackageBoundaries(t *testing.T) {
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
	if slices.Contains(directories, "./monitor/testdata") {
		t.Error("local test discovery included the inert external PromQL engine fixture")
	}
	for _, requiredDirectory := range []string{
		".",
		"./connect/perfvar",
		"./connect/sim-latency/evaluator/container/testdata/resource-bomb",
		"./grafana",
		"./proxy",
	} {
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
		"local/run-local-state.sh",
		"local/run-suite-proxy.sh",
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

// Builds an isolated copy of the root runner with four synthetic package tiers
// and injected go/grep commands; no repository test or service preflight runs.
func writeTestServerRunnerFixture(t *testing.T, grepScript string) (string, string, string) {
	t.Helper()
	workspaceRoot := t.TempDir()
	serverDir := filepath.Join(workspaceRoot, "server")
	binDir := filepath.Join(workspaceRoot, "bin")
	for _, directory := range []string{
		serverDir,
		binDir,
		filepath.Join(workspaceRoot, "tests"),
		filepath.Join(serverDir, "proxy"),
		filepath.Join(serverDir, "connect", "perfvar"),
		filepath.Join(serverDir, "fixture"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runnerBytes, err := os.ReadFile("test.sh")
	if err != nil {
		t.Fatal(err)
	}
	runnerPath := filepath.Join(serverDir, "test.sh")
	if err := os.WriteFile(runnerPath, runnerBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(serverDir, "test-env.sh"):                                  "#!/usr/bin/env bash\nreturn 0\n",
		filepath.Join(serverDir, "test-dirs.sh"):                                 "#!/usr/bin/env bash\nprintf './proxy\\n./connect/perfvar\\n./fixture\\n'\n",
		filepath.Join(workspaceRoot, "tests", "network-intensive-suite-lock.sh"): "#!/bin/sh\n[ \"$1\" = --verify-held ] && [ \"$2\" = run-all ]\n",
		filepath.Join(binDir, "go"): `#!/bin/sh
"$TEST_SERVER_RUNNER_BINARY" -test.run="^${TEST_SERVER_RUNNER_TEST_NAME}$" -test.count=1
test_status=$?
if [ -n "${TEST_SERVER_RUNNER_UPSTREAM_STATUS:-}" ]; then
  printf '%s\n' "$test_status" > "$TEST_SERVER_RUNNER_UPSTREAM_STATUS"
fi
exit "$test_status"
`,
		filepath.Join(binDir, "grep"): grepScript,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return runnerPath, binDir, workspaceRoot
}

// Binary-looking test output remains text throughout all four runner tiers, so
// grep cannot close successfully before the test process finishes writing.
func TestServerTestScriptTreatsBinaryOutputAsText(t *testing.T) {
	if os.Getenv(testServerRunnerChildMode) == "binary" {
		payload := []byte("test-runner-binary-prefix\nbinary:\x00record\n")
		payload = append(payload, bytes.Repeat([]byte("test-runner-binary-padding\n"), 32*1024)...)
		payload = append(payload, []byte("test-runner-binary-suffix\n")...)
		writtenByteCount, err := os.Stdout.Write(payload)
		if err != nil || writtenByteCount != len(payload) {
			os.Exit(141)
		}
		os.Exit(0)
	}

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	grepScript := `#!/bin/sh
text_mode=0
for argument do
  if [ "$argument" = --binary-files=text ] || [ "$argument" = -a ]; then text_mode=1; fi
done
if [ "$text_mode" != 1 ]; then exit 0; fi
exec "$TEST_SERVER_RUNNER_CAT"`
	runnerPath, binDir, workspaceRoot := writeTestServerRunnerFixture(t, grepScript)
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", runnerPath)
	cmd.Env = testCommandEnvironment(map[string]string{
		"PATH":                             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TEST_SERVER_RUNNER_BINARY":        testBinary,
		"TEST_SERVER_RUNNER_CAT":           catPath,
		"TEST_SERVER_RUNNER_TEST_NAME":     "TestServerTestScriptTreatsBinaryOutputAsText",
		"URNETWORK_NETWORK_TEST_LOCK_HELD": "1",
		"URNETWORK_ROOT":                   workspaceRoot,
		testServerRunnerChildMode:          "binary",
	})
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runner rejected binary test output: %v\n%s", err, output)
	}
	for _, signature := range [][]byte{
		[]byte("test-runner-binary-prefix\n"),
		[]byte{0},
		[]byte("test-runner-binary-suffix\n"),
	} {
		if count := bytes.Count(output, signature); count != 4 {
			t.Fatalf("binary runner signature %q count = %d; want 4", signature, count)
		}
	}
	if bytes.Contains(output, []byte("--- FAIL")) || bytes.Contains(output, []byte("[flaky]")) {
		t.Fatal("captured binary fixture leaked a synthetic failure signature")
	}
}

// A signaled filter closes the pipe and gives the upstream test process exit
// 141. The runner must preserve the filter's causal status instead.
func TestServerTestScriptReportsSignaledOutputFilter(t *testing.T) {
	if os.Getenv(testServerRunnerChildMode) == "filter-signal" {
		chunk := bytes.Repeat([]byte("test-runner-filter-padding\n"), 1024)
		for i := 0; i < 1024; i++ {
			writtenByteCount, err := os.Stdout.Write(chunk)
			if err != nil || writtenByteCount != len(chunk) {
				os.Exit(141)
			}
		}
		os.Exit(70)
	}

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	upstreamStatusPath := filepath.Join(t.TempDir(), "upstream-status")
	runnerPath, binDir, workspaceRoot := writeTestServerRunnerFixture(t, "#!/bin/sh\nkill -TERM \"$$\"\n")
	cmd := exec.Command("bash", runnerPath)
	cmd.Env = testCommandEnvironment(map[string]string{
		"PATH":                               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TEST_SERVER_RUNNER_BINARY":          testBinary,
		"TEST_SERVER_RUNNER_TEST_NAME":       "TestServerTestScriptReportsSignaledOutputFilter",
		"TEST_SERVER_RUNNER_UPSTREAM_STATUS": upstreamStatusPath,
		"URNETWORK_NETWORK_TEST_LOCK_HELD":   "1",
		"URNETWORK_ROOT":                     workspaceRoot,
		testServerRunnerChildMode:            "filter-signal",
	})
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 143 {
		t.Fatalf("runner filter failure = %v, %q; want exit 143", err, output)
	}
	upstreamStatusBytes, readErr := os.ReadFile(upstreamStatusPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(upstreamStatusBytes) != "141\n" {
		t.Fatalf("upstream status = %q; want 141", upstreamStatusBytes)
	}
	if count := strings.Count(string(output), "test output filter failed with status 143 (upstream test status 141)"); count != 1 {
		t.Fatalf("filter diagnostic count = %d; want 1", count)
	}
	if bytes.Contains(output, []byte("--- FAIL")) || bytes.Contains(output, []byte("[flaky]")) {
		t.Fatal("captured filter fixture leaked a synthetic failure signature")
	}
}

// The manifest is an independently pinned description of the exact local
// vault/config boundary, including resources resolved from config/all.
func TestSuiteProxyResourceManifestMatchesFullLocalSuiteBoundary(t *testing.T) {
	manifestBytes, err := os.ReadFile(filepath.Join("local", "suite-resource-manifest.txt"))
	if err != nil {
		t.Fatal(err)
	}
	expectedLines := []string{"format=urnetwork-server-suite-resources-v1"}
	for _, resourceName := range suiteProxyTestVaultResourceNames {
		expectedLines = append(expectedLines, "vault="+resourceName)
	}
	for _, resourceName := range suiteProxyTestVaultTreeNames {
		expectedLines = append(expectedLines, "vault_tree="+resourceName)
	}
	configResourceNames := append([]string{}, suiteProxyTestLocalConfigResourceNames...)
	configResourceNames = append(configResourceNames, suiteProxyTestAllConfigResourceNames...)
	slices.Sort(configResourceNames)
	for _, resourceName := range configResourceNames {
		expectedLines = append(expectedLines, "config="+resourceName)
	}
	expectedManifest := strings.Join(expectedLines, "\n") + "\n"
	if string(manifestBytes) != expectedManifest {
		t.Fatalf("suite resource manifest = %q; want %q", manifestBytes, expectedManifest)
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

// Suite-proxy mode consumes only the repository owner's stable direct-endpoint
// attestation and explicit resource roots, then retains the bounded probes.
func TestTestEnvironmentScriptAcceptsSuiteProxyState(t *testing.T) {
	stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
	vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
		t,
		"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
		"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
	)
	binDir := writeTestEnvironmentSuiteProxyTools(t)
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
		`source ./test-env.sh && printf '%s\n%s\n%s\n' "$BRINGYOUR_POSTGRES_HOSTNAME" "$BRINGYOUR_REDIS_HOSTNAME" "$TEST_ENV_SUITE_PROXY_MODE"`,
	)
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH":                                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"SUITE_TEST_HOST_IP":                  suiteProxyTestAddress,
			"SUITE_TEST_OWNER_TOKEN":              suiteProxyTestToken,
			"WARP_CONFIG_HOME":                    configDir,
			"WARP_TEST_ENV_PROBE_RECORD":          probeRecordPath,
			"WARP_TEST_ENV_SUITE_PROXY_STATE_DIR": stateDir,
			"WARP_TEST_ENV_TCP_PROBE":             probePath,
			"WARP_VAULT_HOME":                     vaultDir,
		},
		"BRINGYOUR_POSTGRES_HOSTNAME",
		"BRINGYOUR_REDIS_HOSTNAME",
		"WARP_ENV",
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("suite proxy test environment: %v\n%s", err, output)
	}
	expectedOutput := suiteProxyTestAddress + "\n" + suiteProxyTestAddress + "\n1\n"
	if string(output) != expectedOutput {
		t.Fatalf("suite proxy exports = %q; want %q", output, expectedOutput)
	}
	probeRecord, err := os.ReadFile(probeRecordPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedProbes := "postgres " + suiteProxyTestAddress + " 15432\n" +
		"redis " + suiteProxyTestAddress + " 16379\n"
	if string(probeRecord) != expectedProbes {
		t.Fatalf("suite proxy probes = %q; want %q", probeRecord, expectedProbes)
	}
}

// Managed-local test state and both portable escape selectors cannot be mixed
// with the separately owned suite proxy topology.
func TestTestEnvironmentScriptKeepsSuiteProxyModeExclusive(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		errorText string
	}{
		{
			name: "portable resources",
			overrides: map[string]string{
				"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1",
			},
			errorText: "mutually exclusive with portable or unmanaged services",
		},
		{
			name: "unmanaged services",
			overrides: map[string]string{
				"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1",
				"WARP_TEST_ENV_USE_PORTABLE_RESOURCES":            "1",
			},
			errorText: "mutually exclusive with portable or unmanaged services",
		},
		{
			name: "managed local state",
			overrides: map[string]string{
				"WARP_TEST_ENV_TCP_PROBE":               filepath.Join(t.TempDir(), "probe"),
				"WARP_TEST_ENV_TEST_HOSTS_FILE":         filepath.Join(t.TempDir(), "hosts"),
				"WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR": filepath.Join(t.TempDir(), "lock"),
			},
			errorText: "mutually exclusive with managed-local state paths",
		},
	}

	for _, test := range tests {
		overrides := map[string]string{
			"WARP_CONFIG_HOME":                    t.TempDir(),
			"WARP_TEST_ENV_SUITE_PROXY_STATE_DIR": filepath.Join(t.TempDir(), "state"),
			"WARP_VAULT_HOME":                     t.TempDir(),
		}
		for name, value := range test.overrides {
			overrides[name] = value
		}
		cmd := exec.Command("bash", "./test-env.sh")
		cmd.Env = testCommandEnvironment(overrides, "WARP_ENV")
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), test.errorText) {
			t.Fatalf("%s mixed suite proxy mode = %v, %q; want %q", test.name, err, output, test.errorText)
		}
	}
}

// Suite mode never guesses resource repositories: both absolute roots must be
// explicit and contain the complete vault/config resource set.
func TestTestEnvironmentScriptRequiresExplicitSuiteProxyResources(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	tests := []struct {
		name      string
		overrides map[string]string
		errorText string
	}{
		{
			name: "missing config root",
			overrides: map[string]string{
				"WARP_VAULT_HOME": t.TempDir(),
			},
			errorText: "requires explicit WARP_VAULT_HOME and WARP_CONFIG_HOME",
		},
		{
			name: "relative root",
			overrides: map[string]string{
				"WARP_CONFIG_HOME": "relative/config",
				"WARP_VAULT_HOME":  t.TempDir(),
			},
			errorText: "resource roots must be absolute paths",
		},
	}
	for _, test := range tests {
		test.overrides["WARP_TEST_ENV_SUITE_PROXY_STATE_DIR"] = stateDir
		cmd := exec.Command("bash", "./test-env.sh")
		cmd.Env = testCommandEnvironment(
			test.overrides,
			"WARP_CONFIG_HOME",
			"WARP_ENV",
			"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
			"WARP_VAULT_HOME",
		)
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), test.errorText) {
			t.Fatalf("%s suite resource roots = %v, %q; want %q", test.name, err, output, test.errorText)
		}
	}
}

// Password hashing is part of the ordinary full suite, so an otherwise valid
// explicit suite vault missing password.yml fails before its first DB probe.
func TestTestEnvironmentScriptRejectsSuiteProxyVaultWithoutPassword(t *testing.T) {
	stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
	vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
		t,
		"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
		"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
	)
	if err := os.Remove(filepath.Join(vaultDir, "local", "password.yml")); err != nil {
		t.Fatal(err)
	}
	binDir := writeTestEnvironmentSuiteProxyTools(t)
	probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
	probePath := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probePath, []byte("#!/bin/sh\nprintf called >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH":                                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"SUITE_TEST_HOST_IP":                  suiteProxyTestAddress,
			"SUITE_TEST_OWNER_TOKEN":              suiteProxyTestToken,
			"WARP_CONFIG_HOME":                    configDir,
			"WARP_TEST_ENV_PROBE_RECORD":          probeRecordPath,
			"WARP_TEST_ENV_SUITE_PROXY_STATE_DIR": stateDir,
			"WARP_TEST_ENV_TCP_PROBE":             probePath,
			"WARP_VAULT_HOME":                     vaultDir,
		},
		"BRINGYOUR_POSTGRES_HOSTNAME",
		"BRINGYOUR_REDIS_HOSTNAME",
		"WARP_ENV",
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "password.yml") ||
		!strings.Contains(string(output), "required resource is missing") {
		t.Fatalf("missing suite password resource = %v, %q", err, output)
	}
	if _, err := os.Stat(probeRecordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing password resource reached probe: %v", err)
	}
}

// Config/all data is part of the same checked-in boundary as credentials; a
// missing non-secret manifest entry also stops before the first service probe.
func TestTestEnvironmentScriptRejectsSuiteProxyConfigManifestGap(t *testing.T) {
	stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
	vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
		t,
		"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
		"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
	)
	if err := os.Remove(filepath.Join(configDir, "all", "apple_roots.pem")); err != nil {
		t.Fatal(err)
	}
	binDir := writeTestEnvironmentSuiteProxyTools(t)
	probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
	probePath := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probePath, []byte("#!/bin/sh\nprintf called >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH":                                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"SUITE_TEST_HOST_IP":                  suiteProxyTestAddress,
			"SUITE_TEST_OWNER_TOKEN":              suiteProxyTestToken,
			"WARP_CONFIG_HOME":                    configDir,
			"WARP_TEST_ENV_PROBE_RECORD":          probeRecordPath,
			"WARP_TEST_ENV_SUITE_PROXY_STATE_DIR": stateDir,
			"WARP_TEST_ENV_TCP_PROBE":             probePath,
			"WARP_VAULT_HOME":                     vaultDir,
		},
		"BRINGYOUR_POSTGRES_HOSTNAME",
		"BRINGYOUR_REDIS_HOSTNAME",
		"WARP_ENV",
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "apple_roots.pem") ||
		!strings.Contains(string(output), "required resource is missing") {
		t.Fatalf("missing suite config resource = %v, %q", err, output)
	}
	if _, err := os.Stat(probeRecordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing config resource reached probe: %v", err)
	}
}

// Runs suite preflight with an inert probe and requires TLS validation to stop
// before that probe can observe either service endpoint.
func requireTestEnvironmentSuiteProxyTlsRejection(
	t *testing.T,
	stateDir string,
	vaultDir string,
	configDir string,
	description string,
) {
	t.Helper()
	binDir := writeTestEnvironmentSuiteProxyTools(t)
	probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
	probePath := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probePath, []byte("#!/bin/sh\nprintf called >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH":                                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"SUITE_TEST_HOST_IP":                  suiteProxyTestAddress,
			"SUITE_TEST_OWNER_TOKEN":              suiteProxyTestToken,
			"WARP_CONFIG_HOME":                    configDir,
			"WARP_TEST_ENV_PROBE_RECORD":          probeRecordPath,
			"WARP_TEST_ENV_SUITE_PROXY_STATE_DIR": stateDir,
			"WARP_TEST_ENV_TCP_PROBE":             probePath,
			"WARP_VAULT_HOME":                     vaultDir,
		},
		"BRINGYOUR_POSTGRES_HOSTNAME",
		"BRINGYOUR_REDIS_HOSTNAME",
		"WARP_ENV",
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "TLS certificate/key pair is incomplete") ||
		!strings.Contains(string(output), "required complete resource tree is missing") {
		t.Fatalf("%s = %v, %q", description, err, output)
	}
	if _, err := os.Stat(probeRecordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s reached service probes: %v", description, err)
	}
}

// Nested versioned TLS assets are resolved through vault/all rather than a flat
// local resource, so an overlay that preserves only vault/local must fail before
// any service probe or database test starts.
func TestTestEnvironmentScriptRejectsSuiteProxyVaultTreeGap(t *testing.T) {
	stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
	vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
		t,
		"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
		"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
	)
	if err := os.Remove(filepath.Join(
		vaultDir,
		"all",
		"tls",
		"2026.9.2",
		"ur.network",
		"ur.network.key",
	)); err != nil {
		t.Fatal(err)
	}
	requireTestEnvironmentSuiteProxyTlsRejection(
		t,
		stateDir,
		vaultDir,
		configDir,
		"missing suite TLS key",
	)
}

// A certificate and key in different version directories are not a usable
// resolver result, even when both leaf paths exist somewhere in the TLS tree.
func TestTestEnvironmentScriptRejectsSuiteProxySplitVersionTlsPair(t *testing.T) {
	stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
	vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
		t,
		"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
		"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
	)
	sourcePath := filepath.Join(
		vaultDir,
		"all",
		"tls",
		"2026.9.2",
		"ur.network",
		"ur.network.key",
	)
	targetDir := filepath.Join(vaultDir, "all", "tls", "2026.9.3", "ur.network")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sourcePath, filepath.Join(targetDir, "ur.network.key")); err != nil {
		t.Fatal(err)
	}
	requireTestEnvironmentSuiteProxyTlsRejection(
		t,
		stateDir,
		vaultDir,
		configDir,
		"split-version suite TLS pair",
	)
}

// A direct root leaf precedes every versioned root/local/all candidate, so it
// cannot be allowed to combine with the key from a later complete pair.
func TestTestEnvironmentScriptRejectsSuiteProxyPartialDirectTlsPair(t *testing.T) {
	stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
	vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
		t,
		"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
		"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
	)
	hostDir := filepath.Join(vaultDir, "tls", "ur.network")
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "ur.network.crt"), []byte("synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireTestEnvironmentSuiteProxyTlsRejection(
		t,
		stateDir,
		vaultDir,
		configDir,
		"partial direct suite TLS pair",
	)
}

// A lower complete rotation does not make higher independently selected
// certificate and key versions safe to combine.
func TestTestEnvironmentScriptRejectsSuiteProxyHigherSplitTlsPair(t *testing.T) {
	stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
	vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
		t,
		"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
		"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
	)
	certDir := filepath.Join(vaultDir, "all", "tls", "2026.9.3", "ur.network")
	keyDir := filepath.Join(vaultDir, "all", "tls", "2026.9.4", "ur.network")
	for _, path := range []string{certDir, keyDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(certDir, "ur.network.crt"), []byte("synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, "ur.network.key"), []byte("synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireTestEnvironmentSuiteProxyTlsRejection(
		t,
		stateDir,
		vaultDir,
		configDir,
		"higher split-version suite TLS pair",
	)
}

// Coreos semver accepts empty prerelease and metadata suffixes, so shell
// preflight must not ignore partial rotations under either directory alias.
func TestTestEnvironmentScriptRejectsSuiteProxyEmptySemverSuffixTlsPartialPair(t *testing.T) {
	for _, suffix := range []string{"-", "+"} {
		stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
		vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
			t,
			"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
			"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
		)
		hostDir := filepath.Join(vaultDir, "all", "tls", "2026.9.3"+suffix, "ur.network")
		if err := os.MkdirAll(hostDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hostDir, "ur.network.crt"), []byte("synthetic\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		requireTestEnvironmentSuiteProxyTlsRejection(
			t,
			stateDir,
			vaultDir,
			configDir,
			"empty semver suffix "+suffix+" partial suite TLS pair",
		)
	}
}

// Resource authorities are expanded first and then compared to both direct
// attested endpoints; reachability cannot mask a root or port mismatch.
func TestTestEnvironmentScriptRejectsSuiteProxyAuthorityMismatch(t *testing.T) {
	stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
	vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
		t,
		"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15433",
		"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
	)
	binDir := writeTestEnvironmentSuiteProxyTools(t)
	probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
	probePath := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probePath, []byte("#!/bin/sh\nprintf called >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH":                                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"SUITE_TEST_HOST_IP":                  suiteProxyTestAddress,
			"SUITE_TEST_OWNER_TOKEN":              suiteProxyTestToken,
			"WARP_CONFIG_HOME":                    configDir,
			"WARP_TEST_ENV_PROBE_RECORD":          probeRecordPath,
			"WARP_TEST_ENV_SUITE_PROXY_STATE_DIR": stateDir,
			"WARP_TEST_ENV_TCP_PROBE":             probePath,
			"WARP_VAULT_HOME":                     vaultDir,
		},
		"BRINGYOUR_POSTGRES_HOSTNAME",
		"BRINGYOUR_REDIS_HOSTNAME",
		"WARP_ENV",
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "PostgreSQL resource authority does not match") {
		t.Fatalf("suite authority mismatch = %v, %q", err, output)
	}
	if _, err := os.Stat(probeRecordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authority mismatch reached service probes: %v", err)
	}
}

// Malformed ids, owner/readiness disagreement, an unpublished sentinel, a
// stale argv challenge, and loopback endpoints all stop before reachability.
func TestTestEnvironmentScriptRejectsInvalidSuiteProxyState(t *testing.T) {
	tests := []struct {
		name          string
		host          string
		challenge     string
		mutate        func(t *testing.T, stateDir string)
		expectedError string
	}{
		{
			name:          "owner token mismatch",
			host:          suiteProxyTestAddress,
			challenge:     suiteProxyTestToken,
			expectedError: "immutable snapshot",
			mutate: func(t *testing.T, stateDir string) {
				t.Helper()
				ownerPath := filepath.Join(stateDir, "owner")
				contents, err := os.ReadFile(ownerPath)
				if err != nil {
					t.Fatal(err)
				}
				changed := strings.Replace(string(contents), suiteProxyTestToken, "other-owner-token", 1)
				if err := os.WriteFile(ownerPath, []byte(changed), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:          "malformed proxy id",
			host:          suiteProxyTestAddress,
			challenge:     suiteProxyTestToken,
			expectedError: "PostgreSQL proxy id is malformed",
			mutate: func(t *testing.T, stateDir string) {
				t.Helper()
				recordPath := filepath.Join(stateDir, "ready", "record")
				contents, err := os.ReadFile(recordPath)
				if err != nil {
					t.Fatal(err)
				}
				changed := strings.Replace(string(contents), strings.Repeat("d", 64), "short", 1)
				if err := os.WriteFile(recordPath, []byte(changed), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:          "not published",
			host:          suiteProxyTestAddress,
			challenge:     suiteProxyTestToken,
			expectedError: "readiness sentinel is missing",
			mutate: func(t *testing.T, stateDir string) {
				t.Helper()
				if err := os.Remove(filepath.Join(stateDir, "ready", "published")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:          "stale owner challenge",
			host:          suiteProxyTestAddress,
			challenge:     "other-owner-token",
			expectedError: "owner process challenge is not live",
		},
		{
			name:          "loopback endpoint",
			host:          "127.0.0.1",
			challenge:     suiteProxyTestToken,
			expectedError: "must be a non-loopback unicast address",
		},
	}

	for _, test := range tests {
		stateDir := writeTestEnvironmentSuiteProxyState(t, test.host, "15432", "16379")
		if test.mutate != nil {
			test.mutate(t, stateDir)
		}
		vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
			t,
			"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
			"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
		)
		binDir := writeTestEnvironmentSuiteProxyTools(t)
		probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
		probePath := filepath.Join(t.TempDir(), "probe")
		if err := os.WriteFile(probePath, []byte("#!/bin/sh\nprintf called >> \"$WARP_TEST_ENV_PROBE_RECORD\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "./test-env.sh")
		cmd.Env = testCommandEnvironment(
			map[string]string{
				"PATH":                                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"SUITE_TEST_HOST_IP":                  test.host,
				"SUITE_TEST_OWNER_TOKEN":              test.challenge,
				"WARP_CONFIG_HOME":                    configDir,
				"WARP_TEST_ENV_PROBE_RECORD":          probeRecordPath,
				"WARP_TEST_ENV_SUITE_PROXY_STATE_DIR": stateDir,
				"WARP_TEST_ENV_TCP_PROBE":             probePath,
				"WARP_VAULT_HOME":                     vaultDir,
			},
			"BRINGYOUR_POSTGRES_HOSTNAME",
			"BRINGYOUR_REDIS_HOSTNAME",
			"WARP_ENV",
			"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
		)
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), test.expectedError) {
			t.Fatalf("%s invalid suite proxy state = %v, %q; want %q", test.name, err, output, test.expectedError)
		}
		if _, err := os.Stat(probeRecordPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s invalid state reached probe: %v", test.name, err)
		}
	}
}

// The saved token/generation snapshot is checked after both probes, catching a
// readiness replacement that occurs while services are being contacted.
func TestTestEnvironmentScriptRejectsSuiteProxyStateChangedDuringProbes(t *testing.T) {
	stateDir := writeTestEnvironmentSuiteProxyState(t, suiteProxyTestAddress, "15432", "16379")
	vaultDir, configDir := writeTestEnvironmentSuiteProxyResources(
		t,
		"{{ env:BRINGYOUR_POSTGRES_HOSTNAME }}:15432",
		"{{ env:BRINGYOUR_REDIS_HOSTNAME }}:16379",
	)
	binDir := writeTestEnvironmentSuiteProxyTools(t)
	probeRecordPath := filepath.Join(t.TempDir(), "probe-record")
	probePath := filepath.Join(t.TempDir(), "probe")
	probe := `#!/bin/sh
printf '%s %s %s\n' "$1" "$2" "$3" >> "$WARP_TEST_ENV_PROBE_RECORD"
if [ "$1" = redis ]; then
  sed 's/generation=test-generation/generation=replaced-generation/' \
    "$WARP_TEST_ENV_SUITE_PROXY_STATE_DIR/ready/record" > \
    "$WARP_TEST_ENV_SUITE_PROXY_STATE_DIR/ready/record.changed"
  /bin/mv "$WARP_TEST_ENV_SUITE_PROXY_STATE_DIR/ready/record.changed" \
    "$WARP_TEST_ENV_SUITE_PROXY_STATE_DIR/ready/record"
fi
`
	if err := os.WriteFile(probePath, []byte(probe), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "./test-env.sh")
	cmd.Env = testCommandEnvironment(
		map[string]string{
			"PATH":                                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"SUITE_TEST_HOST_IP":                  suiteProxyTestAddress,
			"SUITE_TEST_OWNER_TOKEN":              suiteProxyTestToken,
			"WARP_CONFIG_HOME":                    configDir,
			"WARP_TEST_ENV_PROBE_RECORD":          probeRecordPath,
			"WARP_TEST_ENV_SUITE_PROXY_STATE_DIR": stateDir,
			"WARP_TEST_ENV_TCP_PROBE":             probePath,
			"WARP_VAULT_HOME":                     vaultDir,
		},
		"BRINGYOUR_POSTGRES_HOSTNAME",
		"BRINGYOUR_REDIS_HOSTNAME",
		"WARP_ENV",
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES",
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "suite proxy state changed during service probes") {
		t.Fatalf("suite proxy TOCTOU = %v, %q", err, output)
	}
	probeRecord, readErr := os.ReadFile(probeRecordPath)
	if readErr != nil || strings.Count(string(probeRecord), "\n") != 2 {
		t.Fatalf("bounded probes before stable-snapshot rejection = %q, %v", probeRecord, readErr)
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
