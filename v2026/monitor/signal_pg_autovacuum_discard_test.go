// Synthetic progress, completion and maintenance frames exercise discard
// detection across consecutive runs of one signal instance. No database is
// read.
package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	discardTestDatabase = 16384
	discardTestRelation = 16400
	discardTestHeap     = 514 * 1000 * 1000 * 1000
	discardTestBlocks   = 62700000
)

type discardTestPass struct {
	relation, pid, backendStarted, queryStarted int64
	mode, phase                                 string
	heapTotal, heapScanned, heapVacuumed        int64
	indexCycles, indexesProcessed, indexesTotal int64
	heapBytes, autovacuumCount                  int64
}

type discardTestRelationRow struct {
	relation, heapBytes, count, last int64
}

// One observation as the primary returns it.
type discardTestFrame struct {
	now         time.Time
	started     int64
	passes      []discardTestPass
	relations   []discardTestRelationRow
	maintenance [][2]int64
	mutate      func([]Row) []Row
}

func discardTestRows(frame discardTestFrame) []Row {
	started := frame.started
	if started == 0 {
		// One postmaster generation across a scenario's runs.
		started = syntheticSettings(nil).Now().Add(-30 * 24 * time.Hour).UnixMicro()
	}
	blank := func(count int) []string { return make([]string, count) }
	rows := []Row{append([]string{"meta", "pg-1", strconv.FormatInt(frame.now.UnixMicro(), 10), strconv.FormatInt(started, 10),
		strconv.Itoa(discardTestDatabase), "0", "8192"}, blank(8)...)}
	format := func(values ...int64) []string {
		cells := []string{}
		for _, value := range values {
			cells = append(cells, strconv.FormatInt(value, 10))
		}
		return cells
	}
	for _, pass := range frame.passes {
		row := append([]string{"pass"}, format(pass.relation, pass.pid, pass.backendStarted, pass.queryStarted)...)
		row = append(row, pass.mode, pass.phase)
		row = append(row, format(pass.heapTotal, pass.heapScanned, pass.heapVacuumed, pass.indexCycles, pass.indexesProcessed,
			pass.indexesTotal, pass.heapBytes, pass.autovacuumCount)...)
		rows = append(rows, row)
	}
	for _, relation := range frame.relations {
		rows = append(rows, append(append([]string{"relation"}, format(relation.relation, relation.heapBytes, relation.count, relation.last)...), blank(10)...))
	}
	for _, window := range frame.maintenance {
		rows = append(rows, append(append([]string{"maintenance"}, format(window[0], window[1])...), blank(12)...))
	}
	rows = append(rows, append([]string{"end", strconv.Itoa(len(frame.passes)), strconv.Itoa(len(frame.relations)), strconv.Itoa(len(frame.maintenance))}, blank(11)...))
	if frame.mutate != nil {
		rows = frame.mutate(rows)
	}
	return rows
}

// The 10-13 transfer_contract pass at the given scanned blocks.
func discardTestScanning(now time.Time, scanned int64) discardTestPass {
	return discardTestPass{
		relation: discardTestRelation, pid: 4100, backendStarted: now.Add(-3 * time.Hour).UnixMicro(),
		queryStarted: now.Add(-2 * time.Hour).UnixMicro(), mode: "ordinary", phase: "scanning heap",
		heapTotal: discardTestBlocks, heapScanned: scanned, indexCycles: 0, indexesProcessed: 0, indexesTotal: 21,
		heapBytes: discardTestHeap, autovacuumCount: 100,
	}
}

func discardTestRelationAt(count int64, last time.Time) discardTestRelationRow {
	return discardTestRelationRow{relation: discardTestRelation, heapBytes: discardTestHeap, count: count, last: last.UnixMicro()}
}

// Run one frame through the instance; queries are checked for their guards.
func discardTestRun(t *testing.T, signal Signal, frame discardTestFrame) Alerts {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{"hostname -s", "pg_is_in_recovery()", "pg_stat_progress_vacuum", "autovacuum_count", "finished_task", "pending_task",
			"work.DbMaintenance", "pg_relation_size(c.oid) >= 100000000000"} {
			if !strings.Contains(query, want) {
				t.Fatalf("discard query lost %q", want)
			}
		}
		return discardTestRows(frame), nil
	}})
	at := frame.now
	settings.Now = func() time.Time { return at }
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("discard run failed")
	}
	return alerts
}

func TestPgAutovacuumDiscardRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "pg-autovacuum-discard" {
			continue
		}
		found++
		if signal.Number() != "2.4a" || signal.Cadence() != 5*time.Minute || signal.ID() != "pg/autovacuum-discard" {
			t.Fatal("discard reader lost its catalog identity or five-minute cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4a discarded-pass reader is not registered")
	}
}

