// Legacy SQL mirrors and public Redis request tokens have distinct recovery
// and TTL contracts. These controls inspect both without dropping read errors.
package model

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
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
		posts := settleNetEscrowOrderingTestContract(ctx, escrow.ContractId)
		server.RunPosts(ctx, posts...)
		server.RunPosts(ctx, posts...)
		ReconcileRedisContractReservation(ctx, escrow.ContractId)
		// Even zero consumption retains its journal/token until the debit
		// worker acknowledges cleanup. Settlement must not extend its lease.
		server.Redis(ctx, func(r server.RedisClient) {
			amount, err := r.HGet(ctx, keys[1], escrow.ContractId.String()).Int64()
			server.Raise(err)
			expiry, err := r.ZScore(ctx, keys[2], escrow.ContractId.String()).Result()
			server.Raise(err)
			if amount != 0 || expiry != originalExpiry {
				t.Fatal("pending zero debit lost its original token or lease", amount, expiry, originalExpiry)
			}
		})
		assertPayoutDebitTestConsumptionAndDrain(t, ctx, f.balanceId, 1000, 0)
		replay, err := FlushTransferDebits(ctx, transferDebitShard(f.balanceId), nil, 1)
		if err != nil || replay != (TransferDebitFlushResult{}) {
			t.Fatal("empty public zero-debit replay repeated work", replay, err)
		}
		// A post delayed until after writeback cannot restore the old token.
		server.RunPosts(ctx, posts...)
		ReconcileRedisContractReservation(ctx, escrow.ContractId)
		server.Redis(ctx, func(r server.RedisClient) {
			value, err := r.Get(ctx, keys[0]).Int64()
			server.Raise(err)
			if value != 0 || !errors.Is(r.HGet(ctx, keys[1], escrow.ContractId.String()).Err(), server.RedisNil) ||
				!errors.Is(r.ZScore(ctx, keys[2], escrow.ContractId.String()).Err(), server.RedisNil) {
				t.Fatal("terminal public settlement did not release the exact request once", value)
			}
		})
	})
}
