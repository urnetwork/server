package server

import (
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
			"WARP_HOME":                            t.TempDir(),
			"WARP_TEST_ENV_PROBE_RECORD":           probeRecordPath,
			"WARP_TEST_ENV_TCP_PROBE":              probePath,
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1",
			"BRINGYOUR_POSTGRES_HOSTNAME":          "local-pg.bringyour.com",
			"BRINGYOUR_REDIS_HOSTNAME":             "local-redis.bringyour.com",
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
