// Synthetic presence and measurement frames exercise the reclaimable bloat
// signal (SIGNALS.md §2.4k) through the public signal. No database is read.
package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const tableBloatTestGigabyte = int64(1000 * 1000 * 1000)

type tableBloatTestRelation struct {
	table         string
	kind          string
	schema        string
	name          string
	method        string
	measuredAgo   time.Duration
	measuredBytes int64
	reclaimable   int64
	currentBytes  int64
	oid           int64
}

type tableBloatTestTable struct {
	name string
	oid  int64
	live int64
	dead int64
}

// The two frames as the primary returns them.
type tableBloatTestSource struct {
	now                time.Time
	present            bool
	newestAgo          time.Duration
	noMeasurement      bool
	windowRelations    int64
	relations          []tableBloatTestRelation
	tables             []tableBloatTestTable
	presenceErr        error
	measurementErr     error
	mutatePresence     func([]Row) []Row
	mutateMeasurements func([]Row) []Row
	// the reads, in order
	reads []string
}

// A healthy primary: a measurement two days old and no relation over the line.
func tableBloatTestHealthy() *tableBloatTestSource {
	return &tableBloatTestSource{
		now:             syntheticSettings(nil).Now(),
		present:         true,
		newestAgo:       2 * 24 * time.Hour,
		windowRelations: 900,
	}
}

func tableBloatTestMicros(at time.Time) string { return strconv.FormatInt(at.UnixMicro(), 10) }

func (self *tableBloatTestSource) presenceRows() []Row {
	present := "0"
	if self.present {
		present = "1"
	}
	rows := []Row{{"meta", "pg-1", tableBloatTestMicros(self.now), "0", present}}
	if self.mutatePresence != nil {
		rows = self.mutatePresence(rows)
	}
	return rows
}

func (self *tableBloatTestSource) measurementRows() []Row {
	newest := tableBloatTestMicros(self.now.Add(-self.newestAgo))
	if self.noMeasurement {
		newest = "none"
	}
	rows := []Row{{"meta", "pg-1", tableBloatTestMicros(self.now), "0", newest,
		strconv.FormatInt(self.windowRelations, 10), strconv.Itoa(len(self.relations)), "", "", "", ""}}
	for _, relation := range self.relations {
		rows = append(rows, Row{"relation", relation.table, relation.kind, relation.schema, relation.name, relation.method,
			tableBloatTestMicros(self.now.Add(-relation.measuredAgo)), strconv.FormatInt(relation.measuredBytes, 10),
			strconv.FormatInt(relation.reclaimable, 10), strconv.FormatInt(relation.currentBytes, 10), strconv.FormatInt(relation.oid, 10)})
	}
	for _, table := range self.tables {
		rows = append(rows, Row{"table", table.name, strconv.FormatInt(table.oid, 10), strconv.FormatInt(table.live, 10),
			strconv.FormatInt(table.dead, 10), "", "", "", "", "", ""})
	}
	rows = append(rows, Row{"end", strconv.Itoa(len(self.relations)), strconv.Itoa(len(self.tables)), "", "", "", "", "", "", "", ""})
	if self.mutateMeasurements != nil {
		rows = self.mutateMeasurements(rows)
	}
	return rows
}

func tableBloatTestSettings(t *testing.T, source *tableBloatTestSource) SignalSettings {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if !strings.HasPrefix(query, pgPrimaryHostnameMetaCommand+"\n") || strings.Count(query, ":'monitor_primary_hostname'") != 1 {
			t.Fatal("a bloat read lost its primary guard")
		}
		if strings.Contains(query, "to_regclass('public.db_bloat_measurement') IS NULL") {
			source.reads = append(source.reads, "presence")
			return source.presenceRows(), source.presenceErr
		}
		for _, want := range []string{
			"pg_is_in_recovery()",
			"DISTINCT ON (m.table_name, m.relation_schema, m.relation_name)",
			"3024000 * interval '1 second'",
			"newest.reclaimable_bytes >= 5000000000",
			"newest.reclaimable_bytes >= 0.5 * newest.relation_bytes",
			"LIMIT 65",
			"pg_relation_size(candidate.current_relation)",
			"pg_stat_user_tables",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("the measurement read lost %q", want)
			}
		}
		source.reads = append(source.reads, "measurements")
		return source.measurementRows(), source.measurementErr
	}})
	at := source.now
	settings.Now = func() time.Time { return at }
	return settings
}

