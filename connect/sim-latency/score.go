package main

// Official Apex scorer for score_schema 1. Evaluation orchestration remains
// outside this file; the scorer is a deterministic, file-only reducer over an
// immutable artifact bundle.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/docopt/docopt-go"
	"google.golang.org/protobuf/proto"

	statspkg "github.com/urnetwork/server/stats"
	"github.com/urnetwork/server/stats/sample"
)

const scoreBaselineKind = "sim-latency-score-baseline"
const accountingSnapshotKind = "sim-latency-accounting"
const resourceReportKind = "sim-latency-resource-report"
const scoreResultMaxInputBytes = 128 << 20
const minimumFindProvidersSampleSpanFraction = 0.90

// ScoreBaselineReplicate contains trusted same-round baseline diagnostics for
// one replicate. The scorer recomputes every aggregate from these values.
type ScoreBaselineReplicate struct {
	RawScore                        float64 `json:"raw_score"`
	SuccessRate                     float64 `json:"success_rate"`
	RequestCount                    int64   `json:"request_count"`
	ReceivedBytes                   int64   `json:"received_bytes"`
	FindProvidersLoadP95Ms          float64 `json:"findproviders_load_p95_ms"`
	FindProvidersPoolP05            float64 `json:"findproviders_pool_p05"`
	FindProvidersSampleSpanFraction float64 `json:"findproviders_sample_span_fraction"`
}

// ScoreBaselineManifest is the signed, hidden-seed round contract consumed by
// score. Replicates and takeover margin are deliberately data, not CLI knobs:
// they are frozen once for a round after calibration.
type ScoreBaselineManifest struct {
	ScoreSchema      int                      `json:"score_schema"`
	Kind             string                   `json:"kind"`
	ScorerVersion    string                   `json:"scorer_version"`
	RoundId          string                   `json:"round_id"`
	ConfigSha256     string                   `json:"config_sha256"`
	RequestTimeoutMs int64                    `json:"request_timeout_ms"`
	RunFlags         map[string]string        `json:"run_flags"`
	TakeoverMargin   float64                  `json:"takeover_margin"`
	Replicates       []ScoreBaselineReplicate `json:"replicates"`
}

type AccountingSnapshot struct {
	Schema              int    `json:"schema"`
	Kind                string `json:"kind"`
	EvaluationId        string `json:"evaluation_id"`
	Complete            bool   `json:"complete"`
	MeasureStartMs      int64  `json:"measure_start_ms"`
	MeasureEndMs        int64  `json:"measure_end_ms"`
	ProviderEgressBytes int64  `json:"provider_egress_bytes"`
}

type ResourceReport struct {
	Schema             int     `json:"schema"`
	Kind               string  `json:"kind"`
	EvaluationId       string  `json:"evaluation_id"`
	CgroupId           string  `json:"cgroup_id"`
	MeasurementStartMs int64   `json:"measurement_start_ms"`
	MeasurementEndMs   int64   `json:"measurement_end_ms"`
	Complete           bool    `json:"complete"`
	ExitCode           int     `json:"exit_code"`
	OomKilled          bool    `json:"oom_killed"`
	HardKilled         bool    `json:"hard_killed"`
	LimitEscape        bool    `json:"limit_escape"`
	MeasurementMissing bool    `json:"measurement_missing"`
	CpuSeconds         float64 `json:"cpu_seconds,omitempty"`
	PeakRssBytes       int64   `json:"peak_rss_bytes,omitempty"`
}

type ScoreInputs struct {
	Runs             []string
	Stderr           []string
	Accounting       []string
	Samples          []string
	ResourceReports  []string
	Markers          []string
	BaselineManifest string
}

// ScoreBaselineInputs is the evaluator-owned baseline artifact bundle. It has
// the same authenticated inputs as a candidate bundle, except that its round
// contract is derived from the first run and cross-checked across every
// replicate before the manifest is emitted.
type ScoreBaselineInputs struct {
	Runs            []string
	Stderr          []string
	Accounting      []string
	Samples         []string
	ResourceReports []string
	Markers         []string
	RoundId         string
	TakeoverMargin  float64
}

