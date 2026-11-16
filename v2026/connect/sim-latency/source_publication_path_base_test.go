// Real synthetic per-repository objects exercise ordinary mono-root source
// publication after instrument adoption, without pushing or qualifying images.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Two genuinely changed dependency repositories share one exact approved patch.
// The prospective epoch-three source inherits minimum-work, not a second
// instrument-adoption proof; per-repository patch authority remains mandatory.
func sourceMultiRepositoryPublicationFixture(t *testing.T) (*sourcePublicationIntent, *gitSourcePublicationBackend) {
	t.Helper()
	root := t.TempDir()
	policy := controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096, AllowedPaths: []string{"connect/**", "sdk/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
	rawPatch := "diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-connect\n+improved connect\n" +
		"diff --git a/sdk/source.txt b/sdk/source.txt\n--- a/sdk/source.txt\n+++ b/sdk/source.txt\n@@ -1 +1 @@\n-sdk\n+improved sdk\n"
	patch, err := controller.ValidateAndCanonicalizePatch(rawPatch, policy)
	if err != nil {
		t.Fatal(err)
	}
	patchPath := filepath.Join(root, "approved-product.patch")
	if err := os.WriteFile(patchPath, patch.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	proposal := reviewedSourceProposal{CandidatePathBase: policy.PathBase, PatchSha256: patch.Sha256}
	activation := model.CompetitionSourceActivation{TargetEpoch: 3, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork,
		Kind: "reviewed_source_promotion", CandidatePathBase: proposal.CandidatePathBase}
	intent := &sourcePublicationIntent{Schema: 2, ActivationGeneration: 3, SourceDecisionDigest: strings.Repeat("c", 64), ManifestSha256: strings.Repeat("d", 64),
		ApprovedPatchSha256: proposal.PatchSha256, ApprovedPatchPath: patchPath, CandidatePathBase: activation.CandidatePathBase}
	for _, name := range append(sourceRepositoryNames(), "config") {
		branch, expected, commit := "sim-latency", strings.Repeat("a", 40), strings.Repeat("a", 40)
		stagedRoot := filepath.Join(root, name)
		if name == "connect" || name == "sdk" {
			expected = sourceTestRepository(t, root, name)
			if err := os.WriteFile(filepath.Join(stagedRoot, "source.txt"), []byte("improved "+name+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sourceTestGit(t, stagedRoot, "add", "source.txt")
			sourceTestGit(t, stagedRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic approved dependency")
			commit = sourceTestGit(t, stagedRoot, "rev-parse", "HEAD")
		} else if name == "config" {
			branch, commit = "main", strings.Repeat("b", 40)
		}
		intent.Refs = append(intent.Refs, sourcePublicationRef{Repository: name, Branch: branch, ExpectedCommit: expected, Commit: commit, StagedRoot: stagedRoot})
	}
	return intent, &gitSourcePublicationBackend{patchPolicy: policy}
}

// Ordinary epoch-three promotion verifies both changed repositories with their
// original exact patch sections and parents, without inventing another proof.
func TestSourcePublicationInheritedMinimumWorkPromotesMultipleRepositories(t *testing.T) {
	intent, backend := sourceMultiRepositoryPublicationFixture(t)
	if intent.TrustedInstrument != nil || intent.ActivationGeneration != 3 {
		t.Fatal("inherited policy acquired a fictitious instrument adoption")
	}
	if err := validateSourcePublicationIntent(intent); err != nil {
		t.Fatal(err)
	}
	verified := 0
	for _, ref := range intent.Refs {
		if ref.Repository == "connect" || ref.Repository == "sdk" {
			if err := backend.Verify(context.Background(), ref, intent); err != nil {
				t.Fatalf("exact approved %s change was rejected: %v", ref.Repository, err)
			}
			verified++
		}
	}
	if verified != 2 {
		t.Fatal("not every changed repository was independently verified")
	}
}

// The new optional flag is not authority: legacy omission is server-only and
// an asserted mono mode must still match the authenticated evaluated policy.
func TestSourcePublicationPathBaseCannotGrantItsOwnAuthority(t *testing.T) {
	intent, backend := sourceMultiRepositoryPublicationFixture(t)
	var dependency sourcePublicationRef
	for _, ref := range intent.Refs {
		if ref.Repository == "connect" {
			dependency = ref
		}
	}
	intent.CandidatePathBase = ""
	if err := validateSourcePublicationIntent(intent); err == nil {
		t.Fatal("legacy server-only publication admitted dependency changes")
	}
	if err := backend.Verify(context.Background(), dependency, intent); err == nil {
		t.Fatal("missing evaluated mono mode reached dependency publication")
	}
	intent.CandidatePathBase = "unsupported_path_base"
	if err := validateSourcePublicationIntent(intent); err == nil {
		t.Fatal("unknown publication path mode was accepted")
	}
	intent.CandidatePathBase = "mono_root_v1"
	backend.patchPolicy.PathBase = ""
	if err := backend.Verify(context.Background(), dependency, intent); err == nil {
		t.Fatal("self-reported mono mode overrode the authenticated legacy policy")
	}
}

// A clean single-parent commit is insufficient: additional changes in either
// dependency must exactly reconstruct from that repository's approved section.
func TestSourcePublicationRejectsAdditionalDependencyChanges(t *testing.T) {
	for _, repository := range []string{"connect", "sdk"} {
		intent, backend := sourceMultiRepositoryPublicationFixture(t)
		for _, ref := range intent.Refs {
			if ref.Repository != repository {
				continue
			}
			if err := os.WriteFile(filepath.Join(ref.StagedRoot, "extra.txt"), []byte("synthetic unapproved dependency change\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sourceTestGit(t, ref.StagedRoot, "add", "extra.txt")
			sourceTestGit(t, ref.StagedRoot, "commit", "--amend", "--quiet", "--no-gpg-sign", "--no-edit")
			ref.Commit = sourceTestGit(t, ref.StagedRoot, "rev-parse", "HEAD")
			if err := backend.Verify(context.Background(), ref, intent); err == nil {
				t.Fatalf("unapproved %s change was hidden by mono publication mode", repository)
			}
		}
	}
}

// Rehashed noncanonical bytes cannot replace the exact canonical approval,
// and an extra commit cannot hide behind an otherwise identical patch tree.
func TestSourcePublicationRejectsNoncanonicalPatchAndExtraParent(t *testing.T) {
	intent, backend := sourceMultiRepositoryPublicationFixture(t)
	var dependency sourcePublicationRef
	for _, ref := range intent.Refs {
		if ref.Repository == "sdk" {
			dependency = ref
		}
	}
	original, err := os.ReadFile(intent.ApprovedPatchPath)
	if err != nil {
		t.Fatal(err)
	}
	noncanonical := append([]byte("\n"), original...)
	if err := os.WriteFile(intent.ApprovedPatchPath, noncanonical, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(noncanonical)
	intent.ApprovedPatchSha256 = hex.EncodeToString(digest[:])
	if err := backend.Verify(context.Background(), dependency, intent); err == nil {
		t.Fatal("rehashed noncanonical approval bytes authorized a dependency")
	}
	if err := os.WriteFile(intent.ApprovedPatchPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	digest = sha256.Sum256(original)
	intent.ApprovedPatchSha256 = hex.EncodeToString(digest[:])
	sourceTestGit(t, dependency.StagedRoot, "commit", "--quiet", "--no-gpg-sign", "--allow-empty", "-m", "synthetic extra lineage")
	dependency.Commit = sourceTestGit(t, dependency.StagedRoot, "rev-parse", "HEAD")
	if err := backend.Verify(context.Background(), dependency, intent); err == nil {
		t.Fatal("multi-commit dependency lineage bypassed the exact one-child reviewed product guard")
	}
}
