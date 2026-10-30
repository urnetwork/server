// Runs the reclaimable bloat reads (SIGNALS.md §2.4k) on the PostgreSQL 18
// TestEnv against the server's own weekly measurement: a churned queue's index
// fires the signal, and a rewrite clears it before the next measurement.
package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Both reads, through the public signal, on the test database.
func tableBloatNativeRun(t testing.TB, ctx context.Context, signal Signal) Alerts {
	t.Helper()
	source := &contractPartitionNativeSource{syntheticSource: &syntheticSource{}, t: t}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: contractPartitionNativeHost, Roles: []string{"pg-primary"}}}
	settings.Now = time.Now
	alerts, err := signal.Run(ctx, settings)
	if err != nil || source.calls != 2 {
		t.Fatalf("table bloat did not read the database twice: %v", err)
	}
	return alerts
}

// The queue's alert, or none.
func tableBloatNativeQueueAlert(alerts Alerts) *Alert {
	for _, alert := range alerts {
		if alert.Class == "table-bloat" && alert.Target == contractPartitionNativeHost+"/synthetic_bloat_native_queue" {
			return &alert
		}
	}
	return nil
}

func TestTableBloatNativeFiresOnAChurnedIndexAndCreditsARewrite(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultTableBloatSettings()
		// the fixture's index is megabytes, not gigabytes
		settings.MinReclaimableBytes = 1024 * 1024
		signal := NewTableBloatSignalWithSettings(settings)

		// before any measurement the signal cannot rule bloat out
		alerts := tableBloatNativeRun(t, ctx, signal)
		if len(alerts) != 1 || alerts[0].Class != "table-bloat-unobservable" || !strings.Contains(alerts[0].Observed, "reason=no-measurement") {
			t.Fatalf("before the first measurement: %+v", alerts)
		}

		contractPartitionNativeExec(t, ctx, `
			CREATE TABLE synthetic_bloat_native_queue (
				task_id bigint NOT NULL PRIMARY KEY,
				available_at timestamp NOT NULL,
				payload bigint NOT NULL
			) WITH (autovacuum_enabled = false);
			CREATE INDEX synthetic_bloat_native_queue_available_at ON synthetic_bloat_native_queue (available_at)
		`)
		// time-ordered keys; one in fifty stays queued
		contractPartitionNativeExec(t, ctx, `
			INSERT INTO synthetic_bloat_native_queue (task_id, available_at, payload)
			SELECT n, timestamp '2026-10-01' + n * interval '1 second', n
			FROM generate_series(1, 200000) n
		`)
		contractPartitionNativeExec(t, ctx, `DELETE FROM synthetic_bloat_native_queue WHERE task_id % 50 <> 0`)
		contractPartitionNativeExec(t, ctx, `VACUUM (ANALYZE) synthetic_bloat_native_queue`)

		measureSettings := server.DefaultDbBloatMeasureSettings()
		if report, _ := server.MeasureDbBloat(ctx, server.NowUtc(), server.DbBloatMeasureCursor{}, measureSettings); report.Stopped != "" {
			t.Fatalf("measurement = %+v", *report)
		}
		alerts = tableBloatNativeRun(t, ctx, signal)
		queue := tableBloatNativeQueueAlert(alerts)
		if queue == nil || !strings.Contains(queue.Observed, "synthetic_bloat_native_queue_available_at index btree_estimate") ||
			!strings.Contains(queue.Observed, "synthetic_bloat_native_queue heap statistics") {
			t.Fatalf("the churned queue did not alert: %+v", alerts)
		}
		for _, alert := range alerts {
			if alert.Class == "table-bloat-unobservable" {
				t.Fatalf("a fresh measurement was unknown: %s", alert.Observed)
			}
		}

		// a rewrite returns the space; the next read credits it at once
		contractPartitionNativeExec(t, ctx, `VACUUM (FULL) synthetic_bloat_native_queue`)
		if queue := tableBloatNativeQueueAlert(tableBloatNativeRun(t, ctx, signal)); queue != nil {
			t.Fatalf("the rewritten queue still alerts: %s", queue.Observed)
		}
	})
}
