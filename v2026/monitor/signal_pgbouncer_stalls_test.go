package monitor

import (
	"context"
	"testing"
)

func TestPgBouncerStallsSignalSyntheticUnreachable(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return "closed", nil }}
	alerts, err := NewPgBouncerStallsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "pgbouncer-unreachable")
}

func TestPgBouncerStallsSignalSyntheticClientWriteStall(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(HostSettings, string) (string, error) { return "open", nil },
		localFn: func(_ string, args ...string) (string, error) {
			if len(args) > 2 && args[2] == "api" {
				return "pgproto3.writeError=write failed: write tcp 10.0.0.2:50000->10.0.0.1:6432: i/o timeout", nil
			}
			return "", nil
		},
	}
	alerts, err := NewPgBouncerStallsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "pgbouncer-write-stall")
}
