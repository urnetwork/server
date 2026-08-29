package controller

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
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

type Principal struct {
	Id   string
	Role string
}

func Authenticate(request *http.Request, settings *Settings) (*Principal, bool) {
	const prefix = "Bearer "
	header := request.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) || strings.Contains(header[len(prefix):], " ") {
		return nil, false
	}
	raw := header[len(prefix):]
	if raw == "" || len(raw) > 1024 {
		return nil, false
	}
	digest := sha256.Sum256([]byte(raw))
	var matched *Token
	for i := range settings.Tokens {
		expected, err := hex.DecodeString(settings.Tokens[i].Sha256)
		if err != nil || len(expected) != sha256.Size {
			continue
		}
		if subtle.ConstantTimeCompare(digest[:], expected) == 1 {
			matched = &settings.Tokens[i]
		}
	}
	if matched == nil {
		return nil, false
	}
	return &Principal{Id: matched.Name, Role: matched.Role}, true
}

const (
	ResourceName                       = "competition.yml"
	evaluationStageOverheadSeconds     = int64(600)
	submissionEvaluationTimeoutSeconds = 3 * 60 * 60
)

var (
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	gitShaPattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	tokenNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type Settings struct {
	Enabled                    bool             `yaml:"enabled"`
	CompetitionId              string           `yaml:"competition_id"`
	BaseSha                    string           `yaml:"base_sha"`
	EvaluatorImageDigest       string           `yaml:"evaluator_image_digest"`
	PatchPolicy                PatchPolicy      `yaml:"patch_policy"`
	EvaluationPolicy           EvaluationPolicy `yaml:"evaluation_policy"`
	SeasonPolicy               SeasonPolicy     `yaml:"season_policy"`
	ArtifactRoot               string           `yaml:"artifact_root"`
	ConfigLocalDirectory       string           `yaml:"config_local_directory"`
	VaultLocalDirectory        string           `yaml:"vault_local_directory"`
	SeasonEndsAt               time.Time        `yaml:"season_ends_at"`
	RetainUntil                time.Time        `yaml:"retain_until"`
	WorkerLeaseSeconds         int              `yaml:"worker_lease_seconds"`
	WorkerHeartbeatSeconds     int              `yaml:"worker_heartbeat_seconds"`
	HostHeartbeatMaxAgeSeconds int              `yaml:"host_heartbeat_max_age_seconds"`
	MaxInfrastructureAttempts  int              `yaml:"max_infrastructure_attempts"`
	SimulatorCommand           string           `yaml:"simulator_command"`
	EvaluatorCommand           string           `yaml:"evaluator_command"`
	EvaluatorCommandSha256     string           `yaml:"evaluator_command_sha256"`
	SelfCheckCommand           string           `yaml:"self_check_command"`
	SelfCheckCommandSha256     string           `yaml:"self_check_command_sha256"`
	Tokens                     []Token          `yaml:"-"`
	SeedKey                    []byte           `yaml:"-"`
	workloadGenerator          WorkloadGenerator
	artifactArchive            artifactArchive
}

type settingsFile struct {
	Enabled                    bool             `yaml:"enabled"`
	CompetitionId              string           `yaml:"competition_id"`
	BaseSha                    string           `yaml:"base_sha"`
	EvaluatorImageDigest       string           `yaml:"evaluator_image_digest"`
	PatchPolicy                PatchPolicy      `yaml:"patch_policy"`
	EvaluationPolicy           EvaluationPolicy `yaml:"evaluation_policy"`
	SeasonPolicy               SeasonPolicy     `yaml:"season_policy"`
	ArtifactRoot               string           `yaml:"artifact_root"`
	ConfigLocalDirectory       string           `yaml:"config_local_directory"`
	VaultLocalDirectory        string           `yaml:"vault_local_directory"`
	SeasonEndsAt               time.Time        `yaml:"season_ends_at"`
	RetainUntil                time.Time        `yaml:"retain_until"`
	WorkerLeaseSeconds         int              `yaml:"worker_lease_seconds"`
	WorkerHeartbeatSeconds     int              `yaml:"worker_heartbeat_seconds"`
	HostHeartbeatMaxAgeSeconds int              `yaml:"host_heartbeat_max_age_seconds"`
	MaxInfrastructureAttempts  int              `yaml:"max_infrastructure_attempts"`
	SimulatorCommand           string           `yaml:"simulator_command"`
	EvaluatorCommand           string           `yaml:"evaluator_command"`
	EvaluatorCommandSha256     string           `yaml:"evaluator_command_sha256"`
	SelfCheckCommand           string           `yaml:"self_check_command"`
	SelfCheckCommandSha256     string           `yaml:"self_check_command_sha256"`
}

type secretsFile struct {
	SeedKeyBase64 string  `yaml:"seed_key_base64"`
	Tokens        []Token `yaml:"tokens"`
}

type Token struct {
	Name   string `yaml:"name"`
	Role   string `yaml:"role"`
	Sha256 string `yaml:"sha256"`
}

func LoadSettings() (*Settings, error) {
	ordinary, err := server.Config.SimpleResource(ResourceName)
	if err != nil {
		return nil, fmt.Errorf("config %s unavailable: %w", ResourceName, err)
	}
	var public settingsFile
	if err := safeUnmarshal(ordinary, &public); err != nil {
		return nil, fmt.Errorf("config %s: %w", ResourceName, err)
	}
	secretResource, err := server.Vault.SimpleResource(ResourceName)
	if err != nil {
		return nil, fmt.Errorf("vault %s unavailable: %w", ResourceName, err)
	}
	var secret secretsFile
	if err := safeUnmarshal(secretResource, &secret); err != nil {
		return nil, fmt.Errorf("vault %s: %w", ResourceName, err)
	}
	seedKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(secret.SeedKeyBase64))
	if err != nil {
		return nil, fmt.Errorf("seed_key_base64: %w", err)
	}
	s := &Settings{
		Enabled:                    public.Enabled,
		CompetitionId:              strings.TrimSpace(public.CompetitionId),
		BaseSha:                    strings.TrimSpace(public.BaseSha),
		EvaluatorImageDigest:       strings.TrimSpace(public.EvaluatorImageDigest),
		PatchPolicy:                public.PatchPolicy,
		EvaluationPolicy:           public.EvaluationPolicy,
		SeasonPolicy:               public.SeasonPolicy,
		ArtifactRoot:               filepath.Clean(public.ArtifactRoot),
		ConfigLocalDirectory:       filepath.Clean(public.ConfigLocalDirectory),
		VaultLocalDirectory:        filepath.Clean(public.VaultLocalDirectory),
		SeasonEndsAt:               public.SeasonEndsAt.UTC(),
		RetainUntil:                public.RetainUntil.UTC(),
		WorkerLeaseSeconds:         public.WorkerLeaseSeconds,
		WorkerHeartbeatSeconds:     public.WorkerHeartbeatSeconds,
		HostHeartbeatMaxAgeSeconds: public.HostHeartbeatMaxAgeSeconds,
		MaxInfrastructureAttempts:  public.MaxInfrastructureAttempts,
		SimulatorCommand:           filepath.Clean(public.SimulatorCommand),
		EvaluatorCommand:           filepath.Clean(public.EvaluatorCommand),
		EvaluatorCommandSha256:     strings.TrimSpace(public.EvaluatorCommandSha256),
		SelfCheckCommand:           filepath.Clean(public.SelfCheckCommand),
		SelfCheckCommandSha256:     strings.TrimSpace(public.SelfCheckCommandSha256),
		Tokens:                     secret.Tokens,
		SeedKey:                    seedKey,
	}
	s.artifactArchive, err = loadArtifactArchive()
	if err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

func safeUnmarshal(resource *server.SimpleResource, value any) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%v", recovered)
		}
	}()
	resource.UnmarshalYaml(value)
	return nil
}

func (self *Settings) Validate() error {
	if self == nil {
		return errors.New("competition settings are nil")
	}
	if !self.Enabled {
		return errors.New("competition is disabled")
	}
	if self.CompetitionId == "" || len(self.CompetitionId) > 128 {
		return errors.New("competition_id must contain 1..128 characters")
	}
	if !gitShaPattern.MatchString(self.BaseSha) {
		return errors.New("base_sha must be a lowercase 40-character Git SHA")
	}
	if !imageDigestPattern.MatchString(self.EvaluatorImageDigest) {
		return errors.New("evaluator_image_digest must be a pinned sha256 digest")
	}
	if self.PatchPolicy.MaxPatchBytes <= 0 || 262144 < self.PatchPolicy.MaxPatchBytes {
		return errors.New("patch_policy.max_patch_bytes must be in 1..262144")
	}
	if len(self.PatchPolicy.AllowedPaths) == 0 || len(self.PatchPolicy.ForbiddenPaths) == 0 {
		return errors.New("patch allowlist and hard-forbidden list must both be nonempty")
	}
	if err := validatePatterns(self.PatchPolicy.AllowedPaths, "allowed_paths"); err != nil {
		return err
	}
	if err := validateLiteralAllowedPaths(self.PatchPolicy.AllowedPaths); err != nil {
		return err
	}
	if err := validatePatterns(self.PatchPolicy.ForbiddenPaths, "forbidden_paths"); err != nil {
		return err
	}
	if !slices.Contains(self.PatchPolicy.ForbiddenPaths, protectedSimulatorTreePattern) {
		return fmt.Errorf("patch_policy.forbidden_paths must explicitly contain %q", protectedSimulatorTreePattern)
	}
	p := self.EvaluationPolicy
	season := self.SeasonPolicy
	if season.EpochCount != 6 || season.SubmissionWindowSeconds != 7*24*60*60 ||
		season.PreparationWindowSeconds < 0 || 7*24*60*60 < season.PreparationWindowSeconds ||
		season.SubmissionFeeUsd != 20 {
		return errors.New("season_policy must freeze six seven-day submission epochs, a $20 USD submission fee, and a preparation window of at most seven days")
	}
	if p.HardwareId == "" ||
		!sha256Pattern.MatchString(p.HostQualificationSha256) ||
		!sha256Pattern.MatchString(p.ConfigLocalSha256) ||
		!sha256Pattern.MatchString(p.VaultLocalSha256) ||
		!sha256Pattern.MatchString(p.SimulatorSha256) ||
		!sha256Pattern.MatchString(p.ScorerSha256) ||
		p.DurationMs <= 0 || p.RequestTimeoutMs <= 0 {
		return errors.New("evaluation policy is not frozen")
	}
	if p.ProviderCount <= 0 || p.ClientPoolSize <= 0 || p.ArrivalsPerMinute <= 0 ||
		p.QualityWindowSize < 0 || 32 < p.QualityWindowSize ||
		p.ExchangeHosts <= 0 || 32 < p.ExchangeHosts || p.FleetShards < 0 || 32 < p.FleetShards {
		return errors.New("evaluation scale is outside its supported bounds")
	}
	host, port, err := net.SplitHostPort(p.SiteListen)
	if err != nil || port == "" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || p.ApiPort <= 0 || 65535 < p.ApiPort {
		return errors.New("site_listen and api_port must be valid loopback endpoints")
	}
	if p.RampMs < 0 || p.PrewarmMs < 0 || p.SettleMs < 0 ||
		p.ClientWarmupTimeoutMs <= 0 || 24*time.Hour.Milliseconds() < p.ClientWarmupTimeoutMs ||
		p.PipelineIntervalMs <= 0 || p.TestTimeoutMs <= 0 || p.AnnounceTimeoutMs <= 0 {
		return errors.New("evaluation timing policy is invalid")
	}
	if 24*time.Hour.Milliseconds() < p.DurationMs || p.DurationMs < p.RequestTimeoutMs {
		return errors.New("duration must be at most 24h and at least the request timeout")
	}
	if p.Replicates <= 0 || 9 < p.Replicates || p.Replicates%2 == 0 {
		return errors.New("evaluation_policy.replicates must be an odd number in 1..9")
	}
	if math.IsNaN(p.TakeoverMargin) || math.IsInf(p.TakeoverMargin, 0) || p.TakeoverMargin <= 0 || .5 < p.TakeoverMargin {
		return errors.New("evaluation_policy.takeover_margin must be finite and in (0, 0.5]")
	}
	if p.QueueLimit != 0 || p.ScoreTimeoutSeconds != submissionEvaluationTimeoutSeconds {
		return errors.New("queue_limit must be zero for unbounded epoch admission and score_timeout_seconds must equal the frozen three-hour submission limit")
	}
	if len(self.SeedKey) != 32 {
		return errors.New("seed_key_base64 must decode to exactly 32 bytes")
	}
	if !filepath.IsAbs(self.ArtifactRoot) || self.ArtifactRoot == string(filepath.Separator) || self.ArtifactRoot == "." {
		return errors.New("artifact_root must be an absolute non-root path")
	}
	if err := validateLocalMountDirectory(self.ConfigLocalDirectory, "config"); err != nil {
		return err
	}
	if err := validateLocalMountDirectory(self.VaultLocalDirectory, "vault"); err != nil {
		return err
	}
	if self.SeasonEndsAt.IsZero() || self.RetainUntil.Before(self.SeasonEndsAt) {
		return errors.New("retain_until must be at or after season_ends_at")
	}
	if self.WorkerLeaseSeconds < 30 || self.WorkerHeartbeatSeconds <= 0 || self.WorkerLeaseSeconds <= 2*self.WorkerHeartbeatSeconds {
		return errors.New("worker lease must be at least 30s and more than twice its heartbeat")
	}
	if self.HostHeartbeatMaxAgeSeconds < 2*self.WorkerHeartbeatSeconds {
		return errors.New("host heartbeat max age must cover at least two worker heartbeats")
	}
	if self.MaxInfrastructureAttempts <= 0 || 10 < self.MaxInfrastructureAttempts {
		return errors.New("max_infrastructure_attempts must be in 1..10")
	}
	if err := validatePinnedCommand(self.SimulatorCommand, p.SimulatorSha256, "simulator"); err != nil {
		return err
	}
	if err := validatePinnedCommand(self.EvaluatorCommand, self.EvaluatorCommandSha256, "evaluator"); err != nil {
		return err
	}
	if err := validatePinnedCommand(self.SelfCheckCommand, self.SelfCheckCommandSha256, "self-check"); err != nil {
		return err
	}
	roles := map[string]bool{}
	names := map[string]bool{}
	for _, token := range self.Tokens {
		if !tokenNamePattern.MatchString(token.Name) || names[token.Name] {
			return errors.New("competition token names must be nonempty and unique")
		}
		if token.Role != "submitter" && token.Role != "operator" {
			return fmt.Errorf("competition token %q has invalid role", token.Name)
		}
		if !sha256Pattern.MatchString(token.Sha256) {
			return fmt.Errorf("competition token %q has an invalid SHA-256", token.Name)
		}
		names[token.Name] = true
		roles[token.Role] = true
	}
	if !roles["submitter"] || !roles["operator"] {
		return errors.New("at least one submitter token and one operator token are required")
	}
	return nil
}

// Includes the frozen per-stage startup/cleanup allowance used by the trusted
// evaluator's outer TERM/KILL boundary.
func evaluationStageTimeoutSeconds(p EvaluationPolicy) int64 {
	phaseMs := p.RampMs + p.SettleMs + p.ClientWarmupTimeoutMs + p.DurationMs + p.RequestTimeoutMs
	return (phaseMs+999)/1000 + evaluationStageOverheadSeconds
}

func validateLocalMountDirectory(path, parent string) error {
	if !filepath.IsAbs(path) || filepath.Base(path) != "local" || filepath.Base(filepath.Dir(path)) != parent {
		return fmt.Errorf("%s_local_directory must be an absolute %s/local path", parent, parent)
	}
	if strings.ContainsAny(path, "\r\n\x00") {
		return fmt.Errorf("%s_local_directory contains unsafe characters", parent)
	}
	return nil
}

func validatePatterns(patterns []string, label string) error {
	seen := map[string]bool{}
	for _, pattern := range patterns {
		if pattern == "" || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "\\") || strings.Contains(pattern, "..") {
			return fmt.Errorf("%s contains unsafe pattern %q", label, pattern)
		}
		if _, err := filepath.Match(pattern, "probe"); err != nil {
			return fmt.Errorf("%s contains invalid pattern %q: %w", label, pattern, err)
		}
		if seen[pattern] {
			return fmt.Errorf("%s contains duplicate pattern %q", label, pattern)
		}
		seen[pattern] = true
	}
	return nil
}

func validateLiteralAllowedPaths(paths []string) error {
	for _, value := range paths {
		if strings.ContainsAny(value, `*?[\\`) {
			return fmt.Errorf("allowed_paths must contain literal repository files, not pattern %q", value)
		}
		if err := validateRepositoryPath(value); err != nil {
			return fmt.Errorf("allowed_paths contains unsafe file %q: %w", value, err)
		}
		if filepath.Ext(value) != ".go" {
			return fmt.Errorf("allowed_paths contains non-Go file %q", value)
		}
		if matchAny(hardForbiddenPatchPaths, value) {
			return fmt.Errorf("allowed_paths contains protected file %q", value)
		}
	}
	return nil
}

func validatePinnedCommand(path, digest, label string) error {
	if !filepath.IsAbs(path) || path == string(filepath.Separator) {
		return fmt.Errorf("%s command must be an absolute non-root path", label)
	}
	if !sha256Pattern.MatchString(digest) {
		return fmt.Errorf("%s command SHA-256 is not pinned", label)
	}
	return nil
}

func (self *Settings) PublicInfo() InfoResult {
	return InfoResult{
		CompetitionId:        self.CompetitionId,
		Enabled:              self.Enabled,
		ScoreSchema:          ScoreSchema,
		ScorerVersion:        ScorerVersion,
		BaseSha:              self.BaseSha,
		EvaluatorImageDigest: self.EvaluatorImageDigest,
		PatchPolicy:          self.PatchPolicy,
		EvaluationPolicy:     self.EvaluationPolicy,
		SeasonPolicy:         self.SeasonPolicy,
	}
}

func (self *Settings) TokenNames() []string {
	names := make([]string, 0, len(self.Tokens))
	for _, token := range self.Tokens {
		names = append(names, token.Name)
	}
	slices.Sort(names)
	return names
}

const roundCommitmentDomain = "urnetwork-sim-latency-round-v1\x00"

func createRoundSecret(settings *Settings, roundId server.Id) (nonce, ciphertext []byte, commitment string, err error) {
	seed := make([]byte, 32)
	if _, err = rand.Read(seed); err != nil {
		return nil, nil, "", err
	}
	block, err := aes.NewCipher(settings.SeedKey)
	if err != nil {
		return nil, nil, "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, "", err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, "", err
	}
	aad := roundAAD(settings, roundId)
	ciphertext = gcm.Seal(nil, nonce, seed, aad)
	h := sha256.New()
	h.Write([]byte(roundCommitmentDomain))
	h.Write(roundId.Bytes())
	h.Write(seed)
	commitment = hex.EncodeToString(h.Sum(nil))
	clear(seed)
	return nonce, ciphertext, commitment, nil
}

