// Deterministic public-entrypoint controls for the missing freeze-age reader.
// Fixtures use independent fixed source fields; no database or host is used.
package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const pgFreezeAgePrivateCanary = "PRIVATE_CANARY_FREEZE_AGE"

// A source-complete relation with no active pass still has an authoritative age.
func pgFreezeAgeTestRelation(id, age, limit int64) Row {
	return Row{"relation", fmt.Sprint(id), fmt.Sprint(age), fmt.Sprint(limit),
		"0", "0", "0", "none", "none", "0", "0", "0", "0", "0", "0", "0", "123", "r"}
}

// Census and generation values are independently declared, not copied from
// production schema constants. Boot and pass starts stay fixed across ticks.
func pgFreezeAgeTestRows(now time.Time, relations ...Row) []Row {
	origin := syntheticSettings(nil).Now()
	rows := []Row{{"meta", "pg-1", fmt.Sprint(now.UnixMicro()), fmt.Sprint(origin.Add(-24 * time.Hour).UnixMicro()),
		"1234", "0", "200000000", "1600000000", fmt.Sprint(len(relations)), "1", "", "", "", "", "", "", "", ""}}
	for _, relation := range relations {
		rows = append(rows, append(Row(nil), relation...))
	}
	return append(rows, Row{"end", fmt.Sprint(len(relations)), "4096", "", "", "", "", "", "", "", "", "", "", "", "", "", "", ""})
}

// The same identified anti-wraparound pass has thirteen indexes, one completed.
func pgFreezeAgeTestPass() Row {
	row := pgFreezeAgeTestRelation(10, 200000001, 200000000)
	origin := syntheticSettings(nil).Now()
	row[4], row[5], row[6] = "42", fmt.Sprint(origin.Add(-2*time.Hour).UnixMicro()), fmt.Sprint(origin.Add(-time.Hour).UnixMicro())
	row[7], row[8] = "antiwrap", "vacuuming indexes"
	row[9], row[10], row[11], row[12], row[13], row[14] = "100", "100", "0", "0", "1", "13"
	return row
}

// Keep failures fixed: no private source bytes or full alert dumps are logged.
func pgFreezeAgeTestClass(t *testing.T, alerts Alerts, class string) Alert {
	t.Helper()
	var jsonl bytes.Buffer
	if err := WriteAlertsJSONL(&jsonl, alerts); err != nil {
		t.Fatal("synthetic JSON rendering failed")
	}
	if strings.Contains(jsonl.String(), pgFreezeAgePrivateCanary) || strings.Contains(AlertsMarkdown(alerts), pgFreezeAgePrivateCanary) {
		t.Fatal("private freeze-age source entered public output")
	}
	for _, alert := range alerts {
		if alert.Class == class {
			return alert
		}
	}
	t.Fatal("required freeze-age finding is absent")
	return Alert{}
}

// The setting, not a dead-tuple floor or hard-coded 200M, owns the threshold.
func TestPgFreezeAgePublicThresholdAndRegistration(t *testing.T) {
	for _, sample := range []struct {
		age, limit int64
		risk       bool
	}{
		{0, 200000000, false}, {199999999, 200000000, false}, {200000000, 200000000, false}, {200000001, 200000000, true},
		{2147483647, 200000000, true},
		{50000000, 50000000, false}, {50000001, 50000000, true},
	} {
		now := syntheticSettings(nil).Now()
		calls := 0
		settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
			calls++
			if query != pgFreezeAgeQuery {
				t.Fatal("freeze-age source request changed")
			}
			return pgFreezeAgeTestRows(now, pgFreezeAgeTestRelation(10, sample.age, sample.limit)), nil
		}})
		signal := NewPgFreezeAgeSignal()
		if signal.Number() != "2.4d" || signal.Key() != "pg-freeze-age" || signal.ID() != "pg/freeze-age" || signal.Cadence() != 15*time.Minute {
			t.Fatal("approved freeze-age identity or cadence changed")
		}
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || calls != 1 {
			t.Fatal("freeze-age source did not complete exactly once")
		}
		if !sample.risk {
			if len(alerts) != 0 {
				t.Fatal("in-band age became a risk finding")
			}
			continue
		}
		alert := pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-risk")
		if alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.Target != "pg-1/database-1234/relation-10" || alert.unobservable {
			t.Fatal("freeze-age risk lost its approved policy or per-relation identity")
		}
		if !strings.Contains(alert.Mechanism, "not a diagnosis of stalled") || !strings.Contains(alert.Context, "cancellation and lost-work attribution remain unimplemented") {
			t.Fatal("age risk acquired unsupported stall or cancellation authority")
		}
	}
	found := false
	for _, signal := range NewSignals() {
		found = found || signal.Key() == "pg-freeze-age"
	}
	if !found {
		t.Fatal("supported freeze-age predicate remains absent from the registry")
	}
}

