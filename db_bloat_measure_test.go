// Pure tests of the weekly bloat measurement (db_bloat_measure.go): the
// fillfactor reserve a scan subtracts, the statistics and TOAST estimates, the
// row each relation gets, the scan cycle's cursor and budget, and the week the
// run belongs to. No database is read.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

const testDbBloatBlockSize = int64(8192)

// bigint, bigint, timestamp and uuid without nulls: 40 bytes of data after the
// 24-byte aligned tuple header, so a 64-byte tuple and its 4-byte line pointer
// take 68 bytes, and 120 fill the 8168 bytes after a page's header.
func testDbBloatFixedColumns() []dbBloatColumn {
	return []dbBloatColumn{
		{length: 8, align: 'd'},
		{length: 8, align: 'd'},
		{length: 8, align: 'd'},
		{length: 16, align: 'c'},
	}
}

// A scan's free space counts only beyond what a rewrite at the fillfactor
// keeps free next to the live tuples.
func TestDbBloatScanReclaimableSubtractsTheFillfactorReserve(t *testing.T) {
	scan := dbBloatScan{relationBytes: 4 * testDbBloatBlockSize, liveTupleBytes: 1000, deadTupleBytes: 300, freeBytes: 2000}
	for _, c := range []struct {
		fillfactor  int
		reserved    int64
		reclaimable int64
	}{
		// a full page keeps nothing free on purpose
		{fillfactor: 100, reserved: 0, reclaimable: 2300},
		// half of each page stays free: as many free bytes as live ones
		{fillfactor: 50, reserved: 1000, reclaimable: 1300},
		{fillfactor: 80, reserved: 250, reclaimable: 2050},
		// out of range is no reserve
		{fillfactor: 0, reserved: 0, reclaimable: 2300},
		{fillfactor: 120, reserved: 0, reclaimable: 2300},
	} {
		if reserved := scan.reservedBytes(c.fillfactor); reserved != c.reserved {
			t.Errorf("fillfactor %d reserves %d bytes, want %d", c.fillfactor, reserved, c.reserved)
		}
		if reclaimable := scan.reclaimableBytes(c.fillfactor); reclaimable != c.reclaimable {
			t.Errorf("fillfactor %d reclaims %d bytes, want %d", c.fillfactor, reclaimable, c.reclaimable)
		}
	}

	// a relation denser than its fillfactor, set after its rows were written,
	// would grow in a rewrite: it reclaims nothing rather than a negative
	dense := dbBloatScan{relationBytes: 2 * testDbBloatBlockSize, liveTupleBytes: 8000, deadTupleBytes: 100, freeBytes: 500}
	if reclaimable := dense.reclaimableBytes(50); reclaimable != 0 {
		t.Fatalf("a relation denser than its fillfactor reclaims %d bytes", reclaimable)
	}

	// an inconsistent result never reclaims more than the relation
	overcounted := dbBloatScan{relationBytes: testDbBloatBlockSize, deadTupleBytes: 6000, freeBytes: 6000}
	if reclaimable := overcounted.reclaimableBytes(100); reclaimable != testDbBloatBlockSize {
		t.Fatalf("a relation of %d bytes reclaims %d", testDbBloatBlockSize, reclaimable)
	}
}

