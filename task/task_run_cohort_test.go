// Cohorts expand durable membership, never physical concurrency or finite Eval.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type taskRunCohortTestTarget struct {
	Target
	before func(context.Context, *Task) error
}

func (self *taskRunCohortTestTarget) TaskRunCohort(argsJson string) (server.Id, int) {
	var args taskClaimGroupTestArgs
	server.Raise(json.Unmarshal([]byte(argsJson), &args))
	return args.GroupId, taskCompletionBatchLimit
}

func (self *taskRunCohortTestTarget) TaskClaimGroupIds(argsJson string) ([]server.Id, int) {
	id, limit := self.TaskRunCohort(argsJson)
	return []server.Id{id}, limit
}

func (self *taskRunCohortTestTarget) PrepareTaskBatch(_ []*Task) Target { return self }
func (self *taskRunCohortTestTarget) TaskCompletionBatchEnabled() bool  { return true }
func (self *taskRunCohortTestTarget) TaskCompletionOwnershipKeys(_ *Task, _ string) ([]server.PgOwnershipKey, error) {
	return nil, nil
}

func (self *taskRunCohortTestTarget) Run(ctx context.Context, queued *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	if self.before != nil {
		if err := self.before(ctx, queued); err != nil {
			return nil, nil, err
		}
	}
	return self.Target.Run(ctx, queued)
}

func taskRunCohortTestSchedule(owner *session.ClientSession, group server.Id, count int) []server.Id {
	ids := make([]server.Id, 0, count)
	for index := range count {
		ids = append(ids, ScheduleTask(taskClaimGroupTestCall, &taskClaimGroupTestArgs{GroupId: group}, owner,
			RunAt(server.NowUtc().Add(-time.Hour+time.Duration(index)*time.Second))))
	}
	return ids
}

// The old slot cap admits one row here. The explicit Run cohort admits64;
// finite EvalTasks(3) still completes exactly three independently owned rows.
func TestTaskRunCohortClaimUsesOneSlotAndKeepsFiniteEvalBound(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := taskRunCohortTestSchedule(owner, server.NewId(), 65)
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(&taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)})
		claimed, guard, isolated, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{runCohorts: true})
		if guard != nil {
			defer guard.release()
		}
		if err != nil || isolated || len(claimed) != 64 || len(taskRunSlots(claimed)) != 1 || claimed[ids[64]] != nil {
			t.Fatalf("one Run slot lost its exact64 bound: claimed=%d slots=%d isolated=%t err=%v", len(claimed), len(taskRunSlots(claimed)), isolated, err)
		}
		for _, id := range ids[:64] {
			if claimed[id] == nil || claimed[id].ClaimGeneration != 1 || claimed[id].runCohort == nil {
				t.Fatal("cohort omitted an earlier durable member or its generation")
			}
		}
		guard.release()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET release_time=$2 WHERE task_id=ANY($1)`, ids, time.Time{}))
		})
		finished, retried, posts, err := worker.EvalTasks(3)
		if err != nil || len(finished) != 3 || len(retried)+len(posts) != 0 || len(GetTasks(ctx, ids...)) != 62 {
			t.Fatalf("finite EvalTasks expanded its requested bound: finished=%d retries=%d/%d error=%v", len(finished), len(retried), len(posts), err)
		}
	})
}

// A spare member position is not permission to bypass earlier ordinary work.
func TestTaskRunCohortClaimPreservesFifoBoundary(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		group := server.NewId()
		ids := taskRunCohortTestSchedule(owner, group, 16)
		ordinary := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner,
			RunAt(server.NowUtc().Add(-time.Hour+7500*time.Millisecond)))
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(&taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)}, NewTaskTarget(claimProfileAllowed))
		// Different available blocks make this a queue-order proof, independent
		// of random task identities or the wall-clock second of publication.
		server.Tx(ctx, func(tx server.PgTx) {
			base := server.NowUtc().Truncate(time.Second).Add(-time.Hour)
			for index, id := range ids {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE task_id=$1`, id, base.Add(time.Duration(2*index)*time.Second)))
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE task_id=$1`, ordinary, base.Add(15*time.Second)))
		})
		claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{runCohorts: true})
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 8 || claimed[ordinary] != nil {
			t.Fatalf("cohort crossed ordinary FIFO boundary: count=%d err=%v", len(claimed), err)
		}
		for index, id := range ids {
			if (claimed[id] != nil) != (index < 8) {
				t.Fatal("cohort skipped an earlier member or claimed a later one")
			}
		}
	})
}

// Explicit per-worker target admission still counts every durable reservation.
func TestTaskRunCohortClaimHonorsTargetAdmission(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := taskRunCohortTestSchedule(owner, server.NewId(), 65)
		target := &taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)}
		settings := DefaultTaskWorkerSettings()
		settings.TargetClaimLimits = map[string]int{target.TargetFunctionName(): 2}
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{runCohorts: true})
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 2 || claimed[ids[0]] == nil || claimed[ids[1]] == nil {
			t.Fatalf("cohort borrowed unreserved target capacity: count=%d err=%v", len(claimed), err)
		}
		guard.release()
		if len(worker.claimTargetCounts) != 0 {
			t.Fatal("cohort leaked a member's target reservation")
		}
	})
}

// Discovery does not authorize a later priority change to expand a full slot.
func TestTaskRunCohortClaimRechecksIsolationBeforeExpansion(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := taskRunCohortTestSchedule(owner, server.NewId(), 3)
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(&taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)})
		worker.claimCandidatesReady = func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_priority=$2 WHERE task_id=$1`, ids[1], DefaultPriority+1))
			})
		}
		claimed, guard, isolated, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{runCohorts: true})
		if guard != nil {
			defer guard.release()
		}
		if err != nil || isolated || len(claimed) != 1 || claimed[ids[0]] == nil {
			t.Fatalf("changed priority borrowed a cohort position: count=%d isolated=%t err=%v", len(claimed), isolated, err)
		}
		later := GetTasks(ctx, ids[1])[ids[1]]
		if later == nil || later.ClaimGeneration != 0 || later.RunPriority != DefaultPriority+1 {
			t.Fatal("isolation recheck claimed or reverted the changed priority")
		}
	})
}

