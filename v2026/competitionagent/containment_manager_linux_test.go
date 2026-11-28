// Private manager replies and ordinary temporary files exercise both halves
// of admission without invoking systemctl, Docker or a native cgroup.
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
	"strings"
	"testing"
)

// Replies use the real show property's quota-per-second normalization.
func containmentManagerTestReply(limit runSliceLimits) []byte {
	return []byte(fmt.Sprintf("MemoryMax=%d\nMemorySwapMax=%d\nTasksMax=%d\nCPUQuotaPerSecUSec=%dus\nCPUQuotaPeriodUSec=%dus\nAllowedCPUs=%s\n",
		limit.MemoryBytes, limit.SwapBytes, limit.TasksMaximum, limit.CpuQuotaMicros/limit.CpuPeriodMicros*1000000,
		limit.CpuPeriodMicros, strings.ReplaceAll(limit.CpuSet, ",", " ")))
}

// Reading is one exact unit and six exact properties, including empty values.
func TestContainmentManagerReadUsesClosedExactUnitGrammar(t *testing.T) {
	_, name, _, _ := newContainmentLimitKernelTest(t)
	got, err := containmentManagerReadArguments(name)
	want := []string{"show", "--no-pager", "--all", "--property=MemoryMax,MemorySwapMax,TasksMax,CPUQuotaPerSecUSec,CPUQuotaPeriodUSec,AllowedCPUs", "--", name}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("manager observation expanded its typed vocabulary", got, err)
	}
	for _, name := range []string{"system.slice", "user.slice", "--all", "urnetwork-evaluation-*.slice", "../foreign.slice", ""} {
		if _, err := containmentManagerReadArguments(name); err == nil {
			t.Fatal("manager observation accepted a foreign or wildcard unit", name)
		}
	}
}

// These are systemd's formatted times/ranges, not an invented numeric-only API.
func TestContainmentManagerAcceptsExactFormattedTimesAndProcessorRanges(t *testing.T) {
	_, _, _, limit := newContainmentLimitKernelTest(t)
	for _, times := range []struct{ quota, period string }{
		{quota: "2s", period: "100ms"}, {quota: "2.000000s", period: "100.000ms"}, {quota: "2000ms", period: "100000us"},
	} {
		content := string(containmentManagerTestReply(limit))
		content = strings.Replace(content, "CPUQuotaPerSecUSec=2000000us", "CPUQuotaPerSecUSec="+times.quota, 1)
		content = strings.Replace(content, "CPUQuotaPeriodUSec=100000us", "CPUQuotaPeriodUSec="+times.period, 1)
		if err := validateContainmentManagerLimits([]byte(content), limit, 4096); err != nil {
			t.Fatal("real finite manager formatting was rejected", times, err)
		}
	}
	limit.CpuSet = "2,3"
	content := strings.Replace(string(containmentManagerTestReply(limit)), "AllowedCPUs=2 3", "AllowedCPUs=2-3", 1)
	if err := validateContainmentManagerLimits([]byte(content), limit, 4096); err != nil {
		t.Fatal("equivalent manager CPU range was rejected", err)
	}
}

