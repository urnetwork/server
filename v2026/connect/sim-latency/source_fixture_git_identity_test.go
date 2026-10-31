// Synthetic fixture identities come from explicit global configuration, never
// from command-level overrides that hide a filter test's leaked environment.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A different configured identity detects helper-level overrides without using
// an operator's real name/email or depending on the surrounding host.
func TestSourceGitFixtureRetainsExplicitGlobalIdentity(t *testing.T) {
	global := filepath.Join(t.TempDir(), "global-gitconfig")
	if err := os.WriteFile(global, []byte("[user]\nname = Synthetic Configured\nemail = global@account.example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
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
		t.Fatal("configured-global fixture did not commit its source")
	}
	identity := sourceTestGit(t, filepath.Join(root, "connect"), "show", "-s", "--format=%an%x00%ae%x00%cn%x00%ce", "HEAD")
	if identity != "Synthetic Configured\x00global@account.example\x00Synthetic Configured\x00global@account.example" {
		t.Fatalf("fixture changed its explicitly configured global identity: %q", identity)
	}
}
