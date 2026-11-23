// The cutover's lock requests stay short and never queue behind a vacuum or a
// long transaction: in production the large contract tables are under
// autovacuum most of the day, and every application statement gives up after
// PgLockTimeout behind a queued exclusive request.
package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The duration a SET LOCAL lock_timeout statement sets.
func testContractLockTimeoutOf(t testing.TB, statement string) time.Duration {
	t.Helper()
	groups := regexp.MustCompile(`(?i)^SET LOCAL lock_timeout = '([0-9]+(?:ms|s|min)?)'$`).FindStringSubmatch(statement)
	if groups == nil {
		t.Fatalf("not a lock timeout: %s", statement)
	}
	value := strings.Replace(groups[1], "min", "m", 1)
	if regexp.MustCompile(`^[0-9]+$`).MatchString(value) {
		value += "ms"
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		t.Fatal(err)
	}
	return timeout
}

// Every main-pool statement waits at most PgLockTimeout for a lock and then
// fails (55P03), not rerun. Statements queued behind the swap's or rollback's
// ACCESS EXCLUSIVE request wait out that request's lock wait and then its
// work, so each of those lock waits must stay well below PgLockTimeout. The
// build of 2026-10-23 waited up to 3 s.
func TestContractCutoverLockTimeoutsStayBelowTheApplicationLockBound(t *testing.T) {
	shapes := map[string]*contractTableShape{}
	staged := map[string]*contractTableShape{}
	for _, table := range ContractPartitionTables {
		shapes[table] = &contractTableShape{table: table, relkind: "r"}
		staged[table] = &contractTableShape{table: contractStagedParentName(table), relkind: "p"}
	}
	statements := contractSwapSql(shapes, staged, ContractIdFloor(time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)))
	if swap := testContractLockTimeoutOf(t, statements[0]); PgLockTimeout/2 < swap {
		t.Errorf("the swap waits up to %s for each lock; application statements queued behind it give up after %s", swap, PgLockTimeout)
	}
	if rollback := DefaultContractPartitionRollbackSettings().LockTimeout; PgLockTimeout/2 < rollback {
		t.Errorf("rollback waits up to %s for each lock; application statements queued behind it give up after %s", rollback, PgLockTimeout)
	}
}

