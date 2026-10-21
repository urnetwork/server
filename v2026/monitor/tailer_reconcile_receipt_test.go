package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// A fixed projection time for existing source-window fixtures.
func fixedReconcileReceiptClock(now time.Time) func() time.Time {
	return func() time.Time { return now }
}

func syntheticReceiptTailer(now *time.Time, service string) *logTailer {
	tailer := newLogTailer(service, &probeEnv{now: func() time.Time { return *now }})
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return "", nil }
	return tailer
}

func parseSyntheticReconcileReceipt(t *testing.T, output string) logReconcileReceipt {
	t.Helper()
	line := strings.TrimSpace(output)
	if !strings.HasPrefix(line, logReconcileReceiptPrefix) || strings.Contains(line, "\n") || len(line)+2 > logReconcileReceiptMaxBytes {
		t.Fatal("reconciliation diagnostic lost its bounded single-line framing")
	}
	encoded := strings.TrimPrefix(line, logReconcileReceiptPrefix)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &fields); err != nil {
		t.Fatal("reconciliation diagnostic is not complete JSON")
	}
	var keys []string
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	wantKeys := []string{"collector_started_at", "collectors", "consecutive_two", "enabled", "fresh", "latest_completed_at", "latest_window_start", "observed_at", "previous_completed_at", "previous_window_start", "schema"}
	wantKeys = append(wantKeys, "collector_scopes", "scope_projection_complete", "unsupported_collectors", "duplicate_service_keys")
	sort.Strings(wantKeys)
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("reconciliation receipt changed fixed schema: %v", keys)
	}
	var receipt logReconcileReceipt
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil || receipt.Schema != 2 {
		t.Fatal("reconciliation diagnostic lost its versioned schema")
	}
	for _, forbidden := range []string{"private-service", "private-block", "private-error", "private-token", "192.0.2.91", "aaaaaaaa-aaaa"} {
		if strings.Contains(output, forbidden) {
			t.Fatal("reconciliation receipt exposed an identity, error, or source contents")
		}
	}
	return receipt
}

func requireReceiptTimeRange(t *testing.T, got logReconcileTimeRange, oldest, newest time.Time) {
	t.Helper()
	if got.Oldest == nil || got.Newest == nil || !got.Oldest.Equal(oldest) || !got.Newest.Equal(newest) {
		t.Fatal("reconciliation receipt lost the exact oldest/newest source-window boundary")
	}
}

func TestReconcileReceiptTwoAdvancingWindowsStayOutOfAlertJSONL(t *testing.T) {
	initial := time.Date(2026, 9, 15, 23, 40, 0, 0, time.UTC)
	now := initial
	tailers := []*logTailer{
		syntheticReceiptTailer(&now, "private-service-one"),
		syntheticReceiptTailer(&now, "private-service-two"),
	}
	var diagnostics strings.Builder
	probe := &logTailProbe{tailers: tailers, reconcileDiagnostics: &diagnostics}
	signal := &signalAdapter{number: "1.5", key: "log-errors", name: "Log error-class rates", probe: probe}
	settings := syntheticSettings(&syntheticSource{})
	settings.Now = func() time.Time { return now }
	check := func() logReconcileReceipt {
		t.Helper()
		diagnostics.Reset()
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 {
			t.Fatal("healthy reconciliation manufactured an Alert")
		}
		var alertOutput strings.Builder
		if err := WriteAlertsJSONL(&alertOutput, alerts); err != nil || alertOutput.Len() != 0 {
			t.Fatal("reconciliation receipt entered alert-only JSONL")
		}
		return parseSyntheticReconcileReceipt(t, diagnostics.String())
	}
	for _, tailer := range tailers {
		tailer.reconcileOnce(context.Background())
		now = now.Add(time.Second)
	}
	first := check()
	if first.Collectors != 2 || first.Enabled != 2 || first.Fresh != 2 || first.ConsecutiveTwo != 0 || first.PreviousCompletedAt.Oldest != nil {
		t.Fatal("one successful query per collector became two-window proof")
	}
	if repeated := check(); repeated.ConsecutiveTwo != 0 {
		t.Fatal("repeated diagnostic emission manufactured a second query window")
	}
	now = initial.Add(logReconcileInterval)
	for _, tailer := range tailers {
		tailer.reconcileOnce(context.Background())
		now = now.Add(time.Second)
	}
	second := check()
	if second.Collectors != 2 || second.Enabled != 2 || second.Fresh != 2 || second.ConsecutiveTwo != 2 {
		t.Fatal("two complete advancing windows did not produce aggregate proof")
	}
	requireReceiptTimeRange(t, second.CollectorStartedAt, initial, initial)
	requireReceiptTimeRange(t, second.PreviousCompletedAt, initial, initial.Add(time.Second))
	requireReceiptTimeRange(t, second.LatestCompletedAt, initial.Add(logReconcileInterval), initial.Add(logReconcileInterval+time.Second))
	requireReceiptTimeRange(t, second.PreviousWindowStart, initial.Add(-logReconcileLookback), initial.Add(-logReconcileLookback+time.Second))
	requireReceiptTimeRange(t, second.LatestWindowStart, initial.Add(logReconcileInterval-logReconcileLookback), initial.Add(logReconcileInterval-logReconcileLookback+time.Second))
}

