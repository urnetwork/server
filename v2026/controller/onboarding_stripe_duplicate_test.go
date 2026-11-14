package controller

import (
	"slices"
	"strings"
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
		// the network's plan paid with Gamma renews (auto_renew on)
		gammaRenews bool
		want        bool
	}{
		{"none", nil, false, false},
		{"stripe", []model.SubscriptionMarket{model.SubscriptionMarketStripe}, false, true},
		{"apple", []model.SubscriptionMarket{model.SubscriptionMarketApple}, false, true},
		{"google", []model.SubscriptionMarket{model.SubscriptionMarketGoogle}, false, true},
		{"unknown legacy market", []model.SubscriptionMarket{""}, false, true},
		{"manual grant only", []model.SubscriptionMarket{model.SubscriptionMarketManual}, false, false},
		{"x402 only", []model.SubscriptionMarket{model.SubscriptionMarketX402}, false, false},
		{"manual and stripe", []model.SubscriptionMarket{model.SubscriptionMarketManual, model.SubscriptionMarketStripe}, false, true},
		{"monero year", []model.SubscriptionMarket{model.SubscriptionMarketMonero}, false, false},
		{"zcash year", []model.SubscriptionMarket{model.SubscriptionMarketZcash}, false, false},
		// mmm/ONBOARDING-RETENTION.md P17: a USDC year never renews either, and
		// it blocked the card checkout its own renewal notices sent the buyer
		// to, through the 30-day grace
		{"usdc year", []model.SubscriptionMarket{model.SubscriptionMarketSolana}, false, false},
		{"usdc year and stripe", []model.SubscriptionMarket{model.SubscriptionMarketSolana, model.SubscriptionMarketStripe}, false, true},
		// the TAO, Bitcoin and ETH years are one-time too (mmm/BTCPAY.md §17)
		{"tao year", []model.SubscriptionMarket{model.SubscriptionMarketBittensor}, false, false},
		{"bitcoin year", []model.SubscriptionMarket{model.SubscriptionMarketBitcoin}, false, false},
		{"eth year", []model.SubscriptionMarket{model.SubscriptionMarketEthereum}, false, false},
		{"bitcoin year and apple", []model.SubscriptionMarket{model.SubscriptionMarketBitcoin, model.SubscriptionMarketApple}, false, true},
		// the owner, 2026-11-11: a plan paid with Gamma blocks while it renews
		// from the deposit; with renewal off it ends with its term like a
		// crypto year
		{"gamma plan renewing", []model.SubscriptionMarket{model.SubscriptionMarketGamma}, true, true},
		{"gamma plan, renewal off", []model.SubscriptionMarket{model.SubscriptionMarketGamma}, false, false},
		{"gamma plan, renewal off, and a usdc year", []model.SubscriptionMarket{model.SubscriptionMarketGamma, model.SubscriptionMarketSolana}, false, false},
		{"gamma plan, renewal off, and stripe", []model.SubscriptionMarket{model.SubscriptionMarketGamma, model.SubscriptionMarketStripe}, false, true},
		// the renewal of a plan paid with Gamma is only about that plan
		{"usdc year, gamma renewal on but no gamma plan", []model.SubscriptionMarket{model.SubscriptionMarketSolana}, true, false},
		// the owner, 2026-11-13: a redeemed gift year never renews, like a
		// crypto year (mmm/GIFTOFPRIVACY.md)
		{"gift year", []model.SubscriptionMarket{model.SubscriptionMarketGift}, false, false},
		{"gift year, gamma renewal on but no gamma plan", []model.SubscriptionMarket{model.SubscriptionMarketGift}, true, false},
		{"gift year and stripe", []model.SubscriptionMarket{model.SubscriptionMarketGift, model.SubscriptionMarketStripe}, false, true},
		{"gift year and a renewing gamma plan", []model.SubscriptionMarket{model.SubscriptionMarketGift, model.SubscriptionMarketGamma}, true, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := subscriptionMarketsBlockNewPurchase(test.markets, test.gammaRenews); got != test.want {
				t.Fatalf("block = %t, want %t", got, test.want)
			}
		})
	}
}

