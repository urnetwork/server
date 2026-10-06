// Scoped expiry leaves debit-backed projections to their existing recovery owner.
// Financial state and the clock are checked across a blocked release and lost ACK.
package model

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// A command arrival, rather than elapsed time, ends the baseline's blocked
// projection. Disabling the hook restores the same real fixture for recovery.
type redisExpiryProjectionGate struct {
	enabled atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (self *redisExpiryProjectionGate) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *redisExpiryProjectionGate) wait(ctx context.Context) error {
	select {
	case self.entered <- struct{}{}:
	default:
	}
	<-self.release
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("synthetic projection unavailable")
}

func (self *redisExpiryProjectionGate) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		// Health checks and the existing live clock increment remain available.
		// Reconciliation's actual EVAL is the blocked dependency witness.
		if self.enabled.Load() && command.Name() != "ping" && command.Name() != "incrby" {
			return self.wait(ctx)
		}
		return next(ctx, command)
	}
}

func (self *redisExpiryProjectionGate) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		if self.enabled.Load() {
			return self.wait(ctx)
		}
		return next(ctx, commands)
	}
}

// The public model must observe every selected outcome without entering debit
// reconciliation or stream cleanup. Its existing clock attempt must still run.
// The old path is canceled only after its actual projection command arrives;
// every admitted callback is then joined before inspecting durable accounting.
func repairRedisExpiryWithProjectionGate(t testing.TB, ctx context.Context, request ContractExpiryRepairRequest) ContractExpiryRepairResult {
	t.Helper()
	gate := &redisExpiryProjectionGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
	server.Redis(ctx, func(client server.RedisClient) { client.AddHook(gate) })
	installCtx, stopInstall := context.WithTimeout(ctx, time.Second)
	server.Raise(server.RedisWithDeadline(installCtx, func(client server.RedisClient) error {
		client.AddHook(gate)
		return nil
	}))
	stopInstall()
	gate.enabled.Store(true)
	caller, cancel := context.WithCancel(ctx)
	defer cancel()
	release := sync.OnceFunc(func() { close(gate.release) })
	defer release()
	defer gate.enabled.Store(false)
	type outcome struct {
		result ContractExpiryRepairResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := RepairRedisContractExpiry(caller, request)
		done <- outcome{result: result, err: err}
	}()
	var got outcome
	blocked := false
	select {
	case got = <-done:
	case <-gate.entered:
		blocked = true
		cancel()
		release()
		select {
		case got = <-done:
		case <-ctx.Done():
			t.Fatal("scoped expiry did not join its canceled projection")
		}
	case <-ctx.Done():
		cancel()
		release()
		<-done
		t.Fatal("scoped expiry did not reach an outcome or projection boundary")
	}
	if blocked {
		t.Error("debit-backed Redis projection blocked scoped terminal observation")
	}
	if got.err != nil || len(got.result.Contracts) != len(request.ContractIds) {
		t.Fatal("scoped expiry did not complete its selected financial owners")
	}
	for _, entry := range got.result.Contracts {
		if entry.Status != "terminal" || !entry.ProofCommitted || entry.ObservedOutcome == nil ||
			*entry.ObservedOutcome != ContractOutcomeSettled || entry.LegacyIntentPresent == nil || *entry.LegacyIntentPresent {
			t.Fatal("scoped expiry lost a terminal observation behind optional work")
		}
	}
	return got.result
}

