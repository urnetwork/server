package handlers

import (
	"net/http"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

func SubscriptionBalance(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.SubscriptionBalance, w, r)
}

// SubscriptionDetails lists every store billing the caller's network with the
// paid-through date, the store's auto-renew state and the control that stops
// it (the "Manage subscription" screen).
func SubscriptionDetails(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.SubscriptionDetails, w, r)
}

// SubscriptionCancel lets the Stripe subscription run out at the end of the
// paid period; other stores answer with where to cancel instead.
func SubscriptionCancel(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SubscriptionCancel, w, r)
}

// SubscriptionResume undoes SubscriptionCancel while the paid period is still
// running.
func SubscriptionResume(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SubscriptionResume, w, r)
}

func StripeWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyStripeBody,
		controller.StripeWebhook,
		w,
		r,
	)
}

func CoinbaseWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyCoinbaseBody,
		controller.CoinbaseWebhook,
		w,
		r,
	)
}

func PlayWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyPlayBody,
		controller.PlayWebhook,
		w,
		r,
	)
}

func HeliusWebhook(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputBodyFormatterNoAuth(
		controller.VerifyHeliusBody,
		controller.HeliusWebhook,
		w,
		r,
	)
}

func SubscriptionCheckBalanceCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.CheckBalanceCode, w, r)
}

func SubscriptionRedeemBalanceCode(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.RedeemBalanceCode, w, r)
}

// SubscriptionVerifyPlayPurchase is the client purchase-reporting endpoint for
// Play (UPGRADE.md §4 item 2): the android app reports the purchase token
// BEFORE acknowledging, the server verifies it with the Android Publisher API
// and credits through the same advisory-lock gate as the RTDN webhook and the
// payment reconciler.
func SubscriptionVerifyPlayPurchase(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.VerifyPlayPurchase, w, r)
}

// subscriptionAppleVerifierFunc is the verifier seam, replaceable only by
// hermetic tests (this test env has no apple.yml to build the default,
// panicking verifier from). Production never mutates it.
var subscriptionAppleVerifierFunc = func() *appleNotificationVerifier {
	return defaultAppleNotificationVerifier()
}

const maxAppleSignedTransactionBytes = 64 * 1024

// SubscriptionVerifyAppleTransaction is the client purchase-reporting endpoint
// for the App Store (UPGRADE.md §4 item 2): the apple app reports the StoreKit
// transaction JWS BEFORE calling finish(). The JWS is a client push of
// unauthenticated content, so it goes through the FULL pinned-root webhook
// verifier (unlike the reconciler's authenticated pulls from Apple); the
// verified claims then credit through the apple_subscription_transaction
// ledger gate shared with the notification webhook and the reconciler.
func SubscriptionVerifyAppleTransaction(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(verifyAppleTransaction, w, r)
}

func verifyAppleTransaction(
	args *controller.VerifyAppleTransactionArgs,
	clientSession *session.ClientSession,
) (*controller.VerifyStorePurchaseResult, error) {
	// cheap validation first, then the rate limit, then the cryptographic
	// verification -- so malformed requests burn no budget and a budget-
	// exhausted caller burns no CPU
	if args.SignedTransaction == "" || maxAppleSignedTransactionBytes < len(args.SignedTransaction) {
		return controller.NewVerifyStorePurchaseInvalid(), nil
	}
	if err := controller.CheckVerifyPurchaseRateLimit(clientSession); err != nil {
		return nil, err
	}
	verifier := subscriptionAppleVerifierFunc()
	claims, err := verifier.verifyTransaction(args.SignedTransaction)
	if err != nil {
		glog.Infof("[apple] Rejected client-reported transaction: %v", err)
		// a JWS that fails webhook-grade verification will never verify --
		// terminal, so the client stops retrying
		return controller.NewVerifyStorePurchaseInvalid(), nil
	}
	return controller.VerifyAppleTransactionClaims(claims, verifier.config.ProductIds, clientSession)
}

func SubscriptionCreatePaymentId(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.SubscriptionCreatePaymentId, w, r)
}

func CreateSolanaPaymentIntent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.CreateSolanaPaymentIntent, w, r)
}

func CreateStripePaymentIntent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.StripeCreatePaymentIntent, w, r)
}

// StripeCreateCheckoutSession starts a Stripe Checkout Session for the caller's network.
//
// ui_mode "hosted" (the default) returns Stripe's hosted checkout_url -- the client just
// navigates there. ui_mode "embedded" instead returns a client_secret + publishable_key,
// which Stripe.js mounts as Embedded Checkout on our own /checkout page (that is what the
// desktop apps render in a webview). A session is one or the other, never both.
func StripeCreateCheckoutSession(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.StripeCreateCheckoutSession, w, r)
}

// PayDataCheckout starts a hosted Stripe or Coinbase checkout for a data pack
// without a signed-in session (the buy-data page).
func PayDataCheckout(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.PayDataCheckout, w, r)
}

// PayDataNetworkLookup answers whether a network with exactly this name exists,
// for the buy-data page.
func PayDataNetworkLookup(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.PayDataNetworkLookup, w, r)
}

// PayDataSolanaIntent quotes a data pack for a named network and records the
// Solana payment intent the Helius webhook credits when the USDC arrives.
func PayDataSolanaIntent(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.PayDataSolanaIntent, w, r)
}

// PayDataSolanaStatus is what the buy-data page polls while it waits for the
// USDC transfer.
func PayDataSolanaStatus(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.PayDataSolanaStatus, w, r)
}

func StripeCreateCustomerPortal(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.StripeCreateCustomerPortal, w, r)
}
