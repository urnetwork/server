package monitor

import (
	"context"
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
		"Do not add concurrent reapers",
		"Each run finishes before the task deadline",
		"SIGNALS.md §2.5, §2.10, and §8.9",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("bounded retention diagnosis lost %q:\n%s", want, markdown)
		}
	}
}
