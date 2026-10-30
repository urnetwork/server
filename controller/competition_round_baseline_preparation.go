// Round controls are prepared once before the first real job's execution clock.
// A durable reservation names that still-queued job; only a fenced transaction
// may start it and attach the completed control. Public API and schema stay unchanged.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Preparation is not a submission attempt and never resets a submission timer.
// Every timeout, retry and evidence bound belongs to this independent owner.
type roundBaselinePreparationSettings struct {
	TotalTimeout, CleanupTimeout, FenceInterval, ArchiveBackoff     time.Duration
	MaximumRecordBytes                                              int64
	MaximumDiagnosticBytes                                          int64
	MaximumManifestBytes, MaximumEvidenceBytes, MaximumRawFileBytes int64
	MaximumArtifacts                                                int
	ArchiveAttempts                                                 int
	Now                                                             func() time.Time
	// Nil uses the production interval; tests drive explicit fence edges.
	FenceNotify <-chan time.Time
}

// Three hours cover control production and retained success; cancellation has
// its own bounded cleanup grace. Candidate execution keeps its full three hours.
func defaultRoundBaselinePreparationSettings() roundBaselinePreparationSettings {
	return roundBaselinePreparationSettings{TotalTimeout: 3 * time.Hour, CleanupTimeout: 2 * time.Minute,
		FenceInterval: 5 * time.Second, ArchiveBackoff: time.Second, MaximumRecordBytes: 8 * 1024 * 1024, MaximumDiagnosticBytes: 4 * 1024 * 1024,
		MaximumManifestBytes: 8 * 1024 * 1024, MaximumEvidenceBytes: 4 * 1024 * 1024 * 1024,
		MaximumRawFileBytes: 512 * 1024 * 1024, MaximumArtifacts: 4096,
		ArchiveAttempts: 3, Now: server.NowUtc}
}

// Only a root-pinned worker CLI configuration can enable this additional role.
// It is intentionally absent from the API settings and frozen round wire schema.
type roundBaselinePreparationPin struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

type roundBaselinePreparationConfig struct {
	G2VolumePolicy     model.CompetitionG2VolumePolicy `json:"g2_volume_policy,omitempty"`
	Schema             int                             `json:"schema"`
	Kind               string                          `json:"kind"`
	Program            roundBaselinePreparationPin     `json:"program"`
	Dependencies       []roundBaselinePreparationPin   `json:"dependencies"`
	ZeroSampleRecovery *roundBaselinePreparationPin    `json:"zero_sample_recovery,omitempty"`
}

// Runtime-only policy survives source/frozen-settings copies without changing
// the API configuration, manifests or evaluator request format.
type roundBaselinePreparationProfile struct {
	Config   roundBaselinePreparationConfig
	Settings roundBaselinePreparationSettings
	Recovery *roundBaselineRecoveryCapability
}

