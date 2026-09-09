package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The onboarding program's endpoints (mmm/onboarding/PLAN.md). The plan response
// itself is GET /subscription/balance, which grew the price_tier,
// onboarding_offer and experiments fields (see SubscriptionBalance).

// OnboardingOfferIssue issues the caller's welcome offer once (idempotent).
func OnboardingOfferIssue(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.OnboardingOfferIssue, w, r)
}

// ClientEventsSend stores a batch of product events against the closed schema.
func ClientEventsSend(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.ClientEventsSend, w, r)
}

// OnboardingClick records a landing-page click for a signed campaign token and
// returns the in-app destination. No auth: the token is the credential.
func OnboardingClick(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.OnboardingClick, w, r)
}

// OnboardingFeedbackToken resolves a feedback link token to its pre-filled rating
// or reason. No auth: the token is the credential.
func OnboardingFeedbackToken(w http.ResponseWriter, r *http.Request) {
	pathValues := router.GetPathValues(r)
	token := ""
	if 0 < len(pathValues) {
		token = pathValues[0]
	}
	query := r.URL.Query()
	rating := query.Get("r")
	reason := query.Get("why")
	impl := func(clientSession *session.ClientSession) (*controller.OnboardingFeedbackTokenResult, error) {
		return controller.OnboardingFeedbackToken(token, rating, reason, clientSession)
	}
	router.WrapNoAuth(impl, w, r)
}

// StripePaymentSheet prepares an inline Stripe PaymentSheet purchase of Pro.
func StripePaymentSheet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.StripePaymentSheet, w, r)
}

// StripePrices returns the caller's tier's Stripe price ids
// (?storefront_country=XX when the app knows the storefront).
func StripePrices(w http.ResponseWriter, r *http.Request) {
	storefrontCountry := r.URL.Query().Get("storefront_country")
	impl := func(clientSession *session.ClientSession) (*controller.StripePricesResult, error) {
		return controller.StripePrices(storefrontCountry, clientSession)
	}
	router.WrapRequireAuth(impl, w, r)
}
