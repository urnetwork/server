// The fixed recovery role consumes root custody only, independently of worker
// accounts, candidate mounts, signing keys and private engine admission health.
package competitionagent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This is a per-host singleton, never a run-supplied or templated target.
const managementRecoveryServiceName = "urnetwork-subeval-recovery.service"

// Parsing is strict, but cleanup validates only the static authority it needs.
// It does not require worker execution, local fixture readability or a daemon.
func LoadRecoveryConfig(path string) (*Config, error) {
	settings := DefaultAgentSettings()
	if err := protectedRootPath(path, false, true); err != nil {
		return nil, err
	}
	content, err := readBounded(path, settings.ConfigBytes)
	if err != nil {
		return nil, err
	}
	defer clear(content)
	config := &Config{Limits: DefaultLimits(), configPath: path}
	if validateDocumentKeys(content) != nil || decodeStrict(content, config) != nil {
		return nil, errors.New("invalid protected recovery configuration")
	}
	if config.StateDirectory == "" {
		config.StateDirectory = filepath.Join(config.BaseDirectory, "state")
	}
	if config.WorkDirectory == "" {
		config.WorkDirectory = filepath.Join(config.BaseDirectory, "jobs")
	}
	digest := sha256.Sum256(content)
	config.configSha256 = hex.EncodeToString(digest[:])
	if err := config.validateRecoveryConfiguration(settings, true); err != nil {
		return nil, err
	}
	return config, nil
}

// Configured paths have no systemd expansion or ambiguous displayed argv. The
// rendered recovery service/ExecStart contract intentionally fails closed on
// whitespace/quoting authority paths rather than weakening manager comparison.
func recoveryAuthorityPath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && value != "/" && !strings.ContainsAny(value, " \t\r\n\x00$%\"'\\;{}")
}

// Recovery needs only fixed root control/data authority; an exhausted/dead
// ordinary engine or absent worker account cannot disable this validation.
func (self *Config) validateRecoveryConfiguration(settings AgentSettings, protected bool) error {
	if self == nil || self.Schema != Schema || !namePattern.MatchString(self.HostId) || self.TrustedOperatorUser == "" || self.ExpectedDataDeviceId == 0 ||
		!recoveryAuthorityPath(self.configPath) || !recoveryAuthorityPath(self.Agent.Path) || !recoveryAuthorityPath(self.BaseDirectory) ||
		self.StateDirectory == self.BaseDirectory || !pathWithin(self.BaseDirectory, self.StateDirectory) || !pathWithin(self.BaseDirectory, self.WorkDirectory) ||
		!recoveryAuthorityPath(self.StateDirectory) || !recoveryAuthorityPath(self.WorkDirectory) || filepath.Base(self.RecoveryService.Path) != managementRecoveryServiceName ||
		!recoveryAuthorityPath(self.RecoveryService.Path) || !digestPattern.MatchString(self.RecoveryService.Sha256) || settings.ConfigBytes <= 0 ||
		settings.RecoveryMemoryHeadroomBytes <= 0 || settings.RecoveryTasksHeadroom <= 0 {
		return errors.New("recovery requires exact static protected role authority")
	}
	if self.Limits.Validate() != nil || self.Limits.MaximumJobDuration > DefaultLimits().MaximumJobDuration {
		return errors.New("recovery cannot widen original job limits")
	}
	for _, value := range []string{self.SourceLockSha256, self.QualificationSha256, self.ConfigLocalSha256, self.VaultLocalSha256, self.Evaluator.Sha256} {
		if !digestPattern.MatchString(value) {
			return errors.New("recovery omitted frozen evaluator/fixture identity")
		}
	}
	key, err := base64.StdEncoding.DecodeString(self.CoordinatorPublicKeyBase64)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("recovery omitted its independent coordinator public identity")
	}
	if validatePin(self.Agent, protected) != nil || validatePin(self.Docker, protected) != nil {
		return errors.New("recovery control executables do not match protected pins")
	}
	if _, err := loadManagementResourcePolicy(self.ManagementPolicy, DefaultManagementBoundarySettings(), settings.ConfigBytes, protected); err != nil {
		return err
	}
	for _, pin := range []Pin{self.RecoveryService, self.DockerRole.ResourceBoundary} {
		if err := validateFilePin(pin, settings.ConfigBytes, protected); err != nil {
			return err
		}
	}
	if protected {
		operator, err := trustedOperatorUid(self.TrustedOperatorUser)
		if err != nil {
			return err
		}
		if protectedRuntimePath(self.BaseDirectory, self.BaseDirectory, operator) != nil || protectedRuntimePath(self.StateDirectory, self.BaseDirectory, operator) != nil || self.requireDataMount() != nil {
			return errors.New("recovery lost protected independent journal storage")
		}
		info, err := os.Lstat(self.StateDirectory)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			return errors.New("recovery journal root is not private")
		}
	}
	return nil
}

