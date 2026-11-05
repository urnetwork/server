// Full-R9, unchanged-code admission against the single frozen round control.
// Passing this operational screen is deliberately not called statistical
// equivalence: the complete run-level uncertainty is retained beside the result.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
)

// Run-level variance is the error term; request counts are never substituted
// for independent replicates. Block standard errors remain diagnostics only.
type competitionHostMetricScreen struct {
	Metric                  string    `json:"metric"`
	BaselineValues          []float64 `json:"baseline_values"`
	CandidateValues         []float64 `json:"candidate_values"`
	BaselineWithinRunSe     []float64 `json:"baseline_within_run_se,omitempty"`
	CandidateWithinRunSe    []float64 `json:"candidate_within_run_se,omitempty"`
	BaselineMean            float64   `json:"baseline_mean"`
	CandidateMean           float64   `json:"candidate_mean"`
	BaselineSampleVariance  float64   `json:"baseline_sample_variance"`
	CandidateSampleVariance float64   `json:"candidate_sample_variance"`
	Difference              float64   `json:"difference"`
	StandardError           float64   `json:"standard_error"`
	DegreesOfFreedom        float64   `json:"degrees_of_freedom"`
	DifferenceCiLow         float64   `json:"difference_ci_low"`
	DifferenceCiHigh        float64   `json:"difference_ci_high"`
	TwoSidedPValue          float64   `json:"two_sided_p_value"`
	SignificantShift        bool      `json:"significant_shift"`
}

// The immutable retained receipt binds every host to this exact seed/workload,
// source, image, local fixtures, full shared baseline and original no-op clock.
type competitionExecutorEquivalenceReceipt struct {
	Schema                            int                           `json:"schema"`
	Kind                              string                        `json:"kind"`
	RoundId                           string                        `json:"round_id"`
	HostId                            string                        `json:"host_id"`
	BaselineSha256                    string                        `json:"baseline_sha256"`
	SourceLockSha256                  string                        `json:"source_lock_sha256"`
	ProvidersSha256                   string                        `json:"providers_sha256"`
	EvaluatorImageDigest              string                        `json:"evaluator_image_digest"`
	QualificationSha256               string                        `json:"qualification_sha256"`
	ConfigLocalSha256                 string                        `json:"config_local_sha256"`
	VaultLocalSha256                  string                        `json:"vault_local_sha256"`
	PolicyApprovalSha256              string                        `json:"policy_approval_sha256"`
	QualificationJobId                string                        `json:"qualification_job_id"`
	QualificationStartedAt            time.Time                     `json:"qualification_started_at"`
	QualificationDeadlineAt           time.Time                     `json:"qualification_deadline_at"`
	Noop                              bool                          `json:"noop"`
	Passed                            bool                          `json:"passed"`
	Replicates                        int                           `json:"replicates"`
	ScreenKind                        string                        `json:"screen_kind"`
	MaximumRawScoreRelativeDifference float64                       `json:"maximum_raw_score_relative_difference"`
	RawScoreRelativeDifference        float64                       `json:"raw_score_relative_difference"`
	ConfidenceLevel                   float64                       `json:"confidence_level"`
	StatisticalMethod                 string                        `json:"statistical_method"`
	StatisticalEquivalenceProven      bool                          `json:"statistical_equivalence_proven"`
	Metrics                           []competitionHostMetricScreen `json:"metrics"`
	ArchiveManifestSha256             string                        `json:"archive_manifest_sha256"`
	CreatedAt                         time.Time                     `json:"created_at"`
	Retention                         *retainedArtifact             `json:"retention,omitempty"`
}

