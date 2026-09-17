// Authenticates sanitized post-run diagnostics independently of completed scores.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxFailureManifestBytes = 8 * 1024 * 1024
	maxFailureArtifacts     = 100000
	maxFailureEvidenceBytes = int64(4 * 1024 * 1024 * 1024)
	failureArchiveTimeout   = 2 * time.Minute
)

// Requires every sanitizer identity field and an explicit byte count per entry.
type failedEvidenceManifest struct {
	Schema    int    `json:"schema"`
	Kind      string `json:"kind"`
	JobId     string `json:"job_id"`
	RoundId   string `json:"round_id"`
	Attempt   int    `json:"attempt"`
	Sanitized bool   `json:"sanitized"`
	Artifacts []struct {
		Path   string `json:"path"`
		Sha256 string `json:"sha256"`
		Bytes  *int64 `json:"bytes"`
	} `json:"artifacts"`
}

// Carries only the narrowly classified candidate run failure, never score gates.
type evaluatorStageFailure struct {
	Schema   int    `json:"schema"`
	Kind     string `json:"kind"`
	JobId    string `json:"job_id"`
	RoundId  string `json:"round_id"`
	Attempt  int    `json:"attempt"`
	Role     string `json:"role"`
	Stage    string `json:"stage"`
	ExitCode int    `json:"exit_code"`
	Error    *struct {
		Kind      string `json:"kind"`
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retriable *bool  `json:"retriable"`
	} `json:"error"`
}

