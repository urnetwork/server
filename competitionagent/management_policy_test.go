// Pure ledgers and inert templates prove no host admission or pressure result.
package competitionagent

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every fixture pin is synthetic; no installed file or live service is read.
func managementTestSlice(name, cgroup string, memory, tasks int64) ManagementSlicePolicy {
	return ManagementSlicePolicy{SliceName: name, CgroupPath: cgroup,
		Unit:        Pin{Path: "/etc/systemd/system/" + name, Sha256: strings.Repeat("a", 64)},
		MemoryBytes: memory, SwapBytes: 0, TasksMaximum: tasks}
}

// Constructing explicit fixture values does not introduce production defaults
// or a policy factory that could silently fill a missing protected role.
func managementTestPolicy(settings ManagementBoundarySettings) ManagementResourcePolicy {
	policy := ManagementResourcePolicy{Schema: Schema, Profile: ManagementResourcePolicyProfile,
		ReserveBytes: settings.ReserveBytes, ManagementCpuSet: "2,4",
		Parent: managementTestSlice(managementParentSliceName, managementParentCgroupPath, settings.ParentMemoryBytes, settings.ParentTasksMaximum)}
	for _, role := range []struct {
		role, name, cgroup string
		memory, tasks      int64
	}{
		{role: ManagementCoordinatorRole, name: managementCoordinatorSliceName, cgroup: managementCoordinatorCgroupPath,
			memory: settings.CoordinatorMemoryBytes, tasks: settings.CoordinatorTasksMaximum},
		{role: ManagementPrivateRole, name: managementPrivateSliceName, cgroup: managementPrivateCgroupPath,
			memory: settings.PrivateMemoryBytes, tasks: settings.PrivateTasksMaximum},
		{role: ManagementCleanupRole, name: managementCleanupSliceName, cgroup: managementCleanupCgroupPath,
			memory: settings.CleanupMemoryBytes, tasks: settings.CleanupTasksMaximum},
		{role: ManagementCoreRole, name: managementCoreSliceName, cgroup: managementCoreCgroupPath,
			memory: settings.CoreMemoryBytes, tasks: settings.CoreTasksMaximum},
	} {
		slice := managementTestSlice(role.name, role.cgroup, role.memory, role.tasks)
		policy.Roles = append(policy.Roles, ManagementRolePolicy{Role: role.role, SliceName: slice.SliceName, CgroupPath: slice.CgroupPath,
			Unit: slice.Unit, MemoryBytes: slice.MemoryBytes, SwapBytes: slice.SwapBytes, TasksMaximum: slice.TasksMaximum})
	}
	policy.Roles = append(policy.Roles, ManagementRolePolicy{Role: ManagementOsReservedRole, MemoryBytes: settings.OsReservedMemoryBytes})
	return policy
}

// The parent is counted as a constraint, not an additional reserve allocation.
func TestManagementPolicyPartitionsOriginalReserveOnce(t *testing.T) {
	settings := DefaultManagementBoundarySettings()
	policy := managementTestPolicy(settings)
	if err := ValidateManagementResourcePolicy(policy, settings); err != nil {
		t.Fatal(err)
	}
	total := int64(0)
	for _, role := range policy.Roles {
		total += role.MemoryBytes
	}
	if total != 24*1024*1024*1024 || policy.Parent.MemoryBytes != 16*1024*1024*1024 ||
		settings.CoordinatorMemoryBytes != 8*1024*1024*1024 || settings.PrivateMemoryBytes != 8*1024*1024*1024 ||
		settings.CleanupMemoryBytes != 2*1024*1024*1024 || settings.CoreMemoryBytes != 256*1024*1024 ||
		settings.OsReservedMemoryBytes != 5888*1024*1024 || settings.CoreTasksMaximum != 128 ||
		settings.ParentTasksMaximum != settings.CoordinatorTasksMaximum+settings.PrivateTasksMaximum {
		t.Fatal("approved reserve partition or existing core class changed", settings)
	}
	policy.Roles[0], policy.Roles[4] = policy.Roles[4], policy.Roles[0]
	if err := ValidateManagementResourcePolicy(policy, settings); err != nil {
		t.Fatal("explicit role ordering changed accounting", err)
	}
}

