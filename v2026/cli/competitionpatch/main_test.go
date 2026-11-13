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

// The composed CLI authenticates the original patch and binds each routed file.
func TestRunWritesPrivateAuthenticatedRepositoryPatches(t *testing.T) {
	root := t.TempDir()
	policy := []byte(`{"path_base":"mono_root_v1","max_patch_bytes":4096,"allowed_paths":["connect/**","sdk/**"],"forbidden_paths":["server/connect/sim-latency/**"]}`)
	patch := []byte("diff --git a/connect/example.go b/connect/example.go\n--- a/connect/example.go\n+++ b/connect/example.go\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/sdk/example.go b/sdk/example.go\n--- a/sdk/example.go\n+++ b/sdk/example.go\n@@ -1 +1 @@\n-old\n+new\n")
	policyPath := writeTestFile(t, root, "policy.json", policy)
	patchPath := writeTestFile(t, root, "canonical.patch", patch)
	policyDigest, patchDigest := sha256.Sum256(policy), sha256.Sum256(patch)
	directory := filepath.Join(root, "routed")
	var output bytes.Buffer
	err := run([]string{"--base-sha", strings.Repeat("a", 40), "--policy", policyPath,
		"--expected-policy-sha256", hex.EncodeToString(policyDigest[:]), "--patch", patchPath,
		"--expected-patch-sha256", hex.EncodeToString(patchDigest[:]), "--repository-patch-directory", directory}, &output)
	if err != nil {
		t.Fatal(err)
	}
	var identity imageIdentity
	if err := json.Unmarshal(output.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	if len(identity.RepositoryPatches) != 2 || identity.PatchSha256 != hex.EncodeToString(patchDigest[:]) {
		t.Fatalf("incomplete original/routed identity: %#v", identity)
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("output is not private: %v", err)
	}
	for _, repository := range []string{"connect", "sdk"} {
		binding := identity.RepositoryPatches[repository]
		content, err := os.ReadFile(filepath.Join(directory, binding.Path))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		info, err := os.Lstat(filepath.Join(directory, binding.Path))
		if err != nil || info.Mode().Perm() != 0600 || len(content) != binding.Bytes || hex.EncodeToString(digest[:]) != binding.Sha256 || !bytes.HasPrefix(content, []byte("diff --git a/example.go b/example.go\n")) {
			t.Fatalf("routed bytes or permissions unbound: %s %v", repository, err)
		}
	}
}

// Re-execution must retain the first output, never replace its evidence.
func TestRepositoryPatchWriterRefusesExistingDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "routed")
	patches := map[string][]byte{"connect": []byte("first\n")}
	if _, err := writeRepositoryPatches(directory, patches); err != nil {
		t.Fatal(err)
	}
	if _, err := writeRepositoryPatches(directory, map[string][]byte{"connect": []byte("replacement\n")}); err == nil {
		t.Fatal("existing routed evidence overwritten")
	}
	content, err := os.ReadFile(filepath.Join(directory, "connect.patch"))
	if err != nil || string(content) != "first\n" {
		t.Fatalf("original evidence changed: %q %v", content, err)
	}
}

// A canonical-looking path with a redirected ancestor is still unsafe.
func TestRepositoryPatchWriterRefusesSymlinkParent(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := writeRepositoryPatches(filepath.Join(alias, "routed"), map[string][]byte{"connect": []byte("patch\n")}); err == nil {
		t.Fatal("redirected parent accepted")
	}
	entries, err := os.ReadDir(actual)
	if err != nil || len(entries) != 0 {
		t.Fatalf("refused output still wrote through alias: %v", err)
	}
}
