// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"slices"
	"testing"
)

// Every send named only its program, template and variant, so Brevo's
// per-tag report could not tell an experiment's arms apart, and the variant
// tag ("default") is shared by both programs (2026-11-13 review). A send an
// experiment covers carries the experiment and arm tags after the three the
// webhook reads.
func TestCampaignTags(t *testing.T) {
	got := CampaignTags("reengagement", "r2_reason", "lapsed", "reengage_lapsed_1", "control")
	want := []string{"reengagement", "r2_reason", "lapsed", "exp-reengage_lapsed_1", "arm-reengage_lapsed_1-control"}
	if !slices.Equal(got, want) {
		t.Fatalf("tags %v, want %v", got, want)
	}
	// no experiment, or no arm: the three positional tags only
	for _, c := range [][2]string{{"", ""}, {"email_sequence_2", ""}, {"", "control"}, {" ", " "}} {
		if got := CampaignTags("onboarding", "e1_connect", "default", c[0], c[1]); !slices.Equal(got, []string{"onboarding", "e1_connect", "default"}) {
			t.Errorf("%q/%q: tags %v", c[0], c[1], got)
		}
	}
	// the arm tags of two arms differ, and never equal a template or variant tag
	if ArmTag("x", "control") == ArmTag("x", "holdout") || ExperimentTag("default") == "default" {
		t.Fatal("tags collide")
	}
}
