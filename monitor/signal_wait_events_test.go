package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestWaitEventsSignalSyntheticWALWaitCluster(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"LWLock", "WALWrite", "12", "75", "INSERT INTO hot_table"}}, nil
	}}
	alerts, err := NewWaitEventsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "wait-event-cluster")
	if alert.Frame != "LWLock:WALWrite" {
		t.Fatalf("frame = %q", alert.Frame)
	}
}

func TestWaitEventsSignalAgedSingletonIncludesAttribution(t *testing.T) {
	const sample = "SELECT payment_id FROM temp_account_payment"
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if !strings.Contains(query, "(array_agg(pid ORDER BY query_start, pid))[1]") {
			t.Fatalf("wait query does not preserve the oldest waiter's PID:\n%s", query)
		}
		return []Row{{"IO", "DataFileRead", "1", "71", sample, "8123", "9911", "taskworker", "127.0.0.1/32"}}, nil
	}}
	alerts, err := NewWaitEventsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "wait-event-cluster")
	for name, check := range map[string]struct {
		got  string
		want string
	}{
		"baseline count branch": {alert.Baseline, "five active client backends"},
		"baseline age branch":   {alert.Baseline, "more than one minute"},
		"sample attribution":    {alert.Evidence, sample},
		"pid attribution":       {alert.Evidence, "pid=8123"},
		"query attribution":     {alert.Evidence, "query_id=9911"},
		"client attribution":    {alert.Evidence, "client=127.0.0.1/32"},
		"read mechanism":        {alert.Mechanism, "relation data page"},
		"bounded action":        {alert.Action, "Do not cancel one bounded read"},
	} {
		if !strings.Contains(check.got, check.want) {
			t.Fatalf("%s missing %q: %q", name, check.want, check.got)
		}
	}
}

func TestWaitEventsSignalExplainsClientWriteBackpressure(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"Client", "ClientWrite", "1", "96", "SELECT result FROM large_plan"}}, nil
	}}
	alerts, err := NewWaitEventsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "wait-event-cluster")
	for name, check := range map[string]struct {
		got  string
		want string
	}{
		"mechanism": {alert.Mechanism, "client is not currently reading"},
		"action":    {alert.Action, "result-consumption path"},
	} {
		if !strings.Contains(check.got, check.want) {
			t.Fatalf("%s missing %q: %q", name, check.want, check.got)
		}
	}
}

func TestWaitEventsSignalAttributesTransferEscrowReindexExtension(t *testing.T) {
	const sample = "REINDEX TABLE CONCURRENTLY transfer_escrow"
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"IO", "DataFileExtend", "1", "190", sample, "3597393", "unknown", "taskworker", "192.0.2.10"}}, nil
	}}
	alerts, err := NewWaitEventsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "wait-event-cluster").Markdown()
	for _, want := range []string{
		"very large, high-churn transfer_escrow table",
		"clustered WAL and storage work",
		"PgBouncer only exposes the resulting queueing",
		"Do not interrupt the protected in-progress transfer_escrow rebuild",
		"excludes transfer_escrow from full-table reindex",
		"reindex-debris",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("transfer_escrow DataFileExtend alert missing %q: %s", want, markdown)
		}
	}
}

func TestWaitEventsSignalClientReadRequiresOldWaiter(t *testing.T) {
	tests := []struct {
		name      string
		oldest    string
		wantAlert bool
	}{
		{name: "fresh protocol handoffs", oldest: "0", wantAlert: false},
		{name: "client stalled beyond one minute", oldest: "61", wantAlert: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
				return []Row{{"Client", "ClientRead", "7", test.oldest, "BEGIN ISOLATION LEVEL REPEATABLE READ"}}, nil
			}}
			alerts, err := NewWaitEventsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			gotAlert := false
			for _, alert := range alerts {
				gotAlert = gotAlert || alert.Class == "wait-event-cluster"
			}
			if gotAlert != test.wantAlert {
				t.Fatalf("ClientRead alert = %t, want %t: %+v", gotAlert, test.wantAlert, alerts)
			}
		})
	}
}

func TestWaitEventsSignalSyntheticConcurrentReindexBound(t *testing.T) {
	tests := []struct {
		name      string
		row       Row
		wantAlert bool
	}{
		{
			name:      "expected concurrent reindex inside two hour bound",
			row:       Row{"Lock", "virtualxid", "1", "533", "REINDEX TABLE CONCURRENTLY pending_task"},
			wantAlert: false,
		},
		{
			name:      "concurrent reindex at two hour bound",
			row:       Row{"Lock", "virtualxid", "1", "7200", "REINDEX TABLE CONCURRENTLY pending_task"},
			wantAlert: true,
		},
		{
			name:      "unrelated virtual xid waiter",
			row:       Row{"Lock", "virtualxid", "1", "533", "ALTER TABLE pending_task ADD COLUMN surprise int"},
			wantAlert: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
				return []Row{test.row}, nil
			}}
			alerts, err := NewWaitEventsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			gotAlert := false
			for _, alert := range alerts {
				gotAlert = gotAlert || alert.Class == "wait-event-cluster"
			}
			if gotAlert != test.wantAlert {
				t.Fatalf("wait alert = %t, want %t: %+v", gotAlert, test.wantAlert, alerts)
			}
		})
	}
}
