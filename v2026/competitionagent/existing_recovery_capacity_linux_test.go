//go:build linux

// Capacity regressions compose protected-content pins, the real recovery proof,
// private descriptor trees, durable cleanup and signed observer framing. No test
// reads native systemd/proc/cgroups or runs an external program.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A cleanup owner can stop only the original evaluator, never the observer or
// an arbitrary management service. The underlying stop exposes a join barrier.
type existingRecoveryCapacityTestRunner struct {
	Runner
	originalUnit string
}

func (self existingRecoveryCapacityTestRunner) Stop(ctx context.Context, unit string) error {
	if unit != self.originalUnit {
		return errors.New("cleanup attempted to preempt an observer or unrelated owner")
	}
	return self.Runner.Stop(ctx, unit)
}

// The new contract is installed before this fixture's first original attempt.
// Old native identity is never rewritten to manufacture compatibility.
func existingRecoveryCapacityTestFixture(t *testing.T) (*agentTestFixture, *SystemdRunner, *ManagementResourcePolicy) {
	t.Helper()
	fixture, runner, _, policy, _ := recoveryArmTestFixture(t)
	config := fixture.config
	config.ExistingRecoveryProfile = existingRecoveryCapacityProfile
	service, err := RenderExistingRecoveryService(config.Agent.Path, config.configPath, *policy, fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryTestAuthority(t, fixture, config.RecoveryService.Path, service)
	config.RecoveryService = testAgentPin(t, config.RecoveryService.Path)
	content, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryTestAuthority(t, fixture, config.configPath, content)
	config.configSha256 = testAgentPin(t, config.configPath).Sha256
	fixture.agent.config = *config
	return fixture, runner, policy
}

// Every child and outer aggregate property is a private regular-file fixture.
// Both children may grow to their hard limits, independent of their low RSS here.
func existingRecoveryCapacityTestTree(t *testing.T, policy ManagementResourcePolicy, memory, tasks int64) (string, string, string, string) {
	t.Helper()
	policy, role, settings, root, proc, group := managementRecoveryTestTreeForPolicy(t, true, policy)
	observer := defaultExistingControlSettings()
	name := observer.unitName(strings.Repeat("e", 64))
	child := filepath.Join(group, name)
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"memory.max": strconv.FormatInt(observer.MemoryBytes, 10), "memory.high": "max", "memory.swap.max": "0", "memory.current": "1024",
		"pids.max": strconv.FormatInt(observer.TasksMaximum, 10), "pids.current": "1", "cgroup.type": "domain", "memory.oom.group": "1",
		"cpu.max":     strconv.FormatInt(int64(settings.ManagementCpuQuotaPercent)*settings.ContainmentCpuPeriodMicros/100, 10) + " " + strconv.FormatInt(settings.ContainmentCpuPeriodMicros, 10),
		"cpuset.cpus": policy.ManagementCpuSet, "cpuset.cpus.effective": policy.ManagementCpuSet}
	for property, value := range values {
		if err := os.WriteFile(filepath.Join(child, property), []byte(value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for path, value := range map[string]string{
		filepath.Join(group, managementRecoveryServiceName, "memory.max"): strconv.FormatInt(memory, 10),
		filepath.Join(group, managementRecoveryServiceName, "pids.max"):   strconv.FormatInt(tasks, 10),
		filepath.Join(group, "memory.current"):                            "2048", filepath.Join(group, "pids.current"): "2",
	} {
		if err := os.WriteFile(path, []byte(value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if role.MemoryBytes != 2*1024*1024*1024 || role.TasksMaximum != 1024 {
		t.Fatal("test widened the shared outer recovery aggregate")
	}
	return root, proc, group, name
}

// Only the fixed native roots are substituted; the actual descriptor reads and
// complete closure/resource validation remain inside the real service proof.
func existingRecoveryCapacityTestBoundary(root, proc string) func(context.Context, *ManagementResourcePolicy, Pin, AgentSettings, bool, bool) error {
	return func(ctx context.Context, policy *ManagementResourcePolicy, _ Pin, settings AgentSettings, protected, current bool) error {
		if protected || !current {
			return errors.New("synthetic boundary requires current unprotected recovery")
		}
		role, err := managementPolicyRole(policy, ManagementCleanupRole)
		if err != nil {
			return err
		}
		facts, err := readManagementRecoveryFacts(ctx, role, settings, root, proc, false, current)
		if err != nil {
			return err
		}
		return validateManagementRecoveryFacts(facts, *policy, role, settings, current)
	}
}

// The original bug admitted future 2.25GiB/1152-task commitments at tiny RSS.
// Neither a valid companion config nor current headroom may excuse either cap.
func TestExistingRecoveryCapacityLowUsageCannotHideFutureOvercommit(t *testing.T) {
	for _, fault := range []string{"memory", "tasks", "both"} {
		fixture, _, policy := existingRecoveryCapacityTestFixture(t)
		memory, tasks, err := recoveryServiceLimits(*fixture.config, "--recover-active")
		if err != nil {
			t.Fatal(err)
		}
		if fault != "tasks" {
			memory = DefaultManagementBoundarySettings().CleanupMemoryBytes
		}
		if fault != "memory" {
			tasks = DefaultManagementBoundarySettings().CleanupTasksMaximum
		}
		root, proc, _, _ := existingRecoveryCapacityTestTree(t, *policy, memory, tasks)
		command := func(context.Context, Pin, []string, AgentSettings) ([]byte, error) {
			return recoveryTestProjection(*fixture.config, fixture.settings, policy, true), nil
		}
		if verifyRecoveryService(t.Context(), *fixture.config, fixture.settings, false, true, existingRecoveryCapacityTestBoundary(root, proc), command) == nil {
			t.Fatal("low observed usage hid oversubscribed future child ceilings", fault)
		}
		if fixture.runner.launchCount() != 0 || fixture.cleaner.count() != 0 {
			t.Fatal("capacity refusal performed execution or cleanup")
		}
	}
}

// The positive boundary consumes exactly the unchanged outer future allowance;
// measured usage still leaves the original independent helper runnable margin.
func TestExistingRecoveryCapacityExactFutureBoundaryRemainsRunnable(t *testing.T) {
	fixture, _, policy := existingRecoveryCapacityTestFixture(t)
	memory, tasks, err := recoveryServiceLimits(*fixture.config, "--recover-active")
	if err != nil {
		t.Fatal(err)
	}
	observer := defaultExistingControlSettings()
	aggregate := DefaultManagementBoundarySettings()
	if memory != 1879048192 || tasks != 896 || memory+observer.MemoryBytes != aggregate.CleanupMemoryBytes || tasks+observer.TasksMaximum != aggregate.CleanupTasksMaximum {
		t.Fatal("future hard ceilings do not partition the unchanged aggregate exactly")
	}
	root, proc, group, name := existingRecoveryCapacityTestTree(t, *policy, memory, tasks)
	for path, value := range map[string]int64{
		filepath.Join(group, managementRecoveryServiceName, "memory.current"): memory - fixture.settings.RecoveryMemoryHeadroomBytes,
		filepath.Join(group, managementRecoveryServiceName, "pids.current"):   tasks - fixture.settings.RecoveryTasksHeadroom,
		filepath.Join(group, name, "memory.current"):                          observer.MemoryBytes,
		filepath.Join(group, name, "pids.current"):                            observer.TasksMaximum,
		filepath.Join(group, "memory.current"):                                aggregate.CleanupMemoryBytes - fixture.settings.RecoveryMemoryHeadroomBytes,
		filepath.Join(group, "pids.current"):                                  aggregate.CleanupTasksMaximum - fixture.settings.RecoveryTasksHeadroom,
	} {
		if err := os.WriteFile(path, []byte(strconv.FormatInt(value, 10)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := func(context.Context, Pin, []string, AgentSettings) ([]byte, error) {
		return recoveryTestProjection(*fixture.config, fixture.settings, policy, true), nil
	}
	if err := verifyRecoveryService(t.Context(), *fixture.config, fixture.settings, false, true, existingRecoveryCapacityTestBoundary(root, proc), command); err != nil {
		t.Fatal("exact future-fit companion and observer lost runnable cleanup", err)
	}
}

// Native affinity is not a fixture input. The authenticated synthetic policy
// must produce matching outer, child and process facts; mismatches still refuse.
func TestExistingRecoveryCapacityFixtureBindsAuthenticatedCpuPolicy(t *testing.T) {
	fixture, _, policy := existingRecoveryCapacityTestFixture(t)
	memory, tasks, err := recoveryServiceLimits(*fixture.config, "--recover-active")
	if err != nil {
		t.Fatal(err)
	}
	root, proc, group, name := existingRecoveryCapacityTestTree(t, *policy, memory, tasks)
	command := func(context.Context, Pin, []string, AgentSettings) ([]byte, error) {
		return recoveryTestProjection(*fixture.config, fixture.settings, policy, true), nil
	}
	verify := func() error {
		return verifyRecoveryService(t.Context(), *fixture.config, fixture.settings, false, true, existingRecoveryCapacityTestBoundary(root, proc), command)
	}
	if err := verify(); err != nil {
		t.Fatal("authenticated synthetic CPU policy did not compose", err)
	}
	wrong := managementTestPolicy(DefaultManagementBoundarySettings()).ManagementCpuSet
	if wrong == policy.ManagementCpuSet {
		t.Fatal("CPU root-cause control did not retain distinct synthetic policies")
	}
	for _, property := range []string{"cpuset.cpus", "cpuset.cpus.effective"} {
		path := filepath.Join(group, name, property)
		if err := os.WriteFile(path, []byte(wrong+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := verify(); err == nil || err.Error() != "recovery child escaped management processors" {
			t.Fatal("wrong child CPU facts lost the precise production refusal", property, err)
		}
		if err := os.WriteFile(path, []byte(policy.ManagementCpuSet+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := verify(); err != nil {
			t.Fatal("restored authenticated CPU facts did not recover", property, err)
		}
	}
}

// Adding recovery-manager metadata must retain the original declared id map.
// Coherently re-pinned zero maps must fail at namespace validation, not pin I/O.
func TestExistingRecoveryCapacityFixturePreservesNamespaceAuthority(t *testing.T) {
	readBoundary := func(config *Config) RuntimeResourceBoundary {
		content, err := os.ReadFile(config.DockerRole.ResourceBoundary.Path)
		if err != nil {
			t.Fatal(err)
		}
		var boundary RuntimeResourceBoundary
		if err := json.Unmarshal(content, &boundary); err != nil {
			t.Fatal(err)
		}
		return boundary
	}
	original := readBoundary(newAgentTestFixture(t).config)
	for _, worker := range []uint32{1000, 65534} {
		plan := DockerProxyPlan{WorkerUid: worker, HostFiles: &HostFilesPlan{WorkerGid: worker,
			ContainerRootUid: original.ContainerRootUid, ContainerRootGid: original.ContainerRootGid,
			ContainerIdMapLength: original.ContainerIdMapLength, WorkDeviceId: 1, EvidenceBytes: 1}}
		if err := validateHostFilesPlan(plan, DefaultHostFilesSettings()); err != nil {
			t.Fatal("synthetic namespace depended on the native host worker uid", worker, err)
		}
	}
	for _, fault := range []string{"none", "zero-uid", "zero-gid", "zero-length"} {
		fixture, _, _ := existingRecoveryCapacityTestFixture(t)
		boundary := readBoundary(fixture.config)
		if boundary.ContainerRootUid != original.ContainerRootUid || boundary.ContainerRootGid != original.ContainerRootGid ||
			boundary.ContainerIdMapLength != original.ContainerIdMapLength || boundary.Systemctl.Path == "" {
			t.Fatal("recovery fixture replaced original namespace authority")
		}
		switch fault {
		case "zero-uid":
			boundary.ContainerRootUid = 0
		case "zero-gid":
			boundary.ContainerRootGid = 0
		case "zero-length":
			boundary.ContainerIdMapLength = 0
		}
		content, err := json.Marshal(boundary)
		if err != nil {
			t.Fatal(err)
		}
		writeRecoveryTestAuthority(t, fixture, fixture.config.DockerRole.ResourceBoundary.Path, content)
		fixture.config.DockerRole.ResourceBoundary = testAgentPin(t, fixture.config.DockerRole.ResourceBoundary.Path)
		content, err = json.Marshal(fixture.config)
		if err != nil {
			t.Fatal(err)
		}
		writeRecoveryTestAuthority(t, fixture, fixture.config.configPath, content)
		fixture.config.configSha256 = testAgentPin(t, fixture.config.configPath).Sha256
		fixture.agent.config = *fixture.config
		_, err = fixture.call(t, "start", 1, &fixture.manifest)
		if fault == "none" {
			if err != nil || fixture.runner.launchCount() != 1 {
				t.Fatal("preserved namespace did not reach the actual signed start", err)
			}
		} else if err == nil || err.Error() != "host-file namespace range overlaps privileged or trusted host identity" || fixture.runner.launchCount() != 0 {
			t.Fatal("zero namespace did not fail at the actual production gate before launch", fault, err)
		}
	}
}

// Protected template, loaded PID1 state and actual child caps must agree.
// A launch-time manager change and a descriptor-bracketed native cap change
// cannot slip through a low-RSS observation.
func TestExistingRecoveryCapacityPinsAndLaunchRacesFailClosed(t *testing.T) {
	for _, fault := range []string{"text-memory", "text-tasks", "manager-memory", "manager-tasks", "manager-race", "native-race", "duplicate-observer", "child-memory-margin", "child-task-margin"} {
		fixture, _, policy := existingRecoveryCapacityTestFixture(t)
		memory, tasks, err := recoveryServiceLimits(*fixture.config, "--recover-active")
		if err != nil {
			t.Fatal(err)
		}
		root, proc, group, name := existingRecoveryCapacityTestTree(t, *policy, memory, tasks)
		if fault == "child-memory-margin" || fault == "child-task-margin" {
			property, current := "memory.current", memory-fixture.settings.RecoveryMemoryHeadroomBytes+1
			if fault == "child-task-margin" {
				property, current = "pids.current", tasks-fixture.settings.RecoveryTasksHeadroom+1
			}
			for path, value := range map[string]int64{filepath.Join(group, managementRecoveryServiceName, property): current, filepath.Join(group, property): current + 1024} {
				if property == "pids.current" && path == filepath.Join(group, property) {
					value = current + 1
				}
				if err := os.WriteFile(path, []byte(strconv.FormatInt(value, 10)+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		if strings.HasPrefix(fault, "text-") {
			content, err := os.ReadFile(fixture.config.RecoveryService.Path)
			if err != nil {
				t.Fatal(err)
			}
			old, replacement := "MemoryMax="+strconv.FormatInt(memory, 10), "MemoryMax="+strconv.FormatInt(DefaultManagementBoundarySettings().CleanupMemoryBytes, 10)
			if fault == "text-tasks" {
				old, replacement = "TasksMax="+strconv.FormatInt(tasks, 10), "TasksMax="+strconv.FormatInt(DefaultManagementBoundarySettings().CleanupTasksMaximum, 10)
			}
			writeRecoveryTestAuthority(t, fixture, fixture.config.RecoveryService.Path, []byte(strings.Replace(string(content), old, replacement, 1)))
			fixture.config.RecoveryService = testAgentPin(t, fixture.config.RecoveryService.Path)
			content, err = json.Marshal(fixture.config)
			if err != nil {
				t.Fatal(err)
			}
			writeRecoveryTestAuthority(t, fixture, fixture.config.configPath, content)
			fixture.config.configSha256 = testAgentPin(t, fixture.config.configPath).Sha256
		}
		if fault == "duplicate-observer" {
			if err := os.Mkdir(filepath.Join(group, strings.Replace(name, strings.Repeat("e", 64), strings.Repeat("f", 64), 1)), 0700); err != nil {
				t.Fatal(err)
			}
		}
		calls := 0
		command := func(context.Context, Pin, []string, AgentSettings) ([]byte, error) {
			calls++
			projection := string(recoveryTestProjection(*fixture.config, fixture.settings, policy, true))
			if fault == "manager-memory" || fault == "manager-race" && calls > 1 {
				projection = strings.Replace(projection, "MemoryMax="+strconv.FormatInt(memory, 10), "MemoryMax="+strconv.FormatInt(DefaultManagementBoundarySettings().CleanupMemoryBytes, 10), 1)
			}
			if fault == "manager-tasks" {
				projection = strings.Replace(projection, "TasksMax="+strconv.FormatInt(tasks, 10), "TasksMax="+strconv.FormatInt(DefaultManagementBoundarySettings().CleanupTasksMaximum, 10), 1)
			}
			return []byte(projection), nil
		}
		boundary := existingRecoveryCapacityTestBoundary(root, proc)
		if fault == "native-race" {
			boundary = func(ctx context.Context, policy *ManagementResourcePolicy, _ Pin, settings AgentSettings, _, current bool) error {
				role, err := managementPolicyRole(policy, ManagementCleanupRole)
				if err != nil {
					return err
				}
				facts, err := readManagementRecoveryFactsWithHook(ctx, role, settings, root, proc, false, current, func() {
					if err := os.WriteFile(filepath.Join(group, managementRecoveryServiceName, "memory.max"), []byte(strconv.FormatInt(DefaultManagementBoundarySettings().CleanupMemoryBytes, 10)+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				})
				if err != nil {
					return err
				}
				return validateManagementRecoveryFacts(facts, *policy, role, settings, current)
			}
		}
		if verifyRecoveryService(t.Context(), *fixture.config, fixture.settings, false, true, boundary, command) == nil {
			t.Fatal("changed future companion authority passed the actual proof", fault)
		}
	}
}

// Defaults and explicit-profile incompatibility are separate: no ordinary cap
// changes, no legacy/mixed observer activation, and no second observer allowance.
func TestExistingRecoveryCapacityLegacyDefaultsAndMixedRolesRemainClosed(t *testing.T) {
	policy := managementTestPolicy(DefaultManagementBoundarySettings())
	settings := DefaultAgentSettings()
	config := Config{Agent: Pin{Path: "/synthetic/bin/agent"}, configPath: "/synthetic/agent.json"}
	ordinary, err := RenderRecoveryService(config.Agent.Path, config.configPath, policy, settings)
	if err != nil || validateRecoveryServiceText(ordinary, config, &policy, settings) != nil ||
		!strings.Contains(string(ordinary), "MemoryMax=2147483648\n") || !strings.Contains(string(ordinary), "TasksMax=1024\n") {
		t.Fatal("default-absent ordinary recovery changed", err)
	}
	fixture, observer, _, _, _ := newExistingControlTest(t)
	legacy := *fixture.config
	legacy.ExistingRecoveryProfile = ""
	if validateExistingRecoveryCompatibility(observer.existingControl, &legacy) == nil {
		t.Fatal("matching binary alone authorized a legacy full-cap companion")
	}
	if _, err := newExistingControlAgent(observer.existingControl, &legacy, fixture.runner, fixture.settings, false); err == nil {
		t.Fatal("legacy profile reached existing observer construction")
	}
	for _, mutation := range []string{"profile", "memory", "tasks", "count"} {
		profile := defaultExistingControlSettings()
		if mutation == "profile" {
			invalid := config
			invalid.ExistingRecoveryProfile = "synthetic-unknown/v1"
			if _, _, err := recoveryServiceLimits(invalid, "--recover-active"); err == nil {
				t.Fatal("unknown protected capacity profile was interpreted")
			}
			continue
		}
		switch mutation {
		case "memory":
			profile.MemoryBytes++
		case "tasks":
			profile.TasksMaximum++
		case "count":
			profile.MaximumControls++
		}
		if profile.validate() == nil {
			t.Fatal("observer expanded the once-only reservation", mutation)
		}
	}
}

// Emergency cleanup owns only the original evaluator stop. It joins while the
// observer is alive, without starting, preempting or relabeling either role.
func TestExistingRecoveryCapacityConcurrentCleanupJoinsWithoutObserverPreemption(t *testing.T) {
	fixture, _, policy := existingRecoveryCapacityTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	clock := recoveryClockBytes(t, fixture)
	control := *fixture.config
	control.configPath, control.configSha256 = filepath.Join(control.BaseDirectory, "observer.json"), strings.Repeat("b", 64)
	control.Agent = Pin{Path: filepath.Join(control.BaseDirectory, "matching-observer-copy"), Sha256: fixture.config.Agent.Sha256}
	control.ExistingControl = &ExistingControlRole{ApprovalSha256: strings.Repeat("d", 64), OriginalConfig: Pin{Path: fixture.config.configPath, Sha256: fixture.config.configSha256}}
	observer, err := newExistingControlAgent(&control, fixture.config, fixture.runner, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	observer.existingPlacement = func(context.Context) error { return nil }
	observer.existingProof = func(context.Context, *StartManifest, *cleanupPlan, *runState) (string, error) {
		return "", errors.New("cleanup-active observer must refuse before proof")
	}
	memory, tasks, err := recoveryServiceLimits(*fixture.config, "--recover-active")
	if err != nil {
		t.Fatal(err)
	}
	root, proc, group, name := existingRecoveryCapacityTestTree(t, *policy, memory, tasks)
	before, err := os.Stat(filepath.Join(group, name))
	if err != nil {
		t.Fatal(err)
	}
	recovery := recoveryTestAgent(t, fixture)
	recovery.runner = existingRecoveryCapacityTestRunner{Runner: fixture.runner, originalUnit: unitName(fixture.manifest.Identity.RunId)}
	command := func(context.Context, Pin, []string, AgentSettings) ([]byte, error) {
		return recoveryTestProjection(*fixture.config, fixture.settings, policy, true), nil
	}
	recovery.recoveryPlacement = func(ctx context.Context, current bool) error {
		return verifyRecoveryService(ctx, *fixture.config, fixture.settings, false, current, existingRecoveryCapacityTestBoundary(root, proc), command)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	t.Cleanup(func() { cancel(); once.Do(func() { close(release) }); <-done })
	fixture.runner.beforeStop = func() {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	go func() { done <- recovery.RecoverActive(ctx) }()
	guard, finish := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer finish()
	select {
	case <-entered:
	case <-guard.Done():
		t.Fatal("compatible recovery did not join while observer was alive")
	}
	response, _, err := callExistingControlTest(t, fixture, observer, Request{Operation: "status", RunId: fixture.manifest.Identity.RunId, FencingToken: 2})
	if err != nil || response.State != "unknown" || response.ExistingRun != nil {
		t.Fatal("concurrent recovery produced synthetic observer success", err)
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal("compatible companion failed to join cleanup", err)
		}
	case <-guard.Done():
		t.Fatal("cleanup did not join its original owner")
	}
	after, err := os.Stat(filepath.Join(group, name))
	state, stateErr := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || !os.SameFile(before, after) || stateErr != nil || state.RecoveryActive || !state.CleanupVerified || !state.StopCleanupVerified ||
		fixture.runner.launchCount() != 1 || !bytes.Equal(clock, recoveryClockBytes(t, fixture)) || observer.config.Agent != fixture.config.Agent {
		t.Fatal("cleanup preempted observer, restarted execution or changed original custody", err, stateErr)
	}
}
