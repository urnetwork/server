// Management affinity regressions use private files and synthetic kernel facts.
// They neither inspect host daemons nor change native CPU/controller policy.
package competitionagent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A correct private namespace is independent of its processor/cgroup role.
func runtimeManagementTestFacts() (runtimeBoundaryUnit, runtimeBoundaryManagementFacts) {
	unit := runtimeBoundaryUnit{Name: runtimeBoundaryEngineUnitName, ControlGroup: "/system.slice/" + runtimeBoundaryEngineUnitName,
		Pid: 123, ProcessStart: 456, Namespace: runtimeBoundaryNamespaceId{Device: 4, Inode: 200}}
	facts := runtimeBoundaryManagementFacts{ProcessStartBefore: unit.ProcessStart, ProcessStartAfter: unit.ProcessStart,
		CgroupBefore: []byte("0::" + unit.ControlGroup + "\n"), CgroupAfter: []byte("0::" + unit.ControlGroup + "\n"),
		StatusBefore: []byte("Name:\tsynthetic-private-engine\nCpus_allowed_list:\t2,4\n"), StatusAfter: []byte("Cpus_allowed_list:\t2,4\n"),
		ConfiguredBefore: []byte("2,4\n"), ConfiguredAfter: []byte("2,4\n"), EffectiveBefore: []byte("2,4\n"), EffectiveAfter: []byte("2,4\n")}
	return unit, facts
}

// A namespace/unit-file match must not hide a manager's default broad set.
func TestRuntimeResourceBoundaryManagementRejectsUnrestrictedManager(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	pin := fixture.boundary.EngineUnit
	for _, value := range []string{"", "0-4", "2 3 4", "2\t4", "2 4 ", "2 2 4", "infinity"} {
		properties := runtimeBoundaryTestUnitProperties(fixture.boundary, pin)
		properties["AllowedCPUs"] = value
		if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(properties), pin, fixture.boundary, fixture.settings); err == nil {
			t.Fatal("correct namespace hid an unrestricted manager CPU policy", value)
		}
	}
	properties := runtimeBoundaryTestUnitProperties(fixture.boundary, pin)
	delete(properties, "AllowedCPUs")
	if _, err := parseRuntimeBoundaryUnit(runtimeBoundaryTestUnitBytes(properties), pin, fixture.boundary, fixture.settings); err == nil {
		t.Fatal("missing manager CPU policy inherited admission authority")
	}
}

// Existing protected documents cannot omit an explicit prospective role set.
func TestRuntimeResourceBoundaryManagementRequiresExplicitProcessors(t *testing.T) {
	for _, value := range []string{"", "2,2", "02,4", "4-2", "2,"} {
		fixture := newRuntimeBoundaryTest(t)
		fixture.boundary.ManagementCpuSet = value
		if err := fixture.boundary.validate(fixture.scope); err == nil {
			t.Fatal("unbounded or ambiguous private management CPU set was admitted", value)
		}
	}
}

// The resource helper's frozen management role cannot be substituted by a
// separately pinned daemon policy that happens to use some valid processors.
func TestRuntimeResourceBoundaryManagementMatchesResourcePolicy(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	resource := runResourceIdentity{ManagementCpuSet: "2,4"}
	if err := validateRuntimeBoundaryManagementIdentity(fixture.boundary, resource); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "0,6", "2", "2,4,6", "2,2,4"} {
		resource.ManagementCpuSet = value
		if err := validateRuntimeBoundaryManagementIdentity(fixture.boundary, resource); err == nil {
			t.Fatal("private daemon selected a different management role", value)
		}
	}
}

