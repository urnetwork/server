//go:build linux

// An explicit operator amendment can close an authenticated prelaunch attempt.
// Missing units alone never prove safety, and the original cleanup is unchanged.
package competitionagent

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
	"reflect"
)

// All pins are operator-installed root authority, not peer-supplied proof.
// OriginalConfig remains the historical execution role, never the new helper.
type LegacyPrelaunchAuthorization struct {
	Schema                         int                          `json:"schema"`
	Kind                           string                       `json:"kind"`
	ApprovalSha256                 string                       `json:"approval_sha256"`
	HostId                         string                       `json:"host_id"`
	RunId                          string                       `json:"run_id"`
	StateSha256                    string                       `json:"state_sha256"`
	ManifestSha256                 string                       `json:"manifest_sha256"`
	CleanupPlanSha256              string                       `json:"cleanup_plan_sha256"`
	ClockSha256                    string                       `json:"clock_sha256"`
	BootId                         string                       `json:"boot_id"`
	OriginalConfig                 Pin                          `json:"original_config"`
	OriginalRecoveryServiceArchive Pin                          `json:"original_recovery_service_archive"`
	RecoveryAgent                  Pin                          `json:"recovery_agent"`
	RecoveryService                Pin                          `json:"recovery_service"`
	IngressWrapper                 Pin                          `json:"ingress_wrapper"`
	OriginalIngressWrapper         Pin                          `json:"original_ingress_wrapper"`
	OriginalIngressWrapperArchive  Pin                          `json:"original_ingress_wrapper_archive"`
	AuthorizedKeys                 Pin                          `json:"authorized_keys"`
	Sshd                           Pin                          `json:"sshd"`
	ControlUser                    string                       `json:"control_user"`
	OwnerStartDenial               Pin                          `json:"owner_start_denial"`
	PriorControlRoles              []LegacyPrelaunchControlRole `json:"prior_control_roles,omitempty"`
}

// Retiring an old PID1 record must not erase its launcher coverage. The exact
// historical root executables/configs remain independently pinned in the permit.
type LegacyPrelaunchControlRole struct {
	Agent  Pin `json:"agent"`
	Config Pin `json:"config"`
}

// The old accepted state is retained before cleanup mutates its two flags.
// This permits crash resumption without authorizing another execution.
type legacyPrelaunchStarted struct {
	Schema                        int      `json:"schema"`
	AuthorizationSha256           string   `json:"authorization_sha256"`
	OriginalStateSha256           string   `json:"original_state_sha256"`
	OriginalState                 runState `json:"original_state"`
	OriginalEvaluatorStderrAbsent bool     `json:"original_evaluator_stderr_absent"`
}

// The closure is a new proof type, never a fabricated original stop receipt.
type legacyPrelaunchClosed struct {
	Schema                        int    `json:"schema"`
	Kind                          string `json:"kind"`
	AuthorizationSha256           string `json:"authorization_sha256"`
	ApprovalSha256                string `json:"approval_sha256"`
	RunId                         string `json:"run_id"`
	ManifestSha256                string `json:"manifest_sha256"`
	CleanupPlanSha256             string `json:"cleanup_plan_sha256"`
	ClockSha256                   string `json:"clock_sha256"`
	BootId                        string `json:"boot_id"`
	BeforeSha256                  string `json:"before_sha256"`
	AfterSha256                   string `json:"after_sha256"`
	ResourcesSha256               string `json:"resources_sha256"`
	Provenance                    string `json:"provenance"`
	EvaluatorStarted              bool   `json:"evaluator_started"`
	OriginalEvaluatorStderrExists bool   `json:"original_evaluator_stderr_exists"`
}

// The compatibility filename does not imply evaluator output. Its complete
// non-secret closure receipt survives the existing three-object failure archive.
type legacyPrelaunchDiagnostic struct {
	Schema                        int             `json:"schema"`
	Kind                          string          `json:"kind"`
	Provenance                    string          `json:"provenance"`
	EvaluatorStarted              bool            `json:"evaluator_started"`
	OriginalEvaluatorStderrExists bool            `json:"original_evaluator_stderr_exists"`
	Closure                       json.RawMessage `json:"closure"`
}

// Private test owners replace observations, never the custody/cleanup path.
type legacyPrelaunchHost interface {
	Placement(context.Context) error
	CloseControls(context.Context) error
	Verify(context.Context) ([]byte, error)
}

