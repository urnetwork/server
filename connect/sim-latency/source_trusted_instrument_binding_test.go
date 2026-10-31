// Pure synthetic envelopes exercise cross-binding without executing a build,
// contacting a database or asserting any real review/qualification occurred.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Mirrors the final source envelope, after its synthetic build receipt is
// attached to the shared generation/activation proof before manifest sealing.
func sourceTrustedInstrumentModeFixture(t *testing.T) (*model.CompetitionSourceActivation, *sourceManifest) {
	t.Helper()
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Epochs = nil
	baseline := manifest.EvaluationSource.Generations[0]
	patch := []byte("diff --git a/connect/sim-latency/main.go b/connect/sim-latency/main.go\n--- a/connect/sim-latency/main.go\n+++ b/connect/sim-latency/main.go\n@@ -1 +1 @@\n-// old synthetic scorer\n+// new synthetic scorer\n")
	patchHash := sha256.Sum256(patch)
	approval := model.CompetitionTrustedInstrumentApproval{Schema: 1, ApprovalId: "synthetic-instrument-mode", TargetEpoch: 2,
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, InstrumentBaseCommits: baseline.Repositories.commits(),
		InstrumentBaseSourceLockSha256: baseline.SourceLockSha256, InstrumentBaseImageDigest: "sha256:" + strings.Repeat("b", 64),
		Patch: patch, PatchSha256: hex.EncodeToString(patchHash[:]), ProtectedPaths: []string{"connect/sim-latency/main.go"}}
	var err error
	approval.ApprovalSha256, err = model.CompetitionAutomationDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	commits := baseline.Repositories.commits()
	commits["server"] = strings.Repeat("d", 40)
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &model.CompetitionReviewedSourceBuildReceipt{Schema: 2, Operation: "trusted_instrument_build", InstrumentApprovalSha256: approval.ApprovalSha256,
		PatchSha256: approval.PatchSha256, SourceLockSha256: lock, EvaluatorImageDigest: "sha256:" + strings.Repeat("e", 64),
		SimulatorSha256: strings.Repeat("f", 64), G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork,
		HostQualificationSha256: strings.Repeat("1", 64), ContainmentSha256: strings.Repeat("2", 64), TestEvidenceSha256: strings.Repeat("3", 64),
		SandboxId: "synthetic-private-build", Offline: true, NoMainCredentials: true, ReadOnlySource: true, Passed: true, TestModel: "gpt-6.1-sol", TestReasoning: "low"}
	proof := &model.CompetitionTrustedInstrumentProof{Schema: 1, Approval: approval, SelectedProductKind: "reviewed_source_carry_forward",
		SelectedProductCommits: baseline.Repositories.commits(), SelectedProductSourceLockSha256: baseline.SourceLockSha256,
		InstrumentedServerCommit: commits["server"], BuildReceipt: receipt}
	manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, reviewedSourceGeneration{Generation: 1,
		Kind: "trusted_instrument_transition", G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, TrustedInstrument: proof,
		SourceLockSha256: lock, SignificantImprovementPercent: baseline.SignificantImprovementPercent, ThresholdEvidenceSha256: baseline.ThresholdEvidenceSha256,
		Repositories: sourceRepositoriesFromCommits(commits)})
	manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, reviewedSourceRound{Epoch: 2, SourceGeneration: 1, ActivationGeneration: 2,
		SourceDecisionDigest: strings.Repeat("4", 64), Kind: "trusted_instrument_transition", Reason: "no_reward_winner", RewardDecisionDigest: strings.Repeat("5", 64),
		SourceLockSha256: lock, SignificantImprovementPercent: baseline.SignificantImprovementPercent, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork})
	intent := &model.CompetitionSourceActivation{TargetEpoch: 2, Generation: 2, SourceGeneration: 1, Kind: "trusted_instrument_transition", Reason: "no_reward_winner",
		SourceDecisionDigest: strings.Repeat("4", 64), RewardDecisionDigest: strings.Repeat("5", 64), SourceLockSha256: lock, Commits: commits,
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, TrustedInstrument: proof, Threshold: baseline.SignificantImprovementPercent / 100,
		SimulatorSha256: receipt.SimulatorSha256, ScorerSha256: receipt.SimulatorSha256, EvaluatorImageDigest: receipt.EvaluatorImageDigest,
		HostQualificationSha256: receipt.HostQualificationSha256, ThresholdEvidenceSha256: baseline.ThresholdEvidenceSha256}
	intent.Manifest, err = yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(intent.Manifest)
	intent.ManifestSha256 = hex.EncodeToString(digest[:])
	return intent, manifest
}

// Matching finalized proof bytes survive YAML/JSON round trips. Substituting
// only the top-level receipt or selection cannot reuse the manifest's proof.
func TestTrustedInstrumentSourceModeBindsFinalizedProof(t *testing.T) {
	intent, _ := sourceTrustedInstrumentModeFixture(t)
	if err := validateSourceActivationMode(intent, &controller.Settings{}); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*model.CompetitionSourceActivation){
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument.BuildReceipt.TestEvidenceSha256 = strings.Repeat("9", 64)
		},
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument.SelectedProductReviewDigest = strings.Repeat("9", 64)
		},
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument.Approval.ApprovalSha256 = strings.Repeat("9", 64)
		},
		func(value *model.CompetitionSourceActivation) { value.TrustedInstrument = nil },
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument, value.Kind = nil, "reviewed_source_carry_forward"
		},
		func(value *model.CompetitionSourceActivation) { value.TargetEpoch = 1 },
	} {
		var changed model.CompetitionSourceActivation
		if err := json.Unmarshal(encoded, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		if err := validateSourceActivationMode(&changed, &controller.Settings{}); err == nil {
			t.Fatal("source mode accepted a substituted instrument proof or epoch")
		}
	}
}

