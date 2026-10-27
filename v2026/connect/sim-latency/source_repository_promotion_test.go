// Reviewed promotion authenticates changes in every locked product repository,
// not just server. Fixtures never execute candidate code or contact remotes.
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

// A separately approved dependency-only improvement is still a source change.
func TestReviewedSourceChoiceAcceptsEveryLockedRepositoryChange(t *testing.T) {
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		current, proposal, deadline := reviewedProposalFixture(t)
		proposal.Commits = map[string]string{}
		for name, commit := range current.Commits {
			proposal.Commits[name] = commit
		}
		proposal.Commits[repository] = strings.Repeat("b", 40)
		var err error
		proposal.SourceLockSha256, err = model.CompetitionSourceLockDigest(proposal.Commits)
		if err != nil {
			t.Fatal(err)
		}
		proposal.BuildReceipt.SourceLockSha256 = proposal.SourceLockSha256
		if selected := selectReviewedSourceProposal([]reviewedSourceProposal{proposal}, current, deadline); selected == nil || selected.ReviewId != proposal.ReviewId {
			t.Fatalf("approved %s-only change refused", repository)
		}
	}
}

// Dependency publication must reconstruct its exact approved patch, rather
// than trusting an unrelated clean single-parent commit in that repository.
func TestSourcePublicationAuthenticatesDependencyPatchTree(t *testing.T) {
	root := t.TempDir()
	base := sourceTestRepository(t, root, "connect")
	repository := filepath.Join(root, "connect")
	if err := os.WriteFile(filepath.Join(repository, "source.txt"), []byte("improved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "add", "source.txt")
	sourceTestGit(t, repository, "commit", "--quiet", "-m", "reviewed dependency")
	commit := sourceTestGit(t, repository, "rev-parse", "HEAD")
	patch := []byte("diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-connect\n+improved\n")
	patchPath := filepath.Join(root, "approved.patch")
	if err := os.WriteFile(patchPath, patch, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(patch)
	intent := &sourcePublicationIntent{ApprovedPatchPath: patchPath, ApprovedPatchSha256: hex.EncodeToString(digest[:])}
	ref := sourcePublicationRef{Repository: "connect", Branch: "sim-latency", ExpectedCommit: base, Commit: commit, StagedRoot: repository}
	backend := &gitSourcePublicationBackend{patchPolicy: controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096, AllowedPaths: []string{"connect/**", "sdk/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}}
	if err := backend.Verify(context.Background(), ref, intent); err != nil {
		t.Fatalf("exact reviewed dependency tree refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, "source.txt"), []byte("unreviewed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "add", "source.txt")
	sourceTestGit(t, repository, "commit", "--amend", "--quiet", "--no-edit")
	ref.Commit = sourceTestGit(t, repository, "rev-parse", "HEAD")
	if err := backend.Verify(context.Background(), ref, intent); err == nil {
		t.Fatal("unreviewed dependency tree accepted")
	}
	wrongRepositoryPatch := []byte(strings.ReplaceAll(string(patch), "connect/source.txt", "sdk/source.txt"))
	if err := os.WriteFile(patchPath, wrongRepositoryPatch, 0600); err != nil {
		t.Fatal(err)
	}
	digest = sha256.Sum256(wrongRepositoryPatch)
	intent.ApprovedPatchSha256 = hex.EncodeToString(digest[:])
	if err := backend.Verify(context.Background(), ref, intent); err == nil {
		t.Fatal("a different repository's approval authorized this change")
	}
}
