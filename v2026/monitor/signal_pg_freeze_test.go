// Composed controls distinguish witnessed observed-log clusters from missing
// membership, source coverage, clock and native transport authority.
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

// Edits only synthetic reduced framing, never a production log or reducer.
func pgFreezeEditPayload(t testing.TB, payload string, edit func(map[string]any)) string {
	t.Helper()
	var frame map[string]any
	if json.Unmarshal([]byte(payload), &frame) != nil {
		t.Fatal("invalid synthetic projection")
	}
	edit(frame)
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatal("cannot edit synthetic projection")
	}
	return string(encoded) + "\n"
}

// Count, exact median and two-cluster thresholds are independent policy axes.
func TestPgFreezeQualifiedThresholds(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		count    int
		duration int64
		clusters int
		want     bool
	}{
		{count: 249, duration: 3000000000, clusters: 1, want: false},
		{count: 250, duration: 2999999999, clusters: 1, want: false},
		{count: 250, duration: 3000000000, clusters: 1, want: true},
		{count: 250, duration: 2000000000, clusters: 1, want: false},
		{count: 250, duration: 1999999999, clusters: 2, want: false},
		{count: 250, duration: 2000000000, clusters: 2, want: true},
	} {
		events := []pgFreezeTestEvent{}
		for index := 0; index < c.clusters; index++ {
			events = append(events, pgFreezeTestCluster(now.Add(time.Duration(index-3)*time.Minute), c.count, c.duration)...)
		}
		alerts := pgFreezeRunForTest(t, now, pgFreezeTestPayload(t, now, events))
		if (requireAlertClassCount(alerts, "pg-freeze-clusters") == 1) != c.want {
			t.Fatalf("count=%d duration=%d clusters=%d crossed the wrong boundary", c.count, c.duration, c.clusters)
		}
	}
}

// Chained gaps, including their equality boundary, define cluster membership.
func TestPgFreezeGapAndEvenMedianBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, gap := range []time.Duration{100 * time.Millisecond, 100*time.Millisecond + time.Microsecond} {
		start := now.Add(-time.Minute)
		events := []pgFreezeTestEvent{{ended: start.Add(-time.Second), duration: 1000000000}}
		for index := 0; index < 250; index++ {
			duration := int64(2999999999)
			if index >= 125 {
				duration = 3000000001
			}
			events = append(events, pgFreezeTestEvent{ended: start.Add(time.Duration(index) * gap), duration: duration})
		}
		events = append(events, pgFreezeTestEvent{ended: events[len(events)-1].ended.Add(time.Second), duration: 1000000000})
		alerts := pgFreezeRunForTest(t, now, pgFreezeTestPayload(t, now, events))
		if (requireAlertClassCount(alerts, "pg-freeze-clusters") == 1) != (gap == 100*time.Millisecond) {
			t.Fatal("gap equality or the even-member median changed")
		}
	}
}

// The event time is the chain's first completion, not the local run time.
func TestPgFreezeWindowBoundary(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{15 * time.Minute, 15*time.Minute + time.Microsecond} {
		alerts := pgFreezeRunForTest(t, now, pgFreezeTestPayload(t, now, pgFreezeTestCluster(now.Add(-age), 250, 3000000000)))
		if (requireAlertClassCount(alerts, "pg-freeze-clusters") == 1) != (age == 15*time.Minute) {
			t.Fatal("fifteen-minute event boundary changed")
		}
	}
}

// Small or low-median edge chains are still missing membership authority.
func TestPgFreezeEveryUnclosedEdgeStaysUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, count := range []int{1, 249, 250} {
		for _, duration := range []int64{1000000000, 3000000000} {
			for _, missing := range []string{"before", "after", "both"} {
				events := pgFreezeTestCluster(now.Add(-time.Minute), count, duration)
				if missing == "before" || missing == "both" {
					events = events[1:]
				}
				if missing == "after" || missing == "both" {
					events = events[:len(events)-1]
				}
				alerts := pgFreezeRunForTest(t, now, pgFreezeTestPayload(t, now, events))
				if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 || !requireAlertClass(t, alerts, "pg-freeze-unobservable").unobservable {
					t.Fatal("an unresolved edge manufactured an incident or complete absence")
				}
			}
		}
	}
}