func revealRoundSecret(settings *Settings, round *roundRecord) (string, error) {
	block, err := aes.NewCipher(settings.SeedKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(round.SeedNonce) != gcm.NonceSize() {
		return "", fmt.Errorf("round nonce has invalid length")
	}
	seed, err := gcm.Open(nil, round.SeedNonce, round.SeedCiphertext, roundAAD(settings, round.RoundId))
	if err != nil {
		return "", fmt.Errorf("decrypt round seed: %w", err)
	}
	defer clear(seed)
	if len(seed) != 32 {
		return "", fmt.Errorf("round seed has invalid length")
	}
	h := sha256.New()
	h.Write([]byte(roundCommitmentDomain))
	h.Write(round.RoundId.Bytes())
	h.Write(seed)
	if hex.EncodeToString(h.Sum(nil)) != round.WorkloadCommitment {
		return "", fmt.Errorf("round seed does not match commitment")
	}
	return hex.EncodeToString(seed), nil
}

func roundAAD(settings *Settings, roundId server.Id) []byte {
	return []byte(roundCommitmentDomain + settings.CompetitionId + "\x00" + roundId.String() + "\x00" + settings.BaseSha)
}

const runtimeImageDigestEnvironment = "WARP_IMAGE_DIGEST"

func runtimeImageDigest() (string, error) {
	return validateRuntimeImageDigest(os.Getenv(runtimeImageDigestEnvironment))
}

func validateRuntimeImageDigest(value string) (string, error) {
	imageDigest := strings.TrimSpace(value)
	if !imageDigestPattern.MatchString(imageDigest) {
		return "", errors.New("runtime image digest must be an exact sha256 content identity")
	}
	return imageDigest, nil
}

const (
	maxSelfCheckBytes       = 1 * 1024 * 1024
	maxEvaluatorResultBytes = 8 * 1024 * 1024
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
	SourceEpoch          int              `json:"source_epoch"`
	Attempt              int              `json:"attempt"`
	CompetitionId        string           `json:"competition_id"`
	BaseSha              string           `json:"base_sha"`
	EvaluatorImageDigest string           `json:"evaluator_image_digest"`
	ApiImageDigest       string           `json:"api_image_digest"`
	WorkerImageDigest    string           `json:"worker_image_digest"`
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

func (self evaluationSecurity) passedFor(evalError *CompetitionError) bool {
	buildBoundary := self.DefaultDenyNetwork && self.OfflineBuild &&
		self.OfflineBuildResourceLimits && self.ManagementCpuReserved &&
		self.ManagementMemoryReserved &&
		self.NoProductionSecrets && self.StructuralPatchCheck &&
		self.CleanupComplete && self.ImmutableReports
	if evalError != nil && evalError.Kind == "submission" && evalError.Code == "candidate_build_failed" {
		return buildBoundary
	}
	return buildBoundary && self.TemplateDatabaseReset && self.RedisReset &&
		self.CgroupContained && self.ResourceLimits && self.AccountingComplete &&
		self.ResourceReportComplete && self.CgroupId != "" &&
		self.TemplateDatabaseId != "" && self.RedisGenerationId != ""
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
	SourceEpoch            int                  `json:"source_epoch"`
	Attempt                int                  `json:"attempt"`
	EvaluatorImageDigest   string               `json:"evaluator_image_digest"`
	ApiImageDigest         string               `json:"api_image_digest"`
	WorkerImageDigest      string               `json:"worker_image_digest"`
	EvaluatorCommandSha256 string               `json:"evaluator_command_sha256"`
	RequestSha256          string               `json:"request_sha256"`
	PatchSha256            string               `json:"patch_sha256"`
	StderrSha256           string               `json:"stderr_sha256"`
	ResultSha256           string               `json:"result_sha256"`
	Security               evaluationSecurity   `json:"security"`
	Artifacts              []evaluationArtifact `json:"artifacts"`
	Retention              *artifactRetention   `json:"retention,omitempty"`
}

func (self CommandEvaluator) SelfCheck(ctx context.Context, settings *Settings) (HostSelfCheck, error) {
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

func (self CommandEvaluator) Evaluate(ctx context.Context, settings *Settings, job *queuedJob) (outcome EvaluationOutcome) {
	infrastructureFailure := func(code, message string) EvaluationOutcome {
		return EvaluationOutcome{Error: infrastructureError(code, message), Infrastructure: true}
	}
	if err := verifyPinnedExecutable(settings.EvaluatorCommand, settings.EvaluatorCommandSha256); err != nil {
		return infrastructureFailure("evaluator_identity_mismatch", "pinned evaluator identity check failed")
	}
	if !storedPolicyMatches(settings, job.Round.PolicyJson) {
		return infrastructureFailure("round_policy_mismatch", "round policy does not match the frozen evaluator policy")
	}
	if job.Round.Epoch < 1 || settings.SeasonPolicy.EpochCount < job.Round.Epoch {
		return infrastructureFailure("source_epoch_invalid", "round does not map to a configured measured-source epoch")
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
	stopProgressMetrics := startEvaluationProgressMetrics(
		ctx,
		filepath.Join(attemptDir, evaluationProgressFileName),
		job.JobId.String(),
		job.RoundId.String(),
		settings.EvaluationPolicy.Replicates,
	)
	defer stopProgressMetrics()
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
		SourceEpoch: job.Round.Epoch - 1,
		Attempt:     job.AttemptCount, EvaluatorImageDigest: job.EvaluatorImageDigest,
		ApiImageDigest: job.ApiImageDigest, WorkerImageDigest: job.WorkerImageDigest,
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
		SourceEpoch: job.Round.Epoch - 1,
		Attempt:     job.AttemptCount, CompetitionId: settings.CompetitionId,
		BaseSha: settings.BaseSha, EvaluatorImageDigest: job.EvaluatorImageDigest,
		ApiImageDigest: job.ApiImageDigest, WorkerImageDigest: job.WorkerImageDigest,
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

func (self *boundedBuffer) Write(value []byte) (int, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.exceeded || self.limit < self.buffer.Len()+len(value) {
		self.exceeded = true
		return 0, errors.New("command output exceeded limit")
	}
	return self.buffer.Write(value)
}

func (self *boundedBuffer) Bytes() []byte {
	self.mu.Lock()
	defer self.mu.Unlock()
	return bytes.Clone(self.buffer.Bytes())
}

const hardRequestBodyLimit = 2 * 1024 * 1024

func HealthHandler(w http.ResponseWriter, r *http.Request) {
	writeCompetitionJson(w, http.StatusOK, DefaultService().Health())
}

func ReadyHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requirePrincipal(w, r, service, true)
	if !ok || principal == nil {
		return
	}
	result, evalError := service.Ready(r.Context())
	if evalError != nil {
		writeCompetitionJson(w, http.StatusServiceUnavailable, evalError)
		return
	}
	writeCompetitionJson(w, http.StatusOK, result)
}

func InfoHandler(w http.ResponseWriter, r *http.Request) {
	result, evalError := DefaultService().Info(r.Context())
	if evalError != nil {
		writeCompetitionJson(w, http.StatusServiceUnavailable, evalError)
		return
	}
	writeCompetitionJson(w, http.StatusOK, result)
}

func LeaderboardHandler(w http.ResponseWriter, r *http.Request) {
	result, evalError := DefaultService().Leaderboards(r.Context())
	if evalError != nil {
		writeCompetitionJson(w, http.StatusServiceUnavailable, evalError)
		return
	}
	writeCompetitionJson(w, http.StatusOK, result)
}

func GetRoundWorkloadHandler(w http.ResponseWriter, r *http.Request) {
	values := router.GetPathValues(r)
	if len(values) != 1 {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_round_id", "round id is missing"))
		return
	}
	roundId, err := server.ParseId(values[0])
	if err != nil {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_round_id", "round id is malformed"))
		return
	}
	providers, digest, status, evalError := DefaultService().GetRoundWorkload(r.Context(), roundId)
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	defer clear(providers)
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="providers.yml"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(providers)))
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("X-Content-SHA256", digest)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(providers)
}

func GenerateRoundHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	if _, ok := requirePrincipal(w, r, service, true); !ok {
		return
	}
	var args GenerateRoundArgs
	if evalError := decodeCompetitionBody(w, r, &args, 16*1024); evalError != nil {
		writeCompetitionJson(w, http.StatusBadRequest, evalError)
		return
	}
	result, evalError := service.GenerateRound(r.Context(), args)
	if evalError != nil {
		status := http.StatusBadRequest
		if evalError.Code == "round_overlap" {
			status = http.StatusConflict
		} else if evalError.Kind == "infrastructure" {
			status = http.StatusServiceUnavailable
		}
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, http.StatusCreated, result)
}

func SubmitScoreHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requirePrincipal(w, r, service, false)
	if !ok {
		return
	}
	var args ScoreArgs
	limit := int64(hardRequestBodyLimit)
	if service.settings != nil && service.settings.PatchPolicy.MaxPatchBytes*6+4096 < hardRequestBodyLimit {

		limit = int64(service.settings.PatchPolicy.MaxPatchBytes*6 + 4096)
	}
	if evalError := decodeCompetitionBody(w, r, &args, limit); evalError != nil {
		status := http.StatusBadRequest
		if evalError.Code == "request_too_large" {
			status = http.StatusRequestEntityTooLarge
		}
		writeCompetitionJson(w, status, evalError)
		return
	}
	result, status, evalError := service.Submit(r.Context(), args, principal)
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, status, result)
}

func GetScoreHandler(w http.ResponseWriter, r *http.Request) {
	service := DefaultService()
	principal, ok := requirePrincipal(w, r, service, false)
	if !ok {
		return
	}
	values := router.GetPathValues(r)
	if len(values) != 1 {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_job_id", "job id is missing"))
		return
	}
	jobId, err := server.ParseId(values[0])
	if err != nil {
		writeCompetitionJson(w, http.StatusBadRequest, submissionError("invalid_job_id", "job id is malformed"))
		return
	}
	result, status, evalError := service.GetScore(r.Context(), jobId, principal)
	if evalError != nil {
		writeCompetitionJson(w, status, evalError)
		return
	}
	writeCompetitionJson(w, status, result)
}

func requirePrincipal(w http.ResponseWriter, r *http.Request, service *Service, operator bool) (*Principal, bool) {
	settings, err := service.Settings()
	if err != nil {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("configuration_unavailable", "competition configuration is not ready"))
		return nil, false
	}
	principal, ok := Authenticate(r, settings)
	if !ok || operator && principal.Role != "operator" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="competition"`)
		writeCompetitionJson(w, http.StatusUnauthorized, &CompetitionError{
			Kind: "auth", Code: "unauthorized", Message: "missing or invalid competition bearer token", Retriable: false,
		})
		return nil, false
	}
	return principal, true
}

func decodeCompetitionBody(w http.ResponseWriter, r *http.Request, value any, limit int64) *CompetitionError {
	if contentType := r.Header.Get("Content-Type"); contentType != "" && !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return submissionError("invalid_content_type", "Content-Type must be application/json")
	}
	if r.Body == nil {
		return submissionError("invalid_json", "request body is required")
	}
	reader := http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(reader)
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return submissionError("request_too_large", "request body exceeds the published limit")
		}
		return submissionError("invalid_json", "request body could not be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return submissionError("invalid_json", "request body is not valid for this endpoint")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return submissionError("invalid_json", "request body must contain one JSON value")
	}
	return nil
}

func writeCompetitionJson(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(value); err != nil {
		return
	}
}

const (
	evaluationProgressFileName  = "evaluation-progress.json"
	maxEvaluationProgressBytes  = 256 * 1024
	evaluationProgressPollEvery = time.Second
)

var competitionLiveEvaluationMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork", Subsystem: "competition", Name: "live_evaluation_metric_value",
	Help: "Completed baseline and candidate replicate p50/p95 values for the current internal evaluation.",
}, []string{"job_id", "round_id", "role", "replicate", "metric", "quantile", "significance"})

func init() {
	prometheus.MustRegister(competitionLiveEvaluationMetric)
}

type evaluationProgress struct {
	Schema             int                        `json:"schema"`
	Kind               string                     `json:"kind"`
	JobId              string                     `json:"job_id"`
	RoundId            string                     `json:"round_id"`
	Phase              string                     `json:"phase"`
	ReplicateCount     int                        `json:"replicate_count"`
	BaselineCompleted  int                        `json:"baseline_completed"`
	CandidateCompleted int                        `json:"candidate_completed"`
	UpdatedUnixMs      int64                      `json:"updated_unix_ms"`
	Metrics            []evaluationProgressMetric `json:"metrics"`
}

type evaluationProgressMetric struct {
	Role         string   `json:"role"`
	Replicate    int      `json:"replicate"`
	Metric       string   `json:"metric"`
	Quantile     string   `json:"quantile"`
	Value        float64  `json:"value"`
	PImprovement *float64 `json:"p_improvement"`
	PRegression  *float64 `json:"p_regression"`
	Significance string   `json:"significance"`
}

var evaluationProgressMetrics = map[string]string{
	"ttfb_p50_ms":                "p50",
	"ttfb_p95_ms":                "p95",
	"throughput_p50_bytes_per_s": "p50",
	"throughput_p95_bytes_per_s": "p95",
}

// startEvaluationProgressMetrics watches the trusted evaluator's atomically
// replaced progress document. It is deliberately internal telemetry: the
// public score API continues to reveal results only after epoch finalization.
func startEvaluationProgressMetrics(
	ctx context.Context,
	path string,
	jobId string,
	roundId string,
	replicateCount int,
) func() {
	competitionLiveEvaluationMetric.Reset()
	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	lastUpdate := int64(0)
	lastError := ""
	refresh := func() {
		progress, err := readEvaluationProgress(path, jobId, roundId, replicateCount)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && err.Error() != lastError {
				glog.Infof("[competition]live evaluation progress ignored: %s\n", err)
			}
			lastError = err.Error()
			return
		}
		lastError = ""
		if progress.UpdatedUnixMs <= lastUpdate {
			return
		}
		applyEvaluationProgress(progress)
		lastUpdate = progress.UpdatedUnixMs
	}
	refresh()
	go func() {
		defer close(done)
		ticker := time.NewTicker(evaluationProgressPollEvery)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
	return func() {
		cancel()
		<-done

		refresh()
	}
}

func readEvaluationProgress(
	path string,
	jobId string,
	roundId string,
	replicateCount int,
) (*evaluationProgress, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || maxEvaluationProgressBytes < info.Size() {
		return nil, errors.New("evaluation progress is empty, oversized, or non-regular")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxEvaluationProgressBytes+1))
	decoder.DisallowUnknownFields()
	progress := &evaluationProgress{}
	if err := decoder.Decode(progress); err != nil {
		return nil, fmt.Errorf("decode evaluation progress: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("evaluation progress has trailing content")
	}
	if err := validateEvaluationProgress(progress, jobId, roundId, replicateCount); err != nil {
		return nil, err
	}
	return progress, nil
}

func validateEvaluationProgress(
	progress *evaluationProgress,
	jobId string,
	roundId string,
	replicateCount int,
) error {
	if progress == nil || progress.Schema != 1 ||
		progress.Kind != "sim-latency-evaluation-progress" ||
		progress.JobId != jobId || progress.RoundId != roundId ||
		progress.ReplicateCount != replicateCount ||
		replicateCount < 1 || 9 < replicateCount || replicateCount%2 == 0 ||
		progress.UpdatedUnixMs <= 0 {
		return errors.New("evaluation progress identity is invalid")
	}
	validPhase := map[string]bool{
		"preparing": true, "building": true, "baseline": true,
		"candidate": true, "scoring": true, "complete": true, "failed": true,
	}
	if !validPhase[progress.Phase] || progress.BaselineCompleted < 0 ||
		replicateCount < progress.BaselineCompleted || progress.CandidateCompleted < 0 ||
		replicateCount < progress.CandidateCompleted {
		return errors.New("evaluation progress phase or counts are invalid")
	}
	seen := map[string]bool{}
	roleCounts := map[string]int{"baseline": 0, "candidate": 0}
	for _, metric := range progress.Metrics {
		quantile, ok := evaluationProgressMetrics[metric.Metric]
		if !ok || metric.Quantile != quantile ||
			(metric.Role != "baseline" && metric.Role != "candidate") ||
			metric.Replicate < 1 || replicateCount < metric.Replicate ||
			math.IsNaN(metric.Value) || math.IsInf(metric.Value, 0) || metric.Value < 0 {
			return errors.New("evaluation progress metric identity or value is invalid")
		}
		completed := progress.BaselineCompleted
		if metric.Role == "candidate" {
			completed = progress.CandidateCompleted
		}
		if completed < metric.Replicate {
			return errors.New("evaluation progress metric exceeds its completed count")
		}
		key := fmt.Sprintf("%s/%d/%s", metric.Role, metric.Replicate, metric.Metric)
		if seen[key] {
			return errors.New("evaluation progress contains a duplicate metric")
		}
		seen[key] = true
		roleCounts[metric.Role]++
		if !validProbability(metric.PImprovement) || !validProbability(metric.PRegression) {
			return errors.New("evaluation progress p-value is invalid")
		}
		if metric.Role == "baseline" {
			if metric.Significance != "baseline" || metric.PImprovement != nil || metric.PRegression != nil {
				return errors.New("baseline progress claims candidate significance")
			}
			continue
		}
		switch metric.Significance {
		case "not_testable":
			if metric.PImprovement != nil || metric.PRegression != nil {
				return errors.New("untestable progress includes a p-value")
			}
		case "not_significant":
			if metric.PImprovement == nil || metric.PRegression == nil ||
				*metric.PImprovement <= 0.05 || *metric.PRegression <= 0.05 {
				return errors.New("nonsignificant progress contradicts its p-values")
			}
		case "improved":
			if metric.PImprovement == nil || 0.05 < *metric.PImprovement {
				return errors.New("improved progress lacks statistical support")
			}
		case "regressed":
			if metric.PRegression == nil || 0.05 < *metric.PRegression {
				return errors.New("regressed progress lacks statistical support")
			}
		default:
			return errors.New("evaluation progress significance is invalid")
		}
	}
	for role, completed := range map[string]int{
		"baseline":  progress.BaselineCompleted,
		"candidate": progress.CandidateCompleted,
	} {
		if roleCounts[role] != completed*len(evaluationProgressMetrics) {
			return errors.New("evaluation progress does not contain every completed metric")
		}
	}
	return nil
}

func validProbability(value *float64) bool {
	return value == nil || !math.IsNaN(*value) && !math.IsInf(*value, 0) && 0 <= *value && *value <= 1
}

func applyEvaluationProgress(progress *evaluationProgress) {
	competitionLiveEvaluationMetric.Reset()
	for _, metric := range progress.Metrics {
		competitionLiveEvaluationMetric.WithLabelValues(
			progress.JobId,
			progress.RoundId,
			metric.Role,
			strconv.Itoa(metric.Replicate),
			metric.Metric,
			metric.Quantile,
			metric.Significance,
		).Set(metric.Value)
	}
}

const runnerHeartbeatInterval = 15 * time.Second

var (
	competitionConfigured = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "configured",
		Help: "1 when the process loaded a valid enabled competition configuration.",
	})
	competitionJobs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "jobs",
		Help: "Durable competition jobs by state.",
	}, []string{"state"})
	competitionOldestJobAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "oldest_job_age_seconds",
		Help: "Age in seconds of the oldest competition job in each state.",
	}, []string{"state"})
	competitionWorkerHeartbeatAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "worker_heartbeat_age_seconds",
		Help: "Age of the singleton worker-slot heartbeat, or -1 when absent.",
	})
	competitionRunnerHeartbeatTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "runner_heartbeat_timestamp_seconds",
		Help: "Unix timestamp of the latest heartbeat emitted by the sim-latency runner process.",
	})
	competitionSubmissionQueueSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "submission_queue_size",
		Help: "Number of submissions waiting in the durable FIFO queue.",
	})
	competitionCurrentEvaluationInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "current_evaluation_info",
		Help: "Identity of the submission currently being evaluated; the value is always one.",
	}, []string{"job_id", "round_id", "patch_sha256", "attempt"})
	competitionCurrentEvaluationElapsed = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "current_evaluation_elapsed_seconds",
		Help: "Elapsed wall time of the submission currently being evaluated, or zero when idle.",
	})
	competitionSignificantSubmissionFound = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "significant_submission_found",
		Help: "1 when any completed submission in the latest epoch is statistically significant.",
	})
	competitionEvaluationDurationEstimate = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "evaluation_duration_estimate_seconds",
		Help: "Estimated duration of one submission from the recent p75, falling back to the evaluation time limit.",
	})
	competitionSubmissionBacklogEstimate = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "submission_backlog_estimated_seconds",
		Help: "Estimated wall time until the running submission and durable FIFO queue are drained.",
	})
	competitionCurrentEpoch = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "current_epoch",
		Help: "Latest durable epoch number, or zero before the first epoch.",
	})
	competitionRoundPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "round_phase",
		Help: "One-hot phase of the latest competition epoch.",
	}, []string{"phase"})
	competitionArtifactArchiveReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "artifact_archive_ready",
		Help: "1 when the retained blob backend proves versioning and object-lock readiness.",
	})
	competitionMetricRefreshErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "metric_refresh_errors_total",
		Help: "Operational metric refresh failures.",
	})
	competitionSubmissions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "submissions_total",
		Help: "Score submission requests by bounded outcome.",
	}, []string{"outcome"})
	competitionEvaluations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "evaluations_total",
		Help: "Evaluator attempts by terminal or retry outcome.",
	}, []string{"outcome"})
	competitionEvaluationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "evaluation_seconds",
		Help:    "Wall time of one evaluator attempt.",
		Buckets: []float64{60, 300, 900, 3600, 7200, 14400, 28800, 57600},
	})
	competitionRoundEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "round_events_total",
		Help: "Epoch lifecycle events.",
	}, []string{"event"})
	competitionArtifactObjects = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "artifact_objects_total",
		Help: "Authenticated retained competition objects, excluding the enclosing manifest.",
	})
	competitionArtifactBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "artifact_bytes_total",
		Help: "Authenticated retained competition artifact bytes, excluding the enclosing manifest.",
	})
	competitionArtifactFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "artifact_failures_total",
		Help: "Retained artifact upload or post-upload authentication failures.",
	})
)

func init() {
	prometheus.MustRegister(
		competitionConfigured,
		competitionJobs,
		competitionOldestJobAge,
		competitionWorkerHeartbeatAge,
		competitionRunnerHeartbeatTimestamp,
		competitionSubmissionQueueSize,
		competitionCurrentEvaluationInfo,
		competitionCurrentEvaluationElapsed,
		competitionSignificantSubmissionFound,
		competitionEvaluationDurationEstimate,
		competitionSubmissionBacklogEstimate,
		competitionCurrentEpoch,
		competitionRoundPhase,
		competitionArtifactArchiveReady,
		competitionMetricRefreshErrors,
		competitionSubmissions,
		competitionEvaluations,
		competitionEvaluationSeconds,
		competitionRoundEvents,
		competitionArtifactObjects,
		competitionArtifactBytes,
		competitionArtifactFailures,
	)
}

// StartRunnerHeartbeat emits the runner-process liveness signal immediately
// and every 15 seconds until ctx is canceled. Start this only in the dedicated
// competition worker; API processes deliberately leave the signal untouched.
func StartRunnerHeartbeat(ctx context.Context) {
	recordRunnerHeartbeat(server.NowUtc())
	go func() {
		ticker := time.NewTicker(runnerHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				recordRunnerHeartbeat(now.UTC())
			}
		}
	}()
}

func recordRunnerHeartbeat(now time.Time) {
	competitionRunnerHeartbeatTimestamp.Set(float64(now.UnixNano()) / float64(time.Second))
}

// StartMetrics refreshes database-derived gauges for the existing main Grafana
// pipeline. Counters are updated synchronously at their durable boundaries.
// An unconfigured competition leaves a truthful configured=0 and no goroutine.
func StartMetrics(ctx context.Context) {
	settings, err := DefaultService().Settings()
	if err != nil {
		competitionConfigured.Set(0)
		return
	}
	competitionConfigured.Set(1)
	refresh := func() {
		if err := refreshOperationalMetrics(ctx, settings); err != nil {
			competitionMetricRefreshErrors.Inc()
			glog.Infof("[competition]metric refresh failed: %s\n", err)
		}
	}
	refresh()
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

func refreshOperationalMetrics(ctx context.Context, settings *Settings) error {
	now := server.NowUtc()
	queuedCount := 0
	reviewPending := false
	currentEvaluationElapsed := 0.0
	evaluationDurationEstimate := float64(settings.EvaluationPolicy.ScoreTimeoutSeconds)
	for _, state := range []string{"queued", "running", "succeeded", "failed", "canceled"} {
		competitionJobs.WithLabelValues(state).Set(0)
		competitionOldestJobAge.WithLabelValues(state).Set(0)
	}
	competitionWorkerHeartbeatAge.Set(-1)
	competitionSubmissionQueueSize.Set(0)
	competitionCurrentEvaluationInfo.Reset()
	competitionCurrentEvaluationElapsed.Set(0)
	competitionSignificantSubmissionFound.Set(0)
	competitionEvaluationDurationEstimate.Set(evaluationDurationEstimate)
	competitionSubmissionBacklogEstimate.Set(0)
	err := captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, queryErr := conn.Query(ctx, `
				SELECT job.state, count(*),
				       COALESCE(extract(epoch FROM ($2::timestamp - min(job.submitted_at))), 0)
				FROM competition_job AS job
				JOIN competition_round AS round ON round.round_id = job.round_id
				WHERE round.competition_id = $1
				GROUP BY job.state
			`, settings.CompetitionId, now)
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					var state string
					var count int
					var age float64
					server.Raise(rows.Scan(&state, &count, &age))
					competitionJobs.WithLabelValues(state).Set(float64(count))
					competitionOldestJobAge.WithLabelValues(state).Set(age)
					if state == "queued" {
						queuedCount = count
					}
				}
			})
			var heartbeat *time.Time
			server.Raise(conn.QueryRow(ctx, `
				SELECT heartbeat_at FROM competition_worker_slot WHERE slot_id = 1
			`).Scan(&heartbeat))
			if heartbeat != nil {
				competitionWorkerHeartbeatAge.Set(math.Max(0, now.Sub(*heartbeat).Seconds()))
			}
			server.Raise(conn.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM competition_round AS round
					WHERE round.competition_id = $1 AND round.canceled = false
					  AND round.epoch_number = (
					      SELECT max(latest.epoch_number)
					      FROM competition_round AS latest
					      WHERE latest.competition_id = $1 AND latest.canceled = false
					  )
					  AND round.closes_at <= $2 AND round.finalized_at IS NULL
					  AND NOT EXISTS (
					      SELECT 1 FROM competition_job AS active
					      WHERE active.round_id = round.round_id
					        AND active.state IN ('queued', 'running')
					  )
				)
			`, settings.CompetitionId, now).Scan(&reviewPending))

			var jobId string
			var roundId string
			var patchSha256 string
			var attempt int
			var startedAt time.Time
			scanErr := conn.QueryRow(ctx, `
				SELECT job.job_id::text, job.round_id::text, job.patch_sha256,
				       job.attempt_count, job.started_at
				FROM competition_job AS job
				JOIN competition_round AS round ON round.round_id = job.round_id
				WHERE round.competition_id = $1
				  AND job.state = 'running'
				  AND job.started_at IS NOT NULL
				ORDER BY job.started_at, job.job_id
				LIMIT 1
			`, settings.CompetitionId).Scan(
				&jobId, &roundId, &patchSha256, &attempt, &startedAt,
			)
			if scanErr == nil {
				currentEvaluationElapsed = math.Max(0, now.Sub(startedAt).Seconds())
				competitionCurrentEvaluationInfo.WithLabelValues(
					jobId, roundId, patchSha256, strconv.Itoa(attempt),
				).Set(1)
			} else if !errors.Is(scanErr, pgx.ErrNoRows) {
				server.Raise(scanErr)
			}

			var significant bool
			server.Raise(conn.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM competition_job AS job
					JOIN competition_round AS round ON round.round_id = job.round_id
					WHERE round.competition_id = $1
					  AND round.epoch_number = (
					      SELECT max(latest.epoch_number)
					      FROM competition_round AS latest
					      WHERE latest.competition_id = $1
					        AND latest.canceled = false
					  )
					  AND job.state = 'succeeded'
					  AND job.score_json @> '{"significance":{"statistically_significant":true}}'::jsonb
				)
			`, settings.CompetitionId).Scan(&significant))
			if significant {
				competitionSignificantSubmissionFound.Set(1)
			}

			server.Raise(conn.QueryRow(ctx, `
				SELECT COALESCE(
					percentile_cont(0.75) WITHIN GROUP (ORDER BY recent.duration_seconds),
					$2::double precision
				)
				FROM (
					SELECT extract(epoch FROM (job.completed_at - job.started_at))::double precision AS duration_seconds
					FROM competition_job AS job
					JOIN competition_round AS round ON round.round_id = job.round_id
					WHERE round.competition_id = $1
					  AND job.state = 'succeeded'
					  AND job.started_at IS NOT NULL
					  AND job.completed_at >= job.started_at
					ORDER BY job.completed_at DESC
					LIMIT 20
				) AS recent
			`, settings.CompetitionId, evaluationDurationEstimate).Scan(&evaluationDurationEstimate))
		})
	})
	if err != nil {
		return err
	}
	competitionSubmissionQueueSize.Set(float64(queuedCount))
	competitionCurrentEvaluationElapsed.Set(currentEvaluationElapsed)
	competitionEvaluationDurationEstimate.Set(evaluationDurationEstimate)
	backlogEstimate := float64(queuedCount) * evaluationDurationEstimate
	if 0 < currentEvaluationElapsed {
		backlogEstimate += math.Max(0, evaluationDurationEstimate-currentEvaluationElapsed)
	}
	competitionSubmissionBacklogEstimate.Set(backlogEstimate)
	for _, phase := range []string{"none", "scheduled", "open", "grading", "review", "finalized", "canceled"} {
		competitionRoundPhase.WithLabelValues(phase).Set(0)
	}
	round, err := (PostgresStore{}).CurrentRound(ctx, settings)
	if err != nil {
		return err
	}
	if round == nil {
		competitionCurrentEpoch.Set(0)
		competitionRoundPhase.WithLabelValues("none").Set(1)
	} else {
		competitionCurrentEpoch.Set(float64(round.Epoch))
		phase := round.Status
		if phase == "grading" && reviewPending {
			phase = "review"
		}
		competitionRoundPhase.WithLabelValues(phase).Set(1)
	}
	archiveReady := settings.artifactArchive != nil && settings.artifactArchive.Check(ctx) == nil
	if archiveReady {
		competitionArtifactArchiveReady.Set(1)
	} else {
		competitionArtifactArchiveReady.Set(0)
	}
	return nil
}

var (
	hunkHeaderPattern = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?: .*)?$`)
	indexLinePattern  = regexp.MustCompile(`^index [0-9a-f]{7,64}\.\.[0-9a-f]{7,64}(?: 100644)?$`)
)

