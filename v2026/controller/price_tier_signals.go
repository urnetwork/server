package controller

// The price tier of a purchase the server prices itself: crypto (USDC
// through Solana Pay among it), Gamma and Stripe. These rails have no store
// between the buyer and the price that would set the tier from its own
// storefront, so the tier comes from the country signals the server holds
// about the caller, never from a country the client sends
// (storefront_country, the claimed country). A tier other than the default
// (the regional tier) is priced only when every signal resolves to it (the
// owner, 2026-11-21):
//
//   - ip: the purchase request's address;
//   - signup: the address the account signed up from (network_onboarding);
//   - connection: where the network's devices last connected from, as the
//     connect servers placed them (model.NetworkConnectionCountries);
//   - billing: the Stripe customer's billing country, from its card
//     (stripe_customer);
//   - store: where the App Store and Google Play sold the network's
//     subscriptions (subscription_renewal.country).
//
// A signal of another tier, or no signal at all, prices the default tier
// (standard). The stores keep pricing by their own storefront: the plan
// response and the welcome offer show a store app's storefront tier
// (ResolvePriceTier), and the store charges it.
//
// Each purchase keeps what priced it (PriceTierEvidence): a crypto invoice, a
// Gamma intent and an old Solana Pay intent on its row
// (price_tier_evidence), a Stripe subscription in its metadata without the
// countries, which stay on our servers. A Stripe trial's card adds its
// billing signal to the subscription's before the trial ends
// (stripeFinalizedPriceTier).

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Where a price tier signal's country came from.
const (
	// the purchase request's address
	PriceTierSignalIp = "ip"
	// the address the account signed up from (network_onboarding.country)
	PriceTierSignalSignup = "signup"
	// where one of the network's devices last connected from
	PriceTierSignalConnection = "connection"
	// the Stripe customer's billing country: the card's billing address,
	// else its issuing country
	PriceTierSignalBilling = "billing"
	// where the App Store or Google Play sold one of the network's
	// subscriptions
	PriceTierSignalStore = "store"
	// the tier a Stripe subscription started before it kept its evidence was
	// priced at, which its trial's card is judged with
	PriceTierSignalCheckout = "checkout"
)

// PriceTierSignal is one country signal a price tier was decided from.
type PriceTierSignal struct {
	// one of the PriceTierSignal* values
	Kind string `json:"kind"`
	// upper-case ISO 3166-1 alpha-2
	Country string `json:"country,omitempty"`
	// the tier the country resolved to when the purchase was priced
	Tier string `json:"tier"`
}

// PriceTierEvidence is what decided a purchase's price tier, kept with the
// purchase so its price can be audited.
type PriceTierEvidence struct {
	Tier string `json:"tier"`
	// signals when a signal had a country, default when none did
	Source string `json:"source"`
	// the country the client sent (storefront_country), "" for none: kept to
	// audit with, never priced from
	ClaimedCountry string `json:"claimed_country,omitempty"`
	// each signal once, in the order they were read
	Signals []*PriceTierSignal `json:"signals,omitempty"`
}

// The reads behind a purchase's price tier.
type priceTierSignalSettings struct {
	// The most of the network's devices whose connection countries are read,
	// newest first. 32: an account's own devices are far fewer, and a
	// provider fleet's newest 32 name where it runs. Non-positive reads none.
	MaxConnectionDevices int
}

// A new instance on every call.
func defaultPriceTierSignalSettings() *priceTierSignalSettings {
	return &priceTierSignalSettings{
		MaxConnectionDevices: 32,
	}
}

// The settings purchases are priced with. Replaceable only by tests;
// production never mutates it.
var priceTierSignalSettingsFunc = defaultPriceTierSignalSettings

// The country of a request's address, "" when unknown. Replaceable only by
// tests.
var priceTierClientCountryFunc = clientCountryCode

// The countries the server holds about a network, in one statement.
// Replaceable only by tests.
var priceTierNetworkCountriesFunc = func(ctx context.Context, networkId server.Id, settings *priceTierSignalSettings) *model.NetworkCountrySignals {
	return model.GetNetworkCountrySignals(ctx, networkId, settings.MaxConnectionDevices)
}

// ResolvePurchasePriceTier is the tier a purchase on a rail without a store
// is priced at, with its evidence. claimedCountry is the country the client
// sent: kept in the evidence, never priced from.
func ResolvePurchasePriceTier(clientSession *session.ClientSession, claimedCountry string) *ResolvedPriceTier {
	evidence := &PriceTierEvidence{
		ClaimedCountry: model.CountryCodeFromStorefront(claimedCountry),
	}
	evidence.addSignal(PriceTierSignalIp, priceTierClientCountryFunc(clientSession))
	if clientSession.ByJwt != nil {
		countries := priceTierNetworkCountriesFunc(clientSession.Ctx, clientSession.ByJwt.NetworkId, priceTierSignalSettingsFunc())
		evidence.addSignal(PriceTierSignalSignup, countries.Signup)
		for _, country := range countries.Connections {
			evidence.addSignal(PriceTierSignalConnection, country)
		}
		evidence.addSignal(PriceTierSignalBilling, countries.Billing)
		for _, country := range countries.Stores {
			evidence.addSignal(PriceTierSignalStore, country)
		}
	}
	tier, source := decidePriceTier(evidence.Signals)
	evidence.Tier = tier.Name
	evidence.Source = source
	return &ResolvedPriceTier{
		Tier:     tier,
		Source:   source,
		Evidence: evidence,
	}
}