// The old diagnostic top-ten shape can hide the eleventh relation's risk.
func TestPgFreezeAgeCompleteCensusBeyondTopTenAndCap(t *testing.T) {
	now := syntheticSettings(nil).Now()
	relations := []Row{}
	for id := int64(1); id <= 11; id++ {
		relations = append(relations, pgFreezeAgeTestRelation(id, 1, 200000000))
	}
	relations[10][2] = "200000001"
	rows := pgFreezeAgeTestRows(now, relations...)
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }})
	alerts, err := NewPgFreezeAgeSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatal("complete eleven-relation census lost its single risk")
	}
	if pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-risk").Target != "pg-1/database-1234/relation-11" {
		t.Fatal("risk outside top ten was hidden")
	}
	relations = nil
	for id := int64(1); id <= 4096; id++ {
		relations = append(relations, pgFreezeAgeTestRelation(id, 1, 200000000))
	}
	rows = pgFreezeAgeTestRows(now, relations...)
	if alerts, err := NewPgFreezeAgeSignal().Run(context.Background(), settings); err != nil || len(alerts) != 0 {
		t.Fatal("exact complete census cap was rejected")
	}
	rows[0][8] = "4097"
	alerts, err = NewPgFreezeAgeSignal().Run(context.Background(), settings)
	if err != nil || !pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-unobservable").unobservable {
		t.Fatal("over-cap census became healthy from a subset")
	}
	for _, want := range []string{"pg_is_in_recovery()", "pg_postmaster_start_time()", "current_database()", "current_setting('autovacuum_freeze_max_age')", "current_setting('vacuum_failsafe_age')", "pg_options_to_table(c.reloptions)", "c.relkind IN ('r', 't')", "count(*) FROM relations", "count(*) FROM projected", "LIMIT 4096", "to prevent wraparound", "v.datid = (SELECT database_oid FROM source)", "1000000"} {
		if !strings.Contains(pgFreezeAgeQuery, want) {
			t.Fatal("bounded producer lost a required source authority field")
		}
	}
	if strings.Contains(pgFreezeAgeQuery, "c.relname") || strings.Contains(pgFreezeAgeQuery, "LIMIT 10;") {
		t.Fatal("producer restored names or a top-ten census")
	}
}

