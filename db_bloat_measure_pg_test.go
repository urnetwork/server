// Database tests of the weekly bloat measurement (db_bloat_measure.go) on the
// PostgreSQL 18 TestEnv: known dead rows through the statistics estimate and
// through a scan, a bloated index, the scan budget resumed the next week, the
// fallbacks for a missing privilege, a refused scan and a slow one, a load
// deferral, an interrupted scan, and retention. The test role is not a
// superuser and cannot create the untrusted pgstattuple extension, so the
// scan tests install a stand-in with pgstattuple_approx's exact interface; the
// statistics tests run without one, as on a database where the extension is
// missing. Every wait is a barrier on observed PostgreSQL state.
package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A stand-in for pgstattuple_approx(regclass) with the extension's columns. It
// counts live tuples and their bytes with SQL, takes dead tuples from the
// statistics counter at the live tuples' width, and leaves as free space what
// the page headers, line pointers and tuples do not use. Each call records
// its session's timeouts. A fault row makes the call for its relation sleep
// past any test bound, wait for advisory lock 4242, or refuse as an
// unprivileged call to the extension would.
const testDbBloatScanStubSql = `
	CREATE TABLE synthetic_bloat_scan_fault (
		relation_name text NOT NULL PRIMARY KEY,
		fault text NOT NULL
	);
	CREATE TABLE synthetic_bloat_scan_call (
		relation_name text NOT NULL,
		transaction_timeout text NOT NULL,
		statement_timeout text NOT NULL
	);
	CREATE FUNCTION public.pgstattuple_approx(
		reloid regclass,
		OUT table_len bigint,
		OUT scanned_percent double precision,
		OUT approx_tuple_count bigint,
		OUT approx_tuple_len bigint,
		OUT approx_tuple_percent double precision,
		OUT dead_tuple_count bigint,
		OUT dead_tuple_len bigint,
		OUT dead_tuple_percent double precision,
		OUT approx_free_space bigint,
		OUT approx_free_percent double precision
	) LANGUAGE plpgsql AS $stub$
	DECLARE
		relation_fault text;
		tuple_width bigint;
	BEGIN
		SELECT synthetic_bloat_scan_fault.fault INTO relation_fault
		FROM synthetic_bloat_scan_fault
		WHERE synthetic_bloat_scan_fault.relation_name = reloid::text;
		IF relation_fault = 'sleep' THEN
			PERFORM pg_sleep(300);
		ELSIF relation_fault = 'block' THEN
			PERFORM pg_advisory_xact_lock(4242);
		ELSIF relation_fault = 'refuse' THEN
			RAISE EXCEPTION 'permission denied for function pgstattuple_approx' USING ERRCODE = 'insufficient_privilege';
		END IF;
		INSERT INTO synthetic_bloat_scan_call (relation_name, transaction_timeout, statement_timeout)
		VALUES (reloid::text, current_setting('transaction_timeout'), current_setting('statement_timeout'));
		table_len := pg_relation_size(reloid);
		scanned_percent := 100;
		EXECUTE format('SELECT count(*), coalesce(sum(pg_column_size(t.*)), 0) FROM %s t', reloid)
		INTO approx_tuple_count, approx_tuple_len;
		dead_tuple_count := pg_stat_get_dead_tuples(reloid);
		tuple_width := CASE WHEN approx_tuple_count = 0 THEN 0 ELSE approx_tuple_len / approx_tuple_count END;
		dead_tuple_len := dead_tuple_count * tuple_width;
		approx_free_space := greatest(
			0,
			table_len - (table_len / 8192) * 24 - (approx_tuple_count + dead_tuple_count) * 4 - approx_tuple_len - dead_tuple_len
		);
		approx_tuple_percent := CASE WHEN table_len = 0 THEN 0 ELSE 100.0 * approx_tuple_len / table_len END;
		dead_tuple_percent := CASE WHEN table_len = 0 THEN 0 ELSE 100.0 * dead_tuple_len / table_len END;
		approx_free_percent := CASE WHEN table_len = 0 THEN 0 ELSE 100.0 * approx_free_space / table_len END;
	END
	$stub$;
`

// Rows in each fixed-width fixture: 200 heap pages at fillfactor 100.
const testDbBloatFixtureRowCount = 24000

// The fixture's heap and primary key are measured; the measurement table and
// the stand-in's tables stay under the line.
const testDbBloatFixtureMinRelationBytes = int64(256 * 1024)

