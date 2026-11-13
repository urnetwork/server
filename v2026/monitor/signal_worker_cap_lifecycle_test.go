package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Override the actual public Host callback; Shell is not a SignalSource API.
type workerCapLifecycleSource struct {
	SignalSource
	hostContextFn  func(context.Context, HostSettings, string) (string, error)
	localContextFn func(context.Context, string, ...string) (string, error)
}

func (self *workerCapLifecycleSource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	return self.hostContextFn(ctx, host, command)
}

// Preserve the optional local source capability used by memory/churn enrichment.
func (self *workerCapLifecycleSource) Local(ctx context.Context, name string, args ...string) (string, error) {
	if self.localContextFn != nil {
		return self.localContextFn(ctx, name, args...)
	}
	if source, ok := self.SignalSource.(LocalSignalSource); ok {
		return source.Local(ctx, name, args...)
	}
	return "", errors.New("synthetic local source capability unavailable")
}

// A live precondition proves valid settings, actual callback entry and pressure
// before the same context-ignoring source is given an already-ended parent.
func TestWorkerCapEndedParentNeverEntersHost(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		for _, failSource := range []bool{false, true} {
			now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
			settings := workerCapSyntheticSettings(t, now, workerCapFixture{host: "edge-a", block: "g1", instance: "synthetic-pressure", cpu: 3.8, maxprocs: 4, goroutines: 20_000})
			baseSource := settings.Source
			calls := 0
			var sourceErr error
			settings.Source = &workerCapLifecycleSource{
				SignalSource: baseSource,
				hostContextFn: func(ctx context.Context, host HostSettings, command string) (string, error) {
					calls++
					payload, err := baseSource.Host(context.Background(), host, command)
					if err != nil {
						t.Fatal(err)
					}
					return payload, sourceErr
				},
			}
			alerts, err := NewWorkerCapSignal().Run(context.Background(), settings)
			if err != nil || calls != 1 || requireAlertClassCount(alerts, "worker-scheduler-capacity") != 1 {
				t.Fatalf("live entry precondition failed: calls=%d alerts=%d err=%v", calls, len(alerts), err)
			}
			calls = 0
			if failSource {
				sourceErr = errors.New("synthetic-private-worker-source.example")
			}
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			if deadline {
				parent, cancel = context.WithDeadline(context.Background(), time.Time{})
				defer cancel()
			}
			alerts, err = NewWorkerCapSignal().Run(parent, settings)
			if calls != 0 {
				t.Fatalf("ended parent entered Host callback: deadline=%t sourceFailure=%t calls=%d", deadline, failSource, calls)
			}
			if !errors.Is(err, parent.Err()) || len(alerts) != 0 {
				t.Fatalf("ended parent published findings or lost cause: alerts=%d err=%v", len(alerts), err)
			}
		}
	}
}

// A callback can cancel its parent then return a valid complete/partial payload
// anyway. Neither independently valid pressure nor visibility may escape it.
func TestWorkerCapCancellationDuringHostDropsAllFindings(t *testing.T) {
	for _, partial := range []bool{false, true} {
		now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		workers := []workerCapFixture{{host: "edge-a", block: "g1", instance: "synthetic-pressure", cpu: 3.8, maxprocs: 4, goroutines: 20_000}}
		if partial {
			workers = append(workers, workerCapFixture{host: "edge-b", block: "g2", instance: "synthetic-incomplete", cpu: 1, goroutines: 500, omitMaxprocs: true})
		}
		settings := workerCapSyntheticSettings(t, now, workers...)
		baseSource := settings.Source
		parent, cancel := context.WithCancel(context.Background())
		calls := 0
		settings.Source = &workerCapLifecycleSource{SignalSource: baseSource,
			hostContextFn: func(_ context.Context, host HostSettings, command string) (string, error) {
				calls++
				payload, err := baseSource.Host(context.Background(), host, command)
				cancel()
				return payload, err
			},
		}
		alerts, err := NewWorkerCapSignal().Run(parent, settings)
		cancel()
		if calls != 1 || !errors.Is(err, context.Canceled) || len(alerts) != 0 {
			t.Fatalf("callback cancellation leaked findings: partial=%t calls=%d alerts=%d err=%v", partial, calls, len(alerts), err)
		}
	}
}

