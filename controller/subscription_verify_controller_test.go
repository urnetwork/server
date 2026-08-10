package controller

// Tests for the client purchase-reporting endpoints (UPGRADE.md §4 item 2).
// The Play side drives the REAL VerifyPlayPurchase flow against the hermetic
// fake Android Publisher API (the S5 seams); the Apple side drives
// VerifyAppleTransactionClaims with decoded claims, exactly the input the
// api/handlers pinned-root verifier hands it (the JWS verification itself is
// covered in api/handlers).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func verifyTestSession(ctx context.Context, t testing.TB, name string) (*session.ClientSession, server.Id) {
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, name, userId)
	clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})
	return clientSession, networkId
}

var verifyTestSupporterSkus = map[string]*Sku{
	"supporter_monthly": {
		FeeFraction:    0.3,
		PriceAmountUsd: 5.0,
		Supporter:      true,
	},
}

// TestVerifyPlayPurchaseCreditsAndIsIdempotent: the happy path credits through
// PlaySubscriptionRenewal (advisory-lock gate), a second report answers
// already_credited without a second credit, and an RTDN delivery for the same
// token afterwards also credits nothing -- the client report and the webhook
// share one gate.
func TestVerifyPlayPurchaseCreditsAndIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		env := newPlayWebhookTestEnv(t, verifyTestSupporterSkus)
		clientSession, networkId := verifyTestSession(ctx, t, "verifyplaycredit")

		purchaseToken := "verify-play-token-1"
		expiryTime := server.NowUtc().Add(30 * 24 * time.Hour)
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-1*time.Minute),
			expiryTime,
		)

		result, err := VerifyPlayPurchase(
			&VerifyPlayPurchaseArgs{
				ProductId:     "supporter_monthly",
				PurchaseToken: purchaseToken,
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusCredited)
		connect.AssertEqual(t, result.ExpiryTime != nil, true)

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// the client retries (e.g. the response was lost): terminal
		// already_credited, no second credit
		result, err = VerifyPlayPurchase(
			&VerifyPlayPurchaseArgs{
				ProductId:     "supporter_monthly",
				PurchaseToken: purchaseToken,
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusAlreadyCredited)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)

		// the RTDN webhook for the same purchase finally arrives: the shared
		// overlap gate credits nothing new
		webhookResult, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, webhookResult, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestVerifyPlayPurchaseRacesWebhookOnce: a client report and an RTDN delivery
// for the same token racing concurrently produce exactly ONE credit -- the
// purchase-token advisory xact lock with the in-tx overlap re-check is the
// same gate for both.
func TestVerifyPlayPurchaseRacesWebhookOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		env := newPlayWebhookTestEnv(t, verifyTestSupporterSkus)
		clientSession, networkId := verifyTestSession(ctx, t, "verifyplayrace")

		purchaseToken := "verify-play-token-race-1"
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-1*time.Minute),
			server.NowUtc().Add(30*24*time.Hour),
		)

		var wg sync.WaitGroup
		wg.Add(2)
		var verifyErr, webhookErr error
		go func() {
			defer wg.Done()
			_, verifyErr = VerifyPlayPurchase(
				&VerifyPlayPurchaseArgs{
					ProductId:     "supporter_monthly",
					PurchaseToken: purchaseToken,
				},
				clientSession,
			)
		}()
		go func() {
			defer wg.Done()
			_, webhookErr = PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), clientSession)
		}()
		wg.Wait()

		connect.AssertEqual(t, verifyErr, nil)
		connect.AssertEqual(t, webhookErr, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestVerifyPlayPurchaseWrongNetworkDoesNotCredit: a token linked (via the
// obfuscated external account id set at purchase-flow launch) to a DIFFERENT
// network answers wrong_network -- terminal, clearly distinguishable -- and
// credits NOTHING, neither to the session network nor the linked one.
func TestVerifyPlayPurchaseWrongNetworkDoesNotCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		env := newPlayWebhookTestEnv(t, verifyTestSupporterSkus)
		clientSession, sessionNetworkId := verifyTestSession(ctx, t, "verifyplaywrongb")

		linkedNetworkId := server.NewId()
		linkedUserId := server.NewId()
		model.Testing_CreateNetwork(ctx, linkedNetworkId, "verifyplaywronga", linkedUserId)

		purchaseToken := "verify-play-token-wrong-1"
		env.subscriptions[purchaseToken] = playTestSubscription(
			linkedNetworkId,
			"supporter_monthly",
			server.NowUtc().Add(-1*time.Minute),
			server.NowUtc().Add(30*24*time.Hour),
		)

		result, err := VerifyPlayPurchase(
			&VerifyPlayPurchaseArgs{
				ProductId:     "supporter_monthly",
				PurchaseToken: purchaseToken,
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusWrongNetwork)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, sessionNetworkId)), 0)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, linkedNetworkId)), 0)
	})
}