// One slow projection must not spend the next contract's shared model budget.
// Omitted callbacks model a process exit after the acknowledged outcomes. The
// durable worker must repair metadata, preserve the neighbor and debit once even
// when its first Redis release succeeds but the acknowledgement is lost.
func TestRedisExpiryRepairProjectionWaitDoesNotBlockLaterContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		first := newRedisExpiryRepairTestContract(t, ctx, f, "positive_checkpoint")
		second := newRedisExpiryRepairTestContract(t, ctx, f, "source_zero")
		neighbor := createRedisAdmissionTest(ctx, f, 37)
		neighborBefore := readRedisExpiryRepairTestState(ctx, neighbor.ContractId)
		request := ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{first, second}, Apply: true}
		repairRedisExpiryWithProjectionGate(t, ctx, request)
		proof, snapshot := readContractExpiryTestSnapshot(t, ctx, first)
		if snapshot.ByteCount != 11 || snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 2 {
			t.Fatal("deferred projection changed the original usage proof")
		}
		requireRedisExpiryRepairTestCredit(t, ctx, f, 1000, 237)
		requireRedisExpiryClock(t, ctx, "11")
		requireLegacyProviderDurability(t, ctx, f, first, 14)
		server.Db(ctx, func(conn server.PgConn) {
			var durable, deferred bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*)=2 AND bool_and(NOT applied) AND sum(debit_byte_count)=14 FROM transfer_debit_journal WHERE contract_id=ANY($1)),
				(SELECT count(*)=2 AND bool_and(NOT settled) FROM transfer_escrow WHERE contract_id=ANY($1))`, request.ContractIds).Scan(&durable, &deferred))
			if !durable || !deferred {
				t.Fatal("committed outcomes lost their durable debit obligations")
			}
		})
		releaseHook := &asyncDebitReleaseHook{key: redisContractReservationKeys(f.balanceId)[0], after: true}
		releaseHook.enabled.Store(true)
		asyncDebitInstallHook(ctx, releaseHook)
		_, _, _, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err == nil || releaseHook.hits.Load() != 1 {
			t.Fatal("worker did not exercise the actual lost release acknowledgement")
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 986 || pending != 0 || applied != 2 {
			t.Fatal("lost release acknowledgement discarded or repeated financial consumption")
		}
		releaseHook.enabled.Store(false)
		count, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || count != 0 || released != 2 || busy {
			t.Fatal("release recovery repeated the committed debit")
		}
		count, released, busy, err = flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || count != 0 || released != 0 || busy {
			t.Fatal("empty worker replay changed settled accounting")
		}
		requireRedisExpiryRepairTestCredit(t, ctx, f, 986, 37)
		server.Db(ctx, func(conn server.PgConn) {
			var metadata bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(settled AND settle_time IS NOT NULL) AND sum(payout_byte_count)=14
				FROM transfer_escrow WHERE contract_id=ANY($1)`, request.ContractIds).Scan(&metadata))
			if !metadata {
				t.Fatal("durable worker did not repair omitted settlement metadata")
			}
		})
		var ownerId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, task.RunOnce("legacy_provider_totals", first).String()).Scan(&ownerId))
		})
		owner := task.GetTasks(ctx, ownerId)[ownerId]
		if owner == nil {
			t.Fatal("financial commit lost the provider projection owner")
		}
		target := task.NewTaskTarget(ApplyLegacyProviderTotals)
		for range 2 {
			_, _, err := target.RunSpecific(ctx, owner)
			server.Raise(err)
		}
		requireLegacyProviderDurability(t, ctx, f, first, 14)
		requireRedisExpiryClock(t, ctx, "11")
		replay, err := RepairRedisContractExpiry(ctx, request)
		if err != nil {
			t.Fatal("terminal replay failed")
		}
		for _, entry := range replay.Contracts {
			if entry.Status != "terminal" || entry.ProofCommitted {
				t.Fatal("terminal replay rewrote proof or claimed another settlement")
			}
		}
		after, _ := readContractExpiryTestSnapshot(t, ctx, first)
		if !bytes.Equal(proof, after) || !bytes.Equal(neighborBefore, readRedisExpiryRepairTestState(ctx, neighbor.ContractId)) {
			t.Fatal("recovery changed retained usage or an active neighbor")
		}
		requireRedisExpiryClock(t, ctx, "11")
	})
}

// Already-final reports take the continuation's direct-settlement branch.
func TestRedisExpiryRepairFinalReportsDoNotWaitForProjection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id := newRedisExpiryRepairTestContract(t, ctx, f, "reportless")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				VALUES($1,'source',17,$2,false),($1,'destination',11,$2,false)`, id, server.NowUtc().Add(-time.Hour)))
		})
		repairRedisExpiryWithProjectionGate(t, ctx, ContractExpiryRepairRequest{
			ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true,
		})
		requireRedisExpiryRepairTestCredit(t, ctx, f, 1000, 100)
		requireRedisExpiryClock(t, ctx, "11")
		_, _, _, err := flushTransferDebitBalance(ctx, f.balanceId)
		server.Raise(err)
		requireRedisExpiryRepairTestCredit(t, ctx, f, 986, 0)
		requireRedisExpiryClock(t, ctx, "11")
	})
}

func requireRedisExpiryClock(t testing.TB, ctx context.Context, expected string) {
	t.Helper()
	clock, ok := GetClock(ctx)
	if !ok || clock.TotalTransferByteCount != expected {
		t.Fatal("scoped expiry changed the existing clock increment or repeated it on recovery")
	}
}
