package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAuthenticatesPatchAndPolicy(t *testing.T) {
	root := t.TempDir()
	policy := []byte(`{"max_patch_bytes":4096,"allowed_paths":["connect/*.go"],"forbidden_paths":["connect/sim-latency/**"]}` + "\n")
	patch := []byte("diff --git a/connect/example.go b/connect/example.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/connect/example.go\n" +
		"+++ b/connect/example.go\n" +
		"@@ -1 +1 @@\n" +
		"-package old\n" +
		"+package example\n")
	policyPath := writeTestFile(t, root, "policy.json", policy)
	patchPath := writeTestFile(t, root, "canonical.patch", patch)
	policyDigest := sha256.Sum256(policy)
	patchDigest := sha256.Sum256(patch)
	var output bytes.Buffer
	err := run([]string{
		"--base-sha", strings.Repeat("a", 40),
		"--policy", policyPath,
		"--expected-policy-sha256", hex.EncodeToString(policyDigest[:]),
		"--patch", patchPath,
		"--expected-patch-sha256", hex.EncodeToString(patchDigest[:]),
	}, &output)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var identity imageIdentity
	if err := json.Unmarshal(output.Bytes(), &identity); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if identity.PatchSha256 != hex.EncodeToString(patchDigest[:]) || len(identity.Paths) != 1 || identity.Paths[0] != "connect/example.go" {
		t.Fatalf("unexpected identity: %#v", identity)
	}
}

func TestRunRejectsPolicyDigestMismatch(t *testing.T) {
	root := t.TempDir()
	policyPath := writeTestFile(t, root, "policy.json", []byte(`{"max_patch_bytes":1,"allowed_paths":["x"],"forbidden_paths":["y"]}`))
	patchPath := writeTestFile(t, root, "canonical.patch", []byte("x"))
	err := run([]string{
		"--base-sha", strings.Repeat("a", 40),
		"--policy", policyPath,
		"--expected-policy-sha256", strings.Repeat("0", 64),
		"--patch", patchPath,
		"--expected-patch-sha256", strings.Repeat("0", 64),
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "policy SHA-256 mismatch") {
		t.Fatalf("error = %v, want policy digest mismatch", err)
	}
}

func TestRunRejectsProtectedSimulatorPathEvenWhenPolicyAllowsIt(t *testing.T) {
	root := t.TempDir()
	filePath := "connect/sim-latency/main.go"
	policy := []byte(`{"max_patch_bytes":4096,"allowed_paths":["connect/sim-latency/main.go"],"forbidden_paths":["unrelated/**"]}` + "\n")
	patch := []byte("diff --git a/" + filePath + " b/" + filePath + "\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/" + filePath + "\n" +
		"+++ b/" + filePath + "\n" +
		"@@ -1 +1 @@\n" +
		"-package old\n" +
		"+package main\n")
	policyPath := writeTestFile(t, root, "policy.json", policy)
	patchPath := writeTestFile(t, root, "canonical.patch", patch)
	policyDigest := sha256.Sum256(policy)
	patchDigest := sha256.Sum256(patch)
	err := run([]string{
		"--base-sha", strings.Repeat("a", 40),
		"--policy", policyPath,
		"--expected-policy-sha256", hex.EncodeToString(policyDigest[:]),
		"--patch", patchPath,
		"--expected-patch-sha256", hex.EncodeToString(patchDigest[:]),
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "validate canonical patch: path_not_allowed") {
		t.Fatalf("error = %v, want protected simulator path rejection", err)
	}
}

func writeTestFile(t *testing.T, root string, name string, bytes []byte) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
