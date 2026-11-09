package monitor

// SIGNALS.md §30.3 on fixture answers: the primary refusing on its host, or answering in recovery, pages
// §19.8's rota, joining FS-50's page classes; an answer from the primary out of recovery is healthy; a host
// the monitor cannot reach, or another host answering, is a visibility error and never healthy.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The signal's alerts and error on one answer of the primary's query.
func runPostgresPrimary(t *testing.T, rows []Row, err error) (Alerts, error) {
	t.Helper()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if !strings.Contains(query, pgPrimaryHostnameMetaCommand) || !strings.Contains(query, "pg_is_in_recovery()") {
			t.Fatalf("the primary's query is %q", query)
		}
		return rows, err
	}}
	return NewPostgresPrimarySignal().Run(context.Background(), syntheticSettings(source))
}

// The primary answering as itself, out of recovery, raises nothing.
func TestPostgresPrimaryServingIsHealthy(t *testing.T) {
	alerts, err := runPostgresPrimary(t, []Row{{"pg-1", "f"}}, nil)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("alerts = %+v, err = %v", alerts, err)
	}
}

// Refused on its host, or a replica in its place: the page, at page severity, with a row in §19.8's table
// so FS-50's rota reaches a person.
func TestPostgresPrimaryDownPages(t *testing.T) {
	for _, c := range []struct {
		label string
		rows  []Row
		err   error
	}{
		{label: "refused", err: errors.New(`pg-1: exit status 2: psql: error: connection to server at "localhost" (127.0.0.1), port 5432 failed: Connection refused`)},
		{label: "starting up", err: errors.New(`pg-1: exit status 2: psql: error: connection to server at "localhost" (::1), port 5432 failed: FATAL:  the database system is starting up`)},
		{label: "in recovery", rows: []Row{{"pg-1", "t"}}},
	} {
		alerts, err := runPostgresPrimary(t, c.rows, c.err)
		if err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
		alert := requireAlertClass(t, alerts, postgresPrimaryClassDown)
		if row, paged := pageClassOf(alert.Class); !paged || alert.Severity != SeverityPage || alert.Target != "pg-1" || !strings.Contains(row.action, "restore") {
			t.Errorf("%s: %+v", c.label, alert)
		}
		if strings.Contains(alert.Markdown(), "127.0.0.1") {
			t.Errorf("%s: the page carries an address", c.label)
		}
	}
}

// What cannot tell the primary from the path to it is an error, which the registry turns into a visibility
// alert, never a page and never healthy.
func TestPostgresPrimaryBlindIsAnErrorNotAVerdict(t *testing.T) {
	for _, c := range []struct {
		label string
		rows  []Row
		err   error
	}{
		{label: "ssh refused", err: errors.New("pg-1: exit status 255: ssh: connect to host pg-1 port 22: Connection refused")},
		{label: "another host", rows: []Row{{"pg-2", "f"}}},
		{label: "malformed", rows: []Row{{"pg-1"}}},
		{label: "no rows", rows: []Row{}},
		{label: "an unknown state", rows: []Row{{"pg-1", "maybe"}}},
	} {
		alerts, err := runPostgresPrimary(t, c.rows, c.err)
		if err == nil || len(alerts) != 0 {
			t.Errorf("%s: alerts = %+v, err = %v", c.label, alerts, err)
		}
	}
	monitor := NewWithSignals(syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, errors.New("pg-1: exit status 255: ssh: connect to host pg-1 port 22: Connection timed out")
	}}), NewPostgresPrimarySignal())
	alerts, err := monitor.Run(context.Background())
	if err == nil || len(alerts) != 1 || !alerts[0].unobservable || alerts[0].Class == postgresPrimaryClassDown {
		t.Errorf("an unreachable host: alerts = %+v, err = %v", alerts, err)
	}
}
