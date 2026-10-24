// Tests of VacuumHotTables' vacuum pass (PARTITION-MAINTENANCE-PLAN.md A1).
// The pure tests drive the real session code over the synthetic PostgreSQL
// wire of db_pool_hot_acquire_test.go, which answers each VACUUM with the
// notices PostgreSQL 18 sends, under a clock the test advances by hand. The
// database tests reproduce the plan's cases in PostgreSQL: a table another
// session holds, a statement that times out on every run, and a horizon a
// snapshot pins.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The notices of one VACUUM (VERBOSE, ANALYZE) of a table with a toast table,
// in the order PostgreSQL 18 sends them, from the format strings of
// heap_vacuum_rel (src/backend/access/heap/vacuumlazy.c), vacuum_rel
// (src/backend/commands/vacuum.c) and do_analyze_rel
// (src/backend/commands/analyze.c) on REL_18_STABLE. The figures are
// synthetic; the heap's summary carries the optional tuples missed and I/O
// timings lines.
var testDbHotVacuumPostgres18Notices = []*pgconn.Notice{
	{Severity: "INFO", Code: "00000", Message: `vacuuming "synthetic.public.pending_task"`},
	{Severity: "INFO", Code: "00000", Message: `finished vacuuming "synthetic.public.pending_task": index scans: 1
pages: 0 removed, 245 remain, 245 scanned (100.00% of total), 0 eagerly scanned
tuples: 31337 removed, 812 remain, 4096 are dead but not yet removable
tuples missed: 7 dead from 2 pages not removed due to cleanup lock contention
removable cutoff: 754312, which was 4102 XIDs old when operation ended
new relfrozenxid: 750210, which is 12 XIDs ahead of previous value
frozen: 0 pages from table (0.00% of total) had 0 tuples frozen
visibility map: 3 pages set all-visible, 0 pages set all-frozen (0 were all-visible)
index scan needed: 240 pages from table (97.96% of total) had 31337 dead item identifiers removed
index "pending_task_pkey": pages: 90 in total, 0 newly deleted, 3 currently deleted, 3 reusable
index "pending_task_run_at_block": pages: 120 in total, 0 newly deleted, 0 currently deleted, 0 reusable
I/O timings: read: 0.512 ms, write: 0.000 ms
avg read rate: 0.000 MB/s, avg write rate: 0.000 MB/s
buffer usage: 1203 hits, 0 reads, 412 dirtied
WAL usage: 1450 records, 245 full page images, 2123456 bytes, 0 buffers full
system usage: CPU: user: 0.01 s, system: 0.00 s, elapsed: 0.03 s`},
	{Severity: "INFO", Code: "00000", Message: `vacuuming "synthetic.pg_toast.pg_toast_16384"`},
	{Severity: "INFO", Code: "00000", Message: `finished vacuuming "synthetic.pg_toast.pg_toast_16384": index scans: 0
pages: 0 removed, 3 remain, 3 scanned (100.00% of total), 0 eagerly scanned
tuples: 9 removed, 4 remain, 5 are dead but not yet removable
removable cutoff: 754312, which was 4102 XIDs old when operation ended
frozen: 0 pages from table (0.00% of total) had 0 tuples frozen
visibility map: 0 pages set all-visible, 0 pages set all-frozen (0 were all-visible)
index scan not needed: 0 pages from table (0.00% of total) had 0 dead item identifiers removed
index "pg_toast_16384_index": pages: 2 in total, 0 newly deleted, 0 currently deleted, 0 reusable
avg read rate: 0.000 MB/s, avg write rate: 0.000 MB/s
buffer usage: 12 hits, 0 reads, 1 dirtied
WAL usage: 2 records, 0 full page images, 236 bytes, 0 buffers full
system usage: CPU: user: 0.00 s, system: 0.00 s, elapsed: 0.00 s`},
	{Severity: "INFO", Code: "00000", Message: `analyzing "public.pending_task"`},
	{Severity: "INFO", Code: "00000", Message: `"pending_task": scanned 245 of 245 pages, containing 812 live rows and 4096 dead rows; 812 rows in sample, 812 estimated total rows`},
	{Severity: "INFO", Code: "00000", Message: `finished analyzing table "synthetic.public.pending_task"
avg read rate: 0.000 MB/s, avg write rate: 0.000 MB/s
buffer usage: 251 hits, 0 reads, 2 dirtied
WAL usage: 9 records, 1 full page images, 9024 bytes, 0 buffers full
system usage: CPU: user: 0.00 s, system: 0.00 s, elapsed: 0.01 s`},
}

// A PostgreSQL 18 summary of one relation with the given figures, in the
// format of testDbHotVacuumPostgres18Notices.
func testDbHotVacuumSummary(relation string, removedRows int64, notRemovableRows int64) string {
	return fmt.Sprintf(`finished vacuuming "%s": index scans: 1
pages: 0 removed, 12 remain, 12 scanned (100.00%% of total), 0 eagerly scanned
tuples: %d removed, 500 remain, %d are dead but not yet removable
removable cutoff: 1203, which was 1 XIDs old when operation ended
frozen: 0 pages from table (0.00%% of total) had 0 tuples frozen
visibility map: 12 pages set all-visible, 0 pages set all-frozen (0 were all-visible)
index scan needed: 12 pages from table (100.00%% of total) had 500 dead item identifiers removed
index "synthetic_pkey": pages: 4 in total, 0 newly deleted, 0 currently deleted, 0 reusable
avg read rate: 0.000 MB/s, avg write rate: 0.000 MB/s
buffer usage: 60 hits, 0 reads, 14 dirtied
WAL usage: 40 records, 12 full page images, 99210 bytes, 0 buffers full
system usage: CPU: user: 0.00 s, system: 0.00 s, elapsed: 0.00 s`, relation, removedRows, notRemovableRows)
}

