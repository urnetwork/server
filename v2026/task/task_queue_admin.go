// Administrative mutations use the same durable queue identity as publishers
// and workers. Their unlocked discovery is revalidated under the owned backend;
// a completed/deleted original id is never redirected to its successor.
package task

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

func withPendingTaskQueueOwner(ctx context.Context, taskId server.Id, mutate func(server.PgTx, *string)) {
	var key *string
	found := false
	server.Tx(ctx, func(tx server.PgTx) {
		err := tx.QueryRow(ctx, `SELECT run_once_key FROM pending_task WHERE task_id=$1`, taskId).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		found = true
	}, server.TxReadCommitted, server.OptNoRetry())
	if !found {
		return
	}
	server.OwnedTx(ctx, []server.PgOwnershipKey{PendingTaskOwnershipKey(taskId, key)}, func(tx server.PgTx) {
		mutate(tx, key)
	}, server.TxReadCommitted, server.OptNoRetry())
}
