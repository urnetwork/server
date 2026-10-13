// Regressions for maintenance that queued behind an autovacuum's lock. After
// deadlock_timeout PostgreSQL cancels an autovacuum that blocks another
// session, so the daily VACUUM (ANALYZE) pending_task and the final ANALYZE
// cancelled the autovacuums of transfer_escrow, transfer_contract and
// pending_task on 2026-10-13 (07:11:59 to 07:12:12Z), and a REINDEX
// CONCURRENTLY or DROP INDEX CONCURRENTLY would do the same. The database
// tests stand a session holding SHARE UPDATE EXCLUSIVE, the lock an autovacuum
// holds, in for the autovacuum (the priority index test holds SHARE, which the
// vacuum check before the priority rebuilds does not see), and fail as soon as
// a maintenance statement takes a place in that lock's queue, which is what
// triggers the cancel. They use only DbMaintenance and SQL, so they ran
// unchanged on the code before the fix and failed there; the priority index
// test now also sets PriorityReindex, added later, since the priority rebuilds
// stay off until enabled. Every wait is a barrier on an observed PostgreSQL
// state.
package server

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Every VACUUM and ANALYZE the rotation issues must skip a locked table rather
// than queue for it. VACUUM (FULL, ...) is VacuumFullAllTables, an operator run
// that takes ACCESS EXCLUSIVE with the system offline and is left as it is.
func TestDbMaintenanceVacuumAndAnalyzeSkipLockedTables(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "db_maintenance.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		text, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		upper := strings.ToUpper(strings.TrimSpace(text))
		if strings.HasPrefix(upper, "VACUUM") || strings.HasPrefix(upper, "ANALYZE") {
			statements = append(statements, text)
		}
		return true
	})

	checked := 0
	for _, statement := range statements {
		upper := strings.ToUpper(strings.TrimSpace(statement))
		// the operator VACUUM FULL statement and its log lines
		if strings.HasPrefix(upper, "VACUUM (FULL") || strings.HasPrefix(upper, "VACUUM FULL") {
			continue
		}
		checked += 1
		if !strings.Contains(upper, "SKIP_LOCKED") {
			t.Errorf("%q queues for a table an autovacuum holds, which cancels the autovacuum", statement)
		}
	}
	// the pending_task vacuum and the final analyze
	if checked < 2 {
		t.Fatalf("found %d rotation vacuum or analyze statement(s) in %v, want at least 2", checked, statements)
	}
}

