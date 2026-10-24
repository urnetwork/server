// Vacuums the churn tables every few minutes and rebuilds their bloated
// indexes once an hour (PARTITION-MAINTENANCE-PLAN.md A1 and A2).
//
// A churn table holds a small live set that is rewritten constantly. Before
// the 2026-10-23 VACUUM FULL almost all of their size was dead: six
// autovacuum workers serve a database of several TB, and a pass over a large
// table holds a worker for hours, so these small tables waited. An explicit
// VACUUM needs no worker. Each run vacuums every table on the list in turn on
// a direct maintenance session, adds ANALYZE once a table's last one is an
// hour old, and reads each table's dead rows removed and not yet removable
// from the VACUUM (VERBOSE) summary (pg_stat_progress_vacuum does not report
// the second). Each statement has a statement_timeout and the run a budget;
// a run that stops early resumes at the table it did not reach, so a table
// that times out on every run never starves the tables after it. Rows not yet
// removable mean an old snapshot, replication slot or prepared transaction
// holds the cleanup horizon back (A6), so the run names the oldest holders
// instead of vacuuming harder. A table whose vacuum has not finished for an
// hour, skipped or timed out, raises a warning: a vacuum that started is not
// one that finished.
//
// A plain vacuum stops a heap from growing but does not shrink a time-ordered
// index, whose pages keep a few live entries each. So once an hour the run
// measures every index of the tables against its compact estimate and
// rebuilds the bloated ones with REINDEX INDEX CONCURRENTLY, behind the
// rotation's guards (db_maintenance.go), within what is left of the budget.
//
// Like DbMaintenance, nothing here queues behind a lock an autovacuum holds,
// which would cancel it: the vacuum skips a locked table (SKIP_LOCKED), and a
// rebuild waits for its table to clear without taking a place in the queue.
//
// The run's state (where it resumes, each table's last finished vacuum and
// analyze, and the last index pass) travels in the task chain's arguments
// (taskworker/work/maintenance_work.go), as DbMaintenance's done lists do.
package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"
)

// The bounds and cadence of VacuumHotTables.
type DbHotVacuumSettings struct {
	// The churn tables, vacuumed in this order, each a plain or partitioned
	// public table (see PARTITION-MAINTENANCE-PLAN.md A1 for the evidence).
	Tables []string
	// The task chain's cadence: a run starts this long after the previous
	// one started.
	Interval time.Duration
	// A run starts no statement once this long has passed since it started,
	// and no statement's timeout reaches past it, so a run ends within
	// Interval.
	RunBudget time.Duration
	// Each VACUUM statement's statement_timeout.
	StatementTimeout time.Duration
	// A table's vacuum adds ANALYZE once its last finished analyze is this
	// old.
	AnalyzeInterval time.Duration
	// A table whose vacuum has not finished for this long, skipped or timed
	// out, raises a warning.
	StaleAfter time.Duration
	// A run that leaves at least this many dead rows not yet removable in one
	// table names the oldest holders of the cleanup horizon; a busy table
	// always has a few from the transactions in flight.
	MinNotRemovableRows int64
	// How often the index pass measures the tables' indexes.
	IndexPassInterval time.Duration
	// An index is rebuilt at this many times its compact estimate, and only
	// with at least IndexMinReclaimBytes to reclaim, so a tiny index is not
	// rebuilt every hour.
	IndexBloatFactor     int64
	IndexMinReclaimBytes int64
	// A rebuild reads its table's heap twice in what is left of the run
	// budget, so an index of a larger heap is left to the daily rotation
	// (DbMaintenance), whose rebuilds have two hours.
	IndexMaxHeapBytes int64
	// The most rebuilds one index pass starts.
	MaxIndexRebuilds int
	// The load guard before each rebuild (dbMaintenanceDeferReason). A
	// rebuild waits out every older snapshot before its swap, so a
	// transaction this old would likely hold it past the run budget.
	RebuildMaxTransactionAge  time.Duration
	RebuildMaxRunningBackends int
	// Bounds a rebuild's wait for its table to clear, and each lock and
	// snapshot wait of the rebuild itself.
	RebuildLockTimeout time.Duration
	// Bounds each lock wait of a debris drop.
	CleanupLockTimeout time.Duration
}

// The A1 list and bounds: compact, each table is under 10 GB, so a pass over
// all of them takes seconds, and a compact table's index rebuild takes
// seconds too.
func DefaultDbHotVacuumSettings() *DbHotVacuumSettings {
	return &DbHotVacuumSettings{
		Tables: []string{
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
		},
		Interval:                  5 * time.Minute,
		RunBudget:                 4 * time.Minute,
		StatementTimeout:          2 * time.Minute,
		AnalyzeInterval:           time.Hour,
		StaleAfter:                time.Hour,
		MinNotRemovableRows:       10000,
		IndexPassInterval:         time.Hour,
		IndexBloatFactor:          2,
		IndexMinReclaimBytes:      64 * 1024 * 1024,
		IndexMaxHeapBytes:         8 * 1024 * 1024 * 1024,
		MaxIndexRebuilds:          3,
		RebuildMaxTransactionAge:  time.Minute,
		RebuildMaxRunningBackends: DefaultDbMaintenanceMaxRunningBackends,
		RebuildLockTimeout:        time.Minute,
		CleanupLockTimeout:        DefaultDbMaintenanceCleanupLockTimeout,
	}
}