func TestReconcileReceiptFailureCancellationAndIncompleteQueryResetPair(t *testing.T) {
	for _, failure := range []string{"query-error", "pre-canceled", "mid-query-canceled", "saturated-boundary", "incomplete-block", "canceled-block"} {
		t.Run(failure, func(t *testing.T) {
			now := time.Date(2026, 9, 15, 23, 40, 0, 0, time.UTC)
			tailer := syntheticReceiptTailer(&now, "private-service")
			probe := &logTailProbe{tailers: []*logTailer{tailer}}
			tailer.reconcileOnce(context.Background())
			now = now.Add(logReconcileInterval)
			tailer.reconcileOnce(context.Background())
			if probe.reconcileReceipt(fixedReconcileReceiptClock(now)).ConsecutiveTwo != 1 {
				t.Fatal("fixture did not establish two successful windows")
			}
			now = now.Add(logReconcileInterval)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			saturated := strings.Repeat("[private-block]["+now.Add(-3*time.Minute).Format(time.RFC3339)+"]ordinary private-token\n", logReconcileLimit)
			tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
				return "", errors.New("private-error 192.0.2.91 aaaaaaaa-aaaa")
			}
			switch failure {
			case "pre-canceled":
				cancel()
				tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
					t.Fatal("pre-canceled reconciliation invoked a query")
					return "", nil
				}
			case "mid-query-canceled":
				tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
					cancel()
					return "", nil
				}
			case "saturated-boundary":
				tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return saturated, nil }
			case "incomplete-block", "canceled-block":
				tailer.blocks = []string{"private-block-one", "private-block-two"}
				tailer.reconcile = func(_ context.Context, _ time.Time, blocks []string) (string, error) {
					if len(blocks) == 0 {
						return saturated, nil
					}
					if blocks[0] == "private-block-one" {
						return "", nil
					}
					if failure == "canceled-block" {
						cancel()
						return "", nil
					}
					return "", errors.New("private-error")
				}
			}
			tailer.reconcileOnce(ctx)
			cancel()
			var diagnostics strings.Builder
			probe.reconcileDiagnostics = &diagnostics
			probe.writeReconcileReceipt(fixedReconcileReceiptClock(now))
			receipt := parseSyntheticReconcileReceipt(t, diagnostics.String())
			if receipt.Fresh != 0 || receipt.ConsecutiveTwo != 0 || receipt.LatestCompletedAt.Oldest != nil {
				t.Fatal("failed, incomplete, or canceled query retained a consecutive-success claim")
			}
			tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return "", nil }
			for attempt := 1; attempt <= 2; attempt++ {
				now = now.Add(logReconcileInterval)
				tailer.reconcileOnce(context.Background())
				want := 0
				if attempt == 2 {
					want = 1
				}
				if got := probe.reconcileReceipt(fixedReconcileReceiptClock(now)); got.ConsecutiveTwo != want || got.Fresh != 1 {
					t.Fatal("recovery did not require two new successful windows after the break")
				}
			}
		})
	}
}

