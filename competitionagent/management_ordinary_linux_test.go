// Ordinary management proofs use synthetic cgroup files and private directory
// descriptors only. No test contacts PID1, procfs, a daemon or live sysfs.
package competitionagent

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// An explicit fixture ledger owns three caps and one private service child.
func managementOrdinaryTestFacts(t *testing.T) (ManagementResourcePolicy, AgentSettings, managementOrdinaryFacts) {
	t.Helper()
	policy := managementTestPolicy(DefaultManagementBoundarySettings())
	settings := DefaultAgentSettings()
	groups, err := managementOrdinarySlices(policy)
	if err != nil {
		t.Fatal(err)
	}
	before := managementOrdinarySnapshot{Groups: map[string]map[string]string{}, Children: map[string][]string{}}
	for _, group := range groups {
		before.Groups[group.CgroupPath] = map[string]string{"memory.max": strconv.FormatInt(group.MemoryBytes, 10), "memory.high": "max", "memory.swap.max": "0", "memory.current": "1024",
			"pids.max": strconv.FormatInt(group.TasksMaximum, 10), "pids.current": "1", "cpuset.cpus": policy.ManagementCpuSet, "cpuset.cpus.effective": policy.ManagementCpuSet,
			"cgroup.type": "domain", "memory.oom.group": "0", "cpu.max": "max 100000", "cgroup.procs": ""}
		before.Children[group.CgroupPath] = []string{}
	}
	children := []string{managementCoordinatorSliceName, managementPrivateSliceName}
	slices.Sort(children)
	before.Children[policy.Parent.CgroupPath] = children
	service := managementPrivateCgroupPath + "/synthetic-control.service"
	before.Groups[service] = map[string]string{"cpuset.cpus.effective": policy.ManagementCpuSet, "cgroup.type": "domain"}
	before.Children[managementPrivateCgroupPath] = []string{"synthetic-control.service"}
	before.Children[service] = []string{}
	after := managementOrdinarySnapshot{Groups: map[string]map[string]string{}, Children: map[string][]string{}}
	for path, values := range before.Groups {
		after.Groups[path] = maps.Clone(values)
		after.Children[path] = slices.Clone(before.Children[path])
	}
	common := map[string]string{"memory.max": "max", "memory.high": "max", "pids.max": "max", "cpu.max": "max 100000", "memory.oom.group": "0"}
	return policy, settings, managementOrdinaryFacts{Before: before, After: after, CommonBefore: common, CommonAfter: maps.Clone(common)}
}