// What one run leaves for the next. A chain scheduled fresh starts from the
// zero state.
type DbHotVacuumState struct {
	// The table the next run starts with; an empty or unlisted one starts
	// with the first.
	NextTable string `json:"next_table,omitempty"`
	// Each listed table's times; a table no longer listed is dropped.
	Tables map[string]DbHotVacuumTableState `json:"tables,omitempty"`
	// When the last index pass measured the indexes; zero before the first.
	IndexPassTime time.Time `json:"index_pass_time"`
}

type DbHotVacuumTableState struct {
	// When the chain first listed the table: the stale baseline until a
	// vacuum finishes.
	TrackTime time.Time `json:"track_time"`
	// The last vacuum that finished; zero until one does.
	VacuumTime time.Time `json:"vacuum_time"`
	// The last analyze that finished; zero until one does.
	AnalyzeTime time.Time `json:"analyze_time"`
}

func (self DbHotVacuumState) clone() DbHotVacuumState {
	state := self
	state.Tables = maps.Clone(self.Tables)
	return state
}

// Counts each table's vacuum attempts by outcome, so a table that keeps being
// skipped or timing out shows on a dashboard.
var dbHotVacuumAttemptCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "db",
		Name:      "hot_vacuum_attempts_total",
		Help:      "Vacuums of the churn tables, by table and outcome",
	},
	[]string{"table", "outcome"},
)

// The stale warning's metric: counts each run that found a table with no
// finished vacuum for StaleAfter.
var dbHotVacuumStaleCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "db",
		Name:      "hot_vacuum_stale_total",
		Help:      "Runs of the churn table vacuum that found a table with no finished vacuum for the stale bound, by table",
	},
	[]string{"table"},
)

// Registers the hot vacuum metrics with the default registry.
func init() {
	prometheus.MustRegister(dbHotVacuumAttemptCounter, dbHotVacuumStaleCounter)
}

// Each statement names its table after this. SKIP_LOCKED skips a table
// another session holds rather than queue for it and cancel an autovacuum
// (see the 2026-10-13 note at dbMaintenanceAnalyzeStatement), VERBOSE reports
// what the vacuum removed, and SKIP_DATABASE_STATS leaves the database-wide
// frozen xid update to autovacuum: it scans pg_class and waits for a lock that
// SKIP_LOCKED does not skip, and these small tables rarely hold the oldest
// unfrozen xid.
const dbHotVacuumStatement = `VACUUM (SKIP_LOCKED, VERBOSE, SKIP_DATABASE_STATS) `
const dbHotVacuumAnalyzeStatement = `VACUUM (SKIP_LOCKED, VERBOSE, SKIP_DATABASE_STATS, ANALYZE) `

// How a table's turn in a run ended. The values are metric labels.
type dbHotVacuumOutcome string

const (
	// a summary reported the vacuum finished
	dbHotVacuumVacuumed dbHotVacuumOutcome = "vacuumed"
	// another session held the table, or it was dropped meanwhile
	dbHotVacuumSkipped dbHotVacuumOutcome = "skipped"
	// the statement reached its own statement_timeout
	dbHotVacuumTimedOut dbHotVacuumOutcome = "timed_out"
	// the run budget ended the statement first; the table keeps its turn
	dbHotVacuumOutOfBudget dbHotVacuumOutcome = "out_of_budget"
	// the caller stopped; the table keeps its turn
	dbHotVacuumInterrupted dbHotVacuumOutcome = "interrupted"
	dbHotVacuumFailed      dbHotVacuumOutcome = "failed"
	// the statement completed without a summary for the table
	dbHotVacuumUnreported dbHotVacuumOutcome = "unreported"
)

// What PostgreSQL 18 reported for one VACUUM (VERBOSE) statement, read from
// its notices. Each relation it vacuums (the table, or each leaf of a
// partitioned one, and their toast tables) sends one INFO summary:
//
//	finished vacuuming "<database>.<schema>.<relation>": index scans: <n>
//	pages: ...
//	tuples: <removed> removed, <remaining> remain, <not removable> are dead but not yet removable
//	...
//
// With ANALYZE, each relation it analyzes sends one beginning `finished
// analyzing table "<database>.<schema>.<relation>"`. A relation it skips
// sends a WARNING with lock_not_available, or with undefined_table when it was
// dropped meanwhile; other warnings, such as a refused permission or a
// removable cutoff far in the past, are kept for the log. The summaries are
// matched in English: a server whose lc_messages translates them reports
// none, so every table counts as unfinished and warns after StaleAfter,
// loudly rather than wrongly.
type dbHotVacuumNotices struct {
	// a summary named a relation of the table other than a toast table
	vacuumed bool
	analyzed bool
	// over the table's relations, less their toast tables
	removedRows      int64
	notRemovableRows int64
	// the first skip warning's message
	skipped string
	// the first other warning's message
	warning string
}

var dbHotVacuumSummaryPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^finished vacuuming "(.*)": index scans: [0-9]+$`)
})

var dbHotVacuumTuplesPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^tuples: ([0-9]+) removed, [0-9]+ remain, ([0-9]+) are dead but not yet removable$`)
})

var dbHotVacuumAnalyzePattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^finished analyzing table "(.*)"$`)
})

func parseDbHotVacuumNotices(notices []*pgconn.Notice) dbHotVacuumNotices {
	parsed := dbHotVacuumNotices{}
	for _, notice := range notices {
		if notice.Code == pgerrcode.LockNotAvailable || notice.Code == pgerrcode.UndefinedTable {
			if parsed.skipped == "" {
				parsed.skipped = notice.Message
			}
			continue
		}
		if notice.SeverityUnlocalized == "WARNING" {
			if parsed.warning == "" {
				parsed.warning = notice.Message
			}
			continue
		}
		firstLine, _, _ := strings.Cut(notice.Message, "\n")
		if match := dbHotVacuumSummaryPattern().FindStringSubmatch(firstLine); match != nil {
			// a toast relation is pg_toast.pg_toast_<oid>
			if strings.Contains(match[1], ".pg_toast.") {
				continue
			}
			parsed.vacuumed = true
			if tuples := dbHotVacuumTuplesPattern().FindStringSubmatch(notice.Message); tuples != nil {
				removedRows, _ := strconv.ParseInt(tuples[1], 10, 64)
				notRemovableRows, _ := strconv.ParseInt(tuples[2], 10, 64)
				parsed.removedRows += removedRows
				parsed.notRemovableRows += notRemovableRows
			}
		} else if dbHotVacuumAnalyzePattern().MatchString(firstLine) {
			parsed.analyzed = true
		}
	}
	return parsed
}

// A parent's ANALYZE needs its own completion as well as its leaves' summaries.
func dbHotVacuumRelationReported(notices []*pgconn.Notice, tableName string, pattern *regexp.Regexp) bool {
	for _, notice := range notices {
		firstLine, _, _ := strings.Cut(notice.Message, "\n")
		match := pattern.FindStringSubmatch(firstLine)
		if match != nil && strings.HasSuffix(match[1], ".public."+tableName) {
			return true
		}
	}
	return false
}

// One VACUUM statement: its notices, and how it ended.
type dbHotVacuumAttempt struct {
	notices []*pgconn.Notice
	// The heap or intended leaves, including leaves SKIP_LOCKED omits silently.
	relations []string
	// nil when the statement completed
	err error
}

// Inventory the intended heaps before PostgreSQL expands VACUUM's target.
// Partition expansion can silently omit a locked leaf without a skip notice.
func dbHotVacuumRelationsSql(tableName string) string {
	relation := "'" + strings.ReplaceAll(pgx.Identifier{"public", tableName}.Sanitize(), "'", "''") + "'"
	return fmt.Sprintf(`SELECT current_database() || '.' || n.nspname || '.' || c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind <> 'p' AND (c.oid=to_regclass(%s)
			OR c.oid IN (SELECT relid FROM pg_partition_tree(to_regclass(%s))))
		ORDER BY c.oid`, relation, relation)
}

// A summary from one leaf cannot stand in for another intended heap.
func dbHotVacuumMissingRelations(notices []*pgconn.Notice, relations []string, pattern *regexp.Regexp) []string {
	finished := map[string]bool{}
	for _, notice := range notices {
		firstLine, _, _ := strings.Cut(notice.Message, "\n")
		if match := pattern.FindStringSubmatch(firstLine); match != nil {
			finished[match[1]] = true
		}
	}
	missing := []string{}
	for _, relation := range relations {
		if !finished[relation] {
			missing = append(missing, relation)
		}
	}
	return missing
}

// Runs one table's VACUUM (VERBOSE) under timeout on a direct session of
// pool, collecting its notices. The session's statement_timeout is reset
// before it goes back to the pool, also when the caller has stopped; a session
// that cannot be reset, or that the stop closed, is discarded instead.
func dbHotVacuumTable(ctx context.Context, pool *safePgPool, tableName string, analyze bool, timeout time.Duration) *dbHotVacuumAttempt {
	attempt := &dbHotVacuumAttempt{}
	// The inventory and VACUUM share the table's budget.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := acquirePgPoolConn(ctx, pool)
	if err != nil {
		attempt.err = err
		return attempt
	}
	reset := false
	defer func() {
		if reset {
			conn.Release()
		} else {
			discardPgConnection(ctx, conn)
		}
	}()

	if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", timeout.Milliseconds())); err != nil {
		attempt.err = err
		return attempt
	}
	rows, err := conn.Query(ctx, dbHotVacuumRelationsSql(tableName))
	if err != nil {
		attempt.err = err
		return attempt
	}
	for rows.Next() {
		var relation string
		if err := rows.Scan(&relation); err != nil {
			rows.Close()
			attempt.err = err
			return attempt
		}
		attempt.relations = append(attempt.relations, relation)
	}
	rows.Close()
	if attempt.err = rows.Err(); attempt.err != nil {
		return attempt
	}
	statement := dbHotVacuumStatement
	if analyze {
		statement = dbHotVacuumAnalyzeStatement
	}
	withPgNotices(conn.Conn().PgConn(), func(notice *pgconn.Notice) {
		attempt.notices = append(attempt.notices, notice)
	}, func() {
		_, attempt.err = conn.Exec(ctx, statement+pgx.Identifier{"public", tableName}.Sanitize())
	})
	if conn.Conn().IsClosed() {
		return attempt
	}
	resetCtx, resetCancel := context.WithTimeout(context.WithoutCancel(ctx), PgCloseTimeout)
	defer resetCancel()
	_, err = conn.Exec(resetCtx, `RESET statement_timeout`)
	reset = err == nil
	return attempt
}

// A session, replication slot or prepared transaction holding the cleanup
// horizon back, named only by its process id, application name, state and
// ages; its query and client address are never read.
type dbHorizonHolder struct {
	// session, replication slot xmin, replication slot catalog_xmin or
	// prepared transaction
	kind string
	// zero for a prepared transaction or an inactive slot
	pid             int32
	applicationName string
	state           string
	// transactions since the xid it holds
	xidAge int64
	// how long its transaction has been open; zero when unknown
	age time.Duration
}

func (self *dbHorizonHolder) String() string {
	open := "unknown"
	if 0 < self.age {
		open = self.age.Round(time.Second).String()
	}
	return fmt.Sprintf(
		"%s pid %d (application %q, state %q, %d xids old, open %s)",
		self.kind,
		self.pid,
		self.applicationName,
		self.state,
		self.xidAge,
		open,
	)
}

// The oldest holder of each kind that keeps dead rows of this database's
// tables from being removed: a session of this database by its backend_xmin
// or backend_xid (a walsender's hot standby feedback included), a replication
// slot by its xmin or its catalog_xmin, which holds back only the catalogs,
// and a prepared transaction. A session that is itself vacuuming is skipped:
// a vacuum's snapshot holds back no other vacuum.
const dbHorizonHoldersQuery = `
	WITH holder AS (
		SELECT
			'session' AS kind,
			activity.pid,
			coalesce(activity.application_name, '') AS application_name,
			coalesce(activity.state, '') AS state,
			greatest(coalesce(age(activity.backend_xmin), 0), coalesce(age(activity.backend_xid), 0))::bigint AS xid_age,
			coalesce(extract(epoch FROM now() - activity.xact_start), 0)::float8 AS age_seconds
		FROM pg_stat_activity activity
		WHERE activity.pid <> pg_backend_pid()
		  AND (activity.datname = current_database() OR activity.datname IS NULL)
		  AND (activity.backend_xmin IS NOT NULL OR activity.backend_xid IS NOT NULL)
		  AND NOT EXISTS (
			SELECT 1 FROM pg_stat_progress_vacuum progress WHERE progress.pid = activity.pid
		  )
		UNION ALL
		SELECT
			'replication slot xmin',
			coalesce(slot.active_pid, 0),
			'',
			CASE WHEN slot.active THEN 'active' ELSE 'inactive' END,
			age(slot.xmin)::bigint,
			0::float8
		FROM pg_replication_slots slot
		WHERE slot.xmin IS NOT NULL
		UNION ALL
		SELECT
			'replication slot catalog_xmin',
			coalesce(slot.active_pid, 0),
			'',
			CASE WHEN slot.active THEN 'active' ELSE 'inactive' END,
			age(slot.catalog_xmin)::bigint,
			0::float8
		FROM pg_replication_slots slot
		WHERE slot.catalog_xmin IS NOT NULL
		UNION ALL
		SELECT
			'prepared transaction',
			0,
			'',
			'prepared',
			age(prepared.transaction)::bigint,
			coalesce(extract(epoch FROM now() - prepared.prepared), 0)::float8
		FROM pg_prepared_xacts prepared
		WHERE prepared.database = current_database()
	)
	SELECT DISTINCT ON (holder.kind)
		holder.kind,
		holder.pid,
		holder.application_name,
		holder.state,
		holder.xid_age,
		holder.age_seconds
	FROM holder
	ORDER BY holder.kind, holder.xid_age DESC, holder.age_seconds DESC