func TestReconcileReceiptProducerDeclaredTimestampLossResetsEveryPage(t *testing.T) {
	for _, phase := range []string{"aggregate", "block", "aggregate-continuation", "block-continuation"} {
		for _, prefix := range []string{"", "2026/09/15 23:41:30 client.go:471: "} {
			t.Run(phase+fmt.Sprintf("/prefixed=%t", prefix != ""), func(t *testing.T) {
				now := time.Date(2026, 9, 15, 23, 40, 0, 0, time.UTC)
				tailer := syntheticReceiptTailer(&now, "private-service")
				probe := &logTailProbe{tailers: []*logTailer{tailer}}
				tailer.reconcileOnce(context.Background())
				now = now.Add(logReconcileInterval)
				tailer.reconcileOnce(context.Background())
				if probe.reconcileReceipt(fixedReconcileReceiptClock(now)).ConsecutiveTwo != 1 {
					t.Fatal("fixture did not establish the prior two-success receipt")
				}
				now = now.Add(logReconcileInterval)
				start := now.Add(-logReconcileLookback)
				ordinary := "[private-block][" + now.Add(-time.Minute).Format(time.RFC3339Nano) + "]ordinary private-token\n"
				saturated := strings.Repeat(ordinary, logReconcileLimit)
				warning := prefix + "Warning: at least 1000 entries at 2026-09-15 23:40:30 +0000 UTC. The range api cannot page within one nanosecond; skipping the rest of this timestamp.\n"
				if strings.HasPrefix(phase, "block") {
					tailer.blocks = []string{"private-block-one", "private-block-two"}
				}
				calls := 0
				tailer.reconcile = func(_ context.Context, pageStart time.Time, blocks []string) (string, error) {
					calls++
					switch phase {
					case "aggregate":
						return ordinary + warning, nil
					case "aggregate-continuation":
						if pageStart.Equal(start) {
							return saturated, nil
						}
					case "block":
						if len(blocks) == 0 {
							return saturated, nil
						}
						if blocks[0] == "private-block-one" {
							return "", nil
						}
					case "block-continuation":
						if len(blocks) == 0 || pageStart.Equal(start) {
							return saturated, nil
						}
					}
					return ordinary + warning, nil
				}
				live := "[private-block][" + now.Format(time.RFC3339Nano) + "]eval error = Bad status: 429 Too Many Requests {\"code\":5,\"message\":\"API rate limit error\"}"
				tailer.ingestStanding(live, true, true)
				tailer.reconcileOnce(context.Background())
				wantCalls := map[string]int{"aggregate": 1, "block": 3, "aggregate-continuation": 2, "block-continuation": 3}[phase]
				if calls != wantCalls {
					t.Fatalf("query calls=%d, want bounded phase path %d", calls, wantCalls)
				}
				_, _, _, lastError := tailer.reconcileSnapshot()
				if lastError != errLogReconcileIncomplete.Error() {
					t.Fatal("producer loss was accepted or its diagnostic retained raw page/selector evidence")
				}
				var diagnostics strings.Builder
				probe.reconcileDiagnostics = &diagnostics
				findings, err := probe.check(context.Background(), &probeEnv{now: func() time.Time { return now }})
				if err != nil {
					t.Fatal(err)
				}
				if findingByClass(t, findings, "tailer-reconcile").healthy {
					t.Fatal("producer-declared loss did not preserve per-service visibility")
				}
				if findingByClass(t, findings, "payment-processor-rate-limit").healthy {
					t.Fatal("incomplete query discarded a separately observed live finding")
				}
				receipt := parseSyntheticReconcileReceipt(t, diagnostics.String())
				if receipt.Fresh != 0 || receipt.ConsecutiveTwo != 0 || receipt.LatestCompletedAt.Oldest != nil {
					t.Fatal("producer-declared incomplete page retained completed-window proof")
				}
				tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return "", nil }
				for attempt := 1; attempt <= 2; attempt++ {
					now = now.Add(logReconcileInterval)
					tailer.reconcileOnce(context.Background())
					want := 0
					if attempt == 2 {
						want = 1
					}
					if got := probe.reconcileReceipt(fixedReconcileReceiptClock(now)); got.Fresh != 1 || got.ConsecutiveTwo != want {
						t.Fatal("loss recovery did not require two new complete advancing queries")
					}
				}
			})
		}
	}
}

func TestReconcileReceiptSuccessfulRetryAndFramedWarningAreNotProducerLoss(t *testing.T) {
	warning := "Warning: at least 1000 entries at 2026-09-15 23:40:30 +0000 UTC. The range api cannot page within one nanosecond; skipping the rest of this timestamp."
	for _, output := range []string{
		"",
		"2026/09/15 23:41:30 client.go:277: Loki query attempt 1/3 failed (Loki query error (502): Bad Gateway). Retrying in 100ms.\n",
		"[private-block][2026-09-15T23:40:30Z]" + warning + "\n",
		"Warning: at least one successful retry completed.\n",
		"prefix " + warning + "\n",
		warning + " trailing unrelated text\n",
	} {
		now := time.Date(2026, 9, 15, 23, 40, 0, 0, time.UTC)
		tailer := syntheticReceiptTailer(&now, "private-service")
		tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return output, nil }
		probe := &logTailProbe{tailers: []*logTailer{tailer}}
		tailer.reconcileOnce(context.Background())
		now = now.Add(logReconcileInterval)
		tailer.reconcileOnce(context.Background())
		if receipt := probe.reconcileReceipt(fixedReconcileReceiptClock(now)); receipt.Fresh != 1 || receipt.ConsecutiveTwo != 1 {
			t.Fatal("empty success, retry diagnostics, a framed remote message, or a warning lookalike became producer-declared loss")
		}
	}
}

