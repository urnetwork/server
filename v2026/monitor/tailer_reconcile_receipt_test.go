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

func syntheticReceiptTailer(now *time.Time, service string) *logTailer {
	tailer := newLogTailer(service, &probeEnv{now: func() time.Time { return *now }})
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return "", nil }
	return tailer
}

func parseSyntheticReconcileReceipt(t *testing.T, output string) logReconcileReceipt {
	t.Helper()
	line := strings.TrimSpace(output)
	if !strings.HasPrefix(line, logReconcileReceiptPrefix) || strings.Contains(line, "\n") || len(line) > 1536 {
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
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("reconciliation receipt changed fixed schema: %v", keys)
	}
	var receipt logReconcileReceipt
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil || receipt.Schema != 1 {
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
			if probe.reconcileReceipt(now).ConsecutiveTwo != 1 {
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
			probe.writeReconcileReceipt(now)
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
				if got := probe.reconcileReceipt(now); got.ConsecutiveTwo != want || got.Fresh != 1 {
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
				if probe.reconcileReceipt(now).ConsecutiveTwo != 1 {
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
					if got := probe.reconcileReceipt(now); got.Fresh != 1 || got.ConsecutiveTwo != want {
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
		if receipt := probe.reconcileReceipt(now); receipt.Fresh != 1 || receipt.ConsecutiveTwo != 1 {
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
			receipt := probe.reconcileReceipt(now)
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
		receipt := probe.reconcileReceipt(now)
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
	if probe.reconcileReceipt(settings.Now()).ConsecutiveTwo != 0 {
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
