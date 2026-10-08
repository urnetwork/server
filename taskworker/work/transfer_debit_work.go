// Debit flushing is recurring recovery work, independent of request callbacks.
package work

import (
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type FlushTransferDebitsArgs struct {
	Shard          int        `json:"shard"`
	AfterBalanceId *server.Id `json:"after_balance_id,omitempty"`
}
type FlushTransferDebitsResult struct{ model.TransferDebitFlushResult }

func scheduleFlushTransferDebits(clientSession *session.ClientSession, tx server.PgTx, shard int, after *server.Id, more bool) {
	next := server.NowUtc().Add(2 * time.Second)
	if more {
		next = server.NowUtc()
	}
	task.ScheduleTaskInTx(tx, FlushTransferDebits, &FlushTransferDebitsArgs{Shard: shard, AfterBalanceId: after}, clientSession,
		task.RunOnce(fmt.Sprintf("flush_transfer_debits_%d", shard)), task.RunAt(next), task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
}
func ScheduleFlushTransferDebits(clientSession *session.ClientSession, tx server.PgTx) {
	requireSettlementStartupOwnershipInTx(clientSession.Ctx, tx)
	for shard := range model.TransferDebitShardCount {
		scheduleFlushTransferDebits(clientSession, tx, shard, nil, false)
	}
}
func FlushTransferDebits(args *FlushTransferDebitsArgs, clientSession *session.ClientSession) (*FlushTransferDebitsResult, error) {
	result, err := model.FlushTransferDebits(clientSession.Ctx, args.Shard, args.AfterBalanceId, 64)
	return &FlushTransferDebitsResult{TransferDebitFlushResult: result}, err
}
func FlushTransferDebitsPost(args *FlushTransferDebitsArgs, result *FlushTransferDebitsResult, clientSession *session.ClientSession, tx server.PgTx) error {
	scheduleFlushTransferDebits(clientSession, tx, args.Shard, result.LastBalanceId, result.More && result.Failed == 0 && result.Released > 0)
	return nil
}