// Settings for the scan fixtures: their relations are measured and every scan
// statement shows a recognizable bound.
func testDbBloatScanSettings() *DbBloatMeasureSettings {
	settings := DefaultDbBloatMeasureSettings()
	settings.MinRelationBytes = testDbBloatFixtureMinRelationBytes
	settings.MaxScanDuration = 7 * time.Second
	return settings
}

func testDbBloatExec(t testing.TB, ctx context.Context, sql string, args ...any) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, sql, args...))
	}, OptReadWrite(), OptNoRetry())
}

// The measurement table its migration creates, for fixtures without the
// migrations.
func testDbBloatCreateMeasurementTable(t testing.TB, ctx context.Context) {
	testDbBloatExec(t, ctx, dbBloatMeasurementTableSql)
}

// A table of fixed-width columns (bigint, bigint, timestamp, uuid) holding
// rowCount rows, with autovacuum off so only the test vacuums it. The
// inserting session flushes its statistics, so the live tuple counter holds
// the rows when the next statement reads it.
func testDbBloatCreateFixedTable(t testing.TB, ctx context.Context, tableName string, rowCount int, fillfactor int) {
	name := pgx.Identifier{"public", tableName}.Sanitize()
	MaintenanceDb(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			CREATE TABLE %s (
				id bigint NOT NULL PRIMARY KEY,
				value bigint NOT NULL,
				create_time timestamp NOT NULL,
				token uuid NOT NULL
			) WITH (fillfactor = %d, autovacuum_enabled = false)
		`, name, fillfactor)))
		RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (id, value, create_time, token)
			SELECT n, n * 7, timestamp '2026-10-01' + n * interval '1 second', md5(n::text)::uuid
			FROM generate_series(1, $1::bigint) n
		`, name), rowCount))
		RaisePgResult(conn.Exec(ctx, `SELECT pg_stat_force_next_flush()`))
	}, OptReadWrite(), OptNoRetry())
}