// No state directories, worker probes, signing-key reads or engine admission
// are performed. Existing roots and physical slot state must already exist.
func NewRecoveryAgent(config *Config, runner Runner, settings AgentSettings) (*Agent, error) {
	return newRecoveryAgent(config, runner, settings, true)
}

func newRecoveryAgent(config *Config, runner Runner, settings AgentSettings, protected bool) (*Agent, error) {
	if runner == nil || settings.Now == nil || settings.CleanupTimeout <= 0 || settings.ControlTimeout <= 0 || settings.CancelGrace <= 0 || settings.MaximumClockSkew <= 0 || settings.DiagnosticBytes <= 0 || settings.DirectoryBatch <= 0 || settings.CleanupObjectCount <= 0 || settings.OwnerPollInterval <= 0 || settings.ManagementCpuQuotaPercent <= 0 || settings.ContainmentCpuPeriodMicros <= 0 || settings.CleanupCommandTimeout <= 0 || settings.CleanupCommandTimeout > settings.CleanupTimeout || settings.CleanupPollInterval <= 0 || settings.LockPollInterval <= 0 || settings.ConfigBytes <= 0 {
		return nil, errors.New("recovery has no bounded control owner")
	}
	if err := config.validateRecoveryConfiguration(settings, protected); err != nil {
		return nil, err
	}
	public, err := base64.StdEncoding.DecodeString(config.CoordinatorPublicKeyBase64)
	if err != nil {
		return nil, err
	}
	self := &Agent{config: *config, settings: settings, runner: runner, cleaner: newHostCleaner(*config, settings), containment: newHostRunContainment(*config, settings, nil),
		coordinatorPublicKey: public, protected: protected, recoveryOnly: true}
	self.recoveryPlacement = self.verifyRecoveryPlacement
	return self, nil
}

