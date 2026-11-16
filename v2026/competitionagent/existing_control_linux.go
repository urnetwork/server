//go:build linux

// A separately pinned observer owns only existing-run control. Its bounded
// child shares the existing recovery allowance, never the exhausted private
// aggregate, and cannot launch an evaluator, owner, build or preparation.
package competitionagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// OriginalConfig names the immutable role which actually launched the run.
// The corrected observer's executable must never be substituted for that pin.
type ExistingControlRole struct {
	ApprovalSha256 string `json:"approval_sha256"`
	OriginalConfig Pin    `json:"original_config"`
}

// Purpose is internal source vocabulary, never a Boolean/configuration waiver.
type managementObservationPurpose int

const (
	managementFreshAdmission managementObservationPurpose = iota + 1
	managementExistingContainment
)

// Reuse the existing control child's ceilings inside the independent aggregate.
// Its separate singleton registry/lock and additional headroom charge preserve
// the original recovery margin. Native pressure qualification remains required.
func defaultExistingControlSettings() ManagementSshSettings {
	settings := DefaultManagementSshSettings()
	settings.existingOnly, settings.MaximumControls = true, 1
	settings.MaximumRuntime = settings.Agent.ControlTimeout + settings.Agent.OutputTransferTimeout + settings.Agent.ControlTimeout + settings.Agent.CancelGrace
	return settings
}

// Fixed placement/name helpers keep all manager, registry and proc checks on
// the same role, including stop/unknown-session retirement.
func (self ManagementSshSettings) unitPrefix() string {
	if self.existingOnly {
		return "urnetwork-subeval-existing-control-"
	}
	return "urnetwork-subeval-control-"
}

// A service name remains derived only from its locally allocated identity.
func (self ManagementSshSettings) unitName(id string) string {
	return self.unitPrefix() + id + ".service"
}

// Separate journals prevent ordinary RPC receipts becoming observer authority.
func (self ManagementSshSettings) registryName() string {
	if self.existingOnly {
		return "managed-existing-controls"
	}
	return "managed-controls"
}

// Existing control alone uses the independently bounded recovery aggregate.
func (self ManagementSshSettings) sliceName() string {
	if self.existingOnly {
		return managementCleanupSliceName
	}
	return managementPrivateSliceName
}

// The native path mirrors the exact selected source-owned slice identity.
func (self ManagementSshSettings) cgroupPath() string {
	if self.existingOnly {
		return managementCleanupCgroupPath
	}
	return managementPrivateCgroupPath
}

// The child vocabulary is closed before any signed frame is read.
func (self ManagementSshSettings) serveFlag() string {
	if self.existingOnly {
		return "--serve-existing"
	}
	return "--serve"
}

// Only locally generated bounded control identities belong to this class.
func validExistingControlUnit(unit string) bool {
	settings := defaultExistingControlSettings()
	id := strings.TrimSuffix(strings.TrimPrefix(unit, settings.unitPrefix()), ".service")
	return digestPattern.MatchString(id) && unit == settings.unitName(id)
}

// The new fixed forced role is deliberately absent from ordinary transports.
func ServeExistingManagedSsh(ctx context.Context, configPath string, input io.Reader, output, diagnostic io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("existing control requires its separately pinned root wrapper")
	}
	return newManagementSshHost(defaultExistingControlSettings()).serve(ctx, configPath, input, output, diagnostic)
}

// The observer configuration is distinct, but its journal and frozen execution
// role remain the original protected ones. Chains of amended original roles
// cannot relabel an execution which used another binary/configuration.
func loadExistingManagementConfig(path string, settings ManagementSshSettings) (*Config, error) {
	config, err := loadManagementSshConfig(path, settings)
	if err != nil {
		return nil, err
	}
	original, err := loadExistingOriginalConfig(config, settings.Agent, true)
	if err != nil {
		return nil, err
	}
	if err := validateExistingRecoveryCompatibility(config, original); err != nil {
		return nil, err
	}
	if original.StateDirectory != config.StateDirectory || original.BaseDirectory != config.BaseDirectory || original.WorkDirectory != config.WorkDirectory ||
		original.ManagementPolicy != config.ManagementPolicy || original.DockerRole.ResourceBoundary != config.DockerRole.ResourceBoundary || original.HostId != config.HostId {
		return nil, errors.New("observer changed the original host journal, management or hard-containment identity")
	}
	return config, nil
}