func TestReconcileReceiptRejectsRepeatedNonadvancingAndStaleEvidence(t *testing.T) {
	for _, boundary := range []string{"repeated-window", "backward-window", "nonadvancing-completion", "missed-cadence", "stale", "future"} {
		t.Run(boundary, func(t *testing.T) {
			initial := time.Date(2026, 9, 15, 23, 40, 0, 0, time.UTC)
			now := initial
			tailer := syntheticReceiptTailer(&now, "private-service")
			probe := &logTailProbe{tailers: []*logTailer{tailer}}
			tailer.reconcileOnce(context.Background())
			switch boundary {
			case "backward-window":
				now = now.Add(-time.Second)
			case "nonadvancing-completion":
				now = now.Add(logReconcileInterval)
				tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
					now = initial
					return "", nil
				}
			case "missed-cadence":
				now = now.Add(2 * logReconcileInterval)
			case "stale", "future":
				now = now.Add(logReconcileInterval)
			}
			tailer.reconcileOnce(context.Background())
			if boundary == "stale" {
				now = now.Add(2 * logReconcileInterval)
			} else if boundary == "future" {
				now = now.Add(-time.Second)
			}
			receipt := probe.reconcileReceipt(fixedReconcileReceiptClock(now))
			if receipt.ConsecutiveTwo != 0 {
				t.Fatal("repeated, nonadvancing, stale, or future evidence certified two windows")
			}
			if (boundary == "stale" || boundary == "future") && receipt.Fresh != 0 {
				t.Fatal("stale or future completion became fresh evidence")
			}
		})
	}
}

func TestReconcileReceiptEmptyDisabledAndNewGenerationRemainUnproved(t *testing.T) {
	now := time.Date(2026, 9, 15, 23, 40, 0, 0, time.UTC)
	freshGeneration := syntheticReceiptTailer(&now, "private-service")
	disabled := syntheticReceiptTailer(&now, "private-service-disabled")
	disabled.reconcile = nil
	for _, tailers := range [][]*logTailer{nil, {disabled}, {freshGeneration}, {disabled, freshGeneration}} {
		probe := &logTailProbe{tailers: tailers}
		receipt := probe.reconcileReceipt(fixedReconcileReceiptClock(now))
		if receipt.Collectors != len(tailers) || receipt.Fresh != 0 || receipt.ConsecutiveTwo != 0 || receipt.LatestWindowStart.Newest != nil {
			t.Fatal("empty, disabled, or new-generation collector manufactured completion evidence")
		}
	}
}

type failingReconcileDiagnosticWriter struct{}

func (failingReconcileDiagnosticWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestReconcileReceiptSinkFailureAndCanceledCheckDoNotChangeFindings(t *testing.T) {
	now := time.Date(2026, 9, 15, 23, 40, 0, 0, time.UTC)
	check := func(writer io.Writer, ctx context.Context) []finding {
		t.Helper()
		tailer := syntheticReceiptTailer(&now, "private-service")
		tailer.restartCount = tailerHotRestartThreshold
		probe := &logTailProbe{tailers: []*logTailer{tailer}, reconcileDiagnostics: writer}
		findings, err := probe.check(ctx, &probeEnv{now: func() time.Time { return now }})
		if err != nil {
			t.Fatal("diagnostic emission failure changed probe correctness")
		}
		if findingByClass(t, findings, "tailer-restarting").healthy {
			t.Fatal("fixture lost its independent real finding")
		}
		return findings
	}
	want := check(io.Discard, context.Background())
	if got := check(failingReconcileDiagnosticWriter{}, context.Background()); !reflect.DeepEqual(got, want) {
		t.Fatal("failed diagnostic sink changed or invented a finding")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var diagnostics strings.Builder
	_ = check(&diagnostics, ctx)
	if diagnostics.Len() != 0 {
		t.Fatal("canceled probe emitted a reconciliation receipt")
	}
}

func TestRunLoopArmsReconciliationReceiptsOnlyOnDiagnostics(t *testing.T) {
	source := &runLoopStreamingSource{syntheticSource: &syntheticSource{}}
	settings := syntheticSettings(source)
	settings.LogServices = []string{"private-service"}
	signals, tailers, err := NewWithSignals(settings, NewLogErrorsSignal()).prepareRunLoop(context.Background())
	if err != nil || len(signals) != 1 || len(tailers) != 1 {
		t.Fatal("standing collector preparation failed")
	}
	probe, ok := signals[0].(*signalAdapter).probe.(*logTailProbe)
	if !ok || probe.reconcileDiagnostics != os.Stderr {
		t.Fatal("production standing collector omitted its retained diagnostic receipt sink")
	}
	if probe.reconcileReceipt(settings.Now).ConsecutiveTwo != 0 {
		t.Fatal("new watcher generation inherited a prior success streak")
	}
}

func TestReconcileReceiptDoesNotChangeRunLoopHandlerOutput(t *testing.T) {
	now := time.Date(2026, 9, 15, 23, 40, 0, 0, time.UTC)
	tailer := syntheticReceiptTailer(&now, "private-service")
	tailer.reconcileOnce(context.Background())
	now = now.Add(logReconcileInterval)
	tailer.reconcileOnce(context.Background())
	var diagnostics, alertOutput strings.Builder
	probe := &logTailProbe{tailers: []*logTailer{tailer}, reconcileDiagnostics: &diagnostics}
	signal := &signalAdapter{number: "1.5", key: "log-errors", name: "Log error-class rates", probe: probe}
	settings := syntheticSettings(&syntheticSource{})
	settings.Now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	ticks <- now
	handlerCalls, handledAlerts := 0, 0
	err := NewWithSignals(settings, signal).runLoop(ctx, func(_ context.Context, _ Signal, alerts Alerts) error {
		handlerCalls++
		handledAlerts += len(alerts)
		cancel()
		return WriteAlertsJSONL(&alertOutput, alerts)
	}, func(time.Duration) runLoopTicker { return &manualRunLoopTicker{c: ticks} })
	if err != nil || handlerCalls != 1 || handledAlerts != 0 || alertOutput.Len() != 0 {
		t.Fatal("reconciliation receipt changed the alert handler or JSONL contract")
	}
	if receipt := parseSyntheticReconcileReceipt(t, diagnostics.String()); receipt.ConsecutiveTwo != 1 {
		t.Fatal("standing loop omitted the separate two-window receipt")
	}
}

// Fixed-key fixtures share clocks so an aggregate cannot identify the failed scope.
func syntheticScopePair(now time.Time, service string) *logTailer {
	tailer := syntheticReceiptTailer(&now, service)
	tailer.reconcileSuccesses = [2]logReconcileSuccess{
		{start: now.Add(-logReconcileLookback - logReconcileInterval), completed: now.Add(-logReconcileInterval)},
		{start: now.Add(-logReconcileLookback), completed: now},
	}
	tailer.lastReconcileTime = now
	return tailer
}

// Missing a known key is the causal discriminator, never a guessed error class.
func requireCollectorScope(t testing.TB, receipt logReconcileReceipt, service string) logReconcileCollectorScope {
	t.Helper()
	for _, scope := range receipt.CollectorScopes {
		if scope.ServiceKey == service {
			return scope
		}
	}
	t.Fatalf("causal assertion: missing collector scope: %s", service)
	return logReconcileCollectorScope{}
}

// Equal aggregate counts/ranges conceal different missing service windows.
func TestReconcileReceiptScopeProjectionIdentifiesEqualAggregateDeficits(t *testing.T) {
	now := time.Date(2026, 10, 17, 23, 57, 43, 0, time.UTC)
	observe := func(failed string) map[string]json.RawMessage {
		tailers := []*logTailer{syntheticReceiptTailer(&now, "proxy"), syntheticReceiptTailer(&now, "api")}
		for _, tailer := range tailers {
			tailer.reconcileSuccesses = [2]logReconcileSuccess{
				{start: now.Add(-logReconcileLookback - logReconcileInterval), completed: now.Add(-logReconcileInterval)},
				{start: now.Add(-logReconcileLookback), completed: now},
			}
			tailer.lastReconcileTime = now
			if tailer.service == failed {
				tailer.recordReconcile(now.Add(-logReconcileLookback), errors.New("private-error private-token"))
			}
		}
		encoded, err := json.Marshal((&logTailProbe{tailers: tailers}).reconcileReceipt(fixedReconcileReceiptClock(now)))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}
	apiFailed, proxyFailed := observe("api"), observe("proxy")
	for key, value := range map[string]string{"collectors": "2", "enabled": "2", "fresh": "1", "consecutive_two": "1"} {
		if string(apiFailed[key]) != value || string(proxyFailed[key]) != value {
			t.Fatalf("aggregate fixture prerequisite failed: %s", key)
		}
	}
	for _, key := range []string{"schema", "observed_at", "collectors", "enabled", "fresh", "consecutive_two",
		"collector_started_at", "previous_window_start", "latest_window_start", "previous_completed_at", "latest_completed_at"} {
		if !reflect.DeepEqual(apiFailed[key], proxyFailed[key]) || len(apiFailed[key]) == 0 {
			t.Fatalf("equal aggregate fixture prerequisite failed: %s", key)
		}
	}
	// This regression uses only baseline collector APIs and raw JSON. A schema1
	// baseline reaches this exact assertion rather than failing to compile.
	if len(apiFailed["collector_scopes"]) == 0 {
		t.Fatal("causal assertion: equal aggregates omit the collector_scopes discriminator")
	}
	rows := func(fields map[string]json.RawMessage) map[string]map[string]json.RawMessage {
		var values []map[string]json.RawMessage
		if err := json.Unmarshal(fields["collector_scopes"], &values); err != nil {
			t.Fatal(err)
		}
		result := map[string]map[string]json.RawMessage{}
		for _, value := range values {
			var key string
			if err := json.Unmarshal(value["service_key"], &key); err != nil {
				t.Fatal(err)
			}
			result[key] = value
		}
		return result
	}
	first, second := rows(apiFailed), rows(proxyFailed)
	if len(first) != 2 || len(second) != 2 || string(first["api"]["coverage_state"]) != `"no-valid-window"` ||
		string(first["api"]["fresh"]) != "false" || string(first["api"]["last_failure_present"]) != "true" ||
		string(first["api"]["latest_completed_at"]) != "null" || string(first["api"]["last_success_at"]) == "null" ||
		string(first["proxy"]["coverage_state"]) != `"fresh-two-windows"` ||
		string(second["api"]["coverage_state"]) != `"fresh-two-windows"` ||
		string(second["proxy"]["coverage_state"]) != `"no-valid-window"` ||
		string(apiFailed["scope_projection_complete"]) != "true" {
		t.Fatal("fixed scope vector failed to distinguish equal aggregate deficits")
	}
}

// Unsupported and repeated names cannot acquire a synthetic healthy identity.
func TestReconcileReceiptScopeKeysDuplicatesAndPrivacy(t *testing.T) {
	now := time.Date(2026, 10, 17, 23, 57, 43, 0, time.UTC)
	web := syntheticScopePair(now, "web")
	web.reconcile = nil
	tailers := []*logTailer{web, syntheticScopePair(now, "api"), syntheticScopePair(now, "private-service"),
		syntheticScopePair(now, "api"), syntheticScopePair(now, "api"), syntheticScopePair(now, "private-token-192.0.2.91")}
	tailers[1].lastReconcileError = "private-error private-block aaaaaaaa-aaaa"
	receipt := (&logTailProbe{tailers: tailers}).reconcileReceipt(fixedReconcileReceiptClock(now))
	if receipt.ScopeProjectionComplete || receipt.UnsupportedCollectors != 2 || receipt.DuplicateServiceKeys != 1 ||
		len(receipt.CollectorScopes) != 2 || receipt.CollectorScopes[0].ServiceKey != "api" || receipt.CollectorScopes[1].ServiceKey != "web" {
		t.Fatal("unsupported/duplicate scopes did not remain explicit, bounded and ordered")
	}
	api := requireCollectorScope(t, receipt, "api")
	if api.CollectorCount != 3 || api.CoverageState != "ambiguous" || api.Enabled != nil || api.Fresh != nil ||
		api.ConsecutiveTwo != nil || api.LastFailurePresent != nil || api.LatestCompletedAt != nil || api.CollectorStartedAt != nil {
		t.Fatal("duplicate service selected one collector's clocks or health")
	}
	for _, incomplete := range [][]*logTailer{nil, {syntheticScopePair(now, "private-service")}} {
		projection := (&logTailProbe{tailers: incomplete}).reconcileReceipt(fixedReconcileReceiptClock(now))
		if projection.ScopeProjectionComplete || len(projection.CollectorScopes) != 0 || projection.UnsupportedCollectors != len(incomplete) {
			t.Fatal("empty or unsupported-only inventory acquired a complete scope map")
		}
	}
	encoded := string(encodeLogReconcileReceipt(receipt))
	for _, secret := range []string{"private-service", "private-token", "private-error", "private-block", "192.0.2.91", "aaaaaaaa-aaaa", "http-429"} {
		if strings.Contains(encoded, secret) {
			t.Fatal("scope projection leaked an identifier/error or invented a cause")
		}
	}
	if scope := requireCollectorScope(t, receipt, "web"); scope.CoverageState != "disabled" || scope.Enabled == nil || *scope.Enabled {
		t.Fatal("disabled collector became fresh proof")
	}
}

// Null/partial/future source clocks survive without becoming a healthy window.
func TestReconcileReceiptScopeClockBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 17, 23, 57, 43, 123, time.UTC)
	for _, testCase := range []struct {
		name       string
		change     func(*logTailer)
		state      string
		fresh, two bool
	}{
		{"two", func(*logTailer) {}, "fresh-two-windows", true, true},
		{"one", func(tailer *logTailer) { tailer.reconcileSuccesses[0] = logReconcileSuccess{} }, "fresh-one-window", true, false},
		{"edge-fresh", func(tailer *logTailer) {
			tailer.reconcileSuccesses[1].completed = now.Add(-2*logReconcileInterval + time.Nanosecond)
		}, "fresh-one-window", true, false},
		{"edge-stale", func(tailer *logTailer) { tailer.reconcileSuccesses[1].completed = now.Add(-2 * logReconcileInterval) }, "stale-window", false, false},
		{"future", func(tailer *logTailer) { tailer.reconcileSuccesses[1].completed = now.Add(time.Second) }, "invalid-time", false, false},
		{"reversed", func(tailer *logTailer) { tailer.reconcileSuccesses[1].completed = tailer.reconcileSuccesses[1].start }, "invalid-time", false, false},
		{"future-started", func(tailer *logTailer) { tailer.startedAt = now.Add(time.Second) }, "invalid-time", true, true},
		{"future-last-success", func(tailer *logTailer) { tailer.lastReconcileTime = now.Add(time.Second) }, "invalid-time", true, true},
		{"future-previous-start", func(tailer *logTailer) { tailer.reconcileSuccesses[0].start = now.Add(time.Second) }, "invalid-time", true, false},
		{"future-previous-completion", func(tailer *logTailer) { tailer.reconcileSuccesses[0].completed = now.Add(time.Second) }, "invalid-time", true, false},
		{"partial-previous", func(tailer *logTailer) { tailer.reconcileSuccesses[0].completed = time.Time{} }, "fresh-one-window", true, false},
		{"null-metadata", func(tailer *logTailer) { tailer.startedAt = time.Time{}; tailer.lastReconcileTime = time.Time{} }, "fresh-two-windows", true, true},
		{"partial", func(tailer *logTailer) { tailer.reconcileSuccesses[1].completed = time.Time{} }, "no-valid-window", false, false},
		{"empty", func(tailer *logTailer) { tailer.reconcileSuccesses = [2]logReconcileSuccess{} }, "no-valid-window", false, false},
		{"disabled", func(tailer *logTailer) { tailer.reconcile = nil }, "disabled", false, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tailer := syntheticScopePair(now, "api")
			testCase.change(tailer)
			receipt := (&logTailProbe{tailers: []*logTailer{tailer}}).reconcileReceipt(fixedReconcileReceiptClock(now))
			scope := requireCollectorScope(t, receipt, "api")
			if scope.CoverageState != testCase.state || scope.Fresh == nil || *scope.Fresh != testCase.fresh ||
				scope.ConsecutiveTwo == nil || *scope.ConsecutiveTwo != testCase.two {
				t.Fatalf("wrong clock authority for %s: %+v", testCase.name, scope)
			}
			want := tailer.reconcileSuccesses[1]
			previous := tailer.reconcileSuccesses[0]
			if !reflect.DeepEqual(scope.LatestWindowStart, logReconcileTime(want.start)) ||
				!reflect.DeepEqual(scope.LatestCompletedAt, logReconcileTime(want.completed)) ||
				!reflect.DeepEqual(scope.PreviousWindowStart, logReconcileTime(previous.start)) ||
				!reflect.DeepEqual(scope.PreviousCompletedAt, logReconcileTime(previous.completed)) ||
				!reflect.DeepEqual(scope.CollectorStartedAt, logReconcileTime(tailer.startedAt)) ||
				!reflect.DeepEqual(scope.LastSuccessAt, logReconcileTime(tailer.lastReconcileTime)) {
				t.Fatal("projection discarded partial/future clocks")
			}
		})
	}
}

