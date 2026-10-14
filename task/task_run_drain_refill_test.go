// A Run loop's refill claim keeps only its deadline, so one in flight when
// Drain begins completes. The draining loop starts nothing it claimed: each
// row goes back unstarted, claimable at once by another worker. A refill that
// returned before Drain began still launches what it claimed.
package task

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Run-once tasks of the protocol target: the first blocks until release, and
// every call and finalizing backend is recorded per scope.
type taskRunDrainRefillFixture struct {
	first, second server.Id
	entered       chan struct{}
	release       chan struct{}
	releaseOnce   sync.Once
	firstCalls    atomic.Int32
	secondCalls   atomic.Int32
	// Runs in each call of another scope, when set before Run.
	secondRun     func(context.Context) error
	stateLock     sync.Mutex
	finalizedPids map[server.Id][]uint32
}

func newTaskRunDrainRefillFixture() *taskRunDrainRefillFixture {
	return &taskRunDrainRefillFixture{first: server.NewId(), second: server.NewId(), entered: make(chan struct{}), release: make(chan struct{}),
		finalizedPids: map[server.Id][]uint32{}}
}

func (self *taskRunDrainRefillFixture) unblock() { self.releaseOnce.Do(func() { close(self.release) }) }

func (self *taskRunDrainRefillFixture) scope(queued *Task) server.Id {
	var args runOnceGenerationArgs
	if err := json.Unmarshal([]byte(queued.ArgsJson), &args); err != nil {
		panic(err)
	}
	return args.Scope
}

func (self *taskRunDrainRefillFixture) target() Target {
	return &taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{
		Target: NewTaskTarget(runOnceGenerationWork),
		before: func(taskCtx context.Context, queued *Task) error {
			if self.scope(queued) != self.first {
				self.secondCalls.Add(1)
				if self.secondRun != nil {
					return self.secondRun(taskCtx)
				}
				return nil
			}
			if self.firstCalls.Add(1) == 1 {
				close(self.entered)
			}
			select {
			case <-self.release:
				return nil
			case <-taskCtx.Done():
				return taskCtx.Err()
			}
		},
		after: func(tx server.PgTx, queued *Task) error {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			scope := self.scope(queued)
			self.finalizedPids[scope] = append(self.finalizedPids[scope], tx.Conn().PgConn().PID())
			return nil
		},
	}}
}

func (self *taskRunDrainRefillFixture) pids(scope server.Id) []uint32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]uint32(nil), self.finalizedPids[scope]...)
}

// A worker whose first task runs in one of its slots, so a free slot is
// refilled at once, and whose second claim, that refill, waits for resume.
func newTaskRunDrainRefillWorker(ctx context.Context, fixture *taskRunDrainRefillFixture, batchSize int, refilling chan struct{}, resume chan struct{}, targets ...Target) *TaskWorker {
	settings := DefaultTaskWorkerSettings()
	settings.BatchSize = batchSize
	settings.ClaimRegisteredTargetsOnly = true
	settings.FairClaimFunctions = true
	settings.RetryTimeoutAfterError = 100 * time.Millisecond
	settings.DrainFinishTimeout = 20 * time.Second
	settings.DrainCancelTimeout = 5 * time.Second
	worker := NewTaskWorker(ctx, settings)
	worker.AddTargets(append([]Target{fixture.target()}, targets...)...)
	var claims atomic.Int32
	worker.claimBeforeQuery = func(server.PgTx) error {
		if claims.Add(1) == 2 {
			close(refilling)
			taskQueueWait(ctx, resume)
		}
		return nil
	}
	return worker
}

