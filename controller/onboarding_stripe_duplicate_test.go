package controller

import (
	"testing"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/server/model"
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
		{"monero year", []model.SubscriptionMarket{model.SubscriptionMarketMonero}, false},
		{"zcash year", []model.SubscriptionMarket{model.SubscriptionMarketZcash}, false},
		// mmm/ONBOARDING-RETENTION.md P17: a USDC year never renews either, and
		// it blocked the card checkout its own renewal notices sent the buyer
		// to, through the 30-day grace
		{"usdc year", []model.SubscriptionMarket{model.SubscriptionMarketSolana}, false},
		{"usdc year and stripe", []model.SubscriptionMarket{model.SubscriptionMarketSolana, model.SubscriptionMarketStripe}, true},
		// the TAO, Bitcoin and ETH years are one-time too (mmm/BTCPAY.md §17)
		{"tao year", []model.SubscriptionMarket{model.SubscriptionMarketBittensor}, false},
		{"bitcoin year", []model.SubscriptionMarket{model.SubscriptionMarketBitcoin}, false},
		{"eth year", []model.SubscriptionMarket{model.SubscriptionMarketEthereum}, false},
		{"bitcoin year and apple", []model.SubscriptionMarket{model.SubscriptionMarketBitcoin, model.SubscriptionMarketApple}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := subscriptionMarketsBlockNewPurchase(test.markets); got != test.want {
				t.Fatalf("block = %t, want %t", got, test.want)
			}
		})
	}
}

// The full list: every market but the store subscriptions (Apple, Google,
// Stripe) leaves a new purchase open. Manual grants, x402 and every one-time
// crypto market (Solana from P17, Monero, Zcash, and TAO, Bitcoin and ETH)
// never renew, so a card checkout is never a second charge. A one-time crypto
// market added later must join the list.
func TestSubscriptionMarketsBlockNewPurchaseFullList(t *testing.T) {
	open := []model.SubscriptionMarket{
		model.SubscriptionMarketManual,
		model.SubscriptionMarketX402,
		model.SubscriptionMarketSolana,
		model.SubscriptionMarketMonero,
		model.SubscriptionMarketZcash,
		model.SubscriptionMarketBittensor,
		model.SubscriptionMarketBitcoin,
		model.SubscriptionMarketEthereum,
	}
	blocking := []model.SubscriptionMarket{
		model.SubscriptionMarketApple,
		model.SubscriptionMarketGoogle,
		model.SubscriptionMarketStripe,
	}
	for _, market := range open {
		if subscriptionMarketsBlockNewPurchase([]model.SubscriptionMarket{market}) {
			t.Fatalf("%s blocks a new purchase", market)
		}
	}
	for _, market := range blocking {
		if !subscriptionMarketsBlockNewPurchase([]model.SubscriptionMarket{market}) {
			t.Fatalf("%s does not block a new purchase", market)
		}
	}
	for _, market := range model.SubscriptionOneTimeCryptoMarkets {
		if subscriptionMarketsBlockNewPurchase([]model.SubscriptionMarket{market}) {
			t.Fatalf("the one-time crypto market %s blocks a new purchase", market)
		}
	}
	if subscriptionMarketsBlockNewPurchase(open) {
		t.Fatalf("the open markets together block a new purchase")
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
