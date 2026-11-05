// Recovery observations use private regular-file hierarchies and synthetic
// manager rows only. No test queries systemd, real proc/cgroups or a daemon.
package competitionagent

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Every native-looking property is synthetic data, never a host snapshot.
func managementRecoveryTestFacts(current bool) (ManagementResourcePolicy, ManagementRolePolicy, AgentSettings, managementRecoveryFacts) {
	policy := managementTestPolicy(DefaultManagementBoundarySettings())
	role, _ := managementPolicyRole(&policy, ManagementCleanupRole)
	settings := DefaultAgentSettings()
	limits := map[string]string{"memory.max": strconv.FormatInt(role.MemoryBytes, 10), "memory.high": "max", "memory.swap.max": "0", "memory.current": "1024",
		"pids.max": strconv.FormatInt(role.TasksMaximum, 10), "pids.current": "0", "cpuset.cpus": policy.ManagementCpuSet, "cpuset.cpus.effective": policy.ManagementCpuSet, "cgroup.type": "domain"}
	ancestor := map[string]string{"memory.max": "max", "memory.high": "max", "pids.max": "max", "cpu.max": "max 100000", "memory.oom.group": "0"}
	facts := managementRecoveryFacts{Before: limits, After: maps.Clone(limits), Ancestors: map[string]map[string]string{"/urnetwork.slice": ancestor},
		AncestorsAfter: map[string]map[string]string{"/urnetwork.slice": maps.Clone(ancestor)}, ChildrenBefore: []string{}, ChildrenAfter: []string{}}
	if current {
		facts.Before["pids.current"], facts.After["pids.current"] = "1", "1"
		facts.ChildrenBefore, facts.ChildrenAfter = []string{managementRecoveryServiceName}, []string{managementRecoveryServiceName}
		facts.ProcessStartBefore, facts.ProcessStartAfter = 123, 123
		facts.ProcessCgroupBefore, facts.ProcessCgroupAfter = "0::"+role.CgroupPath+"/"+managementRecoveryServiceName, "0::"+role.CgroupPath+"/"+managementRecoveryServiceName
		facts.ProcessStatusBefore, facts.ProcessStatusAfter = []byte("Cpus_allowed_list:\t2,4\n"), []byte("Cpus_allowed_list:\t2,4\n")
	}
	return policy, role, settings, facts
}

