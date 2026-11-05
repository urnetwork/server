// Durable, single-host admission for signed subevaluations. Disk state is
// committed before systemd launch, and uncertain outcomes are never replayed.
package competitionagent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
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
	"syscall"
	"time"
)

// Calls are safe across goroutines and independent SSH processes. A protected
// flock serializes durable admission; long evaluation never retains that lock.
type Agent struct {
	config               Config
	settings             AgentSettings
	runner               Runner
	cleaner              runCleaner
	containment          runContainment
	coordinatorPublicKey ed25519.PublicKey
	signingKey           ed25519.PrivateKey
	credential           *syscall.Credential
	protected            bool
	runtimeAdmission     func(context.Context) (*RuntimeResourceBoundary, error)
}

// State and hidden-input manifests live outside the unprivileged run directory.
// LaunchAttempted is fsynced before the only possible service-start operation.
type runState struct {
	Schema              int    `json:"schema"`
	ManifestSha256      string `json:"manifest_sha256"`
	FencingToken        int64  `json:"fencing_token"`
	LaunchAttempted     bool   `json:"launch_attempted"`
	ExecutionStarted    bool   `json:"execution_started"`
	CancelRequested     bool   `json:"cancel_requested"`
	CleanupVerified     bool   `json:"cleanup_verified"`
	StopCleanupVerified bool   `json:"stop_cleanup_verified"`
	CleanupPlanSha256   string `json:"cleanup_plan_sha256"`
	State               string `json:"state"`
	RequestSha256       string `json:"request_sha256"`
	ExitCode            int    `json:"exit_code"`
	ErrorCode           string `json:"error_code,omitempty"`
	Files               []File `json:"files,omitempty"`
}

// A retained unknown run continues occupying the physical slot. Reboot or SSH
// failure is not evidence of cleanup and never makes another launch safe.
type slotState struct {
	RunId string `json:"run_id"`
}

// Attempts may differ, but one job/cycle keeps its first signed absolute clock.
type jobClock struct {
	JobId      string    `json:"job_id"`
	RoundId    string    `json:"round_id"`
	Cycle      int       `json:"cycle"`
	StartedAt  time.Time `json:"started_at"`
	DeadlineAt time.Time `json:"deadline_at"`
}

// Construction verifies protected authority, independent keys and local pins.
// The service manager is injectable without weakening production validation.
func NewAgent(config *Config, runner Runner, settings AgentSettings) (*Agent, error) {
	return newAgent(config, runner, settings, true)
}

// Tests bypass root ownership only, keeping framing, signatures, persistence,
// executable hashes, slot arbitration and every state transition unchanged.
func newAgent(config *Config, runner Runner, settings AgentSettings, protected bool) (*Agent, error) {
	if err := config.validate(protected); err != nil {
		return nil, err
	}
	if runner == nil || settings.ControlTimeout <= 0 || settings.SelfCheckTimeout <= 0 || settings.CancelGrace <= 0 || settings.LockPollInterval <= 0 ||
		settings.MaximumClockSkew <= 0 || settings.DiagnosticBytes <= 0 || settings.ConfigBytes <= 0 || settings.DirectoryBatch <= 0 || settings.Now == nil {
		return nil, errors.New("invalid bounded subevaluation agent owner")
	}
	if settings.CleanupTimeout <= 0 || settings.CleanupCommandTimeout <= 0 || settings.CleanupCommandTimeout > settings.CleanupTimeout ||
		settings.CleanupPollInterval <= 0 || settings.CleanupObjectCount <= 0 || settings.InputTransferTimeout <= 0 || settings.OwnerPollInterval <= 0 ||
		settings.ManagementMemoryBytes <= 0 || settings.ManagementCpuQuotaPercent <= 0 {
		return nil, errors.New("invalid bounded subevaluation cleanup owner")
	}
	keyContent, err := readBounded(config.SigningKeyPath, settings.ConfigBytes)
	if err != nil {
		return nil, err
	}
	defer clear(keyContent)
	privateBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyContent)))
	if err != nil {
		return nil, errors.New("invalid independent agent signing key")
	}
	defer clear(privateBytes)
	var privateKey ed25519.PrivateKey
	switch len(privateBytes) {
	case ed25519.SeedSize:
		privateKey = ed25519.NewKeyFromSeed(privateBytes)
	case ed25519.PrivateKeySize:
		privateKey = append(ed25519.PrivateKey(nil), privateBytes...)
	default:
		return nil, errors.New("invalid independent agent signing key length")
	}
	publicKey, err := base64.StdEncoding.DecodeString(config.CoordinatorPublicKeyBase64)
	if err != nil {
		return nil, errors.New("invalid independent coordinator public key")
	}
	var credential *syscall.Credential
	if protected {
		credential, err = workerCredential(config.WorkerUser)
		if err != nil {
			return nil, err
		}
		accessCtx, cancel := context.WithTimeout(context.Background(), settings.ControlTimeout)
		err = verifyWorkerRuntimeAccess(accessCtx, config, credential, probeWorkerRuntimeAccess)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Join(config.StateDirectory, "runs"), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(config.StateDirectory, "clocks"), 0700); err != nil {
		return nil, err
	}
	if err := syncDirectory(config.StateDirectory); err != nil {
		return nil, err
	}
	self := &Agent{config: *config, settings: settings, runner: runner, cleaner: newHostCleaner(*config, settings), containment: newHostRunContainment(*config, settings, credential), coordinatorPublicKey: publicKey,
		signingKey: privateKey, credential: credential, protected: protected}
	self.runtimeAdmission = self.verifyCleanupRuntime
	return self, nil
}

