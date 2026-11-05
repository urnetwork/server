package controller

// Prospective policy ownership and mandatory-G7 consumers are tested without
// a database, evaluator, host profile, deployment receipt or real workload.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// New source preparation cannot treat a staged v2 profile as authority for a
// coverage epoch. Existing frozen policy codecs retain their original meaning.
func TestCompetitionWorkCoverageNewEpochRequiresExplicitIndependentApproval(t *testing.T) {
	settings := validSettings()
	legacy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	if current, err := settings.ForCompetitionEpoch(1); err != nil || current.EvaluationPolicy.WorkCoveragePolicy != nil {
		t.Fatal("staged future coverage changed epoch one", err)
	}
	if staged, err := policySnapshot(settings); err != nil || !bytes.Equal(legacy, staged) {
		t.Fatal("staging coverage changed legacy policy bytes", err)
	}
	for _, mutate := range []func(*Settings){
		func(settings *Settings) { settings.FutureEpochPolicy = nil },
		func(settings *Settings) { settings.FutureEpochPolicy.WorkCoveragePolicy = nil },
		func(settings *Settings) { settings.FutureEpochPolicy.TrustedInstrument.WorkCoveragePolicy = nil },
		func(settings *Settings) {
			settings.FutureEpochPolicy.WorkCoveragePolicy = &model.CompetitionWorkCoveragePolicy{}
		},
	} {
		changed := validSettings()
		changed.FutureEpochPolicy = g2FuturePolicyForTest(t)
		mutate(changed)
		if _, err := changed.ForCompetitionEpoch(2); err == nil {
			t.Fatal("new source acquired missing or independently unapproved coverage")
		}
	}
	future, err := settings.ForCompetitionEpoch(2)
	if err != nil || future.scorerVersion() != model.CompetitionWorkCoverageScorerVersion || future.EvaluationPolicy.WorkCoveragePolicy == nil || !slices.Contains(future.PatchPolicy.ForbiddenPaths, "model/competition_work_coverage_policy.go") {
		t.Fatal("new epoch lost its explicit scorer policy or protected dependency", err)
	}
	second, err := future.ForCompetitionEpoch(2)
	if err != nil || !reflect.DeepEqual(second.PatchPolicy, future.PatchPolicy) {
		t.Fatal("repeat source selection duplicated the forbidden dependency", err)
	}
}

// Historical v2 active receipts still reconstruct their original settings;
// the same unactivated proposal cannot use this read-only compatibility path.
func TestCompetitionWorkCoverageHistoricalActiveSourceAndFrozenV2RemainReadable(t *testing.T) {
	settings := automationApiSettings()
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	settings.FutureEpochPolicy.WorkCoveragePolicy = nil
	approval := settings.FutureEpochPolicy.TrustedInstrument
	approval.WorkCoveragePolicy = nil
	approval.ApprovalSha256 = ""
	var err error
	approval.ApprovalSha256, err = model.CompetitionAutomationDigest(*approval)
	if err != nil {
		t.Fatal(err)
	}
	activated := time.Unix(1000, 0)
	intent := &model.CompetitionSourceActivation{State: "active", ActivatedAt: &activated,
		Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		TargetRoundId: server.NewId(), TargetEpoch: 2, Commits: approval.InstrumentBaseCommits, SourceLockSha256: approval.InstrumentBaseSourceLockSha256,
		EvaluatorImageDigest: "sha256:" + strings.Repeat("d", 64), HostQualificationSha256: strings.Repeat("e", 64),
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, SimulatorSha256: strings.Repeat("f", 64), ScorerSha256: strings.Repeat("f", 64), Threshold: settings.EvaluationPolicy.TakeoverMargin}
	derived, err := sourceSettingsFromIntent(settings, intent)
	if err != nil || derived.EvaluationPolicy.WorkCoveragePolicy != nil || derived.scorerVersion() != model.CompetitionMinimumWorkScorerVersion {
		t.Fatal("historical active receipt was retroactively rejected or relabeled", err)
	}
	content, err := policySnapshot(derived)
	if err != nil {
		t.Fatal(err)
	}
	if frozen, err := settingsForFrozenRound(derived, &roundRecord{RoundResult: RoundResult{Epoch: 2}, PolicyJson: content}); err != nil || frozen.scorerVersion() != model.CompetitionMinimumWorkScorerVersion {
		t.Fatal("historical frozen v2 round lost its policy", err)
	}
	intent.State, intent.ActivatedAt = "prepared", nil
	if _, err := sourceSettingsFromIntent(settings, intent); err == nil {
		t.Fatal("unactivated old proposal bypassed the prospective coverage gate")
	}
}

