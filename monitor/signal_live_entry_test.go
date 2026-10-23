// Live-entry adapter authority is independent of opt-in host exclusions.
// These controls never query a production source or persist probe state.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Both fault and healthy source completions are rejected after a live owner
// ends; host-scoped behavior remains the existing compatible control.
func TestSignalLiveEntryDirectCancellationParity(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, healthy := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			probe := hostScopeSyntheticProbe{checkFn: func(probeCtx context.Context, env *probeEnv) ([]finding, error) {
				calls++
				if probeCtx.Err() != nil {
					t.Fatal("synthetic evaluation did not enter live")
				}
				_, actualScoped := env.runner.(*hostScopeRunner)
				if actualScoped != scoped {
					t.Fatal("synthetic scope precondition changed")
				}
				cancel()
				return []finding{{probeId: "synthetic/live-entry", tier: tierWarn, class: "synthetic-live-entry", target: "synthetic-source", healthy: healthy}}, nil
			}}
			settings := syntheticSettings(&syntheticSource{})
			if scoped {
				settings.disabledHosts = []HostSettings{{Name: "excluded.example.test"}}
			}
			adapter := &signalAdapter{number: "1.7", key: "synthetic-live-entry", name: "Synthetic live entry", probe: probe}
			alerts, err := adapter.Run(ctx, settings)
			if calls != 1 || len(alerts) != 0 || err != context.Canceled {
				t.Fatal("ended live evaluation retained findings or lost exact context authority")
			}
			cancel()
		}
	}
}

// Returned source causes survive without replacing canonical parent text or
// leaking their private payload into public visibility rendering.
func TestSignalLiveEntryReturnedCauseAndPrivacy(t *testing.T) {
	private := errors.New("synthetic-private-source-cause")
	typed := &sshAdmissionUnavailableError{err: private, phase: sshAdmissionFailureAcquire}
	sourceErr := errors.Join(typed, context.DeadlineExceeded)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter := &signalAdapter{number: "1.7", key: "synthetic-live-entry", name: "Synthetic live entry", probe: hostScopeSyntheticProbe{checkFn: func(context.Context, *probeEnv) ([]finding, error) {
		cancel()
		return []finding{{probeId: "synthetic/live-entry", class: "synthetic-positive", target: "synthetic-source"}}, sourceErr
	}}}
	settings := syntheticSettings(&syntheticSource{})
	alerts, err := adapter.Run(ctx, settings)
	var returned *sshAdmissionUnavailableError
	if len(alerts) != 0 || !errors.Is(err, context.Canceled) || !errors.Is(err, sourceErr) || !errors.Is(err, private) || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &returned) || returned != typed || err.Error() != context.Canceled.Error() {
		t.Fatal("ended unscoped result lost joined source/parent authority")
	}
	alert := visibilityAlert(settings, adapter, err)
	if alert.Class != "cannot-observe" || !alert.unobservable {
		t.Fatal("source cause overrode parent visibility authority")
	}
	requireAlertOmits(t, alert, "synthetic-private-source-cause", "synthetic-positive")
}

// The direct public registry entry does not have an outer scheduler fence.
func TestSignalLiveEntryRunSignalRejectsEndedResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	adapter := &signalAdapter{number: "1.7", key: "synthetic-live-entry", name: "Synthetic live entry", probe: hostScopeSyntheticProbe{checkFn: func(context.Context, *probeEnv) ([]finding, error) {
		calls++
		cancel()
		return []finding{{probeId: "synthetic/live-entry", class: "synthetic-positive", target: "synthetic-source"}}, nil
	}}}
	alerts, err := NewWithSignals(syntheticSettings(&syntheticSource{}), adapter).RunSignal(ctx, adapter.Key())
	if calls != 1 || len(alerts) != 0 || err != context.Canceled {
		t.Fatal("public RunSignal accepted an ended evaluation")
	}
}

// One-shot execution keeps its visibility contract, without accepting the
// ended evaluation's positive finding or claiming a complete healthy result.
func TestSignalLiveEntryOneShotRetainsFailureAuthority(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter := &signalAdapter{number: "1.7", key: "synthetic-live-entry", name: "Synthetic live entry", probe: hostScopeSyntheticProbe{checkFn: func(context.Context, *probeEnv) ([]finding, error) {
		cancel()
		return []finding{{probeId: "synthetic/live-entry", tier: tierPage, class: "synthetic-positive", target: "synthetic-source"}}, nil
	}}}
	alerts, err := NewWithSignals(syntheticSettings(&syntheticSource{}), adapter).Run(ctx)
	if !errors.Is(err, context.Canceled) || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable {
		t.Fatal("one-shot accepted an interrupted result as a positive or healthy completion")
	}
}

