// New/deleted text operations traverse the real canonical validator, repository
// router and trusted Git applier without Docker, network or candidate execution.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Both ordinary text and standard empty-file metadata must produce one direct
// candidate commit per touched repository while leaving server at its base.
func TestCompetitionPatchCliAppliesTextAndEmptyFileCreationDeletion(t *testing.T) {
	for _, empty := range []bool{false, true} {
		root, content, identityDigest, original := repositoryApplyFixture(t)
		deletedPath := "source.txt"
		creation := "new file mode 100644\n--- /dev/null\n+++ b/connect/new.txt\n@@ -0,0 +1 @@\n+created\n"
		deletion := "deleted file mode 100644\n--- a/sdk/source.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-original\n"
		if empty {
			deletedPath = "empty.txt"
			if err := os.WriteFile(filepath.Join(root, "sdk", deletedPath), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			repositoryApplyTestGit(t, filepath.Join(root, "sdk"), "add", "--", deletedPath)
			repositoryApplyTestGit(t, filepath.Join(root, "sdk"), "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic empty base")
			original["sdk"] = repositoryApplyTestGit(t, filepath.Join(root, "sdk"), "rev-parse", "HEAD")
			var identity repositorySourceIdentity
			if err := json.Unmarshal(content, &identity); err != nil {
				t.Fatal(err)
			}
			identity.Repositories = original
			var err error
			content, err = json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content)
			identityDigest = hex.EncodeToString(digest[:])
			creation = "new file mode 100644\nindex 0000000..e69de29\n"
			deletion = "deleted file mode 100644\nindex e69de29..0000000\n"
		}
		patch := []byte("diff --git a/connect/new.txt b/connect/new.txt\n" + creation + "diff --git a/sdk/" + deletedPath + " b/sdk/" + deletedPath + "\n" + deletion)
		policy := []byte(`{"path_base":"mono_root_v1","max_patch_bytes":4096,"allowed_paths":["connect/**","sdk/**"],"forbidden_paths":["server/connect/sim-latency/**"]}`)
		policyDigest, patchDigest := sha256.Sum256(policy), sha256.Sum256(patch)
		inputs := t.TempDir()
		patchPath := writeTestFile(t, inputs, "canonical.patch", patch)
		policyPath := writeTestFile(t, inputs, "policy.json", policy)
		identityPath := writeTestFile(t, inputs, "original-source.json", content)
		var output bytes.Buffer
		if err := run([]string{"--base-sha", original["server"], "--policy", policyPath,
			"--expected-policy-sha256", hex.EncodeToString(policyDigest[:]), "--patch", patchPath,
			"--expected-patch-sha256", hex.EncodeToString(patchDigest[:]), "--apply-source-root", root,
			"--source-identity", identityPath, "--expected-source-identity-sha256", identityDigest}, &output); err != nil {
			t.Fatalf("empty=%t: %v", empty, err)
		}
		var receipt imageIdentity
		if err := json.Unmarshal(output.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if len(receipt.CandidateCommits) != len(original) || receipt.CandidateCommits["server"] != original["server"] {
			t.Fatal("file operations changed the frozen server or lost a sibling identity")
		}
		for _, repository := range []string{"connect", "sdk"} {
			path := filepath.Join(root, repository)
			if receipt.CandidateCommits[repository] == original[repository] || repositoryApplyTestGit(t, path, "rev-parse", "HEAD^") != original[repository] || repositoryApplyTestGit(t, path, "status", "--porcelain=v1", "--untracked-files=all") != "" {
				t.Fatalf("%s operation did not make one clean direct candidate commit", repository)
			}
		}
		created, err := os.ReadFile(filepath.Join(root, "connect", "new.txt"))
		if err != nil || empty && len(created) != 0 || !empty && string(created) != "created\n" {
			t.Fatalf("empty=%t: created bytes=%q error=%v", empty, created, err)
		}
		if _, err := os.Lstat(filepath.Join(root, "sdk", deletedPath)); !os.IsNotExist(err) {
			t.Fatal("deleted candidate file survived", err)
		}
	}
}
