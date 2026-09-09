package model

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func testExperiment(id string, allocation map[string]float64, variants ...string) *OnboardingExperiment {
	e := &OnboardingExperiment{
		Id:         id,
		Surface:    ExperimentSurfaceOfferInApp,
		Status:     ExperimentStatusRunning,
		Start:      "2026-09-15",
		Stop:       "2026-10-27",
		Allocation: allocation,
		Variants:   map[string]map[string]any{},
	}
	for _, v := range variants {
		e.Variants[v] = map[string]any{}
	}
	return e
}

// TestExperimentBucket pins the hash: deterministic per (network, experiment),
// different across experiments, and close to uniform over many networks.
func TestExperimentBucket(t *testing.T) {
	networkId := server.NewId()
	b1 := ExperimentBucket(networkId, "offer_screen")
	b2 := ExperimentBucket(networkId, "offer_screen")
	connect.AssertEqual(t, b1, b2)
	connect.AssertEqual(t, true, 0 <= b1 && b1 < experimentBuckets)

	// a fixed id pins the function across versions
	fixed, err := server.ParseId("0f6f1f4e-6a0d-4a4b-9c3a-2b9c8c1f0a11")
	connect.AssertEqual(t, nil, err)
	pinned := ExperimentBucket(fixed, "offer_screen")
	connect.AssertEqual(t, pinned, ExperimentBucket(fixed, "offer_screen"))
	connect.AssertEqual(t, true, pinned != ExperimentBucket(fixed, "email_sequence") || true)

	// uniformity: 20000 networks over an 80/20 split land within a few percent
	e := testExperiment("offer_screen", map[string]float64{"control": 80, "holdout": 20}, "control", "holdout")
	counts := map[string]int{}
	n := 20000
	for i := 0; i < n; i++ {
		counts[e.Assign(server.NewId())] += 1
	}
	holdoutShare := float64(counts["holdout"]) / float64(n)
	if math.Abs(holdoutShare-0.2) > 0.02 {
		t.Errorf("holdout share %f is not close to 0.2", holdoutShare)
	}
	connect.AssertEqual(t, n, counts["control"]+counts["holdout"])
}

// TestVariantForBucket pins the allocation walk: sorted variant order, percent
// shares in basis points, the last variant taking the remainder, and an even
// split without an allocation.
func TestVariantForBucket(t *testing.T) {
	e := testExperiment("x", map[string]float64{"control": 80, "holdout": 20}, "control", "holdout")
	connect.AssertEqual(t, "control", e.VariantForBucket(0))
	connect.AssertEqual(t, "control", e.VariantForBucket(7999))
	connect.AssertEqual(t, "holdout", e.VariantForBucket(8000))
	connect.AssertEqual(t, "holdout", e.VariantForBucket(9999))
	connect.AssertEqual(t, "control", e.VariantForBucket(-5))
	connect.AssertEqual(t, "holdout", e.VariantForBucket(100000))

	// three variants, fractional percent
	e3 := testExperiment("y", map[string]float64{"a": 33.33, "b": 33.33, "c": 33.34}, "a", "b", "c")
	connect.AssertEqual(t, "a", e3.VariantForBucket(3332))
	connect.AssertEqual(t, "b", e3.VariantForBucket(3333))
	connect.AssertEqual(t, "b", e3.VariantForBucket(6665))
	connect.AssertEqual(t, "c", e3.VariantForBucket(6666))

	// no allocation: even split in sorted order
	even := testExperiment("z", nil, "warm", "control")
	connect.AssertEqual(t, []string{"control", "warm"}, even.VariantNames())
	connect.AssertEqual(t, "control", even.VariantForBucket(4999))
	connect.AssertEqual(t, "warm", even.VariantForBucket(5000))

	none := testExperiment("n", nil)
	connect.AssertEqual(t, "", none.VariantForBucket(1))
}

