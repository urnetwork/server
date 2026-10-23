// Public continuity calls must not let rejected late observations seed or
// erase the comparators used by later live calls on the same signal.
package monitor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Preserve the real invocation context at the public synthetic source seam.
type mimirContinuityHistorySource struct {
	*syntheticSource
	hostContextFn func(context.Context, HostSettings, string) (string, error)
}

// Forward the actual caller context without a replacement or detached owner.
func (self *mimirContinuityHistorySource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	return self.hostContextFn(ctx, host, command)
}

// Snapshot only after a public call has returned, preserving nil-map identity.
func mimirContinuityOwnedHistory(signal Signal) map[int64]mimirContinuityGapHistory {
	probe := signal.(*signalAdapter).probe.(*mimirContinuityProbe)
	probe.historyLock.Lock()
	defer probe.historyLock.Unlock()
	if probe.history == nil {
		return nil
	}
	history := make(map[int64]mimirContinuityGapHistory, len(probe.history))
	for key, value := range probe.history {
		history[key] = value
	}
	return history
}

// Run a single gateway callback through the actual public signal adapter.
func runMimirContinuityHistoryCall(t *testing.T, signal Signal, now time.Time, ctx context.Context, payload string, sourceErr error, beforeReturn func(context.Context), scoped bool) (Alerts, error) {
	t.Helper()
	calls := 0
	source := &mimirContinuityHistorySource{
		syntheticSource: &syntheticSource{},
		hostContextFn: func(callCtx context.Context, host HostSettings, command string) (string, error) {
			calls++
			if host.Name != "continuity-history.example" || !strings.Contains(command, mimirContinuityMarker) {
				t.Error("continuity call missed its synthetic gateway or owning command")
			}
			if callCtx.Done() != ctx.Done() {
				t.Error("source callback lost the actual parent cancellation channel")
			}
			if beforeReturn != nil {
				beforeReturn(callCtx)
			}
			return payload, sourceErr
		},
	}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{
		{Name: "continuity-history.example", Roles: []string{"services"}},
		{Name: "excluded-history.example"},
	}
	if scoped {
		settings.ExcludedHosts = []string{"excluded-history.example"}
	}
	alerts, err := signal.Run(ctx, settings)
	if calls != 1 {
		t.Fatalf("public continuity source callbacks = %d, want one", calls)
	}
	return alerts, err
}

// Cancel the actual live parent inside the callback, then return late data.
func runMimirContinuityHistoryCanceled(t *testing.T, signal Signal, now time.Time, payload string, sourceErr error, scoped bool) error {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("synthetic continuity owner ended")
	alerts, err := runMimirContinuityHistoryCall(t, signal, now, ctx, payload, sourceErr, func(callCtx context.Context) {
		if callCtx.Err() != nil {
			t.Fatal("source callback did not enter with a live parent")
		}
		cancel(cause)
		if callCtx.Err() != context.Canceled || context.Cause(callCtx) != cause {
			t.Fatal("actual parent cancellation was not established before source return")
		}
	}, scoped)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || context.Cause(ctx) != cause {
		t.Fatalf("late canceled public result lost lifecycle authority: alerts=%d err=%v", len(alerts), err)
	}
	return err
}

// Establish accepted restoration history through two real public calls.
func seedMimirContinuityOwnedRecovery(t *testing.T, signal Signal, now time.Time) (time.Time, time.Time) {
	t.Helper()
	firstStart, missingEnd := now.Add(-4*time.Hour), now.Add(-2*time.Hour)
	first, err, _ := runMimirContinuitySyntheticAt(t, signal, now,
		mimirContinuityTimes(now.Add(-mimirContinuityWindow), now, mimirContinuityOmitRange(firstStart, missingEnd)), nil)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, first, "mimir-continuity-gap-unclassified")
	later := now.Add(mimirContinuityStep)
	advanced := firstStart.Add(2 * mimirContinuityStep)
	second, err, _ := runMimirContinuitySyntheticAt(t, signal, later,
		mimirContinuityTimes(later.Add(-mimirContinuityWindow), later, mimirContinuityOmitRange(advanced, missingEnd)), nil)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, second, "mimir-query-store-visibility-gap")
	return advanced, missingEnd
}

