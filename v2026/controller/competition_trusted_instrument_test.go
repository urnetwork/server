// Pure synthetic authorization tests do not create review, qualification or
// deployment evidence in any external system.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A minimal proof isolates configured operator approval from selected-product
// identity. The receipt is synthetic data for validation, never runtime proof.
func trustedInstrumentAuthorizationFixture(t *testing.T) (*Settings, *model.CompetitionSourceActivation, *model.CompetitionSourceActivation) {
	t.Helper()
	commits := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		commits[repository] = strings.Repeat("a", 40)
	}
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/connect/sim-latency/main.go b/connect/sim-latency/main.go\n--- a/connect/sim-latency/main.go\n+++ b/connect/sim-latency/main.go\n@@ -1 +1 @@\n-old\n+new\n")
	patchHash := sha256.Sum256(patch)
	approval := &model.CompetitionTrustedInstrumentApproval{Schema: 1, ApprovalId: "synthetic-instrument-approval", TargetEpoch: 2,
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, InstrumentBaseCommits: commits, InstrumentBaseSourceLockSha256: lock,
		InstrumentBaseImageDigest: "sha256:" + strings.Repeat("b", 64), PatchSha256: hex.EncodeToString(patchHash[:]), Patch: patch,
		ProtectedPaths: []string{"connect/sim-latency/main.go"}}
	approval.ApprovalSha256, err = model.CompetitionAutomationDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	settings := &Settings{FutureEpochPolicy: &CompetitionFutureEpochPolicy{FromEpoch: 2, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, TrustedInstrument: approval,
		EvaluatorCommand: "/synthetic/minimum-work/evaluator.sh", EvaluatorCommandSha256: strings.Repeat("7", 64),
		SelfCheckCommand: "/synthetic/minimum-work/host-check.sh", SelfCheckCommandSha256: strings.Repeat("8", 64)}}
	current := &model.CompetitionSourceActivation{TargetEpoch: 1, SourceGeneration: 0, SourceLockSha256: lock, Commits: commits,
		Threshold: 0.161, ThresholdEvidenceSha256: strings.Repeat("c", 64), EvaluatorImageDigest: approval.InstrumentBaseImageDigest}
	childCommits := map[string]string{}
	for repository, commit := range commits {
		childCommits[repository] = commit
	}
	childCommits["server"] = strings.Repeat("d", 40)
	childLock, _ := model.CompetitionSourceLockDigest(childCommits)
	receipt := &model.CompetitionReviewedSourceBuildReceipt{Schema: 2, Operation: "trusted_instrument_build", InstrumentApprovalSha256: approval.ApprovalSha256,
		PatchSha256: approval.PatchSha256, SourceLockSha256: childLock, EvaluatorImageDigest: "sha256:" + strings.Repeat("e", 64),
		SimulatorSha256: strings.Repeat("f", 64), G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork,
		HostQualificationSha256: strings.Repeat("1", 64), ContainmentSha256: strings.Repeat("2", 64), TestEvidenceSha256: strings.Repeat("3", 64),
		SandboxId: "synthetic-private-build-owner", Offline: true, NoMainCredentials: true, ReadOnlySource: true, Passed: true, TestModel: "gpt-6.1-sol", TestReasoning: "low"}
	proof := &model.CompetitionTrustedInstrumentProof{Schema: 1, Approval: *approval, SelectedProductKind: "reviewed_source_carry_forward", SelectedProductCommits: commits,
		SelectedProductSourceLockSha256: lock, InstrumentedServerCommit: childCommits["server"], BuildReceipt: receipt}
	intent := &model.CompetitionSourceActivation{TargetEpoch: 2, SourceGeneration: 1, Kind: "trusted_instrument_transition", Reason: "no_reward_winner",
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, Commits: childCommits, SourceLockSha256: childLock,
		Threshold: current.Threshold, ThresholdEvidenceSha256: current.ThresholdEvidenceSha256, TrustedInstrument: proof,
		EvaluatorImageDigest: receipt.EvaluatorImageDigest, HostQualificationSha256: receipt.HostQualificationSha256, SimulatorSha256: receipt.SimulatorSha256, ScorerSha256: receipt.SimulatorSha256}
	return settings, current, intent
}