const protectedSimulatorTreePattern = "connect/sim-latency/**"

var hardForbiddenPatchPaths = []string{
	".git/**",
	".github/**",
	"**/go.mod",
	"**/go.sum",
	"go.mod",
	"go.sum",
	"vendor/**",
	"api/**",
	"cli/**",
	"competition/**",
	"config/**",
	"vault/**",
	"site/**",
	"db_migrations.go",
	"db_migrations_*.go",
	protectedSimulatorTreePattern,
	"stats/**",
}

type CanonicalPatch struct {
	Bytes  []byte
	Sha256 string
	Paths  []string
}

func ValidateAndCanonicalizePatch(raw string, policy PatchPolicy) (*CanonicalPatch, *CompetitionError) {
	bytes := []byte(raw)
	if len(bytes) == 0 {
		return nil, submissionError("empty_patch", "patch must not be empty")
	}
	if policy.MaxPatchBytes < len(bytes) {
		return nil, submissionError("patch_too_large", fmt.Sprintf("patch exceeds %d bytes", policy.MaxPatchBytes))
	}
	if !utf8.Valid(bytes) || strings.IndexByte(raw, 0) >= 0 {
		return nil, submissionError("invalid_patch_encoding", "patch must be valid UTF-8 text without NUL bytes")
	}
	if strings.Contains(raw, "\r") {
		return nil, submissionError("noncanonical_patch", "patch must use LF line endings")
	}
	if !strings.HasSuffix(raw, "\n") {
		return nil, submissionError("noncanonical_patch", "patch must end with exactly one LF")
	}
	if strings.HasSuffix(raw, "\n\n") {
		return nil, submissionError("noncanonical_patch", "patch must not contain extra trailing blank lines")
	}
	for _, r := range raw {
		if r < 0x20 && r != '\n' && r != '\t' {
			return nil, submissionError("invalid_patch_encoding", "patch contains a disallowed control character")
		}
	}
	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "diff --git ") {
		return nil, submissionError("invalid_patch_structure", "patch must start with a diff --git header")
	}
	paths := []string{}
	seen := map[string]bool{}
	currentPath := ""
	oldHeaderSeen, newHeaderSeen, hunkSeen := false, false, false
	oldRemaining, newRemaining := 0, 0
	lastHunkContent := false
	finishSection := func() *CompetitionError {
		if currentPath != "" && (!oldHeaderSeen || !newHeaderSeen || !hunkSeen) {
			return submissionError("invalid_patch_structure", fmt.Sprintf("path %q is missing canonical file headers or a hunk", currentPath))
		}
		if hunkSeen && (oldRemaining != 0 || newRemaining != 0) {
			return submissionError("invalid_patch_structure", fmt.Sprintf("path %q has a hunk whose declared line counts do not match its body", currentPath))
		}
		return nil
	}
	for i, line := range lines {
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if sectionErr := finishSection(); sectionErr != nil {
				return nil, sectionErr
			}
			filePath, err := parseDiffHeader(line)
			if err != nil {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: %s", i+1, err))
			}
			if seen[filePath] {
				return nil, submissionError("duplicate_patch_path", fmt.Sprintf("path %q appears more than once", filePath))
			}
			if !pathAllowed(filePath, policy) {
				return nil, submissionError("path_not_allowed", fmt.Sprintf("path %q is outside the editable surface", filePath))
			}
			seen[filePath] = true
			paths = append(paths, filePath)
			currentPath = filePath
			oldHeaderSeen, newHeaderSeen, hunkSeen = false, false, false
		case strings.HasPrefix(line, "diff "):
			return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: only diff --git file headers are accepted", i+1))
		case !hunkSeen && strings.HasPrefix(line, "--- "):
			if currentPath == "" || oldHeaderSeen || line != "--- a/"+currentPath {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: old-file header does not match diff path", i+1))
			}
			oldHeaderSeen = true
		case !hunkSeen && strings.HasPrefix(line, "+++ "):
			if currentPath == "" || !oldHeaderSeen || newHeaderSeen || line != "+++ b/"+currentPath {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: new-file header does not match diff path", i+1))
			}
			newHeaderSeen = true
		case strings.HasPrefix(line, "@@ "):
			if currentPath == "" || !oldHeaderSeen || !newHeaderSeen {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: hunk appears before canonical file headers", i+1))
			}
			if hunkSeen && (oldRemaining != 0 || newRemaining != 0) {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: previous hunk line counts do not match its body", i+1))
			}
			var err error
			oldRemaining, newRemaining, err = parseHunkHeader(line)
			if err != nil {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: %s", i+1, err))
			}
			hunkSeen = true
			lastHunkContent = false
		case strings.HasPrefix(line, "@@@"):
			return nil, submissionError("invalid_patch_structure", "combined diffs are not accepted")
		case !hunkSeen && strings.HasPrefix(line, "index "):
			if currentPath == "" || !indexLinePattern.MatchString(line) {
				return nil, submissionError("unsupported_patch_operation", "only regular 100644 file indexes are accepted")
			}
		case !hunkSeen && strings.HasPrefix(line, "Binary files "), line == "GIT binary patch":
			return nil, submissionError("binary_patch", "binary patches are not accepted")
		case !hunkSeen && (strings.HasPrefix(line, "new file mode ") || strings.HasPrefix(line, "deleted file mode ") ||
			strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode ") ||
			strings.HasPrefix(line, "rename from ") || strings.HasPrefix(line, "rename to ") ||
			strings.HasPrefix(line, "copy from ") || strings.HasPrefix(line, "copy to ") ||
			strings.HasPrefix(line, "similarity index ") || strings.HasPrefix(line, "dissimilarity index ")):
			return nil, submissionError("unsupported_patch_operation", "new/deleted files, renames, copies, and mode changes are not accepted")
		case strings.Contains(lower, "subproject commit"):
			return nil, submissionError("submodule_patch", "submodule changes are not accepted")
		case (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")) &&
			(strings.HasPrefix(strings.TrimSpace(line[1:]), "//go:build") || strings.HasPrefix(strings.TrimSpace(line[1:]), "// +build")):
			return nil, submissionError("build_tag_patch", "build constraint changes are not accepted")
		case hunkSeen:
			switch {
			case strings.HasPrefix(line, " "):
				oldRemaining--
				newRemaining--
				lastHunkContent = true
			case strings.HasPrefix(line, "+"):
				newRemaining--
				lastHunkContent = true
			case strings.HasPrefix(line, "-"):
				oldRemaining--
				lastHunkContent = true
			case line == `\ No newline at end of file` && lastHunkContent:
				lastHunkContent = false
			default:
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: invalid unified-diff hunk line", i+1))
			}
			if oldRemaining < 0 || newRemaining < 0 {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: hunk body exceeds its declared line counts", i+1))
			}
		default:
			return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: unexpected file metadata", i+1))
		}
	}
	if sectionErr := finishSection(); sectionErr != nil {
		return nil, sectionErr
	}
	if len(paths) == 0 {
		return nil, submissionError("invalid_patch_structure", "patch contains no diff --git file header")
	}
	if !sort.StringsAreSorted(paths) {
		return nil, submissionError("noncanonical_patch", "file sections must be sorted by path")
	}
	hash := sha256.Sum256(bytes)
	return &CanonicalPatch{Bytes: bytes, Sha256: hex.EncodeToString(hash[:]), Paths: paths}, nil
}

func parseHunkHeader(line string) (int, int, error) {
	match := hunkHeaderPattern.FindStringSubmatch(line)
	if match == nil {
		return 0, 0, fmt.Errorf("malformed hunk header")
	}
	count := func(value string) (int, error) {
		if value == "" {
			return 1, nil
		}
		return strconv.Atoi(value)
	}
	oldCount, err := count(match[2])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid old-file hunk count")
	}
	newCount, err := count(match[4])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid new-file hunk count")
	}
	return oldCount, newCount, nil
}

func parseDiffHeader(line string) (string, error) {
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != "diff" || fields[1] != "--git" {
		return "", fmt.Errorf("malformed diff header")
	}
	if strings.ContainsAny(fields[2]+fields[3], `"'\\`) || !strings.HasPrefix(fields[2], "a/") || !strings.HasPrefix(fields[3], "b/") {
		return "", fmt.Errorf("quoted, escaped, or non-canonical paths are not accepted")
	}
	a, b := strings.TrimPrefix(fields[2], "a/"), strings.TrimPrefix(fields[3], "b/")
	if a != b {
		return "", fmt.Errorf("rename-like diff header is not accepted")
	}
	if err := validateRepositoryPath(a); err != nil {
		return "", err
	}
	return a, nil
}

func validateRepositoryPath(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.Contains(value, "//") {
		return fmt.Errorf("unsafe repository path")
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path traversal is not accepted")
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == ".git" || segment == ".." || segment == "." {
			return fmt.Errorf("repository metadata path is not accepted")
		}
	}
	return nil
}

func pathAllowed(value string, policy PatchPolicy) bool {
	forbidden := append(append([]string{}, hardForbiddenPatchPaths...), policy.ForbiddenPaths...)
	if matchAny(forbidden, value) {
		return false
	}
	return matchAny(policy.AllowedPaths, value)
}

func matchAny(patterns []string, value string) bool {
	for _, pattern := range patterns {

		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "**")
			if strings.HasPrefix(value, prefix) {
				return true
			}
		}
		if matched, _ := filepath.Match(pattern, value); matched {
			return true
		}
	}
	return false
}

func submissionError(code, message string) *CompetitionError {
	return &CompetitionError{Kind: "submission", Code: code, Message: message, Retriable: false}
}

// Epoch admission is start-inclusive and end-exclusive. Keep this predicate
// shared by API admission and durable queue defenses so boundary behavior
// cannot drift between them.
func submissionWithinEpoch(round *roundRecord, submittedAt time.Time) bool {
	return round != nil && !submittedAt.Before(round.OpensAt) && submittedAt.Before(round.ClosesAt)
}

// PostgreSQL remains the authority for job state, FIFO order, leases, and
// finalization. This Redis list is a rebuildable dispatch index: losing it can
// add at most one worker poll interval because Claim always falls back to the
// authoritative ordered SQL query.
func competitionFifoKeys(settings *Settings) (string, string) {
	digest := sha256.Sum256([]byte(settings.CompetitionId))
	prefix := "competition:{" + hex.EncodeToString(digest[:]) + "}:fifo:v1"
	return prefix + ":list", prefix + ":members"
}

// Adds a queued job at most once. The set is only an O(1) deduplication sidecar;
// the list is the FIFO. Both keys share one Redis Cluster hash slot and mutate
// in one script. A stale or missing index remains recoverable from PostgreSQL.
func enqueueCompetitionJob(ctx context.Context, settings *Settings, jobId server.Id) error {
	const enqueueScript = `
if redis.call('SADD', KEYS[2], ARGV[1]) == 1 then
  redis.call('RPUSH', KEYS[1], ARGV[1])
  return 1
end
return 0
`
	listKey, memberKey := competitionFifoKeys(settings)
	return captureRedisError(func() {
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Eval(
				ctx,
				enqueueScript,
				[]string{listKey, memberKey},
				jobId.String(),
			).Err())
		})
	})
}

func dequeueCompetitionJob(ctx context.Context, settings *Settings) (*server.Id, error) {
	const dequeueScript = `
local value = redis.call('LPOP', KEYS[1])
if value then
  redis.call('SREM', KEYS[2], value)
end
return value
`
	listKey, memberKey := competitionFifoKeys(settings)
	var encoded string
	err := captureRedisError(func() {
		server.Redis(ctx, func(client server.RedisClient) {
			value, popErr := client.Eval(ctx, dequeueScript, []string{listKey, memberKey}).Text()
			if errors.Is(popErr, server.RedisNil) {
				return
			}
			server.Raise(popErr)
			encoded = value
		})
	})
	if err != nil || encoded == "" {
		return nil, err
	}
	jobId, err := server.ParseId(encoded)
	if err != nil {

		return nil, nil
	}
	return &jobId, nil
}

func removeCompetitionJobSignal(ctx context.Context, settings *Settings, jobId server.Id) error {
	const removeScript = `
redis.call('LREM', KEYS[1], 0, ARGV[1])
redis.call('SREM', KEYS[2], ARGV[1])
return 1
`
	listKey, memberKey := competitionFifoKeys(settings)
	return captureRedisError(func() {
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Eval(
				ctx,
				removeScript,
				[]string{listKey, memberKey},
				jobId.String(),
			).Err())
		})
	})
}

func checkCompetitionFifo(ctx context.Context, settings *Settings) error {
	listKey, _ := competitionFifoKeys(settings)
	return captureRedisError(func() {
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.LLen(ctx, listKey).Err())
		})
	})
}

func captureRedisError(run func()) error {
	if recovered := server.HandleError(run); recovered != nil {
		if err, ok := recovered.(error); ok {
			return err
		}
		return fmt.Errorf("%v", recovered)
	}
	return nil
}

type Service struct {
	settings            *Settings
	settingsErr         error
	store               Store
	apiImageDigest      string
	apiImageIdentityErr error
}

func NewService(settings *Settings, store Store) *Service {
	apiImageDigest, apiImageIdentityErr := runtimeImageDigest()
	return newServiceWithImageDigest(settings, store, apiImageDigest, apiImageIdentityErr)
}

func newServiceWithImageDigest(settings *Settings, store Store, apiImageDigest string, identityErr error) *Service {
	service := &Service{
		settings: settings, store: store, apiImageDigest: apiImageDigest,
		apiImageIdentityErr: identityErr,
	}
	if settings == nil {
		service.settingsErr = errors.New("competition settings unavailable")
	} else if err := settings.Validate(); err != nil {
		service.settingsErr = err
	}
	if store == nil {
		service.settingsErr = errors.New("competition store unavailable")
	}
	return service
}