// A heap row applies the table's fillfactor and carries the row counters; a
// TOAST row is always at 100 and names the TOAST relation.
func TestDbBloatScanMeasurementAppliesEachRelationsFillfactor(t *testing.T) {
	measureTime := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	table := &dbBloatTable{
		name:            "synthetic_queue",
		fillfactor:      50,
		rowCount:        300,
		toastName:       "pg_toast_16400",
		toastChunkCount: 40,
		liveRows:        300,
		deadRows:        3000000,
	}
	scan := dbBloatScan{relationBytes: 64 * 8192, scannedPercent: 37.5, liveTupleCount: 300, liveTupleBytes: 30000, deadTupleCount: 1000, deadTupleBytes: 100000, freeBytes: 300000}

	heap := dbBloatScanMeasurement(measureTime, table, dbBloatRelationHeap, scan, 2*time.Second)
	if heap.method != dbBloatMethodScan || heap.relationSchema != "public" || heap.relationName != "synthetic_queue" ||
		heap.relationKind != dbBloatRelationHeap || heap.fillfactor != 50 || heap.relationBytes != 64*8192 {
		t.Fatalf("heap row identity = %+v", heap)
	}
	if *heap.reservedBytes != 30000 || *heap.reclaimableBytes != 100000+300000-30000 {
		t.Fatalf("heap row reserves %d and reclaims %d bytes", *heap.reservedBytes, *heap.reclaimableBytes)
	}
	if heap.scan.scannedPercent != 37.5 || *heap.scanDuration != 2*time.Second || *heap.estimatedRowCount != 300 ||
		*heap.liveRows != 300 || *heap.deadRows != 3000000 {
		t.Fatalf("heap row lost its scan or counters: %+v", heap)
	}

	toast := dbBloatScanMeasurement(measureTime, table, dbBloatRelationToast, scan, time.Second)
	if toast.relationSchema != "pg_toast" || toast.relationName != "pg_toast_16400" || toast.tableName != "synthetic_queue" ||
		toast.fillfactor != 100 || *toast.reservedBytes != 0 || *toast.reclaimableBytes != 400000 {
		t.Fatalf("toast row = %+v", toast)
	}
	if *toast.estimatedRowCount != 40 || toast.liveRows != nil || toast.deadRows != nil {
		t.Fatalf("toast row carries heap counters: %+v", toast)
	}
}

// The statistics estimate packs the live rows at the fillfactor, the way a
// rewrite would, and is compared against the heap's size.
func TestDbBloatEstimateHeapBytesPacksRowsAtTheFillfactor(t *testing.T) {
	for _, c := range []struct {
		name           string
		columns        []dbBloatColumn
		attributeCount int
		rowCount       float64
		fillfactor     int
		bytes          int64
		ok             bool
	}{
		// 120 a page
		{name: "fixed", columns: testDbBloatFixedColumns(), attributeCount: 4, rowCount: 24000, fillfactor: 100, bytes: 200 * testDbBloatBlockSize, ok: true},
		{name: "partial page", columns: testDbBloatFixedColumns(), attributeCount: 4, rowCount: 24001, fillfactor: 100, bytes: 201 * testDbBloatBlockSize, ok: true},
		// (4096 - 24) / 68 = 59 a page
		{name: "fillfactor 50", columns: testDbBloatFixedColumns(), attributeCount: 4, rowCount: 24000, fillfactor: 50, bytes: 407 * testDbBloatBlockSize, ok: true},
		// a nullable int4 then an int8 among nine attributes: a 2-byte bitmap
		// makes the header 32, the data is 16 less the expected 2 null bytes,
		// so 48 + 4 bytes, 157 a page
		{
			name: "nullable",
			columns: []dbBloatColumn{
				{length: 4, align: 'i', analyzed: true, averageWidth: 4, nullFraction: 0.5},
				{length: 8, align: 'd', analyzed: true, averageWidth: 8},
			},
			attributeCount: 9, rowCount: 1570, fillfactor: 100, bytes: 10 * testDbBloatBlockSize, ok: true,
		},
		// a variable-width column takes its average width: 24 + 8 + 20 = 52,
		// 56 aligned, 60 with its line pointer, 136 a page
		{
			name: "variable width",
			columns: []dbBloatColumn{
				{length: 8, align: 'd'},
				{length: -1, align: 'i', analyzed: true, averageWidth: 20},
			},
			attributeCount: 2, rowCount: 1360, fillfactor: 100, bytes: 10 * testDbBloatBlockSize, ok: true,
		},
		// no columns: the line pointer limit, MaxHeapTuplesPerPage (291)
		{name: "no columns", attributeCount: 0, rowCount: 2910, fillfactor: 100, bytes: 10 * testDbBloatBlockSize, ok: true},
		{name: "empty", columns: testDbBloatFixedColumns(), attributeCount: 4, rowCount: 0, fillfactor: 100, bytes: 0, ok: true},
		// never vacuumed or analyzed
		{name: "unknown rows", columns: testDbBloatFixedColumns(), attributeCount: 4, rowCount: -1, fillfactor: 100},
		{
			name:           "not analyzed",
			columns:        []dbBloatColumn{{length: -1, align: 'i'}},
			attributeCount: 1, rowCount: 100, fillfactor: 100,
		},
		{name: "bad fillfactor", columns: testDbBloatFixedColumns(), attributeCount: 4, rowCount: 100, fillfactor: 0},
	} {
		bytes, ok := dbBloatEstimateHeapBytes(c.columns, c.attributeCount, c.rowCount, c.fillfactor, testDbBloatBlockSize)
		if ok != c.ok || bytes != c.bytes {
			t.Errorf("%s: estimate %d bytes (%t), want %d (%t)", c.name, bytes, ok, c.bytes, c.ok)
		}
	}
}

