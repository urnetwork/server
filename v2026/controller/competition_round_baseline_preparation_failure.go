// Preparation failures retain only known, bounded native diagnostics under
// their own hold namespace. They never become candidate completions/attempts.
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
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

// A durable event can cite a retained private hold document without publishing
// hidden artifacts, stdout, credentials or untrusted error messages in SQL.
type roundBaselinePreparationFailure struct {
	Cause          error
	ManifestSha256 string
}

// The original infrastructure/cancellation attribution remains inspectable.
func (self *roundBaselinePreparationFailure) Error() string {
	return "round baseline preparation infrastructure hold; reconcile original retained evidence before proceeding"
}

// Cancellation/lease errors are not transformed into submission failures.
func (self *roundBaselinePreparationFailure) Unwrap() error { return self.Cause }

// Drains the trusted process's stderr while retaining only its owner's bounded
// prefix. The separate summary reports any truncation; raw run logs are intact.
type roundBaselinePreparationLogWriter struct {
	Writer                    io.Writer
	Limit, Observed, Retained int64
}

// The contained command serializes writes to its stderr stream.
func (self *roundBaselinePreparationLogWriter) Write(content []byte) (int, error) {
	count := len(content)
	self.Observed += int64(count)
	remaining := self.Limit - self.Retained
	if remaining <= 0 {
		return count, nil
	}
	if int64(len(content)) > remaining {
		content = content[:remaining]
	}
	written, err := self.Writer.Write(content)
	self.Retained += int64(written)
	if err != nil {
		return written, err
	}
	if written != len(content) {
		return written, io.ErrShortWrite
	}
	return count, nil
}

// Every selected object is sealed and authenticated before the first upload.
// No walk of a source checkout, live mount, secret input or arbitrary log occurs.
func authenticateRoundBaselinePreparationArchiveArtifacts(directory string, settings *Settings, preparation *roundBaselinePreparation, artifacts []evaluationArtifact) error {
	policy := settings.roundBaselinePreparation.Settings
	if len(artifacts) == 0 || len(artifacts) > policy.MaximumArtifacts+6 {
		return errors.New("baseline archive exceeds its bounded inventory")
	}
	seen := map[string]bool{}
	var total int64
	for _, artifact := range artifacts {
		allowed := roundBaselinePreparationArtifactAllowed(artifact.Path, preparation)
		if strings.HasPrefix(artifact.Path, "failed-evidence/") {
			allowed = roundBaselinePreparationArtifactAllowed("evidence/"+strings.TrimPrefix(artifact.Path, "failed-evidence/"), preparation) || artifact.Path == "failed-evidence/failure.json"
		}
		switch artifact.Path {
		case "baseline.json", "evidence-manifest.json", "failed-evidence-manifest.json", "round-baseline-preparation.complete.json", "round-baseline-preparation.hold.json", "baseline-preparation.stderr.log", "baseline-preparation.stderr-summary.json":
			allowed = true
		}
		if !allowed || seen[artifact.Path] || artifact.Bytes < 0 || artifact.Bytes > policy.MaximumRawFileBytes || artifact.Bytes > policy.MaximumEvidenceBytes-total {
			return errors.New("baseline archive contains an unsafe, duplicated or oversized object")
		}
		seen[artifact.Path] = true
		total += artifact.Bytes
		if err := authenticateFailureArtifact(directory, artifact); err != nil {
			return err
		}
		file, err := openFailureArtifactPath(directory, artifact.Path, false)
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil || info.Mode().Perm() != 0o400 {
			return errors.New("baseline archive object is not sealed")
		}
	}
	return nil
}

