package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestRunRequiresPinnedPatchDigest(t *testing.T) {
	err := run(context.Background(), []string{
		"--round_id", server.NewId().String(),
		"--patch", "/trusted/noop.patch",
		"--output", "/trusted/rebaseline.json",
	})
	if err == nil || !strings.Contains(err.Error(), "--patch_sha256") {
		t.Fatalf("missing patch digest error = %v", err)
	}
}

func TestWriteExclusiveJsonSealsEvidence(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "rebaseline.json")
	value := map[string]any{"schema": 1, "passed": true}
	if err := writeExclusiveJson(path, value); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0400 {
		t.Fatalf("output mode = %v", info.Mode())
	}
	var decoded map[string]any
	bytes, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(bytes, &decoded) != nil || decoded["passed"] != true {
		t.Fatalf("output = %s, error = %v", bytes, err)
	}
	if err := writeExclusiveJson(path, value); err == nil {
		t.Fatal("exclusive evidence was overwritten")
	}
}

func TestWriteExclusiveJsonRejectsUnsafeParent(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0777); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusiveJson(
		filepath.Join(directory, "rebaseline.json"),
		map[string]int{"schema": 1},
	); err == nil {
		t.Fatal("group/world-writable output parent accepted")
	}
	if err := writeExclusiveJson("relative.json", map[string]int{"schema": 1}); err == nil {
		t.Fatal("relative output path accepted")
	}
}

func TestReadPatchRejectsAliasesAndOversize(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "noop.patch")
	if err := os.WriteFile(path, []byte("patch\n"), 0400); err != nil {
		t.Fatal(err)
	}
	bytes, err := readPatch(path, 6)
	if err != nil || string(bytes) != "patch\n" {
		t.Fatalf("patch = %q, error = %v", bytes, err)
	}
	if _, err := readPatch(path, 5); err == nil {
		t.Fatal("oversized patch accepted")
	}
	alias := filepath.Join(directory, "alias.patch")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readPatch(alias, 6); err == nil {
		t.Fatal("symbolic-link patch accepted")
	}
	if _, err := readPatch(directory+"/./noop.patch", 6); err == nil {
		t.Fatal("non-canonical patch path accepted")
	}
}
