// Private fake-kernel and filesystem tests cover retirement ordering and
// accounting. They do not qualify native memory pressure or Docker behavior.
package competitionagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Native callbacks run outside this fake's lock, just as no owner state lock
// may surround a potentially blocking manager/kernel operation.
type buildRetirementTestKernel struct {
	stateLock     sync.Mutex
	events        *cleanupTestEvents
	frozen, empty bool
	limit         runSliceLimits
	memory        buildRetirementMemory
	before        func(context.Context, string) error
}

// Every test has all mandatory finite kernel counters, not clean-file guesses.
func testBuildRetirementMemory() buildRetirementMemory {
	counters := map[string]uint64{}
	for _, name := range buildRetirementCounterNames() {
		counters[name] = 0
	}
	return buildRetirementMemory{CurrentBytes: 1024, StatSha256: strings.Repeat("a", 64), Counters: counters}
}

// Synthetic headroom is three GiB while the original build ceiling is twelve.
func testBuildRetirementPolicy(t *testing.T) BuildRetirementPolicy {
	t.Helper()
	resource := testRunResourceIdentity()
	resource.BuildMemoryLimit, resource.BuildMemoryLimitBytes = "12g", 12*1024*1024*1024
	resource.EvidenceMemoryLimitBytes = 5 * 1024 * 1024 * 1024
	policy, err := newBuildRetirementPolicy(runSliceResources{ResourcePolicySha256: strings.Repeat("f", 64), Identity: resource,
		BuildTasksMaximum: 4096, CpuPeriodMicros: 100000}, "urnetwork-submission-build-"+strings.Repeat("b", 32)+".slice", int64(os.Getpagesize()))
	if err != nil {
		t.Fatal(err)
	}
	return *policy
}

// Event callbacks precede state changes and can expose deterministic barriers.
func (self *buildRetirementTestKernel) event(ctx context.Context, operation string) error {
	self.events.record(operation)
	if self.before != nil {
		return self.before(ctx, operation)
	}
	return ctx.Err()
}

// Freezing never means suspension was acknowledged until Frozen is read.
func (self *buildRetirementTestKernel) Freeze(ctx context.Context, _ string) error {
	if err := self.event(ctx, "freeze"); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.frozen = true
	return nil
}

// The fake's process census joins immediately unless its explicit callback waits.
func (self *buildRetirementTestKernel) Kill(ctx context.Context, _ string) error {
	if err := self.event(ctx, "kill"); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.empty = true
	return nil
}