// Once the tables are partitioned, their vacuums run on the leaves, and the
// LOCK TABLE of a parent waits for the locks of its leaves. Rollback must
// read a leaf's long lock (here a vacuum's SHARE UPDATE EXCLUSIVE on the
// legacy leaf) before it requests its exclusive locks, not queue behind it:
// queued, it would stall every contract write for its lock timeout, or cancel
// a routine autovacuum after deadlock_timeout. The build of 2026-10-23 read the
// locks of the parents only.
func TestContractPartitionRollbackDoesNotQueueBehindALeafLock(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		Testing_PartitionContracts(ctx)
		legacy := ContractLegacyPartitionName("transfer_contract")

		holderPid := make(chan uint32, 1)
		release := make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- contractPartitionTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`LOCK TABLE public.%s IN SHARE UPDATE EXCLUSIVE MODE`, quoteIdentifier(legacy))))
				holderPid <- tx.Conn().PgConn().PID()
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}()
		var pid uint32
		select {
		case pid = <-holderPid:
		case err := <-holderDone:
			t.Fatalf("the leaf lock holder failed: %v", err)
		}
		releaseHolder := func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		defer func() {
			releaseHolder()
			<-holderDone
		}()

		settings := DefaultContractPartitionRollbackSettings()
		// the holder is a long transaction from the start, a lock wait would
		// last the test, and there is one attempt
		settings.LockSettings.MaxTransactionAge = time.Millisecond
		settings.LockTimeout = time.Minute
		settings.StatementTimeout = time.Minute
		settings.MaxAttempts = 1
		settings.RetryInterval = time.Millisecond
		rollbackDone := make(chan error, 1)
		go func() {
			if failure := testContractPartitionRecover(func() {
				Raise(MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Rollback: true, RollbackSettings: settings}))
			}); failure != nil {
				rollbackDone <- fmt.Errorf("%v", failure)
			} else {
				rollbackDone <- nil
			}
		}()

		// Until rollback returns, no backend may wait on a lock the holder blocks.
		queued := false
		var rollbackErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			for {
				select {
				case rollbackErr = <-rollbackDone:
					return
				default:
				}
				var waiting bool
				Raise(conn.QueryRow(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_locks AS waiting
					WHERE NOT waiting.granted AND $1::int = ANY(pg_blocking_pids(waiting.pid)))`, pid).Scan(&waiting))
				if waiting {
					queued = true
					// let a queued rollback finish rather than wait out its timeout
					releaseHolder()
					rollbackErr = <-rollbackDone
					return
				}
				select {
				case rollbackErr = <-rollbackDone:
					return
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
		if queued {
			t.Fatalf("rollback queued its exclusive lock request behind the lock on %s (it then returned %v)", legacy, rollbackErr)
		}
		if rollbackErr == nil {
			t.Fatal("rollback returned without error while the legacy leaf was held")
		}
		if !IsContractPartitioned(ctx) {
			t.Fatal("a refused rollback changed the layout")
		}
	})
}

// Prepare adds and validates each table's legacy bound under locks that
// conflict with vacuum (SHARE UPDATE EXCLUSIVE to validate, a brief ACCESS
// EXCLUSIVE to add). A table that a vacuum or a long transaction holds is
// deferred to the next run without waiting, and the other tables are prepared
// in the same run. The build of 2026-10-23 waited on the busy table (up to ten
// minutes to add, ten minute-long lock waits to validate, cancelling a routine
// autovacuum after deadlock_timeout) and prepared nothing after it.
func TestContractPartitionPrepareDefersABusyTable(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if IsContractPartitioned(ctx) {
			t.Skip("prepare starts from plain contract tables")
		}
		logf := testContractPartitionLogf(t)
		cutoverDay := ContractPartitionDayStart(NowUtc()).AddDate(0, 0, 2)
		busy := "transfer_escrow"

		// A vacuum-like holder of transfer_escrow, older than the admission's
		// transaction age by the database clock.
		holderPid := make(chan uint32, 1)
		release := make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- contractPartitionTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`LOCK TABLE public.%s IN SHARE UPDATE EXCLUSIVE MODE`, quoteIdentifier(busy))))
				holderPid <- tx.Conn().PgConn().PID()
				<-release
			})
		}()
		pid := <-holderPid
		released := false
		releaseHolder := func() {
			if !released {
				released = true
				close(release)
				if err := <-holderDone; err != nil {
					t.Fatalf("the holder failed: %v", err)
				}
			}
		}
		defer releaseHolder()
		settings := DefaultContractPartitionCutoverSettings()
		age := settings.LockSettings.MaxTransactionAge + time.Second
		MaintenanceDb(ctx, func(conn PgConn) {
			for {
				var old bool
				Raise(conn.QueryRow(ctx, `SELECT clock_timestamp() - xact_start > make_interval(secs => $2) FROM pg_stat_activity WHERE pid = $1`, pid, age.Seconds()).Scan(&old))
				if old {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		})

		// a few quick attempts: the default tries for minutes to meet a quiet moment
		settings.StepAttempts = 3
		settings.StepRetryInterval = 100 * time.Millisecond
		prepareCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		var err error
		if failure := testContractPartitionRecover(func() {
			err = MigrateContractsToPartitions(prepareCtx, ContractPartitionMigrateOptions{CutoverDay: cutoverDay, Logf: logf, CutoverSettings: settings})
		}); failure != nil {
			err = fmt.Errorf("%v", failure)
		}
		if err == nil || !strings.Contains(err.Error(), busy) {
			t.Fatalf("prepare with %s held must report it deferred, got %v", busy, err)
		}
		shapes := readContractShapes(ctx, contractTableName)
		for _, table := range ContractPartitionTables {
			bound := shapes[table].legacyBound
			if table == busy {
				if bound != nil {
					t.Fatalf("%s is held, yet prepare added its legacy bound", table)
				}
				continue
			}
			if bound == nil || !bound.validated {
				t.Fatalf("%s is free, yet prepare left its legacy bound %+v", table, bound)
			}
		}

		// once the holder is gone, the next run completes the deferred table
		releaseHolder()
		if err := MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverDay: cutoverDay, Logf: logf}); err != nil {
			t.Fatal(err)
		}
		shapes = readContractShapes(ctx, contractTableName)
		for _, table := range ContractPartitionTables {
			if bound := shapes[table].legacyBound; bound == nil || !bound.validated {
				t.Fatalf("%s is not prepared after the second run: %+v", table, bound)
			}
		}
		if err := MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Abort: true, Logf: logf}); err != nil {
			t.Fatal(err)
		}
	})
}

// A reader of transfer_contract older than the admission age (the provider
// list's fill and the payout sweeps read for 26 to 45 s on Main): prepare must
// defer the legacy bound without a lock request, never queue every new
// statement on the table behind the reader.
func TestContractPartitionPrepareDoesNotQueueBehindALongReader(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if IsContractPartitioned(ctx) {
			t.Skip("prepare starts from plain contract tables")
		}
		logf := testContractPartitionLogf(t)
		cutoverDay := ContractPartitionDayStart(NowUtc()).AddDate(0, 0, 2)
		busy := "transfer_contract"

		holderPid := make(chan uint32, 1)
		release := make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- contractPartitionTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`LOCK TABLE public.%s IN ACCESS SHARE MODE`, quoteIdentifier(busy))))
				holderPid <- tx.Conn().PgConn().PID()
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}()
		var pid uint32
		select {
		case pid = <-holderPid:
		case err := <-holderDone:
			t.Fatalf("the reader failed: %v", err)
		}
		released := false
		releaseHolder := func() {
			if !released {
				released = true
				close(release)
				if err := <-holderDone; err != nil {
					t.Fatalf("the reader failed: %v", err)
				}
			}
		}
		defer releaseHolder()
		// older than the admission age by the database clock
		settings := DefaultContractPartitionCutoverSettings()
		age := settings.LockSettings.MaxTransactionAge + 500*time.Millisecond
		MaintenanceDb(ctx, func(conn PgConn) {
			for {
				var old bool
				Raise(conn.QueryRow(ctx, `SELECT clock_timestamp() - xact_start > make_interval(secs => $2) FROM pg_stat_activity WHERE pid = $1`, pid, age.Seconds()).Scan(&old))
				if old {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		})

		// the default lock wait and admission age; a few quick attempts
		settings.StepAttempts = 3
		settings.StepRetryInterval = 100 * time.Millisecond
		prepareDone := make(chan error, 1)
		go func() {
			var err error
			if failure := testContractPartitionRecover(func() {
				err = MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverDay: cutoverDay, Logf: logf, CutoverSettings: settings})
			}); failure != nil {
				err = fmt.Errorf("%v", failure)
			}
			prepareDone <- err
		}()

		// Until prepare returns, no exclusive request on the table may wait.
		queued := false
		var prepareErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			for {
				select {
				case prepareErr = <-prepareDone:
					return
				default:
				}
				var waiting bool
				Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'relation' AND relation = to_regclass($1) AND mode = 'AccessExclusiveLock' AND NOT granted)`, "public."+busy).Scan(&waiting))
				if waiting {
					queued = true
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
		if queued {
			t.Fatalf("prepare queued an exclusive request on %s behind a long reader (it then returned %v)", busy, prepareErr)
		}
		if prepareErr == nil || !strings.Contains(prepareErr.Error(), busy) {
			t.Fatalf("prepare with %s held by a long reader must report it deferred, got %v", busy, prepareErr)
		}
		if bound := readContractShapes(ctx, contractTableName)[busy].legacyBound; bound != nil {
			t.Fatalf("%s is read, yet prepare added its legacy bound", busy)
		}

		releaseHolder()
		if err := MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Abort: true, Logf: logf}); err != nil {
			t.Fatal(err)
		}
	})
}
