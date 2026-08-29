package competition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"
)

const (
	evaluationProgressFileName  = "evaluation-progress.json"
	maxEvaluationProgressBytes  = 256 << 10
	evaluationProgressPollEvery = time.Second
)

var competitionLiveEvaluationMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork", Subsystem: "competition", Name: "live_evaluation_metric_value",
	Help: "Completed baseline and candidate replicate p50/p95 values for the current internal evaluation.",
}, []string{"job_id", "round_id", "role", "replicate", "metric", "quantile", "significance"})

func init() {
	prometheus.MustRegister(competitionLiveEvaluationMetric)
}

type evaluationProgress struct {
	Schema             int                        `json:"schema"`
	Kind               string                     `json:"kind"`
	JobId              string                     `json:"job_id"`
	RoundId            string                     `json:"round_id"`
	Phase              string                     `json:"phase"`
	ReplicateCount     int                        `json:"replicate_count"`
	BaselineCompleted  int                        `json:"baseline_completed"`
	CandidateCompleted int                        `json:"candidate_completed"`
	UpdatedUnixMs      int64                      `json:"updated_unix_ms"`
	Metrics            []evaluationProgressMetric `json:"metrics"`
}

type evaluationProgressMetric struct {
	Role         string   `json:"role"`
	Replicate    int      `json:"replicate"`
	Metric       string   `json:"metric"`
	Quantile     string   `json:"quantile"`
	Value        float64  `json:"value"`
	PImprovement *float64 `json:"p_improvement"`
	PRegression  *float64 `json:"p_regression"`
	Significance string   `json:"significance"`
}

var evaluationProgressMetrics = map[string]string{
	"ttfb_p50_ms":                "p50",
	"ttfb_p95_ms":                "p95",
	"throughput_p50_bytes_per_s": "p50",
	"throughput_p95_bytes_per_s": "p95",
}

// startEvaluationProgressMetrics watches the trusted evaluator's atomically
// replaced progress document. It is deliberately internal telemetry: the
// public score API continues to reveal results only after epoch finalization.
func startEvaluationProgressMetrics(
	ctx context.Context,
	path string,
	jobId string,
	roundId string,
	replicateCount int,
) func() {
	competitionLiveEvaluationMetric.Reset()
	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	lastUpdate := int64(0)
	lastError := ""
	refresh := func() {
		progress, err := readEvaluationProgress(path, jobId, roundId, replicateCount)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && err.Error() != lastError {
				glog.Infof("[competition]live evaluation progress ignored: %s\n", err)
			}
			lastError = err.Error()
			return
		}
		lastError = ""
		if progress.UpdatedUnixMs <= lastUpdate {
			return
		}
		applyEvaluationProgress(progress)
		lastUpdate = progress.UpdatedUnixMs
	}
	refresh()
	go func() {
		defer close(done)
		ticker := time.NewTicker(evaluationProgressPollEvery)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
	return func() {
		cancel()
		<-done
		// Capture a final atomic replacement written just before evaluator exit.
		refresh()
	}
}

func readEvaluationProgress(
	path string,
	jobId string,
	roundId string,
	replicateCount int,
) (*evaluationProgress, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || maxEvaluationProgressBytes < info.Size() {
		return nil, errors.New("evaluation progress is empty, oversized, or non-regular")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxEvaluationProgressBytes+1))
	decoder.DisallowUnknownFields()
	progress := &evaluationProgress{}
	if err := decoder.Decode(progress); err != nil {
		return nil, fmt.Errorf("decode evaluation progress: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("evaluation progress has trailing content")
	}
	if err := validateEvaluationProgress(progress, jobId, roundId, replicateCount); err != nil {
		return nil, err
	}
	return progress, nil
}

