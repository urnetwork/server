// Synthetic native Git and fixed process outcomes exercise the real source
// entrypoint without inspecting an actual submission or executing product code.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// SDK initialization runs before a test role. Preserve the native pipes outside
// its standard descriptors, then restore them only inside the explicit role.
const repositoryGitDiagnosticBootstrap = "exec 3>&1 4>&2\nexport COMPETITION_TEST_GIT_DIAGNOSTIC_FDS=3,4\nexec \"$@\" >/dev/null 2>&1\n"

// Only the selected fault needs the diagnostic executable and its SDK startup.
// The matcher shifts its own arguments, leaving native Git's argv intact.
const repositoryGitDiagnosticShim = `#!/bin/sh
set -e
matches_target() {
    while [ "$#" -ge 2 ]; do
        case "$1" in
            -C|-c) shift 2 ;;
            *) break ;;
        esac
    done
    case "$COMPETITION_TEST_GIT_DIAGNOSTIC_TARGET:${1:-}:${2:-}" in
        head_identity:rev-parse:HEAD|protected_tree:rev-parse:HEAD:connect/sim-latency)
            [ "$#" -eq 2 ] ;;
        protected_helper_tree:--literal-pathspecs:ls-tree|patch_base_tree:--literal-pathspecs:ls-tree)
            [ "$#" -ge 7 ] && [ "$3" = -z ] && [ "$4" = --full-tree ] && [ "$6" = -- ] || return 1
            if [ "$COMPETITION_TEST_GIT_DIAGNOSTIC_TARGET" = protected_helper_tree ]; then
                [ "$5" = HEAD ]
            else
                case "$5" in *[!0-9a-f]*) return 1 ;; esac
                [ "${#5}" -eq 40 ]
            fi ;;
        protected_helper_index:--literal-pathspecs:ls-files)
            [ "$#" -ge 6 ] && [ "$3" = --stage ] && [ "$4" = -z ] && [ "$5" = -- ] ;;
        protected_helper_bytes:hash-object:--no-filters)
            [ "$#" -ge 4 ] && [ "$3" = -- ] ;;
        apply_check:apply:--check|apply_index:apply:--index)
            [ "$#" -eq 4 ] && [ "$3" = --whitespace=error-all ] ;;
        index_check:diff:--cached)
            [ "$#" -eq 3 ] && [ "$3" = --check ] ;;
        commit:commit:--quiet)
            [ "$#" -eq 5 ] && [ "$3" = --no-gpg-sign ] && [ "$4" = -m ] ;;
        *) return 1 ;;
    esac
}
if ! matches_target "$@"; then
    exec "$COMPETITION_TEST_REAL_GIT" "$@"
fi
set -- "$COMPETITION_TEST_GIT_EXECUTABLE" -test.run=^TestRepositorySourceGitDiagnosticHelper$ -- "$@"
` + repositoryGitDiagnosticBootstrap