// One SSH invocation owns one authenticated operation. Response files are sent
// only after a signed bounded manifest, never as an archive or arbitrary path.
func (self *Agent) Serve(ctx context.Context, reader io.Reader, writer io.Writer) error {
	request := Request{}
	if err := ReadDocument(reader, &request, self.config.Limits.DocumentBytes); err != nil {
		return err
	}
	if err := VerifyRequest(request, self.coordinatorPublicKey); err != nil {
		return err
	}
	if request.Schema != Schema || request.HostId != self.config.HostId {
		return errors.New("subevaluation request has another role identity")
	}
	var response Response
	var err error
	switch request.Operation {
	case "check", "availability":
		if request.RunId != "" || request.FencingToken != 0 || request.Start != nil {
			return errors.New("invalid self-check operation")
		}
		if request.Operation == "check" {
			response, err = self.check(ctx)
		} else {
			response, err = self.availability(ctx)
		}
	case "start":
		if request.Start == nil || request.RunId != request.Start.Identity.RunId || request.FencingToken <= 0 {
			return errors.New("invalid start operation")
		}
		response, err = self.start(ctx, request, reader)
	case "status", "cancel", "artifacts":
		if request.Start != nil || !digestPattern.MatchString(request.RunId) || request.FencingToken <= 0 {
			return errors.New("invalid fixed-run operation")
		}
		if request.Operation == "cancel" {
			response, err = self.cancel(ctx, request)
		} else {
			response, err = self.status(ctx, request)
		}
	default:
		return errors.New("unsupported subevaluation operation")
	}
	if err != nil {
		return err
	}
	if request.Operation == "artifacts" && !terminalState(response.State) {
		return errors.New("artifacts require a committed terminal execution")
	}
	if err := SignResponse(&response, self.signingKey); err != nil {
		return err
	}
	if err := WriteDocument(writer, response, self.config.Limits.DocumentBytes); err != nil {
		return err
	}
	if request.Operation == "artifacts" {
		return WriteFiles(writer, self.workDirectory(request.RunId), response.Files, self.config.Limits.ArtifactBytes, self.config.Limits)
	}
	return nil
}

// A same-run retry authenticates its duplicate body but never dispatches again
// after LaunchAttempted. Ownership may advance without changing any input byte.
func (self *Agent) start(ctx context.Context, request Request, reader io.Reader) (Response, error) {
	if err := self.validateManifest(*request.Start); err != nil {
		return Response{}, err
	}
	digest, err := ManifestDigest(*request.Start)
	if err != nil {
		return Response{}, err
	}
	// Network reads, large file hashing and fixed helper calls never own the
	// authority lock. A stalled reconnect cannot delay post-stop resource reap.
	incoming, err := os.MkdirTemp(self.config.WorkDirectory, ".incoming-"+request.RunId+"-")
	if err != nil {
		return Response{}, err
	}
	defer os.RemoveAll(incoming)
	if err := self.receiveStartInputs(ctx, request, reader, incoming); err != nil {
		return Response{}, err
	}
	if err := self.materializeRequest(incoming, self.workDirectory(request.RunId), *request.Start); err != nil {
		return Response{}, err
	}
	cleanup, err := self.prepareCleanupPlan(ctx, *request.Start)
	if err != nil {
		return Response{}, err
	}
	cleanupBytes, err := json.Marshal(cleanup)
	if err != nil {
		return Response{}, err
	}
	if err := self.prepareWorkerOwnership(incoming); err != nil {
		return Response{}, err
	}
	return self.commitStart(ctx, request, digest, incoming, cleanupBytes)
}

// Recheck immutable identity, current fence, physical slot and original clock
// after the private transfer. Concurrent duplicate uploads cannot launch twice.
func (self *Agent) commitStart(ctx context.Context, request Request, digest, incoming string, cleanupBytes []byte) (Response, error) {
	lock, err := self.lock(ctx)
	if err != nil {
		return Response{}, err
	}
	state, loadErr := self.loadState(request.RunId)
	if loadErr == nil {
		if state.ManifestSha256 != digest || request.FencingToken < state.FencingToken {
			lock.Close()
			return Response{}, errors.New("stale fence or conflicting immutable execution")
		}
		state.FencingToken = request.FencingToken
		if err := self.storeState(request.RunId, state); err != nil {
			lock.Close()
			return Response{}, err
		}
		launch := !state.LaunchAttempted && state.State == "accepted"
		if launch {
			if _, err := self.loadCleanupPlan(request.RunId, state, request.Start); err != nil {
				lock.Close()
				return Response{}, err
			}
			if !self.settings.Now().UTC().Before(request.Start.Identity.DeadlineAt) {
				state.State, state.ErrorCode, state.CleanupVerified = "expired", "deadline_elapsed_before_launch", true
				if err := self.storeState(request.RunId, state); err != nil {
					lock.Close()
					return Response{}, err
				}
				lock.Close()
				return self.status(ctx, request)
			}
			if err := self.requireFreeSlot(ctx, request.RunId); err != nil {
				lock.Close()
				return Response{}, err
			}
			if err := self.storeSlot(request.RunId); err != nil {
				lock.Close()
				return Response{}, err
			}
			state.LaunchAttempted = true
			if err := self.storeState(request.RunId, state); err != nil {
				lock.Close()
				return Response{}, err
			}
		}
		lock.Close()
		if launch {
			self.launch(ctx, *request.Start)
		}
		return self.status(ctx, Request{Schema: Schema, HostId: request.HostId, RunId: request.RunId, FencingToken: request.FencingToken})
	}
	if !errors.Is(loadErr, os.ErrNotExist) {
		lock.Close()
		return Response{}, loadErr
	}
	if !self.settings.Now().UTC().Before(request.Start.Identity.DeadlineAt) {
		lock.Close()
		return Response{}, errors.New("original subevaluation deadline elapsed before admission")
	}
	if err := self.requireFreeSlot(ctx, request.RunId); err != nil {
		lock.Close()
		return Response{}, err
	}
	if err := self.requireOriginalClock(request.Start.Identity); err != nil {
		lock.Close()
		return Response{}, err
	}
	cleanupDigest := sha256.Sum256(cleanupBytes)
	if err := os.Rename(incoming, self.workDirectory(request.RunId)); err != nil {
		lock.Close()
		return Response{}, errors.New("run workspace already exists or cannot be committed")
	}
	if err := syncDirectory(self.config.WorkDirectory); err != nil {
		lock.Close()
		return Response{}, err
	}
	manifestContent, err := json.Marshal(request.Start)
	if err != nil {
		lock.Close()
		return Response{}, err
	}
	defer clear(manifestContent)
	if err := os.Mkdir(self.stateDirectory(request.RunId), 0700); err != nil {
		lock.Close()
		return Response{}, err
	}
	if err := syncDirectory(filepath.Join(self.config.StateDirectory, "runs")); err != nil {
		lock.Close()
		return Response{}, err
	}
	if err := atomicFile(self.stateDirectory(request.RunId), "manifest.json", manifestContent); err != nil {
		lock.Close()
		return Response{}, err
	}
	if err := atomicFile(self.stateDirectory(request.RunId), "cleanup-plan.json", cleanupBytes); err != nil {
		lock.Close()
		return Response{}, err
	}
	requestFile, err := self.fileDigest(self.workDirectory(request.RunId), "worker-request.json")
	if err != nil {
		lock.Close()
		return Response{}, err
	}
	state = &runState{Schema: Schema, ManifestSha256: digest, FencingToken: request.FencingToken,
		State: "accepted", RequestSha256: requestFile.Sha256, CleanupPlanSha256: hex.EncodeToString(cleanupDigest[:]), ExitCode: -1}
	if err := self.storeState(request.RunId, state); err != nil {
		lock.Close()
		return Response{}, err
	}
	if err := self.storeSlot(request.RunId); err != nil {
		lock.Close()
		return Response{}, err
	}
	if !self.settings.Now().UTC().Before(request.Start.Identity.DeadlineAt) {
		state.State, state.ErrorCode, state.CleanupVerified = "expired", "deadline_elapsed_before_launch", true
		if err := self.storeState(request.RunId, state); err != nil {
			lock.Close()
			return Response{}, err
		}
		lock.Close()
		return self.status(ctx, request)
	}
	state.LaunchAttempted = true
	if err := self.storeState(request.RunId, state); err != nil {
		lock.Close()
		return Response{}, err
	}
	lock.Close()
	self.launch(ctx, *request.Start)
	return self.status(ctx, request)
}

