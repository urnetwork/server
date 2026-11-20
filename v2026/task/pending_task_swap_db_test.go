// The table swap of pending_task (server.SwapTable, pending_task_swap.go)
// against the real task runner: every row keeps every column across the swap;
// after it, due work is claimed (FOR UPDATE SKIP LOCKED in read committed) and
// moved to finished_task, a backed-off retry keeps its error count and retries
// again, a run-once producer merges into the swapped row, and a stopped
// worker's claim is claimed again once its lease ends. The preflight refuses a
// live owner and a fresh heartbeat. A repeatable read producer whose snapshot
// predates the swap is rerun on its serialization failure, and a claim that
// waited on the swap's lock reads the new table.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Fast lock retries for a test database.
func testPendingTaskSwapSettings() *server.TableSwapSettings {
	settings := server.DefaultTableSwapSettings()
	settings.LockRetryInterval = 10 * time.Millisecond
	settings.LockAttempts = 5
	return settings
}

func swapPendingTask(t testing.TB, ctx context.Context, options server.TableSwapOptions) error {
	t.Helper()
	options.Table = pendingTaskSwapTable
	options.Logf = t.Logf
	if options.Settings == nil {
		options.Settings = testPendingTaskSwapSettings()
	}
	return server.SwapTable(ctx, options)
}

// Every pending row as JSON, by task id: every column, generated ones
// included.
func pendingTaskSwapRows(t testing.TB, ctx context.Context) map[server.Id]string {
	t.Helper()
	rows := map[server.Id]string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT task_id, row_to_json(pending_task)::text FROM pending_task`)
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

func pendingTaskSwapOid(t testing.TB, ctx context.Context) uint32 {
	t.Helper()
	var oid uint32
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT 'public.pending_task'::regclass::oid`).Scan(&oid))
	})
	return oid
}

func pendingTaskSwapOldTables(t testing.TB, ctx context.Context) (names []string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT relname FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname LIKE 'pending\_task\_old\_%' ORDER BY relname`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				server.Raise(result.Scan(&name))
				names = append(names, name)
			}
		})
	})
	return
}

// A claim as a stopped worker leaves it: a lease ahead, its claim generation
// taken, the last heartbeat at heartbeatAge ago, and no session holding the
// task's advisory lock.
func pendingTaskSwapStoppedClaim(t testing.TB, ctx context.Context, taskId server.Id, heartbeatAge time.Duration) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE pending_task SET claim_time = $2, release_time = $3, claim_generation = claim_generation + 1 WHERE task_id = $1`,
			taskId,
			server.NowUtc().Add(-heartbeatAge),
			server.NowUtc().Add(4*time.Minute),
		))
		if tag.RowsAffected() != 1 {
			t.Fatal("no pending row to claim", taskId)
		}
	})
}

// A target whose task fails while its scope is in the failing set.
type pendingTaskSwapTarget struct {
	stateLock sync.Mutex
	failing   map[server.Id]bool
}

func (self *pendingTaskSwapTarget) setFailing(scope server.Id, failing bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failing[scope] = failing
}

func (self *pendingTaskSwapTarget) target() *runOnceGenerationTarget {
	target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
	target.before = func(ctx context.Context, queued *Task) error {
		var args runOnceGenerationArgs
		server.Raise(json.Unmarshal([]byte(queued.ArgsJson), &args))
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.failing[args.Scope] {
			return errors.New("synthetic failure")
		}
		return nil
	}
	return target
}