// Facts from the sealed metadata-preflight v3 producer, not configurable policy.
// The v4 role adds only its independently qualified entry script beside these.
func roundBaselinePreparationDependencyDigests() map[string]string {
	return map[string]string{
		"evaluator.sh":                   "0cccda1d1dcc227f30fda800ddb9bbb28e29dd48cc62885da637fcd4eb4eb00b",
		"network-boundary.sh":            "9ab82a9a8bb2272934461472509c15df4242f14859351dadd6dfcb2a1cd27cb7",
		"compose.yml":                    "f4ca3a5eedfd5f081fbb507fc0102fa03ce11c095b406bcc0f7dc0119b5e0464",
		"hash-local-mount.sh":            "94ee94b9cc42dfe2f7236d49cc24e26b7bd79e9b63c5d582315b7fd1878dcb86",
		"docker-id-map.sh":               "0babbcce2d286bf861827c9c11fa02d841d487b33c23765c96916ee1063d854c",
		"resource-boundary.sh":           "14909d09069d0ee329a2779796bf003b12765fa4fad435c0aeb784c316ccbd03",
		"retain-failure-evidence.sh":     "f3f119c06b12662c1f2d1dae433be40de7a7b222fc42f55aba8b75a3cd14c4d1",
		"postgres-init.sh":               "653b81b0fcf2fdaf4a3f4421a252a582484581e3a03463c0fad03186ee14d77c",
		"timeout-budget.sh":              "7f3298edaca3bd31f4e5a02d17fd88ccb66f961e9d8493d9b047426d8fab476d",
		"entrypoint.sh":                  "efbfe1a8dd8fb58b4fef8e57bea205e819dfdf21a85ec31d71d85365cf467758",
		"build-submission.sh":            "4a998275ffe94d322462ec9d7876af1ff288c3e8ba85f0c3278b4b512baa0172",
		"submission-builder-identity.sh": "1764b7a97f1f99d33aa201c7e721dcce37ed3f58c5aeed8c99b50644706b8c19",
		"prepare-evaluation-source.sh":   "a2445db1aa6e8655da12ca8ec810eda86c5e93df84801887c76ed490f232d4b6",
		"Dockerfile.submission":          "255d1827494c22482bb851323ee13d8ee03fb407fbf40348531b7d2ec32fc034",
		"competitionpatch":               "3d161d76bace48bff6536164a84f330b1618c82f319775b8551c19872b937e54",
		"competitionpatch.sha256":        "560358c0192972f71e715144289b8871fc4c3566d382cd30a9b7d3deb74c6df7",
	}
}

// A new producer has its own immutable release. The historical program and
// dependency fingerprints above continue to authenticate version-one controls.
func roundBaselinePreparationPolicyDependencies(policy model.CompetitionG2VolumePolicy) (map[string]string, string, error) {
	dependencies := roundBaselinePreparationDependencyDigests()
	switch policy {
	case model.CompetitionG2VolumePolicyLegacy:
		return dependencies, "prepare-round-baseline.sh", nil
	case model.CompetitionG2VolumePolicyMinimumWork:
		delete(dependencies, "evaluator.sh")
		dependencies["evaluator-minimum-work.sh"] = "7780930d44ebc1df5d8f775a75c37aadb2048fc1b9386027615f3ce6b5fe56d4"
		return dependencies, "prepare-round-baseline-minimum-work.sh", nil
	default:
		return nil, "", errors.New("unsupported baseline preparation g2 volume policy")
	}
}

// A reservation is append-only database evidence, not a claimed/running job.
// Its identity contains no seed, providers, credentials or caller-selected command.
type roundBaselinePreparation struct {
	Schema        int                              `json:"schema"`
	Kind          string                           `json:"kind"`
	Owner         string                           `json:"owner"`
	WorkerId      string                           `json:"worker_id"`
	JobId         server.Id                        `json:"job_id"`
	RoundId       server.Id                        `json:"round_id"`
	PatchSha256   string                           `json:"patch_sha256"`
	ContextSha256 string                           `json:"context_sha256"`
	WorkerImage   string                           `json:"worker_image_digest"`
	StartedAt     time.Time                        `json:"started_at"`
	Deadline      time.Time                        `json:"deadline"`
	SourceJob     *queuedJob                       `json:"-"`
	Recovery      *roundBaselineRecoveryTransition `json:"-"`
}

// The producer must finish and archive before publication. The archive is a
// baseline-preparation document, never a successful candidate attempt or score.
type preparedRoundBaseline struct {
	BaselineJson           []byte
	BaselineSha256         string
	ArtifactManifest       json.RawMessage
	ArtifactManifestSha256 string
}

// Holds expose only bounded provenance, not stderr, providers or hidden inputs.
type roundBaselinePreparationHold struct {
	Stage                   string `json:"stage"`
	ArtifactManifestSha256  string `json:"artifact_manifest_sha256,omitempty"`
	ArtifactArchiveRetained bool   `json:"artifact_archive_retained"`
}