// The committed launch-attempt marker is intentionally not cleared on error.
// A transport cancellation after this call can only reconcile the original unit.
func (self *Agent) launch(ctx context.Context, manifest StartManifest) {
	controlCtx, cancel := context.WithTimeout(ctx, self.settings.ControlTimeout)
	defer cancel()
	state, err := self.loadState(manifest.Identity.RunId)
	if err != nil {
		return
	}
	plan, err := self.loadCleanupPlan(manifest.Identity.RunId, state, &manifest)
	if err != nil || self.containment.Prepare(controlCtx, *plan) != nil || self.containment.Start(controlCtx, *plan) != nil {
		return
	}
	_ = self.runner.Start(controlCtx, Launch{UnitName: unitName(manifest.Identity.RunId), RunId: manifest.Identity.RunId,
		ConfigPath: self.config.configPath, BaseDirectory: self.config.BaseDirectory, Agent: self.config.Agent, DeadlineAt: manifest.Identity.DeadlineAt,
		OwnerUnitName: dockerOwnerUnitName(manifest.Identity.RunId), ManagementCpuSet: plan.ManagementCpuSet})
}

// Status advances only custody, never the execution clock, attempt or launch.
func (self *Agent) status(ctx context.Context, request Request) (Response, error) {
	lock, err := self.lock(ctx)
	if err != nil {
		return Response{}, err
	}
	state, err := self.loadState(request.RunId)
	if errors.Is(err, os.ErrNotExist) {
		lock.Close()
		return Response{Schema: Schema, HostId: self.config.HostId, RunId: request.RunId, FencingToken: request.FencingToken,
			State: "unknown", ErrorCode: "run_not_found", ExitCode: -1}, nil
	}
	if err != nil {
		lock.Close()
		return Response{}, err
	}
	if request.FencingToken < state.FencingToken {
		lock.Close()
		return Response{}, errors.New("stale subevaluation ownership fence")
	}
	manifest, err := self.loadManifest(request.RunId, state.ManifestSha256)
	if err != nil {
		lock.Close()
		return Response{}, err
	}
	if request.FencingToken != state.FencingToken {
		state.FencingToken = request.FencingToken
		if err := self.storeState(request.RunId, state); err != nil {
			lock.Close()
			return Response{}, err
		}
	}
	response := self.response(manifest, state)
	lock.Close()
	if !state.LaunchAttempted {
		return response, nil
	}
	controlCtx, cancel := context.WithTimeout(ctx, self.settings.ControlTimeout)
	defer cancel()
	unit, statusErr := self.runner.Status(controlCtx, unitName(request.RunId))
	if terminalState(state.State) && state.StopCleanupVerified && statusErr == nil && !unit.Active {
		return response, nil
	}
	if statusErr != nil || !unit.Exists {
		response.State, response.ErrorCode = "unknown", "launch_outcome_unknown"
	} else if unit.Active {
		if terminalState(state.State) {
			response.State, response.ErrorCode = "unknown", "stop_cleanup_pending"
		} else if state.ExecutionStarted {
			response.State = "running"
		} else {
			response.State = "accepted"
		}
	} else {
		response.State, response.ErrorCode = "unknown", "executor_exit_uncommitted"
	}
	if !self.settings.Now().UTC().Before(manifest.Identity.DeadlineAt) {
		response.ErrorCode = "deadline_elapsed_unknown"
	}
	if statusErr == nil && (unit.Exists && !unit.Active || !unit.Exists && !self.settings.Now().UTC().Before(manifest.Identity.DeadlineAt)) {
		// A missing pre-deadline unit is still uncertain; after the immutable
		// deadline it may be finalized only with an independent cleanup proof.
		return self.reconcileStopped(ctx, request, manifest, unit)
	}
	return response, nil
}

