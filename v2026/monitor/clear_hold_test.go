package monitor

// Availability sink controls cover per-call ownership, ended evaluations,
// shared-probe filtering, and the notifier's existing reporting ownership.

import (
	"context"
	"errors"
	"testing"
)

// Concurrent calls of the same public adapter own independent, copied sinks.
func TestSignalClearHoldsRemainScopedToConcurrentExecutions(t *testing.T) {
	type targetContextKey struct{}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	adapter := &signalAdapter{number: "8.5b", key: "journal-buffer", name: "Synthetic journal", probe: hostScopeSyntheticProbe{
		checkFn: func(ctx context.Context, _ *probeEnv) ([]finding, error) {
			entered <- struct{}{}
			<-release
			return []finding{{probeId: "host/journal-buffer", class: "journal-buffer-short", target: ctx.Value(targetContextKey{}).(string), clearHold: true}}, nil
		},
	}}
	observations := []*signalClearHoldObservation{{}, {}}
	finished := make(chan error, 2)
	for index, target := range []string{"first-journal.example", "second-journal.example"} {
		ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, observations[index])
		ctx = context.WithValue(ctx, targetContextKey{}, target)
		go func() {
			alerts, err := adapter.Run(ctx, syntheticSettings(&syntheticSource{}))
			if len(alerts) != 0 {
				err = errors.New("silent hold escaped as an alert")
			}
			finished <- err
		}()
	}
	<-entered
	<-entered
	close(release)
	for range 2 {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	for index, target := range []string{"first-journal.example", "second-journal.example"} {
		holds := observations[index].snapshot(false)
		if len(holds) != 1 || holds[0] != (SignalClassClearHold{SignalId: "host/journal-buffer", Class: "journal-buffer-short", Target: target}) {
			t.Fatal("concurrent Run calls shared class availability")
		}
		holds[0].Target = "mutated-copy.example"
		if observations[index].snapshot(false)[0].Target != target || len(observations[index].snapshot(true)) != 0 {
			t.Fatal("snapshot shared mutable storage or retained holds on failure")
		}
	}
}

// Neither a returned source error nor a canceled live parent can publish
// quiet recovery metadata, even if a probe returns a hold with its failure.
func TestSignalClearHoldsCannotOverrideFailureOrCancellation(t *testing.T) {
	for _, cancelDuringRun := range []bool{false, true} {
		observation := &signalClearHoldObservation{}
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), signalClearHoldContextKey{}, observation))
		sourceErr := errors.New("synthetic-private-journal-cause")
		adapter := &signalAdapter{number: "8.5b", key: "journal-buffer", name: "Synthetic journal", probe: hostScopeSyntheticProbe{
			checkFn: func(context.Context, *probeEnv) ([]finding, error) {
				if cancelDuringRun {
					cancel()
				}
				return []finding{{probeId: "host/journal-buffer", class: "journal-buffer-short", target: "ended-journal.example", clearHold: true}}, sourceErr
			},
		}}
		alerts, err := adapter.Run(ctx, syntheticSettings(&syntheticSource{}))
		cancel()
		if len(alerts) != 0 || !errors.Is(err, sourceErr) || len(observation.snapshot(false)) != 0 {
			t.Fatal("failed evaluation published availability or lost source authority")
		}
		if cancelDuringRun && (!errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error()) {
			t.Fatal("clear hold changed the canonical ended-parent verdict")
		}
	}
}

// An accept filter cannot silently hold another class from a shared probe.
func TestSignalClearHoldsRespectSharedProbeAcceptFilter(t *testing.T) {
	observation := &signalClearHoldObservation{}
	ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, observation)
	adapter := &signalAdapter{number: "8.5b", key: "journal-buffer", name: "Synthetic journal", accept: func(finding) bool { return false }, probe: hostScopeSyntheticProbe{
		checkFn: func(context.Context, *probeEnv) ([]finding, error) {
			return []finding{{probeId: "host/journal-buffer", class: "journal-buffer-short", target: "excluded-journal.example", clearHold: true}}, nil
		},
	}}
	if alerts, err := adapter.Run(ctx, syntheticSettings(&syntheticSource{})); err != nil || len(alerts) != 0 || len(observation.snapshot(false)) != 0 {
		t.Fatal("accept filter enrolled an excluded class's availability")
	}
}

// A hold never creates an incident or enrolls a new reporting signal, even
// when another signal already owns exactly that probe/class/target.
func TestSignalClearHoldsDoNotCreateIncidentsOrReportingOwners(t *testing.T) {
	notifier, webhook := newJournalRefillNotifier(t)
	signal := NewJournalBufferSignal()
	hold := SignalClassClearHold{SignalId: "host/journal-buffer", Class: "journal-buffer-short", Target: "shared-journal.example"}
	notifier.Handle(signal, SignalExecution{ClearHolds: []SignalClassClearHold{hold}})
	if present, _ := journalRefillIncident(notifier, hold.Class, hold.Target); present {
		t.Fatal("silent hold created an incident")
	}
	alert := Alert{SignalID: hold.SignalId, Class: hold.Class, Target: hold.Target, Severity: SeverityWarn, Environment: "synthetic"}
	notifier.Handle(slackOtherTestSignal, SignalExecution{Alerts: Alerts{alert}})
	notifier.Handle(slackOtherTestSignal, SignalExecution{})
	notifier.Handle(signal, SignalExecution{ClearHolds: []SignalClassClearHold{hold}})
	notifier.Handle(slackOtherTestSignal, SignalExecution{})
	if present, _ := journalRefillIncident(notifier, hold.Class, hold.Target); present {
		t.Fatal("unowned hold enrolled its signal or reset the other owner's absence")
	}
	closeSlackNotifier(t, notifier)
	if texts := webhook.posted(); len(texts) != 2 {
		t.Fatalf("unowned holds changed webhook transitions: %d", len(texts))
	}
}
