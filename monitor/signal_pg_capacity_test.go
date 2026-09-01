package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func syntheticPgCapacityRows(total string, active string, idle string, idleInTx string) []Row {
	return []Row{
		{"summary", "1024", "3", "0", "1021", total, active, idle, idleInTx, "239674kB", "256GB"},
		{"owner", "connect", "bringyour", "10.0.0.31", "idle", "300", "Client:ClientRead", "540", "3550", "", ""},
		{"owner", "taskworker", "bringyour", "10.0.0.32", "active", "80", "IO:DataFileRead", "3", "1200", "", ""},
	}
}

func TestPgCapacitySignalHealthyWithHeadroom(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{
			"pg_settings",
			"superuser_reserved_connections",
			"reserved_connections",
			"backend_type = 'client backend'",
			"LIMIT 10",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("pg-capacity query missing %q:\n%s", want, query)
			}
		}
		return syntheticPgCapacityRows("400", "31", "340", "7"), nil
	}}
	alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy PostgreSQL capacity produced alerts: %+v", alerts)
	}
}

func TestPgCapacitySignalWarnsWithBoundedOwnerEvidence(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return syntheticPgCapacityRows("800", "230", "540", "7"), nil
	}}
	alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pg-client-capacity")
	if alert.Severity != SeverityWarn || alert.SignalNumber != "1.3a" || alert.SignalKey != "pg-capacity" {
		t.Fatalf("capacity alert identity = %+v", alert)
	}
	for _, want := range []string{
		"uses 78.4%",
		"normal_role_ceiling=1021",
		"normal_role_slots_remaining=221",
		"work_mem=239674kB",
		"shared_buffers=256GB",
		"application=connect role=bringyour address=10.0.0.31 state=idle clients=300 waits=Client:ClientRead oldest_state_s=540 oldest_backend_s=3550",
		"bounded to ten; no query text",
		"Independent PgBouncer processes",
		"SHOW POOLS where administrative access exists",
		"60-66-second COMMIT latency",
		"replacement overlap",
		"idle recovery cohort did not establish retention",
		"Do not raise max_connections first",
		"large work_mem",
		"diagnostic amplification",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, want) {
			t.Fatalf("capacity warning missing %q:\n%s", want, markdown)
		}
	}
}

func TestPgCapacitySignalPagesNearExhaustion(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return syntheticPgCapacityRows("940", "746", "170", "12"), nil
	}}
	alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pg-client-capacity")
	if alert.Severity != SeverityPage || alert.Sustain != 1 {
		t.Fatalf("near-exhaustion alert = %+v", alert)
	}
	if !strings.Contains(alert.Observed, "normal_role_slots_remaining=81") {
		t.Fatalf("near-exhaustion observed = %q", alert.Observed)
	}
}

func TestPgCapacitySignalClassifiesDirectSlotRejection(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, errors.New("psql: FATAL: sorry, too many clients already")
	}}
	alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "pg-client-capacity")
	if alert.Severity != SeverityPage {
		t.Fatalf("direct rejection severity = %q", alert.Severity)
	}
	for _, want := range []string{
		"rejected the direct capacity observation",
		"direct_connection_result=too_many_clients_already",
		"canonical `too many clients already`",
		"capacity_values=unavailable",
		"not a count of unique rejected PostgreSQL sessions",
		"60-66-second COMMIT latency",
		"replacement",
		"headroom stays above 25%",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, want) {
			t.Fatalf("direct rejection alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestPgCapacitySignalPreservesUnrelatedQueryFailure(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, errors.New("synthetic route unavailable")
	}}
	if _, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source)); err == nil ||
		!strings.Contains(err.Error(), "route unavailable") {
		t.Fatalf("unrelated capacity query error = %v", err)
	}
}

func TestPgCapacitySignalRejectsMalformedSummary(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"owner", "connect", "bringyour", "10.0.0.31", "idle", "4", "Client:ClientRead", "10", "20", "", ""}}, nil
	}}
	if _, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source)); err == nil ||
		!strings.Contains(err.Error(), "no summary row") {
		t.Fatalf("missing capacity summary error = %v", err)
	}
}