// A byte cap withholds absence even when an interior observed cluster is real.
func TestPgFreezeCappedTailKeepsIndependentCluster(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	payload := pgFreezeTestPayload(t, now, pgFreezeTestCluster(now.Add(-time.Minute), 250, 3000000000), "tail-capped", "partial-first")
	payload = pgFreezeEditPayload(t, payload, func(frame map[string]any) {
		frame["file_before"].([]any)[2] = "40005000"
		frame["file_after"].([]any)[2] = "40005000"
		frame["read"] = []string{"5000", "40000000"}
		for _, row := range frame["rows"].([]any) {
			cells := row.([]any)
			var offset int64
			fmt.Sscan(cells[0].(string), &offset)
			cells[0] = fmt.Sprint(offset + 5000)
		}
	})
	alerts := pgFreezeRunForTest(t, now, payload)
	requireAlertClass(t, alerts, "pg-freeze-clusters")
	if !requireAlertClass(t, alerts, "pg-freeze-unobservable").unobservable {
		t.Fatal("capped tail claimed full-window recovery")
	}
}

// Complete captured membership can be sorted, but order violations still
// prevent the chronological coverage proof from authorizing recovery.
func TestPgFreezeOutOfOrderFullSnapshotKeepsScopedPositive(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	events := pgFreezeTestCluster(now.Add(-time.Minute), 250, 3000000000)
	events[0], events[len(events)-1] = events[len(events)-1], events[0]
	alerts := pgFreezeRunForTest(t, now, pgFreezeTestPayload(t, now, events))
	requireAlertClass(t, alerts, "pg-freeze-clusters")
	if !strings.Contains(requireAlertClass(t, alerts, "pg-freeze-unobservable").Observed, "record-order") {
		t.Fatal("out-of-order log supplied complete coverage")
	}
}

// Only a complete full-file window with no unresolved current edge is quiet.
func TestPgFreezeCompleteQuietWindow(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, events := range [][]pgFreezeTestEvent{nil, {{ended: now.Add(-16 * time.Minute), duration: 4000000000}}} {
		alerts := pgFreezeRunForTest(t, now, pgFreezeTestPayload(t, now, events))
		if len(alerts) != 0 {
			t.Fatal("complete quiet observed-log control alerted")
		}
	}
}

// Bad framing and invalid rows cannot be ignored to manufacture a high median.
func TestPgFreezeMalformedSourceRemainsPrivate(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	base := pgFreezeTestPayload(t, now, pgFreezeTestCluster(now.Add(-time.Minute), 250, 3000000000))
	for _, edit := range []func(map[string]any){
		func(frame map[string]any) { frame["host"] = "private-host-canary" },
		func(frame map[string]any) {
			frame["clock"] = []string{"9223372036854775807", "9223372036854775807", "9223372036854775807", "0", "0"}
		},
		func(frame map[string]any) { frame["clock"] = []string{"private-clock-canary", "1", "1", "0", "0"} },
		func(frame map[string]any) { frame["boot"].([]any)[1] = "private-boot-canary" },
		func(frame map[string]any) { frame["file_after"].([]any)[1] = "3" },
		func(frame map[string]any) { frame["flags"] = []string{"private-error-canary"} },
		func(frame map[string]any) { frame["rows"].([]any)[1].([]any)[0] = frame["rows"].([]any)[0].([]any)[0] },
		func(frame map[string]any) { frame["rows"].([]any)[1].([]any)[2] = "999999999" },
		func(frame map[string]any) { frame["rows"].([]any)[1].([]any)[2] = "NaN" },
		func(frame map[string]any) { frame["rows"].([]any)[1].([]any)[2] = "9223372036854775808" },
	} {
		alerts := pgFreezeRunForTest(t, now, pgFreezeEditPayload(t, base, edit))
		if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 {
			t.Fatal("invalid rows supplied cluster authority")
		}
		alert := requireAlertClass(t, alerts, "pg-freeze-unobservable")
		if !alert.unobservable {
			t.Fatal("invalid source could clear a prior incident")
		}
		requireAlertOmits(t, alert, "private-host-canary", "private-clock-canary", "private-boot-canary", "private-error-canary", "NaN")
	}
	for _, payload := range []string{strings.TrimSuffix(base, "\n"), base + "{}\n", strings.Replace(base, `"status":"snapshot"`, `"status":"snapshot","status":"snapshot"`, 1)} {
		alerts := pgFreezeRunForTest(t, now, payload)
		if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 {
			t.Fatal("truncated or duplicated frame supplied cluster authority")
		}
		requireAlertClass(t, alerts, "pg-freeze-unobservable")
	}
}

