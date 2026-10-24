// Debit flushing is recurring recovery work, independent of request callbacks.
package work

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type FlushTransferDebitsArgs struct {
	Shard          int        `json:"shard"`
	AfterBalanceId *server.Id `json:"after_balance_id,omitempty"`
	// Consecutive passes before this one that found the partition empty,
	// capped where the chain backs off. A backed-off chain's idle mark may
	// still be set.
	EmptyPasses int `json:"empty_passes,omitempty"`
}
type FlushTransferDebitsResult struct{ model.TransferDebitFlushResult }

// Publishes the partition's next run at runAt, in the caller's transaction that
// owns the chain's queue key.
func scheduleFlushTransferDebits(clientSession *session.ClientSession, tx server.PgTx, args *FlushTransferDebitsArgs, runAt time.Time, settings *SettlementChainSettings) {
	task.ScheduleTaskInTx(tx, FlushTransferDebits, args, clientSession,
		task.RunOnce(model.TransferDebitFlushRunOnceKey(args.Shard)), task.RunAt(runAt), task.MaxTime(settings.MaxTime), task.RequireQueueOwnership(tx))
}
func ScheduleFlushTransferDebits(clientSession *session.ClientSession, tx server.PgTx) {
	requireSettlementStartupOwnershipInTx(clientSession.Ctx, tx, transferDebitStartupOwnershipKeys())
	settings := DefaultSettlementChainSettings()
	for shard := range model.TransferDebitShardCount {
		scheduleFlushTransferDebits(clientSession, tx, &FlushTransferDebitsArgs{Shard: shard}, settings.nextRunAt(server.NowUtc(), false, false), settings)
	}
}
func FlushTransferDebits(args *FlushTransferDebitsArgs, clientSession *session.ClientSession) (*FlushTransferDebitsResult, error) {
	result, err := model.FlushTransferDebits(clientSession.Ctx, args.Shard, args.AfterBalanceId, 64)
	return &FlushTransferDebitsResult{TransferDebitFlushResult: result}, err
}

// Whether the pass found its partition empty: a traversal from the start of the
// partition that met no balance key at all. A traversal that resumed after a
// cursor and met the end wraps to the start at the active delay instead.
func transferDebitFlushEmpty(args *FlushTransferDebitsArgs, result *FlushTransferDebitsResult) bool {
	return args.AfterBalanceId == nil && result.LastBalanceId == nil && result.Balances == 0 && !result.More
}

func FlushTransferDebitsPost(args *FlushTransferDebitsArgs, result *FlushTransferDebitsResult, clientSession *session.ClientSession, tx server.PgTx) error {
	settings := DefaultSettlementChainSettings()
	more := result.More && result.Failed == 0 && result.Released > 0
	emptyPasses := settings.nextEmptyPasses(args.EmptyPasses, transferDebitFlushEmpty(args, result))
	scheduleFlushTransferDebits(clientSession, tx, &FlushTransferDebitsArgs{Shard: args.Shard, AfterBalanceId: result.LastBalanceId, EmptyPasses: emptyPasses},
		settings.nextRunAt(server.NowUtc(), more, settings.backedOff(emptyPasses)), settings)
	return nil
}

// The registered post: FlushTransferDebitsPost in the finishing transaction,
// then, once that commit has published the successor, the partition's idle
// mark. A backed-off run sets the mark and looks at the journal again: a debit
// that committed after this run's traversal but before the mark found nothing
// to clear, and the second look wakes the chain for it. The first run back at
// work clears the mark, so producers do not kick an active chain.
func FlushTransferDebitsCommitPost(args *FlushTransferDebitsArgs, result *FlushTransferDebitsResult, clientSession *session.ClientSession, tx server.PgTx) ([]server.PostFunction, error) {
	if err := FlushTransferDebitsPost(args, result, clientSession, tx); err != nil {
		return nil, err
	}
	settings := DefaultSettlementChainSettings()
	// The post's session ends with the post; the mark keeps only its values.
	ctx := context.WithoutCancel(clientSession.Ctx)
	key := model.TransferDebitFlushRunOnceKey(args.Shard)
	switch {
	case settings.backedOff(settings.nextEmptyPasses(args.EmptyPasses, transferDebitFlushEmpty(args, result))):
		return []server.PostFunction{func() any {
			// Without the mark no producer kicks the chain and it keeps its
			// idle delay. A failed second look wakes the chain rather than
			// leave a debit that no producer will kick.
			if task.MarkIdleRunOnce(ctx, key, settings.Wake) != nil {
				return nil
			}
			pending := false
			server.HandleError(func() {
				bounded, cancel := context.WithTimeout(ctx, settings.Wake.Timeout)
				defer cancel()
				pending = model.TransferDebitShardPending(bounded, args.Shard)
			}, func(error) {
				pending = true
			})
			if pending {
				_, _ = task.WakeIdleRunOnce(ctx, settings.Wake, key)
			}
			return nil
		}}, nil
	case settings.backedOff(args.EmptyPasses):
		return []server.PostFunction{func() any {
			// A failed or late clear leaves at most one needless kick, or one
			// lost kick if it lands after the next back-off's mark.
			_ = task.ClearIdleRunOnce(ctx, key, settings.Wake)
			return nil
		}}, nil
	}
	return nil, nil
}
