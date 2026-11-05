// Independent recovery has real manager and kernel limits outside ordinary
// management's failure domain. A protected ledger alone never supplies proof.
package competitionagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Each successful load authenticates the bytes actually decoded, not just an
// earlier stat/hash before a pathname could change. Tests alone bypass root.
func loadManagementResourcePolicy(pin Pin, settings ManagementBoundarySettings, maximum int64, protected bool) (*ManagementResourcePolicy, error) {
	if maximum <= 0 || validateFilePin(pin, maximum, protected) != nil {
		return nil, errors.New("management ledger has no protected bounded content pin")
	}
	content, err := readBounded(pin.Path, maximum)
	if err != nil {
		return nil, err
	}
	defer clear(content)
	digest := sha256.Sum256(content)
	policy := &ManagementResourcePolicy{}
	if hex.EncodeToString(digest[:]) != pin.Sha256 || validateDocumentKeys(content) != nil || decodeStrict(content, policy) != nil || ValidateManagementResourcePolicy(*policy, settings) != nil {
		return nil, errors.New("management ledger bytes differ from their exact admitted profile")
	}
	return policy, nil
}

// Fixed role identities are validated before this helper is called. Returning
// a copy cannot let a caller mutate a retained protected policy observation.
func managementPolicyRole(policy *ManagementResourcePolicy, role string) (ManagementRolePolicy, error) {
	if policy == nil {
		return ManagementRolePolicy{}, errors.New("management policy is absent")
	}
	for _, candidate := range policy.Roles {
		if candidate.Role == role {
			return candidate, nil
		}
	}
	return ManagementRolePolicy{}, errors.New("management role is absent")
}

// The native entrypoint has no alternate filesystem, caller PID, or proof
// Boolean. Recovery may verify itself without querying the unhealthy engine.
func verifyManagementRecoveryBoundary(parent context.Context, policy *ManagementResourcePolicy, systemctl Pin, settings AgentSettings, protected bool, requireCurrent bool) error {
	if policy == nil || ValidateManagementResourcePolicy(*policy, DefaultManagementBoundarySettings()) != nil ||
		settings.ControlTimeout <= 0 || settings.ConfigBytes <= 0 || settings.DiagnosticBytes <= 0 || settings.CleanupObjectCount <= 0 ||
		settings.RecoveryMemoryHeadroomBytes <= 0 || settings.RecoveryTasksHeadroom <= 0 {
		return errors.New("independent recovery lacks finite owning settings or policy")
	}
	ctx, cancel := context.WithTimeout(parent, settings.ControlTimeout)
	defer cancel()
	role, err := managementPolicyRole(policy, ManagementCleanupRole)
	if err != nil || validateFilePin(role.Unit, settings.ConfigBytes, protected) != nil {
		return errors.New("independent recovery has no protected slice pin")
	}
	properties := "Id,LoadState,ActiveState,FragmentPath,DropInPaths,ControlGroup,MemoryMax,MemoryHigh,MemorySwapMax,TasksMax,AllowedCPUs"
	commandSettings := DefaultRuntimeResourceBoundarySettings()
	commandSettings.DocumentBytes, commandSettings.NativeBytes, commandSettings.CommandTimeout = settings.ConfigBytes, settings.DiagnosticBytes, settings.ControlTimeout
	output, err := runtimeBoundaryRunCommand(ctx, systemctl, []string{"show", "--no-pager", "--property=" + properties, "--", role.SliceName}, nil, commandSettings)
	if err != nil || validateManagementRecoveryManager(output, *policy, role, settings) != nil {
		return errors.New("PID1 has not loaded the independent recovery slice with its exact limits")
	}
	facts, err := readManagementRecoveryFacts(ctx, role, settings, "/sys/fs/cgroup", "/proc", true, requireCurrent)
	if err != nil {
		return err
	}
	if err := validateManagementRecoveryFacts(facts, *policy, role, settings, requireCurrent); err != nil {
		return err
	}
	if err := validateFilePin(role.Unit, settings.ConfigBytes, protected); err != nil {
		return err
	}
	return ctx.Err()
}

