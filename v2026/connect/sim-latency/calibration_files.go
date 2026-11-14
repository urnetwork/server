// Authenticates operator-pinned offline calibration artifacts before reduction.
// A trusted replay callback must execute the original scorer with its original
// read-only artifact layout; this does not weaken scorer path-identity checks.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// Pins one whole campaign independently of the files it names.
type calibrationFileManifest struct {
	Schema                 int                     `json:"schema"`
	Kind                   string                  `json:"kind"`
	BaseSha                string                  `json:"base_sha"`
	EvaluatorImageDigest   string                  `json:"evaluator_image_digest"`
	ProvidersSha256        string                  `json:"providers_sha256"`
	NoopPatchSha256        string                  `json:"noop_patch_sha256"`
	EvaluationPolicySha256 string                  `json:"evaluation_policy_sha256"`
	RoundSeedHex           string                  `json:"round_seed_hex"`
	SourceEpoch            int                     `json:"source_epoch"`
	Cohorts                []calibrationFileCohort `json:"cohorts"`
	Protocol               string                  `json:"protocol,omitempty"`
	ProtocolApprovalSha256 string                  `json:"protocol_approval_sha256,omitempty"`
	OriginalPlanSha256     string                  `json:"original_plan_sha256,omitempty"`
	OriginalPositions      []int                   `json:"original_positions,omitempty"`
}

// Paths in Inputs are relative to ArtifactDirectory, never candidate commands.
// Failed positions require their original request and terminal receipt too.
type calibrationFileCohort struct {
	JobId                  string      `json:"job_id"`
	RoundId                string      `json:"round_id"`
	ArtifactDirectory      string      `json:"artifact_directory"`
	RequestSha256          string      `json:"request_sha256"`
	BaselineSha256         string      `json:"baseline_sha256"`
	ScoreSha256            string      `json:"score_sha256"`
	WorkerResultSha256     string      `json:"worker_result_sha256"`
	EvidenceManifestSha256 string      `json:"evidence_manifest_sha256"`
	TerminalSha256         string      `json:"terminal_sha256"`
	FailureCode            string      `json:"failure_code,omitempty"`
	Inputs                 ScoreInputs `json:"inputs"`
	BaselineInputs         ScoreInputs `json:"baseline_inputs"`
}

// Matches the immutable evaluator's path/digest/length evidence contract.
type calibrationArtifactBinding struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Only a trusted local replay implementation supplies this callback. The
// returned bytes must exactly reproduce the retained score identity.
type calibrationScoreReplay func(context.Context, string, ScoreInputs, string, string, bool) ([]byte, error)

// Requires canonical roots and relative descendants, rejecting symlink aliases
// and traversal rather than allowing an artifact manifest to read host secrets.
func calibrationArtifactPath(root string, relative string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || relative == "" ||
		filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == "." ||
		relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("calibration artifact path is not canonical")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || resolvedRoot != root {
		return "", errors.New("calibration root is redirected or unavailable")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("calibration root must be a private directory")
	}
	path := filepath.Join(root, relative)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", errors.New("calibration artifact is redirected or unavailable")
	}
	return path, nil
}

// Reads digest-pinned bounded bytes; callers use the scorer's strict decoder.
func readCalibrationArtifact(root string, relative string, expectedSha256 string) ([]byte, error) {
	if !validSha256(expectedSha256) {
		return nil, errors.New("calibration artifact digest is missing or invalid")
	}
	path, err := calibrationArtifactPath(root, relative)
	if err != nil {
		return nil, err
	}
	content, err := readBoundedFile(path, "calibration artifact")
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expectedSha256 {
		return nil, errors.New("calibration artifact digest mismatch")
	}
	return content, nil
}