// Existing no-policy readers deliberately use a pre-canceled context to skip
// delayed work. This repair does not redefine that compatibility contract.
func TestSignalLiveEntryPreCanceledLocalCompatibility(t *testing.T) {
	sourceErr := errors.New("synthetic-local-source-authority")
	for _, returnedErr := range []error{nil, sourceErr} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		adapter := &signalAdapter{number: "1.7", key: "synthetic-live-entry", name: "Synthetic live entry", probe: hostScopeSyntheticProbe{checkFn: func(probeCtx context.Context, env *probeEnv) ([]finding, error) {
			calls++
			if probeCtx.Err() != context.Canceled {
				t.Fatal("compatibility context did not enter canceled")
			}
			if _, scoped := env.runner.(*hostScopeRunner); scoped {
				t.Fatal("compatibility fixture enabled host scope")
			}
			return []finding{{probeId: "synthetic/live-entry", tier: tierWarn, class: "synthetic-local-result", target: "synthetic-source"}}, returnedErr
		}}}
		alerts, err := adapter.Run(ctx, syntheticSettings(&syntheticSource{}))
		if calls != 1 || err != returnedErr || (returnedErr == nil && len(alerts) != 1) || (returnedErr != nil && len(alerts) != 0) {
			t.Fatal("pre-canceled no-policy local contract changed")
		}
	}
}

// A child/source failure with a live evaluation owner retains source error
// authority, rather than becoming an adapter parent-lifecycle wrapper.
func TestSignalLiveEntryLiveParentKeepsChildError(t *testing.T) {
	providerErr := newProviderVisibilityError(providerAuthenticationClass, "synthetic-private-provider")
	for _, sourceErr := range []error{context.Canceled, context.DeadlineExceeded, providerErr} {
		ctx, cancel := context.WithCancel(context.Background())
		adapter := &signalAdapter{number: "1.7", key: "synthetic-live-entry", name: "Synthetic live entry", probe: hostScopeSyntheticProbe{checkFn: func(context.Context, *probeEnv) ([]finding, error) { return nil, sourceErr }}}
		alerts, err := adapter.Run(ctx, syntheticSettings(&syntheticSource{}))
		var wrapped *signalContextError
		if ctx.Err() != nil || err != sourceErr || len(alerts) != 0 || errors.As(err, &wrapped) {
			t.Fatal("live owner lost independent source error authority")
		}
		cancel()
	}
}

// An observer is not called after an already ended probe, and cancellation
// during a live callback withholds results without rolling back its write.
func TestSignalLiveEntryObserverBoundaries(t *testing.T) {
	for _, cancelInSource := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		later := 0
		settings := syntheticSettings(&syntheticSource{})
		settings.UrlProbeCoverageObserver = func(UrlProbeCoverageObservation) error {
			calls++
			if ctx.Err() != nil {
				t.Fatal("observer began after its owner ended")
			}
			cancel()
			return nil
		}
		settings.ProviderSelectionObserver = func(ProviderSelectionObservation) error { later++; return nil }
		adapter := &signalAdapter{number: "1.7", key: "synthetic-live-entry", name: "Synthetic live entry", probe: nil}
		adapter.probe = hostScopeSyntheticProbe{checkFn: func(_ context.Context, env *probeEnv) ([]finding, error) {
			env.urlProbeCoverageObservation = &UrlProbeCoverageObservation{}
			env.providerSelectionObservation = &ProviderSelectionObservation{}
			if cancelInSource {
				cancel()
			}
			return []finding{{probeId: "synthetic/live-entry", tier: tierWarn, class: "synthetic-positive", target: "synthetic-source"}}, nil
		}}
		alerts, err := adapter.Run(ctx, settings)
		want := 1
		if cancelInSource {
			want = 0
		}
		if err != context.Canceled || len(alerts) != 0 || calls != want || later != 0 {
			t.Fatal("observer boundary accepted canceled results or started later output")
		}
		cancel()
	}
}

// A per-execution owned budget is distinct from the monitor's live parent.
type liveEntryBudgetProbe struct {
	checkFn func(context.Context, *probeEnv) ([]finding, error)
}

// The synthetic probe has no transport identity or remote side effects.
func (*liveEntryBudgetProbe) id() string { return "synthetic/live-entry" }

// Its fault is a warning; the public cadence gate owns admission.
func (*liveEntryBudgetProbe) tier() string { return tierWarn }

// Explicit ticker channels drive cadence independently from the owned budget.
func (*liveEntryBudgetProbe) cadence() time.Duration { return time.Minute }