// Drain cancels runCtx while a Run loop's refill claim is in flight, here
// paused before its query, with a second task due. The refill keeps only its
// deadline, so it completes after Drain began, and the draining worker started
// the task it claimed. A draining worker must start no new task.
func TestTaskRunDrainDuringRefillStartsNoNewTask(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		fixture := newTaskRunDrainRefillFixture()
		ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: fixture.first}, client, runOnceGenerationKey(fixture.first), RunAt(server.NowUtc().Add(-time.Hour)))
		refilling, resume := make(chan struct{}), make(chan struct{})
		var resumeOnce sync.Once
		worker := newTaskRunDrainRefillWorker(ctx, fixture, 2, refilling, resume)
		defer worker.Close()
		done := make(chan any, 1)
		go func() { done <- server.HandleError(worker.Run) }()
		defer func() {
			resumeOnce.Do(func() { close(resume) })
			fixture.unblock()
		}()
		taskQueueWait(ctx, fixture.entered)
		taskQueueWait(ctx, refilling)
		secondId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: fixture.second}, client, runOnceGenerationKey(fixture.second), RunAt(server.NowUtc().Add(-time.Hour)))

		drained := make(chan struct{})
		drainStarted := time.Now()
		var drainTook time.Duration
		go func() { defer close(drained); worker.Drain(); drainTook = time.Since(drainStarted) }()
		taskQueueWait(ctx, worker.runCtx.Done())
		resumeOnce.Do(func() { close(resume) })
		// Let the refill finish, handing its task back or finishing it, then
		// let the live task return.
		taskSessionAwait(ctx, `SELECT NOT EXISTS (SELECT 1 FROM pending_task WHERE task_id=$1 AND reschedule_error IS NULL)`, secondId)
		fixture.unblock()
		taskQueueWait(ctx, drained)
		if recovered := <-done; recovered != nil {
			t.Fatal("Run failed", recovered)
		}
		t.Logf("drain took %s; second task ran %d times; finished %d, pending %d", drainTook, fixture.secondCalls.Load(), len(GetFinishedTasks(ctx, secondId)), len(GetTasks(ctx, secondId)))
		if fixture.firstCalls.Load() != 1 || len(fixture.pids(fixture.first)) != 1 {
			t.Fatal("live task did not finish once", fixture.firstCalls.Load(), fixture.pids(fixture.first))
		}
		if calls := fixture.secondCalls.Load(); calls != 0 {
			t.Fatalf("the draining worker started a task its in-flight refill claimed after Drain began: %d calls, finished %d", calls, len(GetFinishedTasks(ctx, secondId)))
		}
	})
}