// The descriptor-verified observer program contains the updated closure code.
// Exact matching executable bytes prove its pinned companion recovery does too;
// legacy/mixed programs need a separately approved design, never a relabeled pin.
func validateExistingRecoveryCompatibility(control, original *Config) error {
	if control == nil || original == nil || !digestPattern.MatchString(control.Agent.Sha256) || control.Agent.Sha256 != original.Agent.Sha256 {
		return errors.New("existing observer requires an actually matching companion recovery program; legacy mixed roles are unsupported")
	}
	if original.ExistingRecoveryProfile != existingRecoveryCapacityProfile || control.ExistingRecoveryProfile != original.ExistingRecoveryProfile {
		return errors.New("existing observer requires the explicit full-growth companion capacity profile")
	}
	return nil
}

// No worker-account probe, fresh engine admission or signing-key read is needed
// before PID1 placement. Both configurations still undergo protected pin checks.
func loadExistingOriginalConfig(control *Config, settings AgentSettings, protected bool) (*Config, error) {
	if control == nil || control.ExistingControl == nil || !digestPattern.MatchString(control.ExistingControl.ApprovalSha256) ||
		control.ExistingControl.OriginalConfig.Path == control.configPath || validateFilePin(control.ExistingControl.OriginalConfig, settings.ConfigBytes, protected) != nil {
		return nil, errors.New("existing control omitted independently approved original-role custody")
	}
	content, err := readBounded(control.ExistingControl.OriginalConfig.Path, settings.ConfigBytes)
	if err != nil || digestDockerBytes(content) != control.ExistingControl.OriginalConfig.Sha256 {
		return nil, errors.New("original role bytes changed")
	}
	defer clear(content)
	original := &Config{Limits: DefaultLimits(), configPath: control.ExistingControl.OriginalConfig.Path, configSha256: digestDockerBytes(content)}
	if validateDocumentKeys(content) != nil || decodeStrict(content, original) != nil || original.ExistingControl != nil {
		return nil, errors.New("invalid immutable original role")
	}
	if original.StateDirectory == "" {
		original.StateDirectory = filepath.Join(original.BaseDirectory, "state")
	}
	if original.WorkDirectory == "" {
		original.WorkDirectory = filepath.Join(original.BaseDirectory, "jobs")
	}
	if err := original.validateRecoveryConfiguration(settings, protected); err != nil {
		return nil, err
	}
	return original, nil
}

