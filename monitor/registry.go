package monitor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
		NewRebootCollisionSignal(),
		NewConnectionRateSignal(),
		NewSelectionFreshnessSignal(),
		NewSelectionPopulationSignal(),
		NewRetentionFanoutSignal(),
		NewPgBouncerStallsSignal(),
		NewWorkerMemorySignal(),
		NewWorkerChurnSignal(),
		NewRedisMemorySignal(),
		NewRedisBuffersSignal(),
		NewKeyFamiliesSignal(),
		NewTTLLeaksSignal(),
		NewRedisBytesSignal(),
		NewRedisProcessSignal(),
		NewRedisConnectionsSignal(),
		NewRedisTopologySignal(),
		NewReliabilityPipelineSignal(),
		NewSourceAttributionSignal(),
		NewMigrationsSignal(),
		NewReliabilityIndexSignal(),
		NewRolloutGuardSignal(),
		NewProvenanceSignal(),
		NewRedisKeyEventsSignal(),
		NewStuckLeasesSignal(),
		NewTaskConvergenceSignal(),
		NewProxyPathSignal(),
		NewProxyMemorySignal(),
		NewProxyPoolSignal(),
		NewProxyRuntimeSignal(),
		NewProxyCacheSignal(),
		NewKeyPublicationSignal(),
		NewEdgeIPv6Signal(),
		NewGrafanaIngressSignal(),
		NewGrafanaNodeSignal(),
		NewMimirIndexSignal(),
		NewLokiTailersSignal(),
		NewAssociationFilesSignal(),
		NewEmailAssetsSignal(),
	}
}

// ExcludeSignals returns the registered signals except those named by short
// key, SIGNALS.md number, or probe ID. Unknown names fail closed so a typo
// cannot silently re-enable a signal an operator intended to pause.
func ExcludeSignals(signals []Signal, identifiers ...string) ([]Signal, error) {
	requested := map[string]bool{}
	for _, identifier := range identifiers {
		requested[identifier] = false
	}
	selected := make([]Signal, 0, len(signals))
	for _, signal := range signals {
		excluded := false
		for identifier := range requested {
			if identifier == signal.Key() || identifier == signal.Number() || identifier == signal.ID() {
				requested[identifier] = true
				excluded = true
			}
		}
		if !excluded {
			selected = append(selected, signal)
		}
	}
	for identifier, found := range requested {
		if !found {
			return nil, fmt.Errorf("monitor: excluded signal %q is not registered", identifier)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("monitor: every registered signal was excluded")
	}
	return selected, nil
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
	if target, ok := sshAdmissionResetTarget(err); ok {
		return Alert{
			SignalNumber: signal.Number(),
			SignalKey:    signal.Key(),
			SignalID:     "monitor/visibility",
			SignalName:   signal.Name(),
			Severity:     SeverityWarn,
			Class:        "ssh-admission-reset",
			Target:       target,
			Environment:  settings.Environment,
			ObservedAt:   settings.Now(),
			Sustain:      2,
			Symptom:      fmt.Sprintf("SSH admission to %s was reset while running signal %s (%s)", target, signal.Number(), signal.ID()),
			Mechanism:    "The SSH connection closed during key exchange, before the remote observation command could run. On this fleet the same signature occurred when slow public pre-auth clients occupied OpenSSH's global MaxStartups pool and concurrent monitor probes supplied the trip connection. An sshd reload/restart or a network reset can look similar; the host journal is the discriminator.",
			Baseline:     "Every monitor SSH command authenticates without an sshd MaxStartups throttle, key-exchange reset, or connection close.",
			Observed:     err.Error(),
			Context:      fmt.Sprintf("failed_signal=%s failed_probe=%s; this is observation-path failure, not evidence that the probed database or service rejected the command", signal.Key(), signal.ID()),
			Action:       "On the target, read the ssh/sshd journal across this timestamp and inspect the listener's current startup count plus [accepted]/[net] children. If it reports beginning MaxStartups/past MaxStartups, keep the monitor's shared per-host command cap and deploy the shared xops SSH pre-auth hardening; restrict public SSH where operational access permits. Do not blame PostgreSQL or merely raise MaxStartups. If the journal instead shows an sshd lifecycle or host network event, repair that event.",
			Verify:       "The target journal records no new MaxStartups throttle or key-exchange drop through monitor startup and at least two recurring cadences, and the failed signal returns a concrete observation.",
			Playbook:     "SIGNALS.md monitor SSH-admission note and MONITOR.md §4",
		}
	}
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

// sshAdmissionResetTarget recognizes the client-side OpenSSH text emitted
// when a connection dies before authentication. It intentionally does not
// assert MaxStartups from this text alone; visibilityAlert directs the
// operator to the authoritative sshd journal for that discriminator.
func sshAdmissionResetTarget(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	message := err.Error()
	keyExchangeReset := strings.Contains(message, "kex_exchange_identification")
	port22Close := strings.Contains(message, "port 22") &&
		(strings.Contains(message, "Connection reset by") || strings.Contains(message, "Connection closed by"))
	if !keyExchangeReset && !port22Close {
		return "", false
	}
	target, _, found := strings.Cut(message, ":")
	target = strings.TrimSpace(target)
	if !found || target == "" || strings.ContainsAny(target, " \t\r\n") {
		target = "ssh-target"
	}
	return target, true
}
