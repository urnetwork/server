package model

// ----- App Store refund reversals -----
//
// Owner, 2026-11-17, on REFUND_REVERSED: "Fix it." Apple reverses a refund it
// granted when the dispute behind it goes the developer's way, and asks the
// app to reinstate what the refund took. The REFUND notification ended the
// transaction's paid window (EndReconciledEntitlementForTransactionsInTx); the
// reversal gives back what is left of it, the way a won Stripe dispute does
// (RestoreDisputedStripeEntitlementInTx).

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

// The App Store Server API's subscription status for a refunded or revoked
// transaction (the status of a lastTransactions entry).
const appleStoreStatusRevoked = 5

// Whether a refund of the App Store transaction ended its paid window here:
// the REFUND notification's revoked event says it ended something, or the
// reconcile ended the transaction's window when the store reported it revoked
// (status 5), before the notification came. A window that ended for another
// reason (a family-sharing REVOKE, an expiry, a move) is not the refund's to
// give back. Dry runs ended nothing.
func AppleRefundEndedEntitlementInTx(tx server.PgTx, ctx context.Context, transactionId string) (ended bool) {
	result, err := tx.Query(
		ctx,
		`
		SELECT action, details FROM payment_reconciliation_event
		WHERE store = $1
		  AND action = ANY($2::varchar[])
		  AND evidence = $3
		  AND NOT dry_run
		`,
		SubscriptionMarketApple,
		[]string{PaymentReconcileActionRevoked, PaymentReconcileActionEnded},
		transactionId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var action string
			var details *string
			server.Raise(result.Scan(&action, &details))
			if details != nil && appleRefundEndedEvent(action, *details) {
				ended = true
			}
		}
	})
	return
}

// Whether one payment reconciliation event of an App Store transaction records
// that a refund ended its window.
func appleRefundEndedEvent(action string, details string) bool {
	var parsed struct {
		NotificationType string `json:"notification_type"`
		Ended            bool   `json:"ended"`
		Status           *int   `json:"status"`
	}
	if json.Unmarshal([]byte(details), &parsed) != nil {
		return false
	}
	switch action {
	case PaymentReconcileActionRevoked:
		return parsed.NotificationType == "REFUND" && parsed.Ended
	case PaymentReconcileActionEnded:
		return parsed.Status != nil && *parsed.Status == appleStoreStatusRevoked
	default:
		return false
	}
}

