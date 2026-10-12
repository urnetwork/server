// Cleanup cadence regressions run the real worker, cleanup target, Post and
// retry policy on a synthetic clock. A committed retention group costs
// synthetic time in proportion to the rows it held, and the task's max-time
// timer fires on the same clock, so a run's length, its budget stop or
// timeout, and the wait before the next run are all deterministic.
//
// The cost model is Main's on 2026-10-12, where cleanup ran 64-row groups:
// one measured run removed 327,744 rows in its 120 s (0.37 ms a row), and the
// preceding four days averaged about 0.86 ms a row; the slower figure is used.
// Rows crossed the retention boundary at up to 1.7M an hour, and the pending
// cleanup carried 103 consecutive timeouts. One seeded row stands for 100
// production rows, which keeps each test to a few thousand rows while every
// ratio of work to time is unchanged.
package task

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const taskCleanupTestRowWeight = 100

// Synthetic time one seeded row costs a retention group.
const taskCleanupTestRowCost = taskCleanupTestRowWeight * 860 * time.Microsecond

// Seeded rows crossing the retention boundary per synthetic hour.
const taskCleanupTestArrivalRate = 1700000 / taskCleanupTestRowWeight

// Main's pending cleanup when it was measured.
const taskCleanupTestErrorCount = 103

func taskCleanupTestEnv(t *testing.T, run func(testing.TB, context.Context)) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		run(t, ctx)
	})
}

// Hooks are called from the worker's body and timer goroutines while the test
// goroutine waits in EvalTasks; stateLock guards everything they share.
type taskCleanupSimulation struct {
	ctx      context.Context
	owner    *session.ClientSession
	worker   *TaskWorker
	runOnce  *RunOnceOption
	baseTime time.Time

	stateLock       sync.Mutex
	now             time.Time
	rowCost         time.Duration
	finishedKeys    map[server.PgOwnershipKey]bool
	runStartTime    time.Time
	timerRegistered chan struct{}
	timer           chan time.Time
	timerDeadline   time.Time
	timerBodyCtx    context.Context
}

type taskCleanupTestPending struct {
	taskId     server.Id
	runAt      time.Time
	argsJson   string
	errorCount int
	errorText  string
}

// A successor's resume position, read without the cleanup's own types.
type taskCleanupTestArgs struct {
	After *struct {
		RunEndTime time.Time `json:"run_end_time"`
		TaskId     server.Id `json:"task_id"`
	} `json:"after"`
}

type taskCleanupTestRun struct {
	finishedTaskIds    []server.Id
	rescheduledTaskIds []server.Id
	err                error
	runLength          time.Duration
	// Until the pending successor or retry is due.
	wait    time.Duration
	pending *taskCleanupTestPending
	args    taskCleanupTestArgs
}