// Streams raw CSV/log/sample digests instead of retaining their bytes in memory.
func verifyCalibrationArtifactBindings(root string, bindings []calibrationArtifactBinding, settings calibrationAnalysisSettings) (map[string]bool, error) {
	if len(bindings) == 0 || len(bindings) > settings.MaximumEvidenceBindings || settings.MaximumArtifactBytes <= 0 || settings.MaximumCohortBytes <= 0 {
		return nil, errors.New("calibration evidence count or byte policy is invalid")
	}
	bound := map[string]bool{}
	var totalBytes int64
	for _, binding := range bindings {
		if bound[binding.Path] || !validSha256(binding.Sha256) || binding.Bytes < 0 ||
			binding.Bytes > settings.MaximumArtifactBytes || binding.Bytes > settings.MaximumCohortBytes-totalBytes {
			return nil, errors.New("calibration artifact binding is invalid or duplicated")
		}
		path, err := calibrationArtifactPath(root, binding.Path)
		if err != nil {
			return nil, err
		}
		if err := func() error {
			before, err := os.Lstat(path)
			if err != nil || !before.Mode().IsRegular() || before.Size() != binding.Bytes {
				return errors.New("calibration artifact length or type mismatch")
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() != binding.Bytes {
				return errors.New("calibration artifact length or type mismatch")
			}
			digest := sha256.New()
			n, err := io.Copy(digest, io.LimitReader(file, binding.Bytes+1))
			if err != nil || n != binding.Bytes || hex.EncodeToString(digest.Sum(nil)) != binding.Sha256 {
				return errors.New("calibration raw artifact digest mismatch")
			}
			return nil
		}(); err != nil {
			return nil, err
		}
		bound[binding.Path] = true
		totalBytes += binding.Bytes
	}
	return bound, nil
}

// Compares the pinned source, workload and complete shared evaluation policy.
// Recorded original paths are provenance, not paths followed by this reader.
func validateCalibrationRequest(content []byte, manifest calibrationFileManifest, cohort calibrationFileCohort, settings calibrationAnalysisSettings) error {
	var request map[string]json.RawMessage
	if err := decodeStrictJSONBytes(content, &request, "calibration request"); err != nil {
		return err
	}
	for key, expected := range map[string]string{
		"job_id": cohort.JobId, "round_id": cohort.RoundId, "base_sha": manifest.BaseSha,
		"evaluator_image_digest": manifest.EvaluatorImageDigest, "providers_sha256": manifest.ProvidersSha256,
		"patch_sha256": manifest.NoopPatchSha256, "round_seed_hex": manifest.RoundSeedHex,
		"round_baseline_path": "", "round_baseline_sha256": "", "scorer_version": officialScorerVersion,
	} {
		var value string
		if raw, ok := request[key]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value != expected {
			return errors.New("calibration request identity mismatch: " + key)
		}
	}
	for key, expected := range map[string]int{"schema": 1, "source_epoch": manifest.SourceEpoch} {
		var value int
		if raw, ok := request[key]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value != expected {
			return errors.New("calibration request version mismatch")
		}
	}
	var policy model.CompetitionEvaluationPolicy
	if err := decodeStrictJSONBytes(request["evaluation_policy"], &policy, "calibration policy"); err != nil {
		return err
	}
	canonical, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	var recordedFields, canonicalFields map[string]json.RawMessage
	if err := decodeStrictJSONBytes(request["evaluation_policy"], &recordedFields, "calibration policy fields"); err != nil {
		return err
	}
	if err := json.Unmarshal(canonical, &canonicalFields); err != nil {
		return err
	}
	if len(recordedFields) != len(canonicalFields) {
		return errors.New("calibration policy fields are incomplete")
	}
	for key := range canonicalFields {
		if raw, ok := recordedFields[key]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("calibration policy field is missing or null")
		}
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != manifest.EvaluationPolicySha256 ||
		policy.Replicates != settings.Replicates || policy.TakeoverMargin != settings.MarginFloor || policy.ScoreTimeoutSeconds != settings.ExecutionLimitSeconds {
		return errors.New("calibration evaluation policy mismatch")
	}
	return nil
}

// A successful terminal receipt must prove the whole bounded invocation, not
// just the measurement window. Failed receipts remain opaque retained evidence.
func validateCalibrationTerminal(content []byte, executionLimitSeconds int) error {
	var start, end time.Time
	var duration int
	seenStart, seenEnd, seenExit := false, false, false
	for _, line := range strings.Split(string(content), "\n") {
		switch {
		case strings.HasPrefix(line, "start_utc="):
			if seenStart {
				return errors.New("duplicate calibration start")
			}
			var err error
			start, err = time.Parse(time.RFC3339, strings.TrimPrefix(line, "start_utc="))
			if err != nil {
				return err
			}
			seenStart = true
		case strings.HasPrefix(line, "end_utc="):
			if seenEnd {
				return errors.New("duplicate calibration end")
			}
			var err error
			end, err = time.Parse(time.RFC3339, strings.TrimPrefix(line, "end_utc="))
			if err != nil {
				return err
			}
			seenEnd = true
		case strings.HasPrefix(line, "evaluator_exit="):
			var exit int
			if seenExit {
				return errors.New("duplicate calibration exit")
			}
			if n, err := fmt.Sscanf(line, "evaluator_exit=%d duration_seconds=%d", &exit, &duration); err != nil || n != 2 || exit != 0 ||
				line != fmt.Sprintf("evaluator_exit=%d duration_seconds=%d", exit, duration) {
				return errors.New("calibration invocation failed or has invalid timing")
			}
			seenExit = true
		}
	}
	if !seenStart || !seenEnd || !seenExit || duration < 0 || duration > executionLimitSeconds ||
		end.Sub(start) != time.Duration(duration)*time.Second || !strings.Contains(string(content), "\nterminal_cleanup_verified\n") {
		return errors.New("calibration invocation lacks bounded completion and cleanup")
	}
	return nil
}

// Validates the successful worker's independent artifact and containment record.
func validateCalibrationWorkerResult(content []byte, root string, jobId string, score *ScoreResult, settings calibrationAnalysisSettings) error {
	var result struct {
		Schema    int                           `json:"schema"`
		JobId     string                        `json:"job_id"`
		Score     *model.CompetitionScoreResult `json:"score"`
		EvalError json.RawMessage               `json:"eval_error"`
		Security  map[string]json.RawMessage    `json:"security"`
		Artifacts []calibrationArtifactBinding  `json:"artifacts"`
	}
	if err := decodeStrictJSONBytes(content, &result, "calibration worker result"); err != nil {
		return err
	}
	if result.Schema != 1 || result.JobId != jobId || result.Score == nil || string(result.EvalError) != "null" {
		return errors.New("calibration worker result identity or completion mismatch")
	}
	if err := model.ValidateCompetitionScore(result.Score); err != nil {
		return err
	}
	for _, key := range []string{
		"template_database_reset", "redis_reset", "cgroup_contained", "resource_limits", "management_cpu_reserved",
		"management_memory_reserved", "default_deny_network", "offline_build", "offline_build_resource_limits",
		"no_production_secrets", "structural_patch_check", "accounting_complete", "resource_report_complete",
		"cleanup_complete", "immutable_reports",
	} {
		var passed bool
		if json.Unmarshal(result.Security[key], &passed) != nil || !passed {
			return errors.New("calibration containment proof failed: " + key)
		}
	}
	for _, key := range []string{"cgroup_id", "template_database_id", "redis_generation_id"} {
		var value string
		if json.Unmarshal(result.Security[key], &value) != nil || value == "" {
			return errors.New("calibration isolation identity missing")
		}
	}
	bindings, err := verifyCalibrationArtifactBindings(root, result.Artifacts, settings)
	if err != nil {
		return err
	}
	required := []string{"accounting.json", "baseline.json", "resources.json", "score.json", "evaluation-progress.json", "evaluation.complete.json", "evidence-manifest.json"}
	if len(bindings) != len(required) {
		return errors.New("calibration worker artifact coverage mismatch")
	}
	for _, name := range required {
		if !bindings[name] {
			return errors.New("calibration worker artifact is missing")
		}
	}
	localContent, err := json.Marshal(score)
	if err != nil {
		return err
	}
	var local model.CompetitionScoreResult
	if err := json.Unmarshal(localContent, &local); err != nil {
		return err
	}
	if score.Diagnostics.BaselineTakeoverEligible == nil {
		return errors.New("calibration score omitted takeover verdict")
	}
	local.TakeoverEligible = *score.Diagnostics.BaselineTakeoverEligible
	if !reflect.DeepEqual(&local, result.Score) {
		return errors.New("calibration worker score differs from replay")
	}
	return nil
}

// A self-consistent recorded run is not enough if it shortened the requested
// workload or changed impairment. Validate material policy against actual inputs.
func validateCalibrationWorkload(request []byte, workload []byte, baseline *ScoreBaselineManifest, inputs ...ScoreInputs) error {
	var fields map[string]json.RawMessage
	if err := decodeStrictJSONBytes(request, &fields, "calibration workload request"); err != nil {
		return err
	}
	var policy model.CompetitionEvaluationPolicy
	if err := decodeStrictJSONBytes(fields["evaluation_policy"], &policy, "calibration workload policy"); err != nil {
		return err
	}
	var sourceEpoch int
	if err := json.Unmarshal(fields["source_epoch"], &sourceEpoch); err != nil || sourceEpoch < 0 || bytes.Equal(bytes.TrimSpace(fields["source_epoch"]), []byte("null")) {
		return errors.New("calibration workload source epoch is invalid")
	}
	config, err := decodeConfig(workload)
	if err != nil {
		return err
	}
	if baseline == nil || policy.ProviderCount <= 0 || policy.ClientPoolSize <= 0 || policy.DurationMs <= 0 || policy.RequestTimeoutMs <= 0 ||
		config.Providers.Count != policy.ProviderCount || len(config.Fleet) != policy.ProviderCount ||
		config.Clients.PoolSize != policy.ClientPoolSize || config.Clients.MeanPerMinute != float64(policy.ArrivalsPerMinute) ||
		config.Clients.QualityWindowSize != policy.QualityWindowSize || baseline.RequestTimeoutMs != policy.RequestTimeoutMs {
		return errors.New("calibration workload scale or request timeout differs from policy")
	}
	for key, milliseconds := range map[string]int64{
		"ramp": policy.RampMs, "prewarm": policy.PrewarmMs, "settle": policy.SettleMs, "duration": policy.DurationMs,
		"client_warmup_timeout": policy.ClientWarmupTimeoutMs, "request_timeout": policy.RequestTimeoutMs,
		"pipeline_interval": policy.PipelineIntervalMs, "test_timeout": policy.TestTimeoutMs, "announce_timeout": policy.AnnounceTimeoutMs,
	} {
		if milliseconds < 0 || milliseconds > math.MaxInt64/int64(time.Millisecond) ||
			baseline.RunFlags[key] != (time.Duration(milliseconds)*time.Millisecond).String() {
			return errors.New("calibration recorded timer differs from requested policy: " + key)
		}
	}
	for key, expected := range map[string]string{
		"impair": strconv.FormatBool(policy.ImpairmentEnabled), "fleet_shards": strconv.Itoa(policy.FleetShards),
		"hosts": strconv.Itoa(policy.ExchangeHosts), "site_listen": policy.SiteListen,
		"api_port": strconv.Itoa(policy.ApiPort), "reset": "true",
	} {
		if baseline.RunFlags[key] != expected {
			return errors.New("calibration recorded setting differs from requested policy: " + key)
		}
	}
	for _, bundle := range inputs {
		for _, run := range bundle.Runs {
			stats, _, err := readOfficialRunSidecar(run)
			if err != nil || stats.Seed != config.Seed || stats.ClientsPool != policy.ClientPoolSize || stats.SourceEpoch != sourceEpoch {
				return errors.New("calibration run seed or client pool differs from policy")
			}
		}
	}
	return nil
}
