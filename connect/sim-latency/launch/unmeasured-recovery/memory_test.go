// Deterministic synthetic cgroup tests reproduce the management-scope root cause.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Synthetic settings reproduce the frozen resource contract without host state.
func syntheticMemoryBudget() memoryBudget {
	return memoryBudget{EvaluationLimitBytes: 96 * 1024 * 1024 * 1024, EvidenceLimitBytes: 5 * 1024 * 1024 * 1024,
		ManagementReserveBytes: 24 * 1024 * 1024 * 1024, NonEvidenceMinimumBytes: 3 * 1024 * 1024 * 1024, ProposedScopeLimitBytes: 8 * 1024 * 1024 * 1024}
}

// Evidence alone exceeded the old scope before any simulation could start.
func TestThreeGibibyteScopeFailsBeforeWork(t *testing.T) {
	budget := syntheticMemoryBudget()
	budget.ProposedScopeLimitBytes = 3 * 1024 * 1024 * 1024
	started := false
	if _, err := validateMemoryBudget(budget, []cgroupLimit{{Path: "/synthetic/scope", LimitBytes: budget.ProposedScopeLimitBytes}}); err == nil {
		started = true
	}
	if started {
		t.Fatal("insufficient scope admitted start/copy work")
	}
}

// Existing own-scope allocation is included, rather than counted a second time.
func TestEightGibibyteScopePassesWithoutChangingEvaluation(t *testing.T) {
	budget := syntheticMemoryBudget()
	before := budget
	required, err := validateMemoryBudget(budget, []cgroupLimit{{Path: "/synthetic/scope", LimitBytes: 8 * 1024 * 1024 * 1024, CurrentBytes: 16 * 1024 * 1024}, {Path: "/synthetic", LimitBytes: 8 * 1024 * 1024 * 1024, CurrentBytes: 16 * 1024 * 1024}})
	if err != nil || required != 8*1024*1024*1024 || budget != before {
		t.Fatal("eight-gibibyte management gate changed frozen evaluation facts or rejected valid scope")
	}
}

// The evidence allowance cannot be borrowed by excessive pre-start allocations.
func TestPreStartUsageMustFitNonEvidenceAllowance(t *testing.T) {
	limits := []cgroupLimit{{Path: "/synthetic/scope", LimitBytes: 8 * 1024 * 1024 * 1024, CurrentBytes: 3*1024*1024*1024 + 1}}
	if _, err := validateMemoryBudget(syntheticMemoryBudget(), limits); err == nil {
		t.Fatal("pre-start usage consumed reserved evidence capacity")
	}
}

// An eight-gibibyte child must not hide a three-gibibyte parent.
func TestRestrictiveCgroupAncestorFails(t *testing.T) {
	limits := []cgroupLimit{{Path: "/synthetic/scope", LimitBytes: 8 * 1024 * 1024 * 1024}, {Path: "/synthetic", LimitBytes: 3 * 1024 * 1024 * 1024}}
	if _, err := validateMemoryBudget(syntheticMemoryBudget(), limits); err == nil {
		t.Fatal("restrictive parent limit was ignored")
	}
}

// Sibling pressure consumes parent headroom even when the child limit is ample.
func TestCgroupAncestorSiblingPressureFails(t *testing.T) {
	limits := []cgroupLimit{{Path: "/synthetic/scope", LimitBytes: 8 * 1024 * 1024 * 1024, CurrentBytes: 1024 * 1024 * 1024},
		{Path: "/synthetic", LimitBytes: 10 * 1024 * 1024 * 1024, CurrentBytes: 4 * 1024 * 1024 * 1024}}
	if _, err := validateMemoryBudget(syntheticMemoryBudget(), limits); err == nil {
		t.Fatal("parent sibling pressure was ignored")
	}
}

// Unlimited ancestors and exact sufficient parent headroom remain admissible.
func TestCgroupAncestorHeadroomPasses(t *testing.T) {
	limits := []cgroupLimit{{Path: "/synthetic/scope", LimitBytes: 8 * 1024 * 1024 * 1024, CurrentBytes: 1024 * 1024 * 1024},
		{Path: "/synthetic", LimitBytes: 10 * 1024 * 1024 * 1024, CurrentBytes: 3 * 1024 * 1024 * 1024}, {Path: "/", LimitBytes: -1, CurrentBytes: 4 * 1024 * 1024 * 1024}}
	if _, err := validateMemoryBudget(syntheticMemoryBudget(), limits); err != nil {
		t.Fatal(err)
	}
}