// Every ceiling must be finite, complete and exactly equal to the frozen role.
func TestContainmentManagerRejectsUnknownUnboundedAndMismatchedProperties(t *testing.T) {
	_, _, _, limit := newContainmentLimitKernelTest(t)
	valid := string(containmentManagerTestReply(limit))
	for _, change := range []struct{ old, new string }{
		{old: "MemoryMax=12884901888", new: "MemoryMax=infinity"},
		{old: "MemoryMax=12884901888", new: "MemoryMax=12884901889"},
		{old: "MemoryMax=12884901888", new: "MemoryMax=012884901888"},
		{old: "MemorySwapMax=0", new: "MemorySwapMax=1"},
		{old: "MemorySwapMax=0", new: "MemorySwapMax="},
		{old: "TasksMax=4096", new: "TasksMax=infinity"},
		{old: "TasksMax=4096", new: "TasksMax=18446744073709551616"},
		{old: "TasksMax=4096\n", new: ""},
		{old: "CPUQuotaPerSecUSec=2000000us", new: "CPUQuotaPerSecUSec=infinity"},
		{old: "CPUQuotaPerSecUSec=2000000us", new: "CPUQuotaPerSecUSec=2.1s"},
		{old: "CPUQuotaPeriodUSec=100000us", new: "CPUQuotaPeriodUSec=99ms"},
		{old: "AllowedCPUs=2 4", new: "AllowedCPUs=2 4 6"},
		{old: "AllowedCPUs=2 4", new: "AllowedCPUs=2 2 4"},
		{old: "AllowedCPUs=2 4", new: "AllowedCPUs=0-999999999"},
		{old: "AllowedCPUs=2 4", new: "AllowedCPUs=2\t4"},
		{old: "AllowedCPUs=2 4", new: "AllowedCPUs="},
		{old: "TasksMax=4096", new: "TasksMax=4096\nTasksMax=4096"},
		{old: "TasksMax=4096", new: "TasksMax=4096\nUnrelated=yes"},
	} {
		content := []byte(strings.Replace(valid, change.old, change.new, 1))
		if err := validateContainmentManagerLimits(content, limit, 4096); err == nil {
			t.Fatal("unproven manager property became admission authority", change)
		}
	}
	if validateContainmentManagerLimits([]byte(valid), limit, int64(len(valid)-1)) == nil || validateContainmentManagerLimits([]byte(valid), limit, 0) == nil {
		t.Fatal("manager reply exceeded its independent finite output bound")
	}
}

// Parsing retains exact microseconds without float rounding or overflow.
func TestContainmentManagerDurationParsingIsExactAndFinite(t *testing.T) {
	for _, check := range []struct {
		value string
		want  int64
	}{{value: "1h 8min 16s", want: 4096 * 1000000}, {value: "100.001ms", want: 100001},
		{value: "0.000001s", want: 1}, {value: "1.000001s", want: 1000001}} {
		if got, err := containmentManagerMicroseconds(check.value); err != nil || got != check.want {
			t.Fatal("manager duration lost exact microseconds", check, got, err)
		}
	}
	for _, value := range []string{"", "0", "0s", "infinity", "max", "1e3s", "-1s", "+1s", "01s", "1.0000001s", "0.1us", "1s 1s", "1s 1min", "1s  1us", " 1s", "1s ", "9223372036854775807s", "9223372036854.999999s"} {
		if _, err := containmentManagerMicroseconds(value); err == nil {
			t.Fatal("nonfinite, ambiguous or inexact duration was accepted", value)
		}
	}
}

// This reproduces the former hole: correct kernel state cannot hide wrong
// manager defaults even after set-property reports a successful write.
func TestContainmentManagerMismatchRefusesOtherwiseCorrectKernel(t *testing.T) {
	groups, name, directory, limit := newContainmentLimitKernelTest(t)
	if err := groups.SetLimits(t.Context(), name, limit); err != nil {
		t.Fatal(err)
	}
	read := groups.limitReadCommand
	groups.limitReadCommand = func(ctx context.Context, args []string) ([]byte, error) {
		content, err := read(ctx, args)
		return []byte(strings.Replace(string(content), "MemoryMax=12884901888", "MemoryMax=infinity", 1)), err
	}
	if err := groups.VerifyLimits(t.Context(), name, limit); err == nil {
		t.Fatal("kernel-only readback hid unbounded manager state")
	}
	if content, err := os.ReadFile(filepath.Join(directory, "memory.max")); err != nil || string(content) != "12884901888\n" {
		t.Fatal("manager mismatch fixture did not retain correct kernel state", err)
	}
	groups.limitReadCommand = read
	if err := groups.VerifyLimits(t.Context(), name, limit); err != nil {
		t.Fatal("exact manager and kernel control was refused", err)
	}
}

// Independent manager proof must not replace the existing kernel proof.
func TestContainmentManagerCorrectStateCannotHideWrongKernel(t *testing.T) {
	groups, name, directory, limit := newContainmentLimitKernelTest(t)
	if err := groups.SetLimits(t.Context(), name, limit); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "memory.max"), []byte("max\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := groups.VerifyLimits(t.Context(), name, limit); err == nil {
		t.Fatal("manager-only proof hid an unlimited kernel boundary")
	}
}

