// This file verifies the host-side digest helper against the manifest contract
// used by the evaluator container.
package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Resolve macOS's temporary-directory symlink so the shell helper receives the
// absolute canonical path required by its security boundary.
func canonicalTemporaryDirectory(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// The shell manifest must match bytewise digesting for sorted relative paths,
// including a filename that sha256sum would otherwise escape in its output.
func TestHashLocalMountScriptIsDeterministicAndFilenameSafe(t *testing.T) {
	root := canonicalTemporaryDirectory(t)
	contentsByRelativePath := map[string]string{
		`back\slash.yml`: "backslash: 1\n",
		"nested/a.yml":   "alpha: 1\n",
		"z.yml":          "zulu: 1\n",
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	relativePaths := make([]string, 0, len(contentsByRelativePath))
	for relativePath, contents := range contentsByRelativePath {
		if err := os.WriteFile(filepath.Join(root, relativePath), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		relativePaths = append(relativePaths, relativePath)
	}
	sort.Strings(relativePaths)
	manifestHash := sha256.New()
	for _, relativePath := range relativePaths {
		contentHash := sha256.Sum256([]byte(contentsByRelativePath[relativePath]))
		if _, err := fmt.Fprintf(manifestHash, "%x  %s\n", contentHash, relativePath); err != nil {
			t.Fatal(err)
		}
	}
	want := fmt.Sprintf("%x", manifestHash.Sum(nil))
	command := exec.Command("./evaluator/container/hash-local-mount.sh", root)
	command.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("hash local mount: %v: %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != want {
		t.Fatalf("local mount digest = %q, want %q", got, want)
	}
}

// A link can redirect an authenticated path after validation and must make the
// shell helper fail closed.
func TestHashLocalMountScriptRejectsSymbolicLinks(t *testing.T) {
	root := canonicalTemporaryDirectory(t)
	targetPath := filepath.Join(root, "target.yml")
	if err := os.WriteFile(targetPath, []byte("target: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, filepath.Join(root, "linked.yml")); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(
		"./evaluator/container/hash-local-mount.sh",
		root,
	).CombinedOutput(); err == nil {
		t.Fatalf("local mount containing a symbolic link was accepted: %q", output)
	}
}

// Newline and carriage-return path components cannot be represented by the
// line-oriented authenticated manifest.
func TestHashLocalMountScriptRejectsUnsafePaths(t *testing.T) {
	root := canonicalTemporaryDirectory(t)
	if err := os.WriteFile(filepath.Join(root, "unsafe\npath.yml"), []byte("unsafe: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(
		"./evaluator/container/hash-local-mount.sh",
		root,
	).CombinedOutput(); err == nil {
		t.Fatalf("local mount containing an unsafe path was accepted: %q", output)
	}
}
