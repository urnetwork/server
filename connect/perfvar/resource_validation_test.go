// This file validates PERFVAR resource surrogates in isolated scheduler
// configurations and reconciles workload goroutines, heap, and message pools.
package perfvar

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

const (
	perfvarResourceHelperEnvironment = "CONNECT_PERFVAR_RESOURCE_HELPER"
	perfvarResourceHelperName        = "CONNECT_PERFVAR_RESOURCE_HELPER_NAME"
)

// The helper observation makes the synthetic interpretation explicit in test
// output; GOMAXPROCS is a scheduler surrogate, not a physical CPU constraint.
type perfvarResourceHelperObservation struct {
	MeasurementKind        string          `json:"measurement_kind"`
	Resource               perfvarResource `json:"resource"`
	ResourceInterpretation string          `json:"resource_interpretation"`
	SchedulerConstraint    string          `json:"scheduler_constraint"`
	GoMaxProcs             int             `json:"go_max_procs"`
	PhysicalDevice         bool            `json:"physical_device"`
}

// A point-in-time lifecycle view distinguishes checked-out pooled buffers from
// the bounded free lists that the pools deliberately retain for reuse.
type perfvarResourceLifecycleSnapshot struct {
	GoroutineCount       int
	HeapAndStackInuse    uint64
	PoolTakenCount       uint64
	PoolReturnedCount    uint64
	PoolOutstandingCount int64
	PoolRetainedCount    int
	PoolCapacity         int
}

// The ordinary profile leaves production socket buffers unchanged and only
// raises the harness's packet and batch capacities.
func TestPerfvarDefaultResourceProfile(t *testing.T) {
	resources := perfvarTunResources(perfvarResourceDefault)
	if resources.ChannelSize != 4096 {
		t.Errorf("channel capacity=%d, want 4096", resources.ChannelSize)
	}
	if resources.TcpBufferDefault != 0 {
		t.Errorf("TCP default override=%d, want production default", resources.TcpBufferDefault)
	}
	if resources.TcpBufferMax != 0 {
		t.Errorf("TCP maximum override=%d, want production default", resources.TcpBufferMax)
	}
	if resources.UdpBuffer != 0 {
		t.Errorf("UDP override=%d, want production default", resources.UdpBuffer)
	}
	if resources.BatchSize != 64 {
		t.Errorf("batch size=%d, want 64", resources.BatchSize)
	}
	if resources.AppDelay != 0 {
		t.Errorf("application delay=%s, want none", resources.AppDelay)
	}
	if resources.ApplicationMtu != 0 {
		t.Errorf("application MTU=%d, want profile default", resources.ApplicationMtu)
	}
}

// The constrained profile pins every documented synthetic TUN and application
// boundary setting without implying a particular phone or operating system.
func TestPerfvarMobileSurrogateResourceProfile(t *testing.T) {
	resources := perfvarTunResources(perfvarResourceMobile)
	if resources.ChannelSize != 256 {
		t.Errorf("channel capacity=%d, want 256", resources.ChannelSize)
	}
	if resources.TcpBufferDefault != 256*1024 {
		t.Errorf("TCP default override=%d, want %d", resources.TcpBufferDefault, 256*1024)
	}
	if resources.TcpBufferMax != 2*1024*1024 {
		t.Errorf("TCP maximum override=%d, want %d", resources.TcpBufferMax, 2*1024*1024)
	}
	if resources.UdpBuffer != 128*1024 {
		t.Errorf("UDP override=%d, want %d", resources.UdpBuffer, 128*1024)
	}
	if resources.BatchSize != 8 {
		t.Errorf("batch size=%d, want 8", resources.BatchSize)
	}
	if resources.AppDelay != 100*time.Microsecond {
		t.Errorf("application delay=%s, want %s", resources.AppDelay, 100*time.Microsecond)
	}
	if resources.ApplicationMtu != 0 {
		t.Errorf("application MTU=%d, want profile default", resources.ApplicationMtu)
	}
}

// Full-route fixtures advertise the same MTU as the product VPN by default,
// without conflating it with the independently modeled physical link MTU.
func TestResolvedFullTunApplicationMtu(t *testing.T) {
	profile := initialNetworkProfiles(20260819)["clean-lan"]
	if got := resolvedFullTunApplicationMtu(profile, defaultTunResourceProfile()); got != clientconnect.DefaultMtu {
		t.Fatalf("default full-TUN application MTU=%d want=%d", got, clientconnect.DefaultMtu)
	}

	smallProfile := profile
	smallProfile.InnerMtu = clientconnect.DefaultMtu - 100
	if got := resolvedFullTunApplicationMtu(smallProfile, defaultTunResourceProfile()); got != smallProfile.InnerMtu {
		t.Fatalf("small-profile application MTU=%d want=%d", got, smallProfile.InnerMtu)
	}

	resources := defaultTunResourceProfile()
	resources.ApplicationMtu = 1200
	if got := resolvedFullTunApplicationMtu(profile, resources); got != resources.ApplicationMtu {
		t.Fatalf("explicit application MTU=%d want=%d", got, resources.ApplicationMtu)
	}

	if fullTunQUICInitialPacketSize <= clientconnect.DefaultMtu {
		t.Fatalf(
			"QUIC Initial=%d must exercise IPv4 fragmentation above product MTU=%d",
			fullTunQUICInitialPacketSize,
			clientconnect.DefaultMtu,
		)
	}
}