func tableBloatTestRun(t *testing.T, signal Signal, source *tableBloatTestSource) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), tableBloatTestSettings(t, source))
	if err != nil {
		t.Fatal("table bloat run failed")
	}
	return alerts
}

func TestTableBloatRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "table-bloat" {
			continue
		}
		found++
		if signal.Number() != "2.4k" || signal.Cadence() != time.Hour || signal.ID() != "pg/table-bloat" {
			t.Fatal("table bloat lost its catalog identity or hourly cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4k table bloat signal is not registered")
	}
}

func TestTableBloatHealthy(t *testing.T) {
	source := tableBloatTestHealthy()
	if alerts := tableBloatTestRun(t, NewTableBloatSignal(), source); len(alerts) != 0 {
		t.Fatalf("healthy bloat alerted: %d alerts, first %s %s", len(alerts), alerts[0].Class, alerts[0].Observed)
	}
	if strings.Join(source.reads, ",") != "presence,measurements" {
		t.Fatalf("reads = %v", source.reads)
	}
}

// The 2026-10-21 queue: a few hundred live rows and 3M dead ones, under the
// §2.4 line, while its two poll indexes hold 94 GB. Only this signal sees it.
func TestTableBloatCatchesTheQueueUnderTheDeadTupleLine(t *testing.T) {
	const live, dead = int64(300), int64(3000000)
	if vacuumDeadTupleAlertThreshold(0) <= dead {
		t.Fatal("the fixture's dead rows are not under the §2.4 line")
	}
	source := tableBloatTestHealthy()
	source.relations = []tableBloatTestRelation{
		{table: "pending_task", kind: "index", schema: "public", name: "pending_task_available_block_priority", method: "btree_estimate",
			measuredAgo: 3 * 24 * time.Hour, measuredBytes: 47 * tableBloatTestGigabyte, reclaimable: 47*tableBloatTestGigabyte - 8*1024*1024, currentBytes: 47 * tableBloatTestGigabyte, oid: 16401},
		{table: "pending_task", kind: "index", schema: "public", name: "pending_task_available_block_function", method: "btree_estimate",
			measuredAgo: 3 * 24 * time.Hour, measuredBytes: 47 * tableBloatTestGigabyte, reclaimable: 47*tableBloatTestGigabyte - 8*1024*1024, currentBytes: 47 * tableBloatTestGigabyte, oid: 16402},
	}
	source.tables = []tableBloatTestTable{{name: "pending_task", oid: 16400, live: live, dead: dead}}
	alerts := tableBloatTestRun(t, NewTableBloatSignal(), source)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want the queue's bloat", len(alerts))
	}
	alert := requireAlertClass(t, alerts, "table-bloat")
	if alert.Target != "pg-1/pending_task" || alert.Severity != SeverityWarn || alert.Sustain != 1 {
		t.Fatalf("wrong queue alert identity: %s %s %d", alert.Target, alert.Severity, alert.Sustain)
	}
	for _, want := range []string{
		"reclaimable_gb=94.0 relations=2 kinds=index live_rows=300 dead_rows=3000000 dead_tuple_line=10000000",
		"pending_task_available_block_function index btree_estimate size_gb=47.0 reclaimable_gb=47.0 share_pct=100.0 measured_days_ago=3.0",
		"pending_task_available_block_priority index btree_estimate",
	} {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("queue alert lost %q: %s", want, alert.Observed)
		}
	}
	if !strings.Contains(alert.Symptom, "its indexes hold it") {
		t.Fatalf("queue symptom = %s", alert.Symptom)
	}
}

