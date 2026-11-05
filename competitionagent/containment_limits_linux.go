// Admission installs fixed aggregate ceilings on the exact precommitted slice
// inventory. Positive kernel readback, not a successful write, grants launch.
package competitionagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"syscall"
)

// This metadata is stored and hashed in the root-only plan before any launch.
// Emergency cleanup needs its identities, never healthy current kernel limits.
type runSliceResources struct {
	ResourcePolicySha256 string              `json:"resource_policy_sha256"`
	Identity             runResourceIdentity `json:"identity"`
	BuildTasksMaximum    int64               `json:"build_tasks_maximum"`
	CpuPeriodMicros      int64               `json:"cpu_period_micros"`
}

// A parent limit covers all descendant containers and subprocesses together.
// SwapBytes is zero: Docker's memory-swap == memory policy permits no swap.
type runSliceLimits struct {
	MemoryBytes     int64  `json:"memory_bytes"`
	SwapBytes       int64  `json:"swap_bytes"`
	TasksMaximum    int64  `json:"tasks_maximum"`
	CpuQuotaMicros  int64  `json:"cpu_quota_micros"`
	CpuPeriodMicros int64  `json:"cpu_period_micros"`
	CpuSet          string `json:"cpu_set"`
}

// A delegated parent could let the worker replace or widen a verified limit.
// This admission-only check never disables emergency kill/freeze access.
func rootOwnedContainmentNode(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat != nil && stat.Uid == 0 && info.Mode().Perm()&0022 == 0
}

// The role ceilings are read from the pinned helper, not evaluator arguments.
// All sums are checked before arithmetic; no missing value means unlimited.
func validateRunResourceLimits(resource runResourceIdentity) error {
	settings := DefaultDockerProxySettings()
	for _, value := range []int64{resource.RunnerMemoryLimitBytes, resource.PostgresMemoryLimitBytes, resource.RedisMemoryLimitBytes,
		resource.BuildMemoryLimitBytes, resource.ScorerMemoryLimitBytes, resource.PreflightMemoryLimitBytes} {
		if value <= 0 || value > settings.MaximumContainerMemoryBytes {
			return errors.New("pinned resource helper omitted a finite role memory ceiling")
		}
	}
	for _, value := range []int64{resource.RunnerPidsLimit, resource.PostgresPidsLimit, resource.RedisPidsLimit, resource.ScorerPidsLimit, resource.PreflightPidsLimit} {
		if value <= 0 || value > settings.MaximumContainerPids {
			return errors.New("pinned resource helper omitted a finite role task ceiling")
		}
	}
	if resource.RunnerMemoryLimitBytes > math.MaxInt64-resource.PostgresMemoryLimitBytes ||
		resource.RunnerMemoryLimitBytes+resource.PostgresMemoryLimitBytes > math.MaxInt64-resource.RedisMemoryLimitBytes ||
		resource.ActiveMemoryLimitBytes != resource.RunnerMemoryLimitBytes+resource.PostgresMemoryLimitBytes+resource.RedisMemoryLimitBytes ||
		resource.RunnerPidsLimit > math.MaxInt64-resource.PostgresPidsLimit ||
		resource.RunnerPidsLimit+resource.PostgresPidsLimit > math.MaxInt64-resource.RedisPidsLimit ||
		resource.ActivePidsLimit != resource.RunnerPidsLimit+resource.PostgresPidsLimit+resource.RedisPidsLimit {
		return errors.New("pinned resource helper aggregate differs from its frozen role ceilings")
	}
	// Docker's build memory string must describe the same byte ceiling as the
	// parent; a mismatched suffix cannot silently widen either boundary.
	bytes, err := pinnedResourceMemoryBytes(resource.BuildMemoryLimit)
	if err != nil || bytes != resource.BuildMemoryLimitBytes {
		return errors.New("pinned build memory string differs from its byte ceiling")
	}
	return nil
}

// Both immutable owner plans and pinned helper identities use one unit parser.
func pinnedResourceMemoryBytes(memory string) (int64, error) {
	scale := int64(1)
	if !dockerProxyMemoryPattern.MatchString(memory) {
		return 0, errors.New("pinned build memory ceiling is absent or malformed")
	}
	switch memory[len(memory)-1] {
	case 'k', 'K':
		scale, memory = 1024, memory[:len(memory)-1]
	case 'm', 'M':
		scale, memory = 1024*1024, memory[:len(memory)-1]
	case 'g', 'G':
		scale, memory = 1024*1024*1024, memory[:len(memory)-1]
	}
	count, err := strconv.ParseInt(memory, 10, 64)
	if err != nil || count <= 0 || count > math.MaxInt64/scale {
		return 0, errors.New("pinned build memory string is not finite")
	}
	return count * scale, nil
}

