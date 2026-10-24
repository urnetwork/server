// finished_task on daily partitions (PARTITION-MAINTENANCE-PLAN.md C3), against
// a test database cut over ten days back: failed Posts keep their retries
// through the retention pass, the drop waits or defers for a running retry
// and a held owner or row, a task finishes once whatever its end times, the
// runway keeps the days ahead, and the readers' lookups by task id stay index
// probes.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Runs on a database whose finished_task is partitioned: the days from ten
// days back through the days ahead, older history in the legacy partition.
func finishedTaskPartitionEnv(t *testing.T, run func(testing.TB, context.Context)) {
	t.Helper()
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		server.Testing_PartitionFinishedTasks(ctx, server.TestingFinishedTaskCutoverDay(server.NowUtc()))
		if !finishedTaskPartitioned(ctx) {
			t.Fatal("the test database did not cut over")
		}
		run(t, ctx)
	})
}

// Short lock waits, so a drop that has to wait gives up quickly, and a quiet
// check that admits any transaction younger than an hour.
func testFinishedTaskPartitionSettings() *server.FinishedTaskPartitionSettings {
	settings := server.DefaultFinishedTaskPartitionSettings()
	settings.ExclusiveLockTimeout = 300 * time.Millisecond
	settings.MaxTransactionAge = time.Hour
	return settings
}

func finishedTaskPartitionDeadline() time.Time {
	return time.Now().Add(time.Minute)
}

func finishedTaskPartitionLeafExists(t testing.TB, ctx context.Context, leaf string) (exists bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.' || quote_ident($1)) IS NOT NULL`, leaf).Scan(&exists))
	})
	return
}

func finishedTaskRowCount(t testing.TB, ctx context.Context, taskId server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM finished_task WHERE task_id=$1`, taskId).Scan(&count))
	})
	return
}

// Moves a finished row to another end time, and so to that day's partition.
func ageFinishedTask(t testing.TB, ctx context.Context, taskId server.Id, runEndTime time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(ctx, `UPDATE finished_task SET run_end_time=$2 WHERE task_id=$1`, taskId, runEndTime))
		if tag.RowsAffected() != 1 {
			server.Raise(fmt.Errorf("aged %d rows of %s", tag.RowsAffected(), taskId))
		}
	}, server.TxReadCommitted, server.OptNoRetry())
}

// Sets a pending Post retry's next run under its pending owner.
func setFinishedTaskRetryRunAt(ctx context.Context, retryId server.Id, runAt time.Time) {
	server.OwnedTx(ctx, []server.PgOwnershipKey{PendingTaskOwnershipKey(retryId, nil)}, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$3 WHERE task_id=$1`, retryId, runAt, time.Time{}))
	}, server.TxReadCommitted, server.OptNoRetry())
}

func createFinishedTaskPostEffects(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE test_finished_task_post_effect (scope uuid NOT NULL)`))
	})
}

// The Post effects that committed for scope.
func finishedTaskPostEffects(t testing.TB, ctx context.Context, scope server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM test_finished_task_post_effect WHERE scope=$1`, scope).Scan(&count))
	})
	return
}

type finishedTaskPartitionPostKey struct{}

// The Posts of a test: each scope's first attempt fails, so its task leaves a
// failed Post and a pending retry; later attempts record one effect in their
// transaction. A held scope's next attempt waits, its finished owner and its
// row held, until released.
type finishedTaskPartitionPosts struct {
	stateLock sync.Mutex
	attempts  map[server.Id]int
	held      server.Id
	entered   chan struct{}
	released  chan struct{}
}