var defaultService = sync.OnceValue(func() *Service {
	settings, err := LoadSettings()
	if err != nil {
		return &Service{settingsErr: err, store: PostgresStore{}}
	}
	return NewService(settings, PostgresStore{})
})

func DefaultService() *Service { return defaultService() }

func (self *Service) Settings() (*Settings, error) {
	if self == nil || self.settingsErr != nil {
		if self == nil {
			return nil, errors.New("competition service unavailable")
		}
		return nil, self.settingsErr
	}
	return self.settings, nil
}

func (self *Service) Health() HealthResult {
	version, err := server.Version()
	if err != nil {
		version = "unknown"
	}
	return HealthResult{Status: "alive", Version: version, Time: server.NowUtc()}
}

func (self *Service) Ready(ctx context.Context) (ReadinessResult, *CompetitionError) {
	result := ReadinessResult{Ready: false, Checks: map[string]bool{}, CheckedAt: server.NowUtc()}
	settings, err := self.Settings()
	if err != nil {
		result.Checks["configuration"] = false
		return result, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	checks, err := self.readinessChecks(ctx, settings)
	if err != nil {
		result.Checks["database"] = false
		return result, infrastructureError("readiness_failed", "competition dependencies did not pass readiness")
	}
	result.Checks = checks
	result.Ready = allChecks(checks)
	if !result.Ready {
		return result, infrastructureError("not_ready", "competition evaluator is not ready")
	}
	return result, nil
}

func (self *Service) readinessChecks(ctx context.Context, settings *Settings) (map[string]bool, error) {
	checks, err := self.store.Readiness(ctx, settings)
	if checks == nil {
		checks = map[string]bool{}
	}
	checks["api_image_identity"] = self.apiImageIdentityErr == nil && imageDigestPattern.MatchString(self.apiImageDigest)
	if err != nil {
		return checks, err
	}
	checks["artifact_archive"] = settings.artifactArchive != nil && settings.artifactArchive.Check(ctx) == nil
	return checks, nil
}

func allChecks(checks map[string]bool) bool {
	if len(checks) == 0 {
		return false
	}
	for _, passed := range checks {
		if !passed {
			return false
		}
	}
	return true
}

func secureEvaluatorChecksPass(checks map[string]bool) bool {
	return allChecks(checks)
}

func roundGenerationChecksPass(checks map[string]bool) bool {
	if len(checks) == 0 {
		return false
	}
	for name, passed := range checks {

		if name != "host_rebaseline" && !passed {
			return false
		}
	}
	return true
}

func (self *Service) requireSecureEvaluator(ctx context.Context, settings *Settings) *CompetitionError {
	checks, err := self.readinessChecks(ctx, settings)
	if err != nil || !secureEvaluatorChecksPass(checks) {
		return infrastructureError("not_ready", "competition evaluator containment is not ready")
	}
	return nil
}

func (self *Service) requireRoundGenerationInfrastructure(ctx context.Context, settings *Settings) *CompetitionError {
	checks, err := self.readinessChecks(ctx, settings)
	if err != nil || !roundGenerationChecksPass(checks) {
		return infrastructureError("not_ready", "competition evaluator infrastructure is not ready for round generation")
	}
	return nil
}

func (self *Service) Info(ctx context.Context) (*InfoResult, *CompetitionError) {
	settings, err := self.Settings()
	if err != nil {
		return nil, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	result := settings.PublicInfo()
	round, err := self.store.CurrentRound(ctx, settings)
	if err != nil {
		return nil, infrastructureError("storage_unavailable", "competition round storage is unavailable")
	}
	if round != nil {
		view, revealErr := self.roundView(round)
		if revealErr != nil {
			return nil, revealErr
		}
		result.ActiveRound = view
	}
	return &result, nil
}

func (self *Service) GenerateRound(ctx context.Context, args GenerateRoundArgs) (*RoundResult, *CompetitionError) {
	settings, err := self.Settings()
	if err != nil {
		return nil, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	args.OpensAt, args.ClosesAt, args.RevealAt = args.OpensAt.UTC(), args.ClosesAt.UTC(), args.RevealAt.UTC()
	if args.OpensAt.IsZero() || args.ClosesAt.IsZero() || args.RevealAt.IsZero() ||
		!args.OpensAt.Before(args.ClosesAt) || !args.RevealAt.Equal(args.ClosesAt) ||
		args.ClosesAt.Sub(args.OpensAt) != time.Duration(settings.SeasonPolicy.SubmissionWindowSeconds)*time.Second {
		return nil, submissionError("invalid_round_times", "round must use the frozen seven-day window with reveal_at equal to closes_at")
	}
	if args.OpensAt.Before(server.NowUtc().Add(-time.Minute)) {
		return nil, submissionError("invalid_round_times", "opens_at may not be in the past")
	}
	if settings.SeasonEndsAt.Before(args.ClosesAt) || settings.RetainUntil.Before(args.RevealAt) {
		return nil, submissionError("invalid_round_times", "round close/reveal exceeds the frozen season retention window")
	}
	if readyErr := self.requireRoundGenerationInfrastructure(ctx, settings); readyErr != nil {
		return nil, readyErr
	}
	round, err := self.store.CreateRound(ctx, settings, args)
	if errors.Is(err, ErrConflict) {
		return nil, &CompetitionError{Kind: "submission", Code: "round_overlap", Message: "round overlaps an existing round", Retriable: false}
	}
	if errors.Is(err, ErrPreviousEpochOpen) {
		return nil, &CompetitionError{Kind: "submission", Code: "previous_epoch_open", Message: "the previous epoch has not finished grading", Retriable: false}
	}
	if errors.Is(err, ErrSeasonComplete) {
		return nil, &CompetitionError{Kind: "submission", Code: "season_complete", Message: "all six competition epochs already exist", Retriable: false}
	}
	if err != nil {
		return nil, infrastructureError("round_create_failed", "round could not be committed")
	}
	return &round.RoundResult, nil
}

func (self *Service) Leaderboards(ctx context.Context) (*SeasonLeaderboardResult, *CompetitionError) {
	settings, err := self.Settings()
	if err != nil {
		return nil, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	result, err := self.store.Leaderboards(ctx, settings)
	if err != nil {
		return nil, infrastructureError("storage_unavailable", "competition leaderboard storage is unavailable")
	}
	return result, nil
}

func (self *Service) Submit(ctx context.Context, args ScoreArgs, principal *Principal) (*ScoreAcceptedResult, int, *CompetitionError) {
	metricOutcome := "infrastructure_error"
	defer func() { competitionSubmissions.WithLabelValues(metricOutcome).Inc() }()
	settings, err := self.Settings()
	if err != nil {
		return nil, 503, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	patch, patchErr := ValidateAndCanonicalizePatch(args.Patch, settings.PatchPolicy)
	if patchErr != nil {
		metricOutcome = "rejected"
		status := 422
		if patchErr.Code == "patch_too_large" {
			status = 413
		}
		return nil, status, patchErr
	}
	if readyErr := self.requireSecureEvaluator(ctx, settings); readyErr != nil {
		return nil, 503, readyErr
	}
	job, hit, err := self.store.Enqueue(ctx, settings, args.RoundId, patch, principal.Id, self.apiImageDigest)
	switch {
	case errors.Is(err, ErrNotFound):
		metricOutcome = "rejected"
		return nil, 404, submissionError("round_not_found", "round does not exist")
	case errors.Is(err, ErrRoundClosed):
		metricOutcome = "rejected"
		return nil, 409, submissionError("round_not_open", "round is not open for submissions")
	case err != nil:
		return nil, 503, infrastructureError("enqueue_failed", "submission could not be durably enqueued")
	}
	result := &ScoreAcceptedResult{
		JobId: job.JobId, RoundId: job.RoundId, PatchSha256: job.PatchSha256,
		State:    scoreJobStateView(job.State, roundPublished(&job.Round, server.NowUtc()), principal),
		CacheHit: hit, StatusUrl: "/competition/score/" + job.JobId.String(),
	}
	if hit {
		metricOutcome = "cache_hit"
	} else {
		metricOutcome = "accepted"
	}
	return result, 202, nil
}

func (self *Service) GetScore(ctx context.Context, jobId server.Id, principal *Principal) (*ScoreJobResult, int, *CompetitionError) {
	settings, err := self.Settings()
	if err != nil {
		return nil, 503, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	job, err := self.store.GetJob(ctx, settings, jobId, principal)
	if errors.Is(err, ErrNotFound) {
		return nil, 404, submissionError("job_not_found", "score job does not exist")
	}
	if err != nil {
		return nil, 503, infrastructureError("storage_unavailable", "score job storage is unavailable")
	}
	result := scoreJobView(job, principal, server.NowUtc())
	return &result, 200, nil
}

func (self *Service) GetRoundWorkload(ctx context.Context, roundId server.Id) ([]byte, string, int, *CompetitionError) {
	settings, err := self.Settings()
	if err != nil {
		return nil, "", 503, infrastructureError("configuration_unavailable", "competition configuration is not ready")
	}
	round, err := self.store.GetRound(ctx, settings, roundId)
	if errors.Is(err, ErrNotFound) {
		return nil, "", 404, submissionError("round_not_found", "round does not exist")
	}
	if err != nil {
		return nil, "", 503, infrastructureError("storage_unavailable", "competition round storage is unavailable")
	}
	if round.Canceled || !roundPublished(round, server.NowUtc()) {
		return nil, "", 409, submissionError("round_not_revealed", "round workload is unavailable until post-review epoch finalization")
	}
	providers, err := readRoundWorkload(ctx, settings, round)
	if err != nil {
		return nil, "", 503, infrastructureError("round_workload_unavailable", "committed round workload failed authentication")
	}
	return providers, round.ProvidersSha256, 200, nil
}

func (self *Service) roundView(round *roundRecord) (*RoundResult, *CompetitionError) {
	view := round.RoundResult
	if roundPublished(round, server.NowUtc()) {
		seed, err := revealRoundSecret(self.settings, round)
		if err != nil {
			return nil, infrastructureError("round_reveal_failed", "round commitment could not be revealed")
		}
		view.RevealedSeed = &seed
		view.ProvidersUrl = "/competition/round/" + round.RoundId.String() + "/providers.yml"
	}
	return &view, nil
}

func roundPublished(round *roundRecord, now time.Time) bool {
	return round.FinalizedAt != nil && !now.Before(round.RevealAt)
}

func scoreJobStateView(state string, published bool, principal *Principal) string {
	if principal.Role != "operator" && !published && (state == "succeeded" || state == "failed") {
		return "completed"
	}
	return state
}

func scoreJobView(job *queuedJob, principal *Principal, now time.Time) ScoreJobResult {
	result := job.ScoreJobResult
	if principal.Role != "operator" && !roundPublished(&job.Round, now) {
		result.State = scoreJobStateView(result.State, false, principal)
		result.Score = nil
		result.EvalError = nil
	}
	return result
}

func validateScore(score *ScoreResult) error {
	if score == nil || score.ScoreSchema != ScoreSchema || score.RawScore == nil || score.NormalizedScore == nil {
		return errors.New("score result is missing required fields")
	}
	if math.IsNaN(*score.RawScore) || math.IsInf(*score.RawScore, 0) || *score.RawScore <= 0 {
		return errors.New("raw score must be finite and positive")
	}
	if math.IsNaN(*score.NormalizedScore) || math.IsInf(*score.NormalizedScore, 0) || *score.NormalizedScore < 1 || 200 < *score.NormalizedScore {
		return errors.New("normalized score must be finite and in [1, 200]")
	}
	if score.Gates == nil {
		return errors.New("score gates are missing")
	}
	if err := validateScoreSignificance(score.Significance); err != nil {
		return err
	}
	if score.TakeoverEligible && (!score.Placeable ||
		!score.Significance.StatisticallySignificant ||
		!score.Significance.RecommendedNextEpochTakeoverMarginSupported) {
		return errors.New("takeover eligibility contradicts statistical significance")
	}
	for name, gate := range score.Gates {
		if strings.TrimSpace(name) == "" || gate.Details == nil {
			return fmt.Errorf("score gate %q is malformed", name)
		}
	}
	return nil
}

func validateScoreSignificance(significance *ScoreSignificance) error {
	if significance == nil || significance.Method != "one-sided-welch-t" ||
		significance.Alpha != 0.05 || significance.ReplicateCount <= 0 ||
		9 < significance.ReplicateCount || significance.ReplicateCount%2 == 0 ||
		!finitePositiveNumber(significance.BaselineMeanRawScore) ||
		!finitePositiveNumber(significance.CandidateMeanRawScore) ||
		!finiteNumber(significance.ObservedImprovementPercent) ||
		!finitePositiveNumber(significance.TakeoverMarginPercent) ||
		50 < significance.TakeoverMarginPercent {
		return errors.New("score significance metadata is malformed")
	}
	if significance.ReplicateCount == 1 {
		if significance.BaselineSampleVariance != nil ||
			significance.CandidateSampleVariance != nil ||
			significance.MinimumSignificantImprovementPercent != nil ||
			significance.RequiredImprovementPercent != nil ||
			significance.OneSidedPValue != nil || significance.WelchT != nil ||
			significance.WelchDegreesOfFreedom != nil ||
			significance.StatisticallySignificant ||
			significance.NextEpochMinimumImprovementPercent != nil ||
			significance.RecommendedNextEpochTakeoverMarginPercent != nil ||
			significance.RecommendedNextEpochTakeoverMarginSupported {
			return errors.New("single-replicate score claims unavailable significance")
		}
		return nil
	}
	if !finiteNonnegativePointer(significance.BaselineSampleVariance) ||
		!finiteNonnegativePointer(significance.CandidateSampleVariance) ||
		!finiteNonnegativePointer(significance.MinimumSignificantImprovementPercent) ||
		!finiteNonnegativePointer(significance.RequiredImprovementPercent) ||
		!finiteNonnegativePointer(significance.NextEpochMinimumImprovementPercent) ||
		!finitePositivePointer(significance.RecommendedNextEpochTakeoverMarginPercent) ||
		significance.OneSidedPValue == nil ||
		!finiteNumber(*significance.OneSidedPValue) ||
		*significance.OneSidedPValue < 0 || 1 < *significance.OneSidedPValue {
		return errors.New("score significance calculation is incomplete")
	}
	if significance.WelchT != nil && !finiteNumber(*significance.WelchT) {
		return errors.New("score Welch statistic is not finite")
	}
	if significance.WelchDegreesOfFreedom != nil &&
		!finitePositiveNumber(*significance.WelchDegreesOfFreedom) {
		return errors.New("score Welch degrees of freedom are invalid")
	}
	statisticallySignificant := 0 < significance.ObservedImprovementPercent &&
		*significance.OneSidedPValue <= significance.Alpha
	if significance.StatisticallySignificant != statisticallySignificant {
		return errors.New("score significance decision is inconsistent")
	}
	supported := 0 < *significance.RecommendedNextEpochTakeoverMarginPercent &&
		*significance.RecommendedNextEpochTakeoverMarginPercent <= 50
	if significance.RecommendedNextEpochTakeoverMarginSupported != supported {
		return errors.New("next-epoch significance margin support is inconsistent")
	}
	return nil
}

func finiteNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func finitePositiveNumber(value float64) bool {
	return finiteNumber(value) && 0 < value
}

func finiteNonnegativePointer(value *float64) bool {
	return value != nil && finiteNumber(*value) && 0 <= *value
}

func finitePositivePointer(value *float64) bool {
	return value != nil && finitePositiveNumber(*value)
}

func infrastructureError(code, message string) *CompetitionError {
	return &CompetitionError{Kind: "infrastructure", Code: code, Message: message, Retriable: true}
}

var (
	ErrNotFound          = errors.New("competition object not found")
	ErrConflict          = errors.New("competition state conflict")
	ErrRoundClosed       = errors.New("competition round is not open")
	ErrLeaseLost         = errors.New("competition worker lease lost")
	ErrSeasonComplete    = errors.New("competition season is complete")
	ErrPreviousEpochOpen = errors.New("previous competition epoch is not finalized")
	ErrReviewNotReady    = errors.New("competition epoch is not ready for honesty review")
	ErrReviewOutOfOrder  = errors.New("competition honesty review candidate is out of order")
)

type Store interface {
	CreateRound(context.Context, *Settings, GenerateRoundArgs) (*roundRecord, error)
	CurrentRound(context.Context, *Settings) (*roundRecord, error)
	GetRound(context.Context, *Settings, server.Id) (*roundRecord, error)
	PrepareCandidateReview(context.Context, *Settings, int) (*CandidateReviewState, error)
	RecordCandidateReview(context.Context, *Settings, int, CandidateReviewDecision) (*CandidateReviewState, error)
	Leaderboards(context.Context, *Settings) (*SeasonLeaderboardResult, error)
	Enqueue(context.Context, *Settings, server.Id, *CanonicalPatch, string, string) (*queuedJob, bool, error)
	GetJob(context.Context, *Settings, server.Id, *Principal) (*queuedJob, error)
	Readiness(context.Context, *Settings) (map[string]bool, error)
	RegisterHost(context.Context, *Settings, HostSelfCheck) error
	Claim(context.Context, *Settings, string, string) (*queuedJob, error)
	Heartbeat(context.Context, *Settings, string, server.Id) error
	Complete(context.Context, *Settings, string, server.Id, EvaluationOutcome) (bool, error)
	HandBack(context.Context, string, server.Id, string) error
}

type PostgresStore struct {
	now func() time.Time
}

func (self PostgresStore) nowUtc() time.Time {
	if self.now != nil {
		return self.now().UTC()
	}
	return server.NowUtc()
}

type roundPolicySnapshot struct {
	Schema               int              `json:"schema"`
	CompetitionId        string           `json:"competition_id"`
	BaseSha              string           `json:"base_sha"`
	EvaluatorImageDigest string           `json:"evaluator_image_digest"`
	ScoreSchema          int              `json:"score_schema"`
	ScorerVersion        string           `json:"scorer_version"`
	PatchPolicy          PatchPolicy      `json:"patch_policy"`
	EvaluationPolicy     EvaluationPolicy `json:"evaluation_policy"`
	SeasonPolicy         SeasonPolicy     `json:"season_policy"`
}

func policySnapshot(settings *Settings) ([]byte, error) {
	return json.Marshal(roundPolicySnapshot{
		Schema:               1,
		CompetitionId:        settings.CompetitionId,
		BaseSha:              settings.BaseSha,
		EvaluatorImageDigest: settings.EvaluatorImageDigest,
		ScoreSchema:          ScoreSchema,
		ScorerVersion:        ScorerVersion,
		PatchPolicy:          settings.PatchPolicy,
		EvaluationPolicy:     settings.EvaluationPolicy,
		SeasonPolicy:         settings.SeasonPolicy,
	})
}

// Reads the evaluator identity frozen with a round rather than the current
// process configuration, so historical job responses retain exact provenance.
func evaluatorImageDigestFromPolicy(stored json.RawMessage) (string, error) {
	var policy roundPolicySnapshot
	if len(stored) == 0 {
		return "", errors.New("round policy is empty")
	}
	if err := json.Unmarshal(stored, &policy); err != nil {
		return "", fmt.Errorf("decode round policy: %w", err)
	}
	if !imageDigestPattern.MatchString(policy.EvaluatorImageDigest) {
		return "", errors.New("round evaluator image digest is invalid")
	}
	return policy.EvaluatorImageDigest, nil
}

func (self PostgresStore) CreateRound(ctx context.Context, settings *Settings, args GenerateRoundArgs) (*roundRecord, error) {
	round := &roundRecord{
		RoundResult: RoundResult{
			RoundId:     server.NewId(),
			ScoreSchema: ScoreSchema,
			OpensAt:     args.OpensAt.UTC(),
			ClosesAt:    args.ClosesAt.UTC(),
			RevealAt:    args.RevealAt.UTC(),
			CreatedAt:   self.nowUtc(),
		},
		CompetitionId: settings.CompetitionId,
	}
	policy, err := policySnapshot(settings)
	if err != nil {
		return nil, err
	}
	round.PolicyJson = policy
	round.SeedNonce, round.SeedCiphertext, round.WorkloadCommitment, err = createRoundSecret(settings, round.RoundId)
	if err != nil {
		return nil, err
	}
	seed, err := revealRoundSecret(settings, round)
	if err != nil {
		return nil, err
	}
	workload, err := generateRoundWorkload(ctx, settings, round.RoundId, seed)
	seed = ""
	if err != nil {
		return nil, err
	}
	round.ProvidersPath = workload.Path
	round.ProvidersSha256 = workload.Sha256
	if settings.artifactArchive == nil {
		removeRoundWorkload(settings, round)
		return nil, errors.New("competition artifact archive is unavailable")
	}
	if err := settings.artifactArchive.ArchiveRound(ctx, settings, round, workload); err != nil {
		removeRoundWorkload(settings, round)
		return nil, fmt.Errorf("archive round workload: %w", err)
	}
	conflict := false
	var stateErr error
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-round-v1', 0))`))
			var previousEpoch int
			server.Raise(tx.QueryRow(ctx, `
				SELECT COALESCE(max(epoch_number), 0)
				FROM competition_round WHERE competition_id = $1
			`, settings.CompetitionId).Scan(&previousEpoch))
			if settings.SeasonPolicy.EpochCount <= previousEpoch {
				stateErr = ErrSeasonComplete
				return
			}
			if 0 < previousEpoch {
				var previousFinalized *time.Time
				server.Raise(tx.QueryRow(ctx, `
					SELECT finalized_at FROM competition_round
					WHERE competition_id = $1 AND epoch_number = $2
				`, settings.CompetitionId, previousEpoch).Scan(&previousFinalized))
				if previousFinalized == nil {
					stateErr = ErrPreviousEpochOpen
					return
				}
			}
			round.Epoch = previousEpoch + 1
			var overlap bool
			server.Raise(tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM competition_round
					WHERE competition_id = $1 AND canceled = false
					  AND opens_at < $3 AND $2 < closes_at
				)
			`, settings.CompetitionId, round.OpensAt, round.ClosesAt).Scan(&overlap))
			if overlap {
				conflict = true
				return
			}
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO competition_round (
					round_id, competition_id, epoch_number, workload_commitment, seed_nonce,
					seed_ciphertext, providers_sha256, providers_path, policy_json,
					opens_at, closes_at, reveal_at, created_at, canceled
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13, false)
			`, round.RoundId, round.CompetitionId, round.Epoch, round.WorkloadCommitment,
				round.SeedNonce, round.SeedCiphertext, round.ProvidersSha256,
				round.ProvidersPath, string(round.PolicyJson), round.OpensAt,
				round.ClosesAt, round.RevealAt, round.CreatedAt))
		})
	})
	if err != nil {
		removeRoundWorkload(settings, round)
		return nil, err
	}
	if stateErr != nil {
		removeRoundWorkload(settings, round)
		return nil, stateErr
	}
	if conflict {
		removeRoundWorkload(settings, round)
		return nil, ErrConflict
	}
	setRoundStatus(round, self.nowUtc())
	competitionRoundEvents.WithLabelValues("created").Inc()
	return round, nil
}

