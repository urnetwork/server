// Report arithmetic must not turn excessive usage into released payer credit.
package model

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The arithmetic reference has unbounded precision and covers both report orders.
func TestNetEscrowMeanExactAtStorageBoundaries(t *testing.T) {
	for _, first := range []ByteCount{0, 1, 2, math.MaxInt64 / 2, math.MaxInt64/2 + 1, math.MaxInt64 - 1, math.MaxInt64} {
		for _, second := range []ByteCount{0, 1, 2, math.MaxInt64 / 2, math.MaxInt64/2 + 1, math.MaxInt64 - 1, math.MaxInt64} {
			want := new(big.Int).Add(big.NewInt(int64(first)), big.NewInt(int64(second)))
			want.Quo(want, big.NewInt(2))
			actual, err := meanContractByteCount(first, second)
			if err != nil || int64(actual) != want.Int64() {
				t.Fatalf("mean(%d,%d)=%d error=%v want=%s", first, second, actual, err, want)
			}
		}
	}
}

// Stored legacy negatives must not become a smaller positive debit.
func TestNetEscrowMeanRejectsNegativeReports(t *testing.T) {
	for _, negative := range []ByteCount{-1, math.MinInt64} {
		for _, other := range []ByteCount{0, math.MaxInt64, negative} {
			for _, pair := range [][2]ByteCount{{negative, other}, {other, negative}} {
				if value, err := meanContractByteCount(pair[0], pair[1]); err == nil || value != 0 {
					t.Fatalf("negative report pair %v returned value=%d error=%v", pair, value, err)
				}
			}
		}
	}
}

func TestNetEscrowSettlementOverflowDoesNotReleaseCredit(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, _ := createNetEscrowOrderingTestContract(ctx, f, 700)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				VALUES($1,'source',$2,$3,false),($1,'destination',$2,$3,false)`,
				contract.ContractId, ByteCount(math.MaxInt64), server.NowUtc()))
			posts, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			if !errors.Is(err, errContractInsufficientEscrow) || closed || len(posts) != 0 {
				t.Errorf("overflow released spent credit: closed=%t posts=%d error=%v", closed, len(posts), err)
			}
		}, server.TxReadCommitted)
		if reserved := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; reserved != 700 {
			t.Fatalf("oversized reports released reservation: %d", reserved)
		}
	})
}

// Values at the storage limit remain valid when backed by that exact credit;
// rejecting every large report would conceal the arithmetic defect.
func TestNetEscrowSettlementMaximumCreditDebitsExactly(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, first := range []ByteCount{math.MaxInt64, math.MaxInt64 - 1} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance
					SET balance_byte_count=$2,start_balance_byte_count=$2 WHERE balance_id=$1`,
					f.balanceId, ByteCount(math.MaxInt64)))
			}, server.TxReadCommitted)
			contract, _ := createNetEscrowOrderingTestContract(ctx, f, math.MaxInt64)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
					(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
					VALUES($1,'source',$2,$4,false),($1,'destination',$3,$4,false)`,
					contract.ContractId, first, ByteCount(math.MaxInt64), server.NowUtc()))
				_, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
				server.Raise(err)
				if !closed {
					t.Fatal("valid maximum-credit settlement was not claimed")
				}
				var remaining ByteCount
				server.Raise(tx.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&remaining))
				if remaining != math.MaxInt64-first {
					t.Fatalf("maximum-credit debit: remaining=%d want=%d", remaining, math.MaxInt64-first)
				}
			}, server.TxReadCommitted)
		}
	})
}

// Legacy rows lack the newer usage snapshot validator. Every financial outcome
// still rejects negative consumed reports before claiming or releasing credit.
func TestNetEscrowSettlementRejectsNegativeLegacyReports(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, outcome := range []ContractOutcome{ContractOutcomeSettled, ContractOutcomeDisputeResolvedToSource, ContractOutcomeDisputeResolvedToDestination} {
			for _, negative := range []ByteCount{-1, math.MinInt64} {
				for _, pair := range [][2]ByteCount{{negative, 600}, {600, negative}} {
					// Destination adjudication consumes neither the source report
					// nor a source clock value; it has no negative input here.
					if outcome == ContractOutcomeDisputeResolvedToDestination && pair[0] < 0 {
						continue
					}
					f := newNetEscrowOrderingTestFixture(t, ctx)
					contract, _ := createNetEscrowOrderingTestContract(ctx, f, 700)
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL WHERE contract_id=$1`, contract.ContractId))
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
							(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
							VALUES($1,'source',$2,$4,false),($1,'destination',$3,$4,false)`,
							contract.ContractId, pair[0], pair[1], server.NowUtc()))
						posts, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, outcome)
						if err == nil || !strings.Contains(err.Error(), "negative contract close byte count") || closed || len(posts) != 0 {
							t.Fatalf("legacy %s reports %v released credit: closed=%t posts=%d error=%v", outcome, pair, closed, len(posts), err)
						}
						var remaining ByteCount
						server.Raise(tx.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&remaining))
						if remaining != 1000 {
							t.Fatalf("rejected legacy reports changed payer credit: %d", remaining)
						}
					}, server.TxReadCommitted)
					if reserved := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; reserved != 700 {
						t.Fatalf("rejected legacy reports changed reservation: %d", reserved)
					}
				}
			}
		}
	})
}