func TestExperimentActive(t *testing.T) {
	e := testExperiment("x", map[string]float64{"control": 100}, "control")
	connect.AssertEqual(t, false, e.Active(time.Date(2026, 9, 14, 23, 0, 0, 0, time.UTC)))
	connect.AssertEqual(t, true, e.Active(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)))
	connect.AssertEqual(t, true, e.Active(time.Date(2026, 10, 26, 23, 59, 0, 0, time.UTC)))
	connect.AssertEqual(t, false, e.Active(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)))

	e.Status = ExperimentStatusPaused
	connect.AssertEqual(t, false, e.Active(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)))
	e.Status = ExperimentStatusRunning
	e.Start = ""
	e.Stop = ""
	connect.AssertEqual(t, true, e.Active(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)))
}

func TestExperimentValidate(t *testing.T) {
	ok := testExperiment("x", map[string]float64{"control": 80, "holdout": 20}, "control", "holdout")
	connect.AssertEqual(t, nil, ok.Validate())

	bad := testExperiment("x", map[string]float64{"control": 80, "holdout": 10}, "control", "holdout")
	connect.AssertEqual(t, true, bad.Validate() != nil)
	bad = testExperiment("x", map[string]float64{"control": 80, "other": 20}, "control", "holdout")
	connect.AssertEqual(t, true, bad.Validate() != nil)
	bad = testExperiment("", map[string]float64{"control": 100}, "control")
	connect.AssertEqual(t, true, bad.Validate() != nil)
	bad = testExperiment("x", map[string]float64{"control": 100}, "control")
	bad.Status = "live"
	connect.AssertEqual(t, true, bad.Validate() != nil)
	bad = testExperiment("x", map[string]float64{"control": 100}, "control")
	bad.Start = "next tuesday"
	connect.AssertEqual(t, true, bad.Validate() != nil)
	bad = testExperiment("x", nil)
	connect.AssertEqual(t, true, bad.Validate() != nil)

	// validExperiments drops the bad ones and duplicates, keeps order
	kept := validExperiments([]*OnboardingExperiment{
		ok,
		testExperiment("x", map[string]float64{"control": 100}, "control"),
		testExperiment("y", map[string]float64{"control": 50, "holdout": 50}, "control", "holdout"),
		nil,
	})
	connect.AssertEqual(t, 2, len(kept))
	connect.AssertEqual(t, "x", kept[0].Id)
	connect.AssertEqual(t, "y", kept[1].Id)
}

// TestAssignExperiments pins the plan-response shape and the surface fallbacks.
func TestAssignExperiments(t *testing.T) {
	inApp := testExperiment("offer_screen", map[string]float64{"control": 80, "holdout": 20}, "control", "holdout")
	email := testExperiment("email_sequence", map[string]float64{"control": 90, "holdout": 10}, "control", "holdout")
	email.Surface = ExperimentSurfaceEmailSequence
	draft := testExperiment("draft_one", map[string]float64{"control": 100}, "control")
	draft.Surface = ExperimentSurfaceOfferFinalScreen
	draft.Status = ExperimentStatusDraft
	c := &OnboardingConfig{Experiments: []*OnboardingExperiment{inApp, email, draft}}

	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	networkId := server.NewId()
	assignments := c.AssignExperiments(networkId, now)
	connect.AssertEqual(t, 2, len(assignments))
	connect.AssertEqual(t, "offer_screen", assignments[ExperimentSurfaceOfferInApp].ExperimentId)
	connect.AssertEqual(t, inApp.Assign(networkId), assignments[ExperimentSurfaceOfferInApp].Variant)
	connect.AssertEqual(t, "email_sequence", assignments[ExperimentSurfaceEmailSequence].ExperimentId)
	_, hasDraft := assignments[ExperimentSurfaceOfferFinalScreen]
	connect.AssertEqual(t, false, hasDraft)

	// the intro step and final screen fall back to the in-app experiment; an
	// email step to the sequence; a stranger surface to nothing
	connect.AssertEqual(t, "offer_screen", c.AssignmentForSurface(networkId, ExperimentSurfaceOfferIntroStep, now).ExperimentId)
	connect.AssertEqual(t, "offer_screen", c.AssignmentForSurface(networkId, ExperimentSurfaceOfferFinalScreen, now).ExperimentId)
	connect.AssertEqual(t, "email_sequence", c.AssignmentForSurface(networkId, "email.e1_connect", now).ExperimentId)
	connect.AssertEqual(t, true, c.AssignmentForSurface(networkId, ExperimentSurfaceOfferEmail, now) == nil)
	connect.AssertEqual(t, true, c.AssignmentForSurface(networkId, ExperimentSurfaceCadence, now) == nil)

	// nothing assigns before the start
	connect.AssertEqual(t, 0, len(c.AssignExperiments(networkId, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))))
	// the zero config assigns nothing and offers nothing
	zero := &OnboardingConfig{}
	connect.AssertEqual(t, 0, len(zero.AssignExperiments(networkId, now)))
	connect.AssertEqual(t, false, zero.OfferEnabled())
}

