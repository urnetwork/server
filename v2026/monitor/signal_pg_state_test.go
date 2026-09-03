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
	requireAlertClass(t, alerts, "active-pileup")
}
