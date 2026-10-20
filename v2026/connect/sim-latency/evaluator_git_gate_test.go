// Exercises the real release/build shell gates without Docker, remote source,
// or application dependencies. Failed Git reads must never produce success.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Extracts real shell code so the fixtures cannot replace its exit handling.
func sourceGitScriptSection(t *testing.T, name string, startMarker string, endMarker string) string {
	t.Helper()
	scriptBytes, err := os.ReadFile(filepath.Join("evaluator", "container", name))
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	start := strings.Index(script, startMarker)
	if start < 0 {
		t.Fatalf("%s is missing %q", name, startMarker)
	}
	end := strings.Index(script[start:], endMarker)
	if end < 0 {
		t.Fatalf("%s is missing %q after %q", name, endMarker, startMarker)
	}
	return script[start : start+end]
}

// Retains each command's actual assignment/predicate connector. Testing a
// reimplemented clean-tree helper would miss errors hidden by test or sed.
type sourceGitStatusGate struct {
	line             int
	script           string
	trackedAllowed   bool
	untrackedAllowed bool
}

// Extracts every status command from shell/Dockerfile source, including legacy
// test/command substitutions, so reverting a fix still exercises the bug.
func sourceGitStatusGates(t *testing.T, name string) []sourceGitStatusGate {
	t.Helper()
	scriptBytes, err := os.ReadFile(filepath.Join("evaluator", "container", name))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(scriptBytes), "\n")
	gates := []sourceGitStatusGate{}
	for index, line := range lines {
		if !strings.Contains(line, "status --porcelain") {
			continue
		}
		command := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "\\"))
		command = strings.TrimPrefix(command, "RUN --network=none ")
		command = strings.TrimPrefix(command, "RUN ")
		command = strings.TrimPrefix(command, "&& ")
		if strings.HasPrefix(command, "source_status=") {
			if index+1 == len(lines) {
				t.Fatalf("%s:%d status assignment has no predicate", name, index+1)
			}
			predicate := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(lines[index+1]), "\\"))
			if !strings.HasPrefix(strings.TrimPrefix(predicate, "&& "), "test -z ") {
				t.Fatalf("%s:%d unexpected status predicate: %s", name, index+2, predicate)
			}
			command += "\n" + predicate
		}
		// Dockerfile lines use a continued &&, which must remain one list.
		command = strings.ReplaceAll(command, "\n&& ", " && ")
		diagnostic := strings.HasPrefix(command, "git ")
		gates = append(gates, sourceGitStatusGate{
			line: index + 1, script: command,
			trackedAllowed:   diagnostic || strings.Contains(command, "sed -n '/^??/p'"),
			untrackedAllowed: diagnostic,
		})
	}
	return gates
}

// Empty or partial stdout on exit 128 cannot satisfy any image/isolation
// cleanliness check, even the pipeline that filters tracked changes.
func TestEvaluatorBuildStatusGatesFailClosed(t *testing.T) {
	for _, file := range []struct {
		name      string
		gateCount int
	}{
		{name: "Dockerfile.base", gateCount: 4},
		{name: "Dockerfile.submission", gateCount: 5},
		{name: "test-build-isolation.sh", gateCount: 2},
	} {
		gates := sourceGitStatusGates(t, file.name)
		if len(gates) != file.gateCount {
			t.Fatalf("%s has %d source-status checks, want %d", file.name, len(gates), file.gateCount)
		}
		for _, gate := range gates {
			for _, fixture := range []struct {
				exitCode    int
				status      string
				wantSuccess bool
			}{
				{status: "", wantSuccess: true},
				{status: " M candidate.go\n", wantSuccess: gate.trackedAllowed},
				{status: "?? candidate.go\n", wantSuccess: gate.untrackedAllowed},
				{exitCode: 128},
				{exitCode: 128, status: " M candidate.go\n"},
				{exitCode: 128, status: "?? candidate.go\n"},
			} {
				command := exec.Command("sh", "-eu", "-c", `
evaluation_source_root=/synthetic/source
git() { printf '%s' "$SOURCE_TEST_STATUS"; return "$SOURCE_TEST_EXIT"; }
`+gate.script)
				command.Env = append(os.Environ(), "SOURCE_TEST_STATUS="+fixture.status,
					fmt.Sprintf("SOURCE_TEST_EXIT=%d", fixture.exitCode))
				output, err := command.CombinedOutput()
				if (err == nil) != fixture.wantSuccess {
					t.Errorf("%s:%d git exit=%d status=%q: error=%v output=%s; want success=%t",
						file.name, gate.line, fixture.exitCode, fixture.status, err, output, fixture.wantSuccess)
				}
			}
		}
	}
}

