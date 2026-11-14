package model

// Reengagement is the parsed re-engagement program config
// (config/<env>/reengagement.yml; mmm/reengagement/PLAN.md). Like
// onboarding.yml it is data, shipped by config injection, and OPTIONAL: without
// the file the program is inert (nothing is scanned, nothing is sent).
//
//	reengagement:
//	  enabled: false             # master switch of every outbound side effect
//	  brevo:
//	    templates: {...}         # machine-written by mmm/reengagement/build.mjs
//	    reply_to: support@ur.io
//	    company_line: ""
//	  schedule: { dormant_days: 30, r2_after_r1_days: 7, ... }
//	  streams: { lapsed: true, never_activated: true, fix: true, winback: true }
//	  scan: { batch_limit: 2000, catch_up_days: 3 }
//	  pq_ready: false
//	  highlights:
//	    - id: outages
//	      released: { "*": 2026-10-27 }
//	      last_connect_from: 2026-10-12
//	      last_connect_to: 2026-10-27
//	  experiments: [...]         # the onboarding registry's shape; surfaces reengagement.<stream>
//	  results:
//	    rollup_days: 120         # cohort days the nightly results rollup recomputes
//	  service_reminders:         # account-mailer notices; service mail, not gated by `enabled`
//	    pro_ending: false        # crypto Pro stops in 14 and 2 days
//	    trial_ending: false      # an App Store or Play trial ends in 2 days
//	    trial_ending_stores: [apple, play]
//	  winback_offer:             # W1's comeback offer; inert unless enabled
//	    enabled: false
//	    percent_off: 50
//	    plan: monthly
//	    validity_days: 14
//	    stripe_coupon_id: winback_monthly_50
//	    play_offer_tag: winback-monthly-50
//	    apple_offer_id: ""       # the App Store win-back offer's identifier
//	    apple_offer_url: ""      # its direct link, https://apps.apple.com/win-back/<Apple ID>
//
// The mail policy (who may be mailed, by country and consent evidence) is
// onboarding.yml `policy`, shared by both programs.

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/reengagement"
)

type reengagementYaml struct {
	Reengagement ReengagementConfig `yaml:"reengagement"`
}

type ReengagementScheduleConfig struct {
	DormantDays      int    `yaml:"dormant_days"`
	R2AfterR1Days    int    `yaml:"r2_after_r1_days"`
	R3Day            int    `yaml:"r3_day"`
	EndAfterR3Days   int    `yaml:"end_after_r3_days"`
	NeverActiveDay   int    `yaml:"never_active_day"`
	NKeepAfterN1Days int    `yaml:"n_keep_after_n1_days"`
	CooldownDays     int    `yaml:"cooldown_days"`
	QuietDays        int    `yaml:"quiet_days"`
	FixWindowDays    int    `yaml:"fix_window_days"`
	WinbackDays      int    `yaml:"winback_days"`
	StoreRetryDays   int    `yaml:"store_retry_days"`
	KeepDays         int    `yaml:"keep_days"`
	MinGapDays       int    `yaml:"min_gap_days"`
	MaxPerYear       int    `yaml:"max_per_year"`
	SendHour         int    `yaml:"send_hour"`
	SendWindow       string `yaml:"send_window"`
}

type ReengagementStreamsConfig struct {
	Lapsed         bool `yaml:"lapsed"`
	NeverActivated bool `yaml:"never_activated"`
	Fix            bool `yaml:"fix"`
	Winback        bool `yaml:"winback"`
}

type ReengagementScanConfig struct {
	// the most episodes one scan opens per stream (the rest wait for the next day)
	BatchLimit int `yaml:"batch_limit"`
	// how many days a missed scan catches up
	CatchUpDays int `yaml:"catch_up_days"`
}

