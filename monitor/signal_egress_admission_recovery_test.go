// Actual public egress observations feed the existing gate and in-memory
// notifier; quiet work is observable but cannot certify incident recovery.
package monitor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// One bounded synthetic native-pair response, with per-execution hold capture.
// The returned body is this point's native data, not observer counter history.
func egressRecoveryExecution(t *testing.T, signal Signal, ctx context.Context, now time.Time, processes []egressAdmissionTestProcess, previousBody string, sourceErr error, retire context.CancelFunc) (SignalExecution, string, error) {
	t.Helper()
	body := egressAdmissionTestJson(t, now, processes)
	paired := egressAdmissionTestPairJson(t, now, body)
	if previousBody != "" {
		paired = egressAdmissionTestPairJson(t, now, body, previousBody)
	}
	calls := 0
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		calls++
		if retire != nil {
			retire()
		}
		if sourceErr != nil {
			return "private-recovery-output", sourceErr
		}
		return paired, nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-metrics.example", Roles: []string{"services"}}}
	settings.Now = func() time.Time { return now }
	if err := settings.Validate(); err != nil {
		t.Fatal("synthetic recovery settings invalid")
	}
	observation := &signalClearHoldObservation{}
	ctx = context.WithValue(ctx, signalClearHoldContextKey{}, observation)
	alerts, err := signal.Run(ctx, settings)
	if calls != 1 {
		t.Fatal("recovery observation changed one-query cost")
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "private-recovery-output", "private-recovery-error", "synthetic-api-process", "synthetic-worker-process", "synthetic-private-label.example")
	}
	return SignalExecution{Alerts: alerts, ClearHolds: observation.snapshot(err != nil), Failed: err != nil}, body, err
}

// Uses the actual sustain split and incident reducer without an HTTP sender.
func egressRecoveryConsumer() (*cadenceAlertGate, *SlackAlertNotifier) {
	return &cadenceAlertGate{streaks: map[string]map[string]int{}}, &SlackAlertNotifier{
		settings:     SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{},
	}
}

// Carries the public execution's held availability through the real gate.
func egressRecoveryDeliver(signal Signal, gate *cadenceAlertGate, notifier *SlackAlertNotifier, now time.Time, execution SignalExecution) []slackTransition {
	execution.Alerts, execution.HeldAlerts = gate.split(signal, execution.Alerts)
	return notifier.observeWithLock(now, signal.Key(), execution)
}

// A traffic-bearing failure followed by two complete fresh idle intervals must
// remain armed. On the old reducer the second idle execution emits CLEARED.
func TestEgressAdmissionIdleIntervalsCannotClearNotifierPage(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(now)
	signal := NewEgressAdmissionSignal()
	gate, notifier := egressRecoveryConsumer()
	_, previous, err := egressRecoveryExecution(t, signal, context.Background(), now, processes, "", nil, nil)
	if err != nil {
		t.Fatal("warmup failed")
	}
	now = now.Add(time.Minute)
	processes[1].values["full/attempted"] += 8
	processes[1].values["full/failed"] += 8
	execution, body, err := egressRecoveryExecution(t, signal, context.Background(), now, processes, previous, nil, nil)
	if err != nil {
		t.Fatal("positive source failed")
	}
	raised := egressRecoveryDeliver(signal, gate, notifier, now, execution)
	if len(raised) != 1 || raised[0].kind != slackRaised || raised[0].alert.Class != "egress-full-no-submission" || raised[0].alert.Severity != SeverityPage {
		t.Fatal("actual full-no-submission PAGE not armed")
	}
	previous = body
	for range 2 {
		now = now.Add(time.Minute)
		execution, body, err = egressRecoveryExecution(t, signal, context.Background(), now, processes, previous, nil, nil)
		if err != nil || len(execution.Alerts) != 0 {
			t.Fatal("complete idle native interval became a new incident or unknown")
		}
		transitions := egressRecoveryDeliver(signal, gate, notifier, now, execution)
		if len(transitions) != 0 || len(notifier.keyIncidents) != 1 {
			t.Fatal("idle complete observations falsely cleared armed PAGE")
		}
		previous = body
	}
	// Two genuinely exercised native intervals can satisfy ordinary recovery.
	for index := range 2 {
		now = now.Add(time.Minute)
		processes[0].values["requests"]++
		processes[1].values["full/attempted"]++
		processes[1].values["full/submitted"]++
		processes[1].values["submission/health/acknowledged"]++
		processes[1].values["submission/attempt/acknowledged"]++
		execution, body, err = egressRecoveryExecution(t, signal, context.Background(), now, processes, previous, nil, nil)
		if err != nil || len(execution.Alerts) != 0 || len(execution.ClearHolds) != 0 {
			t.Fatal("clean traffic-bearing source did not release absence credit")
		}
		transitions := egressRecoveryDeliver(signal, gate, notifier, now, execution)
		if index == 0 && (len(transitions) != 0 || len(notifier.keyIncidents) != 1) {
			t.Fatal("one clean interval cleared PAGE early")
		}
		if index == 1 && (len(transitions) != 1 || transitions[0].kind != slackCleared || transitions[0].alert.Severity != SeverityPage || len(notifier.keyIncidents) != 0) {
			t.Fatal("two clean exercised intervals could not clear PAGE")
		}
		previous = body
	}
}

