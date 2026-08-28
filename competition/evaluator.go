package competition

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxSelfCheckBytes       = 1 << 20
	maxEvaluatorResultBytes = 8 << 20
	processTermGrace        = 10 * time.Second
)

type Evaluator interface {
	SelfCheck(context.Context, *Settings) (HostSelfCheck, error)
	Evaluate(context.Context, *Settings, *queuedJob) EvaluationOutcome
}

type CommandEvaluator struct{}

type evaluatorRequest struct {
	Schema               int              `json:"schema"`
	JobId                string           `json:"job_id"`
	RoundId              string           `json:"round_id"`
	Attempt              int              `json:"attempt"`
	CompetitionId        string           `json:"competition_id"`
	BaseSha              string           `json:"base_sha"`
	EvaluatorImageDigest string           `json:"evaluator_image_digest"`
	ScorerVersion        string           `json:"scorer_version"`
	RoundSeedHex         string           `json:"round_seed_hex"`
	ProvidersPath        string           `json:"providers_path"`
	ProvidersSha256      string           `json:"providers_sha256"`
	PatchPath            string           `json:"patch_path"`
	PatchSha256          string           `json:"patch_sha256"`
	ArtifactDirectory    string           `json:"artifact_directory"`
	ConfigLocalDirectory string           `json:"config_local_directory"`
	VaultLocalDirectory  string           `json:"vault_local_directory"`
	PatchPolicy          PatchPolicy      `json:"patch_policy"`
	EvaluationPolicy     EvaluationPolicy `json:"evaluation_policy"`
}

type evaluatorResult struct {
	Schema    int                  `json:"schema"`
	JobId     string               `json:"job_id"`
	Score     *ScoreResult         `json:"score"`
	EvalError *CompetitionError    `json:"eval_error"`
	Security  evaluationSecurity   `json:"security"`
	Artifacts []evaluationArtifact `json:"artifacts"`
}

type evaluationSecurity struct {
	TemplateDatabaseReset      bool   `json:"template_database_reset"`
	RedisReset                 bool   `json:"redis_reset"`
	CgroupContained            bool   `json:"cgroup_contained"`
	ResourceLimits             bool   `json:"resource_limits"`
	ManagementCpuReserved      bool   `json:"management_cpu_reserved"`
	ManagementMemoryReserved   bool   `json:"management_memory_reserved"`
	DefaultDenyNetwork         bool   `json:"default_deny_network"`
	OfflineBuild               bool   `json:"offline_build"`
	OfflineBuildResourceLimits bool   `json:"offline_build_resource_limits"`
	NoProductionSecrets        bool   `json:"no_production_secrets"`
	StructuralPatchCheck       bool   `json:"structural_patch_check"`
	AccountingComplete         bool   `json:"accounting_complete"`
	ResourceReportComplete     bool   `json:"resource_report_complete"`
	CleanupComplete            bool   `json:"cleanup_complete"`
	ImmutableReports           bool   `json:"immutable_reports"`
	CgroupId                   string `json:"cgroup_id"`
	TemplateDatabaseId         string `json:"template_database_id"`
	RedisGenerationId          string `json:"redis_generation_id"`
}

func (s evaluationSecurity) passedFor(evalError *CompetitionError) bool {
	buildBoundary := s.DefaultDenyNetwork && s.OfflineBuild &&
		s.OfflineBuildResourceLimits && s.ManagementCpuReserved &&
		s.ManagementMemoryReserved &&
		s.NoProductionSecrets && s.StructuralPatchCheck &&
		s.CleanupComplete && s.ImmutableReports
	if evalError != nil && evalError.Kind == "submission" && evalError.Code == "candidate_build_failed" {
		return buildBoundary
	}
	return buildBoundary && s.TemplateDatabaseReset && s.RedisReset &&
		s.CgroupContained && s.ResourceLimits && s.AccountingComplete &&
		s.ResourceReportComplete && s.CgroupId != "" &&
		s.TemplateDatabaseId != "" && s.RedisGenerationId != ""
}

