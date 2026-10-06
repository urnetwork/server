// A completed page must publish its continuation while an unrelated claimed
// task is still executing. The collector's next select is the causal barrier.
package task

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Wraps the real target without replacing its result/post lifecycle.
type taskBatchCompletionTarget struct {
	Target
	before func()
}

// Supplies a held sibling and observable external work after its successful
// finish, including when another task in the batch fails finalization.
type taskBatchCompletionSlowTarget struct {
	Target
	fixture *taskBatchCompletionFixture
	started chan struct{}
}

// Cancellation remains visible after the explicit unwind barrier releases.
func (self *taskBatchCompletionSlowTarget) Run(ctx context.Context, _ *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	defer close(self.fixture.slowDone)
	self.fixture.slowCtx = ctx
	close(self.started)
	<-self.fixture.releaseSlow
	return &claimProfileResult{}, func(server.PgTx) ([]server.PostFunction, error) {
		return []server.PostFunction{func() any { self.fixture.slowCommitCount.Add(1); return nil }}, nil
	}, ctx.Err()
}

// The fast target cannot finish until its unrelated sibling is executing.
func (self *taskBatchCompletionTarget) Run(ctx context.Context, task *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	self.before()
	return self.Target.Run(ctx, task)
}

// Records both committed outcomes and the original finalization panic.
type taskBatchCompletionOutcome struct {
	finishedTaskIds        []server.Id
	rescheduledTaskIds     []server.Id
	postRescheduledTaskIds []server.Id
	err                    error
	panicValue             any
}

// Owns every barrier so fixture cleanup joins execution before database teardown.
type taskBatchCompletionFixture struct {
	worker            *TaskWorker
	fastTaskId        server.Id
	slowTaskId        server.Id
	slowCtx           context.Context
	collected         chan struct{}
	continueCollector chan struct{}
	releaseSlow       chan struct{}
	slowDone          chan struct{}
	heartbeatTick     chan time.Time
	heartbeatTaskIds  chan []server.Id
	done              chan struct{}
	outcome           taskBatchCompletionOutcome
	slowCommitCount   atomic.Int32
	continueOnce      sync.Once
	releaseOnce       sync.Once
}

// The second select can only follow receipt of the fast task: the slow task
// remains behind its explicit release barrier, and no heartbeat fires itself.
func newTaskBatchCompletionFixture(
	ctx context.Context,
	post TaskCommitPostFunction[*commitPostArgs, *commitPostResult],
	configure ...func(*TaskWorker),
) *taskBatchCompletionFixture {
	fixture := &taskBatchCompletionFixture{
		collected:         make(chan struct{}),
		continueCollector: make(chan struct{}),
		releaseSlow:       make(chan struct{}),
		slowDone:          make(chan struct{}),
		heartbeatTick:     make(chan time.Time, 1),
		heartbeatTaskIds:  make(chan []server.Id, 1),
		done:              make(chan struct{}),
	}
	slowStarted := make(chan struct{})
	fixture.worker = newCommitPostWorker(ctx, post)
	syntheticNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fixture.worker.heartbeatNow = func() time.Time { return syntheticNow }
	fixture.worker.AddTargets(
		&taskBatchCompletionTarget{
			Target: NewTaskTargetWithCommitPost(commitPostWork, post),
			before: func() { <-slowStarted },
		},
		&taskBatchCompletionSlowTarget{
			Target:  NewTaskTarget(claimProfileExcluded),
			fixture: fixture,
			started: slowStarted,
		},
	)
	selectCount := 0
	fixture.worker.heartbeatAfter = func(time.Duration) <-chan time.Time {
		selectCount++
		if selectCount == 2 {
			close(fixture.collected)
			<-fixture.continueCollector
		}
		return fixture.heartbeatTick
	}
	fixture.worker.refreshTaskTimestampLeases = func(ctx context.Context, tasks map[server.Id]*Task) {
		taskIds := make([]server.Id, 0, len(tasks))
		for taskId := range tasks {
			taskIds = append(taskIds, taskId)
		}
		refreshTaskTimestampLeases(ctx, tasks)
		fixture.heartbeatTaskIds <- taskIds
	}
	clientSession := session.NewLocalClientSession(context.Background(), "192.0.2.1:1", nil)
	defer clientSession.Cancel()
	fixture.fastTaskId = scheduleCommitPostWork(clientSession)
	fixture.slowTaskId = ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
	for _, apply := range configure {
		apply(fixture.worker)
	}
	go func() {
		defer close(fixture.done)
		defer func() { fixture.outcome.panicValue = recover() }()
		fixture.outcome.finishedTaskIds, fixture.outcome.rescheduledTaskIds, fixture.outcome.postRescheduledTaskIds, fixture.outcome.err = fixture.worker.EvalTasks(2)
	}()
	return fixture
}

// Releases both barriers even on an assertion failure, then joins the evaluator.
func (self *taskBatchCompletionFixture) close(t testing.TB) {
	self.releaseOnce.Do(func() { close(self.releaseSlow) })
	self.continueOnce.Do(func() { close(self.continueCollector) })
	waitForSignal(t, self.done, 10*time.Second, "batch completion cleanup")
	waitForSignal(t, self.slowDone, 10*time.Second, "batch sibling unwind")
	self.worker.Close()
}

