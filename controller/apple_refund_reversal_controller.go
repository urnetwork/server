package controller

// App Store REFUND_REVERSED (owner, 2026-11-17: "Fix it."): Apple reversed a
// refund it had granted, and asks the app to reinstate what the refund took.
// The REFUND ended the transaction's paid window; the reversal gives back the
// rest of it, as a won Stripe dispute does (stripeHandleDisputeClosed).
//
// ProcessAppleNotification verifies the notification through the same pinned
// root as every other one, resolves the transaction's network the same way (an
// offer-code binding, or the account a deleted account's subscription moved
// to), and gates it on the apple_notification ledger, so a redelivery does
// nothing. These are the rules it then applies in its transaction.

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The reads and writes the reversal rules make in the notification's
// transaction. The database implementation is appleRefundReversalDbTx; the
// hermetic tests use a fake.
type appleRefundReversalTx interface {
	// RefundEnded reports whether a refund of the transaction ended its paid
	// window here (model.AppleRefundEndedEntitlementInTx).
	RefundEnded(transactionId string) bool
	// Restore gives the transaction's network back the paid window the refund
	// ended (model.RestoreRefundedAppleEntitlementInTx), or returns nil when
	// there is no such window.
	Restore(transaction *validatedAppleTransaction, now time.Time) (*model.ReconcileSubscriptionRenewal, error)
	// RecordEvent writes the reversal's refund_reversed audit event.
	RecordEvent(transaction *validatedAppleTransaction, details map[string]any)
}

// Applies a verified REFUND_REVERSED of the transaction. The window comes back
// only when all of these hold; otherwise the event says which did not:
//   - a refund of this transaction ended its window here. A window that ended
//     for another reason (a family-sharing REVOKE, an expiry) stays ended, and
//     a reversal of a refund this server never processed changes nothing.
//   - the paid period is still running. Once it is over there is nothing left
//     to give back, and a renewal since then has its own window.
//   - the window is still the cut one: a second reversal of the same refund
//     finds it restored and changes nothing.
//
// Every applied reversal writes one refund_reversed event. It returns the
// restored renewal, or nil.
func appleReverseRefund(
	store appleRefundReversalTx,
	transaction *validatedAppleTransaction,
	notification AppleNotificationDecodedPayload,
	now time.Time,
) *model.ReconcileSubscriptionRenewal {
	reason := ""
	var restored *model.ReconcileSubscriptionRenewal
	switch {
	case !store.RefundEnded(transaction.transactionId):
		reason = "refund_ended_nothing"
	case !now.Before(transaction.expiresTime):
		reason = "paid_period_over"
	default:
		var err error
		restored, err = store.Restore(transaction, now)
		switch {
		case errors.Is(err, model.ErrPaymentNetworkNotFound):
			reason = "network_deleted"
		case err != nil:
			// the event and the restore commit together or not at all; Apple
			// redelivers the notification
			server.Raise(err)
		case restored == nil:
			reason = "window_not_found"
		}
	}

	details := map[string]any{
		"notification_type": notification.NotificationType,
		"subtype":           notification.Subtype,
		"restored":          restored != nil,
		"expires_date":      transaction.expiresTime.UTC().Format(time.RFC3339),
	}
	if transaction.originalTransactionId != "" {
		details["original_transaction"] = transaction.originalTransactionId
	}
	if reason != "" {
		details["reason"] = reason
	}
	store.RecordEvent(transaction, details)
	return restored
}

// appleRefundReversalTx in the notification's ReadCommitted, no-retry
// transaction.
type appleRefundReversalDbTx struct {
	tx  server.PgTx
	ctx context.Context
}

func (self *appleRefundReversalDbTx) RefundEnded(transactionId string) bool {
	return model.AppleRefundEndedEntitlementInTx(self.tx, self.ctx, transactionId)
}

func (self *appleRefundReversalDbTx) Restore(transaction *validatedAppleTransaction, now time.Time) (*model.ReconcileSubscriptionRenewal, error) {
	restored, err := model.RestoreRefundedAppleEntitlementInTx(
		self.tx,
		self.ctx,
		transaction.networkId,
		transaction.transactionId,
		transaction.purchaseTime,
		transaction.expiresTime,
		transaction.expiresTime.Add(SubscriptionGracePeriod),
		now,
	)
	if err != nil || restored == nil {
		return restored, err
	}
	// the cycle due now, when the refund's cycle did not come back with the
	// window (a credit path's step; the hourly sweep would grant it within the
	// hour)
	model.GrantProCycleInTx(self.tx, self.ctx, transaction.networkId)
	return restored, nil
}

func (self *appleRefundReversalDbTx) RecordEvent(transaction *validatedAppleTransaction, details map[string]any) {
	networkId := transaction.networkId
	if err := model.AddPaymentReconciliationEventInTx(self.tx, self.ctx, &model.PaymentReconciliationEvent{
		RunId:     server.NewId(),
		Store:     model.SubscriptionMarketApple,
		NetworkId: &networkId,
		Action:    model.PaymentReconcileActionRefundReversed,
		Evidence:  transaction.transactionId,
		Details:   details,
	}); err != nil {
		// the event commits with the restore or neither does; Apple redelivers
		server.Raise(err)
	}
}
