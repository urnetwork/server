//go:build linux

// Manager projections and held native-read code are exercised with synthetic
// observations/private regular files. No real systemctl, proc or cgroup runs.
package competitionagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every requested property is present, including empty forbidden hooks.
func managementSshTestProjection(config Config, policy ManagementResourcePolicy, plan managementSshPlan, settings ManagementSshSettings) map[string]string {
	values := map[string]string{}
	for _, key := range strings.Split(managementSshProperties, ",") {
		values[key] = ""
	}
	for key, value := range map[string]string{
		"Id": plan.UnitName, "LoadState": "loaded", "ActiveState": "active", "SubState": "running", "Transient": "yes", "Slice": managementPrivateSliceName,
		// Read-only PID1 observation of an existing owned test unit:
		// Transient=yes, FragmentPath=/run/systemd/transient/<exact unit>.
		"FragmentPath": "/run/systemd/transient/" + plan.UnitName,
		"ControlGroup": managementPrivateCgroupPath + "/" + plan.UnitName, "User": "root", "Group": "root", "Type": "exec", "Restart": "no", "RemainAfterExit": "yes",
		"KillMode": "control-group", "MainPID": "17", "InvocationID": strings.Repeat("e", 32), "ExecMainStartTimestampMonotonic": "100",
		"MemoryMax": strconv.FormatInt(plan.MemoryBytes, 10), "MemoryHigh": "infinity", "MemorySwapMax": "0", "TasksMax": strconv.FormatInt(plan.TasksMaximum, 10),
		"CPUQuotaPerSecUSec": fmt.Sprintf("%dus", settings.Agent.ManagementCpuQuotaPercent*10000), "CPUQuotaPeriodUSec": fmt.Sprintf("%dus", settings.Agent.ContainmentCpuPeriodMicros),
		"AllowedCPUs": strings.ReplaceAll(policy.ManagementCpuSet, ",", " "), "CPUAffinity": strings.ReplaceAll(policy.ManagementCpuSet, ",", " "),
		"RuntimeMaxUSec": fmt.Sprintf("%dus", plan.RuntimeMaximum.Microseconds()), "TimeoutStopUSec": fmt.Sprintf("%dus", settings.Agent.CancelGrace.Microseconds()),
		"NoNewPrivileges": "yes", "CapabilityBoundingSet": strings.ToLower(managementSshCapabilities),
		"ExecStart": "{ path=" + config.Agent.Path + " ; argv[]=" + config.Agent.Path + " --config " + config.configPath + " --serve ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=17 ; code=(null) ; status=0 }",
	} {
		values[key] = value
	}
	return values
}

// Ordering is irrelevant but the finite requested property set is exact.
func managementSshTestProjectionBytes(values map[string]string) []byte {
	var content strings.Builder
	for _, key := range strings.Split(managementSshProperties, ",") {
		if value, found := values[key]; found {
			fmt.Fprintf(&content, "%s=%s\n", key, value)
		}
	}
	return []byte(content.String())
}

// Running and active/exited remain distinct from inactive. The latter state
// drives Stop without making systemd-run --wait a circular prerequisite.
func TestManagementSshManagerProjectionAcceptsExactRunningAndExitedClass(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	plan := managementSshTestPlan(config, policy, settings)
	values := managementSshTestProjection(config, policy, plan, settings)
	unit, err := validateManagementSshUnit(managementSshTestProjectionBytes(values), config, &policy, plan, settings)
	if err != nil || !unit.exists || unit.completed || unit.inactive || unit.pid != 17 {
		t.Fatal("exact running control projection was rejected", err)
	}
	values["SubState"], values["MainPID"] = "exited", "0"
	unit, err = validateManagementSshUnit(managementSshTestProjectionBytes(values), config, &policy, plan, settings)
	if err != nil || !unit.exists || !unit.completed || unit.inactive {
		t.Fatal("active/exited was not retained for explicit stop", err)
	}
	values["ActiveState"], values["SubState"], values["ControlGroup"] = "inactive", "dead", ""
	unit, err = validateManagementSshUnit(managementSshTestProjectionBytes(values), config, &policy, plan, settings)
	if err != nil || !unit.inactive {
		t.Fatal("exact inactive generation was rejected", err)
	}
}

