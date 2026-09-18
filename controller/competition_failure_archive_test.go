// Exercises durable, sanitized evidence retention after failed evaluator runs.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Records retention calls without using a database, network, or evaluator image.
type failureArchiveTestArchive struct {
	fakeArtifactArchive
	calls      int
	manifest   artifactManifest
	archiveErr error
	contextErr error
	deadline   time.Time
	onArchive  func(context.Context, string, artifactManifest)
}

// Captures the exact manifest and cancellation boundary presented for retention.
func (self *failureArchiveTestArchive) ArchiveAttempt(ctx context.Context, _ *Settings, _ *queuedJob, directory string, manifest artifactManifest) (json.RawMessage, error) {
	self.calls++
	self.manifest = manifest
	self.contextErr = ctx.Err()
	self.deadline, _ = ctx.Deadline()
	if self.onArchive != nil {
		self.onArchive(ctx, directory, manifest)
	}
	if self.archiveErr != nil {
		return nil, self.archiveErr
	}
	return json.Marshal(manifest)
}

// Constructs a fully authenticated synthetic job with a pinned local shell fixture.
func failureArchiveEvaluatorFixture(t *testing.T, body func(*Settings, *queuedJob) string) (*Settings, *queuedJob, *failureArchiveTestArchive) {
	t.Helper()
	settings := validSettings()
	settings.ArtifactRoot = t.TempDir()
	if err := os.Chmod(settings.ArtifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	settings.ConfigLocalDirectory = t.TempDir()
	settings.VaultLocalDirectory = t.TempDir()
	var err error
	settings.EvaluationPolicy.ConfigLocalSha256, err = hashLocalMountDirectory(settings.ConfigLocalDirectory)
	if err != nil {
		t.Fatal(err)
	}
	settings.EvaluationPolicy.VaultLocalSha256, err = hashLocalMountDirectory(settings.VaultLocalDirectory)
	if err != nil {
		t.Fatal(err)
	}
	settings.EvaluationPolicy.ScoreTimeoutSeconds = 30
	round, _ := sealedTestRound(t, settings)
	round.Epoch = 1
	providers := writeArchiveTestFile(t, settings.ArtifactRoot,
		filepath.ToSlash(filepath.Join("rounds", round.RoundId.String(), "providers.yml")),
		[]byte("synthetic-hidden-workload\n"))
	round.ProvidersPath = filepath.Join(settings.ArtifactRoot, filepath.FromSlash(providers.Path))
	round.ProvidersSha256 = providers.Sha256
	patch := []byte("synthetic canonical patch\n")
	patchDigest := sha256.Sum256(patch)
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId: server.NewId(), RoundId: round.RoundId,
			PatchSha256:          hex.EncodeToString(patchDigest[:]),
			EvaluatorImageDigest: settings.EvaluatorImageDigest,
			ApiImageDigest:       testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest(),
		},
		Patch: patch, AttemptCount: 1, Round: *round,
	}
	settings.EvaluatorCommand = filepath.Join(t.TempDir(), "synthetic-evaluator.sh")
	if err := os.WriteFile(settings.EvaluatorCommand, []byte("#!/bin/sh\nset -eu\n"+body(settings, job)), 0o700); err != nil {
		t.Fatal(err)
	}
	settings.EvaluatorCommandSha256, _, err = hashRegularFile(settings.EvaluatorCommand)
	if err != nil {
		t.Fatal(err)
	}
	archive := &failureArchiveTestArchive{}
	settings.artifactArchive = archive
	t.Cleanup(func() {
		_ = filepath.WalkDir(settings.ArtifactRoot, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return err
		})
	})
	return settings, job, archive
}