// No absence or unacknowledged freeze counts as a retained parent.
func (self *buildRetirementTestKernel) Frozen(ctx context.Context, _ string) (bool, error) {
	if err := self.event(ctx, "frozen"); err != nil {
		return false, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.frozen, nil
}

// The fake's census can block at an exact ownership boundary.
func (self *buildRetirementTestKernel) Empty(ctx context.Context, _ string) (bool, error) {
	if err := self.event(ctx, "empty"); err != nil {
		return false, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.empty, nil
}

// A manager mutation refuses populated or unfrozen parents.
func (self *buildRetirementTestKernel) LowerBuildLimit(ctx context.Context, _ string, limit runSliceLimits) error {
	if err := self.event(ctx, "lower"); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.frozen || !self.empty {
		return errors.New("synthetic unjoined parent")
	}
	self.limit = limit
	return nil
}

// Positive exact readback is independent of the manager's successful return.
func (self *buildRetirementTestKernel) VerifyLimits(ctx context.Context, _ string, limit runSliceLimits) error {
	if err := self.event(ctx, "verify"); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.limit != limit {
		return errors.New("synthetic missing finite cap acknowledgment")
	}
	return nil
}

// The map is immutable fixture evidence, never mutated by a caller.
func (self *buildRetirementTestKernel) RetirementMemory(ctx context.Context, _ string, _ BuildRetirementSettings) (buildRetirementMemory, error) {
	if err := self.event(ctx, "memory"); err != nil {
		return buildRetirementMemory{}, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.memory, nil
}

// Arithmetic preserves the frozen charges and refuses even one missing page.
func TestBuildRetirementCapUsesOnlyPageRoundedMeasuredHeadroom(t *testing.T) {
	base := testBuildRetirementPolicy(t)
	const gib = int64(1024 * 1024 * 1024)
	for _, check := range []struct {
		host, want int64
		valid      bool
	}{
		{host: 125 * gib}, {host: 125*gib + base.PageBytes - 1},
		{host: 125*gib + base.PageBytes, want: base.PageBytes, valid: true},
		{host: 128*gib + base.PageBytes - 1, want: 3 * gib, valid: true},
		{host: 160 * gib, want: 12 * gib, valid: true},
	} {
		policy := base
		policy.HostMemoryBytes = check.host
		cap, err := retiredBuildMemoryMaximum(policy)
		if (err == nil) != check.valid || cap != check.want {
			t.Fatal("retirement consumed or silently changed the frozen reserve", check, cap, err)
		}
		if policy.ActiveMemoryBytes != 96*gib || policy.EvidenceMemoryBytes != 5*gib || policy.ManagementReserveBytes != 24*gib {
			t.Fatal("retirement rewrote frozen ledger ceilings")
		}
	}
}

// The production owner has no configuration field that selects a fake kernel.
func TestBuildRetirementConstructorAlwaysUsesNativeFiniteOwner(t *testing.T) {
	engine := newHostBuildRetirement(Pin{}, DefaultBuildRetirementSettings())
	kernel, ok := engine.kernel.(*hostCleanupCgroups)
	if !ok || !kernel.requireCgroupFs || kernel.rootPath != "/sys/fs/cgroup" || kernel.limitCommand != nil || kernel.limitCommandTimeout <= 0 {
		t.Fatal("retirement constructor bypassed native finite cgroup ownership")
	}
}

// Lowering cannot precede the freeze/kill acknowledgments, and success needs
// exact cap readback plus current-charge/stat evidence and a second census.
func TestBuildRetirementOrdersFreezeKillJoinLowerReadbackAndCharge(t *testing.T) {
	policy := testBuildRetirementPolicy(t)
	kernel := &buildRetirementTestKernel{events: &cleanupTestEvents{}, memory: testBuildRetirementMemory()}
	engine := &hostBuildRetirement{settings: DefaultBuildRetirementSettings(), kernel: kernel}
	memory, err := engine.Retire(t.Context(), policy)
	if err != nil || memory.CurrentBytes != 1024 {
		t.Fatal(err)
	}
	wanted := []string{"freeze", "kill", "frozen", "empty", "lower", "verify", "memory", "frozen", "empty"}
	if !reflect.DeepEqual(kernel.events.snapshot(), wanted) || kernel.limit.MemoryBytes != 3*1024*1024*1024 || !kernel.frozen || !kernel.empty {
		t.Fatal("retirement omitted or reordered a positive native boundary")
	}
}

// A bounded join uses an acknowledged channel barrier, not a sleep or lucky
// scheduler order. Lowering and publication cannot pass the held census.
func TestBuildRetirementCannotLowerBeforePositiveProcessJoin(t *testing.T) {
	policy := testBuildRetirementPolicy(t)
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	kernel := &buildRetirementTestKernel{events: &cleanupTestEvents{}, memory: testBuildRetirementMemory()}
	var once sync.Once
	kernel.before = func(ctx context.Context, operation string) error {
		if operation == "empty" {
			once.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
		return ctx.Err()
	}
	engine := &hostBuildRetirement{settings: DefaultBuildRetirementSettings(), kernel: kernel}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { defer close(finished); _, err := engine.Retire(ctx, policy); done <- err }()
	var releaseOnce sync.Once
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Error("retirement test owner did not join")
		}
	})
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("retirement did not reach process join")
	}
	if !reflect.DeepEqual(kernel.events.snapshot(), []string{"freeze", "kill", "frozen", "empty"}) {
		t.Fatal("cap lowering crossed an unjoined process barrier")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Configured caps do not excuse over-budget actual charge, swap, malformed
// evidence, thawing or a late child. None is classified as clean/free memory.
func TestBuildRetirementVerifyRefusesUnboundedOrChangedKernelFacts(t *testing.T) {
	policy := testBuildRetirementPolicy(t)
	for _, fault := range []string{"charge", "swap", "missing-stat", "wrong-cap", "thawed", "populated"} {
		kernel := &buildRetirementTestKernel{events: &cleanupTestEvents{}, frozen: true, empty: true, limit: policy.Limits, memory: testBuildRetirementMemory()}
		switch fault {
		case "charge":
			kernel.memory.CurrentBytes = uint64(policy.RetainedMemoryMaximumBytes) + 1
		case "swap":
			kernel.memory.SwapCurrentBytes = 1
		case "missing-stat":
			delete(kernel.memory.Counters, "anon")
		case "wrong-cap":
			kernel.limit.MemoryBytes++
		case "thawed":
			kernel.frozen = false
		case "populated":
			kernel.empty = false
		}
		engine := &hostBuildRetirement{settings: DefaultBuildRetirementSettings(), kernel: kernel}
		if _, err := engine.Verify(t.Context(), policy); err == nil {
			t.Fatal("unproven retired memory became measurement authority", fault)
		}
	}
}

// Source files mimic only kernel interfaces. No actual cgroup or unit exists.
func testBuildRetirementStats() []byte {
	var content strings.Builder
	for _, name := range buildRetirementCounterNames() {
		fmt.Fprintf(&content, "%s 0\n", name)
	}
	return []byte(content.String())
}

// Missing, duplicate, nonfinite and oversized fields refuse native evidence.
func TestBuildRetirementKernelStatsRemainFiniteAndComplete(t *testing.T) {
	settings := DefaultBuildRetirementSettings()
	valid := testBuildRetirementStats()
	if _, err := parseBuildRetirementStats(valid, settings); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{
		[]byte(strings.Replace(string(valid), "anon 0\n", "", 1)),
		append(append([]byte(nil), valid...), []byte("anon 0\n")...),
		[]byte(strings.Replace(string(valid), "anon 0", "anon -1", 1)),
		[]byte(strings.Replace(string(valid), "anon 0", "anon 18446744073709551616", 1)),
		[]byte(strings.Replace(string(valid), "anon 0", "anon max", 1)),
		[]byte(strings.Repeat("x", int(settings.NativeBytes)+1)),
	} {
		if _, err := parseBuildRetirementStats(invalid, settings); err == nil {
			t.Fatal("unknown kernel stats were accepted")
		}
	}
}

// The native manager path can lower only an empty frozen exact build parent.
// Actual charge/stat parsing is tested under a private ordinary filesystem.
func TestBuildRetirementNativeLowerAndChargeEvidence(t *testing.T) {
	groups, name, directory, limit := newContainmentLimitKernelTest(t)
	if err := groups.SetLimits(t.Context(), name, limit); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cgroup.events"), []byte("populated 0\nfrozen 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	apply := groups.limitCommand
	groups.limitCommand = func(ctx context.Context, args []string) error {
		if err := os.WriteFile(filepath.Join(directory, "memory.max"), nil, 0600); err != nil {
			return err
		}
		return apply(ctx, args)
	}
	limit.MemoryBytes = 3 * 1024 * 1024 * 1024
	if err := groups.LowerBuildLimit(t.Context(), name, limit); err != nil {
		t.Fatal(err)
	}
	if err := groups.VerifyLimits(t.Context(), name, limit); err != nil {
		t.Fatal(err)
	}
	for property, content := range map[string][]byte{"memory.current": []byte("1024\n"), "memory.swap.current": []byte("0\n"), "memory.stat": testBuildRetirementStats()} {
		if err := os.WriteFile(filepath.Join(directory, property), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	memory, err := groups.RetirementMemory(t.Context(), name, DefaultBuildRetirementSettings())
	if err != nil || memory.CurrentBytes != 1024 || memory.SwapCurrentBytes != 0 || !digestPattern.MatchString(memory.StatSha256) {
		t.Fatal("native finite retirement evidence was lost", err)
	}
	limit.MemoryBytes++
	if err := groups.LowerBuildLimit(t.Context(), name, limit); err == nil {
		t.Fatal("retirement raised its previously lowered memory ceiling")
	}
}