// All hosts finish their independently owned probes, including when one fails.
// None may claim a public job before every immutable passing receipt exists.
func qualifyCompetitionDistributedHosts(ctx context.Context, settings *Settings, store competitionSupervisorWorkerStore, baseline *roundBaselineRecord) error {
	if settings.EvaluationPolicy.Replicates != 9 {
		return errors.New("distributed host admission requires the exact full R9 policy")
	}
	round, err := store.GetRound(ctx, settings, store.roundId)
	if err != nil {
		return err
	}
	results := make(chan error, len(settings.DistributedEvaluationPolicy.Hosts))
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		go func() {
			results <- qualifyCompetitionDistributedHost(ctx, settings, store.PostgresStore, round, baseline, host)
		}()
	}
	var failures []error
	for range settings.DistributedEvaluationPolicy.Hosts {
		if err := <-results; err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// A durable clock reservation precedes input preparation. Reconnects reconstruct
// exactly the same signed manifest, never another no-op sample or later clock.
func qualifyCompetitionDistributedHost(ctx context.Context, settings *Settings, store PostgresStore, round *roundRecord, baseline *roundBaselineRecord, host CompetitionExecutorHostPolicy) error {
	qualified, err := store.distributedExecutorQualified(ctx, settings, round, baseline, host)
	if err != nil || qualified {
		return err
	}
	jobId := competitionExecutorQualificationJobId(round.RoundId, host.HostId, settings.DistributedEvaluationPolicy.ApprovalSha256)
	run, err := store.reserveDistributedExecutorQualification(ctx, settings, round.RoundId, host.HostId, jobId)
	if err != nil {
		return err
	}
	startedAt, deadlineAt := run.StartedAt, run.DeadlineAt
	operationCtx, cancel := context.WithDeadline(ctx, deadlineAt)
	defer cancel()
	selected := *settings
	selected.executorHostId, selected.executorSlotId = host.HostId, host.SlotId
	transport, err := competitionTransportForHost(&selected, host)
	if err != nil {
		return err
	}
	evaluator := &competitionAgentEvaluator{host: host, transport: transport}
	check, err := evaluator.SelfCheck(operationCtx, &selected)
	if err != nil || !check.RebaselinePassed || check.RebaselineRoundId == nil || *check.RebaselineRoundId != round.RoundId {
		return errors.New("executor no-op admission requires genuine same-round containment readiness")
	}
	if err := store.RegisterHost(operationCtx, &selected, check); err != nil {
		return err
	}
	path, source, err := readCompetitionQualificationSource(operationCtx, settings.ArtifactRoot, settings)
	if err != nil {
		return err
	}
	patch, err := competitionQualificationPatch(path, source, settings.PatchPolicy)
	if err != nil {
		return err
	}
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: jobId, RoundId: round.RoundId, StartedAt: &startedAt,
		PatchSha256: patch.Sha256, EvaluatorImageDigest: settings.EvaluatorImageDigest, ApiImageDigest: run.ApiImageDigest, WorkerImageDigest: run.WorkerImageDigest},
		Patch: patch.Bytes, AttemptCount: 1, Round: *round, RoundBaseline: baseline, ExecutorHostId: host.HostId, SlotId: host.SlotId,
		SlotFencingToken: settings.automationLease.FencingToken, ExecutionDeadline: &deadlineAt}
	// The real main products provide control-plane identity; no competition
	// branch is ever used to run the coordinator or its database services.
	var screened bool
	evaluator.afterRetained = func(directory string, response competitionagent.Response, outcome EvaluationOutcome) error {
		receipt, err := screenCompetitionExecutorNoop(&selected, job, host, directory, response, outcome)
		if err != nil {
			return err
		}
		archiver, ok := settings.artifactArchive.(interface {
			ArchiveExecutorQualification(context.Context, *Settings, *queuedJob, []byte) (*retainedArtifact, error)
		})
		if !ok {
			return errors.New("executor admission requires retained qualification evidence")
		}
		content, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		retained, err := archiver.ArchiveExecutorQualification(operationCtx, settings, job, content)
		if err != nil {
			return err
		}
		receipt.Retention = retained
		if !receipt.Passed {
			return errors.New("no-op host admission failed; original sample and uncertainty retained, no resampling")
		}
		if err := store.recordDistributedExecutorQualification(operationCtx, settings, round, baseline, host, receipt); err != nil {
			return err
		}
		screened = true
		return nil
	}
	outcome := evaluator.Evaluate(operationCtx, &selected, job)
	if !screened || !outcome.distributedArchiveVerified || outcome.Error != nil {
		return fmt.Errorf("executor %s did not pass retained full-R9 no-op admission", host.HostId)
	}
	return nil
}

