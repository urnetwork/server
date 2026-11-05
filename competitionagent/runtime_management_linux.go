// Private daemons and their managed helpers occupy only the frozen management
// processors. A correct network namespace or unit file does not prove that.
package competitionagent

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
)

// Manager, kernel and process observations are independent requirements. Both
// snapshots come from held proc/cgroup descriptors, not reopened PID paths.
type runtimeBoundaryManagementFacts struct {
	ProcessStartBefore, ProcessStartAfter uint64
	CgroupBefore, CgroupAfter             []byte
	StatusBefore, StatusAfter             []byte
	ConfiguredBefore, ConfiguredAfter     []byte
	EffectiveBefore, EffectiveAfter       []byte
}

// Range spelling may differ in manager/kernel output; processor membership
// must be identical and finite, never a default/inherited broad host set.
func validateRuntimeBoundaryManagementCpuSet(actual, expected string) error {
	maximum := DefaultDockerProxySettings().MaximumCpuIndex
	actualCpus, err := dockerPolicyCpuSet(actual, maximum)
	expectedCpus, expectedErr := dockerPolicyCpuSet(expected, maximum)
	if err != nil || expectedErr != nil || !reflect.DeepEqual(actualCpus, expectedCpus) {
		return errors.New("private management processors differ from their explicit frozen set")
	}
	return nil
}

// systemd prints CPU sets with spaces. Unbounded/missing values and malformed
// whitespace cannot be confused with an explicitly restricted manager policy.
func validateRuntimeBoundaryManagementManager(actual string, boundary RuntimeResourceBoundary) error {
	if strings.TrimSpace(actual) != actual || strings.ContainsAny(actual, "\t\r\n") {
		return errors.New("private manager CPU policy is not a finite range list")
	}
	return validateRuntimeBoundaryManagementCpuSet(strings.ReplaceAll(actual, " ", ","), boundary.ManagementCpuSet)
}

// The independently pinned resource helper remains the only role selector;
// protected daemon configuration cannot silently choose other processors.
func validateRuntimeBoundaryManagementIdentity(boundary RuntimeResourceBoundary, resource runResourceIdentity) error {
	return validateRuntimeBoundaryManagementCpuSet(boundary.ManagementCpuSet, resource.ManagementCpuSet)
}

// Only the captured keeper/engine/containerd service groups can own private
// management processes. A shim is a member of the exact containerd group.
func runtimeBoundaryManagementGroup(unit runtimeBoundaryUnit) bool {
	switch unit.Name {
	case runtimeBoundaryKeeperUnitName, runtimeBoundaryEngineUnitName, runtimeBoundaryContainerdUnitName:
		return unit.ControlGroup == "/system.slice/"+unit.Name
	}
	return false
}

// The native path never accepts a caller-selected filesystem or process path.
// Pure tests replace only these roots, with mandatory kernel checks disabled.
func verifyRuntimeBoundaryManagement(unit runtimeBoundaryUnit, boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings) error {
	facts, err := readRuntimeBoundaryManagementFacts(unit, "/proc", "/sys/fs/cgroup", settings, true)
	if err != nil {
		return err
	}
	return validateRuntimeBoundaryManagementFacts(unit, boundary, facts, settings)
}