// Quotes only synthetic fixture bytes for an evaluator shell command.
func failureArchiveShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// Emits the real sanitized manifest wire format without depending on new code.
func failureArchiveEvidenceScript(t *testing.T, job *queuedJob, mutate func(map[string]any)) string {
	t.Helper()
	evidence := "synthetic sanitized failure evidence\n"
	digest := sha256.Sum256([]byte(evidence))
	manifest := map[string]any{
		"schema": 1, "kind": "sim-latency-failed-evidence-manifest",
		"job_id": job.JobId.String(), "round_id": job.RoundId.String(),
		"attempt": job.AttemptCount, "sanitized": true,
		"artifacts": []evaluationArtifact{{
			Path: "failed-evidence/failure.json", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(evidence)),
		}},
	}
	if mutate != nil {
		mutate(manifest)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return "mkdir failed-evidence\nprintf '%s' " + failureArchiveShellQuote(evidence) + " > failed-evidence/failure.json\n" +
		"printf '%s' " + failureArchiveShellQuote(string(manifestBytes)) + " > failed-evidence-manifest.json\n" +
		"chmod 0400 failed-evidence/failure.json failed-evidence-manifest.json\n" +
		"printf '%s\\n' 'synthetic evaluator stderr' >&2\n"
}

// Emits the trusted candidate-only sidecar independently of a completed result.
func failureArchiveStageScript(t *testing.T, job *queuedJob, mutate func(map[string]any)) string {
	t.Helper()
	failure := map[string]any{
		"schema": 1, "kind": "sim-latency-stage-failure", "job_id": job.JobId.String(),
		"round_id": job.RoundId.String(), "attempt": job.AttemptCount,
		"role": "candidate", "stage": "run", "exit_code": 17,
		"error": map[string]any{
			"kind": "submission", "code": "run_process_failed",
			"message": "candidate runner exited unsuccessfully", "retriable": false,
		},
	}
	if mutate != nil {
		mutate(failure)
	}
	value, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	return "printf '%s' " + failureArchiveShellQuote(string(value)) + " > evaluator-failure.json\nchmod 0400 evaluator-failure.json\n"
}

// Reads failure metadata through the public retained wire format.
func failureArchiveOutcomeFailure(t *testing.T, outcome EvaluationOutcome) *CompetitionError {
	t.Helper()
	var manifest struct {
		Failure *CompetitionError `json:"failure"`
	}
	if err := json.Unmarshal(outcome.ArtifactManifest, &manifest); err != nil {
		t.Fatalf("retained failure manifest = %s, error = %v", outcome.ArtifactManifest, err)
	}
	return manifest.Failure
}

// A nonzero evaluator exit must retain its original failure plus sanitized evidence.
func TestCommandEvaluatorPreservesOriginalFailureAfterFailureEvidenceArchive(t *testing.T) {
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, nil) + "exit 17\n"
	})
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Code != "evaluator_exit" || outcome.Score != nil {
		t.Fatalf("failure outcome = %+v", outcome)
	}
	if archive.calls != 1 || archive.contextErr != nil || archive.deadline.IsZero() ||
		time.Until(archive.deadline) <= 0 || 2*time.Minute < time.Until(archive.deadline) {
		t.Fatalf("failure retention calls = %d, context = %v, deadline = %v", archive.calls, archive.contextErr, archive.deadline)
	}
	if failure := failureArchiveOutcomeFailure(t, outcome); failure == nil || *failure != *outcome.Error {
		t.Fatalf("retained original failure = %+v, outcome = %+v", failure, outcome.Error)
	}
	wanted := map[string]bool{"failed-evidence/failure.json": false, "failed-evidence-manifest.json": false}
	for _, artifact := range archive.manifest.Artifacts {
		if _, ok := wanted[artifact.Path]; ok {
			wanted[artifact.Path] = true
		}
		if strings.Contains(artifact.Path, "worker-request") || strings.Contains(artifact.Path, "providers") {
			t.Fatalf("hidden input selected for failure archive: %q", artifact.Path)
		}
	}
	for path, present := range wanted {
		if !present {
			t.Errorf("failure evidence %q was not selected for retention", path)
		}
	}
}

