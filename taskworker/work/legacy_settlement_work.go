// Legacy settlement intents have independent bounded recovery owners.
package work

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type FlushLegacySettlementsArgs struct {
	Shard       int                                `json:"shard"`
	Cursor      *model.LegacySettlementCursor      `json:"cursor,omitempty"`
	PayerCursor *model.LegacySettlementPayerCursor `json:"payer_cursor,omitempty"`
}
type FlushLegacySettlementsResult struct {
	model.LegacySettlementShardResult
	Dispatch       *model.LegacySettlementDispatchResult      `json:"dispatch,omitempty"`
	IndexReadiness *model.LegacySettlementPayerIndexReadiness `json:"index_readiness,omitempty"`
}

func scheduleFlushLegacySettlements(clientSession *session.ClientSession, tx server.PgTx, shard int, after *model.LegacySettlementCursor, payerAfter *model.LegacySettlementPayerCursor, more bool) {
	next := server.NowUtc().Add(2 * time.Second)
	if more {
		next = server.NowUtc()
	}
	task.ScheduleTaskInTx(tx, FlushLegacySettlements, &FlushLegacySettlementsArgs{Shard: shard, Cursor: after, PayerCursor: payerAfter}, clientSession,
		task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard)), task.RunAt(next), task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
}
func ScheduleFlushLegacySettlements(clientSession *session.ClientSession, tx server.PgTx) {
	requireSettlementStartupOwnershipInTx(clientSession.Ctx, tx, legacySettlementStartupOwnershipKeys())
	for shard := range model.LegacySettlementShardCount {
		scheduleFlushLegacySettlements(clientSession, tx, shard, nil, nil, false)
	}
}
func FlushLegacySettlements(args *FlushLegacySettlementsArgs, clientSession *session.ClientSession) (*FlushLegacySettlementsResult, error) {
	return flushLegacySettlements(args, clientSession.Ctx,
		model.DispatchLegacySettlementPayersWithReadiness, model.FlushLegacySettlementShard)
}

func flushLegacySettlements(args *FlushLegacySettlementsArgs, ctx context.Context,
	dispatch func(context.Context, int, *model.LegacySettlementCursor, *model.LegacySettlementPayerCursor) (model.LegacySettlementDispatchResult, *model.LegacySettlementPayerIndexReadiness, error),
	legacy func(context.Context, int, *model.LegacySettlementCursor, *model.LegacySettlementPayerCursor, int) (model.LegacySettlementShardResult, error),
) (*FlushLegacySettlementsResult, error) {
	result, readiness, err := dispatch(ctx, args.Shard, args.Cursor, args.PayerCursor)
	if errors.Is(err, model.ErrLegacySettlementPayerIndexUnavailable) {
		// Index/schema visibility is a scheduling prerequisite. Preserve the
		// original guarded service during migration or a transient read error.
		legacyResult, legacyErr := legacy(ctx, args.Shard, args.Cursor, args.PayerCursor, model.LegacySettlementPageLimit)
		return &FlushLegacySettlementsResult{LegacySettlementShardResult: legacyResult, IndexReadiness: readiness}, legacyErr
	}
	return &FlushLegacySettlementsResult{Dispatch: &result, IndexReadiness: readiness}, err
}
func FlushLegacySettlementsPost(args *FlushLegacySettlementsArgs, result *FlushLegacySettlementsResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if result.Dispatch != nil {
		for _, payerNetworkId := range result.Dispatch.PayerNetworkIds {
			model.QueueLegacyPayerSettlementsInTx(clientSession, tx, payerNetworkId)
		}
		scheduleFlushLegacySettlements(clientSession, tx, args.Shard, result.Dispatch.Cursor,
			result.Dispatch.PayerCursor, result.Dispatch.More)
		return nil
	}
	// A persisted RunPost from the earlier financial target still owns its
	// original continuation. The next invocation enters dispatch above.
	scheduleFlushLegacySettlements(clientSession, tx, args.Shard, result.Cursor, result.PayerCursor, result.More && result.Failed == 0 && result.Completed > 0)
	return nil
}
