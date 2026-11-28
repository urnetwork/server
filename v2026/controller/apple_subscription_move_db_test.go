package controller

// The App Store subscription move of a deleted account (support inbox 2489;
// apple_subscription_move_controller.go) against the test database: the
// deletion's own record, the restore that moves the paid window, the renewal
// webhook after the move, and the refusals that stay. The hermetic tests in
// apple_subscription_move_test.go pin the rules without a database.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func appleMoveDbClaims(tokenNetworkId server.Id, originalTransactionId string, transactionId string, purchase time.Time) map[string]any {
	return map[string]any{
		"appAccountToken":       tokenNetworkId.String(),
		"transactionId":         transactionId,
		"originalTransactionId": originalTransactionId,
		"productId":             "supporter_monthly_26",
		"purchaseDate":          float64(purchase.UnixMilli()),
		"expiresDate":           float64(purchase.Add(30 * 24 * time.Hour).UnixMilli()),
		"price":                 float64(4990),
		"currency":              "USD",
	}
}

func TestAppleRestoreMovesDeletedAccountSubscriptionDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		products := []string{"supporter_monthly_26"}
		now := server.NowUtc().Truncate(time.Millisecond)

		// the account the subscription was bought for, credited, then deleted
		// inside its paid period
		deletedNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, deletedNetworkId, "applemovedeleted", server.NewId())
		originalTransactionId := "apple-move-orig-" + server.NewId().String()
		firstTransactionId := "apple-move-1-" + server.NewId().String()
		firstClaims := appleMoveDbClaims(deletedNetworkId, originalTransactionId, firstTransactionId, now.Add(-time.Hour))
		processed, err := ProcessAppleNotification(ctx, AppleNotificationDecodedPayload{
			NotificationType: "SUBSCRIBED",
			Subtype:          "INITIAL_BUY",
			NotificationUUID: server.NewId().String(),
			SignedDate:       now.UnixMilli(),
			TransactionInfo:  firstClaims,
		}, products)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, true)

		outcome, _ := model.RemoveNetworkWithStoreSnapshot(ctx, deletedNetworkId, nil, &model.RemoveNetworkStoreSnapshot{
			AppleTransactionIds: []string{firstTransactionId},
		})
		connect.AssertEqual(t, outcome, model.RemoveNetworkRemoved)
		connect.AssertEqual(t, model.IsNetworkDeleted(ctx, deletedNetworkId), true)

		// the owner restores it from their new account: the rest of the paid
		// window moves there, and the deleted account's window ends
		clientSession, networkId := verifyTestSession(ctx, t, "applemovenew")
		result, err := VerifyAppleTransactionClaims(firstClaims, products, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusCredited)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketApple), 1)
		connect.AssertEqual(t, activeRenewalCount(t, ctx, deletedNetworkId, model.SubscriptionMarketApple), 0)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketApple, model.PaymentReconcileActionSubscriptionMoved, originalTransactionId),
			1,
		)

		// the same proof again: nothing moves or opens twice
		result, err = VerifyAppleTransactionClaims(firstClaims, products, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusAlreadyCredited)
		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketApple), 1)

		// another live account stays refused
		otherSession, otherNetworkId := verifyTestSession(ctx, t, "applemoveother")
		result, err = VerifyAppleTransactionClaims(firstClaims, products, otherSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusWrongNetwork)
		connect.AssertEqual(t, model.IsProNetwork(ctx, otherNetworkId), false)

		// the next renewal names the deleted account and credits the new one
		secondTransactionId := "apple-move-2-" + server.NewId().String()
		processed, err = ProcessAppleNotification(ctx, AppleNotificationDecodedPayload{
			NotificationType: "DID_RENEW",
			NotificationUUID: server.NewId().String(),
			SignedDate:       now.UnixMilli(),
			TransactionInfo:  appleMoveDbClaims(deletedNetworkId, originalTransactionId, secondTransactionId, now),
		}, products)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, true)
		creditedNetworkId, credited := model.GetAppleTransactionNetworkId(ctx, secondTransactionId)
		connect.AssertEqual(t, credited, true)
		connect.AssertEqual(t, creditedNetworkId, networkId)
	})
}

func TestAppleRestoreKeepsLiveAndUnrecordedAccountsPurchasesDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		products := []string{"supporter_monthly_26"}
		now := server.NowUtc().Truncate(time.Millisecond)
		clientSession, networkId := verifyTestSession(ctx, t, "applemovekeep")

		// a live account's purchase
		liveNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, liveNetworkId, "applemovelive", server.NewId())
		claims := appleMoveDbClaims(liveNetworkId, "apple-move-live-"+server.NewId().String(), "apple-move-live-tx-"+server.NewId().String(), now.Add(-time.Hour))
		result, err := VerifyAppleTransactionClaims(claims, products, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusWrongNetwork)

		// a network that is not found but has no deletion record
		claims = appleMoveDbClaims(server.NewId(), "apple-move-unknown-"+server.NewId().String(), "apple-move-unknown-tx-"+server.NewId().String(), now.Add(-time.Hour))
		result, err = VerifyAppleTransactionClaims(claims, products, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusWrongNetwork)

		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
	})
}