// A self-consistently resealed manifest must still map the exact activation,
// decision and selected generation to epoch two; an unused generation is not
// sufficient evidence that this source was selected for the next round.
func TestTrustedInstrumentSourceModeBindsExactEpochTwoMapping(t *testing.T) {
	original, _ := sourceTrustedInstrumentModeFixture(t)
	for _, mutate := range []func(*sourceManifest){
		func(value *sourceManifest) { value.EvaluationSource.Rounds = value.EvaluationSource.Rounds[:1] },
		func(value *sourceManifest) { value.EvaluationSource.Rounds[1].ActivationGeneration = 3 },
		func(value *sourceManifest) {
			value.EvaluationSource.Rounds[1].SourceDecisionDigest = strings.Repeat("9", 64)
		},
		func(value *sourceManifest) { value.EvaluationSource.Rounds[1].Reason = "substituted_mapping_reason" },
		func(value *sourceManifest) {
			value.EvaluationSource.Rounds[1].RewardDecisionDigest = strings.Repeat("9", 64)
		},
	} {
		manifest, err := decodeSourceManifest(original.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		mutate(manifest)
		changed := *original
		changed.Manifest, err = yaml.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(changed.Manifest)
		changed.ManifestSha256 = hex.EncodeToString(digest[:])
		if err := validateSourceActivationMode(&changed, &controller.Settings{}); err == nil {
			t.Fatal("source mode accepted a resealed but different epoch-two mapping")
		}
	}
}

// Product-promotion provenance cannot be synthesized by instrument changes
// alone, even if its generation and real-shaped review fields appear valid.
func TestTrustedInstrumentGenerationRequiresChangedSelectedProduct(t *testing.T) {
	_, manifest := sourceTrustedInstrumentModeFixture(t)
	previous := manifest.EvaluationSource.Generations[0]
	generation := manifest.EvaluationSource.Generations[1]
	proof := generation.TrustedInstrument
	id := server.NewId()
	generation.Kind, generation.SourceApprovalId = "reviewed_source_promotion", id.String()
	generation.SourceApprovalDigest, generation.PatchSha256 = strings.Repeat("6", 64), strings.Repeat("7", 64)
	proof.SelectedProductKind = "reviewed_source_promotion"
	proof.SelectedProductReviewId, proof.SelectedProductReviewDigest, proof.SelectedProductPatchSha256 = &id, generation.SourceApprovalDigest, generation.PatchSha256
	if err := validateTrustedInstrumentGeneration(&generation, &previous); err == nil {
		t.Fatal("instrument-only generation masqueraded as a changed selected product")
	}
	selected := previous.Repositories.commits()
	selected["server"] = strings.Repeat("8", 40)
	proof.SelectedProductCommits = selected
	proof.SelectedProductSourceLockSha256, _ = model.CompetitionSourceLockDigest(selected)
	if err := validateTrustedInstrumentGeneration(&generation, &previous); err != nil {
		t.Fatal("genuinely distinct selected product was rejected", err)
	}
}

// Unchanged refs are still product identity: omitting a dependency from the
// remote transition does not permit it to differ from the selected graph.
func TestTrustedInstrumentPublicationBindsEveryUnchangedDependency(t *testing.T) {
	activation, _ := sourceTrustedInstrumentModeFixture(t)
	intent := &sourcePublicationIntent{Schema: 2, ActivationGeneration: 2, SourceDecisionDigest: activation.SourceDecisionDigest,
		ManifestSha256: activation.ManifestSha256, TrustedInstrument: activation.TrustedInstrument}
	for _, repository := range append(sourceRepositoryNames(), "config") {
		branch, expected, commit := "sim-latency", activation.TrustedInstrument.SelectedProductCommits[repository], activation.Commits[repository]
		if repository == "config" {
			branch, expected, commit = "main", strings.Repeat("a", 40), strings.Repeat("b", 40)
		}
		intent.Refs = append(intent.Refs, sourcePublicationRef{Repository: repository, Branch: branch, ExpectedCommit: expected, Commit: commit, StagedRoot: "/synthetic/" + repository})
	}
	if err := validateSourcePublicationIntent(intent); err != nil {
		t.Fatal(err)
	}
	for index, ref := range intent.Refs {
		if ref.Repository == "server" || ref.Repository == "config" {
			continue
		}
		original := intent.Refs[index]
		intent.Refs[index].ExpectedCommit, intent.Refs[index].Commit = strings.Repeat("9", 40), strings.Repeat("9", 40)
		if err := validateSourcePublicationIntent(intent); err == nil {
			t.Fatalf("unchanged %s ref substituted the actual selected dependency", ref.Repository)
		}
		intent.Refs[index] = original
	}
}