// The heap's figures are the table's; its toast summary, the start and analyze
// progress lines are not, and the analyze summary reports the analyze.
func TestDbHotVacuumParsesPostgres18VerboseText(t *testing.T) {
	parsed := parseDbHotVacuumNotices(testDbHotVacuumPostgres18Notices)
	if !parsed.vacuumed || !parsed.analyzed || parsed.removedRows != 31337 || parsed.notRemovableRows != 4096 || parsed.skipped != "" {
		t.Fatalf("parsed %+v, want the heap's 31337 removed and 4096 not yet removable, analyzed", parsed)
	}

	// without the analyze summary the analyze did not finish
	parsed = parseDbHotVacuumNotices(testDbHotVacuumPostgres18Notices[:4])
	if !parsed.vacuumed || parsed.analyzed {
		t.Fatalf("parsed %+v without the analyze summary", parsed)
	}

	// a skipped table sends a WARNING with lock_not_available, and the run's
	// toast summary alone is not the table's vacuum
	for _, c := range []struct {
		notices []*pgconn.Notice
		want    dbHotVacuumNotices
	}{
		{
			notices: []*pgconn.Notice{{Severity: "WARNING", Code: pgerrcode.LockNotAvailable, Message: `skipping vacuum of "pending_task" --- lock not available`}},
			want:    dbHotVacuumNotices{skipped: `skipping vacuum of "pending_task" --- lock not available`},
		},
		{
			notices: []*pgconn.Notice{{Severity: "WARNING", Code: pgerrcode.UndefinedTable, Message: `skipping vacuum of "pending_task" --- relation no longer exists`}},
			want:    dbHotVacuumNotices{skipped: `skipping vacuum of "pending_task" --- relation no longer exists`},
		},
		{
			notices: testDbHotVacuumPostgres18Notices[2:4],
			want:    dbHotVacuumNotices{},
		},
		// a skipped analyze after a finished vacuum
		{
			notices: []*pgconn.Notice{
				testDbHotVacuumPostgres18Notices[1],
				{Severity: "WARNING", Code: pgerrcode.LockNotAvailable, Message: `skipping analyze of "pending_task" --- lock not available`},
			},
			want: dbHotVacuumNotices{vacuumed: true, removedRows: 31337, notRemovableRows: 4096, skipped: `skipping analyze of "pending_task" --- lock not available`},
		},
		// each leaf of a partitioned table sends its own summary
		{
			notices: []*pgconn.Notice{
				{Severity: "INFO", Code: "00000", Message: testDbHotVacuumSummary("synthetic.public.finished_task_p20261023", 100, 1)},
				{Severity: "INFO", Code: "00000", Message: testDbHotVacuumSummary("synthetic.public.finished_task_p20261024", 20, 2)},
			},
			want: dbHotVacuumNotices{vacuumed: true, removedRows: 120, notRemovableRows: 3},
		},
		// another warning says why a vacuum did not run, or what it saw
		{
			notices: []*pgconn.Notice{{Severity: "WARNING", SeverityUnlocalized: "WARNING", Code: "01000", Message: `permission denied to vacuum "pending_task", skipping it`}},
			want:    dbHotVacuumNotices{warning: `permission denied to vacuum "pending_task", skipping it`},
		},
		{
			notices: []*pgconn.Notice{
				{Severity: "WARNING", SeverityUnlocalized: "WARNING", Code: "01000", Message: "cutoff for removing and freezing tuples is far in the past"},
				testDbHotVacuumPostgres18Notices[1],
			},
			want: dbHotVacuumNotices{vacuumed: true, removedRows: 31337, notRemovableRows: 4096, warning: "cutoff for removing and freezing tuples is far in the past"},
		},
		// translated messages report no vacuum, so the table goes stale
		{
			notices: []*pgconn.Notice{{Severity: "INFO", Code: "00000", Message: `vacuum de « synthetic.public.pending_task » terminé : parcours d'index : 1`}},
			want:    dbHotVacuumNotices{},
		},
	} {
		if parsed := parseDbHotVacuumNotices(c.notices); parsed != c.want {
			t.Errorf("parsed %+v from %d notice(s), want %+v", parsed, len(c.notices), c.want)
		}
	}
}

// The list and bounds of the plan's A1 and A2.
func TestDbHotVacuumSettingsFollowThePlan(t *testing.T) {
	settings := DefaultDbHotVacuumSettings()
	if !slices.Equal(settings.Tables, []string{
		"pending_task",
		"finished_task",
		"network_client_connection",
		"network_client_location",
		"network_client_handler",
		"transfer_debit_journal",
		"transfer_balance",
		"client_connection_reliability_score",
		"provider_egress_probe_cycle",
		"provider_url_probe_run",
		"device",
		"search_provider_stats",
	}) {
		t.Fatalf("tables = %v", settings.Tables)
	}
	if settings.Interval != 5*time.Minute || settings.RunBudget != 4*time.Minute || settings.StatementTimeout != 2*time.Minute ||
		settings.AnalyzeInterval != time.Hour || settings.StaleAfter != time.Hour || settings.IndexPassInterval != time.Hour ||
		settings.IndexBloatFactor != 2 || settings.IndexMinReclaimBytes != 64*1024*1024 {
		t.Fatalf("settings = %+v", *settings)
	}
	if !(settings.RunBudget < settings.Interval && settings.StatementTimeout < settings.RunBudget) {
		t.Fatal("a run must end within its interval and give a statement its whole timeout")
	}
}

// A clock a test advances by hand. The wire fixture's goroutine advances it
// too, so it locks.
type testDbHotVacuumClock struct {
	stateLock sync.Mutex
	time      time.Time
}

func newTestDbHotVacuumClock() *testDbHotVacuumClock {
	return &testDbHotVacuumClock{time: time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC)}
}

func (self *testDbHotVacuumClock) now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.time
}

func (self *testDbHotVacuumClock) advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.time = self.time.Add(duration)
}

// How the synthetic server answers one table's VACUUM.
type testDbHotVacuumReply struct {
	notices []*pgproto3.NoticeResponse
	err     *pgproto3.ErrorResponse
	// how far the statement advances the clock
	duration time.Duration
}

// The synthetic PostgreSQL side of a hot vacuum run.
type testDbHotVacuumServer struct {
	stateLock sync.Mutex
	clock     *testDbHotVacuumClock
	replies   map[string]testDbHotVacuumReply
	// rows of the horizon query, in its column order
	holders [][][]byte
	// statements answered with an error, by exact text
	statementErrors map[string]*pgproto3.ErrorResponse
	// runs before the reply of the VACUUM of a table; false drops the session
	beforeVacuum func(tableName string) bool
	fixture      *pgPoolWireFixture
	pool         *safePgPool
}

