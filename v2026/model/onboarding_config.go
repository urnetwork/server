package model

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/onboarding"
)

// Onboarding is the parsed onboarding program config (config/<env>/onboarding.yml;
// see mmm/onboarding/PLAN.md). It carries the welcome offer terms, the email
// schedule, the Brevo template ids and the EXPERIMENT REGISTRY. Everything here is
// data: a change ships by config injection, never by a server release.
//
// The file is OPTIONAL. Without it every accessor returns the zero config, which
// is an INERT program: no offer can be issued (OfferEnabled is false), no
// experiment is running, and the campaign engine has nothing to send.

// ----- raw yaml shapes -----

type onboardingYaml struct {
	Onboarding OnboardingConfig `yaml:"onboarding"`
}

type OnboardingBrevoConfig struct {
	SenderId int `yaml:"sender_id"`
	// Templates is step -> variant -> locale -> Brevo template id, written by
	// mmm/onboarding/build.sh. Read through TemplateId, which applies the
	// variant (`default`) and locale (`en`) fallbacks; a missing or 0 id means
	// "not uploaded": the campaign engine must skip the step rather than send it.
	Templates  map[string]map[string]map[string]int `yaml:"templates"`
	TestListId int                                  `yaml:"test_list_id"`
	// CompanyLine is the footer company line every campaign template renders as
	// `{{ params.company_line }}` (the postal address never lives in a template).
	// Empty = the account mailer's line (OnboardingCompanyLineDefault).
	CompanyLine string `yaml:"company_line"`
	// ReplyTo is the monitored reply address on every campaign send. Empty =
	// support@ur.io.
	ReplyTo string `yaml:"reply_to"`
	// WebhookUrl is the public URL of POST /updates/brevo that the transactional
	// webhook is registered against (bringyourctl onboarding webhook-ensure).
	// Empty = the registration is skipped.
	WebhookUrl string `yaml:"webhook_url"`
}

// OnboardingCompanyLineDefault mirrors the account mailer's footer line
// (controller/email_templates/_layout.txt) so the two mails match when the
// config does not override it.
const OnboardingCompanyLineDefault = "BringYour, Inc. · 2261 Market Street #5245, San Francisco, CA 94114, United States"

// OnboardingAppleOfferCodesConfig is the non-secret side of the App Store
// Connect one-time offer code top-up (mmm/onboarding/PLAN.md "THE OFFER" App
// Store). The credentials live in vault/main/apple_app_store_connect.yml.
type OnboardingAppleOfferCodesConfig struct {
	// OfferCodeId is the App Store Connect `subscriptionOfferCodes` resource id
	// of the onboarding25 offer code. Empty = the top-up task is inert and the
	// custom code stays the only App Store path.
	OfferCodeId string `yaml:"offer_code_id"`
	// BatchSize per top-up (Apple's minimum is 500)
	BatchSize int `yaml:"batch_size"`
	// MinAvailable: a top-up runs when fewer unexpired, unassigned codes remain
	MinAvailable int `yaml:"min_available"`
	// ExpiryDays of a batch (the offer's validity, 5)
	ExpiryDays int `yaml:"expiry_days"`
}

// Template variant and locale fallbacks.
const (
	TemplateVariantDefault = "default"
	TemplateLocaleDefault  = "en"
)

// TemplateId selects a Brevo template: the step's variant (else `default`), in
// the recipient's locale (else the locale's language, else `en`). ok is false
// when nothing usable is configured.
func (c *OnboardingBrevoConfig) TemplateId(step string, variant string, locale string) (int, bool) {
	variants, ok := c.Templates[step]
	if !ok {
		return 0, false
	}
	locales, ok := variants[variant]
	if !ok || len(locales) == 0 {
		locales, ok = variants[TemplateVariantDefault]
		if !ok || len(locales) == 0 {
			return 0, false
		}
	}
	for _, candidate := range TemplateLocaleCandidates(locale) {
		if id, ok := locales[candidate]; ok && 0 < id {
			return id, true
		}
	}
	return 0, false
}

// TemplateLocaleCandidates orders the locales to try for a recipient: the exact
// tag, its language alone, then `en`. Tags are matched as the store writes them
// (es-419, pt-BR, zh-HK), case-insensitively on the language.
func TemplateLocaleCandidates(locale string) []string {
	locale = strings.TrimSpace(strings.ReplaceAll(locale, "_", "-"))
	candidates := []string{}
	if locale != "" {
		candidates = append(candidates, locale)
		if language, _, ok := strings.Cut(locale, "-"); ok && language != "" {
			candidates = append(candidates, strings.ToLower(language))
		} else {
			candidates = append(candidates, strings.ToLower(locale))
		}
	}
	candidates = append(candidates, TemplateLocaleDefault)
	return candidates
}

