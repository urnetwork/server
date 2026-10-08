// A single RunOnce key owns each payer's bounded financial turns. Independent
// per-contract intents survive every dispatch, task or optional wake failure.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type LegacyPayerSettlementArgs struct {
	Private        bool                    `json:"_private_task_arguments"`
	PayerNetworkId server.Id               `json:"payer_network_id"`
	Cursor         *LegacySettlementCursor `json:"cursor,omitempty"`
}

type LegacyPayerSettlementResult struct {
	LegacySettlementFlushResult
	Pages int `json:"pages"`
}

type legacyPayerSettlementCollectionWindowKey struct{}

// Tests retain an actual collection wait without changing process globals or
// the production policy. Carry this context through scheduling and worker Run.
func Testing_WithLegacyPayerSettlementCollectionWindow(ctx context.Context) context.Context {
	return context.WithValue(ctx, legacyPayerSettlementCollectionWindowKey{}, true)
}

func legacyPayerSettlementCollectionWindow(ctx context.Context) time.Duration {
	if testing, _ := ctx.Value(legacyPayerSettlementCollectionWindowKey{}).(bool); testing {
		return 5 * time.Second
	}
	return 30 * time.Second
}

// Initial discovery collects a bounded burst. RunOnce keeps the earliest
// requested deadline, so another discovery never restarts this window.
func QueueLegacyPayerSettlementsInTx(clientSession *session.ClientSession, tx server.PgTx, payerNetworkId server.Id) {
	ScheduleLegacyPayerSettlementsInTx(clientSession, tx, payerNetworkId, nil,
		server.NowUtc().Add(legacyPayerSettlementCollectionWindow(clientSession.Ctx)))
}

// Dispatchers coalesce these keys outside close and financial transactions.
// A conflict keeps the existing turn's cursor; its own Post owns continuation.
func ScheduleLegacyPayerSettlementsInTx(clientSession *session.ClientSession, tx server.PgTx,
	payerNetworkId server.Id, cursor *LegacySettlementCursor, next time.Time) {
	task.ScheduleTaskInTx(tx, ApplyLegacyPayerSettlements,
		&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: payerNetworkId, Cursor: cursor},
		clientSession, task.RunOnce("flush_legacy_payer_settlements", payerNetworkId),
		task.RunAt(next), task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
}

// Financial execution happens before the task's completion transaction takes
// any pending-row lock. The existing task claim guard serializes new owners;
// row-level financial guards remain valid against rolling older shard workers.
func ApplyLegacyPayerSettlements(args *LegacyPayerSettlementArgs,
	clientSession *session.ClientSession) (*LegacyPayerSettlementResult, error) {
	if args == nil || !args.Private || args.PayerNetworkId == (server.Id{}) {
		return nil, fmt.Errorf("invalid legacy payer task scope")
	}
	result, err := runLegacyPayerSettlementPages(clientSession.Ctx, args.PayerNetworkId,
		args.Cursor, LegacySettlementPageLimit, true)
	return &result, err
}

// Deletion of the current pending row precedes this Post. A bounded head read
// rediscovers predecessors at EOF and preserves future accounting cooldowns.
// New work racing this read is recovered by RunOnce generation or the durable
// dispatcher. No grant, history census or Redis operation enters this handoff.
func ApplyLegacyPayerSettlementsPost(args *LegacyPayerSettlementArgs,
	result *LegacyPayerSettlementResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if args == nil || !args.Private || args.PayerNetworkId == (server.Id{}) || result == nil {
		return fmt.Errorf("invalid legacy payer task completion")
	}
	next, err := NextLegacySettlementPayerAttemptInTx(clientSession.Ctx, tx, args.PayerNetworkId)
	if err != nil || next == nil {
		return err
	}
	now := server.NowUtc()
	runAt := maxTime(*next, now.Add(2*time.Second))
	nextCursor := result.Cursor
	if result.PassEndTime.IsZero() || next.After(result.PassEndTime) {
		// Work arriving after this fixed pass belongs to a new collection
		// window. A running owner's earlier wake remains RunOnce authority.
		runAt = maxTime(*next, now.Add(legacyPayerSettlementCollectionWindow(clientSession.Ctx)))
		nextCursor = nil
	} else if result.More && result.Failed == 0 && result.Completed > 0 {
		runAt = maxTime(*next, now)
	}
	ScheduleLegacyPayerSettlementsInTx(clientSession, tx, args.PayerNetworkId, nextCursor, runAt)
	return nil
}

func maxTime(first, second time.Time) time.Time {
	if first.After(second) {
		return first
	}
	return second
}

// The registered production target is also the owner used by native drain tests.
func NewLegacyPayerSettlementTaskTarget() task.Target {
	return &legacyPayerSettlementTaskTarget{Target: task.NewTaskTargetWithPost(ApplyLegacyPayerSettlements, ApplyLegacyPayerSettlementsPost)}
}