// A later synthetic child realization reapplies only manager state. Divergence
// is refused both before and after it can reset an otherwise correct kernel.
func TestContainmentManagerChildRealizationCannotRestoreDefaultsSilently(t *testing.T) {
	groups, name, directory, limit := newContainmentLimitKernelTest(t)
	if err := groups.SetLimits(t.Context(), name, limit); err != nil {
		t.Fatal(err)
	}
	read := groups.limitReadCommand
	groups.limitReadCommand = func(ctx context.Context, args []string) ([]byte, error) {
		content, err := read(ctx, args)
		return []byte(strings.Replace(string(content), "MemoryMax=12884901888", "MemoryMax=infinity", 1)), err
	}
	if err := groups.VerifyLimits(t.Context(), name, limit); err == nil {
		t.Fatal("future child realization had permission to restore manager defaults")
	}
	if err := os.WriteFile(filepath.Join(directory, "memory.max"), []byte("max\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := groups.VerifyLimits(t.Context(), name, limit); err == nil {
		t.Fatal("realized manager defaults became valid kernel authority")
	}
	groups.limitReadCommand = read
	if err := groups.SetLimits(t.Context(), name, limit); err != nil {
		t.Fatal(err)
	}
	if err := groups.VerifyLimits(t.Context(), name, limit); err != nil {
		t.Fatal("unchanged finite manager+kernel control failed", err)
	}
}

// Read failure/cancellation never falls back to trusting a successful write.
func TestContainmentManagerReadFailureAndCanceledOwnerRefuseAdmission(t *testing.T) {
	groups, name, _, limit := newContainmentLimitKernelTest(t)
	if err := groups.SetLimits(t.Context(), name, limit); err != nil {
		t.Fatal(err)
	}
	groups.limitReadCommand = func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("synthetic manager unavailable")
	}
	if err := groups.VerifyLimits(t.Context(), name, limit); err == nil {
		t.Fatal("failed manager read fell back to successful write")
	}
	ctx, cancel := context.WithCancel(t.Context())
	groups.limitReadCommand = func(ctx context.Context, _ []string) ([]byte, error) {
		cancel()
		return containmentManagerTestReply(limit), nil
	}
	if err := groups.VerifyLimits(ctx, name, limit); err == nil {
		t.Fatal("canceled manager observation granted new admission")
	}
}