// The CLI loads no signing key, worker authority or private-engine admission.
func RecoverLegacyPrelaunch(ctx context.Context, authorizationPath string) error {
	settings := DefaultAgentSettings()
	authorization, _, err := readLegacyPrelaunchAuthorization(authorizationPath, settings, true)
	if err != nil {
		return err
	}
	if validateFilePin(authorization.OriginalConfig, settings.ConfigBytes, true) != nil {
		return errors.New("legacy closure original role pin changed")
	}
	config, err := loadLegacyPrelaunchOriginalConfig(*authorization, settings)
	if err != nil || config.configSha256 != authorization.OriginalConfig.Sha256 {
		return errors.New("legacy closure original role is unavailable")
	}
	agent, err := NewRecoveryAgent(config, NewSystemdRunner(settings), settings)
	if err != nil {
		return err
	}
	return agent.CloseLegacyPrelaunch(ctx, authorizationPath)
}

// Only a recovery-only instance can consume this local root authorization.
// It cannot execute, relaunch, renew a clock or clear an uncertain cleanup.
func (self *Agent) CloseLegacyPrelaunch(ctx context.Context, authorizationPath string) error {
	if self == nil || !self.recoveryOnly {
		return errors.New("legacy prelaunch closure requires a recovery-only owner")
	}
	authorization, digest, err := readLegacyPrelaunchAuthorization(authorizationPath, self.settings, self.protected)
	if err != nil {
		return err
	}
	host, err := newLegacyPrelaunchNativeHost(self, *authorization, authorizationPath, digest)
	if err != nil {
		return err
	}
	return self.closeLegacyPrelaunch(ctx, authorizationPath, *authorization, digest, host)
}

// Root authority is immutable, single-link and private; duplicate/unknown keys
// and a caller-selected noncanonical path are rejected before native actions.
func readLegacyPrelaunchAuthorization(path string, settings AgentSettings, protected bool) (*LegacyPrelaunchAuthorization, string, error) {
	if !recoveryAuthorityPath(path) || settings.ConfigBytes <= 0 {
		return nil, "", errors.New("legacy closure authorization path is invalid")
	}
	if protected && protectedRootPath(path, false, true) != nil {
		return nil, "", errors.New("legacy closure authorization is not protected root custody")
	}
	if err := legacyPrelaunchSealedFile(path, protected); err != nil {
		return nil, "", err
	}
	content, err := readBounded(path, settings.ConfigBytes)
	if err != nil {
		return nil, "", err
	}
	authorization := &LegacyPrelaunchAuthorization{}
	if validateDocumentKeys(content) != nil || decodeStrict(content, authorization) != nil || authorization.Schema != Schema || authorization.Kind != "competition-legacy-prelaunch-closure/v1" ||
		!namePattern.MatchString(authorization.HostId) || !namePattern.MatchString(authorization.ControlUser) || !cleanupUuidPattern.MatchString(authorization.BootId) {
		return nil, "", errors.New("legacy closure authorization schema is invalid")
	}
	for _, digest := range []string{authorization.ApprovalSha256, authorization.RunId, authorization.StateSha256, authorization.ManifestSha256, authorization.CleanupPlanSha256, authorization.ClockSha256} {
		if !digestPattern.MatchString(digest) {
			return nil, "", errors.New("legacy closure omitted an exact custody digest")
		}
	}
	for _, pin := range []Pin{authorization.OriginalConfig, authorization.OriginalRecoveryServiceArchive, authorization.RecoveryAgent, authorization.RecoveryService, authorization.IngressWrapper, authorization.OriginalIngressWrapper, authorization.OriginalIngressWrapperArchive, authorization.AuthorizedKeys, authorization.Sshd, authorization.OwnerStartDenial} {
		if !recoveryAuthorityPath(pin.Path) || !digestPattern.MatchString(pin.Sha256) {
			return nil, "", errors.New("legacy closure omitted a protected authority pin")
		}
	}
	if len(authorization.PriorControlRoles) > DefaultManagementSshSettings().MaximumControls {
		return nil, "", errors.New("legacy closure prior control-role census exceeded its bound")
	}
	for _, role := range authorization.PriorControlRoles {
		for _, pin := range []Pin{role.Agent, role.Config} {
			if !recoveryAuthorityPath(pin.Path) || !digestPattern.MatchString(pin.Sha256) {
				return nil, "", errors.New("legacy closure prior control role omitted exact authority")
			}
		}
	}
	return authorization, digestDockerBytes(content), nil
}