// Calls the real development-overlay function. The Git shim injects one exact
// command failure, while file enumeration/copying uses real nul-delimited paths.
func runEvaluatorDevelopmentOverlay(t *testing.T, mode string) ([]byte, error, string) {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"source", "clone"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "source", "new file\nname.go"), []byte("synthetic source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	function := sourceGitScriptSection(t, "build-base.sh", "overlay_worktree() {\n", "\nfor repository in ")
	command := exec.Command("bash", "-c", `set -Eeuo pipefail
build_context="$SOURCE_TEST_ROOT"
git() {
    local operation="$3"
    if [[ "$operation" == "$SOURCE_TEST_MODE" ]]; then
        printf 'synthetic Git invocation failure\n' >&2
        return 128
    fi
    case "$operation" in
        diff) return 0 ;;
        ls-files) printf 'new file\nname.go\0' ;;
        status) printf '?? new file\n' ;;
        add) : ;;
        commit) printf 'snapshot committed\n' ;;
        *) printf 'unexpected git operation: %s\n' "$*" >&2; return 99 ;;
    esac
}
`+function+`overlay_worktree "$SOURCE_TEST_ROOT/source" "$SOURCE_TEST_ROOT/clone"
printf 'snapshot complete\n'
`)
	command.Env = append(os.Environ(), "SOURCE_TEST_ROOT="+root, "SOURCE_TEST_MODE="+mode)
	output, err := command.CombinedOutput()
	return output, err, root
}

// Git diff, untracked enumeration, and status failures are all fatal before a
// development snapshot can be mistaken for an authenticated release source.
func TestEvaluatorDevelopmentOverlayRejectsGitInspectionFailures(t *testing.T) {
	for _, mode := range []string{"diff", "ls-files", "status"} {
		output, err, _ := runEvaluatorDevelopmentOverlay(t, mode)
		if err == nil || !strings.Contains(string(output), "could not") || strings.Contains(string(output), "snapshot complete") {
			t.Errorf("%s failure: %v: %s", mode, err, output)
		}
	}
}

// Capturing a nul stream through a checked file preserves embedded newlines;
// an ordinary shell variable would silently lose the filename boundary.
func TestEvaluatorDevelopmentOverlayPreservesUntrackedFileNames(t *testing.T) {
	output, err, root := runEvaluatorDevelopmentOverlay(t, "")
	if err != nil || string(output) != "snapshot committed\nsnapshot complete\n" {
		t.Fatalf("valid development snapshot: %v: %s", err, output)
	}
	content, err := os.ReadFile(filepath.Join(root, "clone", "new file\nname.go"))
	if err != nil || string(content) != "synthetic source\n" {
		t.Fatalf("untracked file content = %q, %v", content, err)
	}
}

// Runs the final checked-clone status predicate from the real base builder,
// independent of the development-only overlay path.
func TestEvaluatorBaseCloneRejectsStatusFailure(t *testing.T) {
	section := sourceGitScriptSection(t, "build-base.sh", "    clone_status=", "    clone_revision=")
	command := exec.Command("bash", "-c", `set -Eeuo pipefail
clone_root=/synthetic/clone
repository=synthetic
git() { printf 'synthetic Git invocation failure\n' >&2; return 128; }
`+section+"\nprintf 'clone accepted\\n'\n")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "could not inspect temporary clone") || strings.Contains(string(output), "clone accepted") {
		t.Fatalf("base clone status failure: %v: %s", err, output)
	}
}

// Infrastructure/build failures are not proof that protected source edits are
// rejected. Require the builder's specific terminal rejection and exit code.
func TestEvaluatorIsolationRequiresProtectedSourceRejection(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "build-submission.sh"), []byte(`#!/usr/bin/env bash
printf '%s\n' "$SOURCE_TEST_MESSAGE" >&2
exit "$SOURCE_TEST_EXIT"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	section := sourceGitScriptSection(t, "test-build-isolation.sh", "protected_build_status=", "git -C \"$evaluation_source_root/server\" checkout")
	for _, fixture := range []struct {
		exitCode    int
		message     string
		wantSuccess bool
	}{
		{exitCode: 1, message: "submission attempted to modify the protected sim-latency source tree", wantSuccess: true},
		{exitCode: 0, message: "submission attempted to modify the protected sim-latency source tree"},
		{exitCode: 1, message: "synthetic unrelated build failure"},
		{exitCode: 2, message: "evaluator infrastructure failure: synthetic Git failure"},
		{exitCode: 2, message: "submission attempted to modify the protected sim-latency source tree"},
	} {
		command := exec.Command("bash", "-c", `set -Eeuo pipefail
SCRIPT_DIR="$SOURCE_TEST_ROOT"
test_root="$SOURCE_TEST_ROOT"
build_args=()
base_image=synthetic-base
evaluation_source_root=/synthetic/source
protected_patch=/synthetic/patch
malformed_policy=/synthetic/policy
protected_tag=synthetic-tag
`+section)
		command.Env = append(os.Environ(), "SOURCE_TEST_ROOT="+root,
			fmt.Sprintf("SOURCE_TEST_EXIT=%d", fixture.exitCode), "SOURCE_TEST_MESSAGE="+fixture.message)
		output, err := command.CombinedOutput()
		if (err == nil) != fixture.wantSuccess {
			t.Errorf("builder exit=%d message=%q: %v: %s; want success=%t", fixture.exitCode, fixture.message, err, output, fixture.wantSuccess)
		}
	}
}
