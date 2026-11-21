package controller

// The price tier of a purchase the server prices itself
// (price_tier_signals.go), without a database: a country the client sends
// never prices it, the regional tier needs every server-side signal to agree,
// a value that is not a country is no signal, what a Stripe subscription
// keeps holds no country, and a trial's card joins the subscription's signals
// rather than deciding alone. The request's address and the network's
// countries are fixed per test; the tiers are pinned as pro.yml shapes them.

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const priceTierTestRegional = "regional"

// The tiers these tests price at: standard at 40 USD a year for four
// countries, regional at 4 for every other.
func priceTierTestTiers(t testing.TB) {
	restore := model.Testing_SetProPriceTiers(
		model.Testing_ProPriceTier(model.PriceTierStandard, 5, 40, "US", "CA", "DE", "GB"),
		model.Testing_ProPriceTier(priceTierTestRegional, 0.5, 4),
	)
	t.Cleanup(restore)
}

// The request's address is placed in ipCountry ("" for nowhere) for one
// test, and when countries is not nil, the network's countries are those
// instead of the database's.
func priceTierTestSignals(t testing.TB, ipCountry string, countries *model.NetworkCountrySignals) {
	previousIp := priceTierClientCountryFunc
	previousNetwork := priceTierNetworkCountriesFunc
	priceTierClientCountryFunc = func(clientSession *session.ClientSession) string {
		return ipCountry
	}
	if countries != nil {
		priceTierNetworkCountriesFunc = func(ctx context.Context, networkId server.Id, settings *priceTierSignalSettings) *model.NetworkCountrySignals {
			return countries
		}
	}
	t.Cleanup(func() {
		priceTierClientCountryFunc = previousIp
		priceTierNetworkCountriesFunc = previousNetwork
	})
}

func priceTierTestSignal(kind string, country string, tier string) *PriceTierSignal {
	return &PriceTierSignal{Kind: kind, Country: country, Tier: tier}
}

// A claimed regional country with standard-country signals is priced
// standard, and a claimed standard country with regional signals is priced
// regional: the claim is kept in the evidence and nothing else.
func TestPurchasePriceTierIgnoresTheClaimedCountry(t *testing.T) {
	priceTierTestTiers(t)
	clientSession := solanaTestPaymentSession(server.NewId())

	priceTierTestSignals(t, "US", &model.NetworkCountrySignals{Signup: "US"})
	tier := ResolvePurchasePriceTier(clientSession, "IN")
	connect.AssertEqual(t, tier.Tier.Name, model.PriceTierStandard)
	connect.AssertEqual(t, tier.Tier.YearlyUsd, 40.0)
	connect.AssertEqual(t, tier.Source, PriceTierSourceSignals)
	connect.AssertEqual(t, tier.Evidence.Tier, model.PriceTierStandard)
	connect.AssertEqual(t, tier.Evidence.Source, PriceTierSourceSignals)
	connect.AssertEqual(t, tier.Evidence.ClaimedCountry, "IN")
	connect.AssertEqual(t, tier.Evidence.Signals, []*PriceTierSignal{
		priceTierTestSignal(PriceTierSignalIp, "US", model.PriceTierStandard),
		priceTierTestSignal(PriceTierSignalSignup, "US", model.PriceTierStandard),
	})
	// an App Store storefront's alpha-3 claim alike
	tier = ResolvePurchasePriceTier(clientSession, "IND")
	connect.AssertEqual(t, tier.Tier.Name, model.PriceTierStandard)
	connect.AssertEqual(t, tier.Evidence.ClaimedCountry, "IN")

	priceTierTestSignals(t, "IN", &model.NetworkCountrySignals{Signup: "IN"})
	tier = ResolvePurchasePriceTier(clientSession, "US")
	connect.AssertEqual(t, tier.Tier.Name, priceTierTestRegional)
	connect.AssertEqual(t, tier.Tier.YearlyUsd, 4.0)
	connect.AssertEqual(t, tier.Evidence.ClaimedCountry, "US")
}

