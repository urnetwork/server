package main

// Fixture commits retain the user's existing global author and committer.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Refuse overrides before invoking Git: the negative control must never make
// a fixture commit under an identity different from the configured global one.
func TestSourceFixtureGitUsesGlobalIdentity(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	global := func(key string) string {
		output, err := exec.Command(realGit, "config", "--global", "--get", key).Output()
		if err != nil || strings.TrimSpace(string(output)) == "" {
			t.Fatalf("fixture commits require an existing global %s: %v", key, err)
		}
		return strings.TrimSpace(string(output))
	}
	name, email := global("user.name"), global("user.email")
	tools := t.TempDir()
	quotedGit := "'" + strings.ReplaceAll(realGit, "'", "'\"'\"'") + "'"
	wrapper := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
if [[ -n ${GIT_AUTHOR_NAME:-}${GIT_AUTHOR_EMAIL:-}${GIT_COMMITTER_NAME:-}${GIT_COMMITTER_EMAIL:-} ]]; then
  echo 'fixture supplied a forbidden author or committer override' >&2
  exit 91
fi
for argument in "$@"; do
  case "$argument" in
    user.name|user.email|user.name=*|user.email=*|-cuser.name=*|-cuser.email=*)
      echo 'fixture supplied a forbidden local identity configuration' >&2
      exit 92
      ;;
  esac
done
exec %s "$@"
`, quotedGit)
	if err := os.WriteFile(filepath.Join(tools, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	repositories := t.TempDir()
	sourceTestRepository(t, repositories, "identity")
	repository := filepath.Join(repositories, "identity")
	actual := sourceTestGit(t, repository, "show", "-s", "--format=%an%n%ae%n%cn%n%ce", "HEAD")
	if actual != strings.Join([]string{name, email, name, email}, "\n") {
		t.Fatal("fixture author or committer differs from the existing global identity")
	}
	command := exec.Command(realGit, "-C", repository, "config", "--local", "--get-regexp", `^user\.(name|email)$`)
	_, err = command.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatal("fixture added a repository-local author identity", err)
	}
	// The real remote-preflight fixture also advances a branch. Its commit
	// arguments must preserve the same identity, beyond the shared helper.
	TestLaunchPreflightVerifiesRemoteEpochWithoutChangingWorkingTrees(t)
}
