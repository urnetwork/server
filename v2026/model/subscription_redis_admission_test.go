package model

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

func createRedisAdmissionTest(ctx context.Context, f netEscrowOrderingTestFixture, amount ByteCount) *TransferEscrow {
	escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, amount)
	server.Raise(err)
	return escrow
}
func TestRedisAdmissionMixedSettlementReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		legacy, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		first := createRedisAdmissionTest(ctx, f, 200)
		_ = createRedisAdmissionTest(ctx, f, 300)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 600 {
			t.Fatalf("mixed reservations=%d", got)
		}
		released := settleNetEscrowOrderingTestContract(ctx, first.ContractId)
		server.RunPosts(ctx, released...)
		server.RunPosts(ctx, released...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 400 {
			t.Fatalf("replay removed neighbor reservation=%d", got)
		}
		server.RunPosts(ctx, settleNetEscrowOrderingTestContract(ctx, legacy.ContractId)...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 300 {
			t.Fatalf("legacy settlement changed Redis reservation=%d", got)
		}
		if got := GetActiveTransferBalanceByteCount(ctx, f.sourceNetworkId); got != 700 {
			t.Fatalf("mixed available=%d", got)
		}
	})
}
func TestRedisAdmissionAtomicSameBalanceBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		balance := server.NewId()
		start := make(chan struct{})
		amounts := make(chan ByteCount, 128)
		errs := make(chan error, 128)
		var wg sync.WaitGroup
		for range 128 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				a, e := redisContractReservation(ctx, "reserve", balance, server.NewId(), 1000, 17, redisContractReservationLease)
				amounts <- a
				errs <- e
			}()
		}
		close(start)
		wg.Wait()
		close(amounts)
		close(errs)
		var total ByteCount
		for amount := range amounts {
			total += amount
		}
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if total != 1000 {
			t.Fatalf("atomic healthy reservation=%d", total)
		}
		if got := Testing_NetEscrowByteCount(ctx, balance); got != total {
			t.Fatalf("counter=%d", got)
		}
	})
}
func TestRedisAdmissionDecimalPrecisionAndIdempotency(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		balance, contract := server.NewId(), server.NewId()
		for range 2 {
			amount, err := redisContractReservation(ctx, "reserve", balance, contract, math.MaxInt64, math.MaxInt64, redisContractReservationLease)
			if err != nil || amount != math.MaxInt64 {
				t.Fatal("full signed precision or retry failed", err)
			}
		}
		amount, err := redisContractReservation(ctx, "reserve", balance, server.NewId(), math.MaxInt64, 1, redisContractReservationLease)
		if err != nil || amount != 0 {
			t.Fatal("over-admitted atomic budget", err)
		}
		for range 2 {
			releaseRedisContractReservations(ctx, contract, []server.Id{balance})
		}
		if got := Testing_NetEscrowByteCount(ctx, balance); got != 0 {
			t.Fatal("release replay changed reservation", got)
		}
	})
}
func TestRedisAdmissionCanceledCorruptAndMissingCounter(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		balance := server.NewId()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := redisContractReservation(canceled, "reserve", balance, server.NewId(), 100, 17, redisContractReservationLease); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled admitted", err)
		}
		keys := redisContractReservationKeys(balance)
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Set(ctx, keys[0], "malformed", 0).Err()) })
		if _, err := redisContractReservation(ctx, "reserve", balance, server.NewId(), 100, 17, redisContractReservationLease); err == nil {
			t.Fatal("corrupt counter treated as zero")
		}
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, keys[:3]...).Err()) })
		// Explicitly accepted approximation: missing Redis state begins at zero.
		amount, err := redisContractReservation(ctx, "reserve", balance, server.NewId(), 100, 17, redisContractReservationLease)
		if err != nil || amount != 17 {
			t.Fatal("missing-state policy", err)
		}
	})
}
func TestRedisAdmissionRollbackExpiresWithoutDatabaseLocks(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		ctx = withRedisContractAdmission(ctx)
		conn, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			conn.Release()
			t.Fatal(err)
		}
		escrow, _, err := createTransferEscrowInTx(ctx, tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 17, nil)
		if err != nil {
			t.Fatal(err)
		}
		server.Raise(tx.Rollback(ctx))
		conn.Release()
		var durable bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1)`, escrow.ContractId).Scan(&durable))
		})
		if durable || Testing_NetEscrowByteCount(ctx, f.balanceId) != 17 {
			t.Fatal("rollback window not reproduced")
		}
		// Advance the owned reservation's Redis deadline directly; no sleeps.
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.ZAdd(ctx, redisContractReservationKeys(f.balanceId)[2], redis.Z{Score: 0, Member: escrow.ContractId.String()}).Err())
		})
		amount, err := redisContractReservation(ctx, "reserve", f.balanceId, server.NewId(), 1000, 23, redisContractReservationLease)
		if err != nil || amount != 23 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 23 {
			t.Fatal("abandoned reservation did not expire", err)
		}
	})
}
func TestRedisAdmissionDoesNotUseHeldProcessPayerPermit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		release, err := transferEscrowAdmissionQueue.acquire(ctx, f.sourceNetworkId)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 17)
		if err != nil || escrow == nil {
			t.Fatal("creation depended on held local permit", err)
		}
	})
}
func TestRedisAdmissionCompanionUsesSamePayerAndCredit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		origin := createRedisAdmissionTest(ctx, f, 100)
		companion, err := CreateCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, f.sourceId, 100, time.Hour)
		if err != nil || companion == nil || companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
			t.Fatal("companion authority", err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 200 {
			t.Fatal("companion payer reservation", got)
		}
		server.RunPosts(ctx, settleNetEscrowOrderingTestContract(ctx, companion.ContractId)...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 100 {
			t.Fatal("companion release changed origin", got)
		}
	})
}

func TestRedisAdmissionRecoveryAfterLossAndLateRelease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		first := createRedisAdmissionTest(ctx, f, 17)
		second := createRedisAdmissionTest(ctx, f, 23)
		keys := redisContractReservationKeys(f.balanceId)
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, keys[:3]...).Err()) })
		ReconcileRedisContractReservation(ctx, first.ContractId)
		ReconcileRedisContractReservation(ctx, second.ContractId)
		ReconcileRedisContractReservation(ctx, first.ContractId)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("recovery replay", got)
		}
		posts := settleNetEscrowOrderingTestContract(ctx, first.ContractId)
		ReconcileRedisContractReservation(ctx, first.ContractId)
		server.RunPosts(ctx, posts...)
		server.RunPosts(ctx, posts...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 23 {
			t.Fatal("delayed release after repair", got)
		}
	})
}
func TestRedisAdmissionLegacyPolicyValueDoesNotDisableCreation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		var enabled bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT enabled FROM redis_contract_admission_policy WHERE singleton`).Scan(&enabled))
		})
		if enabled {
			t.Fatal("migration unexpectedly enabled new admission")
		}
		_ = createRedisAdmissionTest(ctx, f, 900)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE redis_contract_admission_policy SET enabled=false WHERE singleton`))
		})
		if escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 101); err == nil || escrow != nil {
			t.Fatal("unconditional admission ignored existing reservations")
		}
		next := createRedisAdmissionTest(ctx, f, 100)
		server.RunPosts(ctx, settleNetEscrowOrderingTestContract(ctx, next.ContractId)...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 900 {
			t.Fatal("legacy policy value changed approximate debt", got)
		}
	})
}
func TestRedisAdmissionExpiryCleanupIsBounded(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		balance := server.NewId()
		var tokens []redis.Z
		for range 33 {
			contract := server.NewId()
			_, err := redisContractReservation(ctx, "reserve", balance, contract, 100, 1, redisContractReservationLease)
			server.Raise(err)
			tokens = append(tokens, redis.Z{Score: 0, Member: contract.String()})
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.ZAdd(ctx, redisContractReservationKeys(balance)[2], tokens...).Err())
		})
		live := server.NewId()
		_, err := redisContractReservation(ctx, "reserve", balance, live, 100, 1, redisContractReservationLease)
		server.Raise(err)
		if got := Testing_NetEscrowByteCount(ctx, balance); got != 2 {
			t.Fatal("one operation did not expire exactly32", got)
		}
		releaseRedisContractReservations(ctx, live, []server.Id{balance})
		if got := Testing_NetEscrowByteCount(ctx, balance); got != 0 {
			t.Fatal("remaining expiry/release did not converge", got)
		}
	})
}

func TestRedisAdmissionDurableUsageAndLostSettlementPost(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		escrow := createRedisAdmissionTest(ctx, f, 100)
		var posts []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
                VALUES($1,'source',17,$2,false),($1,'destination',17,$2,false)`, escrow.ContractId, server.NowUtc()))
			var closed bool
			var err error
			posts, closed, err = settleEscrowInTx(ctx, tx, escrow.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if !closed {
				t.Fatal("settlement not claimed")
			}
		}, server.TxReadCommitted)
		var credit ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&credit))
		})
		if credit != 1000 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 100 {
			t.Fatal("lost post did not preserve deferred credit plus conservative admission debt")
		}
		ReconcileRedisContractReservation(ctx, escrow.ContractId)
		server.RunPosts(ctx, posts...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatal("pending consumed credit was released before writeback", got)
		}
		_, _, _, flushErr := flushTransferDebitBalance(ctx, f.balanceId)
		server.Raise(flushErr)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 0 {
			t.Fatal("recovered closed contract retained debt", got)
		}
		err := CloseContract(ctx, escrow.ContractId, f.sourceId, 17, false)
		if err != nil && !isOnlyContractAlreadySettled(err) {
			t.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&credit))
		})
		if credit != 983 {
			t.Fatal("duplicate close debited twice")
		}
	})
}

