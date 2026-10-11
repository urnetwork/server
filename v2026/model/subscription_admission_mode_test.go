package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Keeps legacy payer-handoff ordering observable without changing the
// unconditional public Redis entry points or adding an operator mode switch.
func newLegacyShardCompanionChainFixture(t testing.TB, ctx context.Context, bytes ByteCount) shardCompanionChainFixture {
	t.Helper()
	f := shardCompanionChainFixture{owner: shardTestOwner(t, ctx, shardTestKey(0)), peer: newEscrowSelectionTestClients(t, ctx)}
	var err error
	f.origin, err = createTransferEscrow(ctx, f.owner.NetworkId, f.owner.ClientId, f.peer.providerNetworkId, f.peer.providerId, bytes)
	if err != nil {
		t.Fatal(err)
	}
	f.back, err = createCompanionTransferEscrow(ctx, f.peer.providerNetworkId, f.peer.providerId, f.owner.NetworkId, f.owner.ClientId, bytes, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Public origin and inherited-payer companion admission must ignore the old
// permit, preserve the same grant, and reject already-canceled callers with
// no durable write. Real Redis debt and row markers prevent a legacy fallback
// from passing this control merely by changing queue instrumentation.
func TestPublicRedisCompanionIgnoresLegacyPermitAndPreservesCancellation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		f := newShardCompanionChainFixture(t, ctx, 1024)
		release, err := transferEscrowAdmissionQueue.acquire(ctx, f.owner.NetworkId)
		server.Raise(err)
		defer release()
		reply, err := f.reply(ctx, 4096)
		if err != nil || reply == nil {
			t.Fatal("public reply waited for legacy permit", err)
		}
		assertCompanionReservation(t, ctx, reply, 1024)
		if got := payerQueueReferences(&transferEscrowAdmissionQueue, f.owner.NetworkId); got != 1 {
			t.Fatalf("public creator joined legacy queue: %d", got)
		}
		var marked int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND redis_reserved`, reply.ContractId).Scan(&marked))
		})
		if marked != 1 || Testing_NetEscrowByteCount(ctx, f.owner.BalanceId) != 3072 {
			t.Fatal("public companion did not reserve exact Redis credit")
		}
		before := shardCompanionFinancialState(ctx, f)
		canceled, stop := context.WithCancel(ctx)
		stop()
		panicValue := captureShardQueryPanic(func() { _, err = f.reply(canceled, 4096) })
		if panicValue != nil {
			var ok bool
			err, ok = panicValue.(error)
			if !ok {
				t.Fatalf("unexpected cancellation type %T", panicValue)
			}
		}
		if !errors.Is(err, context.Canceled) && !server.IsDoneError(err) {
			t.Fatalf("canceled public reply: %v", err)
		}
		if after := shardCompanionFinancialState(ctx, f); after != before {
			t.Fatal("canceled public reply wrote financial state")
		}
	})
}

// Expired grants may still own open contracts. Global legacy reconciliation
// must not synthesize their marked Redis debt; the explicit exact-contract
// recovery reconstructs the token within its original deadline, then a real
// bilateral close releases it once without extending another contract.
func TestRedisExpiredBalanceKnownContractRecoveryAndRelease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		escrow := createRedisAdmissionTest(ctx, f, 17)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, f.balanceId, server.NowUtc().Add(-time.Minute)))
		})
		keys := redisContractReservationKeys(f.balanceId)
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, keys[:3]...).Err()) })
		ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, true)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 0 {
			t.Fatalf("legacy reconciliation manufactured marked debt: %d", got)
		}
		ReconcileRedisContractReservation(ctx, escrow.ContractId)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatalf("known-contract recovery: %d", got)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			debt, err := r.HGet(ctx, keys[1], escrow.ContractId.String()).Int64()
			if err != nil || debt != 17 {
				t.Fatal("recovery token", debt, err)
			}
			ttl, err := r.PTTL(ctx, keys[0]).Result()
			if err != nil || ttl < 23*time.Hour || ttl > 25*time.Hour {
				t.Fatal("bounded shared Redis TTL", ttl, err)
			}
		})
		posts := settleNetEscrowOrderingTestContract(ctx, escrow.ContractId)
		server.RunPosts(ctx, posts...)
		server.RunPosts(ctx, posts...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 0 {
			t.Fatalf("replayed release: %d", got)
		}
	})
}