// Every signal regional prices regional; one signal of any kind in a
// standard country prices standard.
func TestPurchasePriceTierRegionalOnlyWhenEverySignalAgrees(t *testing.T) {
	priceTierTestTiers(t)
	clientSession := solanaTestPaymentSession(server.NewId())

	regional := func() *model.NetworkCountrySignals {
		return &model.NetworkCountrySignals{
			Signup:      "IN",
			Connections: []string{"BR", "IN"},
			Billing:     "IN",
			Stores:      []string{"", "IN"},
		}
	}
	priceTierTestSignals(t, "IN", regional())
	tier := ResolvePurchasePriceTier(clientSession, "")
	connect.AssertEqual(t, tier.Tier.Name, priceTierTestRegional)
	connect.AssertEqual(t, tier.Source, PriceTierSourceSignals)
	connect.AssertEqual(t, tier.Evidence.ClaimedCountry, "")
	connect.AssertEqual(t, tier.Evidence.Signals, []*PriceTierSignal{
		priceTierTestSignal(PriceTierSignalIp, "IN", priceTierTestRegional),
		priceTierTestSignal(PriceTierSignalSignup, "IN", priceTierTestRegional),
		priceTierTestSignal(PriceTierSignalConnection, "BR", priceTierTestRegional),
		priceTierTestSignal(PriceTierSignalConnection, "IN", priceTierTestRegional),
		priceTierTestSignal(PriceTierSignalBilling, "IN", priceTierTestRegional),
		priceTierTestSignal(PriceTierSignalStore, "IN", priceTierTestRegional),
	})

	for _, c := range []struct {
		name      string
		ipCountry string
		standard  func(countries *model.NetworkCountrySignals)
	}{
		{name: "ip", ipCountry: "US", standard: func(countries *model.NetworkCountrySignals) {}},
		{name: "signup", ipCountry: "IN", standard: func(countries *model.NetworkCountrySignals) { countries.Signup = "DE" }},
		{name: "connection", ipCountry: "IN", standard: func(countries *model.NetworkCountrySignals) {
			countries.Connections = append(countries.Connections, "GB")
		}},
		{name: "billing", ipCountry: "IN", standard: func(countries *model.NetworkCountrySignals) { countries.Billing = "CA" }},
		{name: "store", ipCountry: "IN", standard: func(countries *model.NetworkCountrySignals) { countries.Stores[0] = "US" }},
	} {
		countries := regional()
		c.standard(countries)
		priceTierTestSignals(t, c.ipCountry, countries)
		tier := ResolvePurchasePriceTier(clientSession, "IN")
		if tier.Tier.Name != model.PriceTierStandard || tier.Source != PriceTierSourceSignals {
			t.Fatalf("a standard %s signal priced %s from %s", c.name, tier.Tier.Name, tier.Source)
		}
	}
}

// No signal prices the default tier. A value that is not a country is no
// signal, so it neither prices regional alone nor disagrees with the rest.
func TestPurchasePriceTierWithoutASignalIsTheDefault(t *testing.T) {
	priceTierTestTiers(t)
	clientSession := solanaTestPaymentSession(server.NewId())

	priceTierTestSignals(t, "", &model.NetworkCountrySignals{})
	tier := ResolvePurchasePriceTier(clientSession, "IN")
	connect.AssertEqual(t, tier.Tier.Name, model.PriceTierStandard)
	connect.AssertEqual(t, tier.Source, PriceTierSourceDefault)
	connect.AssertEqual(t, tier.Result().Estimate, true)
	connect.AssertEqual(t, len(tier.Evidence.Signals), 0)

	priceTierTestSignals(t, "EU", &model.NetworkCountrySignals{Connections: []string{"A1", "ZZ"}, Stores: []string{"", ""}})
	tier = ResolvePurchasePriceTier(clientSession, "")
	connect.AssertEqual(t, tier.Tier.Name, model.PriceTierStandard)
	connect.AssertEqual(t, tier.Source, PriceTierSourceDefault)

	priceTierTestSignals(t, "EU", &model.NetworkCountrySignals{Signup: "BR", Connections: []string{"A1"}})
	tier = ResolvePurchasePriceTier(clientSession, "")
	connect.AssertEqual(t, tier.Tier.Name, priceTierTestRegional)
	connect.AssertEqual(t, tier.Result().Estimate, false)
	connect.AssertEqual(t, tier.Evidence.Signals, []*PriceTierSignal{
		priceTierTestSignal(PriceTierSignalSignup, "BR", priceTierTestRegional),
	})
}