// Preserves every run value and block diagnostic, and rejects significant shifts
// independently for volume, latency and throughput. The five-percent bound is
// the approved raw-score admission band only, not a candidate volume ceiling.
func screenCompetitionExecutorNoop(settings *Settings, job *queuedJob, host CompetitionExecutorHostPolicy, directory string, response competitionagent.Response, outcome EvaluationOutcome) (*competitionExecutorEquivalenceReceipt, error) {
	if outcome.Score == nil || outcome.Error != nil || !outcome.distributedArchiveVerified || validateScore(outcome.Score) != nil || !outcome.Score.Placeable || len(outcome.ArtifactManifest) == 0 || job.ExecutionDeadline == nil || job.StartedAt == nil {
		return nil, errors.New("no-op did not complete all scoring, containment and retention gates")
	}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		gate, present := outcome.Score.Gates[name]
		if !present || !gate.Passed {
			return nil, errors.New("no-op omitted or failed a mandatory frozen score gate")
		}
	}
	for _, gate := range outcome.Score.Gates {
		if !gate.Passed {
			return nil, errors.New("no-op failed a frozen score gate")
		}
	}
	baseline := &roundBaselineManifest{}
	if err := json.Unmarshal(job.RoundBaseline.Json, baseline); err != nil || len(baseline.Replicates) != 9 {
		return nil, errors.New("no-op requires the exact shared full-R9 control")
	}
	diagnosticsBytes, err := json.Marshal(outcome.Score.Diagnostics)
	if err != nil {
		return nil, err
	}
	diagnostics := struct {
		ReplicateCount int `json:"replicate_count"`
		Replicates     []struct {
			EvaluationId  string  `json:"evaluation_id"`
			RawScore      float64 `json:"raw_score"`
			RequestCount  int64   `json:"request_count"`
			ReceivedBytes int64   `json:"received_bytes"`
		} `json:"replicates"`
	}{}
	if json.Unmarshal(diagnosticsBytes, &diagnostics) != nil || diagnostics.ReplicateCount != 9 || len(diagnostics.Replicates) != 9 {
		return nil, errors.New("no-op omitted full-R9 run-level diagnostics")
	}
	baselineValues := map[string][]float64{"raw_score": {}, "request_count": {}, "received_bytes": {}}
	candidateValues := map[string][]float64{"raw_score": {}, "request_count": {}, "received_bytes": {}}
	baselineWithinSe, candidateWithinSe := map[string][]float64{}, map[string][]float64{}
	for _, replicate := range baseline.Replicates {
		baselineValues["raw_score"] = append(baselineValues["raw_score"], replicate.RawScore)
		baselineValues["request_count"] = append(baselineValues["request_count"], float64(replicate.RequestCount))
		baselineValues["received_bytes"] = append(baselineValues["received_bytes"], float64(replicate.ReceivedBytes))
		for name := range roundBaselineLiveMetrics {
			metric, present := replicate.LiveMetrics[name]
			if !present || metric.N <= 0 {
				return nil, errors.New("shared control omitted a live-metric uncertainty record")
			}
			baselineValues[name] = append(baselineValues[name], metric.Value)
			baselineWithinSe[name] = append(baselineWithinSe[name], metric.BlockSe)
		}
	}
	for _, replicate := range diagnostics.Replicates {
		candidateValues["raw_score"] = append(candidateValues["raw_score"], replicate.RawScore)
		candidateValues["request_count"] = append(candidateValues["request_count"], float64(replicate.RequestCount))
		candidateValues["received_bytes"] = append(candidateValues["received_bytes"], float64(replicate.ReceivedBytes))
		// Read the exact scorer-input sidecar, not another retained copy of
		// that run. Its digest is already in the signed raw evidence manifest.
		runPath := filepath.ToSlash(filepath.Join("evidence", "scorer-input", replicate.EvaluationId, "run.json"))
		declared := false
		for _, file := range response.Files {
			declared = declared || file.Path == runPath
		}
		if !declared || !strings.HasPrefix(replicate.EvaluationId, "candidate-") || strings.ContainsAny(replicate.EvaluationId, "/\\") {
			return nil, errors.New("candidate live metrics are not bound to authenticated run evidence")
		}
		content, err := readRegularFile(filepath.Join(directory, runPath), settings.distributedCoordinator.Config.Settings.Limits.DocumentBytes)
		if err != nil {
			return nil, err
		}
		run := struct {
			EvaluationId string                                `json:"evaluation_id"`
			ConfigSha256 string                                `json:"config_sha256"`
			SourceEpoch  int                                   `json:"source_epoch"`
			Metrics      map[string]roundBaselineMetricSummary `json:"metrics"`
		}{}
		if json.Unmarshal(content, &run) != nil || run.EvaluationId != replicate.EvaluationId || run.ConfigSha256 != job.Round.ProvidersSha256 || run.SourceEpoch != evaluationSourceEpoch(&job.Round) {
			return nil, errors.New("no-op sidecar changed the frozen workload or source epoch")
		}
		for name := range roundBaselineLiveMetrics {
			metric, present := run.Metrics[name]
			if !present || metric.N <= 0 || metric.BlockSe < 0 || math.IsNaN(metric.BlockSe) || math.IsInf(metric.BlockSe, 0) {
				return nil, errors.New("no-op omitted per-metric run-level uncertainty")
			}
			candidateValues[name] = append(candidateValues[name], metric.Value)
			candidateWithinSe[name] = append(candidateWithinSe[name], metric.BlockSe)
		}
	}
	policy := settings.DistributedEvaluationPolicy
	manifestDigest := sha256.Sum256(outcome.ArtifactManifest)
	receipt := &competitionExecutorEquivalenceReceipt{Schema: 1, Kind: "competition-executor-noop-admission/v1", RoundId: job.RoundId.String(), HostId: host.HostId,
		BaselineSha256: job.RoundBaseline.Sha256, SourceLockSha256: settings.AutomationPolicy.SourceLockSha256, ProvidersSha256: job.Round.ProvidersSha256,
		EvaluatorImageDigest: job.EvaluatorImageDigest, QualificationSha256: host.QualificationSha256, ConfigLocalSha256: settings.EvaluationPolicy.ConfigLocalSha256, VaultLocalSha256: settings.EvaluationPolicy.VaultLocalSha256,
		PolicyApprovalSha256: policy.ApprovalSha256, QualificationJobId: job.JobId.String(), QualificationStartedAt: job.StartedAt.UTC(), QualificationDeadlineAt: job.ExecutionDeadline.UTC(),
		Noop: true, Passed: true, Replicates: 9, ScreenKind: policy.EquivalencePolicy.ScreenKind, MaximumRawScoreRelativeDifference: policy.EquivalencePolicy.MaximumRawScoreRelativeDifference,
		ConfidenceLevel: 0.95, StatisticalMethod: "two-sided-welch-t-run-level", StatisticalEquivalenceProven: false,
		ArchiveManifestSha256: hex.EncodeToString(manifestDigest[:]), CreatedAt: server.NowUtc().Truncate(time.Microsecond)}
	for _, name := range []string{"raw_score", "request_count", "received_bytes", "ttfb_p50_ms", "ttfb_p95_ms", "throughput_p50_bytes_per_s", "throughput_p95_bytes_per_s"} {
		screen, err := compareCompetitionHostMetric(name, baselineValues[name], candidateValues[name])
		if err != nil {
			return nil, err
		}
		screen.BaselineWithinRunSe, screen.CandidateWithinRunSe = baselineWithinSe[name], candidateWithinSe[name]
		receipt.Metrics = append(receipt.Metrics, *screen)
		receipt.Passed = receipt.Passed && !screen.SignificantShift
		if name == "raw_score" {
			if screen.BaselineMean <= 0 {
				return nil, errors.New("shared no-op raw-score mean must be positive")
			}
			receipt.RawScoreRelativeDifference = math.Abs(screen.Difference) / screen.BaselineMean
			receipt.Passed = receipt.Passed && receipt.RawScoreRelativeDifference <= policy.EquivalencePolicy.MaximumRawScoreRelativeDifference
		}
	}
	return receipt, nil
}

