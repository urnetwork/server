package monitor

// The Slack notifier behind the real run loop and cadence gate. Each test
// scripts one signal's executions and steps the loop with explicit ticks, so
// the notifier sees exactly what the continuous CLI hands it.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// One scripted signal execution: the alerts the signal returns, or its error.
type scriptedExecution struct {
	alerts Alerts
	err    error
}

// A signal whose every Run returns the next scripted execution.
type scriptedSignal struct {
	executions chan scriptedExecution
}

func (self *scriptedSignal) Number() string         { return slackTestSignal.number }
func (self *scriptedSignal) Key() string            { return slackTestSignal.key }
func (self *scriptedSignal) ID() string             { return slackTestSignal.id }
func (self *scriptedSignal) Name() string           { return "PostgreSQL active backends" }
func (self *scriptedSignal) Cadence() time.Duration { return time.Minute }
func (self *scriptedSignal) Run(ctx context.Context, _ SignalSettings) (Alerts, error) {
	select {
	case execution := <-self.executions:
		return execution.alerts, execution.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Runs the scripted executions through the run loop, one tick each, and hands
// every gated execution to handle in order.
func runScriptedExecutions(t *testing.T, executions []scriptedExecution, handle func(Signal, SignalExecution)) {
	t.Helper()
	signal := &scriptedSignal{executions: make(chan scriptedExecution)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	handled := make(chan struct{})
	runErr := make(chan error, 1)
	go func() {
		runErr <- NewWithSignals(syntheticSettings(nil), signal).runExecutionLoop(
			ctx,
			RunLoopOptions{},
			func(_ context.Context, signal Signal, execution SignalExecution) error {
				handle(signal, execution)
				handled <- struct{}{}
				return nil
			},
			func(time.Duration) runLoopTicker { return &manualRunLoopTicker{c: ticks} },
		)
	}()
	for i, execution := range executions {
		// the first execution starts at once; each later one waits for a tick
		if 0 < i {
			select {
			case ticks <- time.Now():
			case <-time.After(30 * time.Second):
				t.Fatalf("execution %d: the loop never waited for its cadence", i+1)
			}
		}
		select {
		case signal.executions <- execution:
		case <-time.After(30 * time.Second):
			t.Fatalf("execution %d never started", i+1)
		}
		select {
		case <-handled:
		case <-time.After(30 * time.Second):
			t.Fatalf("execution %d was never handled", i+1)
		}
	}
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run loop returned %v after cancellation", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run loop did not stop after cancellation")
	}
}

// Wires the scripted executions into a notifier the way the continuous CLI
// does and returns what it posted.
func postThroughTheGate(t *testing.T, executions []scriptedExecution) []string {
	t.Helper()
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	runScriptedExecutions(t, executions, notifier.Handle)
	closeSlackNotifier(t, notifier)
	return webhook.posted()
}

// A Sustain 2 alert that misses one run comes back held by the gate for a
// run. That empty gated run is not an absence, so the incident stays raised;
// two complete runs without it still clear it.
func TestSlackAlertNotifierBehindTheGateIgnoresAOneRunDip(t *testing.T) {
	alert := slackTestAlert("pg-1", "", SeverityWarn)
	alert.Sustain = 2
	present := scriptedExecution{alerts: Alerts{alert}}
	absent := scriptedExecution{}
	texts := postThroughTheGate(t, []scriptedExecution{present, present, present, absent, present, present, present, absent, absent})
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-1",
		"[CLEARED] WARN (synthetic)",
	)
}

// The gate holds a failed run's visibility alert below its Sustain of 2, so
// that run reaches the notifier with no gated alerts. It is still incomplete:
// only the complete run after it counts, and one is not enough to clear.
func TestSlackAlertNotifierBehindTheGateIgnoresOneFailedRun(t *testing.T) {
	present := scriptedExecution{alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}}
	failed := scriptedExecution{err: errors.New("synthetic ssh timeout")}
	texts := postThroughTheGate(t, []scriptedExecution{present, present, failed, {}, present, present})
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-1",
	)
}

// A page promoted by PageSustain survives a one-run dip: the gate restarts the
// alert at warn, which neither clears nor re-escalates the incident.
func TestSlackAlertNotifierBehindTheGateKeepsAPageThroughADip(t *testing.T) {
	alert := slackTestAlert("pg-1", "", SeverityWarn)
	alert.Sustain = 2
	alert.PageSustain = 3
	present := scriptedExecution{alerts: Alerts{alert}}
	texts := postThroughTheGate(t, []scriptedExecution{present, present, present, present, {}, present, present, present, present})
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-1",
		"[PAGE] escalated from WARN (synthetic): Active client backends on pg-1",
	)
}

// A run that reports a visibility alert for a target it could not observe,
// without failing, is incomplete even while the gate holds that alert: the
// signal's other incidents are unknown, not absent.
func TestSlackAlertNotifierBehindTheGateTreatsAPartialRunAsIncomplete(t *testing.T) {
	present := scriptedExecution{alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}}
	// what a signal adapter emits for one target it could not observe
	unobserved := alertFromFinding(syntheticSettings(nil), slackTestSignal.number, slackTestSignal.key, "PostgreSQL active backends",
		cannotObserveFinding("pg-2", errors.New("synthetic ssh timeout")))
	if unobserved.Sustain != 2 {
		t.Fatalf("cannot-observe Sustain = %d, want 2", unobserved.Sustain)
	}
	partial := scriptedExecution{alerts: Alerts{unobserved}}
	texts := postThroughTheGate(t, []scriptedExecution{present, present, partial, {}, present})
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-1",
	)
}

// The execution handler learns which alerts the gate holds and which runs
// failed, alongside the gated alerts an AlertHandler receives.
func TestRunExecutionLoopReportsHeldAlertsAndFailures(t *testing.T) {
	alert := slackTestAlert("pg-1", "", SeverityWarn)
	alert.Sustain = 2
	failed := scriptedExecution{err: errors.New("synthetic ssh timeout")}
	executions := []SignalExecution{}
	runScriptedExecutions(t, []scriptedExecution{{alerts: Alerts{alert}}, {alerts: Alerts{alert}}, failed, failed}, func(_ Signal, execution SignalExecution) {
		executions = append(executions, execution)
	})
	summary := func(alerts Alerts) []string {
		identities := []string{}
		for _, alert := range alerts {
			identities = append(identities, alert.SignalID+"|"+alert.Class)
		}
		return identities
	}
	for i, want := range []struct {
		alerts []string
		held   []string
		failed bool
	}{
		{alerts: []string{}, held: []string{"pg/active-pileup|active-pileup"}},
		{alerts: []string{"pg/active-pileup|active-pileup"}, held: []string{}},
		{alerts: []string{}, held: []string{"monitor/visibility|cannot-observe"}, failed: true},
		{alerts: []string{"monitor/visibility|cannot-observe"}, held: []string{}, failed: true},
	} {
		execution := executions[i]
		alerts, held := summary(execution.Alerts), summary(execution.HeldAlerts)
		if fmt.Sprint(alerts) != fmt.Sprint(want.alerts) || fmt.Sprint(held) != fmt.Sprint(want.held) || execution.Failed != want.failed {
			t.Errorf("execution %d: alerts %v, held %v, failed %t; want %v, %v, %t", i+1, alerts, held, execution.Failed, want.alerts, want.held, want.failed)
		}
	}
}
