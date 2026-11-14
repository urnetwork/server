// Pure management-scope admission includes evidence tmpfs and ancestor pressure.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Evaluation, evidence, and reserve remain fixed; the scope must cover evidence
// plus at least three gibibytes of non-evidence management allocations.
type memoryBudget struct {
	EvaluationLimitBytes    int64 `json:"evaluation_limit_bytes"`
	EvidenceLimitBytes      int64 `json:"evidence_limit_bytes"`
	ManagementReserveBytes  int64 `json:"management_reserve_bytes"`
	NonEvidenceMinimumBytes int64 `json:"non_evidence_minimum_bytes"`
	ProposedScopeLimitBytes int64 `json:"proposed_scope_limit_bytes"`
}

// The first entry is the current cgroup; remaining entries are all ancestors.
// LimitBytes=-1 means max; current usage always remains a nonnegative byte count.
type cgroupLimit struct {
	Path         string
	LimitBytes   int64
	CurrentBytes int64
}

// Pure/injectable gate: a configured eight-gibibyte child cannot escape a
// three-gibibyte parent. Existing own-scope usage is not double-counted.
func validateMemoryBudget(budget memoryBudget, limits []cgroupLimit) (int64, error) {
	if budget.EvaluationLimitBytes != frozenEvaluationBytes || budget.EvidenceLimitBytes != frozenEvidenceBytes || budget.ManagementReserveBytes != frozenManagementReserveBytes || budget.NonEvidenceMinimumBytes < minimumNonEvidenceBytes {
		return 0, errors.New("frozen evaluation/evidence/reserve or management allowance changed")
	}
	if budget.NonEvidenceMinimumBytes > budget.ManagementReserveBytes-budget.EvidenceLimitBytes {
		return 0, errors.New("management scope exceeds the unchanged reserve")
	}
	requiredBytes := budget.EvidenceLimitBytes + budget.NonEvidenceMinimumBytes
	if budget.ProposedScopeLimitBytes < requiredBytes || budget.ProposedScopeLimitBytes > budget.ManagementReserveBytes {
		return 0, errors.New("management scope must cover evidence plus non-evidence allowance within the frozen reserve")
	}
	if len(limits) == 0 {
		return 0, errors.New("complete current-cgroup ancestor limits required")
	}
	if limits[0].CurrentBytes > budget.NonEvidenceMinimumBytes {
		return 0, errors.New("pre-start management usage exceeds its non-evidence allowance")
	}
	for index, limit := range limits {
		if limit.Path == "" || limit.CurrentBytes < 0 || limit.LimitBytes < -1 {
			return 0, errors.New("invalid cgroup memory observation")
		}
		if index > 0 && filepath.Dir(limits[index-1].Path) != limit.Path {
			return 0, errors.New("incomplete or unordered cgroup ancestor observations")
		}
		if index > 0 && limit.CurrentBytes < limits[0].CurrentBytes {
			return 0, errors.New("inconsistent ancestor memory observation")
		}
		if limit.LimitBytes == -1 {
			continue
		}
		externalBytes := limit.CurrentBytes - limits[0].CurrentBytes
		if limit.LimitBytes < externalBytes || limit.LimitBytes-externalBytes < budget.ProposedScopeLimitBytes {
			return 0, fmt.Errorf("cgroup cannot cover management evidence plus non-evidence allowance: %s", limit.Path)
		}
	}
	return requiredBytes, nil
}

// Read-only cgroup v2 observation is injected so tests need no systemd or mounts.
func currentCgroupLimits(readFile func(string) ([]byte, error)) ([]cgroupLimit, error) {
	content, err := readFile("/proc/self/cgroup")
	if err != nil {
		return nil, errors.New("cannot read current cgroup")
	}
	cgroupPath := ""
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "0::") {
			if cgroupPath != "" {
				return nil, errors.New("ambiguous cgroup v2 membership")
			}
			cgroupPath = strings.TrimPrefix(line, "0::")
		}
	}
	if !filepath.IsAbs(cgroupPath) || filepath.Clean(cgroupPath) != cgroupPath {
		return nil, errors.New("canonical cgroup v2 membership required")
	}
	root := "/sys/fs/cgroup"
	path := filepath.Join(root, strings.TrimPrefix(cgroupPath, "/"))
	limits := []cgroupLimit{}
	for {
		maximum, maximumErr := readFile(filepath.Join(path, "memory.max"))
		current, currentErr := readFile(filepath.Join(path, "memory.current"))
		// The real unified hierarchy root has no memory controller files. A
		// namespaced root with files is not exempt from finite limits.
		if path == root && errors.Is(maximumErr, os.ErrNotExist) && errors.Is(currentErr, os.ErrNotExist) {
			currentBytes := int64(0)
			if len(limits) > 0 {
				currentBytes = limits[len(limits)-1].CurrentBytes
			}
			limits = append(limits, cgroupLimit{Path: path, LimitBytes: -1, CurrentBytes: currentBytes})
			break
		}
		if maximumErr != nil {
			return nil, errors.New("cannot read every cgroup ancestor memory.max")
		}
		limitBytes := int64(-1)
		if strings.TrimSpace(string(maximum)) != "max" {
			limitBytes, err = strconv.ParseInt(strings.TrimSpace(string(maximum)), 10, 64)
			if err != nil || limitBytes < 0 {
				return nil, errors.New("invalid cgroup memory.max")
			}
		}
		if currentErr != nil {
			return nil, errors.New("cannot read every cgroup ancestor memory.current")
		}
		currentBytes, err := strconv.ParseInt(strings.TrimSpace(string(current)), 10, 64)
		if err != nil || currentBytes < 0 {
			return nil, errors.New("invalid cgroup memory.current")
		}
		limits = append(limits, cgroupLimit{Path: path, LimitBytes: limitBytes, CurrentBytes: currentBytes})
		if path == root {
			break
		}
		path = filepath.Dir(path)
	}
	return limits, nil
}