// Cancellation at the reducer's clock read is later than source return. The
// same owner still prevents the completed pressure/partial tuple publishing.
func TestWorkerCapCancellationDuringReductionDropsAllFindings(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	settings := workerCapSyntheticSettings(t, now,
		workerCapFixture{host: "edge-a", block: "g1", instance: "synthetic-pressure", cpu: 3.8, maxprocs: 4, goroutines: 20_000},
		workerCapFixture{host: "edge-b", block: "g2", instance: "synthetic-incomplete", cpu: 1, goroutines: 500, omitMaxprocs: true})
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	clockReads := 0
	settings.Now = func() time.Time {
		clockReads++
		cancel()
		return now
	}
	alerts, err := NewWorkerCapSignal().Run(parent, settings)
	if clockReads != 1 || !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("reducer cancellation escaped: clock=%d alerts=%d err=%v", clockReads, len(alerts), err)
	}
}

// Ending the owner must retain an independent source failure without exposing
// private source text as the lifecycle verdict. Live failures remain errors.
func TestWorkerCapLifecyclePreservesIndependentSourceFailure(t *testing.T) {
	for _, ended := range []bool{false, true} {
		now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		settings := workerCapSyntheticSettings(t, now)
		parent, cancel := context.WithCancel(context.Background())
		failure := errors.New("synthetic-private-independent-worker.example")
		calls := 0
		settings.Source = &workerCapLifecycleSource{SignalSource: settings.Source,
			hostContextFn: func(context.Context, HostSettings, string) (string, error) {
				calls++
				if ended {
					cancel()
				}
				return "", failure
			},
		}
		alerts, err := NewWorkerCapSignal().Run(parent, settings)
		cancel()
		if calls != 1 || !errors.Is(err, failure) || len(alerts) != 0 {
			t.Fatalf("independent source cause lost: ended=%t calls=%d alerts=%d err=%v", ended, calls, len(alerts), err)
		}
		if ended && (!errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() || strings.Contains(err.Error(), failure.Error())) {
			t.Fatal("ended owner lost lifecycle precedence or exposed private error text")
		}
		if !ended && errors.Is(err, context.Canceled) {
			t.Fatal("live source failure invented parent cancellation")
		}
	}
}

// Live pressure and incomplete-runtime visibility remain independent after the
// lifecycle guard; an ordinary partial response cannot become cancellation.
func TestWorkerCapLivePartialPreservesPressureAndVisibility(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	settings := workerCapSyntheticSettings(t, now,
		workerCapFixture{host: "edge-a", block: "g1", instance: "synthetic-pressure", cpu: 3.8, maxprocs: 4, goroutines: 20_000},
		workerCapFixture{host: "edge-b", block: "g2", instance: "synthetic-incomplete", cpu: 1, goroutines: 500, omitMaxprocs: true})
	parent := context.Background()
	alerts, err := NewWorkerCapSignal().Run(parent, settings)
	if err != nil || parent.Err() != nil || requireAlertClassCount(alerts, "worker-scheduler-capacity") != 1 || requireAlertClassCount(alerts, "worker-scheduler-unobservable") != 1 {
		t.Fatalf("live independent pressure/partial visibility changed: alerts=%d err=%v", len(alerts), err)
	}
}