// Fixed source uncertainty cannot certify a quiet window, and unknown event
// membership cannot be discarded while calculating a positive median.
func TestPgFreezeSourceCoverageFlags(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, flag := range []string{"short-read", "snapshot-changed", "partial-last", "record-schema", "record-clock", "future-record", "duration-schema", "duration-range", "duration-origin", "record-cap", "row-cap"} {
		alerts := pgFreezeRunForTest(t, now, pgFreezeTestPayload(t, now, pgFreezeTestCluster(now.Add(-time.Minute), 250, 3000000000), flag))
		if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 {
			t.Fatalf("%s lost cluster membership uncertainty", flag)
		}
		requireAlertClass(t, alerts, "pg-freeze-unobservable")
	}
	for _, flag := range []string{"tail-capped", "partial-first", "path-changed", "record-order", "source-index-changed"} {
		alerts := pgFreezeRunForTest(t, now, pgFreezeTestPayload(t, now, nil, flag))
		requireAlertClass(t, alerts, "pg-freeze-unobservable")
	}
}

// Source clock boundaries are deterministic; local completion is not a new
// timestamp for an old snapshot, and a forward clock is not coverage.
func TestPgFreezeSourceClockBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{-31 * time.Second, -30 * time.Second, 30 * time.Second, 31 * time.Second} {
		payload := pgFreezeTestPayload(t, now.Add(offset), nil)
		alerts := pgFreezeRunForTest(t, now, payload)
		if (len(alerts) == 0) != (offset >= -30*time.Second && offset <= 30*time.Second) {
			t.Fatal("source freshness boundary changed")
		}
	}
}

// Native error identity stays reachable while public findings discard text.
type pgFreezeTestError struct{}

func (*pgFreezeTestError) Error() string { return "private-source-error-canary" }

func TestPgFreezeCanceledSourcePreservesCause(t *testing.T) {
	for _, failed := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cause := &pgFreezeTestError{}
		calls := 0
		settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			cancel()
			if failed {
				return "private-source-output-canary", cause
			}
			return "private-source-output-canary", nil
		}})
		alerts, err := pgFreezeSignalForTest(t).Run(ctx, settings)
		var typed *pgFreezeTestError
		if calls != 1 || len(alerts) != 0 || !errors.Is(err, context.Canceled) || (failed && (!errors.Is(err, cause) || !errors.As(err, &typed))) {
			t.Fatal("canceled duration reader lost context/source cause or published")
		}
	}
}

// Ambiguous, absent and explicitly excluded inventory cannot start a read.
func TestPgFreezeInventoryPreventsUnownedRead(t *testing.T) {
	for _, mode := range []string{"missing", "ambiguous", "excluded"} {
		settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			t.Fatal("unowned duration source was contacted")
			return "", nil
		}})
		switch mode {
		case "missing":
			settings.Hosts = nil
		case "ambiguous":
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-2", Roles: []string{"pg-primary"}})
		case "excluded":
			settings.ExcludedHosts = []string{"pg-1"}
		}
		alerts, err := pgFreezeSignalForTest(t).Run(context.Background(), settings)
		if err != nil {
			t.Fatal("inventory uncertainty returned an execution error")
		}
		found := false
		for _, alert := range alerts {
			found = found || alert.unobservable
		}
		if !found {
			t.Fatal("inventory uncertainty supplied complete absence")
		}
	}
}

// The actual cadence/notifier cannot recover through a quiet partial frame.
func TestPgFreezeNotifierRequiresCompleteQuietWindow(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	payload := ""
	settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }})
	settings.Now = func() time.Time { return now }
	signal := pgFreezeSignalForTest(t)
	gate := newCadenceAlertGate()
	notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
	for run := 0; run < 5; run++ {
		now = now.Add(15 * time.Minute)
		switch run {
		case 0:
			payload = pgFreezeTestPayload(t, now, pgFreezeTestCluster(now.Add(-time.Minute), 250, 3000000000))
		case 1, 2:
			payload = pgFreezeTestPayload(t, now, nil, "tail-capped")
		default:
			payload = pgFreezeTestPayload(t, now, nil)
		}
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("notifier source failed")
		}
		ready, held := gate.split(signal, alerts)
		transitions := notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
		matches := []slackTransition{}
		for _, transition := range transitions {
			if transition.alert.Class == "pg-freeze-clusters" {
				matches = append(matches, transition)
			}
		}
		if run == 0 {
			if len(matches) != 1 || matches[0].kind != slackRaised {
				t.Fatal("qualified duration cluster was not raised")
			}
		} else if run < 4 {
			if len(matches) != 0 {
				t.Fatal("partial or single quiet window cleared the duration incident")
			}
		} else if len(matches) != 1 || matches[0].kind != slackCleared {
			t.Fatal("two complete quiet observed windows did not recover")
		}
	}
}