// The first accepted gap stays unclassified even after a rejected first gap.
func TestMimirContinuityCanceledFirstGapCannotSeedFutureLoss(t *testing.T) {
	now := time.Date(2097, 2, 3, 12, 0, 0, 0, time.UTC)
	start, end := now.Add(-30*time.Hour), now.Add(-24*time.Hour)
	signal := NewMimirContinuitySignal()
	before := mimirContinuityOwnedHistory(signal)
	payload := mimirContinuityFixture(t, mimirContinuityTimes(now.Add(-mimirContinuityWindow), now, mimirContinuityOmitRange(start, end)))
	runMimirContinuityHistoryCanceled(t, signal, now, payload, nil, false)
	after := mimirContinuityOwnedHistory(signal)
	liveAt := now.Add(mimirContinuityStep)
	timestamps := mimirContinuityTimes(liveAt.Add(-mimirContinuityWindow), liveAt, mimirContinuityOmitRange(start, end))
	alerts, err, _ := runMimirContinuitySyntheticAt(t, signal, liveAt, timestamps, nil)
	if err != nil {
		t.Fatal(err)
	}
	unclassified := false
	for _, alert := range alerts {
		unclassified = unclassified || alert.Class == "mimir-continuity-gap-unclassified"
	}
	if !unclassified {
		t.Fatal("canceled first gap changed future live classification")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("canceled first gap mutated continuity history")
	}
	baseline, err, _ := runMimirContinuitySyntheticAt(t, NewMimirContinuitySignal(), liveAt, timestamps, nil)
	if err != nil {
		t.Fatal(err)
	}
	if requireAlertClass(t, alerts, "mimir-continuity-gap-unclassified").Observed != requireAlertClass(t, baseline, "mimir-continuity-gap-unclassified").Observed {
		t.Fatal("first accepted observation differs from live-only baseline")
	}
	secondAt := liveAt.Add(mimirContinuityStep)
	second, err, _ := runMimirContinuitySyntheticAt(t, signal, secondAt,
		mimirContinuityTimes(secondAt.Add(-mimirContinuityWindow), secondAt, mimirContinuityOmitRange(start, end)), nil)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, second, "mimir-ingestion-gap")
}

// A rejected healthy result cannot erase previously accepted restoration.
func TestMimirContinuityCanceledHealthyCannotEraseFutureRecovery(t *testing.T) {
	now := time.Date(2097, 3, 4, 12, 0, 0, 0, time.UTC)
	signal, baseline := NewMimirContinuitySignal(), NewMimirContinuitySignal()
	start, end := seedMimirContinuityOwnedRecovery(t, signal, now)
	seedMimirContinuityOwnedRecovery(t, baseline, now)
	before := mimirContinuityOwnedHistory(signal)
	cancelAt := now.Add(2 * mimirContinuityStep)
	runMimirContinuityHistoryCanceled(t, signal, cancelAt,
		mimirContinuityFixture(t, mimirContinuityTimes(cancelAt.Add(-mimirContinuityWindow), cancelAt, nil)), nil, false)
	after := mimirContinuityOwnedHistory(signal)
	liveAt := now.Add(3 * mimirContinuityStep)
	timestamps := mimirContinuityTimes(liveAt.Add(-mimirContinuityWindow), liveAt, mimirContinuityOmitRange(start, end))
	alerts, err, _ := runMimirContinuitySyntheticAt(t, signal, liveAt, timestamps, nil)
	if err != nil {
		t.Fatal(err)
	}
	recovering := false
	for _, alert := range alerts {
		recovering = recovering || alert.Class == "mimir-query-store-visibility-gap"
	}
	if !recovering {
		t.Fatal("canceled healthy response erased future recovery comparator")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("canceled healthy result mutated continuity history")
	}
	control, err, _ := runMimirContinuitySyntheticAt(t, baseline, liveAt, timestamps, nil)
	if err != nil {
		t.Fatal(err)
	}
	if requireAlertClass(t, alerts, "mimir-query-store-visibility-gap").Observed != requireAlertClass(t, control, "mimir-query-store-visibility-gap").Observed {
		t.Fatal("future recovery differs from live-only baseline")
	}
}

