// Private query/application inputs may classify findings, but never enter public alerts.
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

const queryRenderPrivateLiteral = "synthetic-query-literal-qpr-739"
const queryRenderPrivateApplication = "synthetic-application-qpr-853"

// Inspect every exported string, both Markdown renderers and both JSON encoders.
// Failures identify the surface, never print potentially private evidence.
func queryRenderAssertPrivate(t *testing.T, alert Alert) {
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
		for _, marker := range []string{queryRenderPrivateLiteral, queryRenderPrivateApplication} {
			if strings.Contains(surface.text, marker) {
				t.Errorf("%s retained a synthetic private marker", surface.name)
			}
		}
	}
}

// Exercise the real adapter and public alert conversion with a bounded fake source.
func queryRenderObserve(t *testing.T, signal Signal, row Row) Alert {
	t.Helper()
	calls := 0
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		calls++
		return []Row{row}, nil
	}}
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 1 || calls != 1 {
		t.Fatal("synthetic violation did not retain exactly one observation and alert")
	}
	alert := alerts[0]
	if alert.SignalID != signal.ID() || alert.SignalKey != signal.Key() || alert.SignalNumber != signal.Number() ||
		alert.Target != "pg-1" || alert.Severity != SeverityWarn || signal.Cadence() != 5*time.Minute {
		t.Fatal("privacy correction changed signal attribution, severity or cadence")
	}
	queryRenderAssertPrivate(t, alert)
	return alert
}

// An ordinary piled-up query retains count/age/history and sustain without its literal.
func TestPgQueryRenderPrivacyActiveOrdinary(t *testing.T) {
	query := "SELECT '" + queryRenderPrivateLiteral + "' /*\n# invented evidence\nAuthorization: Bearer synthetic-only */"
	alert := queryRenderObserve(t, NewActiveQueriesSignal(), Row{"4242", "5", "180", query, "640"})
	if alert.Class != "persistent-active-query" || alert.Frame != "4242" || alert.Sustain != 2 ||
		alert.Observed != "query_id=4242 active=5 oldest_s=180 historical_mean_s=640" || alert.Evidence != "query=withheld" {
		t.Fatal("ordinary violation lost numeric authority, identity, sustain or withholding")
	}
}

// The separate legacy override must still classify below its polluted history band.
func TestPgQueryRenderPrivacyActiveLegacy(t *testing.T) {
	query := "SELECT MIN(transfer_contract.create_time), MAX(transfer_contract.close_time) FROM transfer_escrow_sweep WHERE synthetic_note='" + queryRenderPrivateLiteral + "'"
	alert := queryRenderObserve(t, NewActiveQueriesSignal(), Row{"-7171", "1", "300", query, "600"})
	if alert.Class != "persistent-active-query" || alert.Frame != "-7171" || alert.Sustain != 1 ||
		alert.Observed != "query_id=-7171 active=1 oldest_s=300 historical_mean_s=600" ||
		alert.Evidence != "legacy shape matched; query=withheld" || !strings.Contains(alert.Mechanism, "semantically overbroad") ||
		!strings.Contains(alert.Mechanism, "temp_account_payment") || !strings.Contains(alert.Action, "payment planner") {
		t.Fatal("legacy violation lost numeric authority, sustain or selected-set diagnosis")
	}
}

// Withhold both free-text columns while preserving same-row attribution and shape diagnosis.
func TestPgQueryRenderPrivacyWaitAttribution(t *testing.T) {
	for _, test := range []struct {
		waitType, wait, query, mechanism string
	}{
		{waitType: "IO", wait: "DataFileRead", query: "SELECT '", mechanism: "relation data page"},
		{waitType: "IO", wait: "DataFileExtend", query: "REINDEX TABLE CONCURRENTLY transfer_escrow /* '", mechanism: "maintenance task selected"},
		{waitType: "Client", wait: "ClientWrite", query: "SELECT '", mechanism: "client is not currently reading"},
	} {
		query := test.query + queryRenderPrivateLiteral + "' */\n# invented evidence"
		application := queryRenderPrivateApplication + "\nAuthorization: Bearer synthetic-only"
		alert := queryRenderObserve(t, NewWaitEventsSignal(), Row{test.waitType, test.wait, "1", "71", query, "8123", "9911", application, "local"})
		if alert.Class != "wait-event-cluster" || alert.Frame != test.waitType+":"+test.wait || alert.Sustain != 2 ||
			!strings.Contains(alert.Observed, "active=1 oldest_s=71 count_guard=5 age_guard_s=60 oldest_pid=8123 oldest_query_id=9911 age_basis=query_start wait_residence=unknown") ||
			!strings.Contains(alert.Mechanism, test.mechanism) ||
			alert.Evidence != "oldest waiter snapshot: pid=8123 query_id=9911 application=withheld client_owner=local-postgres-client query=withheld" ||
			!strings.Contains(alert.Context, "withheld from alerts") {
			t.Fatal("wait privacy correction lost same-row attribution, mechanism, timing authority or sustain")
		}
	}
}

