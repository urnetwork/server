// Sealing restores read-only modes from committed objects, never incidental
// filesystem execute bits. The fixture executes Git only, not candidate scripts.
package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Both adding and removing incidental execute bits must converge to the exact
// tracked modes without dirtying the resulting immutable build source.
func TestSourceBuildSealsExecutableBitsFromGitTree(t *testing.T) {
	root := t.TempDir()
	commits := map[string]string{}
	for _, repository := range sourceRepositoryNames() {
		commits[repository] = sourceTestRepository(t, root, repository)
	}
	repository := filepath.Join(root, "connect")
	script := filepath.Join(repository, "script.sh")
	if err := os.WriteFile(script, []byte("synthetic text; never executed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "add", "--", "script.sh")
	sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic executable source")
	commits["connect"] = sourceTestGit(t, repository, "rev-parse", "HEAD")
	if err := os.Chmod(script, 0o600); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(repository, "source.txt")
	if err := os.Chmod(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	if err := sealSourceBuildTree(context.Background(), root, commits, defaultSourceBuildSandboxSettings()); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		path string
		mode os.FileMode
	}{
		{path: script, mode: 0o555}, {path: plain, mode: 0o444},
	} {
		info, err := os.Lstat(fixture.path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != fixture.mode {
			t.Fatalf("sealed mode %s: %v %v", fixture.path, info, err)
		}
	}
	for _, repository := range sourceRepositoryNames() {
		if status := sourceTestGit(t, filepath.Join(root, repository), "--no-optional-locks", "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
			t.Fatal("sealed source is dirty", repository, status)
		}
	}
}