// Full chunks fill a page four at a time; an unvacuumed TOAST relation has no
// chunk count.
func TestDbBloatEstimateToastBytesIsAnUpperBound(t *testing.T) {
	for _, c := range []struct {
		chunkCount float64
		bytes      int64
		ok         bool
	}{
		{chunkCount: 4000, bytes: 1000 * testDbBloatBlockSize, ok: true},
		{chunkCount: 4001, bytes: 1001 * testDbBloatBlockSize, ok: true},
		{chunkCount: 0, bytes: 0, ok: true},
		{chunkCount: -1},
	} {
		if bytes, ok := dbBloatEstimateToastBytes(c.chunkCount, testDbBloatBlockSize); ok != c.ok || bytes != c.bytes {
			t.Errorf("%.0f chunks: %d bytes (%t), want %d (%t)", c.chunkCount, bytes, ok, c.bytes, c.ok)
		}
	}
}

// A heap or TOAST relation recorded without a scan keeps why, and one that
// cannot be estimated is recorded unmeasured.
func TestDbBloatStatisticsMeasurementsRecordTheEstimateAndItsReason(t *testing.T) {
	measureTime := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	table := &dbBloatTable{
		name:            "synthetic_history",
		heapBytes:       800 * testDbBloatBlockSize,
		rowCount:        24000,
		fillfactor:      100,
		attributeCount:  4,
		columns:         testDbBloatFixedColumns(),
		toastName:       "pg_toast_16500",
		toastBytes:      3000 * testDbBloatBlockSize,
		toastChunkCount: 4000,
		liveRows:        24000,
		deadRows:        -1,
	}
	measurements := dbBloatStatisticsMeasurements(measureTime, table, testDbBloatBlockSize, true, true, dbBloatFallbackOverScanSize)
	if len(measurements) != 2 {
		t.Fatalf("got %d measurements, want the heap and its TOAST relation", len(measurements))
	}
	heap, toast := measurements[0], measurements[1]
	if heap.method != dbBloatMethodStatistics || heap.fallbackReason != dbBloatFallbackOverScanSize ||
		*heap.compactBytes != 200*testDbBloatBlockSize || *heap.reclaimableBytes != 600*testDbBloatBlockSize {
		t.Fatalf("heap estimate = %+v", heap)
	}
	if *heap.liveRows != 24000 || heap.deadRows != nil || *heap.estimatedRowCount != 24000 {
		t.Fatalf("heap counters: live %v dead %v", heap.liveRows, heap.deadRows)
	}
	if toast.method != dbBloatMethodStatistics || toast.relationSchema != "pg_toast" || toast.relationName != "pg_toast_16500" ||
		*toast.compactBytes != 1000*testDbBloatBlockSize || *toast.reclaimableBytes != 2000*testDbBloatBlockSize || toast.fillfactor != 100 {
		t.Fatalf("toast estimate = %+v", toast)
	}

	// smaller than its estimate, as a recently grown table can be: nothing to
	// reclaim, never a negative
	table.heapBytes = 100 * testDbBloatBlockSize
	if heap := dbBloatHeapStatisticsMeasurement(measureTime, table, testDbBloatBlockSize, dbBloatFallbackNoExecute); *heap.reclaimableBytes != 0 {
		t.Fatalf("a heap under its estimate reclaims %d bytes", *heap.reclaimableBytes)
	}

	table.rowCount = -1
	table.toastChunkCount = -1
	unmeasured := dbBloatStatisticsMeasurements(measureTime, table, testDbBloatBlockSize, true, true, dbBloatFallbackNoExecute)
	if unmeasured[0].method != dbBloatMethodNone || unmeasured[0].fallbackReason != dbBloatUnmeasuredNotAnalyzed || unmeasured[0].reclaimableBytes != nil {
		t.Fatalf("unanalyzed heap = %+v", unmeasured[0])
	}
	if unmeasured[1].method != dbBloatMethodNone || unmeasured[1].fallbackReason != dbBloatUnmeasuredNotVacuumed || unmeasured[1].reclaimableBytes != nil {
		t.Fatalf("unvacuumed toast = %+v", unmeasured[1])
	}
	if only := dbBloatStatisticsMeasurements(measureTime, table, testDbBloatBlockSize, false, true, dbBloatFallbackNoExecute); len(only) != 1 || only[0].relationKind != dbBloatRelationToast {
		t.Fatalf("toast only = %+v", only)
	}
}