// ReengagementResultsConfig tunes the nightly results rollup
// (controller/reengagement_results_controller.go). The admin endpoint's
// volume floor is onboarding.yml `results.min_exposures`, shared with
// GET /admin/onboarding/results.
type ReengagementResultsConfig struct {
	// how many cohort days back the nightly rollup recomputes, so the
	// windows (up to 90 days after the first step) mature in place
	RollupDays int `yaml:"rollup_days"`
}

// ReengagementResultsRollupDaysDefault covers retention_90d's window with a
// month to spare.
const ReengagementResultsRollupDaysDefault = 120

type ReengagementHighlightConfig struct {
	Id        string            `yaml:"id"`
	Platforms []string          `yaml:"platforms"`
	Countries []string          `yaml:"countries"`
	Released  map[string]string `yaml:"released"`
	// only networks whose last connection fell in [from, to)
	LastConnectFrom string   `yaml:"last_connect_from"`
	LastConnectTo   string   `yaml:"last_connect_to"`
	Fixes           []string `yaml:"fixes"`
}

// ReengagementServiceRemindersConfig turns on the account-mailer notices
// (controller/service_reminders_controller.go). They are service mail, not
// marketing: `enabled` above does not gate them, these do.
type ReengagementServiceRemindersConfig struct {
	// crypto Pro stops in 14 and 2 days
	ProEnding bool `yaml:"pro_ending"`
	// a gift year ends in 14 days, with nothing else to carry Pro on
	// (GIFTOFPRIVACY.md §9.2): unlike the others it offers to keep Pro, so it
	// goes only where the mail policy lets an offer go (consent, country,
	// product updates on)
	GiftEnding bool `yaml:"gift_ending"`
	// a free trial ends soon: an App Store or Play trial in 2 days (the daily
	// task), a Stripe trial in 3 (Stripe's customer.subscription.trial_will_end)
	TrialEnding bool `yaml:"trial_ending"`
	// the stores whose trials get the notice; default apple and play. With
	// stripe the Stripe trials get it too, from trial_will_end (Stripe's own
	// dashboard reminder, if it is on, comes besides)
	TrialEndingStores []string `yaml:"trial_ending_stores"`
}

// EffectiveTrialEndingStores are the configured stores, or apple and play.
func (c ReengagementServiceRemindersConfig) EffectiveTrialEndingStores() []string {
	if len(c.TrialEndingStores) == 0 {
		return []string{OnboardingStoreApple, OnboardingStorePlay}
	}
	return c.TrialEndingStores
}

// ReengagementWinbackOfferConfig is the comeback offer W1 issues
// (mmm/reengagement/PLAN.md "THE STORES", decision 5): the monthly plan's
// first month at percent_off on Google Play and Stripe for the W experiment's
// offer arm, and Apple's own win-back offer on the monthly product for the
// networks Apple lists; nothing on yearly, nothing for crypto. Inert unless
// enabled, and refused whole when its shape breaks the rule over every offer
// (reengagement.WinbackShapeRefusal).
type ReengagementWinbackOfferConfig struct {
	Enabled    bool   `yaml:"enabled"`
	PercentOff int    `yaml:"percent_off"`
	Plan       string `yaml:"plan"`
	// how long an issued offer stays redeemable: its real deadline
	ValidityDays int `yaml:"validity_days"`
	// the Stripe coupon (percent_off, duration once), created or fetched by
	// the server; each offer is a per-customer promotion code on it
	StripeCouponId string `yaml:"stripe_coupon_id"`
	// the tag of the developer-determined offer on the Play monthly base plan
	PlayOfferTag string `yaml:"play_offer_tag"`
	// the App Store Connect win-back offer's identifier, as the App Store
	// Server API lists it in eligibleWinBackOfferIds; empty = no App Store offer
	AppleOfferId string `yaml:"apple_offer_id"`
	// the offer's direct link as App Store Connect shows it
	// (https://apps.apple.com/win-back/<the offer's Apple ID>); the W1 landing
	// opens it for the networks Apple lists. Empty = the plan screen.
	AppleOfferUrl string `yaml:"apple_offer_url"`
}

