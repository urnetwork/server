package controller

// The purchase paths priced from the server's country signals, against the
// test database, through their entry points: on every rail without a store
// (crypto with its quote, Solana Pay, Gamma with its terms, Stripe's prices,
// payment sheet and Checkout) a client-sent regional country with
// standard-country signals is charged standard, and every signal regional
// is charged regional whatever country the client sends; each purchase keeps
// its evidence; a Stripe trial's regional card does not make a subscription
// priced standard regional; and the store apps' plan display keeps their
// storefront. The networks' sign-up countries are rows of network_onboarding;
// their requests' addresses are placed per network. Needs the test database
// and pro.yml.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Where each test network's requests come from, by network.
type priceTierDbAddresses map[server.Id]string

// Places every request in its network's country for one test.
func installPriceTierDbAddresses(t testing.TB) priceTierDbAddresses {
	addresses := priceTierDbAddresses{}
	previous := priceTierClientCountryFunc
	priceTierClientCountryFunc = func(clientSession *session.ClientSession) string {
		if clientSession.ByJwt == nil {
			return ""
		}
		return addresses[clientSession.ByJwt.NetworkId]
	}
	t.Cleanup(func() {
		priceTierClientCountryFunc = previous
	})
	return addresses
}

// A signed-in network that signed up from country, and whose requests come
// from it.
func priceTierDbNetwork(t testing.TB, ctx context.Context, addresses priceTierDbAddresses, name string, country string) (server.Id, *session.ClientSession) {
	networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, name)
	created := model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{
		NetworkId: networkId,
		CreatedAt: server.NowUtc(),
		Country:   country,
	})
	connect.AssertEqual(t, created, true)
	addresses[networkId] = country
	return networkId, clientSession
}

func priceTierDbEvidence(t testing.TB, value string) *PriceTierEvidence {
	evidence := &PriceTierEvidence{}
	if err := json.Unmarshal([]byte(value), evidence); err != nil {
		t.Fatalf("price tier evidence %q: %s", value, err)
	}
	return evidence
}

// The evidence of a network in country with a claim, as each row keeps it.
func priceTierDbExpectedEvidence(tier string, claimedCountry string, country string) *PriceTierEvidence {
	return &PriceTierEvidence{
		Tier:           tier,
		Source:         PriceTierSourceSignals,
		ClaimedCountry: claimedCountry,
		Signals: []*PriceTierSignal{
			priceTierTestSignal(PriceTierSignalIp, country, tier),
			priceTierTestSignal(PriceTierSignalSignup, country, tier),
		},
	}
}

// A crypto plan: claimed IN with US signals is a 40 USD invoice, claimed US
// with IN signals a 4 USD one, each keeping its evidence; the quote prices as
// the invoice does, and a data pack keeps none.
func TestCryptoPlanPricedFromTheServerSignals(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		priceTierTestTiers(t)
		cryptoTestResetLimiters()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		solanaTestAmountSuffixes(t, 301, 302, 303)
		addresses := installPriceTierDbAddresses(t)
		_, standardSession := priceTierDbNetwork(t, ctx, addresses, "pricetierstandard", "US")
		_, regionalSession := priceTierDbNetwork(t, ctx, addresses, "pricetierregional", "IN")

		standard, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, Plan: model.SolanaPlanYearly, StorefrontCountry: "IN"}, standardSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, standard.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, standard.Tier, model.PriceTierStandard)
		connect.AssertEqual(t, standard.PriceUsd, 40.0)
		connect.AssertEqual(t, standard.Amount, "40.000301")
		intent := model.GetCryptoPaymentIntent(ctx, server.RequireParseId(standard.IntentId))
		connect.AssertEqual(t, intent.Tier, model.PriceTierStandard)
		connect.AssertEqual(t, priceTierDbEvidence(t, intent.PriceTierEvidenceJson), priceTierDbExpectedEvidence(model.PriceTierStandard, "IN", "US"))

		regional, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, Plan: model.SolanaPlanYearly, StorefrontCountry: "US"}, regionalSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, regional.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, regional.Tier, priceTierTestRegional)
		connect.AssertEqual(t, regional.PriceUsd, 4.0)
		connect.AssertEqual(t, regional.Amount, "4.000302")
		intent = model.GetCryptoPaymentIntent(ctx, server.RequireParseId(regional.IntentId))
		connect.AssertEqual(t, priceTierDbEvidence(t, intent.PriceTierEvidenceJson), priceTierDbExpectedEvidence(priceTierTestRegional, "US", "IN"))

		quote, err := CryptoPaymentQuote(&CryptoPaymentQuoteArgs{Asset: model.CryptoAssetUsdcSolana, Plan: model.SolanaPlanYearly, StorefrontCountry: "IN"}, standardSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, quote.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, quote.Tier, model.PriceTierStandard)
		connect.AssertEqual(t, quote.PriceUsd, 40.0)

		pack, err := CreateCryptoPaymentIntent(&CreateCryptoPaymentIntentArgs{Asset: model.CryptoAssetUsdcSolana, ItemId: StripeItemData1Tib, StorefrontCountry: "IN"}, standardSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, pack.Error, (*CryptoPaymentError)(nil))
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, server.RequireParseId(pack.IntentId)).PriceTierEvidenceJson, "")
	})
}