// A service which exited before committing an inventory cannot report success.
// An exact scoped cleanup plus an inactive service can recover terminal failure
// evidence without replaying execution. Historical self-check markers cannot
// prove this run's cleanup. A missing unit is considered only after its deadline.
func (self *Agent) reconcileStopped(ctx context.Context, request Request, manifest *StartManifest, unit UnitStatus) (Response, error) {
	if err := self.Cleanup(ctx, request.RunId); err != nil {
		lock, lockErr := self.lock(ctx)
		if lockErr != nil {
			return Response{}, lockErr
		}
		defer lock.Close()
		state, loadErr := self.loadState(request.RunId)
		if loadErr != nil {
			return Response{}, loadErr
		}
		response := self.response(manifest, state)
		response.State, response.ErrorCode = "unknown", "cleanup_not_verified"
		return response, nil
	}
	files, artifactErr := self.collectFiles(self.workDirectory(request.RunId))
	lock, err := self.lock(ctx)
	if err != nil {
		return Response{}, err
	}
	defer lock.Close()
	state, err := self.loadState(request.RunId)
	if err != nil {
		return Response{}, err
	}
	if request.FencingToken < state.FencingToken {
		return Response{}, errors.New("stale subevaluation reconciliation fence")
	}
	if terminalState(state.State) {
		return self.response(manifest, state), nil
	}
	state.State, state.ErrorCode, state.ExitCode = "failed", "executor_exit_uncommitted", unit.ExitCode
	state.CleanupVerified = true
	if artifactErr == nil {
		state.Files = files
	} else {
		state.Files, state.ErrorCode = nil, "artifact_authentication_failed"
	}
	if state.CancelRequested {
		state.State, state.ErrorCode = "canceled", "evaluation_canceled"
	}
	if !self.settings.Now().UTC().Before(manifest.Identity.DeadlineAt) {
		state.State, state.ErrorCode = "expired", "original_deadline_elapsed"
	}
	if err := self.storeState(request.RunId, state); err != nil {
		return Response{}, err
	}
	return self.response(manifest, state), nil
}

// Cancellation is committed before stop, then joined by the system manager.
// An unsuccessful stop remains uncertain; it cannot release another host slot.
func (self *Agent) cancel(ctx context.Context, request Request) (Response, error) {
	lock, err := self.lock(ctx)
	if err != nil {
		return Response{}, err
	}
	state, err := self.loadState(request.RunId)
	if err != nil {
		lock.Close()
		return Response{}, err
	}
	if request.FencingToken < state.FencingToken {
		lock.Close()
		return Response{}, errors.New("stale subevaluation cancellation fence")
	}
	state.FencingToken, state.CancelRequested = request.FencingToken, true
	if err := self.storeState(request.RunId, state); err != nil {
		lock.Close()
		return Response{}, err
	}
	lock.Close()
	if !terminalState(state.State) && state.LaunchAttempted {
		controlCtx, cancel := context.WithTimeout(ctx, self.settings.ControlTimeout)
		_ = self.runner.Stop(controlCtx, unitName(request.RunId))
		cancel()
	}
	return self.status(ctx, request)
}

// The fixed systemd entrypoint reauthenticates persisted input and marks
// execution exactly once before invoking the trusted unprivileged evaluator.
func (self *Agent) Execute(ctx context.Context, runId string) error {
	if !digestPattern.MatchString(runId) {
		return errors.New("invalid fixed execution identity")
	}
	// Admission may perform bounded native reads but cannot delay post-stop
	// custody by retaining the publication lock. Recheck state after it joins.
	boundary, err := self.runtimeAdmission(ctx)
	if err != nil {
		return err
	}
	defer boundary.Close()
	lock, err := self.lock(ctx)
	if err != nil {
		return err
	}
	state, err := self.loadState(runId)
	if err != nil {
		lock.Close()
		return err
	}
	manifest, err := self.loadManifest(runId, state.ManifestSha256)
	if err != nil {
		lock.Close()
		return err
	}
	plan, err := self.loadCleanupPlan(runId, state, manifest)
	if err != nil {
		lock.Close()
		return err
	}
	if boundary != nil && plan.WorkDeviceId != boundary.WorkDeviceId {
		lock.Close()
		return errors.New("execution runtime changed its admitted candidate device")
	}
	if err := self.validateManifest(*manifest); err != nil {
		lock.Close()
		return err
	}
	if state.ExecutionStarted || !state.LaunchAttempted || state.CleanupVerified || state.StopCleanupVerified || terminalState(state.State) {
		lock.Close()
		return errors.New("subevaluation execution was already attempted or not authorized")
	}
	slot, err := self.loadSlot()
	if err != nil || slot.RunId != runId {
		lock.Close()
		return errors.New("subevaluation has no physical slot custody")
	}
	if state.CancelRequested || !self.settings.Now().UTC().Before(manifest.Identity.DeadlineAt) {
		state.State, state.ErrorCode, state.CleanupVerified = "expired", "deadline_elapsed_before_execution", true
		if state.CancelRequested {
			state.State, state.ErrorCode = "canceled", "canceled_before_execution"
		}
		err := self.storeState(runId, state)
		lock.Close()
		return err
	}
	if err := self.authenticateInputFiles(*manifest); err != nil {
		lock.Close()
		return err
	}
	requestFile, err := self.fileDigest(self.workDirectory(runId), "worker-request.json")
	if err != nil || requestFile.Sha256 != state.RequestSha256 {
		lock.Close()
		return errors.New("committed hidden request failed authentication")
	}
	state.ExecutionStarted, state.State = true, "running"
	if err := self.storeState(runId, state); err != nil {
		lock.Close()
		return err
	}
	lock.Close()
	evaluationCtx, cancel := context.WithDeadline(ctx, manifest.Identity.DeadlineAt)
	exitCode, runErr := self.runEvaluator(evaluationCtx, manifest, plan)
	evaluationExpired := !self.settings.Now().UTC().Before(manifest.Identity.DeadlineAt) || errors.Is(evaluationCtx.Err(), context.DeadlineExceeded)
	evaluationCanceled := evaluationCtx.Err() != nil
	cancel()
	files, artifactErr := self.collectFiles(self.workDirectory(runId))
	completionCtx, completionCancel := context.WithTimeout(context.WithoutCancel(ctx), self.settings.ControlTimeout)
	defer completionCancel()
	lock, err = self.lock(completionCtx)
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err = self.loadState(runId)
	if err != nil {
		return err
	}
	// Normal result cleanup is an evaluator gate, not proof that Docker/BuildKit
	// resources outside this service cgroup have joined. ExecStopPost owns that.
	state.ExitCode, state.Files, state.CleanupVerified, state.StopCleanupVerified = exitCode, files, false, false
	state.State, state.ErrorCode = "succeeded", ""
	if runErr != nil || exitCode != 0 {
		state.State, state.ErrorCode = "failed", "evaluator_process_failed"
	}
	if artifactErr != nil {
		state.State, state.ErrorCode, state.Files = "failed", "artifact_authentication_failed", nil
	}
	if state.CancelRequested || evaluationCanceled {
		state.State, state.ErrorCode = "canceled", "evaluation_canceled"
	}
	if evaluationExpired {
		state.State, state.ErrorCode = "expired", "original_deadline_elapsed"
	}
	return self.storeState(runId, state)
}

