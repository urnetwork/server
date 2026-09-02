package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestStaleContractsSignalSyntheticInactiveBeforeCreate(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{
			"tc.create_time >= now() - interval '5 minutes'",
			"tc.companion_contract_id IS NULL",
			"NOT destination.active",
			"destination.deactivate_time <= tc.create_time",
			"count(DISTINCT destination_id)",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("stale-contract query missing %q:\n%s", want, query)
			}
		}
		return []Row{{"8705", "8705", "8705", "8705", "135", "14", "641254", "651744"}}, nil
	}}
	alerts, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts=%+v, want one stale-contract success page", alerts)
	}
	alert := requireAlertClass(t, alerts, "stale-contract-success")
	if alert.SignalNumber != "2.20" || alert.SignalKey != "stale-contracts" ||
		alert.SignalID != "pg/stale-contracts" || alert.Severity != SeverityPage || alert.Sustain != 1 {
		t.Fatalf("wrong stale-contract signal identity: %+v", alert)
	}
	for _, want := range []string{
		"8705 successful non-companion contracts",
		"already inactive before creation",
		"same_network=8705",
		"cross_network=0",
		"destination_derived=8705",
		"source_active_top=8705",
		"distinct_destinations=135",
		"median_inactive_before_create_s=641254",
		"server commit c8dfe570",
		"not merely a high rejection rate",
		"no client, network, connection, contract, or destination identifier",
		"SIGNALS.md §2.20, §2.18, §2.17, and §8.12",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("stale-contract alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestStaleContractsSignalSyntheticHealthy(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"0", "0", "0", "0", "0", "0", "0", "0"}}, nil
	}}
	alerts, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("zero stale successes alerted: %+v", alerts)
	}
}

func TestStaleContractsSignalSyntheticRejectsMalformedAggregate(t *testing.T) {
	for _, test := range []struct {
		name string
		row  Row
		want string
	}{
		{name: "negative", row: Row{"-1", "0", "0", "0", "0", "0", "0", "0"}, want: "invalid column 0"},
		{name: "part_above_total", row: Row{"2", "3", "0", "0", "1", "1", "1", "2"}, want: "same_network=3 above total=2"},
		{name: "reversed_quantiles", row: Row{"2", "2", "2", "2", "1", "1", "5", "4"}, want: "median inactive age 5 above p95 4"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
				return []Row{test.row}, nil
			}}
			_, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestStaleContractsSignalSyntheticPreservesPostgresFailure(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, fmt.Errorf("synthetic read failed")
	}}
	_, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
	if err == nil || !strings.Contains(err.Error(), "synthetic read failed") {
		t.Fatalf("error=%v, want source failure", err)
	}
}
