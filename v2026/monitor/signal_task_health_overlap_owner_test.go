package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A regressed export does not make ordinary completed companions overruns.
// The public adapter and fixed-clock source preserve the original finding;
// only unsupported causal attribution and repair direction are rejected.
func TestTaskHealthOrdinaryCompletedOverlapDoesNotProveOwner(t *testing.T) {
	for _, test := range []struct {
		name     string
		overlaps []Row
	}{
		{name: "ordinary-close", overlaps: []Row{{"CloseExpiredContracts", "20", "20"}}},
		{name: "ordinary-netescrow", overlaps: []Row{{"ReconcileNetEscrow", "1", "1"}}},
		{name: "ordinary-both", overlaps: []Row{{"CloseExpiredContracts", "20", "20"}, {"ReconcileNetEscrow", "1", "1"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			alert := taskOverlapPublicFixture(t, test.overlaps)
			if strings.Contains(alert.Mechanism, "already-proven") ||
				strings.Contains(alert.Mechanism, "making shared primary load the leading owner") ||
				strings.Contains(alert.Action, "Deploy and verify the bounded close") {
				t.Fatal("ordinary completed overlap acquired unsupported owner or repair direction")
			}
			for _, row := range test.overlaps {
				if !strings.Contains(alert.Context, row[0]+" duration="+row[1]+"s overlap="+row[2]+"s") {
					t.Fatal("ordinary overlap evidence was discarded")
				}
			}
		})
	}
}

// The completed ExportStats outlier and bounded overlap evidence must survive
// a future attribution correction. Large companion durations alone remain
// correlation; this test does not invent companion baselines or SQL wait edges.
func TestTaskHealthCompletedExportOutlierPreservesOverlap(t *testing.T) {
	for _, test := range []struct {
		name     string
		overlaps []Row
	}{
		{name: "completed-close", overlaps: []Row{{"CloseExpiredContracts", "870", "187"}}},
		{name: "completed-both", overlaps: []Row{{"CloseExpiredContracts", "870", "187"}, {"ReconcileNetEscrow", "317", "121"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			alert := taskOverlapPublicFixture(t, test.overlaps)
			for _, row := range test.overlaps {
				if !strings.Contains(alert.Context, row[0]+" duration="+row[1]+"s overlap="+row[2]+"s") {
					t.Fatal("completed outlier lost its bounded overlap evidence")
				}
			}
		})
	}
}

// Two synthetic PostgreSQL replies exercise the actual public Run path without
// PostgreSQL, a native command, network, wall-clock sleep or a real task record.
func taskOverlapPublicFixture(t *testing.T, overlaps []Row) Alert {
	t.Helper()
	historyCalls := 0
	overlapCalls := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "WITH latest_export AS") {
			overlapCalls++
			return overlaps, nil
		}
		if strings.Contains(query, "WITH hist AS") {
			historyCalls++
			return []Row{{"ExportStats", "187.0", "87.6", "1"}}, nil
		}
		return nil, errors.New("fixed unexpected synthetic task query")
	}}
	alerts, err := NewTaskHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 1 || historyCalls != 1 || overlapCalls != 1 {
		t.Fatal("public task outlier fixture failed its source contract")
	}
	alert := alerts[0]
	if alert.Class != "task-duration-regression" || alert.SignalNumber != "2.5" ||
		alert.Frame != "ExportStats" || alert.Severity != SeverityWarn || alert.Sustain != 1 ||
		!strings.Contains(alert.Observed, "mean_1h=187.0s p95_7d=87.6s runs_1h=1") {
		t.Fatal("completed export outlier classification or measurement changed")
	}
	return alert
}
