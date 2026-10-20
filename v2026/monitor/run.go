package monitor

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// runLoopMaxConcurrentSignals limits the immediate startup wave as well as
// later cadence collisions. Most production signals open SSH sessions, so an
// unbounded fan-out can make the monitor manufacture visibility failures.
const runLoopMaxConcurrentSignals = 4

type runLoopTicker interface {
	C() <-chan time.Time
	Stop()
}

type wallClockRunLoopTicker struct {
	ticker *time.Ticker
}

func (t *wallClockRunLoopTicker) C() <-chan time.Time { return t.ticker.C }
func (t *wallClockRunLoopTicker) Stop()               { t.ticker.Stop() }

type runLoopTickerFactory func(time.Duration) runLoopTicker

// waitRunLoopCadence starts the next cadence only after the preceding probe
// and handler have completed. A wall-clock ticker created before a slow run
// can otherwise buffer a missed tick, or fire just after completion, and drive
// a back-to-back query against the same already-slow dependency.
func waitRunLoopCadence(ctx context.Context, cadence time.Duration, newTicker runLoopTickerFactory) bool {
	ticker := newTicker(cadence)
	defer ticker.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-ticker.C():
		return true
	}
}

// cadenceAlertGate applies the consecutive-tick contract carried by
// Alert.Sustain. One-shot Monitor.Run intentionally bypasses this gate so a
// diagnostic invocation still returns every current band violation.
type cadenceAlertGate struct {
	streaks map[string]map[string]int
}

func newCadenceAlertGate() *cadenceAlertGate {
	return &cadenceAlertGate{streaks: map[string]map[string]int{}}
}

func (g *cadenceAlertGate) filter(signal Signal, alerts Alerts) Alerts {
	ready, _ := g.split(signal, alerts)
	return ready
}

// Returns the alerts that reached their Sustain, and the alerts this execution
// observed that are still held below it.
func (g *cadenceAlertGate) split(signal Signal, alerts Alerts) (Alerts, Alerts) {
	signalKey := signal.Key()
	streaks := g.streaks[signalKey]
	if streaks == nil {
		streaks = map[string]int{}
		g.streaks[signalKey] = streaks
	}

	seen := make(map[string]struct{}, len(alerts))
	ready := make(Alerts, 0, len(alerts))
	held := Alerts{}
	for _, alert := range alerts {
		identity := alert.Identity()
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		streaks[identity]++
		sustain := alert.Sustain
		if sustain <= 0 {
			sustain = 1
		}
		if streaks[identity] >= sustain {
			readyAlert := alert
			if 0 < alert.PageSustain && alert.PageSustain <= streaks[identity] {
				readyAlert.Severity = SeverityPage
			}
			ready = append(ready, readyAlert)
		} else {
			held = append(held, alert)
		}
	}
	for identity := range streaks {
		if _, ok := seen[identity]; !ok {
			delete(streaks, identity)
		}
	}
	return ready, held
}

// AlertHandler consumes the active alerts from one signal execution.
type AlertHandler func(ctx context.Context, signal Signal, alerts Alerts) error

// One signal execution as the cadence gate saw it. A consumer that tracks
// incidents across executions needs more than the gated alerts: an empty
// gated batch can come from a failed execution, or from an alert the gate
// holds again after a one-run dip, and neither means the incident is gone.
type SignalExecution struct {
	// the alerts that reached their Sustain, as AlertHandler receives them
	Alerts Alerts
	// alerts this execution observed that the gate still holds below their
	// Sustain. They are present, not absent.
	HeldAlerts Alerts
	// the signal returned an error. Its visibility alert is in Alerts or
	// HeldAlerts, and whatever else the signal observes is unknown.
	Failed bool
}

// Consumes every gated signal execution, empty ones included.
type SignalExecutionHandler func(ctx context.Context, signal Signal, execution SignalExecution) error

// Per-watcher scheduling policy; zero preserves the registered cadences and
// immediate active startup. Standing log streams and their drains are exempt.
type RunLoopOptions struct {
	MinimumProbeCadence time.Duration
}

// RunSignal executes a registered signal by its short key or SIGNALS.md
// number. Keys are preferred in callers because they remain descriptive.
func (m *Monitor) RunSignal(ctx context.Context, identifier string) (Alerts, error) {
	for _, signal := range m.signals {
		if signal.Key() == identifier || signal.Number() == identifier {
			return signal.Run(ctx, m.settings)
		}
	}
	return nil, fmt.Errorf("monitor: signal %s is not registered", identifier)
}