var testDbHotVacuumStatementPattern = regexp.MustCompile(`^VACUUM \(SKIP_LOCKED, VERBOSE, SKIP_DATABASE_STATS(, ANALYZE)?\) "public"\."([a-z0-9_]+)"$`)

func newTestDbHotVacuumServer(t *testing.T, clock *testDbHotVacuumClock) *testDbHotVacuumServer {
	server := &testDbHotVacuumServer{
		clock:           clock,
		replies:         map[string]testDbHotVacuumReply{},
		statementErrors: map[string]*pgproto3.ErrorResponse{},
	}
	reply := func(query string) (testDbHotVacuumReply, string, bool) {
		match := testDbHotVacuumStatementPattern.FindStringSubmatch(query)
		if match == nil {
			return testDbHotVacuumReply{}, "", false
		}
		server.stateLock.Lock()
		defer server.stateLock.Unlock()
		return server.replies[match[2]], match[2], true
	}
	server.fixture, server.pool = newPgPoolWireFixture(t, func(_ int, query string) bool {
		vacuumReply, tableName, ok := reply(query)
		if !ok {
			return true
		}
		clock.advance(vacuumReply.duration)
		return server.beforeVacuum == nil || server.beforeVacuum(tableName)
	}, nil, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		configurePgPoolNotices(config)
		fixture.queryNotices = func(_ int, query string) []*pgproto3.NoticeResponse {
			vacuumReply, _, _ := reply(query)
			return vacuumReply.notices
		}
		fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
			if vacuumReply, _, ok := reply(query); ok {
				return vacuumReply.err
			}
			server.stateLock.Lock()
			defer server.stateLock.Unlock()
			return server.statementErrors[query]
		}
		fixture.queryRows = func(_ int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
			if query != dbHorizonHoldersQuery {
				return nil, nil
			}
			server.stateLock.Lock()
			defer server.stateLock.Unlock()
			return []pgproto3.FieldDescription{
				{Name: []byte("kind"), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1},
				{Name: []byte("pid"), DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1},
				{Name: []byte("application_name"), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1},
				{Name: []byte("state"), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1},
				{Name: []byte("xid_age"), DataTypeOID: 20, DataTypeSize: 8, TypeModifier: -1},
				{Name: []byte("age_seconds"), DataTypeOID: 701, DataTypeSize: 8, TypeModifier: -1},
			}, server.holders
		}
	})
	return server
}

func (self *testDbHotVacuumServer) setReply(tableName string, reply testDbHotVacuumReply) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.replies[tableName] = reply
}

// The statements the run sent, less the pool's validation pings, since mark.
func (self *testDbHotVacuumServer) statements(mark int) []string {
	self.fixture.stateLock.Lock()
	defer self.fixture.stateLock.Unlock()
	statements := []string{}
	for _, query := range self.fixture.queries[mark:] {
		if query != "-- ping" {
			statements = append(statements, query)
		}
	}
	return statements
}

func (self *testDbHotVacuumServer) mark() int {
	self.fixture.stateLock.Lock()
	defer self.fixture.stateLock.Unlock()
	return len(self.fixture.queries)
}

func (self *testDbHotVacuumServer) dialCount() int {
	self.fixture.stateLock.Lock()
	defer self.fixture.stateLock.Unlock()
	return self.fixture.dialCount
}

// One table's finished vacuum, and its analyze when the statement asks.
func testDbHotVacuumFinished(tableName string, removedRows int64, notRemovableRows int64) testDbHotVacuumReply {
	return testDbHotVacuumReply{notices: []*pgproto3.NoticeResponse{
		{Severity: "INFO", SeverityUnlocalized: "INFO", Code: "00000", Message: testDbHotVacuumSummary("synthetic.public."+tableName, removedRows, notRemovableRows)},
		{Severity: "INFO", SeverityUnlocalized: "INFO", Code: "00000", Message: fmt.Sprintf(`finished analyzing table "synthetic.public.%s"
avg read rate: 0.000 MB/s, avg write rate: 0.000 MB/s
buffer usage: 20 hits, 0 reads, 0 dirtied
WAL usage: 3 records, 0 full page images, 512 bytes, 0 buffers full
system usage: CPU: user: 0.00 s, system: 0.00 s, elapsed: 0.00 s`, tableName)},
	}}
}

// What PostgreSQL sends for a table another session holds.
func testDbHotVacuumLocked(tableName string) testDbHotVacuumReply {
	return testDbHotVacuumReply{notices: []*pgproto3.NoticeResponse{{
		Severity:            "WARNING",
		SeverityUnlocalized: "WARNING",
		Code:                pgerrcode.LockNotAvailable,
		Message:             fmt.Sprintf(`skipping vacuum of "%s" --- lock not available`, tableName),
	}}}
}

// What PostgreSQL sends for a statement its statement_timeout canceled.
func testDbHotVacuumCanceled() *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: pgerrcode.QueryCanceled, Message: "canceling statement due to statement timeout"}
}

// The defaults with the synthetic tables, and no index pass (it reads the
// maintenance pool, which a pure test does not have).
func testDbHotVacuumSettings(tableNames ...string) *DbHotVacuumSettings {
	settings := DefaultDbHotVacuumSettings()
	settings.Tables = tableNames
	settings.IndexPassInterval = 1000 * time.Hour
	return settings
}

func testDbHotVacuumStatements(tableName string, analyze bool, timeout time.Duration) []string {
	statement := dbHotVacuumStatement
	if analyze {
		statement = dbHotVacuumAnalyzeStatement
	}
	return []string{
		fmt.Sprintf("SET statement_timeout = %d", timeout.Milliseconds()),
		statement + pgx.Identifier{"public", tableName}.Sanitize(),
		"RESET statement_timeout",
	}
}

func testDbHotVacuumOutcomes(report *dbHotVacuumReport) []string {
	outcomes := []string{}
	for _, result := range report.tables {
		outcomes = append(outcomes, result.tableName+" "+string(result.outcome))
	}
	return outcomes
}

