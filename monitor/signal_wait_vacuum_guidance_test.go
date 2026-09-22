package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func waitAgePrivateRun(t *testing.T, rows []Row) (Alerts, error) {
	t.Helper()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if !strings.Contains(query, "max(clock_timestamp()-query_start)") ||
			!strings.Contains(query, "HAVING count(*) >= 5 OR max(clock_timestamp()-query_start) > interval '1 minute'") {
			t.Fatal("query-age source or count/age guard changed")
		}
		return rows, nil
	}}
	signal := NewWaitEventsSignal()
	if signal.Cadence() != 5*time.Minute {
		t.Fatal("wait-event cadence changed")
	}
	return NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
}

// The query is old; this observation contains no wait-entry timestamp.
func TestWaitAgeProjectionDoesNotInventWaitResidence(t *testing.T) {
	alerts, err := waitAgePrivateRun(t, []Row{{
		"IO", "DataFileRead", "1", "71", "SELECT value FROM synthetic_work WHERE key=$1",
		"4242", "77", "synthetic-worker", "local",
	}})
	if err != nil || len(alerts) != 1 {
		t.Fatal("aged query sampled waiting lost its alert")
	}
	alert := alerts[0]
	if alert.SignalID != "pg/wait-events" || alert.Class != "wait-event-cluster" ||
		alert.Frame != "IO:DataFileRead" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
		t.Fatal("query-age clarification changed the alert identity or gate")
	}
	if !strings.Contains(alert.Observed, "active=1") || !strings.Contains(alert.Observed, "71") {
		t.Fatal("query age or count disappeared")
	}
	if !strings.Contains(alert.Context, "query_start") || !strings.Contains(alert.Context, "wait residence") {
		t.Error("alert does not distinguish measured query age from unknown wait residence")
	}
	for _, text := range []string{alert.Baseline, alert.Verify, alert.Markdown()} {
		if strings.Contains(text, "remains on one wait event for more than one minute") ||
			strings.Contains(text, "command remains on it beyond one minute") {
			t.Error("query-start age was asserted to be time spent on one wait event")
		}
	}
}

func TestWaitAgeProjectionCountOnlyControl(t *testing.T) {
	alerts, err := waitAgePrivateRun(t, []Row{{
		"IPC", "BgworkerShutdown", "5", "0", "SELECT value FROM synthetic_work",
		"4242", "77", "synthetic-worker", "local",
	}})
	if err != nil || len(alerts) != 1 || alerts[0].Class != "wait-event-cluster" ||
		alerts[0].Severity != SeverityWarn || alerts[0].Sustain != 2 ||
		!strings.Contains(alerts[0].Context, "One-shot observations bypass sustain") {
		t.Fatal("count-only one-shot changed its warning or recurrence qualifier")
	}
}

func TestWaitAgeProjectionHealthyAndIncompleteControls(t *testing.T) {
	for _, fixture := range []struct {
		rows    []Row
		unknown bool
	}{
		{rows: nil},
		{rows: []Row{{"Client", "ClientRead", "5", "0", "BEGIN", "4242", "unknown", "synthetic-worker", "local"}}},
		{rows: []Row{{"IPC", "SyntheticWait"}}, unknown: true},
	} {
		alerts, err := waitAgePrivateRun(t, fixture.rows)
		if fixture.unknown {
			if err == nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
				t.Error("incomplete wait observation became a healthy or concrete result")
			}
		} else if err != nil || len(alerts) != 0 {
			t.Error("empty or young ClientRead control changed")
		}
	}
}

func vacuumReliabilityPrivateRow(query string) Row {
	return Row{
		"transfer_escrow", "25100000", "01-01 00:00", "25000000",
		"scanning heap", "120", "1000", "900", "0", "0", "0", "3",
		"4242", "2000000", "", "123456", "120", "active", "client backend", "synthetic-worker", query,
	}
}