// A no-op self-check probes containment without loading any server services.
// Its environment contains no inherited database, vault or SSH credentials.
func (self *Agent) check(ctx context.Context) (Response, error) {
	if err := validatePin(self.config.SelfCheck, self.protected); err != nil {
		return Response{}, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, self.settings.SelfCheckTimeout)
	defer cancel()
	output := &boundedOutput{maximum: self.settings.DiagnosticBytes}
	arguments := []string{"--json"}
	var boundary *RuntimeResourceBoundary
	if self.protected {
		var err error
		boundary, err = self.runtimeAdmission(checkCtx)
		if err != nil {
			return Response{Schema: Schema, HostId: self.config.HostId, State: "unavailable", ExitCode: -1, ErrorCode: "private_runtime_unqualified"}, nil
		}
		defer boundary.Close()
		if err := self.validateHostCheckConfiguration(boundary); err != nil {
			return Response{Schema: Schema, HostId: self.config.HostId, State: "unavailable", ExitCode: -1, ErrorCode: "host_check_configuration_invalid"}, nil
		}
		arguments = append(arguments, "--config", self.config.HostCheckConfiguration.Path, "--private-runtime", self.config.Agent.Path,
			"--agent-config", self.config.configPath, "--boundary", self.config.DockerRole.ResourceBoundary.Path)
	}
	command := exec.CommandContext(checkCtx, self.config.SelfCheck.Path, arguments...)
	command.Env = controlEnvironment()
	command.Stdout, command.Stderr = output, &discardWriter{}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if !self.protected {
		command.SysProcAttr.Credential = self.credential
	}
	exitCode, err := runProcessGroup(checkCtx, command, self.settings.CancelGrace)
	response := Response{Schema: Schema, HostId: self.config.HostId, State: "ready", ExitCode: exitCode}
	if output.exceeded || !json.Valid(output.Bytes()) {
		response.State, response.ErrorCode = "unavailable", "self_check_invalid"
		return response, nil
	}
	response.SelfCheck = append(json.RawMessage(nil), output.Bytes()...)
	if err != nil || exitCode != 0 {
		response.State, response.ErrorCode = "unavailable", "self_check_failed"
	}
	if boundary != nil && boundary.Recheck(checkCtx) != nil {
		response.State, response.ErrorCode = "unavailable", "private_runtime_changed"
	}
	return response, nil
}

// No new execution can overlap either an uncertain run or unverified cleanup.
// Only a durable terminal proof plus an inactive service releases old custody.
func (self *Agent) requireFreeSlot(ctx context.Context, runId string) error {
	slot, err := self.loadSlot()
	if errors.Is(err, os.ErrNotExist) || err == nil && (slot.RunId == "" || slot.RunId == runId) {
		return nil
	}
	if err != nil {
		return err
	}
	state, err := self.loadState(slot.RunId)
	if err != nil || !terminalState(state.State) || !state.CleanupVerified || state.LaunchAttempted && !state.StopCleanupVerified {
		return errors.New("physical executor slot is occupied or cleanup is uncertain")
	}
	if state.LaunchAttempted {
		controlCtx, cancel := context.WithTimeout(ctx, self.settings.ControlTimeout)
		unit, unitErr := self.runner.Status(controlCtx, unitName(slot.RunId))
		cancel()
		if unitErr != nil || unit.Active {
			return errors.New("previous evaluator service has not joined")
		}
	}
	return nil
}

// A direct indexed record prevents resetting a submission's budget on a known
// later attempt, without scanning a growing historical run directory.
func (self *Agent) requireOriginalClock(identity Identity) error {
	key := sha256.Sum256([]byte(fmt.Sprintf("competition-job-clock/v1\x00%s\x00%d", identity.JobId, identity.Cycle)))
	name := hex.EncodeToString(key[:]) + ".json"
	directory := filepath.Join(self.config.StateDirectory, "clocks")
	content, err := readBounded(filepath.Join(directory, name), self.config.Limits.DocumentBytes)
	if errors.Is(err, os.ErrNotExist) {
		clock := jobClock{JobId: identity.JobId, RoundId: identity.RoundId, Cycle: identity.Cycle, StartedAt: identity.StartedAt, DeadlineAt: identity.DeadlineAt}
		content, err := json.Marshal(clock)
		if err != nil {
			return err
		}
		return atomicFile(directory, name, content)
	}
	if err != nil {
		return err
	}
	clock := jobClock{}
	if err := decodeStrict(content, &clock); err != nil || clock.JobId != identity.JobId || clock.RoundId != identity.RoundId || clock.Cycle != identity.Cycle ||
		!clock.StartedAt.Equal(identity.StartedAt) || !clock.DeadlineAt.Equal(identity.DeadlineAt) {
		return errors.New("subevaluation cannot reset the original submission clock")
	}
	return nil
}

