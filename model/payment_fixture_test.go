package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// testingCreatePaymentNetworkRow inserts only the network domain row required
// by paid-credit writers, without adding a network_user to subsidy counts.
func testingCreatePaymentNetworkRow(ctx context.Context, networkId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network (network_id, network_name, admin_user_id)
			 VALUES ($1, $2, $3)`,
			networkId,
			"synthetic-payment-"+networkId.String(),
			server.NewId(),
		))
	})
}

// testingCreatePaymentClient adds the domain-only network plus one client used
// by synthetic contract-payment tests, without adding a network_user.
func testingCreatePaymentClient(ctx context.Context, networkId, clientId server.Id) {
	testingCreatePaymentNetworkRow(ctx, networkId)
	Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic-payment-client", "synthetic")
}

// testingRedeemPaymentBalanceCode requires a successful synthetic redemption.
func testingRedeemPaymentBalanceCode(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	secret string,
) {
	t.Helper()
	result, err := RedeemBalanceCode(&RedeemBalanceCodeArgs{
		Secret:    secret,
		NetworkId: networkId,
	}, ctx)
	if err != nil {
		t.Fatalf("redeem synthetic balance code: %v", err)
	}
	if result == nil {
		t.Fatal("redeem synthetic balance code returned no result")
	}
	if result.Error != nil {
		t.Fatalf("redeem synthetic balance code: %s", result.Error.Message)
	}
	if result.TransferBalance == nil {
		t.Fatal("redeem synthetic balance code returned no transfer balance")
	}
}

// Records one payment of the network that owns its unpaid sweeps of the
// contracts, as the payout planner assigned them, and returns its id: completed
// at completeTime, with a synthetic record and tx hash as the USDC sender left
// it, or pending when completeTime is nil. Its payout byte count and payout are
// the sweeps'. USDC payouts ended (owner decisions 2026-11-12 and 2026-11-13),
// so nothing plans or completes a payment any more; these rows are the history
// the readers and retention still see.
func testingAssignSweepPayment(ctx context.Context, networkId server.Id, contractIds []server.Id, completeTime *time.Time) (paymentId server.Id) {
	paymentId = server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		var completedTime *time.Time
		var record, txHash *string
		if completeTime != nil {
			utc := completeTime.UTC()
			completedTime = &utc
			syntheticRecord := "synthetic-record-" + paymentId.String()
			syntheticTxHash := "synthetic-tx-" + paymentId.String()
			record, txHash = &syntheticRecord, &syntheticTxHash
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO account_payment (
				payment_id, payment_plan_id, network_id, payout_byte_count, payout_nano_cents, min_sweep_time,
				completed, complete_time, payment_record, tx_hash
			)
			SELECT
				$1, $2, $3,
				COALESCE(SUM(payout_byte_count), 0), COALESCE(SUM(payout_net_revenue_nano_cents), 0),
				COALESCE(MIN(sweep_time), now() AT TIME ZONE 'UTC'),
				$5 IS NOT NULL, $5, $6, $7
			FROM transfer_escrow_sweep
			WHERE network_id = $3 AND contract_id = ANY($4) AND payment_id IS NULL
			`,
			paymentId, server.NewId(), networkId, contractIds, completedTime, record, txHash,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE transfer_escrow_sweep SET payment_id = $1 WHERE network_id = $2 AND contract_id = ANY($3) AND payment_id IS NULL`,
			paymentId, networkId, contractIds,
		))
	})
	return
}

// Records the wallet as the network's payout wallet, as a selection made while
// USDC payouts ran left it. Nothing selects one any more (owner decisions
// 2026-11-12 and 2026-11-13).
func testingSelectPayoutWallet(ctx context.Context, networkId server.Id, walletId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO payout_wallet (network_id, wallet_id) VALUES ($1, $2)
			ON CONFLICT (network_id) DO UPDATE SET wallet_id = $2
			`,
			networkId,
			walletId,
		))
	})
}