// Source-class observation needs its own allowance PLUS the unchanged cleanup
// margin in the same existing two-GiB cap. Private headroom is not a dependency.
func admitExistingManagement(ctx context.Context, config *Config, settings ManagementSshSettings) (*ManagementResourcePolicy, Pin, error) {
	if config == nil || !settings.existingOnly || settings.validate() != nil || config.ExistingControl == nil {
		return nil, Pin{}, errors.New("existing observer role was not explicitly configured")
	}
	original, err := loadExistingOriginalConfig(config, settings.Agent, true)
	if err != nil {
		return nil, Pin{}, err
	}
	if err := validateExistingRecoveryCompatibility(config, original); err != nil {
		return nil, Pin{}, err
	}
	memory, tasks, err := recoveryServiceLimits(*original, "--recover-active")
	if err != nil {
		return nil, Pin{}, err
	}
	policy, err := loadManagementResourcePolicy(config.ManagementPolicy, DefaultManagementBoundarySettings(), settings.Agent.ConfigBytes, true)
	if err != nil {
		return nil, Pin{}, err
	}
	manager, err := recoverySystemctlPin(*config, settings.Agent, true)
	if err != nil {
		return nil, Pin{}, err
	}
	if err := verifyManagementOrdinaryForPurpose(ctx, policy, manager, settings.Agent, managementExistingContainment); err != nil {
		return nil, Pin{}, err
	}
	admission := settings.Agent
	if admission.RecoveryMemoryHeadroomBytes > memory || admission.RecoveryTasksHeadroom > tasks {
		return nil, Pin{}, errors.New("observer cannot consume the original cleanup margin")
	}
	admission.RecoveryMemoryHeadroomBytes += settings.MemoryBytes
	admission.RecoveryTasksHeadroom += settings.TasksMaximum
	if err := verifyManagementRecoveryBoundary(ctx, policy, manager, admission, true, false); err != nil {
		return nil, Pin{}, err
	}
	role, err := managementPolicyRole(policy, ManagementCleanupRole)
	if err != nil {
		return nil, Pin{}, err
	}
	facts, err := readManagementRecoveryFacts(ctx, role, settings.Agent, "/sys/fs/cgroup", "/proc", true, false)
	if err != nil || len(facts.ChildrenBefore) != 0 || len(facts.ChildrenAfter) != 0 {
		return nil, Pin{}, errors.New("another recovery/control owner retains independent custody")
	}
	if err := verifyRecoveryService(ctx, *original, settings.Agent, true, false, verifyManagementRecoveryBoundary, runRecoveryManagerCommand); err != nil {
		return nil, Pin{}, err
	}
	return policy, manager, ctx.Err()
}

// Placement is checked inside the already-created child without reacquiring
// its singleton wrapper lock or requiring empty ordinary/private headroom.
func VerifyExistingControlPlacement(ctx context.Context, configPath string) error {
	if os.Geteuid() != 0 {
		return errors.New("existing control requires the fixed root PID1 child")
	}
	settings := defaultExistingControlSettings()
	config, err := loadExistingManagementConfig(configPath, settings)
	if err != nil {
		return err
	}
	policy, err := loadManagementResourcePolicy(config.ManagementPolicy, DefaultManagementBoundarySettings(), settings.Agent.ConfigBytes, true)
	if err != nil {
		return err
	}
	manager, err := recoverySystemctlPin(*config, settings.Agent, true)
	if err != nil {
		return err
	}
	original, err := loadExistingOriginalConfig(config, settings.Agent, true)
	if err != nil {
		return err
	}
	// Recheck the future companion's protected text/PID1 caps as well as the
	// observer's live closure; an empty RSS snapshot cannot supply that proof.
	boundary := func(ctx context.Context, policy *ManagementResourcePolicy, manager Pin, settings AgentSettings, protected, current bool) error {
		return verifyManagementRecoveryForPurpose(ctx, policy, manager, settings, protected, current, managementExistingContainment)
	}
	if err := verifyRecoveryService(ctx, *original, settings.Agent, true, false, boundary, runRecoveryManagerCommand); err != nil {
		return err
	}
	if err := verifyManagementOrdinaryForPurpose(ctx, policy, manager, settings.Agent, managementExistingContainment); err != nil {
		return err
	}
	return verifyManagementSshCurrent(ctx, config, policy, manager, settings)
}