// An instrument-only carry has an explicit new generation but no product
// review, product score or product threshold change.
func TestTrustedInstrumentAuthorizationCarriesActualIncumbent(t *testing.T) {
	settings, current, intent := trustedInstrumentAuthorizationFixture(t)
	if err := validateTrustedInstrumentSource(settings, intent, current); err != nil {
		t.Fatal(err)
	}
	if intent.ReviewId != nil || intent.TrustedInstrument.SelectedProductReviewId != nil || intent.Threshold != current.Threshold {
		t.Fatal("instrument-only carry fabricated product review or transplanted margin")
	}
	legacy, err := json.Marshal(model.CompetitionSourceActivation{Kind: "reviewed_source_carry_forward"})
	if err != nil || strings.Contains(string(legacy), "trusted_instrument") || strings.Contains(string(legacy), "g2_volume_policy") || strings.Contains(string(legacy), "simulator_sha256") {
		t.Fatal("optional instrument additions changed legacy JSON shape", err)
	}
}

// Even a self-consistent rehashed request cannot approve itself: configured
// operator approval and genuine selected-winner review remain independent.
func TestTrustedInstrumentAuthorizationRejectsTamperedProof(t *testing.T) {
	settings, current, original := trustedInstrumentAuthorizationFixture(t)
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*model.CompetitionSourceActivation){
		func(value *model.CompetitionSourceActivation) { value.TargetEpoch = 1 },
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument.Approval.Patch = append([]byte("\n"), value.TrustedInstrument.Approval.Patch...)
		},
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument.Approval.ProtectedPaths = []string{"connect/synthetic_product.go"}
		},
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument.Approval.InstrumentBaseCommits["server"] = strings.Repeat("9", 40)
		},
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument.InstrumentedServerCommit = strings.Repeat("9", 40)
		},
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument.SelectedProductCommits["connect"] = strings.Repeat("9", 40)
		},
		func(value *model.CompetitionSourceActivation) {
			id := server.NewId()
			value.TrustedInstrument.SelectedProductReviewId = &id
		},
		func(value *model.CompetitionSourceActivation) { value.Kind = "reviewed_source_carry_forward" },
		func(value *model.CompetitionSourceActivation) { value.TrustedInstrument.BuildReceipt.Passed = false },
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument.BuildReceipt.SimulatorSha256 = strings.Repeat("9", 64)
		},
		func(value *model.CompetitionSourceActivation) {
			value.TrustedInstrument.BuildReceipt.InstrumentApprovalSha256 = strings.Repeat("9", 64)
		},
		func(value *model.CompetitionSourceActivation) { value.Threshold += 0.01 },
		func(value *model.CompetitionSourceActivation) { value.TrustedInstrument = nil },
	} {
		var intent model.CompetitionSourceActivation
		if err := json.Unmarshal(encoded, &intent); err != nil {
			t.Fatal(err)
		}
		mutate(&intent)
		if err := validateTrustedInstrumentSource(settings, &intent, current); err == nil {
			t.Fatal("tampered instrument/product authorization accepted")
		}
	}
}

// Separate real review metadata is mandatory for a promoted selected product;
// the operator's instrument approval cannot be relabeled as that review.
func TestTrustedInstrumentAuthorizationPreservesWinnerReview(t *testing.T) {
	settings, current, intent := trustedInstrumentAuthorizationFixture(t)
	id := server.NewId()
	intent.Kind = "reviewed_source_promotion"
	intent.ReviewId, intent.ReviewDecisionDigest, intent.ApprovedPatchSha256 = &id, strings.Repeat("4", 64), strings.Repeat("5", 64)
	proof := intent.TrustedInstrument
	proof.SelectedProductKind = "reviewed_source_promotion"
	selectedCommits := map[string]string{}
	for repository, commit := range current.Commits {
		selectedCommits[repository] = commit
	}
	selectedCommits["server"] = strings.Repeat("7", 40)
	proof.SelectedProductCommits = selectedCommits
	proof.SelectedProductSourceLockSha256, _ = model.CompetitionSourceLockDigest(selectedCommits)
	proof.SelectedProductReviewId, proof.SelectedProductReviewDigest, proof.SelectedProductPatchSha256 = &id, intent.ReviewDecisionDigest, intent.ApprovedPatchSha256
	if err := validateTrustedInstrumentSource(settings, intent, current); err != nil {
		t.Fatal(err)
	}
	proof.SelectedProductReviewDigest = strings.Repeat("6", 64)
	if err := validateTrustedInstrumentSource(settings, intent, current); err == nil {
		t.Fatal("instrument approval concealed a substituted selected winner review")
	}
}

