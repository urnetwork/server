//go:build linux

// Retain diagnostics without traversing independently owned compiler caches.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The real sanitizer runs on tmpfs with an unreadable cache and real secrets.
// Cache ownership/permissions stay unchanged; useful build logs survive.
func TestEvaluatorFailureEvidenceExcludesPrivateCompilerCache(t *testing.T) {
	root, err := os.MkdirTemp("/dev/shm", "synthetic-failure-cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-R", "u+rwX", root).Run(); _ = os.RemoveAll(root) })
	source := filepath.Join(root, ".evidence-runtime")
	cache := filepath.Join(source, "tracked-build", "check-cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "private-cache"), []byte("synthetic-cache-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cache, 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "candidate-build.log"), []byte("synthetic compiler failure"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "runner.env"), []byte("synthetic-throwaway-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("evaluator", "container", "retain-failure-evidence.sh"))
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "sudo"), []byte("#!/bin/sh\nexit 77\n"), 0700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "failed-evidence")
	command := exec.CommandContext(t.Context(), "/bin/bash", script, source, destination)
	command.Env = append([]string{"PATH=" + shim + ":/usr/bin:/bin", "FAILURE_JOB_ID=11111111-2222-4333-8444-555555555555", "FAILURE_ROUND_ID=66666666-7777-4888-8999-000000000000", "FAILURE_ATTEMPT=1", "FAILURE_EXIT_CODE=2", "FAILURE_EVALUATOR_LINE=123"}, subevaluationOwnershipEnvironment()...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sanitizer failed: %v: %s", err, output)
	}
	log, err := os.ReadFile(filepath.Join(destination, "candidate-build.log"))
	if err != nil || string(log) != "synthetic compiler failure" {
		t.Fatalf("diagnostic lost: %v %q", err, log)
	}
	for _, path := range []string{"tracked-build", "runner.env"} {
		if _, err := os.Lstat(filepath.Join(destination, path)); !os.IsNotExist(err) {
			t.Fatalf("private build input archived: %s: %v", path, err)
		}
	}
	info, err := os.Stat(cache)
	if err != nil || info.Mode().Perm() != 0000 {
		t.Fatalf("cache permissions widened: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "failed-evidence-manifest.json"))
	if err != nil || !strings.Contains(string(manifest), "candidate-build.log") || strings.Contains(string(manifest), "tracked-build") {
		t.Fatalf("manifest includes cache or loses logs: %v: %s", err, manifest)
	}
}
