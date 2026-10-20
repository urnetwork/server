// Keep wait-age and vacuum guidance within their observed source authority.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Exercise the public monitor with the owning CSV parser and one source read.
func waitAgePrivateRun(t *testing.T, rows []Row) (Alerts, error) {
	t.Helper()
	reads := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		reads++
		if !strings.Contains(query, "max(clock_timestamp()-query_start)") ||
			!strings.Contains(query, "HAVING count(*) >= 5 OR max(clock_timestamp()-query_start) > interval '1 minute'") {
			t.Fatal("query-age source or count/age guard changed")
		}
		return pgGroupedFixtureRows(t, rows), nil
	}}
	signal := NewWaitEventsSignal()
	if signal.Cadence() != 5*time.Minute {
		t.Fatal("wait-event cadence changed")
	}
	alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
	if reads != 1 {
		t.Fatal("wait projection changed its single source-read boundary")
	}
	return alerts, err
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

// Invalid groups are explicit partial coverage, not an all-or-nothing source
// error. The coverage marker must survive the public adapter for retention.
func TestWaitAgeProjectionHealthyAndIncompleteControls(t *testing.T) {
	for _, fixture := range []struct {
		rows    []Row
		unknown bool
	}{
		{rows: nil},
		{rows: []Row{{"Client", "ClientRead", "5", "0", "BEGIN", "4242", "unknown", "synthetic-worker", "local"}}},
		{rows: []Row{{"IPC", "SyntheticWait"}}, unknown: true},
		{rows: []Row{{}}, unknown: true},
		{rows: []Row{{"Client", "ClientRead", "5", "private-wait-age-canary", "SELECT synthetic_value", "4242", "unknown", "synthetic-worker", "local"}}, unknown: true},
	} {
		alerts, err := waitAgePrivateRun(t, fixture.rows)
		if err != nil {
			t.Fatal("wait projection lost its explicit partial-coverage result")
		}
		if fixture.unknown {
			if len(alerts) != 1 {
				t.Fatal("incomplete wait observation became a healthy or concrete result")
			}
			requireWaitAgeProjectionCoverage(t, alerts[0])
		} else if len(alerts) != 0 {
			t.Error("empty or young ClientRead control changed")
		}
	}
}

// Coverage has one fixed identity and public reason; malformed source values
// never become a family, diagnosis, or consumer recovery authority.
func requireWaitAgeProjectionCoverage(t *testing.T, alert Alert) {
	t.Helper()
	if alert.Identity() != "monitor/visibility|cannot-observe|pg-1|wait-event-groups" ||
		alert.SignalNumber != "2.2" || alert.SignalKey != "wait-events" ||
		alert.Severity != SeverityWarn || alert.Sustain != 2 || !alert.unobservable ||
		alert.Observed != "error_class=observation-invalid-response component=wait-event-groups reason=invalid-grouped-projection" {
		t.Fatal("incomplete wait projection lost its fixed coverage identity or retention marker")
	}
	requireAlertOmits(t, alert, "private-wait-age-canary")
}

// A malformed family cannot erase an independently valid aged query, in
// either source order, or expose its rejected age and private attribution.
func TestWaitAgeProjectionPartialGroupsRetainPrivateIncident(t *testing.T) {
	valid := Row{"IO", "DataFileRead", "1", "71", "SELECT 'private-wait-query-canary'", "4242", "77", "private-wait-application-canary", "192.0.2.77"}
	invalid := Row{"Client", "ClientRead", "5", "private-wait-age-canary", "SELECT 'private-wait-query-canary'", "4243", "78", "private-wait-application-canary", "192.0.2.78"}
	for _, rows := range [][]Row{{valid, invalid}, {invalid, valid}, {invalid, valid, invalid}} {
		alerts, err := waitAgePrivateRun(t, rows)
		if err != nil || len(alerts) != 2 {
			t.Fatal("partial wait projection discarded its independent incident or coverage")
		}
		incident := requireAlertClass(t, alerts, "wait-event-cluster")
		if incident.Identity() != "pg/wait-events|wait-event-cluster|pg-1|IO:DataFileRead" ||
			incident.Severity != SeverityWarn || incident.Sustain != 2 || incident.unobservable ||
			!strings.Contains(incident.Observed, "active=1 oldest_s=71") ||
			!strings.Contains(incident.Observed, "age_basis=query_start wait_residence=unknown") {
			t.Fatal("partial wait projection changed the independent count, query age, or identity")
		}
		requireWaitAgeProjectionCoverage(t, requireAlertClass(t, alerts, "cannot-observe"))
		var jsonl strings.Builder
		if err := alerts.WriteJSONL(&jsonl); err != nil {
			t.Fatal("wait projection JSONL rendering failed")
		}
		for _, private := range []string{"private-wait-query-canary", "private-wait-application-canary", "private-wait-age-canary", "192.0.2.77", "192.0.2.78"} {
			if strings.Contains(alerts.Markdown(), private) || strings.Contains(jsonl.String(), private) {
				t.Fatal("private wait projection escaped into public rendering")
			}
		}
	}
}