// Recovery must not share the ordinary management parent's binding limits.
func TestManagementPolicyKeepsRecoveryIndependent(t *testing.T) {
	settings := DefaultManagementBoundarySettings()
	policy := managementTestPolicy(settings)
	for _, role := range policy.Roles {
		inside := strings.HasPrefix(role.CgroupPath, policy.Parent.CgroupPath+"/")
		if role.Role == ManagementCoordinatorRole || role.Role == ManagementPrivateRole {
			if !inside {
				t.Fatal("ordinary aggregate lost its intended child", role.Role)
			}
		} else if inside {
			t.Fatal("independent reserve entered the ordinary binding ancestor", role.Role)
		}
	}
	policy.Roles[2].SliceName = "urnetwork-management-cleanup.slice"
	policy.Roles[2].CgroupPath = policy.Parent.CgroupPath + "/urnetwork-management-cleanup.slice"
	policy.Roles[2].Unit.Path = "/etc/systemd/system/urnetwork-management-cleanup.slice"
	if err := ValidateManagementResourcePolicy(policy, settings); err == nil {
		t.Fatal("cleanup beneath the ordinary OOM/task ancestor was accepted")
	}
}

// Missing, repeated or invented owners cannot silently receive defaults.
func TestManagementPolicyRequiresEveryExactRole(t *testing.T) {
	settings := DefaultManagementBoundarySettings()
	for _, change := range []struct {
		name  string
		apply func(*ManagementResourcePolicy)
	}{
		{name: "missing", apply: func(policy *ManagementResourcePolicy) { policy.Roles = policy.Roles[:4] }},
		{name: "duplicate", apply: func(policy *ManagementResourcePolicy) { policy.Roles[1] = policy.Roles[0] }},
		{name: "unknown", apply: func(policy *ManagementResourcePolicy) { policy.Roles[1].Role = "another-private-daemon" }},
		{name: "extra-private", apply: func(policy *ManagementResourcePolicy) { policy.Roles = append(policy.Roles, policy.Roles[1]) }},
		{name: "extra-os", apply: func(policy *ManagementResourcePolicy) { policy.Roles = append(policy.Roles, policy.Roles[4]) }},
	} {
		policy := managementTestPolicy(settings)
		change.apply(&policy)
		if err := ValidateManagementResourcePolicy(policy, settings); err == nil {
			t.Errorf("invalid role ledger accepted: %s", change.name)
		}
	}
}

// An additional coordinator allocation or per-daemon private allowance cannot
// be disguised as a larger reserve, a parent cap or an OS-reserve reduction.
func TestManagementPolicyRejectsMismatchedLimitsAndDoubleCounting(t *testing.T) {
	settings := DefaultManagementBoundarySettings()
	for _, change := range []struct {
		name  string
		apply func(*ManagementResourcePolicy)
	}{
		{name: "extra-coordinator", apply: func(policy *ManagementResourcePolicy) { policy.ReserveBytes += policy.Roles[0].MemoryBytes }},
		{name: "parent-in-sum", apply: func(policy *ManagementResourcePolicy) { policy.ReserveBytes += policy.Parent.MemoryBytes }},
		{name: "old-eighteen-parent", apply: func(policy *ManagementResourcePolicy) { policy.Parent.MemoryBytes += settings.CleanupMemoryBytes }},
		{name: "private-per-daemon", apply: func(policy *ManagementResourcePolicy) { policy.Roles[1].MemoryBytes *= 2 }},
		{name: "missing-os", apply: func(policy *ManagementResourcePolicy) { policy.Roles[4].MemoryBytes = 0 }},
		{name: "smaller-os", apply: func(policy *ManagementResourcePolicy) { policy.Roles[4].MemoryBytes-- }},
		{name: "swap", apply: func(policy *ManagementResourcePolicy) { policy.Roles[1].SwapBytes = 1 }},
		{name: "unbounded-tasks", apply: func(policy *ManagementResourcePolicy) { policy.Roles[2].TasksMaximum = 0 }},
		{name: "parent-recovery-tasks", apply: func(policy *ManagementResourcePolicy) { policy.Parent.TasksMaximum += settings.CleanupTasksMaximum }},
		{name: "core-class", apply: func(policy *ManagementResourcePolicy) { policy.Roles[3].TasksMaximum = 16 }},
		{name: "fictitious-os-unit", apply: func(policy *ManagementResourcePolicy) { policy.Roles[4].Unit = policy.Roles[0].Unit }},
		{name: "fictitious-os-tasks", apply: func(policy *ManagementResourcePolicy) { policy.Roles[4].TasksMaximum = 1 }},
	} {
		policy := managementTestPolicy(settings)
		change.apply(&policy)
		if err := ValidateManagementResourcePolicy(policy, settings); err == nil {
			t.Errorf("invalid resource accounting accepted: %s", change.name)
		}
	}
}