// A same-provider future row remains only a one-shot alarm. It never takes a
// queue owner just because the current physical slot has member capacity.
func TestTaskRunCohortClaimKeepsFutureMembershipUnowned(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := taskRunCohortTestSchedule(owner, server.NewId(), 3)
		now := server.NowUtc().Truncate(time.Second).Add(time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE task_id=$1`, ids[2], now.Add(time.Second)))
		})
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.claimNow = func() time.Time { return now }
		worker.AddTargets(&taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)})
		probed := map[server.Id]bool{}
		worker.claimQueueAdmission = func(id server.Id, _ bool) { probed[id] = true }
		poll := &taskClaimPoll{}
		claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{runCohorts: true, poll: poll})
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 2 || len(probed) != 2 || probed[ids[2]] || !poll.availableAt.Equal(now.Add(time.Second)) {
			t.Fatalf("future member acquired ownership or lost its alarm: claimed=%d probes=%d hint=%v err=%v", len(claimed), len(probed), poll.availableAt, err)
		}
		future := GetTasks(ctx, ids[2])[ids[2]]
		if future == nil || future.ClaimGeneration != 0 || !future.ClaimTime.IsZero() {
			t.Fatal("future cohort member received an execution lease")
		}
	})
}

// All members inherit one earliest ordinary max-time deadline and cancellation;
// returning one member cannot reset the deadline for the next member.
func TestTaskRunCohortExecutionSharesDeadlineAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker := NewTaskWorkerWithDefaults(ctx)
	defer worker.Close()
	var deadlines []time.Time
	var ids []server.Id
	target := &taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)}
	target.before = func(runCtx context.Context, queued *Task) error {
		deadline, ok := runCtx.Deadline()
		if !ok {
			return errors.New("cohort member lost shared deadline")
		}
		deadlines, ids = append(deadlines, deadline), append(ids, queued.TaskId)
		if len(ids) == 1 {
			cancel()
			return nil
		}
		<-runCtx.Done()
		return runCtx.Err()
	}
	worker.AddTargets(target)
	first := &Task{TaskId: server.NewId(), FunctionName: target.TargetFunctionName(), ArgsJson: `{}`, RunMaxTimeSeconds: int(DefaultMaxTime/time.Second) + 30}
	second := &Task{TaskId: server.NewId(), FunctionName: target.TargetFunctionName(), ArgsJson: `{}`, RunMaxTimeSeconds: 10}
	started := time.Now()
	results := worker.executeTaskRunCohort(ctx, &taskRunSlot{tasks: []*Task{first, second}}, map[string]Target{target.TargetFunctionName(): target}, taskExecutionAdmissions{})
	if len(results) != 2 || len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) ||
		deadlines[0].Before(started.Add(DefaultMaxTime)) || deadlines[0].After(time.Now().Add(DefaultMaxTime)) ||
		ids[0] != first.TaskId || ids[1] != second.TaskId || !errors.Is(results[1].err, context.Canceled) || worker.InflightCount() != 0 {
		t.Fatal("cohort changed member order, deadline, cancellation or joined execution")
	}
}

// Cancellation reschedules every immutable member under one complete-key
// owner and joins the single execution before releasing any session locks.
func TestTaskRunCohortDrainHandsBackEveryMemberOnce(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := taskRunCohortTestSchedule(owner, server.NewId(), 64)
		var handbacks atomic.Int64
		workerCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted && !event.TransactionScoped && len(event.Keys) == len(ids) {
				handbacks.Add(1)
			}
		})
		settings := DefaultTaskWorkerSettings()
		settings.DrainFinishTimeout = 0
		worker := NewTaskWorker(workerCtx, settings)
		defer worker.Close()
		entered := make(chan struct{})
		var first sync.Once
		target := &taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)}
		target.before = func(runCtx context.Context, _ *Task) error {
			first.Do(func() { close(entered) })
			<-runCtx.Done()
			return runCtx.Err()
		}
		worker.AddTargets(target)
		done := make(chan error, 1)
		go func() {
			var runErr error
			server.HandleError(worker.Run, func(err error) { runErr = err })
			done <- runErr
		}()
		joined := false
		defer func() {
			worker.Close()
			worker.Drain()
			worker.WaitFinalHandback()
			if !joined {
				_ = taskQueueError(ctx, done)
			}
		}()
		taskQueueWait(ctx, entered)
		if worker.InflightCount() != 1 {
			t.Fatal("cohort launched multiple execution owners")
		}
		worker.Drain()
		handedBack := worker.WaitFinalHandback()
		runErr := taskQueueError(ctx, done)
		joined = true
		if !handedBack || runErr != nil || worker.InflightCount() != 0 ||
			worker.DrainCanceledCount() != 64 || handbacks.Load() != 1 || len(GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatalf("cohort drain lost joined handback: canceled=%d handbacks=%d", worker.DrainCanceledCount(), handbacks.Load())
		}
		pending := GetTasks(ctx, ids...)
		if len(pending) != len(ids) {
			t.Fatal("cohort drain lost durable identities")
		}
		for _, queued := range pending {
			if queued.ClaimGeneration != 1 || queued.RescheduleErrorCount != 0 || queued.RescheduleError == "" {
				t.Fatal("cohort drain changed exact claim generation or retry classification")
			}
		}
	})
}

// One changed generation refuses the complete batch atomically. Successful
// neighbors cannot fall back to independent handbacks with fresh budgets.
func TestTaskRunCohortCompletionRejectsChangedGenerationAtomically(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := taskRunCohortTestSchedule(owner, server.NewId(), 4)
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(&taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)})
		claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{runCohorts: true})
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != len(ids) {
			t.Fatal("missing cohort before generation mutation", err)
		}
		results := worker.executeTaskRunCohort(ctx, taskRunSlots(claimed)[0], worker.prepareTaskBatchTargets(claimed), taskExecutionAdmissions{})
		server.OwnedTx(ctx, []server.PgOwnershipKey{PendingTaskOwnershipKey(ids[1], nil)}, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET claim_generation=claim_generation+1 WHERE task_id=$1`, ids[1]))
		}, server.TxReadCommitted, server.OptNoRetry())
		commits := 0
		worker.completionBatchCommitReturned = func() { commits++ }
		if err := worker.finalizeTaskRunCohort(results); !errors.Is(err, errTaskCompletionBatchOwnership) || commits != 0 {
			t.Fatal("cohort acknowledged a changed exact claim generation", err, commits)
		}
		if len(GetTasks(ctx, ids...)) != len(ids) || len(GetFinishedTasks(ctx, ids...)) != 0 || len(guard.taskIds) != len(ids) {
			t.Fatal("cohort replayed independent handbacks or retired an unacknowledged owner")
		}
	})
}

