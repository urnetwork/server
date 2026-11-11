package controller

// A plan paid with Gamma and card checkout (owner, 2026-11-11). While the plan
// renews from the account's deposit (auto_renew on, the default), a card
// subscription beside it would charge the customer twice, so both card paths
// (the payment sheet and web Checkout) refuse and say that the Gamma plan
// renews. With renewal off the plan ends with its term like a one-time crypto
// year (P17, 19deb42c8), so card checkout is open. A store subscription still
// refuses with the general message. Against the test database and a fake
// Stripe API.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// gammaCardTestNetwork is a signed-in network with a Stripe customer on file
// and a yearly plan paid with Gamma, recorded as the credit records it: the
// gamma renewal and the account row, automatic renewal on.
func gammaCardTestNetwork(t testing.TB, ctx context.Context, name string) (server.Id, *session.ClientSession) {
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, name+"-"+networkId.String(), userId)
	connect.AssertEqual(t, model.CreateStripeCustomerForNetwork(ctx, networkId, winbackStripeTestCustomerId), nil)
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId})

	now := server.NowUtc()
	start := now.Add(-24 * time.Hour)
	connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          start,
		EndTime:            start.Add(SubscriptionYearDuration + manualPaymentGracePeriod),
		NetRevenue:         model.UsdToNanoCents(40),
		SubscriptionMarket: model.SubscriptionMarketGamma,
		TransactionId:      server.NewId().String(),
	}), nil)
	server.Tx(ctx, func(tx server.PgTx) {
		model.RecordGammaAccountPaymentInTx(tx, ctx, networkId, gammaTestColdkey, model.SolanaPlanYearly, model.PriceTierStandard, now)
	})
	account := model.GetGammaAccount(ctx, networkId)
	connect.AssertEqual(t, account.AutoRenew, true)
	return networkId, clientSession
}

func gammaCardTestSheet(t testing.TB, clientSession *session.ClientSession) *StripePaymentSheetResult {
	result, err := StripePaymentSheet(&StripePaymentSheetArgs{Plan: model.PlanYearly, StorefrontCountry: "US"}, clientSession)
	connect.AssertEqual(t, err, nil)
	return result
}

func gammaCardTestCheckout(t testing.TB, clientSession *session.ClientSession) *StripeCreateCheckoutSessionResult {
	result, err := StripeCreateCheckoutSession(&StripeCreateCheckoutSessionArgs{
		ItemId:               StripeItemProMonthly,
		StorefrontCountry:    "US",
		UiMode:               StripeUiModeEmbedded,
		RedirectOnCompletion: "never",
	}, clientSession)
	connect.AssertEqual(t, err, nil)
	return result
}

func gammaCardTestSetAutoRenew(t testing.TB, clientSession *session.ClientSession, autoRenew bool) {
	settings, err := SetGammaSettings(&GammaSettingsArgs{AutoRenew: &autoRenew}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, settings.Error == nil, true)
	connect.AssertEqual(t, settings.AutoRenew, autoRenew)
}

// gammaCardTestRenewsRefusal fails unless the refusal is the already-subscribed
// one and names the renewing Gamma plan and the switch that ends its renewal.
func gammaCardTestRenewsRefusal(t testing.TB, entry string, code string, message string) {
	if code != PurchaseErrorCodeAlreadySubscribed ||
		!strings.Contains(message, "Gamma") ||
		!strings.Contains(message, "renews") ||
		!strings.Contains(message, "auto_renew false") {
		t.Fatalf("%s: code=%q message=%q, want the already-subscribed refusal saying the Gamma plan renews", entry, code, message)
	}
}

