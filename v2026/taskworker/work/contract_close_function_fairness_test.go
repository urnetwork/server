// Terminal closure and recurring work must progress through an older backlog.
package work

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

const closeFunctionFairnessCount = 288
const closeFunctionFairnessPrefix = 144

// Public constructors retain actual Redis reservation tokens and payer grants.
// The accepted intent is the same explicit retained-state seam used by the
// qualified scheduled-close/publication controls; no balance or escrow is edited.
type closeFunctionFairnessFixture struct {
	ids          []server.Id
	paid         []server.Id
	free         []server.Id
	payers       []server.Id
	balances     []server.Id
	sourceOwners []server.Id
	provider     server.Id
	bulkAt       time.Time
}

func seedCloseFunctionFairness(t testing.TB, ctx context.Context) closeFunctionFairnessFixture {
	t.Helper()
	f := closeFunctionFairnessFixture{provider: server.NewId(), bulkAt: server.NowUtc().Truncate(time.Second).Add(-5 * time.Minute)}
	destination := server.NewId()
	model.Testing_CreateNetwork(ctx, f.provider, "synthetic-function-fairness-provider", server.NewId())
	model.Testing_CreateDevice(ctx, f.provider, server.NewId(), destination, "synthetic-provider", "synthetic")
	networks := make([]server.Id, 72)
	sources := make([]server.Id, 72)
	for index := range networks {
		networks[index], sources[index] = server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networks[index], fmt.Sprintf("synthetic-function-fairness-owner-%d", index), server.NewId())
		model.Testing_CreateDevice(ctx, networks[index], server.NewId(), sources[index], "synthetic-source", "synthetic")
		if index < 64 {
			server.Raise(model.AddBasicTransferBalance(ctx, networks[index], 10000, server.NowUtc(), server.NowUtc().Add(time.Hour)))
			balances := model.GetActiveTransferBalances(ctx, networks[index])
			if len(balances) != 1 {
				t.Fatal("public paid fixture did not retain exactly one grant", index)
			}
			f.payers = append(f.payers, networks[index])
			f.balances = append(f.balances, balances[0].BalanceId)
		} else {
			f.sourceOwners = append(f.sourceOwners, sources[index])
		}
	}
	// Interleave four contracts for each owner, keeping 64 overlapping payer
	// domains and eight genuine no-escrow source domains in one old queue block.
	for index := range closeFunctionFairnessCount {
		owner := index % len(networks)
		var id server.Id
		var err error
		if owner < 64 {
			id, _, err = model.CreateContract(ctx, networks[owner], sources[owner], f.provider, destination, 100)
			f.paid = append(f.paid, id)
		} else {
			id, err = model.CreateContractNoEscrow(ctx, networks[owner], sources[owner], f.provider, destination, 100)
			f.free = append(f.free, id)
		}
		server.Raise(err)
		server.Raise(model.CloseContract(ctx, id, sources[owner], 17, true))
		f.ids = append(f.ids, id)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=ANY($1)`, f.ids, f.bulkAt))
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, id := range f.ids {
				batch.Queue(`INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
                    VALUES($1,$2,'settled',$3)`, id, int(id[15])%model.LegacySettlementShardCount, f.bulkAt)
			}
		})
	}, server.TxReadCommitted, server.OptNoRetry())
	server.Db(ctx, func(conn server.PgConn) {
		var exact bool
		server.Raise(conn.QueryRow(ctx, `SELECT
            (SELECT count(*)=288 AND bool_and(outcome IS NULL) FROM transfer_contract WHERE contract_id=ANY($1)) AND
            (SELECT count(*)=256 AND bool_and(redis_reserved AND NOT settled AND balance_byte_count=100) FROM transfer_escrow WHERE contract_id=ANY($1)) AND
            NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($2)) AND
            (SELECT count(*)=288 AND count(*) FILTER(WHERE payer_network_id IS NOT NULL)=256
                AND count(DISTINCT payer_network_id)=64
                AND count(DISTINCT source_client_id) FILTER(WHERE payer_network_id IS NULL)=8
                FROM legacy_settlement_intent WHERE contract_id=ANY($1)) AND
            NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1))`, f.ids, f.free).Scan(&exact))
		if !exact {
			t.Fatal("fixture bypassed public reservation or admitted an already-closed contract")
		}
	})
	for _, balance := range f.balances {
		if model.Testing_NetEscrowByteCount(ctx, balance) != 400 {
			t.Fatal("public paid reservation total is not four actual 100-byte contracts")
		}
	}
	return f
}

// These six bodies are admission canaries only. Their real canonical names,
// priority and max-time metadata cover the mixed isolated/ordinary queue lanes;
// their business rollup/location computations are deliberately outside this test.
type closeFunctionFairnessCanary struct {
	task.Target
	queue func(*session.ClientSession, server.PgTx, time.Time)
}

func (self *closeFunctionFairnessCanary) TaskCompletionOwnershipKeys(_ *task.Task, _ string) ([]server.PgOwnershipKey, error) {
	return nil, nil
}
func (self *closeFunctionFairnessCanary) Run(ctx context.Context, _ *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	return struct{}{}, func(tx server.PgTx) ([]server.PostFunction, error) {
		postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), task.DefaultTaskFinalizeTimeout)
		defer cancel()
		owner := session.NewLocalClientSession(postCtx, "", nil)
		defer owner.Cancel()
		self.queue(owner, tx, server.NowUtc().Add(time.Hour))
		return nil, nil
	}, nil
}
func (self *closeFunctionFairnessCanary) RunPost(ctx context.Context, _ *task.FinishedTask, tx server.PgTx) ([]server.PostFunction, error) {
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	self.queue(owner, tx, server.NowUtc().Add(time.Hour))
	return nil, nil
}

func closeFunctionFairnessCanaries() []*closeFunctionFairnessCanary {
	return []*closeFunctionFairnessCanary{
		{Target: task.NewTaskTarget(UpdateClientLocations), queue: func(s *session.ClientSession, tx server.PgTx, at time.Time) {
			task.ScheduleTaskInTx(tx, UpdateClientLocations, &UpdateClientLocationsArgs{}, s, task.RunOnce("update_client_locations"), task.RunAt(at), task.Priority(task.TaskPriorityFastest), task.MaxTime(2*time.Minute), task.RequireQueueOwnership(tx))
		}},
		{Target: task.NewTaskTarget(UpdateClientScores), queue: func(s *session.ClientSession, tx server.PgTx, at time.Time) {
			task.ScheduleTaskInTx(tx, UpdateClientScores, &UpdateClientScoresArgs{}, s, task.RunOnce("update_client_scores"), task.RunAt(at), task.Priority(task.TaskPriorityFastest), task.MaxTime(120*time.Minute), task.RequireQueueOwnership(tx))
		}},
		{Target: task.NewTaskTarget(RollupClientReliabilityStats), queue: func(s *session.ClientSession, tx server.PgTx, at time.Time) {
			task.ScheduleTaskInTx(tx, RollupClientReliabilityStats, &RollupClientReliabilityStatsArgs{}, s, task.RunOnce("rollup_client_reliability_stats"), task.RunAt(at), task.Priority(task.TaskPriorityFastest), task.MaxTime(15*time.Minute), task.RequireQueueOwnership(tx))
		}},
		{Target: task.NewTaskTarget(UpdateReliabilities), queue: func(s *session.ClientSession, tx server.PgTx, at time.Time) {
			task.ScheduleTaskInTx(tx, UpdateReliabilities, &UpdateReliabilitiesArgs{MinTime: at.Add(-time.Hour)}, s, task.RunOnce("update_reliabilities"), task.RunAt(at), task.Priority(task.TaskPriorityFastest), task.MaxTime(120*time.Minute), task.RequireQueueOwnership(tx))
		}},
		{Target: task.NewTaskTarget(RollupSearchProviderStats), queue: func(s *session.ClientSession, tx server.PgTx, at time.Time) {
			task.ScheduleTaskInTx(tx, RollupSearchProviderStats, &RollupSearchProviderStatsArgs{}, s, task.RunOnce("rollup_search_provider_stats"), task.RunAt(at), task.MaxTime(15*time.Minute), task.RequireQueueOwnership(tx))
		}},
		{Target: task.NewTaskTarget(IndexSearchLocations), queue: func(s *session.ClientSession, tx server.PgTx, at time.Time) {
			task.ScheduleTaskInTx(tx, IndexSearchLocations, &IndexSearchLocationsArgs{}, s, task.RunOnce("index_search_locations"), task.RunAt(at), task.MaxTime(4*time.Hour), task.RequireQueueOwnership(tx))
		}},
	}
}

// The wrapper delegates the actual close function, result, Post and complete
// ownership declaration, then verifies the terminal state in that transaction.
// A one-slot Run makes the committed-prefix barrier independent of CPU speed.
type closeFunctionFairnessTarget struct {
	task.Target
	ctx            context.Context
	prefix         chan struct{}
	release        chan struct{}
	committed      atomic.Int64
	verifiedClosed atomic.Bool
}

func (self *closeFunctionFairnessTarget) TaskCompletionOwnershipKeys(queued *task.Task, result string) ([]server.PgOwnershipKey, error) {
	return self.Target.(task.TaskCompletionOwnershipTarget).TaskCompletionOwnershipKeys(queued, result)
}
func (self *closeFunctionFairnessTarget) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	result, original, err := self.Target.Run(ctx, queued)
	if err != nil {
		return result, original, err
	}
	closed, ok := result.(*CloseScheduledContractResult)
	if !ok || closed.Owner != nil || closed.Reconciliation == nil {
		return nil, nil, fmt.Errorf("real close returned an unexpected result")
	}
	return result, func(tx server.PgTx) ([]server.PostFunction, error) {
		posts, err := original(tx)
		if err != nil {
			return nil, err
		}
		var terminal bool
		if err := tx.QueryRow(self.ctx, `SELECT outcome IS NOT NULL AND NOT EXISTS(
			SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
			FROM transfer_contract WHERE contract_id=$1`, closed.Reconciliation.ContractId).Scan(&terminal); err != nil {
			return nil, err
		}
		if !terminal {
			return nil, fmt.Errorf("successful task body left its contract open")
		}
		self.verifiedClosed.Store(true)
		posts = append(posts, func() any {
			if self.committed.Add(1) == closeFunctionFairnessPrefix {
				close(self.prefix)
				select {
				case <-self.release:
				case <-self.ctx.Done():
				}
			}
			return nil
		})
		return posts, nil
	}, nil
}

// Record actual driver-reported transaction/lock refusal classes on both
// ordinary and maintenance fixture pools. This is not a sampled fleet wait claim.
type closeFunctionFairnessTrace struct{ contention atomic.Int64 }

func (self *closeFunctionFairnessTrace) inspect(err error) {
	var failure *pgconn.PgError
	if errors.As(err, &failure) {
		switch failure.Code {
		case "40001", "40P01", "55P03":
			self.contention.Add(1)
		}
	}
}
func (self *closeFunctionFairnessTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (self *closeFunctionFairnessTrace) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	self.inspect(data.Err)
}
func (self *closeFunctionFairnessTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}
func (self *closeFunctionFairnessTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	self.inspect(data.Err)
}
func (self *closeFunctionFairnessTrace) TraceBatchEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	self.inspect(data.Err)
}

type closeFunctionFairnessState struct {
	Children        int
	PendingChildren int
	Paid            int
	Free            int
	Intents         int
	Unsettled       int
	Journals        int
	ProviderPending int
	MirrorPending   int
	Canaries        int
	Errors          int
}

func readCloseFunctionFairness(ctx context.Context, f closeFunctionFairnessFixture, canaries []string) closeFunctionFairnessState {
	var s closeFunctionFairnessState
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
            (SELECT count(DISTINCT args_json::jsonb->>'contract_id') FROM finished_task WHERE function_name=$4 AND post_completed),
            (SELECT count(*) FROM pending_task WHERE function_name=$4),
            (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
            (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($2) AND outcome='settled'),
            (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($3)),
            (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND NOT settled),
            (SELECT count(*) FROM transfer_debit_journal WHERE contract_id=ANY($1)),
            (SELECT count(*) FROM pending_task WHERE function_name=$5),
            (SELECT count(*) FROM pending_task WHERE function_name=$7),
            (SELECT count(DISTINCT function_name) FROM finished_task WHERE function_name=ANY($6) AND post_completed),
            (SELECT count(*) FROM finished_task WHERE COALESCE(reschedule_error,'')<>'' OR COALESCE(post_error,'')<>'')+
            (SELECT count(*) FROM pending_task WHERE reschedule_error_count<>0)`, f.paid, f.free, f.ids,
			NewScheduledContractClosureTaskTarget().TargetFunctionName(), model.NewLegacyProviderTotalsTaskTarget().TargetFunctionName(), canaries, model.NewLegacyNetEscrowMirrorTaskTarget().TargetFunctionName()).Scan(
			&s.Children, &s.PendingChildren, &s.Paid, &s.Free, &s.Intents, &s.Unsettled, &s.Journals, &s.ProviderPending, &s.MirrorPending, &s.Canaries, &s.Errors))
	})
	return s
}