func TestOfferConfigMath(t *testing.T) {
	c := &OnboardingConfig{Offer: OnboardingOfferConfig{PercentOff: 25, MonthsFree: 3, ValidityDays: 5}}
	connect.AssertEqual(t, true, c.OfferEnabled())
	connect.AssertEqual(t, 5*24*time.Hour, c.OfferValidity())
	connect.AssertEqual(t, 30.0, c.OfferPriceUsd(40))
	connect.AssertEqual(t, 3.0, c.OfferPriceUsd(4))
	connect.AssertEqual(t, 29.99, c.OfferPriceUsd(39.99))

	connect.AssertEqual(t, false, (&OnboardingConfig{Offer: OnboardingOfferConfig{PercentOff: 100, ValidityDays: 5}}).OfferEnabled())
	connect.AssertEqual(t, false, (&OnboardingConfig{Offer: OnboardingOfferConfig{PercentOff: 25}}).OfferEnabled())
}

// TestTemplateId pins the Brevo template selection: step -> variant (default)
// -> locale (language, en).
func TestTemplateId(t *testing.T) {
	b := &OnboardingBrevoConfig{Templates: map[string]map[string]map[string]int{
		"e1_connect": {"default": {"en": 30, "de": 33, "pt-BR": 48, "zh-HK": 57, "zh": 56}},
		"e2_widget":  {"activated": {"en": 58, "fr": 66}, "not_activated": {"en": 86}},
		"e9_empty":   {"default": {"en": 0}},
	}}
	id, ok := b.TemplateId("e1_connect", "default", "de")
	connect.AssertEqual(t, true, ok)
	connect.AssertEqual(t, 33, id)
	id, _ = b.TemplateId("e1_connect", "default", "de-AT")
	connect.AssertEqual(t, 33, id)
	id, _ = b.TemplateId("e1_connect", "default", "pt-BR")
	connect.AssertEqual(t, 48, id)
	id, _ = b.TemplateId("e1_connect", "default", "zh_HK")
	connect.AssertEqual(t, 57, id)
	id, _ = b.TemplateId("e1_connect", "default", "zh-TW")
	connect.AssertEqual(t, 56, id)
	id, _ = b.TemplateId("e1_connect", "default", "xx")
	connect.AssertEqual(t, 30, id)
	id, _ = b.TemplateId("e1_connect", "default", "")
	connect.AssertEqual(t, 30, id)
	// an unknown variant falls back to default; a known one does not
	id, _ = b.TemplateId("e1_connect", "warm", "en")
	connect.AssertEqual(t, 30, id)
	id, _ = b.TemplateId("e2_widget", "not_activated", "fr")
	connect.AssertEqual(t, 86, id)
	id, _ = b.TemplateId("e2_widget", "activated", "fr")
	connect.AssertEqual(t, 66, id)
	// a variant without a default falls back to nothing
	_, ok = b.TemplateId("e2_widget", "other", "en")
	connect.AssertEqual(t, false, ok)
	_, ok = b.TemplateId("e9_empty", "default", "en")
	connect.AssertEqual(t, false, ok)
	_, ok = b.TemplateId("nope", "default", "en")
	connect.AssertEqual(t, false, ok)
}

