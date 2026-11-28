// Tests of VacuumHotTables' index pass (PARTITION-MAINTENANCE-PLAN.md A2), and
// of the transaction timeout exemption every rebuild runs under. The database
// fixtures churn a table the way the queues churn: all but one row in fifty
// go, so every leaf page keeps a few live entries and vacuum frees none of
// them. They reuse the lock holders and state barriers of
// db_maintenance_debris_test.go and db_maintenance_skip_locked_test.go.
package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// An index is rebuilt at twice its compact estimate with 64 MiB to reclaim, on
// a heap a run can read twice; the rebuilds go most reclaimable first and stop
// at the per-pass bound. An index without an estimate, and a priority index,
// which keeps its opt-in, are never selected.
func TestDbHotVacuumSelectsBloatedIndexesMostReclaimFirst(t *testing.T) {
	const mib = int64(1024 * 1024)
	settings := DefaultDbHotVacuumSettings()
	indexes := []dbMaintenancePriorityIndex{
		// exactly at both bounds
		{indexName: "synthetic_boundary", indexBytes: 128 * mib, compactBytes: 64 * mib, heapBytes: mib},
		// far past the factor, below the reclaim floor
		{indexName: "synthetic_tiny", indexBytes: 63*mib + 8192, compactBytes: 8192, heapBytes: mib},
		// below the factor
		{indexName: "synthetic_compact", indexBytes: 190 * mib, compactBytes: 100 * mib, heapBytes: mib},
		// not a btree, or never vacuumed or analyzed
		{indexName: "synthetic_unknown", indexBytes: 10000 * mib, heapBytes: mib},
		{indexName: "synthetic_large_heap", indexBytes: 4000 * mib, compactBytes: 10 * mib, heapBytes: settings.IndexMaxHeapBytes + 1},
		{indexName: "transfer_escrow_unsettled_balance_contract", indexBytes: 4000 * mib, compactBytes: 10 * mib, heapBytes: mib},
		// pending_task's poll index on 2026-10-21
		{indexName: "synthetic_most", indexBytes: 94000 * mib, compactBytes: 20 * mib, heapBytes: 200 * mib},
		{indexName: "synthetic_middle", indexBytes: 900 * mib, compactBytes: 100 * mib, heapBytes: 200 * mib},
		{indexName: "synthetic_tie_b", indexBytes: 300 * mib, compactBytes: 100 * mib, heapBytes: mib},
		{indexName: "synthetic_tie_a", indexBytes: 300 * mib, compactBytes: 100 * mib, heapBytes: mib},
	}
	names := func(selectedIndexes []dbMaintenancePriorityIndex) []string {
		selectedNames := []string{}
		for _, selectedIndex := range selectedIndexes {
			selectedNames = append(selectedNames, selectedIndex.indexName)
		}
		return selectedNames
	}
	for _, c := range []struct {
		maxRebuilds int
		want        []string
	}{
		{maxRebuilds: settings.MaxIndexRebuilds, want: []string{"synthetic_most", "synthetic_middle", "synthetic_tie_a"}},
		{maxRebuilds: 10, want: []string{"synthetic_most", "synthetic_middle", "synthetic_tie_a", "synthetic_tie_b", "synthetic_boundary"}},
		{maxRebuilds: 0, want: []string{}},
	} {
		settings.MaxIndexRebuilds = c.maxRebuilds
		selectedIndexes, bloatedCount := settings.selectBloatedIndexes(indexes)
		if got := names(selectedIndexes); !slices.Equal(got, c.want) || bloatedCount != 5 {
			t.Errorf("at most %d: selected %v of %d bloated, want %v of 5", c.maxRebuilds, got, bloatedCount, c.want)
		}
	}
}

// A table whose indexes are bloated past its live rows: all but one row in
// fifty deleted, then vacuumed and analyzed, so each leaf keeps a few entries.
func testDbHotIndexCreateBloatedTable(ctx context.Context, tableName string, rowCount int) {
	Db(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			CREATE TABLE %[1]s (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false);
			CREATE INDEX %[1]s_value ON %[1]s (value);
			INSERT INTO %[1]s SELECT n, n FROM generate_series(1, %[2]d) n;
			DELETE FROM %[1]s WHERE id %% 50 <> 0
		`, tableName, rowCount)))
	})
	testDbMaintenanceVacuumAnalyze(ctx, tableName)
}

// A table of the same rows with no churn.
func testDbHotIndexCreateCompactTable(ctx context.Context, tableName string, rowCount int) {
	Db(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			CREATE TABLE %[1]s (id bigint PRIMARY KEY) WITH (autovacuum_enabled = false);
			INSERT INTO %[1]s SELECT generate_series(1, %[2]d)
		`, tableName, rowCount)))
	})
	testDbMaintenanceVacuumAnalyze(ctx, tableName)
}

