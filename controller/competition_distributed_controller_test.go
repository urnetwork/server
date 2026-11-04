// Pure regressions cover prospective policy, run-level statistics, raw evidence
// identity and the result/shutdown ordering before disposable-database execution.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
)

// An inactive future host policy cannot add a single byte to epoch one's policy.
func TestCompetitionDistributedPolicyIsProspectiveAndFrozen(t *testing.T) {
	settings := automationApiSettings()
	legacy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	settings.FutureEpochPolicy.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	staged, err := policySnapshot(settings)
	if err != nil || !bytes.Equal(legacy, staged) {
		t.Fatal("staged distributed policy changed frozen epoch-one bytes", err)
	}
	current, err := settingsForFrozenRound(settings, &roundRecord{RoundResult: RoundResult{Epoch: 1}, PolicyJson: legacy})
	if err != nil || current.DistributedEvaluationPolicy != nil || competitionDistributedEnabled(current) {
		t.Fatal("epoch one inherited distributed execution", err)
	}
	future, err := settings.ForCompetitionEpoch(2)
	if err != nil {
		t.Fatal(err)
	}
	frozenBytes, err := policySnapshot(future)
	if err != nil || !bytes.Contains(frozenBytes, []byte("distributed_evaluation_policy")) {
		t.Fatal("epoch two omitted approved host policy", err)
	}
	round := &roundRecord{RoundResult: RoundResult{Epoch: 2}, PolicyJson: frozenBytes}
	derived, err := settingsForFrozenRound(future, round)
	if err != nil {
		t.Fatal(err)
	}
	host := derived.DistributedEvaluationPolicy.Hosts[1]
	derived.executorHostId, derived.executorSlotId = host.HostId, host.SlotId
	if !competitionDistributedEnabled(derived) {
		t.Fatal("exact approved frozen host could not select its physical slot")
	}
	settings.FutureEpochPolicy.DistributedEvaluationPolicy.Hosts[1].QualificationSha256 = settings.FutureEpochPolicy.DistributedEvaluationPolicy.Hosts[0].QualificationSha256
	if _, err := settingsForFrozenRound(future, round); err == nil {
		t.Fatal("changed host qualification was accepted for an already frozen round")
	}
}

// Cross-host kernel/container minor differences are deliberately not equality
// gates; the independently frozen qualification and exact host identity are.
func TestCompetitionDistributedHostUsesIndependentQualification(t *testing.T) {
	settings := validSettings()
	settings.EvaluationPolicy.HardwareId = "synthetic-hardware"
	settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	settings.distributedRoundEpoch = 2
	host := settings.DistributedEvaluationPolicy.Hosts[1]
	settings.executorHostId, settings.executorSlotId = host.HostId, host.SlotId
	check := passingHostCheck(settings)
	check.HostId, check.QualificationSha256 = host.HostId, host.QualificationSha256
	check.KernelRelease = "synthetic-kernel-minor"
	if !check.Eligible(settings) {
		t.Fatal("independently qualified approved host was rejected")
	}
	check.HostId = settings.DistributedEvaluationPolicy.Hosts[0].HostId
	if check.Eligible(settings) {
		t.Fatal("a different host's report qualified the selected slot")
	}
}

// NIST's primary Student-t table (three decimal precision) supplies independent
// critical vectors: https://www.itl.nist.gov/div898/handbook/eda/section3/eda3672.htm
func TestCompetitionDistributedStudentCriticalMatchesNistReference(t *testing.T) {
	for _, test := range []struct{ degrees, alpha, critical float64 }{
		{degrees: 1, alpha: 0.025, critical: 12.706}, {degrees: 2, alpha: 0.025, critical: 4.303},
		{degrees: 8, alpha: 0.025, critical: 2.306}, {degrees: 16, alpha: 0.025, critical: 2.120},
		{degrees: 8, alpha: 0.05, critical: 1.860},
	} {
		actual := competitionHostStudentCritical(test.alpha, test.degrees)
		if math.IsNaN(actual) || math.Abs(actual-test.critical) > 0.00051 {
			t.Errorf("Student critical df=%g alpha=%g: got %.12g, reference %.3f", test.degrees, test.alpha, actual, test.critical)
		}
	}
}

