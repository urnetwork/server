// Manual ticks prove per-watcher cadence floors without elapsed-time sleeps.
package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Reuses the blocking, sustained observation while varying native cadence.
type cadenceFloorSignal struct {
	steppedAlertSignal
	cadence time.Duration
}

// Native cadence remains a property of the signal, not the watcher option.
func (self *cadenceFloorSignal) Cadence() time.Duration { return self.cadence }

// Each scheduler wait gets a fresh manually controlled clock.
type cadenceFloorTick struct {
	cadence time.Duration
	ticks   chan time.Time
}

// Deadlines only fail a stuck test; the expected scheduler transitions use ticks.
func receiveCadenceFloorValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("cadence scheduler did not reach its next controlled transition")
		var zero T
		return zero
	}
}

// A positive floor delays startup, keeps slow native cadences, and preserves
// consecutive-observation Sustain across the post-completion waits.
func TestRunLoopCadenceFloorDelaysFirstProbeAndPreservesSustain(t *testing.T) {
	const floor = 15 * time.Minute
	for _, nativeCadence := range []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute} {
		started := make(chan struct{}, 2)
		release := make(chan struct{}, 2)
		handled := make(chan Alerts, 2)
		createdTicks := make(chan cadenceFloorTick, 3)
		runErr := make(chan error, 1)
		probe := &cadenceFloorSignal{steppedAlertSignal: steppedAlertSignal{started: started, release: release}, cadence: nativeCadence}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			runErr <- NewWithSignals(SignalSettings{}, probe).runLoopWithOptions(ctx,
				RunLoopOptions{MinimumProbeCadence: floor},
				func(_ context.Context, _ Signal, alerts Alerts) error { handled <- alerts; return nil },
				func(cadence time.Duration) runLoopTicker {
					tick := cadenceFloorTick{cadence: cadence, ticks: make(chan time.Time, 1)}
					createdTicks <- tick
					return &manualRunLoopTicker{c: tick.ticks}
				})
		}()
		first := receiveCadenceFloorValue(t, createdTicks)
		if first.cadence != floor {
			t.Fatalf("native %s initial wait = %s, want %s", nativeCadence, first.cadence, floor)
		}
		select {
		case <-started:
			t.Fatal("active probe ran before its first floor tick")
		default:
		}
		first.ticks <- time.Time{}
		receiveCadenceFloorValue(t, started)
		select {
		case <-createdTicks:
			t.Fatal("post-probe cadence started during a blocked observation")
		default:
		}
		release <- struct{}{}
		if alerts := receiveCadenceFloorValue(t, handled); len(alerts) != 0 {
			t.Fatal("first observation bypassed Sustain")
		}
		next := receiveCadenceFloorValue(t, createdTicks)
		if next.cadence != max(floor, nativeCadence) {
			t.Fatalf("native %s subsequent wait = %s", nativeCadence, next.cadence)
		}
		select {
		case <-started:
			t.Fatal("probe reran without a fresh post-completion tick")
		default:
		}
		next.ticks <- time.Time{}
		receiveCadenceFloorValue(t, started)
		release <- struct{}{}
		if alerts := receiveCadenceFloorValue(t, handled); len(alerts) != 1 || alerts[0].Sustain != 2 {
			t.Fatal("second actual observation lost its sustained alert")
		}
		cancel()
		if err := receiveCadenceFloorValue(t, runErr); err != nil {
			t.Fatal(err)
		}
	}
}