// A missing object is never silently promoted to a prior inactive generation.
func TestManagementSshManagerProjectionPreservesMissingAndRejectsAuthorityChanges(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	plan := managementSshTestPlan(config, policy, settings)
	for _, mutate := range []func(map[string]string){
		func(values map[string]string) { values["Id"] = unitName(strings.Repeat("f", 64)) },
		func(values map[string]string) { values["FragmentPath"] = "" },
		func(values map[string]string) { values["FragmentPath"] = "/etc/systemd/system/" + plan.UnitName },
		func(values map[string]string) { values["FragmentPath"] = "/run/systemd/transient/other.service" },
		func(values map[string]string) {
			values["FragmentPath"] = "/run/systemd/transient/../transient/" + plan.UnitName
		},
		func(values map[string]string) {
			values["DropInPaths"] = "/run/systemd/system/" + plan.UnitName + ".d/override.conf"
		},
		func(values map[string]string) { values["Slice"] = managementCleanupSliceName },
		func(values map[string]string) {
			values["ControlGroup"] = managementCleanupCgroupPath + "/" + plan.UnitName
		},
		func(values map[string]string) { values["OnFailure"] = managementRecoveryServiceName },
		func(values map[string]string) { values["BindsTo"] = unitName(strings.Repeat("f", 64)) },
		func(values map[string]string) { values["MemoryMax"] = "infinity" },
		func(values map[string]string) { values["TasksMax"] = "infinity" },
		func(values map[string]string) { values["AllowedCPUs"] = "0-4095" },
		func(values map[string]string) { values["CPUAffinity"] = "0-4095" },
		func(values map[string]string) { values["RemainAfterExit"] = "no" },
		func(values map[string]string) { values["RuntimeMaxUSec"] = "30s" },
		func(values map[string]string) { values["NoNewPrivileges"] = "no" },
		func(values map[string]string) { values["AmbientCapabilities"] = "cap_sys_admin" },
		func(values map[string]string) {
			values["CapabilityBoundingSet"] = strings.ReplaceAll(strings.ToLower(managementSshCapabilities), "cap_sys_admin", "")
		},
		func(values map[string]string) {
			values["ExecStart"] = strings.Replace(values["ExecStart"], " --serve ;", " --execute="+strings.Repeat("f", 64)+" ;", 1)
		},
		func(values map[string]string) { delete(values, "MainPID") },
	} {
		values := managementSshTestProjection(config, policy, plan, settings)
		mutate(values)
		if _, err := validateManagementSshUnit(managementSshTestProjectionBytes(values), config, &policy, plan, settings); err == nil {
			t.Fatal("changed manager authority was accepted")
		}
	}
	missing := managementSshTestProjection(config, policy, plan, settings)
	missing["LoadState"], missing["ActiveState"], missing["MainPID"], missing["ControlGroup"] = "not-found", "inactive", "0", ""
	if _, err := validateManagementSshUnit(managementSshTestProjectionBytes(missing), config, &policy, plan, settings); err == nil {
		t.Fatal("missing unit retained a loaded transient fragment")
	}
	missing["FragmentPath"] = ""
	unit, err := validateManagementSshUnit(managementSshTestProjectionBytes(missing), config, &policy, plan, settings)
	if err != nil || unit.exists || unit.inactive {
		t.Fatal("missing unit was converted to positive inactive proof", err)
	}
	content := append(managementSshTestProjectionBytes(missing), []byte("LoadState=loaded\n")...)
	if _, err := validateManagementSshUnit(content, config, &policy, plan, settings); err == nil {
		t.Fatal("duplicate manager authority was accepted")
	}
}