`

// The oldest holder of each kind (see dbHorizonHoldersQuery), oldest first.
func readDbHorizonHolders(ctx context.Context, pool *safePgPool) ([]dbHorizonHolder, error) {
	conn, err := acquirePgPoolConn(ctx, pool)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, dbHorizonHoldersQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	holders := []dbHorizonHolder{}
	for rows.Next() {
		var holder dbHorizonHolder
		var ageSeconds float64
		if err := rows.Scan(
			&holder.kind,
			&holder.pid,
			&holder.applicationName,
			&holder.state,
			&holder.xidAge,
			&ageSeconds,
		); err != nil {
			return nil, err
		}
		holder.age = time.Duration(ageSeconds * float64(time.Second))
		holders = append(holders, holder)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(holders, func(a dbHorizonHolder, b dbHorizonHolder) int {
		return cmp.Or(cmp.Compare(b.xidAge, a.xidAge), cmp.Compare(b.age, a.age), strings.Compare(a.kind, b.kind))
	})
	return holders, nil
}

// One table's turn in a run.
type dbHotVacuumTableResult struct {
	tableName string
	outcome   dbHotVacuumOutcome
	// the statement included ANALYZE, and the analyze finished
	analyze  bool
	analyzed bool
	// from the summary
	removedRows      int64
	notRemovableRows int64
	// a skip's warning or a failure's error
	detail   string
	duration time.Duration
}

func (self *dbHotVacuumTableResult) String() string {
	text := self.tableName + " " + string(self.outcome)
	if self.outcome == dbHotVacuumVacuumed {
		text += fmt.Sprintf(" (%d removed, %d not yet removable", self.removedRows, self.notRemovableRows)
		if self.analyzed {
			text += ", analyzed"
		} else if self.analyze {
			text += ", analyze unfinished"
		}
		text += ")"
	}
	if self.detail != "" {
		text += ": " + self.detail
	}
	return text + fmt.Sprintf(" in %.2fs", self.duration.Seconds())
}

// A table with no finished vacuum for StaleAfter.
type dbHotVacuumStaleTable struct {
	tableName string
	// its last finished vacuum, or when the chain began tracking it
	since time.Time
}

// How an index pass's rebuild of one index ended.
type dbHotIndexOutcome string

const (
	dbHotIndexRebuilt dbHotIndexOutcome = "rebuilt"
	// not started: its table was busy or kept debris
	dbHotIndexSkipped dbHotIndexOutcome = "skipped"
	dbHotIndexFailed  dbHotIndexOutcome = "failed"
)

// One bloated index the pass selected, and what its rebuild did.
type dbHotIndexRebuild struct {
	index   dbMaintenancePriorityIndex
	outcome dbHotIndexOutcome
	detail  string
	// the index's size after its rebuild; zero when unmeasured
	bytesAfter int64
}

func (self *dbHotIndexRebuild) String() string {
	text := fmt.Sprintf(
		"%s on %s %s (%d bytes, compact estimate %d bytes",
		self.index.indexName,
		self.index.tableName,
		self.outcome,
		self.index.indexBytes,
		self.index.compactBytes,
	)
	if 0 < self.bytesAfter {
		text += fmt.Sprintf(", now %d bytes", self.bytesAfter)
	}
	text += ")"
	if self.detail != "" {
		text += ": " + self.detail
	}
	return text
}

type dbHotIndexPassReport struct {
	measuredCount int
	bloatedCount  int
	// set when the measurement failed
	err error
	// why the pass started no rebuild: the load guard refused it
	deferred string
	rebuilds []dbHotIndexRebuild
	// why the pass stopped before its last selected index
	stopped string
}

// What one run did, for its log.
type dbHotVacuumReport struct {
	tables []dbHotVacuumTableResult
	// why the run stopped before every table had its turn; empty when all did
	stopped     string
	staleTables []dbHotVacuumStaleTable
	// the horizon was read, once a table left MinNotRemovableRows
	horizonRead    bool
	horizonHolders []dbHorizonHolder
	horizonErr     error
	// set when an index pass ran
	indexPass *dbHotIndexPassReport
	elapsed   time.Duration
}

// One run over the state the previous run left. Not safe for concurrent use;
// the task's run-once key keeps one run active at a time.
type dbHotVacuumRun struct {
	settings *DbHotVacuumSettings
	// the pool of the vacuums and the horizon read: the maintenance pool
	pool *safePgPool
	// the clock of the run budget and of the state's times
	now      func() time.Time
	state    DbHotVacuumState
	report   dbHotVacuumReport
	deadline time.Time
}

func newDbHotVacuumRun(settings *DbHotVacuumSettings, pool *safePgPool, now func() time.Time, state DbHotVacuumState) *dbHotVacuumRun {
	return &dbHotVacuumRun{
		settings: settings,
		pool:     pool,
		now:      now,
		state:    state.clone(),
	}
}

// Runs one run of the churn tables' maintenance (see the file comment) from
// the state the previous run left, and returns the state for the next run.
func VacuumHotTables(ctx context.Context, state DbHotVacuumState, settings *DbHotVacuumSettings) DbHotVacuumState {
	run := newDbHotVacuumRun(settings, safeMaintenancePool, time.Now, state)
	run.run(ctx)
	return run.state
}

func (self *dbHotVacuumRun) run(ctx context.Context) {
	startTime := self.now()
	self.deadline = startTime.Add(self.settings.RunBudget)
	self.vacuumTables(ctx)
	self.readHorizon(ctx)
	self.findStaleTables()
	if self.report.stopped == "" && self.indexPassDue(ctx) {
		self.rebuildBloatedIndexes(ctx)
	}
	self.report.elapsed = self.now().Sub(startTime)
	self.log()
}

// Gives each table one turn, starting where the previous run stopped, until
// every table had its turn, the budget is spent, or ctx ends. A stop leaves
// the next run to start with the table that did not finish its turn.
func (self *dbHotVacuumRun) vacuumTables(ctx context.Context) {
	tableNames := self.settings.Tables
	if len(tableNames) == 0 {
		return
	}
	trackTime := self.now().UTC()
	tables := map[string]DbHotVacuumTableState{}
	for _, tableName := range tableNames {
		tableState, ok := self.state.Tables[tableName]
		if !ok {
			tableState.TrackTime = trackTime
		}
		tables[tableName] = tableState
	}
	self.state.Tables = tables

	first := max(slices.Index(tableNames, self.state.NextTable), 0)
	// a run that gives every table its turn keeps its start
	self.state.NextTable = tableNames[first]
	for i := range len(tableNames) {
		tableName := tableNames[(first+i)%len(tableNames)]
		stopped := ""
		remaining := self.deadline.Sub(self.now())
		select {
		case <-ctx.Done():
			stopped = fmt.Sprintf("%s: %s", dbHotVacuumInterrupted, context.Cause(ctx))
		default:
			if remaining <= 0 {
				stopped = "the run budget is spent"
			}
		}
		if stopped == "" {
			result := self.vacuumTable(ctx, tableName, remaining)
			self.report.tables = append(self.report.tables, result)
			dbHotVacuumAttemptCounter.WithLabelValues(tableName, string(result.outcome)).Inc()
			if result.outcome == dbHotVacuumInterrupted || result.outcome == dbHotVacuumOutOfBudget {
				stopped = string(result.outcome)
			}
		}
		if stopped != "" {
			self.report.stopped = stopped
			self.state.NextTable = tableName
			return
		}
	}
}

// One table's turn: its vacuum, with ANALYZE when the last one is
// AnalyzeInterval old, under the smaller of StatementTimeout and the
// remaining budget.
func (self *dbHotVacuumRun) vacuumTable(ctx context.Context, tableName string, remaining time.Duration) dbHotVacuumTableResult {
	tableState := self.state.Tables[tableName]
	result := dbHotVacuumTableResult{
		tableName: tableName,
		analyze:   self.settings.AnalyzeInterval <= self.now().Sub(tableState.AnalyzeTime),
	}
	timeout := min(self.settings.StatementTimeout, remaining)
	// a statement_timeout of 0 would disable the bound
	timeout = max(timeout, time.Millisecond)
	startTime := self.now()
	attempt := dbHotVacuumTable(ctx, self.pool, tableName, result.analyze, timeout)
	endTime := self.now()
	result.duration = endTime.Sub(startTime)

	notices := parseDbHotVacuumNotices(attempt.notices)
	result.removedRows = notices.removedRows
	result.notRemovableRows = notices.notRemovableRows
	missing := dbHotVacuumMissingRelations(attempt.notices, attempt.relations, dbHotVacuumSummaryPattern())
	vacuumed := len(attempt.relations) > 0 && len(missing) == 0
	result.analyzed = result.analyze && vacuumed &&
		len(dbHotVacuumMissingRelations(attempt.notices, attempt.relations, dbHotVacuumAnalyzePattern())) == 0 &&
		dbHotVacuumRelationReported(attempt.notices, tableName, dbHotVacuumAnalyzePattern())
	var pgErr *pgconn.PgError
	canceled := errors.As(attempt.err, &pgErr) && pgErr.Code == pgerrcode.QueryCanceled || errors.Is(attempt.err, context.DeadlineExceeded)
	done := false
	select {
	case <-ctx.Done():
		done = true
	default:
	}
	switch {
	case vacuumed:
		// an analyze that failed after it is retried by the next run
		result.outcome = dbHotVacuumVacuumed
	case attempt.err == nil && (notices.skipped != "" || notices.vacuumed && len(missing) > 0):
		result.outcome = dbHotVacuumSkipped
	case attempt.err == nil:
		result.outcome = dbHotVacuumUnreported
	case done:
		result.outcome = dbHotVacuumInterrupted
	case canceled && timeout < self.settings.StatementTimeout:
		result.outcome = dbHotVacuumOutOfBudget
	case canceled:
		result.outcome = dbHotVacuumTimedOut
	default:
		result.outcome = dbHotVacuumFailed
	}
	details := []string{}
	if notices.vacuumed && len(missing) > 0 {
		details = append(details, "vacuum unfinished: "+strings.Join(missing, ", "))
	}
	if notices.skipped != "" {
		details = append(details, notices.skipped)
	}
	if notices.warning != "" {
		details = append(details, notices.warning)
	}
	if attempt.err != nil && result.outcome != dbHotVacuumInterrupted {
		details = append(details, attempt.err.Error())
	}
	if result.detail == "" {
		result.detail = strings.Join(details, "; ")
	}

	if result.outcome == dbHotVacuumVacuumed {
		tableState.VacuumTime = endTime.UTC()
		if result.analyzed {
			tableState.AnalyzeTime = endTime.UTC()
		}
		self.state.Tables[tableName] = tableState
	}
	return result
}

// Reads the oldest holders of the cleanup horizon once a table this run left
// at least MinNotRemovableRows dead rows that could not be removed yet: rows
// an old snapshot can still see, which no vacuum removes before it ends.
func (self *dbHotVacuumRun) readHorizon(ctx context.Context) {
	pinned := false
	for _, result := range self.report.tables {
		if self.settings.MinNotRemovableRows <= result.notRemovableRows {
			pinned = true
		}
	}
	if !pinned {
		return
	}
	select {
	case <-ctx.Done():
		return
	default:
	}
	self.report.horizonRead = true
	self.report.horizonHolders, self.report.horizonErr = readDbHorizonHolders(ctx, self.pool)
}

// Records each table with no finished vacuum for StaleAfter, counting its
// warning.
func (self *dbHotVacuumRun) findStaleTables() {
	now := self.now()
	for _, tableName := range self.settings.Tables {
		tableState := self.state.Tables[tableName]
		since := tableState.TrackTime
		if since.Before(tableState.VacuumTime) {
			since = tableState.VacuumTime
		}
		if self.settings.StaleAfter <= now.Sub(since) {
			self.report.staleTables = append(self.report.staleTables, dbHotVacuumStaleTable{
				tableName: tableName,
				since:     since,
			})
			dbHotVacuumStaleCounter.WithLabelValues(tableName).Inc()
		}
	}
}

// Whether the index pass runs in this run: its last pass is IndexPassInterval
// old, and budget is left.
func (self *dbHotVacuumRun) indexPassDue(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	default:
	}
	now := self.now()
	return self.settings.IndexPassInterval <= now.Sub(self.state.IndexPassTime) && now.Before(self.deadline)
}

// Whether the index pass rebuilds the index (see IndexBloatFactor,
// IndexMinReclaimBytes and IndexMaxHeapBytes). An index without a compact
// estimate is never rebuilt, and a priority index keeps its opt-in
// (DbMaintenancePriorityReindexEnabled).
func (self *DbHotVacuumSettings) indexBloated(index *dbMaintenancePriorityIndex) bool {
	if index.compactBytes <= 0 || IsDbMaintenancePriorityIndex(index.indexName) {
		return false
	}
	return self.IndexBloatFactor*index.compactBytes <= index.indexBytes &&
		self.IndexMinReclaimBytes <= index.indexBytes-index.compactBytes &&
		index.heapBytes <= self.IndexMaxHeapBytes
}

// The bloated indexes, most reclaimable bytes first and names breaking ties,
// at most MaxIndexRebuilds of them, and how many were bloated.
func (self *DbHotVacuumSettings) selectBloatedIndexes(indexes []dbMaintenancePriorityIndex) (selectedIndexes []dbMaintenancePriorityIndex, bloatedCount int) {
	selectedIndexes = []dbMaintenancePriorityIndex{}
	for _, index := range indexes {
		if self.indexBloated(&index) {
			selectedIndexes = append(selectedIndexes, index)
		}
	}
	slices.SortFunc(selectedIndexes, func(a dbMaintenancePriorityIndex, b dbMaintenancePriorityIndex) int {
		return cmp.Or(
			cmp.Compare(b.indexBytes-b.compactBytes, a.indexBytes-a.compactBytes),
			strings.Compare(a.indexName, b.indexName),
		)
	})
	bloatedCount = len(selectedIndexes)
	if self.MaxIndexRebuilds < len(selectedIndexes) {
		selectedIndexes = selectedIndexes[:max(self.MaxIndexRebuilds, 0)]
	}
	return selectedIndexes, bloatedCount
}

// Measures every index of the tables and rebuilds the bloated ones in what is
// left of the run budget, each behind the load guard. The pass counts, and the
// next waits IndexPassInterval, once it found nothing bloated or got past the
// load guard; a pass the guard deferred is tried again by the next run.
func (self *dbHotVacuumRun) rebuildBloatedIndexes(ctx context.Context) {
	pass := &dbHotIndexPassReport{}
	self.report.indexPass = pass
	measuredIndexes, err := dbMaintenanceTableIndexSizes(ctx, self.settings.Tables)
	if err != nil {
		pass.err = err
		return
	}
	pass.measuredCount = len(measuredIndexes)
	selectedIndexes, bloatedCount := self.settings.selectBloatedIndexes(measuredIndexes)
	pass.bloatedCount = bloatedCount
	if len(selectedIndexes) == 0 {
		self.state.IndexPassTime = self.now().UTC()
		return
	}

	// the reason a rebuild must not start now, or ""
	deferReason := func() string {
		load, err := dbMaintenanceSampleLoad(ctx)
		if err != nil {
			return fmt.Sprintf("load sample failed: %s", err)
		}
		return dbMaintenanceDeferReason(load, self.settings.RebuildMaxTransactionAge, self.settings.RebuildMaxRunningBackends)
	}
	if reason := deferReason(); reason != "" {
		pass.deferred = reason
		return
	}
	self.state.IndexPassTime = self.now().UTC()
	for i, index := range selectedIndexes {
		remaining := self.deadline.Sub(self.now())
		select {
		case <-ctx.Done():
			pass.stopped = fmt.Sprintf("%s: %s", dbHotVacuumInterrupted, context.Cause(ctx))
		default:
			if remaining <= 0 {
				pass.stopped = "the run budget is spent"
			} else if 0 < i {
				pass.stopped = deferReason()
			}
		}
		if pass.stopped != "" {
			return
		}
		pass.rebuilds = append(pass.rebuilds, self.rebuildIndex(ctx, index, remaining))
	}
}

// Rebuilds one bloated index as the rotation rebuilds a priority index: not
// while its table is vacuumed, analyzed or indexed, not while debris of an
// earlier rebuild remains on it, without queuing for its table's lock (see
// dbMaintenanceRunReindex), within remaining, and with its debris dropped
// after it, on a detached context after a failure.
func (self *dbHotVacuumRun) rebuildIndex(ctx context.Context, index dbMaintenancePriorityIndex, remaining time.Duration) dbHotIndexRebuild {
	rebuild := dbHotIndexRebuild{
		index:   index,
		outcome: dbHotIndexSkipped,
	}
	if locked, err := dbMaintenanceTableLocked(ctx, index.tableName); err != nil || locked {
		rebuild.detail = fmt.Sprintf("its table %s is being vacuumed, analyzed or indexed (err %v)", index.tableName, err)
		return rebuild
	}
	if remainingDebris, err := dbMaintenanceDropReindexDebris(ctx, index.tableName, self.settings.CleanupLockTimeout); err != nil || 0 < remainingDebris {
		rebuild.detail = fmt.Sprintf("%d incomplete index(es) remain on %s (err %v)", remainingDebris, index.tableName, err)
		return rebuild
	}

	glog.Infof(
		"[db]hot vacuum index pass rebuilding %s on %s (%d bytes, compact estimate %d bytes)\n",
		index.indexName,
		index.tableName,
		index.indexBytes,
		index.compactBytes,
	)
	rebuildCtx, rebuildCancel := context.WithTimeout(ctx, remaining)
	defer rebuildCancel()
	statement := `REINDEX INDEX CONCURRENTLY ` + pgx.Identifier{"public", index.indexName}.Sanitize()
	pid, err := dbMaintenanceRunReindex(rebuildCtx, index.tableName, statement, self.settings.RebuildLockTimeout)
	var lockConflict *dbMaintenanceLockConflict
	if errors.As(err, &lockConflict) {
		// nothing started, so nothing to clean up after
		rebuild.detail = lockConflict.reason
		return rebuild
	}
	if err != nil {
		rebuild.outcome = dbHotIndexFailed
		rebuild.detail = err.Error()
		remainingDebris, cleanupErr := dbMaintenanceCleanupAfterFailedReindex(ctx, index.tableName, pid, self.settings.CleanupLockTimeout)
		if cleanupErr != nil || 0 < remainingDebris {
			rebuild.detail += fmt.Sprintf("; %d incomplete index(es) left for a later cleanup (err %v)", remainingDebris, cleanupErr)
		}
		return rebuild
	}
	rebuild.outcome = dbHotIndexRebuilt
	if remainingDebris, err := dbMaintenanceDropReindexDebris(ctx, index.tableName, self.settings.CleanupLockTimeout); err != nil || 0 < remainingDebris {
		rebuild.detail = fmt.Sprintf("%d incomplete index(es) left for a later cleanup (err %v)", remainingDebris, err)
	}
	if rebuiltIndexes, err := dbMaintenancePriorityIndexSizes(ctx, []string{index.indexName}); err == nil && len(rebuiltIndexes) == 1 {
		rebuild.bytesAfter = rebuiltIndexes[0].indexBytes
	}
	return rebuild
}

// One line for the run, a warning per stale table, the horizon holders when
// they were read, and one line for an index pass.
func (self *dbHotVacuumRun) log() {
	tableTexts := []string{}
	for _, result := range self.report.tables {
		tableTexts = append(tableTexts, result.String())
	}
	stopped := ""
	if self.report.stopped != "" {
		stopped = fmt.Sprintf(", stopped before %s (%s)", self.state.NextTable, self.report.stopped)
	}
	glog.Infof(
		"[db]hot vacuum %d/%d table(s) in %.2fs%s: %s\n",
		len(self.report.tables),
		len(self.settings.Tables),
		self.report.elapsed.Seconds(),
		stopped,
		strings.Join(tableTexts, "; "),
	)

	now := self.now()
	for _, staleTable := range self.report.staleTables {
		lastTurn := "no turn this run"
		for _, result := range self.report.tables {
			if result.tableName == staleTable.tableName {
				lastTurn = "this run: " + result.String()
			}
		}
		glog.Warningf(
			"[db]hot vacuum %s has had no finished vacuum for %s (%s)\n",
			staleTable.tableName,
			now.Sub(staleTable.since).Round(time.Second),
			lastTurn,
		)
	}

	if self.report.horizonRead {
		pinnedTexts := []string{}
		for _, result := range self.report.tables {
			if self.settings.MinNotRemovableRows <= result.notRemovableRows {
				pinnedTexts = append(pinnedTexts, fmt.Sprintf("%s %d", result.tableName, result.notRemovableRows))
			}
		}
		holderTexts := []string{}
		for _, holder := range self.report.horizonHolders {
			holderTexts = append(holderTexts, holder.String())
		}
		holders := strings.Join(holderTexts, "; ")
		if self.report.horizonErr != nil {
			holders = fmt.Sprintf("unavailable: %s", self.report.horizonErr)
		} else if len(holderTexts) == 0 {
			holders = "none visible"
		}
		glog.Infof(
			"[db]hot vacuum dead rows not yet removable (%s): the horizon holds them, the oldest holders: %s\n",
			strings.Join(pinnedTexts, ", "),
			holders,
		)
	}

	if pass := self.report.indexPass; pass != nil {
		switch {
		case pass.err != nil:
			glog.Infof("[db]hot vacuum index pass could not measure the indexes: %s\n", pass.err)
		case pass.deferred != "":
			glog.Infof("[db]hot vacuum index pass deferred with %d bloated index(es): %s\n", pass.bloatedCount, pass.deferred)
		default:
			rebuildTexts := []string{}
			for _, rebuild := range pass.rebuilds {
				rebuildTexts = append(rebuildTexts, rebuild.String())
			}
			if pass.stopped != "" {
				rebuildTexts = append(rebuildTexts, "stopped: "+pass.stopped)
			}
			glog.Infof(
				"[db]hot vacuum index pass measured %d index(es), %d bloated: %s\n",
				pass.measuredCount,
				pass.bloatedCount,
				strings.Join(rebuildTexts, "; "),
			)
		}
	}
}