// This synthetic timeout is only a deterministic control, not runtime policy.
func (*liveEntryBudgetProbe) runBudget() time.Duration { return time.Second }

// The control callback intentionally ignores the ended child after waiting.
func (self *liveEntryBudgetProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	return self.checkFn(ctx, env)
}

// Capture actual execution-loop/cadence-gate outputs under deterministic
// virtual deadlines. Callback faults return to the test owner after loop join.
func liveEntryExecutionTrace(t *testing.T, expire bool, handlerErr error) (Signal, []SignalExecution, error) {
	t.Helper()
	var signal Signal
	var trace []SignalExecution
	var resultErr error
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		fixtureErrors := make(chan error, 1)
		probe := &liveEntryBudgetProbe{checkFn: func(probeCtx context.Context, env *probeEnv) ([]finding, error) {
			calls++
			if probeCtx.Err() != nil {
				err := errors.New("budget fixture did not enter live")
				fixtureErrors <- err
				return nil, err
			}
			if _, scoped := env.runner.(*hostScopeRunner); scoped {
				err := errors.New("budget fixture enabled host scope")
				fixtureErrors <- err
				return nil, err
			}
			if calls <= 2 {
				return []finding{{probeId: "synthetic/live-entry", tier: tierWarn, class: "synthetic-live-entry", target: "synthetic-source", symptom: "Synthetic previously raised incident", sustain: 1}}, nil
			}
			if expire {
				<-probeCtx.Done()
			}
			return nil, nil
		}}
		signal = &signalAdapter{number: "1.7", key: "synthetic-live-entry", name: "Synthetic live entry", probe: probe}
		ticks := make(chan time.Time)
		handled := make(chan SignalExecution)
		done := make(chan error, 1)
		go func() {
			done <- NewWithSignals(syntheticSettings(&syntheticSource{}), signal).runExecutionLoop(ctx, RunLoopOptions{}, func(parent context.Context, _ Signal, execution SignalExecution) error {
				select {
				case err := <-fixtureErrors:
					return err
				default:
				}
				if parent.Err() != nil {
					return errors.New("budget fixture ended outer monitor")
				}
				if handlerErr != nil {
					return handlerErr
				}
				handled <- execution
				return nil
			}, func(time.Duration) runLoopTicker { return &manualRunLoopTicker{c: ticks} })
		}()
		for i := 0; i < 4; i++ {
			if i > 0 {
				select {
				case ticks <- time.Now():
				case resultErr = <-done:
					return
				}
			}
			select {
			case execution := <-handled:
				trace = append(trace, execution)
			case resultErr = <-done:
				return
			}
		}
		if ctx.Err() != nil || calls != 4 {
			t.Fatal("public budget trace lost live outer owner or exact call count")
		}
		cancel()
		resultErr = <-done
	})
	return signal, trace, resultErr
}

// Actual public loop outputs pass directly into the real notifier. No
// private incident/counter mutation or fabricated failure flags are used.
func liveEntryNotifierTexts(t *testing.T, signal Signal, trace []SignalExecution) []string {
	t.Helper()
	webhook, url := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(url))
	for _, execution := range trace {
		notifier.Handle(signal, execution)
	}
	closeSlackNotifier(t, notifier)
	return webhook.posted()
}