// prepareRunLoop replaces the bounded §1.5 log-window probe with standing
// warpctl streams when the runtime transport supports them. Monitor.Run and
// RunSignal deliberately retain the bounded probe for one-shot diagnostics;
// only the long-running scheduler owns stream lifecycle.
func (m *Monitor) prepareRunLoop(ctx context.Context) ([]Signal, []*logTailer, error) {
	signals := append([]Signal(nil), m.signals...)
	if m.settings.Source != nil {
		if _, ok := m.settings.Source.(StreamingSignalSource); !ok {
			return signals, nil, nil
		}
	}

	logSignalIndex := -1
	var logSignal *signalAdapter
	for i, signal := range signals {
		adapter, ok := signal.(*signalAdapter)
		if !ok {
			continue
		}
		if _, ok := adapter.probe.(logWindowProbe); !ok {
			continue
		}
		logSignalIndex = i
		logSignal = adapter
		break
	}
	if logSignalIndex < 0 {
		return signals, nil, nil
	}

	env, err := newProbeEnv(m.settings)
	if err != nil {
		return nil, nil, err
	}
	services := warpServices(ctx, env)
	tailers := make([]*logTailer, 0, len(services))
	for _, service := range services {
		tailers = append(tailers, newLogTailer(service, env))
	}
	signals[logSignalIndex] = &signalAdapter{
		number: logSignal.number,
		key:    logSignal.key,
		name:   logSignal.name,
		probe: &logTailProbe{
			tailers:              tailers,
			monitorRouteEvidence: collectTailTransportMonitorRouteEvidence,
			reconcileDiagnostics: os.Stderr,
		},
		accept: logSignal.accept,
	}
	return signals, tailers, nil
}

// isStandingLogSignal identifies the in-memory drain over the standing
// warpctl streams. Unlike every remote probe, it must wait for one complete
// cadence before its first drain and must not queue behind the bounded SSH/DB
// execution slots. Otherwise startup can label a partial window as "/min",
// while a cadence collision can stretch a window and inflate its rate.
func isStandingLogSignal(signal Signal) bool {
	adapter, ok := signal.(*signalAdapter)
	if !ok {
		return false
	}
	_, ok = adapter.probe.(*logTailProbe)
	return ok
}

// RunLoop schedules every registered signal at its own cadence. All execution
// and scheduling logic lives here so cli/monitor only handles process wiring.
func (m *Monitor) RunLoop(ctx context.Context, handle AlertHandler) error {
	return m.RunLoopWithOptions(ctx, RunLoopOptions{}, handle)
}

// A positive floor delays the first active probe by that floor, then waits at
// least max(floor, native cadence) after each completed probe and handler.
// Consecutive-observation Sustain state stays local to this running watcher.
func (self *Monitor) RunLoopWithOptions(ctx context.Context, options RunLoopOptions, handle AlertHandler) error {
	return self.runLoopWithOptions(ctx, options, handle, newWallClockRunLoopTicker)
}

// The RunLoopWithOptions schedule, with a handler that also learns which
// alerts the gate held and whether the execution failed.
func (self *Monitor) RunExecutionLoop(ctx context.Context, options RunLoopOptions, handle SignalExecutionHandler) error {
	return self.runExecutionLoop(ctx, options, handle, newWallClockRunLoopTicker)
}

// The production ticker factory.
func newWallClockRunLoopTicker(cadence time.Duration) runLoopTicker {
	return &wallClockRunLoopTicker{ticker: time.NewTicker(cadence)}
}

// runLoop carries an injected ticker factory so cadence/slot interactions can
// be tested with explicit ticks instead of wall-clock sleeps.
func (m *Monitor) runLoop(ctx context.Context, handle AlertHandler, newTicker runLoopTickerFactory) error {
	return m.runLoopWithOptions(ctx, RunLoopOptions{}, handle, newTicker)
}

// Hands each execution's gated alerts to an AlertHandler.
func (self *Monitor) runLoopWithOptions(ctx context.Context, options RunLoopOptions, handle AlertHandler, newTicker runLoopTickerFactory) error {
	var handleExecution SignalExecutionHandler
	if handle != nil {
		handleExecution = func(ctx context.Context, signal Signal, execution SignalExecution) error {
			return handle(ctx, signal, execution.Alerts)
		}
	}
	return self.runExecutionLoop(ctx, options, handleExecution, newTicker)
}

