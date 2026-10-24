// Weekly bloat measurement (PARTITION-MAINTENANCE-PLAN.md A4): how much of each
// relation of every public table a rewrite would return. The raw measurements
// stay in db_bloat_measurement for Retention, so the monitor's bloat signal
// (SIGNALS.md §2.4k) reads them and its thresholds can be checked against them.
// Nothing here compacts: online compaction waits for PostgreSQL 19 (owner
// decision 2026-10-24).
//
// A week's first run records the catalog pass, which reads only the catalog:
// every index of a table and of its TOAST table against the btree estimate the
// priority rebuild uses (dbMaintenanceEstimateBtreeBytes), and every heap and
// TOAST relation that is not scanned against the statistics estimate (live rows
// times average width, packed at the fillfactor). Then each run scans, table by
// table in name order from the cursor, every heap and TOAST relation under
// MaxScanRelationBytes with pgstattuple_approx. A scan covers only the relation
// it is given, and it skips all-visible pages but reads every other page, so a
// heavily updated table still costs a scan. The scans share a weekly budget: a
// run that spends it leaves the cursor at its table, and the next week's runs
// continue there. Each scan records the share of its relation it read.
//
// Reclaimable bytes leave out the free space a fillfactor keeps on purpose: a
// rewrite at fillfactor F leaves (100 - F)% of each page free, so a scan's free
// space counts only beyond its live bytes times (100 - F) / F, and both
// estimates pack their rows at F.
//
// pgstattuple_approx needs the pgstattuple extension and EXECUTE on it, which
// the extension grants to pg_stat_scan_tables (a superuser passes too). Without
// either, or when a scan fails or outlasts MaxScanDuration, the relation gets
// the statistics estimate, and its row records why.
//
// Each scan runs on a direct maintenance session of its own, exempt from
// transaction_timeout (WithoutTransactionTimeout) and bounded instead by its
// own statement timeout, after the load check of the REINDEX rotation
// (dbMaintenanceDeferReason). A scan holds a snapshot and an AccessShareLock on
// its relation while it runs, so its statement bound also bounds the cleanup
// horizon it holds and how long a DDL statement can wait behind it.
package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/glog"
)

// The table of raw measurements, created by a migration (db_migrations.go):
// never edit it, since a migration's identity is a hash of its SQL. One row per
// measured relation and measurement; reclaimable_bytes is null when the
// relation could not be estimated. relation_schema and relation_name name the
// measured relation (public for a heap or a table's index, pg_toast for a TOAST
// relation and its index), and table_name the public table it belongs to.
const dbBloatMeasurementTableSql = `
	CREATE TABLE IF NOT EXISTS db_bloat_measurement (
		measurement_id uuid NOT NULL,
		measure_time timestamp NOT NULL,
		table_name varchar(64) NOT NULL,
		relation_schema varchar(64) NOT NULL,
		relation_name varchar(64) NOT NULL,
		relation_kind varchar(8) NOT NULL,
		method varchar(32) NOT NULL,
		fallback_reason varchar(64) NULL,
		relation_bytes bigint NOT NULL,
		reclaimable_bytes bigint NULL,
		compact_bytes bigint NULL,
		fillfactor integer NOT NULL,
		reserved_bytes bigint NULL,
		scanned_percent double precision NULL,
		live_tuple_count bigint NULL,
		live_tuple_bytes bigint NULL,
		dead_tuple_count bigint NULL,
		dead_tuple_bytes bigint NULL,
		free_bytes bigint NULL,
		estimated_row_count double precision NULL,
		live_row_count bigint NULL,
		dead_row_count bigint NULL,
		scan_duration_ms bigint NULL,
		PRIMARY KEY (measurement_id)
	);

	CREATE INDEX IF NOT EXISTS db_bloat_measurement_measure_time
	ON db_bloat_measurement (measure_time);
`

// The measured relation's kind.
const (
	dbBloatRelationHeap  = "heap"
	dbBloatRelationToast = "toast"
	dbBloatRelationIndex = "index"
)

// How a relation was measured.
const (
	dbBloatMethodScan          = "pgstattuple_approx"
	dbBloatMethodStatistics    = "statistics"
	dbBloatMethodBtreeEstimate = "btree_estimate"
	// the relation could not be estimated; its size is recorded alone
	dbBloatMethodNone = "none"
)

// Why a heap or TOAST relation got the statistics estimate in place of a scan.
const (
	dbBloatFallbackNotInstalled    = "pgstattuple-not-installed"
	dbBloatFallbackNotOnSearchPath = "pgstattuple-not-on-search-path"
	dbBloatFallbackNoExecute       = "pgstattuple-no-execute"
	dbBloatFallbackRefused         = "pgstattuple-refused"
	dbBloatFallbackOverScanSize    = "over-scan-size"
	dbBloatFallbackScanTimeout     = "scan-timeout"
	dbBloatFallbackScanLockTimeout = "scan-lock-timeout"
	dbBloatFallbackScanFailed      = "scan-failed"
)

// Why a relation could not be estimated (method none).
const (
	// a variable-width column has no statistics, or the row count is unknown
	dbBloatUnmeasuredNotAnalyzed = "not-analyzed"
	// a TOAST relation's chunk count is unknown until its first vacuum
	dbBloatUnmeasuredNotVacuumed = "not-vacuumed"
	dbBloatUnmeasuredNotBtree    = "not-btree"
)

// PostgreSQL's TOAST_TUPLES_PER_PAGE: TOAST sizes its chunks so that this many
// fill a page.
const dbBloatToastChunksPerPage = 4