// This causal control reaches the notifier before checking the outcome:
// baseline unscoped acceptance clears the raised incident after two expired
// child turns, despite the outer monitor still being live.
func TestSignalLiveEntryOwnedBudgetCannotClearNotifier(t *testing.T) {
	signal, trace, err := liveEntryExecutionTrace(t, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	texts := liveEntryNotifierTexts(t, signal, trace)
	cleared := false
	for _, text := range texts {
		cleared = cleared || strings.Contains(text, "[CLEARED]")
	}
	if len(trace) != 4 || trace[0].Failed || trace[1].Failed || !trace[2].Failed || !trace[3].Failed || len(texts) == 0 || cleared {
		t.Fatal("ended owned-budget results became healthy public notifier absence")
	}
}

// The same public path still clears after two genuinely healthy live turns.
func TestSignalLiveEntryHealthyOwnerAllowsNotifierClear(t *testing.T) {
	signal, trace, err := liveEntryExecutionTrace(t, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	texts := liveEntryNotifierTexts(t, signal, trace)
	cleared := false
	for _, text := range texts {
		cleared = cleared || strings.Contains(text, "[CLEARED]")
	}
	if len(trace) != 4 || trace[2].Failed || trace[3].Failed || !cleared {
		t.Fatal("healthy live owner lost public notifier recovery")
	}
}

// A callback error must reach the test owner without a handled execution;
// the same virtual-time helper joins the actual loop before returning it.
func TestSignalLiveEntryHandlerFailureReachesTestOwner(t *testing.T) {
	failure := errors.New("synthetic execution handler failure")
	signal, trace, err := liveEntryExecutionTrace(t, false, failure)
	if signal == nil || err != failure || len(trace) != 0 {
		t.Fatal("execution handler failure did not reach the test owner after loop join")
	}
}

// The existing outer lifecycle fence is a healthy control, not a new
// assertion that every ended global monitor used to reach its notifier.
func TestSignalLiveEntryEndedMonitorStillSuppressesHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	handled := 0
	adapter := &signalAdapter{number: "1.7", key: "synthetic-live-entry", name: "Synthetic live entry", probe: hostScopeSyntheticProbe{checkFn: func(context.Context, *probeEnv) ([]finding, error) {
		calls++
		cancel()
		return []finding{{probeId: "synthetic/live-entry", tier: tierWarn, class: "synthetic-positive", target: "synthetic-source"}}, nil
	}}}
	err := NewWithSignals(syntheticSettings(&syntheticSource{}), adapter).RunExecutionLoop(ctx, RunLoopOptions{}, func(context.Context, Signal, SignalExecution) error { handled++; return nil })
	if err != nil || calls != 1 || handled != 0 {
		t.Fatal("existing monitor-parent retirement fence changed")
	}
}

// An owning probe's direct parent wrapper remains exact when it already carries
// this parent. A different child verdict is still wrapped, and a live parent's
// source error remains untouched. No arbitrary errors.Is match bypasses policy.
func TestSignalLiveEntryPreservesReturnedParentWrapper(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, test := range []struct {
			returnedParent error
			endOwner       bool
			nestedSource   bool
		}{
			{returnedParent: context.Canceled, endOwner: true},
			{returnedParent: context.DeadlineExceeded, endOwner: true},
			{returnedParent: context.DeadlineExceeded, endOwner: false},
			{returnedParent: context.Canceled, endOwner: true, nestedSource: true},
		} {
			ctx, cancel := context.WithCancel(context.Background())
			privateCause := errors.New("synthetic-private-direct-parent-wrapper")
			admission := &sshAdmissionUnavailableError{err: privateCause, phase: sshAdmissionFailureRelease}
			sourceErr := &signalContextError{contextErr: test.returnedParent, sourceErr: admission}
			var returnedSource error = sourceErr
			if test.nestedSource {
				returnedSource = errors.Join(sourceErr, errors.New("synthetic-private-nested-wrapper"))
			}
			calls := 0
			adapter := &signalAdapter{number: "1.7", key: "synthetic-live-entry", name: "Synthetic live entry", probe: hostScopeSyntheticProbe{checkFn: func(probeCtx context.Context, _ *probeEnv) ([]finding, error) {
				calls++
				if probeCtx.Err() != nil {
					t.Fatal("direct parent-wrapper source did not enter live")
				}
				if test.endOwner {
					cancel()
				}
				return nil, returnedSource
			}}}
			settings := syntheticSettings(&syntheticSource{})
			if scoped {
				settings.disabledHosts = []HostSettings{{Name: "excluded.example.test"}}
			}
			alerts, err := adapter.Run(ctx, settings)
			if calls != 1 || len(alerts) != 0 || !errors.Is(err, returnedSource) || !errors.Is(err, sourceErr) || !errors.Is(err, admission) || !errors.Is(err, privateCause) || !errors.Is(err, test.returnedParent) {
				t.Fatal("direct parent wrapper lost original source identity or repeated work")
			}
			var returned *signalContextError
			if !errors.As(err, &returned) {
				t.Fatal("direct parent wrapper lost typed error authority")
			}
			if !test.endOwner || (test.returnedParent == context.Canceled && !test.nestedSource) {
				if err != sourceErr || returned != sourceErr || returned.sourceErr != admission {
					t.Fatal("same-parent or live-parent source wrapper was unnecessarily replaced")
				}
			} else if returned == sourceErr || returned.contextErr != context.Canceled || returned.sourceErr != returnedSource || err.Error() != context.Canceled.Error() {
				t.Fatal("different child verdict bypassed canonical parent authority")
			}
			var returnedAdmission *sshAdmissionUnavailableError
			if !errors.As(err, &returnedAdmission) || returnedAdmission != admission {
				t.Fatal("direct parent wrapper lost its exact typed source leaf")
			}
			requireAlertOmits(t, visibilityAlert(settings, adapter, err), "synthetic-private-direct-parent-wrapper", "synthetic-private-nested-wrapper")
			cancel()
		}
	}
}