// The 2026-10-13 shape: daily maintenance discarded transfer_contract's pass
// at heap block 51.2M of 62.7M. The discard stays open while the
// replacement runs and recovers when it completes.
func TestPgAutovacuumDiscardSyntheticIncident(t *testing.T) {
	signal := NewPgAutovacuumDiscardSignal()
	start := syntheticSettings(nil).Now()
	lastCompleted := start.Add(-3 * 24 * time.Hour)
	run := func(offset time.Duration, passes []discardTestPass, count int64, last time.Time, maintenance [][2]int64) Alerts {
		return discardTestRun(t, signal, discardTestFrame{now: start.Add(offset), passes: passes,
			relations: []discardTestRelationRow{discardTestRelationAt(count, last)}, maintenance: maintenance})
	}
	if alerts := run(0, []discardTestPass{discardTestScanning(start, 40000000)}, 100, lastCompleted, nil); len(alerts) != 0 {
		t.Fatal("a progressing pass alerted")
	}
	if alerts := run(5*time.Minute, []discardTestPass{discardTestScanning(start, 51200000)}, 100, lastCompleted, nil); len(alerts) != 0 {
		t.Fatal("a progressing pass alerted")
	}
	replacement := discardTestScanning(start, 100000)
	replacement.pid, replacement.queryStarted = 4200, start.Add(9*time.Minute).UnixMicro()
	maintenance := [][2]int64{{start.Add(6 * time.Minute).UnixMicro(), 0}}
	alerts := run(10*time.Minute, []discardTestPass{replacement}, 100, lastCompleted, maintenance)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want the discarded pass", len(alerts))
	}
	alert := requireAlertClass(t, alerts, "pg-autovacuum-discard")
	if alert.Target != "pg-1/database-16384/relation-16400" || alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.unobservable {
		t.Fatalf("wrong discard identity: %+v", alert)
	}
	for _, want := range []string{`last_phase="scanning heap"`, "heap_scanned_pct_at_least=81.7", "lost_heap_gb_at_least=419.4",
		"maintenance_overlap=db-maintenance", "discard_window_s=300", "discards=1", `replacement_pass=ordinary phase="scanning heap" heap_scanned=100000`} {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("discard lost %q: %s", want, alert.Observed)
		}
	}
	if alerts := run(15*time.Minute, []discardTestPass{replacement}, 100, lastCompleted, maintenance); len(alerts) != 1 || alerts[0].Class != "pg-autovacuum-discard" {
		t.Fatal("the open discard cleared before a later pass completed")
	}
	if alerts := run(20*time.Minute, nil, 101, start.Add(19*time.Minute), nil); len(alerts) != 0 {
		t.Fatal("a completed replacement did not recover the discard")
	}
}

// Completed, small, early, manual and dropped-relation passes lost nothing
// worth an alert; an index-phase discard on a big relation does.
func TestPgAutovacuumDiscardHarmPolicy(t *testing.T) {
	start := syntheticSettings(nil).Now()
	lastCompleted := start.Add(-3 * 24 * time.Hour)
	for _, test := range []struct {
		name    string
		pass    discardTestPass
		count   int64
		last    time.Time
		dropped bool
		harmful bool
	}{
		{name: "completed by counter", pass: discardTestScanning(start, 60000000), count: 101, last: lastCompleted},
		{name: "completed by time", pass: discardTestScanning(start, 60000000), count: 0, last: start.Add(time.Minute)},
		{name: "small relation", pass: func() discardTestPass {
			p := discardTestScanning(start, 60000000)
			p.heapBytes = 10 * 1000 * 1000 * 1000
			return p
		}(), count: 100, last: lastCompleted},
		{name: "early heap", pass: discardTestScanning(start, 20000000), count: 100, last: lastCompleted},
		{name: "manual vacuum", pass: func() discardTestPass { p := discardTestScanning(start, 60000000); p.mode = "manual"; return p }(), count: 100, last: lastCompleted},
		{name: "dropped relation", pass: discardTestScanning(start, 60000000), count: 100, last: lastCompleted, dropped: true},
		{name: "index phase", pass: func() discardTestPass {
			p := discardTestScanning(start, discardTestBlocks)
			p.phase, p.indexesProcessed = "vacuuming indexes", 4
			return p
		}(), count: 100, last: lastCompleted, harmful: true},
		{name: "late heap", pass: discardTestScanning(start, 40000000), count: 100, last: lastCompleted, harmful: true},
	} {
		signal := NewPgAutovacuumDiscardSignal()
		relations := []discardTestRelationRow{discardTestRelationAt(100, lastCompleted)}
		discardTestRun(t, signal, discardTestFrame{now: start, passes: []discardTestPass{test.pass}, relations: relations})
		relations = []discardTestRelationRow{discardTestRelationAt(test.count, test.last)}
		if test.dropped {
			relations = nil
		}
		alerts := discardTestRun(t, signal, discardTestFrame{now: start.Add(5 * time.Minute), relations: relations})
		if test.harmful != (len(alerts) == 1 && alerts[0].Class == "pg-autovacuum-discard") || (!test.harmful && len(alerts) != 0) {
			t.Fatalf("%s: harmful=%t but got %d alerts", test.name, test.harmful, len(alerts))
		}
		if test.harmful && !strings.Contains(alerts[0].Observed, "maintenance_overlap=none-observed") {
			t.Fatalf("%s: maintenance overlap was invented: %s", test.name, alerts[0].Observed)
		}
	}
}

