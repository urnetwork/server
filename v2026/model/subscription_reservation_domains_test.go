// Legacy SQL mirrors and public Redis request tokens have distinct recovery
// and TTL contracts. These controls inspect both without dropping read errors.
package model

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A refused public dispute retains its exact token while a separately funded
// legacy peer can settle. The shared total alone cannot hide cross-owner loss.
func TestPublicDisputeAndLegacyPeerKeepSeparateReservations(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const grant = ByteCount(32 * 1024 * 1024)
		fixture := newForceCloseDisputeFixture(t, ctx, false, false, 0, 4*grant, grant)
		var sourceNetwork, source, destinationNetwork, destination server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT source_network_id,source_id,destination_network_id,destination_id
				FROM transfer_contract WHERE contract_id=$1`, fixture.contractId).
				Scan(&sourceNetwork, &source, &destinationNetwork, &destination))
		})
		peer, err := createTransferEscrow(ctx, sourceNetwork, source, destinationNetwork, destination, 1024)
		if err != nil || peer == nil {
			t.Fatal("legacy peer did not reserve", err)
		}
		before := fixture.state(t, ctx)
		if !before.redisReserved || before.legacyEscrowByteCount != 1024 || before.redisEscrowByteCount != grant ||
			before.requestTokenByteCount != grant || before.netEscrowByteCount != grant+1024 {
			t.Fatalf("mixed reservation oracle did not observe both owners: %+v", before)
		}
		count, err := ForceCloseOpenContractIds(ctx, fixture.cutoff, 10, 1, 0, 0)
		if count != 1 || !errors.Is(err, errContractInsufficientEscrow) {
			t.Fatal("public dispute did not reach its actual accounting refusal", count, err)
		}
		after := fixture.state(t, ctx)
		if after != before {
			t.Fatalf("refused settlement changed mixed owners: before=%+v after=%+v", before, after)
		}
		if err := CloseContract(ctx, peer.ContractId, source, 0, false); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, peer.ContractId, destination, 0, false); err != nil {
			t.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var pending, terminal bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1),
				outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1`, peer.ContractId).Scan(&pending, &terminal))
			if !pending || terminal || fixture.state(t, ctx) != before {
				t.Fatal("queued legacy peer did not retain both reservation owners")
			}
		})
		flushed, err := FlushLegacySettlements(ctx, int(peer.ContractId[15])%LegacySettlementShardCount, nil, 1)
		if err != nil || flushed.Visited != 1 || flushed.Completed != 1 || flushed.Failed != 0 {
			t.Fatal("legacy peer worker did not complete its durable owner", flushed, err)
		}
		after = fixture.state(t, ctx)
		if after.legacyEscrowByteCount != 0 || after.redisEscrowByteCount != grant || after.requestTokenByteCount != grant ||
			after.netEscrowByteCount != grant || after.payerBalanceByteCount != before.payerBalanceByteCount ||
			after.providerPayoutByteCount != 0 || after.outcome != "" || !after.dispute {
			t.Fatalf("legacy peer settlement changed the unresolved public obligation: %+v", after)
		}
	})
}