// The full list: every market but the store subscriptions (Apple, Google,
// Stripe) and a renewing plan paid with Gamma leaves a new purchase open.
// Manual grants, x402, every one-time crypto market (Solana from P17, Monero,
// Zcash, and TAO, Bitcoin and ETH), a redeemed gift year and a Gamma plan with
// renewal off never renew, so a card checkout is never a second charge. A
// one-time crypto market added later must join the list.
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
		model.SubscriptionMarketGift,
	}
	blocking := []model.SubscriptionMarket{
		model.SubscriptionMarketApple,
		model.SubscriptionMarketGoogle,
		model.SubscriptionMarketStripe,
	}
	for _, gammaRenews := range []bool{false, true} {
		for _, market := range open {
			if subscriptionMarketsBlockNewPurchase([]model.SubscriptionMarket{market}, gammaRenews) {
				t.Fatalf("%s blocks a new purchase (gamma renews %t)", market, gammaRenews)
			}
		}
		for _, market := range blocking {
			if !subscriptionMarketsBlockNewPurchase([]model.SubscriptionMarket{market}, gammaRenews) {
				t.Fatalf("%s does not block a new purchase (gamma renews %t)", market, gammaRenews)
			}
		}
		for _, market := range model.SubscriptionOneTimeCryptoMarkets {
			if subscriptionMarketsBlockNewPurchase([]model.SubscriptionMarket{market}, gammaRenews) {
				t.Fatalf("the one-time crypto market %s blocks a new purchase (gamma renews %t)", market, gammaRenews)
			}
		}
		if subscriptionMarketsBlockNewPurchase(open, gammaRenews) {
			t.Fatalf("the open markets together block a new purchase (gamma renews %t)", gammaRenews)
		}
	}
	gamma := []model.SubscriptionMarket{model.SubscriptionMarketGamma}
	if subscriptionMarketsBlockNewPurchase(gamma, false) {
		t.Fatalf("a Gamma plan with renewal off blocks a new purchase")
	}
	if !subscriptionMarketsBlockNewPurchase(gamma, true) {
		t.Fatalf("a renewing Gamma plan does not block a new purchase")
	}
	if subscriptionMarketsBlockNewPurchase(append(slices.Clone(open), model.SubscriptionMarketGamma), false) {
		t.Fatalf("the open markets and a Gamma plan with renewal off together block a new purchase")
	}
}

// The biller renews a plan paid with Gamma only with renewal on and a plan to
// renew; card checkout keys off the same answer.
func TestGammaAccountRenews(t *testing.T) {
	tests := []struct {
		name    string
		account *model.GammaAccount
		want    bool
	}{
		{"never paid with gamma", nil, false},
		{"renewal on", &model.GammaAccount{AutoRenew: true, RenewPlan: model.SolanaPlanYearly}, true},
		{"renewal off", &model.GammaAccount{AutoRenew: false, RenewPlan: model.SolanaPlanYearly}, false},
		{"renewal on, no plan paid yet", &model.GammaAccount{AutoRenew: true}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := gammaAccountRenews(test.account); got != test.want {
				t.Fatalf("renews = %t, want %t", got, test.want)
			}
		})
	}
}

// The refusal beside a renewing plan paid with Gamma says that it renews and
// names the switch that ends the renewal.
func TestStripeGammaRenewsMessage(t *testing.T) {
	for _, want := range []string{"Gamma", "renews", "POST /pay/gamma/settings", "auto_renew false", gammaManageUrl} {
		if !strings.Contains(stripeGammaRenewsMessage, want) {
			t.Fatalf("the refusal %q does not say %q", stripeGammaRenewsMessage, want)
		}
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