// The public source interface receives the finite native observation budget.
type pgFreezeTimedTestSource struct {
	*syntheticSource
	payload  string
	calls    int
	timeout  time.Duration
	deadline bool
}

func (self *pgFreezeTimedTestSource) HostTimeout(ctx context.Context, host HostSettings, command string, timeout time.Duration) (string, error) {
	self.calls++
	self.timeout = timeout
	_, self.deadline = ctx.Deadline()
	return self.payload, nil
}

func TestPgFreezeTimedTransportAndFailurePrivacy(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	source := &pgFreezeTimedTestSource{syntheticSource: &syntheticSource{}, payload: pgFreezeTestPayload(t, now, nil)}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	alerts, err := pgFreezeSignalForTest(t).Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || source.calls != 1 || source.timeout != 25*time.Second || !source.deadline {
		t.Fatal("duration reader lost its finite composed transport budget")
	}
	calls := 0
	settings = syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		calls++
		return "private-failed-output-canary", &pgFreezeTestError{}
	}})
	alerts, err = pgFreezeSignalForTest(t).Run(context.Background(), settings)
	if err != nil || calls != 1 {
		t.Fatal("failed log source retried or lost visibility")
	}
	alert := requireAlertClass(t, alerts, "pg-freeze-unobservable")
	pgFreezeRequirePrivate(t, alerts)
	requireAlertOmits(t, alert, "private-source-error-canary", "private-failed-output-canary")
}

// A bounded tail with nonchronological records has no safe unseen-prefix
// ordering assumption; sorting cannot create missing membership authority.
func TestPgFreezeCappedOutOfOrderCannotSupplyMedian(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	events := pgFreezeTestCluster(now.Add(-time.Minute), 250, 3000000000)
	events[0], events[len(events)-1] = events[len(events)-1], events[0]
	payload := pgFreezeEditPayload(t, pgFreezeTestPayload(t, now, events), func(frame map[string]any) {
		frame["file_before"].([]any)[2] = "40005000"
		frame["file_after"].([]any)[2] = "40005000"
		frame["read"] = []string{"5000", "40000000"}
		for _, row := range frame["rows"].([]any) {
			cells := row.([]any)
			var offset int64
			fmt.Sscan(cells[0].(string), &offset)
			cells[0] = fmt.Sprint(offset + 5000)
		}
	})
	alerts := pgFreezeRunForTest(t, now, payload)
	if requireAlertClassCount(alerts, "pg-freeze-clusters") != 0 {
		t.Fatal("nonchronological capped tail supplied complete median membership")
	}
	requireAlertClass(t, alerts, "pg-freeze-unobservable")
}

// A recent/empty prefix never proves a full historical window. The exact
// 40MB boundary is complete only when the file itself fits, not a clipped tail.
func TestPgFreezeWindowCoverageAndByteBoundary(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	base := pgFreezeTestPayload(t, now, nil)
	for _, edit := range []func(map[string]any){
		func(frame map[string]any) { frame["span"] = []string{"0", "0"} },
		func(frame map[string]any) {
			frame["span"] = []string{fmt.Sprint(now.Add(-14 * time.Minute).UnixMicro()), fmt.Sprint(now.Add(-time.Minute).UnixMicro())}
		},
		func(frame map[string]any) { frame["rows"] = nil },
		func(frame map[string]any) { delete(frame, "flags") },
		func(frame map[string]any) { frame["private-sql-canary"] = "private-sql-canary" },
	} {
		alerts := pgFreezeRunForTest(t, now, pgFreezeEditPayload(t, base, edit))
		requireAlertClass(t, alerts, "pg-freeze-unobservable")
	}
	for _, size := range []int64{40000000, 40000001} {
		payload := pgFreezeEditPayload(t, base, func(frame map[string]any) {
			frame["file_before"].([]any)[2] = fmt.Sprint(size)
			frame["file_after"].([]any)[2] = fmt.Sprint(size)
			frame["read"] = []string{fmt.Sprint(size - 40000000), "40000000"}
		})
		alerts := pgFreezeRunForTest(t, now, payload)
		if (len(alerts) == 0) != (size == 40000000) {
			t.Fatal("exact file/capped-tail byte boundary changed")
		}
	}
}
