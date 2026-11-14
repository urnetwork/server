// Admission cases need independent bounded processes because each disposable
// database setup consumes part of Go's package-wide test alarm.
package server

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each synthetic case consumes one complete logical package budget. This
// forces aggregate exhaustion without timing, services or scheduler ordering.
func writeTestServerAdmissionBudgetFixture(t *testing.T) (string, string, string) {
	t.Helper()
	runnerPath, binDir, workspaceRoot := writeTestServerRunnerFixture(t, "#!/bin/sh\ncat\n")
	goScript := `#!/usr/bin/env bash
if [[ "$PWD" != "$URNETWORK_ROOT/server" ]]; then
  printf 'later\n' >> "$URNETWORK_ROOT/admission-record"
  exit 0
fi
[[ "$1" == test && "$2" == -p=1 && "$3" == -race && "$4" == -count=1 && "$5" == -timeout=5m && "${WARP_TEST_ENV_FAIL_FAST:-}" == 1 ]] || exit 86
shift 5
packages=()
mode=
pattern=
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    -run|-list) mode="$1"; pattern="$2"; shift 2 ;;
    ./model|./taskworker/work) packages+=("$1"); shift ;;
    *) exit 86 ;;
  esac
done
[[ -n "$mode" && -n "$pattern" && "${#packages[@]}" -gt 0 ]] || exit 86
remaining=1
for package in "${packages[@]}"; do
  if [[ "$package" == ./model ]]; then
    cases=(TestRedisAdmissionFirstFixture TestAsyncDebitSecondFixture)
  else
    cases=(TestContractCreationSameNetworkLargeNContentionFree TestTransferDebitTaskPartitionsAndBoundedContinuation)
  fi
  if [[ "$mode" == -list ]]; then
    [[ "$pattern" == "$TEST_SERVER_ADMISSION_PATTERN" ]] || exit 86
    if [[ "${TEST_SERVER_ADMISSION_CENSUS_MODE:-}" == failed && "$package" == ./model ]]; then exit 54; fi
    if [[ "${TEST_SERVER_ADMISSION_CENSUS_MODE:-}" == empty && "$package" == ./model ]]; then cases=(); fi
    if [[ "${TEST_SERVER_ADMISSION_CENSUS_MODE:-}" == duplicate && "$package" == ./model ]]; then cases+=("${cases[0]}"); fi
    if [[ "${TEST_SERVER_ADMISSION_CENSUS_MODE:-}" == invalid && "$package" == ./model ]]; then cases+=(TestUnrelatedFixture); fi
    for name in "${cases[@]}"; do printf '%s\n' "$name"; done
    printf 'ok synthetic-package 0.001s\n'
    continue
  fi
  [[ "$mode" == -run ]] || exit 86
  for name in "${cases[@]}"; do
    [[ "$name" =~ $pattern ]] || continue
    if [[ "$remaining" -eq 0 ]]; then
      printf 'synthetic admission package budget exhausted by multiple cases\n' >&2
      exit 88
    fi
    remaining=$((remaining - 1))
    printf '%s %s %s\n' "$package" "$name" "$$" >> "$URNETWORK_ROOT/admission-record"
    if [[ "${TEST_SERVER_ADMISSION_FAILED_CASE:-}" == "$name" ]]; then exit 143; fi
  done
done
`
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(goScript), 0o700); err != nil {
		t.Fatal(err)
	}
	return runnerPath, binDir, workspaceRoot
}

