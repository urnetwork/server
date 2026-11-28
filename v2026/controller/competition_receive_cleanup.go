// Future receive directories retain private creation custody until their exact
// retained outcome is acknowledged by the database. This is not a reaper and
// never discovers or adopts historical directories by name or age.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
)

// Budgets belong to one worker's post-commit local housekeeping. Nonpositive
// limits disable cleanup; they never disable retention or alter a score.
type competitionReceiveCleanupSettings struct {
	Timeout          time.Duration
	MaximumEntries   int
	MaximumDepth     int
	MaximumPathBytes int
	MaximumBytes     int64
	Now              func() time.Time
}

func defaultCompetitionReceiveCleanupSettings() competitionReceiveCleanupSettings {
	limits := competitionagent.DefaultLimits()
	return competitionReceiveCleanupSettings{Timeout: 5 * time.Second, MaximumEntries: 16 * 1024, MaximumDepth: 64,
		MaximumPathBytes: 4096, MaximumBytes: limits.ArtifactBytes + limits.DocumentBytes, Now: server.NowUtc}
}

// Only finite operational phases are observable; neither paths, errors nor
// job identities become metric labels or change committed completion results.
var competitionReceiveCleanupFailures = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "urnetwork", Subsystem: "competition",
	Name: "receive_cleanup_failures_total", Help: "Local future-owned receive custody or post-commit cleanup failures; committed outcomes are unchanged."}, []string{"phase"})

func init() { prometheus.MustRegister(competitionReceiveCleanupFailures) }

// Linux supplies descriptor-relative operations. Other platforms refuse this
// optional housekeeping capability without changing ordinary evaluation.
type competitionReceiveNativeCustody interface {
	Cleanup(context.Context, competitionReceiveCleanupSettings, map[string]competitionReceiveFile) error
	Close() error
}

// A signed file alone never grants deletion: remove requires a separately
// retained manifest/object binding or a pinned controller input/request hash.
type competitionReceiveFile struct {
	bytes  int64
	sha256 string
	remove bool
}

// One sequential owner transfers this pointer from evaluator to worker. The
// held descriptors prevent inode reuse; Close only relinquishes, never deletes.
type competitionReceiveCustody struct {
	identity       competitionagent.Identity
	fence          int64
	slot           int
	originalStart  time.Time
	cycleOffset    int
	native         competitionReceiveNativeCustody
	files          map[string]competitionReceiveFile
	inputs         map[string]competitionagent.File
	manifestSha256 string
	retainedUntil  time.Time
	closed         bool
}

// Production can obtain custody only through the actual exclusive creation
// operation. A failed optional capture leaves existing evaluation behavior
// unchanged; it never causes adoption of a directory found by a later scan.
func createCompetitionReceive(root string, job *queuedJob, identity competitionagent.Identity, inputs []competitionagent.File) (string, *competitionReceiveCustody, error) {
	directory, err := os.MkdirTemp(root, ".executor-receive-"+identity.RunId+"-")
	if err != nil {
		return "", nil, err
	}
	custody, err := captureCompetitionReceiveCustody(root, directory, job, identity)
	if err != nil {
		competitionReceiveCleanupFailures.WithLabelValues("capture").Inc()
	}
	if custody != nil {
		custody.inputs = map[string]competitionagent.File{}
		valid := competitionagent.ValidateFiles(inputs, competitionagent.DefaultLimits().InputBytes, competitionagent.DefaultLimits()) == nil
		for _, input := range inputs {
			switch input.Path {
			case "canonical.patch", "providers.yml", "round-baseline.json":
				custody.inputs[input.Path] = input
			default:
				valid = false
			}
		}
		if !valid {
			custody.Close()
			custody = nil
			competitionReceiveCleanupFailures.WithLabelValues("capture").Inc()
		}
	}
	return directory, custody, nil
}