// The swap keeps every row of every kind a queue holds, column for column:
// a backed-off retry, run-once merges, a far-future task, a due task and a
// stopped worker's claim. After it, the runner finishes the due task through
// finished_task, retries the failing one with its error count carried on, a
// producer merges into the swapped run-once row, and the stopped claim is
// claimed again once its lease ends, under its next claim generation.
func TestPendingTaskSwapKeepsClaimsRetriesAndCompletions(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		failing := &pendingTaskSwapTarget{failing: map[server.Id]bool{}}
		worker := runOnceGenerationWorker(ctx, failing.target())
		defer worker.Close()
		past := server.NowUtc().Add(-time.Hour)

		// a retry that backs off before the swap
		retryScope := server.NewId()
		failing.setFailing(retryScope, true)
		retrying := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: retryScope}, owner, RunAt(past))
		if finished, retried, _, err := worker.EvalTasks(1); err != nil || len(finished) != 0 || len(retried) != 1 || retried[0] != retrying {
			t.Fatal("the failing task did not retry", finished, retried, err)
		}
		if task := GetTasks(ctx, retrying)[retrying]; task == nil || task.RescheduleErrorCount != 1 || !task.RunAt.After(server.NowUtc()) {
			t.Fatal("the retry did not back off", task)
		}
		// run-once wakes merged into one pending row, not yet due
		mergedScope := server.NewId()
		for range 3 {
			ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: mergedScope}, owner,
				runOnceGenerationKey(mergedScope), RunAt(server.NowUtc().Add(time.Hour)))
		}
		// the one pending row under the run-once key; a merge returns the id
		// it offered, not the row's
		mergedRow := func() *Task {
			pending := runOnceGenerationPending(ctx, []server.Id{mergedScope})
			if len(pending) != 1 {
				t.Fatal("the run-once key has", len(pending), "pending rows")
			}
			for _, task := range pending {
				return task
			}
			return nil
		}
		farFuture := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(server.NowUtc().Add(30*24*time.Hour)))
		stopped := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(past))
		pendingTaskSwapStoppedClaim(t, ctx, stopped, time.Minute)
		due := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(past))

		rows := pendingTaskSwapRows(t, ctx)
		mergedBefore := mergedRow()
		if mergedBefore.RunOnceGeneration < 2 {
			t.Fatal("the run-once wakes did not merge", mergedBefore)
		}
		stoppedBefore := GetTasks(ctx, stopped)[stopped]
		oid := pendingTaskSwapOid(t, ctx)
		if err := swapPendingTask(t, ctx, server.TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
		if pendingTaskSwapOid(t, ctx) == oid || len(pendingTaskSwapOldTables(t, ctx)) != 1 {
			t.Fatal("pending_task was not swapped")
		}
		after := pendingTaskSwapRows(t, ctx)
		if len(after) != len(rows) {
			t.Fatalf("%d rows after the swap of %d", len(after), len(rows))
		}
		for taskId, row := range rows {
			if after[taskId] != row {
				t.Fatalf("row %s changed:\n%s\n%s", taskId, row, after[taskId])
			}
		}
		if task := GetTasks(ctx, farFuture)[farFuture]; task == nil || !task.RunAt.After(server.NowUtc().Add(29*24*time.Hour)) {
			t.Fatal("the far-future task changed", task)
		}

		// the due task is claimed and finished from the new table; the stopped
		// claim's lease still holds it
		if finished, retried, _, err := worker.EvalTasks(4); err != nil || len(finished) != 1 || finished[0] != due || len(retried) != 0 {
			t.Fatal("the due task did not finish after the swap", finished, retried, err)
		}
		if finishedTask := GetFinishedTasks(ctx, due)[due]; finishedTask == nil {
			t.Fatal("the due task has no finished row")
		}
		if _, ok := GetTasks(ctx, due)[due]; ok {
			t.Fatal("the finished task is still pending")
		}

		// a producer merges into the swapped run-once row
		ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: mergedScope}, owner,
			runOnceGenerationKey(mergedScope), RunAt(server.NowUtc().Add(time.Hour)))
		if task := mergedRow(); task.TaskId != mergedBefore.TaskId || task.RunOnceGeneration != mergedBefore.RunOnceGeneration+1 {
			t.Fatal("the producer did not merge into the swapped row once", task, mergedBefore)
		}

		// the stopped claim, once its lease ends, is claimed again and finishes;
		// available_block rounds a second up, so the lease ends a minute ago
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET release_time = $2 WHERE task_id = $1`, stopped, server.NowUtc().Add(-time.Minute)))
		})
		if finished, _, _, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || finished[0] != stopped {
			t.Fatal("the stopped claim was not claimed again", finished, err)
		}
		if finishedTask := GetFinishedTasks(ctx, stopped)[stopped]; finishedTask == nil {
			t.Fatal("the stopped claim has no finished row")
		}
		if stoppedBefore.ClaimGeneration < 1 {
			t.Fatal("the stopped claim had no claim generation", stoppedBefore)
		}

		// the retry carries its error count on, then succeeds
		makeDue := func(taskId server.Id) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at = $2, release_time = LEAST(release_time, $2) WHERE task_id = $1`, taskId, server.NowUtc().Add(-time.Minute)))
			})
		}
		makeDue(retrying)
		if _, retried, _, err := worker.EvalTasks(1); err != nil || len(retried) != 1 || retried[0] != retrying {
			t.Fatal("the retry did not run again", retried, err)
		}
		if task := GetTasks(ctx, retrying)[retrying]; task == nil || task.RescheduleErrorCount != 2 {
			t.Fatal("the retry lost its error count across the swap", task)
		}
		failing.setFailing(retryScope, false)
		makeDue(retrying)
		if finished, _, _, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || finished[0] != retrying {
			t.Fatal("the retry did not finish", finished, err)
		}
		if finishedTask := GetFinishedTasks(ctx, retrying)[retrying]; finishedTask == nil {
			t.Fatal("the retry has no finished row")
		}
	})
}