// The line is a share of each relation's size with a floor in bytes: a huge
// table with a small share and a small table with a large one stay quiet,
// and a churn table whose relation is two-thirds reclaimable alerts.
func TestTableBloatScalesWithEachRelationsSize(t *testing.T) {
	source := tableBloatTestHealthy()
	source.relations = []tableBloatTestRelation{
		// 4%: the read's share line would not list it; judged anyway
		{table: "synthetic_history", kind: "heap", schema: "public", name: "synthetic_history", method: "statistics",
			measuredAgo: 24 * time.Hour, measuredBytes: 500 * tableBloatTestGigabyte, reclaimable: 20 * tableBloatTestGigabyte, currentBytes: 500 * tableBloatTestGigabyte, oid: 16500},
		// 97% of 4 GB: under the byte floor
		{table: "synthetic_small_queue", kind: "heap", schema: "public", name: "synthetic_small_queue", method: "pgstattuple_approx",
			measuredAgo: 24 * time.Hour, measuredBytes: 4 * tableBloatTestGigabyte, reclaimable: 3900 * 1000 * 1000, currentBytes: 4 * tableBloatTestGigabyte, oid: 16600},
		{table: "synthetic_churn", kind: "heap", schema: "public", name: "synthetic_churn", method: "pgstattuple_approx",
			measuredAgo: 24 * time.Hour, measuredBytes: 12 * tableBloatTestGigabyte, reclaimable: 8 * tableBloatTestGigabyte, currentBytes: 12 * tableBloatTestGigabyte, oid: 16700},
	}
	source.tables = []tableBloatTestTable{
		{name: "synthetic_history", oid: 16500, live: 2000000000, dead: 1000},
		{name: "synthetic_small_queue", oid: 16600, live: 100, dead: 2000000},
		{name: "synthetic_churn", oid: 16700, live: 4000000, dead: 9000000},
	}
	alerts := tableBloatTestRun(t, NewTableBloatSignal(), source)
	if len(alerts) != 1 || alerts[0].Class != "table-bloat" || alerts[0].Target != "pg-1/synthetic_churn" ||
		!strings.Contains(alerts[0].Observed, "share_pct=66.7") || !strings.Contains(alerts[0].Observed, "kinds=heap") {
		t.Fatalf("size scaling = %+v", alerts)
	}
}

// What a relation shrank by since its measurement was returned: a rebuilt
// index clears, a partial shrink keeps the rest, and growth adds nothing.
func TestTableBloatCreditsAShrinkSinceTheMeasurement(t *testing.T) {
	source := tableBloatTestHealthy()
	source.relations = []tableBloatTestRelation{
		// rebuilt since: 94 GB measured, 0.2 GB now
		{table: "synthetic_rebuilt", kind: "index", schema: "public", name: "synthetic_rebuilt_poll", method: "btree_estimate",
			measuredAgo: 2 * 24 * time.Hour, measuredBytes: 94 * tableBloatTestGigabyte, reclaimable: 93 * tableBloatTestGigabyte, currentBytes: 200 * 1000 * 1000, oid: 16800},
		// 20 GB of the measured 40 returned
		{table: "synthetic_partly", kind: "heap", schema: "public", name: "synthetic_partly", method: "pgstattuple_approx",
			measuredAgo: 2 * 24 * time.Hour, measuredBytes: 50 * tableBloatTestGigabyte, reclaimable: 40 * tableBloatTestGigabyte, currentBytes: 30 * tableBloatTestGigabyte, oid: 16900},
		// grown since: the measured 6 GB is 43% of 14
		{table: "synthetic_grown", kind: "heap", schema: "public", name: "synthetic_grown", method: "statistics",
			measuredAgo: 2 * 24 * time.Hour, measuredBytes: 10 * tableBloatTestGigabyte, reclaimable: 6 * tableBloatTestGigabyte, currentBytes: 14 * tableBloatTestGigabyte, oid: 17000},
	}
	source.tables = []tableBloatTestTable{
		{name: "synthetic_rebuilt", oid: 16799, live: 300, dead: 0},
		{name: "synthetic_partly", oid: 16900, live: 100, dead: 100},
		{name: "synthetic_grown", oid: 17000, live: 100, dead: 100},
	}
	alerts := tableBloatTestRun(t, NewTableBloatSignal(), source)
	if len(alerts) != 1 || alerts[0].Target != "pg-1/synthetic_partly" ||
		!strings.Contains(alerts[0].Observed, "reclaimable_gb=20.0") || !strings.Contains(alerts[0].Observed, "share_pct=66.7") {
		t.Fatalf("shrink credit = %+v", alerts)
	}
}