// Called only immediately after this controller's successful MkdirTemp, before
// any transfer. The native constructor also requires the child to be empty.
func captureCompetitionReceiveCustody(root, directory string, job *queuedJob, identity competitionagent.Identity) (*competitionReceiveCustody, error) {
	startedAt, started := competitionReceiveExecutionStart(job)
	if job == nil || job.StartedAt == nil || job.ExecutionDeadline == nil || job.SlotFencingToken <= 0 || identity.RunId != competitionagent.RunId(job.JobId.String(), job.EvaluationCycle, job.AttemptCount) ||
		identity.JobId != job.JobId.String() || identity.RoundId != job.RoundId.String() || identity.HostId != job.ExecutorHostId || identity.Cycle != job.EvaluationCycle || identity.Attempt != job.AttemptCount ||
		!started || !identity.StartedAt.Equal(startedAt) || !identity.DeadlineAt.Equal(*job.ExecutionDeadline) {
		return nil, errors.New("receive creation custody differs from its original claim")
	}
	native, err := captureCompetitionReceiveNative(root, directory, identity.RunId)
	if err != nil {
		return nil, err
	}
	return &competitionReceiveCustody{identity: identity, fence: job.SlotFencingToken, slot: job.SlotId, originalStart: *job.StartedAt, cycleOffset: job.CycleAttemptOffset, native: native}, nil
}

// Reevaluation keeps the original submission start separately from its
// already-authorized execution-cycle start; neither clock may be substituted.
func competitionReceiveExecutionStart(job *queuedJob) (time.Time, bool) {
	if job == nil || job.StartedAt == nil {
		return time.Time{}, false
	}
	if job.EvaluationCycle > 0 {
		if job.CycleStartedAt == nil || job.CycleAttemptOffset < 1 || job.AttemptCount <= job.CycleAttemptOffset {
			return time.Time{}, false
		}
		return *job.CycleStartedAt, true
	}
	return *job.StartedAt, true
}