// A task with a live owner (its worker holds the task's advisory lock and
// runs it) refuses the swap, dry run or not, and the table stays; once the
// owner finishes, the swap runs.
func TestPendingTaskSwapRefusesWhileAWorkerOwnsATask(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		entered, released := make(chan struct{}), make(chan struct{})
		var enterOnce sync.Once
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		target.before = func(runCtx context.Context, queued *Task) error {
			enterOnce.Do(func() { close(entered) })
			taskQueueWait(runCtx, released)
			return nil
		}
		worker := runOnceGenerationWorker(ctx, target)
		defer worker.Close()
		running := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		evalDone := make(chan error, 1)
		go func() {
			var evalErr error
			server.HandleError(func() {
				if finished, _, _, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || finished[0] != running {
					evalErr = errors.Join(err, errors.New("the running task did not finish"))
				}
			}, func(err error) { evalErr = err })
			evalDone <- evalErr
		}()
		taskQueueWait(ctx, entered)

		oid := pendingTaskSwapOid(t, ctx)
		for _, dryRun := range []bool{true, false} {
			err := swapPendingTask(t, ctx, server.TableSwapOptions{DryRun: dryRun})
			if err == nil || !strings.Contains(err.Error(), "a taskworker is running") || !strings.Contains(err.Error(), running.String()) {
				t.Fatal("the swap ran beside a live owner", dryRun, err)
			}
		}
		if pendingTaskSwapOid(t, ctx) != oid || 0 < len(pendingTaskSwapOldTables(t, ctx)) {
			t.Fatal("the refused swap changed pending_task")
		}
		close(released)
		if err := taskQueueError(ctx, evalDone); err != nil {
			t.Fatal(err)
		}
		if err := swapPendingTask(t, ctx, server.TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
	})
}

// A lease whose heartbeat is fresh refuses the swap even with no lock held: a
// worker ran moments ago. An older heartbeat is a stopped worker's claim,
// which the swap copies with its lease.
func TestPendingTaskSwapRefusesAFreshHeartbeat(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		leased := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		pendingTaskSwapStoppedClaim(t, ctx, leased, 0)
		if err := swapPendingTask(t, ctx, server.TableSwapOptions{}); err == nil || !strings.Contains(err.Error(), "heartbeat within") {
			t.Fatal("the swap ran beside a fresh heartbeat", err)
		}
		pendingTaskSwapStoppedClaim(t, ctx, leased, 2*ReleaseTimeout)
		before := GetTasks(ctx, leased)[leased]
		if err := swapPendingTask(t, ctx, server.TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
		after := GetTasks(ctx, leased)[leased]
		if after == nil || !after.ReleaseTime.Equal(before.ReleaseTime) || after.ClaimGeneration != before.ClaimGeneration {
			t.Fatal("the stopped claim's lease did not survive the swap", before, after)
		}
	})
}