// The df=1 Cauchy and df=2 closed forms independently test tails and central
// probability, rather than using this implementation's inverse as its oracle.
func TestCompetitionDistributedStudentSurvivalMatchesAnalyticForms(t *testing.T) {
	for _, value := range []float64{0, 0.25, 1, 4, 100} {
		cauchy := 0.5 - math.Atan(value)/math.Pi
		studentTwo := 0.5 * (1 - value/math.Sqrt(2+value*value))
		if actual := competitionHostStudentSurvival(value, 1); math.Abs(actual-cauchy) > 1e-12 {
			t.Errorf("Cauchy t=%g: got %.15g want %.15g", value, actual, cauchy)
		}
		if actual := competitionHostStudentSurvival(value, 2); math.Abs(actual-studentTwo) > 1e-12 {
			t.Errorf("Student df=2 t=%g: got %.15g want %.15g", value, actual, studentTwo)
		}
	}
}

// Direction changes must reverse the interval, not the significance verdict.
func TestCompetitionDistributedWelchPreservesVarianceSymmetryAndConstantControls(t *testing.T) {
	baseline, candidate := []float64{}, []float64{}
	for index := range 9 {
		baseline = append(baseline, 90+float64(index)*2)
		candidate = append(candidate, 91+float64(index)*2)
	}
	forward, err := compareCompetitionHostMetric("raw_score", baseline, candidate)
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := compareCompetitionHostMetric("raw_score", candidate, baseline)
	if err != nil || forward.TwoSidedPValue != reverse.TwoSidedPValue || forward.DifferenceCiLow != -reverse.DifferenceCiHigh || forward.DifferenceCiHigh != -reverse.DifferenceCiLow || forward.SignificantShift {
		t.Fatal("run-level Welch uncertainty changed with direction", err)
	}
	constant := []float64{9, 9, 9, 9, 9, 9, 9, 9, 9}
	equal, err := compareCompetitionHostMetric("request_count", constant, constant)
	if err != nil || equal.SignificantShift || equal.TwoSidedPValue != 1 || equal.StandardError != 0 {
		t.Fatal("equal constant controls were classified as a shift", err)
	}
	shift, err := compareCompetitionHostMetric("request_count", constant, []float64{10, 10, 10, 10, 10, 10, 10, 10, 10})
	if err != nil || !shift.SignificantShift || shift.TwoSidedPValue != 0 {
		t.Fatal("zero-noise shift was admitted", err)
	}
}

// Unavailable numeric uncertainty never silently becomes a nonsignificant shift.
func TestCompetitionDistributedWelchRejectsOverflowUnderflowAndNonfiniteSamples(t *testing.T) {
	for _, values := range [][]float64{
		{math.NaN(), 1, 1, 1, 1, 1, 1, 1, 1}, {math.Inf(1), 1, 1, 1, 1, 1, 1, 1, 1},
		{math.MaxFloat64, math.MaxFloat64, math.MaxFloat64, math.MaxFloat64, math.MaxFloat64, math.MaxFloat64, math.MaxFloat64, math.MaxFloat64, math.MaxFloat64},
		{1e-200, 2e-200, 1e-200, 2e-200, 1e-200, 2e-200, 1e-200, 2e-200, 1e-200},
	} {
		if _, err := compareCompetitionHostMetric("raw_score", values, values); err == nil {
			t.Fatal("unavailable numerical uncertainty was accepted")
		}
	}
	if !math.IsNaN(competitionHostStudentCritical(math.NaN(), 8)) || !math.IsNaN(competitionHostStudentSurvival(1, 0)) {
		t.Fatal("invalid distribution parameters produced an admission value")
	}
}

