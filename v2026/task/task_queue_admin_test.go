// Operator writes participate in the same queue owner before changing a live
// row. Each operation preserves its existing exact-id or exact-key semantics.
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

func runTaskQueueAdminOwnership(t *testing.T, operation string) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		runOnce := runOnceGenerationKey(scope)
		future := server.NowUtc().Add(time.Hour)
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner, runOnce, RunAt(future))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET claim_time=$2,release_time=$2 WHERE task_id=$1`, id, future))
		}, server.TxReadCommitted, server.OptNoRetry())
		entered, waiting, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var waitingOnce, releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		ownerDone := make(chan error, 1)
		go func() {
			var resultErr error
			server.HandleError(func() {
				server.OwnedTx(ctx, []server.PgOwnershipKey{RunOnceOwnershipKey(runOnce)}, func(server.PgTx) {
					close(entered)
					taskQueueWait(ctx, release)
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { resultErr = err })
			ownerDone <- resultErr
		}()
		taskQueueWait(ctx, entered)
		adminCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipWaiting {
				waitingOnce.Do(func() { close(waiting) })
				taskQueueWait(ctx, release)
			}
		})
		adminDone := make(chan error, 1)
		go func() {
			var resultErr error
			server.HandleError(func() {
				switch operation {
				case "remove":
					RemovePendingTask(adminCtx, id)
				case "release":
					if !ReleaseTask(adminCtx, id) {
						resultErr = errors.New("owned task was not released")
					}
				case "kick":
					if KickTasks(adminCtx, runOnce.String()) != 1 {
						resultErr = errors.New("owned run-once task was not kicked exactly once")
					}
				default:
					resultErr = errors.New("invalid synthetic administrative operation")
				}
			}, func(err error) { resultErr = err })
			adminDone <- resultErr
		}()
		taskQueueWait(ctx, waiting)
		before := GetTasks(ctx, id)[id]
		if before == nil || !before.RunAt.Equal(future) || !before.ReleaseTime.Equal(future) {
			t.Fatal("administrative mutation entered before queue admission")
		}
		releaseOnce.Do(func() { close(release) })
		ownerErr, adminErr := taskQueueError(ctx, ownerDone), taskQueueError(ctx, adminDone)
		if ownerErr != nil || adminErr != nil || reruns.Load() != 0 {
			t.Fatalf("owned administrative operation failed or retried: owner=%v admin=%v reruns=%d", ownerErr, adminErr, reruns.Load())
		}
		after := GetTasks(ctx, id)[id]
		switch operation {
		case "remove":
			if after != nil {
				t.Fatal("remove retained its original pending identity")
			}
		case "release":
			if after == nil || !after.ClaimTime.IsZero() || !after.ReleaseTime.IsZero() || !after.RunAt.Equal(future) {
				t.Fatal("release changed schedule metadata or retained the old lease")
			}
		case "kick":
			if after == nil || !after.RunAt.Before(future) || !after.ReleaseTime.Equal(future) {
				t.Fatal("kick changed the claim or lost its earlier requested schedule")
			}
		}
	})
}

func TestTaskQueueRemoveUsesExactPendingOwner(t *testing.T) {
	runTaskQueueAdminOwnership(t, "remove")
}

func TestTaskQueueReleaseUsesExactPendingOwner(t *testing.T) {
	runTaskQueueAdminOwnership(t, "release")
}

func TestTaskQueueKickUsesExactStoredRunOnceOwner(t *testing.T) {
	runTaskQueueAdminOwnership(t, "kick")
}