func vacuumReliabilityPrivateRun(t *testing.T, rows []Row, sourceErr error) (Alerts, error) {
	t.Helper()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if query != pgVacuumHealthSQL {
			t.Fatal("vacuum guidance started an additional source query")
		}
		return rows, sourceErr
	}}
	signal := NewVacuumHealthSignal()
	if signal.Cadence() != 5*time.Minute {
		t.Fatal("vacuum cadence changed")
	}
	return NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
}

// Both real maintenance branches share this prefix; ON CONFLICT occurs after
// the production 160-character sample boundary, not inside the observation.
func TestVacuumReliabilityPrefixCannotProveFullAnchorOrLegacyCadence(t *testing.T) {
	prefix := "INSERT INTO client_reliability_running (client_id, lookback_index, network_id, independent_sum, reliability_sum) SELECT client_id, $1, network_id, independent_sum, reliability_sum FROM synthetic_reliability_input"
	full := prefix
	rolling := prefix + " ON CONFLICT (client_id, lookback_index) DO UPDATE SET independent_sum=client_reliability_running.independent_sum+EXCLUDED.independent_sum"
	if len(prefix) <= 160 || full[:160] != rolling[:160] || !strings.Contains(pgVacuumHealthSQL, "160) AS query") {
		t.Fatal("fixture does not exercise the production shared-prefix ambiguity")
	}
	for _, query := range []string{full, rolling} {
		alerts, err := vacuumReliabilityPrivateRun(t, []Row{vacuumReliabilityPrivateRow(query[:160])}, nil)
		if err != nil || len(alerts) != 1 {
			t.Fatal("ambiguous maintenance prefix erased the configured-threshold warning")
		}
		alert := alerts[0]
		if alert.SignalID != "pg/dead-tuples" || alert.Class != "dead-tuples" ||
			alert.Frame != "transfer_escrow" || alert.Severity != SeverityWarn || alert.Sustain != 1 ||
			!strings.Contains(alert.Observed, "n_dead_tup=25100000 alert_threshold=25000000") ||
			!strings.Contains(alert.Evidence, "oldest MVCC horizon candidate:") {
			t.Fatal("maintenance qualification changed threshold, identity, severity, or horizon evidence")
		}
		if !strings.Contains(strings.ToLower(alert.Mechanism), "reliability") || !strings.Contains(strings.ToLower(alert.Context), "phase") {
			t.Error("ambiguous maintenance prefix lost owner context or the missing phase discriminator")
		}
		for _, assertion := range []string{
			"performing a full running-window re-anchor",
			"pre-fix threshold was shorter than the task cadence",
			"roll out the four-hour reliability re-anchor cadence",
		} {
			if strings.Contains(strings.ToLower(alert.Markdown()), strings.ToLower(assertion)) {
				t.Error("truncated shared prefix acquired an unproved full-anchor or legacy-artifact diagnosis")
			}
		}
	}
}

func TestVacuumReliabilityPrefixHealthyAndSourceUnknownControls(t *testing.T) {
	below := vacuumReliabilityPrivateRow("SELECT value FROM synthetic_work")
	below[1] = "25000000"
	for _, fixture := range []struct {
		rows    []Row
		err     error
		unknown bool
	}{
		{rows: []Row{below}},
		{rows: nil},
		{err: errors.New("synthetic vacuum source unavailable"), unknown: true},
	} {
		alerts, err := vacuumReliabilityPrivateRun(t, fixture.rows, fixture.err)
		if fixture.unknown {
			if err == nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
				t.Error("failed vacuum collection became healthy")
			}
		} else if err != nil || len(alerts) != 0 {
			t.Error("threshold-equal or empty vacuum control changed")
		}
	}
}

func TestVacuumReliabilityPrefixUnrelatedHorizonControl(t *testing.T) {
	alerts, err := vacuumReliabilityPrivateRun(t,
		[]Row{vacuumReliabilityPrivateRow("SELECT value FROM synthetic_work WHERE key=$1")}, nil)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "dead-tuples" ||
		strings.Contains(alerts[0].Mechanism, "reliability") ||
		!strings.Contains(alerts[0].Evidence, "heap_scanned=900/1000") {
		t.Fatal("unrelated old horizon or active vacuum acquired the reliability diagnosis")
	}
}
