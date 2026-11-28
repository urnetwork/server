// Synthetic authenticated manifests and sealed files test image provenance only.
// No real approval, compiler, image builder, credential or publication is used.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// Builds an explicit adoption, optionally followed by an ordinary minimum-work review.
func sourceBuildImageMinimumWorkFixture(t *testing.T, inherited bool) reviewedSourceBuildRequest {
	t.Helper()
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Epochs = nil
	base := manifest.EvaluationSource.Generations[0]
	patch := []byte("diff --git a/connect/sim-latency/official-run.sh b/connect/sim-latency/official-run.sh\n--- a/connect/sim-latency/official-run.sh\n+++ b/connect/sim-latency/official-run.sh\n@@ -1 +1 @@\n-legacy\n+minimum-work\n")
	patchHash := sha256.Sum256(patch)
	approval := model.CompetitionTrustedInstrumentApproval{Schema: 1, ApprovalId: "synthetic-image-scripts", TargetEpoch: 2,
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, InstrumentBaseCommits: base.Repositories.commits(),
		InstrumentBaseSourceLockSha256: base.SourceLockSha256, InstrumentBaseImageDigest: "sha256:" + strings.Repeat("b", 64),
		Patch: patch, PatchSha256: hex.EncodeToString(patchHash[:]), ProtectedPaths: []string{"connect/sim-latency/official-run.sh"}}
	var err error
	approval.ApprovalSha256, err = model.CompetitionAutomationDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	proof := &model.CompetitionTrustedInstrumentProof{Schema: 1, Approval: approval, SelectedProductKind: "reviewed_source_carry_forward",
		SelectedProductCommits: base.Repositories.commits(), SelectedProductSourceLockSha256: base.SourceLockSha256,
		InstrumentedServerCommit: strings.Repeat("c", 40)}
	commits := base.Repositories.commits()
	commits["server"] = proof.InstrumentedServerCommit
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	generation := reviewedSourceGeneration{Generation: 1, Kind: "trusted_instrument_transition", SourceLockSha256: lock,
		SignificantImprovementPercent: base.SignificantImprovementPercent, ThresholdEvidenceSha256: base.ThresholdEvidenceSha256,
		Repositories: sourceRepositoriesFromCommits(commits), G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, TrustedInstrument: proof}
	manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, generation)
	manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, reviewedSourceRound{Epoch: 2, SourceGeneration: 1,
		ActivationGeneration: 2, SourceDecisionDigest: strings.Repeat("d", 64), Kind: generation.Kind, Reason: "synthetic_instrument_adoption",
		SourceLockSha256: lock, SignificantImprovementPercent: generation.SignificantImprovementPercent, G2VolumePolicy: generation.G2VolumePolicy})
	request := reviewedSourceBuildRequest{Operation: "isolated_trusted_instrument_build", TrustedInstrument: proof,
		Proposal: &reviewedSourceProposal{Schema: 2, BaseSourceLockSha256: base.SourceLockSha256, SourceLockSha256: lock,
			Commits: commits, PatchSha256: approval.PatchSha256, EvaluatedImageDigest: approval.InstrumentBaseImageDigest},
		SourceGeneration: 1, ConfigLocalDirectory: "/synthetic/config/local", VaultLocalDirectory: "/synthetic/vault/local",
		SandboxContract: "offline_evaluator_containment_v1"}
	if inherited {
		previous := generation
		commits = previous.Repositories.commits()
		commits["server"] = strings.Repeat("e", 40)
		lock, err = model.CompetitionSourceLockDigest(commits)
		if err != nil {
			t.Fatal(err)
		}
		reviewId, jobId := server.Id{15: 1}, server.Id{15: 2}
		significance := validSourceSignificance()
		generation = reviewedSourceGeneration{Generation: 2, Kind: "reviewed_source_promotion", SourceLockSha256: lock,
			SignificantImprovementPercent: significance.RecommendedNextEpochTakeoverMarginPercent, ThresholdEvidenceSha256: significance.ScoreSha256,
			SourceApprovalId: reviewId.String(), SourceApprovalDigest: strings.Repeat("f", 64), EvaluatedBaseSourceLockSha256: previous.SourceLockSha256,
			PatchSha256: strings.Repeat("a", 64), ScoreSha256: significance.ScoreSha256, Significance: significance,
			Repositories: sourceRepositoriesFromCommits(commits), G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork}
		manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, generation)
		manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, reviewedSourceRound{Epoch: 3, SourceGeneration: 2,
			ActivationGeneration: 3, SourceDecisionDigest: strings.Repeat("b", 64), Kind: generation.Kind, Reason: "synthetic_inherited_instrument",
			SourceLockSha256: lock, SignificantImprovementPercent: generation.SignificantImprovementPercent, G2VolumePolicy: generation.G2VolumePolicy})
		request.Operation, request.TrustedInstrument, request.SourceGeneration = "isolated_reviewed_source_build", nil, 2
		request.Proposal = &reviewedSourceProposal{Schema: 2, ReviewId: reviewId, JobId: jobId, ReviewDecisionDigest: generation.SourceApprovalDigest,
			BaseSourceLockSha256: previous.SourceLockSha256, SourceLockSha256: lock, Commits: commits, PatchSha256: generation.PatchSha256,
			Significance: significance, EvaluatedImageDigest: approval.InstrumentBaseImageDigest}
	}
	roots := map[string]string{}
	root := t.TempDir()
	for _, repository := range sourceRepositoryNames() {
		roots[repository] = filepath.Join(root, repository)
	}
	request.Proposal.StagedRoots = roots
	request.SourceManifest, err = yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSourceBuildRequest(request); err != nil {
		t.Fatal("invalid synthetic source-image fixture", err)
	}
	return request
}