// The existing child proves placement before key loading and again before
// framing. Only the exact same already-published generation/projection may
// acknowledge that second proof; ordinary-role publication remains exclusive.
func (self *managementSshRegistry) recordCurrentAdmission(id string, content []byte) error {
	if self.settings.existingOnly {
		previous, err := self.read(id + ".admitted")
		if err == nil {
			if bytes.Equal(previous, content) {
				return nil
			}
			return errors.New("existing control admission generation changed")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return self.write(id+".admitted", content)
}

// This constructor cannot initialize directories, prepare inputs, validate a
// fresh worker account, or start a runtime. It loads only existing root custody.
func NewExistingControlAgent(ctx context.Context, configPath string, runner Runner, settings AgentSettings) (*Agent, error) {
	if err := VerifyExistingControlPlacement(ctx, configPath); err != nil {
		return nil, err
	}
	control, err := loadExistingManagementConfig(configPath, defaultExistingControlSettings())
	if err != nil {
		return nil, err
	}
	original, err := loadExistingOriginalConfig(control, settings, true)
	if err != nil {
		return nil, err
	}
	return newExistingControlAgent(control, original, runner, settings, true)
}

// Tests replace native placement/proof only; durable state, framing, signatures,
// exact original pins and all no-start state transitions remain composed.
func newExistingControlAgent(control, original *Config, runner Runner, settings AgentSettings, protected bool) (*Agent, error) {
	if control == nil || original == nil || control.ExistingControl == nil || runner == nil || settings.Now == nil || settings.ControlTimeout <= 0 || settings.OutputTransferTimeout <= 0 ||
		settings.CleanupTimeout <= 0 || settings.CleanupCommandTimeout <= 0 || settings.CleanupCommandTimeout > settings.CleanupTimeout || settings.CancelGrace <= 0 || settings.LockPollInterval <= 0 ||
		settings.MaximumClockSkew <= 0 || settings.DiagnosticBytes <= 0 || settings.ConfigBytes <= 0 || settings.DirectoryBatch <= 0 || settings.CleanupObjectCount <= 0 || settings.OwnerPollInterval <= 0 ||
		control.ExistingControl.OriginalConfig.Sha256 != original.configSha256 || original.ExistingControl != nil {
		return nil, errors.New("existing control lacks exact original-role and finite owner authority")
	}
	if err := validateExistingRecoveryCompatibility(control, original); err != nil {
		return nil, err
	}
	if protected && protectedRootPath(original.SigningKeyPath, false, true) != nil {
		return nil, errors.New("existing role signing key is not protected")
	}
	content, err := readBounded(original.SigningKeyPath, settings.ConfigBytes)
	if err != nil {
		return nil, err
	}
	defer clear(content)
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(content)))
	if err != nil {
		return nil, err
	}
	defer clear(key)
	if len(key) == ed25519.SeedSize {
		key = ed25519.NewKeyFromSeed(key)
		defer clear(key)
	}
	public, err := base64.StdEncoding.DecodeString(original.CoordinatorPublicKeyBase64)
	if err != nil || len(key) != ed25519.PrivateKeySize || len(public) != ed25519.PublicKeySize {
		return nil, errors.New("existing role signing authority is invalid")
	}
	self := &Agent{config: *original, settings: settings, runner: runner, protected: protected, existingOnly: true, existingControl: control,
		signingKey: append(ed25519.PrivateKey(nil), key...), coordinatorPublicKey: public}
	self.cleaner, self.containment = newHostCleaner(*original, settings), newHostRunContainment(*original, settings, nil)
	self.existingPlacement = func(ctx context.Context) error { return VerifyExistingControlPlacement(ctx, control.configPath) }
	self.existingProof = self.proveExistingRun
	return self, nil
}