// Deletes every row whose id is not a multiple of keepEvery, then flushes the
// deleting session's statistics, so the dead tuple counter holds exactly the
// deleted rows when the next statement reads it.
func testDbBloatDeleteAllBut(t testing.TB, ctx context.Context, tableName string, keepEvery int) {
	MaintenanceDb(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id %% $1 <> 0`, pgx.Identifier{"public", tableName}.Sanitize()), keepEvery))
		RaisePgResult(conn.Exec(ctx, `SELECT pg_stat_force_next_flush()`))
	}, OptReadWrite(), OptNoRetry())
}

func testDbBloatAnalyze(t testing.TB, ctx context.Context, tableName string) {
	testDbBloatExec(t, ctx, `ANALYZE `+pgx.Identifier{"public", tableName}.Sanitize())
}

func testDbBloatCreateScanStub(t testing.TB, ctx context.Context) {
	testDbBloatExec(t, ctx, testDbBloatScanStubSql)
}

func testDbBloatSetScanFault(t testing.TB, ctx context.Context, tableName string, fault string) {
	testDbBloatExec(t, ctx, `
		INSERT INTO synthetic_bloat_scan_fault (relation_name, fault) VALUES ($1, $2)
		ON CONFLICT (relation_name) DO UPDATE SET fault = EXCLUDED.fault
	`, tableName, fault)
}

func testDbBloatClearScanFaults(t testing.TB, ctx context.Context) {
	testDbBloatExec(t, ctx, `DELETE FROM synthetic_bloat_scan_fault`)
}

// One recorded measurement.
type testDbBloatRow struct {
	measureTime       time.Time
	tableName         string
	relationKind      string
	method            string
	fallbackReason    *string
	relationBytes     int64
	reclaimableBytes  *int64
	compactBytes      *int64
	fillfactor        int
	reservedBytes     *int64
	scannedPercent    *float64
	liveTupleCount    *int64
	liveTupleBytes    *int64
	deadTupleCount    *int64
	deadTupleBytes    *int64
	freeBytes         *int64
	estimatedRowCount *float64
	liveRowCount      *int64
	deadRowCount      *int64
	scanDurationMs    *int64
}

func (self testDbBloatRow) reason() string {
	if self.fallbackReason == nil {
		return ""
	}
	return *self.fallbackReason
}

// The measurements of one relation, oldest first.
func testDbBloatRows(t testing.TB, ctx context.Context, relationSchema string, relationName string) []testDbBloatRow {
	t.Helper()
	rows := []testDbBloatRow{}
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `
			SELECT
				measure_time,
				table_name,
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
			FROM db_bloat_measurement
			WHERE relation_schema = $1 AND relation_name = $2
			ORDER BY measure_time, measurement_id
		`, relationSchema, relationName)
		WithPgResult(result, err, func() {
			for result.Next() {
				var row testDbBloatRow
				Raise(result.Scan(
					&row.measureTime,
					&row.tableName,
					&row.relationKind,
					&row.method,
					&row.fallbackReason,
					&row.relationBytes,
					&row.reclaimableBytes,
					&row.compactBytes,
					&row.fillfactor,
					&row.reservedBytes,
					&row.scannedPercent,
					&row.liveTupleCount,
					&row.liveTupleBytes,
					&row.deadTupleCount,
					&row.deadTupleBytes,
					&row.freeBytes,
					&row.estimatedRowCount,
					&row.liveRowCount,
					&row.deadRowCount,
					&row.scanDurationMs,
				))
				rows = append(rows, row)
			}
		})
	}, OptNoRetry())
	return rows
}

// The one measurement of a relation.
func testDbBloatOnlyRow(t testing.TB, ctx context.Context, relationSchema string, relationName string) testDbBloatRow {
	t.Helper()
	rows := testDbBloatRows(t, ctx, relationSchema, relationName)
	if len(rows) != 1 {
		t.Fatalf("%s.%s has %d measurements, want 1: %+v", relationSchema, relationName, len(rows), rows)
	}
	return rows[0]
}

// The stand-in's calls, by relation.
func testDbBloatScanCalls(t testing.TB, ctx context.Context) map[string][]string {
	t.Helper()
	calls := map[string][]string{}
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `SELECT relation_name, transaction_timeout, statement_timeout FROM synthetic_bloat_scan_call`)
		WithPgResult(result, err, func() {
			for result.Next() {
				var relationName string
				var transactionTimeout string
				var statementTimeout string
				Raise(result.Scan(&relationName, &transactionTimeout, &statementTimeout))
				calls[relationName] = append(calls[relationName], transactionTimeout+" "+statementTimeout)
			}
		})
	}, OptNoRetry())
	return calls
}

// A clock that moves step forward at every reading, so the scan budget spends
// step per scan statement.
func testDbBloatSteppingClock(step time.Duration) func() time.Time {
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		at = at.Add(step)
		return at
	}
}

// A week's first run with the stand-in installed, at a fixed weekday.
func testDbBloatNow() time.Time {
	return time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
}

// The plan's evidence counted dead rows only through the statistics counter,
// and a small table's dead rows never reach the §2.4 floor. Without
// pgstattuple the heap gets the statistics estimate, which packs the live
// rows the way PostgreSQL does: 6,000 rows of 200 heap pages fit in 50.
func TestDbBloatMeasureRecordsKnownDeadRowsWithoutPgstattuple(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_bloat_dead"
		testDbBloatCreateFixedTable(t, ctx, tableName, testDbBloatFixtureRowCount, 100)
		testDbBloatDeleteAllBut(t, ctx, tableName, 4)
		testDbBloatAnalyze(t, ctx, tableName)

		report, cursor := MeasureDbBloat(ctx, testDbBloatNow(), DbBloatMeasureCursor{}, DefaultDbBloatMeasureSettings())
		if report.Stopped != "" || !cursor.CatalogDone || !cursor.ScanDone || report.Scanned != 0 {
			t.Fatalf("a run without pgstattuple = %+v, cursor %+v", *report, cursor)
		}
		heap := testDbBloatOnlyRow(t, ctx, "public", tableName)
		heapBytes := testDbMaintenanceRelationBytes(ctx, tableName)
		if heap.method != dbBloatMethodStatistics || heap.reason() != dbBloatFallbackNotInstalled || heap.relationKind != dbBloatRelationHeap {
			t.Fatalf("heap measured by %s (%s)", heap.method, heap.reason())
		}
		if heap.relationBytes != heapBytes || *heap.compactBytes != 50*testDbBloatBlockSize || *heap.reclaimableBytes != heapBytes-50*testDbBloatBlockSize {
			t.Fatalf("heap of %d bytes estimated %v compact, %v reclaimable", heapBytes, *heap.compactBytes, *heap.reclaimableBytes)
		}
		if share := float64(*heap.reclaimableBytes) / float64(heapBytes); share < 0.7 || 0.8 < share {
			t.Fatalf("three quarters of the rows are dead, but the estimate reclaims %.2f of the heap", share)
		}
		if *heap.deadRowCount != 18000 || *heap.liveRowCount != 6000 || *heap.estimatedRowCount != 6000 {
			t.Fatalf("dead rows %v, live rows %v, estimated rows %v", *heap.deadRowCount, *heap.liveRowCount, *heap.estimatedRowCount)
		}
	})
}

// With pgstattuple_approx the heap row keeps the scan's dead tuples and leaves
// out the free space a fillfactor of 50 keeps on purpose. The scan ran on a
// session exempt from the database's transaction_timeout, under its own bound.
func TestDbBloatMeasureScansKnownDeadRowsLessTheFillfactorReserve(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_bloat_fill"
		testDbBloatCreateMeasurementTable(t, ctx)
		testDbBloatCreateScanStub(t, ctx)
		testDbBloatCreateFixedTable(t, ctx, tableName, testDbBloatFixtureRowCount, 50)
		testDbBloatDeleteAllBut(t, ctx, tableName, 4)
		// every new session of this database starts with a transaction bound
		testDbBloatExec(t, ctx, `
			DO $$
			BEGIN
				EXECUTE format('ALTER DATABASE %I SET transaction_timeout = %L', current_database(), '10min');
			END
			$$
		`)
		PgReset()

		report, cursor := MeasureDbBloat(ctx, testDbBloatNow(), DbBloatMeasureCursor{}, testDbBloatScanSettings())
		if report.Stopped != "" || !report.CycleComplete || report.Scanned != 1 || cursor.Table != "" || !cursor.ScanDone {
			t.Fatalf("scan run = %+v, cursor %+v", *report, cursor)
		}
		heap := testDbBloatOnlyRow(t, ctx, "public", tableName)
		if heap.method != dbBloatMethodScan || heap.reason() != "" || heap.fillfactor != 50 || *heap.scannedPercent != 100 || heap.scanDurationMs == nil {
			t.Fatalf("heap row = %+v", heap)
		}
		if *heap.deadTupleCount != 18000 || *heap.liveTupleCount != 6000 || *heap.deadRowCount != 18000 || *heap.liveRowCount != 6000 {
			t.Fatalf("dead tuples %v (counter %v), live tuples %v (counter %v)", *heap.deadTupleCount, *heap.deadRowCount, *heap.liveTupleCount, *heap.liveRowCount)
		}
		// at fillfactor 50 a rewrite keeps as many free bytes as live ones
		if *heap.reservedBytes != *heap.liveTupleBytes || *heap.reclaimableBytes != *heap.deadTupleBytes+*heap.freeBytes-*heap.reservedBytes {
			t.Fatalf("reserved %v of %v live bytes, reclaimable %v of %v dead and %v free",
				*heap.reservedBytes, *heap.liveTupleBytes, *heap.reclaimableBytes, *heap.deadTupleBytes, *heap.freeBytes)
		}
		calls := testDbBloatScanCalls(t, ctx)
		if len(calls) != 1 || len(calls[tableName]) != 1 || calls[tableName][0] != "0 7s" {
			t.Fatalf("scan sessions (transaction_timeout statement_timeout): %v", calls)
		}
		// the primary key, never scanned, is measured by the catalog pass
		if index := testDbBloatOnlyRow(t, ctx, "public", tableName+"_pkey"); index.relationKind != dbBloatRelationIndex || index.tableName != tableName {
			t.Fatalf("primary key row = %+v", index)
		}
	})
}

// A churned queue keeps every index page that still holds one live entry, so
// its index is many times its compact estimate even after vacuum. A rebuild
// brings it under the line the next week.
func TestDbBloatMeasureFindsABloatedIndex(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_bloat_queue"
		const indexName = "synthetic_bloat_queue_available_at"
		testDbBloatCreateMeasurementTable(t, ctx)
		testDbBloatExec(t, ctx, `
			CREATE TABLE synthetic_bloat_queue (
				task_id bigint NOT NULL PRIMARY KEY,
				available_at timestamp NOT NULL,
				payload bigint NOT NULL
			) WITH (autovacuum_enabled = false);
			CREATE INDEX synthetic_bloat_queue_available_at ON synthetic_bloat_queue (available_at)
		`)
		// time-ordered keys land at the right edge; one in fifty stays queued
		testDbBloatExec(t, ctx, `
			INSERT INTO synthetic_bloat_queue (task_id, available_at, payload)
			SELECT n, timestamp '2026-10-01' + n * interval '1 second', n
			FROM generate_series(1, 200000) n
		`)
		testDbBloatExec(t, ctx, `DELETE FROM synthetic_bloat_queue WHERE task_id % 50 <> 0`)
		testDbMaintenanceVacuumAnalyze(ctx, tableName)

		now := testDbBloatNow()
		settings := DefaultDbBloatMeasureSettings()
		// the rebuilt index is a few pages
		settings.MinRelationBytes = testDbBloatBlockSize
		if report, _ := MeasureDbBloat(ctx, now, DbBloatMeasureCursor{}, settings); report.Stopped != "" {
			t.Fatalf("run = %+v", *report)
		}
		index := testDbBloatOnlyRow(t, ctx, "public", indexName)
		indexBytes := testDbMaintenanceRelationBytes(ctx, indexName)
		if index.method != dbBloatMethodBtreeEstimate || index.relationBytes != indexBytes || *index.estimatedRowCount != 4000 || *index.compactBytes <= 0 {
			t.Fatalf("index row = %+v (index %d bytes)", index, indexBytes)
		}
		if share := float64(*index.reclaimableBytes) / float64(indexBytes); share < 0.9 {
			t.Fatalf("the churned index is %d bytes against a %d byte estimate: share %.2f", indexBytes, *index.compactBytes, share)
		}
		// vacuum made the dead heap space reusable, not returned
		if heap := testDbBloatOnlyRow(t, ctx, "public", tableName); heap.method != dbBloatMethodStatistics ||
			float64(*heap.reclaimableBytes) < 0.9*float64(heap.relationBytes) {
			t.Fatalf("heap row = %+v", heap)
		}

		testDbBloatExec(t, ctx, `REINDEX INDEX synthetic_bloat_queue_available_at`)
		testDbMaintenanceVacuumAnalyze(ctx, tableName)
		if report, _ := MeasureDbBloat(ctx, now.AddDate(0, 0, 7), DbBloatMeasureCursor{}, settings); report.Stopped != "" {
			t.Fatalf("next week's run = %+v", *report)
		}
		rows := testDbBloatRows(t, ctx, "public", indexName)
		rebuilt := rows[len(rows)-1]
		if len(rows) != 2 || rebuilt.relationBytes != testDbMaintenanceRelationBytes(ctx, indexName) ||
			0.5 <= float64(*rebuilt.reclaimableBytes)/float64(rebuilt.relationBytes) {
			t.Fatalf("after the rebuild: %+v", rows)
		}
	})
}

// A run that spends the week's budget leaves the cursor at its next table; a
// rerun the same week does nothing; the next week continues there and
// completes the cycle without scanning the earlier tables again.
func TestDbBloatMeasureResumesNextWeekWhereTheBudgetRanOut(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		testDbBloatCreateMeasurementTable(t, ctx)
		testDbBloatCreateScanStub(t, ctx)
		tableNames := []string{"synthetic_bloat_a", "synthetic_bloat_b", "synthetic_bloat_c"}
		for _, tableName := range tableNames {
			testDbBloatCreateFixedTable(t, ctx, tableName, testDbBloatFixtureRowCount, 100)
		}
		settings := testDbBloatScanSettings()
		settings.ScanBudget = time.Hour
		settings.MaxScanDuration = 10 * time.Minute
		// each scan statement spends 40 minutes: two fit in the hour
		clock := testDbBloatSteppingClock(40 * time.Minute)
		now := testDbBloatNow()
		scans := func() map[string]int {
			counts := map[string]int{}
			for _, tableName := range tableNames {
				for _, row := range testDbBloatRows(t, ctx, "public", tableName) {
					if row.method == dbBloatMethodScan {
						counts[tableName] += 1
					}
				}
			}
			return counts
		}

		report, cursor := measureDbBloat(ctx, now, DbBloatMeasureCursor{}, settings, clock)
		if report.Stopped != "" || report.CycleComplete || report.Scanned != 2 {
			t.Fatalf("first week's run = %+v", *report)
		}
		if cursor.Table != "synthetic_bloat_c" || !cursor.CatalogDone || !cursor.ScanDone || cursor.ScanSpent != 80*time.Minute {
			t.Fatalf("first week's cursor = %+v", cursor)
		}
		if counts := scans(); counts["synthetic_bloat_a"] != 1 || counts["synthetic_bloat_b"] != 1 || counts["synthetic_bloat_c"] != 0 {
			t.Fatalf("first week's scans = %v", counts)
		}
		indexRows := len(testDbBloatRows(t, ctx, "public", "synthetic_bloat_a_pkey"))

		// a rerun of the same week has nothing left to do
		again, againCursor := measureDbBloat(ctx, now.Add(time.Hour), cursor, settings, clock)
		if again.Recorded != 0 || againCursor != cursor {
			t.Fatalf("a rerun of the week = %+v, cursor %+v", *again, againCursor)
		}

		nextReport, nextCursor := measureDbBloat(ctx, now.AddDate(0, 0, 7), cursor, settings, clock)
		if nextReport.Stopped != "" || !nextReport.CycleComplete || nextReport.Scanned != 1 {
			t.Fatalf("next week's run = %+v", *nextReport)
		}
		if nextCursor.Table != "" || !nextCursor.ScanDone || nextCursor.ScanSpent != 40*time.Minute {
			t.Fatalf("next week's cursor = %+v", nextCursor)
		}
		if counts := scans(); counts["synthetic_bloat_a"] != 1 || counts["synthetic_bloat_b"] != 1 || counts["synthetic_bloat_c"] != 1 {
			t.Fatalf("scans after the next week = %v", counts)
		}
		// the next week records its own catalog pass
		if rows := len(testDbBloatRows(t, ctx, "public", "synthetic_bloat_a_pkey")); rows != indexRows+1 {
			t.Fatalf("the primary key has %d measurements after two weeks, had %d after one", rows, indexRows)
		}
	})
}

// Without EXECUTE on pgstattuple_approx, as the extension's grant to
// pg_stat_scan_tables leaves a role outside it, every heap gets the
// statistics estimate and the reason, and nothing is scanned.
func TestDbBloatMeasureFallsBackWithoutTheScanPrivilege(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_bloat_unprivileged"
		testDbBloatCreateMeasurementTable(t, ctx)
		testDbBloatCreateScanStub(t, ctx)
		testDbBloatExec(t, ctx, `
			REVOKE EXECUTE ON FUNCTION public.pgstattuple_approx(regclass) FROM PUBLIC;
			REVOKE EXECUTE ON FUNCTION public.pgstattuple_approx(regclass) FROM CURRENT_USER;
			GRANT EXECUTE ON FUNCTION public.pgstattuple_approx(regclass) TO pg_stat_scan_tables
		`)
		testDbBloatCreateFixedTable(t, ctx, tableName, testDbBloatFixtureRowCount, 100)
		testDbBloatAnalyze(t, ctx, tableName)

		report, cursor := MeasureDbBloat(ctx, testDbBloatNow(), DbBloatMeasureCursor{}, testDbBloatScanSettings())
		if report.Stopped != "" || report.Scanned != 0 || !cursor.ScanDone || report.Fallbacks[dbBloatFallbackNoExecute] != 1 {
			t.Fatalf("run without the privilege = %+v, cursor %+v", *report, cursor)
		}
		heap := testDbBloatOnlyRow(t, ctx, "public", tableName)
		if heap.method != dbBloatMethodStatistics || heap.reason() != dbBloatFallbackNoExecute || heap.reclaimableBytes == nil {
			t.Fatalf("heap row = %+v", heap)
		}
		if calls := testDbBloatScanCalls(t, ctx); len(calls) != 0 {
			t.Fatalf("the unprivileged run called the scan: %v", calls)
		}
	})
}

// A scan refused at call time, though the session holds EXECUTE, ends the
// scans for the cycle: the refused table and every later one get the
// statistics estimate with the reason.
func TestDbBloatMeasureFallsBackWhenAScanIsRefused(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		testDbBloatCreateMeasurementTable(t, ctx)
		testDbBloatCreateScanStub(t, ctx)
		tableNames := []string{"synthetic_bloat_a", "synthetic_bloat_b", "synthetic_bloat_c"}
		for _, tableName := range tableNames {
			testDbBloatCreateFixedTable(t, ctx, tableName, testDbBloatFixtureRowCount, 100)
			testDbBloatAnalyze(t, ctx, tableName)
		}
		testDbBloatSetScanFault(t, ctx, "synthetic_bloat_a", "refuse")

		report, cursor := MeasureDbBloat(ctx, testDbBloatNow(), DbBloatMeasureCursor{}, testDbBloatScanSettings())
		if report.Stopped != "" || !report.CycleComplete || report.Scanned != 0 || report.Fallbacks[dbBloatFallbackRefused] != 3 || cursor.Table != "" {
			t.Fatalf("refused run = %+v, cursor %+v", *report, cursor)
		}
		for _, tableName := range tableNames {
			if heap := testDbBloatOnlyRow(t, ctx, "public", tableName); heap.method != dbBloatMethodStatistics || heap.reason() != dbBloatFallbackRefused {
				t.Fatalf("%s row = %+v", tableName, heap)
			}
		}
		if calls := testDbBloatScanCalls(t, ctx); len(calls) != 0 {
			t.Fatalf("scans after the refusal: %v", calls)
		}
	})
}

// A scan that outlasts its bound gets the statistics estimate, and the next
// table is scanned.
func TestDbBloatMeasureFallsBackWhenAScanOutlastsItsBound(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		testDbBloatCreateMeasurementTable(t, ctx)
		testDbBloatCreateScanStub(t, ctx)
		for _, tableName := range []string{"synthetic_bloat_a", "synthetic_bloat_b"} {
			testDbBloatCreateFixedTable(t, ctx, tableName, testDbBloatFixtureRowCount, 100)
			testDbBloatAnalyze(t, ctx, tableName)
		}
		// the stand-in sleeps for minutes; the bound ends it
		testDbBloatSetScanFault(t, ctx, "synthetic_bloat_a", "sleep")
		settings := testDbBloatScanSettings()
		settings.MaxScanDuration = 500 * time.Millisecond

		report, cursor := MeasureDbBloat(ctx, testDbBloatNow(), DbBloatMeasureCursor{}, settings)
		if report.Stopped != "" || !report.CycleComplete || report.Scanned != 1 || report.Fallbacks[dbBloatFallbackScanTimeout] != 1 || cursor.Table != "" {
			t.Fatalf("run with a slow scan = %+v, cursor %+v", *report, cursor)
		}
		if heap := testDbBloatOnlyRow(t, ctx, "public", "synthetic_bloat_a"); heap.method != dbBloatMethodStatistics || heap.reason() != dbBloatFallbackScanTimeout {
			t.Fatalf("slow table row = %+v", heap)
		}
		if heap := testDbBloatOnlyRow(t, ctx, "public", "synthetic_bloat_b"); heap.method != dbBloatMethodScan {
			t.Fatalf("next table row = %+v", heap)
		}
	})
}

// Under load the run defers before its first scan with the catalog pass
// recorded; the continuation the same week scans without repeating it.
func TestDbBloatMeasureDefersUnderLoadAndContinuesTheWeek(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_bloat_a"
		testDbBloatCreateMeasurementTable(t, ctx)
		testDbBloatCreateScanStub(t, ctx)
		testDbBloatCreateFixedTable(t, ctx, tableName, testDbBloatFixtureRowCount, 100)
		testDbBloatAnalyze(t, ctx, tableName)
		testDbBloatExec(t, ctx, `CREATE TABLE synthetic_bloat_writer (id bigint PRIMARY KEY)`)

		// the holder's transaction is older than this bound
		release := testDbMaintenanceHoldWriterLock(t, ctx, "synthetic_bloat_writer")
		settings := testDbBloatScanSettings()
		settings.MaxTransactionAge = time.Millisecond
		now := testDbBloatNow()
		report, cursor := MeasureDbBloat(ctx, now, DbBloatMeasureCursor{}, settings)
		release()
		if report.Stopped != DbBloatMeasureDeferred || report.Scanned != 0 || cursor.Table != tableName || !cursor.CatalogDone || cursor.ScanDone {
			t.Fatalf("run under load = %+v, cursor %+v", *report, cursor)
		}
		indexRows := len(testDbBloatRows(t, ctx, "public", tableName+"_pkey"))
		if indexRows != 1 {
			t.Fatalf("the deferred run recorded %d catalog measurements of the primary key", indexRows)
		}

		continued, continuedCursor := MeasureDbBloat(ctx, now.Add(time.Hour), cursor, testDbBloatScanSettings())
		if continued.Stopped != "" || continued.Scanned != 1 || !continuedCursor.ScanDone || continuedCursor.Table != "" {
			t.Fatalf("continuation = %+v, cursor %+v", *continued, continuedCursor)
		}
		if rows := len(testDbBloatRows(t, ctx, "public", tableName+"_pkey")); rows != 1 {
			t.Fatalf("the continuation repeated the catalog pass: %d primary key measurements", rows)
		}
	})
}

// A drain that cancels a scan leaves the cursor at its table, and the
// continuation scans it.
func TestDbBloatMeasureResumesAnInterruptedScan(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		testDbBloatCreateMeasurementTable(t, ctx)
		testDbBloatCreateScanStub(t, ctx)
		for _, tableName := range []string{"synthetic_bloat_a", "synthetic_bloat_b"} {
			testDbBloatCreateFixedTable(t, ctx, tableName, testDbBloatFixtureRowCount, 100)
		}
		testDbBloatSetScanFault(t, ctx, "synthetic_bloat_b", "block")
		holder, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer discardPgConnection(ctx, holder)
		RaisePgResult(holder.Exec(ctx, `SELECT pg_advisory_lock(4242)`))

		settings := testDbBloatScanSettings()
		settings.ScanLockTimeout = testDbMaintenanceBarrierTimeout
		now := testDbBloatNow()
		runCtx, runCancel := context.WithCancel(ctx)
		defer runCancel()
		done := make(chan struct{})
		var report *DbBloatMeasureReport
		var cursor DbBloatMeasureCursor
		go func() {
			defer close(done)
			report, cursor = MeasureDbBloat(runCtx, now, DbBloatMeasureCursor{}, settings)
		}()

		// the scan of synthetic_bloat_b waits for the advisory lock
		deadline := time.Now().Add(testDbMaintenanceBarrierTimeout)
		for {
			var waiting bool
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `
					SELECT EXISTS (
						SELECT 1 FROM pg_locks
						WHERE locktype = 'advisory' AND NOT granted AND classid = 0 AND objid = 4242
					)
				`).Scan(&waiting))
			}, OptNoRetry())
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the scan never waited for the advisory lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
		runCancel()
		<-done
		RaisePgResult(holder.Exec(ctx, `SELECT pg_advisory_unlock(4242)`))
		if report.Stopped != DbBloatMeasureInterrupted || report.Scanned != 1 || cursor.Table != "synthetic_bloat_b" || !cursor.CatalogDone || cursor.ScanDone {
			t.Fatalf("interrupted run = %+v, cursor %+v", *report, cursor)
		}
		if rows := testDbBloatRows(t, ctx, "public", "synthetic_bloat_b"); len(rows) != 0 {
			t.Fatalf("the interrupted scan recorded %+v", rows)
		}

		testDbBloatClearScanFaults(t, ctx)
		continued, continuedCursor := MeasureDbBloat(ctx, now.Add(5*time.Minute), cursor, settings)
		if continued.Stopped != "" || continued.Scanned != 1 || !continued.CycleComplete || continuedCursor.Table != "" {
			t.Fatalf("continuation = %+v, cursor %+v", *continued, continuedCursor)
		}
		if heap := testDbBloatOnlyRow(t, ctx, "public", "synthetic_bloat_b"); heap.method != dbBloatMethodScan {
			t.Fatalf("continued scan row = %+v", heap)
		}
		if rows := testDbBloatRows(t, ctx, "public", "synthetic_bloat_a"); len(rows) != 1 {
			t.Fatalf("the continuation rescanned the finished table: %+v", rows)
		}
	})
}

// A week's first run deletes the measurements older than the retention, a
// batch at a time, and keeps the rest.
func TestDbBloatMeasureRemovesExpiredMeasurements(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := testDbBloatNow()
		insert := func(relationName string, measureTime time.Time) {
			testDbBloatExec(t, ctx, `
				INSERT INTO db_bloat_measurement (
					measurement_id, measure_time, table_name, relation_schema, relation_name,
					relation_kind, method, relation_bytes, reclaimable_bytes, fillfactor
				) VALUES ($1, $2, $3, 'public', $3, 'heap', 'statistics', 8192, 0, 100)
			`, NewId(), measureTime, relationName)
		}
		for i := range 5 {
			insert("synthetic_expired", now.Add(-91*24*time.Hour-time.Duration(i)*time.Hour))
		}
		insert("synthetic_kept", now.Add(-89*24*time.Hour))
		insert("synthetic_kept", now.Add(-24*time.Hour))

		settings := DefaultDbBloatMeasureSettings()
		settings.RetentionBatchSize = 2
		report, _ := MeasureDbBloat(ctx, now, DbBloatMeasureCursor{}, settings)
		if report.Removed != 5 {
			t.Fatalf("removed %d expired measurements, want 5", report.Removed)
		}
		if expired := testDbBloatRows(t, ctx, "public", "synthetic_expired"); len(expired) != 0 {
			t.Fatalf("expired measurements remain: %+v", expired)
		}
		if kept := testDbBloatRows(t, ctx, "public", "synthetic_kept"); len(kept) != 2 {
			t.Fatalf("kept %d measurements, want 2", len(kept))
		}
	})
}
