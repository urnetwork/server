package controller

// Tests for the Apple REFUND / REVOKE clawback (UPGRADE.md §2 S7): the same
// verified-notification path as SUBSCRIBED/DID_RENEW, ending the transaction's
// renewal and the pro balance it granted while the
// apple_subscription_transaction ledger row stays as history.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const appleRefundTestProductId = "supporter_monthly_26"

func appleRefundTestSubscribe(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	transactionId string,
) {
	now := server.NowUtc().Truncate(time.Millisecond)
	processed, err := ProcessAppleNotification(ctx, AppleNotificationDecodedPayload{
		NotificationType: "SUBSCRIBED",
		Subtype:          "INITIAL_BUY",
		NotificationUUID: server.NewId().String(),
		SignedDate:       now.UnixMilli(),
		TransactionInfo: map[string]any{
			"appAccountToken": networkId.String(),
			"transactionId":   transactionId,
			"productId":       appleRefundTestProductId,
			"purchaseDate":    float64(now.Add(-time.Hour).UnixMilli()),
			"expiresDate":     float64(now.Add(30 * 24 * time.Hour).UnixMilli()),
			"price":           float64(4990),
		},
	}, []string{appleRefundTestProductId})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, processed, true)
	connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
}

// appleRevocationNotification builds a REFUND/REVOKE payload: the same
// verified transaction claims, WITHOUT the entitlement fields (price/dates) a
// revocation payload need not carry.
func appleRevocationNotification(
	notificationType string,
	notificationUuid string,
	networkId server.Id,
	transactionId string,
) AppleNotificationDecodedPayload {
	return AppleNotificationDecodedPayload{
		NotificationType: notificationType,
		NotificationUUID: notificationUuid,
		SignedDate:       server.NowUtc().UnixMilli(),
		TransactionInfo: map[string]any{
			"appAccountToken": networkId.String(),
			"transactionId":   transactionId,
			"productId":       appleRefundTestProductId,
		},
	}
}

// TestProcessAppleNotificationRefundEndsEntitlement pins REFUND: entitlement
// ended (renewal + pro balance), transaction ledger row kept as history, one
// revoked operator event -- and a redelivery of the same notification is
// absorbed by the notification ledger without a second event.
func TestProcessAppleNotificationRefundEndsEntitlement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "apple-refund-test", userId)

		transactionId := "apple-refund-" + server.NewId().String()
		appleRefundTestSubscribe(t, ctx, networkId, transactionId)

		refundUuid := server.NewId().String()
		refund := appleRevocationNotification("REFUND", refundUuid, networkId, transactionId)
		processed, err := ProcessAppleNotification(ctx, refund, []string{appleRefundTestProductId})
		connect.AssertEqual(t, err, nil)
		// a revocation never counts as a NEW entitlement
		connect.AssertEqual(t, processed, false)

		// entitlement over: renewal ended, pro balance ended, pro state refreshed
		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketApple), 0)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)

		// the transaction ledger row STAYS as history
		connect.AssertEqual(t, model.IsAppleTransactionCredited(ctx, transactionId), true)

		// one operator event
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionRevoked, transactionId),
			1,
		)

		// Apple redelivers the same notification UUID: absorbed, no second event
		processed, err = ProcessAppleNotification(ctx, refund, []string{appleRefundTestProductId})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, false)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionRevoked, transactionId),
			1,
		)
	})
}

// TestProcessAppleNotificationRevokeEndsEntitlement pins REVOKE (family
// sharing revocation) through the same clawback, and that a revocation for an
// entitlement already over ends nothing twice.
func TestProcessAppleNotificationRevokeEndsEntitlement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "apple-revoke-test", userId)

		transactionId := "apple-revoke-" + server.NewId().String()
		appleRefundTestSubscribe(t, ctx, networkId, transactionId)

		revoke := appleRevocationNotification("REVOKE", server.NewId().String(), networkId, transactionId)
		processed, err := ProcessAppleNotification(ctx, revoke, []string{appleRefundTestProductId})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, false)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		// a REFUND arriving after the REVOKE (new UUID, same transaction) finds
		// nothing left to end -- one clawback total, though the second
		// notification still records its own audit row (ended = false)
		refund := appleRevocationNotification("REFUND", server.NewId().String(), networkId, transactionId)
		processed, err = ProcessAppleNotification(ctx, refund, []string{appleRefundTestProductId})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
	})
}