// PID1 omits an unset ExecStart for a nonexistent service, not for an owned
// invocation. The missing object remains absence, never empty-group proof.
func TestManagementSshMissingUnitAcceptsOmittedUnsetCommand(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	plan := managementSshTestPlan(config, policy, settings)
	values := managementSshTestProjection(config, policy, plan, settings)
	values["LoadState"], values["ActiveState"], values["SubState"] = "not-found", "inactive", "dead"
	values["MainPID"], values["ControlGroup"], values["FragmentPath"] = "0", "", ""
	delete(values, "ExecStart")
	unit, err := validateManagementSshUnit(managementSshTestProjectionBytes(values), config, &policy, plan, settings)
	if err != nil || unit.exists || unit.inactive || unit.completed {
		t.Fatal("absent unset command was not represented as missing custody", err)
	}
	for _, mutate := range []func(map[string]string){
		func(values map[string]string) { delete(values, "MainPID") },
		func(values map[string]string) { delete(values, "LoadState") },
		func(values map[string]string) { values["MainPID"] = "17" },
		func(values map[string]string) { values["ControlGroup"] = "/synthetic/retained" },
		func(values map[string]string) { values["ActiveState"] = "active" },
		func(values map[string]string) { values["LoadState"] = "loaded" },
	} {
		changed := make(map[string]string, len(values))
		for key, value := range values {
			changed[key] = value
		}
		mutate(changed)
		if _, err := validateManagementSshUnit(managementSshTestProjectionBytes(changed), config, &policy, plan, settings); err == nil {
			t.Fatal("missing command waived uncertain or loaded custody")
		}
	}
}

// Status is synthetic, not an extraction from the executing host.
func managementSshTestStatus(plan managementSshPlan) []byte {
	return []byte(fmt.Sprintf("Name:\tsynthetic-control\nCpus_allowed_list:\t%s\nCapEff:\t%016x\nCapPrm:\t%016x\nCapBnd:\t%016x\nCapAmb:\t%016x\nCapInh:\t%016x\nNoNewPrivs:\t1\n",
		plan.ManagementCpuSet, managementSshCapabilityMask, managementSshCapabilityMask, managementSshCapabilityMask, uint64(0), uint64(0)))
}

// Unknown/narrower root capability state is refusal, not an instruction to
// widen the host. Zero ambient/inheritable and child nnp are independent.
func TestManagementSshCapabilityProfileRequiresDeclaredActualSets(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	plan := managementSshTestPlan(config, policy, settings)
	valid := managementSshTestStatus(plan)
	if err := validateManagementSshCapabilityStatus(valid, true); err != nil {
		t.Fatal(err)
	}
	for _, content := range [][]byte{
		[]byte(strings.Replace(string(valid), "CapEff:\t000001ffffffffff", "CapEff:\t0000000000000000", 1)),
		[]byte(strings.Replace(string(valid), "CapBnd:\t000001ffffffffff", "CapBnd:\tffffffffffffffff", 1)),
		[]byte(strings.Replace(string(valid), "CapAmb:\t0000000000000000", "CapAmb:\t0000000000200000", 1)),
		[]byte(strings.Replace(string(valid), "CapInh:\t0000000000000000", "CapInh:\t0000000000000001", 1)),
		[]byte(strings.Replace(string(valid), "NoNewPrivs:\t1", "NoNewPrivs:\t0", 1)),
		append(append([]byte(nil), valid...), []byte("CapEff:\t000001ffffffffff\n")...),
	} {
		if validateManagementSshCapabilityStatus(content, true) == nil {
			t.Fatal("changed actual root capabilities acquired child admission")
		}
	}
	if validateManagementSshCapabilityNames(strings.ToLower(managementSshCapabilities)) != nil || validateManagementSshCapabilityNames(managementSshCapabilities+" CAP_SYS_ADMIN") == nil {
		t.Fatal("closed manager capability-name comparison changed")
	}
}

