// A fatal Run handback stops admission, cancels cooperative siblings, and joins
// every function and committed post before releasing its execution owners.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Only the deliberately failing handback declares the held publication key.
// Every task still uses the ordinary pending/finished ownership protocol.
type taskRunFinalizationCancelTarget struct {
	*runOnceGenerationTarget
	failedScope server.Id
	publication server.PgOwnershipKey
}

// Delegate cancellation through the real body/session adapter so the collector
// can distinguish its interruption from an unrelated returned task failure.
func taskRunFinalizationCancellationWork(_ *runOnceGenerationArgs, client *session.ClientSession) (*struct{}, error) {
	if err := client.Ctx.Err(); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

func (self *taskRunFinalizationCancelTarget) TaskCompletionOwnershipKeys(queued *Task, _ string) ([]server.PgOwnershipKey, error) {
	var args runOnceGenerationArgs
	if err := json.Unmarshal([]byte(queued.ArgsJson), &args); err != nil {
		return nil, err
	}
	if args.Scope == self.failedScope {
		return []server.PgOwnershipKey{self.publication}, nil
	}
	return nil, nil
}

// Both a successful result with a busy continuation key and a returned error
// with a busy pending key traverse the real five-second pre-BEGIN deadline.
// The collector's next select is a positive barrier: the old deployed Run
// reaches it with the unrelated function context still live and stops refill.
func TestTaskRunFinalizationFailureCancelsSiblingsAndJoinsCommittedPosts(t *testing.T) {
	for _, returnedError := range []bool{false, true} {
		runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
			var reruns atomic.Int64
			ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			failedScope, siblingScope, postScope, nextScope := server.NewId(), server.NewId(), server.NewId(), server.NewId()
			publication := RunOnceOwnershipKey(runOnceGenerationKey(server.NewId()))
			blockingKey := publication
			if returnedError {
				blockingKey = RunOnceOwnershipKey(runOnceGenerationKey(failedScope))
			}
			siblingKey := RunOnceOwnershipKey(runOnceGenerationKey(siblingScope))
			failedStarted, postStarted := make(chan struct{}), make(chan struct{})
			siblingStarted := make(chan context.Context, 1)
			failureRelease, siblingRelease, postRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
			collectorReady, collectorRelease := make(chan struct{}), make(chan struct{})
			ownerReady, ownerRelease := make(chan struct{}), make(chan struct{})
			siblingHandedBack := make(chan struct{})
			var failureOnce, siblingOnce, postOnce, collectorOnce, ownerOnce, handbackOnce sync.Once
			var admissionExpired atomic.Bool
			var refused, failedAdmissions, failedCalls, failedPosts, siblingCalls, postGenerations, nextCalls atomic.Int32
			workerCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
				if slices.Contains(event.Keys, blockingKey) {
					switch event.Kind {
					case server.PgOwnershipWaiting:
						refused.Add(1)
						// Preserve the e038 mechanism control: a real refused
						// advisory probe released its checkout before this wait.
						timer := time.NewTimer(server.PgOwnershipAdmissionTimeout + 25*time.Millisecond)
						defer timer.Stop()
						select {
						case <-timer.C:
							admissionExpired.Store(true)
						case <-ctx.Done():
							panic(ctx.Err())
						}
					case server.PgOwnershipAdmitted:
						failedAdmissions.Add(1)
					}
				}
				if admissionExpired.Load() && event.Kind == server.PgOwnershipReleased && slices.Contains(event.Keys, siblingKey) {
					handbackOnce.Do(func() { close(siblingHandedBack) })
				}
			})
			settings := DefaultTaskWorkerSettings()
			settings.BatchSize = 3
			settings.ClaimRegisteredTargetsOnly = true
			worker := NewTaskWorker(workerCtx, settings)
			defer worker.Close()
			now := server.NowUtc()
			worker.claimNow = func() time.Time { return now }
			worker.heartbeatNow = func() time.Time { return now }
			never := make(chan time.Time)
			observed := false
			worker.heartbeatAfter = func(time.Duration) <-chan time.Time {
				if admissionExpired.Load() && !observed {
					observed = true
					close(collectorReady)
					taskQueueWait(ctx, collectorRelease)
				}
				return never
			}
			target := &taskRunFinalizationCancelTarget{
				runOnceGenerationTarget: &runOnceGenerationTarget{Target: NewTaskTargetWithCommitPost(taskRunFinalizationCancellationWork,
					func(args *runOnceGenerationArgs, _ *struct{}, _ *session.ClientSession, _ server.PgTx) ([]server.PostFunction, error) {
						switch args.Scope {
						case failedScope:
							failedPosts.Add(1)
						case postScope:
							return []server.PostFunction{func() any {
								close(postStarted)
								taskQueueWait(ctx, postRelease)
								postGenerations.Add(1)
								return server.PostFunction(func() any { postGenerations.Add(1); return nil })
							}}, nil
						case nextScope:
							return []server.PostFunction{func() any { worker.runCancel(); return nil }}, nil
						}
						return nil, nil
					})},
				failedScope: failedScope,
				publication: publication,
			}
			target.before = func(runCtx context.Context, queued *Task) error {
				var args runOnceGenerationArgs
				server.Raise(json.Unmarshal([]byte(queued.ArgsJson), &args))
				switch args.Scope {
				case failedScope:
					failedCalls.Add(1)
					close(failedStarted)
					taskQueueWait(runCtx, failureRelease)
					if returnedError {
						return errors.New("synthetic returned task failure")
					}
				case siblingScope:
					siblingCalls.Add(1)
					siblingStarted <- runCtx
					// Expose cancellation without permitting an early join.
					// The test releases this function after probing all guards.
					taskQueueWait(ctx, siblingRelease)
				case nextScope:
					nextCalls.Add(1)
				}
				return nil
			}
			worker.AddTargets(target)
			finalizationFailures := taskFinalizationErrorsTotal.WithLabelValues(worker.metricName(target.TargetFunctionName()), "deadline")
			beforeFinalizationFailures := testutil.ToFloat64(finalizationFailures)
			ids := make([]server.Id, 0, 4)
			for index, scope := range []server.Id{failedScope, siblingScope, postScope, nextScope} {
				at := now.Add(-2 * time.Hour)
				if index == 3 {
					at = now.Add(-time.Hour)
				}
				ids = append(ids, ScheduleTask(taskRunFinalizationCancellationWork, &runOnceGenerationArgs{Scope: scope}, owner,
					runOnceGenerationKey(scope), RunAt(at)))
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error_count=19 WHERE task_id=$1`, ids[1]))
			})
			runDone := make(chan struct{})
			var runErr error
			var ownerDone chan error
			var nextDone chan struct{}
			runJoined, ownerJoined := false, false
			defer func() {
				worker.Close()
				failureOnce.Do(func() { close(failureRelease) })
				siblingOnce.Do(func() { close(siblingRelease) })
				postOnce.Do(func() { close(postRelease) })
				collectorOnce.Do(func() { close(collectorRelease) })
				ownerOnce.Do(func() { close(ownerRelease) })
				if !runJoined {
					<-runDone
				}
				if ownerDone != nil && !ownerJoined {
					if err := <-ownerDone; err != nil {
						t.Error("held queue owner did not join", err)
					}
				}
				if nextDone != nil {
					<-nextDone
				}
			}()
			go func() {
				defer close(runDone)
				server.HandleError(worker.Run, func(err error) { runErr = err })
			}()
			taskQueueWait(ctx, failedStarted)
			taskQueueWait(ctx, postStarted)
			var siblingCtx context.Context
			select {
			case siblingCtx = <-siblingStarted:
			case <-ctx.Done():
				t.Fatal("unrelated function did not enter its actual Run slot")
			}
			before := GetTasks(ctx, ids[0])[ids[0]]
			if before == nil || before.ClaimGeneration != 1 || GetFinishedTasks(ctx, ids[2])[ids[2]] == nil {
				t.Fatal("fixture did not establish claimed failure and committed post custody")
			}
			ownerDone = make(chan error, 1)
			go func() {
				var ownerErr error
				server.HandleError(func() {
					server.OwnedTx(ctx, []server.PgOwnershipKey{blockingKey}, func(server.PgTx) {
						close(ownerReady)
						taskQueueWait(ctx, ownerRelease)
					}, server.TxReadCommitted, server.OptNoRetry())
				}, func(err error) { ownerErr = err })
				ownerDone <- ownerErr
			}()
			taskQueueWait(ctx, ownerReady)
			var siblingWakeAt *time.Time
			if returnedError {
				at := server.NowUtc().Add(time.Minute).Truncate(time.Microsecond)
				siblingWakeAt = &at
				ScheduleTask(taskRunFinalizationCancellationWork, &runOnceGenerationArgs{Scope: siblingScope}, owner,
					runOnceGenerationKey(siblingScope), RunAt(at))
				if siblingCtx.Err() != nil {
					t.Fatal("a concurrent future RunOnce wake canceled the held sibling")
				}
			}
			failureOnce.Do(func() { close(failureRelease) })
			taskQueueWait(ctx, collectorReady)
			if testutil.ToFloat64(finalizationFailures) != beforeFinalizationFailures+1 {
				t.Fatal("Run finalization deadline was hidden or counted twice before collector recovery")
			}
			if !errors.Is(siblingCtx.Err(), context.Canceled) || worker.ctx.Err() != nil || worker.runCtx.Err() != nil {
				t.Fatal("failed Run collector stopped refill without canceling its held sibling")
			}
			if refused.Load() != 1 || failedAdmissions.Load() != 0 || failedCalls.Load() != 1 || failedPosts.Load() != 0 ||
				!reflect.DeepEqual(before, GetTasks(ctx, ids[0])[ids[0]]) || GetFinishedTasks(ctx, ids[0])[ids[0]] != nil {
				t.Fatal("pre-BEGIN failure changed durable custody or replayed its handback")
			}
			if next := GetTasks(ctx, ids[3])[ids[3]]; next == nil || next.ClaimGeneration != 0 || nextCalls.Load() != 0 {
				t.Fatal("failed collector refilled before its sibling and committed post joined")
			}
			probeOwners := func(wantHeld bool) {
				server.MaintenanceDb(ctx, func(conn server.PgConn) {
					for _, id := range ids[:3] {
						var acquired bool
						server.Raise(conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, taskAdvisoryLockKey(id)).Scan(&acquired))
						if acquired {
							server.RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, taskAdvisoryLockKey(id)))
						}
						if acquired == wantHeld {
							t.Fatal("collector changed execution custody before the exact join boundary", wantHeld)
						}
					}
				})
			}
			probeOwners(true)
			collectorOnce.Do(func() { close(collectorRelease) })
			siblingOnce.Do(func() { close(siblingRelease) })
			taskQueueWait(ctx, siblingHandedBack)
			sibling := GetTasks(ctx, ids[1])[ids[1]]
			if sibling == nil || sibling.RescheduleError != context.Canceled.Error() || sibling.RescheduleErrorCount != 19 ||
				GetFinishedTasks(ctx, ids[1])[ids[1]] != nil || siblingCalls.Load() != 1 || postGenerations.Load() != 0 {
				t.Fatal("collector-canceled sibling grew task-failure backoff or lost its fenced retry/post custody")
			}
			if delay := sibling.RunAt.Sub(sibling.ReleaseTime); delay < RescheduleTimeout-time.Microsecond || 2*RescheduleTimeout < delay {
				t.Fatal("collector interruption inherited the sibling's saturated task-failure delay", delay)
			}
			if siblingWakeAt != nil {
				var wake *time.Time
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT run_once_wake_at FROM pending_task WHERE task_id=$1`, ids[1]).Scan(&wake))
				})
				if sibling.RunOnceGeneration != 1 || wake == nil || !wake.Equal(*siblingWakeAt) || !sibling.RunAt.Before(*siblingWakeAt) {
					t.Fatal("interrupted retry lost the later producer wake or delayed its shorter normal retry")
				}
			}
			probeOwners(true)
			postOnce.Do(func() { close(postRelease) })
			taskQueueWait(ctx, runDone)
			runJoined = true
			if !errors.Is(runErr, context.DeadlineExceeded) || runErr.Error() != context.DeadlineExceeded.Error() ||
				postGenerations.Load() != 2 || worker.InflightCount() != 0 || reruns.Load() != 0 ||
				!reflect.DeepEqual(before, GetTasks(ctx, ids[0])[ids[0]]) || failedCalls.Load() != 1 || failedPosts.Load() != 0 {
				t.Fatal("joined failure changed its cause, replayed work, or lost detached post generations", runErr)
			}
			probeOwners(false)
			ownerOnce.Do(func() { close(ownerRelease) })
			ownerErr := <-ownerDone
			ownerJoined = true
			if ownerErr != nil {
				t.Fatal("held queue owner failed to release", ownerErr)
			}
			// The normal caller can start Run again. The original failed
			// row's persisted lease and the canceled sibling's retry remain
			// ineligible at the unchanged claim clock; due unrelated work runs.
			var nextErr error
			nextDone = make(chan struct{})
			go func() {
				defer close(nextDone)
				server.HandleError(worker.Run, func(err error) { nextErr = err })
			}()
			taskQueueWait(ctx, nextDone)
			if testutil.ToFloat64(finalizationFailures) != beforeFinalizationFailures+1 {
				t.Fatal("collector unwind or Run re-entry recounted an earlier finalization failure")
			}
			if nextErr != nil || nextCalls.Load() != 1 || GetFinishedTasks(ctx, ids[3])[ids[3]] == nil ||
				failedCalls.Load() != 1 || siblingCalls.Load() != 1 || !reflect.DeepEqual(before, GetTasks(ctx, ids[0])[ids[0]]) {
				t.Fatal("ordinary Run restart failed to admit unrelated work or replayed unacknowledged custody", nextErr)
			}
		})
	}
}
