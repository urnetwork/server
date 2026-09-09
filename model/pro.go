package model

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Pro is the single source of truth for plan limits/features, loaded once from
// config/<env>/pro.yml (shared spec lives in config/all/pro.yml). All services
// read limits/features/grants through Pro() rather than hardcoding them.

// ----- raw yaml shapes -----

type proFeaturesYaml struct {
	HttpProxy      bool `yaml:"http_proxy"`
	HttpsProxy     bool `yaml:"https_proxy"`
	SocksProxy     bool `yaml:"socks_proxy"`
	WireguardProxy bool `yaml:"wireguard_proxy"`
}

type proPriceYaml struct {
	Monthly float64 `yaml:"monthly"`
	Yearly  float64 `yaml:"yearly"`
}

type proPriceTierYaml struct {
	Name       string   `yaml:"name"`
	YearlyUsd  float64  `yaml:"yearly_usd"`
	MonthlyUsd float64  `yaml:"monthly_usd"`
	Countries  []string `yaml:"countries"`
}

type proTierYaml struct {
	ConcurrentClients int                `yaml:"concurrent_clients"`
	Data              string             `yaml:"data"`
	DataPeriod        string             `yaml:"data_period"`
	PriceUsd          proPriceYaml       `yaml:"price_usd"`
	PriceTiers        []proPriceTierYaml `yaml:"price_tiers"`
	Features          proFeaturesYaml    `yaml:"features"`
}

type proReferralYaml struct {
	BonusPerReferral string `yaml:"bonus_per_referral"`
	ReferredBonus    string `yaml:"referred_bonus"`
	Period           string `yaml:"period"`
	MaxReferrals     int    `yaml:"max_referrals"`
}

type proSeekerYaml struct {
	DataMultiplier float64 `yaml:"data_multiplier"`
}

type proDataCodeSkuYaml struct {
	Data     string  `yaml:"data"`
	PriceUsd float64 `yaml:"price_usd"`
}

type proDataCodeYaml struct {
	Duration string               `yaml:"duration"`
	Skus     []proDataCodeSkuYaml `yaml:"skus"`
}

type proConfigYaml struct {
	EnforceConcurrentClients bool            `yaml:"enforce_concurrent_clients"`
	EnforceFeatures          bool            `yaml:"enforce_features"`
	Free                     proTierYaml     `yaml:"free"`
	Pro                      proTierYaml     `yaml:"pro"`
	Referral                 proReferralYaml `yaml:"referral"`
	Seeker                   proSeekerYaml   `yaml:"seeker"`
	DataCode                 proDataCodeYaml `yaml:"data_code"`
}

// ----- parsed forms -----

type ProTier struct {
	ConcurrentClients int
	Data              ByteCount
	DataPeriod        time.Duration
	// The subscription price in USD. The server quotes from this so a client can never
	// name its own price.
	PriceMonthlyUsd float64
	PriceYearlyUsd  float64
	HttpProxy       bool
	HttpsProxy      bool
	SocksProxy      bool
	WireguardProxy  bool
}

type ProDataCodeSku struct {
	Data     ByteCount
	PriceUsd float64
}

// ProPriceTier is one regional price tier of the Pro subscription (pro.yml
// pro.price_tiers). The prices are in USD; the stores and Stripe derive the local
// currency figure themselves. Countries are upper-case ISO 3166-1 alpha-2; a tier
// with no countries is the catch-all for every country no other tier names.
type ProPriceTier struct {
	Name       string
	YearlyUsd  float64
	MonthlyUsd float64
	Countries  []string
	countries  map[string]bool
}

// Plan names shared by every purchase path.
const (
	PlanYearly  = "yearly"
	PlanMonthly = "monthly"
)

// PriceTierCurrency is the currency every tier price is quoted in.
const PriceTierCurrency = "USD"

// PriceUsd is the tier's price for a plan (PlanYearly or PlanMonthly); 0 for an
// unknown plan, which no purchase path will sell.
func (t *ProPriceTier) PriceUsd(plan string) float64 {
	switch plan {
	case PlanYearly:
		return t.YearlyUsd
	case PlanMonthly:
		return t.MonthlyUsd
	default:
		return 0
	}
}

// CatchAll reports whether the tier applies to every country not named by
// another tier.
func (t *ProPriceTier) CatchAll() bool {
	return len(t.Countries) == 0
}

func (t *ProPriceTier) hasCountry(countryCode string) bool {
	return t.countries[countryCode]
}