// Both lines hold at their edges, and below either the relation is quiet.
func TestTableBloatLineEdges(t *testing.T) {
	for _, c := range []struct {
		name        string
		reclaimable int64
		current     int64
		alerts      int
	}{
		{name: "both lines", reclaimable: 5 * tableBloatTestGigabyte, current: 10 * tableBloatTestGigabyte, alerts: 1},
		{name: "under the bytes", reclaimable: 5*tableBloatTestGigabyte - 1, current: 10*tableBloatTestGigabyte - 2, alerts: 0},
		{name: "under the share", reclaimable: 5 * tableBloatTestGigabyte, current: 10*tableBloatTestGigabyte + 1, alerts: 0},
	} {
		source := tableBloatTestHealthy()
		source.relations = []tableBloatTestRelation{{table: "synthetic_edge", kind: "heap", schema: "public", name: "synthetic_edge", method: "statistics",
			measuredAgo: time.Hour, measuredBytes: c.current, reclaimable: c.reclaimable, currentBytes: c.current, oid: 17100}}
		source.tables = []tableBloatTestTable{{name: "synthetic_edge", oid: 17100, live: 1, dead: 1}}
		if alerts := tableBloatTestRun(t, NewTableBloatSignal(), source); len(alerts) != c.alerts {
			t.Errorf("%s: %d alerts, want %d", c.name, len(alerts), c.alerts)
		}
	}
}

// One finding per table, listing its heap, TOAST relation and indexes by
// reclaimable bytes; tables in name order.
func TestTableBloatGroupsRelationsByTable(t *testing.T) {
	source := tableBloatTestHealthy()
	source.relations = []tableBloatTestRelation{
		{table: "synthetic_b", kind: "heap", schema: "public", name: "synthetic_b", method: "pgstattuple_approx",
			measuredAgo: time.Hour, measuredBytes: 20 * tableBloatTestGigabyte, reclaimable: 15 * tableBloatTestGigabyte, currentBytes: 20 * tableBloatTestGigabyte, oid: 17200},
		{table: "synthetic_b", kind: "toast", schema: "pg_toast", name: "pg_toast_17200", method: "statistics",
			measuredAgo: time.Hour, measuredBytes: 30 * tableBloatTestGigabyte, reclaimable: 25 * tableBloatTestGigabyte, currentBytes: 30 * tableBloatTestGigabyte, oid: 17201},
		{table: "synthetic_b", kind: "index", schema: "public", name: "synthetic_b_pkey", method: "btree_estimate",
			measuredAgo: time.Hour, measuredBytes: 10 * tableBloatTestGigabyte, reclaimable: 6 * tableBloatTestGigabyte, currentBytes: 10 * tableBloatTestGigabyte, oid: 17202},
		{table: "synthetic_a", kind: "index", schema: "public", name: "synthetic_a_time", method: "btree_estimate",
			measuredAgo: time.Hour, measuredBytes: 8 * tableBloatTestGigabyte, reclaimable: 7 * tableBloatTestGigabyte, currentBytes: 8 * tableBloatTestGigabyte, oid: 17301},
	}
	source.tables = []tableBloatTestTable{{name: "synthetic_b", oid: 17200, live: 10, dead: 10}, {name: "synthetic_a", oid: 17300, live: 10, dead: 10}}
	alerts := tableBloatTestRun(t, NewTableBloatSignal(), source)
	if len(alerts) != 2 || alerts[0].Target != "pg-1/synthetic_a" || alerts[1].Target != "pg-1/synthetic_b" {
		t.Fatalf("grouped alerts = %+v", alerts)
	}
	observed := alerts[1].Observed
	if !strings.Contains(observed, "reclaimable_gb=46.0 relations=3 kinds=heap,index,toast") ||
		!(strings.Index(observed, "pg_toast_17200 toast") < strings.Index(observed, "synthetic_b heap") &&
			strings.Index(observed, "synthetic_b heap") < strings.Index(observed, "synthetic_b_pkey index")) {
		t.Fatalf("grouped table = %s", observed)
	}
	if strings.Contains(alerts[1].Symptom, "its indexes hold it") {
		t.Fatalf("a heap's bloat was blamed on its indexes: %s", alerts[1].Symptom)
	}
}

