package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestConnectionOrphansSignalSyntheticLegacyLeak(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{
			"LEFT JOIN network_client_handler",
			"connect_time < now() - interval '2 minutes'",
			"MIN(connect_time) FILTER (WHERE orphan)",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("connection orphan query missing %q", want)
			}
		}
		return []Row{{"150544", "64194", "64194", "38493728", "20", "0"}}, nil
	}}
	alerts, err := NewConnectionOrphansSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "connection-orphans")
	if alert.Frame != "missing-handler" || alert.Sustain != 2 {
		t.Fatalf("wrong orphan alert framing: %+v", alert)
	}
	for _, want := range []string{
		"64194 mature open connection",
		"oldest 445.5 days",
		"mature_orphan_rows=64194",
		"b7599962",
		"Do not update connection rows",
		"SIGNALS.md §2.16",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("connection orphan alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestConnectionOrphansSignalSyntheticFreshRaceIsHealthy(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"10000", "1", "0", "45", "20", "0"}}, nil
	}}
	alerts, err := NewConnectionOrphansSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("fresh cleanup race alerted: %+v", alerts)
	}
}

// Every required metric must be authoritative before either health or an alert.
func TestConnectionOrphansSignalRejectsInvalidAggregates(t *testing.T) {
	validRow := Row{"10", "1", "1", "180", "2", "0"}
	invalidRows := [][]Row{nil, {{}}, {{"0"}}, {validRow, validRow}, {append(append(Row{}, validRow...), "0")},
		{{"0", "1", "0", "0", "0", "0"}}, {{"10", "0", "1", "180", "0", "0"}}, {{"10", "0", "0", "0", "0", "1"}}}
	for width := 2; width < 6; width++ {
		invalidRows = append(invalidRows, []Row{append(Row{}, validRow[:width]...)})
	}
	for column := range validRow {
		for _, invalid := range []string{"", "synthetic-invalid", "0.9", "-1", "9223372036854775808"} {
			row := append(Row{}, validRow...)
			row[column] = invalid
			invalidRows = append(invalidRows, []Row{row})
		}
	}
	for i, rows := range invalidRows {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
		alerts, err := NewConnectionOrphansSignal().Run(context.Background(), syntheticSettings(source))
		if err == nil || len(alerts) != 0 {
			t.Fatalf("case %d: invalid aggregate accepted: alerts=%d err=%v", i, len(alerts), err)
		}
		if strings.Contains(err.Error(), "synthetic-invalid") {
			t.Fatalf("case %d: source cell escaped into error", i)
		}
	}
}

// Numeric zero is a valid observation, distinct from absent or malformed input.
func TestConnectionOrphansSignalAcceptsZeroAggregate(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"0", "0", "0", "0", "0", "0"}}, nil
	}}
	alerts, err := NewConnectionOrphansSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("valid zero aggregate: alerts=%d err=%v", len(alerts), err)
	}
}
