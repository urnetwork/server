//go:build linux

// Completed evidence has the same private compiler-cache boundary as failures.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// Execute real retention/validation functions; only unmount is an inert rename.
func TestEvaluatorSuccessEvidenceExcludesPrivateCompilerCache(t *testing.T) {
	for _, maliciousOutput := range []bool{false, true} {
		root := t.TempDir()
		source := filepath.Join(root, ".evidence-runtime")
		cache := filepath.Join(source, "tracked-build", "check-cache")
		if err := os.MkdirAll(cache, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, "private-cache"), []byte("synthetic-private-cache"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cache, 0000); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "candidate-build.log"), []byte("synthetic completed compiler log"), 0600); err != nil {
			t.Fatal(err)
		}
		if maliciousOutput {
			path := filepath.Join(source, "runs", "candidate-01", "output", "tracked-build")
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(path, "not-evidence"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-R", "u+rwX", root).Run() })
		helper, err := filepath.Abs(filepath.Join("evaluator", "container", "subevaluation-ownership.sh"))
		if err != nil {
			t.Fatal(err)
		}
		evaluator := filepath.Join("evaluator", "container", "evaluator-subevaluation.sh")
		body := `set -Eeuo pipefail
source "$1"
subevaluation_scoped=true
work_dir="$2"
artifact_dir="$3"
active_work_mount="$work_dir"
TEST_RETIRED="$4"
mkdir -p "$artifact_dir"
die() { printf '%s\n' "$*" >&2; exit 1; }
`
		body += g2ScriptFunction(t, evaluator, "validate_output_tree") + "\n" + g2ScriptFunction(t, evaluator, "persist_evidence_tree") + "\n"
		body += `evaluation_host() { [[ $1 = unmount-evidence ]]; mv "$runtime_work_dir" "$TEST_RETIRED"; mkdir "$runtime_work_dir"; }
persist_evidence_tree
`
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body, "retention-test", helper, source, filepath.Join(root, "artifacts"), filepath.Join(root, "retired"))
		command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
		output, err := command.CombinedOutput()
		if maliciousOutput {
			if err == nil {
				t.Fatal("nested output named tracked-build bypassed validation")
			}
			continue
		}
		if err != nil {
			t.Fatalf("completed evidence retention failed: %v: %s", err, output)
		}
		evidence := filepath.Join(root, "artifacts", "evidence")
		if _, err := os.Lstat(filepath.Join(evidence, "tracked-build")); !os.IsNotExist(err) {
			t.Fatalf("private compiler cache archived: %v", err)
		}
		content, err := os.ReadFile(filepath.Join(evidence, "candidate-build.log"))
		if err != nil || string(content) != "synthetic completed compiler log" {
			t.Fatalf("diagnostic lost: %v %q", err, content)
		}
		info, err := os.Stat(filepath.Join(root, "retired", "tracked-build", "check-cache"))
		if err != nil || info.Mode().Perm() != 0000 {
			t.Fatalf("cache permissions widened: %v", err)
		}
	}
}
