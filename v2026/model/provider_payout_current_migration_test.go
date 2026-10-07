// Current-main Redis reservations feed the same immutable settled-work
// snapshot and earning boundary as the payout branch's historical fixtures.
package model

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Complete real paid/free contracts before choosing a synthetic boundary on
// either side of their saved close times. No clock race or terminal-row edit
// supplies the earning classification; both public close paths run unchanged.
func currentPayoutRedisSettlements(t testing.TB, ctx context.Context, legacy bool) ([]*payoutTransitionCohort, time.Time, time.Time) {
	t.Helper()
	cohorts := []*payoutTransitionCohort{newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx)}
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=0 WHERE balance_id=$1`, cohorts[1].balance))
	})
	var firstClose, lastClose time.Time
	for _, f := range cohorts {
		var creditBefore ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balance).Scan(&creditBefore))
		})
		escrow, err := CreateTransferEscrow(ctx, f.sourceNetwork, f.source, f.network, f.client, 1024)
		if err != nil || escrow == nil {
			t.Fatal("current Redis escrow creation failed", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var redisReserved bool
			server.Raise(conn.QueryRow(ctx, `SELECT redis_reserved FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2`, escrow.ContractId, f.balance).Scan(&redisReserved))
			if !redisReserved {
				t.Fatal("current contract silently used legacy reservation")
			}
		})
		if reserved := Testing_NetEscrowByteCount(ctx, f.balance); reserved != 1024 {
			t.Fatal("missing actual Redis reservation", reserved)
		}
		if err := CloseContract(ctx, escrow.ContractId, f.source, 1024, false); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, escrow.ContractId, f.client, 1024, false); err != nil {
			t.Fatal(err)
		}
		if err := SettleEscrow(ctx, escrow.ContractId, ContractOutcomeSettled); err != nil {
			t.Fatal("settlement replay failed", err)
		}

		credit, pending, applied := asyncDebitTestState(t, ctx, f.balance)
		if credit != creditBefore || pending != 1 || applied != 0 || Testing_NetEscrowByteCount(ctx, f.balance) != 1024 {
			t.Fatal("paid/free consumption lost its deferred debit")
		}
		if n, released, busy, err := flushTransferDebitBalance(ctx, f.balance); err != nil || busy || n != 1 || released != 1 {
			t.Fatal("paid/free debit failed to drain", n, released, busy, err)
		}
		credit, pending, applied = asyncDebitTestState(t, ctx, f.balance)
		if credit != creditBefore-1024 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balance) != 0 {
			t.Fatal("paid/free writeback changed payer consumption")
		}

		server.Db(ctx, func(conn server.PgConn) {
			var closedAt time.Time
			var outcome string
			var sweepCount int
			var sweepBytes int64
			server.Raise(conn.QueryRow(ctx, `SELECT close_time,outcome FROM transfer_contract WHERE contract_id=$1`, escrow.ContractId).Scan(&closedAt, &outcome))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=$1`, escrow.ContractId).Scan(&sweepCount, &sweepBytes))
			if outcome != "settled" || sweepCount != 1 || sweepBytes != 1024 {
				t.Fatal("replayed close changed retained allocation", outcome, sweepCount, sweepBytes)
			}
			if firstClose.IsZero() || closedAt.Before(firstClose) {
				firstClose = closedAt
			}
			if closedAt.After(lastClose) {
				lastClose = closedAt
			}
		})
	}
	cutoff := firstClose.Truncate(time.Second).Add(-time.Hour)
	if legacy {
		cutoff = lastClose.Truncate(time.Second).Add(time.Hour)
	}
	data := []byte(fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", cutoff.Format(time.RFC3339), "0x"+strings.Repeat("11", 32)))
	t.Cleanup(server.Config.PushSimpleResource("sn.yml", data))
	policy, err := server.LoadProviderPayoutTransition(ctx)
	if err != nil || policy == nil {
		t.Fatal("synthetic earning declaration failed", err)
	}
	if _, err := server.PrepareProviderPayoutBoundary(ctx, policy.ConfigSha256); err != nil {
		t.Fatal(err)
	}
	return cohorts, firstClose.Add(-time.Hour), lastClose.Add(time.Hour)
}

// Neither zero revenue nor an asynchronous Redis release changes completed
// provider work. Post-boundary usage stays out of a legacy payment plan.
func TestProviderCurrentMigrationRedisPaidFreeUsageRemainsSubnetOnly(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		cohorts, start, end := currentPayoutRedisSettlements(t, ctx, false)
		usages, err := GetStEpochProviderUsageAtEpoch(ctx, 9, start, end)
		if err != nil || len(usages) != 2 {
			t.Fatal("current settlement lost paid/free subnet usage", len(usages), err)
		}
		got := map[server.Id]int64{}
		for _, usage := range usages {
			got[usage.ClientId] = usage.PayoutByteCount
		}
		for _, f := range cohorts {
			if got[f.client] != 1024 {
				t.Fatal("free and paid work have different subnet weight", got)
			}
		}
		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil || plan == nil || len(plan.NetworkPayments) != 0 {
			t.Fatal("post-boundary Redis work entered legacy payment plan", plan, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var unpaid int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow_sweep WHERE payment_id IS NULL`).Scan(&unpaid))
			if unpaid != 2 {
				t.Fatal("subnet usage was consumed by legacy planning", unpaid)
			}
		})
	})
}

// Pre-boundary work still reaches the actual USDC planner after Redis close
// and replay, without becoming a second subnet earning.
func TestProviderCurrentMigrationRedisLegacySettlementKeepsAsset(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		cohorts, start, end := currentPayoutRedisSettlements(t, ctx, true)
		usages, err := GetStEpochProviderUsageAtEpoch(ctx, 9, start, end)
		if err != nil || len(usages) != 0 {
			t.Fatal("legacy Redis work became subnet earning", usages, err)
		}
		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil || plan == nil {
			t.Fatal("legacy Redis work was not planned", err)
		}
		payment := plan.NetworkPayments[cohorts[0].network]
		if payment == nil || payment.PayoutByteCount != 1024 || payment.Payout <= 0 {
			t.Fatal("legacy paid work lost its retained amount", payment)
		}
		if err := RequireProviderUsdcPayment(ctx, payment.PaymentId); err != nil {
			t.Fatal("legacy Redis payment admission refused", err)
		}
		second, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil || second == nil || len(second.NetworkPayments) != 0 {
			t.Fatal("settlement replay paid the same work twice", second, err)
		}
	})
}