// Fixtures expose the actual metric families and source clocks each adjacent
// constructor requests. Empty optional lifecycle logs cannot create a task.
func workerAdjacentLifecycleSettings(t *testing.T, now time.Time, memory bool, quiet bool) SignalSettings {
	t.Helper()
	workers := []workerMetricFixture{
		{host: "edge-a", block: "g1", instance: "synthetic-quiet-a", heap: 128 * 1024 * 1024, cpuRate: 0.02, allocRate: 1024 * 1024},
		{host: "edge-b", block: "g1", instance: "synthetic-quiet-b", heap: 192 * 1024 * 1024, cpuRate: 0.03, allocRate: 2 * 1024 * 1024},
		{host: "edge-c", block: "g2", instance: "synthetic-quiet-c", heap: 256 * 1024 * 1024, cpuRate: 0.04, allocRate: 3 * 1024 * 1024},
	}
	if !quiet {
		workers = append(workers, workerMetricFixture{host: "edge-d", block: "g2", instance: "synthetic-outlier", heap: 32 * 1024 * 1024 * 1024, cpuRate: 4.1, allocRate: 900 * 1024 * 1024})
	}
	metricPayload := workerMetricsFixtureJSON(t, now, workers...)
	ratePayload := workerChurnRatesFixtureJSON(t, now, workers...)
	if memory {
		ratePayload = workerRatesFixtureJSON(t, now, workers...)
	}
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "monitor_rate") {
				return ratePayload, nil
			}
			if strings.Contains(command, "monitor_memory_metric") {
				return metricPayload, nil
			}
			return "", nil
		},
		localFn: func(string, ...string) (string, error) { return "", nil },
	}
	return workerMemorySyntheticSettings(source, now)
}

// Each adjacent public constructor must establish live source entry before
// proving that a canceled or expired parent cannot invoke that same source.
func workerAdjacentEndedParentControl(t *testing.T, constructor func() Signal, memory bool, class string) {
	t.Helper()
	for _, deadline := range []bool{false, true} {
		now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		settings := workerAdjacentLifecycleSettings(t, now, memory, false)
		baseSource := settings.Source
		calls := 0
		settings.Source = &workerCapLifecycleSource{SignalSource: baseSource,
			hostContextFn: func(_ context.Context, host HostSettings, command string) (string, error) {
				calls++
				return baseSource.Host(context.Background(), host, command)
			},
		}
		alerts, err := constructor().Run(context.Background(), settings)
		if err != nil || calls == 0 || requireAlertClassCount(alerts, class) != 1 {
			t.Fatalf("adjacent live source/pressure precondition failed: calls=%d alerts=%d err=%v", calls, len(alerts), err)
		}
		calls = 0
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		if deadline {
			parent, cancel = context.WithDeadline(context.Background(), time.Time{})
			defer cancel()
		}
		alerts, err = constructor().Run(parent, settings)
		if calls != 0 {
			t.Fatalf("adjacent ended parent entered Host callback: deadline=%t calls=%d", deadline, calls)
		}
		if !errors.Is(err, parent.Err()) || len(alerts) != 0 {
			t.Fatalf("adjacent ended parent lost lifecycle/no-alert rule: alerts=%d err=%v", len(alerts), err)
		}
	}
}

// An actual first Host callback ends the parent. Success and independent
// failure both terminate before another source phase and publish no finding.
func workerAdjacentInFlightControl(t *testing.T, constructor func() Signal, memory bool) {
	t.Helper()
	for _, failSource := range []bool{false, true} {
		now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		settings := workerAdjacentLifecycleSettings(t, now, memory, false)
		baseSource := settings.Source
		parent, cancel := context.WithCancel(context.Background())
		calls := 0
		failure := errors.New("synthetic-private-adjacent-worker.example")
		settings.Source = &workerCapLifecycleSource{SignalSource: baseSource,
			hostContextFn: func(_ context.Context, host HostSettings, command string) (string, error) {
				calls++
				payload, err := baseSource.Host(context.Background(), host, command)
				cancel()
				if failSource {
					return payload, failure
				}
				return payload, err
			},
		}
		alerts, err := constructor().Run(parent, settings)
		cancel()
		if calls != 1 || !errors.Is(err, context.Canceled) || len(alerts) != 0 {
			t.Fatalf("adjacent callback cancellation leaked: calls=%d alerts=%d err=%v", calls, len(alerts), err)
		}
		if failSource && (!errors.Is(err, failure) || err.Error() != context.Canceled.Error()) {
			t.Fatal("adjacent independent source failure lost or exposed")
		}
	}
}