// Rejects unknown, repeated, and trailing fields before accepting any identity.
func decodeFailureEvidenceJson(value []byte, result any) error {
	if !utf8.Valid(value) {
		return errors.New("failure evidence is not valid utf-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	var checkValue func(int) error
	checkValue = func(depth int) error {
		if 64 < depth {
			return errors.New("failure evidence nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seenKeys := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || key != strings.ToLower(key) || seenKeys[key] {
					return errors.New("failure evidence contains a duplicate field")
				}
				seenKeys[key] = true
				if err := checkValue(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := checkValue(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("failure evidence contains an unexpected delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := checkValue(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("failure evidence contains trailing data")
	}
	decoder = json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	return decoder.Decode(result)
}

// Restricts sanitizer entries to diagnostics, excluding hidden inputs and credentials.
func safeFailureEvidencePath(value string) bool {
	if !strings.HasPrefix(value, "failed-evidence/") || strings.Contains(value, "\\") ||
		filepath.IsAbs(value) || filepath.ToSlash(filepath.Clean(value)) != value ||
		strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	for _, component := range strings.Split(strings.TrimPrefix(value, "failed-evidence/"), "/") {
		name := strings.ToLower(component)
		if name == "" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "evaluation-sources.") ||
			strings.HasSuffix(name, ".env") || strings.Contains(name, ".env.") {
			return false
		}
		switch name {
		case "input", "scorer-input", "score-runtime", "runtime", "config", "vault", "secrets",
			"worker-request.json", "providers.yml", "providers.yaml", "round-seed", "round_seed",
			"round-seed.hex", "round_seed.hex", "round_seed_hex", "seed.hex":
			return false
		}
	}
	return true
}

// Opens each component relative to an already-open directory; no link can redirect it.
func openFailureArtifactPath(root, relative string, directory bool) (*os.File, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root ||
		filepath.IsAbs(relative) || relative != "" && filepath.Clean(relative) != relative ||
		strings.HasPrefix(relative, "..") || !directory && relative == "" {
		return nil, errors.New("failure artifact path is invalid")
	}
	fullPath := filepath.Join(root, relative)
	components := strings.Split(strings.TrimPrefix(fullPath, string(filepath.Separator)), string(filepath.Separator))
	directoryFd, err := syscall.Open(string(filepath.Separator), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			syscall.Close(directoryFd)
			return nil, errors.New("failure artifact path component is invalid")
		}
		flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
		if index+1 < len(components) || directory {
			flags |= syscall.O_DIRECTORY
		} else {
			flags |= syscall.O_NONBLOCK
		}
		nextFd, err := syscall.Openat(directoryFd, component, flags, 0)
		syscall.Close(directoryFd)
		if err != nil {
			return nil, err
		}
		directoryFd = nextFd
	}
	file := os.NewFile(uintptr(directoryFd), fullPath)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !directory {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
			file.Close()
			return nil, errors.New("failure artifact is not a singly linked regular file")
		}
	}
	return file, nil
}

// Reads bounded control records and authenticates the exact bytes decoded by callers.
func readFailureArtifact(root, relative string, limit int64) ([]byte, evaluationArtifact, error) {
	file, err := openFailureArtifactPath(root, relative, false)
	if err != nil {
		return nil, evaluationArtifact{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || limit < info.Size() {
		return nil, evaluationArtifact{}, errors.New("failure control record is empty or oversized")
	}
	if (relative == "failed-evidence-manifest.json" || relative == "evaluator-failure.json") && info.Mode().Perm() != 0o400 {
		return nil, evaluationArtifact{}, errors.New("failure control record is not sealed read-only")
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(value)) != info.Size() || limit < int64(len(value)) {
		return nil, evaluationArtifact{}, errors.New("failure control record changed while reading")
	}
	digest := sha256.Sum256(value)
	return value, evaluationArtifact{Path: relative, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(value))}, nil
}

// Bounds hashing by the declared size and checks links before any upload can start.
func authenticateFailureArtifact(root string, artifact evaluationArtifact) error {
	if !sha256Pattern.MatchString(artifact.Sha256) || artifact.Bytes < 0 || maxFailureEvidenceBytes < artifact.Bytes {
		return errors.New("failure artifact size or digest is invalid")
	}
	file, err := openFailureArtifactPath(root, artifact.Path, false)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != artifact.Bytes {
		return errors.New("failure artifact size mismatch")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, artifact.Bytes+1))
	if err != nil || size != artifact.Bytes || hex.EncodeToString(hash.Sum(nil)) != artifact.Sha256 {
		return errors.New("failure artifact digest mismatch")
	}
	return nil
}

// A missing manifest permits only minimal worker diagnostics; a malformed one fails closed.
func authenticateFailedEvidence(root string, job *queuedJob) ([]evaluationArtifact, bool, error) {
	value, manifestArtifact, err := readFailureArtifact(root, "failed-evidence-manifest.json", maxFailureManifestBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var manifest failedEvidenceManifest
	if err := decodeFailureEvidenceJson(value, &manifest); err != nil {
		return nil, true, err
	}
	if manifest.Schema != 1 || manifest.Kind != "sim-latency-failed-evidence-manifest" ||
		manifest.JobId != job.JobId.String() || manifest.RoundId != job.RoundId.String() ||
		manifest.Attempt != job.AttemptCount || !manifest.Sanitized ||
		len(manifest.Artifacts) == 0 || maxFailureArtifacts < len(manifest.Artifacts) {
		return nil, true, errors.New("failure evidence manifest identity is invalid")
	}
	artifacts := make([]evaluationArtifact, 0, len(manifest.Artifacts)+1)
	seenPaths := map[string]bool{}
	var totalBytes int64
	for _, declared := range manifest.Artifacts {
		if !safeFailureEvidencePath(declared.Path) || seenPaths[declared.Path] || declared.Bytes == nil ||
			*declared.Bytes < 0 || maxFailureEvidenceBytes-totalBytes < *declared.Bytes ||
			!sha256Pattern.MatchString(declared.Sha256) {
			return nil, true, errors.New("failure evidence manifest contains an unsafe artifact")
		}
		seenPaths[declared.Path] = true
		totalBytes += *declared.Bytes
		artifacts = append(artifacts, evaluationArtifact{Path: declared.Path, Sha256: declared.Sha256, Bytes: *declared.Bytes})
	}
	if !seenPaths["failed-evidence/failure.json"] {
		return nil, true, errors.New("failure evidence manifest omits the sanitizer failure record")
	}
	for _, artifact := range artifacts {
		if err := authenticateFailureArtifact(root, artifact); err != nil {
			return nil, true, err
		}
	}
	artifacts = append(artifacts, manifestArtifact)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return artifacts, true, nil
}

// A sidecar can classify only one authenticated candidate run failure shape.
func authenticateEvaluatorStageFailure(root string, job *queuedJob) (*CompetitionError, *evaluationArtifact, error) {
	value, artifact, err := readFailureArtifact(root, "evaluator-failure.json", maxSelfCheckBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var failure evaluatorStageFailure
	if err := decodeFailureEvidenceJson(value, &failure); err != nil {
		return nil, nil, err
	}
	if failure.Schema != 1 || failure.Kind != "sim-latency-stage-failure" ||
		failure.JobId != job.JobId.String() || failure.RoundId != job.RoundId.String() ||
		failure.Attempt != job.AttemptCount || failure.Role != "candidate" || failure.Stage != "run" ||
		failure.ExitCode < 1 || 255 < failure.ExitCode || failure.Error == nil ||
		failure.Error.Kind != "submission" || failure.Error.Code != "run_process_failed" ||
		failure.Error.Message != "candidate runner exited unsuccessfully" ||
		failure.Error.Retriable == nil || *failure.Error.Retriable {
		return nil, nil, errors.New("candidate stage failure identity is invalid")
	}
	return &CompetitionError{
		Kind: failure.Error.Kind, Code: failure.Error.Code, Message: failure.Error.Message, Retriable: false,
	}, &artifact, nil
}

// A simultaneous command exit and cancellation must retain infrastructure attribution.
func candidateStageFailureEligible(evaluationCanceled bool, exitCode int, runErr, closeErr error) bool {
	return !evaluationCanceled && 1 <= exitCode && exitCode <= 255 && runErr == nil && closeErr == nil
}

// Creates the minimal diagnostic through the trusted worker's original directory descriptor.
func writeControllerFailureArtifact(root string, job *queuedJob, failure *CompetitionError) (evaluationArtifact, error) {
	value, err := json.Marshal(struct {
		Schema  int               `json:"schema"`
		Kind    string            `json:"kind"`
		JobId   string            `json:"job_id"`
		RoundId string            `json:"round_id"`
		Attempt int               `json:"attempt"`
		Failure *CompetitionError `json:"failure"`
	}{
		Schema: 1, Kind: "sim-latency-controller-failure", JobId: job.JobId.String(),
		RoundId: job.RoundId.String(), Attempt: job.AttemptCount, Failure: failure,
	})
	if err != nil {
		return evaluationArtifact{}, err
	}
	directory, err := openFailureArtifactPath(root, "", true)
	if err != nil {
		return evaluationArtifact{}, err
	}
	defer directory.Close()
	// A partially failed whole-tree seal may already have made this directory read-only.
	if err := directory.Chmod(0o700); err != nil {
		return evaluationArtifact{}, err
	}
	fd, err := syscall.Openat(int(directory.Fd()), "controller-failure.json",
		syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o400)
	if err != nil {
		return evaluationArtifact{}, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(root, "controller-failure.json"))
	defer file.Close()
	if _, err := file.Write(value); err != nil {
		return evaluationArtifact{}, err
	}
	if err := file.Sync(); err != nil {
		return evaluationArtifact{}, err
	}
	if err := file.Close(); err != nil {
		return evaluationArtifact{}, err
	}
	digest := sha256.Sum256(value)
	return evaluationArtifact{Path: "controller-failure.json", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(value))}, nil
}

// Authenticates and seals every selected failure object before any remote write.
func authenticateFailureArchiveArtifacts(root string, artifacts []evaluationArtifact) error {
	if maxFailureArtifacts+5 < len(artifacts) {
		return errors.New("failure archive exceeds its artifact count limit")
	}
	var totalBytes int64
	seenPaths := map[string]bool{}
	for _, artifact := range artifacts {
		allowed := safeFailureEvidencePath(artifact.Path)
		switch artifact.Path {
		case "canonical.patch", "worker.stderr.log", "controller-failure.json", "failed-evidence-manifest.json", "evaluator-failure.json":
			allowed = true
		}
		if !allowed || seenPaths[artifact.Path] || artifact.Bytes < 0 || maxFailureEvidenceBytes-totalBytes < artifact.Bytes {
			return errors.New("failure archive contains an unsafe artifact")
		}
		seenPaths[artifact.Path] = true
		totalBytes += artifact.Bytes
	}
	for _, artifact := range artifacts {
		if err := authenticateFailureArtifact(root, artifact); err != nil {
			return err
		}
		file, err := openFailureArtifactPath(root, artifact.Path, false)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		modeErr := file.Chmod(0o400)
		closeErr := file.Close()
		if err := errors.Join(syncErr, modeErr, closeErr); err != nil {
			return err
		}
	}
	return nil
}

// Runs only after command cleanup; cancellation cannot discard otherwise safe evidence.
func archiveFailedEvaluation(ctx context.Context, settings *Settings, job *queuedJob, directory, requestSha256 string, originalFailure *CompetitionError, allowStageFailure bool) EvaluationOutcome {
	archiveFailure := func() EvaluationOutcome {
		return EvaluationOutcome{Error: infrastructureError("artifact_archive_failed", "durable failure evidence retention failed")}
	}
	artifacts, sanitized, err := authenticateFailedEvidence(directory, job)
	if err != nil {
		return archiveFailure()
	}
	failure := originalFailure
	if sanitized {
		stageFailure, stageArtifact, err := authenticateEvaluatorStageFailure(directory, job)
		if err != nil {
			return archiveFailure()
		}
		if stageArtifact != nil {
			artifacts = append(artifacts, *stageArtifact)
			if allowStageFailure {
				failure = stageFailure
			}
		}
	}
	diagnostic, err := writeControllerFailureArtifact(directory, job, originalFailure)
	if err != nil {
		return archiveFailure()
	}
	artifacts = append(artifacts, diagnostic)
	patch := evaluationArtifact{Path: "canonical.patch", Sha256: job.PatchSha256, Bytes: int64(len(job.Patch))}
	stderrFile, err := openFailureArtifactPath(directory, "worker.stderr.log", false)
	if err != nil {
		return archiveFailure()
	}
	stderrInfo, err := stderrFile.Stat()
	if err != nil || stderrInfo.Size() < 0 || maxFailureEvidenceBytes < stderrInfo.Size() {
		stderrFile.Close()
		return archiveFailure()
	}
	stderrHash := sha256.New()
	stderrBytes, readErr := io.Copy(stderrHash, io.LimitReader(stderrFile, stderrInfo.Size()+1))
	closeErr := stderrFile.Close()
	if readErr != nil || closeErr != nil || stderrBytes != stderrInfo.Size() {
		return archiveFailure()
	}
	stderr := evaluationArtifact{Path: "worker.stderr.log", Sha256: hex.EncodeToString(stderrHash.Sum(nil)), Bytes: stderrBytes}
	selected := append(append([]evaluationArtifact(nil), artifacts...), patch, stderr)
	if err := authenticateFailureArchiveArtifacts(directory, selected); err != nil {
		return archiveFailure()
	}
	manifest := artifactManifest{
		Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(),
		SourceEpoch: evaluationSourceEpoch(&job.Round), Attempt: job.AttemptCount,
		EvaluatorImageDigest: job.EvaluatorImageDigest, ApiImageDigest: job.ApiImageDigest,
		WorkerImageDigest: job.WorkerImageDigest, EvaluatorCommandSha256: settings.EvaluatorCommandSha256,
		RequestSha256: requestSha256, PatchSha256: patch.Sha256, StderrSha256: stderr.Sha256,
		Artifacts: artifacts, Failure: failure,
	}
	if settings.artifactArchive == nil {
		return archiveFailure()
	}
	archiveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureArchiveTimeout)
	defer cancel()
	archivedManifest, err := settings.artifactArchive.ArchiveAttempt(archiveCtx, settings, job, directory, manifest)
	if err != nil {
		return archiveFailure()
	}
	return EvaluationOutcome{Error: failure, ArtifactManifest: archivedManifest}
}
