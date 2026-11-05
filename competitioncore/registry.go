// Registry authority consists of complete root-precommitted parents, never
// labels discovered from a candidate or a substring/prefix cgroup heuristic.
package competitioncore

import (
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
)

// These are the four prospective private-engine roles in the frozen source.
// Native qualification must bind actual unit identity before provisioning.
func EngineCgroups() []string {
	return []string{"/system.slice/urnetwork-private-engine.service", "/system.slice/urnetwork-private-containerd.service",
		"/system.slice/urnetwork-private-engine-network-namespace.service", "/system.slice/urnetwork-private-engine-network.service"}
}

// A slice expands into exact hierarchy components, not an ancestor wildcard.
func sliceCgroup(name string) string {
	atoms := strings.Split(strings.TrimSuffix(name, ".slice"), "-")
	parts := make([]string, 0, len(atoms))
	for index := range atoms {
		parts = append(parts, strings.Join(atoms[:index+1], "-")+".slice")
	}
	return "/" + path.Join(parts...)
}

// Full-R9 contains build, two nine-stage/score roles, nine live roles and one
// bounded preflight role. Omitting any one parent cannot qualify a registry.
func RunCgroups(jobId string, attempt int, buildKey string) ([]string, error) {
	if !validBootId(jobId) || attempt <= 0 || !validHex(buildKey, 32) {
		return nil, errors.New("invalid exact core run identity")
	}
	groups := []string{sliceCgroup("urnetwork-submission-build-" + buildKey + ".slice")}
	token := digest([]byte(fmt.Sprintf("%s:%d", jobId, attempt)))[:32]
	prefix := "urnetwork-evaluation-" + token + "-"
	for _, role := range []string{"baseline", "candidate"} {
		for stage := 1; stage <= 9; stage++ {
			groups = append(groups, sliceCgroup(fmt.Sprintf("%s%s-%02d.slice", prefix, role, stage)))
		}
		groups = append(groups, sliceCgroup(prefix+role+"-score.slice"))
	}
	for stage := 1; stage <= 9; stage++ {
		groups = append(groups, sliceCgroup(fmt.Sprintf("urnetwork-evaluation-%s-live-%02d.slice", strings.ReplaceAll(jobId, "-", ""), stage)))
	}
	return append(groups, sliceCgroup(prefix+"preflight.slice")), nil
}

// Kernel paths are canonical absolute components; no parent traversal,
// escaped path, empty atom or excessively deep hierarchy grants authority.
func validCgroup(value string, settings Settings) bool {
	if !strings.HasPrefix(value, "/") || path.Clean(value) != value || len(value) > settings.MaximumCgroupBytes ||
		settings.MaximumCgroupBytes <= 0 || settings.MaximumCgroupDepth <= 0 || strings.ContainsAny(value, "\x00\r\n\\") {
		return false
	}
	return len(strings.Split(strings.TrimPrefix(value, "/"), "/")) <= settings.MaximumCgroupDepth
}

// Matching is equality or a complete slash-separated descendant component.
func cgroupWithin(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, parent+"/")
}

// A missing/ambiguous boot, inventory, deadline or generation is not ordinary
// authority. Expired runs deliberately remain covered until positive cleanup.
func ValidateRegistry(registry Registry, settings Settings) error {
	if registry.Schema != Schema || registry.Generation == 0 || !validBootId(registry.BootId) || registry.HandlerCgroup != HandlerCgroup ||
		!reflect.DeepEqual(registry.EngineCgroups, EngineCgroups()) || settings.MaximumRuns <= 0 || len(registry.Runs) > settings.MaximumRuns || settings.MaximumJobLifetime <= 0 {
		return errors.New("core registry is incomplete or outside its fixed root role")
	}
	groups := append(EngineCgroups(), HandlerCgroup)
	seen := map[string]bool{}
	for _, run := range registry.Runs {
		expected, err := RunCgroups(run.JobId, run.Attempt, run.BuildKey)
		if err != nil || !validHex(run.RunId, 64) || seen[run.RunId] || !validHex(run.PlanSha256, 64) || !sameGroups(run.Cgroups, expected) || run.StartedAt.IsZero() ||
			!run.DeadlineAt.After(run.StartedAt) || run.DeadlineAt.Sub(run.StartedAt) > settings.MaximumJobLifetime {
			return errors.New("core registry omitted an exact precommitted full-R9 run")
		}
		seen[run.RunId] = true
		groups = append(groups, run.Cgroups...)
	}
	for index, group := range groups {
		if !validCgroup(group, settings) {
			return errors.New("core registry has an invalid exact component path")
		}
		for _, other := range groups[:index] {
			if cgroupWithin(other, group) || cgroupWithin(group, other) {
				return errors.New("core registry has duplicate or overlapping scope authority")
			}
		}
	}
	return nil
}

// The authenticated cleanup plan may order roles differently, but cannot omit,
// duplicate or replace any parent in the independently derived complete set.
func sameGroups(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	seen := map[string]bool{}
	for _, group := range first {
		if seen[group] {
			return false
		}
		seen[group] = true
	}
	for _, group := range second {
		if !seen[group] {
			return false
		}
	}
	return true
}

// The boot identity is public kernel metadata, not a host/customer identifier
// copied into fixtures. Tests always use visibly synthetic values.
func validBootId(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	return validHex(strings.ReplaceAll(value, "-", ""), 32)
}

// The caller must validate the snapshot first and recheck it before handoff.
func scopedCgroup(registry Registry, cgroup string, settings Settings) (bool, error) {
	if err := ValidateRegistry(registry, settings); err != nil || !validCgroup(cgroup, settings) {
		return false, errors.New("core classification lacks exact registry/cgroup facts")
	}
	for _, parent := range append(append([]string(nil), registry.EngineCgroups...), registry.HandlerCgroup) {
		if cgroupWithin(parent, cgroup) {
			return true, nil
		}
	}
	for _, run := range registry.Runs {
		for _, parent := range run.Cgroups {
			if cgroupWithin(parent, cgroup) {
				return true, nil
			}
		}
	}
	return false, nil
}
