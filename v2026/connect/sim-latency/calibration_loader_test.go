// Composed loader tests use real scorer reductions over synthetic official-format
// artifacts. Worker containment claims are fixtures, not live qualification.
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

	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// Encodes deterministic private fixture bytes and returns their actual digest.
func writeCalibrationJSONFixture(t *testing.T, path string, value any) string {
	t.Helper()
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// Keeps original path-bearing reports internally consistent in a fresh owned
// fixture location; no production report or host directory is modified.
func relocateCalibrationScoreFixture(t *testing.T, fixture *scoreFixture, target string, configSha256 string, revision string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture.dir, target); err != nil {
		t.Fatal(err)
	}
	for _, path := range []*string{&fixture.run, &fixture.stderr, &fixture.accounting, &fixture.samples, &fixture.resource, &fixture.marker} {
		*path = filepath.Join(target, filepath.Base(*path))
	}
	fixture.runStats.ConfigSha256, fixture.runStats.BuildRevision = configSha256, revision
	fixture.runStats.StatsRoot, fixture.runStats.ResourceReport = fixture.samples, fixture.resource
	fixture.runStats.AccountingReport, fixture.runStats.FinalMarker = fixture.accounting, fixture.marker
	for key, value := range map[string]string{
		"client_warmup_timeout": "2s", "pipeline_interval": "10s", "test_timeout": "3s",
		"announce_timeout": "2s", "site_listen": "192.0.2.1:0", "api_port": "12345",
	} {
		fixture.runStats.Flags[key] = value
	}
	if err := writeRunStats(scoreSidecarForCSV(fixture.run), fixture.runStats); err != nil {
		t.Fatal(err)
	}
	if err := writeFinalMarker(fixture.marker, scoreSidecarForCSV(fixture.run), fixture.runStats); err != nil {
		t.Fatal(err)
	}
}

