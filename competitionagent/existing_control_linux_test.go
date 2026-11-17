//go:build linux

// Existing control tests compose signed framing, durable original state and
// pressure/cap predicates. Only native placement/host observations are replaced.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A real initial signed start precedes the fault. The attach assertions begin
// only afterward and cannot hide another prepare/start/upload behind setup.
func newExistingControlTest(t *testing.T) (*agentTestFixture, *Agent, ManagementResourcePolicy, managementOrdinaryFacts, *atomic.Int32) {
	t.Helper()
	fixture := newAgentTestFixture(t)
	fixture.config.ExistingRecoveryProfile = existingRecoveryCapacityProfile
	fixture.agent.config.ExistingRecoveryProfile = existingRecoveryCapacityProfile
	fixture.config.configSha256 = strings.Repeat("a", 64)
	fixture.agent.config.configSha256 = fixture.config.configSha256
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil {
		t.Fatal(err)
	}
	state.ExecutionStarted, state.State = true, "running"
	if err := fixture.agent.storeState(fixture.manifest.Identity.RunId, state); err != nil {
		t.Fatal(err)
	}
	control := *fixture.config
	control.configPath, control.configSha256 = filepath.Join(control.BaseDirectory, "observer.json"), strings.Repeat("b", 64)
	control.Agent = Pin{Path: filepath.Join(control.BaseDirectory, "matching-observer-copy"), Sha256: fixture.config.Agent.Sha256}
	control.ExistingControl = &ExistingControlRole{ApprovalSha256: strings.Repeat("d", 64), OriginalConfig: Pin{Path: fixture.config.configPath, Sha256: fixture.config.configSha256}}
	observer, err := newExistingControlAgent(&control, fixture.config, fixture.runner, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	observer.cleaner, observer.containment = fixture.cleaner, fixture.containment
	observer.existingPlacement = func(context.Context) error { return nil }
	forbidden := &atomic.Int32{}
	observer.runtimeAdmission = func(context.Context) (*RuntimeResourceBoundary, error) {
		forbidden.Add(1)
		return nil, errors.New("synthetic fresh preparation/admission tripwire")
	}
	fixture.runner.beforeStart = func(Launch) { forbidden.Add(1) }
	policy, _, facts := managementOrdinaryTestFacts(t)
	private, _ := managementPolicyRole(&policy, ManagementPrivateRole)
	for _, snapshot := range []managementOrdinarySnapshot{facts.Before, facts.After} {
		snapshot.Groups[private.CgroupPath]["memory.current"] = strconv.FormatInt(private.MemoryBytes-16*1024*1024, 10)
	}
	observer.existingProof = func(ctx context.Context, manifest *StartManifest, plan *cleanupPlan, state *runState) (string, error) {
		if observer.config.Agent != fixture.config.Agent || observer.config.Agent == control.Agent || !reflect.DeepEqual(manifest.Identity, fixture.manifest.Identity) || plan.RunId != manifest.Identity.RunId {
			return "", errors.New("synthetic changed original binary/identity")
		}
		if err := validateManagementOrdinaryForPurpose(facts, policy, fixture.settings, managementExistingContainment); err != nil {
			return "", err
		}
		unit, err := fixture.runner.Status(ctx, unitName(manifest.Identity.RunId))
		if err != nil || terminalState(state.State) && (!availableRunState(state) || unit.Active) || !terminalState(state.State) && (!state.ExecutionStarted || !unit.Exists || !unit.Active) {
			return "", errors.New("synthetic native custody remains unknown")
		}
		content, _ := json.Marshal(struct {
			Original string
			Identity Identity
			Caps     managementOrdinaryFacts
		}{Original: fixture.config.configSha256, Identity: manifest.Identity, Caps: facts})
		return digestDockerBytes(content), nil
	}
	return fixture, observer, policy, facts, forbidden
}

// The returned body remains available for actual framed artifact verification.
func callExistingControlTest(t *testing.T, fixture *agentTestFixture, observer *Agent, request Request) (Response, *bytes.Buffer, error) {
	t.Helper()
	request.Schema, request.HostId = Schema, fixture.config.HostId
	if err := SignRequest(&request, fixture.coordinatorKey); err != nil {
		t.Fatal(err)
	}
	input, output := &bytes.Buffer{}, &bytes.Buffer{}
	if err := WriteDocument(input, request, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if err := observer.ServeExisting(t.Context(), input, output); err != nil {
		return Response{}, output, err
	}
	response := Response{}
	if err := ReadDocument(output, &response, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if err := VerifyResponse(response, fixture.agentPublicKey); err != nil {
		t.Fatal(err)
	}
	return response, output, nil
}

// Pressure refuses genuine fresh admission, while the actual signed existing
// endpoint advances custody only after positive exact original-run proof.
func TestExistingControlRunningAttachesWhileFreshHeadroomRefuses(t *testing.T) {
	fixture, observer, policy, facts, forbidden := newExistingControlTest(t)
	if validateManagementOrdinaryFacts(facts, policy, fixture.settings) == nil {
		t.Fatal("synthetic fresh pressure was not refused")
	}
	before := fixture.containment.snapshot()
	launches := fixture.runner.launchCount()
	request := Request{Operation: "status", RunId: fixture.manifest.Identity.RunId, FencingToken: 2}
	response, _, err := callExistingControlTest(t, fixture, observer, request)
	if err != nil || response.State != "running" || response.ExistingRun == nil || !response.ExistingRun.ExecutionStarted || response.FencingToken != 2 {
		t.Fatal("already-started native execution did not attach under pressure", err)
	}
	if !sameIdentityForExistingTest(response.Identity, &fixture.manifest.Identity) || response.ExistingRun.OriginalConfigSha256 != fixture.config.configSha256 || response.ExistingRun.OriginalConfigSha256 == observer.existingControl.configSha256 {
		t.Fatal("observer relabeled the original binary/configuration or clock")
	}
	if forbidden.Load() != 0 || fixture.runner.launchCount() != launches || !reflect.DeepEqual(before, fixture.containment.snapshot()) || fixture.cleaner.count() != 0 {
		t.Fatal("attach prepared, launched, uploaded or cleaned execution")
	}
}

// Native proof failure precedes durable fence adoption; changed hard caps can
// neither borrow old signatures nor turn unknown into terminal success.
func TestExistingControlChangedCapsRemainUnknownWithoutFenceAdvance(t *testing.T) {
	fixture, observer, _, facts, forbidden := newExistingControlTest(t)
	facts.After.Groups[managementPrivateCgroupPath]["memory.max"] = "max"
	response, _, err := callExistingControlTest(t, fixture, observer, Request{Operation: "status", RunId: fixture.manifest.Identity.RunId, FencingToken: 2})
	state, loadErr := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || loadErr != nil || response.State != "unknown" || response.ExistingRun != nil || state.FencingToken != 1 || forbidden.Load() != 0 {
		t.Fatal("changed native containment gained attachment authority", err, loadErr)
	}
}

// Accepted is not started, and an arbitrary run id has no original custody.
// Neither branch can consume inputs or call any launch/preparation method.
func TestExistingControlUnstartedAndMissingRunsNeverStart(t *testing.T) {
	fixture, observer, _, _, forbidden := newExistingControlTest(t)
	state, _ := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	state.ExecutionStarted, state.State = false, "accepted"
	if err := fixture.agent.storeState(fixture.manifest.Identity.RunId, state); err != nil {
		t.Fatal(err)
	}
	for _, runId := range []string{fixture.manifest.Identity.RunId, RunId("synthetic-absent-job", 0, 1)} {
		response, _, err := callExistingControlTest(t, fixture, observer, Request{Operation: "status", RunId: runId, FencingToken: 2})
		if err != nil || response.State != "unknown" || response.ExistingRun != nil {
			t.Fatal("unstarted/missing state acquired native proof", err)
		}
	}
	if forbidden.Load() != 0 {
		t.Fatal("unknown custody invoked fresh preparation or launch")
	}
}

// Closed operation parsing rejects the ordinary fallback vocabulary before
// state observation or any input body can be read or persisted.
func TestExistingControlRejectsStartCheckAvailabilityAndEarlyCancel(t *testing.T) {
	fixture, observer, _, _, forbidden := newExistingControlTest(t)
	launches := fixture.runner.launchCount()
	for _, operation := range []string{"start", "check", "availability", "cancel"} {
		request := Request{Operation: operation, RunId: fixture.manifest.Identity.RunId, FencingToken: 2}
		if operation == "start" {
			request.Start = &fixture.manifest
		}
		if _, _, err := callExistingControlTest(t, fixture, observer, request); err == nil {
			t.Fatal("existing endpoint admitted forbidden operation", operation)
		}
	}
	if forbidden.Load() != 0 || fixture.runner.launchCount() != launches {
		t.Fatal("forbidden existing operation crossed a fresh-work boundary")
	}
}

// Artifact transfer consumes the real signed terminal inventory only after
// both cleanup markers and the inactive original service are positively known.
func TestExistingControlTerminalArtifactsRequireOriginalStopCleanup(t *testing.T) {
	fixture, observer, _, _, forbidden := newExistingControlTest(t)
	runId := fixture.manifest.Identity.RunId
	state, _ := fixture.agent.loadState(runId)
	state.State = "succeeded"
	if err := fixture.agent.storeState(runId, state); err != nil {
		t.Fatal(err)
	}
	response, _, err := callExistingControlTest(t, fixture, observer, Request{Operation: "status", RunId: runId, FencingToken: 2})
	if err != nil || response.State != "unknown" || response.ExistingRun != nil {
		t.Fatal("unjoined terminal state became success", err)
	}
	fixture.runner.stateLock.Lock()
	fixture.runner.units[unitName(runId)] = UnitStatus{Exists: true}
	fixture.runner.stateLock.Unlock()
	state.CleanupVerified, state.StopCleanupVerified = true, true
	state.Files, err = fixture.agent.collectFiles(fixture.agent.workDirectory(runId))
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.agent.storeState(runId, state); err != nil {
		t.Fatal(err)
	}
	response, body, err := callExistingControlTest(t, fixture, observer, Request{Operation: "artifacts", RunId: runId, FencingToken: 2})
	if err != nil || response.State != "succeeded" || response.ExistingRun == nil || !response.ExistingRun.StopCleanupVerified {
		t.Fatal("authenticated terminal artifact receipt was unavailable", err)
	}
	if err := ReadFiles(body, t.TempDir(), response.Files, fixture.config.Limits.ArtifactBytes, fixture.config.Limits); err != nil {
		t.Fatal("actual terminal artifact bodies did not authenticate", err)
	}
	if forbidden.Load() != 0 || !sameIdentityForExistingTest(response.Identity, &fixture.manifest.Identity) {
		t.Fatal("terminal receipt prepared another execution or replaced its clock")
	}
	state, _ = fixture.agent.loadState(runId)
	state.ExecutionStarted = false
	if err := fixture.agent.storeState(runId, state); err != nil {
		t.Fatal(err)
	}
	response, _, err = callExistingControlTest(t, fixture, observer, Request{Operation: "status", RunId: runId, FencingToken: 2})
	if err != nil || response.State != "unknown" || response.ExistingRun != nil {
		t.Fatal("terminal cleanup fabricated success for an execution never started", err)
	}
}

// Comparable identity copies retain time instants across canonical JSON.
func sameIdentityForExistingTest(left, right *Identity) bool {
	if left == nil || right == nil {
		return false
	}
	copy := *left
	copy.StartedAt, copy.DeadlineAt = right.StartedAt, right.DeadlineAt
	return copy == *right && left.StartedAt.Equal(right.StartedAt) && left.DeadlineAt.Equal(right.DeadlineAt)
}

// The actual managed wrapper, physical pipes, real signed server and pressure
// predicates compose in one test. Only PID1 service/process owners are replaced.
func TestExistingManagedPressureStillDispatchesExistingSignedStatus(t *testing.T) {
	fixture, observer, policy, facts, forbidden := newExistingControlTest(t)
	config, settings := *observer.existingControl, defaultExistingControlSettings()
	settings.Agent.Now = fixture.settings.Now
	runtime := newRuntimeBoundaryTest(t)
	runtime.host.facts.FreeBytes, runtime.host.facts.BackingFreeBytes = 0, 0
	host := managementSshTestHost(config, policy, settings)
	host.admit = func(context.Context, *Config) (*ManagementResourcePolicy, Pin, error) {
		if validateManagementOrdinaryFacts(facts, policy, fixture.settings) == nil {
			t.Error("fresh pressure gate was not active")
		}
		if err := validateManagementOrdinaryForPurpose(facts, policy, fixture.settings, managementExistingContainment); err != nil {
			return nil, Pin{}, err
		}
		if _, err := verifyRuntimeResourceBoundary(t.Context(), runtime.pin, runtime.scope, runtime.settings, runtime.host); err == nil {
			t.Error("fresh runtime disk watermarks were not enforced")
		}
		boundary, err := verifyRuntimeResourceBoundaryForPurpose(t.Context(), runtime.pin, runtime.scope, runtime.settings, runtime.host, managementExistingContainment)
		if err != nil {
			return nil, Pin{}, err
		}
		defer boundary.Close()
		return &policy, Pin{Path: "/usr/bin/systemctl", Sha256: strings.Repeat("e", 64)}, nil
	}
	var started, completed, stopped, retired atomic.Bool
	joined := make(chan error, 1)
	stopRelease := make(chan struct{})
	var stopOnce sync.Once
	host.start = func(ctx context.Context, _ *Config, plan managementSshPlan, marker string, files []*os.File) (managementSshProcess, error) {
		if !plan.ExistingOnly || plan.UnitName != settings.unitName(plan.Id) || !strings.Contains(marker, settings.registryName()) {
			t.Error("observer used ordinary private placement")
		}
		started.Store(true)
		go func() { err := observer.ServeExisting(ctx, files[0], files[1]); completed.Store(true); joined <- err }()
		return &managementSshTestProcess{wait: func() error { err := <-joined; <-stopRelease; return err }, kill: func() error { stopOnce.Do(func() { close(stopRelease) }); return nil }}, nil
	}
	host.observe = func(context.Context, *Config, *ManagementResourcePolicy, Pin, managementSshPlan) (managementSshUnit, error) {
		return managementSshUnit{exists: started.Load() && !retired.Load(), completed: completed.Load(), inactive: stopped.Load(), invocationId: strings.Repeat("e", 32)}, nil
	}
	host.stop = func(_ context.Context, _ Pin, unit string) error {
		if !validExistingControlUnit(unit) {
			t.Error("transport stopped another role")
		}
		stopped.Store(true)
		stopOnce.Do(func() { close(stopRelease) })
		return nil
	}
	host.empty = func(context.Context, string) (bool, error) { return stopped.Load(), nil }
	host.resetFailed = func(context.Context, Pin, string) error { retired.Store(true); return nil }
	inRead, inWrite, outRead, outWrite, _, diagWrite := managementSshTestPipes(t)
	request := Request{Schema: Schema, HostId: fixture.config.HostId, Operation: "status", RunId: fixture.manifest.Identity.RunId, FencingToken: 2}
	if err := SignRequest(&request, fixture.coordinatorKey); err != nil {
		t.Fatal(err)
	}
	if err := WriteDocument(inWrite, request, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.serve(t.Context(), config.configPath, inRead, outWrite, diagWrite) }()
	response := Response{}
	if err := ReadDocument(outRead, &response, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || response.State != "running" || response.ExistingRun == nil || forbidden.Load() != 0 {
		t.Fatal("independent managed status was stranded behind fresh admission", err)
	}
	if err := VerifyResponse(response, fixture.agentPublicKey); err != nil {
		t.Fatal(err)
	}
}

// The recovery aggregate preserves its original margin while the single
// separately capped control child is alive, including concurrent emergency work.
func TestExistingControlLeavesRecoveryRunnableWithExactFiniteChild(t *testing.T) {
	policy, role, settings, facts := managementRecoveryTestFacts(true)
	settings.existingRecoveryProfile = existingRecoveryCapacityProfile
	memory, tasks, err := recoveryServiceLimits(Config{ExistingRecoveryProfile: existingRecoveryCapacityProfile}, "--recover-active")
	if err != nil {
		t.Fatal(err)
	}
	for _, values := range []map[string]string{facts.RecoveryServiceBefore, facts.RecoveryServiceAfter} {
		values["memory.max"], values["pids.max"] = strconv.FormatInt(memory, 10), strconv.FormatInt(tasks, 10)
	}
	control := defaultExistingControlSettings()
	name := control.unitName(strings.Repeat("e", 64))
	values := map[string]string{"memory.max": strconv.FormatInt(control.MemoryBytes, 10), "memory.high": "max", "memory.swap.max": "0", "memory.current": "1024", "pids.max": strconv.FormatInt(control.TasksMaximum, 10), "pids.current": "1",
		"cpu.max": fmt.Sprintf("%d %d", int64(control.Agent.ManagementCpuQuotaPercent)*control.Agent.ContainmentCpuPeriodMicros/100, control.Agent.ContainmentCpuPeriodMicros), "cpuset.cpus": policy.ManagementCpuSet, "cpuset.cpus.effective": policy.ManagementCpuSet, "cgroup.type": "domain", "memory.oom.group": "1"}
	facts.ChildrenBefore, facts.ChildrenAfter = []string{managementRecoveryServiceName, name}, []string{managementRecoveryServiceName, name}
	facts.ExistingControlsBefore, facts.ExistingControlsAfter = map[string]map[string]string{name: values}, map[string]map[string]string{name: maps.Clone(values)}
	facts.Before["memory.current"], facts.After["memory.current"] = strconv.FormatInt(role.MemoryBytes-settings.RecoveryMemoryHeadroomBytes, 10), strconv.FormatInt(role.MemoryBytes-settings.RecoveryMemoryHeadroomBytes, 10)
	facts.Before["pids.current"], facts.After["pids.current"] = "2", "2"
	if err := validateManagementRecoveryFacts(facts, policy, role, settings, true); err != nil {
		t.Fatal("bounded observer disabled independent emergency recovery", err)
	}
	facts.ExistingControlsAfter[name]["memory.max"] = "max"
	if validateManagementRecoveryFacts(facts, policy, role, settings, true) == nil {
		t.Fatal("an unbounded observer acquired recovery reserve")
	}
}

// Ordinary recovery arming is still exclusive fresh admission. Recognizing a
// companion observer under current recovery never opens that fresh gate.
func TestExistingControlCannotMakeFreshRecoveryAdmissionConcurrent(t *testing.T) {
	policy, role, settings, facts := managementRecoveryTestFacts(true)
	name := defaultExistingControlSettings().unitName(strings.Repeat("e", 64))
	facts.ExistingControlsBefore = map[string]map[string]string{name: {}}
	if validateManagementRecoveryFacts(facts, policy, role, settings, false) == nil {
		t.Fatal("fresh recovery admission accepted an active observer")
	}
}

// This role cannot be reinterpreted as a fresh worker/host-check constructor,
// including local callers which bypass SSH but retain the new configuration.
func TestExistingControlConfigurationCannotCreateFreshWorkerAuthority(t *testing.T) {
	fixture, observer, _, _, forbidden := newExistingControlTest(t)
	if _, err := newAgent(observer.existingControl, fixture.runner, fixture.settings, false); err == nil {
		t.Fatal("observer configuration admitted fresh worker authority")
	}
	if err := observer.Serve(t.Context(), bytes.NewReader(nil), &bytes.Buffer{}); err == nil {
		t.Fatal("existing agent served ordinary framing")
	}
	if err := observer.Execute(t.Context(), fixture.manifest.Identity.RunId); err == nil {
		t.Fatal("existing observer executed a candidate")
	}
	if _, err := observer.prepareCleanupPlan(t.Context(), fixture.manifest); err == nil {
		t.Fatal("existing observer prepared a new execution plan")
	}
	if forbidden.Load() != 0 {
		t.Fatal("role rejection reached fresh native admission")
	}
}

// An explicit fake clock crosses the ORIGINAL deadline. The same signed run
// can then be stopped/cleaned, but it never receives a new execution allowance.
func TestExistingControlCancelUsesOriginalDeadlineAndNoRestart(t *testing.T) {
	fixture, observer, _, _, forbidden := newExistingControlTest(t)
	launches := fixture.runner.launchCount()
	deadline := fixture.manifest.Identity.DeadlineAt
	fixture.now = deadline.Add(time.Second)
	response, _, err := callExistingControlTest(t, fixture, observer, Request{Operation: "cancel", RunId: fixture.manifest.Identity.RunId, FencingToken: 2})
	if err != nil || response.State != "expired" || response.ExistingRun == nil || !response.ExistingRun.CleanupVerified || !response.ExistingRun.StopCleanupVerified || !response.Identity.DeadlineAt.Equal(deadline) {
		t.Fatal("deadline cancellation lost original authority or fabricated success", err)
	}
	if forbidden.Load() != 0 || fixture.runner.launchCount() != launches {
		t.Fatal("deadline cancellation restarted/prepared original execution")
	}
}

// Constructor and framing repeat the real native proof. The same protected
// receipt can acknowledge both, but a changed generation cannot replace it.
func TestExistingControlRepeatedPlacementKeepsExactOriginalAdmissionReceipt(t *testing.T) {
	config, _, _ := managementSshTestConfig(t)
	settings := defaultExistingControlSettings()
	registry, err := openManagementSshRegistry(config, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	id := strings.Repeat("e", 64)
	content := []byte(`{"schema":1,"synthetic_generation":"original"}`)
	if err := registry.recordCurrentAdmission(id, content); err != nil {
		t.Fatal(err)
	}
	if err := registry.recordCurrentAdmission(id, content); err != nil {
		t.Fatal("framing rejected the constructor's identical placed generation", err)
	}
	if registry.recordCurrentAdmission(id, []byte(`{"schema":1,"synthetic_generation":"changed"}`)) == nil {
		t.Fatal("changed generation reused an old native placement receipt")
	}
}

// A new observer cannot silently retrofit an older recovery binary whose
// original closure rejects this child, even with otherwise valid role metadata.
func TestExistingControlMixedLegacyRecoveryProgramRefusesActivation(t *testing.T) {
	fixture, observer, _, _, _ := newExistingControlTest(t)
	if err := validateExistingRecoveryCompatibility(observer.existingControl, fixture.config); err != nil {
		t.Fatal("matching code with distinct original identity paths was refused", err)
	}
	mixed := *observer.existingControl
	mixed.Agent.Sha256 = strings.Repeat("f", 64)
	if validateExistingRecoveryCompatibility(&mixed, fixture.config) == nil {
		t.Fatal("new observer accepted an incompatible legacy recovery program")
	}
	if _, err := newExistingControlAgent(&mixed, fixture.config, fixture.runner, fixture.settings, false); err == nil {
		t.Fatal("mixed companion configuration reached existing control construction")
	}
}