// A repeatable read producer whose snapshot predates the swap cannot see the
// copied row its ON CONFLICT meets: PostgreSQL raises a serialization failure,
// server.Tx reruns it with a fresh snapshot, and the rerun merges into the
// swapped row once. Barriers order the snapshot, the swap and the write.
func TestPendingTaskSwapRepeatableReadProducerRerunsAfterTheSwap(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		original := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(time.Hour)))
		generation := GetTasks(ctx, original)[original].RunOnceGeneration

		snapshotTaken, swapped := make(chan struct{}), make(chan struct{})
		attempts := 0
		producerDone := make(chan error, 1)
		go func() {
			var producerErr error
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					attempts += 1
					if attempts == 1 {
						// the transaction's snapshot, taken before the swap
						var one int
						server.Raise(tx.QueryRow(ctx, `SELECT 1`).Scan(&one))
						close(snapshotTaken)
						taskQueueWait(ctx, swapped)
					}
					ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
						runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(time.Hour)))
				})
			}, func(err error) { producerErr = err })
			producerDone <- producerErr
		}()
		taskQueueWait(ctx, snapshotTaken)
		if err := swapPendingTask(t, ctx, server.TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
		close(swapped)
		if err := taskQueueError(ctx, producerDone); err != nil {
			t.Fatal(err)
		}
		if attempts != 2 {
			t.Fatalf("the producer ran %d times, not once and a rerun", attempts)
		}
		pending := runOnceGenerationPending(ctx, []server.Id{scope})
		if len(pending) != 1 {
			t.Fatal("the producer made a second row", len(pending))
		}
		for _, task := range pending {
			if task.TaskId != original || task.RunOnceGeneration != generation+1 {
				t.Fatal("the rerun did not merge into the swapped row once", task, generation)
			}
		}
	})
}

// Holds the swap script under pending_task's lock, inside its locked
// transaction (server.TableSwapHooks.UnderLock), until release; held closes
// once it is there. The swap's transaction is cut off LockTimeout +
// MaxLockHold after its request for the lock, so the hold must end before.
// (The hold was a wait in the profile's locked check until 11-17; the
// rehearsal now runs that check too, outside the lock.)
func pendingTaskSwapHoldUnderLock(ctx context.Context) (hooks *server.TableSwapHooks, held <-chan struct{}, release func()) {
	heldSignal, releaseSignal := make(chan struct{}), make(chan struct{})
	var heldOnce, releaseOnce sync.Once
	hooks = &server.TableSwapHooks{
		UnderLock: func(ctx context.Context) {
			heldOnce.Do(func() { close(heldSignal) })
			taskQueueWait(ctx, releaseSignal)
		},
	}
	release = func() {
		releaseOnce.Do(func() { close(releaseSignal) })
	}
	return hooks, heldSignal, release
}