// The tunables of the weekly bloat measurement. Sizes are decimal bytes, as
// SIGNALS.md §5.14 measures them, and every duration is positive.
type DbBloatMeasureSettings struct {
	// The weekly slot in UTC: a weekday, and an offset under 24 hours from its
	// midnight. A run that finishes its week's work schedules the next run at
	// the next slot.
	Weekday   time.Weekday
	TimeOfDay time.Duration
	// A chain that startup seeds, having no task, runs this long after.
	FirstRunDelay time.Duration
	// The next run after a load deferral or a failed record, and after an
	// interruption such as a drain; both continue the same week.
	DeferredRetryDelay    time.Duration
	InterruptedRetryDelay time.Duration
	// The task's run bound: the scan budget, the catalog pass, and the last scan
	// statement started before the budget ran out.
	MaxRunTime time.Duration
	// The scan time all runs of a week may spend, and the bound of one scan
	// statement, at most ScanBudget. A relation whose scan outlasts
	// MaxScanDuration gets the statistics estimate; the bound also bounds the
	// snapshot a scan holds.
	ScanBudget      time.Duration
	MaxScanDuration time.Duration
	// Bounds a scan's wait for its relation's lock.
	ScanLockTimeout time.Duration
	// Heap and TOAST relations at least this large get the statistics estimate
	// in place of a scan.
	MaxScanRelationBytes int64
	// Relations under this size are not measured: they hold nothing worth
	// reclaiming.
	MinRelationBytes int64
	// The load check before each scan (dbMaintenanceDeferReason): another index
	// build, a transaction at least this old, or this many backends on cpu
	// defer the run. A scan waits for no snapshot, so the age bound is the
	// priority rebuild's hour, not the rotation's ten minutes.
	MaxTransactionAge  time.Duration
	MaxRunningBackends int
	// Measurements older than this are deleted, at most this many a statement.
	Retention          time.Duration
	RetentionBatchSize int
	// How many of the relations with the most reclaimable bytes a run logs.
	LogLargestCount int
}

// Sunday 12:00 UTC keeps the scans away from the daily rotation's 03:00 and
// 07:00 slots. The hour is meant to cover main's tables under 50 GB, whose
// all-visible pages a scan skips, in one week; the cursor carries what it does
// not cover to the next. Ten minutes per scan stays under the 15-minute horizon
// warning of A6.
func DefaultDbBloatMeasureSettings() *DbBloatMeasureSettings {
	return &DbBloatMeasureSettings{
		Weekday:               time.Sunday,
		TimeOfDay:             12 * time.Hour,
		FirstRunDelay:         10 * time.Minute,
		DeferredRetryDelay:    time.Hour,
		InterruptedRetryDelay: 5 * time.Minute,
		MaxRunTime:            2 * time.Hour,
		ScanBudget:            time.Hour,
		MaxScanDuration:       10 * time.Minute,
		ScanLockTimeout:       10 * time.Second,
		MaxScanRelationBytes:  50 * 1000 * 1000 * 1000,
		MinRelationBytes:      1024 * 1024,
		MaxTransactionAge:     DefaultDbMaintenancePriorityMaxTransactionAge,
		MaxRunningBackends:    DefaultDbMaintenanceMaxRunningBackends,
		Retention:             90 * 24 * time.Hour,
		RetentionBatchSize:    10000,
		LogLargestCount:       5,
	}
}

// The start of the week now falls in: the latest slot at or before now.
func (self *DbBloatMeasureSettings) WeekStart(now time.Time) time.Time {
	now = now.UTC()
	year, month, day := now.Date()
	daysSinceWeekday := (int(now.Weekday()) - int(self.Weekday) + 7) % 7
	slot := time.Date(year, month, day-daysSinceWeekday, 0, 0, 0, 0, time.UTC).Add(self.TimeOfDay)
	if now.Before(slot) {
		slot = slot.AddDate(0, 0, -7)
	}
	return slot
}

// The start of the week after the one now falls in.
func (self *DbBloatMeasureSettings) NextWeekStart(now time.Time) time.Time {
	return self.WeekStart(now).AddDate(0, 0, 7)
}

// Why a heap or TOAST relation of relationBytes gets the statistics estimate in
// place of a scan, or "" when it is scanned. scanUnavailable is why
// pgstattuple_approx cannot scan for this run, or "".
func (self *DbBloatMeasureSettings) scanFallbackReason(relationBytes int64, scanUnavailable string) string {
	if scanUnavailable != "" {
		return scanUnavailable
	}
	if self.MaxScanRelationBytes <= relationBytes {
		return dbBloatFallbackOverScanSize
	}
	return ""
}

// What a week's runs carry between them in the task's arguments. Arguments
// written before a field existed decode to its zero value.
type DbBloatMeasureCursor struct {
	// the start of the week this state belongs to
	Week time.Time `json:"week,omitzero"`
	// the next table of the scan cycle, by name; empty starts the cycle
	Table string `json:"table,omitempty"`
	// the scan time this week's runs have spent
	ScanSpent time.Duration `json:"scan_spent,omitempty"`
	// this week's catalog pass is recorded
	CatalogDone bool `json:"catalog_done,omitempty"`
	// this week's scans are over: the cycle reached its end, the budget is
	// spent, or the catalog pass had no scans to leave
	ScanDone bool `json:"scan_done,omitempty"`
}

// The cursor a run of week continues from: the same week keeps what it spent
// and recorded, and a new week keeps only the scan cycle's table.
func (self DbBloatMeasureCursor) forWeek(week time.Time) DbBloatMeasureCursor {
	if self.Week.Equal(week) {
		return self
	}
	return DbBloatMeasureCursor{Week: week, Table: self.Table}
}

// Why a run stopped before its week's work was done.
type DbBloatMeasureStop string

const (
	// the context ended (drain, deadline or caller)
	DbBloatMeasureInterrupted DbBloatMeasureStop = "interrupted"
	// the load check refused the next scan, or recording failed
	DbBloatMeasureDeferred DbBloatMeasureStop = "deferred"
)

type DbBloatMeasureReport struct {
	// empty when the week's work is done
	Stopped DbBloatMeasureStop
	Reason  string
	// the measurements this run recorded, and how many of them are scans
	Recorded int
	Scanned  int
	// the heap and TOAST relations recorded without a scan, and the relations
	// recorded unmeasured, by reason
	Fallbacks map[string]int
	// the scan cycle reached its last table in this run
	CycleComplete bool
	// the expired measurements deleted
	Removed int64
}

// A table column's or an index column's width, alignment and statistics.
type dbBloatColumn = dbMaintenanceIndexColumn

// One valid index of a table or of its TOAST table, from the catalog.
type dbBloatIndex struct {
	schema       string
	name         string
	accessMethod string
	bytes        int64
	// pg_class.reltuples: negative when unknown
	entryCount float64
	fillfactor int
	columns    []dbBloatColumn
}

// One ordinary public table, a partition leaf included, with its TOAST table
// and indexes, from the catalog.
type dbBloatTable struct {
	name      string
	heapOid   int64
	heapBytes int64
	// pg_class.reltuples: negative when unknown
	rowCount   float64
	fillfactor int
	// pg_class.relnatts, dropped columns included
	attributeCount int
	columns        []dbBloatColumn
	// empty without a TOAST table
	toastName  string
	toastOid   int64
	toastBytes int64
	// the TOAST relation's reltuples: negative when unknown
	toastChunkCount float64
	// pg_stat_user_tables: negative without a statistics row
	liveRows int64
	deadRows int64
	indexes  []dbBloatIndex
}

