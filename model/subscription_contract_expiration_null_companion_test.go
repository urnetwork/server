// Missing deadlines use the same creation-based lifetime for new reservations
// and old reservation reuse as they do for asynchronous cleanup.
package model

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Both origin branches reject an expired null deadline before cleanup reaches
// it, yet a fresh null origin and its normal close linger remain usable.
func TestContractExpirationNullCompanionSelectsLiveRenewal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, chained := range []bool{false, true} {
			for _, redisAdmission := range []bool{true, false} {
				payerNetworkId, payerId, providerNetworkId, providerId := companionReservationFixture(ctx, false)
				if chained {
					_, err := CreateTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, 100)
					server.Raise(err)
				}
				createOrigin := func() *TransferEscrow {
					var origin *TransferEscrow
					var err error
					if chained {
						origin, err = CreateCompanionTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, 100, time.Minute)
					} else {
						origin, err = CreateTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, 100)
					}
					server.Raise(err)
					return origin
				}
				createChild := func() (*TransferEscrow, error) {
					if redisAdmission {
						return CreateCompanionTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, 100, time.Minute)
					}
					return createCompanionTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, 100, time.Minute)
				}
				expired := createOrigin()
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL, create_time=$2 WHERE contract_id=$1`,
						expired.ContractId, server.NowUtc().Add(-61*time.Minute)))
				})
				for _, closed := range []bool{false, true} {
					if closed {
						server.Raise(CloseContract(ctx, expired.ContractId, payerId, 0, false))
						server.Raise(CloseContract(ctx, expired.ContractId, providerId, 0, false))
					}
					before := contractLifecycleTestCount(t, ctx, providerId, payerId)
					if _, err := createChild(); !errors.Is(err, ErrMissingCompanionOrigin) {
						t.Fatalf("chained=%t redis=%t closed=%t expired null origin admitted a child: %v", chained, redisAdmission, closed, err)
					}
					if after := contractLifecycleTestCount(t, ctx, providerId, payerId); after != before {
						t.Fatal("expired origin refusal published child custody")
					}
				}
				live := createOrigin()
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL, create_time=$2 WHERE contract_id=$1`,
						live.ContractId, server.NowUtc().Add(-time.Minute)))
				})
				for _, closed := range []bool{false, true} {
					if closed {
						server.Raise(CloseContract(ctx, live.ContractId, payerId, 0, false))
						server.Raise(CloseContract(ctx, live.ContractId, providerId, 0, false))
					}
					child, err := createChild()
					if err != nil || child == nil || child.CompanionContractId == nil || *child.CompanionContractId != live.ContractId {
						t.Fatalf("chained=%t redis=%t closed=%t fresh null origin was not selected: %v", chained, redisAdmission, closed, err)
					}
					if !child.ExpirationTime.After(server.NowUtc().Add(59 * time.Minute)) {
						t.Fatal("child did not retain its independent explicit deadline")
					}
				}
				// An explicit future deadline remains authoritative even when its
				// creation clock alone would make the fallback due.
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`,
						expired.ContractId, server.NowUtc().Add(time.Hour)))
				})
				child, err := createChild()
				if err != nil || child == nil || child.CompanionContractId == nil || *child.CompanionContractId != expired.ContractId {
					t.Fatal("explicit future deadline lost its normal linger eligibility", err)
				}
			}
		}
	})
}

// Expired larger origins must not raise a probe's reservation through either
// max-budget subquery, including the recently closed and chained branches.
func TestContractExpirationNullCompanionExcludesExpiredProbeBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, chained := range []bool{false, true} {
			payerNetworkId, payerId, providerNetworkId, providerId := companionReservationFixture(ctx, true)
			if chained {
				_, err := CreateTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, 1000)
				server.Raise(err)
			}
			origins := []*TransferEscrow{}
			for _, amount := range []ByteCount{100, 400} {
				var origin *TransferEscrow
				var err error
				if chained {
					origin, err = CreateCompanionTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, amount, time.Minute)
				} else {
					origin, err = CreateTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, amount)
				}
				server.Raise(err)
				age := time.Minute
				if amount == 400 {
					age = 61 * time.Minute
				}
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL, create_time=$2 WHERE contract_id=$1`,
						origin.ContractId, server.NowUtc().Add(-age)))
				})
				origins = append(origins, origin)
			}
			for _, closed := range []bool{false, true} {
				if closed {
					for _, origin := range origins {
						server.Raise(CloseContract(ctx, origin.ContractId, payerId, 0, false))
						server.Raise(CloseContract(ctx, origin.ContractId, providerId, 0, false))
					}
				}
				child, err := CreateCompanionTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, 500, time.Minute)
				if err != nil {
					t.Fatalf("chained=%t closed=%t: %v", chained, closed, err)
				}
				assertCompanionReservation(t, ctx, child, 100)
				if child.CompanionContractId == nil || *child.CompanionContractId != origins[0].ContractId {
					t.Fatal("probe selected the expired larger null origin")
				}
			}
		}
	})
}

// A pending cleanup cannot make an expired null escrow reusable. Fresh null
// custody and explicit future deadlines keep their existing ordering.
func TestContractExpirationNullEscrowReuse(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		expired := createRedisAdmissionTest(ctx, f, 100)
		fresh := createRedisAdmissionTest(ctx, f, 100)
		explicit := createRedisAdmissionTest(ctx, f, 100)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL, create_time=$2 WHERE contract_id=$1`, expired.ContractId, server.NowUtc().Add(-61*time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL, create_time=$2 WHERE contract_id=$1`, fresh.ContractId, server.NowUtc().Add(-time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, explicit.ContractId, server.NowUtc().Add(-2*time.Hour)))
		})
		escrows := GetOpenTransferEscrowsOrderedByPriorityCreateTime(ctx, f.sourceId, f.destinationId, 100)
		if len(escrows) != 2 || escrows[0].ContractId != explicit.ContractId || escrows[1].ContractId != fresh.ContractId {
			t.Fatal("reuse did not exclude only the expired null reservation")
		}
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 300 {
			t.Fatal("read-only eligibility changed existing accounting custody")
		}
	})
}