// Optional private interfaces keep historical workers and Store mocks intact.
// An enabled production worker refuses a store/producer missing this capability.
type roundBaselinePreparationStore interface {
	ReserveRoundBaseline(context.Context, *Settings, string, string) (*roundBaselinePreparation, error)
	CheckRoundBaselinePreparation(context.Context, *Settings, *roundBaselinePreparation) error
	HoldRoundBaselinePreparation(context.Context, *Settings, *roundBaselinePreparation, roundBaselinePreparationHold) error
	ClaimPreparedRoundBaseline(context.Context, *Settings, *roundBaselinePreparation, *preparedRoundBaseline) (*queuedJob, error)
	ReleaseRoundBaselinePreparation(context.Context, *roundBaselinePreparation) error
}

type roundBaselinePreparer interface {
	PrepareRoundBaseline(context.Context, *Settings, *roundBaselinePreparation) (*preparedRoundBaseline, error)
}

var errRoundBaselinePreparationHeld = errors.New("round baseline preparation is held; reconcile original evidence, do not resample")

// A future CLI option may never replace the current startup profile, even if
// the operator accidentally supplies a valid legacy configuration at that path.
func ConfigureCompetitionFutureRoundBaselinePreparation(settings *Settings, path, expected string) error {
	if settings == nil || settings.FutureEpochPolicy == nil {
		return errors.New("future preparation requires an approved future epoch policy")
	}
	copy := *settings
	copy.futureBaselinePreparation = nil
	if err := ConfigureCompetitionRoundBaselinePreparation(&copy, path, expected); err != nil {
		return err
	}
	if copy.futureBaselinePreparation == nil || copy.futureBaselinePreparation.Config.G2VolumePolicy != settings.FutureEpochPolicy.G2VolumePolicy {
		return errors.New("future preparation config does not name the separately staged policy")
	}
	settings.futureBaselinePreparation = copy.futureBaselinePreparation
	return nil
}

// Hashes bind the exact frozen round/source/workload and real queued job while
// leaving the hidden seed out of durable reservation/event payloads.
func roundBaselinePreparationContext(settings *Settings, job *queuedJob) (string, error) {
	if settings == nil || job == nil || job.JobId == (server.Id{}) || job.RoundId != job.Round.RoundId {
		return "", errors.New("baseline preparation requires an exact round/job identity")
	}
	content, err := json.Marshal(struct {
		JobId, RoundId                                                              server.Id
		PatchSha256, ProvidersSha256, Commitment, BaseSha, Image, Evaluator, Worker string
		Policy                                                                      json.RawMessage
	}{JobId: job.JobId, RoundId: job.RoundId, PatchSha256: job.PatchSha256,
		ProvidersSha256: job.Round.ProvidersSha256, Commitment: job.Round.WorkloadCommitment,
		BaseSha: settings.BaseSha, Image: settings.EvaluatorImageDigest,
		Evaluator: settings.EvaluatorCommandSha256, Worker: job.WorkerImageDigest, Policy: job.Round.PolicyJson})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}