type OnboardingOfferConfig struct {
	PercentOff   int `yaml:"percent_off"`
	MonthsFree   int `yaml:"months_free"`
	ValidityDays int `yaml:"validity_days"`
	// Stripe coupon id, created or fetched by the server (percent_off applies)
	StripeCouponId string `yaml:"stripe_coupon_id"`
	// Play offer tag on the discounted first cycle of the yearly base plan
	PlayOfferTag string `yaml:"play_offer_tag"`
	// config resource path of an App Store Connect one-time offer code batch csv
	// (`code,expires` rows); loaded into the network_onboarding_apple_offer_code
	// pool. Empty = no batch.
	AppleOfferCodesPath string `yaml:"apple_offer_codes_path"`
	// App Store custom offer code (redeemable many times) used when the one-time
	// pool is empty or not configured
	AppleCustomOfferCode      string `yaml:"apple_custom_offer_code"`
	AppleOfferCodeBatchPrefix string `yaml:"apple_offer_code_batch_prefix"`
	// Apple is the App Store Connect one-time code top-up (S2); credentials in
	// vault/main/apple_app_store_connect.yml
	Apple OnboardingAppleOfferCodesConfig `yaml:"apple"`
}

type OnboardingScheduleConfig struct {
	E1Day      int    `yaml:"e1_day"`
	E2Day      int    `yaml:"e2_day"`
	E3aDay     int    `yaml:"e3a_day"`
	E4aDay     int    `yaml:"e4a_day"`
	E3bDay     int    `yaml:"e3b_day"`
	E4bDay     int    `yaml:"e4b_day"`
	E5bDay     int    `yaml:"e5b_day"`
	SendWindow string `yaml:"send_window"`
}

type OnboardingHoldoutConfig struct {
	InAppOfferHoldoutPercent int `yaml:"in_app_offer_holdout_percent"`
	EmailHoldoutPercent      int `yaml:"email_holdout_percent"`
}

// OnboardingExperiment is one registry entry, in the exact PLAN.md format.
type OnboardingExperiment struct {
	Id      string `yaml:"id"`
	Surface string `yaml:"surface"`
	// draft | running | paused | done. Only running experiments assign.
	Status string `yaml:"status"`
	// inclusive start day and exclusive stop day, YYYY-MM-DD (UTC); either may be empty
	Start string `yaml:"start"`
	Stop  string `yaml:"stop"`
	// percent per variant, summing to 100; a missing allocation splits evenly
	Allocation map[string]float64 `yaml:"allocation"`
	// per-variant data the surfaces read (copy key set, template name, offsets)
	Variants      map[string]map[string]any `yaml:"variants"`
	PrimaryMetric string                    `yaml:"primary_metric"`
	Secondary     []string                  `yaml:"secondary"`
	Guardrails    map[string]float64        `yaml:"guardrails"`
	MinExposures  int                       `yaml:"min_exposures"`
}

type OnboardingConfig struct {
	// Enabled gates every outbound side effect of the campaign engine: Brevo
	// sends, contact upserts, the webhook registration and the App Store
	// Connect code batches. Default false: the engine still creates rows and
	// runs its schedule, and logs each send it would have made at V(1).
	Enabled bool `yaml:"enabled"`
	// SiteUrl is the base of the landing and feedback links (https://ur.io).
	SiteUrl     string                   `yaml:"site_url"`
	Brevo       OnboardingBrevoConfig    `yaml:"brevo"`
	Offer       OnboardingOfferConfig    `yaml:"offer"`
	Schedule    OnboardingScheduleConfig `yaml:"schedule"`
	Experiment  OnboardingHoldoutConfig  `yaml:"experiment"`
	Experiments []*OnboardingExperiment  `yaml:"experiments"`
	Results     OnboardingResultsConfig  `yaml:"results"`
}

// OnboardingResultsConfig tunes the nightly results rollup and the admin
// results endpoint (PLAN.md "OPTIMIZATION LOOP" §3-§5).
type OnboardingResultsConfig struct {
	// MinExposures is the volume floor of /admin/onboarding/results: a row with
	// fewer exposures is not returned (it would identify a handful of networks
	// by dimension alone). Default 10.
	MinExposures int `yaml:"min_exposures"`
	// RollupDays is how many cohort days back the nightly rollup recomputes so
	// the fixed-window outcomes (up to 60 days) mature in place. Default 60.
	RollupDays int `yaml:"rollup_days"`
	// GuardrailDays is the cohort window the guardrail check sums over before
	// comparing a variant's rates with the registry thresholds. Default 14.
	GuardrailDays int `yaml:"guardrail_days"`
}

