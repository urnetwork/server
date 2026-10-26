// Fixtures reproduce the preparer's private root without weakening admission.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A private ancestor does not make TempDir's umask-filtered leaf private.
func TestRepositorySourceFixtureHasPrivateCanonicalRoot(t *testing.T) {
	root, _, _, _ := repositoryApplyFixture(t)
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("source fixture root must be exactly 0700, got %v", info.Mode())
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(root) || canonical != root {
		t.Fatalf("source fixture root is not canonical: root=%q canonical=%q error=%v", root, canonical, err)
	}
}

// Explicit modes make refusals independent of the test process's own umask.
func TestRepositorySourceRejectsNonPrivateAndRedirectedRoots(t *testing.T) {
	for _, fixture := range []struct {
		name string
		mode os.FileMode
		link bool
	}{
		{name: "world-readable root", mode: 0o755},
		{name: "group-readable root", mode: 0o750},
		{name: "redirected private root", mode: 0o700, link: true},
	} {
		root, content, digest, original := repositoryApplyFixture(t)
		if err := os.Chmod(root, fixture.mode); err != nil {
			t.Fatal(err)
		}
		inputRoot := root
		if fixture.link {
			inputRoot = filepath.Join(t.TempDir(), "redirected-source")
			if err := os.Symlink(root, inputRoot); err != nil {
				t.Fatal(err)
			}
		}
		patch := []byte("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
		commits, err := applyRepositoryPatches(inputRoot, content, digest, original["server"], strings.Repeat("c", 64), "mono_root_v1", map[string][]byte{"connect": patch}, defaultRepositoryApplySettings())
		if err == nil || err.Error() != "source root is not a private canonical directory" || commits != nil {
			t.Errorf("%s: invalid source root returned commits=%v error=%v", fixture.name, commits, err)
		}
		for repository, expected := range original {
			if head := repositoryApplyTestGit(t, filepath.Join(root, repository), "rev-parse", "HEAD"); head != expected {
				t.Errorf("%s refusal moved repository %s", fixture.name, repository)
			}
		}
		if status := repositoryApplyTestGit(t, filepath.Join(root, "connect"), "status", "--porcelain=v1"); status != "" {
			t.Errorf("%s refusal mutated the candidate worktree: %s", fixture.name, status)
		}
	}
}