// Positive individual values cannot wrap into a small apparently valid total.
func TestManagementPolicyRefusesMemoryAndTaskOverflow(t *testing.T) {
	settings := DefaultManagementBoundarySettings()
	policy := managementTestPolicy(settings)
	policy.Roles[0].MemoryBytes = math.MaxInt64
	if err := ValidateManagementResourcePolicy(policy, settings); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatal("memory ledger did not explicitly refuse overflow", err)
	}
	settings.CoordinatorTasksMaximum = math.MaxInt64
	if err := ValidateManagementResourcePolicy(managementTestPolicy(settings), settings); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatal("task ledger did not explicitly refuse overflow", err)
	}
}

// Protected settings do not waive the approved profile or existing core class.
func TestManagementPolicyRefusesMissingAndRepartitionedSettings(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*ManagementBoundarySettings)
	}{
		{name: "zero", apply: func(settings *ManagementBoundarySettings) { *settings = ManagementBoundarySettings{} }},
		{name: "memory-repartition", apply: func(settings *ManagementBoundarySettings) {
			settings.CoordinatorMemoryBytes++
			settings.PrivateMemoryBytes--
		}},
		{name: "larger-reserve", apply: func(settings *ManagementBoundarySettings) { settings.ReserveBytes += settings.CoordinatorMemoryBytes }},
		{name: "negative-tasks", apply: func(settings *ManagementBoundarySettings) { settings.PrivateTasksMaximum = -1 }},
		{name: "missing-tasks", apply: func(settings *ManagementBoundarySettings) { settings.CleanupTasksMaximum = 0 }},
		{name: "wrong-task-sum", apply: func(settings *ManagementBoundarySettings) { settings.ParentTasksMaximum++ }},
		{name: "core-tasks", apply: func(settings *ManagementBoundarySettings) { settings.CoreTasksMaximum++ }},
		{name: "swap", apply: func(settings *ManagementBoundarySettings) { settings.SwapBytes = 1 }},
		{name: "missing-cpu-bound", apply: func(settings *ManagementBoundarySettings) { settings.MaximumCpuIndex = 0 }},
		{name: "unbounded-cpu-expansion", apply: func(settings *ManagementBoundarySettings) { settings.MaximumCpuIndex = math.MaxInt }},
	} {
		settings := DefaultManagementBoundarySettings()
		change.apply(&settings)
		if err := ValidateManagementResourcePolicy(managementTestPolicy(settings), settings); err == nil {
			t.Errorf("invalid owning settings accepted: %s", change.name)
		}
	}
}

// Index syntax and expansion are finite before any native check or rendering.
// Native admission still compares membership with the independent pinned helper.
func TestManagementPolicyRequiresExplicitFiniteManagementCpuSet(t *testing.T) {
	settings := DefaultManagementBoundarySettings()
	for _, invalid := range []string{"", "@MANAGEMENT_CPUSET@", "all", " 2,4", "2,4\n", "02,4", "2,2", "2-4,3", "4-2", "0-99999999999999999999999999999999999"} {
		policy := managementTestPolicy(settings)
		policy.ManagementCpuSet = invalid
		if err := ValidateManagementResourcePolicy(policy, settings); err == nil {
			t.Errorf("missing or malformed management CPU set accepted: %q", invalid)
		}
	}
	settings.MaximumCpuIndex = 3
	if err := ValidateManagementResourcePolicy(managementTestPolicy(settings), settings); err == nil {
		t.Fatal("management CPU set exceeded its owner's finite index bound")
	}
	policy := managementTestPolicy(settings)
	policy.ManagementCpuSet = "2-3"
	if err := ValidateManagementResourcePolicy(policy, settings); err != nil {
		t.Fatal("explicit bounded canonical range was refused", err)
	}
}

