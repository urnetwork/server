package competition

// Durable competition evidence storage. The evaluator keeps its hidden-seed
// request local, while every authenticated score artifact and the canonical
// patch are copied to a compliance-retained BlobStore version before a terminal
// result can enter PostgreSQL. Object keys bind round, job, attempt, path, and
// content hash; reads are verified by SHA-256 rather than trusted by location.

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
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

type artifactArchive interface {
	Check(context.Context) error
	ArchiveRound(context.Context, *Settings, *roundRecord, workloadArtifact) error
	ArchiveSubmission(context.Context, *Settings, server.Id, *CanonicalPatch) (*retainedArtifact, error)
	ArchiveAttempt(context.Context, *Settings, *queuedJob, string, artifactManifest) (json.RawMessage, error)
	ReadRoundWorkload(context.Context, *Settings, *roundRecord) ([]byte, error)
}

type blobArtifactArchive struct {
	store server.RetainedBlobStore
}

type retainedArtifact struct {
	Path        string    `json:"path"`
	Key         string    `json:"key"`
	Sha256      string    `json:"sha256"`
	Bytes       int64     `json:"bytes"`
	VersionId   string    `json:"version_id,omitempty"`
	Mode        string    `json:"mode"`
	RetainUntil time.Time `json:"retain_until"`
}

type artifactRetention struct {
	Backend                   string             `json:"backend"`
	Bucket                    string             `json:"bucket"`
	Prefix                    string             `json:"prefix"`
	ManifestKey               string             `json:"manifest_key"`
	RetainUntil               time.Time          `json:"retain_until"`
	ObjectCount               int                `json:"object_count"`
	Bytes                     int64              `json:"bytes"`
	HiddenSeedRequestRetained bool               `json:"hidden_seed_request_retained"`
	AuthenticatedAfterUpload  bool               `json:"authenticated_after_upload"`
	ComplianceObjectLock      bool               `json:"compliance_object_lock"`
	Objects                   []retainedArtifact `json:"objects"`
}

func loadArtifactArchive() (artifactArchive, error) {
	store, ok := server.LoadBlobStore()
	if !ok {
		return nil, errors.New("competition MinIO blob store is unavailable")
	}
	env, _ := server.Env()
	if env != "local" && strings.HasPrefix(store.Authority(), "local:") {
		return nil, errors.New("competition artifact retention requires MinIO outside local development")
	}
	retainedStore, ok := store.(server.RetainedBlobStore)
	if !ok {
		return nil, errors.New("competition blob store does not support immutable retention")
	}
	return &blobArtifactArchive{store: retainedStore}, nil
}

func (self *blobArtifactArchive) Check(ctx context.Context) error {
	if self == nil || self.store == nil {
		return errors.New("competition artifact archive is unavailable")
	}
	return self.store.CheckRetention(ctx)
}

func (self *blobArtifactArchive) ArchiveRound(
	ctx context.Context,
	settings *Settings,
	round *roundRecord,
	workload workloadArtifact,
) error {
	if settings == nil || round == nil || workload.Path == "" ||
		!sha256Pattern.MatchString(workload.Sha256) || workload.Bytes <= 0 {
		return errors.New("round workload archive identity is invalid")
	}
	key := self.roundWorkloadKey(settings, round, workload.Sha256)
	_, err := self.retainFile(
		ctx,
		settings,
		"providers.yml",
		key,
		workload.Path,
		workload.Sha256,
		workload.Bytes,
		"application/yaml",
	)
	if err == nil {
		competitionArtifactObjects.Inc()
		competitionArtifactBytes.Add(float64(workload.Bytes))
	} else {
		competitionArtifactFailures.Inc()
	}
	return err
}

// ArchiveSubmission durably retains canonical patch bytes before the queue row
// becomes claimable. Attempts retain the patch again inside their complete
// evidence bundle; this admission copy proves that even a not-yet-evaluated
// submission survives outside the control-plane database.
func (self *blobArtifactArchive) ArchiveSubmission(
	ctx context.Context,
	settings *Settings,
	roundId server.Id,
	patch *CanonicalPatch,
) (result *retainedArtifact, resultErr error) {
	defer func() {
		if resultErr != nil {
			competitionArtifactFailures.Inc()
		}
	}()
	if settings == nil || patch == nil || roundId == (server.Id{}) ||
		len(patch.Bytes) == 0 || settings.PatchPolicy.MaxPatchBytes < len(patch.Bytes) ||
		!sha256Pattern.MatchString(patch.Sha256) {
		return nil, errors.New("submission archive identity is invalid")
	}
	digest := sha256.Sum256(patch.Bytes)
	if hex.EncodeToString(digest[:]) != patch.Sha256 {
		return nil, errors.New("submission archive patch hash mismatch")
	}
	temporary, err := os.CreateTemp("", "urnetwork-competition-submission-*.patch")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return nil, err
	}
	if _, err := temporary.Write(patch.Bytes); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	result, err = self.retainFile(
		ctx,
		settings,
		"canonical.patch",
		self.submissionPatchKey(settings, roundId, patch.Sha256),
		temporaryPath,
		patch.Sha256,
		int64(len(patch.Bytes)),
		"text/x-diff",
	)
	if err != nil {
		return nil, err
	}
	competitionArtifactObjects.Inc()
	competitionArtifactBytes.Add(float64(len(patch.Bytes)))
	return result, nil
}