// No new path or role can be supplied by metadata: the complete name inventory
// is independently derived from the authenticated job before this conversion.
func prepareRunSliceLimits(plan cleanupPlan, config Config) ([]runSliceLimits, error) {
	resource := plan.SliceResources
	if resource == nil || !digestPattern.MatchString(resource.ResourcePolicySha256) || resource.ResourcePolicySha256 != config.DockerRole.ResourcePolicy.Sha256 ||
		resource.BuildTasksMaximum <= 0 || resource.BuildTasksMaximum > resource.Identity.ActivePidsLimit {
		return nil, errors.New("containment plan omitted its pinned finite resource authority")
	}
	if err := validateRunResourceIdentity(resource.Identity); err != nil {
		return nil, err
	}
	if plan.DockerOwner != nil && (resource.Identity.EvaluationCpuSet != plan.DockerOwner.CpuSet || resource.Identity.ManagementCpuSet != plan.ManagementCpuSet ||
		resource.Identity.BuildMemoryLimit != plan.DockerOwner.BuildMemory || resource.ResourcePolicySha256 != plan.DockerOwner.ResourcePolicySha256) {
		return nil, errors.New("containment limits differ from their authenticated Docker role")
	}
	limits := make([]runSliceLimits, 0, len(plan.CgroupNames))
	for _, name := range plan.CgroupNames {
		limit := runSliceLimits{MemoryBytes: resource.Identity.ActiveMemoryLimitBytes, TasksMaximum: resource.Identity.ActivePidsLimit,
			CpuSet: resource.Identity.EvaluationCpuSet, CpuPeriodMicros: resource.CpuPeriodMicros}
		switch {
		case cleanupBuildSlicePattern.MatchString(name):
			limit.MemoryBytes, limit.TasksMaximum = resource.Identity.BuildMemoryLimitBytes, resource.BuildTasksMaximum
		case strings.HasSuffix(name, "-preflight.slice"):
			limit.MemoryBytes, limit.TasksMaximum, limit.CpuSet = resource.Identity.PreflightMemoryLimitBytes, resource.Identity.PreflightPidsLimit, resource.Identity.ManagementCpuSet
		case strings.HasSuffix(name, "-score.slice") || strings.Contains(name, "-live-"):
			limit.MemoryBytes, limit.TasksMaximum = resource.Identity.ScorerMemoryLimitBytes, resource.Identity.ScorerPidsLimit
		}
		cpus, err := dockerPolicyCpuSet(limit.CpuSet, DefaultDockerProxySettings().MaximumCpuIndex)
		if err != nil || limit.CpuPeriodMicros <= 0 || limit.CpuPeriodMicros > math.MaxInt64/int64(len(cpus)) {
			return nil, errors.New("containment CPU role has no finite quota")
		}
		limit.CpuQuotaMicros = int64(len(cpus)) * limit.CpuPeriodMicros
		if err := validateRunSliceLimits(limit); err != nil {
			return nil, err
		}
		limits = append(limits, limit)
	}
	return limits, nil
}

// Period bounds are the cgroup v2 cpu.max kernel interface, not policy defaults.
// A quota larger than its exact assigned processor set cannot be requested.
func validateRunSliceLimits(limit runSliceLimits) error {
	cpus, err := dockerPolicyCpuSet(limit.CpuSet, DefaultDockerProxySettings().MaximumCpuIndex)
	if err != nil || limit.MemoryBytes <= 0 || limit.SwapBytes != 0 || limit.TasksMaximum <= 0 ||
		limit.CpuPeriodMicros < 1000 || limit.CpuPeriodMicros > 1000000 || limit.CpuQuotaMicros <= 0 ||
		limit.CpuQuotaMicros != int64(len(cpus))*limit.CpuPeriodMicros {
		return errors.New("containment parent requested an unbounded or mismatched resource limit")
	}
	return nil
}