// Independent owner task settings must be matched explicitly by the ledger.
func TestManagementPolicyMatchesExplicitOwnerTaskSettings(t *testing.T) {
	settings := DefaultManagementBoundarySettings()
	settings.CoordinatorTasksMaximum, settings.PrivateTasksMaximum, settings.CleanupTasksMaximum = 257, 513, 129
	settings.ParentTasksMaximum = settings.CoordinatorTasksMaximum + settings.PrivateTasksMaximum
	policy := managementTestPolicy(settings)
	if err := ValidateManagementResourcePolicy(policy, settings); err != nil {
		t.Fatal("explicit finite owner task settings were ignored", err)
	}
	policy.Roles[1].TasksMaximum++
	if err := ValidateManagementResourcePolicy(policy, settings); err == nil {
		t.Fatal("policy expanded the owning private aggregate task budget")
	}
}

// Candidate/evaluation/build paths and aliases cannot replace fixed role names.
func TestManagementPolicyRefusesWrongSliceIdentitiesAndPins(t *testing.T) {
	settings := DefaultManagementBoundarySettings()
	for _, change := range []struct {
		name  string
		apply func(*ManagementResourcePolicy)
	}{
		{name: "missing-parent-pin", apply: func(policy *ManagementResourcePolicy) { policy.Parent.Unit = Pin{} }},
		{name: "relative", apply: func(policy *ManagementResourcePolicy) {
			policy.Roles[0].Unit.Path = "units/" + managementCoordinatorSliceName
		}},
		{name: "path-alias", apply: func(policy *ManagementResourcePolicy) {
			policy.Roles[0].Unit.Path = "/etc/systemd/system/../system/" + managementCoordinatorSliceName
		}},
		{name: "wrong-basename", apply: func(policy *ManagementResourcePolicy) { policy.Roles[0].Unit.Path = policy.Roles[1].Unit.Path }},
		{name: "missing-sha", apply: func(policy *ManagementResourcePolicy) { policy.Roles[1].Unit.Sha256 = "" }},
		{name: "noncanonical-sha", apply: func(policy *ManagementResourcePolicy) { policy.Roles[1].Unit.Sha256 = strings.Repeat("A", 64) }},
		{name: "wrong-core-path", apply: func(policy *ManagementResourcePolicy) {
			policy.Roles[3].CgroupPath = "/system.slice/urnetwork-core-handler.slice"
		}},
		{name: "evaluation-alias", apply: func(policy *ManagementResourcePolicy) { policy.Roles[1].SliceName = "urnetwork-evaluation.slice" }},
		{name: "build-alias", apply: func(policy *ManagementResourcePolicy) {
			policy.Roles[1].CgroupPath = "/urnetwork.slice/urnetwork-submission-build-synthetic.slice"
		}},
		{name: "parent-alias", apply: func(policy *ManagementResourcePolicy) {
			policy.Parent.CgroupPath = "/system.slice/urnetwork-management.slice"
		}},
	} {
		policy := managementTestPolicy(settings)
		change.apply(&policy)
		if err := ValidateManagementResourcePolicy(policy, settings); err == nil {
			t.Errorf("untrusted slice identity or unit pin accepted: %s", change.name)
		}
	}
}

// Zero values, unknown profiles and proof-waiver fields are never filled in.
func TestManagementPolicyRequiresExplicitProfileWithoutBooleanWaivers(t *testing.T) {
	settings := DefaultManagementBoundarySettings()
	for _, change := range []struct {
		name  string
		apply func(*ManagementResourcePolicy)
	}{
		{name: "missing-schema", apply: func(policy *ManagementResourcePolicy) { policy.Schema = 0 }},
		{name: "unknown-schema", apply: func(policy *ManagementResourcePolicy) { policy.Schema++ }},
		{name: "missing-profile", apply: func(policy *ManagementResourcePolicy) { policy.Profile = "" }},
		{name: "unknown-profile", apply: func(policy *ManagementResourcePolicy) { policy.Profile = "waived-native/v1" }},
	} {
		policy := managementTestPolicy(settings)
		change.apply(&policy)
		if err := ValidateManagementResourcePolicy(policy, settings); err == nil {
			t.Errorf("implicit or unsupported policy accepted: %s", change.name)
		}
	}
	encoded, err := json.Marshal(managementTestPolicy(settings))
	if err != nil {
		t.Fatal(err)
	}
	var decoded ManagementResourcePolicy
	if err := decodeStrict(encoded, &decoded); err != nil {
		t.Fatal("explicit protected policy did not round trip", err)
	}
	if err := ValidateManagementResourcePolicy(decoded, settings); err != nil {
		t.Fatal(err)
	}
	withWaiver := strings.TrimSuffix(string(encoded), "}") + ",\"proof_accepted\":true}"
	if err := decodeStrict([]byte(withWaiver), &decoded); err == nil {
		t.Fatal("a Boolean waiver entered the protected policy contract")
	}
}