// Any lock of an autovacuum worker in the scope defers a rebuild or drop, since
// waiting behind it cancels the autovacuum; another backend's lock defers only
// when it conflicts with SHARE UPDATE EXCLUSIVE.
func TestDbMaintenanceLockConflictReasonDefersForAutovacuumAndConflictingLocks(t *testing.T) {
	// PostgreSQL's conflict table for SHARE UPDATE EXCLUSIVE, over every mode
	for mode, conflicts := range map[string]bool{
		"AccessShareLock":          false,
		"RowShareLock":             false,
		"RowExclusiveLock":         false,
		"ShareUpdateExclusiveLock": true,
		"ShareLock":                true,
		"ShareRowExclusiveLock":    true,
		"ExclusiveLock":            true,
		"AccessExclusiveLock":      true,
	} {
		if dbMaintenanceShareUpdateExclusiveConflicts[mode] != conflicts {
			t.Errorf("%s conflicts = %t, want %t", mode, dbMaintenanceShareUpdateExclusiveConflicts[mode], conflicts)
		}
	}

	writer := dbMaintenanceHeldLock{pid: 101, backendType: "client backend", relation: "synthetic_table", mode: "RowExclusiveLock"}
	reader := dbMaintenanceHeldLock{pid: 102, backendType: "client backend", relation: "synthetic_table", mode: "AccessShareLock"}
	for _, c := range []struct {
		heldLocks []dbMaintenanceHeldLock
		// "" when the statement may queue; otherwise a word the reason names
		want string
	}{
		{heldLocks: nil, want: ""},
		{heldLocks: []dbMaintenanceHeldLock{writer, reader}, want: ""},
		{
			heldLocks: []dbMaintenanceHeldLock{{pid: 201, backendType: "autovacuum worker", relation: "synthetic_table", mode: "ShareUpdateExclusiveLock"}},
			want:      "autovacuum",
		},
		// an autovacuum of the toast table, or one vacuuming an index
		{
			heldLocks: []dbMaintenanceHeldLock{{pid: 202, backendType: "autovacuum worker", relation: "pg_toast.pg_toast_1", mode: "ShareUpdateExclusiveLock"}},
			want:      "autovacuum",
		},
		{
			heldLocks: []dbMaintenanceHeldLock{writer, {pid: 203, backendType: "autovacuum worker", relation: "synthetic_table_pkey", mode: "RowExclusiveLock"}},
			want:      "autovacuum",
		},
		// a manual VACUUM, ANALYZE or concurrent index build
		{
			heldLocks: []dbMaintenanceHeldLock{reader, {pid: 301, backendType: "client backend", relation: "synthetic_table", mode: "ShareUpdateExclusiveLock"}},
			want:      "ShareUpdateExclusiveLock",
		},
		{
			heldLocks: []dbMaintenanceHeldLock{{pid: 302, backendType: "client backend", relation: "synthetic_table", mode: "ShareLock"}},
			want:      "ShareLock",
		},
		{
			heldLocks: []dbMaintenanceHeldLock{{pid: 0, backendType: "", relation: "synthetic_table", mode: "AccessExclusiveLock"}},
			want:      "prepared transaction",
		},
	} {
		reason := dbMaintenanceLockConflictReason(c.heldLocks)
		if c.want == "" && reason != "" {
			t.Errorf("locks %+v defer with %q, want no deferral", c.heldLocks, reason)
		}
		if c.want != "" && !strings.Contains(reason, c.want) {
			t.Errorf("locks %+v: reason %q, want a deferral naming %q", c.heldLocks, reason, c.want)
		}
	}
}

// Holds SHARE UPDATE EXCLUSIVE, the lock an autovacuum holds on the table it
// processes, on each table; see testDbMaintenanceHoldTableLock.
func testDbMaintenanceHoldVacuumLock(
	t testing.TB,
	ctx context.Context,
	tableNames ...string,
) (holder func(tableName string) bool, release func()) {
	return testDbMaintenanceHoldTableLock(t, ctx, "SHARE UPDATE EXCLUSIVE", tableNames...)
}

// Holds mode, a LOCK TABLE mode such as SHARE UPDATE EXCLUSIVE, on each table
// in an open transaction on a direct session until release runs. holder reports
// whether that session still holds the lock on a table, i.e. was neither
// canceled nor terminated. t.Cleanup runs only after the test environment's
// teardown, which waits for this session, so a test defers release too.
func testDbMaintenanceHoldTableLock(
	t testing.TB,
	ctx context.Context,
	mode string,
	tableNames ...string,
) (holder func(tableName string) bool, release func()) {
	// as pg_locks names it: SHARE UPDATE EXCLUSIVE is ShareUpdateExclusiveLock
	heldMode := ""
	for _, word := range strings.Fields(mode) {
		heldMode += word[:1] + strings.ToLower(word[1:])
	}
	heldMode += "Lock"
	conn, err := AcquireMaintenanceDbConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatal(err)
	}
	identifiers := []string{}
	for _, tableName := range tableNames {
		identifiers = append(identifiers, pgx.Identifier{"public", tableName}.Sanitize())
	}
	RaisePgResult(tx.Exec(ctx, `LOCK TABLE `+strings.Join(identifiers, ", ")+` IN `+mode+` MODE`))
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		_ = tx.Rollback(context.WithoutCancel(ctx))
		conn.Release()
	}
	t.Cleanup(release)
	holder = func(tableName string) bool {
		var held bool
		err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_locks
				WHERE pid = pg_backend_pid()
				  AND relation = to_regclass($1)
				  AND mode = $2
				  AND granted
			)
		`, pgx.Identifier{"public", tableName}.Sanitize(), heldMode).Scan(&held)
		return err == nil && held
	}
	return holder, release
}

// Lock requests on any of the tables that wait in their lock queues.
func testDbMaintenanceQueuedLockRequests(ctx context.Context, tableNames ...string) int {
	identifiers := []string{}
	for _, tableName := range tableNames {
		identifiers = append(identifiers, pgx.Identifier{"public", tableName}.Sanitize())
	}
	var queued int
	Db(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_locks waiting
			WHERE waiting.locktype = 'relation'
			  AND NOT waiting.granted
			  AND waiting.relation IN (SELECT to_regclass(name) FROM unnest($1::text[]) name)
		`, identifiers).Scan(&queued))
	})
	return queued
}

