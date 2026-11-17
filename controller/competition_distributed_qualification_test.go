// Retained no-op receipts preserve all run-level uncertainty and cannot trade
// missing metrics, invented control values or a point-estimate claim for admission.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
)

// Failed or stale readiness cannot publish a first admission clock, including
// cancellation observed after the probe but before the reservation boundary.
func TestCompetitionDistributedAdmissionReadinessPrecedesClock(t *testing.T) {
	roundId, otherRoundId := server.NewId(), server.NewId()
	for _, test := range []struct {
		check            HostSelfCheck
		err              error
		cancelAfterCheck bool
	}{
		{err: errors.New("synthetic unavailable host")},
		{check: HostSelfCheck{RebaselinePassed: true}},
		{check: HostSelfCheck{RebaselineRoundId: &roundId}},
		{check: HostSelfCheck{RebaselinePassed: true, RebaselineRoundId: &otherRoundId}},
		{check: HostSelfCheck{RebaselinePassed: true, RebaselineRoundId: &roundId}, cancelAfterCheck: true},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		reservations := 0
		_, run, err := reserveCompetitionReadyExecutorAdmission(ctx, roundId,
			func(context.Context) (HostSelfCheck, error) {
				if test.cancelAfterCheck {
					cancel()
				}
				return test.check, test.err
			}, func(context.Context) (*competitionExecutorQualificationRun, error) {
				reservations++
				return &competitionExecutorQualificationRun{}, nil
			})
		cancel()
		if err == nil || run != nil || reservations != 0 {
			t.Fatal("unready or canceled setup consumed the immutable no-op clock", err, reservations)
		}
	}
}

// Readiness cannot renew an existing durable sample. The store's original
// instants and identities pass through unchanged on every reconnection.
func TestCompetitionDistributedAdmissionPreservesOriginalClockAfterReadiness(t *testing.T) {
	roundId := server.NewId()
	startedAt := time.Unix(1700000000, 0).UTC()
	original := &competitionExecutorQualificationRun{JobId: server.NewId(), StartedAt: startedAt, DeadlineAt: startedAt.Add(3 * time.Hour)}
	checks, reservations := 0, 0
	for range 2 {
		_, run, err := reserveCompetitionReadyExecutorAdmission(t.Context(), roundId,
			func(context.Context) (HostSelfCheck, error) {
				checks++
				return HostSelfCheck{RebaselinePassed: true, RebaselineRoundId: &roundId}, nil
			}, func(context.Context) (*competitionExecutorQualificationRun, error) {
				reservations++
				if checks != reservations {
					t.Fatal("clock reservation preceded readiness")
				}
				return original, nil
			})
		if err != nil || run != original || !run.StartedAt.Equal(startedAt) || !run.DeadlineAt.Equal(startedAt.Add(3*time.Hour)) {
			t.Fatal("readiness replaced the original qualification clock", err)
		}
	}
}

// Synthetic full-R9 evidence uses broad raw noise deliberately: the confidence
// interval exceeds five percent, yet the approved operational screen can pass.
type competitionNoopScreenFixture struct {
	settings  *Settings
	job       *queuedJob
	host      CompetitionExecutorHostPolicy
	directory string
	response  competitionagent.Response
	outcome   EvaluationOutcome
}