// A locked table followed by an eligible one: the skip must not stall the run.
// The locked table is skipped at once, the next is vacuumed in the same run,
// both sessions go back to the pool with their timeout reset, and only the
// vacuumed table's times advance. The next run analyzes only the table whose
// analyze is still due.
func TestDbHotVacuumRunMovesPastASkippedTable(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	server := newTestDbHotVacuumServer(t, clock)
	server.setReply("synthetic_hot_locked", testDbHotVacuumLocked("synthetic_hot_locked"))
	server.setReply("synthetic_hot_free", testDbHotVacuumFinished("synthetic_hot_free", 500, 0))
	settings := testDbHotVacuumSettings("synthetic_hot_locked", "synthetic_hot_free")
	skippedBefore := testutil.ToFloat64(dbHotVacuumAttemptCounter.WithLabelValues("synthetic_hot_locked", string(dbHotVacuumSkipped)))

	run := newDbHotVacuumRun(settings, server.pool, clock.now, DbHotVacuumState{IndexPassTime: clock.now()})
	run.run(t.Context())
	if outcomes := testDbHotVacuumOutcomes(&run.report); !slices.Equal(outcomes, []string{"synthetic_hot_locked skipped", "synthetic_hot_free vacuumed"}) {
		t.Fatalf("outcomes = %v", outcomes)
	}
	if detail := run.report.tables[0].detail; !strings.Contains(detail, "lock not available") {
		t.Fatalf("skip detail = %q", detail)
	}
	if free := run.report.tables[1]; free.removedRows != 500 || !free.analyzed {
		t.Fatalf("free table result = %+v", free)
	}
	want := slices.Concat(
		testDbHotVacuumStatements("synthetic_hot_locked", true, 2*time.Minute),
		testDbHotVacuumStatements("synthetic_hot_free", true, 2*time.Minute),
	)
	if statements := server.statements(0); !slices.Equal(statements, want) {
		t.Fatalf("statements = %q, want %q", statements, want)
	}
	if server.dialCount() != 1 {
		t.Fatalf("%d sessions dialed: a reset session goes back to the pool", server.dialCount())
	}
	locked := run.state.Tables["synthetic_hot_locked"]
	free := run.state.Tables["synthetic_hot_free"]
	if !locked.VacuumTime.IsZero() || !locked.AnalyzeTime.IsZero() || !locked.TrackTime.Equal(clock.now()) ||
		!free.VacuumTime.Equal(clock.now()) || !free.AnalyzeTime.Equal(clock.now()) || run.state.NextTable != "synthetic_hot_locked" {
		t.Fatalf("state = %+v", run.state)
	}
	if skipped := testutil.ToFloat64(dbHotVacuumAttemptCounter.WithLabelValues("synthetic_hot_locked", string(dbHotVacuumSkipped))) - skippedBefore; skipped != 1 {
		t.Fatalf("skip counter rose by %v", skipped)
	}

	clock.advance(5 * time.Minute)
	mark := server.mark()
	run = newDbHotVacuumRun(settings, server.pool, clock.now, run.state)
	run.run(t.Context())
	want = slices.Concat(
		testDbHotVacuumStatements("synthetic_hot_locked", true, 2*time.Minute),
		testDbHotVacuumStatements("synthetic_hot_free", false, 2*time.Minute),
	)
	if statements := server.statements(mark); !slices.Equal(statements, want) {
		t.Fatalf("second run statements = %q, want %q", statements, want)
	}
}

// A statement that times out on every run: every later table still gets its
// turn in every run, and the warning (its metric and the stale list) fires
// once the table has had no finished vacuum for an hour, not before.
func TestDbHotVacuumRunWarnsAfterAnHourOfTimeouts(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	server := newTestDbHotVacuumServer(t, clock)
	server.setReply("synthetic_hot_slow", testDbHotVacuumReply{err: testDbHotVacuumCanceled(), duration: 2 * time.Minute})
	server.setReply("synthetic_hot_after", testDbHotVacuumFinished("synthetic_hot_after", 10, 0))
	settings := testDbHotVacuumSettings("synthetic_hot_slow", "synthetic_hot_after")
	staleBefore := testutil.ToFloat64(dbHotVacuumStaleCounter.WithLabelValues("synthetic_hot_slow"))

	startTime := clock.now()
	state := DbHotVacuumState{IndexPassTime: startTime}
	for runIndex := 0; runIndex <= 13; runIndex += 1 {
		runStart := clock.now()
		run := newDbHotVacuumRun(settings, server.pool, clock.now, state)
		run.run(t.Context())
		state = run.state
		if outcomes := testDbHotVacuumOutcomes(&run.report); !slices.Equal(outcomes, []string{"synthetic_hot_slow timed_out", "synthetic_hot_after vacuumed"}) {
			t.Fatalf("run %d outcomes = %v", runIndex, outcomes)
		}
		staleTableNames := []string{}
		for _, staleTable := range run.report.staleTables {
			staleTableNames = append(staleTableNames, staleTable.tableName)
		}
		// the slow table's statement ends two minutes into each run
		if clock.now().Sub(startTime) < time.Hour {
			if len(staleTableNames) != 0 {
				t.Fatalf("run %d warned at %s: %v", runIndex, clock.now().Sub(startTime), staleTableNames)
			}
		} else if !slices.Equal(staleTableNames, []string{"synthetic_hot_slow"}) {
			t.Fatalf("run %d at %s: stale %v, want the slow table", runIndex, clock.now().Sub(startTime), staleTableNames)
		}
		if after := state.Tables["synthetic_hot_after"]; !after.VacuumTime.Equal(clock.now()) {
			t.Fatalf("run %d: the table after the slow one was not vacuumed: %+v", runIndex, after)
		}
		if !state.Tables["synthetic_hot_slow"].VacuumTime.IsZero() || state.NextTable != "synthetic_hot_slow" {
			t.Fatalf("run %d state = %+v", runIndex, state)
		}
		clock.advance(runStart.Add(5 * time.Minute).Sub(clock.now()))
	}
	// runs at 1h2m and 1h7m into the stretch
	if stale := testutil.ToFloat64(dbHotVacuumStaleCounter.WithLabelValues("synthetic_hot_slow")) - staleBefore; stale != 2 {
		t.Fatalf("stale counter rose by %v, want 2", stale)
	}
}