// The defaults with the fixtures' small indexes counted as worth a rebuild.
func testDbHotIndexSettings(tableNames ...string) *DbHotVacuumSettings {
	settings := DefaultDbHotVacuumSettings()
	settings.Tables = tableNames
	settings.IndexMinReclaimBytes = 1024 * 1024
	return settings
}

func testDbHotIndexOutcomes(pass *dbHotIndexPassReport) []string {
	outcomes := []string{}
	for _, rebuild := range pass.rebuilds {
		outcomes = append(outcomes, rebuild.index.indexName+" "+string(rebuild.outcome))
	}
	slices.Sort(outcomes)
	return outcomes
}

// The hourly pass rebuilds the bloated indexes of the tables, on by default,
// and leaves a compact index and a small bloated one alone; the next run
// within the hour measures nothing.
func TestDbHotIndexPassRebuildsOnlyBloatedIndexes(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		testDbHotIndexCreateBloatedTable(ctx, "synthetic_hot_bloated", testDbMaintenanceChurnRowCount)
		testDbHotIndexCreateCompactTable(ctx, "synthetic_hot_compact", testDbMaintenanceChurnRowCount)
		// bloated, with far less than the floor to reclaim
		testDbHotIndexCreateBloatedTable(ctx, "synthetic_hot_small", 5000)
		untouched := []string{"synthetic_hot_compact_pkey", "synthetic_hot_small_pkey", "synthetic_hot_small_value"}
		rebuilt := []string{"synthetic_hot_bloated_pkey", "synthetic_hot_bloated_value"}
		beforeOids := map[string]uint32{}
		beforeBytes := map[string]int64{}
		for _, indexName := range slices.Concat(untouched, rebuilt) {
			beforeOids[indexName] = testDbMaintenanceIndexOid(ctx, indexName)
			beforeBytes[indexName] = testDbMaintenanceRelationBytes(ctx, indexName)
		}

		clock := newTestDbHotVacuumClock()
		settings := testDbHotIndexSettings("synthetic_hot_bloated", "synthetic_hot_compact", "synthetic_hot_small")
		run := newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, DbHotVacuumState{})
		run.run(ctx)
		pass := run.report.indexPass
		if pass == nil || pass.err != nil || pass.deferred != "" || pass.measuredCount != 5 || pass.bloatedCount != 2 {
			t.Fatalf("index pass = %+v", pass)
		}
		if outcomes := testDbHotIndexOutcomes(pass); !slices.Equal(outcomes, []string{"synthetic_hot_bloated_pkey rebuilt", "synthetic_hot_bloated_value rebuilt"}) {
			t.Fatalf("rebuilds = %v (%+v)", outcomes, pass.rebuilds)
		}
		for _, indexName := range rebuilt {
			if testDbMaintenanceIndexOid(ctx, indexName) == beforeOids[indexName] {
				t.Fatalf("the bloated %s was not rebuilt", indexName)
			}
			if after := testDbMaintenanceRelationBytes(ctx, indexName); beforeBytes[indexName] < 4*after {
				t.Fatalf("%s is %d bytes after its rebuild, was %d", indexName, after, beforeBytes[indexName])
			}
		}
		for _, indexName := range untouched {
			if testDbMaintenanceIndexOid(ctx, indexName) != beforeOids[indexName] {
				t.Fatalf("%s was rebuilt", indexName)
			}
		}
		if !run.state.IndexPassTime.Equal(clock.now()) {
			t.Fatalf("index pass time = %s", run.state.IndexPassTime)
		}
		for _, tableName := range settings.Tables {
			if valid, invalid := testDbMaintenanceIndexes(ctx, tableName); len(invalid) != 0 {
				t.Fatalf("%s kept incomplete indexes %v (valid %v)", tableName, invalid, valid)
			}
		}

		clock.advance(5 * time.Minute)
		run = newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, run.state)
		run.run(ctx)
		if run.report.indexPass != nil {
			t.Fatalf("an index pass ran within the hour: %+v", run.report.indexPass)
		}
	})
}