// No services, database, evaluator process or production identity is needed.
func newCompetitionNoopScreenFixture(t *testing.T) *competitionNoopScreenFixture {
	t.Helper()
	settings := automationApiSettings()
	settings.EvaluationPolicy.HardwareId = "synthetic-hardware"
	settings.EvaluationPolicy.Replicates = 9
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	settings.FutureEpochPolicy.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	var err error
	settings, err = settings.ForCompetitionEpoch(2)
	if err != nil {
		t.Fatal(err)
	}
	settings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{Settings: defaultCompetitionDistributedCoordinatorSettings()}}
	host := settings.DistributedEvaluationPolicy.Hosts[1]
	settings.executorHostId, settings.executorSlotId = host.HostId, host.SlotId
	round, _ := sealedTestRound(t, settings)
	round.Epoch, round.ProvidersSha256 = 2, strings.Repeat("c", 64)
	started := server.NowUtc().Truncate(time.Microsecond)
	deadline := started.Add(3 * time.Hour)
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: competitionExecutorQualificationJobId(round.RoundId, host.HostId, settings.DistributedEvaluationPolicy.ApprovalSha256), RoundId: round.RoundId,
		StartedAt: &started, EvaluatorImageDigest: settings.EvaluatorImageDigest}, Round: *round, AttemptCount: 1, ExecutionDeadline: &deadline}
	rawScores := []float64{90, 100, 110, 90, 100, 110, 90, 100, 110}
	controlBytes := testRoundBaseline(t, settings, round, rawScores)
	controlHash := sha256.Sum256(controlBytes)
	job.RoundBaseline = &roundBaselineRecord{RoundId: round.RoundId, Json: controlBytes, Sha256: hex.EncodeToString(controlHash[:])}
	directory, err := os.MkdirTemp(t.TempDir(), "noop-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &competitionNoopScreenFixture{settings: settings, job: job, host: host, directory: directory}
	replicates := []map[string]any{}
	control := &roundBaselineManifest{}
	if err := json.Unmarshal(controlBytes, control); err != nil {
		t.Fatal(err)
	}
	for index, replicate := range control.Replicates {
		id := fmt.Sprintf("candidate-%02d-synthetic", index+1)
		replicates = append(replicates, map[string]any{"evaluation_id": id, "raw_score": replicate.RawScore, "request_count": replicate.RequestCount, "received_bytes": replicate.ReceivedBytes, "work_coverage": replicate.WorkCoverage})
		content, err := json.Marshal(map[string]any{"evaluation_id": id, "config_sha256": round.ProvidersSha256, "source_epoch": evaluationSourceEpoch(round), "metrics": replicate.LiveMetrics, "work_coverage_policy": settings.EvaluationPolicy.WorkCoveragePolicy, "work_coverage": replicate.WorkCoverage})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("evidence", "scorer-input", id, "run.json")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeExclusiveFile(filepath.Join(directory, path), content, 0400); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		fixture.response.Files = append(fixture.response.Files, competitionagent.File{Path: filepath.ToSlash(path), Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))})
	}
	raw, normalized := 100.0, 100.0
	significance := testScoreSignificance(false)
	significance.ReplicateCount, significance.BaselineMeanRawScore, significance.CandidateMeanRawScore, significance.ObservedImprovementPercent = 9, 100, 100, 0
	significance.TakeoverMarginPercent = settings.EvaluationPolicy.TakeoverMargin * 100
	gates := map[string]Gate{}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		gates[name] = Gate{Passed: true, Details: map[string]any{}}
	}
	fixture.outcome = EvaluationOutcome{Score: &ScoreResult{ScoreSchema: ScoreSchema, RawScore: &raw, NormalizedScore: &normalized, Placeable: true, Gates: gates, Significance: significance,
		Diagnostics: map[string]any{"g2_volume_policy": string(settings.EvaluationPolicy.G2VolumePolicy), "scorer_version": settings.scorerVersion(), "baseline_sha256": job.RoundBaseline.Sha256,
			"replicate_count": 9, "replicates": replicates}}, ArtifactManifest: json.RawMessage("{\"schema\":1,\"synthetic_retained_manifest\":true}"), distributedArchiveVerified: true}
	if settings.EvaluationPolicy.WorkCoveragePolicy != nil {
		fixture.outcome.Score.Gates["G7_work_coverage"] = Gate{Passed: true, Details: map[string]any{"policy": settings.EvaluationPolicy.WorkCoveragePolicy}}
		fixture.outcome.Score.Diagnostics["work_coverage_policy"] = settings.EvaluationPolicy.WorkCoveragePolicy
	}
	return fixture
}

