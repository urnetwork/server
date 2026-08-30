package monitor

import (
	"context"
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
