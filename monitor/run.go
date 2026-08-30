package monitor

import (
	"context"
	"fmt"
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

// discardElapsedRunLoopTicks drops cadence events that accumulated while a
// probe was queued or running. A slow observational probe must not enter a
// back-to-back catch-up loop: that amplifies load on the same dependency that
// is already slow. The next execution begins on a tick that arrives after the
// completed observation.
func discardElapsedRunLoopTicks(ticker runLoopTicker) {
	ticks := ticker.C()
	for {
		select {
		case _, ok := <-ticks:
			if !ok {
				return
			}
		default:
			return
		}
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
	signalKey := signal.Key()
	streaks := g.streaks[signalKey]
	if streaks == nil {
		streaks = map[string]int{}
		g.streaks[signalKey] = streaks
	}

	seen := make(map[string]struct{}, len(alerts))
	ready := make(Alerts, 0, len(alerts))
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
			ready = append(ready, alert)
		}
	}
	for identity := range streaks {
		if _, ok := seen[identity]; !ok {
			delete(streaks, identity)
		}
	}
	return ready
}

// AlertHandler consumes the active alerts from one signal execution.
type AlertHandler func(ctx context.Context, signal Signal, alerts Alerts) error

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
		probe:  &logTailProbe{tailers: tailers},
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
	return m.runLoop(ctx, handle, func(cadence time.Duration) runLoopTicker {
		return &wallClockRunLoopTicker{ticker: time.NewTicker(cadence)}
	})
}

// runLoop carries an injected ticker factory so cadence/slot interactions can
// be tested with explicit ticks instead of wall-clock sleeps.
func (m *Monitor) runLoop(ctx context.Context, handle AlertHandler, newTicker runLoopTickerFactory) error {
	if handle == nil {
		return fmt.Errorf("monitor: alert handler is required")
	}
	if newTicker == nil {
		return fmt.Errorf("monitor: ticker factory is required")
	}
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	signals, tailers, err := m.prepareRunLoop(ctx)
	if err != nil {
		return err
	}

	var handleLock sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	runSlots := make(chan struct{}, runLoopMaxConcurrentSignals)
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
			ticker := newTicker(signal.Cadence())
			defer ticker.Stop()
			standingLogSignal := isStandingLogSignal(signal)
			if standingLogSignal {
				// Tailers begin just before these scheduler goroutines. Preserve
				// their first complete cadence instead of immediately draining
				// a startup fragment and calling it a per-minute rate.
				select {
				case <-ctx.Done():
					return
				case <-ticker.C():
				}
			}
			for {
				var alerts Alerts
				var err error
				if standingLogSignal {
					// This check only drains mutex-protected in-memory counters.
					// Giving it a remote-command slot lets slow unrelated probes
					// distort the very rate window it is meant to measure.
					alerts, err = signal.Run(ctx, m.settings)
				} else {
					select {
					case runSlots <- struct{}{}:
					case <-ctx.Done():
						return
					}
					alerts, err = func() (Alerts, error) {
						defer func() { <-runSlots }()
						return signal.Run(ctx, m.settings)
					}()
				}
				// A probe interrupted by monitor shutdown has not lost visibility;
				// it was deliberately stopped. Do not turn that lifecycle event
				// into a production alert.
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					alerts = append(alerts, visibilityAlert(m.settings, signal, err))
				}
				handleLock.Lock()
				alerts = alertGate.filter(signal, alerts)
				handleErr := handle(ctx, signal, alerts)
				handleLock.Unlock()
				if handleErr != nil {
					select {
					case errCh <- handleErr:
					default:
					}
					cancel()
					return
				}
				discardElapsedRunLoopTicks(ticker)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C():
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