// A discard window that reaches back past the maintenance lookback leaves the
// maintenance attribution unknown; a finished overlapping run is named.
func TestPgAutovacuumDiscardMaintenanceOverlap(t *testing.T) {
	start := syntheticSettings(nil).Now()
	lastCompleted := start.Add(-3 * 24 * time.Hour)
	for _, test := range []struct {
		gap         time.Duration
		maintenance [][2]int64
		want        string
	}{
		{gap: 5 * time.Minute, maintenance: [][2]int64{{start.Add(-time.Hour).UnixMicro(), start.Add(2 * time.Minute).UnixMicro()}}, want: "maintenance_overlap=db-maintenance"},
		{gap: 5 * time.Minute, maintenance: [][2]int64{{start.Add(-time.Hour).UnixMicro(), start.Add(-time.Minute).UnixMicro()}}, want: "maintenance_overlap=none-observed"},
		{gap: 2 * time.Hour, want: "maintenance_overlap=unknown-window-older-than-lookback"},
	} {
		signal := NewPgAutovacuumDiscardSignal()
		discardTestRun(t, signal, discardTestFrame{now: start, passes: []discardTestPass{discardTestScanning(start, 50000000)},
			relations: []discardTestRelationRow{discardTestRelationAt(100, lastCompleted)}})
		alerts := discardTestRun(t, signal, discardTestFrame{now: start.Add(test.gap), maintenance: test.maintenance,
			relations: []discardTestRelationRow{discardTestRelationAt(100, lastCompleted)}})
		if alert := requireAlertClass(t, alerts, "pg-autovacuum-discard"); !strings.Contains(alert.Observed, test.want) {
			t.Fatalf("gap %s: %q not in %s", test.gap, test.want, alert.Observed)
		}
	}
}

// The second run's query carries the tracked relation so its completion
// evidence is read even below the size line.
func TestPgAutovacuumDiscardQueryCarriesTrackedRelations(t *testing.T) {
	start := syntheticSettings(nil).Now()
	signal := NewPgAutovacuumDiscardSignal()
	discardTestRun(t, signal, discardTestFrame{now: start, passes: []discardTestPass{discardTestScanning(start, 50000000)},
		relations: []discardTestRelationRow{discardTestRelationAt(100, start.Add(-time.Hour))}})
	queries := []string{}
	settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		queries = append(queries, query)
		return discardTestRows(discardTestFrame{now: start.Add(5 * time.Minute), relations: []discardTestRelationRow{discardTestRelationAt(101, start.Add(4*time.Minute))}}), nil
	}})
	at := start.Add(5 * time.Minute)
	settings.Now = func() time.Time { return at }
	if _, err := signal.Run(context.Background(), settings); err != nil || len(queries) != 1 || !strings.Contains(queries[0], "ANY(ARRAY[16400]::oid[])") {
		t.Fatal("the tracked relation was not carried into the completion census")
	}
}