// These source-only templates pin the intended aggregate, not live placement.
// All numeric rendering values come from the explicit owning settings.
func TestManagementSliceTemplatesMatchLedgerWithoutActivation(t *testing.T) {
	settings := DefaultManagementBoundarySettings()
	policy := managementTestPolicy(settings)
	replacements := map[string]string{
		"@PARENT_MEMORY_BYTES@":       strconv.FormatInt(settings.ParentMemoryBytes, 10),
		"@PARENT_TASKS_MAXIMUM@":      strconv.FormatInt(settings.ParentTasksMaximum, 10),
		"@COORDINATOR_MEMORY_BYTES@":  strconv.FormatInt(settings.CoordinatorMemoryBytes, 10),
		"@COORDINATOR_TASKS_MAXIMUM@": strconv.FormatInt(settings.CoordinatorTasksMaximum, 10),
		"@PRIVATE_MEMORY_BYTES@":      strconv.FormatInt(settings.PrivateMemoryBytes, 10),
		"@PRIVATE_TASKS_MAXIMUM@":     strconv.FormatInt(settings.PrivateTasksMaximum, 10),
		"@CLEANUP_MEMORY_BYTES@":      strconv.FormatInt(settings.CleanupMemoryBytes, 10),
		"@CLEANUP_TASKS_MAXIMUM@":     strconv.FormatInt(settings.CleanupTasksMaximum, 10),
		"@SWAP_BYTES@":                strconv.FormatInt(settings.SwapBytes, 10),
		"@MANAGEMENT_CPUSET@":         policy.ManagementCpuSet,
	}
	for _, expected := range []struct {
		name          string
		memory, tasks int64
	}{
		{name: managementParentSliceName, memory: settings.ParentMemoryBytes, tasks: settings.ParentTasksMaximum},
		{name: managementCoordinatorSliceName, memory: settings.CoordinatorMemoryBytes, tasks: settings.CoordinatorTasksMaximum},
		{name: managementPrivateSliceName, memory: settings.PrivateMemoryBytes, tasks: settings.PrivateTasksMaximum},
		{name: managementCleanupSliceName, memory: settings.CleanupMemoryBytes, tasks: settings.CleanupTasksMaximum},
	} {
		path := filepath.Join("..", "cli", "competitionsubeval", "management", expected.name+".in")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rendered := string(content)
		for token, value := range replacements {
			rendered = strings.ReplaceAll(rendered, token, value)
		}
		if strings.Contains(rendered, "@") || !strings.Contains(rendered, "[Slice]\n") {
			t.Fatal("slice template did not render explicitly", expected.name)
		}
		properties := map[string]string{}
		for _, line := range strings.Split(rendered, "\n") {
			if line == "" || strings.HasPrefix(line, "#") || line == "[Unit]" || line == "[Slice]" {
				continue
			}
			key, value, present := strings.Cut(line, "=")
			if !present || key == "" || value == "" || properties[key] != "" {
				t.Fatalf("template %s contains an ambiguous property %q", expected.name, line)
			}
			properties[key] = value
		}
		wanted := map[string]string{"MemoryAccounting": "yes", "MemoryMax": strconv.FormatInt(expected.memory, 10),
			"MemorySwapMax": "0", "TasksAccounting": "yes", "TasksMax": strconv.FormatInt(expected.tasks, 10), "AllowedCPUs": policy.ManagementCpuSet}
		if properties["Description"] == "" {
			t.Fatal("template lost its explicit role description", expected.name)
		}
		delete(properties, "Description")
		if len(properties) != len(wanted) {
			t.Fatal("template introduced activation, affinity or an unreviewed property", expected.name, properties)
		}
		for key, value := range wanted {
			if properties[key] != value {
				t.Fatal("template drifted from its explicit ledger", expected.name, key, properties[key], value)
			}
		}
	}
}
