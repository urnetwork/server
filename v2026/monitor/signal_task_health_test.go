package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTaskHealthSignalSyntheticDurationRegression(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"UpdateClientScores", "120.0", "30.0", "12"}}, nil
	}}
	alerts, err := NewTaskHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "task-duration-regression")
}

func TestTaskHealthSignalExplainsBoundedRetentionCatchup(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"RemoveCompletedContracts", "306.2", "6.3", "2"}}, nil
	}}
	alerts, err := NewTaskHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-duration-regression").Markdown()
	for _, want := range []string{
		"five-minute wall-clock budget",
		"not by itself a stuck transaction",
		"contract_retention_pending and cursor progress",
		"Retain the bounded retention implementation",
		"deploy it only where version or code evidence says it is absent",
		"Do not add concurrent reapers",
		"Each run finishes before the task deadline",
		"SIGNALS.md §2.5, §2.10, and §8.9",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("bounded retention diagnosis lost %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "Deploy the bounded retention implementation on every taskworker") {
		t.Fatalf("bounded retention diagnosis retained a stale fleet rollout prescription:\n%s", markdown)
	}
}

// Large completed companion rows preserve correlation without inventing causal authority.
func TestTaskHealthSignalRetainsCompletedOverlapWithoutOwnerAttribution(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "WITH latest_export AS") {
			return []Row{
				{"CloseExpiredContracts", "870", "187"},
				{"ReconcileNetEscrow", "317", "121"},
			}, nil
		}
		return []Row{{"ExportStats", "187.0", "87.6", "1"}}, nil
	}}
	alerts, err := NewTaskHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "task-duration-regression").Markdown()
	for _, want := range []string{
		"four read-heavy 90-day aggregates",
		"not necessarily an interval contributing to the flagged hourly mean",
		"do not establish an overrun, deployed artifact or causal load owner",
		"Temporal overlap is coincident evidence",
		"CloseExpiredContracts duration=870s overlap=187s",
		"ReconcileNetEscrow duration=317s overlap=121s",
		"Keep ExportStats on its hourly cadence",
		"matching companion baselines, evaluator outcomes and deployed artifacts",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("completed ExportStats overlap lost a required diagnostic")
		}
	}
	if strings.Contains(markdown, "already-proven") || strings.Contains(markdown, "Deploy and verify the bounded close") {
		t.Fatal("large completed overlap acquired unsupported causal owner or repair")
	}
}

func TestTaskHealthSignalExportStatsOverlapFailureDoesNotRenderDatabaseError(t *testing.T) {
	hostile := "provider supplied task=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa address=192.0.2.48 pointer=0xdeadbeef goroutine synthetic.Stack"
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "WITH latest_export AS") {
			return nil, errors.New("decode failure: " + hostile)
		}
		return []Row{{"ExportStats", "187.0", "87.6", "1"}}, nil
	}}
	alerts, err := NewTaskHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "task-duration-regression")
	if !strings.Contains(alert.Context, "ExportStats overlap attribution failed; error_class="+observationErrorClassInvalidResponse) {
		t.Fatalf("ExportStats overlap evidence lost its fixed error class:\n%s", alert.Markdown())
	}
	requireAlertOmits(t, alert, "provider supplied", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "192.0.2.48", "0xdeadbeef", "synthetic.Stack")
}
