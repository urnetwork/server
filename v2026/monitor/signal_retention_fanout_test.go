package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestRetentionFanoutSignalSyntheticFailure(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"2", "45", "38", "323304", "544554", "2150562", "1480000000", "1000", "54700000", "2000", "384", "10"}}, nil
	}}
	alerts, err := NewRetentionFanoutSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "retention-fanout")
	markdown := alerts[0].Markdown()
	for _, want := range []string{
		"10 AdvancePayment row(s)",
		"120s connection-cleanup deadline signature",
		"contract_retention_pending",
		"contract_retention_cursor",
		"advance_deadline_cleanup_failures=10",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("alert Markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestRetentionFanoutSignalRetainsDurableDeadlineEvidenceBetweenRetries(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"0", "0", "38", "323304", "544554", "2150562", "1480000000", "1000", "54700000", "2000", "384", "10"}}, nil
	}}
	alerts, err := NewRetentionFanoutSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "retention-fanout")
	markdown := alerts[0].Markdown()
	if !strings.Contains(markdown, "legacy retention query averages 2150562 rows/call") {
		t.Fatalf("durable deadline evidence was lost between retries:\n%s", markdown)
	}
}

func TestRetentionFanoutSignalRejectsUncorrelatedCleanupError(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"0", "0", "1", "5", "5", "10", "10", "0", "0", "0", "1", "1"}}, nil
	}}
	alerts, err := NewRetentionFanoutSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("uncorrelated cleanup error produced retention alert: %+v", alerts)
	}
}

// Validate complete metrics on every statistics partition before reducing health.
func TestRetentionFanoutSignalRejectsInvalidAggregates(t *testing.T) {
	validRow := Row{"0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0"}
	invalidRows := [][]Row{nil, {{}}, {{"0"}}, {append(append(Row{}, validRow...), "0")}}
	for width := 2; width < 12; width++ {
		invalidRows = append(invalidRows, []Row{append(Row{}, validRow[:width]...)})
	}
	for column := range validRow {
		for _, invalid := range []string{"", "synthetic-invalid", "0.9", "-1", "9223372036854775808"} {
			row := append(Row{}, validRow...)
			row[column] = invalid
			invalidRows = append(invalidRows, []Row{row}, []Row{validRow, row})
		}
	}
	for _, column := range []int{0, 1, 10, 11} {
		row := append(Row{}, validRow...)
		row[column] = "1"
		invalidRows = append(invalidRows, []Row{validRow, row})
	}
	for i, rows := range invalidRows {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
		alerts, err := NewRetentionFanoutSignal().Run(context.Background(), syntheticSettings(source))
		if err == nil || len(alerts) != 0 {
			t.Fatalf("case %d: invalid aggregate accepted: alerts=%d err=%v", i, len(alerts), err)
		}
		if strings.Contains(err.Error(), "synthetic-invalid") {
			t.Fatalf("case %d: source cell escaped into error", i)
		}
	}
}

// The left join supplies a complete zero row even when no statement matches.
func TestRetentionFanoutSignalAcceptsZeroAggregate(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0"}}, nil
	}}
	alerts, err := NewRetentionFanoutSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("valid zero aggregate: alerts=%d err=%v", len(alerts), err)
	}
}

// Any qualifying statement partition must retain attribution in either order.
func TestRetentionFanoutSignalRetainsAllStatisticsPartitions(t *testing.T) {
	lowRow := Row{"0", "0", "1", "5", "5", "10", "10", "0", "0", "0", "1", "1"}
	highRow := Row{"0", "0", "2", "50", "60", "100000", "20", "1", "1", "1", "1", "1"}
	for _, rows := range [][]Row{{lowRow, highRow}, {highRow, lowRow}} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
		alerts, err := NewRetentionFanoutSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "retention-fanout")
		if alert.Sustain != 2 || alert.Frame != retentionFanoutQueryID {
			t.Fatalf("wrong partition alert framing: %+v", alert)
		}
		for _, want := range []string{"statement_partitions=2", "calls=1 mean_ms=5", "calls=2 mean_ms=50", "100000 rows/call in at least one statistics partition"} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("partition evidence missing %q", want)
			}
		}
	}
}

// Multiple healthy partitions are ordinary source population, including repeats.
func TestRetentionFanoutSignalAcceptsHealthyStatisticsPartitions(t *testing.T) {
	lowRow := Row{"0", "0", "1", "5", "5", "10", "10", "0", "0", "0", "1", "1"}
	otherRow := Row{"0", "0", "2", "50", "60", "99999", "20", "1", "1", "1", "1", "1"}
	for _, rows := range [][]Row{{lowRow, otherRow}, {otherRow, lowRow}, {lowRow, lowRow}} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
		alerts, err := NewRetentionFanoutSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil || len(alerts) != 0 {
			t.Fatalf("valid healthy partitions: alerts=%d err=%v", len(alerts), err)
		}
	}
}
