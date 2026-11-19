// The pending_task swap with producers writing while it copies outside its
// lock (server.SwapTable, db_table_swap_capture.go): every path the other
// services and operators take while the taskworker is stopped, through the
// task package's own functions.
package task

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Every row of a pending_task relation as JSON, by task id: every column,
// generated ones included.
func pendingTaskSwapRowsOf(t testing.TB, ctx context.Context, table string) map[server.Id]string {
	t.Helper()
	rows := map[server.Id]string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, fmt.Sprintf(`SELECT task_id, row_to_json(t)::text FROM public.%s t`, table))
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId server.Id
				var row string
				server.Raise(result.Scan(&taskId, &row))
				rows[taskId] = row
			}
		})
	})
	return rows
}

// The writes the producers make while the swap copies pending_task outside its
// lock, through the paths they have while the taskworker is stopped: new
// tasks; run-once merges (ON CONFLICT DO UPDATE, which bumps the run-once
// generation); if-absent schedules that insert and that meet a pending key
// (ON CONFLICT DO NOTHING); the raise of a run-once task's max time (UPDATE
// by key); a kick (UPDATE of run_at); a release of a stopped worker's claim
// and a removal (the operator paths); and a repeatable read producer whose
// snapshot predates the whole swap and which writes only after the copy.
// The swapped-in pending_task holds exactly the rows of the old relation, which
// every write reached, run-once keys and generations included. After the
// swap a producer merges into the swapped row once, and the runner claims
// and finishes the kicked task from the new table.
func TestPendingTaskSwapCapturesProducersAroundItsCopy(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		later := server.NowUtc().Add(time.Hour)
		schedule := func(scope server.Id, opts ...any) server.Id {
			return ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner, append([]any{RunAt(later)}, opts...)...)
		}
		merged, raised, kicked, absent, straddled := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		schedule(merged, runOnceGenerationKey(merged))
		schedule(raised, runOnceGenerationKey(raised), MaxTime(time.Minute))
		kickedId := schedule(kicked, runOnceGenerationKey(kicked))
		released := schedule(server.NewId())
		pendingTaskSwapStoppedClaim(t, ctx, released, 2*ReleaseTimeout)
		removed := schedule(server.NewId())
		for range 200 {
			schedule(server.NewId())
		}
		generation := runOnceGenerationPending(ctx, []server.Id{merged})
		if len(generation) != 1 {
			t.Fatal("the merged key has", len(generation), "rows")
		}
		var mergedId server.Id
		var mergedGeneration int64
		for id, task := range generation {
			mergedId, mergedGeneration = id, task.RunOnceGeneration
		}

		// a repeatable read producer whose snapshot predates the swap, its
		// capture and its copy, and which writes only after the copy
		snapshotTaken, writeNow := make(chan struct{}), make(chan struct{})
		var snapshotOnce sync.Once
		attempts := 0
		straddleDone := make(chan error, 1)
		go func() {
			var straddleErr error
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					attempts += 1
					var one int
					server.Raise(tx.QueryRow(ctx, `SELECT 1`).Scan(&one))
					snapshotOnce.Do(func() { close(snapshotTaken) })
					taskQueueWait(ctx, writeNow)
					ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: straddled}, owner, runOnceGenerationKey(straddled), RunAt(later))
				})
			}, func(err error) { straddleErr = err })
			straddleDone <- straddleErr
		}()
		taskQueueWait(ctx, snapshotTaken)

		var underLock []server.Id
		hooks := &server.TableSwapHooks{
			AfterCapture: func(ctx context.Context) {
				// before the copy's snapshot: whether the copy holds these or
				// the capture brings them, they arrive once
				schedule(merged, runOnceGenerationKey(merged))
				if scheduled, _ := ScheduleTaskIfAbsent(runOnceGenerationWork, &runOnceGenerationArgs{Scope: merged}, owner, runOnceGenerationKey(merged), RunAt(later)); scheduled {
					panic(errors.New("an if-absent schedule inserted beside a pending key"))
				}
			},
			AfterPrecopy: func(ctx context.Context) {
				close(writeNow)
				if err := taskQueueError(ctx, straddleDone); err != nil {
					panic(err)
				}
				schedule(server.NewId())
				schedule(merged, runOnceGenerationKey(merged))
				if scheduled, _ := ScheduleTaskIfAbsent(runOnceGenerationWork, &runOnceGenerationArgs{Scope: absent}, owner, runOnceGenerationKey(absent), RunAt(later)); !scheduled {
					panic(errors.New("an if-absent schedule of a free key did not insert"))
				}
				server.Tx(ctx, func(tx server.PgTx) {
					if !RaiseRunOnceMaxTimeInTx(ctx, tx, runOnceGenerationKey(raised), time.Hour) {
						panic(errors.New("the max time was not raised"))
					}
				})
				if KickTasks(ctx, runOnceGenerationKey(kicked).String()) != 1 {
					panic(errors.New("the kick found no task"))
				}
				if !ReleaseTask(ctx, released) {
					panic(errors.New("the claim was not released"))
				}
				RemovePendingTask(ctx, removed)
			},
			BeforeLock: func(ctx context.Context, attempt int) {
				// after the last catch-up: the locked transaction applies these
				if attempt == 1 {
					schedule(merged, runOnceGenerationKey(merged))
					underLock = append(underLock, schedule(server.NewId()))
				}
			},
		}
		oid := pendingTaskSwapOid(t, ctx)
		if err := swapPendingTask(t, ctx, server.TableSwapOptions{Hooks: hooks}); err != nil {
			t.Fatal(err)
		}
		if pendingTaskSwapOid(t, ctx) == oid {
			t.Fatal("pending_task was not swapped")
		}
		oldTables := pendingTaskSwapOldTables(t, ctx)
		if len(oldTables) != 1 {
			t.Fatal("the old relation is not kept", oldTables)
		}
		after := pendingTaskSwapRows(t, ctx)
		if old := pendingTaskSwapRowsOf(t, ctx, oldTables[0]); !maps.Equal(after, old) {
			t.Fatalf("the swapped-in pending_task differs from the old relation: %d rows against %d", len(after), len(old))
		}
		if attempts != 1 {
			t.Fatalf("the straddling producer ran %d times", attempts)
		}

		// each write is in the new table, once
		if pending := runOnceGenerationPending(ctx, []server.Id{merged}); len(pending) != 1 || pending[mergedId] == nil || pending[mergedId].RunOnceGeneration != mergedGeneration+3 {
			t.Fatal("the three merges did not land in the one run-once row", pending, mergedGeneration)
		}
		if pending := runOnceGenerationPending(ctx, []server.Id{straddled, absent}); len(pending) != 2 {
			t.Fatal("the straddling producer's or the if-absent task is missing", pending)
		}
		for _, task := range runOnceGenerationPending(ctx, []server.Id{raised}) {
			if task.RunMaxTimeSeconds != int(time.Hour/time.Second) {
				t.Fatal("the raised max time is missing", task.RunMaxTimeSeconds)
			}
		}
		if task := GetTasks(ctx, kickedId)[kickedId]; task == nil || task.RunAt.After(server.NowUtc()) {
			t.Fatal("the kick is missing", task)
		}
		if task := GetTasks(ctx, released)[released]; task == nil || !task.ReleaseTime.IsZero() || !task.ClaimTime.IsZero() {
			t.Fatal("the release is missing", task)
		}
		if _, ok := GetTasks(ctx, removed)[removed]; ok {
			t.Fatal("the removed task is back")
		}
		for _, taskId := range underLock {
			if _, ok := after[taskId]; !ok {
				t.Fatal("the task scheduled after the last catch-up is missing", taskId)
			}
		}

		// a producer merges into the swapped row once; the runner claims and
		// finishes the kicked task from the new table
		schedule(merged, runOnceGenerationKey(merged))
		if pending := runOnceGenerationPending(ctx, []server.Id{merged}); len(pending) != 1 || pending[mergedId].RunOnceGeneration != mergedGeneration+4 {
			t.Fatal("the merge after the swap did not land in the swapped row once", pending)
		}
		// the kick moved run_at to its moment; available_block rounds a second
		// up, so the task comes due within two seconds
		finished := []server.Id{}
		for deadline := time.Now().Add(10 * time.Second); GetFinishedTasks(ctx, kickedId)[kickedId] == nil; {
			if time.Now().After(deadline) {
				t.Fatal("the kicked task did not finish from the new table", finished)
			}
			evaluated, _, _, err := worker.EvalTasks(10)
			if err != nil {
				t.Fatal(err)
			}
			finished = append(finished, evaluated...)
			time.Sleep(100 * time.Millisecond)
		}
		if !strings.Contains(fmt.Sprint(finished), kickedId.String()) {
			t.Fatal("the kicked task finished, but not through this runner", finished)
		}
	})
}