// Rows returned alongside a failed source call have no current authority,
// even when they repeat the prior successful observation exactly.
func TestWaitAgeProjectionFailedSourceCannotReusePriorRows(t *testing.T) {
	priorRows := []Row{{"IO", "DataFileRead", "1", "71", "SELECT synthetic_value", "4242", "77", "synthetic-worker", "local"}}
	privateErr := errors.New("private-wait-source-canary")
	var sourceErr error
	reads := 0
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		reads++
		return pgGroupedFixtureRows(t, priorRows), sourceErr
	}}
	signal := NewWaitEventsSignal()
	settings := syntheticSettings(source)
	now := settings.Now()
	settings.Now = func() time.Time { return now }
	monitor := NewWithSignals(settings, signal)
	alerts, err := monitor.Run(context.Background())
	if err != nil || len(alerts) != 1 || alerts[0].Class != "wait-event-cluster" {
		t.Fatal("failed-source control did not establish its prior valid observation")
	}
	sourceErr = privateErr
	now = now.Add(signal.Cadence())
	alerts, err = monitor.Run(context.Background())
	if !errors.Is(err, privateErr) || len(alerts) != 1 ||
		alerts[0].Identity() != "monitor/visibility|cannot-observe|pg/wait-events|" || !alerts[0].unobservable ||
		alerts[0].Severity != SeverityWarn || alerts[0].Sustain != 2 || reads != 2 {
		t.Fatal("failed wait source reused prior rows or lost its execution failure")
	}
	requireAlertOmits(t, alerts[0], "private-wait-source-canary", "oldest_s=71")
}

// The public signal feeds the real cadence and incident reducers without a
// sender. Held and emitted coverage retain prior evidence at its original
// clock; a returning family does not re-raise, and two complete runs can clear.
func TestWaitAgeProjectionPartialCoverageRetainsPriorIncident(t *testing.T) {
	valid := Row{"IO", "DataFileRead", "1", "71", "SELECT synthetic_value", "4242", "77", "synthetic-worker", "local"}
	invalid := Row{"Client", "ClientRead", "5", "private-wait-age-canary", "SELECT synthetic_value", "4243", "78", "synthetic-worker", "local"}
	signal := NewWaitEventsSignal()
	gate := newCadenceAlertGate()
	notifier := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	var rows []Row
	reads := 0
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		reads++
		return pgGroupedFixtureRows(t, rows), nil
	}}
	settings := syntheticSettings(source)
	start := settings.Now()
	now := start
	settings.Now = func() time.Time { return now }
	lastReadyAt := time.Time{}
	for run, observation := range []struct {
		rows        []Row
		ready, held int
	}{
		{rows: []Row{valid}, held: 1},
		{rows: []Row{valid}, ready: 1},
		{rows: []Row{invalid}, held: 1},
		{rows: []Row{invalid}, ready: 1},
		{rows: []Row{valid}, held: 1},
		{rows: []Row{valid}, ready: 1},
		{},
		{},
	} {
		rows = observation.rows
		now = start.Add(time.Duration(run) * signal.Cadence())
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("wait retention control lost its public partial-coverage result")
		}
		if run == 2 || run == 3 {
			if len(alerts) != 1 {
				t.Fatal("partial wait sample fabricated current incident evidence")
			}
			requireWaitAgeProjectionCoverage(t, alerts[0])
		}
		ready, held := gate.split(signal, alerts)
		if len(ready) != observation.ready || len(held) != observation.held {
			t.Fatalf("wait retention control changed cadence admission at step %d", run)
		}
		notifier.stateLock.Lock()
		transitions := notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
		notifier.stateLock.Unlock()
		incidentTransitions := []slackTransition{}
		for _, transition := range transitions {
			if transition.alert.Class == "wait-event-cluster" {
				incidentTransitions = append(incidentTransitions, transition)
			}
		}
		if run == 1 {
			if len(incidentTransitions) != 1 || incidentTransitions[0].kind != slackRaised {
				t.Fatal("two valid wait observations did not raise the incident")
			}
		} else if run == 7 {
			if len(incidentTransitions) != 1 || incidentTransitions[0].kind != slackCleared {
				t.Fatal("two complete healthy wait observations did not clear the incident")
			}
		} else if len(incidentTransitions) != 0 {
			t.Fatal("partial wait coverage or one healthy run cleared or re-raised the incident")
		}
		if run == 1 || run == 5 {
			lastReadyAt = now
		}
		incident := notifier.keyIncidents["pg/wait-events|wait-event-cluster|pg-1"]
		if 1 <= run && run < 7 {
			if incident == nil || incident.alert.Identity() != "pg/wait-events|wait-event-cluster|pg-1|IO:DataFileRead" ||
				!incident.alert.ObservedAt.Equal(lastReadyAt) {
				t.Fatal("partial wait coverage erased or refreshed prior evidence without a current ready observation")
			}
		} else if incident != nil {
			t.Fatal("wait incident existed before sustain or after complete recovery")
		}
	}
	if reads != 8 {
		t.Fatal("wait retention control changed its one-read-per-observation boundary")
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
