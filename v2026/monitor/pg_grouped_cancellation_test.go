// Owner cancellation is distinct from a source-local timeout. Public source
// callbacks cancel synchronously, so no sleep, process or scheduler supplies
// the causal ordering; all callbacks are in-memory synthetic observations.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Retain the exact adapter result and source call counts before fixture cleanup.
type pgGroupedCancellationResult struct {
	signal          Signal
	settings        SignalSettings
	alerts          Alerts
	returnedErr     error
	parentErr       error
	triggerReads    int
	readsAfterEnd   int
	completionReads int
	aggregateReads  int
	profileReads    int
}

// Each selected source return can end its owner or fail locally while the
// owner stays live. Later callbacks deliberately return valid data: correctness
// must come from the probe guard, not synthetic transports refusing work.
func pgGroupedCancellationFixture(t *testing.T, signal Signal, component string, scoped, cancelOwner bool, sourceErr error) pgGroupedCancellationResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := pgGroupedCancellationResult{signal: signal}
	taskName := "CloseExpiredContracts"
	if signal.Key() == "netescrow" {
		taskName = "ReconcileNetEscrow"
	}
	observe := func(current string) bool {
		if ctx.Err() != nil {
			result.readsAfterEnd++
		}
		if current != component {
			return false
		}
		result.triggerReads++
		if cancelOwner {
			cancel()
		}
		return true
	}
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "AS legacy_reservation_page") {
				result.profileReads++
				if observe("profile") {
					// A nil error deliberately returns malformed late data. Parent
					// cancellation must win before profile parsing can invent an error.
					return nil, sourceErr
				}
				return []Row{{"0", "0", "0", "0", "0", "0", "0", "0", "0"}}, nil
			}
			result.completionReads++
			if observe("completion") {
				return nil, sourceErr
			}
			if signal.Key() == "netescrow" {
				return []Row{{"completed", "120", "0", "00000000-0000-0000-0000-000000000001", "1788000000"}}, nil
			}
			return []Row{{"00000000-0000-0000-0000-000000000001", "completed", "120", "0", "1788000000"}}, nil
		},
		localFn: func(_ string, args ...string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "--query=[sm]reconcile net escrow") {
				result.aggregateReads++
				if observe("aggregate") {
					return "", sourceErr
				}
				return "", nil
			}
			if observe("lifecycle") {
				return "", sourceErr
			}
			return "[00000000-0000-0000-0000-000000000002]eval error(130.0s) (reschedule) synthetic/work." + taskName + "({}) = Timeout", nil
		},
	}
	result.settings = syntheticSettings(source)
	if scoped {
		result.settings.disabledHosts = []HostSettings{{Name: "disabled.example.test"}}
	}
	result.alerts, result.returnedErr = signal.Run(ctx, result.settings)
	result.parentErr = ctx.Err()
	return result
}

// The existing scoped adapter owns the parent/source error join. Unscoped
// callers keep the original source error, while public evidence stays private.
func pgGroupedRequireCanceledSource(t *testing.T, result pgGroupedCancellationResult, scoped bool, sourceErr, nestedErr error) {
	t.Helper()
	if result.parentErr != context.Canceled || result.triggerReads != 1 ||
		result.returnedErr == nil || len(result.alerts) != 0 {
		t.Fatal("ended grouped owner returned successful partial observations")
	}
	returnedSource := result.returnedErr
	if scoped {
		var interrupted *signalContextError
		if !errors.Is(result.returnedErr, context.Canceled) || !errors.As(result.returnedErr, &interrupted) ||
			result.returnedErr.Error() != context.Canceled.Error() {
			t.Fatal("grouped cancellation lost the scoped parent verdict")
		}
		returnedSource = interrupted.sourceErr
	}
	if returnedSource != sourceErr || !errors.Is(result.returnedErr, sourceErr) || !errors.Is(result.returnedErr, nestedErr) {
		t.Fatal("grouped cancellation discarded the exact returned source cause")
	}
	if result.readsAfterEnd != 0 {
		t.Fatal("grouped cancellation admitted later source work")
	}
	alert := visibilityAlert(result.settings, result.signal, result.returnedErr)
	requireAlertOmits(t, alert, "private-grouped-cancel-cause", "private-grouped-cancel-secondary")
}