// Tests emulate a fully authenticated retained object, then alter/re-hash it
// to verify semantic checks rather than relying only on an unchanged digest.
func sealCompetitionNoopScreenReceipt(t testing.TB, settings *Settings, receipt *competitionExecutorEquivalenceReceipt) {
	t.Helper()
	content, err := competitionExecutorReceiptBytes(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	receipt.Retention = &retainedArtifact{Path: "executor-qualification.json", Key: "synthetic/retained/qualification", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content)),
		VersionId: "synthetic-version", Mode: "COMPLIANCE", RetainUntil: settings.RetainUntil}
}

// Uncertainty larger than the band is retained honestly, not labeled proven
// equivalence. Missing mandatory gates cannot qualify even a perfect point mean.
func TestCompetitionDistributedNoopAdmissionRetainsUncertaintyWithoutEquivalenceClaim(t *testing.T) {
	fixture := newCompetitionNoopScreenFixture(t)
	receipt, err := screenCompetitionExecutorNoop(fixture.settings, fixture.job, fixture.host, fixture.directory, fixture.response, fixture.outcome)
	if err != nil || !receipt.Passed || receipt.StatisticalEquivalenceProven || len(receipt.Metrics) != len(competitionNoopMetricNames(fixture.settings)) || receipt.Metrics[0].DifferenceCiHigh <= receipt.Metrics[0].BaselineMean*0.05 {
		t.Fatal("operational screen weakened or misrepresented full-R9 uncertainty", err)
	}
	sealCompetitionNoopScreenReceipt(t, fixture.settings, receipt)
	if err := validateCompetitionExecutorReceipt(fixture.settings, &fixture.job.Round, fixture.job.RoundBaseline, fixture.host, receipt); err != nil {
		t.Fatal("authentic retained full-R9 admission was rejected", err)
	}
	delete(fixture.outcome.Score.Gates, "G6_resources")
	if _, err := screenCompetitionExecutorNoop(fixture.settings, fixture.job, fixture.host, fixture.directory, fixture.response, fixture.outcome); err == nil {
		t.Fatal("missing resource gate was accepted")
	}
}

// Rehashed duplicate metrics, false raw differences and invented variances/CIs
// must fail; a self-consistent object hash is not sufficient admission evidence.
func TestCompetitionDistributedNoopReceiptRejectsSemanticTampering(t *testing.T) {
	fixture := newCompetitionNoopScreenFixture(t)
	original, err := screenCompetitionExecutorNoop(fixture.settings, fixture.job, fixture.host, fixture.directory, fixture.response, fixture.outcome)
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*competitionExecutorEquivalenceReceipt)
	}{
		{name: "duplicate metrics", mutate: func(receipt *competitionExecutorEquivalenceReceipt) { receipt.Metrics[1] = receipt.Metrics[0] }},
		{name: "invented variance", mutate: func(receipt *competitionExecutorEquivalenceReceipt) { receipt.Metrics[0].BaselineSampleVariance++ }},
		{name: "invented interval", mutate: func(receipt *competitionExecutorEquivalenceReceipt) { receipt.Metrics[0].DifferenceCiHigh++ }},
		{name: "false raw difference", mutate: func(receipt *competitionExecutorEquivalenceReceipt) { receipt.RawScoreRelativeDifference = 0.01 }},
		{name: "missing within-run uncertainty", mutate: func(receipt *competitionExecutorEquivalenceReceipt) { receipt.Metrics[3].CandidateWithinRunSe = nil }},
		{name: "different shared control", mutate: func(receipt *competitionExecutorEquivalenceReceipt) { receipt.Metrics[0].BaselineValues[0]++ }},
		{name: "false equivalence claim", mutate: func(receipt *competitionExecutorEquivalenceReceipt) { receipt.StatisticalEquivalenceProven = true }},
	} {
		receipt := &competitionExecutorEquivalenceReceipt{}
		if err := json.Unmarshal(content, receipt); err != nil {
			t.Fatal(err)
		}
		change.mutate(receipt)
		sealCompetitionNoopScreenReceipt(t, fixture.settings, receipt)
		if err := validateCompetitionExecutorReceipt(fixture.settings, &fixture.job.Round, fixture.job.RoundBaseline, fixture.host, receipt); err == nil {
			t.Errorf("semantically altered receipt was accepted: %s", change.name)
		}
	}
}
