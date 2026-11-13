package controller

// How often one network may start a card purchase (owner, 2026-11-13).
//
// Card testers run stolen cards through checkouts. In 2026-11 one account made
// two Checkout sessions 2.5 minutes after it was created and ran 20 cards
// through them in 92 seconds; another made three. StripeCreateCheckoutSession
// had no limit at all, so an account could make sessions as fast as it liked,
// and the payment sheet had only its address's budget. Both now spend:
//
//   - the address's budget the payment sheet has always spent, shared with
//     POST /onboarding/offer/issue (model.CheckOnboardingRateLimit with
//     model.OnboardingOfferRateLimit), and
//   - the network's own budget of purchase starts
//     (model.AccountActionStripePurchaseStart, spent through
//     model.CheckAndRecordAccountActionBudgets), which the Checkout session
//     and the payment sheet share.
//
// A purchase starts once, or twice when the embedded checkout falls back to
// the hosted one, so the network's budget leaves room for a customer to come
// back and try again; a refusal says how long until there is room. Stripe's
// own card testing defenses still run inside each session; the failures those
// sessions see are watched in subscription_stripe_card_failures.go.

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The purchase start budget's tunables.
type stripePurchaseStartSettings struct {
	// The most card purchases, Checkout sessions and payment sheets together,
	// one network may start within NetworkWindow. 10: a purchase starts one or
	// two, and the 2026-11 card testing started two or three per account, so
	// this does not stop that shape alone, but it stops one account from
	// making sessions without end. Non-positive disables the network's budget.
	NetworkLimit int
	// The trailing window of NetworkLimit. At most the 24 hours the attempts
	// are kept for (model.AccountActionAttemptRetention is twice that).
	NetworkWindow time.Duration
}

// A new instance on every call.
func defaultStripePurchaseStartSettings() *stripePurchaseStartSettings {
	return &stripePurchaseStartSettings{
		NetworkLimit:  10,
		NetworkWindow: time.Hour,
	}
}

// The settings purchase starts are budgeted with. Replaceable only by tests;
// production never mutates it.
var stripePurchaseStartSettingsFunc = defaultStripePurchaseStartSettings

// The answer when another purchase start of the network holds its budget.
const stripePurchaseStartBusyMessage = "Another purchase is starting for your account. Please try again in a moment."

// Spends one purchase start from the network's budget, and answers why the
// start is refused, "" when it may go ahead. A refusal spends nothing. A
// failure to read the budget raises, as every budget read does.
func stripeNetworkPurchaseStartRefusal(ctx context.Context, networkId server.Id, settings *stripePurchaseStartSettings) string {
	if settings.NetworkLimit <= 0 || settings.NetworkWindow <= 0 {
		return ""
	}
	err := model.CheckAndRecordAccountActionBudgets(ctx, model.AccountActionBudget{
		Id:     networkId,
		Action: model.AccountActionStripePurchaseStart,
		Limit:  settings.NetworkLimit,
		Window: settings.NetworkWindow,
	})
	if err == nil {
		return ""
	}
	if errors.Is(err, model.ErrAccountActionBudgetsBusy) {
		return stripePurchaseStartBusyMessage
	}
	// the budget's refusal, which names the wait
	return err.Error()
}