// Lose the reply only after PostgreSQL committed every exact member. The caller
// retains uncertainty and the live guard; it never reruns the committed move.
func TestTaskRunCohortCompletionLostReplyDoesNotReplay(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := taskRunCohortTestSchedule(owner, server.NewId(), 4)
		var handbacks atomic.Int64
		workerCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted && !event.TransactionScoped && len(event.Keys) == len(ids) {
				handbacks.Add(1)
			}
		})
		worker := NewTaskWorkerWithDefaults(workerCtx)
		defer worker.Close()
		worker.AddTargets(&taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)})
		claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{runCohorts: true})
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != len(ids) {
			t.Fatal("missing cohort before lost completion reply", err)
		}
		results := worker.executeTaskRunCohort(ctx, taskRunSlots(claimed)[0], worker.prepareTaskBatchTargets(claimed), taskExecutionAdmissions{})
		lost := errors.New("synthetic committed cohort reply lost")
		commits := 0
		worker.completionBatchCommitReturned = func() { commits++; panic(lost) }
		if err := worker.finalizeTaskRunCohort(results); !errors.Is(err, lost) || commits != 1 || handbacks.Load() != 1 {
			t.Fatal("cohort hid uncertainty or replayed its committed handback", err, commits, handbacks.Load())
		}
		if len(GetTasks(ctx, ids...)) != 0 || len(GetFinishedTasks(ctx, ids...)) != len(ids) || len(guard.taskIds) != len(ids) {
			t.Fatal("lost reply changed committed membership or released the live guard")
		}
		probe, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer probe.Release()
		for _, id := range ids {
			var acquired bool
			server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, taskAdvisoryLockKey(id)).Scan(&acquired))
			if acquired {
				server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1)`, taskAdvisoryLockKey(id)))
				t.Fatal("an unacknowledged member lost its exact session owner")
			}
		}
	})
}