// Creates one complete synthetic R9 cohort. One-cohort settings are test-only;
// the public command still requires all five formal positions.
func newCalibrationLoaderFixture(t *testing.T) (string, string, calibrationAnalysisSettings, calibrationFileManifest, calibrationScoreReplay) {
	t.Helper()
	settings := defaultCalibrationAnalysisSettings()
	settings.Cohorts, settings.BootstrapDraws = 1, 64
	root := newCalibrationFileRoot(t)
	attempt := filepath.Join(root, "attempt")
	artifacts := filepath.Join(attempt, "artifacts")
	if err := os.MkdirAll(artifacts, 0700); err != nil {
		t.Fatal(err)
	}
	request, manifest, input := newCalibrationRequestFixture(t)
	manifest.Schema, manifest.Kind = 1, "sim-latency-same-seed-calibration-inputs"
	input.ArtifactDirectory = artifacts
	workload, err := yaml.Marshal(&Config{
		Providers: ProvidersConfig{Count: 1}, Fleet: []ProviderEntry{{Index: 0, Ip: "192.0.2.1"}},
		Clients: ClientsConfig{PoolSize: 10, MeanPerMinute: 1, QualityWindowSize: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := writeCalibrationFileFixture(t, artifacts, "providers.yml", workload)
	patch := writeCalibrationFileFixture(t, artifacts, "canonical.patch", []byte("synthetic no-op patch identity\n"))
	manifest.ProvidersSha256, manifest.NoopPatchSha256 = provider.Sha256, patch.Sha256
	request["providers_sha256"], request["patch_sha256"] = provider.Sha256, patch.Sha256
	policy := model.CompetitionEvaluationPolicy{
		Replicates: 9, TakeoverMargin: settings.MarginFloor, ScoreTimeoutSeconds: 10800,
		ProviderCount: 1, ClientPoolSize: 10, ArrivalsPerMinute: 1, QualityWindowSize: 2,
		ExchangeHosts: 4, FleetShards: 1, SiteListen: "192.0.2.1:0", ApiPort: 12345,
		RampMs: 1000, PrewarmMs: 46800000, SettleMs: 1000, DurationMs: 1000,
		RequestTimeoutMs: 1000, ClientWarmupTimeoutMs: 2000, PipelineIntervalMs: 10000,
		TestTimeoutMs: 3000, AnnounceTimeoutMs: 2000, ImpairmentEnabled: true,
	}
	policyBytes, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	policyDigest := sha256.Sum256(policyBytes)
	manifest.EvaluationPolicySha256 = hex.EncodeToString(policyDigest[:])
	request["evaluation_policy"] = policy
	input.RequestSha256 = writeCalibrationJSONFixture(t, filepath.Join(artifacts, "worker-request.json"), request)
	bundles := map[string]*ScoreInputs{"baseline": {}, "candidate": {}}
	for _, role := range []string{"baseline", "candidate"} {
		for i := 0; i < settings.Replicates; i++ {
			// Controls require both live throughput series: tiny-request fixtures
			// intentionally lack that tier and cannot qualify an official baseline.
			options := baselineScoreFixtureOptions()
			options.evaluationId = fmt.Sprintf("synthetic-%s-%d", role, i)
			fixture := newScoreFixture(t, options)
			target := filepath.Join(artifacts, "evidence", "scorer-input", fmt.Sprintf("%s-%d", role, i))
			revision := manifest.BaseSha
			if role == "candidate" {
				revision = strings.Repeat("c", 40)
			}
			relocateCalibrationScoreFixture(t, fixture, target, provider.Sha256, revision)
			bundle := bundles[role]
			bundle.Runs = append(bundle.Runs, fixture.run)
			bundle.Stderr = append(bundle.Stderr, fixture.stderr)
			bundle.Accounting = append(bundle.Accounting, fixture.accounting)
			bundle.Samples = append(bundle.Samples, fixture.samples)
			bundle.ResourceReports = append(bundle.ResourceReports, fixture.resource)
			bundle.Markers = append(bundle.Markers, fixture.marker)
		}
	}
	controls := bundles["baseline"]
	baseline, err := BuildScoreBaseline(ScoreBaselineInputs{
		Runs: controls.Runs, Stderr: controls.Stderr, Accounting: controls.Accounting, Samples: controls.Samples,
		ResourceReports: controls.ResourceReports, Markers: controls.Markers, RoundId: input.RoundId, TakeoverMargin: settings.MarginFloor,
	})
	if err != nil {
		t.Fatal(err)
	}
	baselinePath := filepath.Join(artifacts, "evidence", "scorer-input", "baseline.json")
	input.BaselineSha256 = writeCalibrationJSONFixture(t, baselinePath, baseline)
	writeCalibrationJSONFixture(t, filepath.Join(artifacts, "baseline.json"), baseline)
	for _, bundle := range bundles {
		bundle.BaselineManifest = baselinePath
	}
	result := Score(*bundles["candidate"])
	if result.EvalError != nil || !result.Placeable {
		t.Fatalf("synthetic composed scorer fixture failed: %+v", result)
	}
	input.ScoreSha256 = writeCalibrationJSONFixture(t, filepath.Join(artifacts, "score.json"), result)
	for _, name := range []string{"accounting.json", "resources.json", "evaluation-progress.json", "evaluation.complete.json"} {
		writeCalibrationJSONFixture(t, filepath.Join(artifacts, name), map[string]any{"schema": 1, "synthetic": true})
	}
	bindings := []calibrationArtifactBinding{}
	if err := filepath.WalkDir(filepath.Join(artifacts, "evidence"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(artifacts, path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		bindings = append(bindings, calibrationArtifactBinding{Path: relative, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	input.EvidenceManifestSha256 = writeCalibrationJSONFixture(t, filepath.Join(artifacts, "evidence-manifest.json"), map[string]any{
		"schema": 1, "kind": "sim-latency-evidence-manifest", "job_id": input.JobId, "round_id": input.RoundId, "artifacts": bindings,
	})
	workerBindings := []calibrationArtifactBinding{}
	for _, name := range []string{"accounting.json", "baseline.json", "resources.json", "score.json", "evaluation-progress.json", "evaluation.complete.json", "evidence-manifest.json"} {
		content, err := os.ReadFile(filepath.Join(artifacts, name))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		workerBindings = append(workerBindings, calibrationArtifactBinding{Path: name, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))})
	}
	security := map[string]any{}
	for _, name := range []string{"template_database_reset", "redis_reset", "cgroup_contained", "resource_limits", "management_cpu_reserved",
		"management_memory_reserved", "default_deny_network", "offline_build", "offline_build_resource_limits", "no_production_secrets",
		"structural_patch_check", "accounting_complete", "resource_report_complete", "cleanup_complete", "immutable_reports"} {
		security[name] = true
	}
	for _, name := range []string{"cgroup_id", "template_database_id", "redis_generation_id"} {
		security[name] = "synthetic-isolation"
	}
	resultContent, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var workerScore model.CompetitionScoreResult
	if err := json.Unmarshal(resultContent, &workerScore); err != nil {
		t.Fatal(err)
	}
	workerScore.TakeoverEligible = *result.Diagnostics.BaselineTakeoverEligible
	input.WorkerResultSha256 = writeCalibrationJSONFixture(t, filepath.Join(artifacts, "worker-result.json"), map[string]any{
		"schema": 1, "job_id": input.JobId, "score": workerScore, "eval_error": nil, "security": security, "artifacts": workerBindings,
	})
	terminal := []byte("start_utc=2000-01-02T00:00:00Z\nevaluator_exit=0 duration_seconds=6\nend_utc=2000-01-02T00:00:06Z\nterminal_cleanup_verified\n")
	input.TerminalSha256 = writeCalibrationFileFixture(t, attempt, "manager-terminal.log", terminal).Sha256
	relativeInputs := func(bundle ScoreInputs) ScoreInputs {
		for _, values := range [][]string{bundle.Runs, bundle.Stderr, bundle.Accounting, bundle.Samples, bundle.ResourceReports, bundle.Markers} {
			for i, path := range values {
				relative, err := filepath.Rel(artifacts, path)
				if err != nil {
					t.Fatal(err)
				}
				values[i] = relative
			}
		}
		bundle.BaselineManifest, err = filepath.Rel(artifacts, bundle.BaselineManifest)
		if err != nil {
			t.Fatal(err)
		}
		return bundle
	}
	input.Inputs, input.BaselineInputs = relativeInputs(*bundles["candidate"]), relativeInputs(*controls)
	manifest.Cohorts = []calibrationFileCohort{input}
	manifestPath := filepath.Join(root, "input.json")
	manifestSha256 := writeCalibrationJSONFixture(t, manifestPath, manifest)
	replay := func(ctx context.Context, gotRoot string, inputs ScoreInputs, image string, roundId string, baseline bool) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if gotRoot != artifacts || image != manifest.EvaluatorImageDigest || roundId != input.RoundId {
			t.Fatal("loader changed the bound replay identity")
		}
		if baseline {
			value, err := BuildScoreBaseline(ScoreBaselineInputs{Runs: inputs.Runs, Stderr: inputs.Stderr, Accounting: inputs.Accounting,
				Samples: inputs.Samples, ResourceReports: inputs.ResourceReports, Markers: inputs.Markers, RoundId: roundId, TakeoverMargin: settings.MarginFloor})
			if err != nil {
				return nil, err
			}
			return marshalScoreBaseline(value)
		}
		return marshalScoreResult(Score(inputs))
	}
	return manifestPath, manifestSha256, settings, manifest, replay
}

// The complete reader and both production reducers run together; no summary is
// accepted solely because a mocked JSON verdict said it was healthy.
func TestCalibrationLoaderReplaysCompleteOfficialFormatCohort(t *testing.T) {
	path, digest, settings, _, replay := newCalibrationLoaderFixture(t)
	report, err := analyzeCalibrationFiles(context.Background(), path, digest, settings, replay)
	if err != nil || !report.Accepted || report.ObservedReplicates != 18 || report.InputManifestSha256 != digest {
		t.Fatalf("composed calibration failed: %+v, %v", report, err)
	}
}

// A changed CSV is rejected by its original binding before any replay callback.
func TestCalibrationLoaderRejectsTamperedRawInputBeforeReplay(t *testing.T) {
	path, digest, settings, manifest, _ := newCalibrationLoaderFixture(t)
	cohort := manifest.Cohorts[0]
	rawPath := filepath.Join(cohort.ArtifactDirectory, cohort.Inputs.Runs[0])
	if err := os.WriteFile(rawPath, []byte("changed synthetic CSV\n"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := analyzeCalibrationFiles(context.Background(), path, digest, settings,
		func(context.Context, string, ScoreInputs, string, string, bool) ([]byte, error) {
			called = true
			return nil, nil
		})
	if err == nil || called {
		t.Fatalf("changed raw input reached replay: called=%v error=%v", called, err)
	}
}

// A digest pin cannot be bypassed by changing the campaign's round/source fields.
func TestCalibrationLoaderRejectsChangedInputManifestBeforeReplay(t *testing.T) {
	path, digest, settings, manifest, _ := newCalibrationLoaderFixture(t)
	manifest.BaseSha = "changed-synthetic-source"
	writeCalibrationJSONFixture(t, path, manifest)
	called := false
	_, err := analyzeCalibrationFiles(context.Background(), path, digest, settings,
		func(context.Context, string, ScoreInputs, string, string, bool) ([]byte, error) {
			called = true
			return nil, nil
		})
	if err == nil || called {
		t.Fatalf("changed manifest reached replay: called=%v error=%v", called, err)
	}
}

// A replay must reproduce the original control, not simply produce valid JSON.
func TestCalibrationLoaderRejectsDifferentReplayedBaseline(t *testing.T) {
	path, digest, settings, _, replay := newCalibrationLoaderFixture(t)
	_, err := analyzeCalibrationFiles(context.Background(), path, digest, settings,
		func(ctx context.Context, root string, inputs ScoreInputs, image string, round string, baseline bool) ([]byte, error) {
			content, err := replay(ctx, root, inputs, image, round, baseline)
			if baseline && err == nil {
				content = append(content, '\n')
			}
			return content, err
		})
	if err == nil {
		t.Fatal("different baseline replay accepted")
	}
}

// Valid candidate reports cannot be relabeled as the frozen source controls.
func TestCalibrationLoaderRejectsCandidateAsControl(t *testing.T) {
	path, _, settings, manifest, replay := newCalibrationLoaderFixture(t)
	manifest.Cohorts[0].BaselineInputs = manifest.Cohorts[0].Inputs
	digest := writeCalibrationJSONFixture(t, path, manifest)
	_, err := analyzeCalibrationFiles(context.Background(), path, digest, settings, replay)
	if err == nil || !strings.Contains(err.Error(), "frozen source") {
		t.Fatalf("candidate accepted as control: %v", err)
	}
}

// A candidate replay must reproduce exact retained output bytes too.
func TestCalibrationLoaderRejectsDifferentReplayedCandidate(t *testing.T) {
	path, digest, settings, _, replay := newCalibrationLoaderFixture(t)
	_, err := analyzeCalibrationFiles(context.Background(), path, digest, settings,
		func(ctx context.Context, root string, inputs ScoreInputs, image string, round string, baseline bool) ([]byte, error) {
			content, err := replay(ctx, root, inputs, image, round, baseline)
			if !baseline && err == nil {
				content = append(content, '\n')
			}
			return content, err
		})
	if err == nil {
		t.Fatal("different candidate replay accepted")
	}
}

// Pinned bytes alone cannot make a failed containment claim pass.
func TestCalibrationLoaderRejectsFailedWorkerContainment(t *testing.T) {
	path, _, settings, manifest, replay := newCalibrationLoaderFixture(t)
	workerPath := filepath.Join(manifest.Cohorts[0].ArtifactDirectory, "worker-result.json")
	content, err := os.ReadFile(workerPath)
	if err != nil {
		t.Fatal(err)
	}
	var worker map[string]any
	if err := json.Unmarshal(content, &worker); err != nil {
		t.Fatal(err)
	}
	worker["security"].(map[string]any)["cleanup_complete"] = false
	manifest.Cohorts[0].WorkerResultSha256 = writeCalibrationJSONFixture(t, workerPath, worker)
	digest := writeCalibrationJSONFixture(t, path, manifest)
	_, err = analyzeCalibrationFiles(context.Background(), path, digest, settings, replay)
	if err == nil || !strings.Contains(err.Error(), "containment") {
		t.Fatalf("failed worker containment accepted: %v", err)
	}
}

// Even correctly re-pinned request bytes cannot relabel a short/on workload as
// a longer/off policy. Reject before any scorer process is requested.
func TestCalibrationLoaderRejectsPolicyThatDiffersFromRecordedWorkload(t *testing.T) {
	for _, changeDuration := range []bool{false, true} {
		path, _, settings, manifest, _ := newCalibrationLoaderFixture(t)
		input := &manifest.Cohorts[0]
		requestPath := filepath.Join(input.ArtifactDirectory, "worker-request.json")
		content, err := os.ReadFile(requestPath)
		if err != nil {
			t.Fatal(err)
		}
		var request map[string]json.RawMessage
		if err := json.Unmarshal(content, &request); err != nil {
			t.Fatal(err)
		}
		var policy model.CompetitionEvaluationPolicy
		if err := json.Unmarshal(request["evaluation_policy"], &policy); err != nil {
			t.Fatal(err)
		}
		if changeDuration {
			policy.DurationMs++
		} else {
			policy.ImpairmentEnabled = false
		}
		policyBytes, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		request["evaluation_policy"] = policyBytes
		policyDigest := sha256.Sum256(policyBytes)
		manifest.EvaluationPolicySha256 = hex.EncodeToString(policyDigest[:])
		input.RequestSha256 = writeCalibrationJSONFixture(t, requestPath, request)
		digest := writeCalibrationJSONFixture(t, path, manifest)
		called := false
		_, err = analyzeCalibrationFiles(context.Background(), path, digest, settings,
			func(context.Context, string, ScoreInputs, string, string, bool) ([]byte, error) {
				called = true
				return nil, nil
			})
		if err == nil || called {
			t.Fatalf("changed workload policy reached replay: duration=%v called=%v error=%v", changeDuration, called, err)
		}
	}
}