func newFinishedTaskPartitionPosts() *finishedTaskPartitionPosts {
	return &finishedTaskPartitionPosts{
		attempts: map[server.Id]int{},
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (self *finishedTaskPartitionPosts) hold(scope server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.held = scope
}

func (self *finishedTaskPartitionPosts) release() {
	close(self.released)
}

func (self *finishedTaskPartitionPosts) attempt(scope server.Id) (attempt int, held bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.attempts[scope] += 1
	held = self.held == scope
	if held {
		self.held = server.Id{}
	}
	return self.attempts[scope], held
}

func (self *finishedTaskPartitionPosts) post(ctx context.Context, tx server.PgTx, scope server.Id) error {
	attempt, held := self.attempt(scope)
	if attempt == 1 {
		return errors.New("synthetic first Post failure")
	}
	if held {
		close(self.entered)
		taskQueueWait(ctx, self.released)
	}
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO test_finished_task_post_effect (scope) VALUES ($1)`, scope))
	return nil
}

func finishedTaskPartitionPost(args *runOnceGenerationArgs, _ *struct{}, owner *session.ClientSession, tx server.PgTx) error {
	return owner.Ctx.Value(finishedTaskPartitionPostKey{}).(*finishedTaskPartitionPosts).post(owner.Ctx, tx, args.Scope)
}

// A worker whose target declares its queue ownership, so a Post retry holds
// its row's finished owner while it runs, as the owned production targets do.
func finishedTaskPartitionWorker(ctx context.Context, posts *finishedTaskPartitionPosts) *TaskWorker {
	worker := NewTaskWorkerWithDefaults(context.WithValue(ctx, finishedTaskPartitionPostKey{}, posts))
	worker.AddTargets(&taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{
		Target: NewTaskTargetWithPost(runOnceGenerationWork, finishedTaskPartitionPost),
	}})
	return worker
}

// Runs a task whose Post fails, and returns its id and its Post retry's id,
// the retry held back until the test sets it due.
func finishFinishedTaskWithFailedPost(t testing.TB, ctx context.Context, worker *TaskWorker, owner *session.ClientSession, scope server.Id) (taskId server.Id, retryId server.Id) {
	t.Helper()
	taskId = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished)+len(retried) != 0 || len(posts) != 1 || posts[0] != taskId {
		t.Fatal("the task did not finish with a failed Post", finished, retried, posts, err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE function_name=$1 AND args_json::jsonb->>'task_id'=$2`,
			functionName(worker.RunPost), taskId.String()).Scan(&retryId))
	}, server.TxReadCommitted, server.OptNoRetry())
	setFinishedTaskRetryRunAt(ctx, retryId, server.NowUtc().Add(24*time.Hour))
	return
}

// Runs the Post retry now and requires that it finishes.
func runFinishedTaskRetry(t testing.TB, ctx context.Context, worker *TaskWorker, retryId server.Id) {
	t.Helper()
	setFinishedTaskRetryRunAt(ctx, retryId, time.Time{})
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != retryId || len(retried)+len(posts) != 0 {
		t.Fatal("the Post retry did not finish", finished, retried, posts, err)
	}
}

// With a fixed clock, a failed Post 25 hours old and one 6 days old keep
// their rows through a retention pass and a cleanup run at that clock, and
// their retries then run their Posts, once each. The same pass releases and
// drops the day of a failed Post 9 days old, past its 7, whose retry then
// finds no row and completes as a no-op.
func TestFinishedTaskPartitionKeepsFailedPostsForTheirRetries(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		now := server.NowUtc()
		clockCtx := context.WithValue(ctx, taskCleanupClockKey{}, func() time.Time { return now })
		createFinishedTaskPostEffects(t, ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		posts := newFinishedTaskPartitionPosts()
		worker := finishedTaskPartitionWorker(ctx, posts)
		defer worker.Close()
		ages := []time.Duration{25 * time.Hour, 6 * 24 * time.Hour, 9 * 24 * time.Hour}
		scopes := make([]server.Id, len(ages))
		taskIds := make([]server.Id, len(ages))
		retryIds := make([]server.Id, len(ages))
		for index, age := range ages {
			scopes[index] = server.NewId()
			taskIds[index], retryIds[index] = finishFinishedTaskWithFailedPost(t, ctx, worker, owner, scopes[index])
			ageFinishedTask(t, ctx, taskIds[index], now.Add(-age))
		}

		family := newFinishedTaskPartitionFamily(testFinishedTaskPartitionSettings())
		report, _ := family.Retire(clockCtx, now, server.PartitionFamilyState{}, finishedTaskPartitionDeadline())
		for index, age := range ages {
			leaf := family.LeafName("finished_task", family.RangeAt(now.Add(-age)))
			if slices.Contains(report.Dropped, leaf) != (index == 2) || report.Blocked[leaf] != "" {
				t.Fatalf("the day of a failed Post %s old: dropped %v blocked %v", age, report.Dropped, report.Blocked)
			}
		}
		cleanupOwner := session.NewLocalClientSession(clockCtx, "", nil)
		defer cleanupOwner.Cancel()
		if result, err := TaskCleanup(&TaskCleanupArgs{}, cleanupOwner); err != nil || result.Removed != 0 || result.Next != nil {
			t.Fatal("the cleanup deleted rows of a partitioned table", result, err)
		}
		if removed := RemoveFinishedTasks(ctx, now.Add(time.Hour), now.Add(time.Hour)); removed != 0 {
			t.Fatal("row retention ran on a partitioned table", removed)
		}
		finished := GetFinishedTasks(ctx, taskIds...)
		if len(finished) != 2 || finished[taskIds[0]] == nil || finished[taskIds[1]] == nil {
			t.Fatal("a failed Post inside its 7 days lost its row, or one past them kept it", len(finished))
		}

		for index := range 2 {
			runFinishedTaskRetry(t, ctx, worker, retryIds[index])
			row := GetFinishedTasks(ctx, taskIds[index])[taskIds[index]]
			if finishedTaskPostEffects(t, ctx, scopes[index]) != 1 || row == nil || !row.PostCompleted {
				t.Fatalf("the retry of a failed Post %s old did not run its Post once", ages[index])
			}
		}
		orphaned := testutil.ToFloat64(orphanedRunPostCounter)
		runFinishedTaskRetry(t, ctx, worker, retryIds[2])
		if finishedTaskPostEffects(t, ctx, scopes[2]) != 0 || testutil.ToFloat64(orphanedRunPostCounter) != orphaned+1 {
			t.Fatal("the retry of a released Post did not complete as a no-op")
		}
	})
}