// With its renewal off, a plan paid with Gamma ends with its term like a
// crypto year: the payment sheet and web Checkout both start a card
// subscription. A store subscription beside it still refuses, with the
// general message.
func TestGammaPlanCardCheckoutOpenWithRenewalOff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installWinbackTestEnv(t)
		stripeEnv := installWinbackStripeTestEnv(t)
		networkId, clientSession := gammaCardTestNetwork(t, ctx, "gammacardoff")

		gammaCardTestSetAutoRenew(t, clientSession, false)

		sheet := gammaCardTestSheet(t, clientSession)
		if sheet.Error != nil {
			t.Fatalf("payment sheet beside a Gamma plan with renewal off was refused: %+v (unexpected stripe calls %v)", sheet.Error, stripeEnv.unexpected)
		}
		connect.AssertEqual(t, len(stripeEnv.subscriptionForms), 1)
		connect.AssertEqual(t, stripeEnv.lastSubscriptionForm().Get("customer"), winbackStripeTestCustomerId)
		connect.AssertEqual(t, stripeEnv.lastSubscriptionForm().Get("items[0][price]"), "price_standard_yearly")

		checkout := gammaCardTestCheckout(t, clientSession)
		if checkout.Error != nil {
			t.Fatalf("checkout beside a Gamma plan with renewal off was refused: %+v (unexpected stripe calls %v)", checkout.Error, stripeEnv.unexpected)
		}
		connect.AssertEqual(t, len(stripeEnv.checkoutForms), 1)
		connect.AssertEqual(t, stripeEnv.lastCheckoutForm().Get("line_items[0][price]"), "price_standard_monthly")

		// a store subscription bills on its own: the general refusal
		now := server.NowUtc()
		connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-time.Hour),
			EndTime:            now.Add(30 * 24 * time.Hour),
			NetRevenue:         model.UsdToNanoCents(5),
			SubscriptionMarket: model.SubscriptionMarketApple,
			TransactionId:      "apple-gammacard-" + networkId.String(),
		}), nil)
		storeSheet := gammaCardTestSheet(t, clientSession)
		if storeSheet.Error == nil || storeSheet.Error.Code != PurchaseErrorCodeAlreadySubscribed || storeSheet.Error.Message != stripeAlreadySubscribedMessage {
			t.Fatalf("payment sheet beside an App Store subscription: %+v, want the general refusal", storeSheet.Error)
		}
		storeCheckout := gammaCardTestCheckout(t, clientSession)
		if storeCheckout.Error == nil || storeCheckout.Error.Code != PurchaseErrorCodeAlreadySubscribed || storeCheckout.Error.Message != stripeAlreadySubscribedMessage {
			t.Fatalf("checkout beside an App Store subscription: %+v, want the general refusal", storeCheckout.Error)
		}
		// with renewal on as well, the store subscription is still the reason given
		gammaCardTestSetAutoRenew(t, clientSession, true)
		bothSheet := gammaCardTestSheet(t, clientSession)
		if bothSheet.Error == nil || bothSheet.Error.Message != stripeAlreadySubscribedMessage {
			t.Fatalf("payment sheet beside an App Store subscription and a renewing Gamma plan: %+v, want the general refusal", bothSheet.Error)
		}
		connect.AssertEqual(t, len(stripeEnv.subscriptionForms), 1)
		connect.AssertEqual(t, len(stripeEnv.checkoutForms), 1)
		connect.AssertEqual(t, len(stripeEnv.unexpected), 0)
	})
}

// While a plan paid with Gamma renews (auto_renew on, the default), both card
// paths refuse, say that the Gamma plan renews, and ask Stripe for nothing.
// Turning renewal off opens them; turning it back on refuses again.
func TestGammaPlanCardCheckoutRefusedWhileItRenews(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installWinbackTestEnv(t)
		stripeEnv := installWinbackStripeTestEnv(t)
		_, clientSession := gammaCardTestNetwork(t, ctx, "gammacardon")

		sheet := gammaCardTestSheet(t, clientSession)
		if sheet.Error == nil {
			t.Fatalf("payment sheet beside a renewing Gamma plan was not refused: %+v", sheet)
		}
		gammaCardTestRenewsRefusal(t, "payment sheet", sheet.Error.Code, sheet.Error.Message)
		checkout := gammaCardTestCheckout(t, clientSession)
		if checkout.Error == nil {
			t.Fatalf("checkout beside a renewing Gamma plan was not refused: %+v", checkout)
		}
		gammaCardTestRenewsRefusal(t, "checkout", checkout.Error.Code, checkout.Error.Message)
		connect.AssertEqual(t, len(stripeEnv.subscriptionForms), 0)
		connect.AssertEqual(t, len(stripeEnv.checkoutForms), 0)

		gammaCardTestSetAutoRenew(t, clientSession, false)
		if open := gammaCardTestSheet(t, clientSession); open.Error != nil {
			t.Fatalf("payment sheet beside a Gamma plan with renewal off was refused: %+v (unexpected stripe calls %v)", open.Error, stripeEnv.unexpected)
		}
		connect.AssertEqual(t, len(stripeEnv.subscriptionForms), 1)

		gammaCardTestSetAutoRenew(t, clientSession, true)
		again := gammaCardTestCheckout(t, clientSession)
		if again.Error == nil {
			t.Fatalf("checkout beside a Gamma plan with renewal back on was not refused: %+v", again)
		}
		gammaCardTestRenewsRefusal(t, "checkout, renewal back on", again.Error.Code, again.Error.Message)
		connect.AssertEqual(t, len(stripeEnv.checkoutForms), 0)
		connect.AssertEqual(t, len(stripeEnv.unexpected), 0)
	})
}