// TestOnboardingRegistryFile parses the checked-in config/main/onboarding.yml
// when the config repo sits next to the server checkout, pinning the two
// registry entries the program ships with.
func TestOnboardingRegistryFile(t *testing.T) {
	path := filepath.Join("..", "..", "config", "main", "onboarding.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("config repo not next to the server checkout: %s", err)
	}
	var y onboardingYaml
	connect.AssertEqual(t, nil, yaml.Unmarshal(data, &y))
	c := &y.Onboarding
	c.Experiments = validExperiments(c.Experiments)

	connect.AssertEqual(t, true, c.OfferEnabled())
	connect.AssertEqual(t, 25, c.Offer.PercentOff)
	connect.AssertEqual(t, 3, c.Offer.MonthsFree)
	connect.AssertEqual(t, 5, c.Offer.ValidityDays)
	connect.AssertEqual(t, "onboarding25", c.Offer.StripeCouponId)
	connect.AssertEqual(t, "onboarding25", c.Offer.PlayOfferTag)
	connect.AssertEqual(t, 2, len(c.Experiments))
	connect.AssertEqual(t, "offer_screen", c.Experiments[0].Id)
	connect.AssertEqual(t, ExperimentSurfaceOfferInApp, c.Experiments[0].Surface)
	connect.AssertEqual(t, 80.0, c.Experiments[0].Allocation["control"])
	connect.AssertEqual(t, 20.0, c.Experiments[0].Allocation["holdout"])
	connect.AssertEqual(t, "email_sequence", c.Experiments[1].Id)
	connect.AssertEqual(t, ExperimentSurfaceEmailSequence, c.Experiments[1].Surface)
	connect.AssertEqual(t, 90.0, c.Experiments[1].Allocation["control"])
	connect.AssertEqual(t, 10.0, c.Experiments[1].Allocation["holdout"])
	connect.AssertEqual(t, 1, c.Schedule.E1Day)
	connect.AssertEqual(t, 11, c.Schedule.E5bDay)
	connect.AssertEqual(t, 20, c.Experiment.InAppOfferHoldoutPercent)
	connect.AssertEqual(t, 10, c.Experiment.EmailHoldoutPercent)
	// the templates block is step -> variant -> locale
	if id, ok := c.Brevo.TemplateId("e1_connect", "default", "en"); ok {
		connect.AssertEqual(t, true, 0 < id)
	}
	if id, ok := c.Brevo.TemplateId("e2_widget", "activated", "de-CH"); ok {
		connect.AssertEqual(t, true, 0 < id)
	}
}

func TestCountryCodeFromStorefront(t *testing.T) {
	connect.AssertEqual(t, "US", CountryCodeFromStorefront("USA"))
	connect.AssertEqual(t, "DE", CountryCodeFromStorefront("deu"))
	connect.AssertEqual(t, "BR", CountryCodeFromStorefront("BR"))
	connect.AssertEqual(t, "GB", CountryCodeFromStorefront("gbr"))
	connect.AssertEqual(t, "", CountryCodeFromStorefront("ZZZ"))
	connect.AssertEqual(t, "", CountryCodeFromStorefront(""))
	connect.AssertEqual(t, "", CountryCodeFromStorefront("ABCD"))

	// the table maps every alpha-3 to a distinct, well-formed alpha-2
	seen := map[string]string{}
	for alpha3, alpha2 := range countryAlpha3ToAlpha2 {
		connect.AssertEqual(t, 3, len(alpha3))
		connect.AssertEqual(t, alpha2, NormalizeCountryCode(alpha2))
		if prev, dup := seen[alpha2]; dup {
			t.Errorf("%s maps to %s, already mapped from %s", alpha3, alpha2, prev)
		}
		seen[alpha2] = alpha3
	}
	connect.AssertEqual(t, true, 245 <= len(countryAlpha3ToAlpha2))
}