func newTaskCleanupSimulation(t testing.TB, ctx context.Context, rowCost time.Duration, errorCount int) *taskCleanupSimulation {
	t.Helper()
	baseTime := server.NowUtc().Truncate(time.Second)
	simulation := &taskCleanupSimulation{
		runOnce:      RunOnce("task_cleanup"),
		baseTime:     baseTime,
		now:          baseTime,
		rowCost:      rowCost,
		finishedKeys: map[server.PgOwnershipKey]bool{},
	}
	hookCtx := context.WithValue(ctx, taskCleanupClockKey{}, func() time.Time { return simulation.clock() })
	hookCtx = Testing_WithTaskRunAfter(hookCtx, func(bodyCtx context.Context, maxTime time.Duration) <-chan time.Time {
		return simulation.after(bodyCtx, maxTime)
	})
	hookCtx = server.Testing_WithPgOwnershipObservation(hookCtx, func(event server.PgOwnershipEvent) {
		simulation.observe(ctx, event)
	})
	simulation.ctx = hookCtx
	simulation.owner = session.NewLocalClientSession(ctx, "", nil)
	t.Cleanup(simulation.owner.Cancel)
	simulation.worker = NewTaskWorkerWithDefaults(hookCtx)
	t.Cleanup(simulation.worker.Close)
	simulation.worker.AddTargets(NewTaskCleanupTaskTarget())

	ScheduleTask(TaskCleanup, &TaskCleanupArgs{}, simulation.owner, simulation.runOnce, RunAt(baseTime))
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error_count=$2 WHERE run_once_key=$1`,
			simulation.runOnce.String(), errorCount))
	}, server.OptNoRetry())
	return simulation
}

func (self *taskCleanupSimulation) clock() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *taskCleanupSimulation) advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(duration)
}

// The adapter's max-time timer, armed relative to the run's synthetic start.
func (self *taskCleanupSimulation) after(bodyCtx context.Context, maxTime time.Duration) <-chan time.Time {
	timer := make(chan time.Time)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.timerRegistered == nil || self.timer != nil {
		return timer
	}
	self.timer = timer
	self.timerDeadline = self.runStartTime.Add(maxTime)
	self.timerBodyCtx = bodyCtx
	close(self.timerRegistered)
	return timer
}

// A released group of seeded rows has committed: charge its synthetic cost,
// then fire the max-time timer if the run has used up its max time, and wait
// until the body has observed the cancellation before it continues.
func (self *taskCleanupSimulation) observe(ctx context.Context, event server.PgOwnershipEvent) {
	if event.Kind != server.PgOwnershipReleased || !event.TransactionScoped || len(event.Keys) == 0 {
		return
	}
	var timerRegistered chan struct{}
	charged := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, key := range event.Keys {
			if !self.finishedKeys[key] {
				return false
			}
		}
		self.now = self.now.Add(time.Duration(len(event.Keys)) * self.rowCost)
		timerRegistered = self.timerRegistered
		return true
	}()
	if !charged || timerRegistered == nil {
		return
	}
	select {
	case <-timerRegistered:
	case <-ctx.Done():
		return
	}
	var timer chan time.Time
	var bodyCtx context.Context
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.timer != nil && !self.now.Before(self.timerDeadline) {
			timer, bodyCtx = self.timer, self.timerBodyCtx
			self.timer = nil
		}
	}()
	if timer == nil {
		return
	}
	close(timer)
	select {
	case <-bodyCtx.Done():
	case <-ctx.Done():
	}
}

// Seeds finished rows with run end times spread evenly over [from, to).
func (self *taskCleanupSimulation) seed(
	t testing.TB,
	count int,
	from time.Time,
	to time.Time,
	postError bool,
	postCompleted bool,
) []server.Id {
	t.Helper()
	taskIds := make([]server.Id, count)
	runEndTimes := make([]time.Time, count)
	for index := range count {
		taskIds[index] = server.NewId()
		runEndTimes[index] = from.Add(time.Duration(int64(to.Sub(from)) * int64(index) / int64(count))).Truncate(time.Microsecond)
	}
	var postErrorText *string
	if postError {
		text := "synthetic post failure"
		postErrorText = &text
	}
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO finished_task
			(task_id,function_name,args_json,client_address,run_at,run_priority,run_max_time_seconds,
			 run_start_time,run_end_time,result_json,post_error,post_completed)
			SELECT seeded.task_id,'synthetic.cleanup.history','{}','',seeded.run_end_time,0,120,
				seeded.run_end_time,seeded.run_end_time,'{}',$3,$4
			FROM unnest($1::uuid[],$2::timestamp[]) AS seeded(task_id,run_end_time)`,
			taskIds, runEndTimes, postErrorText, postCompleted))
	}, server.OptNoRetry())
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, taskId := range taskIds {
			self.finishedKeys[taskFinishedOwnershipKey(taskId)] = true
		}
	}()
	return taskIds
}

func (self *taskCleanupSimulation) readPending(t testing.TB) *taskCleanupTestPending {
	t.Helper()
	var pendings []*taskCleanupTestPending
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(self.ctx, `SELECT task_id,run_at,args_json,reschedule_error_count,COALESCE(reschedule_error,'')
			FROM pending_task WHERE run_once_key=$1`, self.runOnce.String())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				pending := &taskCleanupTestPending{}
				server.Raise(rows.Scan(&pending.taskId, &pending.runAt, &pending.argsJson, &pending.errorCount, &pending.errorText))
				pendings = append(pendings, pending)
			}
		})
	})
	if len(pendings) != 1 {
		t.Fatalf("cleanup must have exactly one pending run, found %d", len(pendings))
	}
	return pendings[0]
}