// The canonical recovery service is temporarily a new operator role. Its old
// exact bytes remain pinned in an archive used only for this cleanup view;
// original config bytes and all execution/qualification identities stay exact.
func loadLegacyPrelaunchOriginalConfig(authorization LegacyPrelaunchAuthorization, settings AgentSettings) (*Config, error) {
	content, err := readRecoveryAuthority(authorization.OriginalConfig, settings.ConfigBytes, true)
	if err != nil {
		return nil, err
	}
	defer clear(content)
	config := &Config{Limits: DefaultLimits(), configPath: authorization.OriginalConfig.Path, configSha256: authorization.OriginalConfig.Sha256}
	if validateDocumentKeys(content) != nil || decodeStrict(content, config) != nil {
		return nil, errors.New("legacy closure original configuration is invalid")
	}
	if config.StateDirectory == "" {
		config.StateDirectory = filepath.Join(config.BaseDirectory, "state")
	}
	if config.WorkDirectory == "" {
		config.WorkDirectory = filepath.Join(config.BaseDirectory, "jobs")
	}
	archive := authorization.OriginalRecoveryServiceArchive
	if archive.Sha256 != config.RecoveryService.Sha256 || archive.Path == config.RecoveryService.Path || filepath.Base(archive.Path) != managementRecoveryServiceName ||
		validateFilePin(archive, settings.ConfigBytes, true) != nil {
		return nil, errors.New("legacy closure did not retain the exact historical recovery service")
	}
	config.RecoveryService = archive
	if err := config.validateRecoveryConfiguration(settings, true); err != nil {
		return nil, err
	}
	return config, nil
}

// The exact original job clock is read without creating or renewing it.
func (self *Agent) legacyPrelaunchClock(manifest *StartManifest) ([]byte, error) {
	if err := self.verifyRecoveryClock(manifest.Identity); err != nil {
		return nil, err
	}
	identity := manifest.Identity
	key := sha256.Sum256([]byte(fmt.Sprintf("competition-job-clock/v1\x00%s\x00%d", identity.JobId, identity.Cycle)))
	return readBounded(filepath.Join(self.config.StateDirectory, "clocks", hex.EncodeToString(key[:])+".json"), self.config.Limits.DocumentBytes)
}

// Cleanup flag changes are allowed only after the immutable starting snapshot;
// all other original fields, including fence and execution markers, stay exact.
func legacyPrelaunchSameState(current, original *runState) bool {
	if current == nil || original == nil {
		return false
	}
	expected := *original
	expected.CleanupVerified, expected.StopCleanupVerified = current.CleanupVerified, current.StopCleanupVerified
	return reflect.DeepEqual(*current, expected)
}

