// Public controls preserve event authority, unknown history, inventory,
// cancellation, privacy and the actual cadence/notifier recovery boundary.
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

// Edit only synthetic framing, without duplicating the production parser.
func pgAutovacuumCancelEdit(t testing.TB, payload string, edit func(map[string]any)) string {
	t.Helper()
	var frame map[string]any
	if json.Unmarshal([]byte(payload), &frame) != nil {
		t.Fatal("invalid synthetic cancellation frame")
	}
	edit(frame)
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatal("cannot edit synthetic cancellation frame")
	}
	return string(encoded) + "\n"
}

// Each complete observed-file window is quiet unless a record falls on or
// after its inclusive source-clock cutoff; local completion cannot re-date it.
func TestPgAutovacuumCancelWindowAndQuietBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		events []time.Time
		count  int
	}{
		{name: "quiet", events: nil, count: 0},
		{name: "expired", events: []time.Time{now.Add(-15*time.Minute - time.Microsecond)}, count: 0},
		{name: "cutoff", events: []time.Time{now.Add(-15 * time.Minute)}, count: 1},
		{name: "snapshot", events: []time.Time{now}, count: 1},
		{name: "distinct identical records", events: []time.Time{now, now}, count: 2},
	} {
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelPayload(t, now, c.events))
		if c.count == 0 {
			if len(alerts) != 0 {
				t.Fatalf("%s: complete quiet window was not quiet", c.name)
			}
		} else {
			alert := requireAlertClass(t, alerts, "pg-autovacuum-cancel-observed")
			if len(alerts) != 1 || !strings.Contains(alert.Observed, fmt.Sprintf("cancellation_records_at_least=%d", c.count)) {
				t.Fatalf("%s: native record time/count boundary changed", c.name)
			}
		}
	}
	for _, offset := range []time.Duration{-31 * time.Second, -30 * time.Second, 30 * time.Second, 31 * time.Second} {
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelPayload(t, now.Add(offset), nil))
		if (len(alerts) == 0) != (offset >= -30*time.Second && offset <= 30*time.Second) {
			t.Fatal("source clock freshness boundary changed")
		}
	}
}

// Missing observations lower a count's authority but cannot erase a separately
// framed native record; a complete source frame is required for quiet recovery.
func TestPgAutovacuumCancelPartialHistoryRetainsRecords(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, flag := range []string{"tail-capped", "partial-first", "path-changed", "record-order", "short-read", "partial-last", "record-schema", "record-clock", "future-record", "row-cap", "record-session", "source-index-changed", "record-capped"} {
		for _, positive := range []bool{false, true} {
			events := []time.Time{}
			if positive {
				events = append(events, now.Add(-time.Minute))
			}
			alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelPayload(t, now, events, flag))
			unknown := requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable")
			if !unknown.unobservable || unknown.Severity != SeverityWarn || unknown.Sustain != 2 ||
				(requireAlertClassCount(alerts, "pg-autovacuum-cancel-observed") == 1) != positive {
				t.Fatalf("%s: partial history erased a record or supplied recovery", flag)
			}
		}
	}
	base := pgAutovacuumCancelPayload(t, now, nil)
	for _, size := range []int64{40000000, 40000001} {
		payload := pgAutovacuumCancelEdit(t, base, func(frame map[string]any) {
			frame["file_before"].([]any)[2] = fmt.Sprint(size)
			frame["file_after"].([]any)[2] = fmt.Sprint(size)
			frame["read"] = []string{fmt.Sprint(size - 40000000), "40000000"}
		})
		alerts := pgAutovacuumCancelRun(t, now, payload)
		if (len(alerts) == 0) != (size == 40000000) {
			t.Fatal("bounded tail certified full-file absence")
		}
	}
	for _, span := range [][]string{{"0", "0"}, {fmt.Sprint(now.Add(-14 * time.Minute).UnixMicro()), fmt.Sprint(now.UnixMicro())}} {
		payload := pgAutovacuumCancelEdit(t, base, func(frame map[string]any) { frame["span"] = span })
		if !requireAlertClass(t, pgAutovacuumCancelRun(t, now, payload), "pg-autovacuum-cancel-unobservable").unobservable {
			t.Fatal("missing history acquired healthy authority")
		}
	}
}

// Every emitted representation omits source values and private fixture text.
func pgAutovacuumCancelRequirePrivate(t testing.TB, alerts Alerts) {
	t.Helper()
	for _, alert := range alerts {
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal("cannot encode public cancellation alert")
		}
		for _, canary := range []string{"private-source-canary", "private-sql-canary", "private-client-canary", "00000000-0000-4000-8000-000000000001"} {
			if strings.Contains(string(encoded), canary) || strings.Contains(alert.Markdown(), canary) || strings.Contains(alert.Identity(), canary) {
				t.Fatal("public cancellation alert exposed private source content")
			}
		}
	}
}

