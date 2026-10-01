// Pins financial posts, reservations, and rejection through the actual owner.
// The legacy control replaces only its read; the owning arm runs unchanged.
package model

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// Delegates all transaction operations except the exact escrow read under test.
type settlementReadQueryTx struct {
	server.PgTx
	original string
	replace  string
	reads    int
}

// Counts the owner read even when the control retains its original query.
func (self *settlementReadQueryTx) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if query == self.original {
		self.reads++
		if self.replace != "" {
			query = self.replace
		}
	}
	return self.PgTx.Query(ctx, query, args...)
}

// Expected amounts are explicit financial controls, independent of query shape.
type settlementReadBalanceCase struct {
	reserved, start, revenue, payout, payoutRevenue int64
	expiry                                          int
	missing                                         bool
}

// Compares persisted and mirrored results without including generated identities.
type settlementReadBalanceState struct {
	settled, settleTime, balanceFound, siblingSettled bool
	payout, balance, sweptBytes, sweptRevenue         int64
	siblingPayout, reservation                        int64
}

// Uses non-monotonic expiry and unequal prices to expose an ordering change.
func TestSettlementReadPreservesFinancialPosts(t *testing.T) {
	original := settlementEscrowReadSql
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		mixed := []settlementReadBalanceCase{
			{reserved: 256, start: 1024, revenue: 2000, expiry: 4},
			{reserved: 96, start: 1024, revenue: 8000, expiry: 1, payout: 96, payoutRevenue: 375},
			{reserved: 128, start: 2048, revenue: 18000, expiry: 2, payout: 80, payoutRevenue: 352},
			{reserved: 0, start: 1024, revenue: 6000, expiry: 0},
			{reserved: 4096, missing: true},
		}
		for _, test := range []struct {
			name                string
			source, destination int64
			reject              bool
		}{
			{name: "ordered_mixed_prices_and_missing_balance", source: 160, destination: 192},
			{name: "zero_use_releases_all_reservations"},
			{name: "missing_balance_cannot_fund_rejection", source: 512, destination: 512, reject: true},
		} {
			balances := append([]settlementReadBalanceCase(nil), mixed...)
			if test.source == 0 || test.reject {
				for index := range balances {
					balances[index].payout, balances[index].payoutRevenue = 0, 0
				}
			}
			control := settlementReadFinancialArm(t, original, settlementReadLegacySql, balances, test.source, test.destination, test.reject)
			candidate := settlementReadFinancialArm(t, original, "", balances, test.source, test.destination, test.reject)
			if !reflect.DeepEqual(control, candidate) {
				t.Errorf("%s changed actual-model financial results", test.name)
			}
			t.Logf("financial parity case=%s rows=%d", test.name, len(balances))
		}
	})
}

// More than a thousand nonzero rows must all be paid and released; a row cap
// would underfund the contract before it could claim the outcome.
func TestSettlementReadPreservesUncappedPosts(t *testing.T) {
	original := settlementEscrowReadSql
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		balances := make([]settlementReadBalanceCase, 1025)
		for index := range balances {
			balances[index] = settlementReadBalanceCase{reserved: 1, start: 1, revenue: 2, payout: 1, payoutRevenue: 1, expiry: index}
		}
		control := settlementReadFinancialArm(t, original, settlementReadLegacySql, balances, 1025, 1025, false)
		candidate := settlementReadFinancialArm(t, original, "", balances, 1025, 1025, false)
		if !reflect.DeepEqual(control, candidate) {
			t.Fatal("uncapped actual-model financial results changed")
		}
	})
}

// Fully consuming unequal-price equal-expiry rows pins exact money without
// imposing an internal tie order that the existing SQL never specified.
func TestSettlementReadPreservesExpiryTies(t *testing.T) {
	original := settlementEscrowReadSql
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		balances := []settlementReadBalanceCase{
			{reserved: 128, start: 1024, revenue: 6000, expiry: 1, payout: 128, payoutRevenue: 375},
			{reserved: 256, start: 1024, revenue: 8000, expiry: 1, payout: 256, payoutRevenue: 1000},
			{reserved: 0, start: 1024, revenue: 10000, expiry: 1},
		}
		control := settlementReadFinancialArm(t, original, settlementReadLegacySql, balances, 384, 384, false)
		candidate := settlementReadFinancialArm(t, original, "", balances, 384, 384, false)
		if !reflect.DeepEqual(control, candidate) {
			t.Fatal("equal-expiry actual-model financial results changed")
		}
	})
}