func (self PostgresStore) CurrentRound(ctx context.Context, settings *Settings) (round *roundRecord, err error) {
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			row := conn.QueryRow(ctx, `
				SELECT round_id, competition_id, epoch_number, workload_commitment, seed_nonce,
				       seed_ciphertext, providers_sha256, providers_path,
				       policy_json, opens_at, closes_at, reveal_at,
				       created_at, canceled, finalized_at, winner_job_id
				FROM competition_round
				WHERE competition_id = $1 AND canceled = false
				ORDER BY epoch_number DESC
				LIMIT 1
			`, settings.CompetitionId)
			round, err = scanRound(row)
			if errors.Is(err, pgx.ErrNoRows) {
				round, err = nil, nil
			} else {
				server.Raise(err)
			}
		})
	})
	if err == nil && round != nil {
		setRoundStatus(round, self.nowUtc())
	}
	return round, err
}

func (self PostgresStore) GetRound(ctx context.Context, settings *Settings, roundId server.Id) (round *roundRecord, err error) {
	var stateErr error
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			round, err = scanRound(conn.QueryRow(ctx, `
				SELECT round_id, competition_id, epoch_number, workload_commitment, seed_nonce,
				       seed_ciphertext, providers_sha256, providers_path,
				       policy_json, opens_at, closes_at, reveal_at,
				       created_at, canceled, finalized_at, winner_job_id
				FROM competition_round
				WHERE round_id = $1 AND competition_id = $2
			`, roundId, settings.CompetitionId))
			if errors.Is(err, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(err)
		})
	})
	if err == nil && stateErr != nil {
		return nil, stateErr
	}
	if err == nil {
		setRoundStatus(round, self.nowUtc())
	}
	return round, err
}

const nextCandidateReviewSql = `
	WITH eligible AS (
		SELECT job_id, patch_sha256, patch_bytes, submitted_at, score_json,
		       CAST(row_number() OVER (
		           ORDER BY (score_json->>'normalized_score')::numeric DESC,
		                    (score_json->>'raw_score')::numeric ASC,
		                    submitted_at, job_id
		       ) AS integer) AS candidate_rank
		FROM competition_job
		WHERE round_id = $1 AND state = 'succeeded'
		  AND score_json @> '{"placeable":true,"takeover_eligible":true}'::jsonb
		  AND score_json @> '{"significance":{"statistically_significant":true,"recommended_next_epoch_takeover_margin_supported":true}}'::jsonb
		  AND jsonb_typeof(score_json->'gates') = 'object'
		  AND score_json->'gates' <> '{}'::jsonb
		  AND NOT EXISTS (
		      SELECT 1 FROM jsonb_each(score_json->'gates') AS gate
		      WHERE NOT COALESCE((gate.value->>'passed')::boolean, false)
		  )
	)
	SELECT eligible.candidate_rank, eligible.job_id, eligible.patch_sha256,
	       eligible.patch_bytes, eligible.submitted_at, eligible.score_json
	FROM eligible
	LEFT JOIN competition_candidate_review AS review
	  ON review.round_id = $1 AND review.job_id = eligible.job_id
	WHERE review.job_id IS NULL
	ORDER BY eligible.candidate_rank
	LIMIT 1
`

func validateCandidateReviewDecision(decision CandidateReviewDecision) error {
	decision.Reason = strings.TrimSpace(decision.Reason)
	if decision.JobId == (server.Id{}) {
		return errors.New("candidate review job id is required")
	}
	if decision.Decision != "approved" && decision.Decision != "rejected" {
		return errors.New("candidate review decision must be approved or rejected")
	}
	if !workerIdPattern.MatchString(decision.ReviewerId) {
		return errors.New("candidate review reviewer id is invalid")
	}
	if decision.Reason == "" || 4096 < len(decision.Reason) || strings.ContainsRune(decision.Reason, '\x00') {
		return errors.New("candidate review reason must contain 1..4096 bytes")
	}
	if !sha256Pattern.MatchString(decision.EvidenceSha256) ||
		len(decision.Evidence) == 0 || len(decision.Evidence) > 1024*1024 ||
		!json.Valid(decision.Evidence) {
		return errors.New("candidate review evidence must be valid JSON of at most 1 MiB with a SHA-256 digest")
	}
	var evidenceObject map[string]json.RawMessage
	if err := json.Unmarshal(decision.Evidence, &evidenceObject); err != nil || evidenceObject == nil {
		return errors.New("candidate review evidence must be a JSON object")
	}
	digest := sha256.Sum256(decision.Evidence)
	if hex.EncodeToString(digest[:]) != decision.EvidenceSha256 {
		return errors.New("candidate review evidence hash mismatch")
	}
	return nil
}

func candidateReviewState(round *roundRecord, status string, rejectedCount int) *CandidateReviewState {
	return &CandidateReviewState{
		CompetitionId: round.CompetitionId,
		RoundId:       round.RoundId,
		Epoch:         round.Epoch,
		Status:        status,
		RejectedCount: rejectedCount,
		FinalizedAt:   round.FinalizedAt,
		WinnerJobId:   round.WinnerJobId,
	}
}

func scanNextCandidate(row pgx.Row) (*CandidateReviewCandidate, error) {
	candidate := &CandidateReviewCandidate{}
	var scoreBytes []byte
	if err := row.Scan(
		&candidate.Rank,
		&candidate.JobId,
		&candidate.PatchSha256,
		&candidate.Patch,
		&candidate.SubmittedAt,
		&scoreBytes,
	); err != nil {
		return nil, err
	}
	if !sha256Pattern.MatchString(candidate.PatchSha256) || len(candidate.Patch) == 0 {
		return nil, errors.New("candidate review patch identity is invalid")
	}
	digest := sha256.Sum256(candidate.Patch)
	if hex.EncodeToString(digest[:]) != candidate.PatchSha256 {
		return nil, errors.New("candidate review patch hash mismatch")
	}
	if err := json.Unmarshal(scoreBytes, &candidate.Score); err != nil {
		return nil, fmt.Errorf("decode candidate review score: %w", err)
	}
	if err := validateScore(&candidate.Score); err != nil {
		return nil, fmt.Errorf("validate candidate review score: %w", err)
	}
	return candidate, nil
}

func loadCandidateReviewRound(
	ctx context.Context,
	tx server.PgTx,
	settings *Settings,
	epoch int,
) (*roundRecord, error) {
	return scanRound(tx.QueryRow(ctx, `
		SELECT round_id, competition_id, epoch_number, workload_commitment, seed_nonce,
		       seed_ciphertext, providers_sha256, providers_path,
		       policy_json, opens_at, closes_at, reveal_at,
		       created_at, canceled, finalized_at, winner_job_id
		FROM competition_round
		WHERE competition_id = $1 AND epoch_number = $2 AND canceled = false
		FOR UPDATE
	`, settings.CompetitionId, epoch))
}

func countRejectedCandidates(ctx context.Context, tx server.PgTx, roundId server.Id) int {
	var count int
	server.Raise(tx.QueryRow(ctx, `
		SELECT count(*) FROM competition_candidate_review
		WHERE round_id = $1 AND decision = 'rejected'
	`, roundId).Scan(&count))
	return count
}

func roundReadyForCandidateReview(ctx context.Context, tx server.PgTx, round *roundRecord, now time.Time) bool {
	if now.Before(round.ClosesAt) {
		return false
	}
	var active int
	server.Raise(tx.QueryRow(ctx, `
		SELECT count(*) FROM competition_job
		WHERE round_id = $1 AND state IN ('queued', 'running')
	`, round.RoundId).Scan(&active))
	return active == 0
}

func nextCandidateReview(ctx context.Context, tx server.PgTx, roundId server.Id) (*CandidateReviewCandidate, error) {
	candidate, err := scanNextCandidate(tx.QueryRow(ctx, nextCandidateReviewSql, roundId))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return candidate, err
}

func finalizeCandidateReviewRound(
	ctx context.Context,
	tx server.PgTx,
	round *roundRecord,
	winnerJobId *server.Id,
	now time.Time,
) {
	server.RaisePgResult(tx.Exec(ctx, `
		UPDATE competition_round
		SET finalized_at = $2, winner_job_id = $3
		WHERE round_id = $1
	`, round.RoundId, now, winnerJobId))
	round.FinalizedAt = &now
	round.WinnerJobId = winnerJobId
	setRoundStatus(round, now)
}

// PrepareCandidateReview seals the evaluation phase for one epoch without
// publishing a statistically significant candidate as the winner. If no
// eligible candidate remains, the epoch is safely finalized with no winner.
// Otherwise the exact next-ranked candidate stays embargoed for operator
// honesty review.
func (self PostgresStore) PrepareCandidateReview(
	ctx context.Context,
	settings *Settings,
	epoch int,
) (state *CandidateReviewState, err error) {
	now := self.nowUtc()
	finalized := false
	var stateErr error
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-finalize-v2', 0))`))
			round, scanErr := loadCandidateReviewRound(ctx, tx, settings, epoch)
			if errors.Is(scanErr, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(scanErr)
			rejectedCount := countRejectedCandidates(ctx, tx, round.RoundId)
			if round.FinalizedAt != nil {
				state = candidateReviewState(round, "finalized", rejectedCount)
				return
			}
			if !roundReadyForCandidateReview(ctx, tx, round, now) {
				state = candidateReviewState(round, "evaluating", rejectedCount)
				return
			}
			candidate, candidateErr := nextCandidateReview(ctx, tx, round.RoundId)
			server.Raise(candidateErr)
			if candidate == nil {
				finalizeCandidateReviewRound(ctx, tx, round, nil, now)
				state = candidateReviewState(round, "finalized", rejectedCount)
				finalized = true
				return
			}
			state = candidateReviewState(round, "pending_review", rejectedCount)
			state.Candidate = candidate
		})
	})
	if err == nil && stateErr != nil {
		return nil, stateErr
	}
	if err == nil && finalized {
		competitionRoundEvents.WithLabelValues("finalized").Inc()
	}
	return state, err
}

// RecordCandidateReview appends an authenticated honesty decision for exactly
// the current ranked candidate. Rejections advance in score order. Approval is
// atomic with winner publication, and rejecting the last candidate finalizes
// the epoch with no winner.
func (self PostgresStore) RecordCandidateReview(
	ctx context.Context,
	settings *Settings,
	epoch int,
	decision CandidateReviewDecision,
) (state *CandidateReviewState, err error) {
	decision.Reason = strings.TrimSpace(decision.Reason)
	if err := validateCandidateReviewDecision(decision); err != nil {
		return nil, err
	}
	now := self.nowUtc()
	finalized := false
	var stateErr error
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-finalize-v2', 0))`))
			round, scanErr := loadCandidateReviewRound(ctx, tx, settings, epoch)
			if errors.Is(scanErr, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(scanErr)
			if round.FinalizedAt != nil {
				stateErr = ErrConflict
				return
			}
			if !roundReadyForCandidateReview(ctx, tx, round, now) {
				stateErr = ErrReviewNotReady
				return
			}
			candidate, candidateErr := nextCandidateReview(ctx, tx, round.RoundId)
			server.Raise(candidateErr)
			if candidate == nil || candidate.JobId != decision.JobId {
				stateErr = ErrReviewOutOfOrder
				return
			}
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO competition_candidate_review (
					round_id, job_id, candidate_rank, decision, reviewer_id,
					reason, evidence_json, evidence_sha256, reviewed_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7::json, $8, $9)
			`,
				round.RoundId,
				candidate.JobId,
				candidate.Rank,
				decision.Decision,
				decision.ReviewerId,
				decision.Reason,
				string(decision.Evidence),
				decision.EvidenceSha256,
				now,
			))
			rejectedCount := countRejectedCandidates(ctx, tx, round.RoundId)
			if decision.Decision == "approved" {
				winnerJobId := candidate.JobId
				finalizeCandidateReviewRound(ctx, tx, round, &winnerJobId, now)
				state = candidateReviewState(round, "finalized", rejectedCount)
				finalized = true
				return
			}
			next, nextErr := nextCandidateReview(ctx, tx, round.RoundId)
			server.Raise(nextErr)
			if next == nil {
				finalizeCandidateReviewRound(ctx, tx, round, nil, now)
				state = candidateReviewState(round, "finalized", rejectedCount)
				finalized = true
				return
			}
			state = candidateReviewState(round, "pending_review", rejectedCount)
			state.Candidate = next
		})
	})
	if err == nil && stateErr != nil {
		return nil, stateErr
	}
	if err == nil && finalized {
		competitionRoundEvents.WithLabelValues("finalized").Inc()
	}
	return state, err
}

// RequirePromotionDecision is the final source-promotion interlock. The
// promotion CLI may only carry forward a finalized no-winner epoch or apply the
// exact job that has an append-only approved honesty review.
func (self PostgresStore) RequirePromotionDecision(
	ctx context.Context,
	settings *Settings,
	epoch int,
	winnerJobId *server.Id,
) (candidate *CandidateReviewCandidate, err error) {
	var stateErr error
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var finalizedAt *time.Time
			var recordedWinner *server.Id
			scanErr := conn.QueryRow(ctx, `
				SELECT finalized_at, winner_job_id
				FROM competition_round
				WHERE competition_id = $1 AND epoch_number = $2 AND canceled = false
			`, settings.CompetitionId, epoch).Scan(&finalizedAt, &recordedWinner)
			if errors.Is(scanErr, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(scanErr)
			if finalizedAt == nil {
				stateErr = ErrReviewNotReady
				return
			}
			if winnerJobId == nil {
				if recordedWinner != nil {
					stateErr = ErrConflict
				}
				return
			}
			if recordedWinner == nil || *recordedWinner != *winnerJobId {
				stateErr = ErrConflict
				return
			}
			candidate, scanErr = scanNextCandidate(conn.QueryRow(ctx, `
				SELECT review.candidate_rank, job.job_id, job.patch_sha256,
				       job.patch_bytes, job.submitted_at, job.score_json
				FROM competition_candidate_review AS review
				JOIN competition_job AS job ON job.job_id = review.job_id
				JOIN competition_round AS round ON round.round_id = review.round_id
				WHERE round.competition_id = $1 AND round.epoch_number = $2
				  AND review.job_id = $3 AND review.decision = 'approved'
			`, settings.CompetitionId, epoch, *winnerJobId))
			if errors.Is(scanErr, pgx.ErrNoRows) {
				stateErr = ErrConflict
				return
			}
			server.Raise(scanErr)
		})
	})
	if err == nil && stateErr != nil {
		return nil, stateErr
	}
	return candidate, err
}

func (self PostgresStore) Leaderboards(ctx context.Context, settings *Settings) (result *SeasonLeaderboardResult, err error) {
	result = &SeasonLeaderboardResult{CompetitionId: settings.CompetitionId, Epochs: []LeaderboardResult{}}
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, queryErr := conn.Query(ctx, `
				SELECT round_id, epoch_number, finalized_at, winner_job_id
				FROM competition_round
				WHERE competition_id = $1 AND canceled = false AND finalized_at IS NOT NULL
				  AND reveal_at <= $2
				ORDER BY epoch_number
			`, settings.CompetitionId, self.nowUtc())
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					var board LeaderboardResult
					server.Raise(rows.Scan(&board.RoundId, &board.Epoch, &board.FinalizedAt, &board.WinnerJobId))
					board.CompetitionId = settings.CompetitionId
					board.Status = "finalized"
					board.Entries = []LeaderboardEntry{}
					result.Epochs = append(result.Epochs, board)
				}
			})
			for boardIndex := range result.Epochs {
				board := &result.Epochs[boardIndex]
				jobRows, jobsErr := conn.Query(ctx, `
					SELECT job.job_id, job.patch_sha256, job.submitted_at,
					       job.score_json, count(principal.principal_id),
					       COALESCE(review.decision, 'not_reviewed')
					FROM competition_job AS job
					JOIN competition_job_principal AS principal ON principal.job_id = job.job_id
					LEFT JOIN competition_candidate_review AS review
					  ON review.round_id = job.round_id AND review.job_id = job.job_id
					WHERE job.round_id = $1 AND job.state = 'succeeded'
					GROUP BY job.job_id, review.decision
					ORDER BY (job.score_json->>'normalized_score')::numeric DESC,
					         (job.score_json->>'raw_score')::numeric ASC,
					         job.submitted_at, job.job_id
				`, board.RoundId)
				server.WithPgResult(jobRows, jobsErr, func() {
					for jobRows.Next() {
						var entry LeaderboardEntry
						var scoreBytes []byte
						server.Raise(jobRows.Scan(
							&entry.JobId, &entry.PatchSha256, &entry.SubmittedAt,
							&scoreBytes, &entry.SubmitterCount, &entry.HonestyReview,
						))
						server.Raise(json.Unmarshal(scoreBytes, &entry.Score))
						server.Raise(validateScore(&entry.Score))
						entry.Rank = len(board.Entries) + 1
						entry.Winner = board.WinnerJobId != nil && *board.WinnerJobId == entry.JobId
						board.Entries = append(board.Entries, entry)
					}
				})
			}
		})
	})
	return result, err
}

func scanRound(row pgx.Row) (*roundRecord, error) {
	round := &roundRecord{}
	var policy []byte
	err := row.Scan(
		&round.RoundId, &round.CompetitionId, &round.Epoch, &round.WorkloadCommitment,
		&round.SeedNonce, &round.SeedCiphertext, &round.ProvidersSha256,
		&round.ProvidersPath, &policy, &round.OpensAt,
		&round.ClosesAt, &round.RevealAt, &round.CreatedAt, &round.Canceled,
		&round.FinalizedAt, &round.WinnerJobId,
	)
	round.PolicyJson = policy
	round.ScoreSchema = ScoreSchema
	return round, err
}

func setRoundStatus(round *roundRecord, now time.Time) {
	switch {
	case round.Canceled:
		round.Status = "canceled"
	case now.Before(round.OpensAt):
		round.Status = "scheduled"
	case now.Before(round.ClosesAt):
		round.Status = "open"
	case round.FinalizedAt == nil:
		round.Status = "grading"
	case now.Before(round.RevealAt):
		round.Status = "finalized"
	default:
		round.Status = "finalized"
	}
}

func cacheKey(roundId server.Id, patch []byte) string {
	h := sha256.New()
	h.Write([]byte("urnetwork-sim-latency-cache-v1\x00"))
	h.Write(roundId.Bytes())
	h.Write(patch)
	return hex.EncodeToString(h.Sum(nil))
}

func (self PostgresStore) Enqueue(
	ctx context.Context,
	settings *Settings,
	roundId server.Id,
	patch *CanonicalPatch,
	principalId string,
	apiImageDigest string,
) (job *queuedJob, cacheHit bool, err error) {
	if _, identityErr := validateRuntimeImageDigest(apiImageDigest); identityErr != nil {
		return nil, false, identityErr
	}
	if settings == nil || settings.artifactArchive == nil {
		return nil, false, errors.New("competition submission archive is unavailable")
	}
	now := self.nowUtc()
	var stateErr error
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-submit-v1', 0))`))
			round, scanErr := scanRound(tx.QueryRow(ctx, `
				SELECT round_id, competition_id, epoch_number, workload_commitment, seed_nonce,
				       seed_ciphertext, providers_sha256, providers_path,
				       policy_json, opens_at, closes_at, reveal_at,
				       created_at, canceled, finalized_at, winner_job_id
				FROM competition_round WHERE round_id = $1 FOR SHARE
			`, roundId))
			if errors.Is(scanErr, pgx.ErrNoRows) || round.CompetitionId != settings.CompetitionId {
				stateErr = ErrNotFound
				return
			}
			server.Raise(scanErr)
			if round.Canceled || !submissionWithinEpoch(round, now) {
				stateErr = ErrRoundClosed
				return
			}
			key := cacheKey(roundId, patch.Bytes)
			job, scanErr = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.cache_key = $1`, key), true, self.nowUtc())
			if scanErr == nil {
				cacheHit = true
				addPrincipal(ctx, tx, job.JobId, principalId, now)
				appendEvent(ctx, tx, job.JobId, now, "cache_hit", principalId, map[string]any{
					"cache_key": key, "api_image_digest": apiImageDigest,
				})
				return
			}
			if !errors.Is(scanErr, pgx.ErrNoRows) {
				server.Raise(scanErr)
			}
			jobId := server.NewId()
			submissionArtifact, archiveErr := settings.artifactArchive.ArchiveSubmission(
				ctx,
				settings,
				roundId,
				patch,
			)
			server.Raise(archiveErr)
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO competition_job (
					job_id, round_id, patch_bytes, patch_sha256, cache_key, state,
					submitted_at, available_at, artifact_retain_until, api_image_digest
				) VALUES ($1, $2, $3, $4, $5, 'queued', $6, $6, $7, $8)
			`, jobId, roundId, patch.Bytes, patch.Sha256, key, now, settings.RetainUntil, apiImageDigest))
			addPrincipal(ctx, tx, jobId, principalId, now)
			appendEvent(ctx, tx, jobId, now, "submitted", principalId, map[string]any{
				"round_id": roundId.String(), "patch_sha256": patch.Sha256, "cache_key": key,
				"api_image_digest": apiImageDigest, "submission_artifact": submissionArtifact,
			})
			job, scanErr = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.job_id = $1`, jobId), true, self.nowUtc())
			server.Raise(scanErr)
		})
	})
	if err == nil && stateErr != nil {
		err = stateErr
	}
	if err == nil && job != nil && job.State == "queued" {
		err = enqueueCompetitionJob(ctx, settings, job.JobId)
	}
	return job, cacheHit, err
}

const jobSelect = `
	SELECT j.job_id, j.round_id, j.patch_sha256, j.state, j.submitted_at,
	       j.started_at, j.completed_at, j.cache_key, j.score_json,
	       j.eval_error_json, j.patch_bytes, j.attempt_count, COALESCE(j.lease_owner, ''),
	       j.lease_expires_at, j.api_image_digest, COALESCE(j.worker_image_digest, ''),
	       r.competition_id, r.workload_commitment, r.seed_nonce,
	       r.seed_ciphertext, r.providers_sha256, r.providers_path,
	       r.policy_json, r.opens_at, r.closes_at,
	       r.reveal_at, r.created_at, r.canceled, r.epoch_number,
	       r.finalized_at, r.winner_job_id
	FROM competition_job j JOIN competition_round r ON r.round_id = j.round_id