// The signed identity is checked before opening a candidate-controlled byte.
func (self *Agent) validateManifest(manifest StartManifest) error {
	if err := manifest.Identity.Validate(self.config.Limits); err != nil {
		return err
	}
	if manifest.Identity.HostId != self.config.HostId || manifest.EvaluatorImageDigest != self.config.EvaluatorImageDigest ||
		manifest.EvaluatorCommandSha256 != self.config.Evaluator.Sha256 || manifest.SourceLockSha256 != self.config.SourceLockSha256 ||
		manifest.QualificationSha256 != self.config.QualificationSha256 || manifest.ConfigLocalSha256 != self.config.ConfigLocalSha256 ||
		manifest.VaultLocalSha256 != self.config.VaultLocalSha256 || !digestPattern.MatchString(manifest.RoundBaselineSha256) ||
		self.settings.Now().UTC().Add(self.settings.MaximumClockSkew).Before(manifest.Identity.StartedAt) {
		return errors.New("subevaluation manifest differs from the installed restricted role")
	}
	if err := ValidateFiles(manifest.Inputs, self.config.Limits.InputBytes, self.config.Limits); err != nil {
		return err
	}
	if len(manifest.Inputs) != 3 {
		return errors.New("subevaluation requires exactly patch, workload and shared baseline inputs")
	}
	inputPaths := map[string]File{}
	for _, input := range manifest.Inputs {
		if input.Path != "canonical.patch" && input.Path != "providers.yml" && input.Path != "round-baseline.json" {
			return errors.New("unsupported subevaluation input path")
		}
		inputPaths[input.Path] = input
	}
	if inputPaths["round-baseline.json"].Sha256 != manifest.RoundBaselineSha256 || inputPaths["providers.yml"].Bytes <= 0 || inputPaths["round-baseline.json"].Bytes <= 0 {
		return errors.New("shared baseline or workload identity is missing")
	}
	request, err := decodeEvaluatorRequest(manifest.EvaluatorRequest)
	if err != nil {
		return err
	}
	if request.Schema != 1 || request.Staging || request.JobId != manifest.Identity.JobId || request.RoundId != manifest.Identity.RoundId ||
		request.SourceEpoch != manifest.Identity.SourceEpoch || request.Attempt != manifest.Identity.Attempt ||
		request.EvaluatorImageDigest != manifest.EvaluatorImageDigest || request.RoundBaselineSha256 != manifest.RoundBaselineSha256 ||
		request.PatchSha256 != inputPaths["canonical.patch"].Sha256 || request.ProvidersSha256 != inputPaths["providers.yml"].Sha256 ||
		request.ArtifactDirectory != "." || request.PatchPath != "canonical.patch" || request.ProvidersPath != "providers.yml" ||
		request.RoundBaselinePath != "round-baseline.json" || request.ConfigLocalDirectory != "@config-local" || request.VaultLocalDirectory != "@vault-local" {
		return errors.New("subevaluation request is not bound to its immutable named inputs")
	}
	var policy struct {
		HostQualificationSha256 string `json:"host_qualification_sha256"`
		ConfigLocalSha256       string `json:"config_local_sha256"`
		VaultLocalSha256        string `json:"vault_local_sha256"`
		ScoreTimeoutSeconds     int    `json:"score_timeout_seconds"`
		G2VolumePolicy          string `json:"g2_volume_policy"`
	}
	if err := json.Unmarshal(request.EvaluationPolicy, &policy); err != nil || policy.HostQualificationSha256 != manifest.QualificationSha256 ||
		policy.ConfigLocalSha256 != manifest.ConfigLocalSha256 || policy.VaultLocalSha256 != manifest.VaultLocalSha256 ||
		policy.ScoreTimeoutSeconds <= 0 || time.Duration(policy.ScoreTimeoutSeconds)*time.Second > self.config.Limits.MaximumJobDuration ||
		policy.G2VolumePolicy != "minimum-work/1" {
		return errors.New("subevaluation local-fixture or prospective scoring policy mismatch")
	}
	return nil
}

// The wire request is still the existing strict evaluator schema. Policies are
// preserved byte-for-byte semantically; only six named path fields are remapped.
type localEvaluatorRequest struct {
	Staging              bool            `json:"staging,omitempty"`
	Schema               int             `json:"schema"`
	JobId                string          `json:"job_id"`
	RoundId              string          `json:"round_id"`
	SourceEpoch          int             `json:"source_epoch"`
	Attempt              int             `json:"attempt"`
	CompetitionId        string          `json:"competition_id"`
	BaseSha              string          `json:"base_sha"`
	EvaluatorImageDigest string          `json:"evaluator_image_digest"`
	ApiImageDigest       string          `json:"api_image_digest"`
	WorkerImageDigest    string          `json:"worker_image_digest"`
	ScorerVersion        string          `json:"scorer_version"`
	RoundSeedHex         string          `json:"round_seed_hex"`
	ProvidersPath        string          `json:"providers_path"`
	ProvidersSha256      string          `json:"providers_sha256"`
	RoundBaselinePath    string          `json:"round_baseline_path"`
	RoundBaselineSha256  string          `json:"round_baseline_sha256"`
	PatchPath            string          `json:"patch_path"`
	PatchSha256          string          `json:"patch_sha256"`
	ArtifactDirectory    string          `json:"artifact_directory"`
	ConfigLocalDirectory string          `json:"config_local_directory"`
	VaultLocalDirectory  string          `json:"vault_local_directory"`
	PatchPolicy          json.RawMessage `json:"patch_policy"`
	EvaluationPolicy     json.RawMessage `json:"evaluation_policy"`
}

// Decoding rejects an expanded root surface before materializing any request.
func decodeEvaluatorRequest(content []byte) (*localEvaluatorRequest, error) {
	request := &localEvaluatorRequest{}
	if err := decodeStrict(content, request); err != nil {
		return nil, err
	}
	return request, nil
}

