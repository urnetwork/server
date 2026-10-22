// Vacuum horizon text remains private while debt, progress and fixed diagnoses stay public.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const vacuumHorizonPrivateLiteral = "synthetic-horizon-literal-vh37"
const vacuumHorizonPrivateIdentifier = "synthetic_private_column_vh91"
const vacuumHorizonPrivateDomain = "horizon-private.example"
const vacuumHorizonPrivateApplication = "synthetic-horizon-app-vh83"

// Every cell fits the real 21-column source projection; no database is contacted.
func vacuumHorizonPrivacyRow() Row {
	return Row{
		"synthetic_table", "12000001", "01-01 00:00",
		"5000000", "vacuuming indexes", "73", "1000", "900", "800", "1", "2", "4",
		"4242", "9001", "1234", "1230", "120", "active", "client backend",
		vacuumHorizonPrivateApplication + "\n```\n# invented evidence",
		"SELECT '" + vacuumHorizonPrivateLiteral + "', " + vacuumHorizonPrivateIdentifier + " FROM synthetic_work WHERE note='" + vacuumHorizonPrivateDomain + "'",
	}
}

// Public field checks include future exported strings without printing fixture contents.
func vacuumHorizonAssertPublicPrivacy(t *testing.T, alert Alert) {
	t.Helper()
	var jsonl bytes.Buffer
	if err := WriteAlertsJSONL(&jsonl, []Alert{alert}); err != nil {
		t.Fatal("synthetic JSONL rendering failed")
	}
	encoded, err := json.Marshal(alert)
	if err != nil {
		t.Fatal("synthetic JSON rendering failed")
	}
	surfaces := []struct{ name, text string }{
		{name: "Markdown", text: alert.Markdown()},
		{name: "complete Markdown", text: AlertsMarkdown([]Alert{alert})},
		{name: "JSON", text: string(encoded)},
		{name: "JSONL", text: jsonl.String()},
		{name: "identity", text: alert.Identity()},
	}
	value := reflect.ValueOf(alert)
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if field.IsExported() && value.Field(i).Kind() == reflect.String {
			surfaces = append(surfaces, struct{ name, text string }{name: field.Name, text: value.Field(i).String()})
		}
	}
	for _, surface := range surfaces {
		for _, marker := range []string{vacuumHorizonPrivateLiteral, vacuumHorizonPrivateIdentifier, vacuumHorizonPrivateDomain, vacuumHorizonPrivateApplication} {
			if strings.Contains(surface.text, marker) {
				t.Errorf("%s retained a synthetic private horizon marker", surface.name)
			}
		}
	}
}

// The real signal adapter retains one source call and the full public debt contract.
func vacuumHorizonPrivacyObserve(t *testing.T, row Row) Alert {
	t.Helper()
	calls := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		calls++
		if query != pgVacuumHealthSQL {
			t.Fatal("vacuum privacy control changed source selection")
		}
		return []Row{row}, nil
	}}
	signal := NewVacuumHealthSignal()
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 1 || calls != 1 {
		t.Fatal("synthetic debt violation lost its single observation and alert")
	}
	alert := alerts[0]
	if alert.SignalID != "pg/dead-tuples" || alert.SignalKey != "vacuum-health" || alert.SignalNumber != "2.4" ||
		alert.Class != "dead-tuples" || alert.Target != "pg-1" || alert.Frame != row[0] ||
		alert.Severity != SeverityWarn || alert.Sustain != 1 || signal.Cadence() != 5*time.Minute {
		t.Fatal("horizon withholding changed debt identity, severity, sustain or cadence")
	}
	vacuumHorizonAssertPublicPrivacy(t, alert)
	return alert
}

// Baseline RED must be a causal public-marker assertion failure, not a harness failure.
func TestVacuumHorizonPrivacyRejectsRawPublicText(t *testing.T) {
	alert := vacuumHorizonPrivacyObserve(t, vacuumHorizonPrivacyRow())
	if alert.Observed != "n_dead_tup=12000001 alert_threshold=10000000 configured_vacuum_threshold=5000000 last_autovacuum=01-01 00:00 vacuum_age_s=73" ||
		!strings.Contains(alert.Evidence, "vacuum phase=vacuuming indexes age_s=73 heap_scanned=900/1000 heap_vacuumed=800 index_vacuum_count=1 indexes_processed=2/4") ||
		!strings.Contains(alert.Evidence, "oldest MVCC horizon candidate: pid=4242 horizon_age_xids=9001 backend_xid=1234 backend_xmin=1230 xact_age_s=120 state=active backend=client backend application=withheld query=withheld") ||
		!strings.Contains(alert.Verify, "synthetic_table returns below 10M") {
		t.Fatal("withholding discarded numeric horizon, progress or table recovery authority")
	}
}

// Reproduce the actual normalized 160-character source prefix, not a late private suffix.
func TestVacuumHorizonPrivacyRejectsNormalizedPrefix(t *testing.T) {
	row := vacuumHorizonPrivacyRow()
	row[20] = strings.Join(strings.Fields(row[20]+"\n```\n# invented evidence\n"), " ")
	if len(row[20]) > 160 {
		row[20] = row[20][:160]
	}
	for _, marker := range []string{vacuumHorizonPrivateLiteral, vacuumHorizonPrivateIdentifier, vacuumHorizonPrivateDomain} {
		if !strings.Contains(row[20], marker) {
			t.Fatal("synthetic canary is outside the real source prefix")
		}
	}
	vacuumHorizonPrivacyObserve(t, row)
}

