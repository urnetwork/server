// Aggregate-resource tests replace only systemd/kernel boundaries. No test
// writes a host cgroup, runs Docker, or infers native bomb containment from a fake.
package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Synthetic topology and role ceilings are shared by the composed test agent.
func testRunResourceIdentity() runResourceIdentity {
	return runResourceIdentity{Schema: Schema, Kind: "sim-latency-resource-boundary", EvaluationCpuSet: "2,4", ManagementCpuSet: "6,8",
		BuildMemoryLimit: "5g", BuildMemoryLimitBytes: 5 * 1024 * 1024 * 1024,
		RunnerMemoryLimitBytes: 72 * 1024 * 1024 * 1024, PostgresMemoryLimitBytes: 16 * 1024 * 1024 * 1024, RedisMemoryLimitBytes: 8 * 1024 * 1024 * 1024,
		RunnerPidsLimit: 65536, PostgresPidsLimit: 4096, RedisPidsLimit: 4096,
		ActiveMemoryLimitBytes: 96 * 1024 * 1024 * 1024, ActivePidsLimit: 65536 + 4096 + 4096,
		ScorerMemoryLimitBytes: 4 * 1024 * 1024 * 1024, ScorerPidsLimit: 1024,
		PreflightMemoryLimitBytes: 4 * 1024 * 1024 * 1024, PreflightPidsLimit: 1024,
		DisjointCpuSets: true, MemoryCapacityPassed: true, EvidenceMemoryLimitBytes: 1024 * 1024,
		HostMemoryBytes: 128 * 1024 * 1024 * 1024, ManagementReserveBytes: 24 * 1024 * 1024 * 1024}
}

