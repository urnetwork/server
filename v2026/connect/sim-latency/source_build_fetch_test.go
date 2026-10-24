// A complete approved tree may live in a partial checkout without old commit
// objects. Only that tree, never unrelated source history, is needed to build.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSourceBuildFetchDoesNotRequireMissingParentHistory(t *testing.T) {
	root := t.TempDir()
	parent := sourceTestRepository(t, root, "server")
	source := filepath.Join(root, "server")
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("approved synthetic tree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, source, "add", "source.txt")
	sourceTestGit(t, source, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic approved tree")
	approved := sourceTestGit(t, source, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(source, ".git", "objects", parent[:2], parent[2:])); err != nil {
		t.Fatal(err)
	}
	// Force the pre-fix failure without a network, promisor remote or sleeps.
	legacy := exec.Command("git", "-c", "core.hooksPath=/dev/null", "clone", "--quiet", "--no-checkout", "--no-local", "--no-hardlinks", source, filepath.Join(root, "legacy"))
	if output, err := legacy.CombinedOutput(); err == nil {
		t.Fatalf("fixture did not reproduce full-clone missing-history failure: %s", output)
	}
	destination := filepath.Join(root, "approved")
	if err := cloneSourceBuildRepository(context.Background(), "server", source, destination, approved); err != nil {
		t.Fatal("approved complete tree was rejected for unrelated history", err)
	}
	if got := sourceTestGit(t, destination, "rev-parse", "HEAD"); got != approved {
		t.Fatal("shallow fetch changed approved identity", got)
	}
	for _, relative := range []string{".git/FETCH_HEAD", ".git/objects/info/alternates"} {
		if _, err := os.Lstat(filepath.Join(destination, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Fatal("private transport metadata escaped into build context", relative, err)
		}
	}
}
