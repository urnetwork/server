package monitor

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestOpenContractsSignalSyntheticOpenSetBacklog(t *testing.T) {
	counts := []int{160000, 159000, 161000, 161000}
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		count := counts[0]
		counts = counts[1:]
		return []Row{{strconv.Itoa(count), "120000", "8000"}}, nil
	}}
	signal := NewOpenContractsSignal()
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	warmup := requireAlertClass(t, alerts, "open-set-size")
	if strings.Contains(warmup.Symptom, "rising") || !strings.Contains(warmup.Symptom, "warming up") {
		t.Fatalf("startup alert claimed an unobserved trend: %q", warmup.Symptom)
	}

	alerts, err = signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("falling high set must reset the alert, got %+v", alerts)
	}

	alerts, err = signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	rising := requireAlertClass(t, alerts, "open-set-size")
	if !strings.Contains(rising.Symptom, "rising (previous 159000)") ||
		!strings.Contains(rising.Observed, "delta=2000") ||
		!strings.Contains(rising.Observed, "older_5m=120000 older_30m=8000") ||
		!strings.Contains(rising.Mechanism, "cohorts of up to 25,000") ||
		!strings.Contains(rising.Mechanism, "older deployments used 100,000") ||
		!strings.Contains(rising.Context, "retention-fanout") ||
		!strings.Contains(rising.Verify, "older-than-five-minute cohort falls") {
		t.Fatalf("rising alert lacks adjacent-sample evidence: %+v", rising)
	}

	alerts, err = signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("stable high set must not claim growth, got %+v", alerts)
	}
}
