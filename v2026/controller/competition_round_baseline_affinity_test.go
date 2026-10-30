// Exercises the trusted caller ABI through the real contained-command launcher.
// Only safe shell prefixes run; no evaluator, Docker, workload or seed is used.
package controller

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Synthetic role inputs retain their exact argument boundaries across exec.
type roundBaselineAffinityFixture struct {
	Directory        string
	Program          string
	Arguments        []string
	ManagementCpuset string
}

// Selects actual source statements, refusing missing or ambiguous boundaries.
func roundBaselineAffinitySourceBetween(t *testing.T, source, begin, end string) string {
	t.Helper()
	if strings.Count(source, begin) != 1 || strings.Count(source, end) != 1 {
		t.Fatal("caller source boundaries are missing or ambiguous")
	}
	start, finish := strings.Index(source, begin), strings.Index(source, end)
	if finish <= start {
		t.Fatal("caller source boundaries are reversed")
	}
	return source[start:finish]
}

// One dynamically allowed test CPU avoids fixed host topology or identities.
func roundBaselineAffinityTestCpu(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		if !strings.HasPrefix(line, "Cpus_allowed_list:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatal("invalid test process affinity")
		}
		cpu := strings.Split(strings.Split(fields[1], ",")[0], "-")[0]
		if _, err := strconv.Atoi(cpu); err != nil {
			t.Fatal(err)
		}
		return cpu
	}
	t.Fatal("test process affinity is unavailable")
	return ""
}