`

func scanJob(row pgx.Row, includePatch bool, now time.Time) (*queuedJob, error) {
	job := &queuedJob{}
	var scoreJson, errorJson, policyJson []byte
	err := row.Scan(
		&job.JobId, &job.RoundId, &job.PatchSha256, &job.State,
		&job.SubmittedAt, &job.StartedAt, &job.CompletedAt, &job.CacheKey,
		&scoreJson, &errorJson, &job.Patch, &job.AttemptCount, &job.LeaseOwner,
		&job.LeaseExpiresAt, &job.ApiImageDigest, &job.WorkerImageDigest,
		&job.Round.CompetitionId,
		&job.Round.WorkloadCommitment, &job.Round.SeedNonce,
		&job.Round.SeedCiphertext, &job.Round.ProvidersSha256,
		&job.Round.ProvidersPath, &policyJson, &job.Round.OpensAt,
		&job.Round.ClosesAt, &job.Round.RevealAt, &job.Round.CreatedAt,
		&job.Round.Canceled, &job.Round.Epoch, &job.Round.FinalizedAt,
		&job.Round.WinnerJobId,
	)
	if err != nil {
		return nil, err
	}
	job.Round.RoundId = job.RoundId
	job.Round.ScoreSchema = ScoreSchema
	job.Round.PolicyJson = policyJson
	job.EvaluatorImageDigest, err = evaluatorImageDigestFromPolicy(policyJson)
	if err != nil {
		return nil, err
	}
	setRoundStatus(&job.Round, now)
	if len(scoreJson) != 0 {
		job.Score = &ScoreResult{}
		if err := json.Unmarshal(scoreJson, job.Score); err != nil {
			return nil, fmt.Errorf("decode stored score: %w", err)
		}
	}
	if len(errorJson) != 0 {
		job.EvalError = &CompetitionError{}
		if err := json.Unmarshal(errorJson, job.EvalError); err != nil {
			return nil, fmt.Errorf("decode stored evaluation error: %w", err)
		}
	}
	if !includePatch {
		job.Patch = nil
	}
	return job, nil
}

func (self PostgresStore) GetJob(ctx context.Context, settings *Settings, jobId server.Id, principal *Principal) (job *queuedJob, err error) {
	var stateErr error
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			query := jobSelect + ` WHERE j.job_id = $1 AND r.competition_id = $2`
			args := []any{jobId, settings.CompetitionId}
			if principal.Role != "operator" {
				query += ` AND EXISTS (SELECT 1 FROM competition_job_principal p WHERE p.job_id = j.job_id AND p.principal_id = $3)`
				args = append(args, principal.Id)
			}
			job, err = scanJob(conn.QueryRow(ctx, query, args...), false, self.nowUtc())
			if errors.Is(err, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(err)
		})
	})
	if err == nil && stateErr != nil {
		err = stateErr
	}
	return job, err
}

func (self PostgresStore) Readiness(ctx context.Context, settings *Settings) (checks map[string]bool, err error) {
	checks = map[string]bool{
		"configuration":                true,
		"frozen_policy":                true,
		"retention_window":             !settings.RetainUntil.Before(settings.SeasonEndsAt),
		"database":                     false,
		"fifo_slot":                    false,
		"queue_admission":              true,
		"authoritative_evaluator_host": false,
		"artifact_storage":             false,
		"host_rebaseline":              false,
	}
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var one int
			server.Raise(conn.QueryRow(ctx, `SELECT 1`).Scan(&one))
			checks["database"] = one == 1
			var slots int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_worker_slot WHERE slot_id = 1`).Scan(&slots))
			checks["fifo_slot"] = slots == 1
			var hosts, artifactHosts, rebaselineHosts int
			server.Raise(conn.QueryRow(ctx, `
				WITH current_round AS (
					SELECT round_id::text AS round_id
					FROM competition_round
					WHERE competition_id = $6 AND canceled = false AND $5 < closes_at
					ORDER BY opens_at
					LIMIT 1
				)
				SELECT count(*),
				       count(*) FILTER (WHERE (self_check_json->>'artifact_storage')::boolean),
				       count(*) FILTER (
				           WHERE (self_check_json->>'rebaseline_passed')::boolean
				             AND self_check_json->>'rebaseline_round_id' =
				                 COALESCE((SELECT round_id FROM current_round), '')
				       )
				FROM competition_evaluator_host
				WHERE eligible = true AND hardware_id = $1 AND image_digest = $2
				  AND self_check_json->>'qualification_sha256' = $3
				  AND heartbeat_at >= $4
			`, settings.EvaluationPolicy.HardwareId, settings.EvaluatorImageDigest,
				settings.EvaluationPolicy.HostQualificationSha256,
				self.nowUtc().Add(-time.Duration(settings.HostHeartbeatMaxAgeSeconds)*time.Second),
				self.nowUtc(), settings.CompetitionId).Scan(
				&hosts, &artifactHosts, &rebaselineHosts,
			))
			checks["authoritative_evaluator_host"] = 1 <= hosts
			checks["artifact_storage"] = 1 <= artifactHosts
			checks["host_rebaseline"] = 1 <= rebaselineHosts
		})
	})
	if err == nil {
		checks["queue_admission"] = checkCompetitionFifo(ctx, settings) == nil
	}
	return checks, err
}

func (self PostgresStore) RegisterHost(ctx context.Context, settings *Settings, selfCheck HostSelfCheck) error {
	bytes, err := json.Marshal(selfCheck)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(bytes)
	eligible := selfCheck.Eligible(settings)
	return captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO competition_evaluator_host (
					host_id, hardware_id, image_digest, self_check_json,
					self_check_sha256, eligible, heartbeat_at
				) VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
				ON CONFLICT (host_id) DO UPDATE SET
					hardware_id = EXCLUDED.hardware_id,
					image_digest = EXCLUDED.image_digest,
					self_check_json = EXCLUDED.self_check_json,
					self_check_sha256 = EXCLUDED.self_check_sha256,
					eligible = EXCLUDED.eligible,
					heartbeat_at = EXCLUDED.heartbeat_at
			`, selfCheck.HostId, selfCheck.HardwareId, selfCheck.ImageDigest,
				string(bytes), hex.EncodeToString(digest[:]), eligible, self.nowUtc()))
		}, server.OptReadWrite())
	})
}

func (self PostgresStore) Claim(ctx context.Context, settings *Settings, workerId string, workerImageDigest string) (job *queuedJob, err error) {
	if _, identityErr := validateRuntimeImageDigest(workerImageDigest); identityErr != nil {
		return nil, identityErr
	}
	now := self.nowUtc()
	leaseUntil := now.Add(time.Duration(settings.WorkerLeaseSeconds) * time.Second)

	if _, err := dequeueCompetitionJob(ctx, settings); err != nil {
		return nil, err
	}
	discarded := []server.Id{}
	discardError, marshalErr := json.Marshal(submissionError(
		"outside_epoch_window",
		"submission timestamp is outside the epoch admission window",
	))
	if marshalErr != nil {
		return nil, marshalErr
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			var slotWorker *string
			var slotJob *server.Id
			var slotLease *time.Time
			server.Raise(tx.QueryRow(ctx, `
				SELECT worker_id, job_id, lease_expires_at
				FROM competition_worker_slot WHERE slot_id = 1 FOR UPDATE
			`).Scan(&slotWorker, &slotJob, &slotLease))

			if slotWorker != nil && slotLease != nil && now.Before(*slotLease) {
				return
			}
			rows, queryErr := tx.Query(ctx, `
				UPDATE competition_job AS job
				SET state = 'failed', completed_at = $2,
				    eval_error_json = $3::jsonb,
				    lease_owner = NULL, lease_expires_at = NULL
				FROM competition_round AS round
				WHERE job.round_id = round.round_id
				  AND round.competition_id = $1
				  AND job.state = 'queued'
				  AND (job.submitted_at < round.opens_at OR round.closes_at <= job.submitted_at)
				RETURNING job.job_id
			`, settings.CompetitionId, now, string(discardError))
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					var jobId server.Id
					server.Raise(rows.Scan(&jobId))
					discarded = append(discarded, jobId)
				}
			})
			for _, jobId := range discarded {
				appendEvent(ctx, tx, jobId, now, "discarded_outside_epoch", workerId, map[string]any{
					"reason": "submitted_at_outside_start_end",
				})
			}
			row := tx.QueryRow(ctx, jobSelect+`
				WHERE (
				        (j.state = 'queued' AND j.available_at <= $1) OR
				        (j.state = 'running' AND j.lease_expires_at <= $1)
				      )
				  AND r.canceled = false
				  AND r.opens_at <= $1
				  AND r.opens_at <= j.submitted_at
				  AND j.submitted_at < r.closes_at
				  AND r.finalized_at IS NULL
				ORDER BY j.submitted_at, j.job_id
				LIMIT 1 FOR UPDATE OF j SKIP LOCKED
			`, now)
			var scanErr error
			job, scanErr = scanJob(row, true, self.nowUtc())
			if errors.Is(scanErr, pgx.ErrNoRows) {
				server.RaisePgResult(tx.Exec(ctx, `
					UPDATE competition_worker_slot SET worker_id = NULL, job_id = NULL,
					lease_expires_at = NULL, heartbeat_at = $1 WHERE slot_id = 1
				`, now))
				job = nil
				return
			}
			server.Raise(scanErr)
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_job SET state = 'running', started_at = COALESCE(started_at, $2),
					lease_owner = $3, lease_expires_at = $4, attempt_count = attempt_count + 1,
					worker_image_digest = $5
				WHERE job_id = $1
			`, job.JobId, now, workerId, leaseUntil, workerImageDigest))
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_worker_slot SET worker_id = $1, job_id = $2,
					lease_expires_at = $3, heartbeat_at = $4 WHERE slot_id = 1
			`, workerId, job.JobId, leaseUntil, now))
			appendEvent(ctx, tx, job.JobId, now, "claimed", workerId, map[string]any{
				"attempt": job.AttemptCount + 1, "worker_image_digest": workerImageDigest,
			})
			job.State = "running"
			job.AttemptCount++
			job.LeaseOwner = workerId
			job.LeaseExpiresAt = &leaseUntil
			job.WorkerImageDigest = workerImageDigest
			if job.StartedAt == nil {
				job.StartedAt = &now
			}
		})
	})
	if err == nil {
		for _, jobId := range discarded {
			if removeErr := removeCompetitionJobSignal(ctx, settings, jobId); removeErr != nil {
				return nil, removeErr
			}
		}
	}
	return job, err
}

func (self PostgresStore) Heartbeat(ctx context.Context, settings *Settings, workerId string, jobId server.Id) error {
	now := self.nowUtc()
	leaseUntil := now.Add(time.Duration(settings.WorkerLeaseSeconds) * time.Second)
	leaseLost := false
	err := captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Exec(ctx, `
				UPDATE competition_worker_slot SET lease_expires_at = $3, heartbeat_at = $4
				WHERE slot_id = 1 AND worker_id = $1 AND job_id = $2
			`, workerId, jobId, leaseUntil, now)
			server.Raise(err)
			if result.RowsAffected() != 1 {
				leaseLost = true
				return
			}
			result, err = tx.Exec(ctx, `
				UPDATE competition_job SET lease_expires_at = $3
				WHERE job_id = $1 AND state = 'running' AND lease_owner = $2
			`, jobId, workerId, leaseUntil)
			server.Raise(err)
			if result.RowsAffected() != 1 {
				leaseLost = true
				return
			}
		})
	})
	if err == nil && leaseLost {
		return ErrLeaseLost
	}
	return err
}

type EvaluationOutcome struct {
	Score            *ScoreResult
	Error            *CompetitionError
	ArtifactManifest json.RawMessage
	Infrastructure   bool
}

// Schedules an infrastructure retry only when it can begin before the one
// submission-wide execution deadline. Queue wait before the first claim is not
// charged, while every attempt and retry backoff after it is charged.
func infrastructureRetrySchedule(
	settings *Settings,
	startedAt time.Time,
	completedAt time.Time,
	attempts int,
) (time.Time, bool) {
	retryAt := completedAt.Add(time.Duration(attempts*attempts) * 15 * time.Second)
	deadline := startedAt.Add(time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
	return retryAt, attempts < settings.MaxInfrastructureAttempts && retryAt.Before(deadline)
}

func (self PostgresStore) Complete(ctx context.Context, settings *Settings, workerId string, jobId server.Id, outcome EvaluationOutcome) (retry bool, err error) {
	now := self.nowUtc()
	leaseLost := false
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			var state, owner string
			var attempts int
			var apiImageDigest, workerImageDigest string
			var startedAt time.Time
			server.Raise(tx.QueryRow(ctx, `
				SELECT state, COALESCE(lease_owner, ''), attempt_count,
				       api_image_digest, COALESCE(worker_image_digest, ''), started_at
				FROM competition_job WHERE job_id = $1 FOR UPDATE
			`, jobId).Scan(&state, &owner, &attempts, &apiImageDigest, &workerImageDigest, &startedAt))
			if state != "running" || owner != workerId {
				leaseLost = true
				return
			}
			if !imageDigestPattern.MatchString(apiImageDigest) || !imageDigestPattern.MatchString(workerImageDigest) {
				panic(errors.New("competition job runtime image identity is invalid"))
			}
			retryAt, retryBudgetAvailable := infrastructureRetrySchedule(settings, startedAt, now, attempts)
			if outcome.Infrastructure && attempts < settings.MaxInfrastructureAttempts && !retryBudgetAvailable {
				outcome.Score = nil
				outcome.Error = infrastructureError(
					"evaluation_time_budget_exhausted",
					"submission exhausted its total evaluation time budget",
				)
			}
			scoreJson, errorJson, manifestJson := nullableJson(outcome.Score), nullableJson(outcome.Error), []byte(outcome.ArtifactManifest)
			manifestHash := any(nil)
			if len(manifestJson) != 0 {
				if !json.Valid(manifestJson) {
					panic(errors.New("artifact manifest is invalid JSON"))
				}
				h := sha256.Sum256(manifestJson)
				manifestHash = hex.EncodeToString(h[:])
			}
			if outcome.Infrastructure && retryBudgetAvailable {
				retry = true
				server.RaisePgResult(tx.Exec(ctx, `
					UPDATE competition_job SET state = 'queued', available_at = $2,
						lease_owner = NULL, lease_expires_at = NULL
					WHERE job_id = $1
				`, jobId, retryAt))
				errorCode := "unknown_infrastructure_error"
				if outcome.Error != nil {
					errorCode = outcome.Error.Code
				}
				appendEvent(ctx, tx, jobId, now, "infrastructure_retry", workerId, map[string]any{
					"attempt": attempts, "error_code": errorCode,
					"artifact_manifest_sha256": manifestHash,
					"api_image_digest":         apiImageDigest, "worker_image_digest": workerImageDigest,
				})
			} else {
				terminal := "failed"
				if outcome.Score != nil && outcome.Error == nil {
					terminal = "succeeded"
				}
				server.RaisePgResult(tx.Exec(ctx, `
					UPDATE competition_job SET state = $2, completed_at = $3,
						lease_owner = NULL, lease_expires_at = NULL,
						score_json = $4::jsonb, eval_error_json = $5::jsonb,
						artifact_manifest_json = $6::jsonb,
						artifact_manifest_sha256 = $7
					WHERE job_id = $1
				`, jobId, terminal, now, scoreJson, errorJson, nullableBytes(manifestJson), manifestHash))
				appendEvent(ctx, tx, jobId, now, terminal, workerId, map[string]any{
					"attempt": attempts, "api_image_digest": apiImageDigest,
					"worker_image_digest": workerImageDigest,
				})
			}
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_worker_slot SET worker_id = NULL, job_id = NULL,
					lease_expires_at = NULL, heartbeat_at = $1
				WHERE slot_id = 1 AND worker_id = $2 AND job_id = $3
			`, now, workerId, jobId))
		})
	})
	if err == nil && leaseLost {
		err = ErrLeaseLost
	}
	if err == nil && retry {
		err = enqueueCompetitionJob(ctx, settings, jobId)
	}
	return retry, err
}