// Runs maintenance on its own goroutine and waits until it returns, failing
// the test as soon as any request queues for a lock on one of the tables (the
// broken path, which a holder that is an autovacuum turns into a cancel). The
// queued maintenance is then let through by release and joined. ready, if
// set, is polled too; once it reports true it runs proceed, once.
func testDbMaintenanceRunWithoutQueuing(
	t testing.TB,
	ctx context.Context,
	run func() *DbMaintenanceOutcome,
	release func(),
	ready func() bool,
	proceed func(),
	tableNames ...string,
) *DbMaintenanceOutcome {
	outcomes := make(chan *DbMaintenanceOutcome, 1)
	go func() {
		outcomes <- run()
	}()
	proceeded := false
	deadline := time.Now().Add(testDbMaintenanceBarrierTimeout)
	for {
		select {
		case outcome := <-outcomes:
			return outcome
		default:
		}
		if 0 < testDbMaintenanceQueuedLockRequests(ctx, tableNames...) {
			release()
			select {
			case <-outcomes:
			case <-time.After(testDbMaintenanceBarrierTimeout):
			}
			t.Fatalf("maintenance queued for a lock on %v that another session holds", tableNames)
		}
		if !proceeded && ready != nil && ready() {
			proceeded = true
			proceed()
		}
		if time.Now().After(deadline) {
			release()
			select {
			case <-outcomes:
			case <-time.After(testDbMaintenanceBarrierTimeout):
			}
			t.Fatalf("maintenance did not return beside the lock holder on %v", tableNames)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pg_statistic rows exist for the table only once an ANALYZE ran on it.
func testDbMaintenanceAnalyzed(ctx context.Context, tableName string) bool {
	var analyzed bool
	Db(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stats WHERE schemaname = 'public' AND tablename = $1
			)
		`, tableName).Scan(&analyzed))
	})
	return analyzed
}

// The daily pending_task vacuum and the final analyze skip a table another
// session holds, leave that session's transaction alone, and still analyze
// every free table.
func TestDbMaintenanceAnalyzeSkipsATableLockedByAnotherSession(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		// autovacuum stays off the fixtures, so only maintenance analyzes them
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE pending_task (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false);
				INSERT INTO pending_task SELECT n, n FROM generate_series(1, 1000) n;
				CREATE TABLE synthetic_locked_analyze (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_locked_analyze SELECT n, n FROM generate_series(1, 1000) n;
				CREATE TABLE synthetic_free_analyze (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_free_analyze SELECT n, n FROM generate_series(1, 1000) n
			`))
		})
		lockedTableNames := []string{"pending_task", "synthetic_locked_analyze"}
		holder, release := testDbMaintenanceHoldVacuumLock(t, ctx, lockedTableNames...)
		defer release()

		outcome := testDbMaintenanceRunWithoutQueuing(t, ctx, func() *DbMaintenanceOutcome {
			return DbMaintenance(ctx, 0, &DbMaintenanceOptions{Analyze: true})
		}, release, nil, nil, lockedTableNames...)
		if outcome.Stopped != "" {
			t.Fatalf("analyze-only outcome = %+v", *outcome)
		}
		for _, tableName := range lockedTableNames {
			if !holder(tableName) {
				t.Fatalf("the session holding %s lost its lock", tableName)
			}
			if testDbMaintenanceAnalyzed(ctx, tableName) {
				t.Fatalf("%s was analyzed through another session's lock", tableName)
			}
		}
		if !testDbMaintenanceAnalyzed(ctx, "synthetic_free_analyze") {
			t.Fatal("the final analyze skipped a free table")
		}
		release()
	})
}