// Only plain identifiers leave the frame; others are named by their oid.
func TestTableBloatWithholdsUnplainNames(t *testing.T) {
	source := tableBloatTestHealthy()
	source.relations = []tableBloatTestRelation{{table: "Synthetic Private", kind: "index", schema: "public", name: "private-index-canary", method: "btree_estimate",
		measuredAgo: time.Hour, measuredBytes: 9 * tableBloatTestGigabyte, reclaimable: 8 * tableBloatTestGigabyte, currentBytes: 9 * tableBloatTestGigabyte, oid: 17401}}
	source.tables = []tableBloatTestTable{{name: "Synthetic Private", oid: 17400, live: 1, dead: 1}}
	alerts := tableBloatTestRun(t, NewTableBloatSignal(), source)
	if len(alerts) != 1 || alerts[0].Target != "pg-1/relation-17400" || !strings.Contains(alerts[0].Observed, "relation-17401 index") {
		t.Fatalf("withheld names = %+v", alerts)
	}
	requireAlertOmits(t, alerts[0], "private-index-canary", "Synthetic Private")
}

// A list cut at its bound is judged on what it holds and says so.
func TestTableBloatNotesACutList(t *testing.T) {
	settings := DefaultTableBloatSettings()
	settings.RelationLimit = 1
	source := tableBloatTestHealthy()
	for i, name := range []string{"synthetic_c", "synthetic_d"} {
		source.relations = append(source.relations, tableBloatTestRelation{table: name, kind: "heap", schema: "public", name: name, method: "statistics",
			measuredAgo: time.Hour, measuredBytes: 10 * tableBloatTestGigabyte, reclaimable: int64(9-i) * tableBloatTestGigabyte, currentBytes: 10 * tableBloatTestGigabyte, oid: int64(17500 + i)})
		source.tables = append(source.tables, tableBloatTestTable{name: name, oid: int64(17500 + i), live: 1, dead: 1})
	}
	alerts, err := NewTableBloatSignalWithSettings(settings).Run(context.Background(), tableBloatTestSettingsWithLimit(t, source, settings.RelationLimit))
	if err != nil || len(alerts) != 1 || alerts[0].Target != "pg-1/synthetic_c" || !strings.Contains(alerts[0].Evidence, "relation_list=truncated") {
		t.Fatalf("cut list = %+v, %v", alerts, err)
	}
}

// The measurement read of a cut list bounds its LIMIT one past the setting.
func tableBloatTestSettingsWithLimit(t *testing.T, source *tableBloatTestSource, limit int) SignalSettings {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "to_regclass('public.db_bloat_measurement') IS NULL") {
			return source.presenceRows(), nil
		}
		if !strings.Contains(query, "LIMIT "+strconv.Itoa(limit+1)) {
			t.Fatal("the measurement read lost its list bound")
		}
		return source.measurementRows(), nil
	}})
	at := source.now
	settings.Now = func() time.Time { return at }
	return settings
}