// A caller without a network (an unsigned crypto quote) is priced by its
// address alone, and no network is read.
func TestPurchasePriceTierWithoutANetworkReadsTheAddressOnly(t *testing.T) {
	priceTierTestTiers(t)
	priceTierTestSignals(t, "BR", nil)
	previousNetwork := priceTierNetworkCountriesFunc
	priceTierNetworkCountriesFunc = func(ctx context.Context, networkId server.Id, settings *priceTierSignalSettings) *model.NetworkCountrySignals {
		t.Fatalf("read the countries of network %s for a caller without one", networkId)
		return nil
	}
	t.Cleanup(func() {
		priceTierNetworkCountriesFunc = previousNetwork
	})
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	tier := ResolvePurchasePriceTier(clientSession, "US")
	connect.AssertEqual(t, tier.Tier.Name, priceTierTestRegional)
	connect.AssertEqual(t, tier.Evidence.Signals, []*PriceTierSignal{
		priceTierTestSignal(PriceTierSignalIp, "BR", priceTierTestRegional),
	})
}

// The store apps' plan display keeps their storefront's tier, which the store
// charges, whatever the server's signals; without a storefront the display is
// the tier the server charges.
func TestPriceTierDisplayKeepsTheStorefront(t *testing.T) {
	priceTierTestTiers(t)
	clientSession := solanaTestPaymentSession(server.NewId())
	priceTierTestSignals(t, "US", &model.NetworkCountrySignals{Signup: "US"})

	storefront := ResolvePriceTier(clientSession, "IND")
	connect.AssertEqual(t, storefront.Tier.Name, priceTierTestRegional)
	connect.AssertEqual(t, storefront.Source, PriceTierSourceStorefront)
	connect.AssertEqual(t, storefront.Result().Estimate, false)
	connect.AssertEqual(t, storefront.Evidence, (*PriceTierEvidence)(nil))

	display := ResolvePriceTier(clientSession, "")
	connect.AssertEqual(t, display.Tier.Name, model.PriceTierStandard)
	connect.AssertEqual(t, display.Result().Source, PriceTierSourceSignals)
	connect.AssertEqual(t, display.Result().Estimate, false)
}

// The evidence a row keeps names the countries; what a Stripe subscription
// keeps names each signal's kind and tier once, without a country, and fits
// one Stripe metadata value even with every kind in both tiers.
func TestPriceTierEvidenceForTheRowAndForStripe(t *testing.T) {
	priceTierTestTiers(t)
	clientSession := solanaTestPaymentSession(server.NewId())
	priceTierTestSignals(t, "IN", &model.NetworkCountrySignals{Signup: "IN", Connections: []string{"BR", "IN"}})
	tier := ResolvePurchasePriceTier(clientSession, "PK")

	connect.AssertEqual(t, tier.Evidence.rowJson(), `{"tier":"regional","source":"signals","claimed_country":"PK","signals":[`+
		`{"kind":"ip","country":"IN","tier":"regional"},`+
		`{"kind":"signup","country":"IN","tier":"regional"},`+
		`{"kind":"connection","country":"BR","tier":"regional"},`+
		`{"kind":"connection","country":"IN","tier":"regional"}]}`)
	connect.AssertEqual(t, tier.Evidence.stripe().metadataValue(), `{"tier":"regional","source":"signals","signals":["ip:regional","signup:regional","connection:regional"]}`)
	connect.AssertEqual(t, (*PriceTierEvidence)(nil).rowJson(), "")

	every := &model.NetworkCountrySignals{
		Signup:      "US",
		Connections: []string{"AR", "BR", "CA", "DE", "GB", "IN", "MX", "NG", "PH", "US", "VN"},
		Billing:     "GB",
		Stores:      []string{"DE", "IN"},
	}
	priceTierTestSignals(t, "BR", every)
	value := ResolvePurchasePriceTier(clientSession, "IN").Evidence.stripe().metadataValue()
	if value == "" || stripeMetadataValueMaxLength < len(value) {
		t.Fatalf("the evidence of every kind does not fit a Stripe metadata value: %q", value)
	}
	for _, country := range append(every.Connections, "IN") {
		if strings.Contains(value, `"`+country) || strings.Contains(value, ":"+country) {
			t.Fatalf("what Stripe keeps names country %s: %s", country, value)
		}
	}
}

