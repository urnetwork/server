// Database regressions for the reindex rotation's debris: a canceled rebuild
// must not strand its _ccnew copies, cleanup must drop only provable copies of
// valid indexes, and a rebuild must not start beside another index build.
// These use only DbMaintenance and SQL, so they run unchanged on the code
// before the fix and fail there. Each fixture produces genuine REINDEX
// CONCURRENTLY debris the way a taskworker drain does: the rebuild creates its
// copies, blocks behind a writer, and its context is canceled. Every wait is a
// barrier on an observed PostgreSQL state, never elapsed time.
package server

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Bounds every barrier so a broken fixture fails instead of hanging the run.
const testDbMaintenanceBarrierTimeout = 60 * time.Second

// The generation-zero epoch whose hash rotation selects tableName.
func testDbMaintenanceEpoch(tableName string) uint64 {
	hash := fnv.New64()
	_, _ = hash.Write([]byte(tableName))
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], 0)
	_, _ = hash.Write(generation[:])
	return hash.Sum64() % DbReindexEpochs
}

// Holds a writer's lock on tableName in an open transaction on a direct session
// until release runs. A concurrent rebuild then waits in its second phase,
// after creating its copies, and DROP INDEX CONCURRENTLY waits behind it.
func testDbMaintenanceHoldWriterLock(t testing.TB, ctx context.Context, tableName string) (release func()) {
	conn, err := AcquireMaintenanceDbConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatal(err)
	}
	// an older transaction start for the open-transaction guard test
	RaisePgResult(tx.Exec(ctx, `SELECT pg_sleep(0.005)`))
	RaisePgResult(tx.Exec(ctx, `LOCK TABLE `+pgx.Identifier{"public", tableName}.Sanitize()+` IN ROW EXCLUSIVE MODE`))
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
	return release
}

// Waits until an index build on tableName blocks on a lock and returns its pid.
// While a table rebuild waits, its progress row can name the toast relation.
func testDbMaintenanceAwaitBlockedIndexBuild(t testing.TB, ctx context.Context, tableName string) int32 {
	deadline := time.Now().Add(testDbMaintenanceBarrierTimeout)
	for {
		var pids []int32
		Db(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT progress.pid
				FROM pg_stat_progress_create_index progress
				INNER JOIN pg_stat_activity activity ON activity.pid = progress.pid
				INNER JOIN pg_class table_class ON table_class.oid = to_regclass($1)
				WHERE progress.datname = current_database()
				  AND progress.relid IN (table_class.oid, table_class.reltoastrelid)
				  AND activity.wait_event_type = 'Lock'
			`, pgx.Identifier{"public", tableName}.Sanitize())
			WithPgResult(result, err, func() {
				for result.Next() {
					var pid int32
					Raise(result.Scan(&pid))
					pids = append(pids, pid)
				}
			})
		})
		if len(pids) == 1 {
			return pids[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no index build on %s blocked on a lock (found %v)", tableName, pids)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Waits until the backend has left index-build progress, i.e. the server has
// finished aborting or completing its statement.
func testDbMaintenanceAwaitIndexBuildEnd(t testing.TB, ctx context.Context, pid int32) {
	deadline := time.Now().Add(testDbMaintenanceBarrierTimeout)
	for {
		var active bool
		Db(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_progress_create_index WHERE pid = $1)`, pid).Scan(&active))
		})
		if !active {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("index build %d did not end", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Runs a concurrent index statement on tableName that blocks behind a writer,
// then cancels its context the way a drain cancels a task, and returns once
// the server has aborted it. The statement's invalid indexes stay behind.
func testDbMaintenanceInterruptIndexBuild(t testing.TB, ctx context.Context, tableName string, statement string) {
	release := testDbMaintenanceHoldWriterLock(t, ctx, tableName)
	defer release()
	buildCtx, buildCancel := context.WithCancel(ctx)
	defer buildCancel()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		HandleError(func() {
			MaintenanceDb(buildCtx, func(conn PgConn) {
				_, _ = conn.Exec(buildCtx, statement)
			}, OptNoRetry())
		})
	}()
	pid := testDbMaintenanceAwaitBlockedIndexBuild(t, ctx, tableName)
	buildCancel()
	testDbMaintenanceAwaitIndexBuildEnd(t, ctx, pid)
	select {
	case <-returned:
	case <-time.After(testDbMaintenanceBarrierTimeout):
		t.Fatal("canceled index statement did not return")
	}
}

// The valid and invalid indexes of tableName and its toast table, by name.
func testDbMaintenanceIndexes(ctx context.Context, tableName string) (valid []string, invalid []string) {
	Db(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `
			SELECT index_class.relname, index_state.indisvalid
			FROM pg_class table_class
			INNER JOIN pg_index index_state
				ON index_state.indrelid IN (table_class.oid, table_class.reltoastrelid)
			INNER JOIN pg_class index_class ON index_class.oid = index_state.indexrelid
			WHERE table_class.oid = to_regclass($1)
			ORDER BY index_class.relname
		`, pgx.Identifier{"public", tableName}.Sanitize())
		WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				var isValid bool
				Raise(result.Scan(&name, &isValid))
				if isValid {
					valid = append(valid, name)
				} else {
					invalid = append(invalid, name)
				}
			}
		})
	})
	return
}

