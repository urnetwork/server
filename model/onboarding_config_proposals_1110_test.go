package model

import (
	"testing"

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
