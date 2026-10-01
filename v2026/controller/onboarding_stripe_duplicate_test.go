package controller

import (
	"testing"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/server/v2026/model"
)

// a network that already pays for Pro must not be able to start another
// subscription (support inbox 1136: three Pro purchases from Windows)
func TestSubscriptionMarketsBlockNewPurchase(t *testing.T) {
	tests := []struct {
		name    string
		markets []model.SubscriptionMarket
		want    bool
	}{
		{"none", nil, false},
		{"stripe", []model.SubscriptionMarket{model.SubscriptionMarketStripe}, true},
		{"apple", []model.SubscriptionMarket{model.SubscriptionMarketApple}, true},
		{"google", []model.SubscriptionMarket{model.SubscriptionMarketGoogle}, true},
		{"unknown legacy market", []model.SubscriptionMarket{""}, true},
		{"manual grant only", []model.SubscriptionMarket{model.SubscriptionMarketManual}, false},
		{"x402 only", []model.SubscriptionMarket{model.SubscriptionMarketX402}, false},
		{"manual and stripe", []model.SubscriptionMarket{model.SubscriptionMarketManual, model.SubscriptionMarketStripe}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := subscriptionMarketsBlockNewPurchase(test.markets); got != test.want {
				t.Fatalf("block = %t, want %t", got, test.want)
			}
		})
	}
}

func TestStripeSubscriptionBlocksNewPurchase(t *testing.T) {
	card := &stripe.PaymentMethod{ID: "pm_1"}
	pending := map[string]string{stripeMetadataPaymentSheet: stripePaymentSheetPending}
	attached := map[string]string{stripeMetadataPaymentSheet: stripePaymentSheetAttached}

	tests := []struct {
		name string
		sub  *stripe.Subscription
		want bool
	}{
		{"nil", nil, false},
		{"active checkout subscription", &stripe.Subscription{Status: stripe.SubscriptionStatusActive}, true},
		{"trialing yearly", &stripe.Subscription{Status: stripe.SubscriptionStatusTrialing, Metadata: attached}, true},
		{"past due", &stripe.Subscription{Status: stripe.SubscriptionStatusPastDue}, true},
		{"paid sheet before its webhook", &stripe.Subscription{Status: stripe.SubscriptionStatusActive, Metadata: pending, DefaultPaymentMethod: card}, true},
		{"abandoned yearly sheet in trial", &stripe.Subscription{Status: stripe.SubscriptionStatusTrialing, Metadata: pending}, false},
		{"incomplete monthly sheet", &stripe.Subscription{Status: stripe.SubscriptionStatusIncomplete, Metadata: pending}, false},
		{"canceled", &stripe.Subscription{Status: stripe.SubscriptionStatusCanceled, DefaultPaymentMethod: card}, false},
		{"incomplete expired", &stripe.Subscription{Status: stripe.SubscriptionStatusIncompleteExpired}, false},
		{"unpaid", &stripe.Subscription{Status: stripe.SubscriptionStatusUnpaid}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stripeSubscriptionBlocksNewPurchase(test.sub); got != test.want {
				t.Fatalf("block = %t, want %t", got, test.want)
			}
		})
	}
}