// Manager properties own the limits too: a later child-scope realization must
// not restore the unit's default unlimited settings over direct cgroup writes.
// Callers cannot choose a program, property, option or non-run unit identity.
func systemdSliceLimitArguments(name string, limit runSliceLimits) ([]string, error) {
	if _, err := cleanupSlicePath(name); err != nil {
		return nil, err
	}
	if err := validateRunSliceLimits(limit); err != nil {
		return nil, err
	}
	return []string{"set-property", "--runtime", "--", name,
		"MemoryMax=" + strconv.FormatInt(limit.MemoryBytes, 10),
		"MemorySwapMax=" + strconv.FormatInt(limit.SwapBytes, 10),
		"TasksMax=" + strconv.FormatInt(limit.TasksMaximum, 10),
		fmt.Sprintf("CPUQuota=%d%%", limit.CpuQuotaMicros/limit.CpuPeriodMicros*100),
		fmt.Sprintf("CPUQuotaPeriodSec=%dus", limit.CpuPeriodMicros), "AllowedCPUs=" + limit.CpuSet}, nil
}

// Every parent passed the complete census first. A fresh local census refuses
// population, freezing or delegated authority before its manager write as well.
func (self *hostCleanupCgroups) SetLimits(parent context.Context, name string, limit runSliceLimits) error {
	arguments, err := systemdSliceLimitArguments(name, limit)
	if err != nil {
		return err
	}
	if self.limitCommandTimeout <= 0 {
		return errors.New("containment manager command has no finite owner timeout")
	}
	ctx, cancel := context.WithTimeout(parent, self.limitCommandTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := self.openWithLimitAuthority(name, "cgroup.events", os.O_RDONLY, true)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	frozen, freezeErr := self.Frozen(ctx, name)
	empty, emptyErr := self.Empty(ctx, name)
	if freezeErr != nil || emptyErr != nil || frozen || !empty {
		return errors.New("containment parent changed before its manager limit write")
	}
	return self.applyManagerLimitArguments(ctx, arguments)
}

// Both preparation and frozen build retirement use the same fixed manager
// vocabulary. Their different population/freeze prerequisites stay separate.
func (self *hostCleanupCgroups) applyManagerLimitArguments(ctx context.Context, arguments []string) error {
	command := self.limitCommand
	if command == nil {
		command = func(ctx context.Context, arguments []string) error {
			if err := protectedRootPath("/usr/bin/systemctl", false, false); err != nil {
				return err
			}
			process := exec.CommandContext(ctx, "/usr/bin/systemctl", arguments...)
			process.Env = controlEnvironment()
			process.Stdout, process.Stderr = &discardWriter{}, &discardWriter{}
			if err := process.Run(); err != nil {
				return errors.New("exact runtime containment limit properties were not acknowledged")
			}
			return ctx.Err()
		}
	}
	if err := command(ctx, arguments); err != nil {
		return err
	}
	return ctx.Err()
}

// Read all actual limits as well as the effective processor set. An inherited
// subset, offline processor, missing controller or unlimited reply refuses
// admission even if every previous write returned success.
func (self *hostCleanupCgroups) VerifyLimits(ctx context.Context, name string, limit runSliceLimits) error {
	if err := validateRunSliceLimits(limit); err != nil {
		return err
	}
	for _, property := range []struct {
		name  string
		value string
	}{
		{name: "memory.max", value: strconv.FormatInt(limit.MemoryBytes, 10)},
		{name: "memory.swap.max", value: strconv.FormatInt(limit.SwapBytes, 10)},
		{name: "pids.max", value: strconv.FormatInt(limit.TasksMaximum, 10)},
		{name: "cpu.max", value: fmt.Sprintf("%d %d", limit.CpuQuotaMicros, limit.CpuPeriodMicros)},
		{name: "cpuset.cpus", value: limit.CpuSet},
		{name: "cpuset.cpus.effective", value: limit.CpuSet},
	} {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := self.open(name, property.name, os.O_RDONLY)
		if err != nil {
			return err
		}
		if self.maximumBytes <= 0 {
			file.Close()
			return errors.New("invalid finite cgroup limit read bound")
		}
		content, readErr := io.ReadAll(io.LimitReader(file, self.maximumBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || int64(len(content)) > self.maximumBytes {
			return errors.New("cgroup limit acknowledgment is unavailable or exceeds its bound")
		}
		value := strings.TrimSuffix(string(content), "\n")
		if property.name == "cpuset.cpus" || property.name == "cpuset.cpus.effective" {
			actual, err := dockerPolicyCpuSet(value, DefaultDockerProxySettings().MaximumCpuIndex)
			expected, expectedErr := dockerPolicyCpuSet(property.value, DefaultDockerProxySettings().MaximumCpuIndex)
			if err != nil || expectedErr != nil || !reflect.DeepEqual(actual, expected) {
				return errors.New("cgroup effective processors differ from the exact pinned role")
			}
		} else if value != property.value {
			return errors.New("cgroup resource ceiling was not positively acknowledged")
		}
	}
	return ctx.Err()
}