// Forces one real ownership heartbeat while the slow task remains blocked.
func (self *taskBatchCompletionFixture) heartbeat(t testing.TB) []server.Id {
	self.heartbeatTick <- time.Now()
	self.continueOnce.Do(func() { close(self.continueCollector) })
	select {
	case taskIds := <-self.heartbeatTaskIds:
		return taskIds
	case <-time.After(10 * time.Second):
		t.Fatal("batch completion heartbeat did not run")
		return nil
	}
}

// A committed continuation is claimable before the batch ends; completed rows
// leave the heartbeat set, and external commit work retains its old join point.
func TestTaskBatchCompletionPublishesBeforeUnrelatedWorkReturns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var successorId server.Id
		var commitCount atomic.Int32
		fixture := newTaskBatchCompletionFixture(ctx, func(_ *commitPostArgs, _ *commitPostResult, clientSession *session.ClientSession, tx server.PgTx) ([]server.PostFunction, error) {
			successorId = ScheduleTaskInTx(tx, claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)), RunOnce("synthetic_batch_completion_successor"))
			return []server.PostFunction{func() any { commitCount.Add(1); return nil }}, nil
		})
		defer fixture.close(t)
		waitForSignal(t, fixture.collected, 10*time.Second, "completed fast result")
		if GetFinishedTasks(ctx, fixture.fastTaskId)[fixture.fastTaskId] == nil || GetTasks(ctx, fixture.fastTaskId)[fixture.fastTaskId] != nil {
			t.Fatal("completed fast task still waits for unrelated work before persistence")
		}
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		nextWorker := NewTaskWorker(ctx, settings)
		defer nextWorker.Close()
		nextWorker.AddTargets(NewTaskTarget(claimProfileAllowed))
		claimed, guard, err := nextWorker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || claimed[successorId] == nil {
			t.Fatalf("completed fast task did not publish a claimable successor: count=%d error=%v", len(claimed), err)
		}
		if commitCount.Load() != 0 {
			t.Fatal("external commit work ran inside the live batch collector")
		}
		if taskIds := fixture.heartbeat(t); len(taskIds) != 1 || taskIds[0] != fixture.slowTaskId {
			t.Fatalf("completed task retained heartbeat ownership: %v", taskIds)
		}
		fixture.close(t)
		if fixture.outcome.err != nil || fixture.outcome.panicValue != nil || len(fixture.outcome.finishedTaskIds) != 2 || commitCount.Load() != 1 {
			t.Fatalf("batch did not complete exactly once: %+v commit_count=%d", fixture.outcome, commitCount.Load())
		}
	})
}

// A failed finish stays unresolved and owned while live siblings execute. Its
// original panic returns only after sibling completion and committed post work.
func TestTaskBatchCompletionFailureRetainsUnrelatedOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		createCommitPostFault(ctx, 0, true)
		var commitCount atomic.Int32
		var postCount atomic.Int32
		fixture := newTaskBatchCompletionFixture(ctx, func(args *commitPostArgs, _ *commitPostResult, clientSession *session.ClientSession, tx server.PgTx) ([]server.PostFunction, error) {
			postCount.Add(1)
			writeCommitPostMarker(clientSession.Ctx, tx, args.MarkerId)
			return []server.PostFunction{func() any { commitCount.Add(1); return nil }}, nil
		})
		defer fixture.close(t)
		waitForSignal(t, fixture.collected, 10*time.Second, "failed fast finalization")
		if fixture.slowCtx.Err() != nil {
			t.Fatalf("sibling canceled by unrelated completion failure: %v", fixture.slowCtx.Err())
		}
		if GetFinishedTasks(ctx, fixture.fastTaskId)[fixture.fastTaskId] != nil || GetTasks(ctx, fixture.fastTaskId)[fixture.fastTaskId] == nil || commitCount.Load() != 0 {
			t.Fatal("failed completion retained rolled-back state or external work")
		}
		// Expiring only the timestamp cannot bypass the still-live advisory owner.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET release_time = $2 WHERE task_id = $1`, fixture.slowTaskId, server.NowUtc().Add(-time.Hour)))
		})
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		nextWorker := NewTaskWorker(ctx, settings)
		defer nextWorker.Close()
		nextWorker.AddTargets(NewTaskTarget(claimProfileExcluded))
		claimed, guard, err := nextWorker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard != nil || len(claimed) != 0 {
			t.Fatalf("completion failure surrendered an executing sibling: count=%d error=%v", len(claimed), err)
		}
		if taskIds := fixture.heartbeat(t); len(taskIds) != 2 || !slices.Contains(taskIds, fixture.fastTaskId) || !slices.Contains(taskIds, fixture.slowTaskId) {
			t.Fatalf("failed completion or live sibling lost heartbeat: %v", taskIds)
		}
		fixture.close(t)
		if err, ok := fixture.outcome.panicValue.(error); !ok || !strings.Contains(err.Error(), "synthetic failure at commit") {
			t.Fatalf("completion failure lost original panic: %+v", fixture.outcome)
		}
		if GetFinishedTasks(ctx, fixture.slowTaskId)[fixture.slowTaskId] == nil || fixture.slowCommitCount.Load() != 1 {
			t.Fatal("sibling success or committed external work was lost with the failed completion")
		}
		if GetTasks(ctx, fixture.fastTaskId)[fixture.fastTaskId] == nil || commitCount.Load() != 0 || postCount.Load() != 1 {
			t.Fatal("failed completion was replayed or ran its rolled-back external work")
		}
	})
}