// ProConfig is the parsed product spec.
//
// ABSENT pro.yml -> every field here is its ZERO value, and that is a defined, safe
// state rather than an accident. Each accessor below treats an unusable value as "this
// is not configured" and degrades accordingly:
//
//	no enforcement flags -> concurrent limits DISABLED, every feature ALLOWED
//	no data amounts      -> the grants NO-OP (they never write a zero-byte balance)
//	no referral cap      -> referrals UNCAPPED (never capped at zero, which would
//	                        make `count >= cap` true for everyone and block them all)
//	no price / duration  -> purchases REFUSED, loudly, rather than sold for nothing
//
// The checks key off the VALUE, never off a "was it loaded" flag. A flag has to be
// remembered -- and a ProConfig literal that forgot to set it would be silently inert.
// Keying off the value also covers a pro.yml that is present but MIS-SPECIFIED
// (`data: 0`, `duration: 0`), which a flag never would.
type ProConfig struct {
	// EnforceConcurrentClients gates the connected top-level client limit, and
	// EnforceFeatures gates the per-tier proxy features (SOCKS/WireGuard are
	// Pro-only). The checks always run, but while these are false nothing is
	// rejected — enforcement ships dark and is turned on later, once users have
	// product awareness of the limits. Callers MUST consult these before
	// rejecting; use ConcurrentClientsExceeded and FeatureAllowed, which fold
	// them in.
	EnforceConcurrentClients bool
	EnforceFeatures          bool

	Free ProTier
	Pro  ProTier

	// priceTiers are the regional Pro price tiers in pro.yml order (the first is
	// the default). Read through PriceTiers/PriceTierForCountry, which synthesize a
	// single "standard" tier from Pro.PriceYearlyUsd/PriceMonthlyUsd when pro.yml
	// predates tiers (and a zero-price tier when pro.yml is absent, which no
	// purchase path will sell).
	priceTiers []*ProPriceTier

	ReferralBonus  ByteCount
	ReferredBonus  ByteCount
	ReferralPeriod time.Duration
	MaxReferrals   int

	// seekerDataMultiplier scales a Seeker/Saga holder's free + referral data grants;
	// read it via SeekerDataMultiplier(), which defaults a missing/zero value to 1.
	seekerDataMultiplier float64

	DataCodeDuration time.Duration
	DataCodeSkus     []ProDataCodeSku
}

func mustParseByteCount(s string) ByteCount {
	b, err := ParseByteCount(s)
	if err != nil {
		panic(fmt.Errorf("pro.yml: invalid byte count %q: %w", s, err))
	}
	return b
}

// parseByteCountOrZero is mustParseByteCount for an OPTIONAL key: an empty/unset value
// is zero ("not configured"), not a panic, so a pro.yml that predates the key still
// boots and the corresponding grant simply no-ops (see the ProConfig doc).
func parseByteCountOrZero(s string) ByteCount {
	if s == "" {
		return 0
	}
	return mustParseByteCount(s)
}

func mustParseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(fmt.Errorf("pro.yml: invalid duration %q: %w", s, err))
	}
	return d
}

func parseProTier(y proTierYaml) ProTier {
	return ProTier{
		ConcurrentClients: y.ConcurrentClients,
		Data:              mustParseByteCount(y.Data),
		DataPeriod:        mustParseDuration(y.DataPeriod),
		PriceMonthlyUsd:   y.PriceUsd.Monthly,
		PriceYearlyUsd:    y.PriceUsd.Yearly,
		HttpProxy:         y.Features.HttpProxy,
		HttpsProxy:        y.Features.HttpsProxy,
		SocksProxy:        y.Features.SocksProxy,
		WireguardProxy:    y.Features.WireguardProxy,
	}
}

// NormalizeCountryCode canonicalizes an ISO 3166-1 alpha-2 country code for tier
// lookup: trimmed and upper-cased, and "" for anything that is not two ASCII
// letters (the ip database, Apple storefronts and Stripe billing addresses all
// disagree on case; a three-letter or garbage value must not silently match).
func NormalizeCountryCode(countryCode string) string {
	code := strings.ToUpper(strings.TrimSpace(countryCode))
	if len(code) != 2 {
		return ""
	}
	for _, c := range code {
		if c < 'A' || 'Z' < c {
			return ""
		}
	}
	return code
}

