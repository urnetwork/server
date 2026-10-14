package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// One synthetic block's reconcile values; a negative unheld omits the gauge.
type connectionReconcileFixtureBlock struct {
	host        string
	block       string
	unheld      float64
	retired     float64
	okPasses    float64
	errorPasses float64
	// emitted only when positive
	failedRetires float64
	busyRetires   float64
}

// A Mimir vector for the probe's query, sampled at now.
func connectionReconcileFixtureJSON(t testing.TB, now time.Time, blocks []connectionReconcileFixtureBlock) string {
	t.Helper()
	result := []map[string]any{}
	sample := func(host, block, class string, value float64) {
		result = append(result, map[string]any{
			"metric": map[string]string{"host": host, "block": block, "monitor_metric": class},
			"value":  []any{float64(now.Unix()), fmt.Sprintf("%g", value)},
		})
	}
	for _, block := range blocks {
		if 0 <= block.unheld {
			sample(block.host, block.block, "unheld", block.unheld)
		}
		sample(block.host, block.block, "retired", block.retired)
		sample(block.host, block.block, "ok_passes", block.okPasses)
		sample(block.host, block.block, "error_passes", block.errorPasses)
		if 0 < block.failedRetires {
			sample(block.host, block.block, "failed_retires", block.failedRetires)
		}
		if 0 < block.busyRetires {
			sample(block.host, block.block, "busy_retires", block.busyRetires)
		}
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// Runs the signal over blocks, checking the query it sends.
func runConnectionReconcileFixture(t *testing.T, blocks []connectionReconcileFixtureBlock) Alerts {
	t.Helper()
	return runConnectionReconcileFixtureQuerying(t, blocks, nil)
}

// Runs the signal over blocks, checking the query it sends for wantQueried
// too.
func runConnectionReconcileFixtureQuerying(t *testing.T, blocks []connectionReconcileFixtureBlock, wantQueried []string) Alerts {
	t.Helper()
	now := time.Date(2026, 10, 14, 8, 15, 0, 0, time.UTC)
	payload := connectionReconcileFixtureJSON(t, now, blocks)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range append([]string{
			"urnetwork_connect_connection_reconcile_unheld",
			"urnetwork_connect_connection_reconcile_retired_total",
			"urnetwork_connect_connection_reconcile_passes_total",
			"result%3D%22error%22",
			"%5B15m%5D",
			"%22synthetic%22",
			"monitor_metric",
		}, wantQueried...) {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("connection reconcile query missing %q: %s", want, command)
			}
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := NewConnectionReconcileSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// A few unheld rows of a passing race, on a fleet whose passes complete, are
// healthy.
func TestConnectionReconcileSignalSyntheticHealthyFleet(t *testing.T) {
	alerts := runConnectionReconcileFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect-a", block: "g1", unheld: 0, okPasses: 3},
		{host: "connect-b", block: "g2", unheld: 12, okPasses: 3},
	})
	if len(alerts) != 0 {
		t.Fatalf("healthy reconcile alerted: %+v", alerts)
	}
}

// After an outage a block holds thousands of unheld rows while it retires
// them; that is recovery, not a stuck reconcile.
func TestConnectionReconcileSignalSyntheticDrainingBlockIsHealthy(t *testing.T) {
	alerts := runConnectionReconcileFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect-a", block: "g1", unheld: 5000, retired: 2048, okPasses: 10},
	})
	if len(alerts) != 0 {
		t.Fatalf("draining reconcile alerted: %+v", alerts)
	}
}

// Unheld rows that nothing retires are the phantom rows of 2026-10-14: the
// signal warns, names the block, and forbids manual row updates.
func TestConnectionReconcileSignalSyntheticStuckBlock(t *testing.T) {
	alerts := runConnectionReconcileFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect-a", block: "g1", unheld: 13922, okPasses: 3},
		{host: "connect-b", block: "g2", unheld: 0, okPasses: 3},
	})
	alert := requireAlertClass(t, alerts, "connection-reconcile-stuck")
	if alert.SignalNumber != "2.16a" || alert.SignalKey != "connection-reconcile" || alert.Sustain != 2 {
		t.Fatalf("wrong stuck alert identity: %+v", alert)
	}
	for _, want := range []string{
		"1 Connect block(s) hold at least 256 connected rows",
		"connect-a/g1 unheld=13922 retired_15m=0",
		"handler expiry never reaches them",
		"Do not update connection rows",
		"SIGNALS.md §2.16a",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("stuck alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "connect-b/g2") {
		t.Fatalf("stuck alert names a healthy block:\n%s", alert.Markdown())
	}
	for _, other := range alerts {
		if other.Class == "connection-reconcile-failing" || other.Class == "connection-reconcile-unobservable" {
			t.Fatalf("a stuck block also raised %s", other.Class)
		}
	}
}

// Every pass raising on a block is its own warning.
func TestConnectionReconcileSignalSyntheticFailingBlock(t *testing.T) {
	alerts := runConnectionReconcileFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect-a", block: "g1", unheld: 10, errorPasses: 6},
	})
	alert := requireAlertClass(t, alerts, "connection-reconcile-failing")
	for _, want := range []string{
		"the connection reconcile failed in 15m on 1 Connect block(s): every pass raised",
		"connect-a/g1 unheld=10 retired_15m=0 ok_passes=0 error_passes=6",
		"SIGNALS.md §2.16a",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("failing alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	for _, other := range alerts {
		if other.Class == "connection-reconcile-stuck" {
			t.Fatal("ten unheld rows raised a stuck alert")
		}
	}
}

// An absent gauge cannot be read as zero phantom rows.
func TestConnectionReconcileSignalSyntheticAbsentGaugeIsUnobservable(t *testing.T) {
	alerts := runConnectionReconcileFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect-a", block: "g1", unheld: -1, okPasses: 3},
	})
	alert := requireAlertClass(t, alerts, "connection-reconcile-unobservable")
	if !strings.Contains(alert.Markdown(), "visibility_reason=no_reconcile_gauge") {
		t.Fatalf("unobservable alert has no structural reason:\n%s", alert.Markdown())
	}
}

// A failed retire fails only its row, and its pass completes. Retires that
// keep failing on a block whose passes complete are a failing reconcile too,
// below the stuck floor; a block whose retires sometimes fail but that
// retires rows is healthy.
func TestConnectionReconcileSignalSyntheticFailingRetires(t *testing.T) {
	alerts := runConnectionReconcileFixtureQuerying(t, []connectionReconcileFixtureBlock{
		{host: "connect-a", block: "g1", unheld: 40, okPasses: 60, failedRetires: 120, busyRetires: 4},
		{host: "connect-b", block: "g2", unheld: 3, okPasses: 3, retired: 2, failedRetires: 1},
	}, []string{
		"urnetwork_connect_connection_reconcile_unretired_total",
		"reason%3D%22failed%22",
		"reason%3D%22busy%22",
	})
	alert := requireAlertClass(t, alerts, "connection-reconcile-failing")
	for _, want := range []string{
		"connect-a/g1 unheld=40 retired_15m=0 ok_passes=60 error_passes=0 failed_retires=120 busy_retires=4",
		"SIGNALS.md §2.16a",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("failing retires alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "connect-b/g2") {
		t.Fatalf("failing retires alert names a block that retires rows:\n%s", alert.Markdown())
	}
	for _, other := range alerts {
		if other.Class == "connection-reconcile-stuck" || other.Class == "connection-reconcile-unobservable" {
			t.Fatalf("failing retires also raised %s", other.Class)
		}
	}
}
