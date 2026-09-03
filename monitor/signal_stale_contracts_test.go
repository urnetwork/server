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
			"source_parent.active AS source_parent_active",
			"count(DISTINCT destination_parent_id) FILTER (WHERE same_network)",
			"count(DISTINCT source_device_id) FILTER (WHERE same_network)",
			"count(DISTINCT source_network_id) FILTER (WHERE same_network)",
			"count(DISTINCT source_id) FILTER (WHERE NOT same_network)",
			"count(DISTINCT source_parent_id) FILTER (WHERE NOT same_network)",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("stale-contract query missing %q:\n%s", want, query)
			}
		}
		return []Row{{"8705", "8705", "8705", "8705", "135", "14", "641254", "651744", "135", "9", "9", "14", "4", "2", "0", "0", "0", "0", "0", "0", "0"}}, nil
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
		"same_distinct_destinations=135",
		"same_distinct_destination_parents=9",
		"same_distinct_destination_devices=9",
		"same_distinct_sources=14",
		"same_distinct_source_devices=4",
		"same_distinct_networks=2",
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
		return []Row{{"0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0"}}, nil
	}}
	alerts, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("zero stale successes alerted: %+v", alerts)
	}
}

func TestStaleContractsSignalSyntheticConcentratedRetainedClientRoute(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"180", "0", "0", "0", "1", "60", "66000", "67000", "0", "0", "0", "0", "0", "0", "180", "180", "180", "1", "60", "1", "1"}}, nil
	}}
	alerts, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "stale-contract-success")
	for _, want := range []string{
		"cross_network=180",
		"cross_destination_top=180",
		"cross_source_derived=180",
		"cross_source_parent_active=180",
		"cross_distinct_destinations=1",
		"cross_distinct_sources=60",
		"cross_distinct_source_parents=1",
		"cross_distinct_source_devices=1",
		"one window churning derived identities",
		"bounded current-cache control",
		"do not call one retained route global provider-cache contamination",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("concentrated stale-contract alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestStaleContractsSignalSyntheticConcentratedSameNetworkReturnPath(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"240", "240", "240", "240", "24", "8", "700000", "710000", "24", "1", "1", "8", "1", "1", "0", "0", "0", "0", "0", "0", "0"}}, nil
	}}
	alerts, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "stale-contract-success")
	for _, want := range []string{
		"same_network=240",
		"same_distinct_destinations=24",
		"same_distinct_destination_parents=1",
		"same_distinct_destination_devices=1",
		"same_distinct_sources=8",
		"same_distinct_source_devices=1",
		"same_distinct_networks=1",
		"one concentrated relationship/window boundary",
		"without exporting identities",
		"decide whether one relationship/window or multiple networks",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("concentrated same-network alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestStaleContractsSignalSyntheticRejectsMalformedAggregate(t *testing.T) {
	for _, test := range []struct {
		name string
		row  Row
		want string
	}{
		{name: "negative", row: Row{"-1", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0"}, want: "invalid column 0"},
		{name: "part_above_total", row: Row{"2", "3", "0", "0", "1", "1", "1", "2", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0"}, want: "same_network=3 above total=2"},
		{name: "same_parent_above_destinations", row: Row{"2", "2", "2", "2", "1", "1", "1", "2", "1", "2", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"}, want: "same_distinct_destination_parents=2 above enclosing count=1"},
		{name: "cross_part_above_cross", row: Row{"2", "2", "0", "0", "1", "1", "1", "2", "1", "0", "0", "1", "0", "1", "1", "0", "0", "0", "0", "0", "0"}, want: "cross_destination_top=1 above cross_network=0"},
		{name: "reversed_quantiles", row: Row{"2", "2", "2", "2", "1", "1", "5", "4", "1", "1", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"}, want: "median inactive age 5 above p95 4"},
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