// Causal RED: the prior candidate swallowed this original PG cause and, for
// NetEscrow, also contacted the aggregate source after the owner had ended.
func TestPgGroupedCanceledCompletionPreservesSourceCause(t *testing.T) {
	for _, newSignal := range []func() Signal{NewCloseDurationSignal, NewNetEscrowSignal} {
		for _, scoped := range []bool{false, true} {
			signal := newSignal()
			t.Logf("signal=%s scoped=%t", signal.Key(), scoped)
			cause := errors.New("private-grouped-cancel-cause")
			sourceErr := errors.Join(cause, errors.New("private-grouped-cancel-secondary"))
			result := pgGroupedCancellationFixture(t, signal, "completion", scoped, true, sourceErr)
			if !errors.Is(result.returnedErr, sourceErr) {
				t.Fatal("A5-P1 canceled completion discarded original source cause")
			}
			pgGroupedRequireCanceledSource(t, result, scoped, sourceErr, cause)
			if result.completionReads != 1 || result.aggregateReads != 0 || result.profileReads != 0 {
				t.Fatal("canceled completion continued independent reads")
			}
		}
	}
}

// Causal RED: an aggregate failure after parent cancellation is an interrupted
// run, even when an earlier completion independently established an overrun.
func TestPgGroupedCanceledAggregatePreservesSourceCause(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		cause := errors.New("private-grouped-cancel-cause")
		sourceErr := errors.Join(cause, errors.New("private-grouped-cancel-secondary"))
		result := pgGroupedCancellationFixture(t, NewNetEscrowSignal(), "aggregate", scoped, true, sourceErr)
		if !errors.Is(result.returnedErr, sourceErr) {
			t.Fatal("A5-P1 canceled aggregate discarded original source cause")
		}
		pgGroupedRequireCanceledSource(t, result, scoped, sourceErr, cause)
		if result.completionReads != 1 || result.aggregateReads != 1 || result.profileReads != 0 {
			t.Fatal("canceled aggregate continued optional attribution")
		}
	}
}

// Optional attribution also cannot turn an ended owner into a successful run.
func TestPgGroupedCanceledProfilePreservesSourceCause(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		cause := errors.New("private-grouped-cancel-cause")
		sourceErr := errors.Join(cause, errors.New("private-grouped-cancel-secondary"))
		result := pgGroupedCancellationFixture(t, NewNetEscrowSignal(), "profile", scoped, true, sourceErr)
		if !errors.Is(result.returnedErr, sourceErr) {
			t.Fatal("A5-P1 canceled profile discarded original source cause")
		}
		pgGroupedRequireCanceledSource(t, result, scoped, sourceErr, cause)
		if result.completionReads != 1 || result.aggregateReads != 1 || result.profileReads != 1 {
			t.Fatal("optional attribution cancellation changed source ownership")
		}
	}
}

// A returned lifecycle failure stops before PG. The shared reader currently
// replaces a canceled gateway cause and flattens journal errors, so this test
// claims the returned helper error and no further read, not recovery of the
// original gateway cause that the helper never returned.
func TestPgGroupedCanceledLifecycleStopsBeforeCompletion(t *testing.T) {
	for _, newSignal := range []func() Signal{NewCloseDurationSignal, NewNetEscrowSignal} {
		for _, scoped := range []bool{false, true} {
			result := pgGroupedCancellationFixture(t, newSignal(), "lifecycle", scoped, true, errors.New("private-grouped-cancel-cause"))
			if result.returnedErr == nil || result.completionReads != 0 || result.aggregateReads != 0 || result.profileReads != 0 {
				t.Fatal("A5-P1 canceled lifecycle continued source collection")
			}
			if result.parentErr != context.Canceled || result.triggerReads != 1 || result.readsAfterEnd != 0 || len(result.alerts) != 0 {
				t.Fatal("canceled lifecycle returned a successful observation")
			}
			returnedSource := result.returnedErr
			if scoped {
				var interrupted *signalContextError
				if !errors.Is(result.returnedErr, context.Canceled) || !errors.As(result.returnedErr, &interrupted) {
					t.Fatal("canceled lifecycle lost the returned helper error or parent verdict")
				}
				returnedSource = interrupted.sourceErr
			}
			if returnedSource == nil || !strings.HasPrefix(returnedSource.Error(), "fleet log gateway:") ||
				!strings.Contains(returnedSource.Error(), "taskworker journal: no services hosts in inventory") ||
				!errors.Is(result.returnedErr, returnedSource) {
				t.Fatal("canceled lifecycle replaced the returned helper error")
			}
			requireAlertOmits(t, visibilityAlert(result.settings, result.signal, result.returnedErr), "private-grouped-cancel-cause")
		}
	}
}

