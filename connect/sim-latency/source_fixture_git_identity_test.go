// Synthetic source fixtures cannot inherit an operator's name or email and
// must still commit when a filter test replaces global Git configuration.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Explicitly absent author variables and global/system config force the old
// fixture's missing-identity failure without relying on the host's own setup.
func TestSourceGitFixtureDoesNotRequireAmbientIdentity(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	commit := sourceTestRepository(t, root, "connect")
	if !sourceGitShaPattern.MatchString(commit) {
		t.Fatal("identity-independent fixture did not commit its source")
	}
	identity := sourceTestGit(t, filepath.Join(root, "connect"), "show", "-s", "--format=%an%x00%ae%x00%cn%x00%ce", "HEAD")
	if identity != "Synthetic Source\x00source@account.example\x00Synthetic Source\x00source@account.example" {
		t.Fatalf("fixture retained ambient author identity: %q", identity)
	}
}