// A drop racing a running Post retry: the retry holds its row's finished
// owner and its row while its Post runs, so the release leaves the row, and
// the drop defers rather than queue behind the retry's transaction. The retry
// then runs its Post once, which acknowledges the row, and the next pass
// drops the day.
func TestFinishedTaskPartitionDropDefersRunningPostRetry(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		now := server.NowUtc()
		createFinishedTaskPostEffects(t, ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		posts := newFinishedTaskPartitionPosts()
		worker := finishedTaskPartitionWorker(ctx, posts)
		defer worker.Close()
		scope := server.NewId()
		taskId, retryId := finishFinishedTaskWithFailedPost(t, ctx, worker, owner, scope)
		ageFinishedTask(t, ctx, taskId, now.Add(-9*24*time.Hour))

		posts.hold(scope)
		setFinishedTaskRetryRunAt(ctx, retryId, time.Time{})
		evalDone := make(chan error, 1)
		go func() {
			var evalErr error
			server.HandleError(func() {
				finished, retried, postIds, err := worker.EvalTasks(1)
				if err != nil || len(finished) != 1 || finished[0] != retryId || len(retried)+len(postIds) != 0 {
					evalErr = fmt.Errorf("the held retry did not finish: %v %v %v %v", finished, retried, postIds, err)
				}
			}, func(err error) { evalErr = err })
			evalDone <- evalErr
		}()
		taskQueueWait(ctx, posts.entered)

		var refused atomic.Int64
		observedCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused {
				refused.Add(1)
			}
		})
		// the retry's transaction began before the drop's: the fence defers
		// at once instead of queueing behind it
		settings := testFinishedTaskPartitionSettings()
		settings.MaxTransactionAge = 0
		family := newFinishedTaskPartitionFamily(settings)
		leaf := family.LeafName("finished_task", family.RangeAt(now.Add(-9*24*time.Hour)))
		report, _ := family.Retire(observedCtx, now, server.PartitionFamilyState{}, finishedTaskPartitionDeadline())
		if slices.Contains(report.Dropped, leaf) || !strings.Contains(report.Blocked[leaf], "hold finished_task locks") || refused.Load() == 0 {
			t.Fatalf("a drop raced the running retry: dropped %v blocked %v refused %d", report.Dropped, report.Blocked, refused.Load())
		}
		if finishedTaskRowCount(t, ctx, taskId) != 1 || !finishedTaskPartitionLeafExists(t, ctx, leaf) {
			t.Fatal("the running retry's row or day is gone")
		}

		posts.release()
		if err := taskQueueError(ctx, evalDone); err != nil {
			t.Fatal(err)
		}
		if row := GetFinishedTasks(ctx, taskId)[taskId]; finishedTaskPostEffects(t, ctx, scope) != 1 || row == nil || !row.PostCompleted {
			t.Fatal("the retry did not run its Post once")
		}
		report, _ = family.Retire(ctx, now, server.PartitionFamilyState{}, finishedTaskPartitionDeadline())
		if !slices.Contains(report.Dropped, leaf) || finishedTaskRowCount(t, ctx, taskId) != 0 || finishedTaskPostEffects(t, ctx, scope) != 1 {
			t.Fatalf("the day did not drop once its retry finished: dropped %v blocked %v", report.Dropped, report.Blocked)
		}
	})
}

