// Loads digest-pinned offline cohorts, verifies their complete retained evidence
// and requires an exact original-scorer replay before statistical reduction.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolves every scorer input under the private retained artifact directory.
func resolveCalibrationScoreInputs(root string, inputs ScoreInputs, bound map[string]bool, settings calibrationAnalysisSettings) (ScoreInputs, error) {
	resolved := ScoreInputs{}
	examined := 0
	for _, field := range []struct {
		values []string
		out    *[]string
	}{
		{values: inputs.Runs, out: &resolved.Runs}, {values: inputs.Stderr, out: &resolved.Stderr},
		{values: inputs.Accounting, out: &resolved.Accounting}, {values: inputs.ResourceReports, out: &resolved.ResourceReports},
		{values: inputs.Markers, out: &resolved.Markers},
	} {
		for _, relative := range field.values {
			if !bound[relative] {
				return resolved, errors.New("calibration scorer input is not evidence-bound")
			}
			path, err := calibrationArtifactPath(root, relative)
			if err != nil {
				return resolved, err
			}
			*field.out = append(*field.out, path)
		}
	}
	for _, relative := range inputs.Samples {
		path, err := calibrationArtifactPath(root, relative)
		if err != nil {
			return resolved, err
		}
		files := 0
		if err := filepath.WalkDir(path, func(samplePath string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			examined++
			if examined > settings.MaximumTraversalEntries {
				return errors.New("calibration sample traversal exceeds its bound")
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return errors.New("calibration sample corpus contains a link")
			}
			if entry.IsDir() {
				return nil
			}
			key, err := filepath.Rel(root, samplePath)
			if err != nil || !entry.Type().IsRegular() || !bound[key] {
				return errors.New("calibration sample is not a bound regular file")
			}
			files++
			return nil
		}); err != nil {
			return resolved, err
		}
		if files == 0 {
			return resolved, errors.New("calibration sample corpus is empty")
		}
		resolved.Samples = append(resolved.Samples, path)
	}
	if !bound[inputs.BaselineManifest] {
		return resolved, errors.New("calibration scorer control is not evidence-bound")
	}
	baselinePath, err := calibrationArtifactPath(root, inputs.BaselineManifest)
	if err != nil {
		return resolved, err
	}
	resolved.BaselineManifest = baselinePath
	return resolved, nil
}

