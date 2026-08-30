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

// RunLoop schedules every registered signal at its own cadence. All execution
// and scheduling logic lives here so cli/monitor only handles process wiring.
func (m *Monitor) RunLoop(ctx context.Context, handle AlertHandler) error {
	if handle == nil {
		return fmt.Errorf("monitor: alert handler is required")
	}
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var handleLock sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	runSlots := make(chan struct{}, runLoopMaxConcurrentSignals)
	alertGate := newCadenceAlertGate()
	for _, registered := range m.signals {
		signal := registered
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(signal.Cadence())
			defer ticker.Stop()
			for {
				select {
				case runSlots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				alerts, err := func() (Alerts, error) {
					defer func() { <-runSlots }()
					return signal.Run(ctx, m.settings)
				}()
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
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
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