// Cancellation is synchronized after the fixture publishes evidence, without sleeps.
func TestCommandEvaluatorArchivesSanitizedEvidenceAfterCanceledRun(t *testing.T) {
	barrierPath := filepath.Join(t.TempDir(), "ready.fifo")
	if err := syscall.Mkfifo(barrierPath, 0o600); err != nil {
		t.Fatal(err)
	}
	barrier, err := os.OpenFile(barrierPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Close()
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, nil) + failureArchiveStageScript(t, job, nil) +
			"trap 'exit 143' TERM\nprintf x > " + failureArchiveShellQuote(barrierPath) + "\nwhile :; do :; done\n"
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan error, 1)
	go func() {
		var signal [1]byte
		_, err := io.ReadFull(barrier, signal[:])
		ready <- err
	}()
	completed := make(chan EvaluationOutcome, 1)
	go func() { completed <- (CommandEvaluator{}).Evaluate(ctx, settings, job) }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case outcome := <-completed:
		t.Fatalf("evaluator exited before cancellation barrier: %+v", outcome)
	case <-time.After(10 * time.Second):
		t.Fatal("fixture did not reach evidence publication barrier")
	}
	cancel()
	select {
	case outcome := <-completed:
		if outcome.Error == nil || outcome.Error.Code != "evaluator_process_failed" || archive.calls != 1 || archive.contextErr != nil {
			t.Fatalf("canceled evaluator outcome = %+v, archive calls = %d, archive context = %v", outcome, archive.calls, archive.contextErr)
		}
		if failure := failureArchiveOutcomeFailure(t, outcome); failure == nil || failure.Code != "evaluator_process_failed" {
			t.Fatalf("canceled evaluator retained failure = %+v", failure)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("canceled evaluator did not finish and archive")
	}
}

// Retention failure supersedes the run error so missing evidence cannot look retained.
func TestCommandEvaluatorSurfacesFailureEvidenceArchiveFailure(t *testing.T) {
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, nil) + "exit 17\n"
	})
	archive.archiveErr = errors.New("synthetic archive failure")
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Code != "artifact_archive_failed" || archive.calls != 1 || len(outcome.ArtifactManifest) != 0 {
		t.Fatalf("archive failure outcome = %+v, calls = %d", outcome, archive.calls)
	}
}

// Partial launch failures still archive controller-owned diagnostics and fixed artifacts.
func TestCommandEvaluatorArchivesMinimalFailureWithoutFailureManifest(t *testing.T) {
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(*Settings, *queuedJob) string {
		return "printf '%s\\n' 'synthetic early evaluator failure' >&2\nexit 17\n"
	})
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Code != "evaluator_exit" || archive.calls != 1 {
		t.Fatalf("partial-launch failure outcome = %+v, calls = %d", outcome, archive.calls)
	}
	if failure := failureArchiveOutcomeFailure(t, outcome); failure == nil || failure.Code != "evaluator_exit" {
		t.Fatalf("partial-launch retained failure = %+v", failure)
	}
	if len(archive.manifest.Artifacts) != 1 || archive.manifest.Artifacts[0].Path != "controller-failure.json" {
		t.Fatalf("partial-launch retained artifacts = %+v", archive.manifest.Artifacts)
	}
}

// Invalid present manifests must never fall back to trusting arbitrary attempt files.
func TestCommandEvaluatorRejectsUnauthenticatedFailureManifest(t *testing.T) {
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, func(manifest map[string]any) {
			manifest["job_id"] = server.NewId().String()
		}) + "exit 17\n"
	})
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Code != "artifact_archive_failed" || archive.calls != 0 || len(outcome.ArtifactManifest) != 0 {
		t.Fatalf("unauthenticated failure outcome = %+v, calls = %d", outcome, archive.calls)
	}
}