// The Solana Pay plan intent: claimed IN with US signals quotes 40 USD,
// claimed US with IN signals 4, and the intent keeps its evidence.
func TestSolanaPayPlanPricedFromTheServerSignals(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		priceTierTestTiers(t)
		cryptoTestResetLimiters()
		solanaTestAmountSuffixes(t, 401, 402)
		addresses := installPriceTierDbAddresses(t)
		_, standardSession := priceTierDbNetwork(t, ctx, addresses, "pricetiersolanastandard", "US")
		_, regionalSession := priceTierDbNetwork(t, ctx, addresses, "pricetiersolanaregional", "IN")

		intentEvidence := func(reference string) *PriceTierEvidence {
			var value string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT price_tier_evidence::text FROM solana_payment_intent WHERE payment_reference = $1`,
					reference,
				).Scan(&value))
			})
			return priceTierDbEvidence(t, value)
		}

		standardReference := cryptoNewSolanaReference()
		standard, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{Reference: standardReference, Plan: model.SolanaPlanYearly, StorefrontCountry: "IN"}, standardSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, standard.Error, (*SolanaPaymentIntentError)(nil))
		connect.AssertEqual(t, standard.Tier, model.PriceTierStandard)
		connect.AssertEqual(t, standard.AmountUsd, 40.000401)
		connect.AssertEqual(t, intentEvidence(standardReference), priceTierDbExpectedEvidence(model.PriceTierStandard, "IN", "US"))

		regionalReference := cryptoNewSolanaReference()
		regional, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{Reference: regionalReference, Plan: model.SolanaPlanYearly, StorefrontCountry: "US"}, regionalSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, regional.Error, (*SolanaPaymentIntentError)(nil))
		connect.AssertEqual(t, regional.Tier, priceTierTestRegional)
		connect.AssertEqual(t, regional.AmountUsd, 4.000402)
		connect.AssertEqual(t, intentEvidence(regionalReference), priceTierDbExpectedEvidence(priceTierTestRegional, "US", "IN"))
	})
}

// A Gamma plan: claimed IN with US signals is 40 gamma, claimed US with IN
// signals 4, each intent keeping its evidence; the terms show the tier an
// intent is priced at.
func TestGammaPlanPricedFromTheServerSignals(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		priceTierTestTiers(t)
		addresses := installPriceTierDbAddresses(t)
		_, standardSession := priceTierDbNetwork(t, ctx, addresses, "pricetiergammastandard", "US")
		_, regionalSession := priceTierDbNetwork(t, ctx, addresses, "pricetiergammaregional", "IN")
		connect.AssertEqual(t, f.run().Readiness, gammaReady)

		standard := gammaDbIntent(t, standardSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly, StorefrontCountry: "IN"})
		connect.AssertEqual(t, standard.Error == nil, true)
		connect.AssertEqual(t, standard.Tier, model.PriceTierStandard)
		connect.AssertEqual(t, standard.AmountGamma, "40")
		intent := model.GetGammaPaymentIntent(ctx, server.RequireParseId(standard.IntentId))
		connect.AssertEqual(t, priceTierDbEvidence(t, intent.PriceTierEvidenceJson), priceTierDbExpectedEvidence(model.PriceTierStandard, "IN", "US"))

		regional := gammaDbIntent(t, regionalSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly, StorefrontCountry: "US"})
		connect.AssertEqual(t, regional.Error == nil, true)
		connect.AssertEqual(t, regional.Tier, priceTierTestRegional)
		connect.AssertEqual(t, regional.AmountGamma, "4")
		intent = model.GetGammaPaymentIntent(ctx, server.RequireParseId(regional.IntentId))
		connect.AssertEqual(t, priceTierDbEvidence(t, intent.PriceTierEvidenceJson), priceTierDbExpectedEvidence(priceTierTestRegional, "US", "IN"))

		for _, c := range []struct {
			clientSession *session.ClientSession
			tier          string
		}{
			{clientSession: standardSession, tier: model.PriceTierStandard},
			{clientSession: regionalSession, tier: priceTierTestRegional},
		} {
			terms, err := GammaTerms(c.clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, terms.PriceTier, c.tier)
		}
	})
}

// Stripe's prices, payment sheet and Checkout: claimed IN with US signals are
// the standard prices, claimed US with IN signals the regional ones, and each
// subscription keeps what priced it, without a country.
func TestStripePurchasesPricedFromTheServerSignals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installWinbackTestEnv(t)
		priceTierTestTiers(t)
		stripeEnv := installWinbackStripeTestEnv(t)
		addresses := installPriceTierDbAddresses(t)

		for _, c := range []struct {
			name           string
			country        string
			claimedCountry string
			tier           string
			evidence       string
		}{
			{
				name:           "pricetierstripestandard",
				country:        "US",
				claimedCountry: "IN",
				tier:           model.PriceTierStandard,
				evidence:       `{"tier":"standard","source":"signals","signals":["ip:standard","signup:standard"]}`,
			},
			{
				name:           "pricetierstriperegional",
				country:        "IN",
				claimedCountry: "US",
				tier:           priceTierTestRegional,
				evidence:       `{"tier":"regional","source":"signals","signals":["ip:regional","signup:regional"]}`,
			},
		} {
			networkId, clientSession := priceTierDbNetwork(t, ctx, addresses, c.name, c.country)
			connect.AssertEqual(t, model.CreateStripeCustomerForNetwork(ctx, networkId, "cus_"+c.name), nil)

			prices, err := StripePrices(c.claimedCountry, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, prices.Error, (*OnboardingError)(nil))
			connect.AssertEqual(t, prices.Tier, c.tier)
			connect.AssertEqual(t, prices.YearlyPriceId, "price_"+c.tier+"_yearly")

			sheet, err := StripePaymentSheet(&StripePaymentSheetArgs{Plan: model.PlanYearly, StorefrontCountry: c.claimedCountry}, clientSession)
			connect.AssertEqual(t, err, nil)
			if sheet.Error != nil {
				t.Fatalf("%s: payment sheet refused: %+v (unexpected stripe calls %v)", c.name, sheet.Error, stripeEnv.unexpected)
			}
			connect.AssertEqual(t, sheet.Tier, c.tier)
			form := stripeEnv.lastSubscriptionForm()
			connect.AssertEqual(t, form.Get("items[0][price]"), "price_"+c.tier+"_yearly")
			connect.AssertEqual(t, form.Get("metadata[price_tier]"), c.tier)
			connect.AssertEqual(t, form.Get("metadata[price_tier_evidence]"), c.evidence)

			checkout, err := StripeCreateCheckoutSession(&StripeCreateCheckoutSessionArgs{
				ItemId:               StripeItemProMonthly,
				StorefrontCountry:    c.claimedCountry,
				UiMode:               StripeUiModeEmbedded,
				RedirectOnCompletion: "never",
			}, clientSession)
			connect.AssertEqual(t, err, nil)
			if checkout.Error != nil {
				t.Fatalf("%s: checkout refused: %+v (unexpected stripe calls %v)", c.name, checkout.Error, stripeEnv.unexpected)
			}
			checkoutForm := stripeEnv.lastCheckoutForm()
			connect.AssertEqual(t, checkoutForm.Get("line_items[0][price]"), "price_"+c.tier+"_monthly")
			connect.AssertEqual(t, checkoutForm.Get("subscription_data[metadata][price_tier]"), c.tier)
			connect.AssertEqual(t, checkoutForm.Get("subscription_data[metadata][price_tier_evidence]"), c.evidence)
		}
		connect.AssertEqual(t, len(stripeEnv.unexpected), 0)
	})
}

// A trial's card joins its subscription's signals: a regional card leaves a
// subscription priced standard from its signals at standard, and a standard
// card moves one priced regional to standard before the trial ends.
func TestStripeTrialCardJoinsTheSignalsItWasPricedFrom(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		priceTierTestTiers(t)
		fake := newStripeCardCheckFake(t)
		for key, id := range map[string]string{
			model.PriceTierStandard + "/" + model.PlanYearly: "price_standard_yearly",
			priceTierTestRegional + "/" + model.PlanYearly:   "price_regional_yearly",
		} {
			stripeTierPriceCache.Store(key, id)
			t.Cleanup(func() {
				stripeTierPriceCache.Delete(key)
			})
		}
		network := newStripeCardCheckDbNetwork(t, ctx, "pricetiertrial")
		now := server.NowUtc()
		trial := func(subscriptionId string, tier string, signals string) {
			fake.addSubscription(subscriptionId, network.customerId, network.networkId, "trialing", map[string]string{
				stripeMetadataPlan:              model.PlanYearly,
				stripeMetadataPriceTier:         tier,
				stripeMetadataPriceTierEvidence: `{"tier":"` + tier + `","source":"signals","signals":` + signals + `}`,
			}, "", "", now.Add(-time.Minute), now.Add(StripeSubscriptionTrialDays*24*time.Hour))
			fake.setSubscriptionField(subscriptionId, "items", map[string]any{"object": "list", "data": []any{map[string]any{
				"id":     "si_" + subscriptionId,
				"object": "subscription_item",
				"price":  map[string]any{"id": "price_" + tier + "_yearly", "object": "price"},
			}}})
		}
		attachCard := func(country string) {
			deliverStripeCardCheckEvent(t, ctx, "payment_method.attached", map[string]any{
				"id":       "pm_" + country,
				"object":   "payment_method",
				"customer": network.customerId,
				"card":     map[string]any{"brand": "visa", "last4": "4242", "country": country},
			})
		}

		// priced standard: its address was in a standard country
		trial("sub_pricetier_standard", model.PriceTierStandard, `["ip:standard","signup:regional"]`)
		attachCard("IN")
		updates, _, _ := fake.writes()
		connect.AssertEqual(t, len(updates), 0)
		connect.AssertEqual(t, fake.metadata("sub_pricetier_standard")[stripeMetadataPriceTier], model.PriceTierStandard)
		connect.AssertEqual(t, model.GetStripeCustomerBillingCountry(ctx, network.networkId), "IN")

		// priced regional from every signal: a standard card moves it
		trial("sub_pricetier_regional", priceTierTestRegional, `["ip:regional","signup:regional"]`)
		attachCard("US")
		updates, _, _ = fake.writes()
		connect.AssertEqual(t, len(updates), 1)
		connect.AssertEqual(t, updates[0], "sub_pricetier_regional "+
			"items[0][id]=si_sub_pricetier_regional&"+
			"items[0][price]=price_standard_yearly&"+
			`metadata[price_tier]=standard&`+
			`metadata[price_tier_evidence]={"tier":"standard","source":"signals","signals":["ip:regional","signup:regional","billing:standard"]}&`+
			"proration_behavior=none")
		connect.AssertEqual(t, fake.metadata("sub_pricetier_regional")[stripeMetadataPriceTier], model.PriceTierStandard)
	})
}

// The store apps' plan display keeps the storefront's tier, which the store
// charges, against standard-country signals; without a storefront the plan
// shows the tier the server charges.
func TestPlanResponseKeepsTheStorefrontTier(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		priceTierTestTiers(t)
		addresses := installPriceTierDbAddresses(t)
		_, clientSession := priceTierDbNetwork(t, ctx, addresses, "pricetierplan", "US")

		store, err := SubscriptionBalanceForStorefront("IND", clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, store.PriceTier.Name, priceTierTestRegional)
		connect.AssertEqual(t, store.PriceTier.Source, PriceTierSourceStorefront)
		connect.AssertEqual(t, store.PriceTier.Estimate, false)

		charged, err := SubscriptionBalanceForStorefront("", clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, charged.PriceTier.Name, model.PriceTierStandard)
		connect.AssertEqual(t, charged.PriceTier.Source, PriceTierSourceSignals)
		connect.AssertEqual(t, charged.PriceTier.Estimate, false)
	})
}