// The original full Cleanup is the only producer of the cleanup/reap flags.
// Every failure retains RecoveryActive and physical-slot custody.
func (self *Agent) closeLegacyPrelaunch(parent context.Context, authorizationPath string, authorization LegacyPrelaunchAuthorization, digest string, host legacyPrelaunchHost) error {
	if !self.recoveryOnly || host == nil || authorization.HostId != self.config.HostId || authorization.OriginalConfig != (Pin{Path: self.config.configPath, Sha256: self.config.configSha256}) {
		return errors.New("legacy closure differs from its original recovery-only host")
	}
	timeout, err := recoveryServiceTimeout(self.settings)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := host.Placement(ctx); err != nil {
		return err
	}
	recovery, err := self.lockFile(ctx, filepath.Join(self.stateDirectory(authorization.RunId), "recovery.lock"))
	if err != nil {
		return err
	}
	defer recovery.Close()
	state, err := self.loadState(authorization.RunId)
	if err != nil {
		return err
	}
	manifest, err := self.loadManifest(authorization.RunId, authorization.ManifestSha256)
	if err != nil || self.validateManifest(*manifest) != nil || state.ManifestSha256 != authorization.ManifestSha256 || state.CleanupPlanSha256 != authorization.CleanupPlanSha256 {
		return errors.New("legacy closure original manifest differs")
	}
	if _, err := self.loadCleanupPlan(authorization.RunId, state, manifest); err != nil {
		return err
	}
	clock, err := self.legacyPrelaunchClock(manifest)
	if err != nil || digestDockerBytes(clock) != authorization.ClockSha256 {
		return errors.New("legacy closure original clock differs")
	}
	startedPath := filepath.Join(self.stateDirectory(authorization.RunId), "legacy-prelaunch-started.json")
	started := legacyPrelaunchStarted{}
	content, readErr := readBounded(startedPath, self.config.Limits.DocumentBytes)
	if errors.Is(readErr, os.ErrNotExist) {
		content, err = readBounded(filepath.Join(self.stateDirectory(authorization.RunId), "state.json"), self.config.Limits.DocumentBytes)
		if err != nil || digestDockerBytes(content) != authorization.StateSha256 {
			return errors.New("legacy closure original state differs from the permit")
		}
		if _, stderrErr := os.Lstat(filepath.Join(self.workDirectory(authorization.RunId), "worker.stderr.log")); !errors.Is(stderrErr, os.ErrNotExist) {
			return errors.New("legacy prelaunch closure cannot replace existing evaluator stderr")
		}
		started = legacyPrelaunchStarted{Schema: Schema, AuthorizationSha256: digest, OriginalStateSha256: authorization.StateSha256, OriginalState: *state, OriginalEvaluatorStderrAbsent: true}
	} else if readErr != nil || validateDocumentKeys(content) != nil || decodeStrict(content, &started) != nil || started.Schema != Schema || started.AuthorizationSha256 != digest || started.OriginalStateSha256 != authorization.StateSha256 {
		return errors.New("legacy closure starting receipt differs")
	}
	original := &started.OriginalState
	if !started.OriginalEvaluatorStderrAbsent {
		return errors.New("legacy closure did not establish original evaluator stderr absence")
	}
	originalBytes, err := json.Marshal(original)
	if err != nil || digestDockerBytes(originalBytes) != authorization.StateSha256 {
		return errors.New("legacy closure retained original state failed its content pin")
	}
	if original.State != "accepted" || !original.LaunchAttempted || original.ExecutionStarted || !original.RecoveryActive || original.CancelRequested || original.CleanupVerified || original.StopCleanupVerified || len(original.Files) != 0 || original.ErrorCode != "" {
		return errors.New("legacy closure is restricted to the exact unexecuted recovery-held attempt")
	}
	if !legacyPrelaunchSameState(state, original) {
		return errors.New("legacy closure original execution state changed")
	}
	publication, err := self.lock(ctx)
	if err != nil {
		return err
	}
	slot, slotErr := self.loadSlot()
	current, stateErr := self.loadState(authorization.RunId)
	if slotErr != nil || slot.RunId != authorization.RunId || stateErr != nil || !legacyPrelaunchSameState(current, original) {
		publication.Close()
		return errors.New("legacy closure no longer owns its original physical slot")
	}
	if errors.Is(readErr, os.ErrNotExist) {
		content, err = json.Marshal(started)
		if err == nil {
			err = exclusiveFile(startedPath, content, 0400)
		}
		if err == nil {
			err = syncDirectory(self.stateDirectory(authorization.RunId))
		}
	}
	publication.Close()
	if err != nil {
		return err
	}
	if err := host.CloseControls(ctx); err != nil {
		return err
	}
	before, err := host.Verify(ctx)
	if err != nil {
		return err
	}
	if err := self.Cleanup(ctx, authorization.RunId); err != nil {
		return err
	}
	after, err := host.Verify(ctx)
	if err != nil {
		return err
	}
	if err := host.Placement(ctx); err != nil {
		return err
	}
	again, againDigest, err := readLegacyPrelaunchAuthorization(authorizationPath, self.settings, self.protected)
	if err != nil || againDigest != digest || !reflect.DeepEqual(*again, authorization) {
		return errors.New("legacy closure authority changed during cleanup")
	}
	clockAfter, err := self.legacyPrelaunchClock(manifest)
	if err != nil || !bytes.Equal(clock, clockAfter) {
		return errors.New("legacy closure changed its original job clock")
	}
	reaped, err := self.loadResourcesReaped(authorization.RunId, authorization.CleanupPlanSha256)
	if err != nil || !reaped {
		return errors.New("legacy closure has no original completed resource reap")
	}
	resources, err := readBounded(filepath.Join(self.stateDirectory(authorization.RunId), "resources-reaped.json"), self.config.Limits.DocumentBytes)
	if err != nil {
		return err
	}
	closed := legacyPrelaunchClosed{Schema: Schema, Kind: "competition-legacy-prelaunch-closed/v1", AuthorizationSha256: digest, ApprovalSha256: authorization.ApprovalSha256,
		RunId: authorization.RunId, ManifestSha256: authorization.ManifestSha256, CleanupPlanSha256: authorization.CleanupPlanSha256, ClockSha256: authorization.ClockSha256,
		BootId: authorization.BootId, BeforeSha256: digestDockerBytes(before), AfterSha256: digestDockerBytes(after), ResourcesSha256: digestDockerBytes(resources),
		Provenance: "operator_legacy_prelaunch_recovery", EvaluatorStarted: false, OriginalEvaluatorStderrExists: false}
	closedBytes, err := json.Marshal(closed)
	if err != nil {
		return err
	}
	closedBytes, err = func() ([]byte, error) {
		publication, err := self.lock(ctx)
		if err != nil {
			return nil, err
		}
		defer publication.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current, err := self.loadState(authorization.RunId)
		slot, slotErr := self.loadSlot()
		if err != nil || slotErr != nil || slot.RunId != authorization.RunId || !legacyPrelaunchSameState(current, original) || !current.CleanupVerified || !current.StopCleanupVerified {
			return nil, errors.New("legacy closure lost its exact joined custody before publication")
		}
		closedPath := filepath.Join(self.stateDirectory(authorization.RunId), "legacy-prelaunch-closed.json")
		if prior, readErr := readBounded(closedPath, self.config.Limits.DocumentBytes); readErr == nil {
			retained := legacyPrelaunchClosed{}
			if legacyPrelaunchSealedFile(closedPath, self.protected) != nil || validateDocumentKeys(prior) != nil || decodeStrict(prior, &retained) != nil {
				return nil, errors.New("legacy closure retained receipt differs")
			}
			// Fresh manager proof bytes may differ, but every immutable field
			// and the retained receipt's canonical bytes remain exact.
			expected := closed
			expected.BeforeSha256, expected.AfterSha256 = retained.BeforeSha256, retained.AfterSha256
			canonical, err := json.Marshal(retained)
			if err != nil || !bytes.Equal(canonical, prior) || !digestPattern.MatchString(retained.BeforeSha256) || !digestPattern.MatchString(retained.AfterSha256) || !reflect.DeepEqual(retained, expected) {
				return nil, errors.New("legacy closure retained receipt lost immutable provenance")
			}
			return prior, nil
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return nil, readErr
		}
		if err := exclusiveFile(closedPath, closedBytes, 0400); err != nil {
			return nil, err
		}
		if err := syncDirectory(self.stateDirectory(authorization.RunId)); err != nil {
			return nil, err
		}
		return closedBytes, nil
	}()
	if err != nil {
		return err
	}
	if err := self.materializeLegacyPrelaunchDiagnostic(authorization.RunId, closedBytes); err != nil {
		return err
	}
	files, err := self.collectFiles(self.workDirectory(authorization.RunId))
	if err != nil {
		return err
	}
	publication, err = self.lock(ctx)
	if err != nil {
		return err
	}
	defer publication.Close()
	current, err = self.loadState(authorization.RunId)
	slot, slotErr = self.loadSlot()
	if err != nil || slotErr != nil || slot.RunId != authorization.RunId || !legacyPrelaunchSameState(current, original) || !current.CleanupVerified || !current.StopCleanupVerified {
		return errors.New("legacy closure custody changed during diagnostic materialization")
	}
	current.State, current.ErrorCode, current.ExitCode = "failed", "legacy_prelaunch_infrastructure_failure", -1
	current.Files, current.RecoveryActive = files, false
	if err := ctx.Err(); err != nil {
		return err
	}
	return self.storeState(authorization.RunId, current)
}

// This new operator diagnostic is never blank fake evaluator output. Existing
// bytes are never overwritten; crash replay accepts only the identical receipt.
func (self *Agent) materializeLegacyPrelaunchDiagnostic(runId string, receipt []byte) error {
	diagnostic := legacyPrelaunchDiagnostic{Schema: Schema, Kind: "competition-operator-recovery-diagnostic/v1", Provenance: "operator_legacy_prelaunch_recovery",
		EvaluatorStarted: false, OriginalEvaluatorStderrExists: false, Closure: append(json.RawMessage(nil), receipt...)}
	content, err := json.Marshal(diagnostic)
	if err != nil {
		return err
	}
	content = append(content, '\n')
	path := filepath.Join(self.workDirectory(runId), "worker.stderr.log")
	if err := exclusiveFile(path, content, 0400); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		prior, err := readBounded(path, self.config.Limits.DocumentBytes)
		if err != nil || legacyPrelaunchSealedFile(path, self.protected) != nil || !bytes.Equal(prior, content) {
			return errors.New("legacy recovery diagnostic differs from its exact retained receipt")
		}
	}
	return syncDirectory(self.workDirectory(runId))
}