func writeRepositoryGitDiagnosticShim(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte(repositoryGitDiagnosticShim), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// A real Git context mismatch in a pristine checkout is a submission rejection.
func TestRepositoryGitRejectsNonapplyingPatch(t *testing.T) {
	root := t.TempDir()
	init := exec.Command("git", "init", "--quiet", root)
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("frozen source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patchPath := filepath.Join(t.TempDir(), "candidate.patch")
	patch := "diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-other source\n+new source\n"
	if err := os.WriteFile(patchPath, []byte(patch), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"apply", "--check", "--whitespace=error-all", patchPath}
	command := exec.CommandContext(t.Context(), "git", args...)
	command.Dir = root
	command.Env = append(os.Environ(), "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output := &repositoryGitOutput{limit: 4096}
	command.Stdout, command.Stderr = output, output
	runErr := command.Run()
	if runErr == nil {
		t.Fatal("patch unexpectedly applied against the frozen source")
	}
	err := &repositoryApplicationError{cause: repositoryGitFailure(t.Context(), args, command, output, runErr)}
	if got := competitionPatchExitCode(err); got != 42 {
		t.Fatalf("wrong conflict exit: %d (%v); private fixture diagnostic %q", got, err, output.content.String())
	}
	if strings.Contains(err.Error(), "source.txt") {
		t.Fatal("Git path exposed in the public diagnostic")
	}
}

// Infrastructure output cannot borrow the submission rejection classification.
func TestRepositoryGitPatchConflictRefusesAmbiguousOutput(t *testing.T) {
	for _, output := range []string{
		"error: could not open file: Permission denied\n",
		"error: patch failed: a.txt:1\n",
		"error: patch failed: a.txt:1\nerror: b.txt: patch does not apply\n",
		"error: patch failed: a.txt:1\nerror: a.txt: patch does not apply\nfatal: out of memory\n",
	} {
		if repositoryGitPatchConflict(output) {
			t.Errorf("ambiguous error classified as a conflict: %q", output)
		}
	}
}

// The shell replaces itself; native exit/signal and context ownership stay on
// the same process, with no separate launcher left holding the output pipes.
func repositoryGitDiagnosticCommand(ctx context.Context, executable string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-ec", repositoryGitDiagnosticBootstrap,
		"synthetic-git-bootstrap", executable, "-test.run=^TestRepositorySourceGitDiagnosticHelper$", "--")
}

// Only this explicit child role can replace a Git invocation in these tests.
func TestRepositorySourceGitDiagnosticHelper(t *testing.T) {
	mode := os.Getenv("COMPETITION_TEST_GIT_DIAGNOSTIC_MODE")
	if mode == "" {
		return
	}
	var stdout, stderr, nativeStdout, nativeStderr, discard syscall.Stat_t
	if os.Getenv("COMPETITION_TEST_GIT_DIAGNOSTIC_FDS") != "3,4" ||
		syscall.Fstat(1, &stdout) != nil || syscall.Fstat(2, &stderr) != nil ||
		syscall.Fstat(3, &nativeStdout) != nil || syscall.Fstat(4, &nativeStderr) != nil ||
		syscall.Stat("/dev/null", &discard) != nil ||
		stdout.Mode&syscall.S_IFMT != syscall.S_IFCHR || stderr.Mode&syscall.S_IFMT != syscall.S_IFCHR ||
		stdout.Rdev != discard.Rdev || stderr.Rdev != discard.Rdev ||
		nativeStdout.Mode&syscall.S_IFMT != syscall.S_IFIFO || nativeStderr.Mode&syscall.S_IFMT != syscall.S_IFIFO ||
		nativeStdout.Dev != nativeStderr.Dev || nativeStdout.Ino != nativeStderr.Ino ||
		syscall.Dup2(3, 1) != nil || syscall.Dup2(4, 2) != nil ||
		syscall.Close(3) != nil || syscall.Close(4) != nil {
		// A malformed bootstrap must never resemble the deliberate exit1 role.
		os.Exit(96)
	}
	switch mode {
	case "exit1":
		os.Exit(1)
	case "overflow-exit1":
		_, _ = os.Stdout.Write([]byte(strings.Repeat("s", 4096)))
		os.Exit(1)
	case "exit47":
		os.Exit(47)
	case "native-output":
		_, _ = fmt.Fprint(os.Stdout, "SYNTHETIC_GIT_STDOUT\n")
		_, _ = fmt.Fprint(os.Stderr, "SYNTHETIC_GIT_STDERR\n")
		os.Exit(47)
	case "success":
		os.Exit(0)
	case "signal":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		os.Exit(99)
	case "git":
		_, _ = fmt.Fprint(os.Stdout, "SYNTHETIC_OPAQUE_STDOUT /synthetic/private-candidate-path.go")
		_, _ = fmt.Fprint(os.Stderr, "SYNTHETIC_OPAQUE_STDERR argv=--synthetic-secret-argument")
		os.Exit(47)
	default:
		t.Fatal("invalid synthetic diagnostic child mode")
	}
}

// Every fixed command shape, including ls-tree's literal-pathspec offset, has
// an explicit independent expectation; arbitrary argument bytes never print.
func TestRepositorySourceGitDiagnosticsUseClosedPhaseVocabulary(t *testing.T) {
	for _, fixture := range []struct {
		args []string
		want repositoryGitPhase
	}{
		{args: []string{"config", "--local", "--includes", "--name-only", "--get-regexp", `^filter\.`}, want: repositoryGitFilters},
		{args: []string{"rev-parse", "HEAD"}, want: repositoryGitHead},
		{args: []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, want: repositoryGitBranch},
		{args: []string{"status", "--porcelain=v1", "--untracked-files=all"}, want: repositoryGitStatus},
		{args: []string{"rev-parse", "HEAD:connect/sim-latency"}, want: repositoryGitProtectedTree},
		{args: []string{"--literal-pathspecs", "ls-tree", "-z", "--full-tree", strings.Repeat("a", 40), "--", "sentinel-candidate-path"}, want: repositoryGitPatchBase},
		{args: []string{"--literal-pathspecs", "ls-tree", "-z", "--full-tree", "HEAD", "--", "sentinel-helper-path"}, want: repositoryGitHelperTree},
		{args: []string{"--literal-pathspecs", "ls-files", "--stage", "-z", "--", "sentinel-helper-path"}, want: repositoryGitHelperIndex},
		{args: []string{"hash-object", "--no-filters", "--", "sentinel-helper-path"}, want: repositoryGitHelperBytes},
		{args: []string{"apply", "--check", "--whitespace=error-all", "/synthetic/candidate.patch"}, want: repositoryGitApplyCheck},
		{args: []string{"apply", "--index", "--whitespace=error-all", "/synthetic/candidate.patch"}, want: repositoryGitApplyIndex},
		{args: []string{"diff", "--cached", "--check"}, want: repositoryGitIndexCheck},
		{args: []string{"commit", "--quiet", "--no-gpg-sign", "-m", "sentinel-candidate-argument"}, want: repositoryGitCommit},
		{args: []string{"sentinel-unknown-command", "/synthetic/sentinel-secret"}, want: repositoryGitUnknown},
	} {
		if actual := repositoryGitPhaseForArguments(fixture.args); actual != fixture.want {
			t.Fatalf("wrong fixed phase: %v != %v", actual, fixture.want)
		}
	}
	if repositoryGitPhase(999).String() != "unknown" || repositoryGitResult(999).String() != "wait_io" {
		t.Fatal("unknown diagnostic enums gained uncontrolled text")
	}
}

// Real CLI routing and native Git successes reach each injected failure. Public
// error formatting must not print a wrapped candidate path or command output.
func TestCompetitionPatchCliGitDiagnosticsPreserveOpaqueInfrastructureBoundary(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"head_identity", "protected_tree", "protected_helper_tree", "protected_helper_index", "protected_helper_bytes", "patch_base_tree", "apply_check", "apply_index", "index_check", "commit"} {
		fixture := repositoryWorkCoverageFixture(t)
		bin := filepath.Dir(writeRepositoryGitDiagnosticShim(t))
		previousPath := os.Getenv("PATH")
		t.Setenv("PATH", bin+string(os.PathListSeparator)+previousPath)
		t.Setenv("COMPETITION_TEST_REAL_GIT", realGit)
		t.Setenv("COMPETITION_TEST_GIT_EXECUTABLE", executable)
		t.Setenv("COMPETITION_TEST_GIT_DIAGNOSTIC_MODE", "git")
		t.Setenv("COMPETITION_TEST_GIT_DIAGNOSTIC_TARGET", phase)
		patch := []byte("diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
		output, err := repositoryWorkCoverageApply(t, fixture, fixture.policy, patch)
		if err == nil || competitionPatchExitCode(err) != 2 || len(output) != 0 {
			t.Fatal("Git failure changed classification or emitted success", phase, err)
		}
		var diagnostic *repositoryGitOperationError
		var exitError *repositoryGitExitError
		if !errors.As(err, &diagnostic) || !errors.As(err, &exitError) || exitError.exitCode != 47 || diagnostic.phase.String() != phase || diagnostic.result != repositoryGitExited || diagnostic.exitCode != 47 {
			t.Fatal("real Git result/phase was lost", phase, err)
		}
		if !strings.Contains(err.Error(), "phase="+phase+" result=exit exit=47") {
			t.Fatal("opaque CLI diagnostics absent", phase, err)
		}
		for _, private := range []string{"SYNTHETIC_OPAQUE", "source.txt", ".patch", "private-candidate-path", "synthetic-secret-argument", fixture.root, bin} {
			if strings.Contains(err.Error(), private) {
				t.Fatal("Git diagnostic leaked private input", phase, private)
			}
		}
		if err := os.Setenv("PATH", previousPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Unsetenv("COMPETITION_TEST_GIT_DIAGNOSTIC_MODE"); err != nil {
			t.Fatal(err)
		}
	}
}

// Ordinary Git commands remain usable without the synthetic diagnostic child.
func TestRepositoryGitDiagnosticShimKeepsUnrelatedCommandsInNativeGit(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "source with spaces")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	repositoryApplyTestGit(t, root, "init", "--quiet", "--initial-branch=sim-latency")
	repositoryApplyTestGit(t, root, "commit", "--quiet", "--no-gpg-sign", "--allow-empty", "-m", "synthetic native Git checkout")
	head := repositoryApplyTestGit(t, root, "rev-parse", "HEAD")

	shim := writeRepositoryGitDiagnosticShim(t)
	t.Setenv("COMPETITION_TEST_REAL_GIT", realGit)
	t.Setenv("COMPETITION_TEST_GIT_EXECUTABLE", filepath.Join(t.TempDir(), "absent-diagnostic-helper"))
	t.Setenv("COMPETITION_TEST_GIT_DIAGNOSTIC_MODE", "git")
	t.Setenv("COMPETITION_TEST_GIT_DIAGNOSTIC_TARGET", "protected_tree")
	for _, fixture := range []struct {
		args []string
		want string
	}{
		{args: []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, want: "sim-latency"},
		{args: []string{"rev-parse", "HEAD"}, want: head},
	} {
		args := append([]string{"-C", root, "-c", "core.hooksPath=/dev/null"}, fixture.args...)
		output, err := exec.CommandContext(t.Context(), shim, args...).CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) != fixture.want {
			t.Fatalf("unrelated command did not reach native Git: %s: %v: %q", fixture.args[0], err, output)
		}
	}
	filter := exec.CommandContext(t.Context(), shim, "-C", root, "config", "--local", "--includes", "--name-only", "--get-regexp", `^filter\.`)
	output, err := filter.CombinedOutput()
	var native *exec.ExitError
	if !errors.As(err, &native) || native.ExitCode() != 1 || len(output) != 0 {
		t.Fatalf("native no-match filter result changed: %v: %q", err, output)
	}
}

// Native exit status, signals and pre-start cancellation are independently
// observed. Error strings from exec itself must not enter the safe cause chain.
func TestRepositorySourceGitDiagnosticsDistinguishNativeExitSignalAndOwnerStop(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		mode string
		want repositoryGitResult
		exit int
		stop string
	}{
		{mode: "exit47", want: repositoryGitExited, exit: 47},
		{mode: "signal", want: repositoryGitSignaled, exit: -1},
		{mode: "exit47", want: repositoryGitCanceled, exit: -1, stop: "cancel"},
		{mode: "exit47", want: repositoryGitDeadline, exit: -1, stop: "deadline"},
		{mode: "missing", want: repositoryGitStartFailed, exit: -1},
	} {
		ctx := t.Context()
		var stopErr error
		if fixture.stop == "cancel" {
			owned, cancel := context.WithCancel(ctx)
			cancel()
			ctx, stopErr = owned, context.Canceled
		}
		if fixture.stop == "deadline" {
			owned, cancel := context.WithDeadline(ctx, time.Unix(1, 0))
			defer cancel()
			ctx, stopErr = owned, context.DeadlineExceeded
		}
		command := repositoryGitDiagnosticCommand(ctx, executable)
		command.Env = append(os.Environ(), "COMPETITION_TEST_GIT_DIAGNOSTIC_MODE="+fixture.mode)
		if fixture.mode == "missing" {
			command = exec.CommandContext(ctx, filepath.Join(t.TempDir(), "sentinel-private-executable"))
		}
		output := &repositoryGitOutput{limit: 4096}
		command.Stdout, command.Stderr = output, output
		runErr := command.Run()
		if runErr == nil {
			t.Fatal("synthetic native refusal did not fail", fixture.mode)
		}
		failure := repositoryGitFailure(ctx, []string{"apply", "--check", "/synthetic/sentinel.patch"}, command, output, runErr)
		var diagnostic *repositoryGitOperationError
		if !errors.As(failure, &diagnostic) || diagnostic.result != fixture.want || diagnostic.exitCode != fixture.exit || stopErr != nil && !errors.Is(failure, stopErr) {
			t.Fatal("native result/owner stop conflated", fixture.mode, failure)
		}
		if strings.Contains(failure.Error(), "sentinel") || strings.Contains(failure.Error(), executable) {
			t.Fatal("native refusal printed exec input")
		}
	}
}

