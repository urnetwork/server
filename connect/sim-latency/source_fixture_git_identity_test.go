// Synthetic fixture identities come from explicit global configuration, never
// from command-level overrides that hide a filter test's leaked environment.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Tests that create commits explicitly install a private global identity. The
// shared Git helpers still inherit that configuration without actor overrides.
func sourceTestPrivateGlobalIdentity(t *testing.T, name string, email string) {
	t.Helper()
	global := filepath.Join(t.TempDir(), "global-gitconfig")
	configuration := fmt.Sprintf("[user]\nname = %s\nemail = %s\n", name, email)
	if err := os.WriteFile(global, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	for _, variable := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(variable, "")
		if err := os.Unsetenv(variable); err != nil {
			t.Fatal(err)
		}
	}
}

// A different configured identity detects helper-level overrides without using
// an operator's real name/email or depending on the surrounding host.
func TestSourceGitFixtureRetainsExplicitGlobalIdentity(t *testing.T) {
	sourceTestPrivateGlobalIdentity(t, "Synthetic Configured", "global@account.example")
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
