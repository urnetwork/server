// This complete CLI regression compiles against the prior native validator.
// The prior code ignores the genuine raw v3 anchor when both caller markers
// are stripped, and applies an edit to a dedicated instrument helper.
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

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The raw image lock, not the supplied marker/list pair, owns v3 selection.
func TestCompetitionPatchCliRejectsStrippedCoverageIdentityAndPolicy(t *testing.T) {
	root, content, _, original := repositoryApplyFixture(t)
	serverRoot := filepath.Join(root, "server")
	for _, path := range []string{
		"controller/competition_trusted_instrument_approval.go",
		"model/competition_g2_codec_test.go",
		"model/competition_g2_policy.go",
		"model/competition_trusted_instrument.go",
		"model/competition_work_coverage_policy.go",
		"model/competition_work_coverage_policy_test.go",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(serverRoot, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(serverRoot, path), []byte("synthetic original\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	repositoryApplyTestGit(t, serverRoot, "add", "--all")
	repositoryApplyTestGit(t, serverRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic coverage helper base")
	original["server"] = repositoryApplyTestGit(t, serverRoot, "rev-parse", "HEAD")
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	lock, err := json.Marshal(map[string]any{"schema": 1, "development_snapshot": false,
		"candidate_path_base": "mono_root_v1", "repositories": original, "work_coverage_policy": coverage})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".evaluation-source-lock.json"), lock, 0600); err != nil {
		t.Fatal(err)
	}
	var identity map[string]any
	if err := json.Unmarshal(content, &identity); err != nil {
		t.Fatal(err)
	}
	lockDigest := sha256.Sum256(lock)
	identity["source_lock_sha256"] = hex.EncodeToString(lockDigest[:])
	identity["base_sha"], identity["repositories"] = original["server"], original
	delete(identity, "work_coverage_policy")
	content, err = json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	identityDigest := sha256.Sum256(content)
	patch := []byte("diff --git a/server/model/competition_g2_policy.go b/server/model/competition_g2_policy.go\n--- a/server/model/competition_g2_policy.go\n+++ b/server/model/competition_g2_policy.go\n@@ -1 +1 @@\n-synthetic original\n+synthetic changed\n")
	policy, err := json.Marshal(controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096,
		AllowedPaths: []string{"server/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}})
	if err != nil {
		t.Fatal(err)
	}
	patchDigest, policyDigest := sha256.Sum256(patch), sha256.Sum256(policy)
	inputs := t.TempDir()
	patchPath := writeTestFile(t, inputs, "candidate.patch", patch)
	policyPath := writeTestFile(t, inputs, "policy.json", policy)
	identityPath := writeTestFile(t, inputs, "original-source.json", content)
	var output bytes.Buffer
	err = run([]string{"--base-sha", original["server"], "--patch", patchPath,
		"--expected-patch-sha256", hex.EncodeToString(patchDigest[:]), "--policy", policyPath,
		"--expected-policy-sha256", hex.EncodeToString(policyDigest[:]), "--apply-source-root", root,
		"--source-identity", identityPath, "--expected-source-identity-sha256", hex.EncodeToString(identityDigest[:])}, &output)
	if err == nil {
		t.Fatal("trusted v3 raw lock accepted helper edit after identity and policy markers were stripped")
	}
	if competitionPatchExitCode(err) != 2 || !strings.Contains(err.Error(), "coverage") || output.Len() != 0 {
		t.Fatal("stripped-marker refusal came from an unrelated setup failure or emitted a receipt", err)
	}
	if repositoryApplyTestGit(t, serverRoot, "rev-parse", "HEAD") != original["server"] {
		t.Fatal("coverage refusal occurred after source mutation")
	}
}
