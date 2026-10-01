// Real PostgreSQL claims preserve advisory ownership while local admission
// spreads an explicitly limited target across independent worker instances.
package task

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// A returned claim owns its guard until the test explicitly releases it.
type claimAdmissionTestResult struct {
	tasks map[server.Id]*Task
	guard *taskClaimGuard
	err   error
}

// A held limited claim must not admit a second one on this instance or hide
// ordinary work behind more limited rows than the SQL candidate window.
func TestTaskClaimAdmissionDistributesWithoutStarvingOrdinaryWork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		limited := NewTaskTarget(claimProfileAllowed)
		settings := DefaultTaskWorkerSettings()
		settings.TargetClaimLimits = map[string]int{limited.TargetFunctionName(): 1}
		first := NewTaskWorker(ctx, settings)
		second := NewTaskWorker(ctx, settings)
		defer first.Close()
		defer second.Close()
		first.AddTargets(limited, NewTaskTarget(claimProfileExcluded))
		second.AddTargets(limited)
		limitedIds := make([]server.Id, 130)
		for index := range limitedIds {
			limitedIds[index] = ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession,
				RunAt(server.NowUtc().Add(-2*time.Hour)), MaxTime(75*time.Minute))
		}
		claimed, held, err := first.takeTasks(1)
		if held != nil {
			defer held.release()
		}
		if err != nil || held == nil || len(claimed) != 1 {
			t.Fatalf("could not establish the held local claim: count=%d error=%v", len(claimed), err)
		}
		before := GetTasks(ctx, limitedIds...)
		duplicate, extra, err := first.takeTasks(1)
		if extra != nil {
			defer extra.release()
		}
		if err != nil || extra != nil || len(duplicate) != 0 {
			t.Fatalf("one worker co-located limited tasks: extra=%d error=%v", len(duplicate), err)
		}
		ordinaryId := ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		finished, retried, postRetried, err := first.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != ordinaryId || len(retried)+len(postRetried) != 0 {
			t.Fatalf("saturated target hid ordinary work past the candidate limit: finished=%d retries=%d/%d error=%v", len(finished), len(retried), len(postRetried), err)
		}
		if after := GetTasks(ctx, limitedIds...); !reflect.DeepEqual(before, after) {
			t.Fatal("skipping locally saturated work changed its durable claims")
		}
		other, otherGuard, err := second.takeTasks(1)
		if otherGuard != nil {
			defer otherGuard.release()
		}
		if err != nil || otherGuard == nil || len(other) != 1 {
			t.Fatalf("another instance could not use its independent capacity: count=%d error=%v", len(other), err)
		}
		for taskId := range other {
			if claimed[taskId] != nil {
				t.Fatal("instance-local admission bypassed advisory ownership")
			}
		}
	})
}

// Both SELECTs finish before either caller can reserve locally. Their row
// locks protect different candidate windows; the local bound still admits one.
func TestTaskClaimAdmissionConcurrentSqlSnapshots(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		worker, _ := newClaimAdmissionTestWorker(t, 1)
		clientSession := session.NewLocalClientSession(context.Background(), "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		for range 130 {
			ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		}
		ready := make(chan struct{}, 2)
		release := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		worker.claimCandidatesReady = func() { ready <- struct{}{}; <-release }
		results := make(chan claimAdmissionTestResult, 2)
		for range 2 {
			go func() {
				tasks, guard, err := worker.takeTasks(1)
				results <- claimAdmissionTestResult{tasks: tasks, guard: guard, err: err}
			}()
		}
		for range 2 {
			waitForSignal(t, ready, 10*time.Second, "concurrent claim SELECT")
		}
		releaseOnce.Do(func() { close(release) })
		claimed := 0
		for range 2 {
			result := <-results
			if result.guard != nil {
				defer result.guard.release()
			}
			if result.err != nil {
				t.Fatal(result.err)
			}
			claimed += len(result.tasks)
		}
		if claimed != 1 {
			t.Fatalf("concurrent SQL snapshots admitted %d limited tasks, want one", claimed)
		}
	})
}

