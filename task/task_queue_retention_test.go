// Retention is another finished-row writer. Synthetic aged error records model
// the exact rows still eligible for a durable Post retry; no financial effect
// or completion proof is invented by this queue-policy fixture.
package task

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func seedTaskQueueRetention(t testing.TB, ctx context.Context) ([]server.Id, time.Time, time.Time) {
	t.Helper()
	ids := make([]server.Id, taskCompletionBatchLimit+1)
	for index := range ids {
		ids[index] = server.NewId()
	}
	slices.SortFunc(ids, server.Id.Cmp)
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO finished_task
            (task_id,function_name,args_json,run_at,run_priority,run_max_time_seconds,
             run_start_time,run_end_time,result_json,post_error,post_completed)
            SELECT task_id,'synthetic_aged_post_owner','{}',$2,0,30,$2,$2,'{}','synthetic post failure',false
            FROM unnest($1::uuid[]) AS observed(task_id)`, ids, now.Add(-8*24*time.Hour)))
	}, server.TxReadCommitted, server.OptNoRetry())
	return ids, now.Add(-24 * time.Hour), now.Add(-7 * 24 * time.Hour)
}

// Holding only the finished key must exclude deletion before a Post retry
// takes its row. A refused first group cannot prevent the next group sweeping.
func TestTaskQueueRetentionDefersLiveFinishedOwnerBeforeRow(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		ids, minTime, postErrorMinTime := seedTaskQueueRetention(t, ctx)
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		refused := 0
		sweepCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused {
				refused++
			}
		})
		server.OwnedTx(ctx, []server.PgOwnershipKey{taskFinishedOwnershipKey(ids[0])}, func(tx server.PgTx) {
			removed := RemoveFinishedTasks(sweepCtx, minTime, postErrorMinTime)
			if removed != 1 || refused != 1 {
				t.Fatalf("retention deleted under a live finished owner or stopped later progress: removed=%d refused=%d", removed, refused)
			}
			var observed server.Id
			server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM finished_task WHERE task_id=$1 FOR UPDATE NOWAIT`, ids[0]).Scan(&observed))
			if observed != ids[0] || len(GetFinishedTasks(ctx, ids...)) != taskCompletionBatchLimit {
				t.Fatal("refused retention changed live finished custody")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if removed := RemoveFinishedTasks(ctx, minTime, postErrorMinTime); removed != taskCompletionBatchLimit ||
			len(GetFinishedTasks(ctx, ids...)) != 0 || reruns.Load() != 0 {
			t.Fatalf("released finished ownership did not sweep without retry: removed=%d reruns=%d", removed, reruns.Load())
		}
	})
}

// An older nonparticipating row owner also cannot block the retention query.
// Only its exact row remains; the other current candidates finish in this call.
func TestTaskQueueRetentionSkipsHeldRowWithoutBlockingPeers(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		ids, minTime, postErrorMinTime := seedTaskQueueRetention(t, ctx)
		var reruns atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		server.Tx(ctx, func(tx server.PgTx) {
			var observed server.Id
			server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM finished_task WHERE task_id=$1 FOR UPDATE`, ids[0]).Scan(&observed))
			if removed := RemoveFinishedTasks(ctx, minTime, postErrorMinTime); removed != taskCompletionBatchLimit {
				t.Fatalf("held finished row prevented independent retention progress: removed=%d", removed)
			}
			remaining := GetFinishedTasks(ctx, ids...)
			if len(remaining) != 1 || remaining[ids[0]] == nil {
				t.Fatal("retention did not preserve only the held finished identity")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if removed := RemoveFinishedTasks(ctx, minTime, postErrorMinTime); removed != 1 ||
			len(GetFinishedTasks(ctx, ids...)) != 0 || reruns.Load() != 0 {
			t.Fatalf("released finished row did not sweep without retry: removed=%d reruns=%d", removed, reruns.Load())
		}
	})
}
