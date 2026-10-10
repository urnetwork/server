// A saturated payer settlement limit keeps the claim scan bounded.
package taskworker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Due payer turns queued beside the held ones.
const payerScanDue = 64

// Holds the first payer turns until release so the limit stays saturated.
type payerScanHeldTarget struct {
	task.Target
	held    atomic.Int64
	started chan struct{}
	release chan struct{}
}

// Delegates the completion declaration to the real payer target.
func (self *payerScanHeldTarget) TaskCompletionOwnershipKeys(queued *task.Task, result string) ([]server.PgOwnershipKey, error) {
	return self.Target.(task.TaskCompletionOwnershipTarget).TaskCompletionOwnershipKeys(queued, result)
}

// Blocks the held turns before the real body runs.
func (self *payerScanHeldTarget) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	if self.held.Add(1) <= legacyPayerSettlementClaimLimit {
		self.started <- struct{}{}
		<-self.release
	}
	return self.Target.Run(ctx, queued)
}

// Queues one due turn for each of count synthetic payers through the real
// producer, under each turn's queue ownership, one discovery round at a time.
func queuePayerScanTurns(ctx context.Context, owner *session.ClientSession, count int, runAt time.Time) {
	for count > 0 {
		payerIds := make([]server.Id, min(count, 16))
		for index := range payerIds {
			payerIds[index] = server.NewId()
		}
		count -= len(payerIds)
		keys, err := model.LegacyPayerSettlementQueueOwnershipKeys(payerIds)
		server.Raise(err)
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			for _, payerId := range payerIds {
				model.ScheduleLegacyPayerSettlementsInTx(owner, tx, payerId, nil, runAt)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
	}
}

// Saturates the payer limit with production placement using the oldest payer
// turns, queues the waiting payer turns at waitingAt, then records the
// candidate scan of the next claim and returns the most rows it filtered.
func payerScanNextClaimRemoved(t testing.TB, ctx context.Context, owner *session.ClientSession, waitingAt time.Time) int {
	t.Helper()
	queuePayerScanTurns(ctx, owner, legacyPayerSettlementClaimLimit, time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC))
	trace := &claimCursorTrace{}
	scope, err := server.NewTestPgQueryScope(ctx, trace)
	server.Raise(err)
	defer func() { server.Raise(scope.Close()) }()
	settings := task.DefaultTaskWorkerSettings()
	settings.BatchSize = legacyPayerSettlementClaimLimit
	worker := task.NewTaskWorker(ctx, taskWorkerSettingsForProfile(settings, WorkloadProfileProduction))
	defer worker.Close()
	held := &payerScanHeldTarget{Target: model.NewLegacyPayerSettlementTaskTarget(),
		started: make(chan struct{}, legacyPayerSettlementClaimLimit), release: make(chan struct{})}
	worker.AddTargets(held, work.NewScheduledContractClosureTaskTarget())
	heldDone := make(chan struct{})
	var heldErr error
	go func() {
		defer close(heldDone)
		_, _, _, heldErr = worker.EvalTasks(legacyPayerSettlementClaimLimit)
	}()
	released := false
	defer func() {
		if !released {
			close(held.release)
		}
		<-heldDone
	}()
	for range legacyPayerSettlementClaimLimit {
		<-held.started
	}
	queuePayerScanTurns(ctx, owner, payerScanDue, waitingAt)

	trace.reset()
	if _, retried, posts, err := worker.EvalTasks(1); err != nil || len(retried)+len(posts) != 0 {
		t.Fatal("claim beside saturated payer turns failed", retried, posts, err)
	}
	queries, args := trace.snapshot()
	if len(queries) == 0 {
		t.Fatal("observed claim declared no candidate cursor")
	}
	removed := 0
	for index, query := range queries {
		removed = max(removed, claimScanRowsRemoved(t, ctx, query, args[index]))
	}
	close(held.release)
	released = true
	<-heldDone
	if heldErr != nil {
		t.Fatal("held payer turns did not finish", heldErr)
	}
	return removed
}

// Queued closes fill the oldest end of the queue, and newer due payer turns
// wait behind them. Once a worker holds its limit of payer turns, every claim
// filters payer rows from its candidate scan. That filter must cost nothing
// while an older backlog fills the claim window.
func TestProductionClaimScanStaysBoundedBesideSaturatedPayerTurns(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-payer-scan", server.NewId())
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "synthetic-source", "synthetic")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic-destination", "synthetic")
		ids := make([]server.Id, claimScanBacklog)
		for index := range ids {
			ids[index] = server.NewId()
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
				contract_id,source_network_id,source_id,destination_network_id,destination_id,
				transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
				SELECT contract_id,$2,$3,$2,$4,100,true,$5,NULL FROM unnest($1::uuid[]) AS candidate(contract_id)`,
				ids, networkId, sourceId, destinationId, server.NowUtc().Add(-3*time.Hour)))
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		if _, err := work.ScheduleOpenContractClosures(&work.ScheduleOpenContractClosuresArgs{PageSize: 1024,
			StartedAt: server.NowUtc().Truncate(time.Microsecond).Add(-2 * time.Hour)}, owner); err != nil {
			t.Fatal("startup pass did not publish the synthetic backlog", err)
		}
		removed := payerScanNextClaimRemoved(t, ctx, owner, server.NowUtc().Add(-time.Hour))
		t.Logf("claim scan filtered %d rows beside %d waiting payer turns", removed, payerScanDue)
		if window := 1 + 64; window < removed {
			t.Fatalf("claim scan filtered %d rows, more than its window of %d, beside saturated payer turns", removed, window)
		}
	})
}

// With no older work, due payer turns are the oldest rows. A saturated limit
// then filters them, but each payer has a single queued turn, so the scan
// reads at most one row per queued payer, never one per settlement intent.
func TestProductionClaimScanReadsOneRowPerQueuedPayerAtQueueHead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		queued := payerScanDue + legacyPayerSettlementClaimLimit
		removed := payerScanNextClaimRemoved(t, ctx, owner, server.NowUtc().Add(-time.Hour))
		t.Logf("claim scan filtered %d rows for %d queued payer turns", removed, queued)
		if queued < removed {
			t.Fatalf("claim scan filtered %d rows for %d queued payer turns", removed, queued)
		}
	})
}
