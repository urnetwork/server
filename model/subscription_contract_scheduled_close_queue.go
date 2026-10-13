// A legacy contract's queued scheduled close carries its deadline authority
// until the contract closes. Every closer removes it in the closing
// transaction: a queued close that outlives its contract is only claimed,
// read as terminal and moved into finished_task, and on Main such rows made
// up most of the scheduled close queue and most of its claims.
//
// Removal never waits and never takes another owner's row. A row claimed by a
// live worker carries a future release time and finishes through its own
// completion; a row another transaction holds, such as a claim in progress or
// a startup publication, is skipped and finishes on its claim as a terminal
// no-op. Removal takes no queue admission: it skips every locked or leased
// row, so it can neither wait on nor reorder the queue's admitted writers.
package model

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// The startup pass publishes at most one queued close per open legacy
// contract under this run-once identity.
func ScheduledContractCloseRunOnce(contractId server.Id) *task.RunOnceOption {
	return task.RunOnce("close_scheduled_contract", contractId)
}

// The unique run-once index finds each row. Locked rows are skipped, and the
// release time excludes a live claim's lease.
const removeScheduledContractClosesSql = `DELETE FROM pending_task
	WHERE ctid=ANY(ARRAY(SELECT ctid FROM pending_task
		WHERE run_once_key=ANY($1::text[]) AND release_time<=$2
		FOR UPDATE SKIP LOCKED))
	AND run_once_key=ANY($1::text[])`

// The stored run-once keys of the contracts' queued closes.
func scheduledContractCloseKeys(contractIds []server.Id) []string {
	keys := make([]string, 0, len(contractIds))
	for _, contractId := range contractIds {
		keys = append(keys, ScheduledContractCloseRunOnce(contractId).String())
	}
	return keys
}

// Removes the queued scheduled closes of contracts this transaction closed.
// Returns the rows removed.
func removeScheduledContractClosesInTx(ctx context.Context, tx server.PgTx, contractIds []server.Id) (int64, error) {
	if len(contractIds) == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, removeScheduledContractClosesSql, scheduledContractCloseKeys(contractIds), server.NowUtc())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// The batch form for closers that pipeline their writes. It must follow the
// statements that closed the contracts.
func queueRemoveScheduledContractCloses(batch *pgx.Batch, contractIds []server.Id) {
	if len(contractIds) == 0 {
		return
	}
	batch.Queue(removeScheduledContractClosesSql, scheduledContractCloseKeys(contractIds), server.NowUtc())
}