// Seeds failed Posts 9 days old, past their retention, in one day.
func seedFinishedTaskPartitionFailedPosts(t testing.TB, ctx context.Context, count int, runEndTime time.Time) []server.Id {
	t.Helper()
	ids := make([]server.Id, count)
	for index := range ids {
		ids[index] = server.NewId()
	}
	slices.SortFunc(ids, server.Id.Cmp)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO finished_task
            (task_id,function_name,args_json,run_at,run_priority,run_max_time_seconds,
             run_start_time,run_end_time,result_json,post_error,post_completed)
            SELECT task_id,'synthetic_aged_post_owner','{}',$2,0,30,$2,$2,'{}','synthetic post failure',false
            FROM unnest($1::uuid[]) AS observed(task_id)`, ids, runEndTime))
	}, server.TxReadCommitted, server.OptNoRetry())
	return ids
}

// The partitioned form of TestTaskQueueRetentionDefersLiveFinishedOwnerBeforeRow.
// Holding only a 7-day-old failed Post's finished owner keeps its row and
// defers its day: the release leaves its group whole while the next group of
// the day goes, and the drop transaction refuses on the rows left. Once the
// owner is free, the next pass releases the rest and drops the day, with no
// transaction rerun.
func TestFinishedTaskPartitionDropDefersHeldFinishedOwner(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		now := server.NowUtc()
		settings := testFinishedTaskPartitionSettings()
		ids := seedFinishedTaskPartitionFailedPosts(t, ctx, settings.ReleaseGroupSize+1, now.Add(-9*24*time.Hour))
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		var refused atomic.Int64
		observedCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused {
				refused.Add(1)
			}
		})
		family := newFinishedTaskPartitionFamily(settings)
		leaf := family.LeafName("finished_task", family.RangeAt(now.Add(-9*24*time.Hour)))
		server.OwnedTx(ctx, []server.PgOwnershipKey{taskFinishedOwnershipKey(ids[0])}, func(tx server.PgTx) {
			report, _ := family.Retire(observedCtx, now, server.PartitionFamilyState{}, finishedTaskPartitionDeadline())
			if slices.Contains(report.Dropped, leaf) || !strings.Contains(report.Blocked[leaf], "wait for their finished owners") || refused.Load() != 1 {
				t.Fatalf("the drop removed rows under a live finished owner: dropped %v blocked %v refused %d", report.Dropped, report.Blocked, refused.Load())
			}
			if remaining := GetFinishedTasks(ctx, ids...); len(remaining) != settings.ReleaseGroupSize || remaining[ids[0]] == nil {
				t.Fatal("the refused group did not stay whole, or the next group was not released", len(remaining))
			}
			var observed server.Id
			server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM finished_task WHERE task_id=$1 FOR UPDATE NOWAIT`, ids[0]).Scan(&observed))
			if observed != ids[0] {
				t.Fatal("the held owner lost its row")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		report, _ := family.Retire(ctx, now, server.PartitionFamilyState{}, finishedTaskPartitionDeadline())
		if !slices.Contains(report.Dropped, leaf) || len(GetFinishedTasks(ctx, ids...)) != 0 || reruns.Load() != 0 {
			t.Fatalf("the freed owner's day did not drop without a rerun: dropped %v blocked %v reruns %d", report.Dropped, report.Blocked, reruns.Load())
		}
	})
}

// The partitioned form of TestTaskQueueRetentionSkipsHeldRowWithoutBlockingPeers.
// A drop detaches a whole day under the table's exclusive lock, so a row
// another transaction holds defers every drop while it is held, never only its
// own day. The held row is never removed, and no drop waits the holder out or
// queues completions behind it: each gives up at its lock wait, or defers at
// once when the holder's transaction is older than the fence admits, and a
// completion still finishes while the holder runs. Once the holder ends, the
// next pass drops every expired day, with no transaction rerun.
func TestFinishedTaskPartitionHeldRowDefersDropsBoundedly(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		now := server.NowUtc()
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		ages := []time.Duration{9 * 24 * time.Hour, 10 * 24 * time.Hour}
		ids := []server.Id{}
		for _, age := range ages {
			ids = append(ids, seedFinishedTaskPartitionFailedPosts(t, ctx, 1, now.Add(-age))...)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE finished_task SET post_completed=true WHERE task_id=ANY($1)`, ids))
		}, server.TxReadCommitted, server.OptNoRetry())
		family := newFinishedTaskPartitionFamily(testFinishedTaskPartitionSettings())
		quietSettings := testFinishedTaskPartitionSettings()
		quietSettings.MaxTransactionAge = 0
		quiet := newFinishedTaskPartitionFamily(quietSettings)
		leaves := []string{}
		for _, age := range ages {
			leaves = append(leaves, family.LeafName("finished_task", family.RangeAt(now.Add(-age))))
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		worker := runOnceGenerationWorker(ctx, target)
		defer worker.Close()

		server.Tx(ctx, func(tx server.PgTx) {
			var observed server.Id
			server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM finished_task WHERE task_id=$1 FOR UPDATE`, ids[0]).Scan(&observed))
			report, _ := family.Retire(ctx, now, server.PartitionFamilyState{}, finishedTaskPartitionDeadline())
			for _, leaf := range leaves {
				if slices.Contains(report.Dropped, leaf) || !strings.Contains(report.Blocked[leaf], "lock timeout") {
					t.Fatalf("a drop did not give up at its lock wait: dropped %v blocked %v", report.Dropped, report.Blocked)
				}
			}
			report, _ = quiet.Retire(ctx, now, server.PartitionFamilyState{}, finishedTaskPartitionDeadline())
			for _, leaf := range leaves {
				if slices.Contains(report.Dropped, leaf) || !strings.Contains(report.Blocked[leaf], "hold finished_task locks") {
					t.Fatalf("a quiet drop did not defer at once: dropped %v blocked %v", report.Dropped, report.Blocked)
				}
			}
			id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
			if finished, retried, posts, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
				t.Fatal("a completion did not finish beside the held row", finished, retried, posts, err)
			}
			if len(GetFinishedTasks(ctx, ids...)) != len(ids) {
				t.Fatal("a held or deferred row was removed")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		report, _ := family.Retire(ctx, now, server.PartitionFamilyState{}, finishedTaskPartitionDeadline())
		for _, leaf := range leaves {
			if !slices.Contains(report.Dropped, leaf) {
				t.Fatalf("a day did not drop once the holder ended: dropped %v blocked %v", report.Dropped, report.Blocked)
			}
		}
		if len(GetFinishedTasks(ctx, ids...)) != 0 || reruns.Load() != 0 {
			t.Fatal("the released days kept rows, or a transaction reran")
		}
	})
}