// Every operation is bound to a retained immutable run. Unknown or unstarted
// state never falls back to upload/start, and artifacts need positive cleanup.
func (self *Agent) ServeExisting(ctx context.Context, reader io.Reader, writer io.Writer) error {
	if !self.existingOnly || self.existingPlacement == nil || self.existingProof == nil {
		return errors.New("agent has no existing-only native authority")
	}
	if err := self.existingPlacement(ctx); err != nil {
		return err
	}
	input, err := newControlReadStream(reader)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := newControlWriteStream(writer)
	if err != nil {
		return err
	}
	defer output.Close()
	request := Request{}
	if err := input.WithDeadline(ctx, self.settings.ControlTimeout, func(reader io.Reader) error { return ReadDocument(reader, &request, self.config.Limits.DocumentBytes) }); err != nil {
		return err
	}
	if err := VerifyRequest(request, self.coordinatorPublicKey); err != nil {
		return err
	}
	if request.Schema != Schema || request.HostId != self.config.HostId || request.Start != nil || !digestPattern.MatchString(request.RunId) || request.FencingToken <= 0 ||
		request.Operation != "status" && request.Operation != "artifacts" && request.Operation != "cancel" {
		return errors.New("existing role cannot prepare, upload or admit work")
	}
	response, err := self.existingStatus(ctx, request)
	if err != nil {
		return err
	}
	if request.Operation == "cancel" {
		if response.Identity == nil || self.settings.Now().UTC().Before(response.Identity.DeadlineAt) {
			return errors.New("existing control cannot cancel before the original absolute deadline")
		}
		if _, err := self.cancel(ctx, request); err != nil {
			return err
		}
		response, err = self.existingStatus(ctx, request)
		if err != nil {
			return err
		}
	}
	if request.Operation == "artifacts" && (response.ExistingRun == nil || !terminalState(response.State)) {
		return errors.New("existing artifacts require authenticated terminal stop cleanup")
	}
	if err := SignResponse(&response, self.signingKey); err != nil {
		return err
	}
	if err := output.WithDeadline(ctx, self.settings.ControlTimeout, func(writer io.Writer) error { return WriteDocument(writer, response, self.config.Limits.DocumentBytes) }); err != nil {
		return err
	}
	if request.Operation == "artifacts" {
		return output.WithDeadline(ctx, self.settings.OutputTransferTimeout, func(writer io.Writer) error {
			return WriteFiles(writer, self.workDirectory(request.RunId), response.Files, self.config.Limits.ArtifactBytes, self.config.Limits)
		})
	}
	return nil
}

// Native I/O holds no publication lock. Exact durable state/slot/fence is
// reread afterward before any coordinator fence advance is acknowledged.
func (self *Agent) existingStatus(ctx context.Context, request Request) (Response, error) {
	unknown := func(code string) Response {
		return Response{Schema: Schema, HostId: self.config.HostId, RunId: request.RunId, FencingToken: request.FencingToken, State: "unknown", ExitCode: -1, ErrorCode: code}
	}
	state, manifest, plan, err := self.authenticatedOwnedRun(request.RunId)
	if err != nil {
		return unknown("existing_custody_unavailable"), nil
	}
	if request.FencingToken < state.FencingToken {
		return Response{}, errors.New("stale existing execution fence")
	}
	if self.validateManifest(*manifest) != nil || state.RecoveryActive || !state.LaunchAttempted || !state.ExecutionStarted && (!availableRunState(state) || state.State == "succeeded") {
		return unknown("existing_execution_not_proven"), nil
	}
	proof, err := self.existingProof(ctx, manifest, plan, state)
	if err != nil || !digestPattern.MatchString(proof) {
		return unknown("existing_native_custody_unknown"), nil
	}
	lock, err := self.lock(ctx)
	if err != nil {
		return Response{}, err
	}
	defer lock.Close()
	current, err := self.loadState(request.RunId)
	slot, slotErr := self.loadSlot()
	before, _ := jsonState(state)
	after, _ := jsonState(current)
	if err != nil || slotErr != nil || slot.RunId != request.RunId || !bytes.Equal(before, after) {
		return unknown("existing_custody_changed"), nil
	}
	if current.FencingToken != request.FencingToken {
		current.FencingToken = request.FencingToken
		if err := self.storeState(request.RunId, current); err != nil {
			return Response{}, err
		}
	}
	response := self.response(manifest, current)
	if !terminalState(current.State) {
		response.State = "running"
	}
	response.ExistingRun = &ExistingRunEvidence{ApprovalSha256: self.existingControl.ExistingControl.ApprovalSha256,
		OriginalConfigSha256: self.config.configSha256, ResourceBoundarySha256: self.config.DockerRole.ResourceBoundary.Sha256,
		CleanupPlanSha256: current.CleanupPlanSha256, NativeProofSha256: proof, ExecutionStarted: current.ExecutionStarted,
		CleanupVerified: current.CleanupVerified, StopCleanupVerified: current.StopCleanupVerified}
	return response, nil
}