// Manager observations do not substitute for kernel cgroup values. Empty,
// duplicated or unknown keys cannot supply the expected finite property set.
func validateManagementRecoveryManager(content []byte, policy ManagementResourcePolicy, role ManagementRolePolicy, settings AgentSettings) error {
	if int64(len(content)) > settings.DiagnosticBytes {
		return errors.New("recovery manager observation exceeds its bound")
	}
	properties := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			return errors.New("recovery manager observation is malformed")
		}
		if _, present := properties[key]; present {
			return errors.New("recovery manager observation repeats a property")
		}
		properties[key] = value
	}
	expected := map[string]string{"Id": role.SliceName, "LoadState": "loaded", "ActiveState": "active", "FragmentPath": role.Unit.Path,
		"DropInPaths": "", "ControlGroup": role.CgroupPath, "MemoryMax": strconv.FormatInt(role.MemoryBytes, 10), "MemoryHigh": "infinity",
		"MemorySwapMax": "0", "TasksMax": strconv.FormatInt(role.TasksMaximum, 10)}
	if len(properties) != len(expected)+1 {
		return errors.New("recovery manager observation omits or adds properties")
	}
	for key, value := range expected {
		if properties[key] != value {
			return errors.New("recovery manager value differs from its pinned independent role")
		}
	}
	if err := validateRuntimeBoundaryManagementManager(properties["AllowedCPUs"], RuntimeResourceBoundary{ManagementCpuSet: policy.ManagementCpuSet}); err != nil {
		return err
	}
	return nil
}

// Every field comes from held descriptors in one observation. Only package
// tests supply synthetic regular-file trees instead of the actual kernel roots.
type managementRecoveryFacts struct {
	Before, After       map[string]string
	Ancestors           map[string]map[string]string
	AncestorsAfter      map[string]map[string]string
	ChildrenBefore      []string
	ChildrenAfter       []string
	ProcessStartBefore  uint64
	ProcessStartAfter   uint64
	ProcessCgroupBefore string
	ProcessCgroupAfter  string
	ProcessStatusBefore []byte
	ProcessStatusAfter  []byte
}

// Exact names are kernel ABI, not a configurable property-discovery surface.
func managementRecoveryPropertyNames() []string {
	return []string{"memory.max", "memory.high", "memory.swap.max", "memory.current", "pids.max", "pids.current", "cpuset.cpus", "cpuset.cpus.effective", "cgroup.type"}
}

// A synchronous bounded read never follows even a confined relative symlink.
func readManagementKernelProperty(ctx context.Context, directory *os.File, name string, maximum int64, requireKernel bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := openRelativeNoFollow(directory, name, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || maximum <= 0 {
		return "", errors.New("recovery kernel property is not a bounded regular file")
	}
	if requireKernel {
		var filesystem syscall.Statfs_t
		if syscall.Fstatfs(int(file.Fd()), &filesystem) != nil || filesystem.Type != 0x63677270 {
			return "", errors.New("recovery property is not on the held cgroup-v2 filesystem")
		}
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum || ctx.Err() != nil {
		return "", errors.New("recovery kernel property is incomplete, oversized or canceled")
	}
	value := strings.TrimSuffix(string(content), "\n")
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("recovery kernel property is not a single exact value")
	}
	return value, nil
}

// Closure includes empty descendants: an unrelated dormant unit is not silently
// accepted merely because its current task count happens to be zero.
func readManagementRecoveryChildren(directory *os.File, maximum int) ([]string, error) {
	if directory == nil || maximum <= 0 || maximum+1 <= maximum {
		return nil, errors.New("recovery cgroup inventory has no finite owner bound")
	}
	entries, err := directory.ReadDir(maximum + 1)
	if err != nil && err != io.EOF || len(entries) > maximum {
		return nil, errors.New("recovery cgroup inventory exceeds its finite bound")
	}
	children := []string{}
	for _, entry := range entries {
		// Linux O_PATH is an ABI flag, not a policy setting. DirEntry.Info on
		// os.NewFile may reopen its display name; stat the held child instead.
		const openPath = 0x200000
		child, err := openRelativeNoFollow(directory, entry.Name(), openPath, 0)
		if err != nil {
			return nil, err
		}
		info, err := child.Stat()
		_ = child.Close()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("recovery cgroup entry is ambiguous or aliased")
		}
		if info.IsDir() {
			children = append(children, entry.Name())
		}
	}
	sort.Strings(children)
	return children, nil
}