// Real derived/public/probe boundaries own their optional policy values; a
// caller cannot mutate process configuration through a returned shallow copy.
func TestCompetitionWorkCoveragePublicProbeAndFutureCopiesOwnLeaves(t *testing.T) {
	settings := validSettings()
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	future, err := settings.ForCompetitionEpoch(2)
	if err != nil {
		t.Fatal(err)
	}
	original := future.EvaluationPolicy.Clone()
	public := future.PublicInfo()
	probe := competitionHostQualificationSettings(future)
	public.EvaluationPolicy.WorkCoveragePolicy.MaximumPageCoverageLoss = .5
	probe.EvaluationPolicy.WorkCoveragePolicy.MaximumByteCoverageLoss = .5
	if !future.EvaluationPolicy.Equal(original) || !reflect.DeepEqual(settings.FutureEpochPolicy.WorkCoveragePolicy, original.WorkCoveragePolicy) {
		t.Fatal("derived view mutated its official policy owner")
	}
	future.EvaluationPolicy.WorkCoveragePolicy.MaximumPageCoverageLoss = .5
	if !reflect.DeepEqual(settings.FutureEpochPolicy.WorkCoveragePolicy, original.WorkCoveragePolicy) {
		t.Fatal("future settings mutated separately configured operator authority")
	}
	future.FutureEpochPolicy.WorkCoveragePolicy.MaximumPageCoverageLoss = .5
	future.FutureEpochPolicy.TrustedInstrument.WorkCoveragePolicy.MaximumByteCoverageLoss = .5
	if !reflect.DeepEqual(settings.FutureEpochPolicy.WorkCoveragePolicy, original.WorkCoveragePolicy) ||
		!reflect.DeepEqual(settings.FutureEpochPolicy.TrustedInstrument.WorkCoveragePolicy, original.WorkCoveragePolicy) {
		t.Fatal("returned future configuration shared an operator coverage leaf")
	}
}

// Mandatory gate and policy provenance survive private projection and durable
// JSON decoding, while hidden plan identities and ledger counters stay sealed.
func TestCompetitionWorkCoveragePrivateFeedbackAndFrozenScoreBinding(t *testing.T) {
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	score := privateScoreFeedbackFixture()
	score.Diagnostics["g2_volume_policy"] = string(model.CompetitionG2VolumePolicyMinimumWork)
	score.Diagnostics["scorer_version"] = model.CompetitionWorkCoverageScorerVersion
	score.Diagnostics["work_coverage_policy"] = &policy
	score.Gates["G7_work_coverage"] = Gate{Passed: false, Details: map[string]any{"candidate_page_coverage": .7, "baseline_page_coverage": .9, "plan_sha256": strings.Repeat("a", 64), "expected_pages": 100000}}
	if !scoreMatchesFrozenG2Policy(score, model.CompetitionG2VolumePolicyMinimumWork, &policy) || scoreMatchesFrozenG2Policy(score, model.CompetitionG2VolumePolicyMinimumWork) {
		t.Fatal("frozen coverage policy was omitted or inferred from a v2 name")
	}
	feedback := privateCompletedScore(score)
	if feedback == nil || feedback.Gates["G7_work_coverage"].Passed || len(feedback.Gates) != 7 || validateScore(feedback) != nil {
		t.Fatal("owner feedback lost its failed prospective gate")
	}
	content, err := json.Marshal(feedback)
	if err != nil || bytes.Contains(content, []byte("plan_sha256")) || bytes.Contains(content, []byte("expected_pages")) {
		t.Fatal("coverage feedback exposed hidden inventory identity or counts", err)
	}
	var replay ScoreResult
	if err := json.Unmarshal(content, &replay); err != nil || validateScore(&replay) != nil || !reflect.DeepEqual(privateCompletedScore(&replay), feedback) {
		t.Fatal("coverage projection was not idempotent after durable decoding", err)
	}
	delete(score.Gates, "G7_work_coverage")
	if validateScore(score) == nil || scoreMatchesFrozenG2Policy(score, model.CompetitionG2VolumePolicyMinimumWork, &policy) {
		t.Fatal("missing G7 retained the new score contract")
	}
}