// Runs the real owner, commits its claim, executes all posts, and repeats the
// attempt to prove that terminal claims suppress duplicate money and releases.
func settlementReadFinancialArm(
	t testing.TB,
	original, replacement string,
	balances []settlementReadBalanceCase,
	source, destination int64,
	reject bool,
) []settlementReadBalanceState {
	t.Helper()
	ctx := context.Background()
	fixture := newForceCloseDisputeFixture(t, ctx, true, true, source, destination, 0)
	balanceIds := make([]server.Id, len(balances))
	balanceIds[0] = fixture.balanceId
	for index := 1; index < len(balanceIds); index++ {
		balanceIds[index] = server.NewId()
	}
	siblingId := server.NewId()
	const siblingReservation = int64(17)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET checkpoint=false WHERE contract_id=$1`, fixture.contractId))
		// The surviving sibling needs an open durable contract. Orphan escrow
		// rows and Redis-only counters do not reserve database-owned credit.
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
			(contract_id,source_network_id,source_id,destination_network_id,destination_id,
			transfer_byte_count,payer_network_id)
			SELECT $2,source_network_id,source_id,destination_network_id,destination_id,$3,payer_network_id
			FROM transfer_contract WHERE contract_id=$1`, fixture.contractId, siblingId, siblingReservation*int64(len(balances))))
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for index, balance := range balances {
				balanceId := balanceIds[index]
				endTime := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(balance.expiry) * time.Hour)
				if !balance.missing {
					if index == 0 {
						batch.Queue(`UPDATE transfer_balance SET start_balance_byte_count=$2,
							balance_byte_count=$2,net_revenue_nano_cents=$3,end_time=$4 WHERE balance_id=$1`,
							balanceId, balance.start, balance.revenue, endTime)
					} else {
						batch.Queue(`INSERT INTO transfer_balance (balance_id,network_id,start_time,end_time,
							start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,
							subsidy_net_revenue_nano_cents,pro)
							SELECT $2,network_id,start_time,$3,$4,$4,$5,0,false
							FROM transfer_balance WHERE balance_id=$1`,
							fixture.balanceId, balanceId, endTime, balance.start, balance.revenue)
					}
				}
				batch.Queue(`INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
					VALUES($1,$2,$3) ON CONFLICT(contract_id,balance_id) DO UPDATE SET balance_byte_count=$3`,
					fixture.contractId, balanceId, balance.reserved)
				batch.Queue(`INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
					VALUES($1,$2,$3)`, siblingId, balanceId, siblingReservation)
			}
		})
	}, server.TxReadCommitted)
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for index, balance := range balances {
				pipe.Set(ctx, netEscrowKey(balanceIds[index]), balance.reserved+siblingReservation, 0)
			}
			return nil
		})
		server.Raise(err)
	})
	var first []settlementReadBalanceState
	for attempt := range 2 {
		var posts []func() any
		var closed bool
		var settleErr error
		server.Tx(ctx, func(tx server.PgTx) {
			queryTx := &settlementReadQueryTx{PgTx: tx, original: original, replace: replacement}
			posts, closed, settleErr = settleEscrowInTx(ctx, queryTx, fixture.contractId, ContractOutcomeSettled)
			if queryTx.reads != 1 {
				t.Fatal("exact owning settlement read was not exercised once")
			}
		}, server.TxReadCommitted)
		if reject {
			if !errors.Is(settleErr, errContractInsufficientEscrow) || closed || len(posts) != 0 {
				t.Fatal("underfunded actual-model settlement changed its rejection boundary")
			}
		} else {
			if settleErr != nil || closed != (attempt == 0) || (attempt == 1 && len(posts) != 0) {
				t.Fatal("actual-model settlement claim or repeat behavior changed")
			}
			server.RunPosts(ctx, posts...)
		}
		states := make([]settlementReadBalanceState, len(balances))
		server.Db(ctx, func(conn server.PgConn) {
			var terminal bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1`, fixture.contractId).Scan(&terminal))
			if terminal == reject {
				t.Fatal("financial attempt changed the contract outcome incorrectly")
			}
			for index, balanceId := range balanceIds {
				state := &states[index]
				server.Raise(conn.QueryRow(ctx, `SELECT e.settled,e.settle_time IS NOT NULL,
					coalesce(e.payout_byte_count,0),b.balance_id IS NOT NULL,coalesce(b.balance_byte_count,0),
					coalesce(s.payout_byte_count,0),coalesce(s.payout_net_revenue_nano_cents,0),
					sibling.settled,coalesce(sibling.payout_byte_count,0)
					FROM transfer_escrow e LEFT JOIN transfer_balance b USING(balance_id)
					LEFT JOIN transfer_escrow_sweep s ON s.contract_id=e.contract_id AND s.balance_id=e.balance_id AND s.network_id=$4
					JOIN transfer_escrow sibling ON sibling.contract_id=$3 AND sibling.balance_id=e.balance_id
					WHERE e.contract_id=$1 AND e.balance_id=$2`, fixture.contractId, balanceId, siblingId, fixture.providerNetworkId).Scan(
					&state.settled, &state.settleTime, &state.payout, &state.balanceFound, &state.balance,
					&state.sweptBytes, &state.sweptRevenue, &state.siblingSettled, &state.siblingPayout))
			}
		})
		server.Redis(ctx, func(r server.RedisClient) {
			var wantBytes, wantRevenue int64
			for index, balance := range balances {
				state := &states[index]
				server.Raise(r.Get(ctx, netEscrowKey(balanceIds[index])).Scan(&state.reservation))
				wantSettled := !reject && !balance.missing
				wantReservation := siblingReservation
				if reject || balance.missing {
					wantReservation += balance.reserved
				}
				if state.settled != wantSettled || state.settleTime != wantSettled ||
					state.balanceFound == balance.missing || state.payout != balance.payout ||
					state.balance != balance.start-balance.payout || state.sweptBytes != balance.payout ||
					state.sweptRevenue != balance.payoutRevenue || state.reservation != wantReservation ||
					state.siblingSettled || state.siblingPayout != 0 {
					t.Fatalf("financial result mismatch at synthetic balance index %d", index)
				}
				wantBytes += balance.payout
				wantRevenue += balance.payoutRevenue
			}
			for key, want := range map[string]int64{
				accountBalanceNetPayoutByteCountKey(fixture.providerNetworkId): wantBytes,
				accountBalanceNetPayout(fixture.providerNetworkId):             wantRevenue,
			} {
				got, err := r.Get(ctx, key).Int64()
				if (err != nil && !(want == 0 && errors.Is(err, redis.Nil))) || got != want {
					t.Fatal("provider account posts changed financial totals")
				}
			}
		})
		if attempt == 0 {
			first = states
		} else if !reflect.DeepEqual(first, states) {
			t.Fatal("repeat settlement changed financial state")
		}
	}
	return first
}
