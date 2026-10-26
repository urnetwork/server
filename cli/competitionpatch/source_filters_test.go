// Configured local Git filters are an infrastructure qualification error, not
// permission to execute candidate-selected attribute programs on the host.
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

// The synchronous native control arms a harmless filter. The real trusted CLI
// must then refuse before even its pristine status check can invoke that filter.
func TestCompetitionPatchCliRefusesLocalAttributeFiltersBeforeStatus(t *testing.T) {
	root, identityBytes, _, original := repositoryApplyFixture(t)
	repository := filepath.Join(root, "connect")
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("source.txt filter=synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repositoryApplyTestGit(t, repository, "add", "--", ".gitattributes")
	repositoryApplyTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic source attributes")
	original["connect"] = repositoryApplyTestGit(t, repository, "rev-parse", "HEAD")
	inputs := t.TempDir()
	marker, program := filepath.Join(inputs, "filter-called"), filepath.Join(inputs, "filter")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf 'invoked\\n' > \"$COMPETITION_FILTER_TEST_MARKER\"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPETITION_FILTER_TEST_MARKER", marker)
	for _, key := range []string{"filter.synthetic.clean", "filter.synthetic.smudge"} {
		repositoryApplyTestGit(t, repository, "config", key, program)
	}
	repositoryApplyTestGit(t, repository, "add", "--renormalize", "--", "source.txt")
	if content, err := os.ReadFile(marker); err != nil || string(content) != "invoked\n" {
		t.Fatal("native filter counterfactual did not invoke the tripwire", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	var identity repositorySourceIdentity
	if err := json.Unmarshal(identityBytes, &identity); err != nil {
		t.Fatal(err)
	}
	identity.Repositories = original
	identityBytes, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	identityDigest := sha256.Sum256(identityBytes)
	policy := []byte(`{"path_base":"mono_root_v1","max_patch_bytes":4096,"allowed_paths":["connect/**"],"forbidden_paths":["server/connect/sim-latency/**"]}`)
	patch := []byte("diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
	patchDigest, policyDigest := sha256.Sum256(patch), sha256.Sum256(policy)
	patchPath := writeTestFile(t, inputs, "canonical.patch", patch)
	policyPath := writeTestFile(t, inputs, "policy.json", policy)
	identityPath := writeTestFile(t, inputs, "original-source.json", identityBytes)
	var output bytes.Buffer
	err = run([]string{"--base-sha", original["server"], "--policy", policyPath,
		"--expected-policy-sha256", hex.EncodeToString(policyDigest[:]), "--patch", patchPath,
		"--expected-patch-sha256", hex.EncodeToString(patchDigest[:]), "--apply-source-root", root,
		"--source-identity", identityPath, "--expected-source-identity-sha256", hex.EncodeToString(identityDigest[:])}, &output)
	if err == nil || competitionPatchExitCode(err) != 2 || err.Error() != "trusted source application failed: source repository differs from its pristine frozen identity" || output.Len() != 0 {
		t.Fatalf("configured filter reached source application: %v %q", err, output.String())
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("trusted source preparation executed a configured local filter", err)
	}
	if head := repositoryApplyTestGit(t, repository, "rev-parse", "HEAD"); head != original["connect"] {
		t.Fatal("filter refusal changed the source head")
	}
}
