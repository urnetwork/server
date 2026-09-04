package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// playWebhookTestEnv stands up a fake Android Publisher API and swaps the
// vault/config-backed seams for test values, restoring everything on cleanup.
// This test env has no google vault/config, so the seams are the only way to
// drive PlayWebhook end to end -- which is exactly the coverage the Play path
// never had (the old TestGooglePlayPubSub harness below is long dead).
type playWebhookTestEnv struct {
	packageName string
	// purchase token -> the subscription the fake API serves
	subscriptions map[string]*PlaySubscription
	// how many times each token was acknowledged
	acknowledged map[string]*atomic.Int64
	// when true, the acknowledge endpoint answers 500
	failAcknowledge bool

	testServer *httptest.Server
}

func newPlayWebhookTestEnv(t testing.TB, skus map[string]*Sku) *playWebhookTestEnv {
	env := &playWebhookTestEnv{
		packageName:   "network.ur.test",
		subscriptions: map[string]*PlaySubscription{},
		acknowledged:  map[string]*atomic.Int64{},
	}

	mux := http.NewServeMux()
	// GET /androidpublisher/v3/applications/{pkg}/purchases/subscriptionsv2/tokens/{token}
	mux.HandleFunc(
		fmt.Sprintf("GET /androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/{token}", env.packageName),
		func(w http.ResponseWriter, r *http.Request) {
			sub, ok := env.subscriptions[r.PathValue("token")]
			if !ok {
				http.Error(w, "not found", http.StatusGone)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(sub)
		},
	)
	// POST /androidpublisher/v3/applications/{pkg}/purchases/subscriptions/{subId}/tokens/{tokenAndAction}
	mux.HandleFunc(
		fmt.Sprintf("POST /androidpublisher/v3/applications/%s/purchases/subscriptions/{subId}/tokens/{tokenAndAction}", env.packageName),
		func(w http.ResponseWriter, r *http.Request) {
			if env.failAcknowledge {
				http.Error(w, "backend error", http.StatusInternalServerError)
				return
			}
			counter, ok := env.acknowledged[r.PathValue("tokenAndAction")]
			if !ok {
				counter = &atomic.Int64{}
				env.acknowledged[r.PathValue("tokenAndAction")] = counter
			}
			counter.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))
		},
	)
	env.testServer = httptest.NewServer(mux)

	prevBaseUrl := playPublisherApiBaseUrl
	prevPackageName := playPackageNameFunc
	prevSkus := playSkusFunc
	prevAuthHeader := playAuthHeaderFunc

	playPublisherApiBaseUrl = env.testServer.URL
	playPackageNameFunc = func() string { return env.packageName }
	playSkusFunc = func() map[string]*Sku { return skus }
	playAuthHeaderFunc = func(ctx context.Context, header http.Header) {
		header.Add("Authorization", "Bearer test-token")
	}

	t.Cleanup(func() {
		playPublisherApiBaseUrl = prevBaseUrl
		playPackageNameFunc = prevPackageName
		playSkusFunc = prevSkus
		playAuthHeaderFunc = prevAuthHeader
		env.testServer.Close()
	})

	return env
}

func (self *playWebhookTestEnv) acknowledgeCount(purchaseToken string) int64 {
	counter, ok := self.acknowledged[purchaseToken+":acknowledge"]
	if !ok {
		return 0
	}
	return counter.Load()
}