// A fatal collector error after an early commit must still hand off that
// commit's external work, with live function cancellation preceding handoff.
func TestTaskBatchCompletionCollectorPanicPreservesCommittedPosts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		want := errors.New("synthetic collector failure after committed completion")
		var commitCount atomic.Int32
		var canceledBeforePost atomic.Bool
		postCtx := make(chan context.Context, 1)
		fixture := newTaskBatchCompletionFixture(ctx, func(_ *commitPostArgs, _ *commitPostResult, _ *session.ClientSession, _ server.PgTx) ([]server.PostFunction, error) {
			return []server.PostFunction{func() any {
				commitCount.Add(1)
				canceledBeforePost.Store((<-postCtx).Err() != nil)
				return nil
			}}, nil
		}, func(worker *TaskWorker) {
			after := worker.heartbeatAfter
			selectCount := 0
			worker.heartbeatAfter = func(duration time.Duration) <-chan time.Time {
				selectCount++
				result := after(duration)
				if selectCount == 2 {
					panic(want)
				}
				return result
			}
		})
		defer fixture.close(t)
		waitForSignal(t, fixture.collected, 10*time.Second, "committed result before collector failure")
		postCtx <- fixture.slowCtx
		fixture.continueOnce.Do(func() { close(fixture.continueCollector) })
		waitForSignal(t, fixture.done, 10*time.Second, "collector failure handback")
		if fixture.outcome.panicValue != want || commitCount.Load() != 1 || !canceledBeforePost.Load() {
			t.Fatalf("collector failure lost committed handback or cancellation order: panic=%v commit_count=%d canceled=%t", fixture.outcome.panicValue, commitCount.Load(), canceledBeforePost.Load())
		}
		if GetFinishedTasks(ctx, fixture.fastTaskId)[fixture.fastTaskId] == nil {
			t.Fatal("collector failure lost the already committed completion")
		}
	})
}

// A slow finalization advances a synthetic clock past the heartbeat deadline.
// The next select cannot reset the interval and postpone live-sibling upkeep.
func TestTaskBatchCompletionHeartbeatsBetweenSlowFinalizations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		var elapsed atomic.Int64
		syntheticStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		fixture := newTaskBatchCompletionFixture(context.Background(), func(_ *commitPostArgs, _ *commitPostResult, _ *session.ClientSession, _ server.PgTx) ([]server.PostFunction, error) {
			elapsed.Add(int64(ReleaseTimeout))
			return nil, nil
		}, func(worker *TaskWorker) {
			worker.heartbeatNow = func() time.Time { return syntheticStart.Add(time.Duration(elapsed.Load())) }
		})
		defer fixture.close(t)
		waitForSignal(t, fixture.collected, 10*time.Second, "select after slow finalization")
		select {
		case taskIds := <-fixture.heartbeatTaskIds:
			if len(taskIds) != 1 || taskIds[0] != fixture.slowTaskId {
				t.Fatalf("overdue heartbeat did not retain only the live sibling: %v", taskIds)
			}
		default:
			t.Fatal("slow finalization postponed heartbeat behind the next select")
		}
	})
}

// Root cancellation after one commit still hands a returning sibling back with
// the existing drain classification and keeps the successful commit intact.
func TestTaskBatchCompletionSurvivesRootCancellation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fixture := newTaskBatchCompletionFixture(ctx, func(_ *commitPostArgs, _ *commitPostResult, _ *session.ClientSession, _ server.PgTx) ([]server.PostFunction, error) {
			return nil, nil
		})
		defer fixture.close(t)
		waitForSignal(t, fixture.collected, 10*time.Second, "fast completion before cancellation")
		cancel()
		select {
		case <-fixture.slowCtx.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("root cancellation did not reach live sibling")
		}
		fixture.close(t)
		outcome := fixture.outcome
		if outcome.err != nil || outcome.panicValue != nil || len(outcome.finishedTaskIds) != 1 || outcome.finishedTaskIds[0] != fixture.fastTaskId || len(outcome.rescheduledTaskIds) != 1 || outcome.rescheduledTaskIds[0] != fixture.slowTaskId {
			t.Fatalf("root cancellation lost partial completion: %+v", outcome)
		}
		pending := readPendingTaskState(context.Background(), fixture.slowTaskId)
		if !pending.exists || pending.errorCount != 0 || !strings.Contains(pending.rescheduleError, ErrDrained.Error()) {
			t.Fatalf("root cancellation changed drain handback: %+v", pending)
		}
	})
}