// A long-lived grant does not lengthen one public request's original24h token
// or shared25h cache. This proves current lease accounting, not safe recovery
// of an unresolved SQL obligation after its token/cache has expired.
func TestPublicRedisLongLivedGrantRetainsOriginalRequestLease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`,
				f.balanceId, server.NowUtc().Add(100*365*24*time.Hour)))
		})
		escrow := createRedisAdmissionTest(ctx, f, 17)
		keys := redisContractReservationKeys(f.balanceId)
		var originalExpiry float64
		server.Redis(ctx, func(r server.RedisClient) {
			amount, err := r.HGet(ctx, keys[1], escrow.ContractId.String()).Int64()
			server.Raise(err)
			if amount != 17 {
				t.Fatal("public request token did not retain its amount", amount)
			}
			originalExpiry, err = r.ZScore(ctx, keys[2], escrow.ContractId.String()).Result()
			server.Raise(err)
			now, err := r.Time(ctx).Result()
			server.Raise(err)
			remaining := time.Duration(originalExpiry-float64(now.UnixMilli())) * time.Millisecond
			if remaining < 23*time.Hour || remaining > redisContractReservationLease {
				t.Fatal("long-lived grant changed the request lease", remaining)
			}
			for _, key := range keys[:3] {
				ttl, err := r.PTTL(ctx, key).Result()
				server.Raise(err)
				if ttl < 24*time.Hour || ttl > 25*time.Hour {
					t.Fatal("public request cache inherited the100-year balance lifetime", ttl)
				}
			}
		})
		ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, true)
		ReconcileRedisContractReservation(ctx, escrow.ContractId)
		server.Redis(ctx, func(r server.RedisClient) {
			expiry, err := r.ZScore(ctx, keys[2], escrow.ContractId.String()).Result()
			server.Raise(err)
			if expiry != originalExpiry {
				t.Fatal("reconciliation changed the original request's lease", expiry, originalExpiry)
			}
		})
		neighbor := createRedisAdmissionTest(ctx, f, 29)
		var neighborExpiry float64
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			neighborExpiry, err = r.ZScore(ctx, keys[2], neighbor.ContractId.String()).Result()
			server.Raise(err)
		})
		// A terminal outcome queues its debit. Its exact token and original
		// expiry remain owned until the durable worker commits and releases it.
		posts := asyncDebitTestSettle(ctx, escrow.ContractId, 11)
		server.RunPosts(ctx, posts...)
		server.RunPosts(ctx, posts...)
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 1000 || pending != 1 || applied != 0 {
			t.Fatal("outcome did not retain the pending durable debit", credit, pending, applied)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			value, err := r.Get(ctx, keys[0]).Int64()
			server.Raise(err)
			amount, err := r.HGet(ctx, keys[1], escrow.ContractId.String()).Int64()
			server.Raise(err)
			expiry, err := r.ZScore(ctx, keys[2], escrow.ContractId.String()).Result()
			server.Raise(err)
			if value != 40 || amount != 11 || expiry != originalExpiry {
				t.Fatal("pending debit lost its consumption token or original lease", value, amount, expiry == originalExpiry)
			}
		})
		n, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || n != 1 || released != 1 || busy {
			t.Fatal("durable worker did not apply and release the exact debit", n, released, busy, err)
		}
		checkReleased := func() {
			credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
			if credit != 989 || pending+applied != 0 {
				t.Fatal("worker release changed durable accounting", credit, pending, applied)
			}
			server.Redis(ctx, func(r server.RedisClient) {
				value, err := r.Get(ctx, keys[0]).Int64()
				server.Raise(err)
				amount, err := r.HGet(ctx, keys[1], neighbor.ContractId.String()).Int64()
				server.Raise(err)
				expiry, err := r.ZScore(ctx, keys[2], neighbor.ContractId.String()).Result()
				server.Raise(err)
				if value != 29 || amount != 29 || expiry != neighborExpiry ||
					!errors.Is(r.HGet(ctx, keys[1], escrow.ContractId.String()).Err(), server.RedisNil) ||
					!errors.Is(r.ZScore(ctx, keys[2], escrow.ContractId.String()).Err(), server.RedisNil) {
					t.Fatal("worker did not release exactly its request while retaining its neighbor", value, amount, expiry == neighborExpiry)
				}
			})
		}
		checkReleased()
		server.RunPosts(ctx, posts...)
		ReconcileRedisContractReservation(ctx, escrow.ContractId)
		n, released, busy, err = flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || n != 0 || released != 0 || busy {
			t.Fatal("replayed worker or callbacks repeated the debit or release", n, released, busy, err)
		}
		checkReleased()
	})
}
