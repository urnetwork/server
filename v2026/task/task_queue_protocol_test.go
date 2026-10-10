// Real owner and transaction barriers distinguish admission from row locking.
// These controls use synthetic queue identities and never infer exclusion from
// a short absence of work or a sampled PostgreSQL wait state.
package task

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Keep the real ordinary/batched target behavior while declaring this fixture's
// queue ownership. The declaration runs before the handback transaction.
type taskQueueProtocolTarget struct {
	*runOnceGenerationTarget
	extra []server.PgOwnershipKey
}

func (self *taskQueueProtocolTarget) TaskCompletionOwnershipKeys(_ *Task, _ string) ([]server.PgOwnershipKey, error) {
	return self.extra, nil
}

func taskQueueWait(ctx context.Context, signal <-chan struct{}) {
	select {
	case <-signal:
	case <-ctx.Done():
		panic(ctx.Err())
	}
}

func taskQueueError(ctx context.Context, result <-chan error) error {
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The current queue owner intentionally has not locked its row. A refused
// claimer must let that owner take the row immediately, while progressing a
// separate due task and retaining its ordinary advisory execution ownership.
func TestTaskClaimQueueAdmissionPrecedesPendingRow(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		firstId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-2*time.Hour)))
		secondId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner,
			RunAt(server.NowUtc().Add(-time.Hour)))
		before := GetTasks(ctx, firstId)[firstId]
		entered, probe, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var probeOnce, releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		probeResult, ownerDone := make(chan error, 1), make(chan error, 1)
		go func() {
			var ownerErr error
			server.HandleError(func() {
				server.OwnedTx(ctx, []server.PgOwnershipKey{RunOnceOwnershipKey(runOnceGenerationKey(scope))}, func(tx server.PgTx) {
					close(entered)
					taskQueueWait(ctx, probe)
					var observed server.Id
					err := tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE task_id=$1 FOR UPDATE NOWAIT`, firstId).Scan(&observed)
					if err == nil && observed != firstId {
						err = errors.New("synthetic owner observed a different task")
					}
					probeResult <- err
					taskQueueWait(ctx, release)
					server.Raise(err)
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { ownerErr = err })
			ownerDone <- ownerErr
		}()
		taskQueueWait(ctx, entered)
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewTaskTarget(runOnceGenerationWork))
		refused := false
		var rowErr error
		worker.claimQueueAdmission = func(taskId server.Id, admitted bool) {
			if taskId == firstId {
				refused = !admitted
				probeOnce.Do(func() { close(probe) })
				rowErr = taskQueueError(ctx, probeResult)
			}
		}
		claimed, guard, claimErr := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		probeOnce.Do(func() { close(probe) })
		releaseOnce.Do(func() { close(release) })
		ownerErr := taskQueueError(ctx, ownerDone)
		if rowErr != nil {
			t.Fatalf("claimer locked pending row before queue admission: %v", rowErr)
		}
		if ownerErr != nil || claimErr != nil || !refused || len(claimed) != 1 || claimed[secondId] == nil {
			t.Fatalf("queue refusal lost independent progress: owner=%v claim=%v refused=%t count=%d", ownerErr, claimErr, refused, len(claimed))
		}
		if after := GetTasks(ctx, firstId)[firstId]; !reflect.DeepEqual(before, after) {
			t.Fatal("refused queue owner received a speculative lease write")
		}
		worker.claimQueueAdmission = nil
		next, nextGuard, err := worker.takeTasks(1)
		if nextGuard != nil {
			defer nextGuard.release()
		}
		if err != nil || len(next) != 1 || next[firstId] == nil {
			t.Fatalf("released owner did not become claimable: count=%d error=%v", len(next), err)
		}
	})
}

// A changed key is a different owner even when a stale discovery row has the
// same task id. The exact post-admission recheck must refuse that old identity.
func TestTaskClaimRevalidatesStoredQueueKey(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewTaskTarget(runOnceGenerationWork))
		newKey := RunOnce("synthetic_changed_queue_owner", scope).String()
		worker.claimCandidatesReady = func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_once_key=$2 WHERE task_id=$1`, id, newKey))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		claimed, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 0 || guard != nil {
			t.Fatalf("stale queue identity was claimed: count=%d error=%v", len(claimed), err)
		}
		row := GetTasks(ctx, id)[id]
		if row == nil || row.ClaimGeneration != 0 || row.RunOnceKey != newKey {
			t.Fatal("refused stale queue identity changed durable claim state")
		}
		worker.claimCandidatesReady = nil
		claimed, guard, err = worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 1 || claimed[id] == nil || claimed[id].RunOnceKey != newKey {
			t.Fatalf("fresh queue identity was not admitted: count=%d error=%v", len(claimed), err)
		}
	})
}

