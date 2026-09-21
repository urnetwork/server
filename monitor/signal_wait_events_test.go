package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A one-shot retains count-only and age-only violations without manufacturing
// the standing watcher's two-observation evidence.
func TestWaitEventsOneShotDoesNotClaimCadenceRecurrence(t *testing.T) {
	for _, test := range []struct {
		wait   string
		count  string
		oldest string
	}{
		{wait: "BgworkerShutdown", count: "5", oldest: "0"},
		{wait: "MessageQueueInternal", count: "6", oldest: "0"},
		{wait: "SyntheticWait", count: "1", oldest: "61"},
	} {
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if !strings.Contains(query, "HAVING count(*) >= 5 OR max(clock_timestamp()-query_start) > interval '1 minute'") {
				t.Fatal("wait-event count or age threshold changed")
			}
			return []Row{{"IPC", test.wait, test.count, test.oldest, "SELECT synthetic_value FROM synthetic_work", "4242", "77", "synthetic-worker", "local"}}, nil
		}}
		signal := NewWaitEventsSignal()
		alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 1 {
			t.Fatalf("%s one-shot returned %d alerts, want one", test.wait, len(alerts))
		}
		alert := alerts[0]
		if alert.Class != "wait-event-cluster" || alert.Frame != "IPC:"+test.wait || alert.Severity != SeverityWarn || alert.Sustain != 2 || signal.Cadence() != 5*time.Minute {
			t.Fatalf("%s changed wait-event identity or gating: %+v", test.wait, alert)
		}
		if strings.Contains(alert.Markdown(), "recurred across the cadence") {
			t.Fatalf("%s one-shot claimed unobserved cadence recurrence: %s", test.wait, alert.Markdown())
		}
		if !strings.Contains(alert.Mechanism, "in this observation") || !strings.Contains(alert.Context, "One-shot observations bypass sustain and do not prove recurrence") {
			t.Fatalf("%s omitted the one-shot evidence boundary: %s", test.wait, alert.Markdown())
		}
	}
}

// Empty source results remain healthy; an incomplete source row remains a
// visibility failure rather than becoming an invented wait diagnosis.
func TestWaitEventsOneShotHealthyAndIncompleteEvidence(t *testing.T) {
	for _, test := range []struct {
		name    string
		rows    []Row
		wantErr bool
	}{
		{name: "healthy", rows: nil},
		{name: "incomplete", rows: []Row{{"IPC", "SyntheticWait"}}, wantErr: true},
	} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return test.rows, nil }}
		alerts, err := NewWithSignals(syntheticSettings(source), NewWaitEventsSignal()).Run(context.Background())
		if (err != nil) != test.wantErr {
			t.Fatalf("%s error = %v, want error=%t", test.name, err, test.wantErr)
		}
		if !test.wantErr && len(alerts) != 0 {
			t.Fatalf("healthy observation emitted alerts: %+v", alerts)
		}
		if test.wantErr && (len(alerts) != 1 || alerts[0].SignalID != "monitor/visibility") {
			t.Fatalf("incomplete observation did not remain a visibility failure: %+v", alerts)
		}
	}
}

// The real loop still waits for two consecutive family observations and resets
// on a healthy observation. Explicit ticks replace the five-minute wall clock.
func TestWaitEventsRunLoopRetainsConsecutiveObservationGate(t *testing.T) {
	row := Row{"IPC", "BgworkerShutdown", "5", "0", "SELECT synthetic_value FROM synthetic_work", "4242", "77", "synthetic-worker", "local"}
	observations := [][]Row{{row}, {row}, nil, {row}, {row}}
	nextObservation := 0
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		if nextObservation == len(observations) {
			return nil, fmt.Errorf("unexpected observation after the last explicit tick")
		}
		rows := observations[nextObservation]
		nextObservation++
		return rows, nil
	}}
	monitor := NewWithSignals(syntheticSettings(source), NewWaitEventsSignal())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticks := make(chan time.Time)
	handled := make(chan Alerts, 1)
	runErr := make(chan error, 1)
	go func() {
		runErr <- monitor.runLoop(ctx, func(_ context.Context, _ Signal, alerts Alerts) error {
			handled <- alerts
			return nil
		}, func(cadence time.Duration) runLoopTicker {
			if cadence != 5*time.Minute {
				t.Errorf("wait-event cadence = %s, want five minutes", cadence)
			}
			return &manualRunLoopTicker{c: ticks}
		})
	}()
	for observation, want := range []int{0, 1, 0, 0, 1} {
		if observation != 0 {
			select {
			case ticks <- time.Time{}:
			case <-ctx.Done():
				t.Fatal("loop did not accept the next explicit tick")
			}
		}
		select {
		case alerts := <-handled:
			if len(alerts) != want {
				t.Fatalf("observation %d returned %d alerts, want %d", observation+1, len(alerts), want)
			}
			if want != 0 && (alerts[0].Sustain != 2 || alerts[0].Frame != "IPC:BgworkerShutdown") {
				t.Fatalf("standing alert changed identity or sustain: %+v", alerts[0])
			}
		case <-ctx.Done():
			t.Fatal("loop did not finish the explicit observation")
		}
	}
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not join after cancellation")
	}
}

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
		"baseline age branch":    {alert.Baseline, "query age above one minute"},
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