// The old predicate accepts this exact combination: empty retained buffer and
// native exit 1 masking a writer failure. The new actual gate must reject it.
func TestRepositorySourceFilterNoMatchRejectsMaskedOutputFailure(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := repositoryGitDiagnosticCommand(t.Context(), executable)
	command.Env = append(os.Environ(), "COMPETITION_TEST_GIT_DIAGNOSTIC_MODE=exit1")
	healthyOutput := &repositoryGitOutput{limit: 4096}
	command.Stdout, command.Stderr = healthyOutput, healthyOutput
	runErr := command.Run()
	var native *exec.ExitError
	if !errors.As(runErr, &native) || native.ExitCode() != 1 || healthyOutput.content.Len() != 0 || healthyOutput.overflow {
		t.Fatal("native no-match control did not exit 1", runErr)
	}
	output := &repositoryGitOutput{limit: 1}
	if n, err := output.Write([]byte("synthetic opaque overflow")); n != 0 || err == nil || output.content.Len() != 0 {
		t.Fatal("bounded writer did not exercise empty overflow")
	}
	failure := repositoryGitFailure(t.Context(), []string{"config", "--local", "--get-regexp", `^filter\.`}, command, output, runErr)
	var exitError *repositoryGitExitError
	if !errors.As(failure, &exitError) || exitError.exitCode != 1 || output.content.Len() != 0 {
		t.Fatal("old predicate's causal condition was not reached")
	}
	if repositoryGitFilterAbsent(output.content.String(), failure) {
		t.Fatal("masked output error became no configured filter")
	}
	var diagnostic *repositoryGitOperationError
	if !errors.As(failure, &diagnostic) || diagnostic.result != repositoryGitOutputLimit || diagnostic.exitCode != 1 {
		t.Fatal("output failure lost real native status", failure)
	}
	healthy := repositoryGitFailure(t.Context(), []string{"config", "--local", "--get-regexp", `^filter\.`}, command, &repositoryGitOutput{limit: 1}, runErr)
	if !repositoryGitFilterAbsent("", healthy) || repositoryGitFilterAbsent("synthetic-present-filter", healthy) {
		t.Fatal("genuine empty exit1 or present-filter control changed")
	}
}