func (self PostgresStore) HandBack(ctx context.Context, workerId string, jobId server.Id, reason string) error {
	now := self.nowUtc()
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Exec(ctx, `
				UPDATE competition_job SET state = 'queued', available_at = $3,
					lease_owner = NULL, lease_expires_at = NULL
				WHERE job_id = $1 AND state = 'running' AND lease_owner = $2
			`, jobId, workerId, now)
			server.Raise(err)
			if result.RowsAffected() == 1 {
				appendEvent(ctx, tx, jobId, now, "handed_back", workerId, map[string]any{"reason": reason})
			}
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_worker_slot SET worker_id = NULL, job_id = NULL,
					lease_expires_at = NULL, heartbeat_at = $1
				WHERE slot_id = 1 AND worker_id = $2 AND job_id = $3
			`, now, workerId, jobId))
		})
	})
}

func addPrincipal(ctx context.Context, tx server.PgTx, jobId server.Id, principal string, at time.Time) {
	server.RaisePgResult(tx.Exec(ctx, `
		INSERT INTO competition_job_principal (job_id, principal_id, first_seen_at)
		VALUES ($1, $2, $3) ON CONFLICT (job_id, principal_id) DO NOTHING
	`, jobId, principal, at))
}

func appendEvent(ctx context.Context, tx server.PgTx, jobId server.Id, at time.Time, eventType, actor string, payload any) {
	bytes, err := json.Marshal(payload)
	server.Raise(err)
	h := sha256.Sum256(bytes)
	server.RaisePgResult(tx.Exec(ctx, `
		INSERT INTO competition_job_event (
			job_id, event_at, event_type, actor_id, payload_json, payload_sha256
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6)
	`, jobId, at, eventType, actor, string(bytes), hex.EncodeToString(h[:])))
}

func nullableJson(value any) any {
	if value == nil {
		return nil
	}
	bytes, err := json.Marshal(value)
	server.Raise(err)
	return string(bytes)
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func captureDatabaseError(run func()) error {
	if recovered := server.HandleError(run); recovered != nil {
		if err, ok := recovered.(error); ok {
			return err
		}
		return fmt.Errorf("%v", recovered)
	}
	return nil
}

const (
	ScoreSchema   = model.CompetitionScoreSchema
	ScorerVersion = model.CompetitionScorerVersion
)

type HealthResult = model.CompetitionHealthResult

type ReadinessResult = model.CompetitionReadinessResult

type PatchPolicy = model.CompetitionPatchPolicy

type EvaluationPolicy = model.CompetitionEvaluationPolicy

type SeasonPolicy = model.CompetitionSeasonPolicy

type InfoResult = model.CompetitionInfoResult

type GenerateRoundArgs = model.CompetitionGenerateRoundArgs

type RoundResult = model.CompetitionRoundResult

type SeasonLeaderboardResult = model.CompetitionSeasonLeaderboardResult

type LeaderboardResult = model.CompetitionLeaderboardResult

type LeaderboardEntry = model.CompetitionLeaderboardEntry

type CandidateReviewState = model.CompetitionCandidateReviewState

type CandidateReviewCandidate = model.CompetitionCandidateReviewCandidate

type CandidateReviewDecision = model.CompetitionCandidateReviewDecision

type ScoreArgs = model.CompetitionScoreArgs

type ScoreAcceptedResult = model.CompetitionScoreAcceptedResult

type ScoreJobResult = model.CompetitionScoreJobResult

type ScoreResult = model.CompetitionScoreResult

type ScoreSignificance = model.CompetitionScoreSignificance

type Gate = model.CompetitionGate

type CompetitionError = model.CompetitionError

type roundRecord struct {
	RoundResult
	CompetitionId  string
	SeedNonce      []byte
	SeedCiphertext []byte
	ProvidersPath  string
	PolicyJson     json.RawMessage
	Canceled       bool
}

type queuedJob struct {
	ScoreJobResult
	Patch          []byte
	PrincipalId    string
	AttemptCount   int
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	Round          roundRecord
}

type HostSelfCheck struct {
	Schema                      int             `json:"schema"`
	HostId                      string          `json:"host_id"`
	HardwareId                  string          `json:"hardware_id"`
	QualificationSha256         string          `json:"qualification_sha256"`
	ImageDigest                 string          `json:"image_digest"`
	KernelRelease               string          `json:"kernel_release"`
	MicrocodeRevision           string          `json:"microcode_revision"`
	LogicalCpuCount             int             `json:"logical_cpu_count"`
	SMTDisabled                 bool            `json:"smt_disabled"`
	GovernorPinned              bool            `json:"governor_pinned"`
	TurboPinned                 bool            `json:"turbo_pinned"`
	NumaPinned                  bool            `json:"numa_pinned"`
	IrqPinned                   bool            `json:"irq_pinned"`
	CgroupV2                    bool            `json:"cgroup_v2"`
	ServicesInJobCgroup         bool            `json:"services_in_job_cgroup"`
	DefaultDenyNetwork          bool            `json:"default_deny_network"`
	OfflineBuildCache           bool            `json:"offline_build_cache"`
	TemplateDatabase            bool            `json:"template_database"`
	RedisReset                  bool            `json:"redis_reset"`
	ArtifactStorage             bool            `json:"artifact_storage"`
	ImmutableReports            bool            `json:"immutable_reports"`
	NoProductionSecrets         bool            `json:"no_production_secrets"`
	CleanupVerified             bool            `json:"cleanup_verified"`
	ResourceLimitsVerified      bool            `json:"resource_limits_verified"`
	ManagementCpuReserved       bool            `json:"management_cpu_reserved"`
	ManagementMemoryReserved    bool            `json:"management_memory_reserved"`
	ResourceBombCleanupVerified bool            `json:"resource_bomb_cleanup_verified"`
	RebaselinePassed            bool            `json:"rebaseline_passed"`
	RebaselineRoundId           *server.Id      `json:"rebaseline_round_id,omitempty"`
	Checks                      map[string]bool `json:"checks"`
}

func (self HostSelfCheck) Eligible(settings *Settings) bool {
	return self.Schema == 1 &&
		self.HostId != "" &&
		self.HardwareId == settings.EvaluationPolicy.HardwareId &&
		self.QualificationSha256 == settings.EvaluationPolicy.HostQualificationSha256 &&
		self.ImageDigest == settings.EvaluatorImageDigest &&
		self.KernelRelease != "" && self.MicrocodeRevision != "" &&
		self.LogicalCpuCount == 12 &&
		self.SMTDisabled && self.GovernorPinned && self.TurboPinned && self.NumaPinned && self.IrqPinned &&
		self.CgroupV2 && self.ServicesInJobCgroup && self.DefaultDenyNetwork &&
		self.OfflineBuildCache && self.TemplateDatabase && self.RedisReset &&
		self.ArtifactStorage && self.ImmutableReports && self.NoProductionSecrets &&
		self.CleanupVerified && self.ResourceLimitsVerified &&
		self.ManagementCpuReserved && self.ManagementMemoryReserved &&
		self.ResourceBombCleanupVerified &&
		(!self.RebaselinePassed || self.RebaselineRoundId != nil) &&
		allChecks(self.Checks)
}

var workerIdPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Worker struct {
	settings          *Settings
	store             Store
	evaluator         Evaluator
	workerId          string
	workerImageDigest string
	pollEvery         time.Duration
}

func NewWorker(settings *Settings, store Store, evaluator Evaluator, workerId string) (*Worker, error) {
	workerImageDigest, err := runtimeImageDigest()
	if err != nil {
		return nil, err
	}
	return newWorkerWithImageDigest(settings, store, evaluator, workerId, workerImageDigest)
}

func newWorkerWithImageDigest(
	settings *Settings,
	store Store,
	evaluator Evaluator,
	workerId string,
	workerImageDigest string,
) (*Worker, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if store == nil || evaluator == nil {
		return nil, errors.New("competition worker requires a store and evaluator")
	}
	if !workerIdPattern.MatchString(workerId) {
		return nil, errors.New("worker id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
	}
	if _, err := validateRuntimeImageDigest(workerImageDigest); err != nil {
		return nil, err
	}
	return &Worker{
		settings: settings, store: store, evaluator: evaluator, workerId: workerId,
		workerImageDigest: workerImageDigest, pollEvery: time.Second,
	}, nil
}

func (self *Worker) Run(ctx context.Context) error {
	hostCheck, err := self.evaluator.SelfCheck(ctx, self.settings)
	if err != nil {

		if hostCheck.HostId != "" {
			_ = self.store.RegisterHost(context.WithoutCancel(ctx), self.settings, hostCheck)
		}
		return fmt.Errorf("competition evaluator self-check: %w", err)
	}
	if err := self.store.RegisterHost(ctx, self.settings, hostCheck); err != nil {
		return fmt.Errorf("register evaluator host: %w", err)
	}
	hostTicker := time.NewTicker(time.Duration(self.settings.WorkerHeartbeatSeconds) * time.Second)
	defer hostTicker.Stop()
	pollTicker := time.NewTicker(self.pollEvery)
	defer pollTicker.Stop()

	for {
		finished, err := self.finishEpoch(ctx)
		if err != nil {
			return fmt.Errorf("finish competition epoch: %w", err)
		}
		if finished {
			return nil
		}
		job, err := self.store.Claim(ctx, self.settings, self.workerId, self.workerImageDigest)
		if err != nil {
			return fmt.Errorf("claim competition job: %w", err)
		}
		if job != nil {
			if err := self.evaluateOne(ctx, job, hostCheck); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pollTicker.C:
		case <-hostTicker.C:
			fresh, checkErr := self.evaluator.SelfCheck(ctx, self.settings)
			if checkErr != nil {
				if fresh.HostId != "" {
					_ = self.store.RegisterHost(context.WithoutCancel(ctx), self.settings, fresh)
				}
				return fmt.Errorf("competition evaluator lost self-check: %w", checkErr)
			}
			if err := self.store.RegisterHost(ctx, self.settings, fresh); err != nil {
				return fmt.Errorf("refresh evaluator host: %w", err)
			}
			hostCheck = fresh
		}
	}
}

// Seals the one round owned by this process after admission closes and the
// immediate FIFO drains, including work that extends beyond closes_at. A
// significant candidate is left embargoed for the operator-controlled honesty
// review gate; the worker never selects or publishes a winner by itself.
func (self *Worker) finishEpoch(ctx context.Context) (bool, error) {
	latest, err := self.store.CurrentRound(ctx, self.settings)
	if err != nil {
		return false, err
	}
	if latest == nil {
		return false, nil
	}
	if latest.FinalizedAt != nil {
		return true, nil
	}
	state, err := self.store.PrepareCandidateReview(ctx, self.settings, latest.Epoch)
	if err != nil {
		return false, err
	}
	if state.Status == "pending_review" {
		glog.Infof(
			"[competition]epoch %d sealed round=%s candidate=%s rank=%d awaiting_honesty_review=true\n",
			state.Epoch,
			state.RoundId,
			state.Candidate.JobId,
			state.Candidate.Rank,
		)
		return true, nil
	}
	if state.Status == "finalized" {
		winner := "none"
		if state.WinnerJobId != nil {
			winner = state.WinnerJobId.String()
		}
		glog.Infof(
			"[competition]epoch %d finalized round=%s winner=%s\n",
			state.Epoch,
			state.RoundId,
			winner,
		)
		return true, nil
	}
	return false, nil
}

func (self *Worker) evaluateOne(parent context.Context, job *queuedJob, hostCheck HostSelfCheck) error {
	startedAt := time.Now()
	metricOutcome := "infrastructure_failed"
	defer func() {
		competitionEvaluationSeconds.Observe(time.Since(startedAt).Seconds())
		competitionEvaluations.WithLabelValues(metricOutcome).Inc()
	}()
	if !hostCheck.RebaselinePassed || hostCheck.RebaselineRoundId == nil || *hostCheck.RebaselineRoundId != job.RoundId {
		metricOutcome = "rebaseline_mismatch"
		_ = self.handBack(job.JobId, "round_rebaseline_mismatch")
		return fmt.Errorf("competition evaluator host is not re-baselined for round %s", job.RoundId)
	}
	if job.StartedAt == nil {
		_ = self.handBack(job.JobId, "missing_execution_start")
		return fmt.Errorf("competition job %s has no execution start", job.JobId)
	}
	executionDeadline := job.StartedAt.Add(
		time.Duration(self.settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second,
	)
	if !server.NowUtc().Before(executionDeadline) {
		outcome := EvaluationOutcome{
			Error: infrastructureError(
				"evaluation_time_budget_exhausted",
				"submission exhausted its total evaluation time budget",
			),
			Infrastructure: false,
		}
		retry, err := self.store.Complete(
			context.WithoutCancel(parent),
			self.settings,
			self.workerId,
			job.JobId,
			outcome,
		)
		if err != nil {
			return fmt.Errorf("complete expired competition job %s: %w", job.JobId, err)
		}
		if retry {
			return fmt.Errorf("expired competition job %s was retained for retry", job.JobId)
		}
		return nil
	}
	evalCtx, cancel := context.WithDeadline(parent, executionDeadline)
	defer cancel()
	type evalReturn struct{ outcome EvaluationOutcome }
	done := make(chan evalReturn, 1)
	go func() {
		done <- evalReturn{outcome: self.evaluator.Evaluate(evalCtx, self.settings, job)}
	}()
	heartbeatTicker := time.NewTicker(time.Duration(self.settings.WorkerHeartbeatSeconds) * time.Second)
	defer heartbeatTicker.Stop()
	for {
		select {
		case result := <-done:
			cancel()
			if result.outcome.Score != nil {
				if err := validateScore(result.outcome.Score); err != nil {
					result.outcome = EvaluationOutcome{
						Error:          infrastructureError("score_result_invalid", "pinned scorer returned an invalid result"),
						Infrastructure: true,
					}
				}
			}
			retry, err := self.store.Complete(context.WithoutCancel(parent), self.settings, self.workerId, job.JobId, result.outcome)
			if err != nil {
				return fmt.Errorf("complete competition job %s: %w", job.JobId, err)
			}
			if retry {
				metricOutcome = "infrastructure_retry"
				glog.Infof("[competition]job %s retained for infrastructure retry\n", job.JobId)
			} else if result.outcome.Score != nil && result.outcome.Error == nil {
				metricOutcome = "succeeded"
			} else if result.outcome.Error != nil && result.outcome.Error.Kind == "submission" {
				metricOutcome = "submission_failed"
			}
			return nil
		case <-heartbeatTicker.C:
			if err := self.store.Heartbeat(parent, self.settings, self.workerId, job.JobId); err != nil {
				cancel()
				<-done
				_ = self.handBack(job.JobId, "heartbeat_failed")
				return fmt.Errorf("heartbeat competition job %s: %w", job.JobId, err)
			}

			if err := self.store.RegisterHost(parent, self.settings, hostCheck); err != nil {
				cancel()
				<-done
				_ = self.handBack(job.JobId, "host_heartbeat_failed")
				return fmt.Errorf("refresh evaluator host during job %s: %w", job.JobId, err)
			}
		case <-parent.Done():
			cancel()
			<-done
			if err := self.handBack(job.JobId, "worker_shutdown"); err != nil {
				return fmt.Errorf("hand back competition job %s: %w", job.JobId, err)
			}
			return parent.Err()
		}
	}
}

func (self *Worker) handBack(jobId server.Id, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return self.store.HandBack(ctx, self.workerId, jobId, reason)
}

const (
	workloadSeedDomain   = "urnetwork-sim-latency-generator-v1\x00"
	maxProvidersFileSize = 1 * 1024 * 1024 * 1024
)

type workloadArtifact struct {
	Path   string
	Sha256 string
	Bytes  int64
}

// WorkloadGenerator is the trusted round-fixture boundary. Production uses
// CommandWorkloadGenerator; the interface also lets database tests avoid
// executing a season binary.
type WorkloadGenerator interface {
	Generate(context.Context, *Settings, server.Id, string) (workloadArtifact, error)
}

type CommandWorkloadGenerator struct{}

func (self CommandWorkloadGenerator) Generate(ctx context.Context, settings *Settings, roundId server.Id, seedHex string) (artifact workloadArtifact, err error) {
	if err := verifyPinnedExecutable(settings.SimulatorCommand, settings.EvaluationPolicy.SimulatorSha256); err != nil {
		return artifact, fmt.Errorf("simulator identity: %w", err)
	}
	seed, err := workloadSeed(seedHex)
	if err != nil {
		return artifact, err
	}
	roundDirectory, err := createRoundArtifactDirectory(settings.ArtifactRoot, roundId)
	if err != nil {
		return artifact, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(filepath.Join(roundDirectory, "providers.yml"))
			_ = os.Remove(roundDirectory)
		}
	}()
	providersPath := filepath.Join(roundDirectory, "providers.yml")
	p := settings.EvaluationPolicy
	args := []string{
		"init",
		"--out=" + providersPath,
		"--count=" + strconv.Itoa(p.ProviderCount),
		"--clients=" + strconv.Itoa(p.ClientPoolSize),
		"--rate=" + strconv.Itoa(p.ArrivalsPerMinute),
		"--seed=" + strconv.FormatInt(seed, 10),
		"--quality-window=" + strconv.Itoa(p.QualityWindowSize),
	}
	stdout := &boundedBuffer{limit: maxSelfCheckBytes}
	stderr := &boundedBuffer{limit: maxSelfCheckBytes}
	exitCode, runErr := runContainedCommand(ctx, roundDirectory, settings.SimulatorCommand, args, stdout, stderr)
	if runErr != nil {
		return artifact, fmt.Errorf("workload generator: %w", runErr)
	}
	if exitCode != 0 {
		return artifact, fmt.Errorf("workload generator exited %d", exitCode)
	}
	digest, size, err := hashRegularFile(providersPath)
	if err != nil || size <= 0 || maxProvidersFileSize < size {
		return artifact, errors.New("generated providers file is absent, empty, oversized, or unreadable")
	}
	if err := os.Chmod(providersPath, 0400); err != nil {
		return artifact, err
	}
	keep = true
	return workloadArtifact{Path: providersPath, Sha256: digest, Bytes: size}, nil
}

func generateRoundWorkload(ctx context.Context, settings *Settings, roundId server.Id, seedHex string) (workloadArtifact, error) {
	generator := settings.workloadGenerator
	if generator == nil {
		generator = CommandWorkloadGenerator{}
	}
	artifact, err := generator.Generate(ctx, settings, roundId, seedHex)
	if err != nil {
		return workloadArtifact{}, err
	}
	if !filepath.IsAbs(artifact.Path) || !sha256Pattern.MatchString(artifact.Sha256) || artifact.Bytes <= 0 {
		return workloadArtifact{}, errors.New("workload generator returned an invalid artifact identity")
	}
	return artifact, nil
}

func workloadSeed(seedHex string) (int64, error) {
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != 32 {
		return 0, errors.New("round seed is malformed")
	}
	defer clear(seed)
	h := sha256.New()
	h.Write([]byte(workloadSeedDomain))
	h.Write(seed)
	digest := h.Sum(nil)
	value := binary.BigEndian.Uint64(digest[:8]) & math.MaxInt64
	if value == 0 {
		value = 1
	}
	return int64(value), nil
}

func createRoundArtifactDirectory(root string, roundId server.Id) (string, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&0022 != 0 {
		return "", errors.New("artifact root must be an existing non-group/world-writable directory")
	}
	rounds := filepath.Join(root, "rounds")
	if err := os.Mkdir(rounds, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	roundsInfo, err := os.Lstat(rounds)
	if err != nil || !roundsInfo.IsDir() || roundsInfo.Mode()&0022 != 0 {
		return "", errors.New("round artifact root is unsafe")
	}
	directory := filepath.Join(rounds, roundId.String())
	if err := os.Mkdir(directory, 0700); err != nil {
		return "", err
	}
	return directory, nil
}

func removeRoundWorkload(settings *Settings, round *roundRecord) {
	if settings == nil || round == nil || round.ProvidersPath == "" {
		return
	}
	expectedDirectory := filepath.Join(settings.ArtifactRoot, "rounds", round.RoundId.String())
	if filepath.Dir(round.ProvidersPath) != expectedDirectory || filepath.Base(round.ProvidersPath) != "providers.yml" {
		return
	}
	_ = os.Remove(round.ProvidersPath)
	_ = os.Remove(expectedDirectory)
}

func readRoundWorkload(ctx context.Context, settings *Settings, round *roundRecord) ([]byte, error) {
	if settings == nil || round == nil || !sha256Pattern.MatchString(round.ProvidersSha256) {
		return nil, errors.New("round workload identity is missing")
	}
	expected := filepath.Join(settings.ArtifactRoot, "rounds", round.RoundId.String(), "providers.yml")
	if round.ProvidersPath != expected {
		return nil, errors.New("round workload path does not match its immutable identity")
	}
	bytes, err := readRegularFile(round.ProvidersPath, maxProvidersFileSize)
	if err != nil {
		if settings.artifactArchive == nil {
			return nil, err
		}

		return settings.artifactArchive.ReadRoundWorkload(ctx, settings, round)
	}
	digest := sha256.Sum256(bytes)
	if hex.EncodeToString(digest[:]) != round.ProvidersSha256 {
		clear(bytes)
		return nil, errors.New("round workload hash mismatch")
	}
	return bytes, nil
}

const (
	apexAdapterStateSchema = 1
	apexSubmissionFeeUsd   = 20
	apexResponseLimit      = 4 * 1024 * 1024
)

var apexSubmissionIdPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,191}$`)

// ApexAdapterRecord is the durable boundary between one paid Apex admission
// and the immutable job accepted by the competition API. Public score fields
// are populated only by a finalized leaderboard reconciliation.
type ApexAdapterRecord struct {
	Sequence             uint64       `json:"sequence"`
	SubmissionId         string       `json:"submission_id"`
	InputPatchSha256     string       `json:"input_patch_sha256"`
	RoundId              server.Id    `json:"round_id,omitempty"`
	JobId                server.Id    `json:"job_id,omitempty"`
	CanonicalPatchSha256 string       `json:"canonical_patch_sha256,omitempty"`
	StatusUrl            string       `json:"status_url,omitempty"`
	FeeUsd               int          `json:"fee_usd"`
	FeeReceipt           string       `json:"fee_receipt,omitempty"`
	State                string       `json:"state"`
	Published            bool         `json:"published"`
	Winner               bool         `json:"winner,omitempty"`
	HonestyReview        string       `json:"honesty_review,omitempty"`
	Score                *ScoreResult `json:"score,omitempty"`
	SubmittedAt          time.Time    `json:"submitted_at"`
	UpdatedAt            time.Time    `json:"updated_at"`
}

type apexAdapterState struct {
	Schema       int                 `json:"schema"`
	NextSequence uint64              `json:"next_sequence"`
	Records      []ApexAdapterRecord `json:"records"`
}

// ApexAdapterFileStore persists the emulator's admission identity in one
// fsync-and-rename state file. Methods are safe for concurrent goroutines and
// cooperating processes on the evaluator host.
type ApexAdapterFileStore struct {
	directory string
	stateLock sync.Mutex
}