// The fixed rows fit the cap; an oversized encoding cannot mutate source state.
func TestReconcileReceiptScopeEncodingBoundAndAggregateParity(t *testing.T) {
	now := time.Date(2026, 10, 17, 23, 57, 43, 999999999, time.UTC)
	var tailers []*logTailer
	for _, key := range logReconcileServiceKeys {
		tailers = append(tailers, syntheticScopePair(now, key))
	}
	receipt := (&logTailProbe{tailers: tailers}).reconcileReceipt(fixedReconcileReceiptClock(now))
	if len(receipt.CollectorScopes) != 10 || !receipt.ScopeProjectionComplete || receipt.Fresh != 10 || receipt.ConsecutiveTwo != 10 {
		t.Fatal("ten-service projection changed aggregate authority")
	}
	encoded := encodeLogReconcileReceipt(receipt)
	if len(encoded) == 0 || len(encoded)+len(logReconcileReceiptPrefix)+2 > logReconcileReceiptMaxBytes {
		t.Fatal("maximum fixed service rows exceeded the diagnostic bound")
	}
	// A synthetic malformed oversized row exercises the codec refusal directly;
	// the production allowlist cannot create this arbitrary string.
	receipt.CollectorScopes[0].ServiceKey = strings.Repeat("x", logReconcileReceiptMaxBytes)
	before, _ := json.Marshal(receipt)
	if encodeLogReconcileReceipt(receipt) != nil {
		t.Fatal("oversized scope diagnostic was emitted")
	}
	after, _ := json.Marshal(receipt)
	if !reflect.DeepEqual(before, after) || tailers[0].lastReconcileTime != now || tailers[0].lastReconcileError != "" {
		t.Fatal("encoding refusal altered collection/reducer state")
	}
}

