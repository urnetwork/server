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