// Positive same-generation work is authority; unchanged long index work and
// a fresh pass are not a stall or a proven progressing anti-wraparound pass.
func TestPgFreezeAgePositiveAdvancementOnly(t *testing.T) {
	for _, sample := range []struct {
		name    string
		change  func(Row, Row)
		advance bool
	}{
		{"index-work", func(_, after Row) { after[13] = "2" }, true},
		{"heap-scan", func(before, after Row) {
			before[8], after[8] = "scanning heap", "scanning heap"
			before[10], after[10] = "40", "41"
		}, true},
		{"heap-vacuum", func(before, after Row) {
			before[8], after[8] = "vacuuming heap", "vacuuming heap"
			before[11], after[11] = "20", "21"
		}, true},
		{"completed-cycle", func(_, after Row) { after[12], after[8], after[13] = "1", "vacuuming heap", "0" }, true},
		{"long-index-unchanged", func(_, _ Row) {}, false},
		{"phase-only", func(_, after Row) { after[8] = "vacuuming heap" }, false},
		{"new-pid", func(_, after Row) { after[4], after[13] = "43", "2" }, false},
		{"reused-pid", func(_, after Row) {
			after[5] = fmt.Sprint(syntheticSettings(nil).Now().Add(-90 * time.Minute).UnixMicro())
			after[13] = "2"
		}, false},
		{"new-pass", func(_, after Row) {
			after[6] = fmt.Sprint(syntheticSettings(nil).Now().Add(-30 * time.Minute).UnixMicro())
			after[13] = "2"
		}, false},
		{"submillisecond-new-pass", func(_, after Row) {
			after[6] = fmt.Sprint(syntheticSettings(nil).Now().Add(-time.Hour).UnixMicro() + 1)
			after[13] = "2"
		}, false},
		{"ordinary-vacuum", func(_, after Row) { after[7], after[13] = "ordinary", "2" }, false},
		{"unknown-mode", func(_, after Row) { after[7], after[13] = "unknown", "2" }, false},
		{"missing-index-field", func(_, after Row) { after[13], after[14] = "-1", "-1" }, false},
		{"counter-reset", func(before, after Row) { before[12], after[12], after[13] = "1", "0", "2" }, false},
		{"heap-with-index-reset", func(before, after Row) {
			before[8], after[8] = "scanning heap", "scanning heap"
			before[10], after[10], after[13] = "40", "41", "0"
		}, false},
		{"missing-backend-start", func(_, after Row) { after[5], after[13] = "0", "2" }, false},
		{"missing-query-start", func(_, after Row) { after[6], after[13] = "0", "2" }, false},
		{"heap-regression", func(_, after Row) { after[10], after[13] = "99", "2" }, false},
		{"cycle-with-heap-reset", func(_, after Row) { after[10], after[12] = "0", "1" }, false},
		{"completion-clock-alone", func(_, after Row) { after[15] = fmt.Sprint(syntheticSettings(nil).Now().UnixMicro()) }, false},
	} {
		now := syntheticSettings(nil).Now()
		before, after := pgFreezeAgeTestPass(), pgFreezeAgeTestPass()
		sample.change(before, after)
		rows := pgFreezeAgeTestRows(now, before)
		settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }})
		settings.Now = func() time.Time { return now }
		signal := NewPgFreezeAgeSignal()
		first, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("first progress sample failed")
		}
		pgFreezeAgeTestClass(t, first, "pg-freeze-age-risk")
		now = now.Add(15 * time.Minute)
		rows = pgFreezeAgeTestRows(now, after)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: second progress sample failed", sample.name)
		}
		if sample.advance {
			if len(alerts) != 0 {
				t.Fatalf("%s: positive same-pass work failed to qualify", sample.name)
			}
		} else {
			pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-risk")
		}
	}
}

