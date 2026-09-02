package monitor

import (
	"context"
	"strings"
	"testing"
)

func poolRetentionRows(values ...string) []Row {
	return []Row{values}
}

func TestPoolRetentionSignalSyntheticRetainedFleet(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{
			"client_addr <<= inet '127.0.0.0/8'",
			"client_addr = inet '::1'",
			"interval '600 seconds'",
			"backend_type = 'client backend'",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("pool-retention query missing %q:\n%s", want, query)
			}
		}
		return poolRetentionRows("1021", "714", "608", "600", "4", "4", "384", "1700"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pgbouncer-idle-retention")
	if alert.SignalNumber != "1.3b" || alert.SignalKey != "pool-retention" || alert.Sustain != 10 {
		t.Fatalf("pool-retention identity = %+v", alert)
	}
	for _, want := range []string{
		"600 idle loopback backends consume 58.8%",
		"loopback_idle_at_least_600s=384",
		"32 live PgBouncer shards",
		"server_idle_timeout=0",
		"608 total",
		"Zero disables idle draining",
		"Xops descendant of 31ae1e7",
		"run-dbs.sh --pgbouncer-only",
		"there is no separate run-pgbouncer.sh",
		"requires their PIDs to remain unchanged",
		"add database hardware",
		"Do not restart PostgreSQL/PgBouncer",
		"server_idle_timeout=600",
		"256-connection warm floor",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, want) {
			t.Fatalf("pool-retention alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestPoolRetentionSignalSyntheticYoungCohortPreservesDiscriminator(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1021", "710", "610", "600", "5", "5", "0", "175"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pgbouncer-idle-retention")
	for _, want := range []string{
		"No loopback idle backend in this snapshot is yet continuously idle for 600 seconds",
		"young post-peak or recurring-demand cohort",
		"observe one complete timeout interval",
		"not an assumption",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("young-cohort alert missing %q: %s", want, alert.Markdown())
		}
	}
}

func TestPoolRetentionSignalSyntheticHealthyReserve(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1021", "360", "270", "250", "12", "8", "0", "590"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy pool reserve produced alerts: %+v", alerts)
	}
}

func TestPoolRetentionSignalRejectsInconsistentSummary(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1021", "300", "200", "190", "20", "1", "0", "10"), nil
	}}
	if _, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source)); err == nil ||
		!strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("inconsistent pool-retention error = %v", err)
	}
}
