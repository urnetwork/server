// Opt-in file-only calibration analysis reuses the actual scorer's failed-row
// policy. It cannot emit an authenticated scoring baseline or admit a round.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The selected private directory must contain exactly the nine completed CSVs
// and their run sidecars. This diagnostic is intentionally absent from normal
// test execution and never contacts services or consumes model credentials.
func TestExploratoryCalibrationRawScoreEvidence(t *testing.T) {
	root := os.Getenv("SIM_LATENCY_CALIBRATION_EVIDENCE_ROOT")
	if root == "" {
		t.Skip("requires an explicit completed private calibration evidence directory")
	}
	paths, err := filepath.Glob(filepath.Join(root, "runs", "*.csv"))
	if err != nil || len(paths) != 9 {
		t.Fatal("calibration evidence requires exactly nine retained replicates", len(paths), err)
	}
	type replicate struct {
		Csv              string  `json:"csv"`
		CsvSha256        string  `json:"csv_sha256"`
		RunSha256        string  `json:"run_sha256"`
		RawP95Ms         float64 `json:"failure_charged_p95_ms"`
		MeasuredRequests int64   `json:"measured_requests"`
		Failures         int     `json:"failures"`
		SuccessRate      float64 `json:"success_rate"`
	}
	rawScores := []float64{}
	replicates := []replicate{}
	var first *RunStats
	for _, path := range paths {
		runPath := sideCarCandidates(path)[0]
		content, err := os.ReadFile(runPath)
		if err != nil {
			t.Fatal(err)
		}
		var run RunStats
		if err := decodeStrictJSONBytes(content, &run, "exploratory run"); err != nil || run.Kind != runStatsKind || run.CompletionState != "complete" ||
			run.IncompleteCode != "" || run.Official || run.BuildModified || !sourceGitShaPattern.MatchString(run.BuildRevision) ||
			!validSha256(run.ConfigSha256) || run.MeasureStartMs >= run.MeasureEndMs || run.RequestTimeoutMs <= 0 {
			t.Fatal("exploratory replicate is incomplete or has invalid source/window identity", path, err)
		}
		if first == nil {
			first = &run
		} else if first.BuildRevision != run.BuildRevision || first.ConfigSha256 != run.ConfigSha256 || first.Seed != run.Seed ||
			first.RequestTimeoutMs != run.RequestTimeoutMs || first.NumCpu != run.NumCpu || !reflect.DeepEqual(first.Flags, run.Flags) {
			t.Fatal("calibration mixes source, workload, topology or resource identities", path)
		}
		rows, csvDigest, csvBytes, err := readScoreCSV(path)
		if err != nil || csvDigest != run.ResultsCsvSha256 || csvBytes != run.ResultsCsvBytes {
			t.Fatal("calibration CSV bytes differ from the retained sidecar", path, err)
		}
		metrics, err := scoreRows(rows, &run)
		if err != nil || metrics.RequestCount != int64(run.RowsInWindow) || metrics.Failures != run.Failures {
			t.Fatal("failure-charged row metrics differ from the measured window", path, err)
		}
		runDigest := sha256.Sum256(content)
		rawScores = append(rawScores, metrics.RawScore)
		replicates = append(replicates, replicate{Csv: filepath.Join("runs", filepath.Base(path)), CsvSha256: csvDigest,
			RunSha256: fmt.Sprintf("%x", runDigest), RawP95Ms: metrics.RawScore, MeasuredRequests: metrics.RequestCount,
			Failures: metrics.Failures, SuccessRate: metrics.SuccessRate})
	}
	mean, sd := meanStd(rawScores)
	median := quantile(rawScores, 0.5)
	// Historical 16.1% is an illustration, not a calibrated/frozen new margin.
	margin := 0.161
	equalVarianceDelta := studentTCrit(0.05, float64(2*len(rawScores)-2)) * sd * math.Sqrt(2/float64(len(rawScores)))
	content, err := json.MarshalIndent(map[string]any{
		"schema": 1, "kind": "sim-latency-exploratory-failure-charged-calibration", "official": false,
		"scoring_baseline": false, "scale_frozen": false, "margin_frozen": false, "hidden_seed": false,
		"build_revision": first.BuildRevision, "config_sha256": first.ConfigSha256, "seed": first.Seed,
		"run_flags": first.Flags, "request_timeout_ms": first.RequestTimeoutMs, "replicates": replicates,
		"mean_raw_p95_ms": mean, "median_raw_p95_ms": median, "sample_sd_ms": sd, "cv": sd / mean,
		"illustrative_margin_fraction": margin, "illustrative_practical_threshold_ms": median * (1 - margin),
		"approx_equal_variance_welch_alpha": 0.05, "approx_equal_variance_welch_mean_delta_ms": equalVarianceDelta,
		"approx_equal_variance_welch_mean_delta_fraction": equalVarianceDelta / mean,
		"significance_note": "Illustration only. A real candidate must pass all gates, the frozen practical margin and its own Welch test; candidate variance affects that test.",
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, '\n')
	path := filepath.Join(root, "failure-charged-calibration.json")
	if err := publishSourceTestRecord(path, content); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	t.Logf("exploratory raw-score report=%s sha256=%x median_ms=%.3f mean_ms=%.3f sd_ms=%.3f cv=%.4f illustrative_threshold_ms=%.3f", path, digest, median, mean, sd, sd/mean, median*(1-margin))
}