// Cleanup's only non-root shared ancestor must remain nonbinding. Otherwise
// an ordinary-role OOM/throttle could also stop the purported independent reaper.
func readManagementRecoveryFacts(ctx context.Context, role ManagementRolePolicy, settings AgentSettings, cgroupPath, procPath string, requireKernel, requireCurrent bool) (managementRecoveryFacts, error) {
	return readManagementRecoveryFactsWithHook(ctx, role, settings, cgroupPath, procPath, requireKernel, requireCurrent, nil)
}

// The hook is private to deterministic path-generation/snapshot tests. Native
// callers always use the nil-hook entrypoint with fixed actual kernel roots.
func readManagementRecoveryFactsWithHook(ctx context.Context, role ManagementRolePolicy, settings AgentSettings, cgroupPath, procPath string, requireKernel, requireCurrent bool, beforeRecheck func()) (managementRecoveryFacts, error) {
	facts := managementRecoveryFacts{Before: map[string]string{}, After: map[string]string{}, Ancestors: map[string]map[string]string{}, AncestorsAfter: map[string]map[string]string{}}
	root, err := openDirectoryNoFollow(cgroupPath)
	if err != nil {
		return facts, err
	}
	defer root.Close()
	if requireKernel {
		var filesystem syscall.Statfs_t
		if syscall.Fstatfs(int(root.Fd()), &filesystem) != nil || filesystem.Type != 0x63677270 {
			return facts, errors.New("recovery hierarchy is not native cgroup v2")
		}
	}
	group, err := openRelativeNoFollow(root, strings.TrimPrefix(role.CgroupPath, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return facts, err
	}
	defer group.Close()
	original, err := group.Stat()
	if err != nil {
		return facts, err
	}
	for _, name := range managementRecoveryPropertyNames() {
		facts.Before[name], err = readManagementKernelProperty(ctx, group, name, settings.DiagnosticBytes, requireKernel)
		if err != nil {
			return facts, err
		}
	}
	heldAncestors := map[string]*os.File{}
	defer func() {
		for _, parent := range heldAncestors {
			_ = parent.Close()
		}
	}()
	for ancestor := filepath.Dir(role.CgroupPath); ancestor != "/"; ancestor = filepath.Dir(ancestor) {
		parent, err := openRelativeNoFollow(root, strings.TrimPrefix(ancestor, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return facts, err
		}
		heldAncestors[ancestor] = parent
		values := map[string]string{}
		for _, name := range []string{"memory.max", "memory.high", "pids.max", "cpu.max", "memory.oom.group"} {
			values[name], err = readManagementKernelProperty(ctx, parent, name, settings.DiagnosticBytes, requireKernel)
			if err != nil {
				return facts, err
			}
		}
		facts.Ancestors[ancestor] = values
	}
	facts.ChildrenBefore, err = readManagementRecoveryChildren(group, settings.CleanupObjectCount)
	if err != nil {
		return facts, err
	}
	if len(facts.ChildrenBefore) > 1 || len(facts.ChildrenBefore) == 1 && facts.ChildrenBefore[0] != managementRecoveryServiceName {
		return facts, errors.New("independent recovery slice contains another owner")
	}
	for _, name := range facts.ChildrenBefore {
		child, err := openRelativeNoFollow(group, name, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return facts, err
		}
		children, err := readManagementRecoveryChildren(child, settings.CleanupObjectCount)
		_ = child.Close()
		if err != nil || len(children) != 0 {
			return facts, errors.New("independent recovery service has untracked child cgroups")
		}
	}
	if requireCurrent {
		if err := readManagementRecoveryProcess(ctx, procPath, role, settings, requireKernel, &facts); err != nil {
			return facts, err
		}
	}
	if beforeRecheck != nil {
		beforeRecheck()
	}
	// Reopen only through the held hierarchy, then compare the same inode before
	// consuming the second snapshot. A path-generation swap is not fresh proof.
	current, err := openRelativeNoFollow(root, strings.TrimPrefix(role.CgroupPath, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return facts, err
	}
	defer current.Close()
	currentInfo, err := current.Stat()
	if err != nil || !os.SameFile(original, currentInfo) {
		return facts, errors.New("independent recovery cgroup generation changed")
	}
	for _, name := range managementRecoveryPropertyNames() {
		facts.After[name], err = readManagementKernelProperty(ctx, group, name, settings.DiagnosticBytes, requireKernel)
		if err != nil {
			return facts, err
		}
	}
	for path, parent := range heldAncestors {
		heldInfo, err := parent.Stat()
		if err != nil {
			return facts, err
		}
		current, err := openRelativeNoFollow(root, strings.TrimPrefix(path, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return facts, err
		}
		currentInfo, statErr := current.Stat()
		_ = current.Close()
		if statErr != nil || !os.SameFile(heldInfo, currentInfo) {
			return facts, errors.New("independent recovery ancestor generation changed")
		}
		values := map[string]string{}
		for _, name := range []string{"memory.max", "memory.high", "pids.max", "cpu.max", "memory.oom.group"} {
			values[name], err = readManagementKernelProperty(ctx, parent, name, settings.DiagnosticBytes, requireKernel)
			if err != nil {
				return facts, err
			}
		}
		facts.AncestorsAfter[path] = values
	}
	fresh, err := openRelativeNoFollow(group, ".", os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return facts, err
	}
	facts.ChildrenAfter, err = readManagementRecoveryChildren(fresh, settings.CleanupObjectCount)
	_ = fresh.Close()
	return facts, err
}

// Own-PID reads cannot name the crashing process, a candidate, or a caller PID.
// A native proc descriptor is retained around both birth/group observations.
func readManagementRecoveryProcess(ctx context.Context, procPath string, role ManagementRolePolicy, settings AgentSettings, requireKernel bool, facts *managementRecoveryFacts) error {
	root, err := openDirectoryNoFollow(procPath)
	if err != nil {
		return err
	}
	defer root.Close()
	process, err := openRelativeNoFollow(root, strconv.Itoa(os.Getpid()), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer process.Close()
	if requireKernel {
		var filesystem syscall.Statfs_t
		if syscall.Fstatfs(int(process.Fd()), &filesystem) != nil || filesystem.Type != 0x9fa0 {
			return errors.New("independent recovery process is not its native proc identity")
		}
	}
	read := func(name string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := openRelativeNoFollow(process, name, os.O_RDONLY, 0)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("recovery process property is not a regular kernel file")
		}
		if requireKernel {
			var filesystem syscall.Statfs_t
			if syscall.Fstatfs(int(file.Fd()), &filesystem) != nil || filesystem.Type != 0x9fa0 {
				return nil, errors.New("recovery process property left its held proc filesystem")
			}
		}
		content, err := io.ReadAll(io.LimitReader(file, settings.DiagnosticBytes+1))
		if err != nil || int64(len(content)) > settings.DiagnosticBytes {
			return nil, errors.New("recovery process evidence is oversized or incomplete")
		}
		return content, nil
	}
	stat, err := read("stat")
	if err != nil {
		return err
	}
	facts.ProcessStartBefore, err = runtimeBoundaryProcessStart(stat)
	if err != nil {
		return err
	}
	for index := 0; index < 2; index++ {
		group, err := read("cgroup")
		if err != nil {
			return err
		}
		status, err := read("status")
		if err != nil {
			return err
		}
		if index == 0 {
			facts.ProcessCgroupBefore, facts.ProcessStatusBefore = strings.TrimSuffix(string(group), "\n"), status
		} else {
			facts.ProcessCgroupAfter, facts.ProcessStatusAfter = strings.TrimSuffix(string(group), "\n"), status
		}
	}
	stat, err = read("stat")
	if err != nil {
		return err
	}
	facts.ProcessStartAfter, err = runtimeBoundaryProcessStart(stat)
	return err
}

// A zero process count is mandatory when arming a new launch; recovery itself
// retains explicit task/memory headroom for bounded native kill/join helpers.
func validateManagementRecoveryFacts(facts managementRecoveryFacts, policy ManagementResourcePolicy, role ManagementRolePolicy, settings AgentSettings, requireCurrent bool) error {
	expectedRole, roleErr := managementPolicyRole(&policy, ManagementCleanupRole)
	if roleErr != nil || role != expectedRole || ValidateManagementResourcePolicy(policy, DefaultManagementBoundarySettings()) != nil ||
		!reflect.DeepEqual(facts.ChildrenBefore, facts.ChildrenAfter) || !reflect.DeepEqual(facts.Ancestors, facts.AncestorsAfter) || len(facts.Ancestors) != 1 ||
		!pathWithin(managementParentCgroupPath, managementPrivateCgroupPath) || pathWithin(managementParentCgroupPath, role.CgroupPath) {
		return errors.New("independent recovery closure or failure-domain lineage is invalid")
	}
	if len(facts.ChildrenBefore) > 1 || len(facts.ChildrenBefore) == 1 && facts.ChildrenBefore[0] != managementRecoveryServiceName || requireCurrent && len(facts.ChildrenBefore) != 1 {
		return errors.New("recovery cgroup closure contains another owner or omits its running service")
	}
	ancestor, found := facts.Ancestors["/urnetwork.slice"]
	if !found || len(ancestor) != 5 || ancestor["memory.max"] != "max" || ancestor["memory.high"] != "max" || ancestor["pids.max"] != "max" || ancestor["memory.oom.group"] != "0" {
		return errors.New("a common ancestor can OOM or throttle independent cleanup")
	}
	quota := strings.Fields(ancestor["cpu.max"])
	if len(quota) != 2 || quota[0] != "max" {
		return errors.New("a common ancestor shares a binding CPU quota with recovery")
	}
	period, err := strconv.ParseUint(quota[1], 10, 64)
	if err != nil || strconv.FormatUint(period, 10) != quota[1] || period < 1000 || period > 1000000 {
		return errors.New("a common ancestor shares a binding CPU quota with recovery")
	}
	for _, limits := range []map[string]string{facts.Before, facts.After} {
		expected := map[string]string{"memory.max": strconv.FormatInt(role.MemoryBytes, 10), "memory.high": "max", "memory.swap.max": "0",
			"pids.max": strconv.FormatInt(role.TasksMaximum, 10), "cgroup.type": "domain"}
		if len(limits) != len(managementRecoveryPropertyNames()) {
			return errors.New("independent recovery kernel limits are incomplete")
		}
		for key, expected := range expected {
			if limits[key] != expected {
				return errors.New("independent recovery kernel cap differs from its role")
			}
		}
		for _, key := range []string{"cpuset.cpus", "cpuset.cpus.effective"} {
			if err := validateRuntimeBoundaryManagementCpuSet(limits[key], policy.ManagementCpuSet); err != nil {
				return err
			}
		}
		memory, memoryErr := strconv.ParseInt(limits["memory.current"], 10, 64)
		tasks, taskErr := strconv.ParseInt(limits["pids.current"], 10, 64)
		if memoryErr != nil || taskErr != nil || memory < 0 || tasks < 0 || settings.RecoveryMemoryHeadroomBytes <= 0 || settings.RecoveryTasksHeadroom <= 0 ||
			strconv.FormatInt(memory, 10) != limits["memory.current"] || strconv.FormatInt(tasks, 10) != limits["pids.current"] ||
			settings.RecoveryMemoryHeadroomBytes > role.MemoryBytes || settings.RecoveryTasksHeadroom > role.TasksMaximum ||
			memory > role.MemoryBytes-settings.RecoveryMemoryHeadroomBytes || tasks > role.TasksMaximum-settings.RecoveryTasksHeadroom || !requireCurrent && tasks != 0 || requireCurrent && tasks == 0 {
			return errors.New("independent recovery has no positively measured runnable headroom")
		}
	}
	if requireCurrent {
		expectedGroup := "0::" + role.CgroupPath + "/" + managementRecoveryServiceName
		if facts.ProcessStartBefore == 0 || facts.ProcessStartBefore != facts.ProcessStartAfter || facts.ProcessCgroupBefore != expectedGroup || facts.ProcessCgroupAfter != expectedGroup {
			return errors.New("current process is not the exact independent recovery service")
		}
		for _, content := range [][]byte{facts.ProcessStatusBefore, facts.ProcessStatusAfter} {
			seen := false
			for _, line := range bytes.Split(content, []byte{'\n'}) {
				fields := strings.Fields(string(line))
				if len(fields) > 0 && fields[0] == "Cpus_allowed_list:" {
					if seen || len(fields) != 2 || validateRuntimeBoundaryManagementCpuSet(fields[1], policy.ManagementCpuSet) != nil {
						return errors.New("recovery process escaped its management CPU set")
					}
					seen = true
				}
			}
			if !seen {
				return errors.New("recovery process has no positive native CPU affinity")
			}
		}
	}
	return nil
}