// The index's oid; a rebuild replaces it.
func testDbMaintenanceIndexOid(ctx context.Context, indexName string) uint32 {
	var oid uint32
	Db(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT to_regclass($1)::oid`, pgx.Identifier{"public", indexName}.Sanitize()).Scan(&oid))
	})
	return oid
}

// A drain cancels the task context while REINDEX CONCURRENTLY waits. The
// rebuild has already created a copy of every index, so its cleanup must still
// drop them: before the fix it ran on the same canceled context and failed
// immediately, stranding the copies (network_client on 2026-10-05).
func TestDbMaintenanceCanceledRebuildLeavesNoIncompleteIndex(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_canceled_rebuild"
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_canceled_rebuild (id bigint PRIMARY KEY, value bigint);
				CREATE INDEX synthetic_canceled_rebuild_value ON synthetic_canceled_rebuild (value);
				INSERT INTO synthetic_canceled_rebuild SELECT n, n FROM generate_series(1, 100) n
			`))
		})
		release := testDbMaintenanceHoldWriterLock(t, ctx, tableName)
		defer release()

		runCtx, runCancel := context.WithCancel(ctx)
		defer runCancel()
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			HandleError(func() {
				DbMaintenance(runCtx, testDbMaintenanceEpoch(tableName), &DbMaintenanceOptions{
					Reindex: true,
					Cleanup: true,
				})
			})
		}()

		pid := testDbMaintenanceAwaitBlockedIndexBuild(t, ctx, tableName)
		_, blockedInvalid := testDbMaintenanceIndexes(ctx, tableName)
		if len(blockedInvalid) == 0 {
			t.Fatal("the blocked rebuild has not created its copies")
		}
		// what a taskworker drain does to an in-flight task
		runCancel()
		testDbMaintenanceAwaitIndexBuildEnd(t, ctx, pid)
		// the copies can now be dropped once their cleanup gets past the writer
		release()
		select {
		case <-returned:
		case <-time.After(testDbMaintenanceBarrierTimeout):
			t.Fatal("canceled maintenance did not return")
		}

		valid, invalid := testDbMaintenanceIndexes(ctx, tableName)
		if len(invalid) != 0 {
			t.Fatalf("canceled rebuild left incomplete indexes %v (valid %v)", invalid, valid)
		}
		for _, name := range []string{"synthetic_canceled_rebuild_pkey", "synthetic_canceled_rebuild_value"} {
			if !slices.Contains(valid, name) {
				t.Fatalf("index %s is missing after a canceled rebuild (valid %v)", name, valid)
			}
		}
	})
}

