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
		err := testTableSwap(t, ctx, TableSwapOptions{
			Logf: logf,
			afterCopy: func(ctx context.Context, conn PgConn) {
				Raise(conn.QueryRow(
					ctx,
					`SELECT pg_stat_get_xact_tuples_inserted(to_regclass('public.test_table_swap_queue_next'))`,
				).Scan(&insertedUnderLock))
				Raise(conn.QueryRow(
					ctx,
					`
					SELECT count(*)
					FROM pg_index i
					JOIN pg_class index_class ON index_class.oid = i.indexrelid
					WHERE i.indrelid = to_regclass('public.test_table_swap_queue_next')
						AND index_class.xmin = pg_current_xact_id()::xid
					`,
				).Scan(&indexesBuiltUnderLock))
				// rows of the table itself read in this transaction: by the
				// copy, and by the profile's locked check (which reads the
				// relation it is given; pending_task's reads nearly every row)
				Raise(conn.QueryRow(
					ctx,
					`SELECT pg_stat_get_xact_tuples_returned('public.test_table_swap_queue'::regclass) + pg_stat_get_xact_tuples_fetched('public.test_table_swap_queue'::regclass)`,
				).Scan(&tableRowsReadUnderLock))
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