// This checks the same conservation boundaries as the public-close and native
// debit controls: actual grant debit, exact sweep, metadata, reports, provider
// projection, residual journal and both Redis reservation domains.
func requireCloseFunctionFairnessConservation(t testing.TB, ctx context.Context, f closeFunctionFairnessFixture) []byte {
	t.Helper()
	var snapshot []byte
	server.Db(ctx, func(conn server.PgConn) {
		var exact bool
		server.Raise(conn.QueryRow(ctx, `SELECT
            (SELECT count(*)=288 AND bool_and(outcome='settled' AND provider_usage IS NOT NULL) FROM transfer_contract WHERE contract_id=ANY($1)) AND
            (SELECT count(*)=288 AND bool_and(checkpoint AND party='source' AND used_transfer_byte_count=17) FROM contract_close WHERE contract_id=ANY($1)) AND
            (SELECT count(*)=256 AND bool_and(settled AND redis_reserved AND payout_byte_count=17 AND balance_byte_count-payout_byte_count=83) FROM transfer_escrow WHERE contract_id=ANY($2)) AND
            (SELECT count(*)=256 AND sum(payout_byte_count)=4352 AND sum(payout_net_revenue_nano_cents)=0 FROM transfer_escrow_sweep WHERE contract_id=ANY($2) AND network_id=$4) AND
            NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($3)) AND
            NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($3)) AND
            NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1)) AND
            NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1)) AND
            (SELECT count(*)=64 AND bool_and(start_balance_byte_count=10000 AND balance_byte_count=9932) FROM transfer_balance WHERE balance_id=ANY($5)) AND
            (SELECT provided_byte_count=4352 AND provided_net_revenue_nano_cents=0 FROM account_balance WHERE network_id=$4)`,
			f.ids, f.paid, f.free, f.provider, f.balances).Scan(&exact))
		if !exact {
			t.Fatal("completed task traffic did not conserve all real payer/provider obligations")
		}
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_array(
            (SELECT jsonb_agg(to_jsonb(c) ORDER BY contract_id) FROM transfer_contract c WHERE contract_id=ANY($1)),
            (SELECT jsonb_agg(to_jsonb(e) ORDER BY contract_id,balance_id) FROM transfer_escrow e WHERE contract_id=ANY($1)),
            (SELECT jsonb_agg(to_jsonb(s) ORDER BY contract_id,balance_id,network_id) FROM transfer_escrow_sweep s WHERE contract_id=ANY($1)),
            (SELECT jsonb_agg(to_jsonb(b) ORDER BY balance_id) FROM transfer_balance b WHERE balance_id=ANY($2)),
            (SELECT to_jsonb(a) FROM account_balance a WHERE network_id=$3))`, f.ids, f.balances, f.provider).Scan(&snapshot))
	})
	for index, balance := range f.balances {
		if got := model.Testing_NetEscrowByteCount(ctx, balance); got != 0 {
			t.Fatal("finished debit retained or duplicated a Redis reservation", index, got)
		}
		if got := model.GetActiveTransferBalanceByteCount(ctx, f.payers[index]); got != 9932 {
			t.Fatal("payer available credit does not match the exact durable debit", index, got)
		}
	}
	return snapshot
}

func TestCloseScheduledBacklogKeepsFinancialAndRecurringProgress(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 3*time.Minute)
		defer cancel()
		f := seedCloseFunctionFairness(t, ctx)
		var reruns, admissionWaits, admissionRefusals, uncertainCleanup atomic.Int64
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		ctx = server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			switch event.Kind {
			case server.PgOwnershipWaiting:
				admissionWaits.Add(1)
			case server.PgOwnershipRefused:
				admissionRefusals.Add(1)
			case server.PgOwnershipUncertain:
				uncertainCleanup.Add(1)
			}
		})
		trace := &closeFunctionFairnessTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer func() { server.Raise(scope.Close()) }()
		s := session.NewLocalClientSession(ctx, "", nil)
		defer s.Cancel()
		_, err = ScheduleOpenContractClosures(&ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: f.bulkAt}, s)
		server.Raise(err)
		canaries := closeFunctionFairnessCanaries()
		keys := transferDebitStartupOwnershipKeys()
		names := make([]string, 0, len(canaries))
		canaryKeys := []string{"update_client_locations", "update_client_scores", "rollup_client_reliability_stats", "update_reliabilities", "rollup_search_provider_stats", "index_search_locations"}
		for _, key := range canaryKeys {
			keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce(key)))
		}
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			for shard := range model.TransferDebitShardCount {
				task.ScheduleTaskInTx(tx, FlushTransferDebits, &FlushTransferDebitsArgs{Shard: shard}, s,
					task.RunOnce(fmt.Sprintf("flush_transfer_debits_%d", shard)), task.RunAt(f.bulkAt.Add(30*time.Second)), task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
			}
			for _, canary := range canaries {
				names = append(names, canary.TargetFunctionName())
				canary.queue(s, tx, f.bulkAt.Add(35*time.Second))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT count(*)=288 AND bool_and(run_at=$2) FROM pending_task WHERE function_name=$1) AND
                (SELECT count(*)=6 AND bool_and(run_at=$4) FROM pending_task WHERE function_name=ANY($3)) AND
                NOT EXISTS(SELECT 1 FROM pending_task WHERE function_name=ANY($5))`,
				NewScheduledContractClosureTaskTarget().TargetFunctionName(), f.bulkAt, names, f.bulkAt.Add(35*time.Second),
				[]string{model.NewLegacyPayerSettlementTaskTarget().TargetFunctionName(), model.NewLegacySourceSettlementTaskTarget().TargetFunctionName()}).Scan(&exact))
			if !exact {
				t.Fatal("fixture prequeued financial owners or lost the exact old-block/later-canary ordering")
			}
		})
		closeTarget := &closeFunctionFairnessTarget{Target: NewScheduledContractClosureTaskTarget(), ctx: ctx, prefix: make(chan struct{}), release: make(chan struct{})}
		settings := task.DefaultTaskWorkerSettings()
		settings.BatchSize = 1
		settings.ClaimRegisteredTargetsOnly = true
		settings.FairClaimFunctions = true
		settings.PollTimeout = 20 * time.Millisecond
		worker := task.NewTaskWorker(ctx, settings)
		worker.AddTargets(closeTarget, NewLegacySettlementDispatcherTaskTarget(), model.NewLegacyPayerSettlementTaskTarget(), model.NewLegacySourceSettlementTaskTarget(), NewTransferDebitTaskTarget(), model.NewLegacyProviderTotalsTaskTarget(), model.NewLegacyNetEscrowMirrorTaskTarget())
		for _, canary := range canaries {
			worker.AddTargets(canary)
		}
		done := make(chan struct{})
		var runErr error
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(closeTarget.release) }) }
		defer func() { release(); worker.Close(); cancel(); <-done }()
		go func() { defer close(done); server.HandleError(worker.Run, func(err error) { runErr = err }) }()
		select {
		case <-closeTarget.prefix:
		case <-done:
			t.Fatal("actual worker exited before committed-prefix witness", runErr)
		case <-ctx.Done():
			t.Fatal("actual worker did not reach the committed-prefix witness", ctx.Err())
		}
		prefix := readCloseFunctionFairness(ctx, f, names)
		t.Logf("close_function_fairness_committed_prefix=%+v", prefix)
		if !closeTarget.verifiedClosed.Load() || prefix.Children != closeFunctionFairnessPrefix || prefix.Children >= closeFunctionFairnessCount || prefix.Paid == 0 || prefix.Free == 0 || prefix.Canaries != 6 || prefix.Errors != 0 {
			t.Fatal("successful old-block closes still starve real paid/source outcomes or later recurring functions", prefix)
		}
		// A repeat startup pass must not republish a successfully closed child.
		// Remaining open contracts keep their existing exact queue identities.
		var repeatedContract server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT contract_id FROM transfer_contract c
                WHERE contract_id=ANY($1) AND outcome IS NOT NULL
                AND EXISTS(SELECT 1 FROM finished_task f WHERE function_name=$2
                    AND (f.args_json::jsonb->>'contract_id')::uuid=c.contract_id AND post_completed)
                ORDER BY contract_id LIMIT 1`, f.ids, closeTarget.TargetFunctionName()).Scan(&repeatedContract))
		})
		_, err = ScheduleOpenContractClosures(&ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: f.bulkAt}, s)
		server.Raise(err)
		server.Db(ctx, func(conn server.PgConn) {
			var repeated bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1
				FROM pending_task WHERE run_once_key=$1)`, task.RunOnce("close_scheduled_contract", repeatedContract).String()).Scan(&repeated))
			if repeated {
				t.Fatal("repeat startup scan requeued an acknowledged terminal contract")
			}
		})
		release()
		// This observer waits only for positive complete durable state. The
		// causal fairness assertion above is fixed by a committed task count,
		// never by polling latency or a short negative timeout.
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			current := readCloseFunctionFairness(ctx, f, names)
			if current.Errors != 0 {
				t.Fatal("real financial worker refused or retried the healthy fixture", current)
			}
			if current.Children == 288 && current.Paid == 256 && current.Free == 32 && current.PendingChildren+current.Intents+current.Unsettled+current.Journals+current.ProviderPending+current.MirrorPending == 0 {
				break
			}
			select {
			case <-ticker.C:
			case <-done:
				t.Fatal("actual Run exited before complete financial drain", current, runErr)
			case <-ctx.Done():
				t.Fatal("financial drain did not reach durable completion", current, ctx.Err())
			}
		}
		worker.Drain()
		<-done
		if runErr != nil || worker.InflightCount() != 0 || worker.DrainCanceledCount() != 0 {
			t.Fatal("completed financial fixture did not join its actual worker", runErr)
		}
		before := requireCloseFunctionFairnessConservation(t, ctx, f)
		replayIds := []server.Id{f.paid[0], f.free[0]}
		replayKeys := make([]server.PgOwnershipKey, 0, 2)
		for _, id := range replayIds {
			replayKeys = append(replayKeys, task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", id)))
		}
		server.OwnedTx(ctx, replayKeys, func(tx server.PgTx) {
			for _, id := range replayIds {
				scheduleContractClose(s, tx, &CloseScheduledContractArgs{Private: true, ScheduledContractClose: ScheduledContractClose{ContractId: id, Deadline: f.bulkAt}})
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		replay := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer replay.Close()
		finished, retried, posts, err := replay.EvalTasks(2)
		if err != nil || len(finished) != 2 || len(retried)+len(posts) != 0 {
			t.Fatal("terminal public close replay lost its normal owner", finished, retried, posts, err)
		}
		after := requireCloseFunctionFairnessConservation(t, ctx, f)
		if !bytes.Equal(before, after) {
			t.Fatal("terminal task replay repeated financial writes")
		}
		if reruns.Load() != 0 || admissionWaits.Load() != 0 || admissionRefusals.Load() != 0 || uncertainCleanup.Load() != 0 || trace.contention.Load() != 0 {
			t.Fatal("single-owner fixture introduced transaction retries or observed PostgreSQL contention", reruns.Load(), admissionWaits.Load(), admissionRefusals.Load(), uncertainCleanup.Load(), trace.contention.Load())
		}
	})
}