// A refill that finishes after Drain began claims a task with a saturated
// error count and a cohort of three. The draining loop runs none of them. The
// task goes back on its own owner and the cohort on one complete-key owner,
// each as interrupted: its lease ends and its backoff does not grow. Every
// lock is released at once: another worker whose clock leads only by the
// interrupted retry delay claims and runs each row once while the live task
// still holds Drain open.
func TestTaskRunDrainDuringRefillHandsBackClaimedTasks(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		pinRescheduleTimeouts(t)
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		fixture := newTaskRunDrainRefillFixture()
		firstId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: fixture.first}, client, runOnceGenerationKey(fixture.first), RunAt(server.NowUtc().Add(-time.Hour)))
		const members = 3
		var cohortHandbacks atomic.Int32
		workerCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted && len(event.Keys) == 2*members {
				cohortHandbacks.Add(1)
			}
		})
		var cohortCalls atomic.Int32
		cohort := &taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall), before: func(context.Context, *Task) error {
			cohortCalls.Add(1)
			return nil
		}}
		refilling, resume := make(chan struct{}), make(chan struct{})
		var resumeOnce sync.Once
		worker := newTaskRunDrainRefillWorker(workerCtx, fixture, 3, refilling, resume, cohort)
		defer worker.Close()
		done := make(chan any, 1)
		go func() { done <- server.HandleError(worker.Run) }()
		defer func() {
			resumeOnce.Do(func() { close(resume) })
			fixture.unblock()
		}()
		taskQueueWait(ctx, fixture.entered)
		taskQueueWait(ctx, refilling)
		secondId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: fixture.second}, client, runOnceGenerationKey(fixture.second), RunAt(server.NowUtc().Add(-time.Hour)))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error_count=19 WHERE task_id=$1`, secondId))
		})
		ids := append(taskRunCohortTestSchedule(client, server.NewId(), members), secondId)

		drained := make(chan struct{})
		go func() { defer close(drained); worker.Drain() }()
		taskQueueWait(ctx, worker.runCtx.Done())
		resumeOnce.Do(func() { close(resume) })
		taskSessionAwait(ctx, `SELECT NOT EXISTS (SELECT 1 FROM pending_task WHERE task_id=ANY($1) AND reschedule_error IS NULL)`, ids)
		if fixture.secondCalls.Load() != 0 || cohortCalls.Load() != 0 || len(GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatal("the draining worker started a task its refill claimed after Drain began", fixture.secondCalls.Load(), cohortCalls.Load())
		}
		pending := GetTasks(ctx, ids...)
		for _, id := range ids {
			queued := pending[id]
			errorCount := 0
			if id == secondId {
				errorCount = 19
			}
			if queued == nil || queued.ClaimGeneration != 1 || queued.RescheduleError != context.Canceled.Error() || queued.RescheduleErrorCount != errorCount {
				t.Fatal("refill row was not handed back once as interrupted", id, queued)
			}
			if delay := queued.RunAt.Sub(queued.ReleaseTime); delay < RescheduleTimeout-time.Microsecond || 2*RescheduleTimeout < delay || server.NowUtc().Before(queued.ReleaseTime) {
				t.Fatal("handback kept its lease or took a failure delay", id, delay)
			}
		}
		if cohortHandbacks.Load() != 1 {
			t.Fatal("cohort was not handed back on one complete-key owner", cohortHandbacks.Load())
		}

		var ranLock sync.Mutex
		ran := map[server.Id]int{}
		claimantRun := func(_ context.Context, queued *Task) error {
			ranLock.Lock()
			defer ranLock.Unlock()
			ran[queued.TaskId]++
			return nil
		}
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		claimant := NewTaskWorker(ctx, settings)
		defer claimant.Close()
		claimant.AddTargets(
			&taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork), before: claimantRun}},
			&taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall), before: claimantRun},
		)
		// Ahead by the longest interrupted retry delay and the rounding of its
		// eligibility block, far inside the claim's lease.
		claimant.claimNow = func() time.Time { return server.NowUtc().Add(2*RescheduleTimeout + 3*BlockSizeSeconds*time.Second) }
		finished := map[server.Id]bool{}
		for len(finished) < len(ids) {
			finishedIds, rescheduledIds, postIds, err := claimant.EvalTasks(len(ids))
			if err != nil || len(rescheduledIds)+len(postIds) != 0 {
				t.Fatal("another worker could not finish a handed-back row", err, rescheduledIds, postIds)
			}
			for _, id := range finishedIds {
				if slices.Contains(ids, id) {
					finished[id] = true
				}
			}
			if len(finished) < len(ids) {
				select {
				case <-ctx.Done():
					t.Fatal("handed-back rows did not become claimable", len(finished))
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if worker.drainCtx.Err() != nil || fixture.firstCalls.Load() != 1 || len(GetTasks(ctx, firstId)) != 1 {
			t.Fatal("handed-back rows were claimable only after Drain canceled the live task")
		}
		ranLock.Lock()
		runs := maps.Clone(ran)
		ranLock.Unlock()
		for _, id := range ids {
			if runs[id] != 1 {
				t.Fatal("another worker did not run a handed-back row once", id, runs[id])
			}
		}

		fixture.unblock()
		taskQueueWait(ctx, drained)
		if recovered := <-done; recovered != nil {
			t.Fatal("Run failed", recovered)
		}
		if fixture.firstCalls.Load() != 1 || len(fixture.pids(fixture.first)) != 1 || len(GetFinishedTasks(ctx, firstId)) != 1 ||
			fixture.secondCalls.Load() != 0 || cohortCalls.Load() != 0 {
			t.Fatal("live task did not finish once beside its handed-back refill", fixture.firstCalls.Load(), fixture.secondCalls.Load(), cohortCalls.Load())
		}
	})
}

// A refill that returned before Drain began launched its task, which is live
// when Drain begins. Drain lets it finish once, beside the first task.
func TestTaskRunRefillBeforeDrainLaunchesClaimedTasks(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		fixture := newTaskRunDrainRefillFixture()
		firstId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: fixture.first}, client, runOnceGenerationKey(fixture.first), RunAt(server.NowUtc().Add(-time.Hour)))
		refilling, resume := make(chan struct{}), make(chan struct{})
		var resumeOnce sync.Once
		worker := newTaskRunDrainRefillWorker(ctx, fixture, 2, refilling, resume)
		defer worker.Close()
		secondEntered := make(chan struct{})
		fixture.secondRun = func(context.Context) error {
			close(secondEntered)
			taskQueueWait(ctx, worker.runCtx.Done())
			return nil
		}
		done := make(chan any, 1)
		go func() { done <- server.HandleError(worker.Run) }()
		defer func() {
			resumeOnce.Do(func() { close(resume) })
			fixture.unblock()
		}()
		taskQueueWait(ctx, fixture.entered)
		taskQueueWait(ctx, refilling)
		secondId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: fixture.second}, client, runOnceGenerationKey(fixture.second), RunAt(server.NowUtc().Add(-time.Hour)))
		resumeOnce.Do(func() { close(resume) })
		taskQueueWait(ctx, secondEntered)

		drained := make(chan struct{})
		go func() { defer close(drained); worker.Drain() }()
		taskQueueWait(ctx, worker.runCtx.Done())
		fixture.unblock()
		taskQueueWait(ctx, drained)
		if recovered := <-done; recovered != nil {
			t.Fatal("Run failed", recovered)
		}
		finished := GetFinishedTasks(ctx, firstId, secondId)
		if fixture.firstCalls.Load() != 1 || fixture.secondCalls.Load() != 1 || len(GetTasks(ctx, firstId, secondId)) != 0 || len(finished) != 2 ||
			finished[secondId].RescheduleError != "" || len(fixture.pids(fixture.second)) != 1 {
			t.Fatal("a refill that returned before Drain did not run its task once", fixture.firstCalls.Load(), fixture.secondCalls.Load(), len(finished))
		}
	})
}
