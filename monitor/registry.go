package monitor

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Monitor owns a configured, ordered set of registered signals.
type Monitor struct {
	settings SignalSettings
	signals  []Signal
}

// New constructs a monitor with every named probe registered in one explicit
// slice. Adding SIGNALS.md §X.Y (`short-key`) therefore means adding
// signal_short_key.go, signal_short_key_test.go, and its constructor here.
func New(settings SignalSettings) *Monitor {
	return &Monitor{
		settings: settings.withDefaults().withRuntime(),
		signals:  NewSignals(),
	}
}

// NewMonitor is the descriptive alias for New.
func NewMonitor(settings SignalSettings) *Monitor { return New(settings) }

// NewWithSignals constructs a monitor with an explicit signal set. It is
// useful for focused embedding and synthetic integration tests; production
// normally uses New and the complete catalog registry.
func NewWithSignals(settings SignalSettings, signals ...Signal) *Monitor {
	return &Monitor{
		settings: settings.withDefaults().withRuntime(),
		signals:  append([]Signal(nil), signals...),
	}
}

// NewSignals returns fresh stateful signal instances in catalog order.
func NewSignals() []Signal {
	return []Signal{
		NewContractRateSignal(),
		NewTaskCanariesSignal(),
		NewNetEscrowSignal(),
		NewPostgresStateSignal(),
		NewRedisClusterSignal(),
		NewLogErrorsSignal(),
		NewActiveQueriesSignal(),
		NewWaitEventsSignal(),
		NewPlannerFlipsSignal(),
		NewVacuumHealthSignal(),
		NewTaskHealthSignal(),
		NewOpenContractsSignal(),
		NewCloseDurationSignal(),
		NewConnectionRateSignal(),
		NewSelectionFreshnessSignal(),
		NewSelectionPopulationSignal(),
		NewRetentionFanoutSignal(),
		NewPgBouncerStallsSignal(),
		NewWorkerMemorySignal(),
		NewRedisMemorySignal(),
		NewRedisBuffersSignal(),
		NewKeyFamiliesSignal(),
		NewTTLLeaksSignal(),
		NewRedisProcessSignal(),
		NewRedisConnectionsSignal(),
		NewRedisTopologySignal(),
		NewReliabilityPipelineSignal(),
		NewSourceAttributionSignal(),
		NewMigrationsSignal(),
		NewRedisKeyEventsSignal(),
		NewStuckLeasesSignal(),
		NewTaskConvergenceSignal(),
		NewProxyPathSignal(),
		NewKeyPublicationSignal(),
		NewEdgeIPv6Signal(),
		NewGrafanaIngressSignal(),
	}
}

// Signals returns a copy of the registered slice.
func (m *Monitor) Signals() []Signal {
	return append([]Signal(nil), m.signals...)
}

// Run executes all registered signals and returns every active alert. A probe
// execution failure also becomes a structured visibility alert while the
// joined error lets callers choose a non-zero exit status.
func (m *Monitor) Run(ctx context.Context) (Alerts, error) {
	alerts := Alerts{}
	errs := []error{}
	for _, signal := range m.signals {
		signalAlerts, err := signal.Run(ctx, m.settings)
		if err != nil {
			errs = append(errs, fmt.Errorf("signal %s: %w", signal.Number(), err))
			alerts = append(alerts, visibilityAlert(m.settings, signal, err))
			continue
		}
		alerts = append(alerts, signalAlerts...)
	}
	sort.SliceStable(alerts, func(i, j int) bool {
		return alerts[i].Identity() < alerts[j].Identity()
	})
	return alerts, errors.Join(errs...)
}

func visibilityAlert(settings SignalSettings, signal Signal, err error) Alert {
	return Alert{
		SignalNumber: signal.Number(),
		SignalKey:    signal.Key(),
		SignalID:     "monitor/visibility",
		SignalName:   signal.Name(),
		Severity:     SeverityWarn,
		Class:        "cannot-observe",
		Target:       signal.ID(),
		Environment:  settings.Environment,
		ObservedAt:   settings.Now(),
		Sustain:      2,
		Symptom:      fmt.Sprintf("Signal %s (%s) could not run: %s", signal.Number(), signal.ID(), err),
		Mechanism:    "The monitor could not reach or parse a source of truth, so the associated production condition is currently unknown.",
		Baseline:     "Every registered signal completes within its command timeout.",
		Observed:     err.Error(),
		Action:       "Restore access to the signal source, then rerun the failed signal; also check whether the unreachable target is itself the incident.",
		Verify:       "The signal completes and reports either no alert or a concrete target alert.",
		Playbook:     "SIGNALS.md §1.4 and MONITOR.md §3.6",
	}
}