// A run stops at its budget and the next resumes with the table it did not
// reach; a statement whose timeout the budget cut short and that then timed
// out keeps its turn, and its table's state is unchanged.
func TestDbHotVacuumRunResumesWhereTheBudgetEnded(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	server := newTestDbHotVacuumServer(t, clock)
	tableNames := []string{"synthetic_hot_a", "synthetic_hot_b", "synthetic_hot_c", "synthetic_hot_d"}
	for _, tableName := range tableNames {
		reply := testDbHotVacuumFinished(tableName, 1, 0)
		reply.duration = 90 * time.Second
		server.setReply(tableName, reply)
	}
	settings := testDbHotVacuumSettings(tableNames...)

	// 0s, 90s and 180s in: the third gets the minute the budget has left
	run := newDbHotVacuumRun(settings, server.pool, clock.now, DbHotVacuumState{IndexPassTime: clock.now()})
	run.run(t.Context())
	if outcomes := testDbHotVacuumOutcomes(&run.report); !slices.Equal(outcomes, []string{"synthetic_hot_a vacuumed", "synthetic_hot_b vacuumed", "synthetic_hot_c vacuumed"}) {
		t.Fatalf("first run outcomes = %v", outcomes)
	}
	want := slices.Concat(
		testDbHotVacuumStatements("synthetic_hot_a", true, 2*time.Minute),
		testDbHotVacuumStatements("synthetic_hot_b", true, 2*time.Minute),
		testDbHotVacuumStatements("synthetic_hot_c", true, time.Minute),
	)
	if statements := server.statements(0); !slices.Equal(statements, want) {
		t.Fatalf("first run statements = %q, want %q", statements, want)
	}
	if run.report.stopped == "" || run.state.NextTable != "synthetic_hot_d" {
		t.Fatalf("first run stopped %q with next %q, want the budget spent before synthetic_hot_d", run.report.stopped, run.state.NextTable)
	}

	// the next run starts with d, and the budget cuts b's statement short
	server.setReply("synthetic_hot_b", testDbHotVacuumReply{err: testDbHotVacuumCanceled(), duration: 90 * time.Second})
	bBefore := run.state.Tables["synthetic_hot_b"]
	clock.advance(5 * time.Minute)
	mark := server.mark()
	run = newDbHotVacuumRun(settings, server.pool, clock.now, run.state)
	run.run(t.Context())
	if outcomes := testDbHotVacuumOutcomes(&run.report); !slices.Equal(outcomes, []string{"synthetic_hot_d vacuumed", "synthetic_hot_a vacuumed", "synthetic_hot_b out_of_budget"}) {
		t.Fatalf("second run outcomes = %v", outcomes)
	}
	want = slices.Concat(
		testDbHotVacuumStatements("synthetic_hot_d", true, 2*time.Minute),
		testDbHotVacuumStatements("synthetic_hot_a", false, 2*time.Minute),
		testDbHotVacuumStatements("synthetic_hot_b", false, time.Minute),
	)
	if statements := server.statements(mark); !slices.Equal(statements, want) {
		t.Fatalf("second run statements = %q, want %q", statements, want)
	}
	if run.state.NextTable != "synthetic_hot_b" || run.state.Tables["synthetic_hot_b"] != bBefore {
		t.Fatalf("second run state = %+v, want b to keep its turn and its times", run.state)
	}

	// b's turn comes first again, with its whole statement timeout
	server.setReply("synthetic_hot_b", testDbHotVacuumFinished("synthetic_hot_b", 1, 0))
	clock.advance(5 * time.Minute)
	mark = server.mark()
	run = newDbHotVacuumRun(settings, server.pool, clock.now, run.state)
	run.run(t.Context())
	if statements := server.statements(mark); len(statements) < 3 || !slices.Equal(statements[:3], testDbHotVacuumStatements("synthetic_hot_b", false, 2*time.Minute)) {
		t.Fatalf("third run statements = %q", statements)
	}
}

// Rows not yet removable above the threshold name the oldest horizon holders,
// by pid, application name, state and ages only; below it the horizon is not
// read at all. The holder query reads no query text or client address.
func TestDbHotVacuumRunNamesTheHorizonHolders(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	server := newTestDbHotVacuumServer(t, clock)
	settings := testDbHotVacuumSettings("synthetic_hot_pinned")
	server.setReply("synthetic_hot_pinned", testDbHotVacuumFinished("synthetic_hot_pinned", 0, settings.MinNotRemovableRows-1))
	server.holders = [][][]byte{
		{[]byte("prepared transaction"), []byte("0"), []byte(""), []byte("prepared"), []byte("120"), []byte("30")},
		{[]byte("session"), []byte("4242"), []byte("synthetic-app"), []byte("idle in transaction"), []byte("9000"), []byte("912.5")},
	}

	run := newDbHotVacuumRun(settings, server.pool, clock.now, DbHotVacuumState{IndexPassTime: clock.now()})
	run.run(t.Context())
	if run.report.horizonRead || slices.Contains(server.statements(0), dbHorizonHoldersQuery) {
		t.Fatal("the horizon was read below the threshold")
	}

	server.setReply("synthetic_hot_pinned", testDbHotVacuumFinished("synthetic_hot_pinned", 0, settings.MinNotRemovableRows))
	clock.advance(5 * time.Minute)
	run = newDbHotVacuumRun(settings, server.pool, clock.now, run.state)
	run.run(t.Context())
	if result := run.report.tables[0]; result.outcome != dbHotVacuumVacuumed || result.notRemovableRows != settings.MinNotRemovableRows {
		t.Fatalf("result = %+v", result)
	}
	if !run.report.horizonRead || run.report.horizonErr != nil || len(run.report.horizonHolders) != 2 {
		t.Fatalf("horizon read %t, err %v, holders %+v", run.report.horizonRead, run.report.horizonErr, run.report.horizonHolders)
	}
	holder := run.report.horizonHolders[0]
	if text := holder.String(); text != `session pid 4242 (application "synthetic-app", state "idle in transaction", 9000 xids old, open 15m13s)` {
		t.Fatalf("oldest holder = %q", text)
	}
	if text := run.report.horizonHolders[1].String(); text != `prepared transaction pid 0 (application "", state "prepared", 120 xids old, open 30s)` {
		t.Fatalf("second holder = %q", text)
	}
	for _, column := range []string{"query", "client_addr", "client_hostname", "client_port", "usename", "gid"} {
		if regexp.MustCompile(`\b` + column + `\b`).MatchString(dbHorizonHoldersQuery) {
			t.Errorf("the holder query reads %s", column)
		}
	}
}

