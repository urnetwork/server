// Actual EvalTasks cancellation/panic boundaries keep a limited task's slot
// until its goroutine exits, independent of claim-session cleanup ordering.
package task

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Each fixture owns its callback and barriers; no package-global test state.
type claimAdmissionLifecycleTarget struct {
	Target
	run func(context.Context) error
}

// The caller can hold execution after cancellation without delaying unrelated
// callbacks, making the real goroutine-retirement boundary observable.
func (self *claimAdmissionLifecycleTarget) Run(ctx context.Context, _ *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	return &claimProfileResult{}, func(server.PgTx) ([]server.PostFunction, error) { return nil, nil }, self.run(ctx)
}

// Wrap the already-owned capacity return at the pre-commit test seam. Closing
// this channel proves the real guard and execution owners have both retired.
func observeClaimAdmissionReturn(worker *TaskWorker) chan struct{} {
	returned := make(chan struct{})
	var once sync.Once
	worker.claimBeforeCommit = func(guard *taskClaimGuard) error {
		for _, reservation := range guard.admissionKVs {
			returnCapacity := reservation.returnCapacity
			reservation.returnCapacity = func() {
				returnCapacity()
				once.Do(func() { close(returned) })
			}
		}
		return nil
	}
	return returned
}

// A collector panic cancels execution and releases its advisory guard, but a
// context-ignoring unwind must still occupy this instance's local probe slot.
func TestTaskClaimAdmissionCollectorPanicWaitsForUnwind(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		worker, _ := newClaimAdmissionTestWorker(t, 1)
		returned := observeClaimAdmissionReturn(worker)
		started, canceled, unwind := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var unwindOnce sync.Once
		defer unwindOnce.Do(func() { close(unwind) })
		worker.AddTargets(&claimAdmissionLifecycleTarget{Target: NewTaskTarget(claimProfileAllowed), run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-unwind
			return ctx.Err()
		}})
		want := errors.New("synthetic collector panic")
		worker.heartbeatAfter = func(time.Duration) <-chan time.Time { <-started; panic(want) }
		clientSession := session.NewLocalClientSession(context.Background(), "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		for range 2 {
			ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		}
		panicked := make(chan any, 1)
		go func() {
			defer func() { panicked <- recover() }()
			worker.EvalTasks(1)
		}()
		waitForSignal(t, canceled, 10*time.Second, "panic cancellation")
		if recovered := <-panicked; recovered != want {
			t.Fatalf("fixture did not reach the collector panic: %v", recovered)
		}
		claimed, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 0 || guard != nil {
			t.Fatalf("collector panic admitted a replacement before unwind: count=%d error=%v", len(claimed), err)
		}
		unwindOnce.Do(func() { close(unwind) })
		waitForSignal(t, returned, 10*time.Second, "actual execution retirement")
		claimed, guard, err = worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 1 || guard == nil {
			t.Fatalf("completed unwind leaked its local slot: count=%d error=%v", len(claimed), err)
		}
	})
}

// Drain's give-up does not clear admission. The real Run loop keeps its claim
// and token through delayed unwind and the detached reschedule transaction.
func TestTaskClaimAdmissionDrainWaitsForUnwind(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		worker, name := newClaimAdmissionTestWorker(t, 1)
		worker.settings.DrainFinishTimeout = 0
		worker.settings.DrainCancelTimeout = 0
		returned := observeClaimAdmissionReturn(worker)
		started, canceled, unwind := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var unwindOnce sync.Once
		defer unwindOnce.Do(func() { close(unwind) })
		worker.AddTargets(&claimAdmissionLifecycleTarget{Target: NewTaskTarget(claimProfileAllowed), run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-unwind
			return ctx.Err()
		}})
		clientSession := session.NewLocalClientSession(context.Background(), "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		taskId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		done := make(chan struct{})
		go func() { defer close(done); worker.Run() }()
		waitForSignal(t, started, 10*time.Second, "draining task start")
		worker.Drain()
		waitForSignal(t, canceled, 10*time.Second, "drain cancellation")
		select {
		case <-returned:
			t.Error("drain give-up freed a task still executing")
		default:
		}
		unwindOnce.Do(func() { close(unwind) })
		waitForSignal(t, done, 10*time.Second, "detached drain handback")
		waitForSignal(t, returned, 10*time.Second, "drained claim capacity return")
		if worker.claimTargetCounts[name] != 0 {
			t.Fatal("drained task retained admission after execution and handback")
		}
		pending := readPendingTaskState(context.Background(), taskId)
		if !pending.exists || pending.errorCount != 0 || worker.DrainCanceledCount() != 1 {
			t.Fatal("admission changed ordinary drain rescheduling")
		}
	})
}

// A finished limited task does not borrow an unrelated sibling's execution
// lifetime when a later collector panic releases the batch's claim guard.
func TestTaskClaimAdmissionMixedBatchReleasesPerTask(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		worker, _ := newClaimAdmissionTestWorker(t, 1)
		returned := observeClaimAdmissionReturn(worker)
		limitedStarted, ordinaryStarted := make(chan struct{}), make(chan struct{})
		finishLimited, ordinaryCanceled, unwindOrdinary := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var limitedOnce, ordinaryOnce sync.Once
		defer limitedOnce.Do(func() { close(finishLimited) })
		defer ordinaryOnce.Do(func() { close(unwindOrdinary) })
		ordinaryDone := make(chan struct{})
		worker.AddTargets(
			&claimAdmissionLifecycleTarget{Target: NewTaskTarget(claimProfileAllowed), run: func(context.Context) error {
				close(limitedStarted)
				<-finishLimited
				return nil
			}},
			&claimAdmissionLifecycleTarget{Target: NewTaskTarget(claimProfileExcluded), run: func(ctx context.Context) error {
				defer close(ordinaryDone)
				close(ordinaryStarted)
				<-ctx.Done()
				close(ordinaryCanceled)
				<-unwindOrdinary
				return ctx.Err()
			}},
		)
		var polls atomic.Int32
		never := make(chan time.Time)
		want := errors.New("synthetic post-result collector panic")
		worker.heartbeatAfter = func(time.Duration) <-chan time.Time {
			if polls.Add(1) == 2 {
				panic(want)
			}
			return never
		}
		clientSession := session.NewLocalClientSession(context.Background(), "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-3*time.Hour)))
		ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-2*time.Hour)))
		nextId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		panicked := make(chan any, 1)
		go func() {
			defer func() { panicked <- recover() }()
			worker.EvalTasks(2)
		}()
		waitForSignal(t, limitedStarted, 10*time.Second, "mixed limited task")
		waitForSignal(t, ordinaryStarted, 10*time.Second, "mixed ordinary task")
		limitedOnce.Do(func() { close(finishLimited) })
		waitForSignal(t, ordinaryCanceled, 10*time.Second, "mixed collector cancellation")
		if recovered := <-panicked; recovered != want {
			t.Fatalf("fixture missed the post-result collector panic: %v", recovered)
		}
		waitForSignal(t, returned, 10*time.Second, "completed limited task retirement")
		claimed, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || claimed[nextId] == nil {
			t.Fatalf("ordinary sibling retained an already-finished limited task's slot: count=%d error=%v", len(claimed), err)
		}
		ordinaryOnce.Do(func() { close(unwindOrdinary) })
		waitForSignal(t, ordinaryDone, 10*time.Second, "ordinary sibling unwind")
	})
}
