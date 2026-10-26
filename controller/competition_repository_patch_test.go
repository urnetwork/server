// Repository routing tests use synthetic text patches and no local repositories.
package controller

import (
	"bytes"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// Broad policy matches the explicit locked graph, with independent runner denial.
func competitionRepositoryTestPolicy() PatchPolicy {
	policy := PatchPolicy{PathBase: competitionMonoRootPathBase, MaxPatchBytes: 262144, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		policy.AllowedPaths = append(policy.AllowedPaths, repository+"/**")
	}
	return policy
}

// Text files need not be Go files when all product repositories are editable.
func TestCompetitionRepositoryPolicyAllowsEveryLockedProductRepository(t *testing.T) {
	policy := competitionRepositoryTestPolicy()
	if err := validateCompetitionAllowedPaths(policy); err != nil {
		t.Fatal(err)
	}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		for _, relative := range []string{"product.go", "go.mod", "docs/product.md", "nested/Makefile"} {
			if !pathAllowed(repository+"/"+relative, policy) {
				t.Fatalf("locked product path refused: %s/%s", repository, relative)
			}
		}
	}
}

// Broad allowlists cannot bypass evaluator, traversal or Git metadata protection.
func TestCompetitionRepositoryPolicyProtectsRunnerAndMetadata(t *testing.T) {
	policy := competitionRepositoryTestPolicy()
	for _, value := range []string{
		"server/connect/sim-latency/sim.go", "server/connect/sim-latency/evaluator/container/evaluator.sh",
		"connect/.git/config", "connect/nested/.GiT/hooks/pre-commit", "connect/../server/db.go",
		"unknown/product.go", "operator-proxy/product.go", "connect//product.go", "connect", "/connect/product.go",
	} {
		if pathAllowed(value, policy) {
			t.Fatalf("unsafe path accepted: %q", value)
		}
	}
}

// Header rewriting must not rewrite source strings that resemble Git headers.
func TestCompetitionRepositoryPatchRoutesOnlyStructuralHeaders(t *testing.T) {
	raw := "diff --git a/connect/product.go b/connect/product.go\n--- a/connect/product.go\n+++ b/connect/product.go\n@@ -1 +1 @@\n--- a/connect/literal\n+++ b/connect/literal\n" +
		"diff --git a/sdk/product.go b/sdk/product.go\n--- a/sdk/product.go\n+++ b/sdk/product.go\n@@ -1 +1 @@\n-old\n+new\n"
	patches, evalError := SplitCompetitionRepositoryPatch(raw, competitionRepositoryTestPolicy())
	if evalError != nil {
		t.Fatal(evalError)
	}
	if len(patches) != 2 || !strings.HasPrefix(string(patches["connect"]), "diff --git a/product.go b/product.go\n--- a/product.go\n+++ b/product.go\n") || !bytes.Contains(patches["connect"], []byte("\n--- a/connect/literal\n+++ b/connect/literal\n")) || !strings.HasPrefix(string(patches["sdk"]), "diff --git a/product.go b/product.go\n") {
		t.Fatalf("incorrect repository split: %q", patches)
	}
}

// Server's nested connect directory is not the separate connect repository.
func TestCompetitionRepositoryPatchSeparatesServerAndConnectRoots(t *testing.T) {
	for _, value := range []string{"connect/product.go", "server/connect/product.go"} {
		repository, relative, err := competitionRepositoryPath(value)
		if err != nil || (repository == "connect" && relative != "product.go") || (repository == "server" && relative != "connect/product.go") {
			t.Fatalf("ambiguous root routing: %q %q %q %v", value, repository, relative, err)
		}
	}
}

// Empty mode keeps historical patch bytes and digest inputs unchanged.
func TestCompetitionRepositoryPatchPreservesLegacyBytes(t *testing.T) {
	raw := "diff --git a/product.go b/product.go\n--- a/product.go\n+++ b/product.go\n@@ -1 +1 @@\n-old\n+new\n"
	policy := PatchPolicy{MaxPatchBytes: 262144, AllowedPaths: []string{"product.go"}, ForbiddenPaths: []string{protectedSimulatorTreePattern}}
	patches, evalError := SplitCompetitionRepositoryPatch(raw, policy)
	if evalError != nil || len(patches) != 1 || !bytes.Equal(patches["server"], []byte(raw)) {
		t.Fatalf("legacy patch changed: %q %v", patches, evalError)
	}
	policy.PathBase = "unknown_mode"
	if _, evalError := SplitCompetitionRepositoryPatch(raw, policy); evalError == nil {
		t.Fatal("unknown path mode accepted")
	}
}

// Invalid root and wildcard syntax fail policy validation, not only submission.
func TestCompetitionRepositoryPolicyRejectsAmbiguousAllowlist(t *testing.T) {
	for _, value := range []string{"**", "connect/*", "unknown/**", "server/connect/sim-latency/**", "connect/../server/**"} {
		policy := competitionRepositoryTestPolicy()
		policy.AllowedPaths = []string{value}
		if err := validateCompetitionAllowedPaths(policy); err == nil {
			t.Fatalf("ambiguous allowlist accepted: %q", value)
		}
	}
}