// Malformed frames, identity changes and contradictory event fields do not
// supply either positives or healthy recovery, and never leak their contents.
func TestPgAutovacuumCancelInvalidFramesRemainUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	base := pgAutovacuumCancelPayload(t, now, []time.Time{now.Add(-time.Minute), now.Add(-time.Minute)})
	for _, edit := range []func(map[string]any){
		func(frame map[string]any) { frame["version"] = "1" },
		func(frame map[string]any) { frame["host"] = "private-source-canary" },
		func(frame map[string]any) { frame["boot"].([]any)[1] = "private-source-canary" },
		func(frame map[string]any) { frame["file_after"].([]any)[1] = "3" },
		func(frame map[string]any) {
			frame["clock"] = []string{"9223372036854775807", "9223372036854775807", "9223372036854775807", "0", "0"}
		},
		func(frame map[string]any) { frame["clock"].([]any)[0] = "private-source-canary" },
		func(frame map[string]any) { frame["read"] = []string{"0", "1000001"} },
		func(frame map[string]any) { frame["flags"] = []string{"private-source-canary"} },
		func(frame map[string]any) { frame["flags"] = []string{"row-cap", "row-cap"} },
		func(frame map[string]any) { frame["flags"] = []string{"snapshot-changed"} },
		func(frame map[string]any) { frame["rows"].([]any)[1].([]any)[0] = "100" },
		func(frame map[string]any) {
			frame["rows"].([]any)[0].([]any)[1] = fmt.Sprint(now.Add(time.Microsecond).UnixMicro())
		},
		func(frame map[string]any) { frame["rows"].([]any)[0].([]any)[0] = "-1" },
		func(frame map[string]any) { frame["rows"].([]any)[0].([]any)[1] = "01" },
		func(frame map[string]any) { frame["rows"] = nil },
		func(frame map[string]any) { delete(frame, "flags") },
		func(frame map[string]any) { frame["private-source-canary"] = "private-source-canary" },
	} {
		alerts := pgAutovacuumCancelRun(t, now, pgAutovacuumCancelEdit(t, base, edit))
		if len(alerts) != 1 || !requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").unobservable {
			t.Fatal("invalid cancellation frame supplied observation authority")
		}
		pgAutovacuumCancelRequirePrivate(t, alerts)
	}
	for _, payload := range []string{strings.TrimSuffix(base, "\n"), base + "{}\n", strings.Replace(base, `"status":"snapshot"`, `"status":"snapshot","status":"snapshot"`, 1), strings.Repeat("x", pgAutovacuumCancelMaxOutput+1)} {
		alerts := pgAutovacuumCancelRun(t, now, payload)
		if len(alerts) != 1 || !requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").unobservable {
			t.Fatal("truncated, duplicated or overbound frame supplied authority")
		}
	}
}

// A typed native cause survives cancellation without rendering its text.
type pgAutovacuumCancelTestError struct{}

// Source failures are deliberately private synthetic values.
func (*pgAutovacuumCancelTestError) Error() string { return "private-source-canary" }

// The public transport sees a finite parent deadline in addition to its
// explicit timeout; source failure is not retried or promoted to healthy.
type pgAutovacuumCancelTimedSource struct {
	*syntheticSource
	deadline bool
}

// Capture the actual context boundary before returning the fixed source cause.
func (self *pgAutovacuumCancelTimedSource) HostTimeout(ctx context.Context, host HostSettings, command string, timeout time.Duration) (string, error) {
	_, self.deadline = ctx.Deadline()
	return self.syntheticSource.HostTimeout(ctx, host, command, timeout)
}

// Inventory gaps and pauses cannot contact a different or disabled primary.
func TestPgAutovacuumCancelInventoryAndTransport(t *testing.T) {
	for _, mode := range []string{"missing", "ambiguous", "excluded", "disabled"} {
		settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			t.Fatal("unowned cancellation source was contacted")
			return "", nil
		}})
		switch mode {
		case "missing":
			settings.Hosts = nil
		case "ambiguous":
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-2", Roles: []string{"pg-primary"}})
		case "excluded":
			settings.ExcludedHosts = []string{"pg-1"}
		case "disabled":
			settings.disabledHosts = []HostSettings{settings.Hosts[0]}
			settings.Hosts = settings.Hosts[1:]
		}
		alerts, err := NewPgAutovacuumCancelSignal().Run(context.Background(), settings)
		found := false
		for _, alert := range alerts {
			found = found || alert.unobservable
		}
		if err != nil || !found {
			t.Fatalf("%s: missing primary authority became healthy", mode)
		}
	}
	calls := 0
	source := &pgAutovacuumCancelTimedSource{syntheticSource: &syntheticSource{hostTimeoutFn: func(_ HostSettings, _ string, timeout time.Duration) (string, error) {
		calls++
		if timeout != 25*time.Second {
			t.Fatal("native cancellation reader lost transport bound")
		}
		return "private-source-canary", &pgAutovacuumCancelTestError{}
	}}}
	alerts, err := NewPgAutovacuumCancelSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || calls != 1 || !source.deadline || len(alerts) != 1 || !requireAlertClass(t, alerts, "pg-autovacuum-cancel-unobservable").unobservable {
		t.Fatal("failed bounded source retried or supplied healthy authority")
	}
	pgAutovacuumCancelRequirePrivate(t, alerts)
}