// One result of pgstattuple_approx.
type dbBloatScan struct {
	relationBytes  int64
	scannedPercent float64
	liveTupleCount int64
	liveTupleBytes int64
	deadTupleCount int64
	deadTupleBytes int64
	freeBytes      int64
}

// The free space a rewrite at fillfactor keeps next to the live tuples: a page
// filled to fillfactor percent leaves the rest free on purpose.
func (self *dbBloatScan) reservedBytes(fillfactor int) int64 {
	if fillfactor <= 0 || 100 <= fillfactor {
		return 0
	}
	return self.liveTupleBytes * int64(100-fillfactor) / int64(fillfactor)
}

// Dead tuples and the free space beyond the fillfactor's reserve. A relation
// denser than its fillfactor (set after its rows were written) would grow in a
// rewrite, so it reclaims nothing.
func (self *dbBloatScan) reclaimableBytes(fillfactor int) int64 {
	return max(0, self.deadTupleBytes+self.freeBytes-self.reservedBytes(fillfactor))
}

// Estimates the heap bytes of a fresh rewrite of rowCount rows: each page holds,
// within its fillfactor less its 24-byte header, as many average tuples as fit
// (the 23-byte tuple header and a null bitmap when a column can be null,
// aligned, then the aligned column data less the expected width of null
// columns, the whole aligned, plus its 4-byte line pointer), and at most
// MaxHeapTuplesPerPage. The column data follows the btree estimate's layout
// (dbMaintenanceEstimateBtreeBytes). False when the row count is unknown
// (reltuples is -1 before the first vacuum or analyze) or a variable-width
// column has not been analyzed.
func dbBloatEstimateHeapBytes(
	columns []dbBloatColumn,
	attributeCount int,
	rowCount float64,
	fillfactor int,
	blockSize int64,
) (int64, bool) {
	if rowCount < 0 || fillfactor <= 0 || 100 < fillfactor || blockSize <= 0 {
		return 0, false
	}
	alignUp := func(n int, alignment int) int {
		return (n + alignment - 1) / alignment * alignment
	}
	dataBytes := 0
	expectedNullBytes := 0.0
	nullable := false
	for _, column := range columns {
		width := column.length
		if width <= 0 {
			if !column.analyzed {
				return 0, false
			}
			width = column.averageWidth
		}
		alignment := 1
		switch column.align {
		case 's':
			alignment = 2
		case 'i':
			alignment = 4
		case 'd':
			alignment = 8
		}
		dataBytes = alignUp(dataBytes, alignment) + width
		if 0 < column.nullFraction {
			nullable = true
			expectedNullBytes += column.nullFraction * float64(width)
		}
	}
	headerBytes := 23
	if nullable {
		headerBytes += (attributeCount + 7) / 8
	}
	tupleBytes := alignUp(alignUp(headerBytes, 8)+int(math.Ceil(float64(dataBytes)-expectedNullBytes)), 8)
	entryBytes := int64(tupleBytes + 4)
	usableBytes := blockSize*int64(fillfactor)/100 - 24
	// MaxHeapTuplesPerPage: the line pointers of empty tuples fill the page
	maxTuplesPerPage := (blockSize - 24) / (24 + 4)
	tuplesPerPage := min(max(usableBytes/entryBytes, 1), maxTuplesPerPage)
	pages := int64(math.Ceil(rowCount / float64(tuplesPerPage)))
	return pages * blockSize, true
}

// Estimates the bytes of a fresh rewrite of a TOAST relation of chunkCount
// chunks. TOAST sizes a full chunk so that dbBloatToastChunksPerPage fill a
// page, and a value's last chunk is shorter, so this is an upper bound. False
// when the chunk count is unknown: ANALYZE skips TOAST relations, and only a
// vacuum sets their reltuples.
func dbBloatEstimateToastBytes(chunkCount float64, blockSize int64) (int64, bool) {
	if chunkCount < 0 || blockSize <= 0 {
		return 0, false
	}
	return int64(math.Ceil(chunkCount/dbBloatToastChunksPerPage)) * blockSize, true
}

// Why pgstattuple_approx cannot scan for this session, or "" when it can.
func dbBloatScanUnavailableReason(extensionInstalled bool, functionFound bool, executable bool) string {
	switch {
	case functionFound && executable:
		return ""
	case functionFound:
		return dbBloatFallbackNoExecute
	case extensionInstalled:
		return dbBloatFallbackNotOnSearchPath
	default:
		return dbBloatFallbackNotInstalled
	}
}

// Why a failed scan's relation gets the statistics estimate. A statement or
// lock timeout and a refused privilege each have their own reason.
func dbBloatScanFailureReason(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgerrcode.QueryCanceled:
			return dbBloatFallbackScanTimeout
		case pgerrcode.LockNotAvailable:
			return dbBloatFallbackScanLockTimeout
		case pgerrcode.InsufficientPrivilege:
			return dbBloatFallbackRefused
		}
	}
	return dbBloatFallbackScanFailed
}

// The tables of the scan cycle from cursor on, in name order: the first is the
// first name at or after cursor, so a cursor naming a dropped table continues
// at the next name, and an empty cursor starts the cycle.
func dbBloatTablesFrom(tables []*dbBloatTable, cursor string) []*dbBloatTable {
	ordered := slices.Clone(tables)
	slices.SortFunc(ordered, func(a *dbBloatTable, b *dbBloatTable) int {
		return strings.Compare(a.name, b.name)
	})
	start, _ := slices.BinarySearchFunc(ordered, cursor, func(table *dbBloatTable, name string) int {
		return strings.Compare(table.name, name)
	})
	return ordered[start:]
}

// The week's scan budget: what earlier runs of the week spent, plus what this
// run spent between its first clock reading and the latest one. Only the
// constructor and observe read the clock, and a run observes once after each
// scan statement, so a test clock decides exactly when time passes.
type dbBloatScanBudget struct {
	limit      time.Duration
	priorSpent time.Duration
	clock      func() time.Time
	start      time.Time
	elapsed    time.Duration
}

func newDbBloatScanBudget(limit time.Duration, priorSpent time.Duration, clock func() time.Time) *dbBloatScanBudget {
	return &dbBloatScanBudget{
		limit:      limit,
		priorSpent: priorSpent,
		clock:      clock,
		start:      clock(),
	}
}

// Reads the clock: what this run has spent since it started scanning.
func (self *dbBloatScanBudget) observe() {
	self.elapsed = max(0, self.clock().Sub(self.start))
}

