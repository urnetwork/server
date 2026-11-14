// Actual fixture commits inherit the user's existing global Git identity.
package perfvar

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The wrapper rejects overrides before any Git command can write identity or
// create a commit. Successful commits remain real, and their identities are read
// from Git afterward through the original executable.
func guardFixtureGitIdentity(t *testing.T) func(int) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	read := func(root string, args ...string) string {
		t.Helper()
		command := exec.Command(realGit, append([]string{"-C", root}, args...)...)
		raw, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("read fixture identity: %v: %s", err, raw)
		}
		return strings.TrimSuffix(string(raw), "\n")
	}
	queryRoot := t.TempDir()
	name := read(queryRoot, "config", "--global", "--get", "user.name")
	email := read(queryRoot, "config", "--global", "--get", "user.email")
	if name == "" || email == "" {
		t.Fatal("fixture requires an existing global Git identity")
	}
	tools := t.TempDir()
	commitLog := filepath.Join(tools, "commits")
	global, globalSet := os.LookupEnv("GIT_CONFIG_GLOBAL")
	globalPresence := ""
	if globalSet {
		globalPresence = "x"
	}
	const wrapper = `#!/bin/sh
set -eu
if [ "${GIT_AUTHOR_NAME+x}${GIT_AUTHOR_EMAIL+x}${GIT_COMMITTER_NAME+x}${GIT_COMMITTER_EMAIL+x}" != "" ]; then
 echo 'fixture identity override refused before Git execution' >&2
 exit 97
fi
if [ "${GIT_CONFIG_GLOBAL+x}" != "$SERVER_FIXTURE_GLOBAL_PRESENT" ] || [ "${GIT_CONFIG_GLOBAL-}" != "$SERVER_FIXTURE_GLOBAL_VALUE" ]; then
 echo 'fixture global identity suppression refused before Git execution' >&2
 exit 97
fi
repository=''
command=''
previous=''
for argument in "$@"; do
 case "$argument" in
  user.name|user.email|user.name=*|user.email=*|-cuser.name=*|-cuser.email=*|--author|--author=*|--config-env=user.name=*|--config-env=user.email=*)
   echo 'fixture identity configuration refused before Git execution' >&2
   exit 97
   ;;
 esac
 if [ "$previous" = '-C' ]; then
  repository=$argument
  previous=''
  continue
 fi
 if [ "$previous" = '-c' ]; then
  previous=''
  continue
 fi
 case "$argument" in
  -C|-c) previous=$argument;;
  -*) ;;
  *) if [ "$command" = '' ]; then command=$argument; fi;;
 esac
done
"$SERVER_FIXTURE_REAL_GIT" "$@"
if [ "$command" = commit ]; then
 if [ "$repository" = '' ]; then echo 'fixture commit has no explicit repository' >&2; exit 98; fi
 printf '%s\000' "$repository" >> "$SERVER_FIXTURE_COMMIT_LOG"
fi
`
	if err := os.WriteFile(filepath.Join(tools, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SERVER_FIXTURE_REAL_GIT", realGit)
	t.Setenv("SERVER_FIXTURE_COMMIT_LOG", commitLog)
	t.Setenv("SERVER_FIXTURE_GLOBAL_PRESENT", globalPresence)
	t.Setenv("SERVER_FIXTURE_GLOBAL_VALUE", global)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(want int) {
		t.Helper()
		raw, err := os.ReadFile(commitLog)
		if err != nil {
			t.Fatal("fixture did not create its actual commits", err)
		}
		roots := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		if len(roots) != want {
			t.Fatalf("actual fixture commits=%d, want=%d", len(roots), want)
		}
		for _, root := range roots {
			identity := strings.Split(read(root, "show", "-s", "--format=%an%x00%ae%x00%cn%x00%ce", "HEAD"), "\x00")
			if !slices.Equal(identity, []string{name, email, name, email}) {
				t.Fatal("actual fixture author or committer differs from the existing global identity")
			}
			command := exec.Command(realGit, "-C", root, "config", "--local", "--get-regexp", `^user\.(name|email)$`)
			output, err := command.CombinedOutput()
			var status *exec.ExitError
			if !errors.As(err, &status) || status.ExitCode() != 1 || len(output) != 0 {
				t.Fatalf("fixture has repository-local identity: %v: %s", err, output)
			}
		}
	}
}

// Run the existing source fixture and all its original assertions under the
// pre-execution guard, then check its actual commit objects and local config.
func TestPerfvarGitFixtureUsesGlobalIdentity(t *testing.T) {
	requireIdentity := guardFixtureGitIdentity(t)
	TestPerfvarGitStateHashesDirtyContents(t)
	requireIdentity(1)
}
