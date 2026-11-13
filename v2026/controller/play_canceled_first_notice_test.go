package controller

// A Play purchase whose first notification is processed after the customer
// turned auto-renew off: Play reports it cancelled with the paid period still
// running (cancelled is not expired) and still unacknowledged. The webhook used
// to skip that state outright, so the purchase was never acknowledged (Google
// refunded it after three days) or credited, and the verify endpoint answered
// invalid for it (support inbox 8010, 8046, 8086 and 8098).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The fake Play subscription of a purchase whose auto-renew was turned off,
// paid through expiryTime.
func playCanceledTestSubscription(networkId server.Id, productId string, startTime time.Time, expiryTime time.Time) *PlaySubscription {
	sub := playTestSubscription(networkId, productId, startTime, expiryTime)
	sub.SubscriptionState = "SUBSCRIPTION_STATE_CANCELED"
	return sub
}

// The first notification of a paid month arrives after the customer cancelled
// auto-renew: the purchase is acknowledged (or Google refunds it) and its paid
// month is credited, once.
func TestPlayWebhookCreditsPurchaseCancelledBeforeItsNotification(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)

		env := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter_monthly": {
				FeeFraction:    0.3,
				PriceAmountUsd: 5.0,
				Supporter:      true,
			},
		})

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playcancelledfirst", userId)
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		purchaseToken := "play-token-cancelled-first-1"
		env.subscriptions[purchaseToken] = playCanceledTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-2*24*time.Hour),
			server.NowUtc().Add(28*24*time.Hour),
		)

		result, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		// acknowledged, so Google keeps the payment it took
		connect.AssertEqual(t, env.acknowledgeCount(purchaseToken), int64(1))

		// the paid month is credited to the network the purchase names
		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		active, market := model.HasSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeSupporter)
		connect.AssertEqual(t, active, true)
		connect.AssertEqual(t, *market, model.SubscriptionMarketGoogle)

		// a redelivery credits nothing twice
		result, err = PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// A cancelled purchase whose paid time is over is not credited or
// acknowledged: cancelled with no time left is the same as expired.
func TestPlayWebhookCancelledPastItsExpiryCreditsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		env := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter_monthly": {
				FeeFraction:    0.3,
				PriceAmountUsd: 5.0,
				Supporter:      true,
			},
		})

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playcancelledover", userId)
		webhookSession := session.Testing_CreateClientSession(ctx, nil)

		purchaseToken := "play-token-cancelled-over-1"
		env.subscriptions[purchaseToken] = playCanceledTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-31*24*time.Hour),
			server.NowUtc().Add(-time.Minute),
		)

		result, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)
		connect.AssertEqual(t, env.acknowledgeCount(purchaseToken), int64(0))
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
	})
}

// The app reports a purchase the customer already cancelled: the paid month is
// credited (not answered invalid), and a second report answers
// already_credited without a second credit.
func TestVerifyPlayPurchaseCreditsCancelledPaidThrough(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		env := newPlayWebhookTestEnv(t, verifyTestSupporterSkus)
		clientSession, networkId := verifyTestSession(ctx, t, "verifyplaycancelled")

		purchaseToken := "verify-play-token-cancelled-1"
		env.subscriptions[purchaseToken] = playCanceledTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-time.Hour),
			server.NowUtc().Add(30*24*time.Hour),
		)

		args := &VerifyPlayPurchaseArgs{
			ProductId:     "supporter_monthly",
			PurchaseToken: purchaseToken,
		}
		result, err := VerifyPlayPurchase(args, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusCredited)
		connect.AssertEqual(t, result.ExpiryTime != nil, true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		result, err = VerifyPlayPurchase(args, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusAlreadyCredited)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// A cancelled purchase with no paid time left stays invalid at the verify
// endpoint and credits nothing.
func TestVerifyPlayPurchaseCancelledPastItsExpiryIsInvalid(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		env := newPlayWebhookTestEnv(t, verifyTestSupporterSkus)
		clientSession, networkId := verifyTestSession(ctx, t, "verifyplaycancelledover")

		purchaseToken := "verify-play-token-cancelled-over-1"
		env.subscriptions[purchaseToken] = playCanceledTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-31*24*time.Hour),
			server.NowUtc().Add(-time.Minute),
		)

		result, err := VerifyPlayPurchase(
			&VerifyPlayPurchaseArgs{
				ProductId:     "supporter_monthly",
				PurchaseToken: purchaseToken,
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusInvalid)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
	})
}
