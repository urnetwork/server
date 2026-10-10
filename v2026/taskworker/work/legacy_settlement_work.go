// Legacy settlement intents have independent bounded recovery owners.
package work

import (
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type FlushLegacySettlementsArgs struct {
	Shard              int                                `json:"shard"`
	Cursor             *model.LegacySettlementCursor      `json:"cursor,omitempty"`
	PayerCursor        *model.LegacySettlementPayerCursor `json:"payer_cursor,omitempty"`
	SourceCursor       *model.LegacySettlementPayerCursor `json:"source_cursor,omitempty"`
	RegistrationCursor *model.LegacySettlementOwnerCursor `json:"registration_cursor,omitempty"`
}
type FlushLegacySettlementsResult struct {
	model.LegacySettlementShardResult
	Dispatch *model.LegacySettlementDispatchResult `json:"dispatch,omitempty"`
}

func scheduleFlushLegacySettlements(clientSession *session.ClientSession, tx server.PgTx, shard int, after *model.LegacySettlementCursor, payerAfter *model.LegacySettlementPayerCursor, more bool, sourceAfter *model.LegacySettlementPayerCursor, registrationAfter *model.LegacySettlementOwnerCursor) {
	scheduleFlushLegacySettlementsWithBatch(clientSession, tx, nil, shard, after, payerAfter, more, sourceAfter, registrationAfter)
}

func scheduleFlushLegacySettlementsWithBatch(clientSession *session.ClientSession, tx server.PgTx, batch server.PgBatch, shard int, after *model.LegacySettlementCursor, payerAfter *model.LegacySettlementPayerCursor, more bool, sourceAfter *model.LegacySettlementPayerCursor, registrationAfter *model.LegacySettlementOwnerCursor) {
	next := server.NowUtc().Add(2 * time.Second)
	if more {
		next = server.NowUtc()
	}
	args := &FlushLegacySettlementsArgs{Shard: shard, Cursor: after, PayerCursor: payerAfter, SourceCursor: sourceAfter, RegistrationCursor: registrationAfter}
	options := []any{task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard)), task.RunAt(next), task.MaxTime(30 * time.Second), task.RequireQueueOwnership(tx)}
	if batch != nil {
		task.QueueTaskInBatch(tx, batch, FlushLegacySettlements, args, clientSession, options...)
	} else {
		task.ScheduleTaskInTx(tx, FlushLegacySettlements, args, clientSession, options...)
	}
}
func ScheduleFlushLegacySettlements(clientSession *session.ClientSession, tx server.PgTx) {
	requireSettlementStartupOwnershipInTx(clientSession.Ctx, tx, legacySettlementStartupOwnershipKeys())
	for shard := range model.LegacySettlementShardCount {
		scheduleFlushLegacySettlements(clientSession, tx, shard, nil, nil, false, nil, nil)
	}
}
func FlushLegacySettlements(args *FlushLegacySettlementsArgs, clientSession *session.ClientSession) (*FlushLegacySettlementsResult, error) {
	result, err := model.DispatchLegacySettlementCloseOwners(clientSession.Ctx, args.Shard, args.Cursor, args.PayerCursor, args.SourceCursor, args.RegistrationCursor)
	return &FlushLegacySettlementsResult{Dispatch: &result}, err
}
func FlushLegacySettlementsPost(args *FlushLegacySettlementsArgs, result *FlushLegacySettlementsResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if result.Dispatch != nil {
		// All admitted queue keys remain owned until this transaction commits.
		// Pipeline the bounded fan-out instead of paying one exchange per key
		// while blocking every other publisher of any member of that set.
		server.BatchInTx(clientSession.Ctx, tx, func(batch server.PgBatch) {
			for _, payerNetworkId := range result.Dispatch.PayerNetworkIds {
				model.QueueLegacyCloseSettlementsInBatch(clientSession, tx, batch, model.ContractCloseOwner{Kind: model.ContractCloseOwnerPayerNetwork, Id: payerNetworkId})
			}
			for _, sourceClientId := range result.Dispatch.SourceClientIds {
				model.QueueLegacyCloseSettlementsInBatch(clientSession, tx, batch, model.ContractCloseOwner{Kind: model.ContractCloseOwnerSourceClient, Id: sourceClientId})
			}
			scheduleFlushLegacySettlementsWithBatch(clientSession, tx, batch, args.Shard, result.Dispatch.Cursor,
				result.Dispatch.PayerCursor, result.Dispatch.More, result.Dispatch.SourceCursor, result.Dispatch.RegistrationCursor)
		})
		return nil
	}
	// A persisted RunPost from the earlier financial target still owns its
	// original continuation. The next invocation enters dispatch above.
	scheduleFlushLegacySettlements(clientSession, tx, args.Shard, result.Cursor, result.PayerCursor, result.More && result.Failed == 0 && result.Completed > 0, nil, nil)
	return nil
}
