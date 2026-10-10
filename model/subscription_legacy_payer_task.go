// A single RunOnce key owns each payer's bounded financial turns. Independent
// per-contract intents survive every dispatch, task or optional wake failure.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type LegacyPayerSettlementArgs struct {
	Private        bool                    `json:"_private_task_arguments"`
	PayerNetworkId server.Id               `json:"payer_network_id"`
	Owner          *ContractCloseOwner     `json:"owner,omitempty"`
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
	QueueLegacyCloseSettlementsInTx(clientSession, tx, ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: payerNetworkId})
}

// The identity namespace is retained in task arguments and the RunOnce key.
func QueueLegacyCloseSettlementsInTx(clientSession *session.ClientSession, tx server.PgTx, owner ContractCloseOwner) {
	ScheduleLegacyCloseSettlementsInTx(clientSession, tx, owner, nil,
		server.NowUtc().Add(legacyPayerSettlementCollectionWindow(clientSession.Ctx)))
}

// Queue the same owner wake into the caller's transaction-owned batch. The
// caller must drain that batch before commit; publication keeps its ordinary
// collection window and RunOnce conflict semantics.
func QueueLegacyCloseSettlementsInBatch(clientSession *session.ClientSession, tx server.PgTx, batch server.PgBatch, owner ContractCloseOwner) {
	scheduleLegacyCloseSettlements(clientSession, tx, batch, owner, nil,
		server.NowUtc().Add(legacyPayerSettlementCollectionWindow(clientSession.Ctx)))
}

// Dispatchers coalesce these keys outside close and financial transactions.
// A conflict keeps the existing turn's cursor; its own Post owns continuation.
func ScheduleLegacyPayerSettlementsInTx(clientSession *session.ClientSession, tx server.PgTx,
	payerNetworkId server.Id, cursor *LegacySettlementCursor, next time.Time) {
	ScheduleLegacyCloseSettlementsInTx(clientSession, tx, ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: payerNetworkId}, cursor, next)
}

func ScheduleLegacyCloseSettlementsInTx(clientSession *session.ClientSession, tx server.PgTx,
	owner ContractCloseOwner, cursor *LegacySettlementCursor, next time.Time) {
	scheduleLegacyCloseSettlements(clientSession, tx, nil, owner, cursor, next)
}

func scheduleLegacyCloseSettlements(clientSession *session.ClientSession, tx server.PgTx, batch server.PgBatch,
	owner ContractCloseOwner, cursor *LegacySettlementCursor, next time.Time) {
	if !owner.valid() {
		server.Raise(fmt.Errorf("invalid legacy close task scope"))
	}
	args := &LegacyPayerSettlementArgs{Private: true, Owner: &owner, Cursor: cursor}
	if owner.Kind == ContractCloseOwnerPayerNetwork {
		args.PayerNetworkId = owner.Id
	}
	apply := ApplyLegacyPayerSettlements
	if owner.Kind == ContractCloseOwnerSourceClient {
		apply = ApplyLegacySourceSettlements
	}
	options := []any{owner.runOnce(), task.RunAt(next), task.MaxTime(30 * time.Second), task.RequireQueueOwnership(tx)}
	if batch != nil {
		task.QueueTaskInBatch(tx, batch, apply, args, clientSession, options...)
	} else {
		task.ScheduleTaskInTx(tx, apply, args, clientSession, options...)
	}
}

// Financial execution happens before the task's completion transaction takes
// any pending-row lock. The existing task claim guard serializes new owners;
// row-level financial guards remain valid against rolling older shard workers.
func ApplyLegacyPayerSettlements(args *LegacyPayerSettlementArgs,
	clientSession *session.ClientSession) (*LegacyPayerSettlementResult, error) {
	owner, err := legacyCloseTaskOwner(args)
	if err != nil {
		return nil, err
	}
	result, err := runLegacyCloseSettlementPages(clientSession.Ctx, owner,
		args.Cursor, LegacySettlementPageLimit, true)
	return &result, err
}

// Deletion of the current pending row precedes this Post. A bounded head read
// rediscovers predecessors at EOF and preserves future accounting cooldowns.
// New work racing this read is recovered by RunOnce generation or the durable
// dispatcher. No grant, history census or Redis operation enters this handoff.
func ApplyLegacyPayerSettlementsPost(args *LegacyPayerSettlementArgs,
	result *LegacyPayerSettlementResult, clientSession *session.ClientSession, tx server.PgTx) error {
	owner, err := legacyCloseTaskOwner(args)
	if err != nil || result == nil {
		return fmt.Errorf("invalid legacy close task completion")
	}
	next, err := nextLegacyCloseOwnerAttemptInTx(clientSession.Ctx, tx, owner)
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
	ScheduleLegacyCloseSettlementsInTx(clientSession, tx, owner, nextCursor, runAt)
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

// Old queued payer arguments keep their original identity. New arguments must
// never claim both a source owner and a financial payer alias.
func legacyCloseTaskOwner(args *LegacyPayerSettlementArgs) (ContractCloseOwner, error) {
	if args == nil || !args.Private {
		return ContractCloseOwner{}, fmt.Errorf("invalid legacy close task scope")
	}
	owner := ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: args.PayerNetworkId}
	if args.Owner != nil {
		owner = *args.Owner
		if args.PayerNetworkId != (server.Id{}) && (owner.Kind != ContractCloseOwnerPayerNetwork || owner.Id != args.PayerNetworkId) {
			return ContractCloseOwner{}, fmt.Errorf("conflicting legacy close task scope")
		}
	}
	if !owner.valid() {
		return ContractCloseOwner{}, fmt.Errorf("invalid legacy close task scope")
	}
	return owner, nil
}

// A distinct function name keeps a source task out of the older payer decoder.
// Source publishing starts only after workers register this target.
func ApplyLegacySourceSettlements(args *LegacyPayerSettlementArgs, clientSession *session.ClientSession) (*LegacyPayerSettlementResult, error) {
	owner, err := legacyCloseTaskOwner(args)
	if err != nil || owner.Kind != ContractCloseOwnerSourceClient {
		return nil, fmt.Errorf("invalid legacy source task scope")
	}
	return ApplyLegacyPayerSettlements(args, clientSession)
}

func ApplyLegacySourceSettlementsPost(args *LegacyPayerSettlementArgs, result *LegacyPayerSettlementResult,
	clientSession *session.ClientSession, tx server.PgTx) error {
	owner, err := legacyCloseTaskOwner(args)
	if err != nil || owner.Kind != ContractCloseOwnerSourceClient {
		return fmt.Errorf("invalid legacy source task completion")
	}
	return ApplyLegacyPayerSettlementsPost(args, result, clientSession, tx)
}

func NewLegacySourceSettlementTaskTarget() task.Target {
	return &legacyPayerSettlementTaskTarget{Target: task.NewTaskTargetWithPost(ApplyLegacySourceSettlements, ApplyLegacySourceSettlementsPost)}
}