// An operator run with rebuilds but no cleanup steps (--reindex alone) still
// removes the copies of its own canceled rebuild; before the fix nothing did.
func TestDbMaintenanceCanceledRebuildWithoutCleanupStepsDropsItsCopies(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_reindex_only"
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_reindex_only (id bigint PRIMARY KEY);
				INSERT INTO synthetic_reindex_only SELECT generate_series(1, 100)
			`))
		})
		release := testDbMaintenanceHoldWriterLock(t, ctx, tableName)
		defer release()

		runCtx, runCancel := context.WithCancel(ctx)
		defer runCancel()
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			HandleError(func() {
				DbMaintenance(runCtx, testDbMaintenanceEpoch(tableName), &DbMaintenanceOptions{
					Reindex: true,
				})
			})
		}()
		pid := testDbMaintenanceAwaitBlockedIndexBuild(t, ctx, tableName)
		runCancel()
		testDbMaintenanceAwaitIndexBuildEnd(t, ctx, pid)
		release()
		select {
		case <-returned:
		case <-time.After(testDbMaintenanceBarrierTimeout):
			t.Fatal("canceled maintenance did not return")
		}

		if _, invalid := testDbMaintenanceIndexes(ctx, tableName); len(invalid) != 0 {
			t.Fatalf("canceled rebuild without cleanup steps left incomplete indexes %v", invalid)
		}
	})
}

// Cleanup drops an invalid index only when it is the REINDEX CONCURRENTLY copy
// of a valid index with the same definition, including PostgreSQL's truncated
// and counter-suffixed names. An invalid index that merely ends in _ccnew, an
// invalid copy whose definition differs from its namesake, and every valid
// index survive; before the fix each suffixed invalid index was dropped. The
// table has no toast relation: dropping a toast copy needs privileges the local
// test role lacks (the Main maintenance role has them), and the toast pairing
// is covered by TestDbMaintenanceListsToastCopyOfToastIndex.
func TestDbMaintenanceCleanupDropsOnlyCopiesOfValidIndexes(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_debris"
		// the longest name PostgreSQL keeps, so its copy's name is truncated
		longIndexName := "synthetic_debris_" + strings.Repeat("x", 63-len("synthetic_debris_"))
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_debris (id bigint PRIMARY KEY, value bigint, label bigint);
				CREATE INDEX synthetic_debris_value ON synthetic_debris (value);
				CREATE INDEX synthetic_debris_label ON synthetic_debris (label);
				CREATE INDEX synthetic_debris_label_ccnew ON synthetic_debris (label);
				CREATE INDEX `+longIndexName+` ON synthetic_debris (value, id);
				INSERT INTO synthetic_debris SELECT n, n % 10, n FROM generate_series(1, 100) n
			`))
		})
		validBefore, _ := testDbMaintenanceIndexes(ctx, tableName)

		// genuine copies of every valid index
		testDbMaintenanceInterruptIndexBuild(t, ctx, tableName, `REINDEX TABLE CONCURRENTLY synthetic_debris`)
		_, copies := testDbMaintenanceIndexes(ctx, tableName)
		truncatedCopy := longIndexName[:57] + "_ccnew"
		for _, name := range []string{
			"synthetic_debris_pkey_ccnew",
			"synthetic_debris_value_ccnew",
			"synthetic_debris_label_ccnew1",
			"synthetic_debris_label_ccnew_ccnew",
			truncatedCopy,
		} {
			if !slices.Contains(copies, name) {
				t.Fatalf("fixture did not create copy %s (invalid %v)", name, copies)
			}
		}

		// invalid indexes with the suffix that are not copies of a valid index
		testDbMaintenanceInterruptIndexBuild(t, ctx, tableName, `CREATE INDEX CONCURRENTLY synthetic_debris_other_ccnew ON synthetic_debris (value)`)
		Db(ctx, func(conn PgConn) {
			for _, statement := range []string{
				`CREATE UNIQUE INDEX CONCURRENTLY synthetic_debris_orphan_ccnew ON synthetic_debris (value)`,
				`CREATE UNIQUE INDEX CONCURRENTLY synthetic_debris_value_ccnew1 ON synthetic_debris (value)`,
			} {
				// duplicate values leave the failed concurrent build invalid
				if _, err := conn.Exec(ctx, statement); err == nil {
					t.Fatalf("synthetic uniqueness failure did not occur: %s", statement)
				}
			}
		})
		kept := []string{
			"synthetic_debris_orphan_ccnew",
			"synthetic_debris_other_ccnew",
			"synthetic_debris_value_ccnew1",
		}

		DbMaintenance(ctx, testDbMaintenanceEpoch(tableName), &DbMaintenanceOptions{
			Cleanup: true,
		})

		valid, invalid := testDbMaintenanceIndexes(ctx, tableName)
		if !slices.Equal(invalid, kept) {
			t.Fatalf("invalid indexes after cleanup = %v, want only the non-copies %v", invalid, kept)
		}
		if !slices.Equal(valid, validBefore) {
			t.Fatalf("valid indexes after cleanup = %v, want unchanged %v", valid, validBefore)
		}
	})
}