// Failed and canceled queries invalidate coverage without projecting a cause.
func TestReconcileReceiptScopeFailureCancellationAndIncompleteRemainUnattributed(t *testing.T) {
	for _, kind := range []string{"query-error", "pre-canceled", "mid-canceled", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 17, 23, 57, 43, 0, time.UTC)
			tailer := syntheticScopePair(now, "api")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
				calls++
				if kind == "mid-canceled" {
					cancel()
					return "", nil
				}
				if kind == "incomplete" {
					return "", errLogReconcileIncomplete
				}
				return "", errors.New("private-error http-429 private-token")
			}
			if kind == "pre-canceled" {
				cancel()
			}
			tailer.reconcileOnce(ctx)
			receipt := (&logTailProbe{tailers: []*logTailer{tailer}}).reconcileReceipt(fixedReconcileReceiptClock(now))
			scope := requireCollectorScope(t, receipt, "api")
			if scope.CoverageState != "no-valid-window" || scope.LatestCompletedAt != nil ||
				scope.PreviousCompletedAt != nil || scope.Fresh == nil || *scope.Fresh || *scope.ConsecutiveTwo ||
				scope.LastSuccessAt == nil || !scope.LastSuccessAt.Equal(now) {
				t.Fatal("failure/cancellation acquired current completion authority")
			}
			if kind == "pre-canceled" && calls != 0 || kind != "pre-canceled" && calls != 1 {
				t.Fatal("diagnostic projection changed the query call boundary")
			}
			for _, forbidden := range []string{"private-error", "http-429", "private-token", "canceled"} {
				if strings.Contains(string(encodeLogReconcileReceipt(receipt)), forbidden) {
					t.Fatal("coverage projection inferred a failure cause")
				}
			}
		})
	}
}