// An index is the priority rebuild's btree estimate, not a fork of it.
func TestDbBloatIndexMeasurementUsesTheBtreeEstimate(t *testing.T) {
	measureTime := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	index := dbBloatIndex{
		schema:       "public",
		name:         "synthetic_queue_available_block",
		accessMethod: "btree",
		bytes:        47 * 1000 * 1000 * 1000,
		entryCount:   300,
		fillfactor:   90,
		columns:      []dbBloatColumn{{length: 8, align: 'd', analyzed: true, averageWidth: 8}},
	}
	compactBytes, ok := dbMaintenanceEstimateBtreeBytes(index.columns, index.entryCount, index.fillfactor, testDbBloatBlockSize)
	if !ok {
		t.Fatal("the btree estimate is unavailable for the fixture")
	}
	measurement := dbBloatIndexMeasurement(measureTime, "synthetic_queue", index, testDbBloatBlockSize)
	if measurement.method != dbBloatMethodBtreeEstimate || measurement.relationKind != dbBloatRelationIndex ||
		measurement.tableName != "synthetic_queue" || *measurement.compactBytes != compactBytes ||
		*measurement.reclaimableBytes != index.bytes-compactBytes || *measurement.estimatedRowCount != 300 {
		t.Fatalf("index row = %+v, compact estimate %d", measurement, compactBytes)
	}

	index.accessMethod = "gin"
	if gin := dbBloatIndexMeasurement(measureTime, "synthetic_queue", index, testDbBloatBlockSize); gin.method != dbBloatMethodNone ||
		gin.fallbackReason != dbBloatUnmeasuredNotBtree || gin.reclaimableBytes != nil || gin.relationBytes != index.bytes {
		t.Fatalf("gin index row = %+v", gin)
	}
	index.accessMethod = "btree"
	index.columns = []dbBloatColumn{{length: -1, align: 'i'}}
	if unanalyzed := dbBloatIndexMeasurement(measureTime, "synthetic_queue", index, testDbBloatBlockSize); unanalyzed.method != dbBloatMethodNone ||
		unanalyzed.fallbackReason != dbBloatUnmeasuredNotAnalyzed {
		t.Fatalf("unanalyzed index row = %+v", unanalyzed)
	}
}

