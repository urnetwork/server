// Local modules inside the enclosing repository inherit its exact source pin;
// canonical paths alone cannot grant custody to moving files or symlink targets.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A real disposable repository keeps the replacement module wholly synthetic.
func sourceInTreeGraphFixture(t *testing.T, content string) (string, string) {
	t.Helper()
	root := t.TempDir()
	sourceTestRepository(t, root, "server")
	repository := filepath.Join(root, "server")
	module := "module module.example/server\n\ngo 1.23.0\n\nrequire module.example/dependency v0.0.0\n\nreplace module.example/dependency => ./third_party/library\n"
	if err := os.WriteFile(filepath.Join(repository, "go.mod"), []byte(module), 0600); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		path := filepath.Join(repository, "third_party", "library", "go.mod")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sourceTestGit(t, repository, "add", "--all")
	sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic in-tree module")
	return repository, sourceTestGit(t, repository, "rev-parse", "HEAD")
}

// The repository owner does not become an eleventh separately pinned checkout.
func TestSourceGraphInTreeModuleUsesEnclosingFrozenRepository(t *testing.T) {
	repository, commit := sourceInTreeGraphFixture(t, "module module.example/dependency\n\ngo 1.23.0\n")
	for _, staging := range []bool{false, true} {
		if err := verifyFrozenSourceGraphForMode(t.Context(), repository, commit, staging); err != nil {
			t.Fatal("committed in-tree module rejected", err)
		}
	}
	module, err := sourceModuleGraphAtCommit(t.Context(), repository, commit)
	if err != nil {
		t.Fatal(err)
	}
	owners, err := requiredLocalSourceRepositories(module)
	if err != nil || !slices.Equal(owners, []string{"server"}) {
		t.Fatal("in-tree module invented an external source owner", owners, err)
	}
}

// Go's filesystem-local replacements permit a fork's different module line;
// source custody must not impose a stronger module-name rule than the toolchain.
func TestSourceGraphInTreeModuleAcceptsDifferentDeclaredForkPath(t *testing.T) {
	repository, commit := sourceInTreeGraphFixture(t, "module module.example/fork\n\ngo 1.23.0\n")
	if err := verifyFrozenSourceGraph(t.Context(), repository, commit); err != nil {
		t.Fatal(err)
	}
}

// Neither a later worktree file nor a moving newer commit supplies missing pins.
func TestSourceGraphInTreeModuleRejectsUncommittedReplacementFile(t *testing.T) {
	repository, commit := sourceInTreeGraphFixture(t, "")
	path := filepath.Join(repository, "third_party", "library", "go.mod")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("module module.example/dependency\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyFrozenSourceGraph(t.Context(), repository, commit); err == nil || !strings.Contains(err.Error(), "no regular go.mod in frozen source") {
		t.Fatal("moving file supplied missing frozen module", err)
	}
	sourceTestGit(t, repository, "add", "--all")
	sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic later module")
	if err := verifyFrozenSourceGraph(t.Context(), repository, commit); err == nil {
		t.Fatal("newer commit replaced the selected source pin")
	}
}

// Frozen module bytes remain authoritative after moving worktree corruption.
func TestSourceGraphInTreeModuleIgnoresMovingWorktreeContents(t *testing.T) {
	repository, commit := sourceInTreeGraphFixture(t, "module module.example/dependency\n")
	if err := os.WriteFile(filepath.Join(repository, "third_party", "library", "go.mod"), []byte("not a valid module file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyFrozenSourceGraph(t.Context(), repository, commit); err != nil {
		t.Fatal("moving checkout contaminated frozen module custody", err)
	}
}

// A symlinked module file or directory never grants custody of its target.
func TestSourceGraphInTreeModuleRejectsSymlinkedFileAndAncestor(t *testing.T) {
	for _, directoryLink := range []bool{false, true} {
		repository, _ := sourceInTreeGraphFixture(t, "")
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "go.mod"), []byte("module module.example/dependency\n"), 0600); err != nil {
			t.Fatal(err)
		}
		parent := filepath.Join(repository, "third_party")
		if err := os.MkdirAll(parent, 0700); err != nil {
			t.Fatal(err)
		}
		if directoryLink {
			if err := os.Symlink(outside, filepath.Join(parent, "library")); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.MkdirAll(filepath.Join(parent, "library"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "go.mod"), filepath.Join(parent, "library", "go.mod")); err != nil {
				t.Fatal(err)
			}
		}
		sourceTestGit(t, repository, "add", "--all")
		sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic module alias")
		commit := sourceTestGit(t, repository, "rev-parse", "HEAD")
		if err := verifyFrozenSourceGraph(t.Context(), repository, commit); err == nil || !strings.Contains(err.Error(), "no regular go.mod in frozen source") {
			t.Fatal("module alias acquired source custody", directoryLink, err)
		}
	}
}

// Syntax is checked from the selected blob without resolving or executing code.
func TestSourceGraphInTreeModuleRejectsMalformedCommittedModule(t *testing.T) {
	repository, commit := sourceInTreeGraphFixture(t, "module (\n")
	if err := verifyFrozenSourceGraph(t.Context(), repository, commit); err == nil || !strings.Contains(err.Error(), "parse frozen in-tree module") {
		t.Fatal("malformed pinned module accepted", err)
	}
}

// Nested replacements cannot escape or alias their enclosing repository owner.
func TestSourceGraphInTreeModuleRejectsNoncanonicalPaths(t *testing.T) {
	for _, path := range []string{"./", "./.", "./..", "./third_party/../library", "./third_party//library", "./third_party/./library", "./third_party/library/", "./third_party/../../outside", "./third_party\\library", "./third_party\tlibrary"} {
		module := sourceModuleGraph{
			Module:  sourceModuleVersion{Path: "module.example/server"},
			Require: []sourceModuleVersion{{Path: "module.example/dependency", Version: "v0.0.0"}},
			Replace: []sourceModuleReplacement{{Old: sourceModuleVersion{Path: "module.example/dependency"}, New: sourceModuleVersion{Path: path}}},
		}
		content, err := json.Marshal(module)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := requiredLocalSourceRepositories(content); err == nil {
			t.Errorf("noncanonical local module path accepted: %q", path)
		}
	}
}