// A statement that completes without a summary did not vacuum the table: its
// turn is unreported, its warning says why, and its vacuum time stays.
func TestDbHotVacuumRunCountsAVacuumWithoutSummaryAsUnfinished(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	server := newTestDbHotVacuumServer(t, clock)
	server.setReply("synthetic_hot_refused", testDbHotVacuumReply{notices: []*pgproto3.NoticeResponse{{
		Severity:            "WARNING",
		SeverityUnlocalized: "WARNING",
		Code:                "01000",
		Message:             `permission denied to vacuum "synthetic_hot_refused", skipping it`,
	}}})
	run := newDbHotVacuumRun(testDbHotVacuumSettings("synthetic_hot_refused"), server.pool, clock.now, DbHotVacuumState{IndexPassTime: clock.now()})
	run.run(t.Context())
	result := run.report.tables[0]
	if result.outcome != dbHotVacuumUnreported || !strings.Contains(result.detail, "permission denied") {
		t.Fatalf("result = %+v", result)
	}
	if !run.state.Tables["synthetic_hot_refused"].VacuumTime.IsZero() {
		t.Fatal("an unreported vacuum was recorded as finished")
	}
}

// Every session goes back to the pool only with its statement_timeout reset:
// one whose reset fails is discarded, and the next statement dials a new one.
func TestDbHotVacuumSessionIsDiscardedWhenItsTimeoutCannotBeReset(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	server := newTestDbHotVacuumServer(t, clock)
	server.setReply("synthetic_hot_a", testDbHotVacuumFinished("synthetic_hot_a", 1, 0))
	server.setReply("synthetic_hot_b", testDbHotVacuumFinished("synthetic_hot_b", 1, 0))
	server.statementErrors["RESET statement_timeout"] = &pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: pgerrcode.InternalError, Message: "synthetic reset failure"}

	run := newDbHotVacuumRun(testDbHotVacuumSettings("synthetic_hot_a", "synthetic_hot_b"), server.pool, clock.now, DbHotVacuumState{IndexPassTime: clock.now()})
	run.run(t.Context())
	if outcomes := testDbHotVacuumOutcomes(&run.report); !slices.Equal(outcomes, []string{"synthetic_hot_a vacuumed", "synthetic_hot_b vacuumed"}) {
		t.Fatalf("outcomes = %v", outcomes)
	}
	if server.dialCount() != 2 {
		t.Fatalf("%d sessions dialed, want the unreset one discarded and replaced", server.dialCount())
	}
}

// A stop during a statement keeps the table's turn for the next run and
// discards its session; nothing after it runs.
func TestDbHotVacuumRunKeepsTheInterruptedTableForTheNextRun(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	server := newTestDbHotVacuumServer(t, clock)
	server.setReply("synthetic_hot_a", testDbHotVacuumFinished("synthetic_hot_a", 1, 0))
	server.setReply("synthetic_hot_b", testDbHotVacuumFinished("synthetic_hot_b", 1, 0))
	server.setReply("synthetic_hot_c", testDbHotVacuumFinished("synthetic_hot_c", 1, 0))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// the drain cancels the run while b's statement is in flight
	server.beforeVacuum = func(tableName string) bool {
		if tableName != "synthetic_hot_b" {
			return true
		}
		cancel()
		return false
	}

	run := newDbHotVacuumRun(testDbHotVacuumSettings("synthetic_hot_a", "synthetic_hot_b", "synthetic_hot_c"), server.pool, clock.now, DbHotVacuumState{IndexPassTime: clock.now()})
	run.run(ctx)
	if outcomes := testDbHotVacuumOutcomes(&run.report); !slices.Equal(outcomes, []string{"synthetic_hot_a vacuumed", "synthetic_hot_b interrupted"}) {
		t.Fatalf("outcomes = %v", outcomes)
	}
	if run.state.NextTable != "synthetic_hot_b" || !run.state.Tables["synthetic_hot_b"].VacuumTime.IsZero() || run.report.stopped == "" {
		t.Fatalf("state %+v stopped %q", run.state, run.report.stopped)
	}
}

