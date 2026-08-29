package competition

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func testEvaluationProgress() *evaluationProgress {
	progress := &evaluationProgress{
		Schema: 1, Kind: "sim-latency-evaluation-progress",
		JobId: "job-1", RoundId: "round-1", Phase: "candidate",
		ReplicateCount: 1, BaselineCompleted: 1, CandidateCompleted: 1,
		UpdatedUnixMs: 1,
	}
	pImprovement := 0.01
	pRegression := 0.99
	for metric, quantile := range evaluationProgressMetrics {
		progress.Metrics = append(progress.Metrics, evaluationProgressMetric{
			Role: "baseline", Replicate: 1, Metric: metric,
			Quantile: quantile, Value: 100, Significance: "baseline",
		})
		progress.Metrics = append(progress.Metrics, evaluationProgressMetric{
			Role: "candidate", Replicate: 1, Metric: metric,
			Quantile: quantile, Value: 90, PImprovement: &pImprovement,
			PRegression: &pRegression, Significance: "improved",
		})
	}
	return progress
}

func TestEvaluationProgressValidationAndMetricExport(t *testing.T) {
	progress := testEvaluationProgress()
	if err := validateEvaluationProgress(progress, "job-1", "round-1", 1); err != nil {
		t.Fatal(err)
	}
	applyEvaluationProgress(progress)
	defer competitionLiveEvaluationMetric.Reset()
	metrics := make(chan prometheus.Metric, 16)
	competitionLiveEvaluationMetric.Collect(metrics)
	if got := len(metrics); got != 8 {
		t.Fatalf("exported live metrics = %d, want 8", got)
	}

	progress.Metrics = append(progress.Metrics, progress.Metrics[0])
	if err := validateEvaluationProgress(progress, "job-1", "round-1", 1); err == nil {
		t.Fatal("duplicate progress metric passed validation")
	}
}

func TestEvaluationProgressDecoderRejectsUnknownFields(t *testing.T) {
	progress := testEvaluationProgress()
	encoded, err := json.Marshal(progress)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded[:len(encoded)-1], []byte(`,"unexpected":true}`)...)
	path := filepath.Join(t.TempDir(), evaluationProgressFileName)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvaluationProgress(path, "job-1", "round-1", 1); err == nil {
		t.Fatal("unknown progress field passed strict decoding")
	}
}
