package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Decode only the independently framed activity line, as a retained reader would.
func readReconcileActivity(t *testing.T, raw string) logReconcileActivityReceipt {
	t.Helper()
	line := strings.TrimSpace(raw)
	if !strings.HasPrefix(line, logReconcileActivityPrefix) || strings.Contains(line, "\n") ||
		len(raw) > logReconcileActivityMaxBytes {
		t.Fatal("activity diagnostic lost its independent single-line bound")
	}
	var receipt logReconcileActivityReceipt
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(line, logReconcileActivityPrefix)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil || receipt.Schema != 1 {
		t.Fatal("activity diagnostic lost its versioned shape")
	}
	return receipt
}

// The first page may block; returning a full page advances activity and
// starts another read without confirming coverage. All transitions use channels.
func TestReconcileActivitySeparatesIdleBlockedAndPageProgress(t *testing.T) {
	start := time.Date(2026, 11, 18, 16, 55, 50, 123, time.UTC)
	historical := start.Add(-3 * time.Minute)
	tailer := syntheticScopePair(historical, "proxy")
	var nanos atomic.Int64
	nanos.Store(start.UnixNano())
	clock := func() time.Time { return time.Unix(0, nanos.Load()).UTC() }
	tailer.clock = clock
	boundary := start.Add(-time.Minute).Truncate(time.Microsecond)
	line := "[private-block][" + boundary.Format(time.RFC3339Nano) + "]private-token\n"
	full := strings.Repeat(line, logReconcileLimit)
	type page struct {
		number       int
		lower, upper time.Time
	}
	entered := make(chan page)
	release := make(chan struct{})
	ordinal := 0
	tailer.reconcile = func(ctx context.Context, lower, upper time.Time, _ []string) (string, error) {
		ordinal++
		select {
		case entered <- page{number: ordinal, lower: lower, upper: upper}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
		}
		if ordinal == 1 {
			return full, nil
		}
		return "", nil
	}
	var coverageOut, activityOut strings.Builder
	probe := &logTailProbe{tailers: []*logTailer{tailer}, reconcileDiagnostics: &coverageOut, activityDiagnostics: &activityOut}
	observe := func() (string, logReconcileCollectorScope, logReconcileActivityScope) {
		coverageOut.Reset()
		activityOut.Reset()
		probe.writeReconcileReceipt(clock)
		coverage := parseSyntheticReconcileReceipt(t, coverageOut.String())
		activity := readReconcileActivity(t, activityOut.String())
		if len(activity.CollectorScopes) != 1 || !activity.ObservedAt.Equal(coverage.ObservedAt) ||
			!activity.ScopeProjectionComplete || strings.Contains(activityOut.String(), "private") {
			t.Fatal("activity lost shared observation or exposed query data")
		}
		return coverageOut.String(), coverage.CollectorScopes[0], activity.CollectorScopes[0]
	}
	initial, _, idle := observe()
	if idle.State != "idle" || idle.AttemptNumber != nil || idle.PagesCompleted != nil || idle.LastPageAt != nil {
		t.Fatal("idle collector invented an attempt")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tailer.reconcileOnce(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("fixture collector failed to stop")
		}
	})
	waitPage := func(number int, lower time.Time) {
		t.Helper()
		select {
		case got := <-entered:
			if got.number != number || !got.lower.Equal(lower) || !got.upper.Equal(start) {
				t.Fatal("page order or fixed source bounds disagreed with the active attempt")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("fixture page was never entered")
		}
	}
	waitPage(1, start.Add(-logReconcileLookback))
	encoded, coverage, current := observe()
	if initial != encoded || coverage.CoverageState != "stale-window" || current.State != "active" ||
		current.AttemptNumber == nil || *current.AttemptNumber != 1 || current.AttemptStartedAt == nil || !current.AttemptStartedAt.Equal(start) ||
		current.PagesCompleted == nil || *current.PagesCompleted != 0 || current.LastPageAt != nil {
		t.Fatal("blocked first page changed coverage or fabricated a completed page")
	}
	progressAt := start.Add(7 * time.Second)
	nanos.Store(progressAt.UnixNano())
	release <- struct{}{}
	waitPage(2, boundary)
	for _, at := range []time.Time{progressAt, start.Add(12 * time.Second)} {
		nanos.Store(at.UnixNano())
		_, coverage, current = observe()
		if coverage.CoverageState != "stale-window" || current.State != "active" ||
			current.AttemptNumber == nil || *current.AttemptNumber != 1 || current.PagesCompleted == nil || *current.PagesCompleted != 1 ||
			current.LastPageAt == nil || !current.LastPageAt.Equal(progressAt) {
			t.Fatal("a repeated observation invented progress or a saturated page cleared coverage")
		}
	}
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("finite query did not finish")
	}
	_, coverage, current = observe()
	if coverage.CoverageState != "fresh-one-window" || current.State != "idle" || current.AttemptStartedAt != nil || current.LastPageAt != nil {
		t.Fatal("terminal success did not atomically clear the active attempt")
	}
}