// The catalog pass measures every index and leaves each heap and TOAST
// relation under the scan bound to the scans, unless scanning is unavailable.
func TestDbBloatCatalogMeasurementsLeaveScannedRelationsToTheScans(t *testing.T) {
	measureTime := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	settings := DefaultDbBloatMeasureSettings()
	settings.MinRelationBytes = 10 * testDbBloatBlockSize
	settings.MaxScanRelationBytes = 1000 * testDbBloatBlockSize
	table := &dbBloatTable{
		name:            "synthetic_mixed",
		heapBytes:       500 * testDbBloatBlockSize,
		rowCount:        24000,
		fillfactor:      100,
		attributeCount:  4,
		columns:         testDbBloatFixedColumns(),
		toastName:       "pg_toast_16600",
		toastBytes:      2000 * testDbBloatBlockSize,
		toastChunkCount: 4000,
		liveRows:        -1,
		deadRows:        -1,
		indexes: []dbBloatIndex{
			{schema: "public", name: "synthetic_mixed_pkey", accessMethod: "btree", bytes: 100 * testDbBloatBlockSize, entryCount: 24000, fillfactor: 90, columns: []dbBloatColumn{{length: 8, align: 'd'}}},
			// under MinRelationBytes: not measured
			{schema: "public", name: "synthetic_mixed_small", accessMethod: "btree", bytes: testDbBloatBlockSize, entryCount: 10, fillfactor: 90, columns: []dbBloatColumn{{length: 8, align: 'd'}}},
			{schema: "pg_toast", name: "pg_toast_16600_index", accessMethod: "btree", bytes: 50 * testDbBloatBlockSize, entryCount: 4000, fillfactor: 90, columns: []dbBloatColumn{{length: 4, align: 'i'}, {length: 4, align: 'i'}}},
		},
	}
	names := func(measurements []dbBloatMeasurement) []string {
		values := []string{}
		for _, measurement := range measurements {
			values = append(values, fmt.Sprintf("%s.%s %s %s", measurement.relationSchema, measurement.relationName, measurement.method, measurement.fallbackReason))
		}
		return values
	}

	scanning := names(dbBloatCatalogMeasurements(measureTime, table, testDbBloatBlockSize, "", settings))
	want := []string{
		"public.synthetic_mixed_pkey btree_estimate ",
		"pg_toast.pg_toast_16600_index btree_estimate ",
		// over the scan bound
		"pg_toast.pg_toast_16600 statistics over-scan-size",
	}
	if !slices.Equal(scanning, want) {
		t.Fatalf("catalog pass with scans = %v, want %v", scanning, want)
	}

	unavailable := names(dbBloatCatalogMeasurements(measureTime, table, testDbBloatBlockSize, dbBloatFallbackNoExecute, settings))
	want = []string{
		"public.synthetic_mixed_pkey btree_estimate ",
		"pg_toast.pg_toast_16600_index btree_estimate ",
		"public.synthetic_mixed statistics pgstattuple-no-execute",
		"pg_toast.pg_toast_16600 statistics pgstattuple-no-execute",
	}
	if !slices.Equal(unavailable, want) {
		t.Fatalf("catalog pass without scans = %v, want %v", unavailable, want)
	}
}

func TestDbBloatScanUnavailableReasonNamesWhatIsMissing(t *testing.T) {
	for _, c := range []struct {
		extensionInstalled bool
		functionFound      bool
		executable         bool
		reason             string
	}{
		{extensionInstalled: true, functionFound: true, executable: true, reason: ""},
		// a stand-in with the extension's interface scans too
		{extensionInstalled: false, functionFound: true, executable: true, reason: ""},
		{extensionInstalled: true, functionFound: true, executable: false, reason: dbBloatFallbackNoExecute},
		{extensionInstalled: true, functionFound: false, executable: false, reason: dbBloatFallbackNotOnSearchPath},
		{extensionInstalled: false, functionFound: false, executable: false, reason: dbBloatFallbackNotInstalled},
	} {
		if reason := dbBloatScanUnavailableReason(c.extensionInstalled, c.functionFound, c.executable); reason != c.reason {
			t.Errorf("%+v: reason %q", c, reason)
		}
	}
}