// Rejected movement must not refresh historical restoration quantities.
func TestMimirContinuityCanceledAdvanceKeepsRestorationHistory(t *testing.T) {
	now := time.Date(2097, 4, 5, 12, 0, 0, 0, time.UTC)
	signal := NewMimirContinuitySignal()
	start, end := seedMimirContinuityOwnedRecovery(t, signal, now)
	before := mimirContinuityOwnedHistory(signal)
	cancelAt := now.Add(2 * mimirContinuityStep)
	payload := mimirContinuityFixture(t, mimirContinuityTimes(cancelAt.Add(-mimirContinuityWindow), cancelAt,
		mimirContinuityOmitRange(start.Add(2*mimirContinuityStep), end)))
	runMimirContinuityHistoryCanceled(t, signal, cancelAt, payload, nil, false)
	if !reflect.DeepEqual(before, mimirContinuityOwnedHistory(signal)) {
		t.Fatal("canceled advance refreshed continuity restoration history")
	}
}

// Rejected regressions and changed identities cannot replace live history.
func TestMimirContinuityCanceledIdentityChangesKeepHistory(t *testing.T) {
	for _, change := range []struct {
		name       string
		startDelta time.Duration
		endDelta   time.Duration
	}{
		{name: "left regression", startDelta: -mimirContinuityStep},
		{name: "right edge change", endDelta: mimirContinuityStep},
	} {
		now := time.Date(2097, 5, 6, 12, 0, 0, 0, time.UTC)
		signal := NewMimirContinuitySignal()
		start, end := seedMimirContinuityOwnedRecovery(t, signal, now)
		before := mimirContinuityOwnedHistory(signal)
		cancelAt := now.Add(2 * mimirContinuityStep)
		payload := mimirContinuityFixture(t, mimirContinuityTimes(cancelAt.Add(-mimirContinuityWindow), cancelAt,
			mimirContinuityOmitRange(start.Add(change.startDelta), end.Add(change.endDelta))))
		runMimirContinuityHistoryCanceled(t, signal, cancelAt, payload, nil, false)
		if !reflect.DeepEqual(before, mimirContinuityOwnedHistory(signal)) {
			t.Fatalf("canceled %s replaced continuity history", change.name)
		}
	}
}

// Existing source and parse refusal must retain history and source privacy.
func TestMimirContinuitySourceRefusalKeepsHistoryAndCauses(t *testing.T) {
	now := time.Date(2097, 6, 7, 12, 0, 0, 0, time.UTC)
	signal := NewMimirContinuitySignal()
	seedMimirContinuityOwnedRecovery(t, signal, now)
	before := mimirContinuityOwnedHistory(signal)
	privateCause := errors.New("synthetic-private-source-cause")
	err := runMimirContinuityHistoryCanceled(t, signal, now.Add(2*mimirContinuityStep), "", privateCause, false)
	if !errors.Is(err, privateCause) || strings.Contains(err.Error(), privateCause.Error()) {
		t.Fatal("canceled source refusal lost private cause or exposed source text")
	}
	for _, refusal := range []struct {
		payload string
		err     error
	}{
		{err: privateCause},
		{payload: `{"status":"success","data":{"resultType":"matrix","result":[]}}`},
	} {
		alerts, err := runMimirContinuityHistoryCall(t, signal, now.Add(3*mimirContinuityStep), context.Background(), refusal.payload, refusal.err, nil, false)
		if err == nil || len(alerts) != 0 || !reflect.DeepEqual(before, mimirContinuityOwnedHistory(signal)) {
			t.Fatal("live source or parse refusal changed history or became healthy")
		}
	}
}

