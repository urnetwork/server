package competition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/urnetwork/server"
)

const RebaselineSchema = 1

// RebaselineResult authenticates a trusted same-round baseline evaluation.
// It intentionally contains no round seed, bearer token, or vault material.
type RebaselineResult struct {
	Schema                       int       `json:"schema"`
	Kind                         string    `json:"kind"`
	GeneratedAt                  time.Time `json:"generated_at"`
	RoundId                      server.Id `json:"round_id"`
	JobId                        server.Id `json:"job_id"`
	BaseSha                      string    `json:"base_sha"`
	EvaluatorImageDigest         string    `json:"evaluator_image_digest"`
	PatchSha256                  string    `json:"patch_sha256"`
	AttemptDirectory             string    `json:"attempt_directory"`
	BaselineSha256               string    `json:"baseline_sha256"`
	EvidenceManifestSha256       string    `json:"evidence_manifest_sha256"`
	WorkerResultSha256           string    `json:"worker_result_sha256"`
	EvaluationCompleteSha256     string    `json:"evaluation_complete_sha256"`
	WorkerArtifactManifestSha256 string    `json:"worker_artifact_manifest_sha256"`
	CandidatePlaceable           bool      `json:"candidate_placeable"`
}

// RunRebaseline evaluates a structurally valid trusted patch solely to create
// the pristine baseline evidence needed to admit submissions for roundId. The
// caller must stop the ordinary worker and hold the host's single-job
// operational lock for the entire call. Host promotion is a separate
// root-owned step that replays this evaluation with promote-host-containment.sh.
func RunRebaseline(
	ctx context.Context,
	settings *Settings,
	store Store,
	evaluator Evaluator,
	roundId server.Id,
	rawPatch string,
	expectedPatchSha256 string,
) (*RebaselineResult, error) {
	if settings == nil || store == nil || evaluator == nil {
		return nil, errors.New("rebaseline requires settings, store, and evaluator")
	}
	if err := settings.Validate(); err != nil {
		return nil, fmt.Errorf("rebaseline settings: %w", err)
	}
	patch, patchErr := ValidateAndCanonicalizePatch(rawPatch, settings.PatchPolicy)
	if patchErr != nil {
		return nil, fmt.Errorf("rebaseline patch: %w", patchErr)
	}
	defer clear(patch.Bytes)
	if !sha256Pattern.MatchString(expectedPatchSha256) ||
		patch.Sha256 != expectedPatchSha256 {
		return nil, errors.New("rebaseline patch does not match the pinned digest")
	}

	round, err := store.CurrentRound(ctx, settings)
	if err != nil {
		return nil, fmt.Errorf("rebaseline round: %w", err)
	}
	if round == nil || round.Canceled || round.RoundId != roundId {
		return nil, errors.New("rebaseline round is not the current round")
	}
	if round.Status != "scheduled" && round.Status != "open" {
		return nil, errors.New("rebaseline round is no longer eligible")
	}
	if !storedPolicyMatches(settings, round.PolicyJson) {
		return nil, errors.New("rebaseline round policy does not match settings")
	}
	hostCheck, err := evaluator.SelfCheck(ctx, settings)
	if err != nil {
		return nil, fmt.Errorf("rebaseline host self-check: %w", err)
	}
	if !hostCheck.Eligible(settings) {
		return nil, errors.New("rebaseline host is not containment-eligible")
	}
	if hostCheck.RebaselinePassed && hostCheck.RebaselineRoundId != nil &&
		*hostCheck.RebaselineRoundId == roundId {
		return nil, errors.New("host is already re-baselined for this round")
	}

	jobId := server.NewId()
	now := server.NowUtc()
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId:       jobId,
			RoundId:     roundId,
			PatchSha256: patch.Sha256,
			State:       "running",
			SubmittedAt: now,
			StartedAt:   &now,
		},
		Patch:        append([]byte(nil), patch.Bytes...),
		PrincipalId:  "trusted-rebaseline",
		AttemptCount: 1,
		Round:        *round,
	}
	defer clear(job.Patch)
	outcome := evaluator.Evaluate(ctx, settings, job)
	if outcome.Error != nil {
		return nil, fmt.Errorf("rebaseline evaluation: %w", outcome.Error)
	}
	if outcome.Score == nil || outcome.Infrastructure {
		return nil, errors.New("rebaseline evaluation did not return a score")
	}
	if err := validateScore(outcome.Score); err != nil {
		return nil, fmt.Errorf("rebaseline score: %w", err)
	}
	if !outcome.Score.Placeable || !scoreGatesPass(outcome.Score) {
		return nil, errors.New("rebaseline trusted candidate did not pass every score gate")
	}
	if len(outcome.ArtifactManifest) == 0 || !json.Valid(outcome.ArtifactManifest) {
		return nil, errors.New("rebaseline worker artifact manifest is missing")
	}

	attemptDirectory := filepath.Join(
		settings.ArtifactRoot,
		jobId.String(),
		"attempt-01",
	)
	result, err := authenticateRebaselineResult(
		settings,
		job,
		outcome,
		attemptDirectory,
	)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func scoreGatesPass(score *ScoreResult) bool {
	if score == nil || len(score.Gates) != len(requiredScoreGateNames) {
		return false
	}
	for _, name := range requiredScoreGateNames {
		gate, ok := score.Gates[name]
		if !ok || !gate.Passed {
			return false
		}
	}
	return true
}

