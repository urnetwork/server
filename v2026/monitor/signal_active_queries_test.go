package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestActiveQueriesSignalSyntheticPersistentQuery(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"4242", "6", "180", "UPDATE transfer_contract SET reap_time = $1", "640"}}, nil
	}}
	alerts, err := NewActiveQueriesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "persistent-active-query")
}

func TestActiveQueriesSignalSyntheticKnownLongQueryIsNotAnIncident(t *testing.T) {
	tests := []struct {
		name      string
		row       Row
		wantAlert bool
	}{
		{
			name:      "within twice completed mean",
			row:       Row{"-1984868900869276935", "1", "877", "INSERT INTO network_connection_reliability_score", "640"},
			wantAlert: false,
		},
		{
			name:      "duration regression",
			row:       Row{"-1984868900869276935", "1", "1281", "INSERT INTO network_connection_reliability_score", "640"},
			wantAlert: true,
		},
		{
			name:      "new shape has no history",
			row:       Row{"9911", "1", "180", "UPDATE new_table", "0"},
			wantAlert: true,
		},
		{
			name:      "scheduled concurrent reindex within its bound",
			row:       Row{"8811", "1", "420", "REINDEX TABLE CONCURRENTLY web_search_ingest_state", "0"},
			wantAlert: false,
		},
		{
			name:      "concurrent reindex exceeded its bound",
			row:       Row{"8811", "1", "7200", "REINDEX TABLE CONCURRENTLY web_search_ingest_state", "0"},
			wantAlert: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
				return []Row{test.row}, nil
			}}
			alerts, err := NewActiveQueriesSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			gotAlert := false
			for _, alert := range alerts {
				gotAlert = gotAlert || alert.Class == "persistent-active-query"
			}
			if gotAlert != test.wantAlert {
				t.Fatalf("persistent alert = %t, want %t: %+v", gotAlert, test.wantAlert, alerts)
			}
		})
	}
}

func TestActiveQueriesSignalDiagnosesLegacyPayoutSubsidyRange(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{
			"-7171", "1", "300",
			"SELECT MIN(transfer_contract.create_time), MAX(transfer_contract.close_time) FROM transfer_escrow_sweep INNER JOIN transfer_contract ON transfer_contract.contract_id = transfer_escrow_sweep.contract_id",
			"600",
		}}, nil
	}}
	alerts, err := NewActiveQueriesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "persistent-active-query").Markdown()
	for _, want := range []string{
		"complete sweep history",
		"temp_account_payment",
		"semantically overbroad",
		"below twice that bad mean",
		"Deploy the taskworker payment planner",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("legacy Payout query diagnosis missing %q:\n%s", want, markdown)
		}
	}
}