// What the week has spent, as of the latest reading.
func (self *dbBloatScanBudget) spent() time.Duration {
	return self.priorSpent + self.elapsed
}

func (self *dbBloatScanBudget) remaining() time.Duration {
	return max(0, self.limit-self.spent())
}

func (self *dbBloatScanBudget) exhausted() bool {
	return self.remaining() <= 0
}

// The bound of the next scan statement: maxScanDuration, cut to what the
// budget has left.
func (self *dbBloatScanBudget) statementBound(maxScanDuration time.Duration) time.Duration {
	return min(maxScanDuration, self.remaining())
}

// One row of db_bloat_measurement. A nil field is null.
type dbBloatMeasurement struct {
	measureTime       time.Time
	tableName         string
	relationSchema    string
	relationName      string
	relationKind      string
	method            string
	fallbackReason    string
	relationBytes     int64
	reclaimableBytes  *int64
	compactBytes      *int64
	fillfactor        int
	reservedBytes     *int64
	scan              *dbBloatScan
	estimatedRowCount *float64
	liveRows          *int64
	deadRows          *int64
	scanDuration      *time.Duration
}

func dbBloatPointer[T any](value T) *T {
	return &value
}

// An index against the btree estimate. An index of another access method, or
// one whose entry count or variable-width columns are unknown, is recorded
// unmeasured.
func dbBloatIndexMeasurement(measureTime time.Time, tableName string, index dbBloatIndex, blockSize int64) dbBloatMeasurement {
	measurement := dbBloatMeasurement{
		measureTime:       measureTime,
		tableName:         tableName,
		relationSchema:    index.schema,
		relationName:      index.name,
		relationKind:      dbBloatRelationIndex,
		relationBytes:     index.bytes,
		fillfactor:        index.fillfactor,
		estimatedRowCount: dbBloatPointer(index.entryCount),
	}
	if index.accessMethod != "btree" {
		measurement.method = dbBloatMethodNone
		measurement.fallbackReason = dbBloatUnmeasuredNotBtree
		return measurement
	}
	compactBytes, ok := dbMaintenanceEstimateBtreeBytes(index.columns, index.entryCount, index.fillfactor, blockSize)
	if !ok {
		measurement.method = dbBloatMethodNone
		measurement.fallbackReason = dbBloatUnmeasuredNotAnalyzed
		return measurement
	}
	measurement.method = dbBloatMethodBtreeEstimate
	measurement.compactBytes = dbBloatPointer(compactBytes)
	measurement.reclaimableBytes = dbBloatPointer(max(0, index.bytes-compactBytes))
	return measurement
}

// The heap's live and dead row counters, for a heap row.
func (self *dbBloatTable) withRowCounters(measurement dbBloatMeasurement) dbBloatMeasurement {
	if 0 <= self.liveRows {
		measurement.liveRows = dbBloatPointer(self.liveRows)
	}
	if 0 <= self.deadRows {
		measurement.deadRows = dbBloatPointer(self.deadRows)
	}
	return measurement
}

// A heap against the statistics estimate, recording why it was not scanned. A
// heap that cannot be estimated is recorded unmeasured.
func dbBloatHeapStatisticsMeasurement(measureTime time.Time, table *dbBloatTable, blockSize int64, fallbackReason string) dbBloatMeasurement {
	measurement := table.withRowCounters(dbBloatMeasurement{
		measureTime:       measureTime,
		tableName:         table.name,
		relationSchema:    "public",
		relationName:      table.name,
		relationKind:      dbBloatRelationHeap,
		method:            dbBloatMethodStatistics,
		fallbackReason:    fallbackReason,
		relationBytes:     table.heapBytes,
		fillfactor:        table.fillfactor,
		estimatedRowCount: dbBloatPointer(table.rowCount),
	})
	compactBytes, ok := dbBloatEstimateHeapBytes(table.columns, table.attributeCount, table.rowCount, table.fillfactor, blockSize)
	if !ok {
		measurement.method = dbBloatMethodNone
		measurement.fallbackReason = dbBloatUnmeasuredNotAnalyzed
		return measurement
	}
	measurement.compactBytes = dbBloatPointer(compactBytes)
	measurement.reclaimableBytes = dbBloatPointer(max(0, table.heapBytes-compactBytes))
	return measurement
}

// A TOAST relation against its chunk estimate, recording why it was not
// scanned. A TOAST relation is always filled to 100%.
func dbBloatToastStatisticsMeasurement(measureTime time.Time, table *dbBloatTable, blockSize int64, fallbackReason string) dbBloatMeasurement {
	measurement := dbBloatMeasurement{
		measureTime:       measureTime,
		tableName:         table.name,
		relationSchema:    "pg_toast",
		relationName:      table.toastName,
		relationKind:      dbBloatRelationToast,
		method:            dbBloatMethodStatistics,
		fallbackReason:    fallbackReason,
		relationBytes:     table.toastBytes,
		fillfactor:        100,
		estimatedRowCount: dbBloatPointer(table.toastChunkCount),
	}
	compactBytes, ok := dbBloatEstimateToastBytes(table.toastChunkCount, blockSize)
	if !ok {
		measurement.method = dbBloatMethodNone
		measurement.fallbackReason = dbBloatUnmeasuredNotVacuumed
		return measurement
	}
	measurement.compactBytes = dbBloatPointer(compactBytes)
	measurement.reclaimableBytes = dbBloatPointer(max(0, table.toastBytes-compactBytes))
	return measurement
}

// A heap or TOAST relation (kind) against its pgstattuple_approx result.
func dbBloatScanMeasurement(measureTime time.Time, table *dbBloatTable, kind string, scan dbBloatScan, duration time.Duration) dbBloatMeasurement {
	measurement := dbBloatMeasurement{
		measureTime:    measureTime,
		tableName:      table.name,
		relationSchema: "public",
		relationName:   table.name,
		relationKind:   kind,
		method:         dbBloatMethodScan,
		relationBytes:  scan.relationBytes,
		fillfactor:     table.fillfactor,
		scan:           &scan,
		scanDuration:   &duration,
	}
	if kind == dbBloatRelationToast {
		measurement.relationSchema = "pg_toast"
		measurement.relationName = table.toastName
		measurement.fillfactor = 100
		measurement.estimatedRowCount = dbBloatPointer(table.toastChunkCount)
	} else {
		measurement = table.withRowCounters(measurement)
		measurement.estimatedRowCount = dbBloatPointer(table.rowCount)
	}
	measurement.reservedBytes = dbBloatPointer(scan.reservedBytes(measurement.fillfactor))
	measurement.reclaimableBytes = dbBloatPointer(scan.reclaimableBytes(measurement.fillfactor))
	return measurement
}