func parseProPriceTiers(ys []proPriceTierYaml) []*ProPriceTier {
	tiers := make([]*ProPriceTier, 0, len(ys))
	seen := map[string]bool{}
	for _, y := range ys {
		name := strings.TrimSpace(y.Name)
		if name == "" {
			panic(fmt.Errorf("pro.yml: price tier without a name"))
		}
		if seen[name] {
			panic(fmt.Errorf("pro.yml: duplicate price tier %q", name))
		}
		seen[name] = true
		tier := &ProPriceTier{
			Name:       name,
			YearlyUsd:  y.YearlyUsd,
			MonthlyUsd: y.MonthlyUsd,
			countries:  map[string]bool{},
		}
		for _, country := range y.Countries {
			code := NormalizeCountryCode(country)
			if code == "" {
				panic(fmt.Errorf("pro.yml: price tier %q has an invalid country code %q", name, country))
			}
			if !tier.countries[code] {
				tier.countries[code] = true
				tier.Countries = append(tier.Countries, code)
			}
		}
		tiers = append(tiers, tier)
	}
	return tiers
}

var proConfig = sync.OnceValue(func() *ProConfig {
	// OPTIONAL. An environment without pro.yml must not fail to boot — a partial deploy
	// or an env that has not been updated should degrade to "nothing is enforced", not
	// take the process down.
	resource, err := server.Config.SimpleResource("pro.yml")
	if err != nil {
		glog.Errorf(
			"[pro]pro.yml is NOT PRESENT in this environment. Concurrent-client limits and "+
				"feature gating are DISABLED, and the data/referral grant tasks will not run. "+
				"Nothing is enforced and nothing is granted. err = %s\n",
			err,
		)
		// every field zero -- see the ProConfig doc: that is the inert state
		return &ProConfig{}
	}

	var y proConfigYaml
	resource.UnmarshalYaml(&y)

	priceTiers := parseProPriceTiers(y.Pro.PriceTiers)
	if 0 < len(priceTiers) {
		// the default tier IS pro.price_usd (see PriceTiers); a pro.yml where the
		// two disagree would quote one price and charge another
		if priceTiers[0].YearlyUsd != y.Pro.PriceUsd.Yearly || priceTiers[0].MonthlyUsd != y.Pro.PriceUsd.Monthly {
			panic(fmt.Errorf(
				"pro.yml: price_tiers[0] (%s: %g/%g) must equal pro.price_usd (%g/%g)",
				priceTiers[0].Name, priceTiers[0].YearlyUsd, priceTiers[0].MonthlyUsd,
				y.Pro.PriceUsd.Yearly, y.Pro.PriceUsd.Monthly,
			))
		}
	}

	skus := make([]ProDataCodeSku, 0, len(y.DataCode.Skus))
	for _, s := range y.DataCode.Skus {
		skus = append(skus, ProDataCodeSku{Data: mustParseByteCount(s.Data), PriceUsd: s.PriceUsd})
	}

	return &ProConfig{
		EnforceConcurrentClients: y.EnforceConcurrentClients,
		EnforceFeatures:          y.EnforceFeatures,
		Free:                     parseProTier(y.Free),
		Pro:                      parseProTier(y.Pro),
		priceTiers:               priceTiers,
		ReferralBonus:            mustParseByteCount(y.Referral.BonusPerReferral),
		ReferredBonus:            parseByteCountOrZero(y.Referral.ReferredBonus),
		ReferralPeriod:           mustParseDuration(y.Referral.Period),
		MaxReferrals:             y.Referral.MaxReferrals,
		seekerDataMultiplier:     y.Seeker.DataMultiplier,
		DataCodeDuration:         mustParseDuration(y.DataCode.Duration),
		DataCodeSkus:             skus,
	}
})

// Pro returns the parsed product spec (cached; parsed once from pro.yml).
func Pro() *ProConfig {
	return proConfig()
}

// Testing_SetEnforceConcurrentClients overrides the concurrent-client
// enforcement flag on the process's parsed config, returning a function that
// restores the previous value. Pro() caches one config for the process, so this
// mutates that shared value -- callers must restore it (defer the return) and
// must not run in parallel with code that reads the flag. For tests that need to
// exercise the (dark-by-default) concurrent-client / top-level client limits.
func Testing_SetEnforceConcurrentClients(enforce bool) func() {
	c := Pro()
	prev := c.EnforceConcurrentClients
	c.EnforceConcurrentClients = enforce
	return func() { c.EnforceConcurrentClients = prev }
}

