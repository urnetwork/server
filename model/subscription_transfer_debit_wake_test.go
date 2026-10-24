// Both settlement paths that journal a debit wake their partition's backed-off
// flush chain after the commit, and leave an active chain's queue row alone. A
// scoped expiry repair does not wake (subscription_contract_redis_expiry_projection_test.go).
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The stand-in chain's arguments and result carry nothing.
type transferDebitWakeStandInArgs struct{}
type transferDebitWakeStandInResult struct{}

// Holds a partition's chain key in the queue; no worker runs it here.
func transferDebitWakeStandIn(*transferDebitWakeStandInArgs, *session.ClientSession) (*transferDebitWakeStandInResult, error) {
	return &transferDebitWakeStandInResult{}, nil
}

// The raw key's stored RunOnce form is the literal the debit monitor's query and
// the startup ownership keys build.
func TestTransferDebitFlushRunOnceKeyMatchesTheStoredChainKey(t *testing.T) {
	if key := TransferDebitFlushRunOnceKey(3); key != "flush_transfer_debits_3" || task.RunOnce(key).String() != `["flush_transfer_debits_3"]` {
		t.Fatal("the debit chain key changed", key, task.RunOnce(key).String())
	}
}

// A stand-in for the partition's backed-off chain: a queue row under the
// chain's key due at the idle delay, and, when marked, the chain's idle mark.
func backOffTransferDebitWakeStandIn(t testing.TB, ctx context.Context, shard int, marked bool) {
	t.Helper()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	key := TransferDebitFlushRunOnceKey(shard)
	server.Tx(ctx, func(tx server.PgTx) {
		task.ScheduleTaskInTx(tx, transferDebitWakeStandIn, &transferDebitWakeStandInArgs{}, owner,
			task.RunOnce(key), task.RunAt(server.NowUtc().Add(30*time.Second)))
	}, server.TxReadCommitted, server.OptNoRetry())
	if marked {
		server.Raise(task.MarkIdleRunOnce(ctx, key, task.DefaultIdleWakeSettings()))
	}
}

// The stand-in row's run_at and row version.
func readTransferDebitWakeStandIn(t testing.TB, ctx context.Context, shard int) (runAt time.Time, version string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT run_at,xmin::text FROM pending_task WHERE run_once_key=$1`,
			task.RunOnce(TransferDebitFlushRunOnceKey(shard)).String()).Scan(&runAt, &version))
	})
	return
}

// The ordinary settlement journals its debit and, once the commit returned,
// kicks the backed-off chain of the debit's partition. A later settlement of
// the same partition finds no mark and leaves the chain's row unwritten.
func TestAsyncDebitSettlementWakesOnlyABackedOffFlushChain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		shard := transferDebitShard(f.balanceId)
		backOffTransferDebitWakeStandIn(t, ctx, shard, true)
		contract := createRedisAdmissionTest(ctx, f, 100)
		posts := asyncDebitTestSettle(ctx, contract.ContractId, 11)
		if _, pending, _ := asyncDebitTestState(t, ctx, f.balanceId); pending != 1 {
			t.Fatal("the settlement did not journal its debit", pending)
		}
		if runAt, _ := readTransferDebitWakeStandIn(t, ctx, shard); runAt.Before(server.NowUtc().Add(10 * time.Second)) {
			t.Fatal("the chain moved before the settlement's posts ran", runAt)
		}
		server.RunPosts(ctx, posts...)
		runAt, version := readTransferDebitWakeStandIn(t, ctx, shard)
		if runAt.After(server.NowUtc()) {
			t.Fatal("the committed debit did not kick its backed-off chain", runAt)
		}
		second := createRedisAdmissionTest(ctx, f, 100)
		server.RunPosts(ctx, asyncDebitTestSettle(ctx, second.ContractId, 11)...)
		if _, after := readTransferDebitWakeStandIn(t, ctx, shard); after != version {
			t.Fatal("a settlement wrote the queue row of a chain that was not backed off")
		}
	})
}

// A deadline close of a Redis contract journals its debit too, and its
// committed posts kick the partition's backed-off chain.
func TestDeadlineCloseWakesABackedOffFlushChain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		shard := transferDebitShard(f.balanceId)
		backOffTransferDebitWakeStandIn(t, ctx, shard, true)
		escrow := createRedisAdmissionTest(ctx, f, 100)
		keys := deadlineFinancialOwnershipKeys(escrow.ContractId, readDeadlineBalanceIds(ctx, escrow.ContractId))
		var result *ContractDeadlineReconciliation
		var posts []server.PostFunction
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			result, posts = reconcileContractAtDeadlineWithOwnershipInTx(ctx, tx, escrow.ContractId, server.NowUtc(), true)
		}, server.TxReadCommitted, server.OptNoRetry())
		if result == nil || result.AlreadyClosed || result.Missing {
			t.Fatalf("the contract did not close at its deadline: %+v", result)
		}
		if runAt, _ := readTransferDebitWakeStandIn(t, ctx, shard); runAt.Before(server.NowUtc().Add(10 * time.Second)) {
			t.Fatal("the chain moved before the close's posts ran", runAt)
		}
		server.RunPosts(ctx, posts...)
		if runAt, _ := readTransferDebitWakeStandIn(t, ctx, shard); runAt.After(server.NowUtc()) {
			t.Fatal("the deadline close's debit did not kick its backed-off chain", runAt)
		}
	})
}