// The statistics estimates of table's heap and TOAST relation, each when
// measureHeap or measureToast says so, all for fallbackReason.
func dbBloatStatisticsMeasurements(
	measureTime time.Time,
	table *dbBloatTable,
	blockSize int64,
	measureHeap bool,
	measureToast bool,
	fallbackReason string,
) []dbBloatMeasurement {
	measurements := []dbBloatMeasurement{}
	if measureHeap {
		measurements = append(measurements, dbBloatHeapStatisticsMeasurement(measureTime, table, blockSize, fallbackReason))
	}
	if measureToast {
		measurements = append(measurements, dbBloatToastStatisticsMeasurement(measureTime, table, blockSize, fallbackReason))
	}
	return measurements
}

// The catalog pass for table: each index, and the heap and TOAST relation when
// they are not left to the scans.
func dbBloatCatalogMeasurements(
	measureTime time.Time,
	table *dbBloatTable,
	blockSize int64,
	scanUnavailable string,
	settings *DbBloatMeasureSettings,
) []dbBloatMeasurement {
	measurements := []dbBloatMeasurement{}
	for _, index := range table.indexes {
		if settings.MinRelationBytes <= index.bytes {
			measurements = append(measurements, dbBloatIndexMeasurement(measureTime, table.name, index, blockSize))
		}
	}
	if settings.MinRelationBytes <= table.heapBytes {
		if reason := settings.scanFallbackReason(table.heapBytes, scanUnavailable); reason != "" {
			measurements = append(measurements, dbBloatHeapStatisticsMeasurement(measureTime, table, blockSize, reason))
		}
	}
	if table.toastName != "" && settings.MinRelationBytes <= table.toastBytes {
		if reason := settings.scanFallbackReason(table.toastBytes, scanUnavailable); reason != "" {
			measurements = append(measurements, dbBloatToastStatisticsMeasurement(measureTime, table, blockSize, reason))
		}
	}
	return measurements
}

// Every ordinary public table, a partition leaf included (a partitioned parent
// has no storage), with its TOAST relation, its statistics counters and one row
// per live column in column order, or one row with null column fields for a
// table without columns. Column statistics are the table's own, not a parent's
// inherited ones.
const dbBloatTableColumnsQuery = `
	SELECT
		table_class.relname::text,
		table_class.oid::bigint,
		pg_relation_size(table_class.oid),
		table_class.reltuples::float8,
		coalesce((
			SELECT table_option.option_value::int
			FROM pg_options_to_table(table_class.reloptions) table_option
			WHERE table_option.option_name = 'fillfactor'
		), 100),
		table_class.relnatts::int,
		coalesce(toast_class.relname::text, ''),
		coalesce(toast_class.oid::bigint, 0),
		coalesce(pg_relation_size(toast_class.oid), 0),
		coalesce(toast_class.reltuples::float8, -1),
		coalesce(table_stat.n_live_tup, -1),
		coalesce(table_stat.n_dead_tup, -1),
		table_attribute.attnum::int,
		table_attribute.attlen::int,
		table_attribute.attalign::text,
		column_stats.avg_width::int,
		column_stats.null_frac::float8,
		current_setting('block_size')::bigint
	FROM pg_class table_class
	INNER JOIN pg_namespace table_namespace ON table_namespace.oid = table_class.relnamespace
	LEFT JOIN pg_class toast_class ON toast_class.oid = table_class.reltoastrelid
	LEFT JOIN pg_stat_user_tables table_stat ON table_stat.relid = table_class.oid
	LEFT JOIN pg_attribute table_attribute
		ON table_attribute.attrelid = table_class.oid
		AND 0 < table_attribute.attnum
		AND NOT table_attribute.attisdropped
	LEFT JOIN pg_stats column_stats
		ON column_stats.schemaname = 'public'
		AND column_stats.tablename = table_class.relname
		AND column_stats.attname = table_attribute.attname
		AND NOT column_stats.inherited
	WHERE table_namespace.nspname = 'public'
	  AND table_class.relkind = 'r'
	ORDER BY table_class.relname, table_attribute.attnum
`

// Every valid index of an ordinary public table and of its TOAST relation, one
// row per index column in column order, with the public table the index
// belongs to. A column's statistics come from its table column, or from the
// index itself for an expression column, as in
// dbMaintenancePriorityIndexColumnsQuery; a TOAST index's columns are
// fixed-width and need none. The fillfactor is each access method's default
// unless set.
const dbBloatIndexColumnsQuery = `
	WITH measured_table AS (
		SELECT table_class.oid, table_class.relname, table_class.reltoastrelid
		FROM pg_class table_class
		INNER JOIN pg_namespace table_namespace ON table_namespace.oid = table_class.relnamespace
		WHERE table_namespace.nspname = 'public'
		  AND table_class.relkind = 'r'
	), owner AS (
		SELECT measured_table.oid AS relid, measured_table.relname AS table_name
		FROM measured_table
		UNION ALL
		SELECT measured_table.reltoastrelid, measured_table.relname
		FROM measured_table
		WHERE measured_table.reltoastrelid <> 0
	)
	SELECT
		owner.table_name::text,
		index_namespace.nspname::text,
		index_class.relname::text,
		index_am.amname::text,
		pg_relation_size(index_class.oid),
		index_class.reltuples::float8,
		coalesce((
			SELECT index_option.option_value::int
			FROM pg_options_to_table(index_class.reloptions) index_option
			WHERE index_option.option_name = 'fillfactor'
		), CASE index_am.amname WHEN 'hash' THEN 75 WHEN 'spgist' THEN 80 ELSE 90 END),
		index_attribute.attlen::int,
		index_attribute.attalign::text,
		column_stats.avg_width::int,
		column_stats.null_frac::float8
	FROM owner
	INNER JOIN pg_index index_state ON index_state.indrelid = owner.relid
	INNER JOIN pg_class index_class ON index_class.oid = index_state.indexrelid
	INNER JOIN pg_namespace index_namespace ON index_namespace.oid = index_class.relnamespace
	INNER JOIN pg_am index_am ON index_am.oid = index_class.relam
	INNER JOIN pg_class table_class ON table_class.oid = index_state.indrelid
	INNER JOIN pg_attribute index_attribute
		ON index_attribute.attrelid = index_class.oid
		AND 0 < index_attribute.attnum
	LEFT JOIN pg_attribute table_attribute
		ON table_attribute.attrelid = table_class.oid
		AND table_attribute.attnum = index_state.indkey[index_attribute.attnum - 1]
		AND 0 < table_attribute.attnum
	LEFT JOIN pg_stats column_stats
		ON column_stats.schemaname = 'public'
		AND NOT column_stats.inherited
		AND (
			(column_stats.tablename = table_class.relname AND column_stats.attname = table_attribute.attname)
			OR (table_attribute.attname IS NULL AND column_stats.tablename = index_class.relname AND column_stats.attname = index_attribute.attname)
		)
	WHERE index_state.indisvalid
	  AND index_class.relkind = 'i'
	ORDER BY owner.table_name, index_namespace.nspname, index_class.relname, index_attribute.attnum
`

