//go:build linux

// Recovery tests use signed private custody files and fake manager/cgroup/
// daemon observations only. No native wrapper is called, even in unprotected mode.
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Only caller-owned fake status values replace the fixed native unit manager.
type recoveryTestRunner struct {
	Runner
	stopErr, statusErr error
	status             *UnitStatus
	beforeStop         func()
}

func (self *recoveryTestRunner) Stop(ctx context.Context, unit string) error {
	if self.beforeStop != nil {
		self.beforeStop()
	}
	err := self.Runner.Stop(ctx, unit)
	if self.stopErr != nil {
		return self.stopErr
	}
	return err
}
func (self *recoveryTestRunner) Status(ctx context.Context, unit string) (UnitStatus, error) {
	if self.statusErr != nil {
		return UnitStatus{}, self.statusErr
	}
	if self.status != nil {
		return *self.status, nil
	}
	return self.Runner.Status(ctx, unit)
}

// The narrow constructor gets no worker account or signing key. Its placement
// seam is explicitly private; production always calls the real native wrapper.
func recoveryTestAgent(t *testing.T, fixture *agentTestFixture) *Agent {
	t.Helper()
	config := *fixture.config
	config.SigningKeyPath = "/synthetic/unavailable-signing-key"
	config.WorkerUser = ""
	agent, err := newRecoveryAgent(&config, fixture.runner, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	agent.cleaner, agent.containment = fixture.cleaner, fixture.containment
	agent.recoveryPlacement = func(context.Context, bool) error { return nil }
	agent.runtimeAdmission = func(context.Context) (*RuntimeResourceBoundary, error) {
		t.Error("recovery queried healthy private engine admission")
		return nil, errors.New("dead private engine")
	}
	return agent
}

func recoveryClockBytes(t *testing.T, fixture *agentTestFixture) []byte {
	t.Helper()
	identity := fixture.manifest.Identity
	key := sha256.Sum256([]byte(fmt.Sprintf("competition-job-clock/v1\x00%s\x00%d", identity.JobId, identity.Cycle)))
	content, err := os.ReadFile(filepath.Join(fixture.config.StateDirectory, "clocks", hex.EncodeToString(key[:])+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// Only metadata owned by this private fixture is made writable. WriteFile's
// mode does not change an existing 0400 inode; restore that intended mode too.
func writeRecoveryTestAuthority(t *testing.T, fixture *agentTestFixture, path string, content []byte) {
	t.Helper()
	if !pathWithin(fixture.config.BaseDirectory, path) || path == fixture.config.BaseDirectory {
		t.Fatal("test metadata write escaped its owned private fixture")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatal("test metadata write requires an owned regular file")
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryConstructionDoesNotNeedWorkerSigningKeyOrHealthyEngine(t *testing.T) {
	fixture := newAgentTestFixture(t)
	config := *fixture.config
	config.WorkerUser = ""
	config.SigningKeyPath = "/synthetic/absent-key"
	config.Evaluator.Path = "/synthetic/absent-evaluator"
	config.SelfCheck.Path = "/synthetic/absent-check"
	agent, err := newRecoveryAgent(&config, fixture.runner, fixture.settings, false)
	if err != nil || agent.credential != nil || len(agent.signingKey) != 0 || !agent.recoveryOnly {
		t.Fatal("independent recovery loaded worker/key/execution authority", err)
	}
	if agent.Execute(t.Context(), strings.Repeat("a", 64)) == nil || agent.RunDockerOwner(t.Context(), strings.Repeat("a", 64)) == nil || agent.Serve(t.Context(), bytes.NewReader(nil), &bytes.Buffer{}) == nil {
		t.Fatal("recovery-only constructor granted execution or SSH authority")
	}
}

func TestRecoveryExplicitEmptySlotIsNoopButMissingOrCorruptIsUnknown(t *testing.T) {
	for _, mode := range []string{"empty", "missing", "corrupt"} {
		fixture := newAgentTestFixture(t)
		agent := recoveryTestAgent(t, fixture)
		if mode == "empty" {
			if err := agent.storeSlot(""); err != nil {
				t.Fatal(err)
			}
		} else if mode == "corrupt" {
			if err := os.WriteFile(filepath.Join(fixture.config.StateDirectory, "slot.json"), []byte(`{"run_id":"foreign"}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
		err := agent.RecoverActive(t.Context())
		if (err == nil) != (mode == "empty") || fixture.cleaner.count() != 0 || fixture.runner.launchCount() != 0 {
			t.Fatal("missing/corrupt physical custody became empty or launched work", mode, err)
		}
	}
}

func TestRecoveryFreezesBeforeStopAndAcceptsOnlyPositiveInactiveAfterStopError(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	clock := recoveryClockBytes(t, fixture)
	agent := recoveryTestAgent(t, fixture)
	frozen := false
	fixture.containment.before = func(_ context.Context, op string, _ cleanupPlan, _ bool) error {
		if op == "freeze" {
			frozen = true
		}
		return nil
	}
	agent.runner = &recoveryTestRunner{Runner: fixture.runner, stopErr: errors.New("synthetic ExecStopPost failed"), beforeStop: func() {
		if !frozen {
			t.Error("blocking unit stop preceded exact emergency freeze")
		}
	}}
	if err := agent.RecoverActive(t.Context()); err != nil {
		t.Fatal("positive inactive retained unit could not run independent fallback", err)
	}
	state, err := agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || state.RecoveryActive || !state.CleanupVerified || !state.StopCleanupVerified || fixture.cleaner.count() != 1 || fixture.runner.launchCount() != 1 || !bytes.Equal(clock, recoveryClockBytes(t, fixture)) {
		t.Fatal("recovery lost cleanup/clock/single-attempt custody", err)
	}
}

func TestRecoveryMissingActiveOrUncertainUnitStatusCannotPublishCleanup(t *testing.T) {
	for _, fault := range []string{"missing", "active", "unknown"} {
		fixture := newAgentTestFixture(t)
		if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
			t.Fatal(err)
		}
		agent := recoveryTestAgent(t, fixture)
		runner := &recoveryTestRunner{Runner: fixture.runner}
		switch fault {
		case "missing":
			runner.status = &UnitStatus{}
		case "active":
			runner.status = &UnitStatus{Exists: true, Active: true}
		case "unknown":
			runner.statusErr = errors.New("synthetic uncertain manager")
		}
		agent.runner = runner
		if agent.RecoverActive(t.Context()) == nil {
			t.Fatal("unjoined evaluator status permitted cleanup publication", fault)
		}
		state, err := agent.loadState(fixture.manifest.Identity.RunId)
		if err != nil || !state.RecoveryActive || state.CleanupVerified || state.StopCleanupVerified || fixture.cleaner.count() != 0 || fixture.runner.launchCount() != 1 {
			t.Fatal("unknown recovery reset/reaped/published custody", fault, err)
		}
	}
}

// A new recovery process resumes only the retained cleanup attempt. Failure
// never clears the durable marker or turns the original clock into a new one.
func TestRecoveryFailureRetainsCustodyUntilIndependentRetryJoins(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	clock := recoveryClockBytes(t, fixture)
	fixture.cleaner.err = errors.New("synthetic private daemon unavailable")
	if recoveryTestAgent(t, fixture).RecoverActive(t.Context()) == nil {
		t.Fatal("unavailable daemon became a positive recovery cleanup proof")
	}
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || !state.RecoveryActive || state.CleanupVerified || state.StopCleanupVerified {
		t.Fatal("failed fallback released unknown physical custody", err)
	}
	fixture.cleaner.err = nil
	if err := recoveryTestAgent(t, fixture).RecoverActive(t.Context()); err != nil {
		t.Fatal("retained fallback could not resume the same cleanup scope", err)
	}
	state, err = fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || state.RecoveryActive || !state.CleanupVerified || !state.StopCleanupVerified || fixture.cleaner.count() != 2 || fixture.runner.launchCount() != 1 || !bytes.Equal(clock, recoveryClockBytes(t, fixture)) {
		t.Fatal("cleanup retry replayed execution or changed original clock custody", err)
	}
}

func TestRecoveryRequiresUnchangedAuthenticatedClockAndPlan(t *testing.T) {
	for _, fault := range []string{"clock", "clock-duplicate", "clock-alias", "manifest", "plan", "slot-change", "placement"} {
		fixture := newAgentTestFixture(t)
		if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
			t.Fatal(err)
		}
		agent := recoveryTestAgent(t, fixture)
		switch fault {
		case "clock":
			identity := fixture.manifest.Identity
			key := sha256.Sum256([]byte(fmt.Sprintf("competition-job-clock/v1\x00%s\x00%d", identity.JobId, identity.Cycle)))
			if err := os.WriteFile(filepath.Join(fixture.config.StateDirectory, "clocks", hex.EncodeToString(key[:])+".json"), []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
		case "clock-duplicate", "clock-alias":
			identity := fixture.manifest.Identity
			key := sha256.Sum256([]byte(fmt.Sprintf("competition-job-clock/v1\x00%s\x00%d", identity.JobId, identity.Cycle)))
			name := "job_id"
			if fault == "clock-alias" {
				name = "JOB_ID"
			}
			content := strings.TrimSuffix(string(recoveryClockBytes(t, fixture)), "}") + fmt.Sprintf(",%q:%q}", name, identity.JobId)
			if err := os.WriteFile(filepath.Join(fixture.config.StateDirectory, "clocks", hex.EncodeToString(key[:])+".json"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		case "manifest", "plan":
			name := "manifest.json"
			if fault == "plan" {
				name = "cleanup-plan.json"
			}
			if err := os.WriteFile(filepath.Join(agent.stateDirectory(fixture.manifest.Identity.RunId), name), []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
		case "slot-change":
			calls := 0
			agent.recoveryPlacement = func(context.Context, bool) error {
				calls++
				if calls == 2 {
					return agent.storeSlot(strings.Repeat("f", 64))
				}
				return nil
			}
		case "placement":
			agent.recoveryPlacement = func(context.Context, bool) error { return errors.New("ordinary ancestry is not independent") }
		}
		before := fixture.containment.snapshot()
		if agent.RecoverActive(t.Context()) == nil || fixture.cleaner.count() != 0 || fixture.runner.launchCount() != 1 || !reflect.DeepEqual(before, fixture.containment.snapshot()) {
			t.Fatal("changed root authority reached native cleanup or replay", fault)
		}
	}
}

// Both callers prove independent placement before taking the same recovery
// lease. The real per-run lock serializes fallback, not the publication lock.
func TestRecoveryRepeatedAndConcurrentCallersNeverResetClockOrAttempt(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	clock := recoveryClockBytes(t, fixture)
	fixture.now = fixture.manifest.Identity.DeadlineAt.Add(time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	guard, finish := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer finish()
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	var releaseOnce sync.Once
	var callers sync.WaitGroup
	for index := 0; index < 2; index++ {
		agent := recoveryTestAgent(t, fixture)
		calls := 0
		agent.recoveryPlacement = func(ctx context.Context, current bool) error {
			calls++
			if !current {
				return errors.New("not current cleanup placement")
			}
			if calls == 1 {
				ready <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		callers.Add(1)
		go func() {
			defer callers.Done()
			done <- agent.RecoverActive(ctx)
		}()
	}
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		callers.Wait()
	})
	for index := 0; index < 2; index++ {
		select {
		case <-ready:
		case <-guard.Done():
			t.Fatal("concurrent recovery did not reach authenticated barrier")
		}
	}
	releaseOnce.Do(func() { close(release) })
	for index := 0; index < 2; index++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("concurrent recovery failed", err)
			}
		case <-guard.Done():
			t.Fatal("concurrent recovery did not join")
		}
	}
	agent := recoveryTestAgent(t, fixture)
	if err := agent.RecoverActive(t.Context()); err != nil {
		t.Fatal("repeated recovery was not idempotent", err)
	}
	state, err := agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil {
		t.Fatal(err)
	}
	manifest, manifestErr := agent.loadManifest(fixture.manifest.Identity.RunId, state.ManifestSha256)
	if manifestErr != nil || state.RecoveryActive || fixture.cleaner.count() != 1 || fixture.runner.launchCount() != 1 || !bytes.Equal(clock, recoveryClockBytes(t, fixture)) || manifest.Identity.Attempt != fixture.manifest.Identity.Attempt || !manifest.Identity.DeadlineAt.Equal(fixture.manifest.Identity.DeadlineAt) {
		t.Fatal("repeated/concurrent fallback changed original attempt or deadline", err, manifestErr)
	}
}

func TestRecoveryTimeoutArithmeticPreservesEveryBoundAndRejectsOverflow(t *testing.T) {
	settings := DefaultAgentSettings()
	settings.CleanupTimeout = 5 * time.Second
	settings.ControlTimeout = 2 * time.Second
	settings.CancelGrace = time.Second
	if got, err := recoveryServiceTimeout(settings); err != nil || got != 23*time.Second {
		t.Fatal("distinct recovery owners lost their finite budgets", got, err)
	}
	settings.CleanupTimeout = time.Duration(1<<63 - 1)
	if _, err := recoveryServiceTimeout(settings); err == nil {
		t.Fatal("recovery owner budget overflow became infinite")
	}
	settings = DefaultAgentSettings()
	settings.ControlTimeout = 0
	if _, err := recoveryServiceTimeout(settings); err == nil {
		t.Fatal("zero control budget disabled recovery ownership")
	}
}

// A manager/daemon callback can take publication ownership while recovery is
// paused. Even terminal cleanup flags cannot release the durable recovery fence.
func TestRecoveryCustodyBlocksPublicationWithoutHoldingAdmissionLock(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	agent := recoveryTestAgent(t, fixture)
	observed := false
	fixture.containment.before = func(ctx context.Context, op string, _ cleanupPlan, _ bool) error {
		if op != "freeze" {
			return nil
		}
		observed = true
		lock, err := agent.lock(ctx)
		if err != nil {
			return err
		}
		state, err := agent.loadState(fixture.manifest.Identity.RunId)
		if err != nil || !state.RecoveryActive {
			lock.Close()
			return errors.New("freeze preceded durable recovery custody")
		}
		if agent.requireFreeSlot(ctx, strings.Repeat("f", 64)) == nil {
			lock.Close()
			return errors.New("native callback released the physical recovery fence")
		}
		lock.Close()
		response, err := fixture.agent.availability(ctx)
		if err != nil || response.State != "occupied" {
			return errors.New("recovery became available before its native join")
		}
		terminal := *state
		terminal.State = "failed"
		terminal.CleanupVerified = true
		terminal.StopCleanupVerified = true
		if availableRunState(&terminal) || fixture.agent.response(&fixture.manifest, &terminal).State != "unknown" {
			return errors.New("terminal result bypassed independent recovery custody")
		}
		if err := fixture.agent.RunDockerOwner(ctx, fixture.manifest.Identity.RunId); err == nil {
			return errors.New("recovery permitted a late Docker owner")
		}
		if err := fixture.agent.Execute(ctx, fixture.manifest.Identity.RunId); err == nil {
			return errors.New("recovery permitted a late evaluator execution")
		}
		return nil
	}
	guard, cancel := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer cancel()
	if err := agent.RecoverActive(guard); err != nil || !observed {
		t.Fatal("recovery held publication lock across native work or lost its fence", err)
	}
}

// Source template rendering is test-owned and never installs/starts a unit.
func renderRecoveryTestService(t *testing.T, config Config, settings AgentSettings, policy *ManagementResourcePolicy) []byte {
	t.Helper()
	content, err := os.ReadFile("../cli/competitionsubeval/management/" + managementRecoveryServiceName + ".in")
	if err != nil {
		t.Fatal(err)
	}
	timeout, err := recoveryServiceTimeout(settings)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"AGENT_PATH": config.Agent.Path, "CONFIG_PATH": config.configPath, "CLEANUP_MEMORY_BYTES": strconv.FormatInt(DefaultManagementBoundarySettings().CleanupMemoryBytes, 10), "CLEANUP_TASKS_MAXIMUM": strconv.FormatInt(DefaultManagementBoundarySettings().CleanupTasksMaximum, 10), "MANAGEMENT_CPU_QUOTA_PERCENT": strconv.Itoa(settings.ManagementCpuQuotaPercent), "CPU_PERIOD_MICROS": strconv.FormatInt(settings.ContainmentCpuPeriodMicros, 10), "MANAGEMENT_CPUSET": policy.ManagementCpuSet, "RECOVERY_TIMEOUT_SECONDS": strconv.FormatFloat(timeout.Seconds(), 'f', 6, 64), "CANCEL_GRACE_SECONDS": strconv.FormatFloat(settings.CancelGrace.Seconds(), 'f', 6, 64)} {
		content = []byte(strings.ReplaceAll(string(content), "@"+key+"@", value))
	}
	return content
}

func recoveryTestProjection(config Config, settings AgentSettings, policy *ManagementResourcePolicy, current bool) []byte {
	values := map[string]string{}
	for _, key := range strings.Split(recoveryServiceProperties, ",") {
		values[key] = ""
	}
	timeout, _ := recoveryServiceTimeout(settings)
	for key, value := range map[string]string{"Id": managementRecoveryServiceName, "LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "FragmentPath": config.RecoveryService.Path, "Slice": managementCleanupSliceName, "User": "root", "Group": "root", "Type": "oneshot", "Restart": "no", "KillMode": "control-group", "StartLimitIntervalUSec": "0", "PrivateNetwork": "no", "PrivateUsers": "no", "PrivateMounts": "no", "MemoryMax": strconv.FormatInt(DefaultManagementBoundarySettings().CleanupMemoryBytes, 10), "MemorySwapMax": "0", "TasksMax": strconv.FormatInt(DefaultManagementBoundarySettings().CleanupTasksMaximum, 10), "CPUQuotaPerSecUSec": fmt.Sprintf("%dus", settings.ManagementCpuQuotaPercent*10000), "CPUQuotaPeriodUSec": fmt.Sprintf("%dus", settings.ContainmentCpuPeriodMicros), "AllowedCPUs": strings.ReplaceAll(policy.ManagementCpuSet, ",", " "), "CPUAffinity": strings.ReplaceAll(policy.ManagementCpuSet, ",", " "), "TimeoutStartUSec": fmt.Sprintf("%dus", timeout.Microseconds()), "TimeoutStopUSec": fmt.Sprintf("%dus", settings.CancelGrace.Microseconds()), "Requires": managementCleanupSliceName + " sysinit.target", "After": managementCleanupSliceName + " sysinit.target basic.target"} {
		values[key] = value
	}
	pid := 0
	values["MainPID"], values["ExecMainStartTimestampMonotonic"] = "0", "0"
	if current {
		pid = os.Getpid()
		values["MainPID"], values["ExecMainStartTimestampMonotonic"], values["InvocationID"], values["Job"] = strconv.Itoa(pid), "12345", strings.Repeat("a", 32), "7"
		values["ActiveState"], values["SubState"], values["ControlGroup"] = "activating", "start", managementCleanupCgroupPath+"/"+managementRecoveryServiceName
	}
	values["ExecStart"] = fmt.Sprintf("{ path=%s ; argv[]=%s --config %s --recover-active ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=%d ; code=(null) ; status=0/0 }", config.Agent.Path, config.Agent.Path, config.configPath, pid)
	var output strings.Builder
	for _, key := range strings.Split(recoveryServiceProperties, ",") {
		fmt.Fprintf(&output, "%s=%s\n", key, values[key])
	}
	return []byte(output.String())
}

func recoveryArmTestFixture(t *testing.T) (*agentTestFixture, *SystemdRunner, Launch, *ManagementResourcePolicy, *int) {
	t.Helper()
	fixture := newAgentTestFixture(t)
	config := fixture.config
	policy, err := loadManagementResourcePolicy(config.ManagementPolicy, DefaultManagementBoundarySettings(), fixture.settings.ConfigBytes, false)
	if err != nil {
		t.Fatal(err)
	}
	service := renderRecoveryTestService(t, *config, fixture.settings, policy)
	writeRecoveryTestAuthority(t, fixture, config.RecoveryService.Path, service)
	config.RecoveryService = testAgentPin(t, config.RecoveryService.Path)
	boundary := RuntimeResourceBoundary{Schema: Schema, Systemctl: Pin{Path: "/usr/bin/systemctl", Sha256: strings.Repeat("a", 64)}}
	content, err := json.Marshal(boundary)
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryTestAuthority(t, fixture, config.DockerRole.ResourceBoundary.Path, content)
	config.DockerRole.ResourceBoundary = testAgentPin(t, config.DockerRole.ResourceBoundary.Path)
	configBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryTestAuthority(t, fixture, config.configPath, configBytes)
	config.configSha256 = testAgentPin(t, config.configPath).Sha256
	launch := Launch{UnitName: dockerOwnerUnitName(fixture.manifest.Identity.RunId), RunId: fixture.manifest.Identity.RunId, Role: "docker-owner", ConfigPath: config.configPath, ConfigSha256: config.configSha256, BaseDirectory: config.BaseDirectory, Agent: config.Agent, DeadlineAt: fixture.manifest.Identity.DeadlineAt, ManagementCpuSet: policy.ManagementCpuSet}
	launched := new(int)
	runner := NewSystemdRunner(fixture.settings)
	runner.protected = false
	runner.recoveryConfig = func(path string) (*Config, error) {
		if path != config.configPath {
			return nil, errors.New("foreign root config")
		}
		return config, nil
	}
	proved := false
	runner.recoveryBoundary = func(_ context.Context, _ *ManagementResourcePolicy, _ Pin, _ AgentSettings, protected, current bool) error {
		if protected || current {
			return errors.New("test attempted native placement mode")
		}
		proved = true
		return nil
	}
	runner.recoveryCommand = func(_ context.Context, _ Pin, args []string, _ AgentSettings) ([]byte, error) {
		want := []string{"show", "--no-pager", "--all", "--property=" + recoveryServiceProperties, "--", managementRecoveryServiceName}
		if !reflect.DeepEqual(args, want) {
			return nil, errors.New("recovery manager command expanded")
		}
		return recoveryTestProjection(*config, fixture.settings, policy, false), nil
	}
	runner.launchCommand = func(_ context.Context, args []string) error {
		if !proved || !slices.Contains(args, "--property=OnFailure="+managementRecoveryServiceName) || !slices.Contains(args, "--property=Slice="+managementPrivateSliceName) {
			return errors.New("launch preceded fixed independent recovery arming")
		}
		*launched++
		return nil
	}
	return fixture, runner, launch, policy, launched
}

func TestSystemdStartArmsPinnedIndependentRecoveryBeforeOrdinaryLaunch(t *testing.T) {
	_, runner, launch, _, launched := recoveryArmTestFixture(t)
	if err := runner.Start(t.Context(), launch); err != nil || *launched != 1 {
		t.Fatal("native runner did not compose fixed recovery arming before launch", err)
	}
}

func TestSystemdStartRejectsUnarmedChangedConfigOrRecoveryGeneration(t *testing.T) {
	for _, fault := range []string{"config", "pin", "ancestor", "service", "cpuset"} {
		fixture, runner, launch, policy, launched := recoveryArmTestFixture(t)
		switch fault {
		case "config":
			fixture.config.configSha256 = strings.Repeat("d", 64)
		case "pin":
			writeRecoveryTestAuthority(t, fixture, fixture.config.RecoveryService.Path, []byte("changed unit"))
		case "ancestor":
			runner.recoveryBoundary = func(context.Context, *ManagementResourcePolicy, Pin, AgentSettings, bool, bool) error {
				return errors.New("shared binding ordinary ancestor")
			}
		case "service":
			runner.recoveryCommand = func(context.Context, Pin, []string, AgentSettings) ([]byte, error) {
				return []byte(strings.Replace(string(recoveryTestProjection(*fixture.config, fixture.settings, policy, false)), "Slice="+managementCleanupSliceName, "Slice="+managementPrivateSliceName, 1)), nil
			}
		case "cpuset":
			launch.ManagementCpuSet = "0,2"
		}
		if runner.Start(t.Context(), launch) == nil || *launched != 0 {
			t.Fatal("unarmed recovery dispatched ordinary work", fault)
		}
	}
}

func TestRecoveryServiceRejectsExtraCommandsDependenciesAndWrongCurrentRole(t *testing.T) {
	fixture, _, _, policy, _ := recoveryArmTestFixture(t)
	content := renderRecoveryTestService(t, *fixture.config, fixture.settings, policy)
	if err := validateRecoveryServiceText(content, *fixture.config, policy, fixture.settings); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{"ExecStartPost=/bin/sh\n", "EnvironmentFile=/synthetic/credentials\n", "BindsTo=urnetwork-private-engine.service\n"} {
		if validateRecoveryServiceText(append(append([]byte(nil), content...), []byte(extra)...), *fixture.config, policy, fixture.settings) == nil {
			t.Fatal("recovery template expanded privileged authority", extra)
		}
	}
	projection := recoveryTestProjection(*fixture.config, fixture.settings, policy, true)
	if err := validateRecoveryServiceProjection(projection, *fixture.config, policy, fixture.settings, true); err != nil {
		t.Fatal("current fixed recovery service did not validate", err)
	}
	for _, change := range []struct{ old, new string }{{old: "BindsTo=", new: "BindsTo=urnetwork-private-engine.service"}, {old: "DropInPaths=", new: "DropInPaths=/synthetic/override.conf"}, {old: "User=root", new: "User=synthetic-worker"}, {old: " --recover-active", new: " --cleanup=" + strings.Repeat("e", 64)}} {
		if validateRecoveryServiceProjection([]byte(strings.Replace(string(projection), change.old, change.new, 1)), *fixture.config, policy, fixture.settings, true) == nil {
			t.Fatal("recovery manager accepted widened role", change)
		}
	}
}

func TestRecoveryArmingRefusesPendingOrChangedManagerInvocation(t *testing.T) {
	for _, fault := range []string{"pending", "changed"} {
		fixture, runner, launch, policy, launched := recoveryArmTestFixture(t)
		calls := 0
		runner.recoveryCommand = func(context.Context, Pin, []string, AgentSettings) ([]byte, error) {
			calls++
			projection := string(recoveryTestProjection(*fixture.config, fixture.settings, policy, false))
			if fault == "pending" {
				projection = strings.Replace(projection, "Job=\n", "Job=8\n", 1)
			} else if calls == 2 {
				projection = strings.Replace(projection, "InvocationID=\n", "InvocationID="+strings.Repeat("c", 32)+"\n", 1)
			}
			return []byte(projection), nil
		}
		if runner.Start(t.Context(), launch) == nil || *launched != 0 {
			t.Fatal("pending/changed recovery manager generation armed ordinary work", fault)
		}
	}
}

// Every case is refused before opening the executable; no host manager or
// daemon is contacted even when these private tests run as root.
func TestRecoveryManagerRejectsGenericCommandsBeforeNativeAccess(t *testing.T) {
	pin := Pin{Path: "/usr/bin/systemctl", Sha256: strings.Repeat("a", 64)}
	for _, args := range [][]string{
		{"restart", managementRecoveryServiceName},
		{"show", "--no-pager", "--all", "--property=Environment", "--", managementRecoveryServiceName},
		{"show", "--no-pager", "--all", "--property=" + recoveryServiceProperties, "--", "synthetic-foreign.service"},
	} {
		if _, err := runRecoveryManagerCommand(t.Context(), pin, args, DefaultAgentSettings()); err == nil {
			t.Fatal("fixed recovery manager opened a generic privileged command surface", args)
		}
	}
}