// A transaction older than the rebuild bound defers the pass before any
// rebuild, and the pass does not count, so the next run tries again and
// rebuilds once the transaction ended.
func TestDbHotIndexPassDefersBehindAnOpenTransaction(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		testDbHotIndexCreateBloatedTable(ctx, "synthetic_hot_bloated", testDbMaintenanceChurnRowCount)
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE synthetic_unrelated (id bigint)`))
		})
		beforeOid := testDbMaintenanceIndexOid(ctx, "synthetic_hot_bloated_value")
		clock := newTestDbHotVacuumClock()
		settings := testDbHotIndexSettings("synthetic_hot_bloated")
		// the holder's transaction has slept longer than this
		settings.RebuildMaxTransactionAge = time.Millisecond

		release := testDbMaintenanceHoldWriterLock(t, ctx, "synthetic_unrelated")
		defer release()
		run := newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, DbHotVacuumState{})
		run.run(ctx)
		if pass := run.report.indexPass; pass == nil || !strings.Contains(pass.deferred, "transaction") || len(pass.rebuilds) != 0 {
			t.Fatalf("index pass behind an open transaction = %+v", pass)
		}
		if !run.state.IndexPassTime.IsZero() || testDbMaintenanceIndexOid(ctx, "synthetic_hot_bloated_value") != beforeOid {
			t.Fatal("a deferred pass counted or rebuilt")
		}

		release()
		clock.advance(5 * time.Minute)
		run = newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, run.state)
		run.run(ctx)
		if pass := run.report.indexPass; pass == nil || pass.deferred != "" || len(pass.rebuilds) != 2 {
			t.Fatalf("index pass after the transaction ended = %+v", pass)
		}
		if testDbMaintenanceIndexOid(ctx, "synthetic_hot_bloated_value") == beforeOid {
			t.Fatal("the next run did not rebuild the deferred index")
		}
	})
}

// A bloated index whose table another session holds is not rebuilt, and the
// pass never queues for the table's lock: SHARE UPDATE EXCLUSIVE, the lock a
// vacuum holds, skips it at once; SHARE, which that check does not see, makes
// the rebuild wait for the table to clear for its lock bound and then skip it.
func TestDbHotIndexPassSkipsAnIndexWhoseTableIsHeld(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_hot_bloated"
		testDbHotIndexCreateBloatedTable(ctx, tableName, testDbMaintenanceChurnRowCount)
		beforeOid := testDbMaintenanceIndexOid(ctx, "synthetic_hot_bloated_value")
		settings := testDbHotIndexSettings(tableName)
		// how long a rebuild waits for its table to clear
		settings.RebuildLockTimeout = 200 * time.Millisecond

		for _, c := range []struct {
			mode   string
			reason string
		}{
			{mode: "SHARE UPDATE EXCLUSIVE", reason: "being vacuumed"},
			{mode: "SHARE", reason: "ShareLock"},
		} {
			clock := newTestDbHotVacuumClock()
			holder, release := testDbMaintenanceHoldTableLock(t, ctx, c.mode, tableName)
			run := newDbHotVacuumRun(settings, safeMaintenancePool, clock.now, DbHotVacuumState{})
			testDbMaintenanceRunWithoutQueuing(t, ctx, func() *DbMaintenanceOutcome {
				run.run(ctx)
				return &DbMaintenanceOutcome{}
			}, release, nil, nil, tableName)
			pass := run.report.indexPass
			if pass == nil || len(pass.rebuilds) == 0 {
				t.Fatalf("%s: index pass = %+v", c.mode, pass)
			}
			for _, rebuild := range pass.rebuilds {
				if rebuild.outcome != dbHotIndexSkipped || !strings.Contains(rebuild.detail, c.reason) {
					t.Fatalf("%s: rebuild %s, want skipped for %q", c.mode, rebuild.String(), c.reason)
				}
			}
			if !holder(tableName) {
				t.Fatalf("%s: the holder lost its lock", c.mode)
			}
			release()
			if testDbMaintenanceIndexOid(ctx, "synthetic_hot_bloated_value") != beforeOid {
				t.Fatalf("%s: rebuilt through another session's lock", c.mode)
			}
		}
	})
}

// Debris an interrupted rebuild left on a bloated table is dropped before the
// pass rebuilds the table's index, so no numbered copy is created.
func TestDbHotIndexPassDropsDebrisBeforeItsRebuild(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_hot_bloated"
		testDbHotIndexCreateBloatedTable(ctx, tableName, testDbMaintenanceChurnRowCount)
		testDbMaintenanceInterruptIndexBuild(t, ctx, tableName, `REINDEX INDEX CONCURRENTLY synthetic_hot_bloated_value`)
		if _, invalid := testDbMaintenanceIndexes(ctx, tableName); !slices.Equal(invalid, []string{"synthetic_hot_bloated_value_ccnew"}) {
			t.Fatalf("fixture debris = %v", invalid)
		}
		beforeOid := testDbMaintenanceIndexOid(ctx, "synthetic_hot_bloated_value")

		clock := newTestDbHotVacuumClock()
		run := newDbHotVacuumRun(testDbHotIndexSettings(tableName), safeMaintenancePool, clock.now, DbHotVacuumState{})
		run.run(ctx)
		if outcomes := testDbHotIndexOutcomes(run.report.indexPass); !slices.Equal(outcomes, []string{"synthetic_hot_bloated_pkey rebuilt", "synthetic_hot_bloated_value rebuilt"}) {
			t.Fatalf("rebuilds = %v", outcomes)
		}
		valid, invalid := testDbMaintenanceIndexes(ctx, tableName)
		if len(invalid) != 0 || !slices.Contains(valid, "synthetic_hot_bloated_value") {
			t.Fatalf("indexes after the pass: valid %v, invalid %v", valid, invalid)
		}
		if testDbMaintenanceIndexOid(ctx, "synthetic_hot_bloated_value") == beforeOid {
			t.Fatal("the index was not rebuilt after its debris was dropped")
		}
	})
}

// A drain that cancels the pass while a rebuild waits behind a writer leaves
// no copy behind: its cleanup runs on a detached context once the server has
// aborted the rebuild, and the table's indexes stay valid.
func TestDbHotIndexPassCleansUpAfterACanceledRebuild(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_hot_bloated"
		testDbHotIndexCreateBloatedTable(ctx, tableName, testDbMaintenanceChurnRowCount)
		release := testDbMaintenanceHoldWriterLock(t, ctx, tableName)
		defer release()

		runCtx, runCancel := context.WithCancel(ctx)
		defer runCancel()
		clock := newTestDbHotVacuumClock()
		run := newDbHotVacuumRun(testDbHotIndexSettings(tableName), safeMaintenancePool, clock.now, DbHotVacuumState{})
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			run.run(runCtx)
		}()
		pid := testDbMaintenanceAwaitBlockedIndexBuild(t, ctx, tableName)
		if _, invalid := testDbMaintenanceIndexes(ctx, tableName); len(invalid) == 0 {
			t.Fatal("the blocked rebuild has not created its copy")
		}
		// what a taskworker drain does to an in-flight task
		runCancel()
		testDbMaintenanceAwaitIndexBuildEnd(t, ctx, pid)
		release()
		select {
		case <-returned:
		case <-time.After(testDbMaintenanceBarrierTimeout):
			t.Fatal("the canceled run did not return")
		}

		valid, invalid := testDbMaintenanceIndexes(ctx, tableName)
		if len(invalid) != 0 {
			t.Fatalf("the canceled rebuild left incomplete indexes %v (valid %v)", invalid, valid)
		}
		for _, indexName := range []string{"synthetic_hot_bloated_pkey", "synthetic_hot_bloated_value"} {
			if !slices.Contains(valid, indexName) {
				t.Fatalf("%s is missing after a canceled rebuild (valid %v)", indexName, valid)
			}
		}
		pass := run.report.indexPass
		if pass == nil || len(pass.rebuilds) != 1 || pass.rebuilds[0].outcome != dbHotIndexFailed || pass.stopped == "" {
			t.Fatalf("index pass after the drain = %+v", pass)
		}
	})
}

// A partitioned table is vacuumed through its parent, its leaves' summaries
// summed, and its indexes are measured once per leaf, on the leaf; the
// partitioned index, which has no storage, is not a candidate.
func TestDbHotIndexPassMeasuresEachLeafOfAPartitionedTable(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_hot_parted (id bigint NOT NULL, day int NOT NULL, PRIMARY KEY (id, day)) PARTITION BY LIST (day);
				CREATE TABLE synthetic_hot_parted_a PARTITION OF synthetic_hot_parted FOR VALUES IN (1) WITH (autovacuum_enabled = false);
				CREATE TABLE synthetic_hot_parted_b PARTITION OF synthetic_hot_parted FOR VALUES IN (2) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_hot_parted SELECT n, 1 + n % 2 FROM generate_series(1, 1000) n;
				DELETE FROM synthetic_hot_parted WHERE id % 2 = 1
			`))
		})
		clock := newTestDbHotVacuumClock()
		run := newDbHotVacuumRun(testDbHotIndexSettings("synthetic_hot_parted"), safeMaintenancePool, clock.now, DbHotVacuumState{})
		run.run(ctx)
		if result := run.report.tables[0]; result.outcome != dbHotVacuumVacuumed || result.removedRows != 500 {
			t.Fatalf("partitioned table result = %+v", result)
		}

		measured, err := dbMaintenanceTableIndexSizes(ctx, []string{"synthetic_hot_parted"})
		if err != nil {
			t.Fatal(err)
		}
		leaves := []string{}
		for _, index := range measured {
			if index.method != "btree" || index.priorityName != index.indexName || index.compactBytes <= 0 {
				t.Errorf("measured %+v", index)
			}
			leaves = append(leaves, index.indexName+" on "+index.tableName)
		}
		slices.Sort(leaves)
		if !slices.Equal(leaves, []string{"synthetic_hot_parted_a_pkey on synthetic_hot_parted_a", "synthetic_hot_parted_b_pkey on synthetic_hot_parted_b"}) {
			t.Fatalf("measured leaves = %v", leaves)
		}
	})
}

