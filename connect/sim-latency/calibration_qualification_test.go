// Explicit retained-artifact qualification is separate from unit coverage and
// never turns one historical/offline cohort into a formal calibration campaign.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// Discovers only the pinned evaluator's known artifact layout. This helper
// reads caller-provided retained evidence; no production path or identity is
// embedded as fixture data, and no original file is changed.
func retainedCalibrationQualificationInputs(t *testing.T, attempt string) (calibrationFileManifest, calibrationFileCohort) {
	t.Helper()
	artifacts := filepath.Join(attempt, "artifacts")
	requestPath, err := calibrationArtifactPath(artifacts, "worker-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]json.RawMessage
	if err := decodeStrictJSON(requestPath, &request, "retained qualification request"); err != nil {
		t.Fatal(err)
	}
	text := func(key string) string {
		var value string
		if err := json.Unmarshal(request[key], &value); err != nil {
			t.Fatalf("retained request field %s: %v", key, err)
		}
		return value
	}
	digest := func(path string) string {
		content, err := readBoundedFile(path, "retained qualification identity")
		if err != nil {
			t.Fatal(err)
		}
		value := sha256.Sum256(content)
		return hex.EncodeToString(value[:])
	}
	var policy model.CompetitionEvaluationPolicy
	if err := decodeStrictJSONBytes(request["evaluation_policy"], &policy, "retained qualification policy"); err != nil {
		t.Fatal(err)
	}
	// Match the analyzer's shared typed-policy canonicalization, not raw spacing.
	canonicalPolicy, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	policyDigest := sha256.Sum256(canonicalPolicy)
	manifest := calibrationFileManifest{
		Schema: 1, Kind: "sim-latency-same-seed-calibration-inputs", BaseSha: text("base_sha"),
		EvaluatorImageDigest: text("evaluator_image_digest"), ProvidersSha256: text("providers_sha256"),
		NoopPatchSha256: text("patch_sha256"), EvaluationPolicySha256: hex.EncodeToString(policyDigest[:]), RoundSeedHex: text("round_seed_hex"),
	}
	if err := json.Unmarshal(request["source_epoch"], &manifest.SourceEpoch); err != nil {
		t.Fatal(err)
	}
	input := calibrationFileCohort{
		JobId: text("job_id"), RoundId: text("round_id"), ArtifactDirectory: artifacts,
		RequestSha256: digest(requestPath), BaselineSha256: digest(filepath.Join(artifacts, "baseline.json")),
		ScoreSha256: digest(filepath.Join(artifacts, "score.json")), WorkerResultSha256: digest(filepath.Join(artifacts, "worker-result.json")),
		EvidenceManifestSha256: digest(filepath.Join(artifacts, "evidence-manifest.json")), TerminalSha256: digest(filepath.Join(attempt, "manager-terminal.log")),
	}
	corpus := filepath.Join(artifacts, "evidence", "scorer-input")
	entries, err := os.ReadDir(corpus)
	if err != nil {
		t.Fatal(err)
	}
	relative := func(path string) string {
		value, err := filepath.Rel(artifacts, path)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	originalPath := func(path string) string {
		if !strings.HasPrefix(path, "/artifacts/") || strings.Contains(path, "..") {
			t.Fatal("retained sidecar uses an unexpected original scorer namespace")
		}
		return filepath.Join("evidence", "scorer-input", strings.TrimPrefix(path, "/artifacts/"))
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		var bundle *ScoreInputs
		if strings.HasPrefix(entry.Name(), "baseline-") {
			bundle = &input.BaselineInputs
		} else if strings.HasPrefix(entry.Name(), "candidate-") {
			bundle = &input.Inputs
		} else {
			t.Fatalf("unexpected retained corpus directory %q", entry.Name())
		}
		root := filepath.Join(corpus, entry.Name())
		run := filepath.Join(root, "results.csv")
		stats, _, err := readOfficialRunSidecar(run)
		if err != nil {
			t.Fatal(err)
		}
		bundle.Runs = append(bundle.Runs, relative(run))
		bundle.Stderr = append(bundle.Stderr, relative(filepath.Join(root, "stderr.log")))
		bundle.Accounting = append(bundle.Accounting, originalPath(stats.AccountingReport))
		bundle.Samples = append(bundle.Samples, originalPath(stats.StatsRoot))
		bundle.ResourceReports = append(bundle.ResourceReports, originalPath(stats.ResourceReport))
		bundle.Markers = append(bundle.Markers, originalPath(stats.FinalMarker))
		bundle.BaselineManifest = filepath.Join("evidence", "scorer-input", "baseline.json")
	}
	manifest.Cohorts = []calibrationFileCohort{input}
	return manifest, input
}

// Default suite invocation is a no-op helper, not live qualification. Only an
// explicitly selected root execution can read retained evidence and start the
// actual scorer. The caller must unset these variables before normal suites.
func TestCalibrationRetainedArtifactQualificationHelper(t *testing.T) {
	attempt := os.Getenv("SIM_LATENCY_CALIBRATION_QUALIFY_ROOT")
	if attempt == "" {
		return
	}
	if os.Geteuid() != 0 {
		t.Fatal("explicit retained qualification requires the configured root manager principal")
	}
	settings := defaultCalibrationAnalysisSettings()
	manifest, input := retainedCalibrationQualificationInputs(t, attempt)
	replaySettings := defaultCalibrationReplaySettings()
	replaySettings.CpuSet = os.Getenv("SIM_LATENCY_CALIBRATION_QUALIFY_CPUSET")
	ctx := context.Background()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-replaySettings.CleanupTimeout))
		defer cancel()
	}
	_, err := withCalibrationScoringSlot(os.Getenv("SIM_LATENCY_CALIBRATION_QUALIFY_LOCK"), func() (*calibrationAnalysisReport, error) {
		cohort, err := loadCalibrationFileCohort(ctx, manifest, input, settings,
			func(ctx context.Context, root string, inputs ScoreInputs, image string, round string, baseline bool) ([]byte, error) {
				return replayCalibrationBundle(ctx, root, inputs, image, round, baseline, settings, replaySettings, runSourceBuildDocker)
			})
		if err != nil {
			return nil, err
		}
		if err := validateCalibrationAnalysisCohort(cohort, settings); err != nil {
			return nil, err
		}
		if !cohort.Result.Placeable || cohort.Result.EvalError != nil {
			return nil, fmt.Errorf("retained no-op was not placeable: %v", cohort.Result.EvalError)
		}
		t.Log("actual frozen-image baseline and candidate replay passed; this is not a formal calibration sample or release-acceptance decision")
		return &calibrationAnalysisReport{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