// Malformed frames and failed reads are unknown; a postmaster restart with
// tracked passes is a one-run unknown that cannot invent a discard.
func TestPgAutovacuumDiscardUnknownSources(t *testing.T) {
	start := syntheticSettings(nil).Now()
	for _, test := range []struct {
		name   string
		mutate func([]Row) []Row
		reason string
	}{
		{name: "host", mutate: func(rows []Row) []Row { rows[0][1] = "pg-2"; return rows }, reason: "source-identity-or-role-unavailable"},
		{name: "replica", mutate: func(rows []Row) []Row { rows[0][5] = "1"; return rows }, reason: "source-identity-or-role-unavailable"},
		{name: "clock", mutate: func(rows []Row) []Row {
			rows[0][2] = strconv.FormatInt(start.Add(time.Hour).UnixMicro(), 10)
			return rows
		}, reason: "source-clock-unavailable"},
		{name: "width", mutate: func(rows []Row) []Row { rows[1] = rows[1][:14]; return rows }, reason: "invalid-frame-width"},
		{name: "duplicate pass", mutate: func(rows []Row) []Row {
			rows = append(rows[:2], rows[1:]...)
			rows[len(rows)-1][1] = "2"
			return rows
		}, reason: "duplicate-pass"},
		{name: "counters", mutate: func(rows []Row) []Row { rows[1][8] = strconv.Itoa(discardTestBlocks + 1); return rows }, reason: "inconsistent-pass-counters"},
		{name: "phase", mutate: func(rows []Row) []Row { rows[1][6] = "private-source-canary"; return rows }, reason: "invalid-pass-phase"},
		{name: "end", mutate: func(rows []Row) []Row { return rows[:len(rows)-1] }, reason: "incomplete-frame"},
		{name: "count", mutate: func(rows []Row) []Row { rows[len(rows)-1][2] = "9"; return rows }, reason: "incomplete-frame"},
		{name: "kind", mutate: func(rows []Row) []Row {
			return append(rows, append(Row{"private-source-canary"}, make([]string, 14)...))
		}, reason: "unexpected-frame-kind"},
	} {
		alerts := discardTestRun(t, NewPgAutovacuumDiscardSignal(), discardTestFrame{now: start, mutate: test.mutate,
			passes: []discardTestPass{discardTestScanning(start, 1)}, relations: []discardTestRelationRow{discardTestRelationAt(100, start.Add(-time.Hour))}})
		if len(alerts) != 1 {
			t.Fatalf("%s: got %d alerts", test.name, len(alerts))
		}
		alert := requireAlertClass(t, alerts, "pg-autovacuum-discard-unobservable")
		if !alert.unobservable || alert.Sustain != 2 || !strings.Contains(alert.Observed, "reason="+test.reason) {
			t.Fatalf("%s: wrong unknown: %s", test.name, alert.Observed)
		}
		requireAlertOmits(t, alert, "private-source-canary")
	}

	signal := NewPgAutovacuumDiscardSignal()
	discardTestRun(t, signal, discardTestFrame{now: start, passes: []discardTestPass{discardTestScanning(start, 50000000)},
		relations: []discardTestRelationRow{discardTestRelationAt(100, start.Add(-time.Hour))}})
	restarted := discardTestFrame{now: start.Add(5 * time.Minute), started: start.Add(4 * time.Minute).UnixMicro(),
		relations: []discardTestRelationRow{discardTestRelationAt(0, time.UnixMicro(0))}}
	alerts := discardTestRun(t, signal, restarted)
	if len(alerts) != 1 || alerts[0].Class != "pg-autovacuum-discard-unobservable" || alerts[0].Sustain != 1 ||
		!strings.Contains(alerts[0].Observed, "source-generation-changed unclassified_passes=1") {
		t.Fatal("a postmaster restart invented a discard or hid its unclassified passes")
	}
	restarted.now = start.Add(10 * time.Minute)
	if alerts := discardTestRun(t, signal, restarted); len(alerts) != 0 {
		t.Fatal("the restart remained unknown after the generation was re-established")
	}

	failed := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
		return nil, errors.New("exit status 2: private-source-canary")
	}})
	alerts, err := NewPgAutovacuumDiscardSignal().Run(context.Background(), failed)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "pg-autovacuum-discard-unobservable" || !strings.Contains(alerts[0].Observed, "reason=source-unavailable") {
		t.Fatal("a failed read certified that no pass was lost")
	}
	requireAlertOmits(t, alerts[0], "private-source-canary")

	settings := DefaultPgAutovacuumDiscardSettings()
	settings.MaxCensus = 0
	alerts = discardTestRun(t, NewPgAutovacuumDiscardSignalWithSettings(settings), discardTestFrame{now: start,
		relations: []discardTestRelationRow{discardTestRelationAt(100, start.Add(-time.Hour))}})
	if len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "reason=census-overbound") {
		t.Fatal("an over-cap census was judged")
	}
}

// Missing or ambiguous primaries never read a database; cancellation keeps
// lifecycle with the original cause.
func TestPgAutovacuumDiscardInventoryAndCancellation(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
		t.Fatal("discard read without one primary")
		return nil, nil
	}})
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-2", Roles: []string{"pg-primary"}})
	alerts, err := NewPgAutovacuumDiscardSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "primary-inventory-unavailable-or-ambiguous") {
		t.Fatal("an ambiguous primary did not stay unknown")
	}
	for _, beforeContact := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cause := errors.New("private-source-canary")
		settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
			if beforeContact {
				t.Fatal("canceled work read the database")
			}
			cancel()
			return nil, cause
		}})
		if beforeContact {
			cancel()
		}
		alerts, err := NewPgAutovacuumDiscardSignal().Run(ctx, settings)
		cancel()
		if len(alerts) != 0 || !errors.Is(err, context.Canceled) || (!beforeContact && !errors.Is(err, cause)) {
			t.Fatal("canceled discard read lost its causes or published findings")
		}
		if strings.Contains(err.Error(), "private-source-canary") {
			t.Fatal("lifecycle error exposed its private source")
		}
	}
}