// A numeric G7 failure is a real failed gate, not a significant improvement or
// an invented zero. Missing G7 makes the projection invalid instead of green.
func TestCompetitionWorkCoverageOperatorMetricClassifiesFailedAndMissingGate(t *testing.T) {
	_, snapshot, score := competitionSubmissionMetricFixture(t)
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	snapshot.Scope.Epoch = 2
	score.Placeable = false
	score.Diagnostics["baseline_takeover_eligible"] = false
	score.Diagnostics["g2_volume_policy"] = string(model.CompetitionG2VolumePolicyMinimumWork)
	score.Diagnostics["scorer_version"] = model.CompetitionWorkCoverageScorerVersion
	score.Diagnostics["work_coverage_policy"] = &policy
	score.Gates["G7_work_coverage"] = Gate{Passed: false, Details: map[string]any{}}
	point, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, server.NewId(), snapshot.SourceAt, score)
	if err != nil || point.Classification != "gate_failed" {
		t.Fatal("G7 failure was mislabeled as significant or missing evidence", point, err)
	}
	delete(score.Gates, "G7_work_coverage")
	if _, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, server.NewId(), snapshot.SourceAt, score); err == nil {
		t.Fatal("missing prospective gate was plotted as complete")
	}
}

// New preparation uses its own prefix/program fingerprint; old v1/v2 release
// pins are not mutated or reused as proof for a coverage baseline.
func TestCompetitionWorkCoverageBaselineRequiresSeparateProducerAndControl(t *testing.T) {
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	legacy, oldProgram, err := roundBaselinePreparationPolicyDependencies(model.CompetitionG2VolumePolicyMinimumWork)
	if err != nil {
		t.Fatal(err)
	}
	coverage, program, err := roundBaselinePreparationPolicyDependencies(model.CompetitionG2VolumePolicyMinimumWork, &policy)
	if err != nil || oldProgram == program || program != "prepare-round-baseline-work-coverage.sh" || coverage["evaluator-work-coverage.sh"] == "" || coverage["evaluator-minimum-work.sh"] != "" || coverage["evaluator-work-coverage.sh"] == legacy["evaluator-minimum-work.sh"] {
		t.Fatal("coverage inherited a historical producer identity", err)
	}
	settings := validSettings()
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	future, err := settings.ForCompetitionEpoch(2)
	if err != nil {
		t.Fatal(err)
	}
	content, err := policySnapshot(future)
	if err != nil {
		t.Fatal(err)
	}
	round := &roundRecord{RoundResult: RoundResult{RoundId: server.NewId(), Epoch: 2}, PolicyJson: content, ProvidersSha256: strings.Repeat("b", 64)}
	baseline := testRoundBaseline(t, future, round, []float64{99, 100, 101})
	if _, err := validateRoundBaselineJson(future, round, baseline); err != nil {
		t.Fatal("new control did not bind its coverage policy", err)
	}
	var manifest roundBaselineManifest
	if err := json.Unmarshal(baseline, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Replicates[0].WorkCoverage = nil
	changed, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateRoundBaselineJson(future, round, changed); err == nil {
		t.Fatal("relabeling a control without independent work proof succeeded")
	}
}