// A finite atomic-size pipe write followed by exit1 forces the real Cmd copier
// to overflow; Cmd.Wait's native ExitError takes priority over that writer error.
func TestRepositorySourceGitRealWaitRetainsMaskedOutputFailure(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := repositoryGitDiagnosticCommand(t.Context(), executable)
	command.Env = append(os.Environ(), "COMPETITION_TEST_GIT_DIAGNOSTIC_MODE=overflow-exit1")
	output := &repositoryGitOutput{limit: 1}
	command.Stdout, command.Stderr = output, output
	runErr := command.Run()
	var native *exec.ExitError
	if !output.overflow || !errors.As(runErr, &native) || native.ExitCode() != 1 {
		t.Fatal("real Cmd did not mask its bounded writer fault with exit1", runErr)
	}
	failure := repositoryGitFailure(t.Context(), []string{"config", "--local", "--get-regexp", `^filter\.`}, command, output, runErr)
	var diagnostic *repositoryGitOperationError
	if !errors.As(failure, &diagnostic) || diagnostic.result != repositoryGitOutputLimit || diagnostic.exitCode != 1 || repositoryGitFilterAbsent(output.content.String(), failure) {
		t.Fatal("actual output overflow became an ordinary no-match", failure)
	}
	if strings.Contains(failure.Error(), strings.Repeat("s", 8)) {
		t.Fatal("writer output entered diagnostic")
	}
}