// Whether pgstattuple is installed, whether pgstattuple_approx(regclass)
// resolves on the session's search path, and whether the session may execute
// it (EXECUTE is granted to pg_stat_scan_tables, and a superuser passes).
const dbBloatScanAvailabilityQuery = `
	SELECT
		EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pgstattuple'),
		to_regprocedure('pgstattuple_approx(regclass)') IS NOT NULL,
		coalesce(has_function_privilege(to_regprocedure('pgstattuple_approx(regclass)')::oid, 'EXECUTE'), false)
`

// One relation's scan, by oid.
const dbBloatScanQuery = `
	SELECT
		table_len,
		scanned_percent,
		approx_tuple_count,
		approx_tuple_len,
		dead_tuple_count,
		dead_tuple_len,
		approx_free_space
	FROM pgstattuple_approx($1::bigint::oid::regclass)
`

const dbBloatInsertMeasurementSql = `
	INSERT INTO db_bloat_measurement (
		measurement_id,
		measure_time,
		table_name,
		relation_schema,
		relation_name,
		relation_kind,
		method,
		fallback_reason,
		relation_bytes,
		reclaimable_bytes,
		compact_bytes,
		fillfactor,
		reserved_bytes,
		scanned_percent,
		live_tuple_count,
		live_tuple_bytes,
		dead_tuple_count,
		dead_tuple_bytes,
		free_bytes,
		estimated_row_count,
		live_row_count,
		dead_row_count,
		scan_duration_ms
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
`

// A batch of expired measurements, oldest index first.
const dbBloatRemoveExpiredSql = `
	DELETE FROM db_bloat_measurement
	WHERE measurement_id IN (
		SELECT measurement_id
		FROM db_bloat_measurement
		WHERE measure_time < $1
		LIMIT $2
	)
`

// Reads the tables of dbBloatTableColumnsQuery with the indexes of
// dbBloatIndexColumnsQuery, and the block size, on conn. Raises on failure.
func dbBloatReadCatalog(ctx context.Context, conn PgConn) (tables []*dbBloatTable, blockSize int64) {
	tableNames := map[string]*dbBloatTable{}
	result, err := conn.Query(ctx, dbBloatTableColumnsQuery)
	WithPgResult(result, err, func() {
		for result.Next() {
			var table dbBloatTable
			var attributeNumber *int32
			var length *int32
			var align *string
			var averageWidth *int32
			var nullFraction *float64
			Raise(result.Scan(
				&table.name,
				&table.heapOid,
				&table.heapBytes,
				&table.rowCount,
				&table.fillfactor,
				&table.attributeCount,
				&table.toastName,
				&table.toastOid,
				&table.toastBytes,
				&table.toastChunkCount,
				&table.liveRows,
				&table.deadRows,
				&attributeNumber,
				&length,
				&align,
				&averageWidth,
				&nullFraction,
				&blockSize,
			))
			current, ok := tableNames[table.name]
			if !ok {
				current = &table
				tableNames[table.name] = current
				tables = append(tables, current)
			}
			if attributeNumber == nil {
				// a table without columns
				continue
			}
			column := dbBloatColumn{length: int(*length)}
			if align != nil && *align != "" {
				column.align = (*align)[0]
			}
			if averageWidth != nil && nullFraction != nil {
				column.analyzed = true
				column.averageWidth = int(*averageWidth)
				column.nullFraction = *nullFraction
			}
			current.columns = append(current.columns, column)
		}
	})

	result, err = conn.Query(ctx, dbBloatIndexColumnsQuery)
	WithPgResult(result, err, func() {
		var current *dbBloatIndex
		var currentTable *dbBloatTable
		flush := func() {
			if current != nil && currentTable != nil {
				currentTable.indexes = append(currentTable.indexes, *current)
			}
			current = nil
		}
		for result.Next() {
			var tableName string
			var index dbBloatIndex
			var column dbBloatColumn
			var align string
			var averageWidth *int32
			var nullFraction *float64
			Raise(result.Scan(
				&tableName,
				&index.schema,
				&index.name,
				&index.accessMethod,
				&index.bytes,
				&index.entryCount,
				&index.fillfactor,
				&column.length,
				&align,
				&averageWidth,
				&nullFraction,
			))
			if align != "" {
				column.align = align[0]
			}
			if averageWidth != nil && nullFraction != nil {
				column.analyzed = true
				column.averageWidth = int(*averageWidth)
				column.nullFraction = *nullFraction
			}
			if current == nil || current.schema != index.schema || current.name != index.name {
				flush()
				current = &index
				// a table created after the table read is left for the next run
				currentTable = tableNames[tableName]
			}
			current.columns = append(current.columns, column)
		}
		flush()
	})
	return
}

// Reads why pgstattuple_approx cannot scan for conn's session, or "" when it
// can. Raises on failure.
func dbBloatReadScanAvailability(ctx context.Context, conn PgConn) string {
	var extensionInstalled bool
	var functionFound bool
	var executable bool
	Raise(conn.QueryRow(ctx, dbBloatScanAvailabilityQuery).Scan(&extensionInstalled, &functionFound, &executable))
	return dbBloatScanUnavailableReason(extensionInstalled, functionFound, executable)
}