func TestDbBloatScanFailureReasonClassifiesTheError(t *testing.T) {
	for _, c := range []struct {
		err    error
		reason string
	}{
		{err: &pgconn.PgError{Code: pgerrcode.QueryCanceled}, reason: dbBloatFallbackScanTimeout},
		{err: &pgconn.PgError{Code: pgerrcode.LockNotAvailable}, reason: dbBloatFallbackScanLockTimeout},
		{err: fmt.Errorf("scan: %w", &pgconn.PgError{Code: pgerrcode.InsufficientPrivilege}), reason: dbBloatFallbackRefused},
		{err: &pgconn.PgError{Code: pgerrcode.UndefinedTable}, reason: dbBloatFallbackScanFailed},
		{err: errors.New("synthetic connection reset"), reason: dbBloatFallbackScanFailed},
	} {
		if reason := dbBloatScanFailureReason(c.err); reason != c.reason {
			t.Errorf("%v: reason %q, want %q", c.err, reason, c.reason)
		}
	}
}

// The cycle continues at the cursor's table, or the next name after it when it
// was dropped, in name order whatever order the catalog returned.
func TestDbBloatTablesFromResumesAtTheCursor(t *testing.T) {
	tables := []*dbBloatTable{{name: "synthetic_c"}, {name: "synthetic_a"}, {name: "synthetic_d"}, {name: "synthetic_b"}}
	for _, c := range []struct {
		cursor string
		names  []string
	}{
		{cursor: "", names: []string{"synthetic_a", "synthetic_b", "synthetic_c", "synthetic_d"}},
		{cursor: "synthetic_b", names: []string{"synthetic_b", "synthetic_c", "synthetic_d"}},
		// a dropped table
		{cursor: "synthetic_bb", names: []string{"synthetic_c", "synthetic_d"}},
		{cursor: "synthetic_z", names: []string{}},
	} {
		names := []string{}
		for _, table := range dbBloatTablesFrom(tables, c.cursor) {
			names = append(names, table.name)
		}
		if !slices.Equal(names, c.names) {
			t.Errorf("from %q: %v, want %v", c.cursor, names, c.names)
		}
	}
	if tables[0].name != "synthetic_c" {
		t.Fatal("the catalog's slice was reordered")
	}
}

// The budget carries what the week's earlier runs spent, reads its clock only
// when told, and cuts the last scan statement to what is left.
func TestDbBloatScanBudgetSpendsTheWeeksBudget(t *testing.T) {
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	budget := newDbBloatScanBudget(time.Hour, 15*time.Minute, clock)
	if budget.spent() != 15*time.Minute || budget.remaining() != 45*time.Minute || budget.exhausted() {
		t.Fatalf("a fresh run of the week: spent %s remaining %s", budget.spent(), budget.remaining())
	}
	at = at.Add(30 * time.Minute)
	if budget.spent() != 15*time.Minute {
		t.Fatal("the budget read the clock before it was told to")
	}
	budget.observe()
	if budget.spent() != 45*time.Minute || budget.statementBound(10*time.Minute) != 10*time.Minute {
		t.Fatalf("after 30 minutes: spent %s bound %s", budget.spent(), budget.statementBound(10*time.Minute))
	}
	at = at.Add(8 * time.Minute)
	budget.observe()
	if bound := budget.statementBound(10 * time.Minute); bound != 7*time.Minute {
		t.Fatalf("the last statement's bound is %s, want what is left", bound)
	}
	at = at.Add(10 * time.Minute)
	budget.observe()
	if !budget.exhausted() || budget.remaining() != 0 || budget.statementBound(10*time.Minute) != 0 || budget.spent() != 63*time.Minute {
		t.Fatalf("an overrun budget: spent %s remaining %s", budget.spent(), budget.remaining())
	}
	// a clock that steps back spends nothing
	at = at.Add(-2 * time.Hour)
	budget.observe()
	if budget.spent() != 15*time.Minute {
		t.Fatalf("a clock stepping back spent %s", budget.spent())
	}
}

