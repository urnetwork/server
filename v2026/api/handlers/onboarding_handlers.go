package handlers

import (
	"net/http"
	"strconv"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
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

// AdminOnboardingResults pages the nightly onboarding results aggregate. Auth
// is the vault admin bearer, checked by the controller (401 / 403).
func AdminOnboardingResults(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := 0
	if v := query.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	args := &controller.AdminOnboardingResultsArgs{
		Experiment: query.Get("experiment"),
		From:       query.Get("from"),
		To:         query.Get("to"),
		Surface:    query.Get("surface"),
		Platform:   query.Get("platform"),
		Tier:       query.Get("tier"),
		Path:       query.Get("path"),
		Cursor:     query.Get("cursor"),
		Limit:      limit,
	}
	router.WrapNoAuth(
		func(clientSession *session.ClientSession) (*controller.AdminOnboardingResultsResult, error) {
			return controller.AdminOnboardingResults(args, clientSession)
		},
		w,
		r,
	)
}

// AdminOnboardingEmailTracker pages the durable per-flow-step send and
// engagement aggregate. It uses the same Vault admin bearer as results.
func AdminOnboardingEmailTracker(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := 0
	if v := query.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	args := &controller.AdminOnboardingEmailTrackerArgs{
		From: query.Get("from"), To: query.Get("to"), Step: query.Get("step"),
		Experiment: query.Get("experiment"), Platform: query.Get("platform"),
		Path: query.Get("path"), Cursor: query.Get("cursor"), Limit: limit,
	}
	router.WrapNoAuth(
		func(clientSession *session.ClientSession) (*controller.AdminOnboardingEmailTrackerResult, error) {
			return controller.AdminOnboardingEmailTracker(args, clientSession)
		},
		w,
		r,
	)
}

// AdminOnboardingExperiments returns the experiment registry as loaded, with
// the live variant states. Same auth as AdminOnboardingResults.
func AdminOnboardingExperiments(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.AdminOnboardingExperiments, w, r)
}
