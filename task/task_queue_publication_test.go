// Publication assertions validate the complete already-owned set without new
// SQL admission, including queued immutable allocations and foreign backends.
package task

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestTaskQueuePublicationRequiresPredeclaredSubsetAcrossBatchForms(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scopes := make([]server.Id, 5)
		keys := make([]server.PgOwnershipKey, len(scopes))
		for index := range scopes {
			scopes[index] = server.NewId()
			keys[index] = RunOnceOwnershipKey(runOnceGenerationKey(scopes[index]))
		}
		at := server.NowUtc().Add(time.Hour)
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[0]}, owner,
				runOnceGenerationKey(scopes[0]), RunAt(at), RequireQueueOwnership(tx))
			inserted, _ := ScheduleTaskInTxIfAbsent(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[1]}, owner,
				runOnceGenerationKey(scopes[1]), RunAt(at), RequireQueueOwnership(tx))
			if !inserted {
				t.Fatal("owned immutable publication was not inserted")
			}
			server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
				QueueTaskInBatch(batch, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[2]}, owner,
					runOnceGenerationKey(scopes[2]), RunAt(at), RequireQueueOwnership(tx))
				QueueRequiredTaskInBatch(batch, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[3]}, owner,
					runOnceGenerationKey(scopes[3]), RunAt(at), RequireQueueOwnership(tx))
				QueueRequiredTasksInBatch(batch, runOnceGenerationWork, []RequiredTaskBatchItem[*runOnceGenerationArgs]{
					{Args: &runOnceGenerationArgs{Scope: scopes[4]}, RunOnce: runOnceGenerationKey(scopes[4])},
				}, owner, RunAt(at), RequireQueueOwnership(tx))
			})
		}, server.TxReadCommitted, server.OptNoRetry())
		if len(runOnceGenerationPending(ctx, scopes)) != len(scopes) {
			t.Fatal("an owned publication form lost its exact durable owner")
		}
		missing := server.NewId()
		var refused error
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			server.HandleError(func() {
				ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: missing}, owner,
					runOnceGenerationKey(missing), RequireQueueOwnership(tx))
			}, func(err error) { refused = err })
		}, server.TxReadCommitted, server.OptNoRetry())
		if refused == nil || len(runOnceGenerationPending(ctx, []server.Id{missing})) != 0 {
			t.Fatal("publication expanded a financial owner's predeclared set")
		}
	})
}

func TestTaskQueuePublicationRejectsDifferentTransactionBackend(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		var refused error
		server.OwnedTx(ctx, []server.PgOwnershipKey{RunOnceOwnershipKey(runOnceGenerationKey(scope))}, func(admitted server.PgTx) {
			server.HandleError(func() {
				server.Tx(ctx, func(other server.PgTx) {
					ScheduleTaskInTx(other, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
						runOnceGenerationKey(scope), RequireQueueOwnership(admitted))
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { refused = err })
		}, server.TxReadCommitted, server.OptNoRetry())
		if refused == nil || len(runOnceGenerationPending(ctx, []server.Id{scope})) != 0 {
			t.Fatal("an unrelated transaction borrowed a live queue owner's authority")
		}
	})
}
