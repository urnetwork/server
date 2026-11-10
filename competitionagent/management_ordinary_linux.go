// The ordinary sixteen-GiB aggregate is observed in PID1 and the held native
// hierarchy. Independent recovery and ambient host pressure remain separate.
package competitionagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Only these three fixed slices divide ordinary ownership. A fourth sibling
// cannot acquire another allowance merely by using a plausible unit name.
func managementOrdinarySlices(policy ManagementResourcePolicy) ([]ManagementSlicePolicy, error) {
	if ValidateManagementResourcePolicy(policy, DefaultManagementBoundarySettings()) != nil {
		return nil, errors.New("ordinary management has no exact protected resource partition")
	}
	groups := []ManagementSlicePolicy{policy.Parent}
	for _, name := range []string{ManagementCoordinatorRole, ManagementPrivateRole} {
		role, err := managementPolicyRole(&policy, name)
		if err != nil {
			return nil, err
		}
		groups = append(groups, ManagementSlicePolicy{SliceName: role.SliceName, CgroupPath: role.CgroupPath, Unit: role.Unit,
			MemoryBytes: role.MemoryBytes, SwapBytes: role.SwapBytes, TasksMaximum: role.TasksMaximum})
	}
	return groups, nil
}

// This entrypoint has no caller-selected filesystem, process or Boolean waiver.
// It reads caps/closure before launch; it neither reparents nor stops anything.
func verifyManagementOrdinaryBoundary(parent context.Context, policy *ManagementResourcePolicy, systemctl Pin, settings AgentSettings) error {
	if policy == nil || !managementOrdinarySettingsValid(settings) {
		return errors.New("ordinary management lacks finite observation settings")
	}
	ctx, cancel := context.WithTimeout(parent, settings.ControlTimeout)
	defer cancel()
	groups, err := managementOrdinarySlices(*policy)
	if err != nil {
		return err
	}
	commandSettings := DefaultRuntimeResourceBoundarySettings()
	commandSettings.DocumentBytes, commandSettings.NativeBytes, commandSettings.CommandTimeout = settings.ConfigBytes, settings.DiagnosticBytes, settings.ControlTimeout
	properties := "Id,LoadState,ActiveState,FragmentPath,DropInPaths,ControlGroup,MemoryMax,MemoryHigh,MemorySwapMax,TasksMax,AllowedCPUs"
	for _, group := range groups {
		if err := validateFilePin(group.Unit, settings.ConfigBytes, true); err != nil {
			return err
		}
		output, err := runtimeBoundaryRunCommand(ctx, systemctl, []string{"show", "--no-pager", "--property=" + properties, "--", group.SliceName}, nil, commandSettings)
		role := ManagementRolePolicy{SliceName: group.SliceName, CgroupPath: group.CgroupPath, Unit: group.Unit,
			MemoryBytes: group.MemoryBytes, SwapBytes: group.SwapBytes, TasksMaximum: group.TasksMaximum}
		if err != nil || validateManagementRecoveryManager(output, *policy, role, settings) != nil {
			return errors.New("PID1 ordinary aggregate differs from its pinned role")
		}
	}
	facts, err := readManagementOrdinaryFacts(ctx, *policy, settings, "/sys/fs/cgroup", true, nil)
	if err != nil {
		return err
	}
	if err := validateManagementOrdinaryFacts(facts, *policy, settings); err != nil {
		return err
	}
	for _, group := range groups {
		if err := validateFilePin(group.Unit, settings.ConfigBytes, true); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Owner-local bounds limit total examined nodes and retained descriptors, not
// just the number of matching units returned by a recursive inventory.
func managementOrdinarySettingsValid(settings AgentSettings) bool {
	return settings.ControlTimeout > 0 && settings.ConfigBytes > 0 && settings.DiagnosticBytes > 0 && settings.CleanupObjectCount > 0 &&
		settings.ManagementGroupDepth > 0 && settings.ManagementMemoryHeadroomBytes > 0 && settings.ManagementTasksHeadroom > 0
}

// Snapshots retain only finite cgroup names/properties, never command lines or
// process environment. Dynamic service descendants share the same aggregate.
type managementOrdinarySnapshot struct {
	Groups   map[string]map[string]string
	Children map[string][]string
}

type managementOrdinaryFacts struct {
	Before, After             managementOrdinarySnapshot
	CommonBefore, CommonAfter map[string]string
}

// Fixed slice fields prove aggregate caps. All descendants additionally prove
// their effective CPU membership, so a scoped child cannot silently widen it.
func managementOrdinaryPropertyNames() []string {
	return []string{"memory.max", "memory.high", "memory.swap.max", "memory.current", "pids.max", "pids.current", "cpuset.cpus", "cpuset.cpus.effective", "cgroup.type", "memory.oom.group", "cpu.max", "cgroup.procs"}
}

// The private hook forces actual pathname-generation substitution in tests.
// Native callers supply only fixed sysfs and a nil hook, requiring cgroup v2.
func readManagementOrdinaryFacts(ctx context.Context, policy ManagementResourcePolicy, settings AgentSettings, rootPath string, requireKernel bool, beforeRecheck func()) (managementOrdinaryFacts, error) {
	facts := managementOrdinaryFacts{}
	if !managementOrdinarySettingsValid(settings) {
		return facts, errors.New("ordinary hierarchy has no finite observation owner")
	}
	groups, err := managementOrdinarySlices(policy)
	if err != nil {
		return facts, err
	}
	root, err := openDirectoryNoFollow(rootPath)
	if err != nil {
		return facts, err
	}
	defer root.Close()
	if requireKernel {
		var filesystem syscall.Statfs_t
		if syscall.Fstatfs(int(root.Fd()), &filesystem) != nil || filesystem.Type != 0x63677270 {
			return facts, errors.New("ordinary hierarchy is not native cgroup v2")
		}
	}
	held := map[string]*os.File{}
	defer func() {
		for _, file := range held {
			_ = file.Close()
		}
	}()
	commonPath := filepath.Dir(policy.Parent.CgroupPath)
	if commonPath != "/urnetwork.slice" {
		return facts, errors.New("ordinary management has an unexpected common ancestor")
	}
	common, err := openRelativeNoFollow(root, strings.TrimPrefix(commonPath, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return facts, err
	}
	defer common.Close()
	commonInfo, err := common.Stat()
	if err != nil {
		return facts, err
	}
	readCommon := func() (map[string]string, error) {
		values := map[string]string{}
		for _, name := range []string{"memory.max", "memory.high", "pids.max", "cpu.max", "memory.oom.group"} {
			value, err := readManagementKernelProperty(ctx, common, name, settings.DiagnosticBytes, requireKernel)
			if err != nil {
				return nil, err
			}
			values[name] = value
		}
		return values, nil
	}
	facts.CommonBefore, err = readCommon()
	if err != nil {
		return facts, err
	}
	fixed := map[string]bool{}
	for _, group := range groups {
		fixed[group.CgroupPath] = true
	}
	readSnapshot := func(discover bool) (managementOrdinarySnapshot, error) {
		snapshot := managementOrdinarySnapshot{Groups: map[string]map[string]string{}, Children: map[string][]string{}}
		queue := []string{policy.Parent.CgroupPath}
		for len(queue) != 0 {
			path := queue[0]
			queue = queue[1:]
			if err := ctx.Err(); err != nil {
				return snapshot, err
			}
			if len(snapshot.Groups) >= settings.CleanupObjectCount || strings.Count(strings.TrimPrefix(path, policy.Parent.CgroupPath), "/") > settings.ManagementGroupDepth {
				return snapshot, errors.New("ordinary hierarchy exceeds its finite inventory or depth")
			}
			current, err := openRelativeNoFollow(root, strings.TrimPrefix(path, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
			if err != nil {
				return snapshot, err
			}
			if discover {
				held[path] = current
			} else {
				previous := held[path]
				if previous == nil {
					_ = current.Close()
					return snapshot, errors.New("ordinary hierarchy gained an unobserved descendant")
				}
				beforeInfo, beforeErr := previous.Stat()
				afterInfo, afterErr := current.Stat()
				if beforeErr != nil || afterErr != nil || !os.SameFile(beforeInfo, afterInfo) {
					_ = current.Close()
					return snapshot, errors.New("ordinary cgroup generation changed")
				}
			}
			values := map[string]string{}
			names := []string{"cpuset.cpus.effective", "cgroup.type"}
			if fixed[path] {
				names = managementOrdinaryPropertyNames()
			}
			for _, name := range names {
				value, readErr := readManagementKernelProperty(ctx, current, name, settings.DiagnosticBytes, requireKernel)
				if errors.Is(readErr, os.ErrNotExist) && name == "cpuset.cpus.effective" && !fixed[path] {
					// A delegated service can have children without their own
					// cpuset controller. Prove inheritance through held parents,
					// rather than treating an absent property as an empty CPU set.
					parentPath := filepath.Dir(path)
					parentValues := snapshot.Groups[parentPath]
					parent := held[parentPath]
					if parent == nil || parentValues == nil {
						readErr = errors.New("ordinary cpuset inheritance has no observed parent")
					} else {
						var parentControllers, childControllers string
						parentControllers, readErr = readManagementKernelProperty(ctx, parent, "cgroup.subtree_control", settings.DiagnosticBytes, requireKernel)
						if readErr == nil {
							childControllers, readErr = readManagementKernelProperty(ctx, current, "cgroup.controllers", settings.DiagnosticBytes, requireKernel)
						}
						if readErr == nil && (!managementCpusetDisabled(parentControllers) || !managementCpusetDisabled(childControllers)) {
							readErr = errors.New("ordinary missing cpuset has no disabled-controller proof")
						}
						if readErr == nil {
							value = parentValues[name]
							values["cpuset.inherited_from"] = parentPath
							values["cpuset.parent_subtree_control"] = parentControllers
							values["cpuset.controllers"] = childControllers
						}
					}
				}
				if readErr != nil {
					if !discover {
						_ = current.Close()
					}
					return snapshot, readErr
				}
				values[name] = value
			}
			children, readErr := readManagementRecoveryChildren(current, settings.CleanupObjectCount-len(snapshot.Groups))
			if !discover {
				_ = current.Close()
			}
			if readErr != nil {
				return snapshot, readErr
			}
			snapshot.Groups[path], snapshot.Children[path] = values, children
			for _, name := range children {
				if len(queue) >= settings.CleanupObjectCount-len(snapshot.Groups) {
					return snapshot, errors.New("ordinary descendants exceed the total examined-node bound")
				}
				queue = append(queue, path+"/"+name)
			}
		}
		return snapshot, nil
	}
	facts.Before, err = readSnapshot(true)
	if err != nil {
		return facts, err
	}
	if beforeRecheck != nil {
		beforeRecheck()
	}
	facts.After, err = readSnapshot(false)
	if err != nil {
		return facts, err
	}
	currentCommon, err := openRelativeNoFollow(root, strings.TrimPrefix(commonPath, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return facts, err
	}
	currentInfo, statErr := currentCommon.Stat()
	_ = currentCommon.Close()
	if statErr != nil || !os.SameFile(commonInfo, currentInfo) {
		return facts, errors.New("ordinary common ancestor generation changed")
	}
	facts.CommonAfter, err = readCommon()
	return facts, err
}

// Both independent role ceilings and their shared sixteen-GiB parent must be
// real. Current headroom is an admission observation, not future peak proof.
func validateManagementOrdinaryFacts(facts managementOrdinaryFacts, policy ManagementResourcePolicy, settings AgentSettings) error {
	groups, err := managementOrdinarySlices(policy)
	if err != nil || !managementOrdinarySettingsValid(settings) {
		return errors.New("ordinary observations have no exact role/settings owner")
	}
	expectedChildren := []string{managementCoordinatorSliceName, managementPrivateSliceName}
	slices.Sort(expectedChildren)
	if !reflect.DeepEqual(facts.CommonBefore, facts.CommonAfter) || len(facts.CommonBefore) != 5 ||
		facts.CommonBefore["memory.max"] != "max" || facts.CommonBefore["memory.high"] != "max" || facts.CommonBefore["pids.max"] != "max" || facts.CommonBefore["memory.oom.group"] != "0" ||
		!managementNonbindingCpu(facts.CommonBefore["cpu.max"]) {
		return errors.New("ordinary and independent recovery share a binding ancestor")
	}
	if !reflect.DeepEqual(facts.Before.Children, facts.After.Children) || !slices.Equal(facts.Before.Children[policy.Parent.CgroupPath], expectedChildren) ||
		len(facts.Before.Groups) != len(facts.After.Groups) || len(facts.Before.Groups) < len(groups) || len(facts.Before.Groups) > settings.CleanupObjectCount {
		return errors.New("ordinary management closure is incomplete or changed")
	}
	for _, snapshot := range []managementOrdinarySnapshot{facts.Before, facts.After} {
		for path, values := range snapshot.Groups {
			if !pathWithin(policy.Parent.CgroupPath, path) || values["cgroup.type"] != "domain" || validateRuntimeBoundaryManagementCpuSet(values["cpuset.cpus.effective"], policy.ManagementCpuSet) != nil {
				return errors.New("ordinary descendant escaped the management CPU/domain boundary")
			}
			if parentPath, inherited := values["cpuset.inherited_from"]; inherited {
				parent := snapshot.Groups[parentPath]
				_, parentObserved := values["cpuset.parent_subtree_control"]
				_, childObserved := values["cpuset.controllers"]
				if !parentObserved || !childObserved || parentPath != filepath.Dir(path) || parent == nil || values["cpuset.cpus.effective"] != parent["cpuset.cpus.effective"] ||
					!managementCpusetDisabled(values["cpuset.parent_subtree_control"]) || !managementCpusetDisabled(values["cpuset.controllers"]) ||
					!reflect.DeepEqual(facts.Before.Groups[path], facts.After.Groups[path]) {
					return errors.New("ordinary cpuset inheritance is incomplete or changed")
				}
			} else {
				_, beforeInherited := facts.Before.Groups[path]["cpuset.inherited_from"]
				_, afterInherited := facts.After.Groups[path]["cpuset.inherited_from"]
				if beforeInherited || afterInherited {
					return errors.New("ordinary cpuset inheritance changed its observation source")
				}
			}
		}
		for _, group := range groups {
			values, found := snapshot.Groups[group.CgroupPath]
			expected := map[string]string{"memory.max": strconv.FormatInt(group.MemoryBytes, 10), "memory.high": "max", "memory.swap.max": "0",
				"pids.max": strconv.FormatInt(group.TasksMaximum, 10), "cgroup.type": "domain", "memory.oom.group": "0", "cgroup.procs": ""}
			if !found || len(values) != len(managementOrdinaryPropertyNames()) {
				return errors.New("ordinary aggregate omitted a fixed role")
			}
			for key, expected := range expected {
				if values[key] != expected {
					return errors.New("ordinary aggregate caps or direct-process placement differ from the ledger")
				}
			}
			if validateRuntimeBoundaryManagementCpuSet(values["cpuset.cpus"], policy.ManagementCpuSet) != nil || !managementNonbindingCpu(values["cpu.max"]) {
				return errors.New("ordinary aggregate has an inherited/wrong CPU set or unexpected throttle")
			}
			for _, usage := range []struct {
				key            string
				limit, reserve int64
			}{{key: "memory.current", limit: group.MemoryBytes, reserve: settings.ManagementMemoryHeadroomBytes}, {key: "pids.current", limit: group.TasksMaximum, reserve: settings.ManagementTasksHeadroom}} {
				value, err := strconv.ParseInt(values[usage.key], 10, 64)
				if err != nil || value < 0 || strconv.FormatInt(value, 10) != values[usage.key] || usage.reserve > usage.limit || value > usage.limit-usage.reserve {
					return errors.New("ordinary aggregate lacks positively observed runnable headroom")
				}
			}
		}
	}
	return nil
}

// Kernel controller lists are canonical identifiers. Absence proves inherited
// cpuset membership only when neither the parent nor child exposes cpuset.
func managementCpusetDisabled(value string) bool {
	fields := strings.Fields(value)
	if value != strings.Join(fields, " ") {
		return false
	}
	seen := map[string]bool{}
	for _, field := range fields {
		if field == "cpuset" || seen[field] || field[0] < 'a' || field[0] > 'z' {
			return false
		}
		for _, character := range field {
			if character < 'a' || character > 'z' {
				if character < '0' || character > '9' {
					if character != '_' {
						return false
					}
				}
			}
		}
		seen[field] = true
	}
	return true
}

// A canonical kernel period is an ABI range, not a new policy timer.
func managementNonbindingCpu(value string) bool {
	fields := strings.Fields(value)
	if len(fields) != 2 || fields[0] != "max" {
		return false
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	return err == nil && strconv.FormatUint(period, 10) == fields[1] && period >= 1000 && period <= 1000000
}
