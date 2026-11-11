// Synthetic held-descriptor inventories reproduce companion cgroup churn
// without starting a service, modifying live cgroups, or relaxing admission.
package competitionagent

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A short-lived service is not safe merely because its resource caps are
// finite. The complete hierarchy must have the same descendants at both reads.
func TestManagementOrdinaryCompanionTransientCgroupRejectsAdmission(t *testing.T) {
	for _, change := range []string{"appears", "disappears"} {
		t.Run(change, func(t *testing.T) {
			policy, settings, input := managementOrdinaryTestFacts(t)
			const name = "urnetwork-management-cache.service"
			path := managementPrivateCgroupPath + "/" + name
			properties := map[string]string{"cpuset.cpus.effective": policy.ManagementCpuSet, "cgroup.type": "domain"}
			if change == "disappears" {
				input.Before.Groups[path] = maps.Clone(properties)
				input.Before.Children[managementPrivateCgroupPath] = append(input.Before.Children[managementPrivateCgroupPath], name)
				slices.Sort(input.Before.Children[managementPrivateCgroupPath])
				input.Before.Children[path] = []string{}
			}
			root := t.TempDir()
			writeManagementOrdinaryTestTree(t, root, input)
			directory := filepath.Join(root, strings.TrimPrefix(path, "/"))
			facts, err := readManagementOrdinaryFacts(t.Context(), policy, settings, root, false, func() {
				if change == "disappears" {
					// Retain the exact old inode outside the observed hierarchy.
					// Only this private fixture directory is moved.
					if err := os.Rename(directory, filepath.Join(root, "retired-companion")); err != nil {
						t.Fatal(err)
					}
					return
				}
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				for name, value := range properties {
					if err := os.WriteFile(filepath.Join(directory, name), []byte(value+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			})
			if change == "appears" {
				if err == nil || err.Error() != "ordinary hierarchy gained an unobserved descendant" {
					t.Fatal("new companion cgroup did not fail the genuine descriptor walk", err)
				}
				return
			}
			if err != nil {
				t.Fatal("retained fixture could not complete its two reads", err)
			}
			if err := validateManagementOrdinaryFacts(facts, policy, settings); err == nil || err.Error() != "ordinary management closure is incomplete or changed" {
				t.Fatal("removed companion cgroup did not fail the genuine closure predicate", err)
			}
		})
	}
}

// A persistent service may fork bounded pass processes inside its unchanged
// cgroup. Actual aggregate usage is reread and still must preserve its reserve.
func TestManagementOrdinaryCompanionStableServiceProcessChurnKeepsAdmission(t *testing.T) {
	policy, settings, input := managementOrdinaryTestFacts(t)
	const name = "urnetwork-management-cache.service"
	path := managementPrivateCgroupPath + "/" + name
	input.Before.Groups[path] = map[string]string{"cpuset.cpus.effective": policy.ManagementCpuSet, "cgroup.type": "domain", "cgroup.procs": "500"}
	input.Before.Children[managementPrivateCgroupPath] = append(input.Before.Children[managementPrivateCgroupPath], name)
	slices.Sort(input.Before.Children[managementPrivateCgroupPath])
	input.Before.Children[path] = []string{}
	root := t.TempDir()
	writeManagementOrdinaryTestTree(t, root, input)
	directory := filepath.Join(root, strings.TrimPrefix(path, "/"))
	facts, err := readManagementOrdinaryFacts(t.Context(), policy, settings, root, false, func() {
		for path, content := range map[string]string{
			filepath.Join(directory, "cgroup.procs"):                                                    "500\n501\n",
			filepath.Join(root, strings.TrimPrefix(managementPrivateCgroupPath, "/"), "memory.current"): "4096\n",
			filepath.Join(root, strings.TrimPrefix(managementPrivateCgroupPath, "/"), "pids.current"):   "3\n",
		} {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil {
		t.Fatal("stable companion changed the held-descriptor hierarchy", err)
	}
	if err := validateManagementOrdinaryFacts(facts, policy, settings); err != nil {
		t.Fatal("same-cgroup process churn with genuine headroom was rejected", err)
	}
}
