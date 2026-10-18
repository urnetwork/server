// Exercise the documented gate in Bash and observe its environment in a native
// test child. The synthetic preflight uses no PostgreSQL, Redis, or transport.
package monitor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The parent captures intended child failures; an ordinary suite invocation
// has no child marker and does not assert a release-only environment.
func TestRunMainReleaseGateNativeEnvironmentChild(t *testing.T) {
	if os.Getenv("MONITOR_RELEASE_GATE_NATIVE_CHILD") != "1" {
		return
	}
	failFast := os.Getenv("WARP_TEST_ENV_FAIL_FAST") == "1"
	t.Logf("native WARP_TEST_ENV_FAIL_FAST_is_1=%t", failFast)
	if !failFast {
		t.Fatal("documented gate did not deliver the first-failure policy to its native child")
	}
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("documented gate lost its successfully sourced local environment")
	}
}

// Select every operator-facing narrow, package and race invocation, including
// the bootstrap copies, so one stale example cannot silently restore retries.
func runMainReleaseGateCommands(t *testing.T) []string {
	t.Helper()
	wire, err := os.ReadFile("RUN-MAIN.md")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?m)^  '([^'\n]*source \./test-env\.sh[^'\n]*)'$`)
	matches := pattern.FindAllStringSubmatch(string(wire), -1)
	if len(matches) != 5 {
		t.Fatalf("documented release gate count = %d, want 5", len(matches))
	}
	commands := make([]string, 0, len(matches))
	for _, match := range matches {
		commands = append(commands, match[1])
	}
	return commands
}

// The source deliberately clears an inherited release setting. The final
// invocation must apply it after sourcing, and must stop on a failed preflight.
func runMainReleaseGateNativeControl(t *testing.T, command string, preflightFailure bool) (string, error) {
	t.Helper()
	rootDirectory := t.TempDir()
	toolsDirectory := filepath.Join(rootDirectory, "tools")
	if err := os.Mkdir(toolsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	const preflight = `export WARP_ENV=local
export WARP_TEST_ENV_FAIL_FAST=0
if [[ ${MONITOR_RELEASE_GATE_PREFLIGHT_FAILURE:-} == 1 ]]; then return 73; fi
`
	if err := os.WriteFile(filepath.Join(rootDirectory, "test-env.sh"), []byte(preflight), 0o600); err != nil {
		t.Fatal(err)
	}
	const launcher = `#!/bin/sh
test "$1" = test || exit 97
case " $* " in *' -count=1 '*) ;; *) exit 98 ;; esac
exec "$MONITOR_RELEASE_GATE_NATIVE_EXE" -test.run=^TestRunMainReleaseGateNativeEnvironmentChild$ -test.count=1 -test.v
`
	if err := os.WriteFile(filepath.Join(toolsDirectory, "go"), []byte(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	envPath, err := exec.LookPath("env")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(envPath, filepath.Join(toolsDirectory, "env")); err != nil {
		t.Fatal(err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	nativeExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, bash, "-c", command)
	process.Dir = rootDirectory
	process.Env = []string{
		"PATH=" + toolsDirectory,
		"WARP_HOME=" + rootDirectory,
		"WARP_ENV=main",
		"WARP_TEST_ENV_FAIL_FAST=1",
		"MONITOR_RELEASE_GATE_NATIVE_CHILD=1",
		"MONITOR_RELEASE_GATE_NATIVE_EXE=" + nativeExecutable,
	}
	if preflightFailure {
		process.Env = append(process.Env, "MONITOR_RELEASE_GATE_PREFLIGHT_FAILURE=1")
	}
	wire, runErr := process.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("documented gate native control timed out: %v", ctx.Err())
	}
	return string(wire), runErr
}

// Assert inheritance in the native child, not a wrapper's intended setting.
func TestRunMainReleaseGateCommandsPropagateNativeFirstFailurePolicy(t *testing.T) {
	for _, command := range runMainReleaseGateCommands(t) {
		output, err := runMainReleaseGateNativeControl(t, command, false)
		if err != nil || !strings.Contains(output, "native WARP_TEST_ENV_FAIL_FAST_is_1=true") {
			t.Fatalf("documented gate lost native release setting: command=%q error=%v output=%s", command, err, output)
		}
	}
}

// Removing the exact fix must reach the native child with the source's zero
// setting and fail there; a preflight or launch error is not this reproduction.
func TestRunMainReleaseGateCommandsRejectPriorPolicy(t *testing.T) {
	for _, command := range runMainReleaseGateCommands(t) {
		priorCommand := strings.Replace(command, "env WARP_TEST_ENV_FAIL_FAST=1 ", "", 1)
		if priorCommand == command {
			t.Fatalf("documented gate lacks an explicit release setting: %q", command)
		}
		output, err := runMainReleaseGateNativeControl(t, priorCommand, false)
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 || !strings.Contains(output, "native WARP_TEST_ENV_FAIL_FAST_is_1=false") {
			t.Fatalf("prior gate did not fail at native environment boundary: error=%v output=%s", err, output)
		}
	}
}

// A failing source must retain its status and prevent the native test launch.
func TestRunMainReleaseGateCommandsStopOnFailedPreflight(t *testing.T) {
	for _, command := range runMainReleaseGateCommands(t) {
		output, err := runMainReleaseGateNativeControl(t, command, true)
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 73 || strings.Contains(output, "native WARP_TEST_ENV_FAIL_FAST_is_1=") {
			t.Fatalf("failed preflight lost its boundary: error=%v output=%s", err, output)
		}
	}
}