// Gives back the paid window an App Store refund ended, when Apple reversed the
// refund: the transaction's renewal on the network, the period's Pro marker
// matched by its cut window, and the Pro cycle grant the refund ended with the
// period, all get their ends back. The window is a paid one of this transaction:
// it records the transaction's paid end (purchases since the Pro grant cycles),
// or it is a row from before paid_end_time that starts with the purchase. It was
// ended early: its end is past and before endTime, the end the credit gives
// (expires plus the store grace).
//
// A cycle comes back only while the chain has not moved on: when another
// subscription started a cycle after the refund, the network has a running
// grant, and a second one would grant the same month twice. A balance that
// retention already removed is not recreated here; the caller's
// RepairReconciledProEntitlement adds its marker.
//
// It returns the restored renewal, or nil when there is no such window (the
// refund ended nothing here, or a redelivery found it restored). The caller has
// established that the refund was reversed and that the refund ended this
// transaction's window (AppleRefundEndedEntitlementInTx), and refreshes the pro
// cache after commit. The outer transaction must use OptNoRetry: an ownership
// refusal aborts it with the notification's ledger row, so Apple redelivers.
// ErrPaymentNetworkNotFound for a deleted network, which writes nothing.
func RestoreRefundedAppleEntitlementInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	transactionId string,
	purchaseTime time.Time,
	paidEndTime time.Time,
	endTime time.Time,
	now time.Time,
) (*ReconcileSubscriptionRenewal, error) {
	if err := LockPaymentNetworkInTx(tx, ctx, networkId); err != nil {
		return nil, err
	}

	var startTime time.Time
	var cutTime time.Time
	found := false
	result, err := tx.Query(
		ctx,
		`
		SELECT start_time, end_time FROM subscription_renewal
		WHERE network_id = $1
		  AND subscription_type = $2
		  AND market = $3
		  AND transaction_id = $4
		  AND (
			paid_end_time = $5 OR
			(paid_end_time IS NULL AND start_time = $6)
		  )
		  AND end_time <= $7
		  AND end_time < $8
		ORDER BY end_time DESC
		LIMIT 1
		FOR UPDATE
		`,
		networkId,
		SubscriptionTypeSupporter,
		SubscriptionMarketApple,
		transactionId,
		paidEndTime.UTC(),
		purchaseTime.UTC(),
		now.UTC(),
		endTime.UTC(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&startTime, &cutTime))
			found = true
		}
	})
	if !found {
		return nil, nil
	}

	// the period's balance, matched by its cut window; a Pro cycle grant has
	// its own window and comes back with its cycle, below
	balanceIds := []server.Id{}
	result, err = tx.Query(
		ctx,
		`
		SELECT balance_id FROM transfer_balance AS balance
		WHERE network_id = $1
		  AND pro
		  AND start_time = $2
		  AND end_time = $3
		  AND NOT EXISTS (
			SELECT 1 FROM pro_grant_cycle AS cycle
			WHERE cycle.network_id = balance.network_id
			  AND cycle.balance_id = balance.balance_id
		  )
		`,
		networkId,
		startTime,
		cutTime,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			server.Raise(result.Scan(&balanceId))
			balanceIds = append(balanceIds, balanceId)
		}
	})
	// the Pro cycle the refund ended with the period
	// (endReconciledEntitlementInTx), unless a later cycle started since
	cycleBalanceIds := []server.Id{}
	result, err = tx.Query(
		ctx,
		`
		SELECT cycle.balance_id
		FROM pro_grant_cycle AS cycle
		INNER JOIN transfer_balance AS balance ON balance.balance_id = cycle.balance_id
		WHERE cycle.network_id = $1
		  AND cycle.ended_time IS NOT NULL
		  AND balance.end_time = $2
		  AND NOT EXISTS (
			SELECT 1 FROM pro_grant_cycle AS later
			WHERE later.network_id = cycle.network_id
			  AND later.cycle_start_time > cycle.cycle_start_time
			  AND later.cycle_start_time >= cycle.ended_time
		  )
		`,
		networkId,
		cutTime,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			server.Raise(result.Scan(&balanceId))
			cycleBalanceIds = append(cycleBalanceIds, balanceId)
		}
	})
	admitted, ownershipErr := tryTransferBalanceOwnershipInTx(ctx, tx, append(slices.Clone(balanceIds), cycleBalanceIds...))
	server.Raise(ownershipErr)
	if !admitted {
		server.Raise(errTransferBalanceOwnershipBusy)
	}

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		UPDATE subscription_renewal SET end_time = $1
		WHERE network_id = $2
		  AND subscription_type = $3
		  AND market = $4
		  AND transaction_id = $5
		  AND start_time = $6
		  AND end_time = $7
		`,
		endTime.UTC(),
		networkId,
		SubscriptionTypeSupporter,
		SubscriptionMarketApple,
		transactionId,
		startTime,
		cutTime,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		UPDATE transfer_balance SET end_time = $1
		WHERE balance_id = ANY($2::uuid[])
		  AND end_time = $3
		`,
		endTime.UTC(),
		balanceIds,
		cutTime,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		WITH restored AS (
			UPDATE pro_grant_cycle AS cycle SET ended_time = NULL
			WHERE cycle.network_id = $1
			  AND cycle.balance_id = ANY($2::uuid[])
			  AND cycle.ended_time IS NOT NULL
			RETURNING cycle.balance_id, cycle.cycle_end_time
		)
		UPDATE transfer_balance AS balance SET end_time = restored.cycle_end_time + $4::interval
		FROM restored
		WHERE balance.balance_id = restored.balance_id
		  AND balance.end_time = $3
		`,
		networkId,
		cycleBalanceIds,
		cutTime,
		fmt.Sprintf("%d seconds", int64(ProGrantGrace/time.Second)),
	))
	return &ReconcileSubscriptionRenewal{
		NetworkId:     networkId,
		StartTime:     startTime,
		EndTime:       endTime.UTC(),
		TransactionId: transactionId,
	}, nil
}