func authenticateRebaselineResult(
	settings *Settings,
	job *queuedJob,
	outcome EvaluationOutcome,
	attemptDirectory string,
) (*RebaselineResult, error) {
	info, err := os.Lstat(attemptDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("rebaseline attempt directory is missing or unsafe")
	}
	paths := map[string]string{
		"baseline":            filepath.Join(attemptDirectory, "baseline.json"),
		"evidence_manifest":   filepath.Join(attemptDirectory, "evidence-manifest.json"),
		"worker_result":       filepath.Join(attemptDirectory, "worker-result.json"),
		"evaluation_complete": filepath.Join(attemptDirectory, "evaluation.complete.json"),
	}
	hashes := map[string]string{}
	for name, path := range paths {
		digest, size, hashErr := hashRegularFile(path)
		if hashErr != nil || size <= 0 {
			return nil, fmt.Errorf("rebaseline %s artifact is missing: %w", name, hashErr)
		}
		hashes[name] = digest
	}

	baselineBytes, err := readRegularFile(paths["baseline"], maxEvaluatorResultBytes)
	if err != nil {
		return nil, fmt.Errorf("read rebaseline baseline: %w", err)
	}
	var baseline struct {
		ScoreSchema      int               `json:"score_schema"`
		Kind             string            `json:"kind"`
		ScorerVersion    string            `json:"scorer_version"`
		RoundId          string            `json:"round_id"`
		ConfigSha256     string            `json:"config_sha256"`
		RequestTimeoutMs int64             `json:"request_timeout_ms"`
		TakeoverMargin   float64           `json:"takeover_margin"`
		Replicates       []json.RawMessage `json:"replicates"`
	}
	if err := json.Unmarshal(baselineBytes, &baseline); err != nil ||
		baseline.ScoreSchema != ScoreSchema ||
		baseline.Kind != "sim-latency-score-baseline" ||
		baseline.ScorerVersion != ScorerVersion ||
		baseline.RoundId != job.RoundId.String() ||
		baseline.ConfigSha256 != job.Round.ProvidersSha256 ||
		baseline.RequestTimeoutMs != settings.EvaluationPolicy.RequestTimeoutMs ||
		baseline.TakeoverMargin != settings.EvaluationPolicy.TakeoverMargin ||
		len(baseline.Replicates) != settings.EvaluationPolicy.Replicates {
		return nil, errors.New("rebaseline baseline artifact has the wrong identity, policy, or replicate count")
	}

	evidenceBytes, err := readRegularFile(paths["evidence_manifest"], maxEvaluatorResultBytes)
	if err != nil {
		return nil, fmt.Errorf("read rebaseline evidence manifest: %w", err)
	}
	var evidence struct {
		Schema    int                  `json:"schema"`
		Kind      string               `json:"kind"`
		JobId     string               `json:"job_id"`
		RoundId   string               `json:"round_id"`
		Artifacts []evaluationArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(evidenceBytes, &evidence); err != nil ||
		evidence.Schema != 1 ||
		evidence.Kind != "sim-latency-evidence-manifest" ||
		evidence.JobId != job.JobId.String() ||
		evidence.RoundId != job.RoundId.String() ||
		len(evidence.Artifacts) == 0 {
		return nil, errors.New("rebaseline evidence manifest has the wrong identity")
	}

	completionBytes, err := readRegularFile(paths["evaluation_complete"], maxEvaluatorResultBytes)
	if err != nil {
		return nil, fmt.Errorf("read rebaseline completion marker: %w", err)
	}
	var completion struct {
		Schema          int               `json:"schema"`
		Kind            string            `json:"kind"`
		JobId           string            `json:"job_id"`
		RoundId         string            `json:"round_id"`
		Attempt         int               `json:"attempt"`
		BaseImageId     string            `json:"base_image_id"`
		PatchSha256     string            `json:"patch_sha256"`
		ProvidersSha256 string            `json:"providers_sha256"`
		CleanupComplete bool              `json:"cleanup_complete"`
		Artifacts       map[string]string `json:"artifacts"`
	}
	if err := json.Unmarshal(completionBytes, &completion); err != nil ||
		completion.Schema != 1 ||
		completion.Kind != "sim-latency-worker-evaluation-complete" ||
		completion.JobId != job.JobId.String() ||
		completion.RoundId != job.RoundId.String() ||
		completion.Attempt != job.AttemptCount ||
		completion.BaseImageId != settings.EvaluatorImageDigest ||
		completion.PatchSha256 != job.PatchSha256 ||
		completion.ProvidersSha256 != job.Round.ProvidersSha256 ||
		!completion.CleanupComplete ||
		completion.Artifacts["baseline"] != hashes["baseline"] ||
		completion.Artifacts["evidence_manifest"] != hashes["evidence_manifest"] {
		return nil, errors.New("rebaseline completion marker has the wrong identity or evidence chain")
	}

	workerBytes, err := readRegularFile(paths["worker_result"], maxEvaluatorResultBytes)
	if err != nil {
		return nil, fmt.Errorf("read rebaseline worker result: %w", err)
	}
	var worker evaluatorResult
	if err := json.Unmarshal(workerBytes, &worker); err != nil ||
		worker.Schema != 1 || worker.JobId != job.JobId.String() ||
		worker.Score == nil || worker.EvalError != nil ||
		!worker.Security.passedFor(nil) ||
		!reflect.DeepEqual(worker.Score, outcome.Score) {
		return nil, errors.New("rebaseline worker result failed identity or containment checks")
	}

	var manifest artifactManifest
	if err := json.Unmarshal(outcome.ArtifactManifest, &manifest); err != nil ||
		manifest.Schema != 1 || manifest.JobId != job.JobId.String() ||
		manifest.RoundId != job.RoundId.String() ||
		manifest.Attempt != job.AttemptCount ||
		manifest.PatchSha256 != job.PatchSha256 ||
		manifest.EvaluatorImageDigest != settings.EvaluatorImageDigest ||
		manifest.EvaluatorCommandSha256 != settings.EvaluatorCommandSha256 ||
		!sha256Pattern.MatchString(manifest.RequestSha256) ||
		!sha256Pattern.MatchString(manifest.StderrSha256) ||
		manifest.ResultSha256 != hashes["worker_result"] ||
		!manifest.Security.passedFor(nil) {
		return nil, errors.New("rebaseline worker artifact manifest failed authentication")
	}
	requiredArtifacts := map[string]string{
		"baseline.json":            hashes["baseline"],
		"evidence-manifest.json":   hashes["evidence_manifest"],
		"evaluation.complete.json": hashes["evaluation_complete"],
	}
	for _, artifact := range manifest.Artifacts {
		if expected, ok := requiredArtifacts[artifact.Path]; ok &&
			artifact.Sha256 == expected && artifact.Bytes > 0 {
			delete(requiredArtifacts, artifact.Path)
		}
	}
	if len(requiredArtifacts) != 0 {
		return nil, errors.New("rebaseline worker artifact manifest omits required evidence")
	}
	manifestDigest := sha256.Sum256(outcome.ArtifactManifest)
	return &RebaselineResult{
		Schema:                       RebaselineSchema,
		Kind:                         "sim-latency-round-rebaseline-evaluation",
		GeneratedAt:                  server.NowUtc(),
		RoundId:                      job.RoundId,
		JobId:                        job.JobId,
		BaseSha:                      settings.BaseSha,
		EvaluatorImageDigest:         settings.EvaluatorImageDigest,
		PatchSha256:                  job.PatchSha256,
		AttemptDirectory:             attemptDirectory,
		BaselineSha256:               hashes["baseline"],
		EvidenceManifestSha256:       hashes["evidence_manifest"],
		WorkerResultSha256:           hashes["worker_result"],
		EvaluationCompleteSha256:     hashes["evaluation_complete"],
		WorkerArtifactManifestSha256: hex.EncodeToString(manifestDigest[:]),
		CandidatePlaceable:           outcome.Score.Placeable,
	}, nil
}