// Fixture helpers emit the same finite shape as the pinned resource helper.
func testRunResourceIdentityJson(t *testing.T) string {
	t.Helper()
	content, err := json.Marshal(testRunResourceIdentity())
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// Older cleanup-only fixtures deliberately need no healthy admission metadata.
func newContainmentResourceTest(t *testing.T) *hostCleanupTest {
	t.Helper()
	fixture := newHostCleanupTest(t)
	fixture.cleaner.config.DockerRole.ResourcePolicy.Sha256 = strings.Repeat("f", 64)
	fixture.plan.SliceResources = &runSliceResources{ResourcePolicySha256: fixture.cleaner.config.DockerRole.ResourcePolicy.Sha256,
		Identity: testRunResourceIdentity(), BuildTasksMaximum: fixture.cleaner.settings.BuildTasksMaximum,
		CpuPeriodMicros: fixture.cleaner.settings.ContainmentCpuPeriodMicros}
	return fixture
}

// Synthetic stat facts exercise ownership without chown or host privileges.
type containmentLimitTestInfo struct {
	os.FileInfo
	stat *syscall.Stat_t
	mode os.FileMode
}

// The normal FileInfo surface stays real; only the trusted kernel facts vary.
func (self containmentLimitTestInfo) Sys() any { return self.stat }

// Permission changes are facts, not filesystem mutations in this fixture.
func (self containmentLimitTestInfo) Mode() os.FileMode { return self.mode }

// Admission must reject delegated or group/world-writable limit authority.
func TestContainmentLimitsRequireExclusiveRootOwnership(t *testing.T) {
	for _, check := range []struct {
		uid      uint32
		mode     os.FileMode
		accepted bool
	}{
		{uid: 0, mode: 0644, accepted: true}, {uid: 0, mode: 0444, accepted: true},
		{uid: 0, mode: os.ModeDir | 0755, accepted: true}, {uid: 1000, mode: 0644},
		{uid: 0, mode: 0664}, {uid: 0, mode: 0646}, {uid: 0, mode: os.ModeDir | 0775},
	} {
		info := containmentLimitTestInfo{stat: &syscall.Stat_t{Uid: check.uid}, mode: check.mode}
		if rootOwnedContainmentNode(info) != check.accepted {
			t.Fatal("delegated resource authority was accepted", check)
		}
	}
	if rootOwnedContainmentNode(nil) || rootOwnedContainmentNode(containmentLimitTestInfo{}) {
		t.Fatal("missing native ownership became exclusive root authority")
	}
}

// Construction is inert and always selects the native cgroup v2 boundary.
func TestContainmentLimitsConstructorAlwaysUsesNativeHierarchy(t *testing.T) {
	boundary := newHostRunSlices(Config{}, DefaultAgentSettings())
	groups, ok := boundary.cgroups.(*hostCleanupCgroups)
	if !ok || !groups.requireCgroupFs || groups.rootPath != "/sys/fs/cgroup" || groups.maximumBytes <= 0 || groups.limitCommandTimeout <= 0 ||
		groups.limitCommand != nil || boundary.command == nil {
		t.Fatal("public role construction bypassed the native finite cgroup boundary")
	}
}

// The complete first census precedes every limit write; readback precedes the
// complete second census. Every one of the 31 parents receives a finite cap.
func TestContainmentLimitsRequireCompleteCensusWriteReadbackAndRecheck(t *testing.T) {
	fixture := newContainmentResourceTest(t)
	groups := &containmentTestCgroups{events: fixture.events, prepared: true}
	boundary := &hostRunSlices{config: fixture.cleaner.config, settings: fixture.cleaner.settings, cgroups: groups,
		command: func(_ context.Context, operation string, names []string) error {
			if operation != "start" || !reflect.DeepEqual(names, fixture.plan.CgroupNames) {
				t.Fatal("aggregate preparation changed its exact parent inventory")
			}
			fixture.events.record("start")
			return nil
		}}
	if err := boundary.Prepare(t.Context(), fixture.plan); err != nil {
		t.Fatal(err)
	}
	wanted := []string{"start"}
	for _, name := range fixture.plan.CgroupNames {
		wanted = append(wanted, "frozen:"+name, "empty:"+name)
	}
	for _, operation := range []string{"set-limits:", "verify-limits:"} {
		for _, name := range fixture.plan.CgroupNames {
			wanted = append(wanted, operation+name)
		}
	}
	for _, name := range fixture.plan.CgroupNames {
		wanted = append(wanted, "frozen:"+name, "empty:"+name)
	}
	if !reflect.DeepEqual(fixture.events.snapshot(), wanted) || len(groups.limits) != 31 {
		t.Fatal("not every exact parent was empty before any write and rechecked after positive readback")
	}
	for name, limit := range groups.limits {
		memory, tasks, cpus := int64(96*1024*1024*1024), int64(73728), "2,4"
		switch {
		case cleanupBuildSlicePattern.MatchString(name):
			memory, tasks = 5*1024*1024*1024, fixture.cleaner.settings.BuildTasksMaximum
		case strings.HasSuffix(name, "-preflight.slice"):
			memory, tasks, cpus = 4*1024*1024*1024, 1024, "6,8"
		case strings.HasSuffix(name, "-score.slice") || strings.Contains(name, "-live-"):
			memory, tasks = 4*1024*1024*1024, 1024
		}
		if limit.MemoryBytes != memory || limit.TasksMaximum != tasks || limit.SwapBytes != 0 || limit.CpuSet != cpus ||
			limit.CpuPeriodMicros != fixture.cleaner.settings.ContainmentCpuPeriodMicros || limit.CpuQuotaMicros != 2*limit.CpuPeriodMicros {
			t.Fatal("parent ceiling drifted from its frozen role", name, limit)
		}
	}
}

// Even a populated or frozen final parent forbids writes to all earlier ones.
func TestContainmentLimitsNeverWriteBeforeLastParentIsEmptyAndUnfrozen(t *testing.T) {
	for _, refusal := range []string{"populated", "frozen", "missing"} {
		fixture := newContainmentResourceTest(t)
		last := fixture.plan.CgroupNames[len(fixture.plan.CgroupNames)-1]
		groups := &containmentTestCgroups{events: fixture.events, prepared: true}
		groups.frozen = func(_ context.Context, name string) (bool, error) {
			if name == last && refusal == "missing" {
				return false, errCleanupCgroupAbsent
			}
			return name == last && refusal == "frozen", nil
		}
		groups.empty = func(_ context.Context, name string) (bool, error) { return name != last || refusal != "populated", nil }
		boundary := &hostRunSlices{config: fixture.cleaner.config, settings: fixture.cleaner.settings, cgroups: groups,
			command: func(context.Context, string, []string) error { return nil }}
		if err := boundary.Prepare(t.Context(), fixture.plan); err == nil || len(groups.limits) != 0 {
			t.Fatal("an earlier parent was capped before the last parent passed its census", refusal, err)
		}
		for _, event := range fixture.events.snapshot() {
			if strings.HasPrefix(event, "set-limits:") {
				t.Fatal("preexisting populated/frozen/missing parent was touched", refusal)
			}
		}
	}
}

// Refused writes, missing acknowledgments, population and freeze changes are
// explicit failures. None permits preparation to claim a launchable boundary.
func TestContainmentLimitsRefuseWriteReadbackAndFinalCensusFailure(t *testing.T) {
	for _, refusal := range []string{"write", "readback", "late-populated", "late-frozen"} {
		fixture := newContainmentResourceTest(t)
		groups := &containmentTestCgroups{events: fixture.events, prepared: true}
		cause := errors.New("synthetic kernel refused aggregate boundary")
		groups.setLimits = func(context.Context, string, runSliceLimits) error {
			if refusal == "write" {
				return cause
			}
			return nil
		}
		groups.verifyLimits = func(context.Context, string, runSliceLimits) error {
			if refusal == "readback" {
				return cause
			}
			return nil
		}
		groups.empty = func(context.Context, string) (bool, error) {
			return refusal != "late-populated" || len(groups.limits) == 0, nil
		}
		groups.frozen = func(context.Context, string) (bool, error) {
			return refusal == "late-frozen" && len(groups.limits) != 0, nil
		}
		boundary := &hostRunSlices{config: fixture.cleaner.config, settings: fixture.cleaner.settings, cgroups: groups,
			command: func(context.Context, string, []string) error { return nil }}
		if err := boundary.Prepare(t.Context(), fixture.plan); err == nil {
			t.Fatal("unknown aggregate containment became launch authority", refusal)
		}
	}
}

// Bad committed caps cannot reach even unit startup. Reaping keeps its separate
// identity-only validation so an unhealthy resource boundary remains killable.
func TestContainmentLimitsRejectBadAuthorityWithoutDisablingEmergencyReap(t *testing.T) {
	for _, mutation := range []func(*runSliceResources){
		func(resource *runSliceResources) { resource.ResourcePolicySha256 = strings.Repeat("a", 64) },
		func(resource *runSliceResources) { resource.BuildTasksMaximum = 0 },
		func(resource *runSliceResources) { resource.BuildTasksMaximum = resource.Identity.ActivePidsLimit + 1 },
		func(resource *runSliceResources) { resource.CpuPeriodMicros = 0 },
		func(resource *runSliceResources) { resource.CpuPeriodMicros = 999 },
		func(resource *runSliceResources) { resource.CpuPeriodMicros = 1000001 },
		func(resource *runSliceResources) { resource.Identity.EvaluationCpuSet = "4294967295" },
		func(resource *runSliceResources) { resource.Identity.ActivePidsLimit++ },
		func(resource *runSliceResources) { resource.Identity.BuildMemoryLimitBytes++ },
		func(resource *runSliceResources) { resource.Identity.ActiveMemoryLimitBytes++ },
		func(resource *runSliceResources) { resource.Identity.ScorerPidsLimit = 0 },
	} {
		fixture := newContainmentResourceTest(t)
		mutation(fixture.plan.SliceResources)
		groups := &containmentTestCgroups{events: fixture.events}
		boundary := &hostRunSlices{config: fixture.cleaner.config, settings: fixture.cleaner.settings, cgroups: groups,
			command: func(context.Context, string, []string) error { t.Error("bad limits reached unit startup"); return nil }}
		if err := boundary.Prepare(t.Context(), fixture.plan); err == nil || len(fixture.events.snapshot()) != 0 {
			t.Fatal("invalid resource authority reached a native boundary")
		}
		if err := boundary.FreezeAndReap(t.Context(), fixture.plan); err != nil {
			t.Fatal("invalid admission limits disabled emergency containment", err)
		}
	}
}

// Public admission, durable launch markers and retries compose the real cap
// preparation path. A kernel refusal launches neither owner nor evaluator.
func TestAgentAggregateKernelRefusalRetainsCustodyWithoutOwnerOrEvaluator(t *testing.T) {
	fixture := newAgentTestFixture(t)
	events := &cleanupTestEvents{}
	groups := &containmentTestCgroups{events: events, prepared: true,
		verifyLimits: func(context.Context, string, runSliceLimits) error {
			return errors.New("synthetic missing effective CPU acknowledgment")
		}}
	boundary := &hostRunSlices{config: *fixture.config, settings: fixture.settings, cgroups: groups,
		command: func(context.Context, string, []string) error { return nil }}
	fixture.containment.before = func(ctx context.Context, operation string, plan cleanupPlan, _ bool) error {
		if operation == "prepare" {
			state, err := fixture.agent.loadState(plan.RunId)
			if err != nil || state.CleanupPlanSha256 == "" || !state.LaunchAttempted || plan.SliceResources == nil {
				t.Fatal("resource mutation preceded root-committed hash and launch custody", err)
			}
			return boundary.Prepare(ctx, plan)
		}
		t.Error("unverified aggregate caps reached Docker owner launch")
		return nil
	}
	for fence := int64(1); fence <= 2; fence++ {
		response, err := fixture.call(t, "start", fence, &fixture.manifest)
		if err != nil || response.State != "unknown" {
			t.Fatal("unknown kernel admission was not retained", response, err)
		}
	}
	if fixture.runner.launchCount() != 0 || !reflect.DeepEqual(fixture.containment.snapshot(), []string{"prepare"}) ||
		fixture.agent.requireFreeSlot(t.Context(), strings.Repeat("a", 64)) == nil {
		t.Fatal("kernel refusal launched/replayed execution or released physical custody")
	}
}

// Successful signed admission invokes the actual preparation algorithm before
// the independent owner and evaluator, and reconnect cannot reapply any cap.
func TestAgentAggregateKernelAcksPrecedeOwnerAndEvaluator(t *testing.T) {
	fixture := newAgentTestFixture(t)
	fixture.agent.settings.BuildTasksMaximum = 1234
	fixture.agent.settings.ContainmentCpuPeriodMicros = 50000
	events := &cleanupTestEvents{}
	groups := &containmentTestCgroups{events: events, prepared: true}
	boundary := &hostRunSlices{config: *fixture.config, settings: fixture.agent.settings, cgroups: groups,
		command: func(context.Context, string, []string) error { return nil }}
	checkAcknowledged := func() {
		t.Helper()
		if len(groups.limits) != 31 {
			t.Fatal("owner/evaluator launch preceded all aggregate parent writes")
		}
		for name, limit := range groups.limits {
			if limit.CpuPeriodMicros != 50000 || cleanupBuildSlicePattern.MatchString(name) && limit.TasksMaximum != 1234 {
				t.Fatal("parent cap ignored its explicit instance-owned settings")
			}
		}
		readbacks, emptyChecks := 0, 0
		for _, event := range events.snapshot() {
			if strings.HasPrefix(event, "verify-limits:") {
				readbacks++
			}
			if strings.HasPrefix(event, "empty:") {
				emptyChecks++
			}
		}
		if readbacks != 31 || emptyChecks != 62 {
			t.Fatal("owner/evaluator launch preceded positive kernel readback and both complete censuses")
		}
	}
	fixture.containment.before = func(ctx context.Context, operation string, plan cleanupPlan, _ bool) error {
		if operation == "prepare" {
			return boundary.Prepare(ctx, plan)
		}
		if operation != "start" {
			t.Fatal("admission changed containment lifecycle", operation)
		}
		checkAcknowledged()
		return nil
	}
	fixture.runner.beforeStart = func(Launch) { checkAcknowledged() }
	for fence := int64(1); fence <= 2; fence++ {
		if _, err := fixture.call(t, "start", fence, &fixture.manifest); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.runner.launchCount() != 1 || !reflect.DeepEqual(fixture.containment.snapshot(), []string{"prepare", "start"}) {
		t.Fatal("reconnect reapplied capped parents or relaunched execution")
	}
}

// Native properties are ordinary files in a private hierarchy only. Their
// effective CPU reply is separately supplied, as it is read-only in cgroupfs.
func newContainmentLimitKernelTest(t *testing.T) (*hostCleanupCgroups, string, string, runSliceLimits) {
	t.Helper()
	groups, name, directory := newContainmentKernelFixture(t)
	limit := runSliceLimits{MemoryBytes: 12 * 1024 * 1024 * 1024, TasksMaximum: 4096, CpuQuotaMicros: 200000, CpuPeriodMicros: 100000, CpuSet: "2,4"}
	for _, property := range []string{"memory.max", "memory.swap.max", "pids.max", "cpu.max", "cpuset.cpus", "cpuset.cpus.effective"} {
		if err := os.WriteFile(filepath.Join(directory, property), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "cpuset.cpus.effective"), []byte("2,4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cgroup.events"), []byte("populated 0\nfrozen 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	groups.limitCommandTimeout = DefaultAgentSettings().CleanupCommandTimeout
	groups.limitCommand = func(ctx context.Context, arguments []string) error {
		if ctx.Err() != nil || len(arguments) != 10 || !reflect.DeepEqual(arguments[:4], []string{"set-property", "--runtime", "--", name}) {
			return errors.New("synthetic manager received an open or unbounded command")
		}
		properties := map[string]string{}
		for _, argument := range arguments[4:] {
			key, value, ok := strings.Cut(argument, "=")
			if !ok || properties[key] != "" {
				return errors.New("synthetic manager received ambiguous properties")
			}
			properties[key] = value
		}
		quotaPercent, quotaErr := strconv.ParseInt(strings.TrimSuffix(properties["CPUQuota"], "%"), 10, 64)
		period, periodErr := strconv.ParseInt(strings.TrimSuffix(properties["CPUQuotaPeriodSec"], "us"), 10, 64)
		if quotaErr != nil || periodErr != nil {
			return errors.New("synthetic manager received a nonnumeric CPU ceiling")
		}
		// The private synthetic kernel reflects only these exact properties.
		// Descriptor-confined writes keep even failed fixtures off foreign files.
		for _, property := range []struct{ name, value string }{
			{name: "memory.max", value: properties["MemoryMax"]},
			{name: "memory.swap.max", value: properties["MemorySwapMax"]},
			{name: "pids.max", value: properties["TasksMax"]},
			{name: "cpu.max", value: fmt.Sprintf("%d %d", quotaPercent*period/100, period)},
			{name: "cpuset.cpus", value: properties["AllowedCPUs"]},
		} {
			file, err := groups.open(name, property.name, os.O_WRONLY)
			if err != nil {
				return err
			}
			value := property.value + "\n"
			count, writeErr := io.WriteString(file, value)
			closeErr := file.Close()
			if count != len(value) || writeErr != nil || closeErr != nil {
				return errors.New("synthetic manager kernel write was not acknowledged")
			}
		}
		return nil
	}
	return groups, name, directory, limit
}

// The manager surface has one exact unit and six fixed finite properties. No
// caller-supplied option, program, generic property or unrelated unit escapes.
func TestContainmentLimitsUseClosedRuntimeManagerProperties(t *testing.T) {
	_, name, _, limit := newContainmentLimitKernelTest(t)
	arguments, err := systemdSliceLimitArguments(name, limit)
	wanted := []string{"set-property", "--runtime", "--", name, "MemoryMax=12884901888", "MemorySwapMax=0", "TasksMax=4096",
		"CPUQuota=200%", "CPUQuotaPeriodSec=100000us", "AllowedCPUs=2,4"}
	if err != nil || !reflect.DeepEqual(arguments, wanted) {
		t.Fatal("manager limit operation drifted from its typed closed runtime policy", arguments, err)
	}
	for _, name := range []string{"user.slice", "-MemoryMax=infinity", "urnetwork-evaluation-*.slice", "../foreign.slice"} {
		if _, err := systemdSliceLimitArguments(name, limit); err == nil {
			t.Fatal("typed manager operation accepted an unrelated unit or option", name)
		}
	}
}

// Exact manager properties are recorded before readback, and remain the source
// of a synthetic later child realization. Raw cgroup writes cannot pass this.
func TestContainmentKernelLimitsAreOwnedByManagerAcrossChildRealization(t *testing.T) {
	groups, name, directory, limit := newContainmentLimitKernelTest(t)
	apply := groups.limitCommand
	var committed []string
	groups.limitCommand = func(ctx context.Context, arguments []string) error {
		if committed != nil {
			return errors.New("aggregate manager property mutation was unexpectedly repeated")
		}
		committed = append([]string(nil), arguments...)
		return apply(ctx, arguments)
	}
	if err := groups.SetLimits(t.Context(), name, limit); err != nil || len(committed) != 10 {
		t.Fatal("finite controls were not committed to their manager owner", err)
	}
	if err := groups.VerifyLimits(t.Context(), name, limit); err != nil {
		t.Fatal(err)
	}
	// A fake manager realization deliberately reapplies only its committed
	// properties. Native qualification must repeat this with real Docker scopes.
	if err := os.WriteFile(filepath.Join(directory, "memory.max"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := apply(t.Context(), committed); err != nil {
		t.Fatal(err)
	}
	if err := groups.VerifyLimits(t.Context(), name, limit); err != nil {
		t.Fatal("later child realization lost the manager-owned parent limits", err)
	}
}

// A parent that changed after the complete preparation census must still be
// refused by the native writer before any manager property can be applied.
func TestContainmentKernelLimitsRefusePopulatedOrFrozenParentBeforeManager(t *testing.T) {
	for _, state := range []string{"populated 1\nfrozen 0\n", "populated 0\nfrozen 1\n"} {
		groups, name, directory, limit := newContainmentLimitKernelTest(t)
		if err := os.WriteFile(filepath.Join(directory, "cgroup.events"), []byte(state), 0600); err != nil {
			t.Fatal(err)
		}
		calls := 0
		groups.limitCommand = func(context.Context, []string) error { calls++; return nil }
		if err := groups.SetLimits(t.Context(), name, limit); err == nil || calls != 0 {
			t.Fatal("preexisting populated/frozen parent received manager limit writes", state, err)
		}
	}
}

// Invalid typed limits are refused before opening any cgroup property.
func TestContainmentKernelLimitsRefuseUnboundedRequestsBeforeAnyWrite(t *testing.T) {
	for _, mutation := range []func(*runSliceLimits){
		func(limit *runSliceLimits) { limit.MemoryBytes = 0 },
		func(limit *runSliceLimits) { limit.SwapBytes = 1 },
		func(limit *runSliceLimits) { limit.TasksMaximum = 0 },
		func(limit *runSliceLimits) { limit.CpuQuotaMicros = 0 },
		func(limit *runSliceLimits) { limit.CpuPeriodMicros = 999 },
		func(limit *runSliceLimits) { limit.CpuPeriodMicros = 1000001 },
		func(limit *runSliceLimits) { limit.CpuSet = "2,2" },
		func(limit *runSliceLimits) { limit.CpuSet = "" },
	} {
		groups, name, directory, limit := newContainmentLimitKernelTest(t)
		mutation(&limit)
		if err := groups.SetLimits(t.Context(), name, limit); err == nil {
			t.Fatal("unbounded typed request reached native writes")
		}
		for _, property := range []string{"memory.max", "memory.swap.max", "pids.max", "cpu.max", "cpuset.cpus"} {
			if content, err := os.ReadFile(filepath.Join(directory, property)); err != nil || len(content) != 0 {
				t.Fatal("invalid aggregate request changed a native property", property, err)
			}
		}
	}
}

// The native writer covers all five finite controls, and readback accepts the
// kernel's equivalent range syntax only for the same exact processor set.
func TestContainmentKernelLimitsWriteFiniteControlsAndVerifyEffectiveCpu(t *testing.T) {
	groups, name, directory, limit := newContainmentLimitKernelTest(t)
	limit.CpuSet = "2,3"
	if err := groups.SetLimits(t.Context(), name, limit); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cpuset.cpus.effective"), []byte("2-3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := groups.VerifyLimits(t.Context(), name, limit); err != nil {
		t.Fatal("equivalent positive kernel limits were refused", err)
	}
	for property, expected := range map[string]string{"memory.max": "12884901888\n", "memory.swap.max": "0\n", "pids.max": "4096\n", "cpu.max": "200000 100000\n", "cpuset.cpus": "2,3\n"} {
		content, err := os.ReadFile(filepath.Join(directory, property))
		if err != nil || string(content) != expected {
			t.Fatal("native aggregate write did not preserve the closed control policy", property, string(content), err)
		}
	}
}

// Unlimited, swapped, inherited/narrow, expanded, duplicate, malformed and
// oversized replies never count as a positive native resource acknowledgment.
func TestContainmentKernelLimitsRejectUnknownOrMismatchedReadback(t *testing.T) {
	for _, fault := range []struct{ property, value string }{
		{property: "memory.max", value: "max\n"}, {property: "memory.max", value: "12884901889\n"},
		{property: "memory.swap.max", value: "max\n"}, {property: "memory.swap.max", value: "1\n"},
		{property: "pids.max", value: "max\n"}, {property: "pids.max", value: "0\n"},
		{property: "cpu.max", value: "max 100000\n"}, {property: "cpu.max", value: "200000 99999\n"},
		{property: "cpu.max", value: "200000 100000\n200000 100000\n"},
		{property: "cpuset.cpus", value: "2\n"}, {property: "cpuset.cpus.effective", value: "2\n"},
		{property: "cpuset.cpus.effective", value: "2,4,6\n"}, {property: "cpuset.cpus.effective", value: "2,2,4\n"},
		{property: "cpuset.cpus.effective", value: "\n"}, {property: "cpuset.cpus.effective", value: "0-999999999\n"},
		{property: "memory.max", value: strings.Repeat("1", 129)},
	} {
		groups, name, directory, limit := newContainmentLimitKernelTest(t)
		if err := groups.SetLimits(t.Context(), name, limit); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, fault.property), []byte(fault.value), 0600); err != nil {
			t.Fatal(err)
		}
		if err := groups.VerifyLimits(t.Context(), name, limit); err == nil {
			t.Fatal("unknown native limit was accepted", fault.property, fmt.Sprintf("%q", fault.value))
		}
	}
}

// Missing controllers and replaced exact properties cannot silently skip caps
// or redirect a privileged write/read to a foreign filesystem object.
func TestContainmentKernelLimitsRejectMissingSymlinkAndNonCgroupHierarchy(t *testing.T) {
	for _, fault := range []string{"missing", "symlink", "non-cgroup"} {
		groups, name, directory, limit := newContainmentLimitKernelTest(t)
		foreign := filepath.Join(t.TempDir(), "foreign")
		if err := os.WriteFile(foreign, []byte("synthetic-untouched"), 0600); err != nil {
			t.Fatal(err)
		}
		if fault == "non-cgroup" {
			groups.requireCgroupFs = true
		} else {
			property := filepath.Join(directory, "cpuset.cpus")
			if err := os.Remove(property); err != nil {
				t.Fatal(err)
			}
			if fault == "symlink" {
				if err := os.Symlink(foreign, property); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := groups.SetLimits(t.Context(), name, limit); err == nil {
			t.Fatal("unknown controller/property hierarchy accepted aggregate writes", fault)
		}
		if content, err := os.ReadFile(foreign); err != nil || string(content) != "synthetic-untouched" {
			t.Fatal("refused cap write changed a foreign file", fault, err)
		}
	}
}