// The real baseline parser and affinity statements precede the unchanged
// evaluator re-exec block; the remainder cannot invoke native evaluation.
func newRoundBaselineAffinityFixture(t *testing.T, historical bool, observations string) *roundBaselineAffinityFixture {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "synthetic caller with spaces")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	callerBytes, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", "prepare-round-baseline.sh"))
	if err != nil {
		t.Fatal(err)
	}
	caller := string(callerBytes)
	boundary := "# round_baseline_prefix_program_begin\n"
	if strings.Count(caller, boundary) != 1 {
		t.Fatal("baseline caller prefix boundary changed")
	}
	body := caller[:strings.Index(caller, boundary)]
	if historical {
		affinity := roundBaselineAffinitySourceBetween(t, body, "# round_baseline_affinity_program_begin\n", "# round_baseline_affinity_program_end\n")
		body = strings.Replace(body, affinity, "", 1)
	}
	body = strings.Replace(body, "umask 077\n", "umask 077\n"+observations, 1)
	body = strings.Replace(body, "baseline_artifacts=", "printf '%s\\n' \"${COMPETITION_MANAGEMENT_CPUSET_APPLIED:-missing}\" >> \"$baseline_release/sentinel-trace\"\nbaseline_artifacts=", 1)
	evaluatorBytes, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", "evaluator.sh"))
	if err != nil {
		t.Fatal(err)
	}
	evaluatorAffinity := roundBaselineAffinitySourceBetween(t, string(evaluatorBytes), "expected_management_affinity=", "\nwork_dir=\"$artifact_dir/.evidence-runtime\"")
	evaluatorParser := roundBaselineAffinitySourceBetween(t, string(evaluatorBytes), "while [ \"$#\" -gt 0 ]; do\n    case \"$1\" in\n        --request|--result)", "\ncleanup() {")
	roleToEvaluatorArguments := roundBaselineAffinitySourceBetween(t, caller, "set -- --request \"$baseline_request\"", "\neval \"$baseline_prefix\"")
	body += `
die(){ printf 'synthetic affinity refusal\n' >&2; exit 1; }
management_cpuset="$("$baseline_release/resource-boundary.sh" | jq -er '.management_cpuset')"
` + roleToEvaluatorArguments + "\n" + evaluatorParser + "\n" + evaluatorAffinity + `
printf '%s\0' "${baseline_arguments[@]}" "$actual_management_affinity"
`
	program := filepath.Join(directory, "prepare-round-baseline.sh")
	if err := os.WriteFile(program, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	cpu := roundBaselineAffinityTestCpu(t)
	resource := "#!/bin/bash\nset -Eeuo pipefail\nprintf '%s\\n' '{\"management_cpuset\":\"" + cpu + "\"}'\n"
	if err := os.WriteFile(filepath.Join(directory, "resource-boundary.sh"), []byte(resource), 0o700); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(directory, "synthetic request.json")
	if err := os.WriteFile(request, []byte(`{"evaluation_policy":{"replicates":9},"round_baseline_path":"","round_baseline_sha256":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return &roundBaselineAffinityFixture{
		Directory: directory, Program: program, ManagementCpuset: cpu,
		Arguments: []string{"--request", request, "--receipt", filepath.Join(directory, "round-baseline-preparation.complete.json"), "--owner", "baseline.synthetic", "--context-sha256", strings.Repeat("a", 64)},
	}
}

// A sentinel in the parent is intentionally lost by the real clean launch.
// Removing only the caller fix reproduces the original exit-2 ABI failure.
func TestCompetitionRoundBaselineAffinityContainedCommandPreservesRoleAbi(t *testing.T) {
	t.Setenv("COMPETITION_MANAGEMENT_CPUSET_APPLIED", "synthetic-parent-sentinel")
	historical := newRoundBaselineAffinityFixture(t, true, "")
	var oldStdout, oldStderr bytes.Buffer
	code, err := runContainedCommand(t.Context(), historical.Directory, historical.Program, historical.Arguments, &oldStdout, &oldStderr)
	if err != nil || code != 2 || oldStdout.Len() != 0 || oldStderr.Len() != 0 {
		t.Fatalf("historical caller did not reproduce its silent ABI refusal: code=%d err=%v stdout=%q stderr=%q", code, err, &oldStdout, &oldStderr)
	}
	fixed := newRoundBaselineAffinityFixture(t, false, "")
	var stdout, stderr bytes.Buffer
	code, err = runContainedCommand(t.Context(), fixed.Directory, fixed.Program, fixed.Arguments, &stdout, &stderr)
	want := strings.Join(append(append([]string{}, fixed.Arguments...), fixed.ManagementCpuset), "\x00") + "\x00"
	if err != nil || code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("corrected caller lost its role arguments or confinement: code=%d err=%v stdout=%q stderr=%q", code, err, &stdout, &stderr)
	}
	trace, err := os.ReadFile(filepath.Join(fixed.Directory, "sentinel-trace"))
	if err != nil || string(trace) != "missing\n"+fixed.ManagementCpuset+"\n" {
		t.Fatalf("clean environment or actual affinity re-exec was skipped: trace=%q err=%v", trace, err)
	}
}

// A forged matching sentinel cannot substitute for the actual CPU observation.
func TestCompetitionRoundBaselineAffinityRejectsForgedSentinel(t *testing.T) {
	fixture := newRoundBaselineAffinityFixture(t, false, "awk(){ printf 'synthetic-wrong-scope\\n'; }\n")
	command := exec.CommandContext(t.Context(), fixture.Program, fixture.Arguments...)
	command.Dir = fixture.Directory
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "TZ=UTC", "COMPETITION_MANAGEMENT_CPUSET_APPLIED=" + fixture.ManagementCpuset}
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if err == nil || !errors.As(err, &exitError) || exitError.ExitCode() != 2 || len(output) != 0 {
		t.Fatalf("forged sentinel bypassed the actual scope guard: err=%v output=%q", err, output)
	}
}

// Missing, malformed or inapplicable scope observations cannot reach a prefix.
func TestCompetitionRoundBaselineAffinityRejectsInvalidScope(t *testing.T) {
	for _, resource := range []string{`{}`, `{"management_cpuset":""}`, `{"management_cpuset":null}`, `{"management_cpuset":"synthetic-not-a-cpuset"}`} {
		fixture := newRoundBaselineAffinityFixture(t, false, "")
		body := "#!/bin/bash\nset -Eeuo pipefail\nprintf '%s\\n' '" + resource + "'\n"
		if err := os.WriteFile(filepath.Join(fixture.Directory, "resource-boundary.sh"), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code, err := runContainedCommand(t.Context(), fixture.Directory, fixture.Program, fixture.Arguments, &stdout, &stderr)
		if err != nil || code <= 0 || stdout.Len() != 0 {
			t.Fatalf("invalid synthetic scope reached completion: resource=%s code=%d err=%v stdout=%q", resource, code, err, &stdout)
		}
	}
}

// The original parser still refuses evaluator authority and mismatched scope.
func TestCompetitionRoundBaselineAffinityRejectsWrongRoleArguments(t *testing.T) {
	for _, mutation := range []string{"evaluator_result", "receipt", "owner", "context", "replicates"} {
		fixture := newRoundBaselineAffinityFixture(t, false, "")
		switch mutation {
		case "evaluator_result":
			fixture.Arguments = append(fixture.Arguments, "--result", filepath.Join(fixture.Directory, "synthetic-result.json"))
		case "receipt":
			fixture.Arguments[3] = filepath.Join(fixture.Directory, "synthetic-wrong-receipt.json")
		case "owner":
			fixture.Arguments[5] = "synthetic invalid owner"
		case "context":
			fixture.Arguments[7] = "synthetic-invalid-context"
		case "replicates":
			if err := os.WriteFile(fixture.Arguments[1], []byte(`{"evaluation_policy":{"replicates":7},"round_baseline_path":"","round_baseline_sha256":""}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var stdout, stderr bytes.Buffer
		code, err := runContainedCommand(t.Context(), fixture.Directory, fixture.Program, fixture.Arguments, &stdout, &stderr)
		if err != nil || code <= 0 || stdout.Len() != 0 {
			t.Fatalf("wrong role arguments accepted: mutation=%s code=%d err=%v stdout=%q", mutation, code, err, &stdout)
		}
	}
}

// The adjacent candidate builder already re-execs its own preserved ABI;
// execute that exact parser/affinity path without reaching Docker or builds.
func TestCompetitionRoundBaselineAffinityAdjacentBuilderPreservesOwnAbi(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "synthetic builder with spaces")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", "build-submission.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	boundary := "for command in awk git jq sha256sum mktemp realpath sudo stat taskset timeout; do"
	if strings.Count(source, boundary) != 1 {
		t.Fatal("candidate builder pre-host boundary changed")
	}
	cpu := roundBaselineAffinityTestCpu(t)
	affinity := roundBaselineAffinitySourceBetween(t, source, "expected_management_affinity=", "\n[ -f \"$patch_path\" ]")
	body := source[:strings.Index(source, boundary)] + fmt.Sprintf("management_cpuset=%q\n", cpu) + affinity + "\nprintf '%s\\0' \"${original_args[@]}\" \"$actual_management_affinity\"\n"
	program := filepath.Join(directory, "build-submission.sh")
	if err := os.WriteFile(program, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"--allow-local-base", "--base-image", "synthetic-base:fixture", "--source-root", filepath.Join(directory, "synthetic source"), "--patch", filepath.Join(directory, "synthetic patch"), "--policy", filepath.Join(directory, "synthetic policy"), "--tag", "synthetic-candidate:fixture", "--staging"}
	var stdout, stderr bytes.Buffer
	code, err := runContainedCommand(t.Context(), directory, program, arguments, &stdout, &stderr)
	want := strings.Join(append(append([]string{}, arguments...), cpu), "\x00") + "\x00"
	if err != nil || code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("adjacent builder lost its own argv: code=%d err=%v stdout=%q stderr=%q", code, err, &stdout, &stderr)
	}
}