type ScoreEvalError struct {
	Kind    string `json:"kind"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ScoreGate struct {
	Passed  bool           `json:"passed"`
	Details map[string]any `json:"details"`
}

type ScoreReplicateDiagnostics struct {
	EvaluationId                    string   `json:"evaluation_id"`
	RawScore                        float64  `json:"raw_score"`
	SuccessRate                     float64  `json:"success_rate"`
	RequestCount                    int64    `json:"request_count"`
	ReceivedBytes                   int64    `json:"received_bytes"`
	ProviderEgressBytes             int64    `json:"provider_egress_bytes"`
	AccountingCoverage              float64  `json:"accounting_coverage"`
	FindProvidersSamples            int64    `json:"findproviders_samples"`
	FindProvidersEmptyPools         int64    `json:"findproviders_empty_pools"`
	FindProvidersLoadP95Ms          float64  `json:"findproviders_load_p95_ms"`
	FindProvidersPoolP05            float64  `json:"findproviders_pool_p05"`
	FindProvidersSampleSpanFraction float64  `json:"findproviders_sample_span_fraction"`
	StabilityFindings               []string `json:"stability_findings"`
	ResourceFindings                []string `json:"resource_findings"`
}

type ScoreAggregateDiagnostics struct {
	RawScore                        float64 `json:"raw_score"`
	SuccessRate                     float64 `json:"success_rate"`
	RequestCount                    float64 `json:"request_count"`
	ReceivedBytes                   float64 `json:"received_bytes"`
	AccountingCoverage              float64 `json:"accounting_coverage,omitempty"`
	FindProvidersLoadP95Ms          float64 `json:"findproviders_load_p95_ms"`
	FindProvidersPoolP05            float64 `json:"findproviders_pool_p05"`
	FindProvidersSampleSpanFraction float64 `json:"findproviders_sample_span_fraction"`
}

type ScoreDiagnostics struct {
	RoundId                  string                      `json:"round_id,omitempty"`
	ReplicateCount           int                         `json:"replicate_count,omitempty"`
	RequestTimeoutMs         int64                       `json:"request_timeout_ms,omitempty"`
	TakeoverMargin           float64                     `json:"takeover_margin,omitempty"`
	BaselineTakeoverRawMax   float64                     `json:"baseline_takeover_raw_max,omitempty"`
	BaselineTakeoverEligible *bool                       `json:"baseline_takeover_eligible,omitempty"`
	Baseline                 *ScoreAggregateDiagnostics  `json:"baseline,omitempty"`
	Candidate                *ScoreAggregateDiagnostics  `json:"candidate,omitempty"`
	Replicates               []ScoreReplicateDiagnostics `json:"replicates,omitempty"`
}

type ScoreResult struct {
	ScoreSchema     int                  `json:"score_schema"`
	RawScore        float64              `json:"raw_score"`
	NormalizedScore float64              `json:"normalized_score"`
	Placeable       bool                 `json:"placeable"`
	Gates           map[string]ScoreGate `json:"gates"`
	Significance    *ScoreSignificance   `json:"significance"`
	Diagnostics     ScoreDiagnostics     `json:"diagnostics"`
	EvalError       *ScoreEvalError      `json:"eval_error"`
}

type scoreCodedError struct {
	kind    string
	code    string
	message string
}

func (self *scoreCodedError) Error() string { return self.message }

func scoreError(code string, format string, args ...any) error {
	return &scoreCodedError{kind: "infrastructure", code: code, message: fmt.Sprintf(format, args...)}
}

func submissionScoreError(code string, format string, args ...any) error {
	return &scoreCodedError{kind: "submission", code: code, message: fmt.Sprintf(format, args...)}
}

func errorScore(kind string, code string, message string) *ScoreResult {
	return &ScoreResult{
		ScoreSchema: apexScoreSchema,
		Gates:       map[string]ScoreGate{},
		Diagnostics: ScoreDiagnostics{},
		EvalError:   &ScoreEvalError{Kind: kind, Code: code, Message: message},
	}
}

func infrastructureScore(err error) *ScoreResult {
	var coded *scoreCodedError
	if errors.As(err, &coded) {
		return errorScore(coded.kind, coded.code, coded.message)
	}
	return errorScore("infrastructure", "scorer_failure", "the scorer could not validate the artifact bundle")
}

type candidateReplicate struct {
	diagnostics ScoreReplicateDiagnostics
	stabilityOK bool
	resourcesOK bool
}

type baselineAggregate struct {
	ScoreAggregateDiagnostics
}

// Score evaluates a complete candidate bundle. It never returns a Go error:
// all failures are represented by the versioned, typed result document.
func Score(inputs ScoreInputs) *ScoreResult {
	baseline, err := readScoreBaseline(inputs.BaselineManifest)
	if err != nil {
		return infrastructureScore(err)
	}
	if err := validateInputCardinality(inputs, len(baseline.Replicates)); err != nil {
		return infrastructureScore(err)
	}

	replicates := make([]candidateReplicate, 0, len(inputs.Runs))
	evaluationIds := map[string]bool{}
	for i := range inputs.Runs {
		replicate, err := scoreReplicate(
			inputs.Runs[i],
			inputs.Stderr[i],
			inputs.Accounting[i],
			inputs.Samples[i],
			inputs.ResourceReports[i],
			inputs.Markers[i],
			baseline,
		)
		if err != nil {
			return infrastructureScore(err)
		}
		if evaluationIds[replicate.diagnostics.EvaluationId] {
			return infrastructureScore(scoreError("duplicate_replicate", "candidate bundle repeats an evaluation id"))
		}
		evaluationIds[replicate.diagnostics.EvaluationId] = true
		replicates = append(replicates, replicate)
	}

	baseAggregate := aggregateBaseline(baseline.Replicates)
	candidateAggregate := aggregateCandidate(replicates)
	significance := scoreSignificance(baseline.Replicates, replicates, baseline.TakeoverMargin)
	gates := buildScoreGates(baseAggregate, candidateAggregate, replicates)
	rawScore := candidateAggregate.RawScore
	normalized := clampScore(100*baseAggregate.RawScore/rawScore, 1, 200)
	takeoverRawMax := baseAggregate.RawScore * (1 - baseline.TakeoverMargin)

	diagnostics := ScoreDiagnostics{
		RoundId:                baseline.RoundId,
		ReplicateCount:         len(replicates),
		RequestTimeoutMs:       baseline.RequestTimeoutMs,
		TakeoverMargin:         baseline.TakeoverMargin,
		BaselineTakeoverRawMax: takeoverRawMax,
		Baseline:               &baseAggregate.ScoreAggregateDiagnostics,
		Candidate:              &candidateAggregate,
	}
	for _, replicate := range replicates {
		diagnostics.Replicates = append(diagnostics.Replicates, replicate.diagnostics)
	}

	result := &ScoreResult{
		ScoreSchema:     apexScoreSchema,
		RawScore:        rawScore,
		NormalizedScore: normalized,
		Gates:           gates,
		Significance:    significance,
		Diagnostics:     diagnostics,
	}
	result.Placeable = allScoreGatesPass(gates)
	if !gates["G5_stability"].Passed {
		result.Placeable = false
		result.EvalError = &ScoreEvalError{
			Kind:    "submission",
			Code:    "run_unstable",
			Message: "the candidate did not complete every replicate cleanly",
		}
	} else if !gates["G6_resources"].Passed {
		result.Placeable = false
		result.EvalError = &ScoreEvalError{
			Kind:    "submission",
			Code:    "resource_violation",
			Message: "the candidate violated or escaped an evaluation resource boundary",
		}
	}
	takeoverEligible := result.Placeable && rawScore <= takeoverRawMax &&
		significance.StatisticallySignificant &&
		significance.RecommendedNextEpochTakeoverMarginSupported
	result.Diagnostics.BaselineTakeoverEligible = &takeoverEligible
	return result
}

// BuildScoreBaseline validates an evaluator-owned same-round baseline bundle
// with the production scorer and returns the immutable manifest candidate
// scoring consumes. The first run supplies the frozen workload contract; every
// remaining run must match it exactly.
func BuildScoreBaseline(inputs ScoreBaselineInputs) (*ScoreBaselineManifest, error) {
	want := len(inputs.Runs)
	if want == 0 || want%2 == 0 {
		return nil, scoreError("invalid_baseline", "baseline replicate count must be odd and positive")
	}
	counts := []struct {
		name string
		n    int
	}{
		{"stderr", len(inputs.Stderr)},
		{"accounting snapshot", len(inputs.Accounting)},
		{"sample corpus", len(inputs.Samples)},
		{"resource report", len(inputs.ResourceReports)},
		{"completion marker", len(inputs.Markers)},
	}
	for _, count := range counts {
		if count.n != want {
			return nil, scoreError(
				"replicate_count_mismatch",
				"baseline %s count is %d; run count is %d",
				count.name,
				count.n,
				want,
			)
		}
	}
	if !scoreRoundIdPattern.MatchString(inputs.RoundId) {
		return nil, scoreError("invalid_baseline", "baseline round id is invalid")
	}
	if !finite(inputs.TakeoverMargin) || inputs.TakeoverMargin <= 0 || 0.5 < inputs.TakeoverMargin {
		return nil, scoreError("invalid_baseline", "baseline takeover margin is outside (0, 0.5]")
	}

	first, _, err := readOfficialRunSidecar(inputs.Runs[0])
	if err != nil {
		return nil, err
	}
	flags := make(map[string]string, len(first.Flags))
	for key, value := range first.Flags {
		flags[key] = value
	}
	contract := scoreBaselineFromReplicates(
		inputs.RoundId,
		first.ConfigSha256,
		first.RequestTimeoutMs,
		flags,
		inputs.TakeoverMargin,
		nil,
	)

	replicates := make([]ScoreBaselineReplicate, 0, want)
	evaluationIds := map[string]bool{}
	for i := range inputs.Runs {
		replicate, err := scoreReplicate(
			inputs.Runs[i],
			inputs.Stderr[i],
			inputs.Accounting[i],
			inputs.Samples[i],
			inputs.ResourceReports[i],
			inputs.Markers[i],
			contract,
		)
		if err != nil {
			return nil, err
		}
		diagnostic := replicate.diagnostics
		if evaluationIds[diagnostic.EvaluationId] {
			return nil, scoreError("duplicate_replicate", "baseline bundle repeats an evaluation id")
		}
		evaluationIds[diagnostic.EvaluationId] = true
		if diagnostic.SuccessRate < 0.97 {
			return nil, scoreError("baseline_gate_failed", "baseline replicate %d fails the 97%% success floor", i+1)
		}
		if diagnostic.AccountingCoverage < 0.95 {
			return nil, scoreError("baseline_gate_failed", "baseline replicate %d fails path-integrity accounting", i+1)
		}
		if diagnostic.FindProvidersSamples <= 0 || diagnostic.FindProvidersEmptyPools != 0 ||
			!finiteNonnegative(diagnostic.FindProvidersLoadP95Ms) ||
			!finitePositive(diagnostic.FindProvidersPoolP05) ||
			diagnostic.FindProvidersSampleSpanFraction < minimumFindProvidersSampleSpanFraction {
			return nil, scoreError("baseline_gate_failed", "baseline replicate %d fails matchmaking integrity", i+1)
		}
		if !replicate.stabilityOK {
			return nil, scoreError(
				"baseline_gate_failed",
				"baseline replicate %d is not stable (findings: %s)",
				i+1,
				strings.Join(diagnostic.StabilityFindings, ","),
			)
		}
		if !replicate.resourcesOK {
			return nil, scoreError("baseline_gate_failed", "baseline replicate %d violates the resource boundary", i+1)
		}
		if !finitePositive(diagnostic.RawScore) || diagnostic.RequestCount <= 0 || diagnostic.ReceivedBytes <= 0 {
			return nil, scoreError("baseline_gate_failed", "baseline replicate %d has an invalid measured workload", i+1)
		}
		replicates = append(replicates, ScoreBaselineReplicate{
			RawScore:                        diagnostic.RawScore,
			SuccessRate:                     diagnostic.SuccessRate,
			RequestCount:                    diagnostic.RequestCount,
			ReceivedBytes:                   diagnostic.ReceivedBytes,
			FindProvidersLoadP95Ms:          diagnostic.FindProvidersLoadP95Ms,
			FindProvidersPoolP05:            diagnostic.FindProvidersPoolP05,
			FindProvidersSampleSpanFraction: diagnostic.FindProvidersSampleSpanFraction,
		})
	}
	contract.Replicates = replicates
	if err := validateScoreBaseline(contract); err != nil {
		return nil, err
	}
	return contract, nil
}

func validateInputCardinality(inputs ScoreInputs, want int) error {
	if want <= 0 {
		return scoreError("invalid_baseline", "baseline has no replicates")
	}
	counts := []struct {
		name string
		n    int
	}{
		{"run CSV", len(inputs.Runs)},
		{"stderr", len(inputs.Stderr)},
		{"accounting snapshot", len(inputs.Accounting)},
		{"sample corpus", len(inputs.Samples)},
		{"resource report", len(inputs.ResourceReports)},
		{"completion marker", len(inputs.Markers)},
	}
	for _, count := range counts {
		if count.n != want {
			return scoreError(
				"replicate_count_mismatch",
				"candidate %s count is %d; signed baseline requires %d",
				count.name,
				count.n,
				want,
			)
		}
	}
	return nil
}

func readScoreBaseline(path string) (*ScoreBaselineManifest, error) {
	var baseline ScoreBaselineManifest
	if err := decodeStrictJSON(path, &baseline, "baseline manifest"); err != nil {
		return nil, err
	}
	if err := validateScoreBaseline(&baseline); err != nil {
		return nil, err
	}
	return &baseline, nil
}

func validateScoreBaseline(baseline *ScoreBaselineManifest) error {
	if baseline.ScoreSchema != apexScoreSchema || baseline.Kind != scoreBaselineKind {
		return scoreError("invalid_baseline", "baseline manifest has the wrong kind or score schema")
	}
	if baseline.ScorerVersion != officialScorerVersion {
		return scoreError("scorer_version_mismatch", "baseline manifest requires a different scorer version")
	}
	if !scoreRoundIdPattern.MatchString(baseline.RoundId) {
		return scoreError("invalid_baseline", "baseline manifest has an invalid round id")
	}
	if !validSha256(baseline.ConfigSha256) {
		return scoreError("invalid_baseline", "baseline manifest has an invalid providers SHA-256")
	}
	if baseline.RequestTimeoutMs <= 0 {
		return scoreError("invalid_baseline", "baseline request timeout must be positive")
	}
	if len(baseline.RunFlags) == 0 {
		return scoreError("invalid_baseline", "baseline manifest has no frozen run flags")
	}
	if !finite(baseline.TakeoverMargin) || baseline.TakeoverMargin <= 0 || 0.5 < baseline.TakeoverMargin {
		return scoreError("invalid_baseline", "baseline takeover margin is outside (0, 0.5]")
	}
	if len(baseline.Replicates) == 0 || len(baseline.Replicates)%2 == 0 {
		return scoreError("invalid_baseline", "baseline replicate count must be odd and positive")
	}
	for _, replicate := range baseline.Replicates {
		if !finitePositive(replicate.RawScore) ||
			float64(baseline.RequestTimeoutMs) < replicate.RawScore ||
			!finite(replicate.SuccessRate) || replicate.SuccessRate < 0.97 || 1 < replicate.SuccessRate ||
			replicate.RequestCount <= 0 || replicate.ReceivedBytes <= 0 ||
			!finiteNonnegative(replicate.FindProvidersLoadP95Ms) ||
			!finitePositive(replicate.FindProvidersPoolP05) ||
			!finite(replicate.FindProvidersSampleSpanFraction) ||
			replicate.FindProvidersSampleSpanFraction < minimumFindProvidersSampleSpanFraction ||
			1 < replicate.FindProvidersSampleSpanFraction {
			return scoreError("invalid_baseline", "baseline replicate contains an invalid or non-finite diagnostic")
		}
	}
	return nil
}

func aggregateBaseline(replicates []ScoreBaselineReplicate) baselineAggregate {
	rawScores := make([]float64, 0, len(replicates))
	successRates := make([]float64, 0, len(replicates))
	requestCounts := make([]float64, 0, len(replicates))
	receivedBytes := make([]float64, 0, len(replicates))
	loadP95s := make([]float64, 0, len(replicates))
	poolP05s := make([]float64, 0, len(replicates))
	sampleSpanFraction := math.Inf(1)
	for _, replicate := range replicates {
		rawScores = append(rawScores, replicate.RawScore)
		successRates = append(successRates, replicate.SuccessRate)
		requestCounts = append(requestCounts, float64(replicate.RequestCount))
		receivedBytes = append(receivedBytes, float64(replicate.ReceivedBytes))
		loadP95s = append(loadP95s, replicate.FindProvidersLoadP95Ms)
		poolP05s = append(poolP05s, replicate.FindProvidersPoolP05)
		sampleSpanFraction = math.Min(sampleSpanFraction, replicate.FindProvidersSampleSpanFraction)
	}
	return baselineAggregate{ScoreAggregateDiagnostics: ScoreAggregateDiagnostics{
		RawScore:                        scoreMedian(rawScores),
		SuccessRate:                     scoreMedian(successRates),
		RequestCount:                    scoreMedian(requestCounts),
		ReceivedBytes:                   scoreMedian(receivedBytes),
		FindProvidersLoadP95Ms:          scoreMedian(loadP95s),
		FindProvidersPoolP05:            scoreMedian(poolP05s),
		FindProvidersSampleSpanFraction: sampleSpanFraction,
	}}
}

func aggregateCandidate(replicates []candidateReplicate) ScoreAggregateDiagnostics {
	rawScores := make([]float64, 0, len(replicates))
	successRates := make([]float64, 0, len(replicates))
	requestCounts := make([]float64, 0, len(replicates))
	receivedBytes := make([]float64, 0, len(replicates))
	loadP95s := make([]float64, 0, len(replicates))
	poolP05s := make([]float64, 0, len(replicates))
	coverage := math.Inf(1)
	sampleSpanFraction := math.Inf(1)
	for _, replicate := range replicates {
		diagnostic := replicate.diagnostics
		rawScores = append(rawScores, diagnostic.RawScore)
		successRates = append(successRates, diagnostic.SuccessRate)
		requestCounts = append(requestCounts, float64(diagnostic.RequestCount))
		receivedBytes = append(receivedBytes, float64(diagnostic.ReceivedBytes))
		loadP95s = append(loadP95s, diagnostic.FindProvidersLoadP95Ms)
		poolP05s = append(poolP05s, diagnostic.FindProvidersPoolP05)
		coverage = math.Min(coverage, diagnostic.AccountingCoverage)
		sampleSpanFraction = math.Min(sampleSpanFraction, diagnostic.FindProvidersSampleSpanFraction)
	}
	if math.IsInf(coverage, 1) {
		coverage = 0
	}
	if math.IsInf(sampleSpanFraction, 1) {
		sampleSpanFraction = 0
	}
	return ScoreAggregateDiagnostics{
		RawScore:                        scoreMedian(rawScores),
		SuccessRate:                     scoreMedian(successRates),
		RequestCount:                    scoreMedian(requestCounts),
		ReceivedBytes:                   scoreMedian(receivedBytes),
		AccountingCoverage:              coverage,
		FindProvidersLoadP95Ms:          scoreMedian(loadP95s),
		FindProvidersPoolP05:            scoreMedian(poolP05s),
		FindProvidersSampleSpanFraction: sampleSpanFraction,
	}
}

func scoreMedian(values []float64) float64 {
	copyValues := append([]float64(nil), values...)
	return quantile(copyValues, 0.50)
}

func buildScoreGates(
	baseline baselineAggregate,
	candidate ScoreAggregateDiagnostics,
	replicates []candidateReplicate,
) map[string]ScoreGate {
	g1AbsoluteMin := 0.97
	g1RelativeMin := baseline.SuccessRate - 0.01
	g1 := candidate.SuccessRate >= g1AbsoluteMin && candidate.SuccessRate >= g1RelativeMin

	requestMin := 0.95 * baseline.RequestCount
	requestMax := 1.05 * baseline.RequestCount
	bytesMin := 0.95 * baseline.ReceivedBytes
	bytesMax := 1.05 * baseline.ReceivedBytes
	g2 := requestMin <= candidate.RequestCount && candidate.RequestCount <= requestMax &&
		bytesMin <= candidate.ReceivedBytes && candidate.ReceivedBytes <= bytesMax

	g3 := 0.95 <= candidate.AccountingCoverage

	allSamplesPresent := true
	allPoolsNonempty := true
	allSampleSpansComplete := true
	allStable := true
	allResources := true
	for _, replicate := range replicates {
		if replicate.diagnostics.FindProvidersSamples == 0 {
			allSamplesPresent = false
		}
		if 0 < replicate.diagnostics.FindProvidersEmptyPools {
			allPoolsNonempty = false
		}
		if replicate.diagnostics.FindProvidersSampleSpanFraction < minimumFindProvidersSampleSpanFraction {
			allSampleSpansComplete = false
		}
		allStable = allStable && replicate.stabilityOK
		allResources = allResources && replicate.resourcesOK
	}
	loadMax := 1.25 * baseline.FindProvidersLoadP95Ms
	poolMin := 0.90 * baseline.FindProvidersPoolP05
	g4 := allSamplesPresent && allPoolsNonempty && allSampleSpansComplete &&
		candidate.FindProvidersLoadP95Ms <= loadMax && poolMin <= candidate.FindProvidersPoolP05

	return map[string]ScoreGate{
		"G1_success": {
			Passed: g1,
			Details: map[string]any{
				"candidate":              candidate.SuccessRate,
				"baseline":               baseline.SuccessRate,
				"absolute_min_inclusive": g1AbsoluteMin,
				"baseline_minus_max":     g1RelativeMin,
			},
		},
		"G2_volume": {
			Passed: g2,
			Details: map[string]any{
				"candidate_requests": candidate.RequestCount,
				"request_min":        requestMin,
				"request_max":        requestMax,
				"candidate_bytes":    candidate.ReceivedBytes,
				"bytes_min":          bytesMin,
				"bytes_max":          bytesMax,
			},
		},
		"G3_path_integrity": {
			Passed: g3,
			Details: map[string]any{
				"minimum_replicate_coverage": candidate.AccountingCoverage,
				"minimum_inclusive":          0.95,
			},
		},
		"G4_matchmaking": {
			Passed: g4,
			Details: map[string]any{
				"all_replicates_have_samples":                     allSamplesPresent,
				"all_pools_nonempty":                              allPoolsNonempty,
				"all_replicates_span_at_least_minimum":            allSampleSpansComplete,
				"minimum_replicate_sample_span_fraction":          candidate.FindProvidersSampleSpanFraction,
				"baseline_minimum_replicate_sample_span_fraction": baseline.FindProvidersSampleSpanFraction,
				"sample_span_minimum_inclusive":                   minimumFindProvidersSampleSpanFraction,
				"candidate_load_p95_ms":                           candidate.FindProvidersLoadP95Ms,
				"baseline_load_p95_ms":                            baseline.FindProvidersLoadP95Ms,
				"maximum_inclusive_ms":                            loadMax,
				"candidate_pool_p05":                              candidate.FindProvidersPoolP05,
				"baseline_pool_p05":                               baseline.FindProvidersPoolP05,
				"pool_minimum_inclusive":                          poolMin,
			},
		},
		"G5_stability": {
			Passed: allStable,
			Details: map[string]any{
				"all_replicates_clean": allStable,
			},
		},
		"G6_resources": {
			Passed: allResources,
			Details: map[string]any{
				"all_replicates_within_limits": allResources,
			},
		},
	}
}

func allScoreGatesPass(gates map[string]ScoreGate) bool {
	for _, name := range []string{
		"G1_success",
		"G2_volume",
		"G3_path_integrity",
		"G4_matchmaking",
		"G5_stability",
		"G6_resources",
	} {
		gate, ok := gates[name]
		if !ok || !gate.Passed {
			return false
		}
	}
	return true
}

func clampScore(value float64, minValue float64, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if maxValue < value {
		return maxValue
	}
	return value
}

func scoreReplicate(
	runPath string,
	stderrPath string,
	accountingPath string,
	samplesPath string,
	resourcePath string,
	markerPath string,
	baseline *ScoreBaselineManifest,
) (candidateReplicate, error) {
	var out candidateReplicate
	// Read the trusted cgroup/process report first. It lets a crash/OOM remain a
	// typed submission fault even when the process died before it could write a
	// sidecar or marker; an otherwise clean missing sidecar is infrastructure.
	var resource ResourceReport
	if err := decodeStrictJSON(resourcePath, &resource, "resource report"); err != nil {
		return out, err
	}
	if resource.Schema != 1 || resource.Kind != resourceReportKind ||
		resource.EvaluationId == "" || resource.CgroupId == "" ||
		!finiteNonnegative(resource.CpuSeconds) || resource.PeakRssBytes < 0 {
		return out, scoreError("invalid_resource_report", "resource report is structurally invalid")
	}
	preRunResourceFindings := resourceReportFindings(resource)

	runStats, manifestBytes, err := readOfficialRunSidecar(runPath)
	if err != nil {
		if 0 < len(preRunResourceFindings) {
			return out, submissionScoreError("run_process_failed", "candidate process failed before producing a complete artifact set")
		}
		return out, err
	}
	if err := validateRunContract(runStats, baseline); err != nil {
		return out, err
	}

	// A runner-recorded incomplete state is a candidate stability fault, not a
	// placeable partial score. Missing/malformed artifacts on an allegedly
	// complete run remain infrastructure errors below.
	if runStats.CompletionState != "complete" {
		return out, submissionScoreError("incomplete_run", "candidate run did not reach a complete lifecycle state")
	}
	if !sameArtifactPath(markerPath, runStats.FinalMarker) ||
		!sameArtifactPath(accountingPath, runStats.AccountingReport) ||
		!sameArtifactPath(resourcePath, runStats.ResourceReport) {
		return out, scoreError("artifact_path_mismatch", "scorer input path does not match the completed run manifest")
	}
	if err := validateFinalMarker(markerPath, manifestBytes, runStats); err != nil {
		return out, err
	}

	stderrBytes, err := readBoundedFile(stderrPath, "stderr")
	if err != nil {
		return out, err
	}
	if err := validateStderrEvaluationId(stderrBytes, runStats.EvaluationId); err != nil {
		return out, err
	}
	stabilityFindings := scanStabilityFindings(stderrBytes)
	if !runStats.Official {
		stabilityFindings = append(stabilityFindings, "non_official_run")
	}
	if runStats.BuildModified || runStats.BuildRevision == "" {
		stabilityFindings = append(stabilityFindings, "unfrozen_build")
	}
	if runStats.ClientsPool <= 0 || runStats.ClientsEstablished != runStats.ClientsPool {
		stabilityFindings = append(stabilityFindings, "warm_pool_incomplete")
	}

	rows, csvSha256, csvBytes, err := readScoreCSV(runPath)
	if err != nil {
		return out, err
	}
	if csvSha256 != runStats.ResultsCsvSha256 || csvBytes != runStats.ResultsCsvBytes {
		return out, scoreError("results_identity_mismatch", "results CSV does not match the completed run manifest")
	}
	rowMetrics, err := scoreRows(rows, runStats)
	if err != nil {
		return out, err
	}
	if runStats.Rows != len(rows) ||
		runStats.RowsInWindow != int(rowMetrics.RequestCount) ||
		runStats.Failures != rowMetrics.Failures {
		return out, scoreError("manifest_csv_mismatch", "run manifest totals do not match the results CSV")
	}

	var accounting AccountingSnapshot
	if err := decodeStrictJSON(accountingPath, &accounting, "accounting snapshot"); err != nil {
		return out, err
	}
	if accounting.Schema != 1 || accounting.Kind != accountingSnapshotKind ||
		!accounting.Complete || accounting.ProviderEgressBytes < 0 {
		return out, scoreError("invalid_accounting", "accounting snapshot is incomplete or invalid")
	}
	if accounting.EvaluationId != runStats.EvaluationId {
		return out, scoreError("evaluation_id_mismatch", "accounting snapshot evaluation id does not match the run")
	}
	if accounting.MeasureStartMs != runStats.MeasureStartMs || accounting.MeasureEndMs != runStats.MeasureEndMs {
		return out, scoreError("accounting_window_mismatch", "accounting snapshot does not cover the run's measured window")
	}
	coverage := 0.0
	if 0 < rowMetrics.ReceivedBytes {
		coverage = float64(accounting.ProviderEgressBytes) / float64(rowMetrics.ReceivedBytes)
	}
	if !finiteNonnegative(coverage) {
		return out, scoreError("invalid_accounting", "accounting coverage is non-finite")
	}

	if !sameArtifactPath(samplesPath, runStats.StatsRoot) {
		return out, scoreError("sample_identity_mismatch", "FindProviders2 sample corpus does not match the run manifest stats root")
	}
	sampleMetrics, err := readScoreSamples(
		samplesPath,
		runStats.MeasureStartMs,
		runStats.MeasureEndMs,
	)
	if err != nil {
		return out, err
	}

	if resource.EvaluationId != runStats.EvaluationId {
		return out, scoreError("evaluation_id_mismatch", "resource report evaluation id does not match the run")
	}
	if resource.CgroupId == "" || runStats.MeasureStartMs < resource.MeasurementStartMs ||
		resource.MeasurementEndMs < runStats.CompletedUnixMs {
		return out, scoreError("resource_window_mismatch", "resource report does not cover the complete run lifecycle")
	}
	resourceFindings := preRunResourceFindings

	out.diagnostics = ScoreReplicateDiagnostics{
		EvaluationId:                    runStats.EvaluationId,
		RawScore:                        rowMetrics.RawScore,
		SuccessRate:                     rowMetrics.SuccessRate,
		RequestCount:                    rowMetrics.RequestCount,
		ReceivedBytes:                   rowMetrics.ReceivedBytes,
		ProviderEgressBytes:             accounting.ProviderEgressBytes,
		AccountingCoverage:              coverage,
		FindProvidersSamples:            sampleMetrics.Count,
		FindProvidersEmptyPools:         sampleMetrics.EmptyPools,
		FindProvidersLoadP95Ms:          sampleMetrics.LoadP95Ms,
		FindProvidersPoolP05:            sampleMetrics.PoolP05,
		FindProvidersSampleSpanFraction: sampleMetrics.SpanFraction,
		StabilityFindings:               stabilityFindings,
		ResourceFindings:                resourceFindings,
	}
	out.stabilityOK = len(stabilityFindings) == 0
	out.resourcesOK = len(resourceFindings) == 0
	return out, nil
}

// sameArtifactPath accepts the exact recorded spelling or two paths that the
// filesystem resolves to the same object. Official manifests use absolute
// paths, while the SameFile fallback keeps local re-scoring robust to a
// symlinked artifact root without allowing a different corpus to be supplied.
func sameArtifactPath(a string, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aAbs, aErr := filepath.Abs(a)
	bAbs, bErr := filepath.Abs(b)
	if aErr == nil && bErr == nil && filepath.Clean(aAbs) == filepath.Clean(bAbs) {
		return true
	}
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(aInfo, bInfo)
}

func validateRunContract(runStats *RunStats, baseline *ScoreBaselineManifest) error {
	if runStats.Schema != runStatsSchema || runStats.Kind != runStatsKind {
		return scoreError("artifact_schema_mismatch", "official scoring requires a schema-2 run manifest")
	}
	if runStats.ScoreSchema != apexScoreSchema || runStats.ScorerVersion != officialScorerVersion {
		return scoreError("scorer_version_mismatch", "run manifest uses a different score schema or scorer version")
	}
	if runStats.EvaluationId == "" {
		return scoreError("missing_evaluation_id", "run manifest has no evaluation id")
	}
	if runStats.StatsRoot == "" || runStats.StatsInstanceId == "" ||
		!validSha256(runStats.ResultsCsvSha256) || runStats.ResultsCsvBytes <= 0 ||
		runStats.AccountingReport == "" || runStats.ResourceReport == "" || runStats.FinalMarker == "" {
		return scoreError("incomplete_run_manifest", "run manifest omits a mandatory scorer input path or stats identity")
	}
	if runStats.ConfigSha256 != baseline.ConfigSha256 {
		return scoreError("workload_mismatch", "candidate providers SHA-256 does not match the signed baseline")
	}
	if runStats.RequestTimeoutMs != baseline.RequestTimeoutMs {
		return scoreError("timeout_mismatch", "candidate request timeout does not match the signed baseline")
	}
	if !reflect.DeepEqual(runStats.Flags, baseline.RunFlags) {
		return scoreError("run_flags_mismatch", "candidate run flags do not match the signed baseline")
	}
	if runStats.MeasureEndMs <= runStats.MeasureStartMs {
		return scoreError("invalid_measure_window", "run manifest has an empty measured window")
	}
	return nil
}

func readOfficialRunSidecar(runPath string) (*RunStats, []byte, error) {
	for _, candidate := range sideCarCandidates(runPath) {
		manifestBytes, err := readBoundedFile(candidate, "run manifest")
		if err != nil {
			var coded *scoreCodedError
			if errors.As(err, &coded) && coded.code == "missing_run_manifest" {
				continue
			}
			return nil, nil, err
		}
		var runStats RunStats
		if err := decodeStrictJSONBytes(manifestBytes, &runStats, "run manifest"); err != nil {
			return nil, nil, err
		}
		return &runStats, manifestBytes, nil
	}
	return nil, nil, scoreError("missing_run_manifest", "required run manifest is unavailable")
}

func validateFinalMarker(path string, manifestBytes []byte, runStats *RunStats) error {
	var marker runFinalMarker
	if err := decodeStrictJSON(path, &marker, "completion marker"); err != nil {
		return err
	}
	manifestSha := fmt.Sprintf("%x", sha256.Sum256(manifestBytes))
	if marker.Schema != 1 || marker.Kind != "sim-latency-complete" ||
		marker.ScoreSchema != apexScoreSchema || marker.ScorerVersion != officialScorerVersion ||
		marker.EvaluationId != runStats.EvaluationId ||
		marker.RunManifestSha256 != manifestSha ||
		marker.RunManifestBytes != int64(len(manifestBytes)) ||
		marker.CompletedUnixMs != runStats.CompletedUnixMs {
		return scoreError("invalid_final_marker", "completion marker does not authenticate the finalized run manifest")
	}
	return nil
}

type scoreRowMetrics struct {
	RawScore      float64
	SuccessRate   float64
	RequestCount  int64
	ReceivedBytes int64
	Failures      int
}

func scoreRows(rows []resultRow, runStats *RunStats) (scoreRowMetrics, error) {
	var metrics scoreRowMetrics
	observations := []float64{}
	successes := int64(0)
	for _, row := range rows {
		if row.tStartMs < runStats.MeasureStartMs || runStats.MeasureEndMs <= row.tStartMs {
			continue
		}
		metrics.RequestCount++
		if row.bytes < 0 || math.MaxInt64-metrics.ReceivedBytes < row.bytes {
			return metrics, scoreError("invalid_results_csv", "results CSV has invalid received-byte accounting")
		}
		metrics.ReceivedBytes += row.bytes
		if row.success() {
			if !finitePositive(row.totalMs) || float64(runStats.RequestTimeoutMs) < row.totalMs {
				return metrics, scoreError("invalid_results_csv", "successful row has invalid total_ms")
			}
			observations = append(observations, row.totalMs)
			successes++
		} else {
			observations = append(observations, float64(runStats.RequestTimeoutMs))
			metrics.Failures++
		}
	}
	if metrics.RequestCount == 0 {
		return metrics, scoreError("empty_measured_window", "results CSV has no rows in the measured window")
	}
	metrics.SuccessRate = float64(successes) / float64(metrics.RequestCount)
	metrics.RawScore = quantile(observations, 0.95)
	if !finitePositive(metrics.RawScore) || !finite(metrics.SuccessRate) {
		return metrics, scoreError("invalid_results_csv", "results CSV produced a non-finite score diagnostic")
	}
	return metrics, nil
}

func readScoreCSV(path string) ([]resultRow, string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", 0, scoreError("missing_results_csv", "required results CSV is unavailable")
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	digest := sha256.New()
	var digestBytes int64
	addDigestLine := func(line string) {
		_, _ = digest.Write([]byte(line))
		_, _ = digest.Write([]byte{'\n'})
		digestBytes += int64(len(line) + 1)
	}
	headerFound := false
	rows := []resultRow{}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !headerFound {
			if line == resultCSVHeader {
				addDigestLine(line)
				headerFound = true
				continue
			}
			if isLegacyLogLine(line) {
				continue
			}
			return nil, "", 0, scoreError(
				"invalid_results_csv",
				"results CSV contains an unrecognized prefix or non-canonical header",
			)
		}

		if isLegacyLogLine(line) {
			continue
		}
		record, parseErr := parseCSVLine(line)
		if parseErr != nil || len(record) != 9 {
			return nil, "", 0, scoreError("invalid_results_csv", "results CSV contains a malformed record")
		}
		for index := range record {
			record[index] = strings.TrimSpace(record[index])
		}
		tStartMs, timestampErr := strconv.ParseInt(record[0], 10, 64)
		depth, depthErr := strconv.ParseInt(record[3], 10, 64)
		status, statusErr := strconv.Atoi(record[4])
		received, bytesErr := strconv.ParseInt(record[5], 10, 64)
		ttfb, ttfbErr := strconv.ParseFloat(record[6], 64)
		total, totalErr := strconv.ParseFloat(record[7], 64)
		bytesPerSecond, throughputErr := strconv.ParseFloat(record[8], 64)
		if timestampErr != nil || depthErr != nil || statusErr != nil || bytesErr != nil ||
			ttfbErr != nil || totalErr != nil || throughputErr != nil ||
			record[1] == "" || record[2] == "" || tStartMs < 0 || depth < 0 ||
			status < 0 || 599 < status || received < 0 || !finiteNonnegative(ttfb) ||
			!finiteNonnegative(total) || !finiteNonnegative(bytesPerSecond) {
			return nil, "", 0, scoreError("invalid_results_csv", "results CSV contains an invalid or non-finite value")
		}
		addDigestLine(line)
		rows = append(rows, resultRow{
			tStartMs: tStartMs,
			status:   status,
			bytes:    received,
			ttfbMs:   ttfb,
			totalMs:  total,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, "", 0, scoreError("invalid_results_csv", "results CSV could not be read completely")
	}
	if !headerFound {
		return nil, "", 0, scoreError("invalid_results_csv", "results CSV has no recognized header")
	}
	if len(rows) == 0 {
		return nil, "", 0, scoreError("empty_results_csv", "results CSV has no measurement records")
	}
	return rows, fmt.Sprintf("%x", digest.Sum(nil)), digestBytes, nil
}

func parseCSVLine(line string) ([]string, error) {
	reader := csv.NewReader(strings.NewReader(line))
	reader.FieldsPerRecord = -1
	record, err := reader.Read()
	if err != nil {
		return nil, err
	}
	if _, err := reader.Read(); err != io.EOF {
		return nil, errors.New("more than one CSV record on a line")
	}
	return record, nil
}

func isLegacyLogLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed == "[legacy sdk]" ||
		strings.HasPrefix(trimmed, "[legacy sdk] ") ||
		legacyGlogPattern.MatchString(trimmed)
}

type scoreSampleMetrics struct {
	Count         int64
	EmptyPools    int64
	FirstSampleMs int64
	LastSampleMs  int64
	SpanFraction  float64
	LoadP95Ms     float64
	PoolP05       float64
}

func readScoreSamples(path string, startMs int64, endMs int64) (scoreSampleMetrics, error) {
	var metrics scoreSampleMetrics
	info, err := os.Stat(path)
	if err != nil {
		return metrics, scoreError("missing_samples", "required FindProviders2 sample corpus is unavailable")
	}
	loads := []float64{}
	pools := []float64{}
	consume := func(value *sample.FindProviders2Sample) error {
		if value == nil {
			return scoreError("malformed_samples", "FindProviders2 sample corpus contains a nil sample")
		}
		if uint64(math.MaxInt64) < value.TimeUnixMilli {
			return scoreError("malformed_samples", "FindProviders2 sample timestamp is out of range")
		}
		timestamp := int64(value.TimeUnixMilli)
		if timestamp < startMs || endMs <= timestamp {
			return nil
		}
		loadMillis := float64(value.LoadMillis)
		if !finiteNonnegative(loadMillis) {
			return scoreError("malformed_samples", "FindProviders2 sample contains invalid load_millis")
		}
		if metrics.Count == 0 {
			metrics.FirstSampleMs = timestamp
			metrics.LastSampleMs = timestamp
		} else {
			metrics.FirstSampleMs = min(metrics.FirstSampleMs, timestamp)
			metrics.LastSampleMs = max(metrics.LastSampleMs, timestamp)
		}
		metrics.Count++
		if value.PoolCount <= 0 || len(value.Candidates) == 0 {
			metrics.EmptyPools++
		} else {
			pools = append(pools, float64(value.PoolCount))
			for _, candidate := range value.Candidates {
				if candidate == nil {
					return scoreError("malformed_samples", "FindProviders2 sample contains a nil candidate")
				}
			}
		}
		loads = append(loads, loadMillis)
		return nil
	}

	if info.IsDir() {
		streamDir := filepath.Join(path, "findproviders2")
		streamInfo, statErr := os.Stat(streamDir)
		if statErr != nil || !streamInfo.IsDir() {
			return metrics, scoreError("missing_samples", "stats root has no FindProviders2 stream")
		}
		err = statspkg.LoadStreamTyped(
			streamDir,
			func() *sample.FindProviders2Sample { return &sample.FindProviders2Sample{} },
			consume,
		)
	} else if strings.HasSuffix(strings.ToLower(path), ".xz") {
		err = statspkg.ReadFlat(path, consume)
	} else {
		err = statspkg.ReadSegment(path, func(frame []byte) error {
			var value sample.FindProviders2Sample
			if err := proto.Unmarshal(frame, &value); err != nil {
				return err
			}
			return consume(&value)
		})
	}
	if err != nil {
		var coded *scoreCodedError
		if errors.As(err, &coded) {
			return metrics, err
		}
		return metrics, scoreError("malformed_samples", "FindProviders2 sample corpus is malformed")
	}
	if 0 < len(loads) {
		metrics.LoadP95Ms = quantile(loads, 0.95)
	}
	if 0 < len(pools) {
		metrics.PoolP05 = quantile(pools, 0.05)
	}
	if 0 < metrics.Count && startMs < endMs {
		metrics.SpanFraction = float64(metrics.LastSampleMs-metrics.FirstSampleMs) / float64(endMs-startMs)
	}
	return metrics, nil
}

var evaluationLogIdPattern = regexp.MustCompile(`\[sim-latency eval=([^\]]+)\]`)
var legacyGlogPattern = regexp.MustCompile(`^[IWEF][0-9]{4}\s`)
var scoreRoundIdPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func validateStderrEvaluationId(stderr []byte, want string) error {
	matches := evaluationLogIdPattern.FindAllSubmatch(stderr, -1)
	if len(matches) == 0 {
		return scoreError("missing_evaluation_id", "stderr contains no evaluation-id-tagged simulator log")
	}
	for _, match := range matches {
		if len(match) != 2 || string(match[1]) != want {
			return scoreError("evaluation_id_mismatch", "stderr mixes logs from a different evaluation id")
		}
	}
	return nil
}

func scanStabilityFindings(stderr []byte) []string {
	text := string(stderr)
	patterns := []struct {
		code string
		find func(string) bool
	}{
		{"unexpected_recovery", func(value string) bool { return strings.Contains(value, "Unexpected error:") }},
		{"rescue_handler_panic", func(value string) bool { return strings.Contains(value, "Rescue handler panic") }},
		{"client_driver_panic", func(value string) bool { return strings.Contains(value, "client driver panic:") }},
		{"evaluation_panic", func(value string) bool { return strings.Contains(value, "evaluation panic:") }},
		{"http_handler_panic", func(value string) bool {
			lower := strings.ToLower(value)
			return strings.Contains(lower, "http: panic serving") || strings.Contains(lower, "panic recovered")
		}},
		{"runtime_panic", func(value string) bool {
			for _, line := range strings.Split(value, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "panic:") {
					return true
				}
			}
			return false
		}},
		{"fatal_runtime_error", func(value string) bool { return strings.Contains(value, "fatal error:") }},
		{"out_of_memory", func(value string) bool {
			lower := strings.ToLower(value)
			return strings.Contains(lower, "runtime: out of memory") || strings.Contains(lower, "out of memory: killed process")
		}},
		{"service_restart", func(value string) bool {
			lower := strings.ToLower(value)
			return strings.Contains(lower, "service restart") || strings.Contains(lower, "restarting service")
		}},
		{"missing_service", func(value string) bool {
			lower := strings.ToLower(value)
			return strings.Contains(lower, "service unavailable") || strings.Contains(lower, "service missing")
		}},
		{"unclean_drain", func(value string) bool {
			lower := strings.ToLower(value)
			return strings.Contains(lower, "unclean drain") || strings.Contains(lower, "did not drain within")
		}},
	}
	findings := []string{}
	for _, pattern := range patterns {
		if pattern.find(text) {
			findings = append(findings, pattern.code)
		}
	}
	return findings
}

func resourceReportFindings(report ResourceReport) []string {
	findings := []string{}
	if !report.Complete || report.MeasurementMissing {
		findings = append(findings, "measurement_incomplete")
	}
	if report.ExitCode != 0 {
		findings = append(findings, "nonzero_exit")
	}
	if report.OomKilled {
		findings = append(findings, "oom_killed")
	}
	if report.HardKilled {
		findings = append(findings, "hard_killed")
	}
	if report.LimitEscape {
		findings = append(findings, "limit_escape")
	}
	return findings
}

func readBoundedFile(path string, label string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		code := "missing_artifact"
		switch label {
		case "run manifest":
			code = "missing_run_manifest"
		case "stderr":
			code = "missing_stderr"
		case "completion marker":
			code = "missing_final_marker"
		case "baseline manifest":
			code = "missing_baseline"
		case "accounting snapshot":
			code = "missing_accounting"
		case "resource report":
			code = "missing_resource_report"
		}
		return nil, scoreError(code, "required %s is unavailable", label)
	}
	if info.Size() < 0 || scoreResultMaxInputBytes < info.Size() {
		return nil, scoreError("artifact_too_large", "%s exceeds the scorer input limit", label)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, scoreError("artifact_read_failed", "%s could not be read completely", label)
	}
	return content, nil
}

func decodeStrictJSON(path string, target any, label string) error {
	content, err := readBoundedFile(path, label)
	if err != nil {
		return err
	}
	return decodeStrictJSONBytes(content, target, label)
}

func decodeStrictJSONBytes(content []byte, target any, label string) error {
	validator := json.NewDecoder(bytes.NewReader(content))
	validator.UseNumber()
	if err := validateUniqueJSONValue(validator); err != nil {
		return scoreError("malformed_artifact", "%s contains malformed JSON", label)
	}
	if _, err := validator.Token(); err != io.EOF {
		return scoreError("malformed_artifact", "%s contains trailing JSON data", label)
	}

	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return scoreError("malformed_artifact", "%s contains malformed JSON", label)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return scoreError("malformed_artifact", "%s contains trailing JSON data", label)
	}
	return nil
}

// validateUniqueJSONValue consumes one JSON value while rejecting duplicate
// object names at every nesting depth. encoding/json otherwise accepts a
// duplicate and silently lets the later value replace the earlier one, which
// makes a signed artifact ambiguous to non-Go verifiers.
func validateUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("JSON object contains a duplicate key")
			}
			seen[key] = struct{}{}
			if err := validateUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("JSON object is not closed")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := validateUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("JSON array is not closed")
		}
		return nil
	default:
		return errors.New("unexpected closing JSON delimiter")
	}
}

func validSha256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func finitePositive(value float64) bool {
	return finite(value) && 0 < value
}

func finiteNonnegative(value float64) bool {
	return finite(value) && 0 <= value
}

func marshalScoreResult(result *ScoreResult) ([]byte, error) {
	content, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

func marshalScoreBaseline(baseline *ScoreBaselineManifest) ([]byte, error) {
	content, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

func runScoreBaseline(opts docopt.Opts) {
	inputs := ScoreBaselineInputs{
		Runs:            splitPaths(optString(opts, "--run", "")),
		Stderr:          splitPaths(optString(opts, "--stderr", "")),
		Accounting:      splitPaths(optString(opts, "--accounting", "")),
		Samples:         splitPaths(optString(opts, "--samples", "")),
		ResourceReports: splitPaths(optString(opts, "--resource-report", "")),
		Markers:         splitPaths(optString(opts, "--marker", "")),
		RoundId:         optString(opts, "--round-id", ""),
		TakeoverMargin:  optFloat(opts, "--takeover-margin", math.NaN()),
	}
	baseline, err := BuildScoreBaseline(inputs)
	if err != nil {
		fatalf("score baseline: %s", err)
	}
	content, err := marshalScoreBaseline(baseline)
	if err != nil {
		fatalf("score baseline: encode manifest: %s", err)
	}
	if outPath := optString(opts, "--out", ""); outPath != "" {
		if err := writeAtomicFile(outPath, content, 0o644); err != nil {
			fatalf("score baseline: write manifest: %s", err)
		}
	}
	_, _ = os.Stdout.Write(content)
}

func runScore(opts docopt.Opts) {
	inputs := ScoreInputs{
		Runs:             splitPaths(optString(opts, "--run", "")),
		Stderr:           splitPaths(optString(opts, "--stderr", "")),
		Accounting:       splitPaths(optString(opts, "--accounting", "")),
		Samples:          splitPaths(optString(opts, "--samples", "")),
		ResourceReports:  splitPaths(optString(opts, "--resource-report", "")),
		Markers:          splitPaths(optString(opts, "--marker", "")),
		BaselineManifest: optString(opts, "--baseline", ""),
	}
	result := Score(inputs)
	content, err := marshalScoreResult(result)
	if err != nil {
		content = []byte("{\"score_schema\":1,\"raw_score\":0,\"normalized_score\":0,\"placeable\":false,\"gates\":{},\"diagnostics\":{},\"eval_error\":{\"kind\":\"infrastructure\",\"code\":\"json_encode_failed\",\"message\":\"scorer output encoding failed\"}}\n")
	}
	if outPath := optString(opts, "--out", ""); outPath != "" {
		if err := writeAtomicFile(outPath, content, 0o644); err != nil {
			fallback := errorScore("infrastructure", "score_output_write_failed", "scorer JSON output could not be persisted")
			content, _ = marshalScoreResult(fallback)
		}
	}
	_, _ = os.Stdout.Write(content)
}

// scoreBaselineFromReplicates is used by tests and evaluation orchestration to
// construct a manifest from already-validated baseline replicate diagnostics.
// The public scorer intentionally does not derive a trusted baseline from
// candidate-controlled artifacts.
func scoreBaselineFromReplicates(
	roundId string,
	configSha256 string,
	timeoutMs int64,
	flags map[string]string,
	takeoverMargin float64,
	replicates []ScoreBaselineReplicate,
) *ScoreBaselineManifest {
	return &ScoreBaselineManifest{
		ScoreSchema:      apexScoreSchema,
		Kind:             scoreBaselineKind,
		ScorerVersion:    officialScorerVersion,
		RoundId:          roundId,
		ConfigSha256:     configSha256,
		RequestTimeoutMs: timeoutMs,
		RunFlags:         flags,
		TakeoverMargin:   takeoverMargin,
		Replicates:       replicates,
	}
}

func scoreSidecarForCSV(path string) string {
	if strings.HasSuffix(path, ".csv") {
		return strings.TrimSuffix(path, ".csv") + ".run.json"
	}
	return filepath.Clean(path + ".run.json")
}