// A configured successor policy is mandatory even when a legacy carry claims
// not to change anything. Epoch one and configurations without future approval
// retain their historical access and source-choice semantics.
func TestTrustedInstrumentAuthorizationRequiresConfiguredSuccessorPolicy(t *testing.T) {
	settings, current, _ := trustedInstrumentAuthorizationFixture(t)
	legacy := *current
	legacy.TargetEpoch, legacy.Kind = 2, "reviewed_source_carry_forward"
	if err := validateTrustedInstrumentSource(settings, &legacy, current); err == nil {
		t.Fatal("configured epoch-two minimum-work transition was bypassed by a proof-free legacy carry")
	}
	legacy.TargetEpoch = 6
	if err := validateTrustedInstrumentSource(settings, &legacy, current); err == nil {
		t.Fatal("configured later minimum-work policy was bypassed by a legacy carry")
	}
	legacy.TargetEpoch = 1
	if err := validateTrustedInstrumentSource(settings, &legacy, current); err != nil {
		t.Fatal("future policy changed frozen epoch-one access", err)
	}
	settings.FutureEpochPolicy = nil
	legacy.TargetEpoch = 2
	if err := validateTrustedInstrumentSource(settings, &legacy, current); err != nil {
		t.Fatal("legacy configuration without future approval acquired new requirements", err)
	}
}

// Adopting the instrument once does not require a fake second adoption proof
// for a later genuine carry. Invalid configured authority still fails closed.
func TestTrustedInstrumentAuthorizationInheritsApprovedPolicy(t *testing.T) {
	settings, _, current := trustedInstrumentAuthorizationFixture(t)
	current.TargetEpoch, current.TrustedInstrument = 2, nil
	intent := *current
	intent.TargetEpoch, intent.Kind = 3, "reviewed_source_carry_forward"
	if err := validateTrustedInstrumentSource(settings, &intent, current); err != nil {
		t.Fatal("inherited minimum-work carry required another adoption proof", err)
	}
	settings.FutureEpochPolicy.TrustedInstrument.ApprovalSha256 = strings.Repeat("9", 64)
	if err := validateTrustedInstrumentSource(settings, &intent, current); err == nil {
		t.Fatal("a self-reported future policy bypassed configured approval validation")
	}
}

// Instrumentation alone cannot be presented as a reviewed product promotion,
// even if plausible real-shaped review metadata is copied alongside it.
func TestTrustedInstrumentAuthorizationRejectsUnchangedPromotedProduct(t *testing.T) {
	settings, current, intent := trustedInstrumentAuthorizationFixture(t)
	id := server.NewId()
	intent.Kind = "reviewed_source_promotion"
	intent.ReviewId, intent.ReviewDecisionDigest, intent.ApprovedPatchSha256 = &id, strings.Repeat("4", 64), strings.Repeat("5", 64)
	proof := intent.TrustedInstrument
	proof.SelectedProductKind = "reviewed_source_promotion"
	proof.SelectedProductReviewId, proof.SelectedProductReviewDigest, proof.SelectedProductPatchSha256 = &id, intent.ReviewDecisionDigest, intent.ApprovedPatchSha256
	if err := validateTrustedInstrumentSource(settings, intent, current); err == nil {
		t.Fatal("instrument-only source change masqueraded as a selected product promotion")
	}
}