// Every frozen budget and every ancestor must be present and consistent.
func TestMemoryGateRejectsChangedFactsAndMissingAncestors(t *testing.T) {
	budgets := []memoryBudget{syntheticMemoryBudget(), syntheticMemoryBudget(), syntheticMemoryBudget(), syntheticMemoryBudget()}
	budgets[0].EvaluationLimitBytes--
	budgets[1].EvidenceLimitBytes--
	budgets[2].ManagementReserveBytes++
	budgets[3].NonEvidenceMinimumBytes--
	for index, budget := range budgets {
		if _, err := validateMemoryBudget(budget, []cgroupLimit{{Path: "/scope", LimitBytes: -1}}); err == nil {
			t.Errorf("changed fact %d admitted", index)
		}
	}
	for _, limits := range [][]cgroupLimit{nil, {{Path: "/a/b/c", LimitBytes: -1}, {Path: "/a", LimitBytes: -1}}} {
		if _, err := validateMemoryBudget(syntheticMemoryBudget(), limits); err == nil {
			t.Error("missing ancestor observation admitted")
		}
	}
}

// The read function proves full ancestor traversal with no systemd invocation.
func TestCurrentCgroupLimitsReadsEveryAncestor(t *testing.T) {
	files := map[string][]byte{"/proc/self/cgroup": []byte("0::/synthetic/scope\n")}
	for _, path := range []string{"/sys/fs/cgroup/synthetic/scope", "/sys/fs/cgroup/synthetic", "/sys/fs/cgroup"} {
		files[filepath.Join(path, "memory.max")] = []byte("max\n")
		files[filepath.Join(path, "memory.current")] = []byte("0\n")
	}
	readFile := func(path string) ([]byte, error) {
		content, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("missing synthetic observation")
		}
		return content, nil
	}
	limits, err := currentCgroupLimits(readFile)
	if err != nil || len(limits) != 3 {
		t.Fatal("complete ancestor traversal missing")
	}
	delete(files, "/sys/fs/cgroup/synthetic/memory.max")
	if _, err := currentCgroupLimits(readFile); err == nil {
		t.Fatal("missing parent memory.max admitted")
	}
	if _, err := currentCgroupLimits(func(string) ([]byte, error) { return nil, errors.New("synthetic read failure") }); err == nil {
		t.Fatal("unreadable membership admitted")
	}
}

// The real unified hierarchy root lacks both memory controller files, while all
// nonroot controller observations remain mandatory and finite limits enforced.
func TestUnifiedHierarchyRootMissingControllerFilesPasses(t *testing.T) {
	files := map[string][]byte{
		"/proc/self/cgroup":                             []byte("0::/synthetic/scope\n"),
		"/sys/fs/cgroup/synthetic/scope/memory.max":     []byte("8589934592"),
		"/sys/fs/cgroup/synthetic/scope/memory.current": []byte("16777216"),
		"/sys/fs/cgroup/synthetic/memory.max":           []byte("8589934592"),
		"/sys/fs/cgroup/synthetic/memory.current":       []byte("16777216"),
	}
	readFile := func(path string) ([]byte, error) {
		content, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return content, nil
	}
	limits, err := currentCgroupLimits(readFile)
	if err != nil || len(limits) != 3 || limits[2].LimitBytes != -1 {
		t.Fatal("real unified root without controller files was rejected")
	}
	if _, err := validateMemoryBudget(syntheticMemoryBudget(), limits); err != nil {
		t.Fatal(err)
	}
	delete(files, "/sys/fs/cgroup/synthetic/memory.max")
	if _, err := currentCgroupLimits(readFile); err == nil {
		t.Fatal("missing nonroot controller file was incorrectly exempted")
	}
}

// A namespaced root with real memory controller files retains its finite limit.
func TestNamespaceRootFiniteThreeGibibytesFails(t *testing.T) {
	files := map[string][]byte{
		"/proc/self/cgroup":                       []byte("0::/synthetic\n"),
		"/sys/fs/cgroup/synthetic/memory.max":     []byte("8589934592"),
		"/sys/fs/cgroup/synthetic/memory.current": []byte("16777216"),
		"/sys/fs/cgroup/memory.max":               []byte("3221225472"),
		"/sys/fs/cgroup/memory.current":           []byte("16777216"),
	}
	limits, err := currentCgroupLimits(func(path string) ([]byte, error) {
		content, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return content, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateMemoryBudget(syntheticMemoryBudget(), limits); err == nil {
		t.Fatal("finite namespaced root limit was ignored")
	}
}

// Permission errors and a partially missing root do not prove unlimited memory.
func TestHierarchyRootUncertainOrPartlyMissingControllerFilesFail(t *testing.T) {
	for _, failure := range []error{os.ErrPermission, os.ErrNotExist} {
		readFile := func(path string) ([]byte, error) {
			if path == "/proc/self/cgroup" {
				return []byte("0::/\n"), nil
			}
			if path == "/sys/fs/cgroup/memory.max" {
				return nil, failure
			}
			return []byte("0"), nil
		}
		if _, err := currentCgroupLimits(readFile); err == nil {
			t.Fatal("uncertain or partly missing root admitted")
		}
	}
}