// Makes the pending run due, runs it, and reads what it left pending.
func (self *taskCleanupSimulation) run(t testing.TB) *taskCleanupTestRun {
	t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE run_once_key=$1`,
			self.runOnce.String(), time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)))
	}, server.OptNoRetry())
	var runStartTime time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		runStartTime = self.now
		self.runStartTime = self.now
		self.timerRegistered = make(chan struct{})
		self.timer = nil
	}()
	finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := self.worker.EvalTasks(1)
	handbackTime := server.NowUtc()
	if len(postRescheduledTaskIds) != 0 {
		t.Fatal("cleanup post failed", postRescheduledTaskIds)
	}
	if len(finishedTaskIds)+len(rescheduledTaskIds) != 1 {
		t.Fatal("cleanup did not run exactly once", finishedTaskIds, rescheduledTaskIds, err)
	}
	pending := self.readPending(t)
	run := &taskCleanupTestRun{
		finishedTaskIds:    finishedTaskIds,
		rescheduledTaskIds: rescheduledTaskIds,
		err:                err,
		runLength:          self.clock().Sub(runStartTime),
		wait:               max(0, pending.runAt.Sub(handbackTime).Round(time.Second)),
		pending:            pending,
	}
	server.Raise(json.Unmarshal([]byte(pending.argsJson), &run.args))
	return run
}

// The seeded rows still present.
func (self *taskCleanupSimulation) remaining(t testing.TB, taskIds []server.Id) map[server.Id]bool {
	t.Helper()
	remainingTaskIds := map[server.Id]bool{}
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(self.ctx, `SELECT task_id FROM finished_task WHERE task_id=ANY($1::uuid[])`, taskIds)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var taskId server.Id
				server.Raise(rows.Scan(&taskId))
				remainingTaskIds[taskId] = true
			}
		})
	})
	return remainingTaskIds
}

// Main's stall: a backlog larger than one max time of groups, behind a pending
// cleanup that had already timed out 103 times. A run must stop starting groups
// at its budget, well inside its max time, finish, and re-arm a successor that
// is due at once and resumes after the last row it removed. Before the fix the
// run timed out at its max time, which dropped the re-arm, and the generic
// backoff put the next run 30 to 90 minutes away.
func TestTaskCleanupStopsAtBudgetAndRearmsAtOnce(t *testing.T) {
	taskCleanupTestEnv(t, func(t testing.TB, ctx context.Context) {
		simulation := newTaskCleanupSimulation(t, ctx, taskCleanupTestRowCost, taskCleanupTestErrorCount)
		backlogTaskIds := simulation.seed(t, 2000, simulation.baseTime.Add(-27*time.Hour), simulation.baseTime.Add(-25*time.Hour), false, true)
		groupCost := finishedTaskRetentionGroupSize * taskCleanupTestRowCost
		groupCount := int((taskCleanupBudget + groupCost - 1) / groupCost)
		removedCount := groupCount * finishedTaskRetentionGroupSize
		if taskCleanupMaxTime <= time.Duration(groupCount)*groupCost || len(backlogTaskIds) < 2*removedCount {
			t.Fatal("budget fixture does not separate the budget from the max time")
		}

		first := simulation.run(t)
		if first.err != nil || len(first.finishedTaskIds) != 1 || len(first.rescheduledTaskIds) != 0 {
			t.Fatalf("cleanup did not finish inside its max time: finished=%v rescheduled=%v error=%q",
				first.finishedTaskIds, first.rescheduledTaskIds, first.pending.errorText)
		}
		if first.runLength < taskCleanupBudget || taskCleanupBudget+groupCost <= first.runLength {
			t.Fatalf("cleanup did not stop at its budget: ran %s, budget %s, group %s", first.runLength, taskCleanupBudget, groupCost)
		}
		if first.wait != 0 || first.pending.errorCount != 0 {
			t.Fatalf("budget stop did not re-arm at once: wait=%s errors=%d", first.wait, first.pending.errorCount)
		}
		remaining := simulation.remaining(t, backlogTaskIds)
		for index, taskId := range backlogTaskIds {
			if remaining[taskId] == (index < removedCount) {
				t.Fatalf("first run removed the wrong rows: row %d present=%t, want the oldest %d removed", index, remaining[taskId], removedCount)
			}
		}
		last := backlogTaskIds[removedCount-1]
		if first.args.After == nil || first.args.After.TaskId != last {
			t.Fatal("successor does not resume after the last removed row", first.pending.argsJson)
		}

		second := simulation.run(t)
		if second.err != nil || len(second.finishedTaskIds) != 1 || second.wait != 0 {
			t.Fatalf("successor did not finish and re-arm at once: finished=%v wait=%s", second.finishedTaskIds, second.wait)
		}
		remaining = simulation.remaining(t, backlogTaskIds)
		for index, taskId := range backlogTaskIds {
			if remaining[taskId] == (index < 2*removedCount) {
				t.Fatalf("successor did not continue from its position: row %d present=%t", index, remaining[taskId])
			}
		}
	})
}

// Rows keep crossing the retention boundary at Main's peak rate while a
// backlog waits. The cleanup must remove rows more than twice as fast as they
// arrive until the backlog is gone, then keep up. Before the fix each run timed
// out after its max time of groups and the next run came 30 to 90 minutes
// later, so the backlog grew.
func TestTaskCleanupRemovesFasterThanRowsArrive(t *testing.T) {
	taskCleanupTestEnv(t, func(t testing.TB, ctx context.Context) {
		simulation := newTaskCleanupSimulation(t, ctx, taskCleanupTestRowCost, taskCleanupTestErrorCount)
		window := 30 * time.Minute
		// Every row is past retention on both the synthetic and the real clock,
		// and rows arrive in run end order, as they cross the boundary.
		arrivalBase := simulation.baseTime.Add(-24*time.Hour - window)
		backlogTaskIds := simulation.seed(t, 2000, arrivalBase.Add(-2*time.Hour), arrivalBase, false, true)
		var taskIds []server.Id
		taskIds = append(taskIds, backlogTaskIds...)
		arrivedCount := 0
		arrive := func(offset time.Duration) {
			targetCount := int(int64(taskCleanupTestArrivalRate) * int64(offset) / int64(time.Hour))
			if targetCount <= arrivedCount {
				return
			}
			from := arrivalBase.Add(time.Duration(int64(arrivedCount) * int64(time.Hour) / taskCleanupTestArrivalRate))
			to := arrivalBase.Add(time.Duration(int64(targetCount) * int64(time.Hour) / taskCleanupTestArrivalRate))
			taskIds = append(taskIds, simulation.seed(t, targetCount-arrivedCount, from, to, false, true)...)
			arrivedCount = targetCount
		}

		var drainedOffset time.Duration
		drainedRemovedCount := 0
		for {
			run := simulation.run(t)
			offset := simulation.clock().Sub(simulation.baseTime)
			arrive(offset)
			if drainedOffset == 0 && run.err == nil && len(run.finishedTaskIds) == 1 && run.args.After == nil {
				drainedOffset = offset
				drainedRemovedCount = len(taskIds) - len(simulation.remaining(t, taskIds))
			}
			if window <= offset+run.wait {
				break
			}
			simulation.advance(run.wait)
			arrive(simulation.clock().Sub(simulation.baseTime))
		}
		arrive(window)

		if drainedOffset == 0 {
			t.Fatalf("cleanup never drained its backlog in %s: %d of %d rows remain, %d arrived",
				window, len(simulation.remaining(t, taskIds)), len(taskIds), arrivedCount)
		}
		removedPerHour := float64(drainedRemovedCount) / drainedOffset.Hours()
		if removedPerHour < 2*taskCleanupTestArrivalRate {
			t.Fatalf("cleanup removed %.0f rows an hour, under twice the arrival rate of %d", removedPerHour, taskCleanupTestArrivalRate)
		}
		// Only rows that arrived since the last sweep started may remain.
		groupCost := finishedTaskRetentionGroupSize * taskCleanupTestRowCost
		allowedCount := int(int64(taskCleanupTestArrivalRate) * int64(taskCleanupInterval+taskCleanupBudget+groupCost) / int64(time.Hour))
		if remainingCount := len(simulation.remaining(t, taskIds)); allowedCount < remainingCount {
			t.Fatalf("cleanup fell behind after draining: %d rows remain, at most %d may", remainingCount, allowedCount)
		}
	})
}

// A sweep spanning several budgeted runs removes exactly the rows past
// retention: successful history older than a day, and history with an
// unfinished Post only once it is older than a week. Rows the sweep passes but
// may not remove stay, as do recent and future rows. Before the fix the first
// run timed out and the rest of the expired backlog waited for a retry 30 to
// 90 minutes later.
func TestTaskCleanupRemovesOnlyRowsPastRetention(t *testing.T) {
	taskCleanupTestEnv(t, func(t testing.TB, ctx context.Context) {
		simulation := newTaskCleanupSimulation(t, ctx, taskCleanupTestRowCost, taskCleanupTestErrorCount)
		window := 15 * time.Minute
		base := simulation.baseTime
		var expiredTaskIds []server.Id
		var retainedTaskIds []server.Id
		expiredTaskIds = append(expiredTaskIds, simulation.seed(t, 2000, base.Add(-27*time.Hour), base.Add(-25*time.Hour), false, true)...)
		expiredTaskIds = append(expiredTaskIds, simulation.seed(t, 3, base.Add(-8*24*time.Hour), base.Add(-8*24*time.Hour+time.Minute), true, false)...)
		expiredTaskIds = append(expiredTaskIds, simulation.seed(t, 3, base.Add(-26*time.Hour), base.Add(-26*time.Hour+time.Minute), true, true)...)
		// Unfinished Posts inside their week, the first just short of it.
		retainedTaskIds = append(retainedTaskIds, simulation.seed(t, 3, base.Add(-7*24*time.Hour+window+time.Hour), base.Add(-7*24*time.Hour+window+2*time.Hour), true, false)...)
		retainedTaskIds = append(retainedTaskIds, simulation.seed(t, 3, base.Add(-3*24*time.Hour), base.Add(-3*24*time.Hour+time.Minute), true, false)...)
		// Successful history still inside its day for the whole window.
		retainedTaskIds = append(retainedTaskIds, simulation.seed(t, 3, base.Add(-24*time.Hour+window+time.Second), base.Add(-24*time.Hour+window+time.Minute), false, true)...)
		retainedTaskIds = append(retainedTaskIds, simulation.seed(t, 3, base.Add(-time.Hour), base.Add(-time.Hour+time.Minute), false, true)...)
		retainedTaskIds = append(retainedTaskIds, simulation.seed(t, 3, base.Add(24*time.Hour), base.Add(24*time.Hour+time.Minute), false, true)...)

		complete := false
		for !complete {
			run := simulation.run(t)
			complete = run.err == nil && len(run.finishedTaskIds) == 1 && run.args.After == nil
			if !complete && window <= simulation.clock().Sub(base)+run.wait {
				break
			}
			simulation.advance(run.wait)
		}
		remaining := simulation.remaining(t, append(append([]server.Id{}, expiredTaskIds...), retainedTaskIds...))
		expiredRemainingCount := 0
		for _, taskId := range expiredTaskIds {
			if remaining[taskId] {
				expiredRemainingCount++
			}
		}
		if !complete || expiredRemainingCount != 0 {
			t.Fatalf("cleanup left %d of %d rows past retention after %s (sweep complete=%t)",
				expiredRemainingCount, len(expiredTaskIds), simulation.clock().Sub(base), complete)
		}
		for index, taskId := range retainedTaskIds {
			if !remaining[taskId] {
				t.Fatalf("cleanup removed retained row %d", index)
			}
		}
	})
}

// A run can still overrun its max time, for example on one stalled group. The
// timeout must not stretch the cadence: the pending run keeps its position and
// retries within the error retry cap, not after the generic backoff, which
// reached its one hour cap on Main. The group committed before the timeout
// stays removed.
func TestTaskCleanupTimeoutRetriesWithinCap(t *testing.T) {
	taskCleanupTestEnv(t, func(t testing.TB, ctx context.Context) {
		// One group alone outlasts the max time.
		stalledRowCost := taskCleanupMaxTime/finishedTaskRetentionGroupSize + time.Second
		simulation := newTaskCleanupSimulation(t, ctx, stalledRowCost, taskCleanupTestErrorCount)
		backlogTaskIds := simulation.seed(t, 3*finishedTaskRetentionGroupSize, simulation.baseTime.Add(-27*time.Hour), simulation.baseTime.Add(-25*time.Hour), false, true)

		timedOut := simulation.run(t)
		if len(timedOut.rescheduledTaskIds) != 1 || !strings.Contains(timedOut.pending.errorText, "Timeout") {
			t.Fatalf("stalled cleanup did not time out: finished=%v error=%q", timedOut.finishedTaskIds, timedOut.pending.errorText)
		}
		if taskCleanupErrorRetryCap < timedOut.wait {
			t.Fatalf("timed-out cleanup retries after %s, beyond its %s cap", timedOut.wait, taskCleanupErrorRetryCap)
		}
		if timedOut.args.After != nil {
			t.Fatal("timed-out cleanup changed its resume position", timedOut.pending.argsJson)
		}
		remaining := simulation.remaining(t, backlogTaskIds)
		for index, taskId := range backlogTaskIds {
			if remaining[taskId] == (index < finishedTaskRetentionGroupSize) {
				t.Fatalf("timed-out cleanup lost or exceeded its committed group: row %d present=%t", index, remaining[taskId])
			}
		}

		func() {
			simulation.stateLock.Lock()
			defer simulation.stateLock.Unlock()
			simulation.rowCost = taskCleanupTestRowCost
		}()
		simulation.advance(timedOut.wait)
		retried := simulation.run(t)
		if retried.err != nil || len(retried.finishedTaskIds) != 1 || retried.pending.errorCount != 0 {
			t.Fatalf("retry after the timeout did not finish and re-arm: finished=%v error=%q", retried.finishedTaskIds, retried.pending.errorText)
		}
		if len(simulation.remaining(t, backlogTaskIds)) != 0 {
			t.Fatal("retry after the timeout did not remove the remaining expired rows")
		}
	})
}