func validateEvaluationProgress(
	progress *evaluationProgress,
	jobId string,
	roundId string,
	replicateCount int,
) error {
	if progress == nil || progress.Schema != 1 ||
		progress.Kind != "sim-latency-evaluation-progress" ||
		progress.JobId != jobId || progress.RoundId != roundId ||
		progress.ReplicateCount != replicateCount ||
		replicateCount < 1 || 9 < replicateCount || replicateCount%2 == 0 ||
		progress.UpdatedUnixMs <= 0 {
		return errors.New("evaluation progress identity is invalid")
	}
	validPhase := map[string]bool{
		"preparing": true, "building": true, "baseline": true,
		"candidate": true, "scoring": true, "complete": true, "failed": true,
	}
	if !validPhase[progress.Phase] || progress.BaselineCompleted < 0 ||
		replicateCount < progress.BaselineCompleted || progress.CandidateCompleted < 0 ||
		replicateCount < progress.CandidateCompleted {
		return errors.New("evaluation progress phase or counts are invalid")
	}
	seen := map[string]bool{}
	roleCounts := map[string]int{"baseline": 0, "candidate": 0}
	for _, metric := range progress.Metrics {
		quantile, ok := evaluationProgressMetrics[metric.Metric]
		if !ok || metric.Quantile != quantile ||
			(metric.Role != "baseline" && metric.Role != "candidate") ||
			metric.Replicate < 1 || replicateCount < metric.Replicate ||
			math.IsNaN(metric.Value) || math.IsInf(metric.Value, 0) || metric.Value < 0 {
			return errors.New("evaluation progress metric identity or value is invalid")
		}
		completed := progress.BaselineCompleted
		if metric.Role == "candidate" {
			completed = progress.CandidateCompleted
		}
		if completed < metric.Replicate {
			return errors.New("evaluation progress metric exceeds its completed count")
		}
		key := fmt.Sprintf("%s/%d/%s", metric.Role, metric.Replicate, metric.Metric)
		if seen[key] {
			return errors.New("evaluation progress contains a duplicate metric")
		}
		seen[key] = true
		roleCounts[metric.Role]++
		if !validProbability(metric.PImprovement) || !validProbability(metric.PRegression) {
			return errors.New("evaluation progress p-value is invalid")
		}
		if metric.Role == "baseline" {
			if metric.Significance != "baseline" || metric.PImprovement != nil || metric.PRegression != nil {
				return errors.New("baseline progress claims candidate significance")
			}
			continue
		}
		switch metric.Significance {
		case "not_testable":
			if metric.PImprovement != nil || metric.PRegression != nil {
				return errors.New("untestable progress includes a p-value")
			}
		case "not_significant":
			if metric.PImprovement == nil || metric.PRegression == nil ||
				*metric.PImprovement <= 0.05 || *metric.PRegression <= 0.05 {
				return errors.New("nonsignificant progress contradicts its p-values")
			}
		case "improved":
			if metric.PImprovement == nil || 0.05 < *metric.PImprovement {
				return errors.New("improved progress lacks statistical support")
			}
		case "regressed":
			if metric.PRegression == nil || 0.05 < *metric.PRegression {
				return errors.New("regressed progress lacks statistical support")
			}
		default:
			return errors.New("evaluation progress significance is invalid")
		}
	}
	for role, completed := range map[string]int{
		"baseline":  progress.BaselineCompleted,
		"candidate": progress.CandidateCompleted,
	} {
		if roleCounts[role] != completed*len(evaluationProgressMetrics) {
			return errors.New("evaluation progress does not contain every completed metric")
		}
	}
	return nil
}

func validProbability(value *float64) bool {
	return value == nil || !math.IsNaN(*value) && !math.IsInf(*value, 0) && 0 <= *value && *value <= 1
}

func applyEvaluationProgress(progress *evaluationProgress) {
	competitionLiveEvaluationMetric.Reset()
	for _, metric := range progress.Metrics {
		competitionLiveEvaluationMetric.WithLabelValues(
			progress.JobId,
			progress.RoundId,
			metric.Role,
			strconv.Itoa(metric.Replicate),
			metric.Metric,
			metric.Quantile,
			metric.Significance,
		).Set(metric.Value)
	}
}