// The state drops a table no longer listed, tracks a new one from the run
// that first lists it, and starts with the first table when its next table is
// gone; the state survives its JSON round trip through the task arguments.
func TestDbHotVacuumStateFollowsTheList(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	server := newTestDbHotVacuumServer(t, clock)
	for _, tableName := range []string{"synthetic_hot_kept", "synthetic_hot_new"} {
		server.setReply(tableName, testDbHotVacuumFinished(tableName, 1, 0))
	}
	earlier := clock.now().Add(-time.Hour)
	state := DbHotVacuumState{
		NextTable: "synthetic_hot_gone",
		Tables: map[string]DbHotVacuumTableState{
			"synthetic_hot_kept": {TrackTime: earlier, VacuumTime: earlier, AnalyzeTime: clock.now().Add(-time.Minute)},
			"synthetic_hot_gone": {TrackTime: earlier},
		},
		IndexPassTime: clock.now(),
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var decoded DbHotVacuumState
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	run := newDbHotVacuumRun(testDbHotVacuumSettings("synthetic_hot_kept", "synthetic_hot_new"), server.pool, clock.now, decoded)
	run.run(t.Context())
	if _, ok := run.state.Tables["synthetic_hot_gone"]; ok || len(run.state.Tables) != 2 {
		t.Fatalf("tables = %+v", run.state.Tables)
	}
	if outcomes := testDbHotVacuumOutcomes(&run.report); !slices.Equal(outcomes, []string{"synthetic_hot_kept vacuumed", "synthetic_hot_new vacuumed"}) {
		t.Fatalf("outcomes = %v", outcomes)
	}
	if kept := run.state.Tables["synthetic_hot_kept"]; !kept.TrackTime.Equal(earlier) || !kept.AnalyzeTime.Equal(clock.now().Add(-time.Minute)) {
		t.Fatalf("kept table state = %+v", kept)
	}
	if created := run.state.Tables["synthetic_hot_new"]; !created.TrackTime.Equal(clock.now()) {
		t.Fatalf("new table state = %+v", created)
	}
	// the caller's state is not changed by the run
	if _, ok := decoded.Tables["synthetic_hot_gone"]; !ok {
		t.Fatal("the run changed its input state")
	}
	var fresh DbHotVacuumState
	if err := json.Unmarshal([]byte(`{}`), &fresh); err != nil || fresh.NextTable != "" || fresh.Tables != nil || !fresh.IndexPassTime.IsZero() {
		t.Fatalf("fresh state = %+v, %v", fresh, err)
	}
}

// The index pass is due an interval after the last one and only while the
// run has budget left.
func TestDbHotVacuumIndexPassIsDueHourly(t *testing.T) {
	clock := newTestDbHotVacuumClock()
	settings := DefaultDbHotVacuumSettings()
	for _, c := range []struct {
		lastPass time.Time
		spent    time.Duration
		due      bool
	}{
		{lastPass: time.Time{}, due: true},
		{lastPass: clock.now().Add(-settings.IndexPassInterval + time.Second), due: false},
		{lastPass: clock.now().Add(-settings.IndexPassInterval), due: true},
		{lastPass: clock.now().Add(-2 * settings.IndexPassInterval), spent: settings.RunBudget, due: false},
	} {
		run := newDbHotVacuumRun(settings, nil, clock.now, DbHotVacuumState{IndexPassTime: c.lastPass})
		run.deadline = clock.now().Add(settings.RunBudget - c.spent)
		if due := run.indexPassDue(t.Context()); due != c.due {
			t.Errorf("last pass %s, %s spent: due %t, want %t", c.lastPass, c.spent, due, c.due)
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	run := newDbHotVacuumRun(settings, nil, clock.now, DbHotVacuumState{})
	run.deadline = clock.now().Add(settings.RunBudget)
	if run.indexPassDue(canceled) {
		t.Error("an index pass is due after the caller stopped")
	}
}

// Notices reach a handler only on a connection of a pool that dispatches
// them, and only while the handler's statement runs; a handler the pool
// already had keeps every notice. Only the maintenance pool dispatches.
func TestPgPoolNoticesReachOnlyTheRegisteredStatement(t *testing.T) {
	if !safeMaintenancePool.dispatchNotices || safePool.dispatchNotices {
		t.Fatal("only the maintenance pool dispatches notices")
	}
	notice := &pgproto3.NoticeResponse{Severity: "INFO", SeverityUnlocalized: "INFO", Code: "00000", Message: "synthetic notice"}
	earlierNotices := 0
	_, pool := newPgPoolWireFixture(t, nil, nil, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		config.ConnConfig.OnNotice = func(*pgconn.PgConn, *pgconn.Notice) { earlierNotices += 1 }
		configurePgPoolNotices(config)
		fixture.queryNotices = func(_ int, query string) []*pgproto3.NoticeResponse {
			if query == "SELECT 1" {
				return []*pgproto3.NoticeResponse{notice}
			}
			return nil
		}
	})
	conn, err := acquirePgPoolConn(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	received := []string{}
	handler := func(notice *pgconn.Notice) {
		received = append(received, notice.Message)
	}
	RaisePgResult(conn.Exec(t.Context(), "SELECT 1"))
	withPgNotices(conn.Conn().PgConn(), handler, func() {
		RaisePgResult(conn.Exec(t.Context(), "SELECT 1"))
	})
	RaisePgResult(conn.Exec(t.Context(), "SELECT 1"))
	if !slices.Equal(received, []string{"synthetic notice"}) || earlierNotices != 3 {
		t.Fatalf("handler received %q, the pool's own handler %d", received, earlierNotices)
	}
}

// In PostgreSQL: a table another session holds (with the SHARE UPDATE
// EXCLUSIVE lock an autovacuum holds) is skipped without the run queuing for
// its lock, the next table's vacuum removes its dead rows in the same run, and
// the holder keeps its lock. The next run, once the holder is gone, vacuums
// the skipped table.
func TestDbHotVacuumSkipsATableAnotherSessionHolds(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_hot_locked (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_hot_locked SELECT n, n FROM generate_series(1, 1000) n;
				DELETE FROM synthetic_hot_locked WHERE id % 2 = 0;
				CREATE TABLE synthetic_hot_free (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_hot_free SELECT n, n FROM generate_series(1, 1000) n;
				DELETE FROM synthetic_hot_free WHERE id % 2 = 0
			`))
		})
		clock := newTestDbHotVacuumClock()
		settings := testDbHotVacuumSettings("synthetic_hot_locked", "synthetic_hot_free")
		holder, release := testDbMaintenanceHoldVacuumLock(t, ctx, "synthetic_hot_locked")
		defer release()

		run := newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, DbHotVacuumState{IndexPassTime: clock.now()})
		testDbMaintenanceRunWithoutQueuing(t, ctx, func() *DbMaintenanceOutcome {
			run.run(ctx)
			return &DbMaintenanceOutcome{}
		}, release, nil, nil, "synthetic_hot_locked")
		if outcomes := testDbHotVacuumOutcomes(&run.report); !slices.Equal(outcomes, []string{"synthetic_hot_locked skipped", "synthetic_hot_free vacuumed"}) {
			t.Fatalf("outcomes beside the holder = %v", outcomes)
		}
		if free := run.report.tables[1]; free.removedRows != 500 || free.notRemovableRows != 0 || !free.analyzed {
			t.Fatalf("free table result = %+v", free)
		}
		if !holder("synthetic_hot_locked") {
			t.Fatal("the session holding the table lost its lock")
		}

		release()
		clock.advance(5 * time.Minute)
		run = newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, run.state)
		run.run(ctx)
		if locked := run.report.tables[0]; locked.outcome != dbHotVacuumVacuumed || locked.removedRows != 500 {
			t.Fatalf("the next run's result for the released table = %+v", locked)
		}
		if run.state.Tables["synthetic_hot_locked"].VacuumTime.IsZero() {
			t.Fatal("the released table's vacuum was not recorded")
		}
	})
}

// In PostgreSQL: a statement that times out on every run. A session holding
// ACCESS SHARE on the slow table, which pins no snapshot, keeps its emptied
// trailing pages from being truncated: vacuum retries the ACCESS EXCLUSIVE
// lock it needs for five seconds, and every run's one-second statement_timeout
// ends the statement there. The table after it is vacuumed in every run, and
// the warning fires once the slow table has had no finished vacuum for the
// stale bound.
func TestDbHotVacuumStatementTimeoutWarnsAndLaterTablesRun(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_hot_slow (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false, vacuum_truncate = true);
				INSERT INTO synthetic_hot_slow SELECT n, n FROM generate_series(1, 10000) n;
				DELETE FROM synthetic_hot_slow;
				CREATE TABLE synthetic_hot_after (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_hot_after SELECT n, n FROM generate_series(1, 1000) n;
				DELETE FROM synthetic_hot_after WHERE id % 2 = 0
			`))
		})
		_, release := testDbMaintenanceHoldTableLock(t, ctx, "ACCESS SHARE", "synthetic_hot_slow")
		defer release()
		clock := newTestDbHotVacuumClock()
		settings := testDbHotVacuumSettings("synthetic_hot_slow", "synthetic_hot_after")
		// below the five seconds vacuum retries the truncation lock
		settings.StatementTimeout = time.Second
		settings.StaleAfter = 10 * time.Minute
		staleBefore := testutil.ToFloat64(dbHotVacuumStaleCounter.WithLabelValues("synthetic_hot_slow"))

		state := DbHotVacuumState{IndexPassTime: clock.now()}
		for runIndex := range 3 {
			run := newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, state)
			run.run(ctx)
			state = run.state
			if outcomes := testDbHotVacuumOutcomes(&run.report); !slices.Equal(outcomes, []string{"synthetic_hot_slow timed_out", "synthetic_hot_after vacuumed"}) {
				t.Fatalf("run %d outcomes = %v (%+v)", runIndex, outcomes, run.report.tables)
			}
			if runIndex == 0 && run.report.tables[1].removedRows != 500 {
				t.Fatalf("the table after the slow one = %+v", run.report.tables[1])
			}
			stale := 0 < len(run.report.staleTables)
			if stale != (runIndex == 2) || (stale && run.report.staleTables[0].tableName != "synthetic_hot_slow") {
				t.Fatalf("run %d stale tables = %+v", runIndex, run.report.staleTables)
			}
			clock.advance(5 * time.Minute)
		}
		if rose := testutil.ToFloat64(dbHotVacuumStaleCounter.WithLabelValues("synthetic_hot_slow")) - staleBefore; rose != 1 {
			t.Fatalf("stale counter rose by %v", rose)
		}
		// each timed-out statement still reset its session
		MaintenanceDb(ctx, func(conn PgConn) {
			var timeout string
			Raise(conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&timeout))
			if timeout == "1s" {
				t.Fatal("a pooled maintenance session kept the run's statement_timeout")
			}
		}, OptNoRetry())
	})
}