// The trusted host role supplies real paths; callers cannot introduce mounts.
func (self *Agent) materializeRequest(incoming, destination string, manifest StartManifest) error {
	request, err := decodeEvaluatorRequest(manifest.EvaluatorRequest)
	if err != nil {
		return err
	}
	request.ArtifactDirectory = destination
	request.PatchPath = filepath.Join(destination, "canonical.patch")
	request.ProvidersPath = filepath.Join(destination, "providers.yml")
	request.RoundBaselinePath = filepath.Join(destination, "round-baseline.json")
	request.ConfigLocalDirectory, request.VaultLocalDirectory = self.config.ConfigLocalDirectory, self.config.VaultLocalDirectory
	content, err := json.Marshal(request)
	request.RoundSeedHex = ""
	if err != nil {
		return err
	}
	defer clear(content)
	return exclusiveFile(filepath.Join(incoming, "worker-request.json"), append(content, '\n'), 0400)
}

// The worker may access only its private workspace, never the signing key or
// durable manifests. Each committed file starts read-only for the worker.
func (self *Agent) prepareWorkerOwnership(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if !entry.Type().IsRegular() {
			return errors.New("unexpected non-regular staged input")
		}
		if self.credential != nil {
			if err := os.Chown(path, int(self.credential.Uid), int(self.credential.Gid)); err != nil {
				return err
			}
		}
		if err := os.Chmod(path, 0400); err != nil {
			return err
		}
	}
	if self.credential != nil {
		if err := os.Chown(directory, int(self.credential.Uid), int(self.credential.Gid)); err != nil {
			return err
		}
	}
	return os.Chmod(directory, 0700)
}