type evaluationArtifact struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type artifactManifest struct {
	Schema                 int                  `json:"schema"`
	JobId                  string               `json:"job_id"`
	RoundId                string               `json:"round_id"`
	Attempt                int                  `json:"attempt"`
	EvaluatorImageDigest   string               `json:"evaluator_image_digest"`
	EvaluatorCommandSha256 string               `json:"evaluator_command_sha256"`
	RequestSha256          string               `json:"request_sha256"`
	PatchSha256            string               `json:"patch_sha256"`
	StderrSha256           string               `json:"stderr_sha256"`
	ResultSha256           string               `json:"result_sha256"`
	Security               evaluationSecurity   `json:"security"`
	Artifacts              []evaluationArtifact `json:"artifacts"`
	Retention              *artifactRetention   `json:"retention,omitempty"`
}

func (CommandEvaluator) SelfCheck(ctx context.Context, settings *Settings) (HostSelfCheck, error) {
	if err := verifyPinnedExecutable(settings.SelfCheckCommand, settings.SelfCheckCommandSha256); err != nil {
		return HostSelfCheck{}, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout := &boundedBuffer{limit: maxSelfCheckBytes}
	stderr := &boundedBuffer{limit: maxSelfCheckBytes}
	exitCode, err := runContainedCommand(checkCtx, settings.ArtifactRoot, settings.SelfCheckCommand, []string{"--json"}, stdout, stderr)
	if err != nil {
		return HostSelfCheck{}, fmt.Errorf("self-check command: %w", err)
	}
	var result HostSelfCheck
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return HostSelfCheck{}, fmt.Errorf("decode self-check: %w", err)
	}
	if exitCode != 0 {
		return result, fmt.Errorf("self-check exited %d", exitCode)
	}
	if !result.Eligible(settings) {
		return result, errors.New("evaluator host did not pass every containment and re-baseline check")
	}
	return result, nil
}