// AppleWinbackOfferUrlPrefix is the form of an App Store win-back offer's
// direct link (StoreKit "Supporting win-back offers in your app").
const AppleWinbackOfferUrlPrefix = "https://apps.apple.com/win-back/"

type ReengagementConfig struct {
	Enabled  bool                       `yaml:"enabled"`
	Brevo    OnboardingBrevoConfig      `yaml:"brevo"`
	Schedule ReengagementScheduleConfig `yaml:"schedule"`
	Streams  ReengagementStreamsConfig  `yaml:"streams"`
	Scan     ReengagementScanConfig     `yaml:"scan"`
	// the post-quantum discovery fix is live: the trust email may point at the switch
	PqReady     bool                           `yaml:"pq_ready"`
	Highlights  []*ReengagementHighlightConfig `yaml:"highlights"`
	Experiments []*OnboardingExperiment        `yaml:"experiments"`
	// the nightly results rollup
	Results ReengagementResultsConfig `yaml:"results"`
	// the service notices, each off unless turned on
	ServiceReminders ReengagementServiceRemindersConfig `yaml:"service_reminders"`
	// W1's comeback offer, off unless turned on
	WinbackOffer ReengagementWinbackOfferConfig `yaml:"winback_offer"`

	highlights []*reengagement.Highlight
}

// Experiment surfaces, one per stream.
const (
	ExperimentSurfaceReengagementLapsed         = "reengagement.lapsed"
	ExperimentSurfaceReengagementNeverActivated = "reengagement.never_activated"
	ExperimentSurfaceReengagementFix            = "reengagement.fix"
	ExperimentSurfaceReengagementWinback        = "reengagement.winback"
)

// ExperimentSurfaceForStream is a stream's experiment surface.
func ExperimentSurfaceForStream(stream string) string {
	return "reengagement." + stream
}

var reengagementConfig = sync.OnceValue(func() *ReengagementConfig {
	resource, err := server.Config.SimpleResource("reengagement.yml")
	if err != nil {
		glog.Infof("[reengagement]reengagement.yml is not present; the re-engagement program is inert. err = %s\n", err)
		return &ReengagementConfig{}
	}
	var y reengagementYaml
	if err := resource.UnmarshalYamlE(&y); err != nil {
		glog.Errorf("[reengagement]reengagement.yml could not be parsed; the re-engagement program is inert. err = %s\n", err)
		return &ReengagementConfig{}
	}
	return newReengagementConfig(&y.Reengagement)
})

// Reengagement returns the parsed config (cached; parsed once per process).
func Reengagement() *ReengagementConfig {
	if c := reengagementConfigOverride.Load(); c != nil {
		return c
	}
	return reengagementConfig()
}

var reengagementConfigOverride atomic.Pointer[ReengagementConfig]

// Testing_SetReengagementConfig makes Reengagement() answer c, built the way
// the loader builds it, until the returned function restores the previous
// answer. For tests of the program's controllers, which no reengagement.yml
// reaches.
func Testing_SetReengagementConfig(c *ReengagementConfig) func() {
	previous := reengagementConfigOverride.Swap(newReengagementConfig(c))
	return func() {
		reengagementConfigOverride.Store(previous)
	}
}

func newReengagementConfig(c *ReengagementConfig) *ReengagementConfig {
	c.Experiments = validExperiments(c.Experiments)
	c.highlights = parseReengagementHighlights(c.Highlights)
	return c
}

// NewReengagementConfigForTest builds a config the way the loader does.
func NewReengagementConfigForTest(c *ReengagementConfig) *ReengagementConfig {
	return newReengagementConfig(c)
}