// Scans one heap or TOAST relation with pgstattuple_approx on a direct
// maintenance session of its own, discarded afterwards so its settings never
// return to the pool: statementTimeout bounds the scan, lockTimeout its wait
// for the relation's lock, and transaction_timeout is lifted for it. A bound
// under a millisecond is a millisecond, since zero would lift the bound.
func dbBloatScanRelation(
	ctx context.Context,
	relationOid int64,
	statementTimeout time.Duration,
	lockTimeout time.Duration,
) (scan dbBloatScan, returnErr error) {
	conn, err := AcquireMaintenanceDbConn(ctx)
	if err != nil {
		return scan, err
	}
	defer discardPgConnection(ctx, conn)

	if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", max(statementTimeout.Milliseconds(), 1))); err != nil {
		return scan, err
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("SET lock_timeout = %d", max(lockTimeout.Milliseconds(), 1))); err != nil {
		return scan, err
	}
	HandleError(func() {
		WithoutTransactionTimeout(ctx, conn, func() {
			returnErr = conn.QueryRow(ctx, dbBloatScanQuery, relationOid).Scan(
				&scan.relationBytes,
				&scan.scannedPercent,
				&scan.liveTupleCount,
				&scan.liveTupleBytes,
				&scan.deadTupleCount,
				&scan.deadTupleBytes,
				&scan.freeBytes,
			)
		})
	}, func(recovered error) {
		// the scan's own failure comes first
		if returnErr == nil {
			returnErr = recovered
		}
	})
	return scan, returnErr
}

// Records measurements in one maintenance transaction.
func dbBloatRecord(ctx context.Context, measurements []dbBloatMeasurement) (returnErr error) {
	if len(measurements) == 0 {
		return nil
	}
	HandleError(func() {
		MaintenanceTx(ctx, func(tx PgTx) {
			BatchInTx(ctx, tx, func(batch PgBatch) {
				for _, measurement := range measurements {
					var fallbackReason *string
					if measurement.fallbackReason != "" {
						fallbackReason = &measurement.fallbackReason
					}
					var scannedPercent *float64
					var liveTupleCount *int64
					var liveTupleBytes *int64
					var deadTupleCount *int64
					var deadTupleBytes *int64
					var freeBytes *int64
					if scan := measurement.scan; scan != nil {
						scannedPercent = &scan.scannedPercent
						liveTupleCount = &scan.liveTupleCount
						liveTupleBytes = &scan.liveTupleBytes
						deadTupleCount = &scan.deadTupleCount
						deadTupleBytes = &scan.deadTupleBytes
						freeBytes = &scan.freeBytes
					}
					var scanDurationMillis *int64
					if measurement.scanDuration != nil {
						scanDurationMillis = dbBloatPointer(measurement.scanDuration.Milliseconds())
					}
					batch.Queue(
						dbBloatInsertMeasurementSql,
						NewId(),
						measurement.measureTime,
						measurement.tableName,
						measurement.relationSchema,
						measurement.relationName,
						measurement.relationKind,
						measurement.method,
						fallbackReason,
						measurement.relationBytes,
						measurement.reclaimableBytes,
						measurement.compactBytes,
						measurement.fillfactor,
						measurement.reservedBytes,
						scannedPercent,
						liveTupleCount,
						liveTupleBytes,
						deadTupleCount,
						deadTupleBytes,
						freeBytes,
						measurement.estimatedRowCount,
						measurement.liveRows,
						measurement.deadRows,
						scanDurationMillis,
					)
				}
			})
		})
	}, func(recovered error) {
		returnErr = recovered
	})
	return
}

// Deletes the measurements older than the retention, a batch a statement,
// until none is left or ctx ends. A failure ends it; the next week continues.
func dbBloatRemoveExpired(ctx context.Context, now time.Time, settings *DbBloatMeasureSettings) (removed int64, returnErr error) {
	cutoff := now.Add(-settings.Retention)
	for {
		select {
		case <-ctx.Done():
			return removed, context.Cause(ctx)
		default:
		}
		var batchRemoved int64
		HandleError(func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				tag := RaisePgResult(conn.Exec(ctx, dbBloatRemoveExpiredSql, cutoff, settings.RetentionBatchSize))
				batchRemoved = tag.RowsAffected()
			}, OptReadWrite(), OptNoRetry())
		}, func(recovered error) {
			returnErr = recovered
		})
		if returnErr != nil {
			return
		}
		removed += batchRemoved
		if batchRemoved < int64(settings.RetentionBatchSize) {
			return
		}
	}
}

// Runs the week's measurement from cursor (see the file comment) and returns
// what it did with the cursor to continue from. A context that is already done
// raises at the catalog read, so a canceled task is not acknowledged; once the
// catalog is read the run returns a report instead, whose Stopped field says
// whether the week's work is done.
func MeasureDbBloat(
	ctx context.Context,
	now time.Time,
	cursor DbBloatMeasureCursor,
	settings *DbBloatMeasureSettings,
) (*DbBloatMeasureReport, DbBloatMeasureCursor) {
	return measureDbBloat(ctx, now, cursor, settings, time.Now)
}