// The partial-stage exception takes effect only after authenticated retention succeeds.
func TestCommandEvaluatorClassifiesCandidateRunFailureAfterArchive(t *testing.T) {
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, nil) + failureArchiveStageScript(t, job, nil) + "exit 1\n"
	})
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Kind != "submission" || outcome.Error.Code != "run_process_failed" ||
		outcome.Error.Retriable || outcome.Score != nil || archive.calls != 1 {
		t.Fatalf("candidate run failure = %+v, archive calls = %d", outcome, archive.calls)
	}
	if failure := failureArchiveOutcomeFailure(t, outcome); failure == nil || failure.Kind != "submission" || failure.Code != "run_process_failed" {
		t.Fatalf("retained candidate failure = %+v", failure)
	}
	stageRetained := false
	for _, artifact := range archive.manifest.Artifacts {
		if artifact.Path == "evaluator-failure.json" {
			stageRetained = true
			if !sha256Pattern.MatchString(artifact.Sha256) || artifact.Bytes == 0 {
				t.Fatalf("candidate sidecar was not authenticated: %+v", artifact)
			}
		}
	}
	if !stageRetained {
		t.Fatal("candidate sidecar was not selected for retention")
	}
	if archive.manifest.ResultSha256 != "" || archive.manifest.Security.passedFor(outcome.Error) {
		t.Fatal("partial candidate failure fabricated completed result security")
	}
}

// A sidecar without the sanitizer proof cannot replace an infrastructure failure.
func TestCommandEvaluatorIgnoresStageFailureWithoutSanitizedManifest(t *testing.T) {
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveStageScript(t, job, nil) + "exit 17\n"
	})
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Kind != "infrastructure" || outcome.Error.Code != "evaluator_exit" || archive.calls != 1 {
		t.Fatalf("sidecar without sanitizer = %+v, archive calls = %d", outcome, archive.calls)
	}
	for _, artifact := range archive.manifest.Artifacts {
		if artifact.Path == "evaluator-failure.json" {
			t.Fatal("sidecar without sanitizer entered the minimal archive")
		}
	}
}

// A completed command with no full result cannot use a stale partial-stage sidecar.
func TestCommandEvaluatorDoesNotClassifyStageFailureAfterZeroExit(t *testing.T) {
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, nil) + failureArchiveStageScript(t, job, nil) + "exit 0\n"
	})
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Kind != "infrastructure" || outcome.Error.Code != "evaluator_result_missing" || archive.calls != 1 {
		t.Fatalf("zero-exit stale sidecar = %+v, archive calls = %d", outcome, archive.calls)
	}
}

// An archive failure prevents even a valid candidate sidecar from becoming submission blame.
func TestCommandEvaluatorDoesNotClassifyStageFailureWhenArchiveFails(t *testing.T) {
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, nil) + failureArchiveStageScript(t, job, nil) + "exit 1\n"
	})
	archive.archiveErr = errors.New("synthetic failed stage retention")
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Kind != "infrastructure" || outcome.Error.Code != "artifact_archive_failed" || len(outcome.ArtifactManifest) != 0 {
		t.Fatalf("candidate archive failure = %+v", outcome)
	}
}

// Untrusted identities and classifications fail closed before any archive call.
func TestCommandEvaluatorRejectsUnauthenticatedStageFailure(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "schema", mutate: func(value map[string]any) { value["schema"] = 2 }},
		{name: "kind", mutate: func(value map[string]any) { value["kind"] = "synthetic-other-kind" }},
		{name: "job", mutate: func(value map[string]any) { value["job_id"] = server.NewId().String() }},
		{name: "round", mutate: func(value map[string]any) { value["round_id"] = server.NewId().String() }},
		{name: "attempt", mutate: func(value map[string]any) { value["attempt"] = 2 }},
		{name: "baseline", mutate: func(value map[string]any) { value["role"] = "baseline" }},
		{name: "postgres", mutate: func(value map[string]any) { value["role"] = "postgres" }},
		{name: "stage", mutate: func(value map[string]any) { value["stage"] = "build" }},
		{name: "zero exit", mutate: func(value map[string]any) { value["exit_code"] = 0 }},
		{name: "large exit", mutate: func(value map[string]any) { value["exit_code"] = 256 }},
		{name: "extra field", mutate: func(value map[string]any) { value["synthetic_unknown"] = true }},
		{name: "retriable", mutate: func(value map[string]any) { value["error"].(map[string]any)["retriable"] = true }},
		{name: "missing retriable", mutate: func(value map[string]any) { delete(value["error"].(map[string]any), "retriable") }},
		{name: "error kind", mutate: func(value map[string]any) { value["error"].(map[string]any)["kind"] = "infrastructure" }},
		{name: "error code", mutate: func(value map[string]any) { value["error"].(map[string]any)["code"] = "candidate_build_failed" }},
		{name: "error message", mutate: func(value map[string]any) {
			value["error"].(map[string]any)["message"] = "arbitrary candidate diagnostic"
		}},
		{name: "error unknown field", mutate: func(value map[string]any) { value["error"].(map[string]any)["readiness"] = nil }},
	}
	for _, testCase := range cases {
		settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
			return failureArchiveEvidenceScript(t, job, nil) + failureArchiveStageScript(t, job, testCase.mutate) + "exit 1\n"
		})
		outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
		if outcome.Error == nil || outcome.Error.Code != "artifact_archive_failed" || archive.calls != 0 {
			t.Errorf("%s: invalid stage sidecar = %+v, archive calls = %d", testCase.name, outcome, archive.calls)
		}
	}
}

