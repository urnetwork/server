package model

import (
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/onboarding"
)

// mmm/ONBOARDING-RETENTION.md P18: the desktop email goes only once its own
// template is uploaded. TemplateId falls back to a step's `default` variant,
// so it cannot say whether the variant itself exists; HasTemplateVariant does,
// and an id of 0 is "not uploaded".
func TestHasTemplateVariantIgnoresTheDefaultFallback(t *testing.T) {
	b := OnboardingBrevoConfig{Templates: map[string]map[string]map[string]int{
		onboarding.TemplateE2Widget: {
			onboarding.VariantActivated:    {"en": 58},
			onboarding.VariantNotActivated: {"en": 86},
		},
	}}
	if b.HasTemplateVariant(onboarding.TemplateE2Widget, onboarding.VariantDesktop) {
		t.Error("no desktop ids: the variant exists")
	}
	b.Templates[onboarding.TemplateE2Widget][TemplateVariantDefault] = map[string]int{"en": 1}
	if _, ok := b.TemplateId(onboarding.TemplateE2Widget, onboarding.VariantDesktop, "en"); !ok {
		t.Fatal("the fixture's default variant does not stand in")
	}
	if b.HasTemplateVariant(onboarding.TemplateE2Widget, onboarding.VariantDesktop) {
		t.Error("a default variant made the desktop variant exist")
	}
	b.Templates[onboarding.TemplateE2Widget][onboarding.VariantDesktop] = map[string]int{"en": 0, "de": 0}
	if b.HasTemplateVariant(onboarding.TemplateE2Widget, onboarding.VariantDesktop) {
		t.Error("ids of 0 made the desktop variant exist")
	}
	b.Templates[onboarding.TemplateE2Widget][onboarding.VariantDesktop]["de"] = 500
	if !b.HasTemplateVariant(onboarding.TemplateE2Widget, onboarding.VariantDesktop) {
		t.Error("an uploaded desktop template is not found")
	}
	if b.HasTemplateVariant("e9_unknown", onboarding.VariantDesktop) {
		t.Error("an unknown step has the variant")
	}
}

// mmm/ONBOARDING-RETENTION.md P3, pinned on the checked-in config/main
// onboarding.yml (URNETWORK_CONFIG_ROOT, or the checkout next to the server's):
// a restart that is not done decides on a metric both of its arms can reach,
// with the planned control rate fixed before its start. email_sequence_2 was
// to decide on app_open_48h, which is written only after a landing click: its
// holdout gets no email, so the metric was zero there by construction (6 of
// 1,313 control and 0 of 130 holdout networks in email_sequence's last
// cohorts). offer_screen_2 had no base_rate.
func TestOnboardingRegistryRestartsCanDecide(t *testing.T) {
	data := configRepoFile(t, filepath.Join("main", "onboarding.yml"))
	var y onboardingYaml
	if err := yaml.Unmarshal(data, &y); err != nil {
		t.Fatal(err)
	}
	c := newOnboardingConfig(&y)
	byId := map[string]*OnboardingExperiment{}
	for _, e := range c.Experiments {
		byId[e.Id] = e
	}
	for _, e := range c.Experiments {
		if e.Status == ExperimentStatusDone {
			continue
		}
		if _, holdout := e.Variants[ExperimentVariantHoldout]; holdout && e.Surface == ExperimentSurfaceEmailSequence && e.PrimaryMetric == "app_open_48h" {
			t.Errorf("%s decides on app_open_48h, which its mail-less holdout cannot reach", e.Id)
		}
		if e.BaseRate <= 0 || 1 <= e.BaseRate {
			t.Errorf("%s has no planned base_rate in (0, 1): %g", e.Id, e.BaseRate)
		}
	}
	restart := byId["email_sequence_2"]
	if restart == nil {
		t.Fatal("email_sequence_2 is not in the registry")
	}
	if restart.PrimaryMetric != "connect_7d" || restart.BaseRate != 0.72 {
		t.Errorf("email_sequence_2 decides on %s at a base rate of %g, want connect_7d at 0.72", restart.PrimaryMetric, restart.BaseRate)
	}
	offer := byId["offer_screen_2"]
	if offer == nil {
		t.Fatal("offer_screen_2 is not in the registry")
	}
	if offer.PrimaryMetric != "purchase_started_7d" || offer.BaseRate != 0.0047 {
		t.Errorf("offer_screen_2 decides on %s at a base rate of %g, want purchase_started_7d at 0.0047", offer.PrimaryMetric, offer.BaseRate)
	}
}