// Typed canonical bytes authenticate receipt identity before SQL insertion and
// again on restart. The external retained object omits its own retention link.
func competitionExecutorReceiptBytes(receipt *competitionExecutorEquivalenceReceipt) ([]byte, error) {
	copy := *receipt
	copy.Retention = nil
	return json.Marshal(copy)
}

// Existing receipts cannot be relabeled as evidence for a later seed or policy.
func validateCompetitionExecutorReceipt(settings *Settings, round *roundRecord, baseline *roundBaselineRecord, host CompetitionExecutorHostPolicy, receipt *competitionExecutorEquivalenceReceipt) error {
	if receipt == nil || receipt.Schema != 1 || receipt.Kind != "competition-executor-noop-admission/v1" || receipt.RoundId != round.RoundId.String() || receipt.HostId != host.HostId ||
		receipt.BaselineSha256 != baseline.Sha256 || receipt.SourceLockSha256 != settings.AutomationPolicy.SourceLockSha256 || receipt.ProvidersSha256 != round.ProvidersSha256 ||
		receipt.EvaluatorImageDigest != settings.EvaluatorImageDigest || receipt.QualificationSha256 != host.QualificationSha256 || receipt.ConfigLocalSha256 != settings.EvaluationPolicy.ConfigLocalSha256 || receipt.VaultLocalSha256 != settings.EvaluationPolicy.VaultLocalSha256 ||
		receipt.PolicyApprovalSha256 != settings.DistributedEvaluationPolicy.ApprovalSha256 || !receipt.Noop || !receipt.Passed || receipt.Replicates != 9 || receipt.StatisticalEquivalenceProven ||
		receipt.ScreenKind != "operational-admission/v1" || receipt.MaximumRawScoreRelativeDifference != 0.05 || receipt.RawScoreRelativeDifference < 0 || receipt.RawScoreRelativeDifference > 0.05 || math.IsNaN(receipt.RawScoreRelativeDifference) || math.IsInf(receipt.RawScoreRelativeDifference, 0) || receipt.ConfidenceLevel != 0.95 ||
		receipt.StatisticalMethod != "two-sided-welch-t-run-level" || !sha256Pattern.MatchString(receipt.ArchiveManifestSha256) || receipt.CreatedAt.After(receipt.QualificationDeadlineAt) || len(receipt.Metrics) != 7 ||
		receipt.QualificationJobId != competitionExecutorQualificationJobId(round.RoundId, host.HostId, settings.DistributedEvaluationPolicy.ApprovalSha256).String() || receipt.QualificationStartedAt.IsZero() ||
		!receipt.QualificationDeadlineAt.Equal(receipt.QualificationStartedAt.Add(3*time.Hour)) || receipt.CreatedAt.Before(receipt.QualificationStartedAt) ||
		receipt.Retention == nil || receipt.Retention.Mode != "COMPLIANCE" || receipt.Retention.RetainUntil.Before(settings.RetainUntil) {
		return errors.New("retained host admission receipt does not match the frozen round")
	}
	content, err := competitionExecutorReceiptBytes(receipt)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != receipt.Retention.Sha256 || int64(len(content)) != receipt.Retention.Bytes {
		return errors.New("host admission receipt differs from its retained object")
	}
	control := &roundBaselineManifest{}
	if json.Unmarshal(baseline.Json, control) != nil || len(control.Replicates) != 9 {
		return errors.New("host admission shared control is incomplete")
	}
	baselineValues := map[string][]float64{"raw_score": {}, "request_count": {}, "received_bytes": {}}
	baselineWithinSe := map[string][]float64{}
	for _, replicate := range control.Replicates {
		baselineValues["raw_score"] = append(baselineValues["raw_score"], replicate.RawScore)
		baselineValues["request_count"] = append(baselineValues["request_count"], float64(replicate.RequestCount))
		baselineValues["received_bytes"] = append(baselineValues["received_bytes"], float64(replicate.ReceivedBytes))
		for name := range roundBaselineLiveMetrics {
			metric, present := replicate.LiveMetrics[name]
			if !present {
				return errors.New("host admission shared control omitted a required live metric")
			}
			baselineValues[name] = append(baselineValues[name], metric.Value)
			baselineWithinSe[name] = append(baselineWithinSe[name], metric.BlockSe)
		}
	}
	seen := map[string]bool{}
	for _, metric := range receipt.Metrics {
		values, present := baselineValues[metric.Metric]
		if !present || seen[metric.Metric] || len(metric.BaselineValues) != len(values) {
			return errors.New("host admission must retain all seven distinct frozen-control metrics")
		}
		seen[metric.Metric] = true
		for index, value := range values {
			if metric.BaselineValues[index] != value {
				return errors.New("host admission changed its shared-control sample values")
			}
		}
		expected, err := compareCompetitionHostMetric(metric.Metric, metric.BaselineValues, metric.CandidateValues)
		if err != nil || expected.SignificantShift {
			return errors.New("host admission contains a significant or unavailable metric shift")
		}
		expected.BaselineWithinRunSe = baselineWithinSe[metric.Metric]
		expected.CandidateWithinRunSe = metric.CandidateWithinRunSe
		if len(expected.BaselineWithinRunSe) != len(metric.BaselineWithinRunSe) || len(expected.BaselineWithinRunSe) != len(metric.CandidateWithinRunSe) {
			return errors.New("host admission omitted within-run uncertainty records")
		}
		for index, value := range expected.BaselineWithinRunSe {
			candidateSe := metric.CandidateWithinRunSe[index]
			if metric.BaselineWithinRunSe[index] != value || math.IsNaN(candidateSe) || math.IsInf(candidateSe, 0) || candidateSe < 0 {
				return errors.New("host admission changed within-run uncertainty records")
			}
		}
		expectedBytes, expectedErr := json.Marshal(expected)
		actualBytes, actualErr := json.Marshal(metric)
		if expectedErr != nil || actualErr != nil || !bytes.Equal(expectedBytes, actualBytes) {
			return errors.New("host admission receipt changed its run-level uncertainty")
		}
		if metric.Metric == "raw_score" && (expected.BaselineMean <= 0 || receipt.RawScoreRelativeDifference != math.Abs(expected.Difference)/expected.BaselineMean) {
			return errors.New("host admission changed its raw-score five-percent screen")
		}
	}
	return nil
}