// A declared failure cannot be silently replaced; its request and terminal
// evidence still authenticate the intended source/workload and failed position.
func loadCalibrationFileCohort(ctx context.Context, manifest calibrationFileManifest, input calibrationFileCohort,
	settings calibrationAnalysisSettings, replay calibrationScoreReplay) (calibrationAnalysisCohort, error) {
	cohort := calibrationAnalysisCohort{}
	if err := ctx.Err(); err != nil {
		return cohort, err
	}
	request, err := readCalibrationArtifact(input.ArtifactDirectory, "worker-request.json", input.RequestSha256)
	if err != nil {
		return cohort, err
	}
	if err := validateCalibrationRequest(request, manifest, input, settings); err != nil {
		return cohort, err
	}
	terminal, err := readCalibrationArtifact(filepath.Dir(input.ArtifactDirectory), "manager-terminal.log", input.TerminalSha256)
	if err != nil {
		return cohort, err
	}
	if input.FailureCode != "" {
		cohort.FailureCode = input.FailureCode
		return cohort, nil
	}
	// The request's entire policy was already checked against its frozen digest.
	// The global submission budget is separately bound into analyzer settings.
	if err := validateCalibrationTerminal(terminal, settings.ExecutionLimitSeconds); err != nil {
		return cohort, err
	}
	manifestBytes, err := readCalibrationArtifact(input.ArtifactDirectory, "evidence-manifest.json", input.EvidenceManifestSha256)
	if err != nil {
		return cohort, err
	}
	var evidence struct {
		Schema    int                          `json:"schema"`
		Kind      string                       `json:"kind"`
		JobId     string                       `json:"job_id"`
		RoundId   string                       `json:"round_id"`
		Artifacts []calibrationArtifactBinding `json:"artifacts"`
	}
	if err := decodeStrictJSONBytes(manifestBytes, &evidence, "calibration evidence manifest"); err != nil {
		return cohort, err
	}
	if evidence.Schema != 1 || evidence.Kind != "sim-latency-evidence-manifest" || evidence.JobId != input.JobId || evidence.RoundId != input.RoundId {
		return cohort, errors.New("calibration evidence identity mismatch")
	}
	bound, err := verifyCalibrationArtifactBindings(input.ArtifactDirectory, evidence.Artifacts, settings)
	if err != nil {
		return cohort, err
	}
	workload, err := readCalibrationArtifact(input.ArtifactDirectory, "providers.yml", manifest.ProvidersSha256)
	if err != nil {
		return cohort, err
	}
	if _, err := readCalibrationArtifact(input.ArtifactDirectory, "canonical.patch", manifest.NoopPatchSha256); err != nil {
		return cohort, err
	}
	baselineContent, err := readCalibrationArtifact(input.ArtifactDirectory, "baseline.json", input.BaselineSha256)
	if err != nil {
		return cohort, err
	}
	inputs, err := resolveCalibrationScoreInputs(input.ArtifactDirectory, input.Inputs, bound, settings)
	if err != nil {
		return cohort, err
	}
	baseline, baselineSha256, err := readScoreBaseline(inputs.BaselineManifest)
	if err != nil {
		return cohort, err
	}
	if baselineSha256 != input.BaselineSha256 || baseline.ConfigSha256 != manifest.ProvidersSha256 || baseline.RoundId != input.RoundId {
		return cohort, errors.New("calibration control identity mismatch")
	}
	if err := validateInputCardinality(inputs, settings.Replicates); err != nil {
		return cohort, err
	}
	stored, err := readCalibrationArtifact(input.ArtifactDirectory, "score.json", input.ScoreSha256)
	if err != nil {
		return cohort, err
	}
	if replay == nil {
		return cohort, errors.New("calibration requires the original scorer replay")
	}
	controlInputs, err := resolveCalibrationScoreInputs(input.ArtifactDirectory, input.BaselineInputs, bound, settings)
	if err != nil {
		return cohort, err
	}
	if err := validateInputCardinality(controlInputs, settings.Replicates); err != nil {
		return cohort, err
	}
	if controlInputs.BaselineManifest != inputs.BaselineManifest {
		return cohort, errors.New("calibration baseline replay uses a different control manifest")
	}
	if err := validateCalibrationWorkload(request, workload, baseline, controlInputs, inputs); err != nil {
		return cohort, err
	}
	for _, run := range controlInputs.Runs {
		stats, _, err := readOfficialRunSidecar(run)
		if err != nil || stats.BuildRevision != manifest.BaseSha || stats.BuildModified {
			return cohort, errors.New("calibration control run does not bind the frozen source")
		}
		cohort.ControlEvaluationIds = append(cohort.ControlEvaluationIds, stats.EvaluationId)
	}
	replayedBaseline, err := replay(ctx, input.ArtifactDirectory, controlInputs, manifest.EvaluatorImageDigest, input.RoundId, true)
	if err != nil || !bytes.Equal(baselineContent, replayedBaseline) {
		return cohort, errors.New("calibration baseline replay does not reproduce the retained control")
	}
	replayed, err := replay(ctx, input.ArtifactDirectory, inputs, manifest.EvaluatorImageDigest, input.RoundId, false)
	if err != nil {
		return cohort, fmt.Errorf("calibration scorer replay: %w", err)
	}
	if !bytes.Equal(stored, replayed) {
		return cohort, errors.New("calibration replay does not reproduce the exact retained score")
	}
	var result ScoreResult
	if err := decodeStrictJSONBytes(replayed, &result, "calibration replay result"); err != nil {
		return cohort, err
	}
	worker, err := readCalibrationArtifact(input.ArtifactDirectory, "worker-result.json", input.WorkerResultSha256)
	if err != nil {
		return cohort, err
	}
	if err := validateCalibrationWorkerResult(worker, input.ArtifactDirectory, input.JobId, &result, settings); err != nil {
		return cohort, err
	}
	// Recheck the original bytes after replay; neither copy nor replay authorizes
	// a changed corpus. Original path-bearing sidecars remain untouched.
	if _, err := verifyCalibrationArtifactBindings(input.ArtifactDirectory, evidence.Artifacts, settings); err != nil {
		return cohort, err
	}
	cohort.Baseline, cohort.BaselineSha256, cohort.Result = baseline, baselineSha256, &result
	for _, diagnostic := range result.Diagnostics.Replicates {
		cohort.Candidates = append(cohort.Candidates, candidateReplicate{
			diagnostics: diagnostic, stabilityOK: len(diagnostic.StabilityFindings) == 0,
			resourcesOK: len(diagnostic.ResourceFindings) == 0,
		})
	}
	return cohort, nil
}

