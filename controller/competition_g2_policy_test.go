// Prospective policy selection preserves epoch one's exact frozen identity
// and authenticates the new policy independently at every runtime boundary.
package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Approval bytes are visibly synthetic and independent of any selected winner.
func g2FuturePolicyForTest(t *testing.T) *CompetitionFutureEpochPolicy {
	t.Helper()
	commits := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		commits[repository] = strings.Repeat("a", 40)
	}
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/connect/sim-latency/score.go b/connect/sim-latency/score.go\nindex 1111111..2222222 100644\n--- a/connect/sim-latency/score.go\n+++ b/connect/sim-latency/score.go\n@@ -1 +1 @@\n-// synthetic old instrument\n+// synthetic minimum work instrument\n")
	patchDigest := sha256.Sum256(patch)
	approval := &model.CompetitionTrustedInstrumentApproval{Schema: 1, ApprovalId: "synthetic-g2-approval", TargetEpoch: 2,
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, InstrumentBaseCommits: commits, InstrumentBaseSourceLockSha256: lock,
		InstrumentBaseImageDigest: "sha256:" + strings.Repeat("b", 64), Patch: patch, PatchSha256: hex.EncodeToString(patchDigest[:]),
		ProtectedPaths: []string{"connect/sim-latency/score.go"}}
	approval.ApprovalSha256, err = model.CompetitionAutomationDigest(*approval)
	if err != nil {
		t.Fatal(err)
	}
	return &CompetitionFutureEpochPolicy{FromEpoch: 2, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, TrustedInstrument: approval,
		EvaluatorImageDigest: "sha256:" + strings.Repeat("6", 64), SimulatorSha256: strings.Repeat("5", 64), ScorerSha256: strings.Repeat("5", 64), HostQualificationSha256: strings.Repeat("7", 64),
		EvaluatorCommand: "/synthetic/minimum-work/evaluator-minimum-work.sh", EvaluatorCommandSha256: strings.Repeat("8", 64),
		SelfCheckCommand: "/synthetic/minimum-work/host-check.sh", SelfCheckCommandSha256: strings.Repeat("9", 64)}
}

// Staged configuration is deliberately outside the current policy snapshot.
func TestG2FuturePolicyRetainsCurrentRoundBytesAndReadiness(t *testing.T) {
	settings := validSettings()
	legacyPolicy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	stagedPolicy, err := policySnapshot(settings)
	if err != nil || !bytes.Equal(legacyPolicy, stagedPolicy) {
		t.Fatal("staging future configuration rewrote current frozen policy", err)
	}
	round := &roundRecord{RoundResult: RoundResult{Epoch: 1}, PolicyJson: legacyPolicy}
	frozen, err := settingsForFrozenRound(settings, round)
	if err != nil || frozen.EvaluationPolicy.G2VolumePolicy != "" || frozen.scorerVersion() != model.CompetitionScorerVersion {
		t.Fatal("current round became unready after staging future policy", err)
	}
	epochOne, err := settings.ForCompetitionEpoch(1)
	if err != nil || !reflect.DeepEqual(epochOne.EvaluationPolicy, settings.EvaluationPolicy) || epochOne.EvaluatorCommand != settings.EvaluatorCommand || epochOne.EvaluatorImageDigest != settings.EvaluatorImageDigest {
		t.Fatal("future pins contaminated epoch one", err)
	}
	if settings.PublicInfo().ScorerVersion != model.CompetitionScorerVersion || bytes.Contains(stagedPolicy, []byte("g2_volume_policy")) {
		t.Fatal("current discovery acquired future scoring semantics")
	}
}

// The only changed evaluator fields are the explicitly approved instrument pins.
func TestG2FuturePolicySelectsEpochTwoAndCarriesForward(t *testing.T) {
	settings := validSettings()
	settings.EvaluationPolicy.Replicates = 9
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	for _, epoch := range []int{2, 3, 6} {
		future, err := settings.ForCompetitionEpoch(epoch)
		if err != nil {
			t.Fatal(err)
		}
		expected := settings.EvaluationPolicy
		expected.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
		expected.SimulatorSha256, expected.ScorerSha256 = strings.Repeat("5", 64), strings.Repeat("5", 64)
		expected.HostQualificationSha256 = strings.Repeat("7", 64)
		if !reflect.DeepEqual(future.EvaluationPolicy, expected) || future.scorerVersion() != model.CompetitionMinimumWorkScorerVersion ||
			future.EvaluationPolicy.ScoreTimeoutSeconds != 10800 || future.EvaluationPolicy.Replicates != 9 || future.EvaluatorCommand != settings.FutureEpochPolicy.EvaluatorCommand {
			t.Fatalf("epoch %d lost future policy or changed an unrelated contract: %+v", epoch, future.EvaluationPolicy)
		}
		if settings.EvaluationPolicy.G2VolumePolicy != "" || settings.EvaluationPolicy.SimulatorSha256 == future.EvaluationPolicy.SimulatorSha256 {
			t.Fatal("future selection mutated current process settings")
		}
	}
}