// Opening each parent once confines every bounded read to the captured kernel
// generation. No field chooses a write, mount, command or alternate root.
func readRuntimeBoundaryManagementFacts(unit runtimeBoundaryUnit, procPath, cgroupPath string, settings RuntimeResourceBoundarySettings, requireKernel bool) (runtimeBoundaryManagementFacts, error) {
	facts := runtimeBoundaryManagementFacts{}
	if !runtimeBoundaryManagementGroup(unit) || unit.Pid <= 1 || unit.ProcessStart == 0 || settings.DocumentBytes <= 0 {
		return facts, errors.New("private management process has no definite service generation")
	}
	procRoot, err := openDirectoryNoFollow(procPath)
	if err != nil {
		return facts, err
	}
	defer procRoot.Close()
	proc, err := openRelativeNoFollow(procRoot, strconv.Itoa(unit.Pid), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return facts, err
	}
	defer proc.Close()
	cgroupRoot, err := openDirectoryNoFollow(cgroupPath)
	if err != nil {
		return facts, err
	}
	defer cgroupRoot.Close()
	group, err := openRelativeNoFollow(cgroupRoot, strings.TrimPrefix(unit.ControlGroup, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return facts, err
	}
	defer group.Close()
	return readRuntimeBoundaryManagementDescriptors(proc, group, settings, requireKernel)
}

// Descriptor ownership remains with the caller, including across rename or
// process-path replacement. Tests use only private regular-file hierarchies.
func readRuntimeBoundaryManagementDescriptors(proc, group *os.File, settings RuntimeResourceBoundarySettings, requireKernel bool) (runtimeBoundaryManagementFacts, error) {
	facts := runtimeBoundaryManagementFacts{}
	if proc == nil || group == nil || settings.DocumentBytes <= 0 {
		return facts, errors.New("management evidence has no bounded descriptor ownership")
	}
	const procMagic = 0x9fa0
	const cgroup2Magic = 0x63677270
	if requireKernel {
		var procFilesystem, groupFilesystem syscall.Statfs_t
		if syscall.Fstatfs(int(proc.Fd()), &procFilesystem) != nil || procFilesystem.Type != procMagic ||
			syscall.Fstatfs(int(group.Fd()), &groupFilesystem) != nil || groupFilesystem.Type != cgroup2Magic {
			return facts, errors.New("management evidence is not native procfs and cgroup v2")
		}
	}
	read := func(directory *os.File, name string, filesystemType int64) ([]byte, error) {
		fd, err := syscall.Openat(int(directory.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), "private-management-kernel-property")
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("management kernel property is not regular")
		}
		if requireKernel {
			var filesystem syscall.Statfs_t
			if syscall.Fstatfs(fd, &filesystem) != nil || filesystem.Type != filesystemType {
				return nil, errors.New("management kernel property changed its filesystem")
			}
		}
		content, err := io.ReadAll(io.LimitReader(file, settings.DocumentBytes+1))
		if err != nil || int64(len(content)) > settings.DocumentBytes {
			return nil, errors.New("management kernel property is unavailable or oversized")
		}
		return content, nil
	}
	for _, snapshot := range []struct {
		start                 *uint64
		cgroup, status        *[]byte
		configured, effective *[]byte
	}{
		{start: &facts.ProcessStartBefore, cgroup: &facts.CgroupBefore, status: &facts.StatusBefore, configured: &facts.ConfiguredBefore, effective: &facts.EffectiveBefore},
		{start: &facts.ProcessStartAfter, cgroup: &facts.CgroupAfter, status: &facts.StatusAfter, configured: &facts.ConfiguredAfter, effective: &facts.EffectiveAfter},
	} {
		stat, err := read(proc, "stat", procMagic)
		if err != nil {
			return facts, err
		}
		*snapshot.start, err = runtimeBoundaryProcessStart(stat)
		if err != nil {
			return facts, err
		}
		for _, property := range []struct {
			directory  *os.File
			name       string
			filesystem int64
			value      *[]byte
		}{
			{directory: proc, name: "cgroup", filesystem: procMagic, value: snapshot.cgroup},
			{directory: proc, name: "status", filesystem: procMagic, value: snapshot.status},
			{directory: group, name: "cpuset.cpus", filesystem: cgroup2Magic, value: snapshot.configured},
			{directory: group, name: "cpuset.cpus.effective", filesystem: cgroup2Magic, value: snapshot.effective},
		} {
			*property.value, err = read(property.directory, property.name, property.filesystem)
			if err != nil {
				return facts, err
			}
		}
	}
	return facts, nil
}

// An unchanged namespace is insufficient if the process moved, its service
// inherited an unrestricted cpuset, or a later kernel read broadened that set.
func validateRuntimeBoundaryManagementFacts(unit runtimeBoundaryUnit, boundary RuntimeResourceBoundary, facts runtimeBoundaryManagementFacts, settings RuntimeResourceBoundarySettings) error {
	if !runtimeBoundaryManagementGroup(unit) || unit.ProcessStart == 0 || facts.ProcessStartBefore != unit.ProcessStart || facts.ProcessStartAfter != unit.ProcessStart || settings.DocumentBytes <= 0 {
		return errors.New("private management process changed its captured generation")
	}
	for _, membership := range [][]byte{facts.CgroupBefore, facts.CgroupAfter} {
		if int64(len(membership)) > settings.DocumentBytes || !bytes.Equal(bytes.TrimSuffix(membership, []byte{'\n'}), []byte("0::"+unit.ControlGroup)) {
			return errors.New("private management process escaped its exact service cgroup")
		}
	}
	for _, content := range [][]byte{facts.ConfiguredBefore, facts.ConfiguredAfter, facts.EffectiveBefore, facts.EffectiveAfter} {
		if int64(len(content)) > settings.DocumentBytes || validateRuntimeBoundaryManagementCpuSet(strings.TrimSuffix(string(content), "\n"), boundary.ManagementCpuSet) != nil {
			return errors.New("private management kernel CPU policy differs from its frozen role")
		}
	}
	for _, status := range [][]byte{facts.StatusBefore, facts.StatusAfter} {
		if int64(len(status)) > settings.DocumentBytes {
			return errors.New("private management process status exceeds its bound")
		}
		seen := false
		for _, line := range strings.Split(string(status), "\n") {
			if !strings.HasPrefix(line, "Cpus_allowed_list:") {
				continue
			}
			fields := strings.Fields(line)
			if seen || len(fields) != 2 || fields[0] != "Cpus_allowed_list:" || validateRuntimeBoundaryManagementCpuSet(fields[1], boundary.ManagementCpuSet) != nil {
				return errors.New("private management process affinity escaped its frozen role")
			}
			seen = true
		}
		if !seen {
			return errors.New("private management process affinity is absent")
		}
	}
	return nil
}