// The constrained profile starts small but permits the same bounded TCP
// auto-tuning available to a 32 MiB mobile process budget.
func TestPerfvarMobileSurrogateTcpWindowRange(t *testing.T) {
	settings := clientconnect.DefaultTunSettings()
	resources := mobileTunResourceProfile()
	applyTunResourceProfile(settings, resources)
	for name, bufferRange := range map[string]clientconnect.TcpBufferRange{
		"receive": settings.TcpReceiveBuffer,
		"send":    settings.TcpSendBuffer,
	} {
		if bufferRange.Default != 256*1024 || bufferRange.Max != 2*1024*1024 {
			t.Errorf("%s TCP buffer range=%+v", name, bufferRange)
		}
	}
}

// Machine-readable labels state both same-host scope and surrogate status so a
// resource result cannot reasonably be presented as physical-device evidence.
func TestPerfvarResourceLabelsAreSynthetic(t *testing.T) {
	if perfvarResourceMobile != "mobile-surrogate" {
		t.Fatalf("mobile resource label=%q, want an explicit surrogate", perfvarResourceMobile)
	}
	metadata := currentPerfvarHostMetadata()
	if metadata.MeasurementKind != "userspace-same-host" {
		t.Fatalf("measurement kind=%q, want userspace-same-host", metadata.MeasurementKind)
	}
	observation := perfvarResourceHelperObservation{
		MeasurementKind:        metadata.MeasurementKind,
		Resource:               perfvarResourceMobile,
		ResourceInterpretation: "synthetic-tun-resource-profile",
		SchedulerConstraint:    "gomaxprocs-surrogate",
		GoMaxProcs:             runtime.GOMAXPROCS(0),
		PhysicalDevice:         false,
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"resource":"mobile-surrogate"`,
		`"resource_interpretation":"synthetic-tun-resource-profile"`,
		`"scheduler_constraint":"gomaxprocs-surrogate"`,
		`"physical_device":false`,
	} {
		if !strings.Contains(string(encoded), required) {
			t.Errorf("resource observation %s does not contain %s", encoded, required)
		}
	}
}

// Separate processes exercise the real runtime startup setting at one and two
// scheduler threads for both resource profiles; changing it in-process would
// contaminate other package tests and would not reproduce startup behavior.
func TestPerfvarResourceGoMaxProcsSweep(t *testing.T) {
	if testing.Short() {
		return
	}
	if perfvarRaceEnabled {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	setEnvironment := func(environment []string, name string, value string) []string {
		prefix := name + "="
		filtered := make([]string, 0, len(environment)+1)
		for _, entry := range environment {
			if !strings.HasPrefix(entry, prefix) {
				filtered = append(filtered, entry)
			}
		}
		return append(filtered, prefix+value)
	}
	for _, goMaxProcs := range []int{1, 2} {
		for _, resource := range []perfvarResource{perfvarResourceDefault, perfvarResourceMobile} {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			command := exec.CommandContext(
				ctx,
				executable,
				"-test.run=^TestPerfvarResourceHelperProcess$",
				"-test.count=1",
				"-test.timeout=75s",
				"-test.v",
			)
			environment := setEnvironment(os.Environ(), "GOMAXPROCS", strconv.Itoa(goMaxProcs))
			environment = setEnvironment(environment, perfvarResourceHelperEnvironment, "1")
			environment = setEnvironment(environment, perfvarResourceHelperName, string(resource))
			command.Env = environment
			output, commandErr := command.CombinedOutput()
			cancel()
			if commandErr != nil {
				t.Errorf(
					"resource helper resource=%s GOMAXPROCS=%d: %v\n%s",
					resource,
					goMaxProcs,
					commandErr,
					output,
				)
				continue
			}
			for _, required := range []string{
				fmt.Sprintf(`"resource":"%s"`, resource),
				fmt.Sprintf(`"go_max_procs":%d`, goMaxProcs),
				`"measurement_kind":"userspace-same-host"`,
				`"resource_interpretation":"synthetic-tun-resource-profile"`,
				`"scheduler_constraint":"gomaxprocs-surrogate"`,
				`"physical_device":false`,
			} {
				if !strings.Contains(string(output), required) {
					t.Errorf(
						"resource helper resource=%s GOMAXPROCS=%d output lacks %s:\n%s",
						resource,
						goMaxProcs,
						required,
						output,
					)
				}
			}
		}
	}
}

// The child runs concurrent TCP and paced UDP over production gVisor TUNs.
// It is selected only by the parent process's exact test-name filter.
func TestPerfvarResourceHelperProcess(t *testing.T) {
	if os.Getenv(perfvarResourceHelperEnvironment) != "1" {
		return
	}
	resource := perfvarResource(os.Getenv(perfvarResourceHelperName))
	if resource != perfvarResourceDefault && resource != perfvarResourceMobile {
		t.Fatalf("unknown helper resource %q", resource)
	}
	expectedGoMaxProcs, err := strconv.Atoi(os.Getenv("GOMAXPROCS"))
	if err != nil {
		t.Fatalf("parse GOMAXPROCS: %v", err)
	}
	if actual := runtime.GOMAXPROCS(0); actual != expectedGoMaxProcs {
		t.Fatalf("GOMAXPROCS=%d, want %d", actual, expectedGoMaxProcs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	resources := perfvarTunResources(resource)
	tcpResult, err := measureTCPWorkload(ctx, profile, resources, true, 2, 128*1024)
	if err != nil {
		t.Fatalf("TCP resource helper: %v", err)
	}
	if tcpResult.UsefulByteCount != 256*1024 || tcpResult.ContentHash == "" {
		t.Fatalf("TCP resource helper result=%+v", tcpResult)
	}
	udpResult, err := measureUDPWorkload(ctx, profile, resources, 40*time.Millisecond, 5_000_000, 1000)
	if err != nil {
		t.Fatalf("UDP resource helper: %v", err)
	}
	if udpResult.DeliveredPacketCount == 0 || udpResult.CorruptPacketCount != 0 {
		t.Fatalf("UDP resource helper result=%+v", udpResult)
	}
	observation := perfvarResourceHelperObservation{
		MeasurementKind:        "userspace-same-host",
		Resource:               resource,
		ResourceInterpretation: "synthetic-tun-resource-profile",
		SchedulerConstraint:    "gomaxprocs-surrogate",
		GoMaxProcs:             runtime.GOMAXPROCS(0),
		PhysicalDevice:         false,
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("[perfvar-resource] %s", encoded)
}

// A representative lifecycle iteration covers concurrent reliable streams and
// datagram ownership through fresh TUN and scheduler construction and teardown.
func runPerfvarResourceLifecycleWorkloads(
	ctx context.Context,
	profile networkProfile,
	resources tunResourceProfile,
) error {
	tcpResult, err := measureTCPWorkload(ctx, profile, resources, true, 2, 128*1024)
	if err != nil {
		return fmt.Errorf("TCP lifecycle workload: %w", err)
	}
	if tcpResult.UsefulByteCount != 256*1024 || tcpResult.ContentHash == "" {
		return fmt.Errorf("TCP lifecycle verification: %+v", tcpResult)
	}
	udpResult, err := measureUDPWorkload(ctx, profile, resources, 40*time.Millisecond, 5_000_000, 1000)
	if err != nil {
		return fmt.Errorf("UDP lifecycle workload: %w", err)
	}
	if udpResult.DeliveredPacketCount == 0 || udpResult.CorruptPacketCount != 0 {
		return fmt.Errorf("UDP lifecycle verification: %+v", udpResult)
	}
	return nil
}

// A collected snapshot is taken only after each workload has joined its
// workers. Sampling once makes a delayed owner visible instead of polling
// global counts until unrelated activity happens to produce the old value.
func capturePerfvarResourceLifecycle() perfvarResourceLifecycleSnapshot {
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	takenCount, returnedCount, _ := clientconnect.MessagePoolCounts()
	retainedCount := 0
	capacity := 0
	for _, stats := range clientconnect.GetMessagePoolClassStats() {
		retainedCount += stats.Retained
		capacity += stats.Capacity
	}
	return perfvarResourceLifecycleSnapshot{
		GoroutineCount:       runtime.NumGoroutine(),
		HeapAndStackInuse:    memory.HeapInuse + memory.StackInuse,
		PoolTakenCount:       takenCount,
		PoolReturnedCount:    returnedCount,
		PoolOutstandingCount: int64(takenCount) - int64(returnedCount),
		PoolRetainedCount:    retainedCount,
		PoolCapacity:         capacity,
	}
}

// Exact ownership and worker counts are checked separately from the bounded
// heap allowance. Returning to a nonzero warm baseline is still a pool leak.
func perfvarResourceLifecycleFailures(
	before perfvarResourceLifecycleSnapshot,
	after perfvarResourceLifecycleSnapshot,
) []string {
	failures := []string{}
	if after.GoroutineCount != before.GoroutineCount {
		failures = append(failures, fmt.Sprintf(
			"goroutines did not reconcile: %d -> %d",
			before.GoroutineCount,
			after.GoroutineCount,
		))
	}
	if before.PoolOutstandingCount != 0 || after.PoolOutstandingCount != 0 {
		failures = append(failures, fmt.Sprintf(
			"pool ownership did not reconcile to zero: %d -> %d",
			before.PoolOutstandingCount,
			after.PoolOutstandingCount,
		))
	}
	if after.PoolTakenCount <= before.PoolTakenCount {
		failures = append(failures, "lifecycle did not exercise the message pool")
	}
	if after.PoolCapacity < after.PoolRetainedCount {
		failures = append(failures, fmt.Sprintf(
			"retained pool buffers=%d exceed capacity=%d",
			after.PoolRetainedCount,
			after.PoolCapacity,
		))
	}
	const retainedMemoryAllowance = 16 * 1024 * 1024
	if !perfvarRaceEnabled && before.HeapAndStackInuse+retainedMemoryAllowance < after.HeapAndStackInuse {
		failures = append(failures, fmt.Sprintf(
			"heap+stack grew from %d to %d bytes, allowance=%d",
			before.HeapAndStackInuse,
			after.HeapAndStackInuse,
			retainedMemoryAllowance,
		))
	}
	return failures
}

// A warm-up leak and one surviving worker are deterministic failures even
// when the before/after deltas would have fit the former tolerances.
func TestPerfvarResourceLifecycleRejectsWarmBaselineAndWorkerLeaks(t *testing.T) {
	before := perfvarResourceLifecycleSnapshot{
		GoroutineCount:       5,
		HeapAndStackInuse:    8 * 1024 * 1024,
		PoolTakenCount:       10,
		PoolReturnedCount:    9,
		PoolOutstandingCount: 1,
		PoolRetainedCount:    1,
		PoolCapacity:         2,
	}
	after := before
	after.GoroutineCount += 1
	after.PoolTakenCount += 1
	after.PoolReturnedCount += 1
	failures := perfvarResourceLifecycleFailures(before, after)
	if len(failures) != 2 {
		t.Fatalf("warm baseline and worker leaks produced %d failures, want 2: %v", len(failures), failures)
	}
	if !strings.Contains(failures[0], "goroutines") || !strings.Contains(failures[1], "pool ownership") {
		t.Fatalf("unexpected lifecycle failures: %v", failures)
	}

	balanced := before
	balanced.PoolReturnedCount = balanced.PoolTakenCount
	balanced.PoolOutstandingCount = 0
	balancedAfter := balanced
	balancedAfter.PoolTakenCount += 1
	balancedAfter.PoolReturnedCount += 1
	if failures := perfvarResourceLifecycleFailures(balanced, balancedAfter); len(failures) != 0 {
		t.Fatalf("balanced lifecycle failed validation: %v", failures)
	}
}

// Repeated default-resource paths must join workers, release checked-out pool
// messages, and leave only a bounded amount of reusable runtime memory.
func TestPerfvarDefaultResourceLifecycleReconciles(t *testing.T) {
	validatePerfvarResourceLifecycle(t, perfvarResourceDefault)
}

// Repeated constrained-resource paths have the same ownership and teardown
// contract as the default even with smaller channels and socket buffers.
func TestPerfvarMobileSurrogateLifecycleReconciles(t *testing.T) {
	validatePerfvarResourceLifecycle(t, perfvarResourceMobile)
}

// Validation warms process-global caches, then compares several fresh cycles
// with a stable, collected lifecycle baseline.
func validatePerfvarResourceLifecycle(t *testing.T, resource perfvarResource) {
	t.Helper()
	if testing.Short() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	resources := perfvarTunResources(resource)
	if err := runPerfvarResourceLifecycleWorkloads(ctx, profile, resources); err != nil {
		t.Fatal(err)
	}
	before := capturePerfvarResourceLifecycle()
	for range 3 {
		if err := runPerfvarResourceLifecycleWorkloads(ctx, profile, resources); err != nil {
			t.Fatal(err)
		}
	}
	after := capturePerfvarResourceLifecycle()
	for _, failure := range perfvarResourceLifecycleFailures(before, after) {
		t.Errorf("resource=%s %s", resource, failure)
	}
	t.Logf(
		"resource=%s synthetic lifecycle goroutines=%d->%d heap+stack=%d->%d pool_outstanding=%d->%d pool_taken_delta=%d",
		resource,
		before.GoroutineCount,
		after.GoroutineCount,
		before.HeapAndStackInuse,
		after.HeapAndStackInuse,
		before.PoolOutstandingCount,
		after.PoolOutstandingCount,
		after.PoolTakenCount-before.PoolTakenCount,
	)
}