// A future startup profile is cached separately before the immutable source
// is resolved; it cannot replace the current epoch's control-only producer.
func TestG2FutureBaselinePreparationProfileIsInactiveForCurrentRound(t *testing.T) {
	settings := validSettings()
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	legacy := &roundBaselinePreparationProfile{Config: roundBaselinePreparationConfig{}}
	futureProfile := &roundBaselinePreparationProfile{Config: roundBaselinePreparationConfig{G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork}}
	settings.roundBaselinePreparation, settings.futureBaselinePreparation = legacy, futureProfile
	current, err := settings.ForCompetitionEpoch(1)
	if err != nil || current.roundBaselinePreparation != legacy {
		t.Fatal("staged preparation changed current round", err)
	}
	future, err := settings.ForCompetitionEpoch(2)
	if err != nil || future.roundBaselinePreparation != futureProfile || settings.roundBaselinePreparation != legacy {
		t.Fatal("future round did not select its separate preparation role", err)
	}
}

// New source is unavailable until both actual compiled digests and the correct
// new policy are present; no future template can relabel an epoch-one intent.
func TestG2SourceSettingsUseQualifiedFuturePinsOnly(t *testing.T) {
	settings := automationApiSettings()
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	commits := settings.FutureEpochPolicy.TrustedInstrument.InstrumentBaseCommits
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	intent := &model.CompetitionSourceActivation{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		TargetRoundId: server.NewId(), TargetEpoch: 1, Commits: commits, SourceLockSha256: lock, EvaluatorImageDigest: settings.EvaluatorImageDigest,
		HostQualificationSha256: settings.EvaluationPolicy.HostQualificationSha256, Threshold: settings.EvaluationPolicy.TakeoverMargin}
	legacy, err := sourceSettingsFromIntent(settings, intent)
	if err != nil || legacy.EvaluationPolicy.G2VolumePolicy != "" || legacy.EvaluatorCommand != settings.EvaluatorCommand {
		t.Fatal("current activation inherited future pins", err)
	}
	intent.TargetEpoch, intent.G2VolumePolicy = 2, model.CompetitionG2VolumePolicyMinimumWork
	if _, err := sourceSettingsFromIntent(settings, intent); err == nil {
		t.Fatal("uncompiled new instrument accepted")
	}
	intent.SimulatorSha256, intent.ScorerSha256 = strings.Repeat("c", 64), strings.Repeat("c", 64)
	intent.EvaluatorImageDigest, intent.HostQualificationSha256 = "sha256:"+strings.Repeat("d", 64), strings.Repeat("e", 64)
	future, err := sourceSettingsFromIntent(settings, intent)
	if err != nil || future.EvaluationPolicy.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork || future.EvaluationPolicy.ScorerSha256 != intent.ScorerSha256 ||
		future.EvaluationPolicy.SimulatorSha256 != intent.SimulatorSha256 || future.EvaluatorImageDigest != intent.EvaluatorImageDigest || future.EvaluatorCommand != settings.FutureEpochPolicy.EvaluatorCommand {
		t.Fatal("qualified dynamic source pins were not applied exactly", err)
	}
	settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Config: roundBaselinePreparationConfig{}}
	if _, err := sourceSettingsFromIntent(settings, intent); err == nil {
		t.Fatal("future source inherited the current round's sealed producer")
	}
	settings.futureBaselinePreparation = &roundBaselinePreparationProfile{Config: roundBaselinePreparationConfig{G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork}}
	future, err = sourceSettingsFromIntent(settings, intent)
	if err != nil || future.roundBaselinePreparation != settings.futureBaselinePreparation {
		t.Fatal("future source did not use its separately authenticated producer", err)
	}
	intent.TargetEpoch = 1
	if _, err := sourceSettingsFromIntent(settings, intent); err == nil {
		t.Fatal("future policy relabeled the current epoch")
	}
}

// This omission previously selected the legacy branch before checking which
// policy the operator had actually approved for the target epoch.
func TestG2FutureSourceCannotBypassApprovedPolicyWithLegacyOmission(t *testing.T) {
	for _, c := range []struct {
		epoch                   int
		futureConfigured, valid bool
	}{
		{epoch: 1, futureConfigured: true, valid: true},
		{epoch: 2, futureConfigured: true},
		{epoch: 6, futureConfigured: true},
		{epoch: 2, valid: true},
	} {
		settings := automationApiSettings()
		future := g2FuturePolicyForTest(t)
		if c.futureConfigured {
			settings.FutureEpochPolicy = future
		}
		intent := &model.CompetitionSourceActivation{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
			TargetRoundId: server.NewId(), TargetEpoch: c.epoch, Commits: future.TrustedInstrument.InstrumentBaseCommits,
			SourceLockSha256: future.TrustedInstrument.InstrumentBaseSourceLockSha256, EvaluatorImageDigest: settings.EvaluatorImageDigest,
			HostQualificationSha256: settings.EvaluationPolicy.HostQualificationSha256, Threshold: settings.EvaluationPolicy.TakeoverMargin}
		if _, err := sourceSettingsFromIntent(settings, intent); (err == nil) != c.valid {
			t.Errorf("epoch=%d future=%t legacy accepted=%t expected=%t: %v", c.epoch, c.futureConfigured, err == nil, c.valid, err)
		}
	}
}