func (CommandEvaluator) Evaluate(ctx context.Context, settings *Settings, job *queuedJob) (outcome EvaluationOutcome) {
	infrastructureFailure := func(code, message string) EvaluationOutcome {
		return EvaluationOutcome{Error: infrastructureError(code, message), Infrastructure: true}
	}
	if err := verifyPinnedExecutable(settings.EvaluatorCommand, settings.EvaluatorCommandSha256); err != nil {
		return infrastructureFailure("evaluator_identity_mismatch", "pinned evaluator identity check failed")
	}
	if !storedPolicyMatches(settings, job.Round.PolicyJson) {
		return infrastructureFailure("round_policy_mismatch", "round policy does not match the frozen evaluator policy")
	}
	for _, local := range []struct {
		path     string
		expected string
	}{
		{settings.ConfigLocalDirectory, settings.EvaluationPolicy.ConfigLocalSha256},
		{settings.VaultLocalDirectory, settings.EvaluationPolicy.VaultLocalSha256},
	} {
		digest, err := hashLocalMountDirectory(local.path)
		if err != nil || digest != local.expected {
			return infrastructureFailure("local_configuration_mismatch", "frozen local configuration failed authentication")
		}
	}
	providers, err := readRoundWorkload(ctx, settings, &job.Round)
	if err != nil {
		return infrastructureFailure("round_workload_unavailable", "committed round workload failed authentication")
	}
	clear(providers)
	seed, err := revealRoundSecret(settings, &job.Round)
	if err != nil {
		return infrastructureFailure("round_seed_unavailable", "hidden round seed could not be authenticated")
	}
	attemptDir, err := createAttemptDirectory(settings.ArtifactRoot, job.JobId.String(), job.AttemptCount)
	if err != nil {
		return infrastructureFailure("artifact_create_failed", "job artifact directory could not be created")
	}
	defer func() { _ = sealArtifactDirectory(attemptDir) }()
	requestPath := filepath.Join(attemptDir, "worker-request.json")
	patchPath := filepath.Join(attemptDir, "canonical.patch")
	resultPath := filepath.Join(attemptDir, "worker-result.json")
	stderrPath := filepath.Join(attemptDir, "worker.stderr.log")
	if err := writeExclusiveFile(patchPath, job.Patch, 0400); err != nil {
		return infrastructureFailure("artifact_create_failed", "canonical patch artifact could not be written")
	}
	request := evaluatorRequestForJob(settings, job, seed, attemptDir, patchPath)
	requestBytes, err := json.Marshal(request)
	request.RoundSeedHex = ""
	seed = ""
	if err != nil || writeExclusiveFile(requestPath, append(requestBytes, '\n'), 0400) != nil {
		clear(requestBytes)
		return infrastructureFailure("artifact_create_failed", "evaluator request artifact could not be written")
	}
	clear(requestBytes)
	stderrFile, err := os.OpenFile(stderrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return infrastructureFailure("artifact_create_failed", "evaluator stderr artifact could not be created")
	}
	evalCtx, cancel := context.WithTimeout(ctx, time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second)
	stdout := &boundedBuffer{limit: maxSelfCheckBytes}
	exitCode, runErr := runContainedCommand(evalCtx, attemptDir, settings.EvaluatorCommand,
		[]string{"--request", requestPath, "--result", resultPath}, stdout, stderrFile)
	cancel()
	closeErr := stderrFile.Close()
	if runErr != nil || closeErr != nil {
		return infrastructureFailure("evaluator_process_failed", "evaluator process could not be completed and reaped")
	}
	if exitCode != 0 {
		return infrastructureFailure("evaluator_exit", fmt.Sprintf("evaluator exited with status %d", exitCode))
	}
	resultBytes, err := readRegularFile(resultPath, maxEvaluatorResultBytes)
	if err != nil {
		return infrastructureFailure("evaluator_result_missing", "evaluator result is missing or malformed")
	}
	var result evaluatorResult
	decoder := json.NewDecoder(bytes.NewReader(resultBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.Schema != 1 || result.JobId != job.JobId.String() {
		return infrastructureFailure("evaluator_result_invalid", "evaluator result identity or schema is invalid")
	}
	if (result.Score == nil) == (result.EvalError == nil) {
		return infrastructureFailure("evaluator_result_invalid", "evaluator must return exactly one of score or eval_error")
	}
	if result.Score != nil {
		if err := validateScore(result.Score); err != nil {
			return infrastructureFailure("score_result_invalid", "pinned scorer returned an invalid result")
		}
	}
	if result.EvalError != nil {
		if err := validateEvaluationError(result.EvalError); err != nil {
			return infrastructureFailure("evaluator_result_invalid", "evaluator returned an invalid typed error")
		}
	}
	if !result.Security.passedFor(result.EvalError) {
		return infrastructureFailure("containment_gate_failed", "evaluator did not prove every security gate required for its completed phase")
	}
	artifacts, err := authenticateResultArtifacts(attemptDir, result.Artifacts, result.EvalError)
	if err != nil {
		return infrastructureFailure("artifact_authentication_failed", "retained evaluator artifacts failed authentication")
	}
	resultHash := sha256.Sum256(resultBytes)
	requestHash, _, requestHashErr := hashRegularFile(requestPath)
	patchHash, _, patchHashErr := hashRegularFile(patchPath)
	stderrHash, _, stderrHashErr := hashRegularFile(stderrPath)
	if requestHashErr != nil || patchHashErr != nil || stderrHashErr != nil || patchHash != job.PatchSha256 {
		return infrastructureFailure("artifact_authentication_failed", "trusted worker artifacts failed authentication")
	}
	manifest := artifactManifest{
		Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(),
		Attempt: job.AttemptCount, EvaluatorImageDigest: settings.EvaluatorImageDigest,
		EvaluatorCommandSha256: settings.EvaluatorCommandSha256,
		RequestSha256:          requestHash, PatchSha256: patchHash, StderrSha256: stderrHash,
		ResultSha256: hex.EncodeToString(resultHash[:]), Security: result.Security,
		Artifacts: artifacts,
	}
	if _, err := json.Marshal(manifest); err != nil {
		return infrastructureFailure("artifact_manifest_failed", "artifact manifest could not be encoded")
	}
	if err := sealArtifactDirectory(attemptDir); err != nil {
		return infrastructureFailure("artifact_seal_failed", "artifact directory could not be sealed read-only")
	}
	if settings.artifactArchive == nil {
		return infrastructureFailure("artifact_archive_unavailable", "durable artifact retention is unavailable")
	}
	archivedManifest, err := settings.artifactArchive.ArchiveAttempt(
		ctx,
		settings,
		job,
		attemptDir,
		manifest,
	)
	if err != nil {
		return infrastructureFailure("artifact_archive_failed", "durable artifact retention failed")
	}
	return EvaluationOutcome{
		Score: result.Score, Error: result.EvalError,
		ArtifactManifest: archivedManifest,
		Infrastructure:   result.EvalError != nil && result.EvalError.Kind == "infrastructure",
	}
}

// Build the complete immutable handoff from the claimed queue row.
func evaluatorRequestForJob(settings *Settings, job *queuedJob, seed, attemptDir, patchPath string) evaluatorRequest {
	return evaluatorRequest{
		Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(),
		Attempt: job.AttemptCount, CompetitionId: settings.CompetitionId,
		BaseSha: settings.BaseSha, EvaluatorImageDigest: settings.EvaluatorImageDigest,
		ScorerVersion: ScorerVersion, RoundSeedHex: seed, PatchPath: patchPath,
		PatchSha256:   job.PatchSha256,
		ProvidersPath: job.Round.ProvidersPath, ProvidersSha256: job.Round.ProvidersSha256,
		ArtifactDirectory:    attemptDir,
		ConfigLocalDirectory: settings.ConfigLocalDirectory,
		VaultLocalDirectory:  settings.VaultLocalDirectory,
		PatchPolicy:          settings.PatchPolicy,
		EvaluationPolicy:     settings.EvaluationPolicy,
	}
}

// hashLocalMountDirectory authenticates the exact regular files exposed by a
// direct config/local or vault/local bind. The manifest format is one sorted
// "SHA256<two spaces>relative-path" record per file, including its newline.
// Paths with control characters and every non-regular entry are rejected so
// the host shell gate can compute the same digest without ambiguous escaping.
func hashLocalMountDirectory(root string) (string, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("local mount root is not a regular directory")
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("local mount contains a symbolic link")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("local mount contains a non-regular entry")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." || strings.ContainsAny(relative, "\r\n\x00") {
			return errors.New("local mount contains an unsafe path")
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	manifest := sha256.New()
	for _, relative := range paths {
		digest, _, err := hashRegularFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(manifest, "%s  %s\n", digest, relative); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(manifest.Sum(nil)), nil
}

func storedPolicyMatches(settings *Settings, stored json.RawMessage) bool {
	var actual roundPolicySnapshot
	if len(stored) == 0 || json.Unmarshal(stored, &actual) != nil {
		return false
	}
	expectedBytes, err := policySnapshot(settings)
	if err != nil {
		return false
	}
	actualBytes, err := json.Marshal(actual)
	return err == nil && bytes.Equal(actualBytes, expectedBytes)
}

func validateEvaluationError(evalError *CompetitionError) error {
	if evalError == nil || evalError.Code == "" || evalError.Message == "" || 1024 < len(evalError.Message) {
		return errors.New("incomplete evaluation error")
	}
	if evalError.Kind != "submission" && evalError.Kind != "infrastructure" {
		return errors.New("invalid evaluation error kind")
	}
	if evalError.Kind == "submission" && evalError.Retriable {
		return errors.New("submission errors cannot be retriable")
	}
	return nil
}

func verifyPinnedExecutable(path, expected string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return errors.New("pinned command is not a regular executable")
	}
	digest, _, err := hashRegularFile(path)
	if err != nil {
		return err
	}
	if digest != expected {
		return errors.New("pinned command SHA-256 mismatch")
	}
	return nil
}

func createAttemptDirectory(root, jobId string, attempt int) (string, error) {
	if !filepath.IsAbs(root) || root == string(filepath.Separator) {
		return "", errors.New("unsafe artifact root")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&0022 != 0 {
		return "", errors.New("artifact root must be an existing non-group/world-writable directory")
	}
	jobDir := filepath.Join(root, jobId)
	if err := os.Mkdir(jobDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	jobInfo, err := os.Lstat(jobDir)
	if err != nil || !jobInfo.IsDir() || jobInfo.Mode()&0022 != 0 {
		return "", errors.New("unsafe job artifact directory")
	}
	attemptDir := filepath.Join(jobDir, fmt.Sprintf("attempt-%02d", attempt))
	if err := os.Mkdir(attemptDir, 0700); err != nil {
		return "", err
	}
	return attemptDir, nil
}

func writeExclusiveFile(path string, value []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(value); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || limit < info.Size() {
		return nil, errors.New("file is absent, non-regular, empty, or oversized")
	}
	return os.ReadFile(path)
}

func authenticateArtifacts(root string, declared []evaluationArtifact) ([]evaluationArtifact, error) {
	return authenticateArtifactsRequired(root, declared, map[string]bool{
		"accounting.json": false, "baseline.json": false, "resources.json": false,
		"score.json": false, "evaluation.complete.json": false,
	})
}

func authenticateResultArtifacts(root string, declared []evaluationArtifact, evalError *CompetitionError) ([]evaluationArtifact, error) {
	if evalError != nil && evalError.Kind == "submission" && evalError.Code == "candidate_build_failed" {
		return authenticateArtifactsRequired(root, declared, map[string]bool{
			"submission-error.json": false, "evaluation.complete.json": false,
		})
	}
	return authenticateArtifacts(root, declared)
}

func authenticateArtifactsRequired(root string, declared []evaluationArtifact, required map[string]bool) ([]evaluationArtifact, error) {
	if len(declared) == 0 {
		return nil, errors.New("no artifacts declared")
	}
	result := append([]evaluationArtifact(nil), declared...)
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	seen := map[string]bool{}
	for i := range result {
		item := &result[i]
		if item.Path == "" || filepath.IsAbs(item.Path) || filepath.Clean(item.Path) != item.Path || strings.HasPrefix(item.Path, "..") || seen[item.Path] {
			return nil, errors.New("unsafe or duplicate artifact path")
		}
		seen[item.Path] = true
		full := filepath.Join(root, item.Path)
		rel, err := filepath.Rel(root, full)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, errors.New("artifact escapes job directory")
		}
		digest, size, err := hashRegularFile(full)
		if err != nil || digest != item.Sha256 || size != item.Bytes {
			return nil, errors.New("artifact size or hash mismatch")
		}
		if _, ok := required[item.Path]; ok {
			required[item.Path] = true
		}
	}
	for _, present := range required {
		if !present {
			return nil, errors.New("mandatory artifact missing")
		}
	}
	return result, nil
}

func hashRegularFile(path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", 0, errors.New("artifact is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	h := sha256.New()
	n, err := io.Copy(h, file)
	if err != nil || n != info.Size() {
		return "", 0, errors.New("artifact changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func sealArtifactDirectory(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
			return errors.New("artifact tree contains a non-regular entry")
		}
		if info.IsDir() {
			directories = append(directories, path)
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		err = file.Sync()
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return os.Chmod(path, 0400)
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; 0 <= i; i-- {
		if err := os.Chmod(directories[i], 0500); err != nil {
			return err
		}
	}
	return nil
}

func runContainedCommand(ctx context.Context, directory, command string, args []string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = directory
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "TZ=UTC"}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(processTermGrace)
		select {
		case waitErr = <-done:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			waitErr = <-done
		}
		return exitStatus(waitErr), ctx.Err()
	}
	// A trusted evaluator is still required to reap its own tree. Any daemon it
	// left in the process group is killed and makes this attempt fail closed.
	if err := syscall.Kill(-cmd.Process.Pid, 0); err == nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return exitStatus(waitErr), errors.New("evaluator left a descendant process running")
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			return exitError.ExitCode(), nil
		}
		return -1, waitErr
	}
	return 0, nil
}

func exitStatus(err error) int {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

type boundedBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exceeded || b.limit < b.buffer.Len()+len(value) {
		b.exceeded = true
		return 0, errors.New("command output exceeded limit")
	}
	return b.buffer.Write(value)
}

func (b *boundedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buffer.Bytes())
}
