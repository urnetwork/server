// Prospective local failure timing must not become remote response authority.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Read wire fields so the same controls compile against the prior producer.
func requireReconcileFailureReceiptClocks(t testing.TB, receipt logReconcileReceipt, service string, recorded, start time.Time) {
	t.Helper()
	encoded := encodeLogReconcileReceipt(receipt)
	if len(encoded) == 0 {
		t.Fatal("failure receipt was absent or exceeded its existing bound")
	}
	var wire struct {
		CollectorScopes []map[string]json.RawMessage `json:"collector_scopes"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal("failure receipt lost complete JSON framing")
	}
	for _, row := range wire.CollectorScopes {
		var key string
		if err := json.Unmarshal(row["service_key"], &key); err != nil || key != service {
			continue
		}
		for _, field := range []struct {
			name string
			want time.Time
		}{
			{name: "last_failure_monitor_recorded_at", want: recorded},
			{name: "last_failure_attempt_window_start", want: start},
		} {
			value, present := row[field.name]
			if !present {
				t.Fatalf("causal assertion: retained failure lacks prospective local timing field %s", field.name)
			}
			var got *time.Time
			if err := json.Unmarshal(value, &got); err != nil {
				t.Fatalf("failure timing field is not a nullable timestamp: %s", field.name)
			}
			if field.want.IsZero() {
				if got != nil {
					t.Fatalf("unavailable failure clock gained authority: %s", field.name)
				}
			} else if got == nil || !got.Equal(field.want) {
				t.Fatalf("stored failure clock changed or was lost: %s", field.name)
			}
		}
		return
	}
	t.Fatal("failure receipt lost its fixed public service row")
}

// Repeated real checks cannot turn one retained failure into newer attempts.
func TestReconcileFailureReceiptRecordsAttemptBeforeRepeatedChecks(t *testing.T) {
	now := time.Date(2032, 2, 3, 4, 5, 6, 123, time.UTC)
	tailer := syntheticReceiptTailer(&now, "api")
	start := now.Add(-logReconcileLookback)
	recorded := now.Add(7 * time.Second)
	until := recorded.Add(2 * time.Minute)
	queryError := &warpctlLogQueryError{class: observationErrorClassLogHttp429, retryAfter: until, err: errors.New("synthetic-rejection")}
	calls := 0
	tailer.reconcile = func(_ context.Context, actualStart time.Time, blocks []string) (string, error) {
		calls++
		if !actualStart.Equal(start) || len(blocks) != 0 {
			t.Fatal("failure timing changed the ordinary attempt window")
		}
		now = recorded
		return "", queryError
	}
	if advice := tailer.reconcileOnce(context.Background()); !advice.Equal(until) {
		t.Fatal("failure recording changed existing retry advice")
	}
	var diagnostics strings.Builder
	probe := &logTailProbe{tailers: []*logTailer{tailer}, reconcileDiagnostics: &diagnostics}
	for _, observed := range []time.Time{recorded.Add(time.Second), recorded.Add(3 * time.Minute)} {
		now = observed
		diagnostics.Reset()
		findings, err := probe.check(context.Background(), &probeEnv{now: fixedReconcileReceiptClock(observed)})
		if err != nil {
			t.Fatal(err)
		}
		receipt := parseSyntheticReconcileReceipt(t, diagnostics.String())
		requireReconcileFailureReceiptClocks(t, receipt, "api", recorded, start)
		scope := requireCollectorScope(t, receipt, "api")
		if !receipt.ObservedAt.Equal(observed) || receipt.Fresh != 0 || receipt.ConsecutiveTwo != 0 ||
			scope.CoverageState != "no-valid-window" || scope.LastFailurePresent == nil || !*scope.LastFailurePresent {
			t.Fatal("failure metadata changed existing missing-coverage semantics")
		}
		retained := false
		for _, finding := range findings {
			if finding.class == "tailer-reconcile" && strings.Contains(finding.observed, queryError.Error()) {
				retained = true
			}
		}
		if !retained || tailer.lastReconcileError != queryError.Error() {
			t.Fatal("prospective metadata suppressed or replaced the original failure")
		}
	}
	if calls != 1 {
		t.Fatal("diagnostic checks issued another query")
	}
	now = recorded.Add(4 * time.Minute)
	start = now.Add(-logReconcileLookback)
	recorded = now.Add(time.Second)
	tailer.reconcileOnce(context.Background())
	receipt := probe.reconcileReceipt(fixedReconcileReceiptClock(now))
	requireReconcileFailureReceiptClocks(t, receipt, "api", recorded, start)
	if calls != 2 {
		t.Fatal("a new failure did not belong to exactly one new logical attempt")
	}
}

// Recovery clears the retained failure without changing the two-success rule.
func TestReconcileFailureReceiptRecoveryStalenessAndGeneration(t *testing.T) {
	now := time.Date(2032, 2, 3, 4, 5, 6, 0, time.UTC)
	tailer := syntheticReceiptTailer(&now, "api")
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
		return "", errors.New("synthetic-rejection")
	}
	tailer.reconcileOnce(context.Background())
	probe := &logTailProbe{tailers: []*logTailer{tailer}}
	requireReconcileFailureReceiptClocks(t, probe.reconcileReceipt(fixedReconcileReceiptClock(now)), "api", now, now.Add(-logReconcileLookback))
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return "", nil }
	for attempt := 1; attempt <= 2; attempt++ {
		now = now.Add(logReconcileInterval)
		tailer.reconcileOnce(context.Background())
		receipt := probe.reconcileReceipt(fixedReconcileReceiptClock(now))
		requireReconcileFailureReceiptClocks(t, receipt, "api", time.Time{}, time.Time{})
		scope := requireCollectorScope(t, receipt, "api")
		if receipt.Fresh != 1 || receipt.ConsecutiveTwo != attempt-1 ||
			scope.LastFailurePresent == nil || *scope.LastFailurePresent || tailer.lastReconcileError != "" {
			t.Fatal("success changed the original advancing-window recovery rule")
		}
	}
	stale := probe.reconcileReceipt(fixedReconcileReceiptClock(now.Add(2 * logReconcileInterval)))
	requireReconcileFailureReceiptClocks(t, stale, "api", time.Time{}, time.Time{})
	if scope := requireCollectorScope(t, stale, "api"); scope.CoverageState != "stale-window" || stale.Fresh != 0 || stale.ConsecutiveTwo != 0 {
		t.Fatal("a stale successful window acquired failure or fresh-coverage authority")
	}
	replacement := &logTailProbe{tailers: []*logTailer{syntheticReceiptTailer(&now, "api")}}
	receipt := replacement.reconcileReceipt(fixedReconcileReceiptClock(now))
	requireReconcileFailureReceiptClocks(t, receipt, "api", time.Time{}, time.Time{})
	if receipt.Fresh != 0 || receipt.ConsecutiveTwo != 0 || *requireCollectorScope(t, receipt, "api").LastFailurePresent {
		t.Fatal("a new collector inherited historical failure or coverage authority")
	}
}

// Parent cancellation clears coverage and neither creates nor renews a failure.
func TestReconcileFailureReceiptCancellationKeepsExistingErrorState(t *testing.T) {
	for _, kind := range []string{"pre-canceled", "mid-canceled"} {
		for _, priorFailure := range []bool{false, true} {
			now := time.Date(2032, 2, 3, 4, 5, 6, 0, time.UTC)
			tailer := syntheticScopePair(now, "api")
			tailer.clock = func() time.Time { return now }
			var recorded, start time.Time
			if priorFailure {
				recorded, start = now, now.Add(-logReconcileLookback)
				tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
					return "", errors.New("synthetic-old-failure")
				}
				tailer.reconcileOnce(context.Background())
			}
			oldError := tailer.lastReconcileError
			now = now.Add(logReconcileInterval)
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
				calls++
				cancel()
				return "", errors.New("synthetic-canceled-return")
			}
			if kind == "pre-canceled" {
				cancel()
			}
			tailer.reconcileOnce(ctx)
			cancel()
			receipt := (&logTailProbe{tailers: []*logTailer{tailer}}).reconcileReceipt(fixedReconcileReceiptClock(now))
			requireReconcileFailureReceiptClocks(t, receipt, "api", recorded, start)
			scope := requireCollectorScope(t, receipt, "api")
			if tailer.lastReconcileError != oldError || scope.LastFailurePresent == nil || *scope.LastFailurePresent != priorFailure ||
				scope.CoverageState != "no-valid-window" || receipt.Fresh != 0 || receipt.ConsecutiveTwo != 0 {
				t.Fatalf("%s changed retained failure or cancellation semantics", kind)
			}
			if kind == "pre-canceled" && calls != 0 || kind == "mid-canceled" && calls != 1 {
				t.Fatalf("%s changed query admission", kind)
			}
		}
	}
}

// Timeout, incomplete output and later-page failures retain the logical bound.
func TestReconcileFailureReceiptRetainsLogicalAttemptBoundary(t *testing.T) {
	for _, kind := range []string{"timeout", "incomplete", "continuation", "block"} {
		now := time.Date(2032, 2, 3, 4, 5, 6, 0, time.UTC)
		start := now.Add(-logReconcileLookback)
		boundary := now.Add(-time.Minute)
		recorded := now.Add(3 * time.Second)
		tailer := syntheticReceiptTailer(&now, "api")
		if kind == "block" {
			tailer.blocks = []string{"synthetic-block-a", "synthetic-block-b"}
		}
		saturated := ""
		if kind == "continuation" || kind == "block" {
			saturated = strings.Repeat("[synthetic-block]["+boundary.Format(time.RFC3339Nano)+"]ordinary synthetic record\n", logReconcileLimit)
		}
		calls := 0
		var failedPageStart time.Time
		tailer.reconcile = func(_ context.Context, pageStart time.Time, blocks []string) (string, error) {
			calls++
			if saturated != "" && calls == 1 {
				return saturated, nil
			}
			failedPageStart = pageStart
			now = recorded
			switch kind {
			case "timeout":
				return "", context.DeadlineExceeded
			case "incomplete":
				return "Warning: at least 2 entries at synthetic timestamp. The range api cannot page within one nanosecond; skipping the rest of this timestamp.\n", nil
			default:
				queryError := &warpctlLogQueryError{class: observationErrorClassLogHttp429, err: errors.New("synthetic-rejection")}
				return "", fmt.Errorf("synthetic-page-wrapper: %w", queryError)
			}
		}
		ctx := context.Background()
		tailer.reconcileOnce(ctx)
		receipt := (&logTailProbe{tailers: []*logTailer{tailer}}).reconcileReceipt(fixedReconcileReceiptClock(now))
		requireReconcileFailureReceiptClocks(t, receipt, "api", recorded, start)
		if ctx.Err() != nil || tailer.lastReconcileError == "" || receipt.Fresh != 0 || receipt.ConsecutiveTwo != 0 {
			t.Fatalf("%s lost the non-canceled failed attempt", kind)
		}
		if kind == "continuation" && (!failedPageStart.Equal(boundary) || !failedPageStart.After(start)) {
			t.Fatal("continuation fixture did not distinguish the failed page from the logical window")
		}
		wantCalls := 1
		if kind == "continuation" || kind == "block" {
			wantCalls = 2
		}
		if calls != wantCalls {
			t.Fatalf("%s changed the existing query/page budget", kind)
		}
	}
}

// Missing or incoherent local clocks stay visible without a fabricated fallback.
func TestReconcileFailureReceiptUnavailableAndIncoherentClocks(t *testing.T) {
	observed := time.Date(2032, 2, 3, 4, 5, 6, 0, time.UTC)
	start := observed.Add(-logReconcileLookback)
	for _, test := range []struct {
		name     string
		start    time.Time
		recorded time.Time
		noClock  bool
	}{
		{name: "unavailable-clock", start: start, noClock: true},
		{name: "zero-clock", start: start},
		{name: "missing-lower-bound", recorded: observed},
		{name: "both-unavailable", noClock: true},
		{name: "backward-clock", start: start, recorded: start.Add(-time.Nanosecond)},
		{name: "future-recording", start: start, recorded: observed.Add(time.Nanosecond)},
	} {
		tailer := syntheticReceiptTailer(&observed, "api")
		tailer.clock = nil
		if !test.noClock {
			tailer.clock = func() time.Time { return test.recorded }
		}
		tailer.recordReconcile(test.start, errors.New("synthetic-rejection"))
		receipt := (&logTailProbe{tailers: []*logTailer{tailer}}).reconcileReceipt(fixedReconcileReceiptClock(observed))
		requireReconcileFailureReceiptClocks(t, receipt, "api", test.recorded, test.start)
		scope := requireCollectorScope(t, receipt, "api")
		if scope.CoverageState != "no-valid-window" || scope.LastFailurePresent == nil || !*scope.LastFailurePresent || receipt.Fresh != 0 || receipt.ConsecutiveTwo != 0 {
			t.Fatalf("%s changed coverage predicates instead of preserving unavailable timing authority", test.name)
		}
	}
}

// A failure published after all copies belongs only in the following receipt.
func TestReconcileFailureReceiptSnapshotExcludesLaterFailure(t *testing.T) {
	initial := time.Date(2032, 2, 3, 4, 5, 6, 0, time.UTC)
	oldRecorded := initial.Add(time.Second)
	newRecorded := oldRecorded.Add(logReconcileInterval)
	observed := newRecorded.Add(time.Millisecond)
	tailers := []*logTailer{syntheticReceiptTailer(&initial, "api"), syntheticReceiptTailer(&initial, "proxy")}
	for _, tailer := range tailers {
		tailer.clock = func() time.Time { return oldRecorded }
		tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
			return "", errors.New("synthetic-rejection")
		}
		tailer.reconcileOnce(context.Background())
		tailer.clock = func() time.Time { return newRecorded }
	}
	barrier := make(chan struct{})
	published := startReconcileReceiptPublisher(t, barrier, func() {
		for _, tailer := range tailers {
			tailer.reconcileOnce(context.Background())
		}
	})
	clockCalls := 0
	env := &probeEnv{now: func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return oldRecorded
		}
		if clockCalls == 2 {
			close(barrier)
			awaitReconcileReceiptPublisher(t, published)
		}
		return observed
	}}
	var diagnostics strings.Builder
	probe := &logTailProbe{tailers: tailers, reconcileDiagnostics: &diagnostics}
	if _, err := probe.check(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	first := parseSyntheticReconcileReceipt(t, diagnostics.String())
	if clockCalls != 2 || !first.ObservedAt.Equal(observed) {
		t.Fatal("failure receipt changed the existing observation-clock ordering")
	}
	for _, service := range []string{"api", "proxy"} {
		requireReconcileFailureReceiptClocks(t, first, service, oldRecorded, oldRecorded.Add(-logReconcileLookback))
	}
	diagnostics.Reset()
	if _, err := probe.check(context.Background(), &probeEnv{now: fixedReconcileReceiptClock(observed)}); err != nil {
		t.Fatal(err)
	}
	next := parseSyntheticReconcileReceipt(t, diagnostics.String())
	for _, service := range []string{"api", "proxy"} {
		requireReconcileFailureReceiptClocks(t, next, service, newRecorded, newRecorded.Add(-logReconcileLookback))
		requireReconcileFailureReceiptClocks(t, first, service, oldRecorded, oldRecorded.Add(-logReconcileLookback))
	}
}

// Fixed rows remain bounded; ambiguous names select no collector's failure.
func TestReconcileFailureReceiptBoundAndAmbiguousPrivacy(t *testing.T) {
	now := time.Date(2032, 2, 3, 4, 5, 6, 999999999, time.UTC)
	var tailers []*logTailer
	for _, service := range logReconcileServiceKeys {
		tailers = append(tailers, syntheticScopePair(now, service))
	}
	probe := &logTailProbe{tailers: tailers}
	healthy := probe.reconcileReceipt(fixedReconcileReceiptClock(now))
	if len(encodeLogReconcileReceipt(healthy)) == 0 || healthy.Fresh != 10 || healthy.ConsecutiveTwo != 10 {
		t.Fatal("additive null fields exceeded the unchanged ten-collector receipt bound")
	}
	for _, tailer := range tailers {
		tailer.recordReconcile(now.Add(-logReconcileLookback), errors.New("synthetic-rejection"))
	}
	failed := probe.reconcileReceipt(fixedReconcileReceiptClock(now))
	for _, service := range logReconcileServiceKeys {
		requireReconcileFailureReceiptClocks(t, failed, service, now, now.Add(-logReconcileLookback))
	}
	if failed.Fresh != 0 || failed.ConsecutiveTwo != 0 || !failed.ScopeProjectionComplete {
		t.Fatal("failure timing altered collector inventory or coverage")
	}
	unsupported := syntheticReceiptTailer(&now, "synthetic-private-service")
	unsupported.recordReconcile(now.Add(-logReconcileLookback), errors.New("synthetic-private-response"))
	probe.tailers = append(probe.tailers, tailers[1], unsupported)
	ambiguous := probe.reconcileReceipt(fixedReconcileReceiptClock(now))
	requireReconcileFailureReceiptClocks(t, ambiguous, "api", time.Time{}, time.Time{})
	scope := requireCollectorScope(t, ambiguous, "api")
	if ambiguous.ScopeProjectionComplete || ambiguous.UnsupportedCollectors != 1 || ambiguous.DuplicateServiceKeys != 1 ||
		scope.CoverageState != "ambiguous" || scope.LastFailurePresent != nil {
		t.Fatal("duplicate service selected one failure snapshot")
	}
	encoded := string(encodeLogReconcileReceipt(ambiguous))
	for _, forbidden := range []string{"synthetic-private-service", "synthetic-private-response", "synthetic-rejection", "http-429"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatal("failure timing exposed private values or invented an HTTP cause")
		}
	}
}