// Every gap in the source is one fixed reason for the measurement target. A
// stale measurement is still judged.
func TestTableBloatGapsStayUnknown(t *testing.T) {
	bloated := func(source *tableBloatTestSource) {
		source.relations = []tableBloatTestRelation{{table: "synthetic_stale", kind: "heap", schema: "public", name: "synthetic_stale", method: "statistics",
			measuredAgo: 10 * 24 * time.Hour, measuredBytes: 10 * tableBloatTestGigabyte, reclaimable: 9 * tableBloatTestGigabyte, currentBytes: 10 * tableBloatTestGigabyte, oid: 17600}}
		source.tables = []tableBloatTestTable{{name: "synthetic_stale", oid: 17600, live: 1, dead: 1}}
	}
	for _, c := range []struct {
		name   string
		mutate func(*tableBloatTestSource)
		reason string
		bloat  bool
		reads  string
	}{
		{name: "presence error", mutate: func(s *tableBloatTestSource) { s.presenceErr = errors.New("exit status 3: private-source-canary") }, reason: "presence-source-unavailable", reads: "presence"},
		{name: "missing table", mutate: func(s *tableBloatTestSource) { s.present = false }, reason: "measurement-table-missing", reads: "presence"},
		{name: "presence host", mutate: func(s *tableBloatTestSource) {
			s.mutatePresence = func(rows []Row) []Row { rows[0][1] = "pg-2"; return rows }
		}, reason: "source-identity-or-role-unavailable", reads: "presence"},
		{name: "presence replica", mutate: func(s *tableBloatTestSource) {
			s.mutatePresence = func(rows []Row) []Row { rows[0][3] = "1"; return rows }
		}, reason: "source-identity-or-role-unavailable", reads: "presence"},
		{name: "presence width", mutate: func(s *tableBloatTestSource) {
			s.mutatePresence = func(rows []Row) []Row { rows[0] = rows[0][:4]; return rows }
		}, reason: "incomplete-frame", reads: "presence"},
		{name: "presence value", mutate: func(s *tableBloatTestSource) {
			s.mutatePresence = func(rows []Row) []Row { rows[0][4] = "private-source-canary"; return rows }
		}, reason: "invalid-presence", reads: "presence"},
		{name: "measurement error", mutate: func(s *tableBloatTestSource) { s.measurementErr = errors.New("exit status 3: private-source-canary") }, reason: "measurement-source-unavailable", reads: "presence,measurements"},
		{name: "no measurement", mutate: func(s *tableBloatTestSource) { s.noMeasurement = true }, reason: "no-measurement", reads: "presence,measurements"},
		{name: "stale", mutate: func(s *tableBloatTestSource) { s.newestAgo = 10 * 24 * time.Hour; bloated(s) }, reason: "measurement-stale", bloat: true, reads: "presence,measurements"},
		{name: "clock", mutate: func(s *tableBloatTestSource) {
			s.mutateMeasurements = func(rows []Row) []Row { rows[0][2] = tableBloatTestMicros(s.now.Add(time.Minute)); return rows }
		}, reason: "source-clock-unavailable", reads: "presence,measurements"},
		{name: "replica", mutate: func(s *tableBloatTestSource) {
			s.mutateMeasurements = func(rows []Row) []Row { rows[0][3] = "1"; return rows }
		}, reason: "source-identity-or-role-unavailable", reads: "presence,measurements"},
		{name: "narrow frame", mutate: func(s *tableBloatTestSource) {
			s.mutateMeasurements = func(rows []Row) []Row { rows[0] = rows[0][:10]; return rows }
		}, reason: "invalid-frame-width", reads: "presence,measurements"},
		{name: "long cell", mutate: func(s *tableBloatTestSource) {
			s.mutateMeasurements = func(rows []Row) []Row { rows[0][7] = strings.Repeat("x", 257); return rows }
		}, reason: "overbound-cell", reads: "presence,measurements"},
		{name: "unknown kind", mutate: func(s *tableBloatTestSource) {
			s.mutateMeasurements = func(rows []Row) []Row {
				return append(rows, Row{"private-source-canary", "", "", "", "", "", "", "", "", "", ""})
			}
		}, reason: "unexpected-frame-kind", reads: "presence,measurements"},
		{name: "missing end", mutate: func(s *tableBloatTestSource) {
			s.mutateMeasurements = func(rows []Row) []Row { return rows[:len(rows)-1] }
		}, reason: "incomplete-frame", reads: "presence,measurements"},
		{name: "relation kind", mutate: func(s *tableBloatTestSource) {
			bloated(s)
			s.newestAgo = time.Hour
			s.mutateMeasurements = func(rows []Row) []Row { rows[1][2] = "matview"; return rows }
		}, reason: "invalid-relation-row", reads: "presence,measurements"},
		{name: "reclaim over size", mutate: func(s *tableBloatTestSource) {
			bloated(s)
			s.newestAgo = time.Hour
			s.relations[0].reclaimable = s.relations[0].measuredBytes + 1
		}, reason: "invalid-relation-row", reads: "presence,measurements"},
		{name: "relation without its table", mutate: func(s *tableBloatTestSource) {
			bloated(s)
			s.newestAgo = time.Hour
			s.tables = nil
		}, reason: "incomplete-frame", reads: "presence,measurements"},
		{name: "count mismatch", mutate: func(s *tableBloatTestSource) {
			bloated(s)
			s.newestAgo = time.Hour
			s.mutateMeasurements = func(rows []Row) []Row { rows[0][6] = "0"; return rows }
		}, reason: "incomplete-frame", reads: "presence,measurements"},
	} {
		source := tableBloatTestHealthy()
		c.mutate(source)
		alerts := tableBloatTestRun(t, NewTableBloatSignal(), source)
		want := 1
		if c.bloat {
			want = 2
		}
		if len(alerts) != want || strings.Join(source.reads, ",") != c.reads {
			t.Fatalf("%s: %d alerts after reads %v, want %d", c.name, len(alerts), source.reads, want)
		}
		unknown := requireAlertClass(t, alerts, "table-bloat-unobservable")
		if !unknown.unobservable || unknown.Target != "pg-1/bloat-measurement" || unknown.Sustain != 2 || !strings.Contains(unknown.Observed, "reason="+c.reason) {
			t.Fatalf("%s: wrong unknown: %s %s %s", c.name, unknown.Class, unknown.Target, unknown.Observed)
		}
		if c.bloat {
			if stale := requireAlertClass(t, alerts, "table-bloat"); stale.Target != "pg-1/synthetic_stale" || !strings.Contains(unknown.Observed, "newest_measurement_age_days=10.0") {
				t.Fatalf("%s: stale measurement = %s / %s", c.name, stale.Target, unknown.Observed)
			}
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "private-source-canary")
		}
	}
}