// Validates the operator-pinned campaign before any replay container can start.
func loadCalibrationFileManifest(path string, expectedSha256 string, settings calibrationAnalysisSettings) (*calibrationFileManifest, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	content, err := readCalibrationArtifact(filepath.Dir(abs), filepath.Base(abs), expectedSha256)
	if err != nil {
		return nil, err
	}
	var manifest calibrationFileManifest
	if err := decodeStrictJSONBytes(content, &manifest, "calibration input manifest"); err != nil {
		return nil, err
	}
	if manifest.Schema != 1 || manifest.Kind != "sim-latency-same-seed-calibration-inputs" ||
		len(manifest.Cohorts) != settings.Cohorts || !sourceGitShaPattern.MatchString(manifest.BaseSha) ||
		!strings.HasPrefix(manifest.EvaluatorImageDigest, "sha256:") ||
		!validSha256(strings.TrimPrefix(manifest.EvaluatorImageDigest, "sha256:")) ||
		!validSha256(manifest.ProvidersSha256) || !validSha256(manifest.NoopPatchSha256) ||
		!validSha256(manifest.EvaluationPolicySha256) || !validSha256(manifest.RoundSeedHex) || manifest.SourceEpoch < 0 {
		return nil, errors.New("calibration campaign identity or sample cardinality is invalid")
	}
	jobs, rounds := map[string]bool{}, map[string]bool{}
	for _, cohort := range manifest.Cohorts {
		if cohort.JobId == "" || cohort.RoundId == "" || jobs[cohort.JobId] || rounds[cohort.RoundId] {
			return nil, errors.New("calibration campaign repeats or omits a job/round identity")
		}
		jobs[cohort.JobId], rounds[cohort.RoundId] = true, true
	}
	return &manifest, nil
}

// Keeps actual file/manifest identities alongside the derived numerical report.
func analyzeCalibrationFiles(ctx context.Context, path string, expectedSha256 string, settings calibrationAnalysisSettings,
	replay calibrationScoreReplay) (*calibrationAnalysisReport, error) {
	manifest, err := loadCalibrationFileManifest(path, expectedSha256, settings)
	if err != nil {
		return nil, err
	}
	cohorts := make([]calibrationAnalysisCohort, 0, len(manifest.Cohorts))
	for i, input := range manifest.Cohorts {
		cohort, err := loadCalibrationFileCohort(ctx, *manifest, input, settings, replay)
		if err != nil {
			return nil, fmt.Errorf("cohort %d: %w", i+1, err)
		}
		cohorts = append(cohorts, cohort)
	}
	report, err := analyzeCalibrationCohorts(cohorts, settings)
	if err != nil {
		return nil, err
	}
	// Bind the input set explicitly; this is not a claim of MinIO retention.
	report.InputManifestSha256 = expectedSha256
	report.EvaluationPolicySha256 = manifest.EvaluationPolicySha256
	return report, nil
}