// Defaults of OnboardingResultsConfig.
const (
	OnboardingResultsMinExposuresDefault  = 10
	OnboardingResultsRollupDaysDefault    = 60
	OnboardingResultsGuardrailDaysDefault = 14
)

// EffectiveMinExposures is results.min_exposures or the default.
func (c *OnboardingConfig) EffectiveMinExposures() int {
	if 0 < c.Results.MinExposures {
		return c.Results.MinExposures
	}
	return OnboardingResultsMinExposuresDefault
}

// EffectiveRollupDays is results.rollup_days or the default.
func (c *OnboardingConfig) EffectiveRollupDays() int {
	if 0 < c.Results.RollupDays {
		return c.Results.RollupDays
	}
	return OnboardingResultsRollupDaysDefault
}

// EffectiveGuardrailDays is results.guardrail_days or the default.
func (c *OnboardingConfig) EffectiveGuardrailDays() int {
	if 0 < c.Results.GuardrailDays {
		return c.Results.GuardrailDays
	}
	return OnboardingResultsGuardrailDaysDefault
}

// OnboardingSiteUrlDefault is the site the campaign links point at.
const OnboardingSiteUrlDefault = "https://ur.io"

// EffectiveSiteUrl is SiteUrl or the default.
func (c *OnboardingConfig) EffectiveSiteUrl() string {
	if v := strings.TrimSpace(c.SiteUrl); v != "" {
		return v
	}
	return OnboardingSiteUrlDefault
}

// EffectiveCompanyLine is brevo.company_line or the account mailer's line.
func (c *OnboardingConfig) EffectiveCompanyLine() string {
	if v := strings.TrimSpace(c.Brevo.CompanyLine); v != "" {
		return v
	}
	return OnboardingCompanyLineDefault
}

// EffectiveReplyTo is brevo.reply_to or support@ur.io.
func (c *OnboardingConfig) EffectiveReplyTo() string {
	if v := strings.TrimSpace(c.Brevo.ReplyTo); v != "" {
		return v
	}
	return "support@ur.io"
}

// Experiment status values.
const (
	ExperimentStatusDraft   = "draft"
	ExperimentStatusRunning = "running"
	ExperimentStatusPaused  = "paused"
	ExperimentStatusDone    = "done"
)

// ExperimentVariantHoldout is the conventional name of the "show nothing" variant.
// It is an ordinary registry variant; the surfaces give it its meaning.
const ExperimentVariantHoldout = "holdout"

// Well-known surfaces. An experiment may name any surface; these are the ones the
// server's own stamping understands.
const (
	// the whole in-app offer: the intro plan step's welcome price and the final
	// offer screen together
	ExperimentSurfaceOfferInApp       = "offer.in_app"
	ExperimentSurfaceOfferIntroStep   = "offer.intro_step"
	ExperimentSurfaceOfferFinalScreen = "offer.final_screen"
	ExperimentSurfaceOfferEmail       = "offer.email"
	ExperimentSurfaceOfferAccount     = "offer.account"
	// the onboarding mail sequence E1..E5 as a whole
	ExperimentSurfaceEmailSequence = "email.sequence"
	ExperimentSurfaceCadence       = "cadence"
)

// experimentBuckets is the resolution of the hash allocation: percents are
// applied in hundredths (basis points), so 0.01% is the smallest allocation.
const experimentBuckets = 10000

var onboardingConfig = sync.OnceValue(func() *OnboardingConfig {
	// OPTIONAL: see the package doc. A missing file is the inert program, not a
	// boot failure.
	resource, err := server.Config.SimpleResource("onboarding.yml")
	if err != nil {
		glog.Infof("[onboarding]onboarding.yml is not present; the onboarding program is inert. err = %s\n", err)
		return &OnboardingConfig{}
	}
	var y onboardingYaml
	if err := resource.UnmarshalYamlE(&y); err != nil {
		// a malformed registry must not take the process down; it runs inert
		// and says so
		glog.Errorf("[onboarding]onboarding.yml could not be parsed; the onboarding program is inert. err = %s\n", err)
		return &OnboardingConfig{}
	}
	c := &y.Onboarding
	c.Experiments = validExperiments(c.Experiments)
	return c
})