// A failed v3 prefix has already joined its normal sanitizer/cleanup trap.
// The bound manifest is read, never discovered through an unbounded tree walk.
func collectFailedRoundBaselinePreparationEvidence(directory string, settings *Settings, preparation *roundBaselinePreparation) ([]evaluationArtifact, error) {
	policy := settings.roundBaselinePreparation.Settings
	var artifacts, declared []evaluationArtifact
	content, manifestArtifact, err := readRoundBaselinePreparationArtifact(directory, "failed-evidence-manifest.json", policy.MaximumManifestBytes)
	if err == nil {
		var manifest failedEvidenceManifest
		if err := decodeCompetitionPreparationJson(bytes.NewReader(content), policy.MaximumManifestBytes, &manifest); err != nil {
			return nil, err
		}
		if manifest.Schema != 1 || manifest.Kind != "sim-latency-failed-evidence-manifest" || manifest.JobId != preparation.JobId.String() || manifest.RoundId != preparation.RoundId.String() || manifest.Attempt != 1 || !manifest.Sanitized {
			return nil, errors.New("baseline failure sanitizer identity mismatch")
		}
		if len(manifest.Artifacts) > policy.MaximumArtifacts {
			return nil, errors.New("baseline failure inventory exceeds its bound")
		}
		for _, artifact := range manifest.Artifacts {
			if artifact.Bytes == nil {
				return nil, errors.New("baseline failure omitted its byte bound")
			}
			declared = append(declared, evaluationArtifact{Path: artifact.Path, Sha256: artifact.Sha256, Bytes: *artifact.Bytes})
		}
		artifacts = append(artifacts, manifestArtifact)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		content, manifestArtifact, err = readRoundBaselinePreparationArtifact(directory, "evidence-manifest.json", policy.MaximumManifestBytes)
		if err == nil {
			var manifest competitionQualificationEvidence
			if err := decodeCompetitionPreparationJson(bytes.NewReader(content), policy.MaximumManifestBytes, &manifest); err != nil {
				return nil, err
			}
			if manifest.Schema != 1 || manifest.Kind != "sim-latency-evidence-manifest" || manifest.JobId != preparation.JobId.String() || manifest.RoundId != preparation.RoundId.String() {
				return nil, errors.New("baseline completed evidence identity mismatch")
			}
			declared = manifest.Artifacts
			artifacts = append(artifacts, manifestArtifact)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if len(declared) > policy.MaximumArtifacts {
		return nil, errors.New("baseline failure inventory exceeds its bound")
	}
	seen := map[string]bool{}
	var total int64
	for _, artifact := range declared {
		if !sha256Pattern.MatchString(artifact.Sha256) || artifact.Bytes < 0 || artifact.Bytes > policy.MaximumRawFileBytes || artifact.Bytes > policy.MaximumEvidenceBytes-total || seen[artifact.Path] || filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path || strings.HasPrefix(artifact.Path, "..") || strings.Contains(artifact.Path, "\\") || strings.IndexFunc(artifact.Path, unicode.IsControl) >= 0 {
			return nil, errors.New("baseline failure inventory is unsafe or oversized")
		}
		seen[artifact.Path] = true
		total += artifact.Bytes
		allowed := roundBaselinePreparationArtifactAllowed(artifact.Path, preparation)
		if strings.HasPrefix(artifact.Path, "failed-evidence/") {
			allowed = roundBaselinePreparationArtifactAllowed("evidence/"+strings.TrimPrefix(artifact.Path, "failed-evidence/"), preparation) || artifact.Path == "failed-evidence/failure.json"
		}
		if allowed {
			artifacts = append(artifacts, artifact)
		}
	}
	for _, relative := range []string{"baseline.json", "round-baseline-preparation.complete.json", "baseline-preparation.stderr.log", "baseline-preparation.stderr-summary.json"} {
		file, err := openFailureArtifactPath(directory, relative, false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, statErr := file.Stat()
		if statErr != nil || info.Size() < 0 || info.Size() > policy.MaximumRawFileBytes {
			file.Close()
			return nil, errors.New("baseline failure diagnostic exceeds its bound")
		}
		if err := file.Chmod(0o400); err != nil {
			file.Close()
			return nil, err
		}
		hash := sha256.New()
		count, readErr := io.Copy(hash, io.LimitReader(file, info.Size()+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || count != info.Size() {
			return nil, errors.New("baseline failure diagnostic changed")
		}
		artifacts = append(artifacts, evaluationArtifact{Path: relative, Sha256: hex.EncodeToString(hash.Sum(nil)), Bytes: count})
	}
	return artifacts, nil
}

// The fixed hold record is written through the original directory descriptor,
// including after an earlier successful seal made the directory read-only.
func writeRoundBaselinePreparationHold(directory string, preparation *roundBaselinePreparation) (evaluationArtifact, error) {
	content, err := json.Marshal(struct {
		Schema            int    `json:"schema"`
		Kind              string `json:"kind"`
		Owner             string `json:"owner"`
		ContextSha256     string `json:"context_sha256"`
		JobId             string `json:"job_id"`
		RoundId           string `json:"round_id"`
		SubmissionStarted bool   `json:"submission_started"`
		CandidateExecuted bool   `json:"candidate_executed"`
	}{Schema: 1, Kind: "sim-latency-round-baseline-preparation-infrastructure-hold", Owner: preparation.Owner, ContextSha256: preparation.ContextSha256, JobId: preparation.JobId.String(), RoundId: preparation.RoundId.String()})
	if err != nil {
		return evaluationArtifact{}, err
	}
	root, err := openFailureArtifactPath(directory, "", true)
	if err != nil {
		return evaluationArtifact{}, err
	}
	defer root.Close()
	if err := root.Chmod(0o700); err != nil {
		return evaluationArtifact{}, err
	}
	fd, err := unix.Openat(int(root.Fd()), "round-baseline-preparation.hold.json", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o400)
	if err != nil {
		return evaluationArtifact{}, err
	}
	file := os.NewFile(uintptr(fd), "round-baseline-preparation.hold.json")
	_, writeErr := file.Write(content)
	syncErr, closeErr := file.Sync(), file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr, root.Sync()); err != nil {
		return evaluationArtifact{}, err
	}
	digest := sha256.Sum256(content)
	return evaluationArtifact{Path: "round-baseline-preparation.hold.json", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}, nil
}

// Cleanup-only persistence never re-enters the producer, changes a candidate
// attempt, or borrows another round's controls, even when the archive is down.
func retainFailedRoundBaselinePreparation(ctx context.Context, settings *Settings, preparation *roundBaselinePreparation, directory string, archive roundBaselinePreparationArchive) (json.RawMessage, error) {
	artifacts, err := collectFailedRoundBaselinePreparationEvidence(directory, settings, preparation)
	if err != nil {
		return nil, err
	}
	hold, err := writeRoundBaselinePreparationHold(directory, preparation)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, hold)
	if err := authenticateRoundBaselinePreparationArchiveArtifacts(directory, settings, preparation, artifacts); err != nil {
		return nil, err
	}
	document := roundBaselinePreparationArchiveDocument{Schema: 1, Kind: "sim-latency-round-baseline-preparation-artifacts", Outcome: "infrastructure_hold", Owner: preparation.Owner, ContextSha256: preparation.ContextSha256, JobId: preparation.JobId.String(), RoundId: preparation.RoundId.String(), BaseSha: settings.BaseSha, ImageId: settings.EvaluatorImageDigest, ProvidersSha256: preparation.SourceJob.Round.ProvidersSha256, WorkerImage: preparation.WorkerImage, Artifacts: artifacts}
	manifest, err := archive.ArchiveRoundBaselinePreparation(ctx, settings, preparation, directory, document)
	if err != nil {
		return nil, err
	}
	var retained roundBaselinePreparationArchiveDocument
	if err := decodeCompetitionPreparationJson(bytes.NewReader(manifest), settings.roundBaselinePreparation.Settings.MaximumRecordBytes, &retained); err != nil {
		return nil, err
	}
	if retained.Outcome != "infrastructure_hold" || retained.Owner != preparation.Owner || retained.ContextSha256 != preparation.ContextSha256 || retained.Retention == nil || !retained.Retention.AuthenticatedAfterUpload || !retained.Retention.ComplianceObjectLock || !strings.HasSuffix(retained.Retention.ManifestKey, "/"+preparation.Owner+"/baseline-preparation-hold-manifest.json") {
		return nil, errors.New("baseline failure archive did not authenticate its owned hold")
	}
	return manifest, nil
}
