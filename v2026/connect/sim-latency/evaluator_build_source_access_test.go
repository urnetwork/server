//go:build linux

// The real pre-build boundary must publish readonly source to a different uid.
package main

import (
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Extract the actual call before cache lookup/first candidate code. Owner-only
// copies cannot be read through a bind by the container's remapped principal.
func TestEvaluatorBuildSourceReadableAndSealedBeforeCandidate(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "build-submission.sh"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(content), "candidate_source_identity_sha256=\"")
	if start < 0 {
		t.Fatal("missing source identity boundary")
	}
	fragment := string(content)[start:]
	end := strings.Index(fragment, "\nimage_key=")
	if end < 0 {
		t.Fatal("missing build admission boundary")
	}
	fragment = fragment[:end]
	helper, err := filepath.Abs(filepath.Join("evaluator", "container", "subevaluation-ownership.sh"))
	if err != nil {
		t.Fatal(err)
	}
	outer := t.TempDir()
	if err := os.Chmod(outer, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(outer, "candidate")
	repository := filepath.Join(root, "server")
	if err := os.MkdirAll(repository, 0700); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(root, ".evaluation-source.json")
	if err := os.WriteFile(identity, []byte("synthetic identity"), 0400); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(repository, "tool")
	if err := os.WriteFile(tool, []byte("synthetic executable"), 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-R", "u+rwX", root).Run() })
	command := exec.CommandContext(t.Context(), "/bin/bash", "-c", `set -Eeuo pipefail
source "$1"
subevaluation_scoped=true
source_root="$2"
source_identity="$source_root/.evaluation-source.json"
infrastructure_failure() { printf '%s\n' "$*" >&2; exit 1; }
`+fragment, "source-access-test", helper, root)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("source handoff: %v: %s", err, output)
	}
	for _, path := range []string{root, repository, tool} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0555 {
			t.Fatalf("source is not readable and sealed: %s: %v, %v", path, info, err)
		}
	}
	info, err := os.Stat(identity)
	if err != nil || info.Mode().Perm() != 0444 {
		t.Fatalf("source identity not readable by remapped uid: %v", err)
	}
	info, err = os.Stat(outer)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("private ancestor permissions changed: %v", err)
	}
	after, err := os.ReadFile(identity)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatalf("source identity bytes changed: %v", err)
	}
}