// Every existing shape still supplies fixed meaning without copying its private suffix.
func TestVacuumHorizonPrivacyPreservesClassifiers(t *testing.T) {
	for _, test := range []struct{ query, age, mechanism string }{
		{query: "INSERT INTO client_reliability_running SELECT synthetic_value", age: "120", mechanism: "share this truncated prefix"},
		{query: "UPDATE transfer_contract SET reap_time = $1", age: "120", mechanism: "legacy CompletePayment retention fan-out"},
		{query: "UPDATE transfer_contract SET outcome = $1, close_time = $2", age: "2", mechanism: "bounded per-contract CloseExpiredContracts"},
		{query: "CREATE TEMPORARY TABLE temp_account_payment AS SELECT synthetic_value", age: "120", mechanism: "Payout payment planner"},
		{query: "SELECT subsidy_start_time, subsidy_end_time FROM transfer_escrow_sweep", age: "120", mechanism: "Payout payment planner"},
		{query: "SELECT value FROM subsidy_payment WHERE start_time < $2 AND $1 < end_time", age: "120", mechanism: "Payout payment planner"},
		{query: "SELECT value FROM synthetic_work", age: "2", mechanism: "fresh read-only snapshot, not a persistent horizon holder"},
	} {
		row := vacuumHorizonPrivacyRow()
		row[16] = test.age
		row[20] = test.query + " /* " + vacuumHorizonPrivateLiteral + " */"
		alert := vacuumHorizonPrivacyObserve(t, row)
		if !strings.Contains(alert.Mechanism, test.mechanism) || !strings.Contains(alert.Evidence, "application=withheld query=withheld") {
			t.Fatal("private horizon classification was erased or escaped withholding")
		}
		if strings.Contains(test.query, "client_reliability_running") &&
			(!strings.Contains(alert.Mechanism, "does not establish the maintenance phase or deployed cadence") ||
				strings.Contains(alert.Mechanism, "performing a full running-window re-anchor")) {
			t.Fatal("private reliability prefix acquired unsupported phase authority")
		}
	}
}

// A missing candidate does not suppress genuine debt or invent horizon attribution.
func TestVacuumHorizonPrivacyAbsentCandidatePreservesDebt(t *testing.T) {
	row := vacuumHorizonPrivacyRow()
	row[12] = "0"
	alert := vacuumHorizonPrivacyObserve(t, row)
	if !strings.Contains(alert.Evidence, "no backend_xid/backend_xmin horizon candidate reported") ||
		strings.Contains(alert.Evidence, "oldest MVCC horizon candidate:") {
		t.Fatal("absent horizon changed genuine debt or invented a candidate")
	}
}

// Horizon text never independently triggers a warning below either policy floor.
func TestVacuumHorizonPrivacyHealthyThresholdControls(t *testing.T) {
	for _, test := range []struct{ dead, configured string }{
		{dead: "10000000", configured: "5000000"},
		{dead: "15000000", configured: "20000000"},
		{dead: "20000000", configured: "20000000"},
	} {
		row := vacuumHorizonPrivacyRow()
		row[1], row[3] = test.dead, test.configured
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{row}, nil }}
		alerts, err := NewWithSignals(syntheticSettings(source), NewVacuumHealthSignal()).Run(context.Background())
		if err != nil || len(alerts) != 0 {
			t.Fatal("private horizon overrode a healthy table debt floor")
		}
	}
}

// Progressing cascade debt stays a warning without an invented blocker or defect.
func TestVacuumHorizonPrivacyCascadeAttributionControls(t *testing.T) {
	for _, test := range []struct {
		age    string
		normal bool
	}{
		{age: "59", normal: true}, {age: "60", normal: false},
	} {
		row := vacuumHorizonPrivacyRow()
		row[0], row[1], row[3], row[16] = "transfer_escrow", "25000001", "25000000", test.age
		row[20] = "UPDATE synthetic_work SET value='" + vacuumHorizonPrivateLiteral + "'"
		alert := vacuumHorizonPrivacyObserve(t, row)
		if strings.Contains(alert.Mechanism, "normal trigger-to-completion cleanup") != test.normal ||
			!strings.Contains(alert.Verify, "transfer_escrow returns below 25M") {
			t.Fatal("privacy correction changed old-horizon versus progressing-cascade attribution")
		}
	}
}

// Source failures remain errors and cannot-observe findings, never healthy samples.
func TestVacuumHorizonPrivacyUnavailableSourceControls(t *testing.T) {
	for _, sourceErr := range []error{errors.New(vacuumHorizonPrivateLiteral), context.Canceled, context.DeadlineExceeded} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return nil, sourceErr }}
		settings := syntheticSettings(source)
		alerts, err := NewWithSignals(settings, NewVacuumHealthSignal()).Run(context.Background())
		if !errors.Is(err, sourceErr) || len(alerts) != 1 || alerts[0].SignalID != "monitor/visibility" ||
			alerts[0].Class != "cannot-observe" || alerts[0].Severity != SeverityWarn || alerts[0].Sustain != 2 {
			t.Fatal("source failure lost its error or unknown-observation authority")
		}
		vacuumHorizonAssertPublicPrivacy(t, alerts[0])
	}
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return nil, nil }}
	alerts, err := NewWithSignals(syntheticSettings(source), NewVacuumHealthSignal()).Run(context.Background())
	if err != nil || len(alerts) != 0 {
		t.Fatal("complete empty source stopped being healthy")
	}
}
