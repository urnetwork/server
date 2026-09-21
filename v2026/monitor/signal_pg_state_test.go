package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestPostgresStateSignalSyntheticIdleTransactionStorm(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "count(*) FILTER") {
			return []Row{{"3", "121", "532", "140"}}, nil
		}
		if strings.Contains(query, "WITH idle AS MATERIALIZED") {
			return []Row{
				{"0", "oldest", "1", "277", "1742249", "", "SELECT start_time, end_time FROM subsidy_payment WHERE start_time < $2 AND $1 < end_time"},
				{"1", "shape", "119", "1", "0", "", "UPDATE transfer_contract SET outcome = $2, close_time = $3 WHERE contract_id = $1"},
			}, nil
		}
		return nil, nil
	}}
	alerts, err := NewPostgresStateSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "idle-in-tx")
	for _, detail := range []string{
		"oldest_xact_s=532",
		"count and oldest age can have different owners",
		"oldest transaction: pid=1742249 continuously_idle=277s",
		"SELECT start_time, end_time FROM subsidy_payment",
		"backends=119 oldest_continuous_idle=1s",
		"transaction-local idle-timeout fix",
		"Do not mass-terminate",
		"battery begins after the state summary",
	} {
		if markdown := alert.Markdown(); !strings.Contains(markdown, detail) {
			t.Fatalf("mixed idle-in-transaction alert missing %q:\n%s", detail, markdown)
		}
	}
	for _, candidate := range alerts {
		if candidate.Class == "zombie-tx" {
			t.Fatalf("532s transaction incorrectly crossed the 30-minute zombie threshold: %+v", alerts)
		}
	}
}

func TestPostgresStateSignalSyntheticActivePileup(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "count(*) FILTER") {
			return []Row{{"101", "0", "0", "120"}}, nil
		}
		return nil, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // keep the synthetic escalation battery from waiting for its 15s delta
	alerts, err := NewPostgresStateSignal().Run(ctx, syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "active-pileup")
	for _, want := range []string{
		"battery begins after the state summary",
		"not an arithmetic partition of the observed total",
		"empty statement delta is unknown rather than healthy",
	} {
		if !strings.Contains(alert.Context, want) {
			t.Fatalf("one-shot active context missing %q: %s", want, alert.Context)
		}
	}
}

func TestPostgresStateSignalCachedBatteryPrecedesLargerSustainedTotal(t *testing.T) {
	stateCalls := 0
	batteryCalls := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "count(*) FILTER"):
			stateCalls++
			if stateCalls == 1 {
				return []Row{{"101", "0", "0", "140"}}, nil
			}
			return []Row{{"574", "13", "5", "714"}}, nil
		case strings.Contains(query, "GROUP BY query_id ORDER BY backends DESC LIMIT 5"):
			batteryCalls++
			return []Row{{"synthetic-query", "11", "-:-", "SELECT bounded_fixture"}}, nil
		default:
			return nil, nil
		}
	}}
	signal := NewPostgresStateSignal()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // keep the plan-wall battery's bounded delta from waiting
	if _, err := signal.Run(ctx, syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	alerts, err := signal.Run(ctx, syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "active-pileup")
	for _, check := range []struct {
		name string
		got  string
		want string
	}{
		{name: "current total", got: alert.Observed, want: "active=574"},
		{name: "trip battery", got: alert.Evidence, want: "backends=11"},
		{name: "cached annotation", got: alert.Evidence, want: "battery collected once at trip"},
		{name: "earlier frame", got: alert.Context, want: "precedes this later sustained state summary"},
		{name: "non-atomic", got: alert.Context, want: "separate snapshots"},
		{name: "no subtraction", got: alert.Context, want: "not an arithmetic partition of the observed total"},
		{name: "empty delta unknown", got: alert.Context, want: "empty statement delta is unknown rather than healthy"},
	} {
		if !strings.Contains(check.got, check.want) {
			t.Fatalf("%s missing %q: %q", check.name, check.want, check.got)
		}
	}
	if batteryCalls != 1 {
		t.Fatalf("active battery calls = %d, want one trip-time collection", batteryCalls)
	}
}