// Every post-launch validation exit retains its own typed cause without requiring a full result.
func TestCommandEvaluatorArchivesPostRunValidationFailures(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{name: "missing", code: "evaluator_result_missing"},
		{name: "malformed", code: "evaluator_result_invalid"},
		{name: "identity", code: "evaluator_result_invalid"},
		{name: "trailing", code: "evaluator_result_invalid"},
		{name: "both outcomes", code: "evaluator_result_invalid"},
		{name: "score", code: "score_result_invalid"},
		{name: "typed error", code: "evaluator_result_invalid"},
		{name: "security", code: "containment_gate_failed"},
		{name: "artifacts", code: "artifact_authentication_failed"},
		{name: "seal", code: "artifact_seal_failed"},
	}
	for _, testCase := range cases {
		settings, job, archive := failureArchiveEvaluatorFixture(t, func(settings *Settings, job *queuedJob) string {
			result := evaluatorResult{
				Schema: 1, JobId: job.JobId.String(),
				EvalError: &CompetitionError{Kind: "submission", Code: "candidate_build_failed", Message: "synthetic candidate build failed"},
				Security: evaluationSecurity{
					DefaultDenyNetwork: true, OfflineBuild: true, OfflineBuildResourceLimits: true,
					ManagementCpuReserved: true, ManagementMemoryReserved: true,
					NoProductionSecrets: true, StructuralPatchCheck: true, CleanupComplete: true, ImmutableReports: true,
				},
			}
			extra := ""
			switch testCase.name {
			case "identity":
				result.JobId = server.NewId().String()
			case "both outcomes":
				result.Score = &ScoreResult{}
			case "score":
				result.EvalError = nil
				result.Score = &ScoreResult{}
			case "typed error":
				result.EvalError.Retriable = true
			case "security":
				result.Security.CleanupComplete = false
			case "seal":
				rawScores := make([]float64, settings.EvaluationPolicy.Replicates)
				for i := range rawScores {
					rawScores[i] = 100 + float64(i%3) - 1
				}
				baseline := testRoundBaseline(t, settings, &job.Round, rawScores)
				for _, artifact := range []struct {
					path  string
					value []byte
				}{
					{path: "baseline.json", value: baseline},
					{path: "submission-error.json", value: []byte("{}\n")},
					{path: "evaluation.complete.json", value: []byte("{}\n")},
				} {
					digest := sha256.Sum256(artifact.value)
					result.Artifacts = append(result.Artifacts, evaluationArtifact{
						Path: artifact.path, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(artifact.value)),
					})
					extra += "printf '%s' " + failureArchiveShellQuote(string(artifact.value)) + " > " + failureArchiveShellQuote(artifact.path) + "\n"
				}
				extra += "ln -s worker-request.json untrusted-link\n"
			}
			value, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.name == "malformed" {
				value = []byte("synthetic invalid json")
			} else if testCase.name == "trailing" {
				value = append(value, []byte(" {}")...)
			}
			body := failureArchiveEvidenceScript(t, job, nil) + extra
			if testCase.name != "missing" {
				body += "printf '%s' " + failureArchiveShellQuote(string(value)) + " > worker-result.json\n"
			}
			return body + "exit 0\n"
		})
		outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
		if outcome.Error == nil || outcome.Error.Kind != "infrastructure" || outcome.Error.Code != testCase.code || archive.calls != 1 || outcome.Score != nil {
			t.Errorf("%s: post-run outcome = %+v, archive calls = %d", testCase.name, outcome, archive.calls)
			continue
		}
		if failure := failureArchiveOutcomeFailure(t, outcome); failure == nil || *failure != *outcome.Error {
			t.Errorf("%s: retained cause = %+v, original = %+v", testCase.name, failure, outcome.Error)
		}
		if archive.manifest.ResultSha256 != "" {
			t.Errorf("%s: invalid completed result was selected for retention", testCase.name)
		}
	}
}