// The fake filesystem has the exact descriptor-relative paths, not aliases to
// actual sysfs/proc. Own-PID naming is computed only for this test process.
func managementRecoveryTestTree(t *testing.T, current bool) (ManagementResourcePolicy, ManagementRolePolicy, AgentSettings, string, string, string) {
	t.Helper()
	policy, role, settings, facts := managementRecoveryTestFacts(current)
	root := t.TempDir()
	cgroupRoot, procRoot := filepath.Join(root, "cgroup"), filepath.Join(root, "proc")
	group := filepath.Join(cgroupRoot, strings.TrimPrefix(role.CgroupPath, "/"))
	if err := os.MkdirAll(group, 0700); err != nil {
		t.Fatal(err)
	}
	for path, values := range map[string]map[string]string{group: facts.Before, filepath.Dir(group): facts.Ancestors["/urnetwork.slice"]} {
		for name, value := range values {
			if err := os.WriteFile(filepath.Join(path, name), []byte(value+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if current {
		if err := os.Mkdir(filepath.Join(group, managementRecoveryServiceName), 0700); err != nil {
			t.Fatal(err)
		}
		process := filepath.Join(procRoot, strconv.Itoa(os.Getpid()))
		if err := os.MkdirAll(process, 0700); err != nil {
			t.Fatal(err)
		}
		fields := []string{"S"}
		for range 18 {
			fields = append(fields, "0")
		}
		fields = append(fields, strconv.FormatUint(facts.ProcessStartBefore, 10))
		for name, content := range map[string][]byte{"stat": []byte(strconv.Itoa(os.Getpid()) + " (synthetic recovery) " + strings.Join(fields, " ") + "\n"),
			"status": facts.ProcessStatusBefore, "cgroup": []byte(facts.ProcessCgroupBefore + "\n")} {
			if err := os.WriteFile(filepath.Join(process, name), content, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return policy, role, settings, cgroupRoot, procRoot, group
}

// A structurally correct policy is decoded only from its actual pinned bytes.
func TestManagementNativePolicyLoaderAuthenticatesDecodedDocument(t *testing.T) {
	policy := managementTestPolicy(DefaultManagementBoundarySettings())
	content, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "management.json")
	if err := os.WriteFile(path, content, 0400); err != nil {
		t.Fatal(err)
	}
	pin := testAgentPin(t, path)
	loaded, err := loadManagementResourcePolicy(pin, DefaultManagementBoundarySettings(), DefaultAgentSettings().ConfigBytes, false)
	if err != nil || loaded.Profile != policy.Profile || loaded.ManagementCpuSet != policy.ManagementCpuSet {
		t.Fatal("valid synthetic pinned ledger was not decoded", err)
	}
	for _, value := range []string{strings.Replace(string(content), `"schema":1`, `"schema":1,"Schema":1`, 1), string(content) + "{}"} {
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0400); err != nil {
			t.Fatal(err)
		}
		badPin := testAgentPin(t, path)
		if _, err := loadManagementResourcePolicy(badPin, DefaultManagementBoundarySettings(), DefaultAgentSettings().ConfigBytes, false); err == nil {
			t.Fatal("authenticated ambiguous or trailing ledger gained authority")
		}
	}
	if _, err := loadManagementResourcePolicy(pin, DefaultManagementBoundarySettings(), DefaultAgentSettings().ConfigBytes, false); err == nil {
		t.Fatal("old content pin authenticated changed decoded policy")
	}
}

// Even positive fake kernel facts are only test observations, never native
// readiness. Both idle arming and the current independent process are checked.
func TestManagementNativeRecoveryReadsPrivateDescriptorTree(t *testing.T) {
	for _, current := range []bool{false, true} {
		policy, role, settings, cgroupRoot, procRoot, _ := managementRecoveryTestTree(t, current)
		facts, err := readManagementRecoveryFacts(t.Context(), role, settings, cgroupRoot, procRoot, false, current)
		if err != nil || validateManagementRecoveryFacts(facts, policy, role, settings, current) != nil {
			t.Fatal("valid private descriptor observations did not compose", current, err)
		}
		if _, err := readManagementRecoveryFacts(t.Context(), role, settings, cgroupRoot, procRoot, true, current); err == nil {
			t.Fatal("regular test files masqueraded as native cgroup-v2 evidence")
		}
	}
}

// Manager limits cannot be omitted, inherited, aliased or duplicated simply
// because a pinned on-disk ledger looks correct.
func TestManagementNativeRecoveryManagerRejectsPartialOrWrongRole(t *testing.T) {
	policy, role, settings, _ := managementRecoveryTestFacts(false)
	valid := map[string]string{"Id": role.SliceName, "LoadState": "loaded", "ActiveState": "active", "FragmentPath": role.Unit.Path, "DropInPaths": "",
		"ControlGroup": role.CgroupPath, "MemoryMax": strconv.FormatInt(role.MemoryBytes, 10), "MemoryHigh": "infinity", "MemorySwapMax": "0",
		"TasksMax": strconv.FormatInt(role.TasksMaximum, 10), "AllowedCPUs": "2 4"}
	if err := validateManagementRecoveryManager(runtimeBoundaryTestUnitBytes(valid), policy, role, settings); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]string){
		func(values map[string]string) { delete(values, "MemoryHigh") },
		func(values map[string]string) { values["MemoryMax"] = "infinity" },
		func(values map[string]string) { values["ControlGroup"] = managementPrivateCgroupPath },
		func(values map[string]string) { values["DropInPaths"] = "/synthetic/override.conf" },
		func(values map[string]string) { values["AllowedCPUs"] = "0-4" },
		func(values map[string]string) { values["Unknown"] = "another-owner" },
	} {
		values := maps.Clone(valid)
		change(values)
		if err := validateManagementRecoveryManager(runtimeBoundaryTestUnitBytes(values), policy, role, settings); err == nil {
			t.Fatal("partial or different loaded manager role was admitted")
		}
	}
	duplicate := append(runtimeBoundaryTestUnitBytes(valid), []byte("MemoryMax="+valid["MemoryMax"]+"\n")...)
	if err := validateManagementRecoveryManager(duplicate, policy, role, settings); err == nil {
		t.Fatal("duplicate loaded manager property was admitted")
	}
}

// A common parent must not couple recovery to ordinary OOM, task exhaustion,
// memory-high throttling or CPU quota. A late changed snapshot also refuses.
func TestManagementNativeRecoveryRejectsBindingSharedAncestors(t *testing.T) {
	for _, change := range []struct{ key, value string }{
		{key: "memory.max", value: "19327352832"}, {key: "memory.high", value: "1"}, {key: "pids.max", value: "20480"},
		{key: "cpu.max", value: "200000 100000"}, {key: "cpu.max", value: ""}, {key: "cpu.max", value: "max"},
		{key: "cpu.max", value: "max 0"}, {key: "memory.oom.group", value: "1"},
	} {
		policy, role, settings, facts := managementRecoveryTestFacts(false)
		facts.Ancestors["/urnetwork.slice"][change.key] = change.value
		facts.AncestorsAfter["/urnetwork.slice"][change.key] = change.value
		if err := validateManagementRecoveryFacts(facts, policy, role, settings, false); err == nil {
			t.Fatal("shared ordinary failure domain was admitted", change.key)
		}
	}
	policy, role, settings, facts := managementRecoveryTestFacts(false)
	facts.AncestorsAfter["/urnetwork.slice"]["memory.max"] = "1"
	if err := validateManagementRecoveryFacts(facts, policy, role, settings, false); err == nil {
		t.Fatal("common ancestor changed after its first snapshot")
	}
}

// Current pids or near-exhausted independent capacity cannot be hidden by the
// fact that recovery is outside the ordinary parent's ancestry.
func TestManagementNativeRecoveryRequiresRunnableHeadroom(t *testing.T) {
	for _, change := range []struct{ key, value string }{
		{key: "memory.current", value: "2147483647"}, {key: "memory.current", value: "-1"}, {key: "memory.current", value: "+1"},
		{key: "pids.current", value: "1"}, {key: "pids.current", value: "01"}, {key: "pids.current", value: "1024"},
		{key: "memory.max", value: "max"}, {key: "memory.swap.max", value: "1"}, {key: "cpuset.cpus.effective", value: "0-4"},
	} {
		policy, role, settings, facts := managementRecoveryTestFacts(false)
		facts.After[change.key] = change.value
		if err := validateManagementRecoveryFacts(facts, policy, role, settings, false); err == nil {
			t.Fatal("missing cleanup headroom or changed cap was admitted", change.key)
		}
	}
	policy, role, settings, facts := managementRecoveryTestFacts(false)
	settings.RecoveryMemoryHeadroomBytes = 0
	if err := validateManagementRecoveryFacts(facts, policy, role, settings, false); err == nil {
		t.Fatal("disabled recovery headroom bound was admitted")
	}
}

// A running process must prove its exact service, stable birth, both cgroup
// observations and actual affinity; a valid slice's cap alone is insufficient.
func TestManagementNativeRecoveryRejectsMovedOrUnownedProcess(t *testing.T) {
	for _, change := range []func(*managementRecoveryFacts){
		func(facts *managementRecoveryFacts) { facts.ProcessStartAfter++ },
		func(facts *managementRecoveryFacts) {
			facts.ProcessCgroupBefore = "0::" + managementPrivateCgroupPath + "/synthetic.service"
		},
		func(facts *managementRecoveryFacts) { facts.ProcessCgroupAfter += "-other" },
		func(facts *managementRecoveryFacts) { facts.ProcessStatusAfter = []byte("Cpus_allowed_list:\t0-4\n") },
		func(facts *managementRecoveryFacts) {
			facts.ProcessStatusAfter = []byte("Cpus_allowed_list:\t2,4\nCpus_allowed_list:\t2,4\n")
		},
		func(facts *managementRecoveryFacts) { facts.ChildrenAfter = []string{} },
		func(facts *managementRecoveryFacts) { facts.After["pids.current"] = "0" },
	} {
		policy, role, settings, facts := managementRecoveryTestFacts(true)
		change(&facts)
		if err := validateManagementRecoveryFacts(facts, policy, role, settings, true); err == nil {
			t.Fatal("recovery process custody changed without refusal")
		}
	}
}

// Empty foreign groups also violate closure. No process count or filesystem
// directory may silently become generic privileged recovery authority.
func TestManagementNativeRecoveryRejectsUntrackedDescendants(t *testing.T) {
	for _, nested := range []bool{false, true} {
		_, role, settings, cgroupRoot, procRoot, group := managementRecoveryTestTree(t, false)
		extra := filepath.Join(group, "synthetic-foreign.service")
		if nested {
			extra = filepath.Join(group, managementRecoveryServiceName, "synthetic-child")
		}
		if err := os.MkdirAll(extra, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := readManagementRecoveryFacts(t.Context(), role, settings, cgroupRoot, procRoot, false, false); err == nil {
			t.Fatal("foreign empty cgroup was accepted as tracked recovery", nested)
		}
	}
}

// Raw descriptor display names are not filesystem authority. Info() from a
// lazy DirEntry must not reopen a made-up relative parent name in the cwd.
func TestManagementNativeRecoveryInventoryUsesHeldDescriptor(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, managementRecoveryServiceName), 0700); err != nil {
		t.Fatal(err)
	}
	parent, err := openDirectoryNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	directory, err := openRelativeNoFollow(parent, ".", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	children, err := readManagementRecoveryChildren(directory, DefaultAgentSettings().CleanupObjectCount)
	if err != nil || len(children) != 1 || children[0] != managementRecoveryServiceName {
		t.Fatal("descriptor inventory reopened its non-authoritative display name", err)
	}
}

// A real but false display path can hide an actual foreign cgroup directory as
// a regular file. Classification must consume descriptor-relative metadata.
func TestManagementNativeRecoveryInventoryRejectsDisplayPathSubstitution(t *testing.T) {
	root := t.TempDir()
	actual, display := filepath.Join(root, "actual"), filepath.Join(root, "misleading-display")
	foreign := "synthetic-foreign.service"
	if err := os.MkdirAll(filepath.Join(actual, foreign), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(display, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(display, foreign), []byte("not a directory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(actual, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	directory := os.NewFile(uintptr(fd), display)
	defer directory.Close()
	policy, role, settings, facts := managementRecoveryTestFacts(false)
	children, err := readManagementRecoveryChildren(directory, settings.CleanupObjectCount)
	if err != nil || len(children) != 1 || children[0] != foreign {
		t.Fatal("real display-path substitute hid the actual foreign directory", err)
	}
	facts.ChildrenBefore, facts.ChildrenAfter = children, append([]string(nil), children...)
	if err := validateManagementRecoveryFacts(facts, policy, role, settings, false); err == nil {
		t.Fatal("display-path metadata admitted an untracked owner into recovery")
	}
}

// Both a confined relative leaf and an ancestor alias are refused at the
// actual kernel-property consumer, not merely by a configuration linter.
func TestManagementNativeRecoveryPropertyAliasesAreRejected(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		_, role, settings, cgroupRoot, procRoot, group := managementRecoveryTestTree(t, false)
		if ancestor {
			original := filepath.Join(cgroupRoot, "urnetwork.slice")
			retained := filepath.Join(cgroupRoot, "retained-synthetic-parent")
			if err := os.Rename(original, retained); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(retained), original); err != nil {
				t.Fatal(err)
			}
		} else {
			path := filepath.Join(group, "memory.max")
			if err := os.Rename(path, filepath.Join(group, "retained-value")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("retained-value", path); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := readManagementRecoveryFacts(t.Context(), role, settings, cgroupRoot, procRoot, false, false); err == nil {
			t.Fatal("aliased native recovery evidence was consumed", ancestor)
		}
	}
}

// The held descriptor cannot authorize a new path generation that appears at
// the same cleanup name while observations were being consumed.
func TestManagementNativeRecoveryCgroupGenerationSwapIsRejected(t *testing.T) {
	_, role, settings, cgroupRoot, procRoot, group := managementRecoveryTestTree(t, false)
	called := false
	_, err := readManagementRecoveryFactsWithHook(t.Context(), role, settings, cgroupRoot, procRoot, false, false, func() {
		called = true
		if err := os.Rename(group, group+"-retained"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(group, 0700); err != nil {
			t.Fatal(err)
		}
	})
	if !called || err == nil {
		t.Fatal("cleanup directory replacement inherited held-inode authority", err)
	}
}

// A finite read can be canceled without consulting any actual kernel root.
func TestManagementNativeRecoveryCanceledReadHasNoHostFallback(t *testing.T) {
	_, role, settings, cgroupRoot, procRoot, _ := managementRecoveryTestTree(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readManagementRecoveryFacts(ctx, role, settings, cgroupRoot, procRoot, false, false); err == nil {
		t.Fatal("canceled evidence read acquired fresh recovery authority")
	}
}
