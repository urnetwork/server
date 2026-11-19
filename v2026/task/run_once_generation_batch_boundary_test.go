// A native trigger barrier holds the batch's deleted keys before COMMIT.
package task

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The producer starts only after PostgreSQL proves the real batch finalizer
// blocked in its copy trigger. No test hook changes its transaction lifecycle.
func TestRunOnceBatchScheduleAfterCompletionLockKeepsSuccessor(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, scopes, results := runOnceGenerationClaimResults(t, ctx, true)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_run_once_finish_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN PERFORM pg_advisory_xact_lock(726431::bigint); RETURN NEW; END $$;
			 CREATE TRIGGER test_run_once_finish_barrier BEFORE INSERT ON finished_task FOR EACH ROW EXECUTE FUNCTION test_run_once_finish_barrier()`))
		})
		holderPid, producerPid := make(chan int, 1), make(chan int, 1)
		release, heldDone, finishDone, producerDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var resumed sync.Once
		var holderErr, finishErr, producerErr error
		var retrySingles bool
		finishStarted, producerStarted := false, false
		defer func() {
			resumed.Do(func() { close(release) })
			joins := []<-chan struct{}{heldDone}
			if finishStarted {
				joins = append(joins, finishDone)
			}
			if producerStarted {
				joins = append(joins, producerDone)
			}
			for _, done := range joins {
				select {
				case <-done:
				case <-ctx.Done():
				}
			}
		}()
		go func() {
			defer close(heldDone)
			server.HandleError(func() {
				server.Db(ctx, func(conn server.PgConn) {
					server.RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_lock(726431::bigint)`))
					defer func() {
						cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
						defer cancel()
						server.RaisePgResult(conn.Exec(cleanupCtx, `SELECT pg_advisory_unlock(726431::bigint)`))
					}()
					var pid int
					server.Raise(conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
					holderPid <- pid
					select {
					case <-release:
					case <-ctx.Done():
					}
				})
			}, func(err error) { holderErr = err })
		}()
		var holder int
		select {
		case holder = <-holderPid:
		case <-ctx.Done():
			t.Fatal("native barrier holder did not acquire", ctx.Err())
		}
		finishStarted = true
		go func() {
			defer close(finishDone)
			retrySingles, finishErr = worker.finalizeTaskBatch(results)
		}()
		var finisher int
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for finisher == 0 {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(min(pid),0) FROM pg_stat_activity
				 WHERE datid=(SELECT oid FROM pg_database WHERE datname=current_database())
				 AND $1=ANY(pg_blocking_pids(pid))`, holder).Scan(&finisher))
			})
			if finisher == 0 {
				select {
				case <-ctx.Done():
					t.Fatal("batch did not reach its native copy barrier", ctx.Err())
				case <-tick.C:
				}
			}
		}
		// A data-modifying CTE may produce its removed rows incrementally.
		// Select a key already locked by that exact finisher, so this producer
		// cannot lock an as-yet-unremoved neighbor and invent a deadlock.
		unlocked := map[server.Id]bool{}
		server.Tx(ctx, func(tx server.PgTx) {
			rows, err := tx.Query(ctx, `SELECT task_id FROM pending_task WHERE task_id=ANY($1) FOR UPDATE SKIP LOCKED`, ids)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					server.Raise(rows.Scan(&id))
					unlocked[id] = true
				}
			})
		})
		lockedIndex := -1
		for index, id := range ids {
			if !unlocked[id] {
				lockedIndex = index
				break
			}
		}
		if lockedIndex < 0 {
			t.Fatal("native finish barrier did not own a pending row")
		}
		producerStarted = true
		go func() {
			defer close(producerDone)
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					var pid int
					server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
					select {
					case producerPid <- pid:
					default:
					}
					ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[lockedIndex]}, owner,
						runOnceGenerationKey(scopes[lockedIndex]), RunAt(server.NowUtc().Add(-time.Hour)))
				})
			}, func(err error) { producerErr = err })
		}()
		var producer int
		select {
		case producer = <-producerPid:
		case <-ctx.Done():
			t.Fatal("producer did not start its native insert", ctx.Err())
		}
		runOnceGenerationBlockingEdge(t, ctx, producer, finisher)
		resumed.Do(func() { close(release) })
		for _, done := range []<-chan struct{}{heldDone, finishDone, producerDone} {
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("batch boundary did not join", ctx.Err())
			}
		}
		pending := runOnceGenerationPending(ctx, scopes)
		if holderErr != nil || finishErr != nil || producerErr != nil || retrySingles || len(pending) != 1 || len(GetFinishedTasks(ctx, ids...)) != 2 {
			t.Fatal("completion-first batch lost producer custody", holderErr, finishErr, producerErr, retrySingles)
		}
		for _, id := range ids {
			if pending[id] != nil {
				t.Fatal("completion-first batch did not create distinct successors")
			}
		}
	})
}