// The unisolated control observes the actual SDK initializer, not a simulated
// log. The fixed child still returns every byte of native stdout and stderr.
func TestRepositorySourceGitDiagnosticChildSeparatesSdkInitializationFromNativeOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	control := exec.CommandContext(t.Context(), "/bin/sh", "-c", "\"$@\" >/dev/null; exit 1",
		"synthetic-init-control", executable, "-test.run=^$")
	controlOutput := &repositoryGitOutput{limit: 64 * 1024}
	control.Stdout, control.Stderr = controlOutput, controlOutput
	controlErr := control.Run()
	var native *exec.ExitError
	if !errors.As(controlErr, &native) || native.ExitCode() != 1 || controlOutput.overflow ||
		!strings.Contains(controlOutput.content.String(), "[env]glog not configured from command line") {
		t.Fatal("unisolated control did not expose SDK initialization before its exit1")
	}
	controlFailure := repositoryGitFailure(t.Context(), []string{"config", "--local", "--get-regexp", `^filter\.`}, control, controlOutput, controlErr)
	if repositoryGitFilterAbsent(controlOutput.content.String(), controlFailure) {
		t.Fatal("SDK initialization became an empty native no-match")
	}
	for _, fixture := range []struct {
		mode string
		want string
		exit int
	}{
		{mode: "exit1", want: "", exit: 1},
		{mode: "native-output", want: "SYNTHETIC_GIT_STDOUT\nSYNTHETIC_GIT_STDERR\n", exit: 47},
	} {
		command := repositoryGitDiagnosticCommand(t.Context(), executable)
		command.Env = append(os.Environ(), "COMPETITION_TEST_GIT_DIAGNOSTIC_MODE="+fixture.mode)
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		runErr := command.Run()
		if !errors.As(runErr, &native) || native.ExitCode() != fixture.exit || output.String() != fixture.want {
			t.Fatal("pre-init isolation lost native output or exit", fixture.mode, runErr)
		}
	}
	for _, bootstrap := range []string{
		"exec 3>&- 4>&-\nexport COMPETITION_TEST_GIT_DIAGNOSTIC_FDS=3,4\nexec \"$@\" >/dev/null 2>&1\n",
		"exec 3</dev/null 4</dev/null\nexport COMPETITION_TEST_GIT_DIAGNOSTIC_FDS=3,4\nexec \"$@\" >/dev/null 2>&1\n",
	} {
		command := exec.CommandContext(t.Context(), "/bin/sh", "-ec", bootstrap,
			"synthetic-invalid-bootstrap", executable, "-test.run=^TestRepositorySourceGitDiagnosticHelper$")
		command.Env = append(os.Environ(), "COMPETITION_TEST_GIT_DIAGNOSTIC_MODE=exit1")
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		if runErr := command.Run(); !errors.As(runErr, &native) || native.ExitCode() != 96 || output.Len() != 0 {
			t.Fatal("missing or redirected inherited pipes resembled native exit1", runErr)
		}
	}
}
