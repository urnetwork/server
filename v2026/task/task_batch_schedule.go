// Bounded financial owners pipeline task writes inside their existing transaction.
package task

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

const queuedTaskInsertSql = `INSERT INTO pending_task (
    task_id,function_name,args_json,client_address,client_address_hash,
    client_address_port,client_by_jwt_json,run_at,run_once_key,run_priority,
    run_max_time_seconds,claim_time,release_time
) VALUES ($1,$2,$3,'',$4,$5,$6,$7,$8,$9,$10,$11,$11)`

// Queue the existing coalescing write, including its conflict touch. The caller
// must drain the batch before committing; no owner exists before that commit.
func QueueTaskInBatch[T any, R any](tx server.PgTx, batch server.PgBatch, taskFunction TaskFunction[T, R], args T,
	clientSession *session.ClientSession, opts ...any) server.Id {
	requireTaskPublicationBackend(tx, opts)
	prepared := prepareTask(taskFunction, args, clientSession, opts...)
	batch.Queue(queuedTaskInsertSql+` ON CONFLICT (run_once_key) DO UPDATE SET
        run_at=LEAST(pending_task.run_at,$7),
        run_priority=LEAST(pending_task.run_priority,$9),
        run_max_time_seconds=GREATEST(pending_task.run_max_time_seconds,$10),
        run_once_generation=pending_task.run_once_generation+1,
        run_once_wake_at=LEAST(pending_task.run_once_wake_at,$7)
        RETURNING task_id=$1`,
		prepared.taskId, prepared.functionName, prepared.argsJson, prepared.clientAddressHash,
		prepared.clientAddressPort, prepared.byJwtJson, prepared.runAt, prepared.runOnceKey,
		prepared.priority, prepared.maxTimeSeconds, time.Time{}).QueryRow(func(row pgx.Row) error {
		var inserted bool
		if err := row.Scan(&inserted); err != nil {
			return err
		}
		observeTaskSubmissionInTx(tx, inserted)
		return nil
	})
	return prepared.taskId
}

// A duplicate immutable allocation aborts the batch's transaction. Unlike a
// coalescing task, it cannot silently discard a second producer's arguments.
func QueueRequiredTaskInBatch[T any, R any](tx server.PgTx, batch server.PgBatch, taskFunction TaskFunction[T, R], args T,
	clientSession *session.ClientSession, runOnce *RunOnceOption, opts ...any) server.Id {
	if runOnce == nil {
		panic("QueueRequiredTaskInBatch requires a run-once key")
	}
	requireTaskPublicationBackend(tx, opts)
	prepared := prepareTask(taskFunction, args, clientSession, append(opts, runOnce)...)
	batch.Queue(queuedTaskInsertSql+` ON CONFLICT (run_once_key) DO NOTHING`,
		prepared.taskId, prepared.functionName, prepared.argsJson, prepared.clientAddressHash,
		prepared.clientAddressPort, prepared.byJwtJson, prepared.runAt, prepared.runOnceKey,
		prepared.priority, prepared.maxTimeSeconds, time.Time{}).Exec(func(tag pgconn.CommandTag) error {
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("required task already exists: %s", prepared.functionName)
		}
		observeTaskSubmissionInTx(tx, true)
		return nil
	})
	return prepared.taskId
}