// Manager correctness is not a kernel cpuset proof; neither a correct namespace
// nor one matching early read can authorize a later broad effective set.
func TestRuntimeResourceBoundaryManagementCorrectNamespaceCannotHideWrongProcessors(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	unit, valid := runtimeManagementTestFacts()
	units := map[string]runtimeBoundaryUnit{}
	for index, name := range []string{runtimeBoundaryKeeperUnitName, runtimeBoundaryEngineUnitName, runtimeBoundaryContainerdUnitName} {
		participant := unit
		participant.Pid, participant.Name = 120+index, name
		units[name] = participant
	}
	if err := validateRuntimeBoundaryNamespacePeers(units, unit.Namespace, runtimeBoundaryNamespaceId{Device: 4, Inode: 100}); err != nil {
		t.Fatal("synthetic positive namespace control failed", err)
	}
	if err := validateRuntimeBoundaryManagementFacts(unit, fixture.boundary, valid, fixture.settings); err != nil {
		t.Fatal("synthetic positive CPU control failed", err)
	}
	for _, change := range []func(*runtimeBoundaryManagementFacts){
		func(facts *runtimeBoundaryManagementFacts) { facts.ConfiguredBefore = []byte("\n") },
		func(facts *runtimeBoundaryManagementFacts) { facts.ConfiguredAfter = []byte("0-4\n") },
		func(facts *runtimeBoundaryManagementFacts) { facts.EffectiveBefore = []byte("2,3,4\n") },
		func(facts *runtimeBoundaryManagementFacts) { facts.EffectiveAfter = []byte("0-4\n") },
	} {
		facts := valid
		change(&facts)
		if err := validateRuntimeBoundaryManagementFacts(unit, fixture.boundary, facts, fixture.settings); err == nil {
			t.Fatal("correct namespace hid a changed, inherited or broad kernel CPU set")
		}
	}
}

// Kernel CPU sets alone cannot qualify a process that left its captured unit
// or a PID path that now describes a different process generation.
func TestRuntimeResourceBoundaryManagementRejectsMovedOrReusedProcess(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	unit, valid := runtimeManagementTestFacts()
	for _, change := range []func(*runtimeBoundaryManagementFacts){
		func(facts *runtimeBoundaryManagementFacts) { facts.ProcessStartBefore++ },
		func(facts *runtimeBoundaryManagementFacts) { facts.ProcessStartAfter++ },
		func(facts *runtimeBoundaryManagementFacts) {
			facts.CgroupBefore = []byte("0::/system.slice/synthetic-foreign.service\n")
		},
		func(facts *runtimeBoundaryManagementFacts) {
			facts.CgroupAfter = []byte("0::" + unit.ControlGroup + "-alias\n")
		},
		func(facts *runtimeBoundaryManagementFacts) { facts.StatusAfter = []byte("Cpus_allowed_list:\t0-4\n") },
	} {
		facts := valid
		change(&facts)
		if err := validateRuntimeBoundaryManagementFacts(unit, fixture.boundary, facts, fixture.settings); err == nil {
			t.Fatal("management proof survived changed process/cgroup/affinity custody")
		}
	}
}

// Missing or duplicated kernel status fields never inherit another field's
// meaning, and a bounded read is still required for otherwise valid metadata.
func TestRuntimeResourceBoundaryManagementRejectsAmbiguousProcessAffinity(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	unit, valid := runtimeManagementTestFacts()
	for _, value := range []string{"", "Cpus_allowed_list:\n", "Cpus_allowed_list:\t2,4\nCpus_allowed_list:\t2,4\n", "Cpus_allowed_list:\t2,4 extra\n", "Cpus_allowed_list:\t2,2,4\n"} {
		facts := valid
		facts.StatusAfter = []byte(value)
		if err := validateRuntimeBoundaryManagementFacts(unit, fixture.boundary, facts, fixture.settings); err == nil {
			t.Fatal("ambiguous process affinity was admitted")
		}
	}
	facts := valid
	facts.StatusAfter = append([]byte(strings.Repeat("x", int(fixture.settings.DocumentBytes))), valid.StatusAfter...)
	if err := validateRuntimeBoundaryManagementFacts(unit, fixture.boundary, facts, fixture.settings); err == nil {
		t.Fatal("unbounded process affinity document was admitted")
	}
}