// Cancellation and terminal failure preserve their original coverage outcomes
// and release the activity slot. A skipped cooldown tick starts no attempt.
func TestReconcileActivityFailureCancellationAndCooldown(t *testing.T) {
	now := time.Date(2026, 11, 18, 17, 0, 0, 0, time.UTC)
	tailer := syntheticReceiptTailer(&now, "proxy")
	probe := &logTailProbe{tailers: []*logTailer{tailer}}
	for index, cancelPage := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		tailer.reconcile = func(context.Context, time.Time, time.Time, []string) (string, error) {
			snapshots := probe.reconcileSnapshots()
			coverage := newLogReconcileReceipt(snapshots, now)
			row := newLogReconcileActivityReceipt(snapshots, coverage).CollectorScopes[0]
			if row.State != "active" || row.AttemptNumber == nil || *row.AttemptNumber != uint64(index+1) {
				t.Fatal("logical attempt identity changed inside the page")
			}
			if cancelPage {
				cancel()
			}
			return "", errors.New("private-error")
		}
		tailer.reconcileOnce(ctx)
		cancel()
		snapshots := probe.reconcileSnapshots()
		coverage := newLogReconcileReceipt(snapshots, now)
		row := newLogReconcileActivityReceipt(snapshots, coverage).CollectorScopes[0]
		if row.State != "idle" || row.AttemptNumber != nil || row.PagesCompleted != nil ||
			coverage.CollectorScopes[0].CoverageState != "no-valid-window" || !snapshots[0].failed {
			t.Fatal("terminal cleanup retained activity or cleared the prior real failure")
		}
	}
	advice := now.Add(time.Minute)
	if got := tailer.reconcileTick(context.Background(), advice); !got.Equal(advice) || tailer.reconcileAttempt.number != 2 || tailer.reconcileAttempt.active {
		t.Fatal("cooldown invented a logical attempt")
	}
}

// Exercise all fixed keys at maximum numeric/clock widths and demonstrate
// that duplicate or unsupported keys cannot select an arbitrary private row.
func TestReconcileActivityBoundAndProjection(t *testing.T) {
	now := time.Date(2032, 11, 18, 17, 0, 59, 999999999, time.UTC)
	probe := &logTailProbe{}
	for _, service := range logReconcileServiceKeys {
		tailer := syntheticScopePair(now.Add(-time.Minute), service)
		tailer.reconcileAttempt = logReconcileAttempt{active: true, number: ^uint64(0), pagesCompleted: ^uint64(0),
			startedAt: now.Add(-time.Second), lastPageAt: now}
		probe.tailers = append(probe.tailers, tailer)
	}
	var out strings.Builder
	probe.activityDiagnostics = &out
	probe.writeReconcileReceipt(fixedReconcileReceiptClock(now))
	full := readReconcileActivity(t, out.String())
	if len(full.CollectorScopes) != 11 || !full.ScopeProjectionComplete {
		t.Fatal("maximum-width activity receipt lost a scope")
	}
	for _, row := range full.CollectorScopes {
		if row.State != "active" || row.PagesCompleted == nil || *row.PagesCompleted != ^uint64(0) {
			t.Fatal("bounded activity lost a counter")
		}
	}
	probe.tailers = append(probe.tailers, syntheticReceiptTailer(&now, "proxy"), syntheticReceiptTailer(&now, "private-unused"))
	out.Reset()
	probe.writeReconcileReceipt(fixedReconcileReceiptClock(now))
	ambiguous := readReconcileActivity(t, out.String())
	if ambiguous.UnsupportedCollectors != 1 || ambiguous.DuplicateServiceKeys != 1 || ambiguous.ScopeProjectionComplete || strings.Contains(out.String(), "private") {
		t.Fatal("unsupported or duplicate scope was admitted")
	}
	for _, row := range ambiguous.CollectorScopes {
		if row.ServiceKey == "proxy" && (row.State != "ambiguous" || row.CollectorCount != 2 || row.AttemptNumber != nil || row.CollectorStartedAt != nil) {
			t.Fatal("duplicate key selected an arbitrary collector")
		}
	}
}