// Parent end preserves the exact source cause and admits no follow-up read.
func TestPgAutovacuumCancelSourceCancellationPreservesCause(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			cause := &pgAutovacuumCancelTestError{}
			settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				calls++
				cancel()
				if failed {
					return "private-source-canary", cause
				}
				return "private-source-canary", nil
			}})
			if scoped {
				settings.ExcludedHosts = []string{"redis-1"}
			}
			signal := NewPgAutovacuumCancelSignal()
			alerts, err := signal.Run(ctx, settings)
			cancel()
			var typed *pgAutovacuumCancelTestError
			if len(alerts) != 0 || !errors.Is(err, context.Canceled) || (failed && (!errors.Is(err, cause) || !errors.As(err, &typed))) {
				t.Fatal("canceled source lost native cause or published a verdict")
			}
			if _, err := signal.Run(ctx, settings); !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatal("ended parent admitted another cancellation read")
			}
		}
	}
}

// Cancellation in the real completion Now hook precedes every source verdict.
func TestPgAutovacuumCancelCompletionCancellationPreventsVerdict(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, scoped := range []bool{false, true} {
		for _, outcome := range []string{"positive", "quiet", "malformed"} {
			payload := pgAutovacuumCancelPayload(t, now, []time.Time{now.Add(-time.Minute)})
			if outcome == "quiet" {
				payload = pgAutovacuumCancelPayload(t, now, nil)
			}
			if outcome == "malformed" {
				payload = "private-source-canary"
			}
			ctx, cancel := context.WithCancel(context.Background())
			calls, completionClocks := 0, 0
			settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) { calls++; return payload, nil }})
			if scoped {
				settings.ExcludedHosts = []string{"redis-1"}
			}
			settings.Now = func() time.Time {
				if calls > 0 {
					completionClocks++
					cancel()
				}
				return now
			}
			alerts, err := NewPgAutovacuumCancelSignal().Run(ctx, settings)
			cancel()
			if calls != 1 || completionClocks == 0 || len(alerts) != 0 || !errors.Is(err, context.Canceled) {
				t.Fatalf("scoped=%v outcome=%s: completion cancellation published a verdict", scoped, outcome)
			}
		}
	}
}

// The actual gate and notifier retain the event through held/ready unknown
// observations, and recover only after two complete quiet observed windows.
func TestPgAutovacuumCancelNotifierRequiresCompleteQuietHistory(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	payload := ""
	settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }})
	settings.Now = func() time.Time { return now }
	signal := NewPgAutovacuumCancelSignal()
	gate := newCadenceAlertGate()
	notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
	for run := 0; run < 5; run++ {
		now = now.Add(15 * time.Minute)
		switch run {
		case 0:
			payload = pgAutovacuumCancelPayload(t, now, []time.Time{now.Add(-time.Minute)})
		case 1, 2:
			payload = pgAutovacuumCancelPayload(t, now, nil, "path-changed")
		default:
			payload = pgAutovacuumCancelPayload(t, now, nil)
		}
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("notifier source observation failed")
		}
		ready, held := gate.split(signal, alerts)
		if run == 1 && (len(ready) != 0 || len(held) != 1) {
			t.Fatal("first coverage loss was not held by its sustain policy")
		}
		if run == 2 && (len(ready) != 1 || len(held) != 0) {
			t.Fatal("repeated coverage loss was not ready")
		}
		notifier.stateLock.Lock()
		transitions := notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
		notifier.stateLock.Unlock()
		matches := []slackTransition{}
		for _, transition := range transitions {
			if transition.alert.Class == "pg-autovacuum-cancel-observed" {
				matches = append(matches, transition)
			}
		}
		if run == 0 {
			if len(matches) != 1 || matches[0].kind != slackRaised {
				t.Fatal("observed cancellation was not raised")
			}
		} else if run < 4 {
			if len(matches) != 0 {
				t.Fatal("unknown or one quiet history window cleared observed cancellation")
			}
		} else if len(matches) != 1 || matches[0].kind != slackCleared {
			t.Fatal("two complete quiet windows did not clear the observed record")
		}
	}
}
