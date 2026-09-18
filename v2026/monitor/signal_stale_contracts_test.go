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
			"WHEN tc.create_time - destination.deactivate_time < interval '1 second'",
			"THEN 'stale-contract-timestamp-ambiguous'",
			"ELSE 'stale-contract-success'",
			"GROUP BY boundary_class",
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
		return []Row{{staleContractSuccessClass, "8705", "8705", "8705", "8705", "135", "14", "641254000.000", "651744000.000", "135", "9", "9", "14", "4", "2", "0", "0", "0", "0", "0", "0", "0"}}, nil
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
		"recorded inactive at least one second before creation",
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
		"median_inactive_before_create_ms=641254000.000",
		"transactional lifecycle serialization commit 883d39c8",
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
		return nil, nil
	}}
	alerts, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("zero stale successes alerted: %+v", alerts)
	}
}

func TestStaleContractsSignalSyntheticSubsecondClockAmbiguity(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{staleContractTimestampAmbiguousClass, "1", "1", "1", "1", "1", "1", "0.623", "0.623", "1", "1", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"}}, nil
	}}
	alerts, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts=%+v, want one timestamp-ambiguity page", alerts)
	}
	alert := requireAlertClass(t, alerts, staleContractTimestampAmbiguousClass)
	if alert.Severity != SeverityPage || alert.Frame != "subsecond-order-ambiguous" {
		t.Fatalf("wrong timestamp-ambiguity identity: %+v", alert)
	}
	for _, want := range []string{
		"cannot prove which transaction won",
		"cross-host clock offset",
		"timestamps alone do not prove stale acceptance",
		"median_inactive_before_create_ms=0.623",
		"p95_inactive_before_create_ms=0.623",
		"timestamp_boundary_ms=1000.000",
		"PostgreSQL primary clock",
		"Do not call the subsecond row a proven stale acceptance",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("timestamp-ambiguity alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "The API accepted a destination after its durable client lifecycle had ended") {
		t.Fatalf("timestamp ambiguity retained the prior false diagnosis:\n%s", alert.Markdown())
	}
	if strings.Contains(alert.Markdown(), "compare every API artifact with server commit c8dfe570") {
		t.Fatalf("timestamp ambiguity retained obsolete c8dfe570-only deployment guidance:\n%s", alert.Markdown())
	}
}

func TestStaleContractsSignalSyntheticTimestampBoundary(t *testing.T) {
	for _, test := range []struct {
		name  string
		class string
		age   string
		frame string
	}{
		{name: "last_ambiguous_millisecond", class: staleContractTimestampAmbiguousClass, age: "999.999", frame: "subsecond-order-ambiguous"},
		{name: "first_affirmative_millisecond", class: staleContractSuccessClass, age: "1000.000", frame: "inactive-before-create"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
				return []Row{{test.class, "1", "1", "1", "1", "1", "1", test.age, test.age, "1", "1", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"}}, nil
			}}
			alerts, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, test.class)
			if alert.Frame != test.frame || !strings.Contains(alert.Markdown(), "median_inactive_before_create_ms="+test.age) {
				t.Fatalf("boundary alert=%+v markdown=%s", alert, alert.Markdown())
			}
		})
	}
}

func TestStaleContractsSignalSyntheticMixedTimestampClasses(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{
			{staleContractSuccessClass, "2", "0", "0", "0", "1", "1", "1500.000", "2000.000", "0", "0", "0", "0", "0", "0", "2", "0", "0", "1", "1", "0", "1"},
			{staleContractTimestampAmbiguousClass, "1", "1", "1", "1", "1", "1", "0.623", "0.623", "1", "1", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"},
		}, nil
	}}
	alerts, err := NewStaleContractsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts=%+v, want one alert for each timestamp class", alerts)
	}
	requireAlertClass(t, alerts, staleContractSuccessClass)
	requireAlertClass(t, alerts, staleContractTimestampAmbiguousClass)
}

func TestStaleContractsProbeTimestampClassesResolveIndependently(t *testing.T) {
	rowForClass := func(class string) Row {
		age := "1000.000"
		if class == staleContractTimestampAmbiguousClass {
			age = "999.999"
		}
		return Row{class, "1", "1", "1", "1", "1", "1", age, age, "1", "1", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"}
	}
	for _, sequence := range [][]string{
		{staleContractSuccessClass, staleContractTimestampAmbiguousClass},
		{staleContractTimestampAmbiguousClass, staleContractSuccessClass},
	} {
		currentClass := sequence[0]
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			return []Row{rowForClass(currentClass)}, nil
		}}
		env, err := newProbeEnv(syntheticSettings(source).withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		for _, current := range sequence {
			currentClass = current
			findings, err := (staleContractsProbe{}).check(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != 2 {
				t.Fatalf("class %s findings=%+v, want one state for each class", current, findings)
			}
			for _, class := range []string{staleContractSuccessClass, staleContractTimestampAmbiguousClass} {
				finding := findingByClass(t, findings, class)
				if finding.healthy == (class == current) {
					t.Fatalf("current=%s class=%s healthy=%t, want current broken and absent class healthy", current, class, finding.healthy)
				}
			}
		}
	}
}

func TestStaleContractsSignalSyntheticConcentratedRetainedClientRoute(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{staleContractSuccessClass, "180", "0", "0", "0", "1", "60", "66000000.000", "67000000.000", "0", "0", "0", "0", "0", "0", "180", "180", "180", "1", "60", "1", "1"}}, nil
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
		return []Row{{staleContractSuccessClass, "240", "240", "240", "240", "24", "8", "700000000.000", "710000000.000", "24", "1", "1", "8", "1", "1", "0", "0", "0", "0", "0", "0", "0"}}, nil
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
		{name: "negative", row: Row{staleContractSuccessClass, "-1", "0", "0", "0", "0", "0", "1000", "1000", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0"}, want: "invalid column 1"},
		{name: "part_above_total", row: Row{staleContractSuccessClass, "2", "3", "0", "0", "1", "1", "1000", "2000", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0"}, want: "same_network=3 above total=2"},
		{name: "same_parent_above_destinations", row: Row{staleContractSuccessClass, "2", "2", "2", "2", "1", "1", "1000", "2000", "1", "2", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"}, want: "same_distinct_destination_parents=2 above enclosing count=1"},
		{name: "cross_part_above_cross", row: Row{staleContractSuccessClass, "2", "2", "0", "0", "1", "1", "1000", "2000", "1", "0", "0", "1", "0", "1", "1", "0", "0", "0", "0", "0", "0"}, want: "cross_destination_top=1 above cross_network=0"},
		{name: "reversed_quantiles", row: Row{staleContractSuccessClass, "2", "2", "2", "2", "1", "1", "5000", "4000", "1", "1", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"}, want: "median inactive age 5000.000ms above p95 4000.000ms"},
		{name: "ambiguous_at_boundary", row: Row{staleContractTimestampAmbiguousClass, "1", "1", "1", "1", "1", "1", "1000.000", "1000.000", "1", "1", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"}, want: "ambiguous p95 1000.000ms reaches boundary 1000.000ms"},
		{name: "affirmative_below_boundary", row: Row{staleContractSuccessClass, "1", "1", "1", "1", "1", "1", "999.999", "999.999", "1", "1", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"}, want: "affirmative median 999.999ms is below boundary 1000.000ms"},
		{name: "unknown_class", row: Row{"synthetic-unknown", "1", "1", "1", "1", "1", "1", "1000", "1000", "1", "1", "1", "1", "1", "1", "0", "0", "0", "0", "0", "0", "0"}, want: "invalid class"},
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