// An incoherent local clock stays visible and cannot certify a progress
// timestamp. A serialization failure in either line leaves the other available.
func TestReconcileActivityClockAndIndependentEncoding(t *testing.T) {
	now := time.Date(2032, 11, 18, 17, 0, 59, 999999999, time.UTC)
	tailer := syntheticScopePair(now.Add(-time.Minute), "proxy")
	var coverage, activity strings.Builder
	probe := &logTailProbe{tailers: []*logTailer{tailer}, reconcileDiagnostics: &coverage, activityDiagnostics: &activity}
	for _, first := range []time.Time{now.Add(-time.Nanosecond), now.Add(time.Second)} {
		attempt := logReconcileAttempt{active: true, number: 1, startedAt: now}
		attempt.recordPage(first)
		attempt.recordPage(now)
		if !attempt.clockRegressed || attempt.validAt(&tailer.startedAt, now) {
			t.Fatal("a later page erased a backwards clock")
		}
	}
	for _, attempt := range []logReconcileAttempt{
		{active: true, number: 1, startedAt: now.Add(time.Nanosecond)},
		{active: true, number: 1, startedAt: now.Add(-2 * time.Minute)},
		{active: true, number: 1, startedAt: now, pagesCompleted: 1},
		{active: true, number: 1, startedAt: now, lastPageAt: now},
		{active: true, number: 1, startedAt: now, lastPageAt: now.Add(-time.Nanosecond), pagesCompleted: 1},
		{active: true, number: 1, startedAt: now, lastPageAt: now.Add(time.Nanosecond), pagesCompleted: 1},
		{active: true, number: 1, startedAt: now, lastPageAt: now, pagesCompleted: 2, clockRegressed: true},
	} {
		tailer.reconcileAttempt = attempt
		coverage.Reset()
		activity.Reset()
		probe.writeReconcileReceipt(fixedReconcileReceiptClock(now))
		if readReconcileActivity(t, activity.String()).CollectorScopes[0].State != "invalid-time" ||
			parseSyntheticReconcileReceipt(t, coverage.String()).Fresh != 1 {
			t.Fatal("invalid activity clock acquired authority or changed the coverage line")
		}
	}
	unencodable := time.Date(10_000, 1, 1, 0, 0, 0, 0, time.UTC)
	tailer.reconcileAttempt.startedAt = unencodable
	coverage.Reset()
	activity.Reset()
	probe.writeReconcileReceipt(fixedReconcileReceiptClock(now))
	if activity.Len() != 0 || parseSyntheticReconcileReceipt(t, coverage.String()).Fresh != 1 {
		t.Fatal("activity encoding failure dropped the coverage line")
	}
	tailer.reconcileAttempt.finish()
	tailer.lastReconcileTime = unencodable
	coverage.Reset()
	activity.Reset()
	probe.writeReconcileReceipt(fixedReconcileReceiptClock(now))
	if coverage.Len() != 0 || readReconcileActivity(t, activity.String()).CollectorScopes[0].State != "idle" {
		t.Fatal("coverage encoding failure dropped the activity line")
	}
}
