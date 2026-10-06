package controller

// Tests for the Play SUBSCRIPTION_REVOKED clawback (UPGRADE.md §2 S7),
// reusing the S5 fake Android Publisher API env from play_webhook_test.go.
// The rule under test: REVOKED (RTDN type 12, the store refunded the user)
// ends the token's entitlement NOW; EXPIRED (type 13, a normal lapse) never
// claws anything back.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// playWebhookArgsWithType builds the base64 RTDN envelope for an arbitrary
// notification type (webhookArgs in play_webhook_test.go pins PURCHASED).
func playWebhookArgsWithType(
	t testing.TB,
	env *playWebhookTestEnv,
	subscriptionId string,
	purchaseToken string,
	notificationType int,
) *PlayWebhookArgs {
	rtdnMessage := &PlayRtdnMessage{
		Version:     "1.0",
		PackageName: env.packageName,
		SubscriptionNotification: &PlaySubscriptionNotification{
			Version:          "1.0",
			NotificationType: notificationType,
			PurchaseToken:    purchaseToken,
			SubscriptionId:   subscriptionId,
		},
	}
	data, err := json.Marshal(rtdnMessage)
	connect.AssertEqual(t, err, nil)
	return &PlayWebhookArgs{
		Message: &PlayWebhookMessage{
			Data: base64.StdEncoding.EncodeToString(data),
		},
	}
}

func playRevokeTestSupporterSkus() map[string]*Sku {
	return map[string]*Sku{
		"supporter_monthly": {
			FeeFraction:    0.3,
			PriceAmountUsd: 5.0,
			Supporter:      true,
		},
	}
}

func playRevokeTestSession(ctx context.Context, networkId server.Id, userId server.Id) *session.ClientSession {
	clientId := server.NewId()
	return session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: networkId,
		ClientId:  &clientId,
		UserId:    userId,
	})
}

// TestPlayWebhookRevokedEndsEntitlement pins SUBSCRIPTION_REVOKED: the
// token's renewal and the pro balance it granted end NOW, a revoked operator
// event is recorded, and a Pub/Sub redelivery finds nothing left to end (one
// event total). A revoked token that is already 410-Gone at the store is
// clawed back too, from the renewal rows alone.
func TestPlayWebhookRevokedEndsEntitlement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		env := newPlayWebhookTestEnv(t, playRevokeTestSupporterSkus())

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playrevoke", userId)
		webhookSession := playRevokeTestSession(ctx, networkId, userId)

		purchaseToken := "play-token-revoke-1"
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-1*time.Minute),
			server.NowUtc().Add(30*24*time.Hour),
		)

		// credit through the normal PURCHASED flow
		_, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// Google revokes: the store now reports the subscription EXPIRED and
		// delivers RTDN type 12
		env.subscriptions[purchaseToken].SubscriptionState = "SUBSCRIPTION_STATE_EXPIRED"
		result, err := PlayWebhook(
			playWebhookArgsWithType(t, env, "supporter_monthly", purchaseToken, playRtdnNotificationTypeRevoked),
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		connect.AssertEqual(t, activeRenewalCount(t, ctx, networkId, model.SubscriptionMarketGoogle), 0)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketGoogle, model.PaymentReconcileActionRevoked, purchaseToken),
			1,
		)

		// Pub/Sub redelivers: nothing left to end, no second event
		_, err = PlayWebhook(
			playWebhookArgsWithType(t, env, "supporter_monthly", purchaseToken, playRtdnNotificationTypeRevoked),
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketGoogle, model.PaymentReconcileActionRevoked, purchaseToken),
			1,
		)

		// a second network whose revoked token is already GONE from the store:
		// the renewal rows still map the token to the network and the clawback
		// lands
		networkId2 := server.NewId()
		userId2 := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId2, "playrevokegone", userId2)
		webhookSession2 := playRevokeTestSession(ctx, networkId2, userId2)

		purchaseToken2 := "play-token-revoke-gone-1"
		env.subscriptions[purchaseToken2] = playTestSubscription(
			networkId2,
			"supporter_monthly",
			server.NowUtc().Add(-1*time.Minute),
			server.NowUtc().Add(30*24*time.Hour),
		)
		_, err = PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken2), webhookSession2)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId2), true)

		delete(env.subscriptions, purchaseToken2) // the fake answers 410 Gone
		_, err = PlayWebhook(
			playWebhookArgsWithType(t, env, "supporter_monthly", purchaseToken2, playRtdnNotificationTypeRevoked),
			webhookSession2,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId2), false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId2)), 0)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketGoogle, model.PaymentReconcileActionRevoked, purchaseToken2),
			1,
		)
	})
}

// TestPlayWebhookExpiredLapseDoesNotClawBack pins the cancelled ≠ expired
// rule at the webhook: SUBSCRIPTION_EXPIRED (type 13) is the normal end of a
// paid period -- the already-granted window (expiry + grace) stays, nothing
// is clawed back, no revoked event is recorded.
func TestPlayWebhookExpiredLapseDoesNotClawBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		env := newPlayWebhookTestEnv(t, playRevokeTestSupporterSkus())

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playexpire", userId)
		webhookSession := playRevokeTestSession(ctx, networkId, userId)

		purchaseToken := "play-token-expire-1"
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-30*24*time.Hour),
			server.NowUtc().Add(time.Minute),
		)

		_, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)

		// the period lapses normally: SUBSCRIPTION_EXPIRED, RTDN type 13
		env.subscriptions[purchaseToken].SubscriptionState = "SUBSCRIPTION_STATE_EXPIRED"
		result, err := PlayWebhook(
			playWebhookArgsWithType(t, env, "supporter_monthly", purchaseToken, 13),
			webhookSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		// nothing clawed back: the paid-through window (expiry + grace) stands
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketGoogle, model.PaymentReconcileActionRevoked, purchaseToken),
			0,
		)
	})
}