// The actual script must give every case a fresh unchanged budget regardless
// of caller selectors, counts, short mode or timeout flags.
func TestServerTestScriptAdmissionCasesOwnIndependentTimeouts(t *testing.T) {
	runnerPath, binDir, workspaceRoot := writeTestServerAdmissionBudgetFixture(t)
	cmd := exec.Command("bash", runnerPath, "-run", "^TestUnrelatedFixture$", "-short", "-count=7", "-timeout=1ns")
	cmd.Env = testCommandEnvironment(map[string]string{
		"PATH":                             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"URNETWORK_ROOT":                   workspaceRoot,
		"URNETWORK_NETWORK_TEST_LOCK_HELD": "1",
		"WARP_TEST_ENV_FAIL_FAST":          "0",
		"TEST_SERVER_ADMISSION_PATTERN":    testServerRunnerAdmissionPattern,
	})
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("independent admission budgets: %v: %s", err, output)
	}
	record, err := os.ReadFile(filepath.Join(workspaceRoot, "admission-record"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(record)), "\n")
	want := []string{
		"./model TestRedisAdmissionFirstFixture",
		"./model TestAsyncDebitSecondFixture",
		"./taskworker/work TestContractCreationSameNetworkLargeNContentionFree",
		"./taskworker/work TestTransferDebitTaskPartitionsAndBoundedContinuation",
	}
	if len(lines) < len(want) {
		t.Fatalf("admission records = %q", lines)
	}
	processIds := map[string]bool{}
	for i, expected := range want {
		fields := strings.Fields(lines[i])
		if len(fields) != 3 || strings.Join(fields[:2], " ") != expected || processIds[fields[2]] {
			t.Fatalf("case %d does not own exactly one fresh process: %q", i, lines[i])
		}
		processIds[fields[2]] = true
	}
	for _, line := range lines[len(want):] {
		if line != "later" {
			t.Fatalf("mandatory case was repeated: %q", line)
		}
	}
}

// A later failed case is terminal; completed siblings are never retried and
// neither the next package nor the ordinary suite may run afterward.
func TestServerTestScriptAdmissionStopsAtFirstFailedCase(t *testing.T) {
	runnerPath, binDir, workspaceRoot := writeTestServerAdmissionBudgetFixture(t)
	cmd := exec.Command("bash", runnerPath)
	cmd.Env = testCommandEnvironment(map[string]string{
		"PATH":                              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"URNETWORK_ROOT":                    workspaceRoot,
		"URNETWORK_NETWORK_TEST_LOCK_HELD":  "1",
		"TEST_SERVER_ADMISSION_PATTERN":     testServerRunnerAdmissionPattern,
		"TEST_SERVER_ADMISSION_FAILED_CASE": "TestAsyncDebitSecondFixture",
	})
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("failed case status = %v: %s", err, output)
	}
	record, err := os.ReadFile(filepath.Join(workspaceRoot, "admission-record"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(record)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "./model TestRedisAdmissionFirstFixture ") ||
		!strings.HasPrefix(lines[1], "./model TestAsyncDebitSecondFixture ") {
		t.Fatalf("failure continued or retried admission: %q", record)
	}
}

// Failed discovery, an empty census and malformed/duplicate names cannot
// silently turn mandatory coverage into a passing release gate.
func TestServerTestScriptAdmissionRejectsInvalidCensus(t *testing.T) {
	for _, mode := range []string{"failed", "empty", "duplicate", "invalid"} {
		runnerPath, binDir, workspaceRoot := writeTestServerAdmissionBudgetFixture(t)
		cmd := exec.Command("bash", runnerPath)
		cmd.Env = testCommandEnvironment(map[string]string{
			"PATH":                              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"URNETWORK_ROOT":                    workspaceRoot,
			"URNETWORK_NETWORK_TEST_LOCK_HELD":  "1",
			"TEST_SERVER_ADMISSION_PATTERN":     testServerRunnerAdmissionPattern,
			"TEST_SERVER_ADMISSION_CENSUS_MODE": mode,
		})
		output, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		wantStatus := 65
		if mode == "failed" {
			wantStatus = 54
		}
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != wantStatus {
			t.Fatalf("%s census status = %v: %s", mode, err, output)
		}
		if record, err := os.ReadFile(filepath.Join(workspaceRoot, "admission-record")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s census ran tests: %q, %v", mode, record, err)
		}
	}
}