// addSignal records the country as a signal of the kind, with its tier. A
// value that is not a country (model.IsCountryCode) is no signal, and a
// signal is kept once.
func (self *PriceTierEvidence) addSignal(kind string, country string) {
	code := model.CountryCodeFromStorefront(country)
	if !model.IsCountryCode(code) {
		return
	}
	for _, signal := range self.Signals {
		if signal.Kind == kind && signal.Country == code {
			return
		}
	}
	self.Signals = append(self.Signals, &PriceTierSignal{
		Kind:    kind,
		Country: code,
		Tier:    model.Pro().PriceTierForCountry(code).Name,
	})
}

// decidePriceTier is the tier the signals agree on: the one every signal
// resolved to, else the default tier, on a disagreement and with no signal
// alike. source is signals when there was a signal, default when there was
// none.
func decidePriceTier(signals []*PriceTierSignal) (tier *model.ProPriceTier, source string) {
	pro := model.Pro()
	if len(signals) == 0 {
		return pro.DefaultPriceTier(), PriceTierSourceDefault
	}
	for _, signal := range signals[1:] {
		if signal.Tier != signals[0].Tier {
			return pro.DefaultPriceTier(), PriceTierSourceSignals
		}
	}
	if tier := pro.PriceTierByName(signals[0].Tier); tier != nil {
		return tier, PriceTierSourceSignals
	}
	// a tier pro.yml no longer has
	return pro.DefaultPriceTier(), PriceTierSourceSignals
}

// rowJson is the evidence as a purchase's row keeps it
// (price_tier_evidence), "" for none.
func (self *PriceTierEvidence) rowJson() string {
	if self == nil {
		return ""
	}
	value, err := json.Marshal(self)
	if err != nil {
		return ""
	}
	return string(value)
}

// ----- what a Stripe subscription keeps -----

// The most characters Stripe keeps in one metadata value.
const stripeMetadataValueMaxLength = 500

// What a Stripe subscription keeps of its price tier's evidence: the tier,
// the source and each signal as kind:tier, once each. The claimed country
// and the signals' countries stay on our servers.
type stripePriceTierEvidence struct {
	Tier    string   `json:"tier"`
	Source  string   `json:"source"`
	Signals []string `json:"signals,omitempty"`
}

// stripe is what a Stripe subscription keeps of the evidence.
func (self *PriceTierEvidence) stripe() *stripePriceTierEvidence {
	if self == nil {
		return nil
	}
	kept := &stripePriceTierEvidence{
		Tier:   self.Tier,
		Source: self.Source,
	}
	for _, signal := range self.Signals {
		value := signal.Kind + ":" + signal.Tier
		if !slices.Contains(kept.Signals, value) {
			kept.Signals = append(kept.Signals, value)
		}
	}
	return kept
}

// metadataValue is the evidence as the subscription's metadata value, "" for
// none or for one too long for Stripe to keep: the subscription is then
// started without it, and its card is judged with its tier alone.
func (self *stripePriceTierEvidence) metadataValue() string {
	if self == nil {
		return ""
	}
	value, err := json.Marshal(self)
	if err != nil || stripeMetadataValueMaxLength < len(value) {
		return ""
	}
	return string(value)
}

// stripeFinalizedPriceTier is the tier a Stripe subscription in its trial
// moves to once its card's billing country is known: the card's billing
// signal replaces the one the subscription was priced with and joins its
// other signals, which must agree as at the purchase. A subscription started
// before it kept its evidence has the tier it was priced at as its one signal
// (checkout). The evidence to keep with a move comes back with it.
func stripeFinalizedPriceTier(metadata map[string]string, billingCountry string) (*model.ProPriceTier, *stripePriceTierEvidence) {
	evidence := &PriceTierEvidence{}
	var kept stripePriceTierEvidence
	if value := metadata[stripeMetadataPriceTierEvidence]; value != "" && json.Unmarshal([]byte(value), &kept) == nil {
		for _, signal := range kept.Signals {
			kind, tierName, ok := strings.Cut(signal, ":")
			if !ok || kind == PriceTierSignalBilling || tierName == "" {
				continue
			}
			evidence.Signals = append(evidence.Signals, &PriceTierSignal{Kind: kind, Tier: tierName})
		}
	} else if tierName := metadata[stripeMetadataPriceTier]; tierName != "" {
		evidence.Signals = append(evidence.Signals, &PriceTierSignal{Kind: PriceTierSignalCheckout, Tier: tierName})
	}
	evidence.addSignal(PriceTierSignalBilling, billingCountry)
	tier, source := decidePriceTier(evidence.Signals)
	evidence.Tier = tier.Name
	evidence.Source = source
	return tier, evidence.stripe()
}