func (self *blobArtifactArchive) ArchiveAttempt(
	ctx context.Context,
	settings *Settings,
	job *queuedJob,
	attemptDirectory string,
	manifest artifactManifest,
) (result json.RawMessage, resultErr error) {
	defer func() {
		if resultErr != nil {
			competitionArtifactFailures.Inc()
		}
	}()
	if settings == nil || job == nil || manifest.JobId != job.JobId.String() ||
		manifest.RoundId != job.RoundId.String() || manifest.Attempt != job.AttemptCount ||
		manifest.EvaluatorImageDigest != job.EvaluatorImageDigest ||
		manifest.ApiImageDigest != job.ApiImageDigest ||
		manifest.WorkerImageDigest != job.WorkerImageDigest ||
		!imageDigestPattern.MatchString(manifest.EvaluatorImageDigest) ||
		!imageDigestPattern.MatchString(manifest.ApiImageDigest) ||
		!imageDigestPattern.MatchString(manifest.WorkerImageDigest) {
		return nil, errors.New("attempt archive identity is invalid")
	}
	artifacts := append([]evaluationArtifact(nil), manifest.Artifacts...)
	for _, item := range []struct {
		path   string
		digest string
	}{
		{path: "canonical.patch", digest: manifest.PatchSha256},
		{path: "worker-result.json", digest: manifest.ResultSha256},
		{path: "worker.stderr.log", digest: manifest.StderrSha256},
	} {
		_, size, err := hashRegularFile(filepath.Join(attemptDirectory, item.path))
		if err != nil {
			return nil, fmt.Errorf("authenticate retained %s: %w", item.path, err)
		}
		artifacts = append(artifacts, evaluationArtifact{Path: item.path, Sha256: item.digest, Bytes: size})
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })

	basePrefix := self.attemptPrefix(settings, job)
	retention := &artifactRetention{
		Backend: "server/blob", Bucket: self.store.Bucket(), Prefix: self.store.Prefix(),
		ManifestKey: filepath.ToSlash(filepath.Join(basePrefix, "artifact-manifest.json")),
		RetainUntil: settings.RetainUntil.UTC(), HiddenSeedRequestRetained: false,
		AuthenticatedAfterUpload: true,
		ComplianceObjectLock:     true,
	}
	seenPaths := map[string]bool{}
	for _, artifact := range artifacts {
		if artifact.Path == "" || filepath.IsAbs(artifact.Path) ||
			filepath.Clean(artifact.Path) != artifact.Path || strings.HasPrefix(artifact.Path, "..") ||
			seenPaths[artifact.Path] || !sha256Pattern.MatchString(artifact.Sha256) || artifact.Bytes < 0 {
			return nil, errors.New("attempt archive contains an unsafe artifact identity")
		}
		seenPaths[artifact.Path] = true
		key := filepath.ToSlash(filepath.Join(
			basePrefix,
			"objects",
			artifact.Sha256,
			artifact.Path,
		))
		retained, err := self.retainFile(
			ctx,
			settings,
			artifact.Path,
			key,
			filepath.Join(attemptDirectory, filepath.FromSlash(artifact.Path)),
			artifact.Sha256,
			artifact.Bytes,
			artifactContentType(artifact.Path),
		)
		if err != nil {
			return nil, err
		}
		retention.Objects = append(retention.Objects, *retained)
		retention.Bytes += retained.Bytes
		retention.ComplianceObjectLock = retention.ComplianceObjectLock && retained.Mode == "COMPLIANCE"
	}
	retention.ObjectCount = len(retention.Objects)
	competitionArtifactObjects.Add(float64(retention.ObjectCount))
	competitionArtifactBytes.Add(float64(retention.Bytes))
	manifest.Retention = retention
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	temporary, err := os.CreateTemp("", "urnetwork-competition-artifact-manifest-*.json")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return nil, err
	}
	if _, err := temporary.Write(manifestBytes); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	if _, err := self.retainFile(
		ctx,
		settings,
		"artifact-manifest.json",
		retention.ManifestKey,
		temporaryPath,
		hex.EncodeToString(manifestDigest[:]),
		int64(len(manifestBytes)),
		"application/json",
	); err != nil {
		return nil, err
	}
	return manifestBytes, nil
}