// Called only by the worker CLI. All transitive host helpers are immutable root
// files; no API deployment or caller-controlled request field enables this path.
func ConfigureCompetitionRoundBaselinePreparation(settings *Settings, path, expected string) error {
	if settings == nil || !sha256Pattern.MatchString(expected) {
		return errors.New("baseline preparation config requires an exact SHA-256")
	}
	if err := competitionQualificationRootPath(path, false); err != nil {
		return err
	}
	content, err := readCompetitionPreparationFile(path, 64*1024, false)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expected {
		return errors.New("baseline preparation configuration digest mismatch")
	}
	var config roundBaselinePreparationConfig
	if err := decodeCompetitionPreparationJson(bytes.NewReader(content), 64*1024, &config); err != nil {
		return err
	}
	if config.G2VolumePolicy != settings.EvaluationPolicy.G2VolumePolicy &&
		(settings.FutureEpochPolicy == nil || config.G2VolumePolicy != settings.FutureEpochPolicy.G2VolumePolicy) {
		return errors.New("baseline preparation policy is neither current nor explicitly staged for a future epoch")
	}
	expectedDependencies, expectedProgram, err := roundBaselinePreparationPolicyDependencies(config.G2VolumePolicy)
	if err != nil {
		return err
	}
	if config.Schema != 1 || config.Kind != "sim-latency-round-baseline-preparation-config" ||
		filepath.Base(config.Program.Path) != expectedProgram || len(config.Dependencies) != len(expectedDependencies) {
		return errors.New("baseline preparation configuration identity is invalid")
	}
	for _, pin := range config.Dependencies {
		if expectedDependencies[filepath.Base(pin.Path)] != pin.Sha256 {
			return errors.New("baseline preparation changed a sealed v3 dependency")
		}
	}
	seen := map[string]bool{}
	for _, pin := range append([]roundBaselinePreparationPin{config.Program}, config.Dependencies...) {
		if seen[pin.Path] || !sha256Pattern.MatchString(pin.Sha256) || filepath.Dir(pin.Path) != filepath.Dir(config.Program.Path) {
			return errors.New("baseline preparation dependency is duplicated or outside its release")
		}
		seen[pin.Path] = true
		if err := competitionQualificationRootPath(pin.Path, false); err != nil {
			return err
		}
		actual, _, err := hashRegularFile(pin.Path)
		if err != nil || actual != pin.Sha256 {
			return errors.New("baseline preparation dependency digest mismatch")
		}
		info, err := os.Stat(pin.Path)
		if err != nil || info.Mode().Perm()&0o222 != 0 {
			return errors.New("baseline preparation dependency is writable")
		}
	}
	if err := verifyPinnedExecutable(config.Program.Path, config.Program.Sha256); err != nil {
		return err
	}
	root, err := openFailureArtifactPath(settings.ArtifactRoot, "", true)
	if err != nil {
		return errors.New("baseline preparation requires a provisioned private artifact root before queue admission")
	}
	info, statErr := root.Stat()
	closeErr := root.Close()
	if statErr != nil || closeErr != nil || info.Mode().Perm()&0o022 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("baseline preparation artifact root is not owned by the worker or is externally writable")
	}
	profile := &roundBaselinePreparationProfile{Config: config, Settings: defaultRoundBaselinePreparationSettings()}
	if err := configureRoundBaselineRecovery(settings, profile); err != nil {
		return err
	}
	if config.G2VolumePolicy == settings.EvaluationPolicy.G2VolumePolicy {
		settings.roundBaselinePreparation = profile
	} else {
		settings.futureBaselinePreparation = profile
	}
	return nil
}