func parseReengagementHighlights(configs []*ReengagementHighlightConfig) []*reengagement.Highlight {
	out := []*reengagement.Highlight{}
	for _, c := range configs {
		if c == nil || strings.TrimSpace(c.Id) == "" {
			continue
		}
		h := &reengagement.Highlight{
			Id:        strings.TrimSpace(c.Id),
			Platforms: c.Platforms,
			Countries: c.Countries,
			Released:  map[string]time.Time{},
			Fixes:     c.Fixes,
		}
		ok := true
		for platform, day := range c.Released {
			t, err := parseExperimentDay(day)
			if err != nil || t == nil {
				glog.Errorf("[reengagement]highlight %q released.%s: %q is not a date; the highlight is skipped\n", c.Id, platform, day)
				ok = false
				break
			}
			h.Released[strings.ToLower(strings.TrimSpace(platform))] = *t
		}
		for _, bound := range []struct {
			value string
			into  **time.Time
		}{{c.LastConnectFrom, &h.LastConnectFrom}, {c.LastConnectTo, &h.LastConnectTo}} {
			if strings.TrimSpace(bound.value) == "" {
				continue
			}
			t, err := parseExperimentDay(bound.value)
			if err != nil {
				glog.Errorf("[reengagement]highlight %q: %q is not a date; the highlight is skipped\n", c.Id, bound.value)
				ok = false
				break
			}
			*bound.into = t
		}
		if ok {
			out = append(out, h)
		}
	}
	return out
}

// ParsedHighlights are the "what changed" items with their dates parsed.
func (c *ReengagementConfig) ParsedHighlights() []*reengagement.Highlight {
	return c.highlights
}

// ScheduleValue is the schedule with defaults filled in.
func (c *ReengagementConfig) ScheduleValue() reengagement.Schedule {
	s := c.Schedule
	return reengagement.Schedule{
		DormantDays: s.DormantDays, R2AfterR1Days: s.R2AfterR1Days, R3Day: s.R3Day, EndAfterR3Days: s.EndAfterR3Days,
		NeverActiveDay: s.NeverActiveDay, NKeepAfterN1Days: s.NKeepAfterN1Days, CooldownDays: s.CooldownDays,
		QuietDays: s.QuietDays, FixWindowDays: s.FixWindowDays, WinbackDays: s.WinbackDays, StoreRetryDays: s.StoreRetryDays, KeepDays: s.KeepDays,
		MinGapDays: s.MinGapDays, MaxPerYear: s.MaxPerYear, SendHour: s.SendHour, SendWindow: s.SendWindow,
	}.WithDefaults()
}

// StreamEnabled reports whether the scan opens episodes for a stream.
func (c *ReengagementConfig) StreamEnabled(stream string) bool {
	switch stream {
	case reengagement.StreamLapsed:
		return c.Streams.Lapsed
	case reengagement.StreamNeverActivated:
		return c.Streams.NeverActivated
	case reengagement.StreamFix:
		return c.Streams.Fix
	case reengagement.StreamWinback:
		return c.Streams.Winback
	}
	return false
}

func (c *ReengagementConfig) BatchLimit() int {
	if c.Scan.BatchLimit <= 0 {
		return 2000
	}
	return c.Scan.BatchLimit
}

func (c *ReengagementConfig) CatchUpDays() int {
	if c.Scan.CatchUpDays <= 0 {
		return 3
	}
	return c.Scan.CatchUpDays
}

// EffectiveResultsRollupDays is results.rollup_days or the default.
func (c *ReengagementConfig) EffectiveResultsRollupDays() int {
	if c.Results.RollupDays <= 0 {
		return ReengagementResultsRollupDaysDefault
	}
	return c.Results.RollupDays
}

// The site, company line and reply-to fall back to onboarding's.
func (c *ReengagementConfig) EffectiveSiteUrl() string {
	return Onboarding().EffectiveSiteUrl()
}

func (c *ReengagementConfig) EffectiveCompanyLine() string {
	if v := strings.TrimSpace(c.Brevo.CompanyLine); v != "" {
		return v
	}
	return Onboarding().EffectiveCompanyLine()
}