// Only the fixed two paths exist; their read-only bytes stand in for sealed source.
func sourceBuildImageScriptsFixture(t *testing.T) (*sourceBuildContext, []byte, []byte) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	root := &sourceBuildContext{Directory: directory, SourceDirectory: filepath.Join(directory, "source"),
		ManifestPath: filepath.Join(directory, "sim-latency.yml"), LockPath: filepath.Join(directory, "source-lock.json")}
	runner, entrypoint := []byte("#!/bin/bash\n# synthetic protected runner\n"), []byte("#!/bin/bash\n# synthetic protected dispatch\n")
	for relative, content := range map[string][]byte{"connect/sim-latency/official-run.sh": runner, "connect/sim-latency/evaluator/container/entrypoint.sh": entrypoint} {
		path := filepath.Join(root.SourceDirectory, "server", filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	return root, runner, entrypoint
}

// Existing metadata must match the already authenticated plan before any output is written.
func writeSourceBuildImageMetadataFixture(t *testing.T, root *sourceBuildContext, request reviewedSourceBuildRequest, plan *sourceBuildImagePlan) {
	t.Helper()
	for path, content := range map[string][]byte{root.ManifestPath: request.SourceManifest, root.LockPath: plan.SourceLock} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Exact final overrides supersede stale inherited hashes and both historical overlay claims.
func assertSourceBuildImageScriptLabels(t *testing.T, request reviewedSourceBuildRequest, root *sourceBuildContext, runner, entrypoint []byte) {
	t.Helper()
	plan, err := sourceBuildImagePlanForDigest(request, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	identity, lock := bytes.Clone(plan.Identity), bytes.Clone(plan.SourceLock)
	plan.Dockerfile = append(plan.Dockerfile, []byte("LABEL com.urnetwork.competition.official-run-sha256=\"stale-runner\"\nLABEL com.urnetwork.competition.entrypoint-sha256=\"stale-entrypoint\"\nLABEL com.urnetwork.competition.shell-overlay=\"metadata-precision-v1\"\nLABEL com.urnetwork.competition.shell-overlay-base=\"sha256:stale-base\"\n")...)
	writeSourceBuildImageMetadataFixture(t, root, request, plan)
	if err := prepareSourceBuildImageContextFiles(root, request, []byte("synthetic already authenticated executable"), plan); err != nil {
		t.Fatal(err)
	}
	runnerHash, entrypointHash := sha256.Sum256(runner), sha256.Sum256(entrypoint)
	expected := fmt.Sprintf("LABEL com.urnetwork.competition.official-run-sha256=\"%x\"\nLABEL com.urnetwork.competition.entrypoint-sha256=\"%x\"\nLABEL com.urnetwork.competition.shell-overlay=\"authenticated-source-scripts-v1\"\nLABEL com.urnetwork.competition.shell-overlay-base=\"%s\"\n", runnerHash, entrypointHash, request.Proposal.EvaluatedImageDigest)
	dockerfile, err := os.ReadFile(filepath.Join(root.Directory, "Dockerfile"))
	if err != nil || !bytes.HasSuffix(dockerfile, []byte(expected)) || !bytes.Equal(plan.Identity, identity) || !bytes.Equal(plan.SourceLock, lock) {
		t.Fatal("final image claims do not bind actual installed script bytes without altering source identity")
	}
	identityFile, err := os.ReadFile(filepath.Join(root.Directory, "image-identity.json"))
	if err != nil || !bytes.Equal(identityFile, identity) {
		t.Fatal("script labels changed emitted source identity", err)
	}
}

// The one-time protected adoption must not inherit the previous wrapper's hash.
func TestSourceBuildImageInstrumentAdoptionBindsInstalledScriptLabels(t *testing.T) {
	request := sourceBuildImageMinimumWorkFixture(t, false)
	root, runner, entrypoint := sourceBuildImageScriptsFixture(t)
	assertSourceBuildImageScriptLabels(t, request, root, runner, entrypoint)
}

// Later genuine reviewed minimum-work generations refresh the same provenance.
func TestSourceBuildImageInheritedMinimumWorkBindsInstalledScriptLabels(t *testing.T) {
	request := sourceBuildImageMinimumWorkFixture(t, true)
	root, runner, entrypoint := sourceBuildImageScriptsFixture(t)
	assertSourceBuildImageScriptLabels(t, request, root, runner, entrypoint)
}

// Both protected inputs must be sealed and bounded before any image claim is emitted.
func TestSourceBuildImageScriptLabelsRejectUnsealedMissingOrRedirectedInputs(t *testing.T) {
	request := sourceBuildImageMinimumWorkFixture(t, false)
	for _, mutation := range []string{"missing runner", "missing entrypoint", "writable runner", "writable entrypoint", "symlink", "redirected parent", "empty", "oversized"} {
		root, _, _ := sourceBuildImageScriptsFixture(t)
		path := filepath.Join(root.SourceDirectory, "server", "connect", "sim-latency", "official-run.sh")
		if mutation == "missing entrypoint" || mutation == "writable entrypoint" {
			path = filepath.Join(root.SourceDirectory, "server", "connect", "sim-latency", "evaluator", "container", "entrypoint.sh")
		}
		var err error
		switch mutation {
		case "missing runner", "missing entrypoint":
			err = os.Remove(path)
		case "writable runner", "writable entrypoint":
			err = os.Chmod(path, 0o644)
		case "symlink":
			target := filepath.Join(filepath.Dir(root.SourceDirectory), "outside-script")
			if err = os.WriteFile(target, []byte("synthetic outside script\n"), 0o444); err == nil {
				if err = os.Remove(path); err == nil {
					err = os.Symlink(target, path)
				}
			}
		case "redirected parent":
			parent := filepath.Dir(path)
			target := filepath.Join(root.SourceDirectory, "redirected-programs")
			if err = os.Rename(parent, target); err == nil {
				err = os.Symlink(target, parent)
			}
		case "empty", "oversized":
			if err = os.Chmod(path, 0o644); err == nil {
				content := []byte{}
				if mutation == "oversized" {
					content = bytes.Repeat([]byte("x"), maximumSourceConfigBytes+1)
				}
				if err = os.WriteFile(path, content, 0o444); err == nil {
					err = os.Chmod(path, 0o444)
				}
			}
		}
		if err != nil {
			t.Fatal(mutation, err)
		}
		plan, err := sourceBuildImagePlanForDigest(request, strings.Repeat("a", 64))
		if err != nil {
			t.Fatal(err)
		}
		before := bytes.Clone(plan.Dockerfile)
		writeSourceBuildImageMetadataFixture(t, root, request, plan)
		if err := prepareSourceBuildImageContextFiles(root, request, []byte("synthetic already authenticated executable"), plan); err == nil || !bytes.Equal(before, plan.Dockerfile) {
			t.Errorf("%s emitted provenance for an unsealed or changed source", mutation)
		}
		if _, err := os.Lstat(filepath.Join(root.Directory, "Dockerfile")); !os.IsNotExist(err) {
			t.Errorf("%s allocated image inputs before sealed script admission", mutation)
		}
	}
}

// Legacy policy reads no replacement files and retains exact Dockerfile/identity/lock bytes.
func TestSourceBuildImageLegacyScriptLabelBindingPreservesExactBytes(t *testing.T) {
	request := sourceBuildContextFixture(t)
	plan, err := sourceBuildImagePlanForDigest(request, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile, identity, lock := bytes.Clone(plan.Dockerfile), bytes.Clone(plan.Identity), bytes.Clone(plan.SourceLock)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	root := &sourceBuildContext{Directory: directory, SourceDirectory: filepath.Join(directory, "source"),
		ManifestPath: filepath.Join(directory, "sim-latency.yml"), LockPath: filepath.Join(directory, "source-lock.json")}
	writeSourceBuildImageMetadataFixture(t, root, request, plan)
	if err := prepareSourceBuildImageContextFiles(root, request, []byte("synthetic already authenticated executable"), plan); err != nil {
		t.Fatal("legacy assembly bytes or its inherited script behavior changed", err)
	}
	for path, expected := range map[string][]byte{filepath.Join(directory, "Dockerfile"): dockerfile,
		filepath.Join(directory, "image-identity.json"): identity, root.LockPath: lock} {
		content, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(content, expected) {
			t.Fatal("legacy emitted bytes changed", path, err)
		}
	}
}
