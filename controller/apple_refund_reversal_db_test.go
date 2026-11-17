package controller

// App Store REFUND_REVERSED through ProcessAppleNotification against the test
// database (apple_refund_reversal_controller.go): the refund ends the window,
// the reversal gives it back once, a reversal of a refund this server never
// processed changes nothing, and a later subscription's Pro cycle is not
// granted twice. apple_refund_reversal_test.go pins the rules without a
// database.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func appleReversalDbClaims(networkId server.Id, transactionId string, purchase time.Time, period time.Duration) map[string]any {
	return map[string]any{
		"appAccountToken":       networkId.String(),
		"transactionId":         transactionId,
		"originalTransactionId": "orig-" + transactionId,
		"productId":             appleRefundTestProductId,
		"purchaseDate":          float64(purchase.UnixMilli()),
		"expiresDate":           float64(purchase.Add(period).UnixMilli()),
		"price":                 float64(4990),
		"currency":              "USD",
	}
}

func appleReversalDbNotify(t testing.TB, ctx context.Context, notificationType string, notificationUuid string, claims map[string]any) {
	_, err := ProcessAppleNotification(ctx, AppleNotificationDecodedPayload{
		NotificationType: notificationType,
		NotificationUUID: notificationUuid,
		SignedDate:       server.NowUtc().UnixMilli(),
		TransactionInfo:  claims,
	}, []string{appleRefundTestProductId})
	connect.AssertEqual(t, err, nil)
}

func appleReversalDbOpenCycleCount(t testing.TB, ctx context.Context, networkId server.Id) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM pro_grant_cycle WHERE network_id = $1 AND ended_time IS NULL`,
			networkId,
		).Scan(&count))
	})
	return count
}

func TestProcessAppleNotificationRefundReversedRestoresEntitlement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "apple-reversal-test", server.NewId())
		transactionId := "apple-reversal-" + server.NewId().String()
		claims := appleReversalDbClaims(networkId, transactionId, server.NowUtc().Truncate(time.Millisecond).Add(-time.Hour), 30*24*time.Hour)

		appleReversalDbNotify(t, ctx, "SUBSCRIBED", server.NewId().String(), claims)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		appleReversalDbNotify(t, ctx, "REFUND", server.NewId().String(), claims)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketApple), 0)

		reversalUuid := server.NewId().String()
		appleReversalDbNotify(t, ctx, "REFUND_REVERSED", reversalUuid, claims)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketApple), 1)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)) > 0, true)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionRefundReversed, transactionId),
			1,
		)

		// Apple redelivers the same notification: absorbed by its ledger
		appleReversalDbNotify(t, ctx, "REFUND_REVERSED", reversalUuid, claims)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionRefundReversed, transactionId),
			1,
		)
		// a second notification of the same reversal restores nothing more
		appleReversalDbNotify(t, ctx, "REFUND_REVERSED", server.NewId().String(), claims)
		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketApple), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(t, appleReversalDbOpenCycleCount(t, ctx, networkId) <= 1, true)

		// and the next renewal credits as usual
		renewalId := "apple-reversal-renewal-" + server.NewId().String()
		appleReversalDbNotify(t, ctx, "DID_RENEW", server.NewId().String(), appleReversalDbClaims(networkId, renewalId, server.NowUtc().Truncate(time.Millisecond), 30*24*time.Hour))
		creditedNetworkId, credited := model.GetAppleTransactionNetworkId(ctx, renewalId)
		connect.AssertEqual(t, credited, true)
		connect.AssertEqual(t, creditedNetworkId, networkId)
	})
}

func TestProcessAppleNotificationRefundReversedIgnoresUnrefundedTransactions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		purchase := server.NowUtc().Truncate(time.Millisecond).Add(-time.Hour)

		// a family-sharing REVOKE ended it, not a refund: it stays ended
		revokedNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, revokedNetworkId, "apple-reversal-revoked", server.NewId())
		revokedId := "apple-reversal-revoked-" + server.NewId().String()
		revokedClaims := appleReversalDbClaims(revokedNetworkId, revokedId, purchase, 30*24*time.Hour)
		appleReversalDbNotify(t, ctx, "SUBSCRIBED", server.NewId().String(), revokedClaims)
		appleReversalDbNotify(t, ctx, "REVOKE", server.NewId().String(), revokedClaims)
		appleReversalDbNotify(t, ctx, "REFUND_REVERSED", server.NewId().String(), revokedClaims)
		connect.AssertEqual(t, model.IsProNetwork(ctx, revokedNetworkId), false)
		connect.AssertEqual(t, activeRenewalCount(t, ctx, revokedNetworkId, model.SubscriptionMarketApple), 0)

		// a transaction this server never credited or refunded
		unknownNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, unknownNetworkId, "apple-reversal-unknown", server.NewId())
		unknownId := "apple-reversal-unknown-" + server.NewId().String()
		appleReversalDbNotify(t, ctx, "REFUND_REVERSED", server.NewId().String(), appleReversalDbClaims(unknownNetworkId, unknownId, purchase, 30*24*time.Hour))
		connect.AssertEqual(t, model.IsProNetwork(ctx, unknownNetworkId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, unknownNetworkId)), 0)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionRefundReversed, unknownId),
			1,
		)
	})
}

// A yearly subscription is refunded, the customer subscribes again, and then
// Apple reverses the refund: the refunded window comes back, and the refund's
// Pro cycle stays ended, since the new subscription's cycle runs.
func TestProcessAppleNotificationRefundReversedAfterANewSubscription(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "apple-reversal-later", server.NewId())
		now := server.NowUtc().Truncate(time.Millisecond)

		yearlyId := "apple-reversal-yearly-" + server.NewId().String()
		yearlyClaims := appleReversalDbClaims(networkId, yearlyId, now.Add(-2*time.Hour), 365*24*time.Hour)
		appleReversalDbNotify(t, ctx, "SUBSCRIBED", server.NewId().String(), yearlyClaims)
		appleReversalDbNotify(t, ctx, "REFUND", server.NewId().String(), yearlyClaims)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)

		monthlyId := "apple-reversal-monthly-" + server.NewId().String()
		appleReversalDbNotify(t, ctx, "SUBSCRIBED", server.NewId().String(), appleReversalDbClaims(networkId, monthlyId, now.Add(-time.Hour), 30*24*time.Hour))
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		openCycles := appleReversalDbOpenCycleCount(t, ctx, networkId)

		appleReversalDbNotify(t, ctx, "REFUND_REVERSED", server.NewId().String(), yearlyClaims)
		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketApple), 2)
		connect.AssertEqual(t, appleReversalDbOpenCycleCount(t, ctx, networkId), openCycles)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	})
}