// Positive work is class/role specific, and an idle observed sibling cannot
// borrow another worker's acknowledgments or full submissions.
func TestEgressAdmissionRecoveryHoldsRequireEveryObservedRoleAndKind(t *testing.T) {
	for _, variant := range []string{"idle", "due_only", "reporter_only", "full_without_ack", "health_only", "idle_sibling", "all_work"} {
		now := time.Unix(1_700_000_000, 0).UTC()
		processes := egressAdmissionTestProcesses(now)
		if variant == "idle_sibling" {
			worker := processes[1]
			worker.host, worker.instance = "synthetic-idle-worker.example", "synthetic-idle-worker-process"
			worker.values = map[string]float64{}
			processes = append(processes, worker)
		}
		signal := NewEgressAdmissionSignal()
		_, previous, err := egressRecoveryExecution(t, signal, context.Background(), now, processes, "", nil, nil)
		if err != nil {
			t.Fatal("warmup failed")
		}
		now = now.Add(time.Minute)
		if variant != "idle" && variant != "reporter_only" {
			processes[0].values["requests"]++
		}
		if variant == "reporter_only" || variant == "health_only" || variant == "idle_sibling" || variant == "all_work" {
			processes[1].values["submission/health/acknowledged"]++
			if variant != "health_only" {
				processes[1].values["submission/attempt/acknowledged"]++
			}
		}
		if variant == "full_without_ack" || variant == "idle_sibling" || variant == "all_work" {
			processes[1].values["full/attempted"]++
			processes[1].values["full/submitted"]++
		}
		execution, _, err := egressRecoveryExecution(t, signal, context.Background(), now, processes, previous, nil, nil)
		if err != nil || len(execution.Alerts) != 0 {
			t.Fatal("observed idle or clean work manufactured an incident")
		}
		heldClasses := map[string]bool{}
		for _, hold := range execution.ClearHolds {
			if hold.SignalId != "runtime/egress-admission" || hold.Target != "egress-fleet" {
				t.Fatal("recovery held an unrelated owner")
			}
			heldClasses[hold.Class] = true
		}
		wantDue := variant == "idle" || variant == "reporter_only"
		wantReporter := variant != "reporter_only" && variant != "all_work"
		wantFull := variant != "all_work"
		if heldClasses["egress-admission-expired"] != wantDue || heldClasses["egress-submission-unacknowledged"] != wantReporter {
			t.Fatal("recovery credit crossed role or submission-kind boundary")
		}
		for _, class := range []string{"egress-full-no-submission", "egress-prober-unfunded", "egress-prober-funding-unobservable"} {
			if heldClasses[class] != wantFull {
				t.Fatal("silent full/funding work gained recovery credit")
			}
		}
	}
}

// Missing source interrupts recovery while preserving the original PAGE.
// A parent ending inside a callback publishes neither holds nor new state.
func TestEgressAdmissionRecoveryUnknownAndCancellationPreserveAuthority(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(now)
	signal := NewEgressAdmissionSignal()
	gate, notifier := egressRecoveryConsumer()
	_, previous, err := egressRecoveryExecution(t, signal, context.Background(), now, processes, "", nil, nil)
	if err != nil {
		t.Fatal("warmup failed")
	}
	now = now.Add(time.Minute)
	processes[1].values["full/attempted"] += 8
	processes[1].values["full/failed"] += 8
	execution, body, err := egressRecoveryExecution(t, signal, context.Background(), now, processes, previous, nil, nil)
	if err != nil {
		t.Fatal("positive source failed")
	}
	egressRecoveryDeliver(signal, gate, notifier, now, execution)
	previous = body
	now = now.Add(time.Minute)
	execution, _, err = egressRecoveryExecution(t, signal, context.Background(), now, processes, previous, errors.New("private-recovery-error"), nil)
	if err != nil || len(execution.Alerts) != 1 || execution.Alerts[0].Class != "egress-admission-unobservable" {
		t.Fatal("unavailable source lost visibility authority")
	}
	for _, transition := range egressRecoveryDeliver(signal, gate, notifier, now, execution) {
		if transition.kind == slackCleared {
			t.Fatal("unavailable source cleared incident")
		}
	}
	probe := signal.(*signalAdapter).probe.(*egressAdmissionProbe)
	before := map[egressAdmissionIdentity]egressAdmissionObservation{}
	for key, value := range probe.previous {
		before[key] = value
	}
	beforeAt := probe.observedAt
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	execution, _, err = egressRecoveryExecution(t, signal, ctx, now, processes, previous, errors.New("private-recovery-error"), cancel)
	if !errors.Is(err, context.Canceled) || len(execution.Alerts) != 0 || len(execution.ClearHolds) != 0 || !reflect.DeepEqual(before, probe.previous) || !beforeAt.Equal(probe.observedAt) {
		t.Fatal("retired owner published availability or changed observer history")
	}
	if strings.Contains(err.Error(), "private-recovery-error") {
		t.Fatal("lifecycle error rendered private source text")
	}
}
