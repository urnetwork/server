// Retired BuildKit memory remains charged to explicit measured headroom. Only
// a frozen, empty, finitely capped parent can precede candidate measurement.
package competitionagent

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// This root-committed policy preserves every frozen scoring/reserve ceiling.
// Its retention cap is page-rounded measured headroom, never an extra reserve.
type BuildRetirementPolicy struct {
	Schema                     int            `json:"schema"`
	ResourcePolicySha256       string         `json:"resource_policy_sha256"`
	BuildCgroupName            string         `json:"build_cgroup_name"`
	HostMemoryBytes            int64          `json:"host_memory_bytes"`
	ActiveMemoryBytes          int64          `json:"active_memory_bytes"`
	EvidenceMemoryBytes        int64          `json:"evidence_memory_bytes"`
	ManagementReserveBytes     int64          `json:"management_reserve_bytes"`
	OriginalBuildMemoryBytes   int64          `json:"original_build_memory_bytes"`
	PageBytes                  int64          `json:"page_bytes"`
	RetainedMemoryMaximumBytes int64          `json:"retained_memory_maximum_bytes"`
	Limits                     runSliceLimits `json:"limits"`
}

// Every read, join and manager operation has an independent owner-local bound.
// Nonpositive values refuse retirement, not the independent emergency reap.
type BuildRetirementSettings struct {
	Timeout              time.Duration
	CommandTimeout       time.Duration
	PollInterval         time.Duration
	NativeBytes          int64
	MaximumStatKeys      int
	MaximumStatNameBytes int
}

// Retirement may reclaim a bounded build cache but cannot reset the job clock.
func DefaultBuildRetirementSettings() BuildRetirementSettings {
	return BuildRetirementSettings{Timeout: 2 * time.Minute, CommandTimeout: 10 * time.Second, PollInterval: 25 * time.Millisecond,
		NativeBytes: 128 * 1024, MaximumStatKeys: 256, MaximumStatNameBytes: 64}
}

// The entire observed charge is budgeted, regardless of page-cache heuristics.
// Stats are retained as bounded evidence, not an atomic subtraction proof.
type buildRetirementMemory struct {
	CurrentBytes     uint64            `json:"current_bytes"`
	SwapCurrentBytes uint64            `json:"swap_current_bytes"`
	StatSha256       string            `json:"stat_sha256"`
	Counters         map[string]uint64 `json:"counters"`
}

// Private seams replace the kernel/manager only, never policy derivation.
type buildRetirementKernel interface {
	Freeze(context.Context, string) error
	Kill(context.Context, string) error
	Frozen(context.Context, string) (bool, error)
	Empty(context.Context, string) (bool, error)
	LowerBuildLimit(context.Context, string, runSliceLimits) error
	VerifyLimits(context.Context, string, runSliceLimits) error
	RetirementMemory(context.Context, string, BuildRetirementSettings) (buildRetirementMemory, error)
}

// Root construction always uses native cgroup v2 and a typed manager operation.
type hostBuildRetirement struct {
	settings BuildRetirementSettings
	kernel   buildRetirementKernel
}

// Construction is inert; actual mutation belongs only to the durable owner.
func newHostBuildRetirement(boundary Pin, settings BuildRetirementSettings) *hostBuildRetirement {
	return &hostBuildRetirement{settings: settings, kernel: &hostCleanupCgroups{rootPath: "/sys/fs/cgroup",
		maximumBytes: settings.NativeBytes, requireCgroupFs: true, limitCommandTimeout: settings.CommandTimeout, managerBoundary: boundary, managerMaximumBytes: settings.NativeBytes}}
}

