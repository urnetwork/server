// Admission must safely release stale candidates without losing healthy work.
package task

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A nonparticipating writer can hold a row while both advisory keys remain
// free. Prove that the exact recheck skips it, releases its execution owner,
// and claims it after the row owner commits. Channels establish the race.
func TestTaskClaimRecheckSkipsLockedRowAndRecovers(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		locked := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{}, owner, RunAt(past))
		independent := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{}, owner, RunAt(past.Add(time.Hour)))
		before := GetTasks(ctx, locked)[locked]
		entered, release := make(chan struct{}), make(chan struct{})
		ownerDone := make(chan error, 1)
		var releaseOnce sync.Once
		go func() {
			var err error
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					var id server.Id
					server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE task_id=$1 FOR UPDATE`, locked).Scan(&id))
					close(entered)
					taskQueueWait(ctx, release)
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(cause error) { err = cause })
			ownerDone <- err
		}()
		joined := false
		defer func() {
			releaseOnce.Do(func() { close(release) })
			if !joined {
				<-ownerDone
			}
		}()
		taskQueueWait(ctx, entered)
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewTaskTarget(runOnceGenerationWork))
		admitted := false
		worker.claimQueueAdmission = func(id server.Id, ok bool) {
			if id == locked {
				admitted = ok
			}
		}
		claimed, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || !admitted || len(claimed) != 1 || claimed[independent] == nil {
			t.Fatal("row-lock refusal lost independent progress", admitted, len(claimed), err)
		}
		if !reflect.DeepEqual(before, GetTasks(ctx, locked)[locked]) || guard.taskIds[locked] {
			t.Fatal("refused recheck changed the lease or retained the execution owner")
		}
		releaseOnce.Do(func() { close(release) })
		err = taskQueueError(ctx, ownerDone)
		joined = true
		if err != nil {
			t.Fatal(err)
		}
		claimed, nextGuard, err := worker.takeTasks(1)
		if nextGuard != nil {
			defer nextGuard.release()
		}
		if err != nil || len(claimed) != 1 || claimed[locked] == nil {
			t.Fatal("released row stayed unclaimable", len(claimed), err)
		}
	})
}

// Discovery is a cursor snapshot. A deletion or future lease committed at its
// barrier must not be undone by the exact recheck or prevent a later due row.
func TestTaskClaimRecheckPreservesDeletionAndFutureLease(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			stale := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{}, owner, RunAt(past))
			healthy := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{}, owner, RunAt(past.Add(time.Hour)))
			worker := NewTaskWorkerWithDefaults(ctx)
			defer worker.Close()
			worker.AddTargets(NewTaskTarget(runOnceGenerationWork))
			var changed *Task
			worker.claimCandidatesReady = func() {
				server.Tx(ctx, func(tx server.PgTx) {
					if deleted {
						server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE task_id=$1`, stale))
					} else {
						server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET release_time='2099-01-01' WHERE task_id=$1`, stale))
					}
				}, server.TxReadCommitted, server.OptNoRetry())
				changed = GetTasks(ctx, stale)[stale]
			}
			claimed, guard, err := worker.takeTasks(1)
			if guard != nil {
				defer guard.release()
			}
			if err != nil || len(claimed) != 1 || claimed[healthy] == nil {
				t.Fatal("stale discovery hid a healthy task", deleted, len(claimed), err)
			}
			if !reflect.DeepEqual(changed, GetTasks(ctx, stale)[stale]) || guard.taskIds[stale] {
				t.Fatal("stale recheck revived or re-leased a changed task", deleted)
			}
		})
	}
}