func (c *ReengagementConfig) EffectiveReplyTo() string {
	if v := strings.TrimSpace(c.Brevo.ReplyTo); v != "" {
		return v
	}
	return Onboarding().EffectiveReplyTo()
}

// WinbackOfferRefusal is why W1 may not issue the comeback offer at all, or ""
// when it may: turned on, with a deadline, and a shape that keeps the rule
// over every offer against the configured welcome offer at every Pro price
// tier.
func (c *ReengagementConfig) WinbackOfferRefusal() string {
	return c.winbackOfferRefusal(Onboarding().Offer.PercentOff, Pro().PriceTiers())
}

func (c *ReengagementConfig) winbackOfferRefusal(welcomePercentOff int, tiers []*ProPriceTier) string {
	w := c.WinbackOffer
	if !w.Enabled {
		return "winback_offer.enabled is false"
	}
	if w.ValidityDays <= 0 {
		return "winback_offer.validity_days is not set"
	}
	prices := make([]reengagement.WinbackTierPrices, 0, len(tiers))
	for _, tier := range tiers {
		prices = append(prices, reengagement.WinbackTierPrices{Name: tier.Name, MonthlyUsd: tier.MonthlyUsd, YearlyUsd: tier.YearlyUsd})
	}
	if refusal := reengagement.WinbackShapeRefusal(strings.TrimSpace(w.Plan), w.PercentOff, welcomePercentOff, prices); refusal != "" {
		winbackShapeRefusalLog.Do(func() {
			glog.Errorf("[reengagement]winback_offer is refused: %s; W1 issues no offer\n", refusal)
		})
		return refusal
	}
	return ""
}

var winbackShapeRefusalLog sync.Once

// WinbackOfferEnabled is WinbackOfferRefusal() == "".
func (c *ReengagementConfig) WinbackOfferEnabled() bool {
	return c.WinbackOfferRefusal() == ""
}

// WinbackOfferValidity is how long an issued offer stays redeemable.
func (c *ReengagementConfig) WinbackOfferValidity() time.Duration {
	return time.Duration(c.WinbackOffer.ValidityDays) * 24 * time.Hour
}

// WinbackStoreHandle is the configured handle of a store's offer: the Stripe
// coupon, the Play offer tag or the App Store offer identifier. "" when the
// store has none, which means no offer on that store.
func (c *ReengagementConfig) WinbackStoreHandle(store string) string {
	switch store {
	case reengagement.WinbackStoreStripe:
		return strings.TrimSpace(c.WinbackOffer.StripeCouponId)
	case reengagement.WinbackStorePlay:
		return strings.TrimSpace(c.WinbackOffer.PlayOfferTag)
	case reengagement.WinbackStoreApple:
		return strings.TrimSpace(c.WinbackOffer.AppleOfferId)
	}
	return ""
}

// WinbackAppleOfferUrl is the App Store win-back offer's direct link when one
// of the documented form is configured, else "".
func (c *ReengagementConfig) WinbackAppleOfferUrl() string {
	link := strings.TrimSpace(c.WinbackOffer.AppleOfferUrl)
	id, ok := strings.CutPrefix(link, AppleWinbackOfferUrlPrefix)
	if !ok || id == "" || strings.Trim(id, "0123456789") != "" {
		return ""
	}
	return link
}

// AssignmentForSurface is the network's variant of the experiment running on a
// surface at `now`, or nil. Paused variants are served as control, as in
// onboarding.
func (c *ReengagementConfig) AssignmentForSurface(networkId server.Id, surface string, now time.Time) *ExperimentAssignment {
	for _, e := range c.Experiments {
		if e.Surface != surface || !e.Active(now) {
			continue
		}
		variant := e.Assign(networkId)
		if paused := PausedVariantsForExperiment(e.Id, now); len(paused) != 0 {
			variant = onboarding.EffectiveVariant(variant, e.VariantNames(), paused)
		}
		return &ExperimentAssignment{ExperimentId: e.Id, Variant: variant}
	}
	return nil
}