// Onboarding returns the parsed onboarding config (cached; parsed once).
func Onboarding() *OnboardingConfig {
	return onboardingConfig()
}

// validExperiments drops registry entries that cannot be assigned safely and logs
// each one. A bad entry must not stop the others.
func validExperiments(experiments []*OnboardingExperiment) []*OnboardingExperiment {
	out := make([]*OnboardingExperiment, 0, len(experiments))
	ids := map[string]bool{}
	for _, e := range experiments {
		if e == nil {
			continue
		}
		if err := e.Validate(); err != nil {
			glog.Errorf("[onboarding]experiment %q is skipped: %s\n", e.Id, err)
			continue
		}
		if ids[e.Id] {
			glog.Errorf("[onboarding]experiment %q is skipped: duplicate id\n", e.Id)
			continue
		}
		ids[e.Id] = true
		out = append(out, e)
	}
	return out
}

// Validate checks a registry entry: id and surface present, a known status,
// parseable dates, and an allocation that names only declared variants and sums
// to 100.
func (e *OnboardingExperiment) Validate() error {
	if strings.TrimSpace(e.Id) == "" {
		return fmt.Errorf("missing id")
	}
	if strings.TrimSpace(e.Surface) == "" {
		return fmt.Errorf("missing surface")
	}
	switch e.Status {
	case ExperimentStatusDraft, ExperimentStatusRunning, ExperimentStatusPaused, ExperimentStatusDone:
	default:
		return fmt.Errorf("unknown status %q", e.Status)
	}
	if _, err := parseExperimentDay(e.Start); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if _, err := parseExperimentDay(e.Stop); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if len(e.Variants) == 0 {
		return fmt.Errorf("no variants")
	}
	if len(e.Allocation) == 0 {
		return nil
	}
	total := 0.0
	for name, percent := range e.Allocation {
		if _, ok := e.Variants[name]; !ok {
			return fmt.Errorf("allocation names undeclared variant %q", name)
		}
		if percent < 0 || math.IsNaN(percent) || math.IsInf(percent, 0) {
			return fmt.Errorf("allocation %q is not a percent", name)
		}
		total += percent
	}
	if math.Abs(total-100) > 0.001 {
		return fmt.Errorf("allocation sums to %g, not 100", total)
	}
	return nil
}

// ParseExperimentDay parses a registry start/stop day (YYYY-MM-DD or RFC 3339);
// empty means unbounded (nil).
func ParseExperimentDay(value string) (*time.Time, error) {
	return parseExperimentDay(value)
}

func parseExperimentDay(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	// yaml.v3 hands a bare date through as its text; accept a full timestamp too
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			t = t.UTC()
			return &t, nil
		}
	}
	return nil, fmt.Errorf("invalid date %q", value)
}

// Active reports whether the experiment assigns at `now`: status running and
// start <= now < stop.
func (e *OnboardingExperiment) Active(now time.Time) bool {
	if e.Status != ExperimentStatusRunning {
		return false
	}
	start, err := parseExperimentDay(e.Start)
	if err != nil {
		return false
	}
	stop, err := parseExperimentDay(e.Stop)
	if err != nil {
		return false
	}
	if start != nil && now.Before(*start) {
		return false
	}
	if stop != nil && !now.Before(*stop) {
		return false
	}
	return true
}