// Only the fixed physical slot chooses a run. Missing/corrupt state is unknown,
// not an empty slot. No deadline, attempt, launch marker or slot is reset here.
func (self *Agent) RecoverActive(ctx context.Context) error {
	if self == nil || self.recoveryPlacement == nil {
		return errors.New("recovery has no independent placement verifier")
	}
	timeout, err := recoveryServiceTimeout(self.settings)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := self.recoveryPlacement(ctx, true); err != nil {
		return err
	}
	slot, err := self.loadSlot()
	if err != nil {
		return errors.New("recovery physical slot is missing or unauthenticated")
	}
	if slot.RunId == "" {
		return nil
	}
	// Unlike cleanup.lock, this lease spans the stop operation. ExecStopPost
	// may still take cleanup.lock while duplicate recovery processes wait here.
	recovery, err := self.lockFile(ctx, filepath.Join(self.stateDirectory(slot.RunId), "recovery.lock"))
	if err != nil {
		return err
	}
	defer recovery.Close()
	state, err := self.loadState(slot.RunId)
	if err != nil || !state.LaunchAttempted {
		return errors.New("recovery slot has no authenticated attempted run")
	}
	manifest, err := self.loadManifest(slot.RunId, state.ManifestSha256)
	if err != nil || self.validateManifest(*manifest) != nil {
		return errors.New("recovery manifest does not match its original frozen role")
	}
	if err := self.verifyRecoveryClock(manifest.Identity); err != nil {
		return err
	}
	plan, err := self.loadCleanupPlan(slot.RunId, state, manifest)
	if err != nil {
		return err
	}
	if err := self.recoveryPlacement(ctx, true); err != nil {
		return err
	}
	state, err = self.recoveryCustody(ctx, slot.RunId, state, false)
	if err != nil {
		return err
	}
	// Owner failure can precede evaluator BindsTo teardown. Freeze exact
	// parents without publication locks before waiting on any unit stop.
	if !state.StopCleanupVerified || !state.CleanupVerified {
		if err := self.containment.EmergencyFreeze(ctx, *plan); err != nil {
			return err
		}
	}
	stopCtx, stop := context.WithTimeout(ctx, self.settings.CleanupTimeout+self.settings.CancelGrace)
	_ = self.runner.Stop(stopCtx, unitName(slot.RunId))
	stop()
	statusCtx, finish := context.WithTimeout(ctx, self.settings.ControlTimeout)
	unit, statusErr := self.runner.Status(statusCtx, unitName(slot.RunId))
	finish()
	if statusErr != nil || !unit.Exists || unit.Active {
		return errors.New("recovery evaluator stop is still missing, active or uncertain")
	}
	if err := self.Cleanup(ctx, slot.RunId); err != nil {
		return err
	}
	_, err = self.recoveryCustody(ctx, slot.RunId, state, true)
	return err
}

// Fsync custody before any native stop/freeze, then clear only after positive
// cleanup. No external manager/cgroup/daemon I/O retains this publication lock.
// A crashed or uncertain recovery deliberately leaves the physical slot held.
func (self *Agent) recoveryCustody(ctx context.Context, runId string, expected *runState, complete bool) (*runState, error) {
	lock, err := self.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	slot, err := self.loadSlot()
	if err != nil || slot.RunId != runId {
		return nil, errors.New("recovery cannot act after physical slot custody changed")
	}
	state, err := self.loadState(runId)
	if err != nil || !state.LaunchAttempted || state.ManifestSha256 != expected.ManifestSha256 || state.CleanupPlanSha256 != expected.CleanupPlanSha256 {
		return nil, errors.New("recovery lost its authenticated attempted run")
	}
	if complete && (!state.RecoveryActive || !state.CleanupVerified || !state.StopCleanupVerified) {
		return nil, errors.New("recovery cannot release unjoined cleanup custody")
	}
	state.RecoveryActive = !complete
	if err := self.storeState(runId, state); err != nil {
		return nil, err
	}
	return state, nil
}

// Cleanup never creates a missing clock: original submission custody is read,
// compared exactly and retained even after the original deadline has elapsed.
func (self *Agent) verifyRecoveryClock(identity Identity) error {
	key := sha256.Sum256([]byte("competition-job-clock/v1\x00" + identity.JobId + "\x00" + strconv.Itoa(identity.Cycle)))
	content, err := readBounded(filepath.Join(self.config.StateDirectory, "clocks", hex.EncodeToString(key[:])+".json"), self.config.Limits.DocumentBytes)
	if err != nil {
		return errors.New("recovery cannot recreate a missing original job clock")
	}
	var clock jobClock
	if validateDocumentKeys(content) != nil || decodeStrict(content, &clock) != nil || clock.JobId != identity.JobId || clock.RoundId != identity.RoundId || clock.Cycle != identity.Cycle || !clock.StartedAt.Equal(identity.StartedAt) || !clock.DeadlineAt.Equal(identity.DeadlineAt) {
		return errors.New("recovery original job clock differs from its frozen manifest")
	}
	return nil
}