// The ordinary Claim starts the submission clock. A cold round instead owns
// one separate producer first; publication and the real first Claim are atomic.
func (self *Worker) claimWithPreparedRoundBaseline(ctx context.Context, host HostSelfCheck) (*queuedJob, error) {
	if self.settings.roundBaselinePreparation == nil {
		return self.store.Claim(ctx, self.settings, self.workerId, self.workerImageDigest)
	}
	store, ok := self.store.(roundBaselinePreparationStore)
	if !ok {
		return nil, errors.New("enabled baseline preparation requires its fenced store capability")
	}
	producer, ok := self.evaluator.(roundBaselinePreparer)
	if !ok {
		return nil, errors.New("enabled baseline preparation requires its trusted producer capability")
	}
	preparation, err := store.ReserveRoundBaseline(ctx, self.settings, self.workerId, self.workerImageDigest)
	if err != nil {
		return nil, err
	}
	if preparation == nil {
		return self.store.Claim(ctx, self.settings, self.workerId, self.workerImageDigest)
	}
	policy := self.settings.roundBaselinePreparation.Settings
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), policy.CleanupTimeout)
		defer cancel()
		_ = store.ReleaseRoundBaselinePreparation(cleanupCtx, preparation)
	}
	defer cleanup()
	hold := func(stage string, cause error) (*queuedJob, error) {
		holdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), policy.CleanupTimeout)
		defer cancel()
		details := roundBaselinePreparationHold{Stage: stage}
		var failure *roundBaselinePreparationFailure
		if errors.As(cause, &failure) && failure.ManifestSha256 != "" {
			details.ArtifactManifestSha256 = failure.ManifestSha256
			details.ArtifactArchiveRetained = true
		}
		persistErr := store.HoldRoundBaselinePreparation(holdCtx, self.settings, preparation, details)
		return nil, errors.Join(errRoundBaselinePreparationHeld, &roundBaselinePreparationFailure{Cause: cause}, persistErr)
	}
	if !host.RebaselinePassed || host.RebaselineRoundId == nil || *host.RebaselineRoundId != preparation.RoundId ||
		preparation.SourceJob == nil || preparation.SourceJob.StartedAt != nil || preparation.SourceJob.AttemptCount != 0 {
		return hold("host_or_source_job_mismatch", errors.New("baseline preparation cannot borrow host readiness or a started job"))
	}
	executionDeadline := roundBaselinePreparationExecutionDeadline(preparation)
	if executionDeadline.IsZero() {
		return hold("invalid_execution_clock", ErrConflict)
	}
	operationCtx, cancel := context.WithDeadline(ctx, executionDeadline)
	defer cancel()
	operationCtx, fenceCancel := context.WithCancelCause(operationCtx)
	fenceCtx, stopFence := context.WithCancel(operationCtx)
	fenceDone := make(chan struct{})
	lastHostHeartbeat := policy.Now()
	go func() {
		defer close(fenceDone)
		for {
			next := policy.FenceNotify
			if next == nil {
				next = time.After(policy.FenceInterval)
			}
			select {
			case <-fenceCtx.Done():
				return
			case <-next:
			}
			if err := store.CheckRoundBaselinePreparation(fenceCtx, self.settings, preparation); err != nil {
				if fenceCtx.Err() == nil {
					fenceCancel(err)
				}
				return
			}
			if now := policy.Now(); now.Sub(lastHostHeartbeat) >= time.Duration(self.settings.WorkerHeartbeatSeconds)*time.Second {
				if err := self.store.RegisterHost(fenceCtx, self.settings, host); err != nil {
					if fenceCtx.Err() == nil {
						fenceCancel(err)
					}
					return
				}
				lastHostHeartbeat = now
			}
		}
	}()
	joinFence := func() { stopFence(); <-fenceDone }
	defer func() { joinFence(); fenceCancel(nil) }()
	if err := store.CheckRoundBaselinePreparation(operationCtx, self.settings, preparation); err != nil {
		return hold("initial_fence", err)
	}
	prepared, err := producer.PrepareRoundBaseline(operationCtx, self.settings, preparation)
	if err != nil {
		return hold("producer_or_archive", err)
	}
	if err := operationCtx.Err(); err != nil {
		return hold("canceled_before_publication", errors.Join(err, context.Cause(operationCtx)))
	}
	// Publication transfers the singleton to a genuine running job. Join the
	// preparation-only observer first; the SQL transaction rechecks all fences.
	joinFence()
	if err := operationCtx.Err(); err != nil {
		return hold("canceled_before_publication", errors.Join(err, context.Cause(operationCtx)))
	}
	job, err := store.ClaimPreparedRoundBaseline(operationCtx, self.settings, preparation, prepared)
	if err != nil {
		return hold("publication_fence", err)
	}
	if job == nil || job.JobId != preparation.JobId || job.StartedAt == nil || job.RoundBaseline == nil || job.LeaseOwner != self.workerId {
		return nil, fmt.Errorf("baseline publication returned an incomplete real claim: %w", errRoundBaselinePreparationHeld)
	}
	return job, nil
}