// Testable scheduling boundary; reject invalid options before preparing any
// streams or remote observations. Options are never stored on the Monitor.
func (self *Monitor) runExecutionLoop(ctx context.Context, options RunLoopOptions, handle SignalExecutionHandler, newTicker runLoopTickerFactory) error {
	if options.MinimumProbeCadence < 0 {
		return fmt.Errorf("monitor: minimum probe cadence must not be negative")
	}
	if handle == nil {
		return fmt.Errorf("monitor: alert handler is required")
	}
	if newTicker == nil {
		return fmt.Errorf("monitor: ticker factory is required")
	}
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	signals, tailers, err := self.prepareRunLoop(ctx)
	if err != nil {
		return err
	}

	var handleLock sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	runSlots := newRunSlotPool(runLoopMaxConcurrentSignals)
	// All active signals wake at the same startup floor. The enabled recurring
	// sampler gets the first finite turn so that this bulk wave cannot consume
	// its entire queue-inclusive budget before it has ever observed a query.
	// Standing log streams/drains remain independent. Ordinary probes start
	// after this at-most40s turn; subsequent collisions use the fair slot queue.
	var startupSampleDone chan struct{}
	if self.settings.PGQuerySampleContinuous {
		for _, signal := range signals {
			if signal.ID() == "pg/query-sample" {
				startupSampleDone = make(chan struct{})
				break
			}
		}
	}
	alertGate := newCadenceAlertGate()
	for _, registered := range tailers {
		tailer := registered
		wg.Add(1)
		go func() {
			defer wg.Done()
			tailer.run(ctx)
		}()
	}
	for _, registered := range signals {
		signal := registered
		wg.Add(1)
		go func() {
			defer wg.Done()
			standingLogSignal := isStandingLogSignal(signal)
			startupSample := startupSampleDone != nil && signal.ID() == "pg/query-sample"
			initialDelay := options.MinimumProbeCadence
			if standingLogSignal {
				// Tailers begin just before these scheduler goroutines. Preserve
				// their first complete cadence instead of immediately draining
				// a startup fragment and calling it a per-minute rate.
				initialDelay = signal.Cadence()
			}
			if initialDelay > 0 && !waitRunLoopCadence(ctx, initialDelay, newTicker) {
				return
			}
			if startupSampleDone != nil && !startupSample && !standingLogSignal {
				select {
				case <-startupSampleDone:
				case <-ctx.Done():
					return
				}
			}
			for {
				var alerts Alerts
				var err error
				if standingLogSignal {
					// This check only drains mutex-protected in-memory counters.
					// Giving it a remote-command slot lets slow unrelated probes
					// distort the very rate window it is meant to measure.
					alerts, err = signal.Run(ctx, self.settings)
				} else {
					alerts, err = runSignalInSlot(ctx, runSlots, signal, self.settings)
				}
				if startupSample {
					close(startupSampleDone)
					startupSample = false
				}
				// A probe interrupted by monitor shutdown has not lost visibility;
				// it was deliberately stopped. Do not turn that lifecycle event
				// into a production alert.
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					alerts = append(alerts, visibilityAlert(self.settings, signal, err))
				}
				handleLock.Lock()
				ready, held := alertGate.split(signal, alerts)
				handleErr := handle(ctx, signal, SignalExecution{
					Alerts:     ready,
					HeldAlerts: held,
					Failed:     err != nil,
				})
				handleLock.Unlock()
				if handleErr != nil {
					select {
					case errCh <- handleErr:
					default:
					}
					cancel()
					return
				}
				cadence := signal.Cadence()
				if !standingLogSignal {
					cadence = max(cadence, options.MinimumProbeCadence)
				}
				if !waitRunLoopCadence(ctx, cadence, newTicker) {
					return
				}
			}
		}()
	}

	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		if parentCtx.Err() != nil {
			return nil
		}
		return context.Cause(ctx)
	}
}

// A bounded diagnostic's owner starts before the top-level queue. Its parent
// deadline also reaches the shared host limiter and the transport. Cancellation
// never releases a slot the caller did not acquire.
func runSignalInSlot(ctx context.Context, slots *runSlotPool, signal Signal, settings SignalSettings) (Alerts, error) {
	priority := false
	if bounded, ok := signal.(interface{ runBudget() time.Duration }); ok && bounded.runBudget() > 0 {
		priority = true
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bounded.runBudget())
		defer cancel()
	}
	if signal.ID() == "pg/query-sample" {
		ctx = observePgSampleRunSlot(ctx)
	}
	ctx = context.WithValue(ctx, runSlotSignalContextKey{}, signal.Key())
	release, err := slots.acquire(ctx, priority)
	if err != nil {
		return nil, &runSlotAdmissionError{cause: err}
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, &runSlotAdmissionError{cause: err}
	}
	return signal.Run(ctx, settings)
}
