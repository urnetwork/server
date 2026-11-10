package main

// The soak on T2, stepped on a fake clock so each run repeats exactly: a planted goroutine leak and a planted Redis
// key leak each fail their signal, a clean short run reads back on what it reads and is no evidence, each signal's
// comparison on its own, and the stack target refused rather than run on fakes.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/load"
)

// A short soak on clock: phases waited through by moving it, and each service's arrivals run one after another,
// the clock moved by a gap between them.
func fakeSoakSettings(clock *sidecartest.FakeClock) *soakSettings {
	settings := shortSoakSettings()
	settings.Duration = 2 * time.Minute
	settings.Warmup = 30 * time.Second
	settings.SampleCount = 3
	settings.SampleInterval = 10 * time.Second
	settings.Subscribers = 20
	settings.Clock = clock
	settings.wait = func(ctx context.Context, wait time.Duration) error {
		clock.Advance(wait)
		return nil
	}
	settings.pace = func(ctx context.Context, service *soakService, rate float64, duration time.Duration) *load.Report {
		count := int64(rate * duration.Seconds())
		gap := time.Duration(float64(time.Second) / rate)
		report := &load.Report{
			Name: service.Name, OfferedPerSecond: rate, Scheduled: count,
			Settings: map[string]string{}, CommitSet: map[string]string{}, FailureKinds: map[string]int64{},
		}
		for range count {
			err := service.Workload(ctx, service.arrivals)
			service.arrivals += 1
			report.Arrivals += 1
			if err != nil {
				report.Failed += 1
				report.FailureKinds[err.Error()] += 1
			} else {
				report.Succeeded += 1
			}
			clock.Advance(gap)
		}
		return report
	}
	return settings
}

// The t2 target with planted services beside its mix.
type plantedSoakTarget struct {
	*t2SoakTarget
	mix []*soakService
}

