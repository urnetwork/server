// Exercises the host Git boundary with native ownership checks and injected
// command failures, without Docker, root privileges, or production checkouts.
package main

import (
	"encoding/json"
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

// Git's own test switch forces its ownership check without requiring chown.
// The wrapper applies it only to the sealed baseline, matching the incident.
func TestEvaluatorReadsSealedProtectedSourceWithoutPersistentGitTrust(t *testing.T) {
	root := t.TempDir()
	for _, role := range []string{"baseline", "candidate"} {
		sourceTestRepository(t, filepath.Join(root, role), "server")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	const prelude = `set -Eeuo pipefail
baseline_source_root="$SOURCE_TEST_ROOT/baseline"
candidate_source_root="$SOURCE_TEST_ROOT/candidate"
die() { printf '%s\n' "$*" >&2; exit 1; }
git() {
    case " $* " in
        *" -C $baseline_source_root/server "*)
            GIT_TEST_ASSUME_DIFFERENT_OWNER=1 "$SOURCE_TEST_GIT" "$@" ;;
        *) "$SOURCE_TEST_GIT" "$@" ;;
    esac
}
if refusal="$(git -c safe.directory= -C "$baseline_source_root/server" rev-parse HEAD 2>&1)"; then
    die 'ownership fixture did not trigger the native Git refusal'
fi
[[ "$refusal" == *'detected dubious ownership'* ]] || die "$refusal"
`
	const verify = `
authenticate_protected_simulator_tree
if git -c safe.directory= -C "$baseline_source_root/server" rev-parse HEAD >/dev/null 2>&1; then
    die 'ownership trust escaped the protected-tree read'
fi
printf 'protected source authenticated\n'
`
	command := exec.Command("bash", "-c", prelude+evaluatorShellFunction(t, "authenticate_protected_simulator_tree")+verify)
	command.Env = append(os.Environ(), "SOURCE_TEST_ROOT="+root, "SOURCE_TEST_GIT="+gitPath,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"))
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "protected source authenticated\n" {
		t.Fatalf("sealed protected source: %v: %s", err, output)
	}
	if _, err := os.Lstat(filepath.Join(root, "gitconfig")); !os.IsNotExist(err) {
		t.Fatalf("protected read wrote persistent Git configuration: %v", err)
	}
}

// A successful read of different object ids still rejects a protected edit.
func TestEvaluatorRejectsChangedProtectedSource(t *testing.T) {
	root := t.TempDir()
	for _, role := range []string{"baseline", "candidate"} {
		sourceTestRepository(t, filepath.Join(root, role), "server")
	}
	candidateRoot := filepath.Join(root, "candidate", "server")
	if err := os.WriteFile(filepath.Join(candidateRoot, "connect", "sim-latency", "main.go"), []byte("package main\n// changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, candidateRoot, "add", "--all")
	sourceTestGit(t, candidateRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "protected change")
	command := exec.Command("bash", "-c", `set -Eeuo pipefail
baseline_source_root="$SOURCE_TEST_ROOT/baseline"
candidate_source_root="$SOURCE_TEST_ROOT/candidate"
die() { printf '%s\n' "$*" >&2; exit 1; }
`+evaluatorShellFunction(t, "authenticate_protected_simulator_tree")+"\nauthenticate_protected_simulator_tree\n")
	command.Env = append(os.Environ(), "SOURCE_TEST_ROOT="+root)
	output, err := command.CombinedOutput()
	if err == nil || strings.TrimSpace(string(output)) != "candidate changed the protected sim-latency source tree" {
		t.Fatalf("changed protected source: %v: %s", err, output)
	}
}

// Two failed reads must not compare equal; a failed read must not masquerade
// as candidate tampering or a successful empty/malformed object identity.
func TestEvaluatorSeparatesProtectedSourceGitFailuresFromChanges(t *testing.T) {
	function := evaluatorShellFunction(t, "authenticate_protected_simulator_tree")
	for _, fixture := range []struct {
		role      string
		malformed bool
		want      string
	}{
		{role: "baseline", want: "baseline protected sim-latency source tree: Git invocation failed"},
		{role: "candidate", want: "candidate protected sim-latency source tree: Git invocation failed"},
		{role: "both", want: "baseline protected sim-latency source tree: Git invocation failed"},
		{role: "baseline", malformed: true, want: "invalid Git object identity"},
		{role: "candidate", malformed: true, want: "invalid Git object identity"},
	} {
		command := exec.Command("bash", "-c", `set -Eeuo pipefail
baseline_source_root=/synthetic/baseline
candidate_source_root=/synthetic/candidate
die() { printf '%s\n' "$*" >&2; exit 1; }
git() {
    if [[ "$SOURCE_TEST_ROLE" == both || " $* " == *" -C /synthetic/$SOURCE_TEST_ROLE/server "* ]]; then
        if [[ "$SOURCE_TEST_MALFORMED" == true ]]; then
            printf 'not-an-object-id\n'
            return 0
        fi
        printf 'synthetic Git invocation failure\n' >&2
        return 128
    fi
    printf '%040d\n' 1
}
`+function+"\nauthenticate_protected_simulator_tree\n")
		malformed := "false"
		if fixture.malformed {
			malformed = "true"
		}
		command.Env = append(os.Environ(), "SOURCE_TEST_ROLE="+fixture.role, "SOURCE_TEST_MALFORMED="+malformed)
		output, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(output), fixture.want) || strings.Contains(string(output), "candidate changed") {
			t.Errorf("role=%s malformed=%s: %v: %s", fixture.role, malformed, err, output)
		}
	}
}

// All fixtures authenticate real repositories and apply a real patch. Only
// the selected Git failure is synthetic; no Docker command is in this section.
func runSourceGitBuilderFixture(t *testing.T, mode string, protectedPatch bool) ([]byte, error) {
	t.Helper()
	root := t.TempDir()
	repositoryCommits := map[string]string{}
	for _, repository := range sourceRepositoryNames() {
		repositoryCommits[repository] = sourceTestRepository(t, root, repository)
	}
	serverRoot := filepath.Join(root, "server")
	patchRelative := "source.txt"
	original := "server\n"
	if protectedPatch {
		patchRelative = "connect/sim-latency/main.go"
		original = "package main\n"
	}
	patchPath := filepath.Join(serverRoot, filepath.FromSlash(patchRelative))
	if err := os.WriteFile(patchPath, []byte(original+"// changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := sourceTestGit(t, serverRoot, "diff", "--binary") + "\n"
	if err := os.WriteFile(patchPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "canonical.patch"), []byte(patch), 0o600); err != nil {
		t.Fatal(err)
	}
	identityBytes, err := json.Marshal(map[string]any{
		"schema": 1, "kind": "sim-latency-evaluation-source", "temporary": true,
		"base_image_id": "sha256:" + strings.Repeat("b", 64), "base_sha": repositoryCommits["server"],
		"branch": "sim-latency", "source_lock_sha256": strings.Repeat("c", 64),
		"repositories": repositoryCommits, "candidate_patch_sha256": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".evaluation-source.json"), identityBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	const prelude = `set -Eeuo pipefail
source_root="$SOURCE_TEST_ROOT"
build_context="$SOURCE_TEST_ROOT"
base_image_id="sha256:$(printf 'b%.0s' {1..64})"
base_sha="$SOURCE_TEST_BASE"
source_lock_sha256="$(printf 'c%.0s' {1..64})"
patch_sha256="$(printf 'd%.0s' {1..64})"
git() {
    local fail=false
    case "$SOURCE_TEST_MODE" in
        status) [[ "$3" != status ]] || fail=true ;;
        protected-status) [[ "$*" != *'status --porcelain=v1 --untracked-files=all -- connect/sim-latency' ]] || fail=true ;;
        protected-diff) [[ "$*" != *'diff --quiet -- connect/sim-latency' ]] || fail=true ;;
        protected-tree) [[ "$*" != *'HEAD:connect/sim-latency' ]] || fail=true ;;
        candidate-tree)
            if [[ -f "$SOURCE_TEST_ROOT/committed" && "$*" == *'HEAD:connect/sim-latency' ]]; then fail=true; fi ;;
        candidate-status)
            if [[ -f "$SOURCE_TEST_ROOT/committed" && "$3" == status ]]; then fail=true; fi ;;
        candidate-commit) [[ "$3" != commit ]] || fail=true ;;
    esac
    if [[ "$fail" == true ]]; then
        printf 'synthetic Git invocation failure\n' >&2
        return 128
    fi
    "$SOURCE_TEST_GIT" "$@" || return $?
    if [[ "$3" == commit ]]; then
        : > "$SOURCE_TEST_ROOT/committed"
    fi
}
`
	errorFunction := sourceGitScriptSection(t, "build-submission.sh", "infrastructure_failure() {\n", "\nusage() {")
	buildSection := sourceGitScriptSection(t, "build-submission.sh", "source_identity=", "\nchmod 0600 \"$source_identity\"")
	command := exec.Command("bash", "-c", prelude+errorFunction+buildSection+"\nprintf 'candidate source authenticated\\n'\n")
	command.Env = append(os.Environ(), "SOURCE_TEST_ROOT="+root, "SOURCE_TEST_GIT="+gitPath,
		"SOURCE_TEST_BASE="+repositoryCommits["server"], "SOURCE_TEST_MODE="+mode)
	return command.CombinedOutput()
}

// A benign candidate still crosses the exact same source verification path.
func TestSubmissionBuilderAuthenticatesUnchangedProtectedSource(t *testing.T) {
	output, err := runSourceGitBuilderFixture(t, "", false)
	if err != nil || string(output) != "candidate source authenticated\n" {
		t.Fatalf("valid candidate source: %v: %s", err, output)
	}
}

// Exit 2 keeps trusted Git failures out of terminal submission rejection.
func TestSubmissionBuilderClassifiesSourceGitFailuresAsInfrastructure(t *testing.T) {
	for _, mode := range []string{"status", "protected-status", "protected-diff", "protected-tree", "candidate-tree", "candidate-status", "candidate-commit"} {
		output, err := runSourceGitBuilderFixture(t, mode, false)
		exitError, ok := err.(*exec.ExitError)
		if !ok || exitError.ExitCode() != 2 || !strings.Contains(string(output), "evaluator infrastructure failure:") ||
			strings.Contains(string(output), "submission attempted") || strings.Contains(string(output), "candidate source authenticated") {
			t.Errorf("%s: error=%v, output=%s", mode, err, output)
		}
	}
}

// A real diff exit 1 remains distinct from invocation/error exit codes.
func TestSubmissionBuilderRejectsProtectedSourceChanges(t *testing.T) {
	output, err := runSourceGitBuilderFixture(t, "", true)
	exitError, ok := err.(*exec.ExitError)
	if !ok || exitError.ExitCode() != 1 || !strings.Contains(string(output), "submission attempted to modify the protected sim-latency source tree") ||
		strings.Contains(string(output), "infrastructure failure") {
		t.Fatalf("changed protected source: %v: %s", err, output)
	}
}

// A healthy daemon does not convert the builder's Git infrastructure exit into
// a terminal candidate result at the next boundary in the real evaluator.
func TestEvaluatorPreservesBuilderSourceGitInfrastructureFailure(t *testing.T) {
	root := t.TempDir()
	builderPath := filepath.Join(root, "builder")
	if err := os.WriteFile(builderPath, []byte("#!/usr/bin/env bash\nprintf 'synthetic Git infrastructure failure\\n' >&2\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-c", `set -Eeuo pipefail
work_dir="$SOURCE_TEST_ROOT"
BUILD_SUBMISSION="$SOURCE_TEST_ROOT/builder"
MAX_BUILD_LOG_BYTES=1024
replicates=1
base_build_ref=synthetic-base
base_image_id=synthetic-base
candidate_source_root=/synthetic/candidate
patch_path=/synthetic/canonical.patch
policy_path=/synthetic/policy.json
write_evaluation_progress() { :; }
log() { :; }
die() { printf '%s\n' "$*" >&2; exit 1; }
on_error() { return "$2"; }
sudo() { return 0; }
emit_candidate_build_failure() { printf 'unexpected terminal submission failure\n'; exit 99; }
`+evaluatorShellFunction(t, "build_candidate")+"\nbuild_candidate\n")
	command.Env = append(os.Environ(), "SOURCE_TEST_ROOT="+root)
	output, err := command.CombinedOutput()
	exitError, ok := err.(*exec.ExitError)
	if !ok || exitError.ExitCode() != 1 || !strings.Contains(string(output), "candidate builder failed because evaluator infrastructure is unavailable") ||
		strings.Contains(string(output), "unexpected terminal submission failure") {
		t.Fatalf("Git failure through evaluator build stage: %v: %s", err, output)
	}
}

// Empty stdout from a failed status command must never mean a clean checkout.
func TestEvaluatorRefusesToSealSourceWhenGitStatusFails(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".evaluation-source.json"), []byte(`{"repositories":{"server":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-c", `set -Eeuo pipefail
baseline_source_root="$SOURCE_TEST_ROOT"
candidate_source_root=/synthetic/candidate
die() { printf '%s\n' "$*" >&2; exit 1; }
git() {
    case "$3" in
        symbolic-ref) printf 'sim-latency\n' ;;
        rev-parse) printf '%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
        status) printf 'synthetic Git status failure\n' >&2; return 128 ;;
        *) exit 99 ;;
    esac
}
sudo() { printf 'unexpected source seal\n'; exit 99; }
`+evaluatorShellFunction(t, "seal_evaluation_source")+"\nseal_evaluation_source \"$baseline_source_root\" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ''\n")
	command.Env = append(os.Environ(), "SOURCE_TEST_ROOT="+root)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "could not inspect temporary server source worktree") || strings.Contains(string(output), "unexpected source seal") {
		t.Fatalf("failed Git status during sealing: %v: %s", err, output)
	}
}
