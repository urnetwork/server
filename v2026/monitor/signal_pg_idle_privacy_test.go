package monitor

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

const idleSuccessPrivateLiteral = "synthetic-idle-private-value"
const idleSuccessPrivateTaskId = "00000000-0000-4000-8000-000000000123"
const idleSuccessPrivateApplication = "synthetic-private-application-identity"
const idleSuccessPrivateMalformedNumber = "synthetic-private-numeric-cell"

// Each returned cell fits the real idle battery projection; no live SQL runs.
func idleSuccessPrivateRows() []Row {
	query := "SELECT '" + idleSuccessPrivateLiteral + "' FROM task WHERE task_id='" + idleSuccessPrivateTaskId + "'"
	return []Row{
		{"0", "oldest", "1", "7", "73111", idleSuccessPrivateApplication, query},
		{"1", "shape", "101", "7", "0", "", query[:90]},
	}
}

// The actual Monitor owns probe conversion, latch state, and reusable Alerts.
func idleSuccessPrivateMonitor(t *testing.T, summary Row, rows []Row) (*Monitor, *int, *int) {
	t.Helper()
	summaryCalls, batteryCalls := new(int), new(int)
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "count(*) FILTER"):
			*summaryCalls++
			return []Row{summary}, nil
		case strings.Contains(query, "WITH idle AS MATERIALIZED"):
			*batteryCalls++
			return rows, nil
		default:
			t.Fatal("unexpected synthetic query; real source fallback is prohibited")
			return nil, nil
		}
	}}
	return NewWithSignals(syntheticSettings(source), NewPostgresStateSignal()), summaryCalls, batteryCalls
}

// Never print rendered evidence or fixture marker values, even on a failure.
func idleSuccessPrivateAlert(t *testing.T, monitor *Monitor) Alert {
	t.Helper()
	alerts, err := monitor.Run(context.Background())
	if err != nil || len(alerts) != 1 {
		t.Fatal("synthetic count violation did not produce exactly one alert")
	}
	alert := alerts[0]
	if alert.SignalID != "pg/idle-in-tx" || alert.Class != "idle-in-tx" ||
		alert.Target != "pg-1" || alert.Severity != SeverityWarn || alert.Sustain != 2 ||
		alert.Observed != "idle_in_tx=101 oldest_xact_s=7 active=0" {
		t.Fatal("optional successful data changed the core count violation")
	}
	return alert
}

func idleSuccessPrivateAssertRedacted(t *testing.T, alert Alert) {
	t.Helper()
	var jsonl bytes.Buffer
	if err := WriteAlertsJSONL(&jsonl, []Alert{alert}); err != nil {
		t.Fatal("synthetic JSONL rendering failed")
	}
	for _, rendered := range []struct {
		name string
		text string
	}{
		{name: "Alert evidence", text: alert.Evidence},
		{name: "Alert Markdown", text: alert.Markdown()},
		{name: "complete Markdown", text: AlertsMarkdown([]Alert{alert})},
		{name: "JSONL", text: jsonl.String()},
	} {
		for _, marker := range []struct {
			name  string
			value string
		}{
			{name: "SQL literal", value: idleSuccessPrivateLiteral},
			{name: "durable task identifier", value: idleSuccessPrivateTaskId},
			{name: "application identity", value: idleSuccessPrivateApplication},
			{name: "malformed numeric cell", value: idleSuccessPrivateMalformedNumber},
		} {
			if strings.Contains(rendered.text, marker.value) {
				t.Errorf("%s retained a synthetic %s", rendered.name, marker.name)
			}
		}
	}
}

func TestPostgresIdleSuccessfulEvidenceRejectsSyntheticLiteralAndIdentity(t *testing.T) {
	monitor, summaryCalls, batteryCalls := idleSuccessPrivateMonitor(t,
		Row{"0", "101", "7", "125"}, idleSuccessPrivateRows())
	alert := idleSuccessPrivateAlert(t, monitor)
	if *summaryCalls != 1 || *batteryCalls != 1 {
		t.Fatal("synthetic probe changed its bounded collection count")
	}
	idleSuccessPrivateAssertRedacted(t, alert)
}

func TestPostgresIdleSuccessfulEvidenceRejectsCachedSyntheticLiteralAndIdentity(t *testing.T) {
	monitor, summaryCalls, batteryCalls := idleSuccessPrivateMonitor(t,
		Row{"0", "101", "7", "125"}, idleSuccessPrivateRows())
	for tick := 0; tick < 2; tick++ {
		alert := idleSuccessPrivateAlert(t, monitor)
		if (tick > 0) != strings.Contains(alert.Evidence, "battery collected once at trip") {
			t.Fatal("synthetic repeat lost its trip-cache provenance")
		}
		idleSuccessPrivateAssertRedacted(t, alert)
	}
	if *summaryCalls != 2 || *batteryCalls != 1 {
		t.Fatal("privacy correction changed the trip-cache collection policy")
	}
}