// Testing_SetConcurrentClientsLimit overrides the per-tier connected-top-level
// client limits on the process's parsed config, returning a restore function.
// Same caveats as Testing_SetEnforceConcurrentClients (mutates the shared
// config; defer the return). For tests exercising the plan concurrent-client
// limit and its public-provider exemption.
func Testing_SetConcurrentClientsLimit(free int, pro int) func() {
	c := Pro()
	prevFree := c.Free.ConcurrentClients
	prevPro := c.Pro.ConcurrentClients
	c.Free.ConcurrentClients = free
	c.Pro.ConcurrentClients = pro
	return func() {
		c.Free.ConcurrentClients = prevFree
		c.Pro.ConcurrentClients = prevPro
	}
}

// Tier returns the free or pro tier limits/features.
func (c *ProConfig) Tier(pro bool) *ProTier {
	if pro {
		return &c.Pro
	}
	return &c.Free
}

// MaxConcurrentClients is the max connected top-level clients for the tier.
func (c *ProConfig) MaxConcurrentClients(pro bool) int {
	return c.Tier(pro).ConcurrentClients
}

// ConcurrentClientsExceeded reports whether a network that already has
// `connectedCount` connected top-level clients has no room for one more.
//
// This is the ONLY thing callers should use to decide whether to reject a
// connection or a client creation: it folds in the EnforceConcurrentClients
// rollout switch, so while enforcement is dark this always returns false and
// nothing is ever refused. Checking MaxConcurrentClients directly would bypass
// the switch. A limit <= 0 means unlimited.
func (c *ProConfig) ConcurrentClientsExceeded(pro bool, connectedCount int) bool {
	// No pro.yml -> this is false (the zero value) -> nothing is ever refused.
	if !c.EnforceConcurrentClients {
		return false
	}
	max := c.MaxConcurrentClients(pro)
	if max <= 0 {
		return false
	}
	return max <= connectedCount
}

// DataAmount / DataPeriod are the recurring data grant for the tier.
func (c *ProConfig) DataAmount(pro bool) ByteCount {
	return c.Tier(pro).Data
}

func (c *ProConfig) DataPeriod(pro bool) time.Duration {
	return c.Tier(pro).DataPeriod
}

// PriceMonthlyUsd / PriceYearlyUsd are the UR Pro subscription prices (pro.yml
// pro.price_usd). The server quotes from these, so a client can never name its own price.
func (c *ProConfig) PriceMonthlyUsd() float64 {
	return c.Pro.PriceMonthlyUsd
}

func (c *ProConfig) PriceYearlyUsd() float64 {
	return c.Pro.PriceYearlyUsd
}

// PriceTiers returns the regional Pro price tiers in pro.yml order; the first is
// the default. A pro.yml without price_tiers yields one "standard" tier priced at
// pro.price_usd, so every caller can assume at least one tier. With no pro.yml at
// all that tier is priced at zero -- and a zero price is never sellable (see the
// ProConfig doc).
//
// The DEFAULT tier's prices are pro.price_usd itself (pro.yml requires the two to
// agree), read live from Pro.PriceYearlyUsd/PriceMonthlyUsd -- so the legacy
// accessors, the tier and the Testing_ overrides on those fields never disagree.
func (c *ProConfig) PriceTiers() []*ProPriceTier {
	if len(c.priceTiers) == 0 {
		return []*ProPriceTier{{
			Name:       PriceTierStandard,
			YearlyUsd:  c.Pro.PriceYearlyUsd,
			MonthlyUsd: c.Pro.PriceMonthlyUsd,
			countries:  map[string]bool{},
		}}
	}
	tiers := make([]*ProPriceTier, len(c.priceTiers))
	copy(tiers, c.priceTiers)
	first := *c.priceTiers[0]
	first.YearlyUsd = c.Pro.PriceYearlyUsd
	first.MonthlyUsd = c.Pro.PriceMonthlyUsd
	tiers[0] = &first
	return tiers
}

// PriceTierStandard is the name of the developed-country tier, which is also the
// tier used when the country is unknown: an unresolvable country must never
// quote the cheaper price.
const PriceTierStandard = "standard"

// DefaultPriceTier is the tier for an unknown country: the first configured tier.
func (c *ProConfig) DefaultPriceTier() *ProPriceTier {
	return c.PriceTiers()[0]
}

// PriceTierForCountry resolves the tier for an ISO 3166-1 alpha-2 country code.
// A tier that names the country wins; otherwise the catch-all tier (the one with
// no countries); otherwise the default tier. An empty or malformed code is
// "unknown" and gets the default tier, never the catch-all.
func (c *ProConfig) PriceTierForCountry(countryCode string) *ProPriceTier {
	code := NormalizeCountryCode(countryCode)
	tiers := c.PriceTiers()
	if code == "" {
		return tiers[0]
	}
	for _, tier := range tiers {
		if tier.hasCountry(code) {
			return tier
		}
	}
	for _, tier := range tiers {
		if tier.CatchAll() {
			return tier
		}
	}
	return tiers[0]
}