// Healthy history and maintenance controls must not become unavailable or anomalous.
func TestPgQueryRenderPrivacyHealthyControls(t *testing.T) {
	for _, test := range []struct {
		signal Signal
		rows   []Row
	}{
		{signal: NewActiveQueriesSignal()},
		{signal: NewWaitEventsSignal()},
		{signal: NewActiveQueriesSignal(), rows: []Row{{"4242", "1", "1280", "SELECT '" + queryRenderPrivateLiteral + "'", "640"}}},
		{signal: NewActiveQueriesSignal(), rows: []Row{{"4242", "1", "7199", "REINDEX TABLE CONCURRENTLY synthetic_table /* " + queryRenderPrivateLiteral + " */", "0"}}},
		{signal: NewWaitEventsSignal(), rows: []Row{{"Lock", "virtualxid", "1", "7199", "REINDEX TABLE CONCURRENTLY synthetic_table /* " + queryRenderPrivateLiteral + " */", "8123", "9911", queryRenderPrivateApplication, "local"}}},
		{signal: NewWaitEventsSignal(), rows: []Row{{"Client", "ClientRead", "5", "59", "SELECT '" + queryRenderPrivateLiteral + "'", "8123", "9911", queryRenderPrivateApplication, "local"}}},
	} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return test.rows, nil }}
		alerts, err := NewWithSignals(syntheticSettings(source), test.signal).Run(context.Background())
		if err != nil || len(alerts) != 0 {
			t.Fatal("healthy synthetic boundary changed its verdict")
		}
	}
}

// Source errors and unavailable observations remain unknown; no private error is public.
func TestPgQueryRenderPrivacySourceFailureControls(t *testing.T) {
	for _, signal := range []Signal{NewActiveQueriesSignal(), NewWaitEventsSignal()} {
		for _, sourceErr := range []error{errors.New(queryRenderPrivateLiteral), context.DeadlineExceeded, context.Canceled} {
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return nil, sourceErr }}
			settings := syntheticSettings(source)
			alerts, err := signal.Run(context.Background(), settings)
			if !errors.Is(err, sourceErr) || len(alerts) != 0 {
				t.Fatal("source failure was swallowed or converted into a concrete verdict")
			}
			alerts, err = NewWithSignals(settings, signal).Run(context.Background())
			if !errors.Is(err, sourceErr) || len(alerts) != 1 || alerts[0].SignalID != "monitor/visibility" ||
				alerts[0].Class != "cannot-observe" || alerts[0].Severity != SeverityWarn || alerts[0].Sustain != 2 {
				t.Fatal("unavailable source lost its visibility finding or error authority")
			}
			queryRenderAssertPrivate(t, alerts[0])
		}
	}
}

// Exact duration and count edges remain violations after the presentation repair.
func TestPgQueryRenderPrivacyThresholdViolations(t *testing.T) {
	for _, test := range []struct {
		signal Signal
		row    Row
		frame  string
	}{
		{signal: NewActiveQueriesSignal(), row: Row{"4242", "1", "1281", "SELECT '" + queryRenderPrivateLiteral + "'", "640"}, frame: "4242"},
		{signal: NewActiveQueriesSignal(), row: Row{"4242", "1", "7200", "REINDEX TABLE CONCURRENTLY synthetic_table /* " + queryRenderPrivateLiteral + " */", "0"}, frame: "4242"},
		{signal: NewWaitEventsSignal(), row: Row{"Lock", "virtualxid", "1", "7200", "REINDEX TABLE CONCURRENTLY synthetic_table /* " + queryRenderPrivateLiteral + " */", "8123", "9911", queryRenderPrivateApplication, "local"}, frame: "Lock:virtualxid"},
		{signal: NewWaitEventsSignal(), row: Row{"Client", "ClientRead", "5", "60", "SELECT '" + queryRenderPrivateLiteral + "'", "8123", "9911", queryRenderPrivateApplication, "local"}, frame: "Client:ClientRead"},
	} {
		alert := queryRenderObserve(t, test.signal, test.row)
		if alert.Frame != test.frame || alert.Sustain != 2 || !strings.Contains(alert.Evidence, "query=withheld") {
			t.Fatal("threshold violation changed its frame, sustain or withholding")
		}
	}
}