// The same fields produce the exact same cap after coordinator/agent restart.
func newBuildRetirementPolicy(resource runSliceResources, name string, pageBytes int64) (*BuildRetirementPolicy, error) {
	identity := resource.Identity
	if err := validateRunResourceIdentity(identity); err != nil {
		return nil, err
	}
	policy := &BuildRetirementPolicy{Schema: Schema, ResourcePolicySha256: resource.ResourcePolicySha256, BuildCgroupName: name,
		HostMemoryBytes: identity.HostMemoryBytes, ActiveMemoryBytes: identity.ActiveMemoryLimitBytes,
		EvidenceMemoryBytes: identity.EvidenceMemoryLimitBytes, ManagementReserveBytes: identity.ManagementReserveBytes,
		OriginalBuildMemoryBytes: identity.BuildMemoryLimitBytes, PageBytes: pageBytes,
		Limits: runSliceLimits{TasksMaximum: resource.BuildTasksMaximum, CpuSet: identity.EvaluationCpuSet, CpuPeriodMicros: resource.CpuPeriodMicros}}
	cap, err := retiredBuildMemoryMaximum(*policy)
	if err != nil {
		return nil, err
	}
	policy.RetainedMemoryMaximumBytes, policy.Limits.MemoryBytes = cap, cap
	cpus, err := dockerPolicyCpuSet(policy.Limits.CpuSet, DefaultDockerProxySettings().MaximumCpuIndex)
	if err != nil || policy.Limits.CpuPeriodMicros < 1000 || policy.Limits.CpuPeriodMicros > 1000000 {
		return nil, errors.New("retired build has no finite pinned CPU role")
	}
	policy.Limits.CpuQuotaMicros = int64(len(cpus)) * policy.Limits.CpuPeriodMicros
	if err := validateBuildRetirementPolicy(*policy); err != nil {
		return nil, err
	}
	return policy, nil
}

// Subtract in order to reject overflow/negative headroom before rounding.
func retiredBuildMemoryMaximum(policy BuildRetirementPolicy) (int64, error) {
	if policy.HostMemoryBytes <= 0 || policy.ActiveMemoryBytes <= 0 || policy.EvidenceMemoryBytes <= 0 || policy.ManagementReserveBytes <= 0 ||
		policy.OriginalBuildMemoryBytes <= 0 || policy.PageBytes <= 0 {
		return 0, errors.New("retired build memory ledger omitted a finite measured value")
	}
	headroom := policy.HostMemoryBytes
	for _, charge := range []int64{policy.ActiveMemoryBytes, policy.EvidenceMemoryBytes, policy.ManagementReserveBytes} {
		if headroom < charge {
			return 0, errors.New("retired build cannot consume the frozen management reserve")
		}
		headroom -= charge
	}
	if headroom > policy.OriginalBuildMemoryBytes {
		headroom = policy.OriginalBuildMemoryBytes
	}
	headroom = headroom / policy.PageBytes * policy.PageBytes
	if headroom <= 0 {
		return 0, errors.New("host has no positive page-rounded retained build headroom")
	}
	return headroom, nil
}

// Only this host's original page identity and exact build parent are valid.
func validateBuildRetirementPolicy(policy BuildRetirementPolicy) error {
	cap, err := retiredBuildMemoryMaximum(policy)
	if err != nil || policy.Schema != Schema || !digestPattern.MatchString(policy.ResourcePolicySha256) ||
		!cleanupBuildSlicePattern.MatchString(policy.BuildCgroupName) || policy.PageBytes != int64(os.Getpagesize()) ||
		policy.RetainedMemoryMaximumBytes != cap || policy.Limits.MemoryBytes != cap {
		return errors.New("retired build policy differs from its explicit measured ledger")
	}
	return validateRunSliceLimits(policy.Limits)
}

// No syscall or manager wait holds the proxy's state/publication lock. The
// independent emergency path can freeze/kill every parent throughout this join.
func (self *hostBuildRetirement) Retire(parent context.Context, policy BuildRetirementPolicy) (buildRetirementMemory, error) {
	if err := self.validate(policy); err != nil {
		return buildRetirementMemory{}, err
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.Timeout)
	defer cancel()
	if err := self.kernel.Freeze(ctx, policy.BuildCgroupName); err != nil {
		return buildRetirementMemory{}, err
	}
	if err := self.kernel.Kill(ctx, policy.BuildCgroupName); err != nil {
		return buildRetirementMemory{}, err
	}
	for {
		frozen, freezeErr := self.kernel.Frozen(ctx, policy.BuildCgroupName)
		empty, emptyErr := self.kernel.Empty(ctx, policy.BuildCgroupName)
		if freezeErr != nil || emptyErr != nil {
			return buildRetirementMemory{}, errors.Join(freezeErr, emptyErr)
		}
		if frozen && empty {
			break
		}
		select {
		case <-ctx.Done():
			return buildRetirementMemory{}, ctx.Err()
		case <-time.After(self.settings.PollInterval):
		}
	}
	if err := self.kernel.LowerBuildLimit(ctx, policy.BuildCgroupName, policy.Limits); err != nil {
		return buildRetirementMemory{}, err
	}
	for {
		memory, err := self.Verify(ctx, policy)
		if err == nil {
			return memory, nil
		}
		if !errors.Is(err, errBuildRetirementChargePending) {
			return buildRetirementMemory{}, err
		}
		select {
		case <-ctx.Done():
			return buildRetirementMemory{}, ctx.Err()
		case <-time.After(self.settings.PollInterval):
		}
	}
}