// Count the existing diagnostic write without introducing another output path.
type reconcileScopeWriteCounter struct {
	writes int
	body   []byte
}

// Append one diagnostic frame and count the existing sink invocation.
func (self *reconcileScopeWriteCounter) Write(value []byte) (int, error) {
	self.writes++
	self.body = append(self.body, value...)
	return len(value), nil
}

// Complete projection and encoding failure preserve one-write/zero-query behavior.
func TestReconcileReceiptScopeSingleWriteAndEncodingFailureAreNonmutating(t *testing.T) {
	now := time.Date(2026, 10, 17, 23, 57, 43, 0, time.UTC)
	tailer := syntheticScopePair(now, "api")
	calls := 0
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { calls++; return "", nil }
	writer := &reconcileScopeWriteCounter{}
	probe := &logTailProbe{tailers: []*logTailer{tailer}, reconcileDiagnostics: writer}
	before := tailer.reconcileSuccesses
	probe.writeReconcileReceipt(fixedReconcileReceiptClock(now))
	if writer.writes != 1 || calls != 0 || tailer.reconcileSuccesses != before {
		t.Fatal("scope diagnostic changed write cadence or collection")
	}
	_ = parseSyntheticReconcileReceipt(t, string(writer.body))
	// A non-JSON year exercises the same diagnostic-only refusal path as the
	// explicit byte bound. No collector callback or reducer runs in the codec.
	tailer.startedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	probe.writeReconcileReceipt(fixedReconcileReceiptClock(now))
	if writer.writes != 1 || calls != 0 || tailer.reconcileSuccesses != before {
		t.Fatal("encoding refusal mutated collection or emitted another write")
	}
}
