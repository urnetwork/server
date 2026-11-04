// Real Git runs only in a synthetic private repository. Its deliberately
// executable worktree hooks must never become a prospective host process.
package competitionagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluatorScopedGitDisablesWorktreeHooksAndFsmonitor(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "synthetic-source")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	runGit := func(arguments ...string) {
		command := exec.Command("git", append([]string{"-C", repository, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, arguments...)...)
		command.Env = append(controlEnvironment(), "HOME="+directory, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal("synthetic Git fixture failed", err, string(output))
		}
	}
	runGit("init", "--quiet")
	runGit("config", "user.name", "Synthetic Source")
	runGit("config", "user.email", "source@account.example")
	if err := os.WriteFile(filepath.Join(repository, "source.txt"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "--", "source.txt")
	runGit("commit", "--quiet", "--no-gpg-sign", "-m", "synthetic base")
	hooks := filepath.Join(repository, ".candidate-hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	program := []byte("#!/bin/sh\nprintf 'unexpected worktree execution\\n' >> \"$SYNTHETIC_GIT_EXECUTION_MARKER\"\nexit 1\n")
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), program, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "candidate-fsmonitor"), program, 0700); err != nil {
		t.Fatal(err)
	}
	runGit("config", "core.hooksPath", ".candidate-hooks")
	runGit("config", "core.fsmonitor", "./candidate-fsmonitor")
	if err := os.WriteFile(filepath.Join(repository, "source.txt"), []byte("improved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "worktree-executed")
	command := exec.Command("bash", "-eu", "-c", `source "$1"
subevaluation_ownership_init
evaluation_git -C "$2" status --porcelain=v1 --untracked-files=all
evaluation_git -C "$2" add --update
evaluation_git -C "$2" commit --quiet --no-gpg-sign -m 'synthetic checked patch'
`, "git-routing-test", evaluatorRoutingHelper(t), repository)
	command.Env = append(controlEnvironment(), "HOME="+directory, "SYNTHETIC_GIT_EXECUTION_MARKER="+marker,
		"URNETWORK_SUBEVALUATION_RUN_ID="+strings.Repeat("a", 64), "URNETWORK_SUBEVALUATION_JOB_ID=00000000-0000-0000-0000-000000000001",
		"URNETWORK_SUBEVALUATION_ROUND_ID=00000000-0000-0000-0000-000000000002", "URNETWORK_SUBEVALUATION_ATTEMPT=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("checked Git operation invoked worktree hooks or fsmonitor", err, string(output))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("worktree-controlled executable escaped its container boundary", err)
	}
}

func TestEvaluatorScopedGitRejectsConfiguredContentFiltersBeforeRead(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "synthetic-source")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"init", "--quiet"}, {"config", "filter.synthetic.clean", "./candidate-filter"}} {
		command := exec.Command("git", append([]string{"-C", repository, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, arguments...)...)
		command.Env = append(controlEnvironment(), "HOME="+directory, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal("synthetic content-filter fixture failed", err, string(output))
		}
	}
	command := exec.Command("bash", "-eu", "-c", `source "$1"; subevaluation_ownership_init; evaluation_git -C "$2" status --porcelain=v1`, "git-filter-test", evaluatorRoutingHelper(t), repository)
	command.Env = append(controlEnvironment(), "HOME="+directory, "URNETWORK_SUBEVALUATION_RUN_ID="+strings.Repeat("a", 64),
		"URNETWORK_SUBEVALUATION_JOB_ID=00000000-0000-0000-0000-000000000001", "URNETWORK_SUBEVALUATION_ROUND_ID=00000000-0000-0000-0000-000000000002", "URNETWORK_SUBEVALUATION_ATTEMPT=1")
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "unqualified Git content filter") {
		t.Fatal("content-filter metadata reached a host worktree read", err, string(output))
	}
}