// A configured cap is insufficient: current charge, zero swap, frozen state
// and emptiness must all be positively observed before any measurement create.
func (self *hostBuildRetirement) Verify(ctx context.Context, policy BuildRetirementPolicy) (buildRetirementMemory, error) {
	if err := self.validate(policy); err != nil {
		return buildRetirementMemory{}, err
	}
	if err := self.kernel.VerifyLimits(ctx, policy.BuildCgroupName, policy.Limits); err != nil {
		return buildRetirementMemory{}, err
	}
	memory, err := self.kernel.RetirementMemory(ctx, policy.BuildCgroupName, self.settings)
	if err != nil {
		return buildRetirementMemory{}, err
	}
	if err := validateBuildRetirementMemory(memory, self.settings); err != nil {
		return buildRetirementMemory{}, err
	}
	frozen, freezeErr := self.kernel.Frozen(ctx, policy.BuildCgroupName)
	empty, emptyErr := self.kernel.Empty(ctx, policy.BuildCgroupName)
	if freezeErr != nil || emptyErr != nil || !frozen || !empty {
		return buildRetirementMemory{}, errors.New("retired build lost its frozen empty kernel boundary")
	}
	if memory.CurrentBytes > uint64(policy.RetainedMemoryMaximumBytes) || memory.SwapCurrentBytes != 0 {
		return buildRetirementMemory{}, errBuildRetirementChargePending
	}
	return memory, ctx.Err()
}

// Missing settings cannot disable a mandatory phase boundary.
func (self *hostBuildRetirement) validate(policy BuildRetirementPolicy) error {
	if self == nil || self.kernel == nil || self.settings.Timeout <= 0 || self.settings.CommandTimeout <= 0 ||
		self.settings.PollInterval <= 0 || self.settings.NativeBytes <= 0 || self.settings.MaximumStatKeys <= 0 || self.settings.MaximumStatNameBytes <= 0 {
		return errors.New("retired build owner has invalid finite native bounds")
	}
	return validateBuildRetirementPolicy(policy)
}

// Only a bounded asynchronous charge convergence is retryable inside this join.
var errBuildRetirementChargePending = errors.New("retired build charge has not converged below its explicit cap")

// Lowering an already frozen empty build parent must never thaw or reuse it.
func (self *hostCleanupCgroups) LowerBuildLimit(parent context.Context, name string, limit runSliceLimits) error {
	if !cleanupBuildSlicePattern.MatchString(name) || self.limitCommandTimeout <= 0 {
		return errors.New("retired build limit has no exact finite manager owner")
	}
	arguments, err := systemdSliceLimitArguments(name, limit)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, self.limitCommandTimeout)
	defer cancel()
	file, err := self.openWithLimitAuthority(name, "cgroup.events", os.O_RDONLY, true)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	frozen, freezeErr := self.Frozen(ctx, name)
	empty, emptyErr := self.Empty(ctx, name)
	if freezeErr != nil || emptyErr != nil || !frozen || !empty {
		return errors.New("build retention cap requires a positively frozen empty parent")
	}
	file, err = self.open(name, "memory.max", os.O_RDONLY)
	if err != nil {
		return err
	}
	content, readErr := io.ReadAll(io.LimitReader(file, self.maximumBytes+1))
	closeErr := file.Close()
	value := strings.TrimSuffix(string(content), "\n")
	previous, parseErr := strconv.ParseInt(value, 10, 64)
	if readErr != nil || closeErr != nil || self.maximumBytes <= 0 || int64(len(content)) > self.maximumBytes ||
		parseErr != nil || strconv.FormatInt(previous, 10) != value || previous < limit.MemoryBytes {
		return errors.New("retired build cannot raise or replace an unknown existing memory ceiling")
	}
	return self.applyManagerLimitArguments(ctx, arguments)
}