func TestTableBloatNeedsOnePrimary(t *testing.T) {
	for _, mode := range []string{"missing", "ambiguous"} {
		settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
			t.Fatal("table bloat read without one primary")
			return nil, nil
		}})
		if mode == "missing" {
			settings.Hosts = settings.Hosts[1:]
		} else {
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-2", Roles: []string{"pg-primary"}})
		}
		alerts, err := NewTableBloatSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "table-bloat-unobservable" || alerts[0].Target != "primary/bloat-measurement" ||
			!strings.Contains(alerts[0].Observed, "primary-inventory-unavailable-or-ambiguous") {
			t.Fatalf("%s primary = %+v, %v", mode, alerts, err)
		}
	}
}

// Cancellation during either read returns lifecycle with the original cause.
func TestTableBloatCancellationKeepsOriginalCause(t *testing.T) {
	for _, stage := range []string{"before", "presence", "measurements"} {
		ctx, cancel := context.WithCancel(context.Background())
		cause := errors.New("private-source-canary")
		source := tableBloatTestHealthy()
		settings := tableBloatTestSettings(t, source)
		settings.Source = &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if stage == "before" {
				t.Fatal("canceled work read the database")
			}
			presence := strings.Contains(query, "to_regclass('public.db_bloat_measurement') IS NULL")
			if (stage == "presence" && presence) || (stage == "measurements" && !presence) {
				cancel()
				return nil, cause
			}
			if presence {
				return source.presenceRows(), nil
			}
			return source.measurementRows(), nil
		}}
		if stage == "before" {
			cancel()
		}
		alerts, err := NewTableBloatSignal().Run(ctx, settings)
		cancel()
		if len(alerts) != 0 || !errors.Is(err, context.Canceled) || (stage != "before" && !errors.Is(err, cause)) {
			t.Fatalf("%s: canceled run lost its causes or published findings", stage)
		}
		if strings.Contains(err.Error(), "private-source-canary") {
			t.Fatal("lifecycle error exposed its private source")
		}
	}
}