// VariantNames are the declared variants in sorted order -- the order the
// allocation is walked in, so assignment does not depend on map iteration.
func (e *OnboardingExperiment) VariantNames() []string {
	names := make([]string, 0, len(e.Variants))
	for name := range e.Variants {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ExperimentBucket is hash(network_id, experiment_id) folded to [0, 10000). The
// same network lands in the same bucket on every server and every day, which is
// what keeps a variant stable for the life of an experiment with no assignment
// table.
func ExperimentBucket(networkId server.Id, experimentId string) int {
	h := sha256.New()
	h.Write(networkId.Bytes())
	h.Write([]byte{0})
	h.Write([]byte(experimentId))
	sum := h.Sum(nil)
	return int(binary.BigEndian.Uint64(sum[:8]) % experimentBuckets)
}

// VariantForBucket walks the allocation in sorted variant order and returns the
// variant whose share covers the bucket. Without an allocation the variants
// split evenly. Rounding leaves the last variant any remainder.
func (e *OnboardingExperiment) VariantForBucket(bucket int) string {
	names := e.VariantNames()
	if len(names) == 0 {
		return ""
	}
	if bucket < 0 {
		bucket = 0
	}
	if experimentBuckets <= bucket {
		bucket = experimentBuckets - 1
	}
	edge := 0
	for i, name := range names {
		var share int
		if len(e.Allocation) == 0 {
			share = experimentBuckets / len(names)
		} else {
			share = int(math.Round(e.Allocation[name] * experimentBuckets / 100))
		}
		if i == len(names)-1 {
			return name
		}
		edge += share
		if bucket < edge {
			return name
		}
	}
	return names[len(names)-1]
}

// Assign is the variant for a network: the hash bucket walked over the allocation.
func (e *OnboardingExperiment) Assign(networkId server.Id) string {
	return e.VariantForBucket(ExperimentBucket(networkId, e.Id))
}

// ExperimentAssignment is what the plan response carries per surface and what
// exposure events are stamped with.
type ExperimentAssignment struct {
	ExperimentId string `json:"experiment_id"`
	Variant      string `json:"variant"`
}

// ActiveExperiments are the registry entries assigning at `now`, in registry order.
func (c *OnboardingConfig) ActiveExperiments(now time.Time) []*OnboardingExperiment {
	out := []*OnboardingExperiment{}
	for _, e := range c.Experiments {
		if e.Active(now) {
			out = append(out, e)
		}
	}
	return out
}

// AssignExperiments returns the network's variant for every active experiment,
// keyed by surface. When two active experiments share a surface the first in
// registry order wins (and the registry should not do that).
//
// The experiment-state overlay (network_onboarding_experiment_state, written by
// the guardrail check and `bringyourctl onboarding experiments`) is applied on
// top of the hash assignment: a network whose variant is paused is served the
// control variant instead (see onboarding.EffectiveVariant). The registry
// assignment itself is unchanged, so resuming the variant restores the same
// networks to it.
func (c *OnboardingConfig) AssignExperiments(networkId server.Id, now time.Time) map[string]*ExperimentAssignment {
	assignments := map[string]*ExperimentAssignment{}
	for _, e := range c.ActiveExperiments(now) {
		if _, taken := assignments[e.Surface]; taken {
			continue
		}
		variant := e.Assign(networkId)
		if paused := PausedVariantsForExperiment(e.Id, now); len(paused) != 0 {
			variant = onboarding.EffectiveVariant(variant, e.VariantNames(), paused)
		}
		assignments[e.Surface] = &ExperimentAssignment{
			ExperimentId: e.Id,
			Variant:      variant,
		}
	}
	return assignments
}

// ExperimentSurfaceFallbacks maps a specific surface to the family surface that
// stands in for it when no experiment names it directly: an exposure on the
// intro step or the final screen belongs to the in-app offer experiment, and any
// email step belongs to the sequence experiment.
func ExperimentSurfaceFallbacks(surface string) []string {
	switch {
	case surface == ExperimentSurfaceOfferIntroStep, surface == ExperimentSurfaceOfferFinalScreen:
		return []string{surface, ExperimentSurfaceOfferInApp}
	case strings.HasPrefix(surface, "email.") && surface != ExperimentSurfaceEmailSequence:
		return []string{surface, ExperimentSurfaceEmailSequence}
	default:
		return []string{surface}
	}
}

// AssignmentForSurface is the assignment an exposure on `surface` is stamped
// with: the surface's own experiment, else its family's (see
// ExperimentSurfaceFallbacks), else nil.
func (c *OnboardingConfig) AssignmentForSurface(networkId server.Id, surface string, now time.Time) *ExperimentAssignment {
	assignments := c.AssignExperiments(networkId, now)
	for _, candidate := range ExperimentSurfaceFallbacks(surface) {
		if a, ok := assignments[candidate]; ok {
			return a
		}
	}
	return nil
}

// OfferEnabled reports whether a welcome offer can be issued at all: a positive
// discount with a positive validity. The zero config (no onboarding.yml) is not
// enabled, so nothing is ever promised that the config cannot describe.
func (c *OnboardingConfig) OfferEnabled() bool {
	return 0 < c.Offer.PercentOff && c.Offer.PercentOff < 100 && 0 < c.Offer.ValidityDays
}

// OfferValidity is how long an issued offer stays redeemable.
func (c *OnboardingConfig) OfferValidity() time.Duration {
	return time.Duration(c.Offer.ValidityDays) * 24 * time.Hour
}

// OfferPriceUsd is a price with the welcome discount applied, rounded to the cent.
func (c *OnboardingConfig) OfferPriceUsd(regularUsd float64) float64 {
	return math.Round(regularUsd*float64(100-c.Offer.PercentOff)) / 100
}