// Source-complete quiet and outlier fixtures keep their original verdicts
// while live. The lifecycle repair does not reinterpret capacity thresholds.
func workerAdjacentLiveControl(t *testing.T, constructor func() Signal, memory bool, class string) {
	t.Helper()
	for _, quiet := range []bool{false, true} {
		now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		settings := workerAdjacentLifecycleSettings(t, now, memory, quiet)
		alerts, err := constructor().Run(context.Background(), settings)
		want := 1
		if quiet {
			want = 0
		}
		if err != nil || requireAlertClassCount(alerts, class) != want || quiet && len(alerts) != 0 {
			t.Fatalf("adjacent live verdict changed: quiet=%t alerts=%d err=%v", quiet, len(alerts), err)
		}
	}
}

// The memory constructor refuses canceled and deadline-ended source entry.
func TestWorkerMemoryEndedParentNeverEntersHost(t *testing.T) {
	workerAdjacentEndedParentControl(t, NewWorkerMemorySignal, true, "worker-memory-skew")
}

// The churn constructor refuses canceled and deadline-ended source entry.
func TestWorkerChurnEndedParentNeverEntersHost(t *testing.T) {
	workerAdjacentEndedParentControl(t, NewWorkerChurnSignal, false, "worker-cpu-allocation-churn")
}

// Memory preserves callback failure while discarding canceled-owner findings.
func TestWorkerMemoryCancellationDuringHostRetainsCause(t *testing.T) {
	workerAdjacentInFlightControl(t, NewWorkerMemorySignal, true)
}

// Churn preserves callback failure while discarding canceled-owner findings.
func TestWorkerChurnCancellationDuringHostRetainsCause(t *testing.T) {
	workerAdjacentInFlightControl(t, NewWorkerChurnSignal, false)
}

// Live heap pressure and quiet controls retain their original thresholds.
func TestWorkerMemoryLifecycleLiveVerdictsRemainIndependent(t *testing.T) {
	workerAdjacentLiveControl(t, NewWorkerMemorySignal, true, "worker-memory-skew")
}

// Live churn pressure and quiet controls retain their original thresholds.
func TestWorkerChurnLifecycleLiveVerdictsRemainIndependent(t *testing.T) {
	workerAdjacentLiveControl(t, NewWorkerChurnSignal, false, "worker-cpu-allocation-churn")
}

// Optional rate failure must preserve a live heap finding, but an ended owner
// publishes nothing and retains that same independent error in its chain.
func TestWorkerMemoryOptionalRateFailureKeepsLivePartialOnly(t *testing.T) {
	for _, ended := range []bool{false, true} {
		now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		settings := workerAdjacentLifecycleSettings(t, now, true, false)
		baseSource := settings.Source
		parent, cancel := context.WithCancel(context.Background())
		failure := errors.New("synthetic-private-optional-rates.example")
		rateCalls := 0
		settings.Source = &workerCapLifecycleSource{SignalSource: baseSource,
			hostContextFn: func(ctx context.Context, host HostSettings, command string) (string, error) {
				if strings.Contains(command, "monitor_rate") {
					rateCalls++
					if ended {
						cancel()
					}
					return "", failure
				}
				return baseSource.Host(ctx, host, command)
			},
		}
		alerts, err := NewWorkerMemorySignal().Run(parent, settings)
		if rateCalls != 1 {
			t.Fatal("optional rate boundary was not entered exactly once")
		}
		if ended {
			if len(alerts) != 0 || !errors.Is(err, context.Canceled) || !errors.Is(err, failure) || err.Error() != context.Canceled.Error() {
				t.Fatal("ended optional rate source lost lifecycle or independent cause")
			}
		} else if err != nil || requireAlertClassCount(alerts, "worker-memory-skew") != 1 {
			t.Fatal("live optional rate failure erased independent heap pressure")
		}
		cancel()
		for _, alert := range alerts {
			requireAlertOmits(t, alert, failure.Error())
		}
	}
}