func TestRedisAdmissionPartialEvictionCannotReleaseNeighbor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		balance, old, next := server.NewId(), server.NewId(), server.NewId()
		_, err := redisContractReservation(ctx, "reserve", balance, old, 1000, 17, redisContractReservationLease)
		server.Raise(err)
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, redisContractReservationKeys(balance)[0]).Err()) })
		_, err = redisContractReservation(ctx, "reserve", balance, next, 1000, 23, redisContractReservationLease)
		server.Raise(err)
		releaseRedisContractReservations(ctx, old, []server.Id{balance})
		if got := Testing_NetEscrowByteCount(ctx, balance); got != 23 {
			t.Fatal("old generation release removed new reservation", got)
		}
	})
}

func TestRedisAdmissionOlderRecoveryPreservesYoungerLease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		balance, young, old := server.NewId(), server.NewId(), server.NewId()
		amount, err := redisContractReservation(ctx, "reserve", balance, young, 1000, 23, redisContractReservationLease)
		if err != nil || amount != 23 {
			t.Fatal("young reservation failed", err)
		}
		amount, err = redisContractReservation(ctx, "restore", balance, old, 1000, 17, time.Hour)
		if err != nil || amount != 17 {
			t.Fatal("older recovery failed", err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			keys := redisContractReservationKeys(balance)
			for _, key := range keys[:3] {
				ttl, err := r.PTTL(ctx, key).Result()
				if err != nil || ttl < 24*time.Hour {
					t.Fatal("older recovery shortened a younger reservation's shared key lifetime", ttl, err)
				}
			}
			youngUntil, err := r.ZScore(ctx, keys[2], young.String()).Result()
			server.Raise(err)
			oldUntil, err := r.ZScore(ctx, keys[2], old.String()).Result()
			server.Raise(err)
			if time.Duration(youngUntil-oldUntil)*time.Millisecond < 22*time.Hour {
				t.Fatal("recovery extended the older token's own horizon")
			}
		})
		if got := Testing_NetEscrowByteCount(ctx, balance); got != 40 {
			t.Fatal("recovery changed combined reservation", got)
		}
	})
}