// Unrelated signal owners keep their own accepted comparator and restoration.
func TestMimirContinuityCanceledOwnerKeepsIndependentHistory(t *testing.T) {
	now := time.Date(2097, 7, 8, 12, 0, 0, 0, time.UTC)
	canceled, sibling := NewMimirContinuitySignal(), NewMimirContinuitySignal()
	seedMimirContinuityOwnedRecovery(t, canceled, now)
	start, end := seedMimirContinuityOwnedRecovery(t, sibling, now)
	before := mimirContinuityOwnedHistory(sibling)
	cancelAt := now.Add(2 * mimirContinuityStep)
	runMimirContinuityHistoryCanceled(t, canceled, cancelAt,
		mimirContinuityFixture(t, mimirContinuityTimes(cancelAt.Add(-mimirContinuityWindow), cancelAt, nil)), nil, false)
	if !reflect.DeepEqual(before, mimirContinuityOwnedHistory(sibling)) {
		t.Fatal("canceling one owner changed independent continuity history")
	}
	alerts, err, _ := runMimirContinuitySyntheticAt(t, sibling, cancelAt,
		mimirContinuityTimes(cancelAt.Add(-mimirContinuityWindow), cancelAt, mimirContinuityOmitRange(start, end)), nil)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "mimir-query-store-visibility-gap")
}

// Scoped public cancellation has the same probe-local history ownership.
func TestMimirContinuityScopedCanceledHealthyKeepsHistory(t *testing.T) {
	now := time.Date(2097, 8, 9, 12, 0, 0, 0, time.UTC)
	signal := NewMimirContinuitySignal()
	seedMimirContinuityOwnedRecovery(t, signal, now)
	before := mimirContinuityOwnedHistory(signal)
	cancelAt := now.Add(2 * mimirContinuityStep)
	runMimirContinuityHistoryCanceled(t, signal, cancelAt,
		mimirContinuityFixture(t, mimirContinuityTimes(cancelAt.Add(-mimirContinuityWindow), cancelAt, nil)), nil, true)
	if !reflect.DeepEqual(before, mimirContinuityOwnedHistory(signal)) {
		t.Fatal("scoped canceled healthy result erased probe history")
	}
}

// Caller Context methods may inspect this probe without running under its lock.
type mimirContinuityHistoryContext struct {
	context.Context
	probe                  *mimirContinuityProbe
	historyLockUnavailable bool
}

// Record lock ownership without waiting or taking a negative-timeout shortcut.
func (self *mimirContinuityHistoryContext) Done() <-chan struct{} {
	if self.probe.historyLock.TryLock() {
		self.probe.historyLock.Unlock()
	} else {
		self.historyLockUnavailable = true
	}
	return self.Context.Done()
}

// Capturing the cancellation channel must not invoke caller code under historyLock.
func TestMimirContinuityContextCallbacksStayOutsideHistoryLock(t *testing.T) {
	now := time.Date(2097, 9, 10, 12, 0, 0, 0, time.UTC)
	signal := NewMimirContinuitySignal()
	ctx := &mimirContinuityHistoryContext{Context: context.Background(), probe: signal.(*signalAdapter).probe.(*mimirContinuityProbe)}
	payload := mimirContinuityFixture(t, mimirContinuityTimes(now.Add(-mimirContinuityWindow), now,
		mimirContinuityOmitRange(now.Add(-4*time.Hour), now.Add(-2*time.Hour))))
	alerts, err := runMimirContinuityHistoryCall(t, signal, now, ctx, payload, nil, nil, false)
	if err != nil || ctx.historyLockUnavailable {
		t.Fatal("continuity invoked caller context code under history lock or rejected live data")
	}
	requireAlertClass(t, alerts, "mimir-continuity-gap-unclassified")
}

// A probe-local history owner also refuses an already-ended successful source.
func TestMimirContinuityEndedParentCannotCommitHistory(t *testing.T) {
	now := time.Date(2097, 10, 11, 12, 0, 0, 0, time.UTC)
	signal := NewMimirContinuitySignal()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	payload := mimirContinuityFixture(t, mimirContinuityTimes(now.Add(-mimirContinuityWindow), now,
		mimirContinuityOmitRange(now.Add(-4*time.Hour), now.Add(-2*time.Hour))))
	alerts, err := runMimirContinuityHistoryCall(t, signal, now, ctx, payload, nil, nil, false)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || mimirContinuityOwnedHistory(signal) != nil {
		t.Fatal("ended continuity owner accepted a source result or committed history")
	}
}