// A rebuild whose table stays locked by another session for its whole lock
// bound does not start, leaves the table for the continuation, and leaves the
// holder alone; the continuation rebuilds it once the holder is gone.
func TestDbMaintenanceRebuildDefersATableLockedByAnotherSession(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_locked_rebuild"
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_locked_rebuild (id bigint PRIMARY KEY) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_locked_rebuild SELECT generate_series(1, 100)
			`))
		})
		epoch := testDbMaintenanceEpoch(tableName)
		opts := &DbMaintenanceOptions{
			Reindex: true,
			Cleanup: true,
			// how long the rebuild waits for the table to clear
			ReindexLockTimeout: 200 * time.Millisecond,
		}
		before := testDbMaintenanceIndexOid(ctx, "synthetic_locked_rebuild_pkey")

		holder, release := testDbMaintenanceHoldVacuumLock(t, ctx, tableName)
		defer release()
		outcome := testDbMaintenanceRunWithoutQueuing(t, ctx, func() *DbMaintenanceOutcome {
			return DbMaintenance(ctx, epoch, opts)
		}, release, nil, nil, tableName)
		if outcome.Stopped != DbMaintenanceDeferred || !strings.Contains(outcome.Reason, tableName) ||
			slices.Contains(outcome.DoneTables, tableName) {
			t.Fatalf("outcome beside the lock holder = %+v, want deferred with the table left", *outcome)
		}
		if !holder(tableName) {
			t.Fatal("the session holding the table lost its lock")
		}
		if after := testDbMaintenanceIndexOid(ctx, "synthetic_locked_rebuild_pkey"); after != before {
			t.Fatal("rebuilt through another session's lock", before, after)
		}

		release()
		opts.DoneTables = outcome.DoneTables
		outcome = DbMaintenance(ctx, epoch, opts)
		if outcome.Stopped != "" || !slices.Contains(outcome.DoneTables, tableName) {
			t.Fatalf("continuation outcome = %+v", *outcome)
		}
		if after := testDbMaintenanceIndexOid(ctx, "synthetic_locked_rebuild_pkey"); after == before {
			t.Fatal("the continuation did not rebuild the table once the holder was gone")
		}
	})
}

// A bloated priority index whose table stays locked by another session for the
// whole priority lock bound is not rebuilt, is left out of the done list, and
// defers the run, so the continuation rebuilds it once the holder is gone; the
// holder keeps its lock. Recorded done, it would be skipped for the rest of the
// epoch while its run reported nothing deferred. The holder takes SHARE (a
// CREATE INDEX without CONCURRENTLY holds it), which the vacuum check before
// the priority rebuilds does not look for, in for any lock that check misses:
// one taken after it, or one on the table's toast table or indexes.
func TestDbMaintenanceDefersAPriorityIndexWhoseTableStaysLocked(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "transfer_escrow"
		const indexName = "transfer_escrow_unsettled_balance_contract"
		testDbMaintenanceCreateEscrowFixture(ctx, true)
		opts := &DbMaintenanceOptions{
			Reindex:         true,
			Cleanup:         true,
			PriorityReindex: true,
			// how long the priority rebuild waits for its table to clear
			PriorityReindexLockTimeout: 200 * time.Millisecond,
		}
		before := testDbMaintenanceIndexOid(ctx, indexName)

		holder, release := testDbMaintenanceHoldTableLock(t, ctx, "SHARE", tableName)
		defer release()
		outcome := testDbMaintenanceRunWithoutQueuing(t, ctx, func() *DbMaintenanceOutcome {
			return DbMaintenance(ctx, 0, opts)
		}, release, nil, nil, tableName)
		if outcome.Stopped != DbMaintenanceDeferred || !strings.Contains(outcome.Reason, indexName) ||
			slices.Contains(outcome.DoneIndexes, indexName) {
			t.Fatalf("outcome beside the lock holder = %+v, want deferred with %s left", *outcome, indexName)
		}
		if !holder(tableName) {
			t.Fatal("the session holding the table lost its lock")
		}
		if after := testDbMaintenanceIndexOid(ctx, indexName); after != before {
			t.Fatalf("%s was rebuilt through another session's lock", indexName)
		}

		release()
		opts.DoneTables = outcome.DoneTables
		opts.DoneIndexes = outcome.DoneIndexes
		outcome = DbMaintenance(ctx, 0, opts)
		if outcome.Stopped != "" || !slices.Equal(outcome.DoneIndexes, []string{indexName}) {
			t.Fatalf("continuation outcome = %+v, want finished with %s done", *outcome, indexName)
		}
		if after := testDbMaintenanceIndexOid(ctx, indexName); after == before {
			t.Fatalf("the continuation did not rebuild %s once the holder was gone", indexName)
		}
	})
}

// A debris drop waits for its locked table to clear without taking a place in
// the lock queue, and drops the debris once the holder is gone.
func TestDbMaintenanceCleanupWaitsForALockedTableWithoutQueuing(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_locked_cleanup"
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_locked_cleanup (id bigint PRIMARY KEY) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_locked_cleanup SELECT generate_series(1, 100)
			`))
		})
		testDbMaintenanceInterruptIndexBuild(t, ctx, tableName, `REINDEX TABLE CONCURRENTLY synthetic_locked_cleanup`)
		if _, invalid := testDbMaintenanceIndexes(ctx, tableName); !slices.Equal(invalid, []string{"synthetic_locked_cleanup_pkey_ccnew"}) {
			t.Fatalf("fixture debris = %v", invalid)
		}

		holder, release := testDbMaintenanceHoldVacuumLock(t, ctx, tableName)
		defer release()
		// A maintenance session is reading the table's locks, not queuing for
		// them. The pattern is split so this query's own text never matches.
		polling := func() bool {
			var sessions int
			Db(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `
					SELECT count(*)
					FROM pg_stat_activity
					WHERE datname = current_database()
					  AND pid <> pg_backend_pid()
					  AND query LIKE '%FROM pg_locks ' || 'held%'
				`).Scan(&sessions))
			})
			return 0 < sessions
		}
		holderKept := true
		proceed := func() {
			holderKept = holder(tableName)
			release()
		}
		outcome := testDbMaintenanceRunWithoutQueuing(t, ctx, func() *DbMaintenanceOutcome {
			return DbMaintenance(ctx, testDbMaintenanceEpoch(tableName), &DbMaintenanceOptions{
				Cleanup: true,
				// longer than the test, so only the holder's release ends the wait
				CleanupLockTimeout: 2 * testDbMaintenanceBarrierTimeout,
			})
		}, release, polling, proceed, tableName)
		if outcome.Stopped != "" {
			t.Fatalf("cleanup outcome = %+v", *outcome)
		}
		if !holderKept {
			t.Fatal("the session holding the table lost its lock while the drop waited")
		}
		if _, invalid := testDbMaintenanceIndexes(ctx, tableName); len(invalid) != 0 {
			t.Fatalf("debris left after the holder ended = %v", invalid)
		}
	})
}