// TestVerifyPlayPurchaseInvalidToken: a token the store does not know (410
// from the publisher API) is terminal invalid, with no credit.
func TestVerifyPlayPurchaseInvalidToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		newPlayWebhookTestEnv(t, verifyTestSupporterSkus)
		clientSession, networkId := verifyTestSession(ctx, t, "verifyplayinvalid")

		result, err := VerifyPlayPurchase(
			&VerifyPlayPurchaseArgs{
				ProductId:     "supporter_monthly",
				PurchaseToken: "verify-play-token-unknown",
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusInvalid)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		// an empty token and a foreign package are rejected before any store
		// call or budget spend
		result, err = VerifyPlayPurchase(&VerifyPlayPurchaseArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusInvalid)

		result, err = VerifyPlayPurchase(
			&VerifyPlayPurchaseArgs{
				PackageName:   "some.other.app",
				PurchaseToken: "verify-play-token-unknown",
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusInvalid)
	})
}

// TestVerifyPlayPurchasePendingDoesNotCredit: a PENDING purchase (e.g. waiting
// on parental approval or a slow payment method) answers pending -- retryable,
// NOT terminal, so the client keeps the proof and does not acknowledge -- and
// credits nothing.
func TestVerifyPlayPurchasePendingDoesNotCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		env := newPlayWebhookTestEnv(t, verifyTestSupporterSkus)
		clientSession, networkId := verifyTestSession(ctx, t, "verifyplaypending")

		purchaseToken := "verify-play-token-pending-1"
		env.subscriptions[purchaseToken] = &PlaySubscription{
			// a pending purchase has no expiry yet -- the endpoint must not
			// touch line-item expiry for this state
			LineItems: []*PlaySubscriptionPurchaseLineItem{
				{ProductId: "supporter_monthly"},
			},
			StartTime:            server.NowUtc().UTC().Format(time.RFC3339),
			SubscriptionState:    "SUBSCRIPTION_STATE_PENDING",
			AcknowledgementState: "ACKNOWLEDGEMENT_STATE_PENDING",
			ExternalAccountIdentifiers: &PlayExternalAccountIdentifiers{
				ObfuscatedExternalAccountId: networkId.String(),
			},
		}

		result, err := VerifyPlayPurchase(
			&VerifyPlayPurchaseArgs{
				ProductId:     "supporter_monthly",
				PurchaseToken: purchaseToken,
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusPending)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		// the purchase completes: the SAME report now credits
		env.subscriptions[purchaseToken] = playTestSubscription(
			networkId,
			"supporter_monthly",
			server.NowUtc().Add(-1*time.Minute),
			server.NowUtc().Add(30*24*time.Hour),
		)
		result, err = VerifyPlayPurchase(
			&VerifyPlayPurchaseArgs{
				ProductId:     "supporter_monthly",
				PurchaseToken: purchaseToken,
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusCredited)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestVerifyPlayPurchaseRateLimited: the per-account budget cuts off a spinner
// with a 429-tagged error (retryable later; the proof is not invalidated).
func TestVerifyPlayPurchaseRateLimited(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		newPlayWebhookTestEnv(t, verifyTestSupporterSkus)
		clientSession, _ := verifyTestSession(ctx, t, "verifyplayratelimit")

		args := &VerifyPlayPurchaseArgs{
			ProductId:     "supporter_monthly",
			PurchaseToken: "verify-play-token-spin",
		}
		for i := 0; i < model.AccountActionVerifyStorePurchaseWindowLimit; i += 1 {
			result, err := VerifyPlayPurchase(args, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Status, VerifyPurchaseStatusInvalid)
		}
		_, err := VerifyPlayPurchase(args, clientSession)
		connect.AssertNotEqual(t, err, nil)
	})
}

// ----- apple -----