// A private success flag comes from the authenticated archiver, not request
// data. Its exact manifest and signed file inventory bind the local capability.
func (self *competitionReceiveCustody) retain(outcome EvaluationOutcome, response competitionagent.Response) error {
	if self == nil || self.closed || !outcome.distributedArchiveVerified || len(outcome.ArtifactManifest) == 0 {
		return errors.New("receive has no verified retained outcome")
	}
	manifest := artifactManifest{}
	if json.Unmarshal(outcome.ArtifactManifest, &manifest) != nil || manifest.Schema != 1 || manifest.JobId != self.identity.JobId || manifest.RoundId != self.identity.RoundId || manifest.Attempt != self.identity.Attempt ||
		manifest.Executor == nil || manifest.Executor.RunId != self.identity.RunId || manifest.Executor.HostId != self.identity.HostId || manifest.Executor.SlotId != self.slot || manifest.Executor.SlotFencingToken != self.fence ||
		manifest.Retention == nil || !manifest.Retention.AuthenticatedAfterUpload || !manifest.Retention.ComplianceObjectLock || manifest.Retention.ManifestKey == "" || manifest.Retention.RetainUntil.IsZero() ||
		manifest.Retention.ObjectCount <= 0 || manifest.Retention.ObjectCount != len(manifest.Retention.Objects) || response.Identity == nil || !sameCompetitionAgentIdentity(response.Identity, &self.identity) ||
		response.RunId != self.identity.RunId || response.FencingToken != self.fence || competitionagent.ValidateFiles(response.Files, competitionagent.DefaultLimits().ArtifactBytes, competitionagent.DefaultLimits()) != nil {
		return errors.New("receive cleanup lacks its exact authenticated retention identity")
	}
	boundArtifacts := map[string]evaluationArtifact{}
	for _, artifact := range manifest.Artifacts {
		if _, duplicate := boundArtifacts[artifact.Path]; duplicate || artifact.Bytes < 0 || !sha256Pattern.MatchString(artifact.Sha256) {
			return errors.New("receive artifact binding is ambiguous")
		}
		boundArtifacts[artifact.Path] = artifact
	}
	// These three files are appended by ArchiveAttempt through dedicated
	// manifest hash fields, rather than necessarily appearing in Artifacts.
	workerHashes := map[string]string{"canonical.patch": manifest.PatchSha256, "worker.stderr.log": manifest.StderrSha256, "worker-result.json": manifest.ResultSha256}
	objects := map[string]retainedArtifact{}
	for _, retained := range manifest.Retention.Objects {
		if _, duplicate := objects[retained.Path]; duplicate || retained.Bytes < 0 || retained.VersionId == "" || retained.Mode != "COMPLIANCE" || !sha256Pattern.MatchString(retained.Sha256) || retained.RetainUntil.Before(manifest.Retention.RetainUntil) {
			return errors.New("receive retention did not preserve every exact immutable object version")
		}
		artifact, explicit := boundArtifacts[retained.Path]
		if explicit {
			if artifact.Sha256 != retained.Sha256 || artifact.Bytes != retained.Bytes {
				return errors.New("receive artifact differs from its retained exact object")
			}
		} else if workerHashes[retained.Path] != retained.Sha256 {
			return errors.New("receive object lacks an independent manifest binding")
		}
		objects[retained.Path] = retained
	}
	for path, artifact := range boundArtifacts {
		object, exists := objects[path]
		if !exists || object.Sha256 != artifact.Sha256 || object.Bytes != artifact.Bytes {
			return errors.New("receive manifest artifact has no exact retained object")
		}
	}
	owned := map[string]competitionReceiveFile{}
	for _, file := range response.Files {
		remove := false
		if input, controlled := self.inputs[file.Path]; controlled {
			if input.Sha256 != file.Sha256 || input.Bytes != file.Bytes {
				return errors.New("receive input differs from its controller-created pin")
			}
			remove = true
		}
		if object, retained := objects[file.Path]; retained {
			if object.Sha256 != file.Sha256 || object.Bytes != file.Bytes {
				return errors.New("receive signed file differs from its retained artifact")
			}
			remove = true
		}
		if file.Path == "worker-request.json" {
			// Agent.remapRequest is the sole producer. Before setting the
			// private verified flag, authenticateDistributedCompetitionAttempt
			// independently normalizes/authenticates these exact bytes against
			// the original controller request, not the peer repeating its hash.
			// The archived hash pins content/length without uploading secrets.
			if !sha256Pattern.MatchString(manifest.RequestSha256) || file.Sha256 != manifest.RequestSha256 || response.RequestSha256 != manifest.RequestSha256 || file.Bytes > competitionagent.DefaultLimits().DocumentBytes {
				return errors.New("receive private request lacks its independent archived hash")
			}
			remove = true
		}
		// Unretained stdout/control/output files have no deletion authority.
		// They and their ancestors remain; unknown on-disk files still make
		// the native whole-inventory preflight refuse all deletion.
		owned[file.Path] = competitionReceiveFile{bytes: file.Bytes, sha256: file.Sha256, remove: remove}
	}
	if _, exists := owned["executor-receipt.json"]; exists {
		return errors.New("receive peer selected a controller-owned receipt path")
	}
	receipt, receiptFound := objects["executor-receipt.json"]
	if !receiptFound || receipt.Sha256 != manifest.Executor.AgentReceiptSha256 {
		return errors.New("receive cleanup omitted its controller-owned receipt")
	}
	owned[receipt.Path] = competitionReceiveFile{bytes: receipt.Bytes, sha256: receipt.Sha256, remove: true}
	digest := sha256.Sum256(outcome.ArtifactManifest)
	self.files, self.manifestSha256, self.retainedUntil = owned, hex.EncodeToString(digest[:]), manifest.Retention.RetainUntil
	return nil
}