// In PostgreSQL: a snapshot older than a delete pins the horizon. The run
// reports the deleted rows as not yet removable and names the session that
// holds the snapshot, by its pid.
func TestDbHotVacuumNamesTheSessionPinningTheHorizon(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_hot_pinned (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_hot_pinned SELECT n, n FROM generate_series(1, 2000) n
			`))
		})
		// a repeatable read snapshot taken before the delete
		conn, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		if err != nil {
			conn.Release()
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = tx.Rollback(context.WithoutCancel(ctx))
				conn.Release()
			}
		}
		defer release()
		var holderPid int32
		Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `DELETE FROM synthetic_hot_pinned WHERE id <= 1500`))
		})

		clock := newTestDbHotVacuumClock()
		settings := testDbHotVacuumSettings("synthetic_hot_pinned")
		settings.MinNotRemovableRows = 1000
		run := newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, DbHotVacuumState{IndexPassTime: clock.now()})
		run.run(ctx)
		if result := run.report.tables[0]; result.outcome != dbHotVacuumVacuumed || result.removedRows != 0 || result.notRemovableRows != 1500 {
			t.Fatalf("result behind the pinned horizon = %+v", result)
		}
		if !run.report.horizonRead || run.report.horizonErr != nil {
			t.Fatalf("horizon read %t, err %v", run.report.horizonRead, run.report.horizonErr)
		}
		// the oldest session; a slot elsewhere in the cluster may be older
		sessionIndex := slices.IndexFunc(run.report.horizonHolders, func(holder dbHorizonHolder) bool {
			return holder.kind == "session"
		})
		if sessionIndex < 0 {
			t.Fatalf("holders %+v name no session", run.report.horizonHolders)
		}
		if oldest := run.report.horizonHolders[sessionIndex]; oldest.pid != holderPid || oldest.xidAge <= 0 || oldest.state != "idle in transaction" {
			t.Fatalf("oldest session holder = %+v, want the session %d", oldest, holderPid)
		}

		// once the snapshot ends the rows go
		release()
		clock.advance(5 * time.Minute)
		run = newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, run.state)
		run.run(ctx)
		if result := run.report.tables[0]; result.removedRows != 1500 || result.notRemovableRows != 0 || run.report.horizonRead {
			t.Fatalf("result after the snapshot ended = %+v, horizon read %t", result, run.report.horizonRead)
		}
	})
}

// On the migrated schema every listed table exists as a table its vacuum can
// name, so a renamed or dropped table cannot silently leave the list.
func TestDbHotVacuumTablesExistInTheMigratedSchema(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, tableName := range DefaultDbHotVacuumSettings().Tables {
			var relationKind string
			MaintenanceDb(ctx, func(conn PgConn) {
				err := conn.QueryRow(
					ctx,
					`SELECT relkind::text FROM pg_class WHERE oid = to_regclass($1)`,
					pgx.Identifier{"public", tableName}.Sanitize(),
				).Scan(&relationKind)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					Raise(err)
				}
			}, OptNoRetry())
			if relationKind != "r" && relationKind != "p" {
				t.Errorf("%s is not a table in the migrated schema (relkind %q)", tableName, relationKind)
			}
		}
	})
}