// Missing, malformed and private fields never acquire source authority.
func TestPgFreezeAgeUnknownSchemaAndPrivacy(t *testing.T) {
	for _, sample := range []struct {
		name   string
		change func([]Row) []Row
	}{
		{"empty", func([]Row) []Row { return nil }},
		{"missing-trailer", func(rows []Row) []Row { return rows[:2] }},
		{"short-row", func(rows []Row) []Row { rows[1] = rows[1][:17]; return rows }},
		{"duplicate-relation", func(rows []Row) []Row {
			rows = append(rows[:2], append(Row(nil), rows[1]...), rows[2])
			rows[0][8], rows[3][1] = "2", "2"
			return rows
		}},
		{"missing-relation", func(rows []Row) []Row { rows[0][8], rows[2][1] = "2", "2"; return rows }},
		{"complete-looking-subset", func(rows []Row) []Row { rows[0][8] = "2"; return rows }},
		{"unframed-extra", func(rows []Row) []Row { return append(rows, rows[1]) }},
		{"wrong-host", func(rows []Row) []Row { rows[0][1] = pgFreezeAgePrivateCanary; return rows }},
		{"replica", func(rows []Row) []Row { rows[0][5] = "1"; return rows }},
		{"stale", func(rows []Row) []Row {
			rows[0][2] = fmt.Sprint(syntheticSettings(nil).Now().Add(-31 * time.Second).UnixMicro())
			return rows
		}},
		{"future", func(rows []Row) []Row {
			rows[0][2] = fmt.Sprint(syntheticSettings(nil).Now().Add(31 * time.Second).UnixMicro())
			return rows
		}},
		{"negative-age", func(rows []Row) []Row { rows[1][2] = "-1"; return rows }},
		{"negative-failsafe", func(rows []Row) []Row { rows[0][7] = "-1"; return rows }},
		{"private-failsafe", func(rows []Row) []Row { rows[0][7] = pgFreezeAgePrivateCanary; return rows }},
		{"pass-after-statement", func(rows []Row) []Row {
			rows[1] = pgFreezeAgeTestPass()
			rows[1][6] = fmt.Sprint(syntheticSettings(nil).Now().UnixMicro() + 1)
			return rows
		}},
		{"completion-after-statement", func(rows []Row) []Row {
			rows[1][15] = fmt.Sprint(syntheticSettings(nil).Now().UnixMicro() + 1)
			return rows
		}},
		{"nonfinite-age", func(rows []Row) []Row { rows[1][2] = "NaN"; return rows }},
		{"overflow-age", func(rows []Row) []Row { rows[1][2] = "9223372036854775808"; return rows }},
		{"private-number", func(rows []Row) []Row { rows[1][3] = pgFreezeAgePrivateCanary; return rows }},
		{"zero-limit", func(rows []Row) []Row { rows[1][3] = "0"; return rows }},
		{"higher-than-server-limit", func(rows []Row) []Row { rows[1][3] = "200000001"; return rows }},
		{"private-phase", func(rows []Row) []Row { rows[1][8] = pgFreezeAgePrivateCanary; return rows }},
		{"unknown-kind", func(rows []Row) []Row { rows[1][17] = pgFreezeAgePrivateCanary; return rows }},
		{"phantom-counter", func(rows []Row) []Row { rows[1][10] = "1"; return rows }},
	} {
		now := syntheticSettings(nil).Now()
		rows := sample.change(pgFreezeAgeTestRows(now, pgFreezeAgeTestRelation(10, 200000001, 200000000)))
		settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }})
		alerts, err := NewPgFreezeAgeSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: unusable census did not return one visibility finding", sample.name)
		}
		if !pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-unobservable").unobservable {
			t.Fatalf("%s: unknown census could clear prior risk", sample.name)
		}
	}
}

// A failed or stale interval cannot bridge counters; neither can a new server
// generation, changed database, a long observation gap or a vanished pass.
func TestPgFreezeAgeProgressContinuityAndMissingTargets(t *testing.T) {
	for _, mode := range []string{"source-gap", "boot-change", "database-change", "long-gap", "replayed-clock", "pass-gone", "relation-gone"} {
		now := syntheticSettings(nil).Now()
		row := pgFreezeAgeTestPass()
		rows := pgFreezeAgeTestRows(now, row)
		var sourceErr error
		settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, sourceErr }})
		settings.Now = func() time.Time { return now }
		signal := NewPgFreezeAgeSignal()
		if _, err := signal.Run(context.Background(), settings); err != nil {
			t.Fatal("initial risk fixture failed")
		}
		if mode == "source-gap" {
			sourceErr = errors.New(pgFreezeAgePrivateCanary)
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatal("live source failure escaped fixed visibility")
			}
			pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-unobservable")
			sourceErr = nil
		}
		now = now.Add(15 * time.Minute)
		if mode == "long-gap" {
			now = now.Add(2 * time.Minute)
		}
		row[13] = "2"
		if mode == "pass-gone" {
			row = pgFreezeAgeTestRelation(10, 200000001, 200000000)
		}
		if mode == "relation-gone" {
			row = pgFreezeAgeTestRelation(11, 1, 200000000)
		}
		rows = pgFreezeAgeTestRows(now, row)
		switch mode {
		case "boot-change":
			rows[0][3] = fmt.Sprint(syntheticSettings(nil).Now().Add(-3 * time.Hour).UnixMicro())
		case "database-change":
			rows[0][4] = "1235"
		case "replayed-clock":
			now = syntheticSettings(nil).Now()
			rows[0][2] = fmt.Sprint(now.UnixMicro())
		}
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || len(alerts) == 0 {
			t.Fatalf("%s: incomplete continuity falsely recovered age risk", mode)
		}
		if mode == "relation-gone" || mode == "replayed-clock" {
			pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-unobservable")
		} else {
			pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-risk")
		}
	}
}