// One canonical comparison catches any durable mutation during native reads.
func jsonState(state *runState) ([]byte, error) {
	if state == nil {
		return nil, errors.New("missing existing state")
	}
	return json.Marshal(state)
}

// The same closed fields authenticate each observer or recovery child through
// a held native descriptor; no descendant can acquire another allowance.
func readExistingControlCgroup(ctx context.Context, group *os.File, settings AgentSettings, requireKernel bool) (map[string]string, error) {
	values := map[string]string{}
	for _, name := range []string{"memory.max", "memory.high", "memory.swap.max", "memory.current", "pids.max", "pids.current", "cpu.max", "cpuset.cpus", "cpuset.cpus.effective", "cgroup.type", "memory.oom.group"} {
		value, err := readManagementKernelProperty(ctx, group, name, settings.DiagnosticBytes, requireKernel)
		if err != nil {
			return nil, err
		}
		values[name] = value
	}
	return values, nil
}

// Recovery can run concurrently with the one independently capped observer;
// the observer cannot make another cleanup/private/fresh-work child legitimate.
func validateExistingRecoveryClosure(facts managementRecoveryFacts, policy ManagementResourcePolicy, settings AgentSettings, requireCurrent bool) error {
	if len(facts.ExistingControlsBefore) != len(facts.ExistingControlsAfter) || len(facts.ExistingControlsBefore) > 1 {
		return errors.New("existing observer closure changed or exceeded its one allowance")
	}
	profile := defaultExistingControlSettings()
	memory, tasks, err := recoveryServiceLimits(Config{ExistingRecoveryProfile: settings.existingRecoveryProfile}, "--recover-active")
	if err != nil {
		return err
	}
	if len(facts.ExistingControlsBefore) != 0 && settings.existingRecoveryProfile != existingRecoveryCapacityProfile {
		return errors.New("legacy full-cap recovery cannot coexist with an observer")
	}
	recoveryFound, observers := false, 0
	for _, name := range facts.ChildrenBefore {
		if name == managementRecoveryServiceName {
			if recoveryFound {
				return errors.New("repeated recovery service")
			}
			recoveryFound = true
			if settings.existingRecoveryProfile != "" {
				for _, values := range []map[string]string{facts.RecoveryServiceBefore, facts.RecoveryServiceAfter} {
					if err := validateRecoveryChildLimits(values, policy, settings, memory, tasks); err != nil {
						return err
					}
					// Native helpers share this child, not just its outer slice.
					for key, maximum := range map[string]int64{"memory.current": memory - settings.RecoveryMemoryHeadroomBytes, "pids.current": tasks - settings.RecoveryTasksHeadroom} {
						value, err := strconv.ParseInt(values[key], 10, 64)
						if err != nil || maximum < 0 || value > maximum {
							return errors.New("companion recovery child has no runnable helper margin")
						}
					}
				}
			}
			continue
		}
		values, present := facts.ExistingControlsBefore[name]
		if !validExistingControlUnit(name) || !present {
			return errors.New("recovery closure contains an unobserved owner")
		}
		observers++
		for _, values := range []map[string]string{values, facts.ExistingControlsAfter[name]} {
			if err := validateRecoveryChildLimits(values, policy, profile.Agent, profile.MemoryBytes, profile.TasksMaximum); err != nil {
				return err
			}
		}
	}
	if observers != len(facts.ExistingControlsBefore) || requireCurrent != recoveryFound {
		return errors.New("recovery closure omitted its current service or retained another active owner")
	}
	return nil
}
