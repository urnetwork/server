// Legacy settlement intents have independent bounded recovery owners.
package work

import (
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type FlushLegacySettlementsArgs struct {
	Shard              int                                `json:"shard"`
	Cursor             *model.LegacySettlementCursor      `json:"cursor,omitempty"`
	PayerCursor        *model.LegacySettlementPayerCursor `json:"payer_cursor,omitempty"`
	SourceCursor       *model.LegacySettlementPayerCursor `json:"source_cursor,omitempty"`
	RegistrationCursor *model.LegacySettlementOwnerCursor `json:"registration_cursor,omitempty"`
	// Consecutive dispatch passes before this one that found the partition
	// empty, capped where the dispatcher backs off.
	EmptyPasses int `json:"empty_passes,omitempty"`
}
type FlushLegacySettlementsResult struct {
	model.LegacySettlementShardResult
	Dispatch *model.LegacySettlementDispatchResult `json:"dispatch,omitempty"`
}

func scheduleFlushLegacySettlements(clientSession *session.ClientSession, tx server.PgTx, shard int, after *model.LegacySettlementCursor, payerAfter *model.LegacySettlementPayerCursor, more bool, sourceAfter *model.LegacySettlementPayerCursor, registrationAfter *model.LegacySettlementOwnerCursor) {
	scheduleFlushLegacySettlementsWithBatch(clientSession, tx, nil, shard, after, payerAfter, more, sourceAfter, registrationAfter)
}

func scheduleFlushLegacySettlementsWithBatch(clientSession *session.ClientSession, tx server.PgTx, batch server.PgBatch, shard int, after *model.LegacySettlementCursor, payerAfter *model.LegacySettlementPayerCursor, more bool, sourceAfter *model.LegacySettlementPayerCursor, registrationAfter *model.LegacySettlementOwnerCursor) {
	settings := DefaultSettlementChainSettings()
	args := &FlushLegacySettlementsArgs{Shard: shard, Cursor: after, PayerCursor: payerAfter, SourceCursor: sourceAfter, RegistrationCursor: registrationAfter}
	queueFlushLegacySettlements(clientSession, tx, batch, args, settings.nextRunAt(server.NowUtc(), more, false), settings)
}

// Publishes the partition's next run at runAt, into the caller's batch when it
// has one.
func queueFlushLegacySettlements(clientSession *session.ClientSession, tx server.PgTx, batch server.PgBatch, args *FlushLegacySettlementsArgs, runAt time.Time, settings *SettlementChainSettings) {
	options := []any{task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", args.Shard)), task.RunAt(runAt), task.MaxTime(settings.MaxTime), task.RequireQueueOwnership(tx)}
	if batch != nil {
		task.QueueTaskInBatch(tx, batch, FlushLegacySettlements, args, clientSession, options...)
	} else {
		task.ScheduleTaskInTx(tx, FlushLegacySettlements, args, clientSession, options...)
	}
}

// Whether a dispatch pass found its partition empty: it began fresh payer,
// source and registration rounds, found no owner and no intent to register
// or leave unresolved, and has nothing more to do.
func legacySettlementDispatchEmpty(args *FlushLegacySettlementsArgs, dispatch *model.LegacySettlementDispatchResult) bool {
	return args.PayerCursor == nil && args.SourceCursor == nil && args.RegistrationCursor == nil &&
		!dispatch.More && dispatch.Probes == 0 && dispatch.SourceProbes == 0 &&
		len(dispatch.PayerNetworkIds) == 0 && len(dispatch.SourceClientIds) == 0 &&
		dispatch.Registered == 0 && dispatch.RegistrationUnresolved == 0 && !dispatch.RegistrationFailed
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
		dispatch := result.Dispatch
		settings := DefaultSettlementChainSettings()
		// An empty partition backs off. Its remaining producers wake it at once
		// through the RunOnce merge (scheduled close handoff, expiry recovery).
		emptyPasses := settings.nextEmptyPasses(args.EmptyPasses, legacySettlementDispatchEmpty(args, dispatch))
		runAt := settings.nextRunAt(server.NowUtc(), dispatch.More, settings.backedOff(emptyPasses))
		next := &FlushLegacySettlementsArgs{Shard: args.Shard, Cursor: dispatch.Cursor, PayerCursor: dispatch.PayerCursor,
			SourceCursor: dispatch.SourceCursor, RegistrationCursor: dispatch.RegistrationCursor, EmptyPasses: emptyPasses}
		// All admitted queue keys remain owned until this transaction commits.
		// Pipeline the bounded fan-out instead of paying one exchange per key
		// while blocking every other publisher of any member of that set.
		server.BatchInTx(clientSession.Ctx, tx, func(batch server.PgBatch) {
			for _, payerNetworkId := range dispatch.PayerNetworkIds {
				model.QueueLegacyCloseSettlementsInBatch(clientSession, tx, batch, model.ContractCloseOwner{Kind: model.ContractCloseOwnerPayerNetwork, Id: payerNetworkId})
			}
			for _, sourceClientId := range dispatch.SourceClientIds {
				model.QueueLegacyCloseSettlementsInBatch(clientSession, tx, batch, model.ContractCloseOwner{Kind: model.ContractCloseOwnerSourceClient, Id: sourceClientId})
			}
			queueFlushLegacySettlements(clientSession, tx, batch, next, runAt, settings)
		})
		return nil
	}
	// A persisted RunPost from the earlier financial target still owns its
	// original continuation. The next invocation enters dispatch above.
	scheduleFlushLegacySettlements(clientSession, tx, args.Shard, result.Cursor, result.PayerCursor, result.More && result.Failed == 0 && result.Completed > 0, nil, nil)
	return nil
}