// Executes the pinned evaluator as the isolated host account with a fresh
// environment. Cancellation joins its process group and bounded normal cleanup.
func (self *Agent) runEvaluator(ctx context.Context, manifest *StartManifest, plan *cleanupPlan) (int, error) {
	if err := validatePin(self.config.Evaluator, self.protected); err != nil {
		return -1, err
	}
	directory := self.workDirectory(manifest.Identity.RunId)
	stderr, err := os.OpenFile(filepath.Join(directory, "worker.stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return -1, err
	}
	defer stderr.Close()
	stdout, err := os.OpenFile(filepath.Join(directory, "worker.stdout.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return -1, err
	}
	defer stdout.Close()
	stderrWriter := &boundedFileWriter{writer: stderr, maximum: self.settings.DiagnosticBytes}
	stdoutWriter := &boundedFileWriter{writer: stdout, maximum: self.settings.DiagnosticBytes}
	command := exec.Command(self.config.Evaluator.Path, "--request", filepath.Join(directory, "worker-request.json"), "--result", filepath.Join(directory, "worker-result.json"))
	command.Dir = directory
	command.Env = append(controlEnvironment(), "HOME="+directory, "USER="+self.config.WorkerUser, "LOGNAME="+self.config.WorkerUser)
	command.Env = append(command.Env, subevaluationScopeEnvironment(manifest.Identity)...)
	command.Env = append(command.Env, "URNETWORK_SUBEVALUATION_CLIENT="+self.config.Agent.Path)
	command.Env = append(command.Env, "URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET="+plan.ManagementCpuSet)
	command.Stdout, command.Stderr = stdoutWriter, stderrWriter
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: self.credential}
	exitCode, runErr := runProcessGroup(ctx, command, self.settings.CancelGrace)
	stderrSyncErr, stdoutSyncErr := stderr.Sync(), stdout.Sync()
	if stderrWriter.exceeded || stdoutWriter.exceeded || stderrSyncErr != nil || stdoutSyncErr != nil {
		return exitCode, errors.New("evaluator diagnostics exceeded their bound or failed persistence")
	}
	return exitCode, runErr
}

// Inputs and the remapped hidden request must still match their committed bytes
// immediately before process ownership transfers to the evaluator.
func (self *Agent) authenticateInputFiles(manifest StartManifest) error {
	for _, input := range manifest.Inputs {
		actual, err := self.fileDigest(self.workDirectory(manifest.Identity.RunId), input.Path)
		if err != nil || actual.Sha256 != input.Sha256 || actual.Bytes != input.Bytes {
			return errors.New("committed evaluator input failed authentication")
		}
	}
	return nil
}

// The terminal inventory bounds work examined, not just files returned. It
// rejects links, devices, deep trees and excess directories before hashing data.
func (self *Agent) collectFiles(root string) ([]File, error) {
	confined, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer confined.Close()
	files := []File{}
	pending := []string{"."}
	entriesSeen := 0
	total := int64(0)
	for len(pending) != 0 {
		relative := pending[0]
		pending = pending[1:]
		directory, err := confined.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		for {
			entries, readErr := directory.ReadDir(self.settings.DirectoryBatch)
			for _, entry := range entries {
				entriesSeen++
				if entriesSeen > self.config.Limits.FileCount {
					directory.Close()
					return nil, errors.New("artifact tree entry count exceeds its bound")
				}
				name := filepath.ToSlash(filepath.Join(relative, entry.Name()))
				if entry.Type()&os.ModeSymlink != 0 {
					directory.Close()
					return nil, errors.New("artifact tree contains a link")
				}
				if entry.IsDir() {
					pending = append(pending, name)
					continue
				}
				file, err := self.fileDigest(root, name)
				if err != nil || self.config.Limits.ArtifactBytes-total < file.Bytes {
					directory.Close()
					return nil, errors.New("artifact tree exceeds its bound or failed authentication")
				}
				total += file.Bytes
				files = append(files, file)
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				directory.Close()
				return nil, readErr
			}
		}
		if err := directory.Close(); err != nil {
			return nil, err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, ValidateFiles(files, self.config.Limits.ArtifactBytes, self.config.Limits)
}

// A single regular file is counted and hashed through the same bounded descriptor.
func (self *Agent) fileDigest(root, name string) (File, error) {
	file, err := openRegular(root, name)
	if err != nil {
		return File{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 0 || self.config.Limits.FileBytes < info.Size() {
		return File{}, errors.New("artifact file exceeds its bound")
	}
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(file, self.config.Limits.FileBytes+1))
	if err != nil || count != info.Size() || count > self.config.Limits.FileBytes {
		return File{}, errors.New("artifact file changed while authenticating")
	}
	return File{Path: name, Sha256: hex.EncodeToString(digest.Sum(nil)), Bytes: count}, nil
}

// Disk reads are strict and bounded; state corruption cannot clear custody.
func (self *Agent) loadState(runId string) (*runState, error) {
	content, err := readBounded(filepath.Join(self.stateDirectory(runId), "state.json"), self.config.Limits.DocumentBytes)
	if err != nil {
		return nil, err
	}
	defer clear(content)
	state := &runState{}
	if err := decodeStrict(content, state); err != nil {
		return nil, err
	}
	if state.Schema != Schema || state.FencingToken <= 0 || !digestPattern.MatchString(state.ManifestSha256) || !digestPattern.MatchString(state.RequestSha256) || !digestPattern.MatchString(state.CleanupPlanSha256) {
		return nil, errors.New("invalid durable subevaluation state")
	}
	return state, nil
}

// The full manifest includes the hidden seed and stays in root-only state.
func (self *Agent) loadManifest(runId, expected string) (*StartManifest, error) {
	content, err := readBounded(filepath.Join(self.stateDirectory(runId), "manifest.json"), self.config.Limits.DocumentBytes)
	if err != nil {
		return nil, err
	}
	defer clear(content)
	manifest := &StartManifest{}
	if err := decodeStrict(content, manifest); err != nil {
		return nil, err
	}
	digest, err := ManifestDigest(*manifest)
	if err != nil || digest != expected || manifest.Identity.RunId != runId || manifest.Identity.HostId != self.config.HostId {
		return nil, errors.New("durable subevaluation manifest failed authentication")
	}
	return manifest, nil
}

// Both file and directory persistence precede a service launch acknowledgement.
func (self *Agent) storeState(runId string, state *runState) error {
	content, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicFile(self.stateDirectory(runId), "state.json", content)
}

// The fixed slot contains no caller-selected path and is never pruned on startup.
func (self *Agent) loadSlot() (*slotState, error) {
	content, err := readBounded(filepath.Join(self.config.StateDirectory, "slot.json"), self.config.Limits.DocumentBytes)
	if err != nil {
		return nil, err
	}
	slot := &slotState{}
	if err := decodeStrict(content, slot); err != nil || slot.RunId != "" && !digestPattern.MatchString(slot.RunId) {
		return nil, errors.New("invalid durable physical slot")
	}
	return slot, nil
}

// Empty is used only when no launch could have occurred; uncertain runs remain.
func (self *Agent) storeSlot(runId string) error {
	content, err := json.Marshal(slotState{RunId: runId})
	if err != nil {
		return err
	}
	return atomicFile(self.config.StateDirectory, "slot.json", content)
}

// Copies only non-secret provenance into the authenticated response envelope.
func (self *Agent) response(manifest *StartManifest, state *runState) Response {
	identity := manifest.Identity
	return Response{Schema: Schema, HostId: self.config.HostId, RunId: identity.RunId, Identity: &identity,
		FencingToken: state.FencingToken, State: state.State, ManifestSha256: state.ManifestSha256,
		RequestSha256: state.RequestSha256, ExitCode: state.ExitCode, ErrorCode: state.ErrorCode,
		Files: append([]File(nil), state.Files...)}
}

// Caller-visible terminal status requires an atomically committed inventory.
func terminalState(state string) bool {
	return state == "succeeded" || state == "failed" || state == "canceled" || state == "expired"
}

// Only validated digest keys reach these fixed protected roots.
func (self *Agent) stateDirectory(runId string) string {
	return filepath.Join(self.config.StateDirectory, "runs", runId)
}

// Work is physically separate from root-only signatures and custody metadata.
func (self *Agent) workDirectory(runId string) string {
	return filepath.Join(self.config.WorkDirectory, runId)
}

// Cross-process locking is cancellable and bounded by the control request owner.
func (self *Agent) lock(ctx context.Context) (*os.File, error) {
	return self.lockFile(ctx, filepath.Join(self.config.StateDirectory, "agent.lock"))
}

// Cleanup has its own run-local lock, independent of admission publication.
func (self *Agent) lockFile(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return file, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(self.settings.LockPollInterval):
		}
	}
}

// Atomic replacement never changes the enclosing authority or permissions.
func atomicFile(directory, name string, content []byte) error {
	file, err := os.CreateTemp(directory, ".commit-")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(directory, name)); err != nil {
		return err
	}
	return syncDirectory(directory)
}

// New evidence files cannot overwrite a prior request or sibling run.
func exclusiveFile(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(content); err != nil {
		return err
	}
	return file.Sync()
}

// Directory sync makes newly created names durable, not just their contents.
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Transfer ownership independently interrupts a blocked read on cancellation.
// This adapter additionally refuses consumption between reads after expiry.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

// An expired owner cannot continue consuming queued input between pipe reads.
func (self *contextReader) Read(content []byte) (int, error) {
	if err := self.ctx.Err(); err != nil {
		return 0, err
	}
	return self.reader.Read(content)
}

// Fixed-size diagnostics never let a child fill the host filesystem.
type boundedFileWriter struct {
	writer   io.Writer
	maximum  int64
	written  int64
	exceeded bool
}

// Excess diagnostics are drained, marked failed and omitted without blocking.
func (self *boundedFileWriter) Write(content []byte) (int, error) {
	count := len(content)
	remaining := self.maximum - self.written
	if remaining < int64(count) {
		self.exceeded = true
		if remaining <= 0 {
			return count, nil
		}
		content = content[:int(remaining)]
	}
	written, err := self.writer.Write(content)
	self.written += int64(written)
	if err != nil {
		return written, err
	}
	return count, nil
}

// Group cancellation joins all host subprocess pipes, including a descendant
// retaining stderr. The systemd unit is the independent hard-kill backstop.
func runProcessGroup(ctx context.Context, command *exec.Cmd, grace time.Duration) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	if err := command.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		select {
		case runErr = <-done:
		case <-time.After(grace):
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			runErr = <-done
		}
		if runErr == nil {
			runErr = ctx.Err()
		}
	}
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	return exitCode, runErr
}
