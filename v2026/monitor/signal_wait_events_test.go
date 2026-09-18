package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestWaitEventsSignalSyntheticWALWaitCluster(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"LWLock", "WALWrite", "12", "75", "INSERT INTO hot_table", "8123", "unknown", "connect", "local"}}, nil
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
		for _, want := range []string{"client_addr::text", "oldest_client_address"} {
			if !strings.Contains(query, want) {
				t.Fatalf("wait query does not collect the transient owner input with %q:\n%s", want, query)
			}
		}
		return []Row{{"IO", "DataFileRead", "1", "71", sample, "8123", "9911", "taskworker", "192.0.2.44/32"}}, nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name:       "worker-synthetic",
		LANAddress: "192.0.2.44",
		Roles:      []string{"services"},
	})
	alerts, err := NewWaitEventsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "wait-event-cluster")
	for name, check := range map[string]struct {
		got  string
		want string
	}{
		"baseline count branch":  {alert.Baseline, "five active client backends"},
		"baseline age branch":    {alert.Baseline, "more than one minute"},
		"sample attribution":     {alert.Evidence, sample},
		"pid attribution":        {alert.Evidence, "pid=8123"},
		"query attribution":      {alert.Evidence, "query_id=9911"},
		"client attribution":     {alert.Evidence, "client_owner=worker-synthetic"},
		"read mechanism":         {alert.Mechanism, "relation data page"},
		"bounded action":         {alert.Action, "Do not cancel one bounded read"},
		"family recurrence":      {alert.Context, "not persistence of the same backend"},
		"wait-family blind spot": {alert.Context, "changes wait family"},
	} {
		if !strings.Contains(check.got, check.want) {
			t.Fatalf("%s missing %q: %q", name, check.want, check.got)
		}
	}
	if strings.Contains(alert.Markdown(), "192.0.2.44/32") {
		t.Fatalf("wait-event alert leaked exact client address: %s", alert.Markdown())
	}
}

func TestWaitEventsSignalDoesNotRenderMalformedClientAddress(t *testing.T) {
	const malformedAddress = "203.0.113.44/99"
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"IO", "DataFileRead", "1", "71", "SELECT 1", "8123", "unknown", "taskworker", malformedAddress}}, nil
	}}
	alerts, err := NewWaitEventsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "wait-event-cluster").Markdown()
	if !strings.Contains(markdown, "client_owner=unmapped-service-client") {
		t.Fatalf("wait-event alert omitted malformed-address fallback: %s", markdown)
	}
	if strings.Contains(markdown, malformedAddress) {
		t.Fatalf("wait-event alert leaked malformed client address: %s", markdown)
	}
}

func TestWaitEventsSignalMalformedRowDoesNotEchoAddress(t *testing.T) {
	const address = "198.51.100.44/32"
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"IO", "DataFileRead", "1", "71", address}}, nil
	}}
	_, err := NewWaitEventsSignal().Run(context.Background(), syntheticSettings(source))
	if err == nil {
		t.Fatal("malformed wait-event row was accepted")
	}
	if strings.Contains(err.Error(), address) {
		t.Fatalf("malformed wait-event error leaked client address: %v", err)
	}
}

func TestWaitEventsSignalExplainsClientWriteBackpressure(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"Client", "ClientWrite", "1", "96", "SELECT result FROM large_plan", "8123", "unknown", "connect", "local"}}, nil
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
				return []Row{{"Client", "ClientRead", "7", test.oldest, "BEGIN ISOLATION LEVEL REPEATABLE READ", "8123", "unknown", "connect", "local"}}, nil
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
			row:       Row{"Lock", "virtualxid", "1", "533", "REINDEX TABLE CONCURRENTLY pending_task", "8123", "unknown", "taskworker", "local"},
			wantAlert: false,
		},
		{
			name:      "concurrent reindex at two hour bound",
			row:       Row{"Lock", "virtualxid", "1", "7200", "REINDEX TABLE CONCURRENTLY pending_task", "8123", "unknown", "taskworker", "local"},
			wantAlert: true,
		},
		{
			name:      "unrelated virtual xid waiter",
			row:       Row{"Lock", "virtualxid", "1", "533", "ALTER TABLE pending_task ADD COLUMN surprise int", "8123", "unknown", "taskworker", "local"},
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