// Only the worker's timely Complete acknowledgement calls this method. A
// copied outcome, changed fence/clock or unavailable retention preserves bytes.
func (self *competitionReceiveCustody) cleanup(ctx context.Context, settings competitionReceiveCleanupSettings, job *queuedJob, outcome EvaluationOutcome) error {
	startedAt, started := competitionReceiveExecutionStart(job)
	if self == nil || self.closed || self.native == nil || settings.Timeout <= 0 || settings.MaximumEntries <= 0 || settings.MaximumDepth <= 0 || settings.MaximumPathBytes <= 0 || settings.MaximumBytes <= 0 || settings.Now == nil ||
		job == nil || job.StartedAt == nil || job.ExecutionDeadline == nil || job.JobId.String() != self.identity.JobId || job.RoundId.String() != self.identity.RoundId || job.ExecutorHostId != self.identity.HostId ||
		job.EvaluationCycle != self.identity.Cycle || job.AttemptCount != self.identity.Attempt || job.SlotFencingToken != self.fence || job.SlotId != self.slot ||
		!started || !startedAt.Equal(self.identity.StartedAt) || !job.StartedAt.Equal(self.originalStart) || job.CycleAttemptOffset != self.cycleOffset || !job.ExecutionDeadline.Equal(self.identity.DeadlineAt) || !outcome.distributedArchiveVerified || len(self.files) == 0 ||
		!settings.Now().Before(self.retainedUntil) {
		return errors.New("receive post-commit cleanup has no current exact ownership proof")
	}
	digest := sha256.Sum256(outcome.ArtifactManifest)
	if hex.EncodeToString(digest[:]) != self.manifestSha256 || !sha256Pattern.MatchString(self.manifestSha256) {
		return errors.New("receive retained outcome changed before completion acknowledgement")
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	return self.native.Cleanup(ctx, settings, self.files)
}

// Repeated close is harmless across the joined completion/defer boundaries.
func (self *competitionReceiveCustody) Close() error {
	if self == nil || self.closed {
		return nil
	}
	self.closed = true
	if self.native == nil {
		return nil
	}
	return self.native.Close()
}

// Always preserving: failure/shutdown consumers close held resources without
// authorizing filesystem changes or another evaluation.
func (self EvaluationOutcome) closeReceiveCustody() {
	if self.receiveCustody != nil {
		if err := self.receiveCustody.Close(); err != nil {
			competitionReceiveCleanupFailures.WithLabelValues("close").Inc()
		}
	}
}

// Qualification consumes the retained local bytes before any capability can
// reach ordinary completion. Failure preserves the tree; no scoring decision
// depends on optional local housekeeping being available on this platform.
func (self *competitionAgentEvaluator) retainedReceiveOutcome(directory string, custody *competitionReceiveCustody, response competitionagent.Response, outcome EvaluationOutcome) (EvaluationOutcome, error) {
	if self.afterRetained != nil {
		if err := self.afterRetained(directory, response, outcome); err != nil {
			return outcome, err
		}
	}
	if custody != nil {
		if err := custody.retain(outcome, response); err != nil {
			competitionReceiveCleanupFailures.WithLabelValues("retention").Inc()
		} else {
			outcome.receiveCustody = custody
		}
	}
	return outcome, nil
}

// Signed relative file paths also define the complete allowed directory set.
func competitionReceiveDirectories(files map[string]competitionReceiveFile, settings competitionReceiveCleanupSettings) (map[string]bool, error) {
	if settings.MaximumEntries <= 0 || settings.MaximumDepth <= 0 || settings.MaximumPathBytes <= 0 || settings.MaximumBytes <= 0 || len(files) == 0 || len(files) >= settings.MaximumEntries {
		return nil, errors.New("receive cleanup inventory has no finite positive limits")
	}
	directories := map[string]bool{"": true}
	var total int64
	for path, file := range files {
		size := file.bytes
		if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasPrefix(path, "..") || len(path) > settings.MaximumPathBytes || len(strings.Split(path, string(filepath.Separator)))-1 > settings.MaximumDepth || !sha256Pattern.MatchString(file.sha256) || size < 0 || size > settings.MaximumBytes-total {
			return nil, errors.New("receive file inventory exceeded its safe cleanup bounds")
		}
		total += size
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			directories[parent] = true
			if len(files)+len(directories) > settings.MaximumEntries {
				return nil, errors.New("receive inventory exceeds its cleanup entry budget")
			}
		}
	}
	if len(files)+len(directories) > settings.MaximumEntries {
		return nil, errors.New("receive inventory exceeds its cleanup entry budget")
	}
	return directories, nil
}