func newPlantedSoakTarget(t *testing.T, clock *sidecartest.FakeClock, planted func(target *t2SoakTarget) *soakService) *plantedSoakTarget {
	t.Helper()
	target, err := newT2SoakTarget(t.Context(), clock, 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(target.Close)
	mix := target.Mix()
	if planted != nil {
		mix = append(mix, planted(target))
	}
	return &plantedSoakTarget{t2SoakTarget: target, mix: mix}
}

func (self *plantedSoakTarget) Mix() []*soakService {
	return self.mix
}

func verdictOf(t *testing.T, report *soakReport, signal soakSignal) *soakVerdict {
	t.Helper()
	for _, verdict := range report.Verdicts {
		if verdict.Signal == signal {
			return verdict
		}
	}
	t.Fatalf("no verdict for %s: %+v", signal, report)
	return nil
}

// A service that leaves one goroutine behind every arrival fails the goroutine signal, and the run.
func TestSoakFailsAPlantedGoroutineLeak(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	leaked := make(chan struct{})
	t.Cleanup(func() { close(leaked) })
	target := newPlantedSoakTarget(t, clock, func(target *t2SoakTarget) *soakService {
		return &soakService{Name: "planted-goroutines", CapacityPerSecond: 10, CapacitySource: "a test's", Workload: func(ctx context.Context, arrival int64) error {
			go func() { <-leaked }()
			return nil
		}}
	})
	report := runSoak(t.Context(), fakeSoakSettings(clock), target)
	if verdict := verdictOf(t, report, soakSignalGoroutines); verdict.Outcome != soakLeaked {
		t.Fatalf("a goroutine a load arrival left behind read %s: %+v", verdict.Outcome, verdict)
	}
	if report.Passed {
		t.Fatal("a leaking soak passed")
	}
}

// A service that writes a key with no ttl every arrival fails the Redis key signal, and the run.
func TestSoakFailsAPlantedKeyLeak(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	target := newPlantedSoakTarget(t, clock, func(target *t2SoakTarget) *soakService {
		return &soakService{Name: "planted-keys", CapacityPerSecond: 10, CapacitySource: "a test's", Workload: func(ctx context.Context, arrival int64) error {
			return target.fakes.Redis.Set(ctx, fmt.Sprintf("{soak-planted}%d", arrival), []byte("kept"), 0)
		}}
	})
	report := runSoak(t.Context(), fakeSoakSettings(clock), target)
	if verdict := verdictOf(t, report, soakSignalRedisKeys); verdict.Outcome != soakLeaked {
		t.Fatalf("keys a load left with no ttl read %s: %+v", verdict.Outcome, verdict)
	}
	if verdict := verdictOf(t, report, soakSignalGoroutines); verdict.Outcome != soakBack {
		t.Fatalf("goroutines in a key leak's run read %s: %+v", verdict.Outcome, verdict)
	}
	if report.Passed {
		t.Fatal("a leaking soak passed")
	}
}

// The evs mix alone: goroutines and keys read back, heap and orphans open on t2, every commit's hints delivered, the
// run passes, and a short run on fakes is no evidence.
func TestSoakCleanShortRunPassesAsNoEvidence(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	target := newPlantedSoakTarget(t, clock, nil)
	report := runSoak(t.Context(), fakeSoakSettings(clock), target)
	if !report.Passed {
		t.Fatalf("a clean soak failed: %+v", report.Verdicts)
	}
	for signal, want := range map[soakSignal]soakOutcome{
		soakSignalGoroutines: soakBack, soakSignalRedisKeys: soakBack, soakSignalHeap: soakOpen, soakSignalOrphans: soakOpen,
	} {
		if verdict := verdictOf(t, report, signal); verdict.Outcome != want {
			t.Errorf("%s read %s, want %s: %+v", signal, verdict.Outcome, want, verdict)
		}
	}
	if len(report.Load) != 1 || report.Load[0].Arrivals == 0 || report.Load[0].Failed != 0 {
		t.Fatalf("the load phase: %+v", report.Load)
	}
	if !report.Smoke {
		t.Fatal("a short run is no smoke")
	}
	err := report.gateEvidence()
	if err == nil || !strings.Contains(err.Error(), "a smoke that proves nothing") || !strings.Contains(err.Error(), "orphan_objects read open") {
		t.Fatalf("a short run on fakes reached a gate: %v", err)
	}
}

// Each signal on its own: within its tolerance back, past it leaked, unread or open on its target open.
func TestCompareSoakReadsEachSignal(t *testing.T) {
	reading := func(heap uint64, goroutines int, keys int64, orphans int64) *soakReading {
		samples := []load.Sample{}
		for range 3 {
			samples = append(samples, load.Sample{HeapAllocBytes: heap, Goroutines: goroutines, RedisKeys: keys})
		}
		return &soakReading{Samples: samples, Orphans: orphans}
	}
	tolerances := defaultSoakTolerances()
	baseline := reading(100*1024*1024, 50, 40, 3)
	cases := []struct {
		end  *soakReading
		open map[soakSignal]string
		want map[soakSignal]soakOutcome
	}{
		{reading(100*1024*1024+26*1024*1024, 58, 56, 3), nil,
			map[soakSignal]soakOutcome{soakSignalHeap: soakBack, soakSignalGoroutines: soakBack, soakSignalRedisKeys: soakBack, soakSignalOrphans: soakBack}},
		{reading(100*1024*1024+27*1024*1024, 59, 57, 4), nil,
			map[soakSignal]soakOutcome{soakSignalHeap: soakLeaked, soakSignalGoroutines: soakLeaked, soakSignalRedisKeys: soakLeaked, soakSignalOrphans: soakLeaked}},
		{reading(1, 1, -1, -1), map[soakSignal]string{soakSignalHeap: "fakes"},
			map[soakSignal]soakOutcome{soakSignalHeap: soakOpen, soakSignalGoroutines: soakBack, soakSignalRedisKeys: soakOpen, soakSignalOrphans: soakOpen}},
	}
	for i, each := range cases {
		for _, verdict := range compareSoak(baseline, each.end, tolerances, each.open) {
			if verdict.Outcome != each.want[verdict.Signal] {
				t.Errorf("case %d: %s read %s, want %s: %+v", i, verdict.Signal, verdict.Outcome, each.want[verdict.Signal], verdict)
			}
		}
	}
}

// The stack target is not built, so the program refuses it and exits 2 rather than run on fakes.
func TestSoakRefusesTheUnbuiltStackTarget(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if code := run(t.Context(), []string{"-short", "-target", "stack"}, stdout, stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "the stack target is not built") {
		t.Fatalf("stderr %q", stderr.String())
	}
}
