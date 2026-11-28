// The root cause of the 2026-11-17 STOP of the pending_task swap: on Main at
// 08:47Z its rehearsal measured a copy of 2.921 s and index builds of 1.662 s,
// and the swap ran both under its ACCESS EXCLUSIVE lock, so every writer would
// have queued behind it for about 4.6 s, past their own 2 s lock bound. The
// copy and the index builds belong before the lock; under it only the changes
// since the copy are applied and the names swapped (db_table_swap_capture.go).
package server

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var testTableSwapRehearsedCopyPattern = regexp.MustCompile(`rehearsal, rolled back: [0-9]+ rows copied in ([0-9.]+[a-zµ]+),? (?:and )?indexes built in ([0-9.]+[a-zµ]+)`)

var testTableSwapHeldPattern = regexp.MustCompile(`ACCESS EXCLUSIVE held ([0-9.]+[a-zµ]+) before the commit`)

// The durations a log line holds, by pattern, or fails.
func testTableSwapLoggedDurations(t testing.TB, lines []string, pattern *regexp.Regexp) []time.Duration {
	t.Helper()
	for _, line := range lines {
		if match := pattern.FindStringSubmatch(line); match != nil {
			durations := []time.Duration{}
			for _, text := range match[1:] {
				duration, err := time.ParseDuration(text)
				if err != nil {
					t.Fatal(line, err)
				}
				durations = append(durations, duration)
			}
			return durations
		}
	}
	t.Fatalf("no line matches %s", pattern)
	return nil
}

// PostgreSQL exposes this backend's pending relation counters, including work
// from earlier transactions which it has not yet reported. Hold the baseline
// transaction-locally before either lock and read its change on the same backend.
func testTableSwapLockWorkSql(source, target string) (baseline, measured string) {
	counts := fmt.Sprintf(`SELECT
            pg_stat_get_xact_tuples_inserted(%[1]s::regclass) AS inserted,
            pg_stat_get_xact_tuples_returned(%[2]s::regclass)
                + pg_stat_get_xact_tuples_fetched(%[2]s::regclass) AS rows_read`,
		tableSwapLiteral(target), tableSwapLiteral(source))
	baseline = fmt.Sprintf(`SELECT
            set_config('urnetwork.test_swap_inserted', inserted::text, true),
            set_config('urnetwork.test_swap_read', rows_read::text, true)
        FROM (%s) work`, counts)
	measured = fmt.Sprintf(`SELECT
            inserted - current_setting('urnetwork.test_swap_inserted')::bigint AS inserted,
            rows_read - current_setting('urnetwork.test_swap_read')::bigint AS rows_read
        FROM (%s) work`, counts)
	return
}

// Two transactions in one simple-query message leave the earlier work pending.
// The same measurement must report zero for that work, then exactly one new
// target insert and one source row read. The tables belong to this test session.
func testTableSwapPendingLockWork(t testing.TB, ctx context.Context) {
	t.Helper()
	const source = "pg_temp.test_swap_counter_source"
	const target = "pg_temp.test_swap_counter_target"
	baseline, measured := testTableSwapLockWorkSql(source, target)
	script := strings.Join([]string{
		"BEGIN",
		"CREATE TEMP TABLE test_swap_counter_source(n int)",
		"CREATE TEMP TABLE test_swap_counter_target(n int)",
		"INSERT INTO " + source + " VALUES (1), (2), (3)",
		"INSERT INTO " + target + " SELECT n FROM " + source,
		"COMMIT",
		"BEGIN",
		baseline,
		fmt.Sprintf(`SELECT set_config('urnetwork.test_swap_before_insert', inserted::text, true),
            set_config('urnetwork.test_swap_before_read', rows_read::text, true) FROM (%s) work`, measured),
		"INSERT INTO " + target + " SELECT n FROM " + source + " LIMIT 1",
		fmt.Sprintf(`SELECT current_setting('urnetwork.test_swap_inserted'),
            current_setting('urnetwork.test_swap_read'), current_setting('urnetwork.test_swap_before_insert'),
            current_setting('urnetwork.test_swap_before_read'), inserted::text, rows_read::text FROM (%s) work`, measured),
		"DROP TABLE " + source + ", " + target,
		"COMMIT",
	}, ";\n")
	conn, err := AcquireMaintenanceDbConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The first COMMIT leaves session-local tables. An unsuccessful batch
	// discards the session, including those tables and any open transaction.
	finished := false
	defer func() {
		if finished {
			conn.Release()
		} else {
			discardPgConnection(ctx, conn)
		}
	}()
	results, err := conn.Conn().PgConn().Exec(ctx, script).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var sample []string
	for _, result := range results {
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		if len(result.FieldDescriptions) == 6 && len(result.Rows) == 1 {
			for _, value := range result.Rows[0] {
				sample = append(sample, string(value))
			}
		}
	}
	finished = true
	if !slices.Equal(sample, []string{"3", "3", "0", "0", "1", "1"}) {
		t.Fatalf("TABLE_SWAP_BACKEND_PENDING_WORK baseline, earlier work attributed to lock, current work = %v; want [3 3 0 0 1 1]", sample)
	}
}