// The failure-only manifest does not require a nonexistent completed worker result.
func TestBlobArtifactArchiveAcceptsFailureManifestWithoutWorkerResult(t *testing.T) {
	settings := validSettings()
	settings.RetainUntil = server.NowUtc().Add(time.Hour).Truncate(time.Second)
	store := server.NewLocalBlobStore(t.TempDir(), "synthetic-evidence").(server.RetainedBlobStore)
	archive := &blobArtifactArchive{store: store}
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId: server.NewId(), RoundId: server.NewId(),
			EvaluatorImageDigest: settings.EvaluatorImageDigest,
			ApiImageDigest:       testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest(),
		},
		AttemptCount: 1,
	}
	directory := t.TempDir()
	patch := writeArchiveTestFile(t, directory, "canonical.patch", []byte("synthetic patch\n"))
	stderr := writeArchiveTestFile(t, directory, "worker.stderr.log", []byte("synthetic stderr\n"))
	diagnostic := writeArchiveTestFile(t, directory, "controller-failure.json", []byte("{\"synthetic\":true}\n"))
	writeArchiveTestFile(t, directory, "worker-request.json", []byte("synthetic hidden request\n"))
	writeArchiveTestFile(t, directory, "providers.yml", []byte("synthetic hidden workload\n"))
	job.PatchSha256 = patch.Sha256
	manifestBytes, err := json.Marshal(map[string]any{
		"schema": 1, "job_id": job.JobId.String(), "round_id": job.RoundId.String(), "attempt": 1,
		"evaluator_image_digest": job.EvaluatorImageDigest, "api_image_digest": job.ApiImageDigest,
		"worker_image_digest": job.WorkerImageDigest, "patch_sha256": patch.Sha256,
		"stderr_sha256": stderr.Sha256, "artifacts": []evaluationArtifact{diagnostic},
		"failure": infrastructureError("evaluator_exit", "synthetic evaluator exit"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var manifest artifactManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	retainedBytes, err := archive.ArchiveAttempt(context.Background(), settings, job, directory, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(retainedBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Retention == nil || manifest.Retention.ObjectCount != 3 || manifest.Retention.HiddenSeedRequestRetained || !manifest.Retention.AuthenticatedAfterUpload {
		t.Fatalf("failure retention = %+v", manifest.Retention)
	}
	for _, artifact := range manifest.Retention.Objects {
		if artifact.Path != "canonical.patch" && artifact.Path != "worker.stderr.log" && artifact.Path != "controller-failure.json" {
			t.Fatalf("unexpected failure retention object: %+v", artifact)
		}
		reader, err := store.GetVersion(context.Background(), artifact.Key, artifact.VersionId)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.New()
		length, readErr := io.Copy(digest, reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || length != artifact.Bytes || hex.EncodeToString(digest.Sum(nil)) != artifact.Sha256 {
			t.Fatalf("retained failure object failed exact-version authentication: %+v, %v, %v", artifact, readErr, closeErr)
		}
	}
	if !strings.Contains(string(retainedBytes), fmt.Sprintf("\"code\":%q", "evaluator_exit")) {
		t.Fatalf("failure metadata missing from retained manifest: %s", retainedBytes)
	}
	manifest.Failure = nil
	if _, err := archive.ArchiveAttempt(context.Background(), settings, job, directory, manifest); err == nil {
		t.Fatal("completed attempt without worker-result.json was accepted")
	}
}