// A separate invocation on the same Monitor retains zero/default startup;
// options are not a process-global or Monitor-global mutable policy.
func TestRunLoopCadenceFloorIsPerInvocation(t *testing.T) {
	started := make(chan struct{}, 1)
	probe := &cadenceFloorSignal{steppedAlertSignal: steppedAlertSignal{started: started, release: make(chan struct{})}, cadence: time.Minute}
	watcher := NewWithSignals(SignalSettings{}, probe)
	ctx, cancel := context.WithCancel(context.Background())
	floorArmed := make(chan struct{}, 1)
	floorErr := make(chan error, 1)
	go func() {
		floorErr <- watcher.runLoopWithOptions(ctx, RunLoopOptions{MinimumProbeCadence: 15 * time.Minute},
			func(context.Context, Signal, Alerts) error { return nil },
			func(time.Duration) runLoopTicker {
				floorArmed <- struct{}{}
				return &manualRunLoopTicker{c: make(chan time.Time)}
			})
	}()
	defer cancel()
	receiveCadenceFloorValue(t, floorArmed)
	defaultErr := make(chan error, 1)
	go func() {
		defaultErr <- watcher.RunLoop(ctx, func(context.Context, Signal, Alerts) error { return nil })
	}()
	receiveCadenceFloorValue(t, started)
	cancel()
	if err := receiveCadenceFloorValue(t, floorErr); err != nil {
		t.Fatal(err)
	}
	if err := receiveCadenceFloorValue(t, defaultErr); err != nil {
		t.Fatal(err)
	}
}

// Standing counters still drain once per native window while active probes
// remain parked behind their first floor, then retain the same drain cadence.
func TestRunLoopCadenceFloorPreservesStandingLogDrains(t *testing.T) {
	const floor = 15 * time.Minute
	const logCadence = time.Minute
	started := make(chan struct{}, 1)
	probe := &cadenceFloorSignal{steppedAlertSignal: steppedAlertSignal{started: started, release: make(chan struct{})}, cadence: time.Minute}
	tailer := newLogTailer("grafana", nil)
	tailer.classify(`error="the result-set has errors: [plugin.notRegistered] plugin not registered"`)
	logs := &signalAdapter{number: "1.5", key: "log-errors", name: "Log error-class rates",
		probe: &logTailProbe{tailers: []*logTailer{tailer}, cadenceOverride: logCadence}}
	createdTicks := make(chan cadenceFloorTick, 4)
	handled := make(chan Alerts, 2)
	runErr := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		runErr <- NewWithSignals(syntheticSettings(&syntheticSource{}), probe, logs).runLoopWithOptions(ctx,
			RunLoopOptions{MinimumProbeCadence: floor},
			func(_ context.Context, signal Signal, alerts Alerts) error {
				if signal == logs {
					handled <- alerts
				}
				return nil
			}, func(cadence time.Duration) runLoopTicker {
				tick := cadenceFloorTick{cadence: cadence, ticks: make(chan time.Time, 1)}
				createdTicks <- tick
				return &manualRunLoopTicker{c: tick.ticks}
			})
	}()
	first, second := receiveCadenceFloorValue(t, createdTicks), receiveCadenceFloorValue(t, createdTicks)
	if first.cadence == floor {
		first, second = second, first
	}
	if first.cadence != logCadence || second.cadence != floor {
		t.Fatalf("startup waits = %s/%s", first.cadence, second.cadence)
	}
	first.ticks <- time.Time{}
	requireAlertClass(t, receiveCadenceFloorValue(t, handled), "grafana-plugin-unregistered")
	if next := receiveCadenceFloorValue(t, createdTicks); next.cadence != logCadence {
		t.Fatalf("standing drain was stretched to %s", next.cadence)
	}
	select {
	case <-started:
		t.Fatal("standing drain released an active probe's floor")
	default:
	}
	cancel()
	if err := receiveCadenceFloorValue(t, runErr); err != nil {
		t.Fatal(err)
	}
}

// Invalid timing policy fails before preparing streams or any observations.
func TestRunLoopCadenceFloorRejectsNegativeBeforeContact(t *testing.T) {
	var calls int
	source := &syntheticSource{localFn: func(string, ...string) (string, error) { calls++; return "", nil }}
	err := NewWithSignals(syntheticSettings(source), NewLogErrorsSignal()).RunLoopWithOptions(context.Background(),
		RunLoopOptions{MinimumProbeCadence: -time.Second}, func(context.Context, Signal, Alerts) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "must not be negative") || calls != 0 {
		t.Fatalf("negative floor error=%v source calls=%d", err, calls)
	}
}