// PriceTierByName returns the named tier, or nil.
func (c *ProConfig) PriceTierByName(name string) *ProPriceTier {
	for _, tier := range c.PriceTiers() {
		if tier.Name == name {
			return tier
		}
	}
	return nil
}

// referralPeriodFallback is how often the referral task wakes when pro.yml gives no
// period. It only decides how often the task wakes up to do NOTHING (the grant noops
// when pro.yml is absent) -- it must simply never be zero. See ReferralGrantPeriod.
const referralPeriodFallback = 24 * time.Hour

// ReferralGrantPeriod is how often the referral bonus is re-granted.
//
// This NEVER returns <= 0, and that is the whole point of it existing. The referral task
// reschedules itself at now+period, so a zero period is not "run with no delay" -- it is
// a HOT LOOP: the task fires, schedules itself for now, fires again, forever, hammering
// the task worker and the database. A missing config file must not spin the server.
func (c *ProConfig) ReferralGrantPeriod() time.Duration {
	if c.ReferralPeriod <= 0 {
		return referralPeriodFallback
	}
	return c.ReferralPeriod
}

// ReferralsCapped reports whether a referrer that already has `referralCount` referrals
// has reached the cap and may take no more.
//
// This is the ONLY way to ask. Reading MaxReferrals directly is a trap: with no pro.yml
// it is 0, and `count >= 0` is always true — so every referral would be silently refused
// and the feature would simply stop working, with nothing to say why. No spec means NO
// CAP, not a cap of nothing.
func (c *ProConfig) ReferralsCapped(referralCount int) bool {
	if c.MaxReferrals <= 0 {
		return false
	}
	return c.MaxReferrals <= referralCount
}

// SeekerDataMultiplier scales a Seeker/Saga holder's free + referral data grants (pro.yml
// seeker.data_multiplier). It NEVER returns <= 0: an unset or non-positive value -- including
// the absent-pro.yml ProConfig{} zero value -- defaults to 1 (no boost), so a missing key can
// never zero out a seeker's grant. Callers multiply a grant's byte count by this.
func (c *ProConfig) SeekerDataMultiplier() float64 {
	if c.seekerDataMultiplier <= 0 {
		return 1.0
	}
	return c.seekerDataMultiplier
}

// Feature names for FeatureEnabled.
const (
	FeatureHttpProxy      = "http_proxy"
	FeatureHttpsProxy     = "https_proxy"
	FeatureSocksProxy     = "socks_proxy"
	FeatureWireguardProxy = "wireguard_proxy"
)

// FeatureEnabled reports whether a proxy feature is included in the tier's plan.
// This is the PLAN DEFINITION — what the site advertises and what an account/plan
// API should report. To decide whether to actually refuse a request, use
// FeatureAllowed, which also honors the rollout switch.
func (c *ProConfig) FeatureEnabled(pro bool, feature string) bool {
	t := c.Tier(pro)
	switch feature {
	case FeatureHttpProxy:
		return t.HttpProxy
	case FeatureHttpsProxy:
		return t.HttpsProxy
	case FeatureSocksProxy:
		return t.SocksProxy
	case FeatureWireguardProxy:
		return t.WireguardProxy
	default:
		return false
	}
}

// FeatureAllowed reports whether a tier may actually USE a proxy feature right
// now. This is the ONLY thing a service should gate on: it folds in the
// EnforceFeatures rollout switch, so while feature enforcement is dark this
// always returns true and no request is refused (a free-tier client can still
// use SOCKS/WireGuard). Gating on FeatureEnabled directly would bypass the
// switch and cut those users off immediately.
func (c *ProConfig) FeatureAllowed(pro bool, feature string) bool {
	// No pro.yml -> this is false (the zero value) -> every feature is allowed.
	if !c.EnforceFeatures {
		return true
	}
	return c.FeatureEnabled(pro, feature)
}

// NetworkFeatureAllowed is FeatureAllowed for a network, resolving its Pro
// entitlement. Services that only have a networkId (proxy, connect) use this.
func NetworkFeatureAllowed(ctx context.Context, networkId server.Id, feature string) bool {
	if !Pro().EnforceFeatures {
		// dark: skip the Pro lookup entirely
		return true
	}
	return Pro().FeatureAllowed(IsPro(ctx, &networkId), feature)
}