// Waits until some statement waits for a lock of the given type.
func pendingTaskSwapAwaitLockWait(t testing.TB, ctx context.Context, lockType string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = $1 AND NOT granted`, lockType).Scan(&waiting))
		})
		if 0 < waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing waited on a %s lock", lockType)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A read committed claim that waited on the swap's lock plans and reads after
// the commit, so it claims the copied row from the new table and finishes it.
func TestPendingTaskSwapClaimWaitingOnTheLockReadsTheNewTable(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		due := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(server.NowUtc().Add(-time.Hour)))

		hooks, held, release := pendingTaskSwapHoldUnderLock(ctx)
		defer release()
		settings := testPendingTaskSwapSettings()
		settings.LockTimeout = 45 * time.Second
		swapDone := make(chan error, 1)
		go func() {
			swapDone <- swapPendingTask(t, ctx, server.TableSwapOptions{Settings: settings, Hooks: hooks})
		}()
		// the swap holds pending_task's lock, inside its locked transaction
		taskQueueWait(ctx, held)

		evalDone := make(chan error, 1)
		var finished []server.Id
		go func() {
			var evalErr error
			server.HandleError(func() {
				finished, _, _, evalErr = worker.EvalTasks(1)
			}, func(err error) { evalErr = err })
			evalDone <- evalErr
		}()
		// the claim's statement waits on the swap's lock
		pendingTaskSwapAwaitLockWait(t, ctx, "relation")
		release()
		if err := taskQueueError(ctx, swapDone); err != nil {
			t.Fatal(err)
		}
		if err := taskQueueError(ctx, evalDone); err != nil {
			t.Fatal(err)
		}
		if len(finished) != 1 || finished[0] != due {
			t.Fatal("the claim that waited on the lock did not finish the copied task", finished)
		}
		if GetFinishedTasks(ctx, due)[due] == nil || len(pendingTaskSwapOldTables(t, ctx)) != 1 {
			t.Fatal("the task did not move to finished_task from the new table")
		}
	})
}

// The locked check alone refuses a live owner: with the preflight passing
// (its state read before a worker claimed), the check under the lock finds
// the owner's advisory lock and the script rolls back.
func TestPendingTaskSwapLockedCheckRefusesALiveOwner(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		entered, released := make(chan struct{}), make(chan struct{})
		var enterOnce sync.Once
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		target.before = func(runCtx context.Context, queued *Task) error {
			enterOnce.Do(func() { close(entered) })
			taskQueueWait(runCtx, released)
			return nil
		}
		worker := runOnceGenerationWorker(ctx, target)
		defer worker.Close()
		running := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		evalDone := make(chan error, 1)
		go func() {
			var evalErr error
			server.HandleError(func() {
				_, _, _, evalErr = worker.EvalTasks(1)
			}, func(err error) { evalErr = err })
			evalDone <- evalErr
		}()
		taskQueueWait(ctx, entered)

		original := pendingTaskSwapProfile.Preflight
		pendingTaskSwapProfile.Preflight = func(context.Context, server.PgQuerier) ([]string, error) {
			return []string{"preflight read before the claim"}, nil
		}
		defer func() { pendingTaskSwapProfile.Preflight = original }()
		oid := pendingTaskSwapOid(t, ctx)
		err := swapPendingTask(t, ctx, server.TableSwapOptions{})
		if err == nil || !strings.Contains(err.Error(), "a taskworker is running") || !strings.Contains(err.Error(), running.String()) {
			t.Fatal("the locked check did not refuse the live owner", err)
		}
		if pendingTaskSwapOid(t, ctx) != oid || 0 < len(pendingTaskSwapOldTables(t, ctx)) {
			t.Fatal("the refused swap changed pending_task")
		}
		close(released)
		if err := taskQueueError(ctx, evalDone); err != nil {
			t.Fatal(err)
		}
	})
}

// The database computes each task's advisory lock key as taskAdvisoryLockKey
// does, so the locked check finds exactly the locks owners take.
func TestPendingTaskSwapLockKeyMatchesTheOwnersKey(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		taskIds := []server.Id{{}, server.NewId(), server.NewId(), server.NewId()}
		for i := range taskIds[3] {
			taskIds[3][i] = 0xff
		}
		for _, taskId := range taskIds {
			var key int64
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, fmt.Sprintf(`SELECT
					('x' || substr(replace($1::uuid::text, '-', ''), 1, 16))::bit(64)::bigint
					# ('x' || substr(replace($1::uuid::text, '-', ''), 17, 16))::bit(64)::bigint
					# (%d)::bigint`, int64(taskAdvisoryLockNamespace)), taskId).Scan(&key))
			})
			if key != taskAdvisoryLockKey(taskId) {
				t.Fatalf("%s: the database's key %d, the owner's %d", taskId, key, taskAdvisoryLockKey(taskId))
			}
			if !strings.Contains(pendingTaskSwapLeasesSql(pendingTaskSwapTable), fmt.Sprintf("# (%d)::bigint", int64(taskAdvisoryLockNamespace))) {
				t.Fatal("the leases query does not use the owner's namespace")
			}
		}
	})
}
