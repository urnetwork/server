package work

// The service debit flush's chains (QUOTA-M2-06; model.FlushServiceDebits): one run-once chain per
// shard, as FlushTransferDebits' (transfer_debit_work.go), on the same cadence: at once while a page
// reports more, the active delay after work, and backed off to the idle delay once its shard has been
// empty, with an idle mark that an applied usage batch clears to wake it (model.ApplyUsageBatch). A
// binary that does not link MG2 neither schedules the chains nor, should an older row run, touches
// anything.

import (
	"context"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type FlushServiceDebitsArgs struct {
	Shard          int        `json:"shard"`
	AfterBalanceId *server.Id `json:"after_balance_id,omitempty"`
	// Consecutive passes before this one that found the shard empty, capped where the chain backs off.
	EmptyPasses int `json:"empty_passes,omitempty"`
}

type FlushServiceDebitsResult struct {
	model.ServiceDebitFlushResult
}

// Publishes the shard's next run at runAt, in the caller's transaction.
func scheduleFlushServiceDebits(clientSession *session.ClientSession, tx server.PgTx, args *FlushServiceDebitsArgs, runAt time.Time, settings *SettlementChainSettings) {
	if !server.SidecarMg2Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		FlushServiceDebits,
		args,
		clientSession,
		task.RunOnce(model.ServiceDebitFlushRunOnceKey(args.Shard)),
		task.RunAt(runAt),
		task.MaxTime(settings.MaxTime),
	)
}

// Schedules every shard's chain. A binary that does not link MG2 schedules nothing.
func ScheduleFlushServiceDebits(clientSession *session.ClientSession, tx server.PgTx) {
	settings := DefaultSettlementChainSettings()
	for shard := range model.ServiceDebitShardCount {
		scheduleFlushServiceDebits(clientSession, tx, &FlushServiceDebitsArgs{Shard: shard}, settings.nextRunAt(server.NowUtc(), false, false), settings)
	}
}

// One page of the shard at real time.
func FlushServiceDebits(args *FlushServiceDebitsArgs, clientSession *session.ClientSession) (*FlushServiceDebitsResult, error) {
	result, err := model.FlushServiceDebits(clientSession.Ctx, args.Shard, args.AfterBalanceId, sidecar.RealClock{})
	return &FlushServiceDebitsResult{ServiceDebitFlushResult: result}, err
}

// Whether the pass found its shard empty: a traversal from the start that met no balance at all.
func serviceDebitFlushEmpty(args *FlushServiceDebitsArgs, result *FlushServiceDebitsResult) bool {
	return args.AfterBalanceId == nil && result.LastBalanceId == nil && result.Balances == 0 && !result.More
}

func FlushServiceDebitsPost(args *FlushServiceDebitsArgs, result *FlushServiceDebitsResult, clientSession *session.ClientSession, tx server.PgTx) error {
	settings := DefaultSettlementChainSettings()
	more := result.More && result.Failed == 0 && 0 < result.Released
	emptyPasses := settings.nextEmptyPasses(args.EmptyPasses, serviceDebitFlushEmpty(args, result))
	scheduleFlushServiceDebits(
		clientSession,
		tx,
		&FlushServiceDebitsArgs{Shard: args.Shard, AfterBalanceId: result.LastBalanceId, EmptyPasses: emptyPasses},
		settings.nextRunAt(server.NowUtc(), more, settings.backedOff(emptyPasses)),
		settings,
	)
	return nil
}

// The registered post: FlushServiceDebitsPost in the finishing transaction, then, once that commit
// has published the successor, the shard's idle mark, as FlushTransferDebitsCommitPost keeps its own.
// A backed-off run sets the mark and looks at the journal again, so a batch that applied after this
// run's traversal but before the mark still wakes the chain.
func FlushServiceDebitsCommitPost(args *FlushServiceDebitsArgs, result *FlushServiceDebitsResult, clientSession *session.ClientSession, tx server.PgTx) ([]server.PostFunction, error) {
	if err := FlushServiceDebitsPost(args, result, clientSession, tx); err != nil {
		return nil, err
	}
	settings := DefaultSettlementChainSettings()
	ctx := context.WithoutCancel(clientSession.Ctx)
	key := model.ServiceDebitFlushRunOnceKey(args.Shard)
	switch {
	case settings.backedOff(settings.nextEmptyPasses(args.EmptyPasses, serviceDebitFlushEmpty(args, result))):
		return []server.PostFunction{func() any {
			if task.MarkIdleRunOnce(ctx, key, settings.Wake) != nil {
				return nil
			}
			pending := false
			server.HandleError(func() {
				bounded, cancel := context.WithTimeout(ctx, settings.Wake.Timeout)
				defer cancel()
				pending = model.ServiceDebitShardPending(bounded, args.Shard)
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
			_ = task.ClearIdleRunOnce(ctx, key, settings.Wake)
			return nil
		}}, nil
	}
	return nil, nil
}