// Full preparation uses the real native-file implementation under one private
// directory. A manager mismatch refuses the complete authenticated inventory.
func TestContainmentManagerPrepareRequiresBothManagerAndKernelForEveryParent(t *testing.T) {
	fixture := newContainmentResourceTest(t)
	limits, err := prepareRunSliceLimits(fixture.plan, fixture.cleaner.config)
	if err != nil {
		t.Fatal(err)
	}
	groups := &hostCleanupCgroups{rootPath: t.TempDir(), maximumBytes: 128, managerMaximumBytes: 4096,
		limitCommandTimeout: fixture.cleaner.settings.CleanupCommandTimeout}
	expected := map[string]runSliceLimits{}
	committed := map[string]bool{}
	for index, name := range fixture.plan.CgroupNames {
		limit := limits[index]
		expected[name] = limit
		relative, err := cleanupSlicePath(name)
		if err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(groups.rootPath, relative)
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		for property, value := range map[string]string{
			"cgroup.events": "populated 0\nfrozen 0\n", "memory.max": fmt.Sprintf("%d\n", limit.MemoryBytes),
			"memory.swap.max": "0\n", "pids.max": fmt.Sprintf("%d\n", limit.TasksMaximum),
			"cpu.max":     fmt.Sprintf("%d %d\n", limit.CpuQuotaMicros, limit.CpuPeriodMicros),
			"cpuset.cpus": limit.CpuSet + "\n", "cpuset.cpus.effective": limit.CpuSet + "\n",
		} {
			if err := os.WriteFile(filepath.Join(directory, property), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	groups.limitCommand = func(ctx context.Context, arguments []string) error {
		if len(arguments) != 10 || ctx.Err() != nil {
			return errors.New("synthetic prepare manager received an invalid write")
		}
		name := arguments[3]
		want, err := systemdSliceLimitArguments(name, expected[name])
		if err != nil || !reflect.DeepEqual(arguments, want) {
			return errors.New("synthetic prepare write changed its exact frozen role")
		}
		committed[name] = true
		return nil
	}
	faulty := true
	last := fixture.plan.CgroupNames[len(fixture.plan.CgroupNames)-1]
	groups.limitReadCommand = func(ctx context.Context, arguments []string) ([]byte, error) {
		if len(arguments) != 6 || ctx.Err() != nil {
			return nil, errors.New("synthetic prepare manager received an invalid read")
		}
		name := arguments[5]
		want, err := containmentManagerReadArguments(name)
		if err != nil || !reflect.DeepEqual(arguments, want) || !committed[name] {
			return nil, errors.New("synthetic manager read lacks an exact committed parent")
		}
		content := containmentManagerTestReply(expected[name])
		if faulty && name == last {
			content = []byte(strings.Replace(string(content), fmt.Sprintf("MemoryMax=%d", expected[name].MemoryBytes), "MemoryMax=infinity", 1))
		}
		return content, nil
	}
	owner := &hostRunSlices{config: fixture.cleaner.config, settings: fixture.cleaner.settings, cgroups: groups,
		command: func(_ context.Context, operation string, names []string) error {
			if operation != "start" || !reflect.DeepEqual(names, fixture.plan.CgroupNames) {
				return errors.New("synthetic preparation changed its complete parent inventory")
			}
			return nil
		}}
	if err := owner.Prepare(t.Context(), fixture.plan); err == nil || len(committed) != len(fixture.plan.CgroupNames) {
		t.Fatal("complete correct kernel inventory hid the final parent's wrong manager state", err)
	}
	faulty = false
	if err := owner.Prepare(t.Context(), fixture.plan); err != nil {
		t.Fatal("complete exact manager and kernel inventory was refused", err)
	}
}

// The receipt cannot become a cached manager assertion: every retirement
// verification repeats native manager observation before current kernel facts.
func TestContainmentManagerRetirementVerifyResamplesManagerAfterReceipt(t *testing.T) {
	groups, name, directory, _ := newContainmentLimitKernelTest(t)
	policy := testBuildRetirementPolicy(t)
	policy.BuildCgroupName = name
	if err := groups.SetLimits(t.Context(), name, policy.Limits); err != nil {
		t.Fatal(err)
	}
	for property, content := range map[string][]byte{"cgroup.events": []byte("populated 0\nfrozen 1\n"), "memory.current": []byte("1024\n"),
		"memory.swap.current": []byte("0\n"), "memory.stat": testBuildRetirementStats()} {
		if err := os.WriteFile(filepath.Join(directory, property), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	engine := &hostBuildRetirement{settings: DefaultBuildRetirementSettings(), kernel: groups}
	if _, err := engine.Verify(t.Context(), policy); err != nil {
		t.Fatal("exact synthetic retired kernel and manager proof failed", err)
	}
	read := groups.limitReadCommand
	groups.limitReadCommand = func(ctx context.Context, arguments []string) ([]byte, error) {
		content, err := read(ctx, arguments)
		return []byte(strings.Replace(string(content), fmt.Sprintf("MemoryMax=%d", policy.Limits.MemoryBytes), "MemoryMax=infinity", 1)), err
	}
	if _, err := engine.Verify(t.Context(), policy); err == nil {
		t.Fatal("old retirement proof hid changed manager state")
	}
}

// Both native paths retain the same immutable boundary pin; nil synthetic
// plans have no authority to invoke a native manager mutation.
func TestContainmentManagerConstructorsBindProtectedBoundaryIdentity(t *testing.T) {
	pin := Pin{Path: "/synthetic/control/boundary.json", Sha256: strings.Repeat("a", 64)}
	config := Config{}
	config.DockerRole.ResourceBoundary = pin
	owner := newHostRunSlices(config, DefaultAgentSettings())
	groups, ok := owner.cgroups.(*hostCleanupCgroups)
	if !ok || groups.managerBoundary != pin || groups.managerMaximumBytes <= 0 || groups.limitReadCommand != nil {
		t.Fatal("preparation omitted its protected native manager binding")
	}
	retirer := newHostBuildRetirement(pin, DefaultBuildRetirementSettings())
	retiredGroups, ok := retirer.kernel.(*hostCleanupCgroups)
	if !ok || retiredGroups.managerBoundary != pin || retiredGroups.managerMaximumBytes <= 0 || retiredGroups.limitReadCommand != nil {
		t.Fatal("retirement omitted its protected native manager binding")
	}
	groups.managerBoundary = Pin{}
	if _, err := groups.runManager(t.Context(), []string{"show"}); err == nil {
		t.Fatal("absent boundary identity invoked a native manager")
	}
}

// Even an otherwise pinned manifest cannot turn this role into a generic
// command launcher by substituting a path, digest or malformed document.
func TestContainmentManagerExecutableDeclarationRejectsPathSubstitution(t *testing.T) {
	boundary := RuntimeResourceBoundary{Schema: Schema, Systemctl: Pin{Path: "/usr/bin/systemctl", Sha256: strings.Repeat("b", 64)}}
	encode := func() []byte {
		content, err := json.Marshal(boundary)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	if got, err := containmentManagerExecutablePin(encode(), 128*1024); err != nil || got != boundary.Systemctl {
		t.Fatal("fixed executable identity was not retained", got, err)
	}
	valid := string(encode())
	tool, err := json.Marshal(boundary.Systemctl)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"systemctl", "Systemctl", "SYSTEMCTL"} {
		ambiguous := strings.TrimSuffix(valid, "}") + ",\"" + key + "\":" + string(tool) + "}"
		if _, err := containmentManagerExecutablePin([]byte(ambiguous), 128*1024); err == nil {
			t.Fatal("duplicate or case-folded manager authority was accepted", key)
		}
	}
	for _, path := range []string{"/synthetic/bin/systemctl", "/bin/sh", "systemctl", "/usr/bin/../bin/systemctl"} {
		boundary.Systemctl.Path = path
		if _, err := containmentManagerExecutablePin(encode(), 128*1024); err == nil {
			t.Fatal("manager role accepted an alternate executable", path)
		}
	}
	boundary.Systemctl.Path, boundary.Systemctl.Sha256 = "/usr/bin/systemctl", ""
	if _, err := containmentManagerExecutablePin(encode(), 128*1024); err == nil {
		t.Fatal("manager role omitted its content identity")
	}
}

// The verified descriptor survives a pathname replacement, while reopening a
// changed or symlinked path cannot pass the original content pin.
func TestContainmentManagerExecDescriptorCannotFollowPathReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manager")
	original := []byte("synthetic pinned executable bytes\n")
	if err := os.WriteFile(path, original, 0700); err != nil {
		t.Fatal(err)
	}
	pin := Pin{Path: path, Sha256: digestDockerBytes(original)}
	file, err := openContainmentManagerExecutable(pin, 4096, false)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("synthetic replacement bytes\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if reopened, err := openContainmentManagerExecutable(pin, 4096, false); err == nil {
		reopened.Close()
		t.Fatal("substituted executable passed the original byte pin")
	}
	retained, err := io.ReadAll(file)
	if err != nil || string(retained) != string(original) {
		t.Fatal("verified execution descriptor followed a pathname replacement", err)
	}
	link := filepath.Join(root, "manager-link")
	if err := os.Symlink(filepath.Join(root, "original"), link); err != nil {
		t.Fatal(err)
	}
	pin.Path = link
	if reopened, err := openContainmentManagerExecutable(pin, 4096, false); err == nil {
		reopened.Close()
		t.Fatal("symlinked executable became authority")
	}
	if reopened, err := openContainmentManagerExecutable(Pin{Path: filepath.Join(root, "original"), Sha256: pin.Sha256}, int64(len(original)-1), false); err == nil {
		reopened.Close()
		t.Fatal("executable exceeded its finite byte bound")
	}
}