func appleVerifyTestClaims(networkId server.Id, transactionId string, now time.Time) map[string]any {
	return map[string]any{
		"appAccountToken": networkId.String(),
		"transactionId":   transactionId,
		"productId":       "supporter_monthly_26",
		"purchaseDate":    float64(now.Add(-time.Hour).UnixMilli()),
		"expiresDate":     float64(now.Add(30 * 24 * time.Hour).UnixMilli()),
		"price":           float64(4990),
	}
}

// TestVerifyAppleTransactionCreditsAndIsIdempotent: the happy path credits
// through the apple_subscription_transaction ledger, a client retry answers
// already_credited, and the notification webhook for the same transaction
// arriving later processes nothing -- one gate for both directions.
func TestVerifyAppleTransactionCreditsAndIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, networkId := verifyTestSession(ctx, t, "verifyapplecredit")

		now := server.NowUtc().Truncate(time.Millisecond)
		transactionId := "verify-apple-" + server.NewId().String()
		claims := appleVerifyTestClaims(networkId, transactionId, now)

		result, err := VerifyAppleTransactionClaims(claims, []string{"supporter_monthly_26"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusCredited)
		connect.AssertEqual(t, result.ExpiryTime != nil, true)

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		// the client retries the same JWS: terminal already_credited, one credit
		result, err = VerifyAppleTransactionClaims(claims, []string{"supporter_monthly_26"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusAlreadyCredited)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)

		// the App Store notification for the same transaction finally arrives:
		// the shared transaction ledger credits nothing new
		notification := AppleNotificationDecodedPayload{
			NotificationType: "SUBSCRIBED",
			Subtype:          "INITIAL_BUY",
			NotificationUUID: server.NewId().String(),
			SignedDate:       now.UnixMilli(),
			TransactionInfo:  claims,
		}
		processed, err := ProcessAppleNotification(ctx, notification, []string{"supporter_monthly_26"})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, processed, false)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)
	})
}

// TestVerifyAppleTransactionWrongNetworkDoesNotCredit: a verified transaction
// whose appAccountToken names a DIFFERENT network answers wrong_network and
// credits nothing anywhere.
func TestVerifyAppleTransactionWrongNetworkDoesNotCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, sessionNetworkId := verifyTestSession(ctx, t, "verifyapplewrongb")

		tokenNetworkId := server.NewId()
		tokenUserId := server.NewId()
		model.Testing_CreateNetwork(ctx, tokenNetworkId, "verifyapplewronga", tokenUserId)

		now := server.NowUtc().Truncate(time.Millisecond)
		claims := appleVerifyTestClaims(tokenNetworkId, "verify-apple-wrong-1", now)

		result, err := VerifyAppleTransactionClaims(claims, []string{"supporter_monthly_26"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Status, VerifyPurchaseStatusWrongNetwork)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, sessionNetworkId)), 0)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, tokenNetworkId)), 0)
	})
}

// TestVerifyAppleTransactionInvalidClaims: claims that fail the shared
// validator -- unapproved product, missing entitlement fields, garbage account
// token -- are terminal invalid. No store or DB write happens; the test runs
// without a DB env for the pure-validation cases.
func TestVerifyAppleTransactionInvalidClaims(t *testing.T) {
	ctx := context.Background()
	networkId := server.NewId()
	clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: networkId,
		UserId:    server.NewId(),
	})
	now := server.NowUtc().Truncate(time.Millisecond)

	// unapproved product
	claims := appleVerifyTestClaims(networkId, "verify-apple-invalid-1", now)
	claims["productId"] = "attacker-controlled-product"
	result, err := VerifyAppleTransactionClaims(claims, []string{"supporter_monthly_26"}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Status, VerifyPurchaseStatusInvalid)

	// missing entitlement fields (no expiresDate)
	claims = appleVerifyTestClaims(networkId, "verify-apple-invalid-2", now)
	delete(claims, "expiresDate")
	result, err = VerifyAppleTransactionClaims(claims, []string{"supporter_monthly_26"}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Status, VerifyPurchaseStatusInvalid)

	// unparseable account token
	claims = appleVerifyTestClaims(networkId, "verify-apple-invalid-3", now)
	claims["appAccountToken"] = "not-a-network-id"
	result, err = VerifyAppleTransactionClaims(claims, []string{"supporter_monthly_26"}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Status, VerifyPurchaseStatusInvalid)
}