// Makes taskId pending again under its own id, as a reused id would.
func repeatFinishedTaskId(t testing.TB, ctx context.Context, taskId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(ctx, `INSERT INTO pending_task (
            task_id,function_name,args_json,client_address,client_address_hash,
            client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
            run_max_time_seconds,claim_time,release_time)
            SELECT task_id,function_name,args_json,client_address,client_address_hash,
                client_address_port,client_by_jwt_json,$2,run_once_key,run_priority,
                run_max_time_seconds,$3,$3
            FROM finished_task WHERE task_id=$1`, taskId, server.NowUtc().Add(-time.Hour), time.Time{}))
		if tag.RowsAffected() != 1 {
			server.Raise(fmt.Errorf("repeated %d pending rows of %s", tag.RowsAffected(), taskId))
		}
	}, server.TxReadCommitted, server.OptNoRetry())
}

var errFinishedTaskControlRollback = errors.New("synthetic control rollback")

// One task id cannot finish twice, with its second end time in the same day's
// partition or in another day's. The composite key alone would take the
// second row, so the move, the only writer, refuses it: the pending row stays,
// and the first row stays the one durable payload.
func TestFinishedTaskIdentityRefusesSecondEndTime(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, _, results := runOnceGenerationClaimResults(t, ctx, false)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		taskId := ids[0]
		target := worker.targets[results[0].task.FunctionName]
		worker.finalizeTask(results[0])
		// the claim's advisory key is the task id's, so a claim of the
		// repeated id needs it free
		guard.release()
		first := GetFinishedTasks(ctx, taskId)[taskId]
		if first == nil || finishedTaskRowCount(t, ctx, taskId) != 1 {
			t.Fatal("the task did not finish once")
		}

		// the control: the key takes a second row of the task
		var controlCount int
		var controlErr error
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO finished_task
                    (task_id,function_name,args_json,run_at,run_priority,run_max_time_seconds,run_start_time,run_end_time,result_json)
                    VALUES ($1,'synthetic','{}',$2,0,30,$2,$2,'{}')`, taskId, first.RunEndTime.Add(time.Second)))
				server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM finished_task WHERE task_id=$1`, taskId).Scan(&controlCount))
				server.Raise(errFinishedTaskControlRollback)
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { controlErr = err })
		if !errors.Is(controlErr, errFinishedTaskControlRollback) || controlCount != 2 {
			t.Fatal("the composite key refused a second end time; the guard is not the only enforcement", controlErr, controlCount)
		}

		for _, runEndTime := range []time.Time{first.RunEndTime.Add(time.Second), first.RunEndTime.AddDate(0, 0, 1)} {
			repeatFinishedTaskId(t, ctx, taskId)
			claimed, claimGuard, err := worker.takeTasks(1)
			if err != nil || claimGuard == nil || claimed[taskId] == nil {
				t.Fatal("the repeated id was not claimed", err)
			}
			result := worker.executeTask(ctx, claimed[taskId], target)
			result.runEndTime = runEndTime
			var finalizeErr error
			server.HandleError(func() { worker.finalizeTask(result) }, func(err error) { finalizeErr = err })
			claimGuard.release()
			if !errors.Is(finalizeErr, errFinishedTaskIdentityExists) {
				t.Fatal("a second end time of one task finished", runEndTime, finalizeErr)
			}
			if finishedTaskRowCount(t, ctx, taskId) != 1 || GetTasks(ctx, taskId)[taskId] == nil {
				t.Fatal("the refused move left a second row or lost its pending row")
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE task_id=$1`, taskId))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		if row := GetFinishedTasks(ctx, taskId)[taskId]; row == nil || !row.RunEndTime.Equal(first.RunEndTime) || row.ResultJson != first.ResultJson {
			t.Fatal("the task's one durable payload changed")
		}
	})
}

// A batch whose member already finished, in another day's partition, is
// refused as a whole and falls back to its members: the other member
// finishes on its own, and the finished one keeps its single row.
func TestFinishedTaskIdentityBatchFallsBackPastFinishedMember(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, _, results := runOnceGenerationClaimResults(t, ctx, true)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO finished_task
                (task_id,function_name,args_json,run_at,run_priority,run_max_time_seconds,run_start_time,run_end_time,result_json)
                VALUES ($1,'synthetic','{}',$2,0,30,$2,$2,'{}')`, ids[0], server.NowUtc().AddDate(0, 0, -1)))
		}, server.TxReadCommitted, server.OptNoRetry())
		retrySingles, err := worker.finalizeTaskBatch(results)
		if !retrySingles || !errors.Is(err, errFinishedTaskIdentityExists) {
			t.Fatal("the batch did not refuse its finished member and fall back", retrySingles, err)
		}
		if finishedTaskRowCount(t, ctx, ids[0]) != 1 || finishedTaskRowCount(t, ctx, ids[1]) != 0 || len(GetTasks(ctx, ids...)) != 2 {
			t.Fatal("the refused batch moved a member")
		}
		worker.finalizeTask(results[1])
		var finalizeErr error
		server.HandleError(func() { worker.finalizeTask(results[0]) }, func(err error) { finalizeErr = err })
		if !errors.Is(finalizeErr, errFinishedTaskIdentityExists) || finishedTaskRowCount(t, ctx, ids[0]) != 1 || finishedTaskRowCount(t, ctx, ids[1]) != 1 {
			t.Fatal("the fallback did not finish the other member alone", finalizeErr)
		}
	})
}

