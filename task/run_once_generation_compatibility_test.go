// Additive schema compatibility does not grant old binaries wake semantics.
package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The old insert/upsert shape is intentionally retained only in this control.
// It remains executable after migration but cannot mark an after-claim wake.
func TestRunOnceOldWriterRemainsSchemaCompatibleWithoutWakeGuarantee(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, scopes, results := runOnceGenerationClaimResults(t, ctx, false)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		prepared := prepareTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[0]}, owner,
			runOnceGenerationKey(scopes[0]), RunAt(server.NowUtc().Add(-time.Hour)))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, queuedTaskInsertSql+` ON CONFLICT (run_once_key) DO UPDATE SET
			 run_at=LEAST(pending_task.run_at,$7),run_priority=LEAST(pending_task.run_priority,$9),
			 run_max_time_seconds=GREATEST(pending_task.run_max_time_seconds,$10)`,
				prepared.taskId, prepared.functionName, prepared.argsJson, prepared.clientAddressHash,
				prepared.clientAddressPort, prepared.byJwtJson, prepared.runAt, prepared.runOnceKey,
				prepared.priority, prepared.maxTimeSeconds, time.Time{}))
		})
		worker.finalizeTask(results[0])
		if len(GetFinishedTasks(ctx, ids...)) != 1 || len(runOnceGenerationPending(ctx, scopes)) != 0 {
			t.Fatal("old-writer compatibility control changed; reassess the explicit rollout limitation")
		}
	})
}

// Old finishers still ignore both generations. This passing limitation control
// forbids attributing the all-new guarantee to a mixed fleet merely from DDL.
func TestRunOnceOldFinisherIgnoresNewWakeUntilRetired(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, scopes, results := runOnceGenerationClaimResults(t, ctx, false)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[0]}, owner,
			runOnceGenerationKey(scopes[0]), RunAt(server.NowUtc().Add(-time.Hour)))
		if row := GetTasks(ctx, ids[0])[ids[0]]; row == nil || row.RunOnceGeneration != 1 {
			t.Fatal("new producer did not establish wake custody")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
				batch.Queue(`INSERT INTO finished_task (
				 task_id,function_name,args_json,client_address,client_address_hash,
				 client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
				 run_max_time_seconds,run_start_time,run_end_time,reschedule_error,result_json)
				 SELECT task_id,function_name,args_json,client_address,client_address_hash,
				 client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
				 run_max_time_seconds,$2,$3,reschedule_error,$4 FROM pending_task WHERE task_id=$1`,
					ids[0], results[0].runStartTime, results[0].runEndTime, results[0].resultJson)
				batch.Queue(`DELETE FROM pending_task WHERE task_id=$1`, ids[0])
			})
		})
		if len(GetFinishedTasks(ctx, ids...)) != 1 || len(runOnceGenerationPending(ctx, scopes)) != 0 {
			t.Fatal("old-finisher limitation changed; reassess the required owner retirement")
		}
	})
}

// IfAbsent protects immutable allocations. Refusing a duplicate must not turn
// it into coalesced work under the default RunOnce scheduling handshake.
func TestRunOnceIfAbsentDoesNotRequestRerun(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, scopes, results := runOnceGenerationClaimResults(t, ctx, false)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			inserted, _ := ScheduleTaskInTxIfAbsent(tx, runOnceGenerationWork,
				&runOnceGenerationArgs{Scope: scopes[0], Cursor: 99}, owner, runOnceGenerationKey(scopes[0]))
			if inserted {
				t.Fatal("immutable allocation accepted a duplicate while claimed")
			}
		})
		worker.finalizeTask(results[0])
		if len(GetFinishedTasks(ctx, ids...)) != 1 || len(runOnceGenerationPending(ctx, scopes)) != 0 {
			t.Fatal("refused immutable duplicate became automatic work")
		}
	})
}

// A returned Post error retains both the established RunPost owner and the
// independent wake; no external post runs before their shared commit.
func TestRunOncePostErrorRetainsWakeAndPostRetry(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		target.before = func(context.Context, *Task) error {
			ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
				runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
			return nil
		}
		target.after = func(server.PgTx, *Task) error { return errors.New("synthetic run once post retry") }
		worker := runOnceGenerationWorker(ctx, target)
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		pending := runOnceGenerationPending(ctx, []server.Id{scope})
		if err != nil || len(finished)+len(retried) != 0 || len(posts) != 1 || posts[0] != id || len(pending) != 1 || pending[id] != nil || len(GetFinishedTasks(ctx, id)) != 1 {
			t.Fatal("Post retry and dirty wake did not retain independent custody", finished, retried, posts, err)
		}
		var retryOwners int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1 AND args_json::jsonb->>'task_id'=$2`,
				functionName(worker.RunPost), id.String()).Scan(&retryOwners))
		})
		if retryOwners != 1 {
			t.Fatal("Post failure lost its durable retry owner", retryOwners)
		}
	})
}