// Policy provenance cannot be omitted or changed while keeping a familiar
// schema/version, including a schema-one snapshot that never had this policy.
func TestG2RoundPolicyRejectsVersionMismatchAndUnknownPolicy(t *testing.T) {
	settings := validSettings()
	settings.EvaluationPolicy.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
	content, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(map[string]any){
		func(policy map[string]any) { policy["scorer_version"] = model.CompetitionScorerVersion },
		func(policy map[string]any) { delete(policy["evaluation_policy"].(map[string]any), "g2_volume_policy") },
		func(policy map[string]any) {
			policy["evaluation_policy"].(map[string]any)["g2_volume_policy"] = "unknown/1"
		},
		func(policy map[string]any) { policy["schema"] = 1 },
	} {
		var policy map[string]any
		if err := json.Unmarshal(content, &policy); err != nil {
			t.Fatal(err)
		}
		mutate(policy)
		changed, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeRoundPolicySnapshot(changed); err == nil {
			t.Fatal("mismatched round policy accepted")
		}
	}
}

// Public discovery follows the snapshot, not the process's future policy.
func TestG2FrozenPublicPolicyUsesItsOwnScorerVersion(t *testing.T) {
	settings := validSettings()
	settings.EvaluationPolicy.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
	content, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.EvaluationPolicy.G2VolumePolicy = ""
	info := settings.PublicInfo()
	if err := applyFrozenRoundPublicPolicy(&info, &roundRecord{PolicyJson: content}); err != nil || info.ScorerVersion != model.CompetitionMinimumWorkScorerVersion || info.EvaluationPolicy.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork {
		t.Fatal("public discovery lost the round's exact scorer policy", err)
	}
}

// A v2 baseline and a historical baseline cannot substitute for each other,
// even when every measured value and workload identifier is otherwise equal.
func TestG2BaselinePolicyMismatchFailsClosed(t *testing.T) {
	settings := validSettings()
	settings.EvaluationPolicy.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
	policy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	round := &roundRecord{RoundResult: RoundResult{RoundId: server.NewId(), Epoch: 2, ProvidersSha256: strings.Repeat("a", 64)}, PolicyJson: policy}
	content := testRoundBaseline(t, settings, round, []float64{100, 101, 99})
	if _, err := validateRoundBaselineJson(settings, round, content); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"missing policy", "legacy version", "unknown policy"} {
		var manifest map[string]any
		if err := json.Unmarshal(content, &manifest); err != nil {
			t.Fatal(err)
		}
		switch mutation {
		case "missing policy":
			delete(manifest, "g2_volume_policy")
		case "legacy version":
			manifest["scorer_version"] = model.CompetitionScorerVersion
		case "unknown policy":
			manifest["g2_volume_policy"] = "unknown/1"
		}
		changed, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := validateRoundBaselineJson(settings, round, changed); err == nil {
			t.Errorf("%s baseline accepted", mutation)
		}
	}
}

// New controls cannot accidentally use the current round's sealed producer.
func TestG2BaselinePreparationUsesSeparateImmutableRelease(t *testing.T) {
	legacy, legacyProgram, err := roundBaselinePreparationPolicyDependencies("")
	if err != nil || legacyProgram != "prepare-round-baseline.sh" || legacy["evaluator.sh"] != "0cccda1d1dcc227f30fda800ddb9bbb28e29dd48cc62885da637fcd4eb4eb00b" {
		t.Fatal("legacy preparation release changed", err)
	}
	future, futureProgram, err := roundBaselinePreparationPolicyDependencies(model.CompetitionG2VolumePolicyMinimumWork)
	if err != nil || futureProgram != "prepare-round-baseline-minimum-work.sh" || future["evaluator-minimum-work.sh"] == "" || future["evaluator.sh"] != "" || len(future) != len(legacy) {
		t.Fatal("future preparation inherited the old instrument", err)
	}
	for path, digest := range legacy {
		if path != "evaluator.sh" && future[path] != digest {
			t.Errorf("future g2 policy changed unrelated producer dependency %s", path)
		}
	}
	if _, _, err := roundBaselinePreparationPolicyDependencies("unknown/1"); err == nil {
		t.Fatal("unknown preparation policy accepted")
	}
}
