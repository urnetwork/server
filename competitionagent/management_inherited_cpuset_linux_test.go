// Descriptor-backed synthetic cgroups reproduce a disabled delegated cpuset.
// No test touches a real service, process, host identity or cgroup filesystem.
package competitionagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A private service is constrained directly; its child has no cpuset file
// because the parent did not delegate that controller into the child domain.
func managementInheritedCpusetTestTree(t *testing.T) (string, ManagementResourcePolicy, AgentSettings, string, string) {
	t.Helper()
	policy, settings, facts := managementOrdinaryTestFacts(t)
	parent := managementPrivateCgroupPath + "/synthetic-control.service"
	child := parent + "/.control"
	facts.Before.Groups[parent]["cgroup.subtree_control"] = ""
	facts.Before.Children[parent] = []string{".control"}
	facts.Before.Groups[child] = map[string]string{"cgroup.type": "domain", "cgroup.controllers": ""}
	facts.Before.Children[child] = []string{}
	root := t.TempDir()
	writeManagementOrdinaryTestTree(t, root, facts)
	return root, policy, settings, parent, child
}

// The old reader deterministically failed on this absent file. Inheritance
// requires actual parent and child controller observations, not an empty set.
func TestManagementOrdinaryProvesDisabledChildCpusetInheritance(t *testing.T) {
	root, policy, settings, parent, child := managementInheritedCpusetTestTree(t)
	if _, err := os.Stat(filepath.Join(root, child, "cpuset.cpus.effective")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture did not reproduce the missing delegated property", err)
	}
	facts, err := readManagementOrdinaryFacts(context.Background(), policy, settings, root, false, nil)
	if err != nil {
		t.Fatal("known bounded parent inheritance was refused", err)
	}
	for _, snapshot := range []managementOrdinarySnapshot{facts.Before, facts.After} {
		values := snapshot.Groups[child]
		if values["cpuset.inherited_from"] != parent || values["cpuset.cpus.effective"] != policy.ManagementCpuSet {
			t.Fatal("effective set was not derived from its observed parent", values)
		}
	}
	if err := validateManagementOrdinaryFacts(facts, policy, settings); err != nil {
		t.Fatal("complete native-shaped inheritance proof was rejected", err)
	}
}

// Multiple disabled descendants retain one anchored, descriptor-observed set.
func TestManagementOrdinaryProvesNestedDisabledCpusetInheritance(t *testing.T) {
	root, policy, settings, _, child := managementInheritedCpusetTestTree(t)
	leaf := filepath.Join(root, child, "synthetic-leaf")
	if err := os.Mkdir(leaf, 0700); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{filepath.Join(root, child, "cgroup.subtree_control"): "\n", filepath.Join(leaf, "cgroup.controllers"): "\n", filepath.Join(leaf, "cgroup.type"): "domain\n"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := readManagementOrdinaryFacts(context.Background(), policy, settings, root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManagementOrdinaryFacts(facts, policy, settings); err != nil {
		t.Fatal(err)
	}
	if facts.Before.Groups[child+"/synthetic-leaf"]["cpuset.inherited_from"] != child {
		t.Fatal("nested child lost its immediate observed parent")
	}
}

// An enabled, malformed or unobserved controller cannot explain a missing
// effective set. Fixed role roots must still expose all their direct limits.
func TestManagementOrdinaryRejectsUnprovedCpusetInheritance(t *testing.T) {
	for _, variation := range []string{"parent-enabled", "child-enabled", "missing-parent", "missing-child", "malformed-parent", "fixed-role", "symlink"} {
		root, policy, settings, parent, child := managementInheritedCpusetTestTree(t)
		var err error
		switch variation {
		case "parent-enabled":
			err = os.WriteFile(filepath.Join(root, parent, "cgroup.subtree_control"), []byte("cpuset\n"), 0600)
		case "child-enabled":
			err = os.WriteFile(filepath.Join(root, child, "cgroup.controllers"), []byte("cpuset\n"), 0600)
		case "missing-parent":
			err = os.Remove(filepath.Join(root, parent, "cgroup.subtree_control"))
		case "missing-child":
			err = os.Remove(filepath.Join(root, child, "cgroup.controllers"))
		case "malformed-parent":
			err = os.WriteFile(filepath.Join(root, parent, "cgroup.subtree_control"), []byte("+cpuset\n"), 0600)
		case "fixed-role":
			err = os.Remove(filepath.Join(root, managementPrivateCgroupPath, "cpuset.cpus.effective"))
		case "symlink":
			err = os.Symlink("synthetic-missing", filepath.Join(root, child, "cpuset.cpus.effective"))
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readManagementOrdinaryFacts(context.Background(), policy, settings, root, false, nil); err == nil {
			t.Fatal("missing or unsafe cpuset proof was accepted", variation)
		}
	}
}

// A controller can become enabled between snapshots without changing the CPU
// numbers. Preserve observation-source changes rather than silently renewing.
func TestManagementOrdinaryRejectsChangedCpusetInheritanceSource(t *testing.T) {
	root, policy, settings, parent, child := managementInheritedCpusetTestTree(t)
	facts, err := readManagementOrdinaryFacts(context.Background(), policy, settings, root, false, func() {
		for path, value := range map[string]string{filepath.Join(root, parent, "cgroup.subtree_control"): "cpuset\n", filepath.Join(root, child, "cgroup.controllers"): "cpuset\n", filepath.Join(root, child, "cpuset.cpus.effective"): policy.ManagementCpuSet + "\n"} {
			if err := os.WriteFile(path, []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManagementOrdinaryFacts(facts, policy, settings); err == nil {
		t.Fatal("changed controller observation source was accepted")
	}
}

// Empty controller observations are meaningful only when actually present.
func TestManagementOrdinaryRejectsMissingInheritedControllerEvidence(t *testing.T) {
	for _, key := range []string{"cpuset.parent_subtree_control", "cpuset.controllers"} {
		root, policy, settings, _, child := managementInheritedCpusetTestTree(t)
		facts, err := readManagementOrdinaryFacts(context.Background(), policy, settings, root, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		delete(facts.Before.Groups[child], key)
		delete(facts.After.Groups[child], key)
		if err := validateManagementOrdinaryFacts(facts, policy, settings); err == nil {
			t.Fatal("missing evidence became an observed empty controller list", key)
		}
	}
}

// Future canonical controller identifiers are permitted, but aliases,
// duplicates and malformed tokens never prove cpuset absence.
func TestManagementCpusetDisabledRequiresCanonicalObservedControllers(t *testing.T) {
	for _, value := range []string{"", "cpu io memory pids", "cpu future2"} {
		if !managementCpusetDisabled(value) {
			t.Fatal("valid disabled-controller observation refused", value)
		}
	}
	for _, value := range []string{"cpuset", "cpu cpuset", "+cpuset", "cpu cpu", "cpu\tmemory", "0cpu", "_cpu", "cpu!", " cpu"} {
		if managementCpusetDisabled(value) {
			t.Fatal("unsafe controller observation accepted", value)
		}
	}
}