func TestPostgresIdleSuccessfulEvidenceParameterizedControl(t *testing.T) {
	query := "UPDATE transfer_contract SET outcome = $2 WHERE contract_id = $1"
	monitor, _, batteryCalls := idleSuccessPrivateMonitor(t,
		Row{"0", "101", "7", "125"}, []Row{
			{"0", "oldest", "1", "7", "73111", "", query},
			{"1", "shape", "101", "7", "0", "", query},
		})
	alert := idleSuccessPrivateAlert(t, monitor)
	for _, diagnostic := range []string{
		"oldest transaction: pid=73111 continuously_idle=7s application=withheld query=withheld",
		"backends=101 oldest_continuous_idle=7s query=withheld",
	} {
		if !strings.Contains(alert.Evidence, diagnostic) {
			t.Error("parameterized diagnostic lost its bounded numeric attribution or withholding marker")
		}
	}
	if *batteryCalls != 1 || strings.Contains(alert.Evidence, "battery failed") || strings.Contains(alert.Evidence, query) {
		t.Fatal("successful parameterized input became an observation error or retained SQL")
	}
	idleSuccessPrivateAssertRedacted(t, alert)
}

func TestPostgresIdleSuccessfulEvidenceEmptyDiagnosticPreservesViolation(t *testing.T) {
	monitor, _, batteryCalls := idleSuccessPrivateMonitor(t, Row{"0", "101", "7", "125"}, nil)
	alert := idleSuccessPrivateAlert(t, monitor)
	if *batteryCalls != 1 || alert.Evidence != "idle-in-tx by last query shape:" {
		t.Fatal("legitimately empty optional grouping changed the concrete violation")
	}
	if !strings.Contains(alert.Context, "separate snapshots") {
		t.Fatal("empty optional grouping lost its non-atomic attribution qualifier")
	}
}

func TestPostgresIdleSuccessfulEvidenceHealthySummarySkipsBattery(t *testing.T) {
	monitor, summaryCalls, batteryCalls := idleSuccessPrivateMonitor(t,
		Row{"0", "2", "7", "20"}, idleSuccessPrivateRows())
	alerts, err := monitor.Run(context.Background())
	if err != nil || len(alerts) != 0 || *summaryCalls != 1 || *batteryCalls != 0 {
		t.Fatal("healthy state ran an optional battery or emitted a false alert")
	}
}

func TestPostgresIdleSuccessfulEvidenceMalformedNumbersPreserveViolation(t *testing.T) {
	for _, test := range []struct {
		name   string
		row    int
		column int
		value  string
	}{
		{name: "oldest count sentinel", column: 2, value: idleSuccessPrivateMalformedNumber},
		{name: "oldest age sentinel", column: 3, value: idleSuccessPrivateMalformedNumber},
		{name: "oldest pid sentinel", column: 4, value: idleSuccessPrivateMalformedNumber},
		{name: "shape count sentinel", row: 1, column: 2, value: idleSuccessPrivateMalformedNumber},
		{name: "shape age sentinel", row: 1, column: 3, value: idleSuccessPrivateMalformedNumber},
		{name: "shape pid sentinel", row: 1, column: 4, value: idleSuccessPrivateMalformedNumber},
		{name: "negative count", row: 1, column: 2, value: "-1"},
		{name: "fractional age", row: 1, column: 3, value: "0.5"},
		{name: "overflowing pid", column: 4, value: "9223372036854775808"},
		{name: "empty age", row: 1, column: 3},
	} {
		rows := idleSuccessPrivateRows()
		rows[test.row][test.column] = test.value
		monitor, summaryCalls, batteryCalls := idleSuccessPrivateMonitor(t,
			Row{"0", "101", "7", "125"}, rows)
		for tick := 0; tick < 2; tick++ {
			alert := idleSuccessPrivateAlert(t, monitor)
			if !strings.HasPrefix(alert.Evidence, "idle-tx battery failed: error_class=invalid-response") ||
				strings.Contains(alert.Evidence, "oldest transaction:") || strings.Contains(alert.Evidence, "backends=") {
				t.Errorf("%s retained partial optional attribution or lost its fixed invalid-response class", test.name)
			}
			if (tick > 0) != strings.Contains(alert.Evidence, "battery collected once at trip") {
				t.Errorf("%s changed trip-cache provenance", test.name)
			}
			idleSuccessPrivateAssertRedacted(t, alert)
		}
		if *summaryCalls != 2 || *batteryCalls != 1 {
			t.Errorf("%s changed the bounded trip-cache policy", test.name)
		}
	}
}

func TestPostgresIdleSuccessfulEvidenceMalformedProjectionPreservesViolation(t *testing.T) {
	for _, test := range []struct {
		name string
		row  Row
	}{
		{name: "missing columns", row: Row{"1", "shape", "101", "7"}},
		{name: "extra column", row: Row{"1", "shape", "101", "7", "0", "", "", idleSuccessPrivateMalformedNumber}},
		{name: "unknown kind", row: Row{"1", idleSuccessPrivateMalformedNumber, "101", "7", "0", "", ""}},
	} {
		rows := idleSuccessPrivateRows()
		rows[1] = test.row
		monitor, _, batteryCalls := idleSuccessPrivateMonitor(t, Row{"0", "101", "7", "125"}, rows)
		alert := idleSuccessPrivateAlert(t, monitor)
		if alert.Evidence != "idle-tx battery failed: error_class=invalid-response" || *batteryCalls != 1 {
			t.Errorf("%s changed the core violation or retained partial optional attribution", test.name)
		}
		idleSuccessPrivateAssertRedacted(t, alert)
	}
}