func (self *blobArtifactArchive) ReadRoundWorkload(
	ctx context.Context,
	settings *Settings,
	round *roundRecord,
) ([]byte, error) {
	if settings == nil || round == nil || !sha256Pattern.MatchString(round.ProvidersSha256) {
		return nil, errors.New("round workload archive identity is invalid")
	}
	reader, err := self.store.Get(ctx, self.roundWorkloadKey(settings, round, round.ProvidersSha256))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	bytes, err := io.ReadAll(io.LimitReader(reader, maxProvidersFileSize+1))
	if err != nil || len(bytes) == 0 || maxProvidersFileSize < int64(len(bytes)) {
		clear(bytes)
		return nil, errors.New("archived round workload is empty, oversized, or unreadable")
	}
	digest := sha256.Sum256(bytes)
	if hex.EncodeToString(digest[:]) != round.ProvidersSha256 {
		clear(bytes)
		return nil, errors.New("archived round workload hash mismatch")
	}
	return bytes, nil
}

func (self *blobArtifactArchive) retainFile(
	ctx context.Context,
	settings *Settings,
	logicalPath string,
	key string,
	localPath string,
	expectedSha256 string,
	expectedBytes int64,
	contentType string,
) (*retainedArtifact, error) {
	retention, err := self.store.PutRetained(ctx, key, localPath, contentType, settings.RetainUntil)
	if err != nil {
		return nil, fmt.Errorf("retain competition artifact %s: %w", logicalPath, err)
	}
	if retention == nil || retention.Key != key || retention.Size != expectedBytes ||
		retention.RetainUntil.Before(settings.RetainUntil.Add(-time.Second)) ||
		retention.Mode != "COMPLIANCE" && retention.Mode != "LOCAL" {
		return nil, fmt.Errorf("retention proof for %s is invalid", logicalPath)
	}
	reader, err := self.store.GetVersion(ctx, key, retention.VersionId)
	if err != nil {
		return nil, fmt.Errorf("read retained competition artifact %s: %w", logicalPath, err)
	}
	hash := sha256.New()
	readBytes, readErr := io.Copy(hash, io.LimitReader(reader, expectedBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || readBytes != expectedBytes ||
		hex.EncodeToString(hash.Sum(nil)) != expectedSha256 {
		return nil, fmt.Errorf("retained competition artifact %s failed authentication", logicalPath)
	}
	return &retainedArtifact{
		Path: logicalPath, Key: key, Sha256: expectedSha256, Bytes: expectedBytes,
		VersionId: retention.VersionId, Mode: retention.Mode,
		RetainUntil: retention.RetainUntil.UTC(),
	}, nil
}

func (self *blobArtifactArchive) roundWorkloadKey(settings *Settings, round *roundRecord, digest string) string {
	return filepath.ToSlash(filepath.Join(
		self.store.Prefix(),
		"competition",
		"v1",
		settings.CompetitionId,
		"rounds",
		round.RoundId.String(),
		"workloads",
		"sha256",
		digest+".providers.yml",
	))
}

func (self *blobArtifactArchive) submissionPatchKey(settings *Settings, roundId server.Id, digest string) string {
	return filepath.ToSlash(filepath.Join(
		self.store.Prefix(),
		"competition",
		"v1",
		settings.CompetitionId,
		"rounds",
		roundId.String(),
		"submissions",
		"sha256",
		digest,
		"canonical.patch",
	))
}

func (self *blobArtifactArchive) attemptPrefix(settings *Settings, job *queuedJob) string {
	return filepath.ToSlash(filepath.Join(
		self.store.Prefix(),
		"competition",
		"v1",
		settings.CompetitionId,
		"rounds",
		job.RoundId.String(),
		"jobs",
		job.JobId.String(),
		fmt.Sprintf("attempt-%02d", job.AttemptCount),
	))
}

func artifactContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return "application/json"
	case ".patch", ".diff":
		return "text/x-diff"
	case ".yml", ".yaml":
		return "application/yaml"
	default:
		return "application/octet-stream"
	}
}