// Inside the locked transaction (afterCopy: after the changes since the copy
// are applied, before the renames), with no write since the copy, the
// transaction has inserted no row into the twin, built none of its indexes
// and read no row of the table, and the lock it reports holding is a small
// fraction of the copy and the index builds its rehearsal measured on the
// same rows. The swap of 11-16 inserted every row and built every index
// there, read every row of the table, and held its lock for at least the
// copy and the index builds.
func TestTableSwapHoldsItsLockOnlyForTheChangesSinceItsCopy(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPendingLockWork(t, ctx)
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 40000)
		rows := testTableSwapRows(t, ctx, testTableSwapTable)

		insertedUnderLock, indexesBuiltUnderLock, tableRowsReadUnderLock := int64(-1), int64(-1), int64(-1)
		lines := []string{}
		logf := func(format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			lines = append(lines, line)
			t.Logf("%s", line)
		}
		baseline, measured := testTableSwapLockWorkSql("public.test_table_swap_queue", "public.test_table_swap_queue_next")
		err := testTableSwap(t, ctx, TableSwapOptions{
			Logf:          logf,
			afterBeginSql: baseline,
			afterCopy: func(ctx context.Context, conn PgConn) {
				Raise(conn.QueryRow(ctx, fmt.Sprintf(`
                    SELECT work.inserted, work.rows_read, (
                        SELECT count(*) FROM pg_index i
                        JOIN pg_class index_class ON index_class.oid = i.indexrelid
                        WHERE i.indrelid = 'public.test_table_swap_queue_next'::regclass
                            AND index_class.xmin = pg_current_xact_id()::xid
                    )
                    FROM (%s) work`, measured)).Scan(&insertedUnderLock, &tableRowsReadUnderLock, &indexesBuiltUnderLock))
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if after := testTableSwapRows(t, ctx, testTableSwapTable); !maps.Equal(after, rows) {
			t.Fatalf("%d rows after the swap of %d, or a row changed", len(after), len(rows))
		}
		if len(testTableSwapOldTables(t, ctx)) != 1 {
			t.Fatal("the swap kept no old table")
		}
		if insertedUnderLock != 0 {
			t.Fatalf("the locked transaction inserted %d rows into the twin; with no write since the copy, none belongs under the lock", insertedUnderLock)
		}
		if indexesBuiltUnderLock != 0 {
			t.Fatalf("the locked transaction built %d of the twin's indexes; they belong before the lock", indexesBuiltUnderLock)
		}
		if tableRowsReadUnderLock != 0 {
			t.Fatalf("the locked transaction read %d rows of the bloated table; with no write since the copy, it reads none", tableRowsReadUnderLock)
		}
		rehearsed := testTableSwapLoggedDurations(t, lines, testTableSwapRehearsedCopyPattern)
		held := testTableSwapLoggedDurations(t, lines, testTableSwapHeldPattern)[0]
		if rehearsed[0]+rehearsed[1] <= 4*held {
			t.Fatalf("the swap held its lock %s, against a rehearsed copy of %s and index builds of %s: the lock must not cover them", held, rehearsed[0], rehearsed[1])
		}
	})
}

// Only the observed script gets the baseline, in its transaction before the
// first lock. Removing that one statement recovers the ordinary script exactly.
func TestTableSwapLockWorkBaselinePrecedesBothLocks(t *testing.T) {
	swap := &tableSwap{profile: tableSwapProfile(testTableSwapTable), settings: DefaultTableSwapSettings(), logf: t.Logf}
	shape := testTableSwapIndexedShape()
	script := tableSwapLockedScript{source: shape, capture: newTableSwapCapture(testTableSwapTable, shape.primaryKey),
		target: testTableSwapTable + tableSwapNextSuffix, stamp: "20261118120000", sourceFingerprint: "synthetic fingerprint"}
	before, after := swap.lockedSql(script)
	baseline, _ := testTableSwapLockWorkSql("public.test_table_swap_queue", "public.test_table_swap_queue_next")
	swap.afterBeginSql = baseline
	observed, observedAfter := swap.lockedSql(script)
	start := slices.Index(observed, baseline)
	firstLock := slices.IndexFunc(observed, func(statement string) bool { return strings.HasPrefix(statement, "LOCK TABLE ") })
	if observed[0] != "BEGIN ISOLATION LEVEL READ COMMITTED" || start < 1 || firstLock <= start ||
		!slices.Equal(after, observedAfter) || !slices.Equal(before, slices.Delete(observed, start, start+1)) {
		t.Fatal("counter baseline must be the only additional statement, before any relation lock")
	}
}