// clock times the scan budget.
func measureDbBloat(
	ctx context.Context,
	now time.Time,
	cursor DbBloatMeasureCursor,
	settings *DbBloatMeasureSettings,
	clock func() time.Time,
) (*DbBloatMeasureReport, DbBloatMeasureCursor) {
	report := &DbBloatMeasureReport{
		Fallbacks: map[string]int{},
	}
	cursor = cursor.forWeek(settings.WeekStart(now))
	if cursor.CatalogDone && cursor.ScanDone {
		return report, cursor
	}

	var tables []*dbBloatTable
	var blockSize int64
	var scanUnavailable string
	MaintenanceDb(ctx, func(conn PgConn) {
		tables, blockSize = dbBloatReadCatalog(ctx, conn)
		scanUnavailable = dbBloatReadScanAvailability(ctx, conn)
	}, OptNoRetry())

	recorded := []dbBloatMeasurement{}
	defer func() {
		cycle := "continues at " + cursor.Table
		if report.CycleComplete {
			cycle = "complete"
		} else if cursor.Table == "" {
			cycle = "at its start"
		}
		stopped := "done for the week"
		if report.Stopped != "" {
			stopped = fmt.Sprintf("%s: %s", report.Stopped, report.Reason)
		}
		glog.Infof(
			"[db]bloat week of %s %s: recorded %d measurements (%d scans), without a scan %v, scan cycle %s, scan time %s of %s, removed %d expired\n",
			cursor.Week.Format(time.DateOnly),
			stopped,
			report.Recorded,
			report.Scanned,
			report.Fallbacks,
			cycle,
			cursor.ScanSpent.Round(time.Second),
			settings.ScanBudget,
			report.Removed,
		)
		slices.SortFunc(recorded, func(a dbBloatMeasurement, b dbBloatMeasurement) int {
			reclaimable := func(measurement dbBloatMeasurement) int64 {
				if measurement.reclaimableBytes == nil {
					return 0
				}
				return *measurement.reclaimableBytes
			}
			return cmp.Or(cmp.Compare(reclaimable(b), reclaimable(a)), strings.Compare(a.relationName, b.relationName))
		})
		for i, measurement := range recorded[:min(len(recorded), settings.LogLargestCount)] {
			if measurement.reclaimableBytes == nil || *measurement.reclaimableBytes == 0 {
				break
			}
			glog.Infof(
				"[db]bloat largest[%d] %s.%s (%s of %s, %s) reclaimable %d of %d bytes\n",
				i+1,
				measurement.relationSchema,
				measurement.relationName,
				measurement.relationKind,
				measurement.tableName,
				measurement.method,
				*measurement.reclaimableBytes,
				measurement.relationBytes,
			)
		}
	}()

	stop := func(stopped DbBloatMeasureStop, reason string) (*DbBloatMeasureReport, DbBloatMeasureCursor) {
		report.Stopped = stopped
		report.Reason = reason
		return report, cursor
	}
	interrupted := func() (*DbBloatMeasureReport, DbBloatMeasureCursor) {
		return stop(DbBloatMeasureInterrupted, context.Cause(ctx).Error())
	}
	// A failed record leaves its measurements for the run that continues.
	record := func(measurements []dbBloatMeasurement) error {
		if err := dbBloatRecord(ctx, measurements); err != nil {
			return err
		}
		recorded = append(recorded, measurements...)
		report.Recorded += len(measurements)
		for _, measurement := range measurements {
			if measurement.method == dbBloatMethodScan {
				report.Scanned += 1
			} else if measurement.fallbackReason != "" {
				report.Fallbacks[measurement.fallbackReason] += 1
			}
		}
		return nil
	}
	recordFailed := func(err error) (*DbBloatMeasureReport, DbBloatMeasureCursor) {
		select {
		case <-ctx.Done():
			return interrupted()
		default:
		}
		return stop(DbBloatMeasureDeferred, fmt.Sprintf("record failed: %s", err))
	}

	if !cursor.CatalogDone {
		removed, err := dbBloatRemoveExpired(ctx, now, settings)
		report.Removed = removed
		if err != nil {
			glog.Infof("[db]bloat expired measurements remain: %s\n", err)
		}
		measurements := []dbBloatMeasurement{}
		for _, table := range tables {
			measurements = append(measurements, dbBloatCatalogMeasurements(now, table, blockSize, scanUnavailable, settings)...)
		}
		if err := record(measurements); err != nil {
			return recordFailed(err)
		}
		cursor.CatalogDone = true
		// without scans the catalog pass measured every relation
		cursor.ScanDone = scanUnavailable != ""
		if cursor.ScanDone {
			return report, cursor
		}
	}

	// The relations of the scan cycle; a smaller one is not measured, and a
	// larger one is the catalog pass's.
	scanned := func(relationBytes int64) bool {
		return settings.MinRelationBytes <= relationBytes && relationBytes < settings.MaxScanRelationBytes
	}
	budget := newDbBloatScanBudget(settings.ScanBudget, cursor.ScanSpent, clock)
	// Set once scanning is unavailable or refused: the rest of the cycle gets
	// the statistics estimate for this reason, which the catalog pass left to
	// the scans.
	fallbackReason := scanUnavailable
	for _, table := range dbBloatTablesFrom(tables, cursor.Table) {
		cursor.Table = table.name
		cursor.ScanSpent = budget.spent()
		select {
		case <-ctx.Done():
			return interrupted()
		default:
		}
		scanHeap := scanned(table.heapBytes)
		scanToast := table.toastName != "" && scanned(table.toastBytes)
		if !scanHeap && !scanToast {
			continue
		}
		if fallbackReason != "" {
			if err := record(dbBloatStatisticsMeasurements(now, table, blockSize, scanHeap, scanToast, fallbackReason)); err != nil {
				return recordFailed(err)
			}
			continue
		}
		if budget.exhausted() {
			cursor.ScanDone = true
			return report, cursor
		}
		load, err := dbMaintenanceSampleLoad(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return interrupted()
			default:
			}
			return stop(DbBloatMeasureDeferred, fmt.Sprintf("load sample failed: %s", err))
		}
		if reason := dbMaintenanceDeferReason(load, settings.MaxTransactionAge, settings.MaxRunningBackends); reason != "" {
			return stop(DbBloatMeasureDeferred, reason)
		}

		measurements := []dbBloatMeasurement{}
		// the budget ran out during this table's scans
		budgetCut := false
		for _, kind := range []string{dbBloatRelationHeap, dbBloatRelationToast} {
			relationOid := table.heapOid
			if kind == dbBloatRelationToast {
				if !scanToast {
					continue
				}
				relationOid = table.toastOid
			} else if !scanHeap {
				continue
			}
			if fallbackReason != "" {
				measurements = append(measurements, dbBloatStatisticsMeasurements(
					now,
					table,
					blockSize,
					kind == dbBloatRelationHeap,
					kind == dbBloatRelationToast,
					fallbackReason,
				)...)
				continue
			}
			bound := budget.statementBound(settings.MaxScanDuration)
			if bound <= 0 {
				budgetCut = true
				break
			}
			scanStart := time.Now()
			scan, err := dbBloatScanRelation(ctx, relationOid, bound, settings.ScanLockTimeout)
			duration := time.Since(scanStart)
			budget.observe()
			cursor.ScanSpent = budget.spent()
			if err == nil {
				measurements = append(measurements, dbBloatScanMeasurement(NowUtc(), table, kind, scan, duration))
				continue
			}
			select {
			case <-ctx.Done():
				return interrupted()
			default:
			}
			reason := dbBloatScanFailureReason(err)
			if reason == dbBloatFallbackScanTimeout && bound < settings.MaxScanDuration {
				// cut by the budget, not by the scan bound: next week rescans
				budgetCut = true
				break
			}
			glog.Infof("[db]bloat scan of %s %s failed (%s), statistics estimate instead: %s\n", kind, table.name, reason, err)
			if reason == dbBloatFallbackRefused {
				fallbackReason = reason
			}
			measurements = append(measurements, dbBloatStatisticsMeasurements(
				now,
				table,
				blockSize,
				kind == dbBloatRelationHeap,
				kind == dbBloatRelationToast,
				reason,
			)...)
		}
		if err := record(measurements); err != nil {
			return recordFailed(err)
		}
		if budgetCut {
			cursor.ScanDone = true
			return report, cursor
		}
	}
	cursor.Table = ""
	cursor.ScanSpent = budget.spent()
	cursor.ScanDone = true
	report.CycleComplete = true
	return report, cursor
}
