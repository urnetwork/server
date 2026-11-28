// Retained four-instance evidence must stay bound to exact CSVs, finalized
// sidecars, completion markers, topology and every original controller outcome.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// Compare against the declared dataset, not copied live identities or rounded
// example values. This fails if one-host data is relabeled as four instances,
// failed requests are discarded, or post-processing failure is hidden.
func TestFourExchangeCalibrationMatchesRawEvidenceAndControllerOutcomes(t *testing.T) {
	root := filepath.Join("baseline", "prelaunch-four-exchange-2026-10-24")
	read := func(path string, target any) []byte {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || json.Unmarshal(content, target) != nil {
			t.Fatal("retained calibration document is absent or malformed", path, err)
		}
		return content
	}
	var index struct {
		Replicates         int    `json:"replicates"`
		Discarded          int    `json:"replicates_discarded"`
		ExchangeHosts      int    `json:"exchange_hosts"`
		PhysicalHosts      int    `json:"physical_hosts"`
		EvaluationCpuCount int    `json:"evaluation_cpu_count"`
		Clients            int    `json:"clients"`
		Official           bool   `json:"official"`
		ScaleFrozen        bool   `json:"scale_frozen"`
		MarginFrozen       bool   `json:"margin_frozen"`
		MeasuredRequests   int64  `json:"measured_requests"`
		Failures           int    `json:"failed_requests"`
		ControllersPassed  int    `json:"measurement_controllers_exit_zero"`
		CoordinatorExit    int    `json:"enclosing_coordinator_exit_code"`
		AnalysisRecovered  bool   `json:"descriptive_analysis_recovered_without_remeasurement"`
		RecoveredExit      int    `json:"recovered_analysis_exit_code"`
		ProvidersSha256    string `json:"providers_sha256"`
		RawAnalysisSha256  string `json:"raw_analysis_sha256"`
	}
	read("INDEX.json", &index)
	if index.Replicates != 9 || index.Discarded != 0 || index.ExchangeHosts != 4 || index.PhysicalHosts != 1 || index.EvaluationCpuCount <= 0 ||
		index.Official || index.ScaleFrozen || index.MarginFrozen || index.ControllersPassed != index.Replicates || index.CoordinatorExit != 1 || !index.AnalysisRecovered || index.RecoveredExit != 0 {
		t.Fatal("calibration lost topology, exploratory status, or the original post-processing failure")
	}
	var report struct {
		Official        bool    `json:"official"`
		ScoringBaseline bool    `json:"scoring_baseline"`
		Mean            float64 `json:"mean_raw_p95_ms"`
		Sd              float64 `json:"sample_sd_ms"`
		Cv              float64 `json:"cv"`
		Replicates      []struct {
			Csv              string  `json:"csv"`
			CsvSha256        string  `json:"csv_sha256"`
			RunSha256        string  `json:"run_sha256"`
			RawScore         float64 `json:"failure_charged_p95_ms"`
			MeasuredRequests int64   `json:"measured_requests"`
			Failures         int     `json:"failures"`
		} `json:"replicates"`
	}
	content := read("failure-charged-calibration.json", &report)
	if fmt.Sprintf("%x", sha256.Sum256(content)) != index.RawAnalysisSha256 || report.Official || report.ScoringBaseline || len(report.Replicates) != index.Replicates {
		t.Fatal("raw calibration report changed identity or claims scoring authority")
	}
	var image struct {
		BaseSha            string `json:"base_sha"`
		SourceConfigSha256 string `json:"source_config_sha256"`
	}
	read("image-record.json", &image)
	manifestBytes, err := os.ReadFile(filepath.Join(root, "source-manifest.yml"))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(manifestBytes)) != image.SourceConfigSha256 {
		t.Fatal("retained source manifest differs from the image's embedded configuration", err)
	}
	manifest, err := decodeSourceManifest(manifestBytes)
	if err != nil || validateSourceManifest(manifest) != nil {
		t.Fatal("calibration source manifest no longer validates the complete live repository set", err)
	}
	generation, err := manifest.generation(0)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Repositories map[string]string `json:"repositories"`
	}
	read("source-record.json", &record)
	if record.Repositories["server"] != image.BaseSha || !reflect.DeepEqual(record.Repositories, generation.Repositories.commits()) {
		t.Fatal("retained source record differs from the source manifest or image")
	}
	providers, err := os.ReadFile(filepath.Join(root, "input", "providers.yml"))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(providers)) != index.ProvidersSha256 {
		t.Fatal("calibration provider input differs from its measured identity", err)
	}
	var requests int64
	failures := 0
	scores := []float64{}
	for i, retained := range report.Replicates {
		prefix := strconv.Itoa(i + 1)
		if retained.Csv != filepath.Join("runs", prefix+".csv") {
			t.Fatal("calibration reordered, duplicated or redirected a replicate", i)
		}
		var run RunStats
		manifest := read(filepath.Join("runs", prefix+".run.json"), &run)
		if run.CompletionState != "complete" || run.IncompleteCode != "" || run.Official || run.BuildModified || run.BuildRevision != image.BaseSha ||
			run.ConfigSha256 != index.ProvidersSha256 || run.NumCpu != index.EvaluationCpuCount || run.Flags["hosts"] != strconv.Itoa(index.ExchangeHosts) ||
			run.Flags["fleet_shards"] != strconv.Itoa(index.ExchangeHosts) || run.Flags["impair"] != "true" || run.ClientsPool != index.Clients || run.ClientsEstablished != index.Clients ||
			fmt.Sprintf("%x", sha256.Sum256(manifest)) != retained.RunSha256 {
			t.Fatal("calibration run changed source, topology, completion or warmup identity", prefix)
		}
		if err := validateFinalMarker(filepath.Join(root, "runs", prefix+".complete.json"), manifest, &run); err != nil {
			t.Fatal("calibration completion marker no longer authenticates its run", prefix, err)
		}
		rows, digest, size, err := readScoreCSV(filepath.Join(root, retained.Csv))
		if err != nil || digest != retained.CsvSha256 || digest != run.ResultsCsvSha256 || size != run.ResultsCsvBytes {
			t.Fatal("retained calibration CSV changed", prefix, err)
		}
		metrics, err := scoreRows(rows, &run)
		if err != nil || math.Abs(metrics.RawScore-retained.RawScore) > 1e-7 || metrics.RequestCount != retained.MeasuredRequests || metrics.RequestCount != int64(run.RowsInWindow) ||
			metrics.Failures != retained.Failures || metrics.Failures != run.Failures {
			t.Fatal("calibration report dropped failures or changed the scored window", prefix, err)
		}
		requests += metrics.RequestCount
		failures += metrics.Failures
		scores = append(scores, metrics.RawScore)
	}
	mean, sd := meanStd(scores)
	if requests != index.MeasuredRequests || failures != index.Failures || math.Abs(mean-report.Mean) > 1e-7 || math.Abs(sd-report.Sd) > 1e-7 || math.Abs(sd/mean-report.Cv) > 1e-7 {
		t.Fatal("calibration totals or between-run variance differ from the exact measured rows")
	}
	progress, err := os.ReadFile(filepath.Join(root, "progress.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(progress))
	for i := 1; i <= index.Replicates; i++ {
		var outcome struct {
			Replicate int  `json:"replicate"`
			ExitCode  int  `json:"exit_code"`
			Elapsed   int  `json:"elapsed_seconds"`
			Official  bool `json:"official"`
		}
		if err := decoder.Decode(&outcome); err != nil || outcome.Replicate != i || outcome.ExitCode != 0 || outcome.Elapsed <= 0 || outcome.Official {
			t.Fatal("calibration lost an original clean measurement outcome", i, err)
		}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatal("calibration contains extra or malformed controller outcomes", err)
	}
}