// Actual sustain and notifier semantics keep an opened risk through unknown
// frames and recover only after complete below-limit observations.
func TestPgFreezeAgeCadenceNotifierRetentionAndRecovery(t *testing.T) {
	now := syntheticSettings(nil).Now()
	row := pgFreezeAgeTestRelation(10, 200000001, 200000000)
	var rows []Row
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }})
	settings.Now = func() time.Time { return now }
	signal := NewPgFreezeAgeSignal()
	gate := newCadenceAlertGate()
	notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
	raised, cleared := 0, 0
	for tick := 0; tick < 6; tick++ {
		rows = pgFreezeAgeTestRows(now, row)
		if tick == 2 || tick == 3 {
			rows = rows[:2]
		}
		if tick >= 4 {
			rows = pgFreezeAgeTestRows(now, pgFreezeAgeTestRelation(10, 200000000, 200000000))
		}
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("notifier source fixture failed")
		}
		ready, held := gate.split(signal, alerts)
		if tick == 0 && (len(ready) != 0 || len(held) != 1) {
			t.Fatal("risk bypassed sustain two")
		}
		for _, transition := range notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held}) {
			if transition.alert.Class != "pg-freeze-age-risk" {
				continue
			}
			if transition.kind == slackRaised {
				raised++
			}
			if transition.kind == slackCleared {
				cleared++
				if tick != 5 {
					t.Fatal("unknown or a single recovery frame cleared risk")
				}
			}
		}
		if tick < 5 && cleared != 0 {
			t.Fatal("age-risk absence acquired recovery authority")
		}
		now = now.Add(15 * time.Minute)
	}
	if raised != 1 || cleared != 1 {
		t.Fatal("actual notifier lost the supported risk/recovery transitions")
	}
}

// Inventory exclusion prevents contact; ended parents retain native causes.
func TestPgFreezeAgeInventoryCancellationAndIndependentVacuum(t *testing.T) {
	for _, mode := range []string{"absent", "ambiguous", "excluded"} {
		settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
			t.Fatal("invalid primary inventory contacted PostgreSQL")
			return nil, nil
		}})
		switch mode {
		case "absent":
			settings.Hosts = nil
		case "ambiguous":
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-2", Roles: []string{"pg-primary"}})
		case "excluded":
			settings.ExcludedHosts = []string{"pg-1"}
		}
		alerts, err := NewPgFreezeAgeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal("invalid inventory escaped visibility")
		}
		unknown := false
		for _, alert := range alerts {
			unknown = unknown || alert.unobservable
		}
		if !unknown {
			t.Fatal("invalid primary inventory certified health")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	sentinel := errors.New(pgFreezeAgePrivateCanary)
	calls := 0
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { calls++; cancel(); return nil, sentinel }})
	alerts, err := NewPgFreezeAgeSignal().Run(ctx, settings)
	if calls != 1 || len(alerts) != 0 || !errors.Is(err, context.Canceled) || !errors.Is(err, sentinel) || strings.Contains(err.Error(), pgFreezeAgePrivateCanary) {
		t.Fatal("ended parent lost lifecycle or private source cause")
	}
	if _, err := NewPgFreezeAgeSignal().Run(ctx, settings); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("ended parent admitted another source call")
	}
	settings = syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if query == pgFreezeAgeQuery {
			return nil, sentinel
		}
		if query != pgVacuumHealthSQL {
			t.Fatal("unexpected sibling query")
		}
		return []Row{{"synthetic_table", "12000000", "01-01 00:00", "0", "", "0", "0", "0", "0", "0", "0", "0", "0", "0", "", "", "0", "", "", "", ""}}, nil
	}})
	alerts, err = NewWithSignals(settings, NewVacuumHealthSignal(), NewPgFreezeAgeSignal()).Run(context.Background())
	if err != nil {
		t.Fatal("optional freeze-age source failure erased the composed vacuum result")
	}
	pgFreezeAgeTestClass(t, alerts, "dead-tuples")
	pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-unobservable")
}