// Even a transport that returns valid data after canceling its parent cannot
// authorize healthy/partial success or a subsequent source read.
func TestPgGroupedCanceledSuccessfulSourceKeepsParentSentinel(t *testing.T) {
	for _, test := range []struct {
		newSignal func() Signal
		component string
	}{
		{newSignal: NewCloseDurationSignal, component: "completion"},
		{newSignal: NewNetEscrowSignal, component: "completion"},
		{newSignal: NewNetEscrowSignal, component: "aggregate"},
		{newSignal: NewNetEscrowSignal, component: "profile"},
	} {
		for _, scoped := range []bool{false, true} {
			result := pgGroupedCancellationFixture(t, test.newSignal(), test.component, scoped, true, nil)
			if result.returnedErr != context.Canceled || len(result.alerts) != 0 || result.readsAfterEnd != 0 {
				t.Fatal("A5-P1 successful late source erased parent cancellation")
			}
			if result.triggerReads != 1 || result.parentErr != context.Canceled {
				t.Fatal("successful late source fixture did not end its owner")
			}
		}
	}
}

// Source-local cancellation/deadline errors leave the owning run live. Its
// complete incident and explicit unknown component must remain together.
func TestPgGroupedLiveParentRetainsSourceTimeoutAndIncident(t *testing.T) {
	for _, test := range []struct {
		newSignal func() Signal
		component string
		class     string
	}{
		{newSignal: NewCloseDurationSignal, component: "completion", class: "close-duration-overrun"},
		{newSignal: NewNetEscrowSignal, component: "completion", class: "netescrow-reconcile-overrun"},
		{newSignal: NewNetEscrowSignal, component: "aggregate", class: "netescrow-reconcile-overrun"},
		{newSignal: NewNetEscrowSignal, component: "profile", class: "netescrow-reconcile-overrun"},
	} {
		for _, sourceErr := range []error{context.Canceled, context.DeadlineExceeded} {
			for _, scoped := range []bool{false, true} {
				result := pgGroupedCancellationFixture(t, test.newSignal(), test.component, scoped, false, sourceErr)
				if result.parentErr != nil || result.returnedErr != nil || result.triggerReads != 1 || result.readsAfterEnd != 0 {
					t.Fatal("live grouped owner lost partial-source behavior")
				}
				if len(result.alerts) != 2 {
					t.Fatal("live source interruption lost incident or coverage")
				}
				requireAlertClass(t, result.alerts, test.class)
				unknown := requireAlertClass(t, result.alerts, "cannot-observe")
				if unknown.Observed != "error_class="+classifyObservationError(sourceErr) {
					t.Fatal("live source interruption changed its component error classification")
				}
			}
		}
	}
}

// Preserve the existing lifecycle-read qualification: a live owner's valid
// completed overrun survives failed log acquisition with an unavailable-log
// qualifier. The shared reader's historical cause-chain limits stay explicit.
func TestPgGroupedLiveParentLifecycleFailureRetainsCompletedOverrun(t *testing.T) {
	for _, newSignal := range []func() Signal{NewCloseDurationSignal, NewNetEscrowSignal} {
		for _, sourceErr := range []error{context.Canceled, context.DeadlineExceeded} {
			for _, scoped := range []bool{false, true} {
				signal := newSignal()
				result := pgGroupedCancellationFixture(t, signal, "lifecycle", scoped, false, sourceErr)
				if result.parentErr != nil || result.returnedErr != nil || result.triggerReads != 1 || result.readsAfterEnd != 0 || result.completionReads != 1 {
					t.Fatal("live lifecycle failure ended grouped source collection")
				}
				class := "close-duration-overrun"
				if signal.Key() == "netescrow" {
					class = "netescrow-reconcile-overrun"
				}
				if len(result.alerts) != 1 {
					t.Fatal("live lifecycle failure changed retained completed evidence")
				}
				incident := requireAlertClass(t, result.alerts, class)
				if !strings.Contains(incident.Observed, "phase=completed") || !strings.Contains(incident.Observed, "log_source=unavailable") {
					t.Fatal("live lifecycle failure lost completed phase or coverage qualifier")
				}
			}
		}
	}
}