func TestDbBloatMeasureWeekStartIsTheLatestSlot(t *testing.T) {
	settings := DefaultDbBloatMeasureSettings()
	slot := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if slot.Weekday() != time.Sunday {
		t.Fatal("the fixture slot is not a Sunday")
	}
	for _, c := range []struct {
		now  time.Time
		week time.Time
	}{
		{now: slot, week: slot},
		{now: slot.Add(-time.Nanosecond), week: slot.AddDate(0, 0, -7)},
		{now: slot.Add(time.Hour), week: slot},
		{now: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC), week: slot},
		{now: time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC), week: slot},
		{now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), week: slot.AddDate(0, 0, 7)},
		// another zone, the same instant
		{now: time.Date(2026, 10, 25, 5, 30, 0, 0, time.FixedZone("synthetic", -7*60*60)), week: slot},
	} {
		if week := settings.WeekStart(c.now); !week.Equal(c.week) {
			t.Errorf("week of %s = %s, want %s", c.now, week, c.week)
		}
	}
	if next := settings.NextWeekStart(time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)); !next.Equal(slot.AddDate(0, 0, 7)) {
		t.Fatalf("next week starts %s", next)
	}
}

// The same week continues where it stopped; a new week keeps only the cycle's
// table, so the next week resumes the scans where the budget ran out.
func TestDbBloatMeasureCursorStartsEachWeekAfresh(t *testing.T) {
	week := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	cursor := DbBloatMeasureCursor{Week: week, Table: "synthetic_c", ScanSpent: 80 * time.Minute, CatalogDone: true, ScanDone: true}
	if same := cursor.forWeek(week); same != cursor {
		t.Fatalf("the same week = %+v", same)
	}
	next := cursor.forWeek(week.AddDate(0, 0, 7))
	if next != (DbBloatMeasureCursor{Week: week.AddDate(0, 0, 7), Table: "synthetic_c"}) {
		t.Fatalf("the next week = %+v", next)
	}

	// the cursor rides in task arguments
	encoded, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var decoded DbBloatMeasureCursor
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded.Week.Equal(week) || decoded.Table != cursor.Table ||
		decoded.ScanSpent != cursor.ScanSpent || !decoded.CatalogDone || !decoded.ScanDone {
		t.Fatalf("round trip %s = %+v, %v", encoded, decoded, err)
	}
	var empty DbBloatMeasureCursor
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil || empty != (DbBloatMeasureCursor{}) {
		t.Fatalf("empty arguments = %+v, %v", empty, err)
	}
	if encoded, _ := json.Marshal(DbBloatMeasureCursor{}); string(encoded) != `{}` {
		t.Fatalf("a new chain's cursor encodes as %s", encoded)
	}
}

// A week whose catalog pass is recorded and whose scans are over does
// nothing more, and reads nothing: this test has no database.
func TestDbBloatMeasureSkipsAFinishedWeek(t *testing.T) {
	settings := DefaultDbBloatMeasureSettings()
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	cursor := DbBloatMeasureCursor{Week: settings.WeekStart(now), Table: "synthetic_c", ScanSpent: time.Hour, CatalogDone: true, ScanDone: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, next := measureDbBloat(ctx, now, cursor, settings, func() time.Time {
		t.Fatal("a finished week read the clock")
		return time.Time{}
	})
	if report.Stopped != "" || report.Recorded != 0 || next != cursor {
		t.Fatalf("a finished week ran: %+v, %+v", *report, next)
	}
}