// A rebuild must not start while another index build is in progress: it would
// contend for the same old snapshots and cpu. Before the fix the rotation
// started it anyway (several stalled for an hour behind long transactions and
// were then interrupted).
func TestDbMaintenanceDoesNotStartRebuildBesideAnotherIndexBuild(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const tableName = "synthetic_guarded_rebuild"
		const otherTableName = "synthetic_other_build"
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_guarded_rebuild (id bigint PRIMARY KEY);
				INSERT INTO synthetic_guarded_rebuild SELECT generate_series(1, 100);
				CREATE TABLE synthetic_other_build (id bigint, value bigint)
			`))
		})
		before := testDbMaintenanceIndexOid(ctx, "synthetic_guarded_rebuild_pkey")

		// another index build, held in progress behind a writer
		release := testDbMaintenanceHoldWriterLock(t, ctx, otherTableName)
		defer release()
		otherReturned := make(chan struct{})
		go func() {
			defer close(otherReturned)
			HandleError(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					_, _ = conn.Exec(ctx, `CREATE INDEX CONCURRENTLY synthetic_other_build_value ON synthetic_other_build (value)`)
				}, OptNoRetry())
			})
		}()
		testDbMaintenanceAwaitBlockedIndexBuild(t, ctx, otherTableName)

		returned := make(chan struct{})
		go func() {
			defer close(returned)
			HandleError(func() {
				DbMaintenance(ctx, testDbMaintenanceEpoch(tableName), &DbMaintenanceOptions{
					Reindex: true,
					Cleanup: true,
				})
			})
		}()
		// either the run returns, or its rebuild of the table has started
		deadline := time.Now().Add(testDbMaintenanceBarrierTimeout)
		for observed := false; !observed; {
			select {
			case <-returned:
				observed = true
			default:
				Db(ctx, func(conn PgConn) {
					Raise(conn.QueryRow(ctx, `
						SELECT EXISTS (
							SELECT 1
							FROM pg_stat_progress_create_index progress
							INNER JOIN pg_class table_class ON table_class.oid = to_regclass('public.synthetic_guarded_rebuild')
							WHERE progress.datname = current_database()
							  AND progress.relid IN (table_class.oid, table_class.reltoastrelid)
						)
					`).Scan(&observed))
				})
				if time.Now().After(deadline) {
					t.Fatal("maintenance neither returned nor started its rebuild")
				}
				if !observed {
					time.Sleep(10 * time.Millisecond)
				}
			}
		}
		release()
		for _, done := range []chan struct{}{returned, otherReturned} {
			select {
			case <-done:
			case <-time.After(testDbMaintenanceBarrierTimeout):
				t.Fatal("maintenance or the other index build did not return")
			}
		}

		if after := testDbMaintenanceIndexOid(ctx, "synthetic_guarded_rebuild_pkey"); after != before {
			t.Fatal("the table was rebuilt while another index build was in progress", before, after)
		}
	})
}