// Every fake property is written beneath a private test root. The operation
// never copies an installed unit, process, filesystem or machine identity.
func writeManagementOrdinaryTestTree(t *testing.T, root string, facts managementOrdinaryFacts) {
	t.Helper()
	for path, values := range facts.Before.Groups {
		directory := filepath.Join(root, strings.TrimPrefix(path, "/"))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, value := range values {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(value+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, value := range facts.CommonBefore {
		if err := os.WriteFile(filepath.Join(root, "urnetwork.slice", name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Three exact ceilings prove one aggregate constraint, not another allocation.
func TestManagementOrdinaryRequiresParentAndBothChildCaps(t *testing.T) {
	policy, settings, facts := managementOrdinaryTestFacts(t)
	if err := validateManagementOrdinaryFacts(facts, policy, settings); err != nil {
		t.Fatal("valid synthetic aggregate rejected", err)
	}
	groups, err := managementOrdinarySlices(policy)
	if err != nil || len(groups) != 3 {
		t.Fatal("ordinary role set lost a cap", err)
	}
	for _, group := range groups {
		values := map[string]string{"Id": group.SliceName, "LoadState": "loaded", "ActiveState": "active", "FragmentPath": group.Unit.Path, "DropInPaths": "", "ControlGroup": group.CgroupPath,
			"MemoryMax": strconv.FormatInt(group.MemoryBytes, 10), "MemoryHigh": "infinity", "MemorySwapMax": "0", "TasksMax": strconv.FormatInt(group.TasksMaximum, 10), "AllowedCPUs": "2 4"}
		role := ManagementRolePolicy{SliceName: group.SliceName, CgroupPath: group.CgroupPath, Unit: group.Unit, MemoryBytes: group.MemoryBytes, TasksMaximum: group.TasksMaximum}
		if err := validateManagementRecoveryManager(runtimeBoundaryTestUnitBytes(values), policy, role, settings); err != nil {
			t.Fatal("valid synthetic PID1 aggregate rejected", err)
		}
	}
}

// Per-service limits do not supply the missing common cap. Each fixed role is
// checked independently rather than inferred from a matching document total.
func TestManagementOrdinaryRejectsUnboundedOrChangedCaps(t *testing.T) {
	for _, path := range []string{managementParentCgroupPath, managementCoordinatorCgroupPath, managementPrivateCgroupPath} {
		for _, change := range []struct{ key, value string }{{key: "memory.max", value: "max"}, {key: "memory.swap.max", value: "max"}, {key: "memory.high", value: "1024"},
			{key: "pids.max", value: "max"}, {key: "memory.oom.group", value: "1"}, {key: "cpu.max", value: "200000 100000"}, {key: "cpuset.cpus", value: ""}} {
			policy, settings, facts := managementOrdinaryTestFacts(t)
			facts.After.Groups[path][change.key] = change.value
			if err := validateManagementOrdinaryFacts(facts, policy, settings); err == nil {
				t.Fatal("partial or changed cap accepted", path, change.key)
			}
		}
	}
}

// Direct tasks bypass role accounting even when their parent remains finite;
// an additional sibling cannot silently share another ordinary allowance.
func TestManagementOrdinaryRejectsDirectTasksAndUnknownSiblings(t *testing.T) {
	for _, path := range []string{managementParentCgroupPath, managementCoordinatorCgroupPath, managementPrivateCgroupPath} {
		policy, settings, facts := managementOrdinaryTestFacts(t)
		facts.Before.Groups[path]["cgroup.procs"], facts.After.Groups[path]["cgroup.procs"] = "123", "123"
		if err := validateManagementOrdinaryFacts(facts, policy, settings); err == nil {
			t.Fatal("direct ordinary slice task escaped role closure", path)
		}
	}
	policy, settings, facts := managementOrdinaryTestFacts(t)
	facts.Before.Children[policy.Parent.CgroupPath] = append(facts.Before.Children[policy.Parent.CgroupPath], "synthetic-extra.slice")
	facts.After.Children[policy.Parent.CgroupPath] = slices.Clone(facts.Before.Children[policy.Parent.CgroupPath])
	if err := validateManagementOrdinaryFacts(facts, policy, settings); err == nil {
		t.Fatal("additional ordinary owner gained an allowance")
	}
}

// All service descendants inherit the management effective CPU boundary.
func TestManagementOrdinaryRejectsEscapedOrChangedDescendant(t *testing.T) {
	for _, change := range []struct{ key, value string }{{key: "cpuset.cpus.effective", value: "0-8"}, {key: "cgroup.type", value: "threaded"}} {
		policy, settings, facts := managementOrdinaryTestFacts(t)
		facts.After.Groups[managementPrivateCgroupPath+"/synthetic-control.service"][change.key] = change.value
		if err := validateManagementOrdinaryFacts(facts, policy, settings); err == nil {
			t.Fatal("private descendant changed its CPU/domain boundary", change.key)
		}
	}
	policy, settings, facts := managementOrdinaryTestFacts(t)
	facts.After.Children[managementPrivateCgroupPath] = []string{}
	if err := validateManagementOrdinaryFacts(facts, policy, settings); err == nil {
		t.Fatal("descendant generation changed between snapshots")
	}
}

// Canonical nonnegative usage and actual margins are required at each cap.
func TestManagementOrdinaryRequiresMeasuredRunnableMargins(t *testing.T) {
	for _, change := range []struct{ key, value string }{{key: "memory.current", value: "8589934592"}, {key: "memory.current", value: "-1"}, {key: "memory.current", value: "01"},
		{key: "pids.current", value: "16384"}, {key: "pids.current", value: "+1"}, {key: "pids.current", value: "9223372036854775808"}} {
		policy, settings, facts := managementOrdinaryTestFacts(t)
		facts.After.Groups[managementPrivateCgroupPath][change.key] = change.value
		if err := validateManagementOrdinaryFacts(facts, policy, settings); err == nil {
			t.Fatal("missing or noncanonical runnable headroom accepted", change.key, change.value)
		}
	}
}

// Independent cleanup cannot depend on the shared ordinary ancestor's quota,
// memory-high wait or OOM domain merely because its own slice is a sibling.
func TestManagementOrdinaryRejectsSharedRecoveryFailureDomain(t *testing.T) {
	for _, change := range []struct{ key, value string }{{key: "memory.max", value: "19327352832"}, {key: "memory.high", value: "1024"}, {key: "pids.max", value: "20480"},
		{key: "cpu.max", value: "200000 100000"}, {key: "memory.oom.group", value: "1"}} {
		policy, settings, facts := managementOrdinaryTestFacts(t)
		facts.CommonAfter[change.key] = change.value
		if err := validateManagementOrdinaryFacts(facts, policy, settings); err == nil {
			t.Fatal("ordinary ancestor can disable independent recovery", change.key)
		}
	}
}

// Private files exercise the actual descriptor walk. They cannot masquerade
// as native evidence even if every synthetic property has the expected value.
func TestManagementOrdinaryComposesPrivateHeldDescriptorTree(t *testing.T) {
	policy, settings, input := managementOrdinaryTestFacts(t)
	root := t.TempDir()
	writeManagementOrdinaryTestTree(t, root, input)
	facts, err := readManagementOrdinaryFacts(t.Context(), policy, settings, root, false, nil)
	if err != nil || validateManagementOrdinaryFacts(facts, policy, settings) != nil {
		t.Fatal("private ordinary descriptor composition failed", err)
	}
	if _, err := readManagementOrdinaryFacts(t.Context(), policy, settings, root, true, nil); err == nil {
		t.Fatal("private ordinary files became native kernel proof")
	}
}

// A confined relative symlink still violates a claimed leaf-inode read.
func TestManagementOrdinaryRejectsRelativeSymlinkProperty(t *testing.T) {
	policy, settings, input := managementOrdinaryTestFacts(t)
	root := t.TempDir()
	writeManagementOrdinaryTestTree(t, root, input)
	directory := filepath.Join(root, strings.TrimPrefix(managementPrivateCgroupPath, "/"))
	path := filepath.Join(directory, "memory.max")
	if err := os.Rename(path, filepath.Join(directory, "synthetic-copy")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("synthetic-copy", path); err != nil {
		t.Fatal(err)
	}
	if _, err := readManagementOrdinaryFacts(t.Context(), policy, settings, root, false, nil); err == nil {
		t.Fatal("confined relative alias supplied ordinary authority")
	}
}

// The actual visible hierarchy is substituted while the first generation's
// descriptors remain held; matching names and bytes cannot renew those FDs.
func TestManagementOrdinaryRejectsPathGenerationSubstitution(t *testing.T) {
	policy, settings, input := managementOrdinaryTestFacts(t)
	root := t.TempDir()
	writeManagementOrdinaryTestTree(t, root, input)
	path := filepath.Join(root, strings.TrimPrefix(policy.Parent.CgroupPath, "/"))
	_, err := readManagementOrdinaryFacts(t.Context(), policy, settings, root, false, func() {
		if err := os.Rename(path, path+".retired"); err != nil {
			t.Fatal(err)
		}
		writeManagementOrdinaryTestTree(t, root, input)
	})
	if err == nil || !strings.Contains(err.Error(), "generation changed") {
		t.Fatal("visible replacement hierarchy renewed held authority", err)
	}
}

// A bounded walk refuses depth/count exhaustion or cancellation before any
// alternate hierarchy can be interpreted as an observation of this owner.
func TestManagementOrdinaryBoundsInventoriesAndCanceledReads(t *testing.T) {
	policy, settings, input := managementOrdinaryTestFacts(t)
	root := t.TempDir()
	writeManagementOrdinaryTestTree(t, root, input)
	for _, change := range []func(*AgentSettings){
		func(value *AgentSettings) { value.CleanupObjectCount = 2 },
		func(value *AgentSettings) { value.ManagementGroupDepth = 1 },
		func(value *AgentSettings) { value.ManagementMemoryHeadroomBytes = 0 },
		func(value *AgentSettings) { value.ManagementTasksHeadroom = 0 },
	} {
		current := settings
		change(&current)
		if _, err := readManagementOrdinaryFacts(t.Context(), policy, current, root, false, nil); err == nil {
			t.Fatal("invalid finite ordinary observation bound was ignored")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readManagementOrdinaryFacts(ctx, policy, settings, root, false, nil); err == nil {
		t.Fatal("canceled ordinary native owner read its hierarchy")
	}
}