// NewApexAdapterFileStore opens or creates a private state directory. Symlinks
// and group/world-writable directories are rejected before credentials or
// submission identities can be written.
func NewApexAdapterFileStore(directory string) (*ApexAdapterFileStore, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("Apex adapter state directory is required")
	}
	absDirectory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absDirectory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absDirectory)
	if err != nil || !info.IsDir() || info.Mode()&0077 != 0 {
		return nil, errors.New("Apex adapter state directory must be private and must not be a symlink")
	}
	store := &ApexAdapterFileStore{directory: absDirectory}
	if err := store.read(func(*apexAdapterState) error { return nil }); err != nil {
		return nil, err
	}
	return store, nil
}

// BeginSubmission durably assigns FIFO order before any fee or network side
// effect. Reuse is idempotent only for byte-identical patch input.
func (self *ApexAdapterFileStore) BeginSubmission(submissionId string, patchSha256 string, now time.Time) (*ApexAdapterRecord, error) {
	if !apexSubmissionIdPattern.MatchString(submissionId) || !sha256Pattern.MatchString(patchSha256) {
		return nil, errors.New("Apex submission identity is malformed")
	}
	var result ApexAdapterRecord
	err := self.update(func(state *apexAdapterState) error {
		for i := range state.Records {
			record := &state.Records[i]
			if record.SubmissionId != submissionId {
				continue
			}
			if record.InputPatchSha256 != patchSha256 {
				return errors.New("Apex submission id is already bound to different patch bytes")
			}
			result = *record
			return nil
		}
		state.NextSequence++
		result = ApexAdapterRecord{
			Sequence:         state.NextSequence,
			SubmissionId:     submissionId,
			InputPatchSha256: patchSha256,
			FeeUsd:           apexSubmissionFeeUsd,
			State:            "pending_fee",
			SubmittedAt:      now.UTC(),
			UpdatedAt:        now.UTC(),
		}
		state.Records = append(state.Records, result)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// RecordFee binds the idempotent payment receipt before admission is retried.
func (self *ApexAdapterFileStore) RecordFee(submissionId string, receipt string, now time.Time) (*ApexAdapterRecord, error) {
	if strings.TrimSpace(receipt) == "" {
		return nil, errors.New("Apex fee receipt is required")
	}
	return self.changeRecord(submissionId, func(record *ApexAdapterRecord) error {
		if record.FeeReceipt != "" && record.FeeReceipt != receipt {
			return errors.New("Apex submission fee receipt changed")
		}
		record.FeeReceipt = receipt
		if record.JobId == (server.Id{}) {
			record.State = "pending_admission"
		}
		record.UpdatedAt = now.UTC()
		return nil
	})
}

// RecordRound freezes the open epoch before money is collected. A retry after
// close must fail against that round rather than silently buying admission to
// the next epoch.
func (self *ApexAdapterFileStore) RecordRound(submissionId string, roundId server.Id, now time.Time) (*ApexAdapterRecord, error) {
	if roundId == (server.Id{}) {
		return nil, errors.New("Apex submission round id is required")
	}
	return self.changeRecord(submissionId, func(record *ApexAdapterRecord) error {
		if record.RoundId != (server.Id{}) && record.RoundId != roundId {
			return errors.New("Apex submission round id changed")
		}
		record.RoundId = roundId
		record.UpdatedAt = now.UTC()
		return nil
	})
}

// RecordAdmission freezes the first API identity and rejects a retry that maps
// the same Apex submission to any different competition job.
func (self *ApexAdapterFileStore) RecordAdmission(submissionId string, accepted ScoreAcceptedResult, now time.Time) (*ApexAdapterRecord, error) {
	if accepted.JobId == (server.Id{}) || accepted.RoundId == (server.Id{}) || !sha256Pattern.MatchString(accepted.PatchSha256) || accepted.StatusUrl == "" {
		return nil, errors.New("competition API returned a malformed admission identity")
	}
	return self.changeRecord(submissionId, func(record *ApexAdapterRecord) error {
		if record.FeeReceipt == "" {
			return errors.New("competition admission cannot precede fee collection")
		}
		if record.RoundId != (server.Id{}) && record.RoundId != accepted.RoundId {
			return errors.New("competition API admitted the submission to a different round")
		}
		if record.JobId != (server.Id{}) && (record.JobId != accepted.JobId || record.RoundId != accepted.RoundId ||
			record.CanonicalPatchSha256 != accepted.PatchSha256 || record.StatusUrl != accepted.StatusUrl) {
			return errors.New("competition API changed an immutable admission identity")
		}
		record.JobId = accepted.JobId
		record.RoundId = accepted.RoundId
		record.CanonicalPatchSha256 = accepted.PatchSha256
		record.StatusUrl = accepted.StatusUrl
		record.State = accepted.State
		record.UpdatedAt = now.UTC()
		return nil
	})
}

// RecordPoll records only outcome-neutral job state. Scores and evaluation
// errors are deliberately excluded until the finalized leaderboard publishes.
func (self *ApexAdapterFileStore) RecordPoll(submissionId string, job ScoreJobResult, now time.Time) (*ApexAdapterRecord, error) {
	return self.changeRecord(submissionId, func(record *ApexAdapterRecord) error {
		if record.JobId != job.JobId || record.RoundId != job.RoundId || record.CanonicalPatchSha256 != job.PatchSha256 {
			return errors.New("competition poll changed an immutable job identity")
		}
		if job.Score != nil || job.EvalError != nil {
			return errors.New("competition poll disclosed an embargoed outcome")
		}
		record.State = job.State
		record.UpdatedAt = now.UTC()
		return nil
	})
}

// ReconcileLeaderboard publishes results only from an atomically finalized
// epoch and authenticates every returned job and patch identity.
func (self *ApexAdapterFileStore) ReconcileLeaderboard(leaderboards SeasonLeaderboardResult, now time.Time) error {
	return self.update(func(state *apexAdapterState) error {
		byJobId := map[server.Id]*ApexAdapterRecord{}
		for i := range state.Records {
			record := &state.Records[i]
			if record.JobId != (server.Id{}) {
				byJobId[record.JobId] = record
			}
		}
		seenJobIds := map[server.Id]bool{}
		for _, leaderboard := range leaderboards.Epochs {
			if leaderboard.Status != "finalized" || leaderboard.RoundId == (server.Id{}) || leaderboard.FinalizedAt.IsZero() {
				return errors.New("Apex reconciliation received a non-finalized leaderboard")
			}
			for _, entry := range leaderboard.Entries {
				if seenJobIds[entry.JobId] {
					return errors.New("Apex reconciliation received a duplicate leaderboard job")
				}
				seenJobIds[entry.JobId] = true
				record, ok := byJobId[entry.JobId]
				if !ok {
					return fmt.Errorf("leaderboard job %s is not an admitted Apex submission", entry.JobId)
				}
				if record.RoundId != leaderboard.RoundId || record.CanonicalPatchSha256 != entry.PatchSha256 {
					return errors.New("leaderboard changed an immutable Apex submission identity")
				}
				if entry.Score.ScoreSchema != ScoreSchema || entry.Score.Significance == nil {
					return errors.New("leaderboard score is missing its statistical record")
				}
				score := entry.Score
				record.Score = &score
				record.Published = true
				record.Winner = entry.Winner
				record.HonestyReview = entry.HonestyReview
				record.State = "published"
				record.UpdatedAt = now.UTC()
			}
		}
		return nil
	})
}

// Get returns a copy so callers cannot mutate durable state without a store
// transaction.
func (self *ApexAdapterFileStore) Get(submissionId string) (*ApexAdapterRecord, error) {
	var result *ApexAdapterRecord
	err := self.read(func(state *apexAdapterState) error {
		for i := range state.Records {
			if state.Records[i].SubmissionId == submissionId {
				record := state.Records[i]
				result = &record
				return nil
			}
		}
		return os.ErrNotExist
	})
	return result, err
}

// Pending returns admitted records in the original durable FIFO order.
func (self *ApexAdapterFileStore) Pending() ([]ApexAdapterRecord, error) {
	var records []ApexAdapterRecord
	err := self.read(func(state *apexAdapterState) error {
		for _, record := range state.Records {
			if record.JobId == (server.Id{}) || record.Published ||
				record.State == "completed" || record.State == "failed" || record.State == "invalid" {
				continue
			}
			record.Score = nil
			records = append(records, record)
		}
		sort.Slice(records, func(i int, j int) bool { return records[i].Sequence < records[j].Sequence })
		return nil
	})
	return records, err
}

func (self *ApexAdapterFileStore) changeRecord(
	submissionId string,
	change func(*ApexAdapterRecord) error,
) (*ApexAdapterRecord, error) {
	var result ApexAdapterRecord
	err := self.update(func(state *apexAdapterState) error {
		for i := range state.Records {
			record := &state.Records[i]
			if record.SubmissionId != submissionId {
				continue
			}
			if err := change(record); err != nil {
				return err
			}
			result = *record
			return nil
		}
		return os.ErrNotExist
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (self *ApexAdapterFileStore) read(read func(*apexAdapterState) error) error {
	return self.withState(false, read)
}

func (self *ApexAdapterFileStore) update(change func(*apexAdapterState) error) error {
	return self.withState(true, change)
}

func (self *ApexAdapterFileStore) withState(write bool, operation func(*apexAdapterState) error) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	lockPath := filepath.Join(self.directory, "adapter.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	statePath := filepath.Join(self.directory, "adapter-state.json")
	state := apexAdapterState{Schema: apexAdapterStateSchema}
	encoded, err := os.ReadFile(statePath)
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&state); err != nil {
			return fmt.Errorf("decode Apex adapter state: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("decode Apex adapter state: trailing JSON")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.Schema != apexAdapterStateSchema {
		return fmt.Errorf("unsupported Apex adapter state schema %d", state.Schema)
	}
	if err := validateApexAdapterState(&state); err != nil {
		return err
	}
	if err := operation(&state); err != nil || !write {
		return err
	}
	if err := validateApexAdapterState(&state); err != nil {
		return err
	}
	encoded, err = json.MarshalIndent(&state, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary, err := os.OpenFile(filepath.Join(self.directory, "adapter-state.json.new"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return errors.New("stale Apex adapter state transaction exists")
	}
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(encoded); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, statePath); err != nil {
		return err
	}
	directoryFile, err := os.Open(self.directory)
	if err != nil {
		return err
	}
	if err := directoryFile.Sync(); err != nil {
		_ = directoryFile.Close()
		return err
	}
	if err := directoryFile.Close(); err != nil {
		return err
	}
	committed = true
	return nil
}

func validateApexAdapterState(state *apexAdapterState) error {
	seenSubmissionIds := map[string]bool{}
	seenJobIds := map[server.Id]bool{}
	lastSequence := uint64(0)
	for _, record := range state.Records {
		if !apexSubmissionIdPattern.MatchString(record.SubmissionId) || !sha256Pattern.MatchString(record.InputPatchSha256) ||
			record.Sequence <= lastSequence || record.Sequence > state.NextSequence || seenSubmissionIds[record.SubmissionId] ||
			record.FeeUsd != apexSubmissionFeeUsd || record.SubmittedAt.IsZero() || record.UpdatedAt.Before(record.SubmittedAt) {
			return errors.New("Apex adapter state contains an invalid submission record")
		}
		if record.JobId != (server.Id{}) {
			if record.RoundId == (server.Id{}) || !sha256Pattern.MatchString(record.CanonicalPatchSha256) || record.StatusUrl == "" || seenJobIds[record.JobId] {
				return errors.New("Apex adapter state contains an invalid job identity")
			}
			seenJobIds[record.JobId] = true
		}
		if record.Published && record.Score == nil {
			return errors.New("Apex adapter published a submission without a score")
		}
		seenSubmissionIds[record.SubmissionId] = true
		lastSequence = record.Sequence
	}
	return nil
}

// ApexFeeCollector must implement idempotency on submissionId. A retry after a
// process crash may call CollectOnce again before the local receipt is durable.
type ApexFeeCollector interface {
	CollectOnce(ctx context.Context, submissionId string, feeUsd int) (receipt string, err error)
}

// ApexAdapterOptions exposes retry hooks so the conformance suite can force
// every backpressure transition without timing or scheduler dependence.
type ApexAdapterOptions struct {
	HttpClient  *http.Client
	MaxAttempts int
	Wait        func(context.Context, time.Duration) error
	Now         func() time.Time
}

// ApexAdapter maps paid external admissions onto the authenticated async API.
// It has no evaluator, hidden-seed, MinIO, Docker, or operator privileges.
type ApexAdapter struct {
	baseUrl      *url.URL
	submitterJwt string
	store        *ApexAdapterFileStore
	feeCollector ApexFeeCollector
	httpClient   *http.Client
	maxAttempts  int
	wait         func(context.Context, time.Duration) error
	now          func() time.Time
}

// NewApexAdapter constructs a fail-closed adapter. The bearer token is kept in
// memory and is sent only to the exact configured API origin.
func NewApexAdapter(
	baseUrl string,
	submitterJwt string,
	store *ApexAdapterFileStore,
	feeCollector ApexFeeCollector,
	options ApexAdapterOptions,
) (*ApexAdapter, error) {
	parsed, err := url.Parse(baseUrl)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Apex adapter API URL must be an absolute origin")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" {
		return nil, errors.New("Apex adapter requires HTTPS outside loopback conformance tests")
	}
	if strings.TrimSpace(submitterJwt) == "" || store == nil || feeCollector == nil {
		return nil, errors.New("Apex adapter token, durable store, and fee collector are required")
	}
	if options.HttpClient == nil {
		options.HttpClient = &http.Client{Timeout: 30 * time.Second}
	}
	httpClient := *options.HttpClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 5
	}
	if options.MaxAttempts < 1 || options.MaxAttempts > 20 {
		return nil, errors.New("Apex adapter max attempts must be between 1 and 20")
	}
	if options.Wait == nil {
		options.Wait = func(ctx context.Context, delay time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				return nil
			}
		}
	}
	if options.Now == nil {
		options.Now = server.NowUtc
	}
	return &ApexAdapter{
		baseUrl:      parsed,
		submitterJwt: submitterJwt,
		store:        store,
		feeCollector: feeCollector,
		httpClient:   &httpClient,
		maxAttempts:  options.MaxAttempts,
		wait:         options.Wait,
		now:          options.Now,
	}, nil
}

// Submit collects the fee once and retries the same immutable bytes through
// typed 429 or retriable 5xx responses until the durable API job is known.
func (self *ApexAdapter) Submit(ctx context.Context, submissionId string, patch []byte) (*ApexAdapterRecord, error) {
	patchDigest := sha256.Sum256(patch)
	record, err := self.store.BeginSubmission(submissionId, hex.EncodeToString(patchDigest[:]), self.now())
	if err != nil {
		return nil, err
	}
	if record.JobId != (server.Id{}) {
		return record, nil
	}
	if record.RoundId == (server.Id{}) {
		var info InfoResult
		if err := self.requestWithRetry(ctx, http.MethodGet, "/competition/info", nil, false, &info); err != nil {
			return nil, err
		}
		if info.ActiveRound == nil || info.ActiveRound.Status != "open" || info.ActiveRound.RoundId == (server.Id{}) {
			return nil, errors.New("competition has no open round for Apex admission")
		}
		record, err = self.store.RecordRound(submissionId, info.ActiveRound.RoundId, self.now())
		if err != nil {
			return nil, err
		}
	}
	if record.FeeReceipt == "" {
		receipt, err := self.feeCollector.CollectOnce(ctx, submissionId, apexSubmissionFeeUsd)
		if err != nil {
			return nil, fmt.Errorf("collect Apex submission fee: %w", err)
		}
		record, err = self.store.RecordFee(submissionId, receipt, self.now())
		if err != nil {
			return nil, err
		}
	}

	request := ScoreArgs{RoundId: record.RoundId, Patch: string(patch)}
	var accepted ScoreAcceptedResult
	if err := self.requestWithRetry(ctx, http.MethodPost, "/competition/score", request, true, &accepted); err != nil {
		return nil, err
	}
	if accepted.State != "queued" && accepted.State != "running" && accepted.State != "completed" {
		return nil, errors.New("competition API returned an invalid admission state")
	}
	return self.store.RecordAdmission(submissionId, accepted, self.now())
}

// PollNext polls exactly one earliest unpublished admission, preserving FIFO
// pressure at the adapter boundary even if callers invoke it concurrently.
func (self *ApexAdapter) PollNext(ctx context.Context) (*ApexAdapterRecord, error) {
	records, err := self.store.Pending()
	if err != nil || len(records) == 0 {
		return nil, err
	}
	record := records[0]
	var job ScoreJobResult
	if err := self.requestWithRetry(ctx, http.MethodGet, record.StatusUrl, nil, true, &job); err != nil {
		return nil, err
	}
	return self.store.RecordPoll(record.SubmissionId, job, self.now())
}

// Reconcile fetches the public finalized leaderboards and atomically releases
// only identities that match prior paid admissions.
func (self *ApexAdapter) Reconcile(ctx context.Context) (*SeasonLeaderboardResult, error) {
	var leaderboards SeasonLeaderboardResult
	if err := self.requestWithRetry(ctx, http.MethodGet, "/competition/leaderboard", nil, false, &leaderboards); err != nil {
		return nil, err
	}
	if err := self.store.ReconcileLeaderboard(leaderboards, self.now()); err != nil {
		return nil, err
	}
	return &leaderboards, nil
}

// Reveal downloads a finalized round workload and verifies both the response
// header and the commitment supplied by the finalized round record.
func (self *ApexAdapter) Reveal(ctx context.Context, round RoundResult) ([]byte, error) {
	if round.Status != "finalized" || round.RevealedSeed == nil || round.ProvidersUrl == "" || !sha256Pattern.MatchString(round.ProvidersSha256) {
		return nil, errors.New("round is not eligible for public reveal")
	}
	requestUrl, err := self.resolvePath(round.ProvidersUrl)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestUrl.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := self.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("competition reveal returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, apexResponseLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > apexResponseLimit {
		return nil, errors.New("competition reveal exceeded the response limit")
	}
	digest := sha256.Sum256(body)
	digestHex := hex.EncodeToString(digest[:])
	headerDigest := strings.Trim(response.Header.Get("X-Content-SHA256"), "\"")
	if digestHex != round.ProvidersSha256 || headerDigest != round.ProvidersSha256 {
		clear(body)
		return nil, errors.New("competition reveal does not match its published SHA-256")
	}
	return body, nil
}

func (self *ApexAdapter) requestWithRetry(
	ctx context.Context,
	method string,
	requestPath string,
	requestValue any,
	authenticated bool,
	responseValue any,
) error {
	requestUrl, err := self.resolvePath(requestPath)
	if err != nil {
		return err
	}
	var body []byte
	if requestValue != nil {
		body, err = json.Marshal(requestValue)
		if err != nil {
			return err
		}
	}
	for attempt := 1; attempt <= self.maxAttempts; attempt++ {
		request, err := http.NewRequestWithContext(ctx, method, requestUrl.String(), bytes.NewReader(body))
		if err != nil {
			return err
		}
		if requestValue != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if authenticated {
			request.Header.Set("Authorization", "Bearer "+self.submitterJwt)
		}
		response, requestErr := self.httpClient.Do(request)
		if requestErr != nil {
			if attempt == self.maxAttempts {
				return requestErr
			}
			if err := self.wait(ctx, apexRetryDelay(nil, attempt)); err != nil {
				return err
			}
			continue
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, apexResponseLimit+1))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			if readErr != nil {
				return readErr
			}
			return closeErr
		}
		if len(responseBody) > apexResponseLimit {
			return errors.New("competition API response exceeded the limit")
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			decoder := json.NewDecoder(bytes.NewReader(responseBody))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(responseValue); err != nil {
				return fmt.Errorf("decode competition API response: %w", err)
			}
			if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
				return errors.New("competition API response contains trailing JSON")
			}
			return nil
		}
		var apiError CompetitionError
		_ = json.Unmarshal(responseBody, &apiError)
		retriable := response.StatusCode == http.StatusTooManyRequests ||
			(response.StatusCode >= 500 && response.StatusCode <= 599)
		if !retriable || attempt == self.maxAttempts {
			if apiError.Code != "" {
				return &apiError
			}
			return fmt.Errorf("competition API returned HTTP %d", response.StatusCode)
		}
		if err := self.wait(ctx, apexRetryDelay(response, attempt)); err != nil {
			return err
		}
	}
	return errors.New("competition API retry loop exhausted")
}

func (self *ApexAdapter) resolvePath(requestPath string) (*url.URL, error) {
	reference, err := url.Parse(requestPath)
	if err != nil {
		return nil, err
	}
	resolved := self.baseUrl.ResolveReference(reference)
	if resolved.Scheme != self.baseUrl.Scheme || resolved.Host != self.baseUrl.Host || resolved.User != nil ||
		resolved.Fragment != "" || !strings.HasPrefix(resolved.EscapedPath(), "/competition/") {
		return nil, errors.New("competition API path escaped the configured origin")
	}
	return resolved, nil
}

func apexRetryDelay(response *http.Response, attempt int) time.Duration {
	if response != nil {
		value := strings.TrimSpace(response.Header.Get("Retry-After"))
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 && seconds <= 60 {
			return time.Duration(seconds) * time.Second
		}
	}
	delay := time.Duration(attempt) * time.Second
	if delay > 10*time.Second {
		return 10 * time.Second
	}
	return delay
}