// Only private filesystem roots are created. These stat fields match the
// production parser's process-birth position, including a spaced comm value.
func newRuntimeManagementFileTest(t *testing.T) (runtimeBoundaryUnit, string, string, string, string) {
	t.Helper()
	unit, facts := runtimeManagementTestFacts()
	root := t.TempDir()
	procRoot, cgroupRoot := filepath.Join(root, "proc"), filepath.Join(root, "cgroup")
	procPath := filepath.Join(procRoot, strconv.Itoa(unit.Pid))
	groupPath := filepath.Join(cgroupRoot, strings.TrimPrefix(unit.ControlGroup, "/"))
	for _, path := range []string{procPath, groupPath} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fields := []string{"S"}
	for range 18 {
		fields = append(fields, "0")
	}
	fields = append(fields, strconv.FormatUint(unit.ProcessStart, 10))
	for path, content := range map[string][]byte{
		filepath.Join(procPath, "stat"):   []byte(strconv.Itoa(unit.Pid) + " (synthetic private daemon) " + strings.Join(fields, " ") + "\n"),
		filepath.Join(procPath, "status"): facts.StatusBefore, filepath.Join(procPath, "cgroup"): facts.CgroupBefore,
		filepath.Join(groupPath, "cpuset.cpus"): facts.ConfiguredBefore, filepath.Join(groupPath, "cpuset.cpus.effective"): facts.EffectiveBefore,
	} {
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return unit, procRoot, cgroupRoot, procPath, groupPath
}

// The production descriptor reader and parser compose on private files; no
// untested string-only stand-in substitutes for its bounded FD reads.
func TestRuntimeResourceBoundaryManagementPrivateDescriptorComposition(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	unit, procRoot, cgroupRoot, _, _ := newRuntimeManagementFileTest(t)
	facts, err := readRuntimeBoundaryManagementFacts(unit, procRoot, cgroupRoot, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeBoundaryManagementFacts(unit, fixture.boundary, facts, fixture.settings); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntimeBoundaryManagementFacts(unit, procRoot, cgroupRoot, fixture.settings, true); err == nil {
		t.Fatal("private regular-file evidence impersonated actual procfs/cgroup v2")
	}
}

// Captured directories survive path replacement without reading the new
// generation. Both handles stay owned until the complete observation joins.
func TestRuntimeResourceBoundaryManagementHeldDescriptorsIgnorePathReplacement(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	unit, _, _, procPath, groupPath := newRuntimeManagementFileTest(t)
	proc, err := os.Open(procPath)
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Close()
	group, err := os.Open(groupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	for _, path := range []string{procPath, groupPath} {
		if err := os.Rename(path, path+"-captured"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(groupPath, "cpuset.cpus.effective"), []byte("0-4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	facts, err := readRuntimeBoundaryManagementDescriptors(proc, group, fixture.settings, false)
	if err != nil {
		t.Fatal("captured directories followed replacement paths", err)
	}
	if err := validateRuntimeBoundaryManagementFacts(unit, fixture.boundary, facts, fixture.settings); err != nil {
		t.Fatal("captured directories lost their original valid evidence", err)
	}
}

// A leaf symlink must fail before reading even an otherwise valid CPU list.
func TestRuntimeResourceBoundaryManagementRejectsSymlinkKernelProperty(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	unit, procRoot, cgroupRoot, _, groupPath := newRuntimeManagementFileTest(t)
	property := filepath.Join(groupPath, "cpuset.cpus.effective")
	if err := os.Rename(property, property+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(property+"-original", property); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntimeBoundaryManagementFacts(unit, procRoot, cgroupRoot, fixture.settings, false); err == nil {
		t.Fatal("symlink kernel property was followed")
	}
}

// Capturing the process directory may not escape through a replaced PID leaf.
func TestRuntimeResourceBoundaryManagementRejectsSymlinkProcessDirectory(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	unit, procRoot, cgroupRoot, procPath, _ := newRuntimeManagementFileTest(t)
	if err := os.Rename(procPath, procPath+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(procPath+"-original", procPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntimeBoundaryManagementFacts(unit, procRoot, cgroupRoot, fixture.settings, false); err == nil {
		t.Fatal("symlink process generation was followed")
	}
}

// A short bound rejects before any partial stat/status can become authority.
func TestRuntimeResourceBoundaryManagementBoundsDescriptorReads(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	unit, procRoot, cgroupRoot, _, _ := newRuntimeManagementFileTest(t)
	fixture.settings.DocumentBytes = 8
	if _, err := readRuntimeBoundaryManagementFacts(unit, procRoot, cgroupRoot, fixture.settings, false); err == nil {
		t.Fatal("oversized kernel document bypassed the owner-local read bound")
	}
}