// Kernel counters are bounded, unique and finite. No clean-page subtraction is
// used to exempt residual anonymous, kernel, dirty or unevictable memory.
func (self *hostCleanupCgroups) RetirementMemory(ctx context.Context, name string, settings BuildRetirementSettings) (buildRetirementMemory, error) {
	read := func(property string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := self.open(name, property, os.O_RDONLY)
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(io.LimitReader(file, settings.NativeBytes+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil || settings.NativeBytes <= 0 || int64(len(content)) > settings.NativeBytes {
			return nil, errors.New("retired build kernel evidence is unavailable or oversized")
		}
		return content, nil
	}
	readCount := func(property string) (uint64, error) {
		content, err := read(property)
		if err != nil {
			return 0, err
		}
		value := strings.TrimSuffix(string(content), "\n")
		count, err := strconv.ParseUint(value, 10, 64)
		if err != nil || strconv.FormatUint(count, 10) != value {
			return 0, errors.New("retired build kernel charge is not a finite canonical counter")
		}
		return count, nil
	}
	before, err := readCount("memory.current")
	if err != nil {
		return buildRetirementMemory{}, err
	}
	content, err := read("memory.stat")
	if err != nil {
		return buildRetirementMemory{}, err
	}
	counters, err := parseBuildRetirementStats(content, settings)
	if err != nil {
		return buildRetirementMemory{}, err
	}
	swap, err := readCount("memory.swap.current")
	if err != nil {
		return buildRetirementMemory{}, err
	}
	after, err := readCount("memory.current")
	if err != nil {
		return buildRetirementMemory{}, err
	}
	if after < before {
		after = before
	}
	return buildRetirementMemory{CurrentBytes: after, SwapCurrentBytes: swap, StatSha256: digestDockerBytes(content), Counters: counters}, ctx.Err()
}

// Required memory.stat names are kernel interface facts for the qualified role.
func parseBuildRetirementStats(content []byte, settings BuildRetirementSettings) (map[string]uint64, error) {
	if settings.NativeBytes <= 0 || settings.MaximumStatKeys <= 0 || settings.MaximumStatNameBytes <= 0 || int64(len(content)) > settings.NativeBytes {
		return nil, errors.New("retired build stat evidence exceeds its finite bounds")
	}
	counters := map[string]uint64{}
	for _, line := range strings.Split(string(content), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) > settings.MaximumStatNameBytes || len(counters) >= settings.MaximumStatKeys {
			return nil, errors.New("retired build stat evidence is malformed or unbounded")
		}
		for _, char := range fields[0] {
			if char != '_' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
				return nil, errors.New("retired build stat counter name is invalid")
			}
		}
		count, err := strconv.ParseUint(fields[1], 10, 64)
		_, duplicate := counters[fields[0]]
		if err != nil || strconv.FormatUint(count, 10) != fields[1] || duplicate {
			return nil, errors.New("retired build stat counter is duplicate or nonfinite")
		}
		counters[fields[0]] = count
	}
	for _, required := range buildRetirementCounterNames() {
		if _, ok := counters[required]; !ok {
			return nil, errors.New("retired build stat evidence omitted a required kernel charge")
		}
	}
	return counters, nil
}

// These names are the qualified kernel interface, not adjustable role budgets.
func buildRetirementCounterNames() []string {
	return []string{"anon", "file", "shmem", "active_anon", "inactive_anon", "active_file", "inactive_file",
		"file_mapped", "file_dirty", "file_writeback", "unevictable", "kernel", "kernel_stack", "pagetables", "slab_reclaimable", "slab_unreclaimable"}
}

// Native and recovered receipts share the same bounded evidence contract.
func validateBuildRetirementMemory(memory buildRetirementMemory, settings BuildRetirementSettings) error {
	if !digestPattern.MatchString(memory.StatSha256) || len(memory.Counters) == 0 ||
		settings.MaximumStatKeys <= 0 || settings.MaximumStatNameBytes <= 0 || len(memory.Counters) > settings.MaximumStatKeys {
		return errors.New("retired build omitted finite stat evidence")
	}
	for name := range memory.Counters {
		if len(name) == 0 || len(name) > settings.MaximumStatNameBytes {
			return errors.New("retired build stat name exceeds its finite bound")
		}
		for _, char := range name {
			if char != '_' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
				return errors.New("retired build stat counter name is invalid")
			}
		}
	}
	for _, required := range buildRetirementCounterNames() {
		if _, ok := memory.Counters[required]; !ok {
			return errors.New("retired build omitted a required kernel charge")
		}
	}
	return nil
}