// webhookArgs builds the base64 RTDN envelope Pub/Sub delivers.
func (self *playWebhookTestEnv) webhookArgs(t testing.TB, subscriptionId string, purchaseToken string) *PlayWebhookArgs {
	rtdnMessage := &PlayRtdnMessage{
		Version:     "1.0",
		PackageName: self.packageName,
		SubscriptionNotification: &PlaySubscriptionNotification{
			Version: "1.0",
			// SUBSCRIPTION_PURCHASED
			NotificationType: 4,
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

func playTestSubscription(networkId server.Id, productId string, startTime time.Time, expiryTime time.Time) *PlaySubscription {
	return &PlaySubscription{
		LineItems: []*PlaySubscriptionPurchaseLineItem{
			{
				ProductId:  productId,
				ExpiryTime: expiryTime.UTC().Format(time.RFC3339),
			},
		},
		StartTime:            startTime.UTC().Format(time.RFC3339),
		SubscriptionState:    "SUBSCRIPTION_STATE_ACTIVE",
		AcknowledgementState: "ACKNOWLEDGEMENT_STATE_PENDING",
		ExternalAccountIdentifiers: &PlayExternalAccountIdentifiers{
			ObfuscatedExternalAccountId: networkId.String(),
		},
	}
}

// TestPlayWebhookCreditsAndAcknowledges is the first live test of the Play webhook:
// a PURCHASED notification for a supporter sku acknowledges the purchase with
// Google (an unacknowledged purchase is auto-refunded after 3 days) and credits the
// pro balance INLINE -- and a redelivery of the same notification credits nothing
// twice, because the purchase token's balance already overlaps the expiry.
func TestPlayWebhookCreditsAndAcknowledges(t *testing.T) {
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
		model.Testing_CreateNetwork(ctx, networkId, "playcredit", userId)

		clientId := server.NewId()
		webhookSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		purchaseToken := "play-token-credit-1"
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-1*time.Minute),
			server.NowUtc().Add(30*24*time.Hour),
		)

		result, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)

		// the purchase was acknowledged with Google
		connect.AssertEqual(t, env.acknowledgeCount(purchaseToken), int64(1))

		// the credit landed inline: a pro balance against the purchase token, and
		// the entitlement is visible immediately
		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, balances[0].BalanceByteCount, RefreshSupporterTransferBalance)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// the renewal row names the google market
		active, market := model.HasSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeSupporter)
		connect.AssertEqual(t, active, true)
		connect.AssertEqual(t, *market, model.SubscriptionMarketGoogle)

		// Pub/Sub redelivers the same notification: acknowledged again (Google's
		// side is idempotent), but the overlapping balance gate credits nothing new
		result, err = PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestPlayWebhookCreditErrorIsNon2xx pins the S5 fix: a credit failure (here, a sku
// play.yml does not know) must surface as an ERROR from the handler -- a non-2xx to
// Pub/Sub, which then RETRIES the delivery. The old handler discarded the credit
// error and acked 200, so the entitlement arrived only via the task scheduled at
// the END of the paid period, or never.
func TestPlayWebhookCreditErrorIsNon2xx(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// play.yml knows no skus at all
		env := newPlayWebhookTestEnv(t, map[string]*Sku{})

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playskuerr", userId)

		clientId := server.NewId()
		webhookSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		purchaseToken := "play-token-badsku-1"
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"unknown_sku",
			server.NowUtc().Add(-1*time.Minute),
			server.NowUtc().Add(30*24*time.Hour),
		)

		_, err := PlayWebhook(env.webhookArgs(t, "unknown_sku", purchaseToken), webhookSession)
		// the error propagates -> the route answers non-2xx -> Pub/Sub retries
		connect.AssertNotEqual(t, err, nil)

		// and no partial credit happened
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)
	})
}

// TestPlayWebhookAcknowledgeFailureIsNon2xx pins the other half of S5: the
// `:acknowledge` result is CHECKED. An acknowledge Google never receives means the
// purchase is auto-refunded after 3 days while any granted balance would stand --
// so a failed acknowledge is a non-2xx, Pub/Sub redelivers, and the acknowledge is
// attempted again before anything is credited.
func TestPlayWebhookAcknowledgeFailureIsNon2xx(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		env := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter_monthly": {
				FeeFraction:    0.3,
				PriceAmountUsd: 5.0,
				Supporter:      true,
			},
		})
		env.failAcknowledge = true

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playackerr", userId)

		clientId := server.NewId()
		webhookSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		purchaseToken := "play-token-ackfail-1"
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-1*time.Minute),
			server.NowUtc().Add(30*24*time.Hour),
		)

		_, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertNotEqual(t, err, nil)

		// the failed delivery credits nothing -- the retry (after Pub/Sub
		// redelivers) will acknowledge and credit together
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		// once the backend recovers, the redelivery acknowledges and credits
		env.failAcknowledge = false
		result, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result, nil)
		connect.AssertEqual(t, env.acknowledgeCount(purchaseToken), int64(1))
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	})
}