// Recovery requires repeated affirmative observations. A prior healthy sample
// cannot turn disappearance into the notifier's second complete recovery run.
func TestPgFreezeAgeMissingAfterRecoveryReadingKeepsRisk(t *testing.T) {
	now := syntheticSettings(nil).Now()
	var rows []Row
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }})
	settings.Now = func() time.Time { return now }
	signal := NewPgFreezeAgeSignal()
	gate := newCadenceAlertGate()
	notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
	raised, cleared := 0, 0
	for tick := 0; tick < 5; tick++ {
		row := pgFreezeAgeTestRelation(10, 200000001, 200000000)
		if tick == 2 {
			row[2] = "200000000"
		}
		if tick >= 3 {
			row = pgFreezeAgeTestRelation(11, 1, 200000000)
		}
		rows = pgFreezeAgeTestRows(now, row)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("recovery/disappearance fixture failed")
		}
		if tick >= 3 && !pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-unobservable").unobservable {
			t.Fatal("missing former risk acquired recovery authority")
		}
		ready, held := gate.split(signal, alerts)
		for _, transition := range notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held}) {
			if transition.alert.Class == "pg-freeze-age-risk" && transition.kind == slackRaised {
				raised++
			}
			if transition.alert.Class == "pg-freeze-age-risk" && transition.kind == slackCleared {
				cleared++
			}
		}
		now = now.Add(15 * time.Minute)
	}
	if raised != 1 || cleared != 0 {
		t.Fatal("one recovery reading plus disappearance cleared risk")
	}
}

// Relation churn cannot grow history without bound or erase unseen risk.
func TestPgFreezeAgeRiskTrackingBound(t *testing.T) {
	now := syntheticSettings(nil).Now()
	relations := []Row{}
	for id := int64(1); id <= 4096; id++ {
		relations = append(relations, pgFreezeAgeTestRelation(id, 200000001, 200000000))
	}
	rows := pgFreezeAgeTestRows(now, relations...)
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }})
	settings.Now = func() time.Time { return now }
	signal := NewPgFreezeAgeSignal()
	if alerts, err := signal.Run(context.Background(), settings); err != nil || len(alerts) != 4096 {
		t.Fatal("complete cap-sized risk fixture failed")
	}
	now = now.Add(15 * time.Minute)
	rows = pgFreezeAgeTestRows(now, pgFreezeAgeTestRelation(4097, 200000001, 200000000))
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("bounded history fixture failed")
	}
	pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-risk")
	found := false
	for _, alert := range alerts {
		found = found || (alert.unobservable && strings.Contains(alert.Observed, "risk-tracking-overbound"))
	}
	if !found {
		t.Fatal("risk history exceeded its finite cap without visibility")
	}
	now = now.Add(15 * time.Minute)
	for _, relation := range relations {
		relation[2] = "1"
	}
	rows = pgFreezeAgeTestRows(now, relations...)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || !strings.Contains(pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-unobservable").Observed, "risk-tracking-overbound") {
		t.Fatal("untracked risk was erased by a later subset")
	}
}

// The failsafe setting is nonnegative numeric context, never this signal's
// threshold and never proof that failsafe is active.
func TestPgFreezeAgeFailsafeContextOnly(t *testing.T) {
	for _, age := range []int64{200000000, 200000001} {
		for _, configured := range []string{"0", "1000000", "1600000000"} {
			now := syntheticSettings(nil).Now()
			rows := pgFreezeAgeTestRows(now, pgFreezeAgeTestRelation(10, age, 200000000))
			rows[0][7] = configured
			settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }})
			alerts, err := NewPgFreezeAgeSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal("context-only failsafe fixture failed")
			}
			if age == 200000000 {
				if len(alerts) != 0 {
					t.Fatal("failsafe context replaced the freeze-age threshold")
				}
			} else if !strings.Contains(pgFreezeAgeTestClass(t, alerts, "pg-freeze-age-risk").Observed, "configured_failsafe_age="+configured) {
				t.Fatal("nonnegative configured context was lost")
			}
		}
	}
}