// A signed raw inventory must name this job and round; hashes alone cannot
// turn another evaluation's otherwise valid evidence into the current result.
func TestCompetitionDistributedEvidenceBindsJobRoundAndRetainsInventory(t *testing.T) {
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId()}}
	for _, mismatch := range []string{"", "job", "round"} {
		directory := t.TempDir()
		inventory := map[string]any{"schema": 1, "kind": "sim-latency-evidence-manifest", "job_id": job.JobId.String(), "round_id": job.RoundId.String(), "artifacts": []evaluationArtifact{}}
		if mismatch == "job" {
			inventory["job_id"] = server.NewId().String()
		}
		if mismatch == "round" {
			inventory["round_id"] = server.NewId().String()
		}
		content, err := json.Marshal(inventory)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeExclusiveFile(filepath.Join(directory, "evidence-manifest.json"), content, 0400); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		files := []competitionagent.File{{Path: "evidence-manifest.json", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}}
		artifacts, err := distributedCompetitionEvidence(directory, job, nil, files, 1024*1024)
		if mismatch != "" && err == nil {
			t.Fatalf("wrong %s raw inventory was accepted", mismatch)
		}
		if mismatch == "" && (err != nil || len(artifacts) != 1 || artifacts[0].Path != "evidence-manifest.json") {
			t.Fatal("authenticated raw inventory was not retained", err)
		}
	}
}

// Force the done branch to win before cancellation. The canceled coordinator
// must hand back the original custody and must never persist an early timeout.
func TestCompetitionDistributedShutdownAfterResultSelectionDoesNotComplete(t *testing.T) {
	fixture := newCompetitionCompletionFixture(t)
	settings := fixture.worker.settings
	settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	settings.distributedRoundEpoch = 2
	host := settings.DistributedEvaluationPolicy.Hosts[0]
	settings.executorHostId, settings.executorSlotId = host.HostId, host.SlotId
	fixture.job.ExecutorHostId, fixture.job.SlotId, fixture.job.SlotFencingToken = host.HostId, host.SlotId, 4
	fixture.job.executorSettings = settings
	started, attempt := *fixture.job.StartedAt, fixture.job.AttemptCount
	fixture.evaluator.outcome = EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic canceled result")}
	selected, release := make(chan struct{}), make(chan struct{})
	fixture.worker.evaluationResultForTest = func() { close(selected); <-release }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fixture.worker.evaluateOne(ctx, fixture.job, fixture.host) }()
	<-selected
	cancel()
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) || fixture.store.calls != 0 || fixture.store.handbacks != 1 || fixture.evaluator.calls != 1 || !fixture.job.StartedAt.Equal(started) || fixture.job.AttemptCount != attempt {
		t.Fatalf("shutdown completed, restarted or reset an owned run: err=%v completions=%d handbacks=%d", err, fixture.store.calls, fixture.store.handbacks)
	}
}

// The stable key is independent of ownership, transport and elapsed queue time.
func TestCompetitionDistributedRunIdentitySeparatesRoundAndSourceEpoch(t *testing.T) {
	started := time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)
	identity := competitionagent.Identity{HostId: "synthetic-executor", RunId: competitionagent.RunId("synthetic-job", 0, 1), JobId: "synthetic-job", RoundId: "synthetic-round",
		RoundEpoch: 2, SourceEpoch: 0, Attempt: 1, StartedAt: started, DeadlineAt: started.Add(3 * time.Hour)}
	if err := identity.Validate(competitionagent.DefaultLimits()); err != nil {
		t.Fatal("epoch two could not carry unchanged reviewed source", err)
	}
	identity.RoundEpoch = 6
	if err := identity.Validate(competitionagent.DefaultLimits()); err != nil {
		t.Fatal("later epoch could not carry the same reviewed source", err)
	}
}