// A trial's card joins the signals its subscription was priced from: a
// regional card alone never moves a standard subscription to the regional
// tier, a standard card moves a regional one to standard, and a regional card
// moves it to regional only when the subscription's signals were regional,
// or it had none. The card's country replaces the billing country it was
// priced with. A subscription started before it kept its evidence is judged
// with the tier it was priced at.
func TestStripeTrialCardJoinsTheSubscriptionSignals(t *testing.T) {
	priceTierTestTiers(t)
	evidence := func(tier string, signals ...string) map[string]string {
		value := (&stripePriceTierEvidence{Tier: tier, Source: PriceTierSourceSignals, Signals: signals}).metadataValue()
		return map[string]string{stripeMetadataPriceTier: tier, stripeMetadataPriceTierEvidence: value}
	}
	for _, c := range []struct {
		name     string
		metadata map[string]string
		card     string
		tier     string
		signals  []string
	}{
		{
			name:     "standard signals, regional card",
			metadata: evidence(model.PriceTierStandard, "ip:standard", "signup:regional"),
			card:     "IN",
			tier:     model.PriceTierStandard,
			signals:  []string{"ip:standard", "signup:regional", "billing:regional"},
		},
		{
			name:     "regional signals, regional card",
			metadata: evidence(priceTierTestRegional, "ip:regional", "signup:regional"),
			card:     "BR",
			tier:     priceTierTestRegional,
			signals:  []string{"ip:regional", "signup:regional", "billing:regional"},
		},
		{
			name:     "regional signals, standard card",
			metadata: evidence(priceTierTestRegional, "ip:regional", "signup:regional"),
			card:     "US",
			tier:     model.PriceTierStandard,
			signals:  []string{"ip:regional", "signup:regional", "billing:standard"},
		},
		{
			name: "no signals, regional card",
			metadata: map[string]string{
				stripeMetadataPriceTier:         model.PriceTierStandard,
				stripeMetadataPriceTierEvidence: `{"tier":"standard","source":"default"}`,
			},
			card:    "IN",
			tier:    priceTierTestRegional,
			signals: []string{"billing:regional"},
		},
		{
			name:     "an earlier standard card is replaced",
			metadata: evidence(model.PriceTierStandard, "ip:regional", "billing:standard"),
			card:     "IN",
			tier:     priceTierTestRegional,
			signals:  []string{"ip:regional", "billing:regional"},
		},
		{
			name:     "started at standard before the evidence, regional card",
			metadata: map[string]string{stripeMetadataPriceTier: model.PriceTierStandard},
			card:     "IN",
			tier:     model.PriceTierStandard,
			signals:  []string{"checkout:standard", "billing:regional"},
		},
		{
			name:     "started at regional before the evidence, standard card",
			metadata: map[string]string{stripeMetadataPriceTier: priceTierTestRegional},
			card:     "GB",
			tier:     model.PriceTierStandard,
			signals:  []string{"checkout:regional", "billing:standard"},
		},
		{
			name: "unreadable evidence is the tier it was priced at",
			metadata: map[string]string{
				stripeMetadataPriceTier:         model.PriceTierStandard,
				stripeMetadataPriceTierEvidence: "{",
			},
			card:    "IN",
			tier:    model.PriceTierStandard,
			signals: []string{"checkout:standard", "billing:regional"},
		},
		{
			name:     "a card country that is no country",
			metadata: evidence(priceTierTestRegional, "ip:regional"),
			card:     "EU",
			tier:     priceTierTestRegional,
			signals:  []string{"ip:regional"},
		},
	} {
		tier, kept := stripeFinalizedPriceTier(c.metadata, c.card)
		if tier.Name != c.tier {
			t.Fatalf("%s: the card moved the subscription to %s, want %s", c.name, tier.Name, c.tier)
		}
		connect.AssertEqual(t, kept.Tier, c.tier)
		connect.AssertEqual(t, kept.Signals, c.signals)
		if value := kept.metadataValue(); strings.Contains(value, c.card) {
			t.Fatalf("%s: what Stripe keeps names the card's country: %s", c.name, value)
		}
	}
}
