// Trusted application failures are infrastructure, and broad application is
// possible only when both the frozen policy and image explicitly support it.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Even a pristine complete old graph cannot gain broad authority from a new
// worker policy. Refusal precedes every Git mutation.
func TestRepositorySourceRequiresProspectiveBroadImageCapability(t *testing.T) {
	for _, capability := range []string{"", "unsupported"} {
		root, content, _, original := repositoryApplyFixture(t)
		var identity repositorySourceIdentity
		if err := json.Unmarshal(content, &identity); err != nil {
			t.Fatal(err)
		}
		identity.CandidatePathBase = capability
		content, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		patch := []byte("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
		if _, err := applyRepositoryPatches(root, content, hex.EncodeToString(digest[:]), original["server"], strings.Repeat("c", 64), "mono_root_v1", map[string][]byte{"connect": patch}, defaultRepositoryApplySettings()); err == nil {
			t.Errorf("broad application accepted image capability %q", capability)
		}
		for repository, commit := range original {
			if actual := repositoryApplyTestGit(t, filepath.Join(root, repository), "rev-parse", "HEAD"); actual != commit {
				t.Fatalf("refusal changed %s", repository)
			}
		}
	}
}

// Image capability is not policy authority; a historical server-relative
// policy cannot use it to change an independently locked dependency.
func TestRepositorySourceBroadImageDoesNotRelabelLegacyPolicy(t *testing.T) {
	root, content, digest, original := repositoryApplyFixture(t)
	patch := []byte("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
	if _, err := applyRepositoryPatches(root, content, digest, original["server"], strings.Repeat("c", 64), "", map[string][]byte{"connect": patch}, defaultRepositoryApplySettings()); err == nil {
		t.Fatal("new image capability widened a legacy policy")
	}
	commits, err := applyRepositoryPatches(root, content, digest, original["server"], strings.Repeat("c", 64), "", map[string][]byte{"server": patch}, defaultRepositoryApplySettings())
	if err != nil || commits["server"] == original["server"] || commits["connect"] != original["connect"] {
		t.Fatalf("legacy application contract changed: %v %v", commits, err)
	}
}

// The actual CLI screens canonical bytes before source access. It never
// converts an absent source receipt into a candidate rejection.
func TestCompetitionPatchCliSeparatesCanonicalAndInfrastructureExits(t *testing.T) {
	root := t.TempDir()
	policy := []byte(`{"max_patch_bytes":4096,"allowed_paths":["source.txt"],"forbidden_paths":["connect/sim-latency/**"]}`)
	patch := []byte("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
	policyDigest, patchDigest := sha256.Sum256(policy), sha256.Sum256(patch)
	policyPath := writeTestFile(t, root, "policy.json", policy)
	patchPath := writeTestFile(t, root, "canonical.patch", patch)
	args := []string{"--base-sha", strings.Repeat("a", 40), "--policy", policyPath,
		"--expected-policy-sha256", hex.EncodeToString(policyDigest[:]), "--patch", patchPath,
		"--expected-patch-sha256", hex.EncodeToString(patchDigest[:]), "--apply-source-root", root,
		"--source-identity", filepath.Join(root, "absent.json"), "--expected-source-identity-sha256", strings.Repeat("b", 64)}
	var output bytes.Buffer
	err := run(args, &output)
	if err == nil || competitionPatchExitCode(err) != 2 || output.Len() != 0 {
		t.Fatalf("source failure was misclassified or emitted success: %v %q", err, output.String())
	}
	if err := os.WriteFile(patchPath, []byte("not a canonical patch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err = run(args, &output)
	if err == nil || competitionPatchExitCode(err) != 1 || output.Len() != 0 {
		t.Fatalf("canonical refusal was misclassified or emitted success: %v %q", err, output.String())
	}
}

// A successful source commit cannot be reported as candidate wrongdoing merely
// because the owning tool could not retain its receipt.
type sourceReceiptFailureWriter struct{}

// This is an injected infrastructure error, never candidate command output.
func (self sourceReceiptFailureWriter) Write(content []byte) (int, error) {
	return 0, errors.New("synthetic receipt sink failure")
}

// Exercises the real canonical validator, routed patch, local Git application,
// complete candidate map, and typed output failure through the CLI entrypoint.
func TestCompetitionPatchCliBindsSourceApplicationAndReceiptFailures(t *testing.T) {
	for _, fixture := range []struct {
		oldLine     string
		failReceipt bool
		wantExit    int
	}{
		{oldLine: "original"},
		{oldLine: "not-present", wantExit: 2},
		{oldLine: "original", failReceipt: true, wantExit: 2},
	} {
		root, content, identityDigest, original := repositoryApplyFixture(t)
		inputs := t.TempDir()
		policy := []byte(`{"path_base":"mono_root_v1","max_patch_bytes":4096,"allowed_paths":["connect/**"],"forbidden_paths":["server/connect/sim-latency/**"]}`)
		patch := []byte("diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-" + fixture.oldLine + "\n+changed\n")
		policyDigest, patchDigest := sha256.Sum256(policy), sha256.Sum256(patch)
		policyPath := writeTestFile(t, inputs, "policy.json", policy)
		patchPath := writeTestFile(t, inputs, "canonical.patch", patch)
		identityPath := writeTestFile(t, inputs, "original-source.json", content)
		var output bytes.Buffer
		var writer io.Writer = &output
		if fixture.failReceipt {
			writer = sourceReceiptFailureWriter{}
		}
		err := run([]string{"--base-sha", original["server"], "--policy", policyPath,
			"--expected-policy-sha256", hex.EncodeToString(policyDigest[:]), "--patch", patchPath,
			"--expected-patch-sha256", hex.EncodeToString(patchDigest[:]), "--apply-source-root", root,
			"--source-identity", identityPath, "--expected-source-identity-sha256", identityDigest}, writer)
		if fixture.wantExit != 0 {
			if err == nil || competitionPatchExitCode(err) != fixture.wantExit || output.Len() != 0 {
				t.Fatalf("old=%q receipt-failure=%t: exit=%v output=%q", fixture.oldLine, fixture.failReceipt, err, output.String())
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var identity imageIdentity
		if err := json.Unmarshal(output.Bytes(), &identity); err != nil {
			t.Fatal(err)
		}
		if len(identity.CandidateCommits) != len(original) || identity.CandidateCommits["server"] != original["server"] || identity.CandidateCommits["connect"] == original["connect"] {
			t.Fatalf("dependency-only application returned an incomplete identity: %+v", identity)
		}
	}
}