// All native-file contents are private ordinary files. Zero filesystem magic
// is passed only to the unexported descriptor seam, never a native entrypoint.
func managementSshTestDescriptors(t *testing.T, plan managementSshPlan, settings ManagementSshSettings) (*os.File, *os.File, string, string) {
	t.Helper()
	root := t.TempDir()
	procPath, groupPath := filepath.Join(root, "proc"), filepath.Join(root, "group")
	for _, path := range []string{procPath, groupPath} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fields := []string{"S"}
	for range 18 {
		fields = append(fields, "0")
	}
	fields = append(fields, "100")
	for path, value := range map[string][]byte{
		filepath.Join(procPath, "stat"):   []byte("17 (synthetic control) " + strings.Join(fields, " ") + "\n"),
		filepath.Join(procPath, "cgroup"): []byte("0::" + managementPrivateCgroupPath + "/" + plan.UnitName + "\n"),
		filepath.Join(procPath, "status"): managementSshTestStatus(plan),
	} {
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range map[string]string{"memory.max": strconv.FormatInt(plan.MemoryBytes, 10), "memory.high": "max", "memory.swap.max": "0", "pids.max": strconv.FormatInt(plan.TasksMaximum, 10),
		"cgroup.type": "domain", "memory.oom.group": "1", "cpu.max": strconv.FormatInt(int64(settings.Agent.ManagementCpuQuotaPercent)*settings.Agent.ContainmentCpuPeriodMicros/100, 10) + " " + strconv.FormatInt(settings.Agent.ContainmentCpuPeriodMicros, 10),
		"cpuset.cpus": plan.ManagementCpuSet, "cpuset.cpus.effective": plan.ManagementCpuSet} {
		if err := os.WriteFile(filepath.Join(groupPath, key), []byte(value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	proc, err := openDirectoryNoFollow(procPath)
	if err != nil {
		t.Fatal(err)
	}
	group, err := openDirectoryNoFollow(groupPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.Close(); _ = group.Close() })
	return proc, group, procPath, groupPath
}

// A correct manager snapshot cannot hide wrong actual memory/task/CPU/cap or
// a process that migrates/changes birth between the two held observations.
func TestManagementSshHeldKernelProofRequiresActualCapsAndStableGeneration(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	plan := managementSshTestPlan(config, policy, settings)
	proc, group, _, _ := managementSshTestDescriptors(t, plan, settings)
	if err := verifyManagementSshDescriptorProfile(t.Context(), proc, group, plan, settings, 0, 0, nil); err != nil {
		t.Fatal("exact private descriptor profile was rejected", err)
	}
	for _, change := range []struct{ name, value string }{
		{name: "memory.max", value: "max\n"}, {name: "pids.max", value: "max\n"}, {name: "cpuset.cpus.effective", value: "0-4095\n"}, {name: "cpu.max", value: "max 100000\n"},
	} {
		proc, group, _, groupPath := managementSshTestDescriptors(t, plan, settings)
		err := verifyManagementSshDescriptorProfile(t.Context(), proc, group, plan, settings, 0, 0, func() {
			if err := os.WriteFile(filepath.Join(groupPath, change.name), []byte(change.value), 0600); err != nil {
				t.Fatal(err)
			}
		})
		if err == nil {
			t.Fatalf("correct manager state hid changed kernel %s", change.name)
		}
	}
	proc, group, procPath, _ := managementSshTestDescriptors(t, plan, settings)
	if err := verifyManagementSshDescriptorProfile(t.Context(), proc, group, plan, settings, 0, 0, func() {
		content, err := os.ReadFile(filepath.Join(procPath, "stat"))
		if err != nil {
			t.Fatal(err)
		}
		content = []byte(strings.Replace(string(content), " 100\n", " 101\n", 1))
		if err := os.WriteFile(filepath.Join(procPath, "stat"), content, 0600); err != nil {
			t.Fatal(err)
		}
	}); err == nil {
		t.Fatal("reused synthetic process birth retained admission")
	}
}

// Even an in-root relative symlink is not the held property's generation.
func TestManagementSshHeldKernelProofRejectsRelativeSymlinkAndCancellation(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	plan := managementSshTestPlan(config, policy, settings)
	proc, group, _, groupPath := managementSshTestDescriptors(t, plan, settings)
	if err := os.Rename(filepath.Join(groupPath, "memory.max"), filepath.Join(groupPath, "memory-retained")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("memory-retained", filepath.Join(groupPath, "memory.max")); err != nil {
		t.Fatal(err)
	}
	if err := verifyManagementSshDescriptorProfile(t.Context(), proc, group, plan, settings, 0, 0, nil); err == nil {
		t.Fatal("relative property symlink bypassed raw no-follow")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := verifyManagementSshDescriptorProfile(ctx, proc, group, plan, settings, 0, 0, nil); err == nil {
		t.Fatal("stopped proof owner continued native observation")
	}
}