// A failure after timestamp writes rolls them back, unlocks the advisory
// session and returns local capacity before this instance retries.
func TestTaskClaimAdmissionRollbackReturnsCapacity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		worker, _ := newClaimAdmissionTestWorker(t, 1)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		taskId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		before := GetTasks(ctx, taskId)
		want := errors.New("synthetic claim rollback")
		worker.claimBeforeCommit = func(guard *taskClaimGuard) error {
			if len(guard.admissionKVs) != 1 {
				t.Error("failure injection missed the reserved/advisory-owned claim")
			}
			return want
		}
		claimed, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if !errors.Is(err, want) || guard != nil || len(claimed) != 0 {
			t.Fatalf("failed claim retained ownership: count=%d error=%v", len(claimed), err)
		}
		if after := GetTasks(ctx, taskId); !reflect.DeepEqual(before, after) {
			t.Fatal("rolled-back claim left timestamp mutations behind")
		}
		worker.claimBeforeCommit = nil
		claimed, guard, err = worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || claimed[taskId] == nil {
			t.Fatalf("rollback leaked local or advisory capacity: count=%d error=%v", len(claimed), err)
		}
	})
}

// Existing high-priority batch isolation may reject a speculatively locked
// limited row. Its token must return while the selected ordinary guard lives.
func TestTaskClaimAdmissionSpeculativePruningReturnsCapacity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		worker, _ := newClaimAdmissionTestWorker(t, 1)
		worker.AddTargets(NewTaskTarget(claimProfileExcluded))
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		limitedId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-2*time.Hour)))
		ordinaryId := ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, clientSession,
			RunAt(server.NowUtc().Add(-time.Hour)), Priority(TaskPriorityFastest))
		before := GetTasks(ctx, limitedId)
		claimed, ordinaryGuard, err := worker.takeTasks(2)
		if ordinaryGuard != nil {
			defer ordinaryGuard.release()
		}
		if err != nil || ordinaryGuard == nil || len(claimed) != 1 || claimed[ordinaryId] == nil {
			t.Fatalf("existing priority batching changed: count=%d error=%v", len(claimed), err)
		}
		if after := GetTasks(ctx, limitedId); !reflect.DeepEqual(before, after) {
			t.Fatal("pruned candidate retained a timestamp claim")
		}
		claimed, limitedGuard, err := worker.takeTasks(1)
		if limitedGuard != nil {
			defer limitedGuard.release()
		}
		if err != nil || limitedGuard == nil || claimed[limitedId] == nil {
			t.Fatalf("speculative pruning leaked local/advisory capacity: count=%d error=%v", len(claimed), err)
		}
	})
}

// A candidate can have an expired timestamp while its advisory owner is live.
// Refusing that lock must not spend the slot needed by the next candidate.
func TestTaskClaimAdmissionAdvisoryRefusalReturnsCapacity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		worker, _ := newClaimAdmissionTestWorker(t, 1)
		other := NewTaskWorkerWithDefaults(ctx)
		defer other.Close()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		heldId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-2*time.Hour)))
		_, heldGuard, err := other.takeTasks(1)
		if heldGuard == nil || err != nil {
			t.Fatalf("missing live advisory fixture: %v", err)
		}
		defer heldGuard.release()
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := tx.Exec(ctx, `UPDATE pending_task SET claim_time = $2, release_time = $2 WHERE task_id = $1`, heldId, server.NowUtc().Add(-2*time.Hour))
			server.Raise(err)
		})
		nextId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		claimed, nextGuard, err := worker.takeTasks(1)
		if nextGuard != nil {
			defer nextGuard.release()
		}
		if err != nil || nextGuard == nil || len(claimed) != 1 || claimed[nextId] == nil {
			t.Fatalf("advisory refusal consumed local capacity or stole a live task: count=%d error=%v", len(claimed), err)
		}
	})
}