// Holding the queue key alone is enough to exclude the heartbeat's business
// write. After release, the same exact live claim refreshes without a retry;
// a stale epoch never updates the current owner's timestamp.
func TestTaskHeartbeatDefersBeforeOwnedPendingWrite(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewTaskTarget(runOnceGenerationWork))
		claimed, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 1 {
			t.Fatalf("heartbeat fixture claim failed: %v", err)
		}
		past := server.NowUtc().Add(-time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET claim_time=$2,release_time=$2 WHERE task_id=$1`, id, past))
		}, server.TxReadCommitted, server.OptNoRetry())
		before := GetTasks(ctx, id)[id]
		refused := false
		heartbeatCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused {
				refused = true
			}
		})
		server.OwnedTx(ctx, []server.PgOwnershipKey{RunOnceOwnershipKey(runOnceGenerationKey(scope))}, func(tx server.PgTx) {
			refreshTaskTimestampLeases(heartbeatCtx, claimed)
			if after := GetTasks(ctx, id)[id]; !reflect.DeepEqual(before, after) {
				t.Fatal("heartbeat wrote under a different live queue owner")
			}
			var observed server.Id
			server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE task_id=$1 FOR UPDATE NOWAIT`, id).Scan(&observed))
		}, server.TxReadCommitted, server.OptNoRetry())
		if !refused {
			t.Fatal("heartbeat did not positively refuse the held queue owner")
		}
		refreshTaskTimestampLeases(ctx, claimed)
		after := GetTasks(ctx, id)[id]
		if !after.ClaimTime.After(before.ClaimTime) || !after.ReleaseTime.After(before.ReleaseTime) {
			t.Fatal("released queue owner prevented the exact live heartbeat")
		}
		stale := *claimed[id]
		stale.ClaimGeneration--
		refreshTaskTimestampLeases(ctx, map[server.Id]*Task{id: &stale})
		if final := GetTasks(ctx, id)[id]; !reflect.DeepEqual(after, final) || reruns.Load() != 0 {
			t.Fatalf("stale heartbeat changed a new epoch or retried: reruns=%d", reruns.Load())
		}
	})
}

// A raw claim cannot silently inherit repeatable read, including an explicit
// in-transaction change. The guarded expression must acquire no queue key.
func TestTaskClaimOwnershipRefusesRepeatableReadBeforeKey(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		key := RunOnceOwnershipKey(RunOnce("synthetic_raw_claim_isolation", server.NewId()))
		conn, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer conn.Release()
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: server.TxRepeatableRead})
		server.Raise(err)
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			_ = tx.Rollback(cleanup)
		}()
		if admitted, err := server.TryPgTaskClaimOwnership(ctx, tx, key); admitted || err == nil {
			t.Fatalf("repeatable-read raw claim was admitted: acquired=%t error=%v", admitted, err)
		}
		entered := false
		server.OwnedTx(ctx, []server.PgOwnershipKey{key}, func(server.PgTx) { entered = true }, server.TxReadCommitted, server.OptNoRetry())
		if !entered {
			t.Fatal("refused raw claim retained a hidden queue key")
		}
	})
}

// A nonparticipating old row owner cannot stall other crash hints. Both rows
// are exact live claims, and the held native row remains locked until after
// the independent timestamp has advanced and the heartbeat has returned.
func TestTaskHeartbeatSkipsHeldRowAndRefreshesIndependentClaim(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ids := make([]server.Id, 2)
		for index := range ids {
			ids[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner,
				RunAt(server.NowUtc().Add(-time.Hour)))
		}
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewTaskTarget(runOnceGenerationWork))
		claimed, guard, err := worker.takeTasks(2)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 2 {
			t.Fatalf("held-row heartbeat fixture claim failed: %v", err)
		}
		past := server.NowUtc().Add(-time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET claim_time=$2,release_time=$2 WHERE task_id=ANY($1)`, ids, past))
		}, server.TxReadCommitted, server.OptNoRetry())
		holder, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer holder.Release()
		tx, err := holder.Begin(ctx)
		server.Raise(err)
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			_ = tx.Rollback(cleanup)
		}()
		var locked server.Id
		server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE task_id=$1 FOR UPDATE`, ids[0]).Scan(&locked))
		refreshTaskTimestampLeases(ctx, claimed)
		rows := GetTasks(ctx, ids...)
		if locked != ids[0] || !rows[ids[0]].ClaimTime.Equal(past) || !rows[ids[1]].ClaimTime.After(past) {
			t.Fatal("held row blocked or replaced the independent exact-epoch heartbeat")
		}
	})
}

// Failures in barrier goroutines are carried back to the joined test owner.
func taskQueueProtocolFailure(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