// A completion repeated as after a commit whose acknowledgement was lost,
// with the same end time or one in another day's partition, finds its
// pending row gone: one durable row, and one Post effect.
func TestFinishedTaskCompletionRepeatedAfterUnacknowledgedCommit(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		createFinishedTaskPostEffects(t, ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		claimed, guard, err := worker.takeTasks(1)
		if err != nil || guard == nil || claimed[id] == nil {
			t.Fatal("the task was not claimed", err)
		}
		defer guard.release()
		postCalls := 0
		result := &taskExecutionResult{task: claimed[id], resultJson: "{}", runStartTime: server.NowUtc(), runEndTime: server.NowUtc(),
			runPost: func(tx server.PgTx) ([]server.PostFunction, error) {
				postCalls += 1
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO test_finished_task_post_effect (scope) VALUES ($1)`, scope))
				return nil, nil
			}}
		worker.finalizeTask(result)
		for _, runEndTime := range []time.Time{result.runEndTime, result.runEndTime.AddDate(0, 0, 1)} {
			repeated := *result
			repeated.runEndTime = runEndTime
			var finalizeErr error
			server.HandleError(func() { worker.finalizeTask(&repeated) }, func(err error) { finalizeErr = err })
			if !errors.Is(finalizeErr, errTaskClaimOwnership) {
				t.Fatal("the repeated completion was not refused", finalizeErr)
			}
		}
		if finishedTaskRowCount(t, ctx, id) != 1 || postCalls != 1 || finishedTaskPostEffects(t, ctx, scope) != 1 || len(GetTasks(ctx, id)) != 0 {
			t.Fatal("a repeated completion wrote a second row or a second Post effect", postCalls)
		}
	})
}

// After the cutover the runway keeps today and the days ahead: it recreates a
// day it missed, catches up two days later in one pass, and a writer whose end
// time has no day creates it.
func TestFinishedTaskPartitionRunwayCreatesTheDaysAhead(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		now := server.NowUtc()
		settings := testFinishedTaskPartitionSettings()
		family := newFinishedTaskPartitionFamily(settings)
		last := family.LeafName("finished_task", family.RangeAt(now.AddDate(0, 0, settings.AheadDays)))
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE finished_task DETACH PARTITION %s`, last)))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, last)))
		}, server.TxReadCommitted, server.OptNoRetry())
		report := family.EnsureRunway(ctx, now)
		if !slices.Equal(report.Created, []string{last}) || !report.CurrentCovered || report.FutureRanges != settings.AheadDays || 0 < len(report.Errors) {
			t.Fatalf("the runway did not recreate its missed day: %+v", report)
		}
		later := now.AddDate(0, 0, 2)
		want := []string{
			family.LeafName("finished_task", family.RangeAt(later.AddDate(0, 0, settings.AheadDays-1))),
			family.LeafName("finished_task", family.RangeAt(later.AddDate(0, 0, settings.AheadDays))),
		}
		if report := family.EnsureRunway(ctx, later); !slices.Equal(report.Created, want) || report.FutureRanges != settings.AheadDays {
			t.Fatalf("the runway did not catch up: %+v", report)
		}
		far := now.AddDate(0, 0, 9)
		created, err := family.EnsureRangeFor(ctx, far)
		if err != nil || !slices.Equal(created, []string{family.LeafName("finished_task", family.RangeAt(far))}) {
			t.Fatal("a writer could not create its day", created, err)
		}
		id := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO finished_task
                (task_id,function_name,args_json,run_at,run_priority,run_max_time_seconds,run_start_time,run_end_time,result_json)
                VALUES ($1,'synthetic','{}',$2,0,30,$2,$2,'{}')`, id, far))
		}, server.TxReadCommitted, server.OptNoRetry())
		if finishedTaskRowCount(t, ctx, id) != 1 {
			t.Fatal("the writer's day did not take its row")
		}
	})
}

// With the runway stalled long enough that today and tomorrow have no day,
// a cleanup run creates both, and a completion then lands in today's.
func TestFinishedTaskCleanupCreatesTodayWhenTheRunwayStalls(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		now := server.NowUtc()
		days := []string{}
		for _, day := range []time.Time{now, now.AddDate(0, 0, 1)} {
			days = append(days, finishedTaskPartitionFamily.LeafName("finished_task", finishedTaskPartitionFamily.RangeAt(day)))
		}
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			for _, day := range days {
				server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE finished_task DETACH PARTITION %s`, day)))
				server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, day)))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		cleanupOwner := session.NewLocalClientSession(context.WithValue(ctx, taskCleanupClockKey{}, func() time.Time { return now }), "", nil)
		defer cleanupOwner.Cancel()
		if result, err := TaskCleanup(&TaskCleanupArgs{}, cleanupOwner); err != nil || result.Removed != 0 || result.Next != nil {
			t.Fatal("the cleanup did not run", result, err)
		}
		for _, day := range days {
			if !finishedTaskPartitionLeafExists(t, ctx, day) {
				t.Fatal("the cleanup did not create", day)
			}
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		if finished, _, _, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || finished[0] != id || finishedTaskRowCount(t, ctx, id) != 1 {
			t.Fatal("a completion did not land in the recreated day", err)
		}
	})
}