// Every rebuild runs with transaction_timeout off, so the 15-minute timeout
// the application role gets (A6) cannot end a long REINDEX CONCURRENTLY. The
// database's sessions here get a 200 ms timeout, and the rebuild waits behind
// a writer, in one of its transactions, until that transaction is older than
// the timeout; it then completes once the writer is gone.
func TestDbMaintenanceRebuildRunsPastTransactionTimeout(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_long_rebuild"
		var databaseName string
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_long_rebuild (id bigint PRIMARY KEY);
				INSERT INTO synthetic_long_rebuild SELECT generate_series(1, 100)
			`))
			Raise(conn.QueryRow(ctx, `SELECT current_database()`).Scan(&databaseName))
		})
		database := pgx.Identifier{databaseName}.Sanitize()
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `ALTER DATABASE `+database+` SET transaction_timeout = '200ms'`))
		}, OptNoRetry())
		defer func() {
			MaintenanceDb(context.WithoutCancel(ctx), func(conn PgConn) {
				RaisePgResult(conn.Exec(context.WithoutCancel(ctx), `ALTER DATABASE `+database+` RESET transaction_timeout`))
			}, OptNoRetry())
			PgReset()
		}()
		// sessions from here on start with the timeout
		PgReset()

		// the writer exempts its own session, so it holds the table past the timeout
		holderConn, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		RaisePgResult(holderConn.Exec(ctx, `SET transaction_timeout = 0`))
		holderTx, err := holderConn.Begin(ctx)
		if err != nil {
			holderConn.Release()
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = holderTx.Rollback(context.WithoutCancel(ctx))
				discardPgConnection(ctx, holderConn)
			}
		}
		defer release()
		RaisePgResult(holderTx.Exec(ctx, `LOCK TABLE synthetic_long_rebuild IN ROW EXCLUSIVE MODE`))

		before := testDbMaintenanceIndexOid(ctx, "synthetic_long_rebuild_pkey")
		reindexErrs := make(chan error, 1)
		go func() {
			_, err := dbMaintenanceRunReindex(ctx, tableName, `REINDEX TABLE CONCURRENTLY synthetic_long_rebuild`, 10*time.Second)
			reindexErrs <- err
		}()
		pid := testDbMaintenanceAwaitBlockedIndexBuild(t, ctx, tableName)
		// the rebuild's waiting transaction outlives the timeout; a rebuild
		// the timeout ended leaves no row, and the wait fails at its bound
		deadline := time.Now().Add(testDbMaintenanceBarrierTimeout)
		for outlived := false; !outlived; {
			Db(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `
					SELECT coalesce((
						SELECT clock_timestamp() - xact_start > interval '400 milliseconds'
						FROM pg_stat_activity
						WHERE pid = $1
					), false)
				`, pid).Scan(&outlived))
			})
			if time.Now().After(deadline) {
				t.Fatal("the rebuild's transaction did not outlive the timeout")
			}
			if !outlived {
				time.Sleep(10 * time.Millisecond)
			}
		}
		release()
		select {
		case err := <-reindexErrs:
			if err != nil {
				t.Fatalf("the rebuild failed: %s", err)
			}
		case <-time.After(testDbMaintenanceBarrierTimeout):
			t.Fatal("the rebuild did not return")
		}
		if testDbMaintenanceIndexOid(ctx, "synthetic_long_rebuild_pkey") == before {
			t.Fatal("the table was not rebuilt")
		}
	})
}