// The nodes of an EXPLAIN (FORMAT JSON) plan, depth first.
func finishedTaskPlanNodes(plan map[string]any) (nodes []map[string]any) {
	nodes = append(nodes, plan)
	if children, ok := plan["Plans"].([]any); ok {
		for _, child := range children {
			if node, ok := child.(map[string]any); ok {
				nodes = append(nodes, finishedTaskPlanNodes(node)...)
			}
		}
	}
	return
}

// Lookups by task id carry no run end time, so they cannot prune; each must
// stay one index probe per day, never a scan of a day. A reader that bounds
// the run end time prunes to the days it needs. Every day and the legacy
// partition hold rows and statistics; GetFinishedTasks' one-row id table is
// analyzed as its real size.
func TestFinishedTaskPartitionReaderPlansStayCheap(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		family := newFinishedTaskPartitionFamily(testFinishedTaskPartitionSettings())
		var leaves []server.PartitionLeaf
		var listErr error
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			leaves, listErr = family.ListLeaves(ctx, conn, "finished_task")
		})
		if listErr != nil || len(leaves) < 10 {
			t.Fatal("the test database has too few days", len(leaves), listErr)
		}
		populated := map[string]bool{}
		probe := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, leaf := range leaves {
				upper := leaf.Range.Upper
				// a row per 40 seconds of the day before the upper bound
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO finished_task
                    (task_id,function_name,args_json,run_at,run_priority,run_max_time_seconds,run_start_time,run_end_time,result_json)
                    SELECT gen_random_uuid(),'synthetic.plan.task','{}',end_time,0,30,end_time,end_time,'{}'
                    FROM (SELECT $1::timestamp - make_interval(secs => 40 * series) AS end_time FROM generate_series(1, 2000) series) observed`, upper))
				populated[leaf.Name] = true
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO finished_task
                (task_id,function_name,args_json,run_at,run_once_key,run_priority,run_max_time_seconds,run_start_time,run_end_time,result_json)
                VALUES ($1,'synthetic.plan.task','{}',$2,'synthetic-plan-key',0,30,$2,$2,'{}')`, probe, server.NowUtc()))
		}, server.TxReadCommitted, server.OptNoRetry())
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE finished_task`))
		}, server.OptReadWrite())

		explain := func(tx server.PgTx, sql string, args ...any) []map[string]any {
			var raw string
			server.Raise(tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&raw))
			var plans []map[string]any
			server.Raise(json.Unmarshal([]byte(raw), &plans))
			return finishedTaskPlanNodes(plans[0]["Plan"].(map[string]any))
		}
		requireProbes := func(name string, nodes []map[string]any) {
			probed := 0
			for _, node := range nodes {
				relation, _ := node["Relation Name"].(string)
				if !populated[relation] {
					continue
				}
				probed += 1
				if nodeType := node["Node Type"]; nodeType == "Seq Scan" || nodeType == "Sample Scan" {
					t.Errorf("%s scans %s: %v", name, relation, nodeType)
				}
			}
			if probed == 0 || len(leaves) < probed {
				t.Errorf("%s probed %d of %d days", name, probed, len(leaves))
			}
		}
		statements := []struct {
			name string
			sql  string
			args []any
		}{
			{"post owner", `SELECT function_name,args_json,result_json,run_once_key FROM finished_task WHERE task_id=$1 FOR UPDATE`, []any{probe}},
			{"post acknowledgement", `UPDATE finished_task SET post_completed = true WHERE task_id = $1`, []any{probe}},
			{"post failure", `UPDATE finished_task SET post_error = $2, post_completed = false WHERE task_id = $1`, []any{probe, "synthetic"}},
			{"checkpoint read", `SELECT args_json FROM finished_task WHERE task_id=$1 AND run_once_key=$2`, []any{probe, "synthetic-plan-key"}},
			{"checkpoint write", `UPDATE finished_task SET args_json=$2 WHERE task_id=$1`, []any{probe, "{}"}},
			{"tally receipt", `SELECT args_json,result_json FROM finished_task WHERE task_id=$1 FOR UPDATE`, []any{probe}},
			{"post retry claim", `SELECT EXISTS (SELECT 1 FROM finished_task WHERE finished_task.task_id = $1
				AND regexp_replace(finished_task.function_name, '/v[0-9]+', '', 'g') = ANY($2))`, []any{probe, []string{"synthetic.plan.task"}}},
			{"run-once successor", `INSERT INTO pending_task (
                task_id,function_name,args_json,client_address,client_address_hash,
                client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
                run_max_time_seconds,claim_time,release_time)
                SELECT $2,function_name,args_json,client_address,client_address_hash,
                    client_address_port,client_by_jwt_json,$3,run_once_key,run_priority,
                    run_max_time_seconds,$4,$4
                FROM finished_task WHERE task_id=$1 AND run_once_key=$5
                ON CONFLICT (run_once_key) DO NOTHING`, []any{probe, server.NewId(), server.NowUtc(), time.Time{}, "synthetic-plan-key"}},
			{"completion move", taskCompletionOwnerSql, []any{probe, server.NowUtc(), server.NowUtc(), "{}", int64(1)}},
			{"batch move", taskCompletionBatchSql, []any{[]server.Id{probe}, []time.Time{server.NowUtc()}, []time.Time{server.NowUtc()}, []string{"{}"},
				[]int64{0}, []server.Id{server.NewId()}, time.Time{}, []int64{1}}},
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, statement := range statements {
				requireProbes(statement.name, explain(tx, statement.sql, statement.args...))
			}
			server.CreateTempTableInTx(ctx, tx, "temp_task_ids(task_id uuid)", probe)
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE temp_task_ids`))
			requireProbes("GetFinishedTasks", explain(tx, `SELECT finished_task.task_id, finished_task.result_json
				FROM finished_task
				INNER JOIN temp_task_ids ON temp_task_ids.task_id = finished_task.task_id`))

			// the last hour's completions read no day that ended before the
			// window: today's, the days ahead, and yesterday's in the first
			// hour of a day
			windowStart := server.NowUtc().Add(-time.Hour)
			ranges := map[string]server.PartitionRange{}
			for _, leaf := range leaves {
				ranges[leaf.Name] = leaf.Range
			}
			scanned := 0
			for _, node := range explain(tx, `SELECT count(*) FROM finished_task WHERE run_end_time > now() - interval '1 hour'`) {
				relation, _ := node["Relation Name"].(string)
				if !populated[relation] {
					continue
				}
				scanned += 1
				if r := ranges[relation]; !r.Upper.IsZero() && r.Upper.Before(windowStart.Add(-time.Minute)) {
					t.Errorf("a one-hour window read %s, which ended at %s", relation, r.Upper)
				}
			}
			if scanned < 1 || len(leaves) <= scanned {
				t.Errorf("a one-hour window read %d of %d days", scanned, len(leaves))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
	})
}
