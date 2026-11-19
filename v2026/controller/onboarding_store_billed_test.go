// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
)

// The offer emails name a price only to readers who pay by card through
// Stripe: the App Store and Google Play charge their own local price
// (mmm/onboarding/REVIEW.md finding 7). Email sign-ups from the Android app
// are often recorded without a platform, and the price rule read the row
// alone, so those readers, who buy through Google Play, were quoted the
// Stripe price in USD. These tests compose the facts the campaign step
// assembles (onboardingClientBuild, onboarding.OfferEmailWithheld), its
// decision and the send's params, as loadOnboardingFactsIn and
// buildOnboardingSendPlan do.

// unknownRowOfferFacts is a path B reader's facts at E4b, on a row recorded
// without a platform whose newest active client is `build`.
func unknownRowOfferFacts(build *model.NetworkClientBuild, appleEmailEnabled bool) onboarding.Facts {
	row := &model.NetworkOnboarding{}
	platform, distribution, widgetBuild := onboardingClientBuild(row, build, onboarding.DefaultWidgetSettings())
	return onboarding.Facts{
		Path:           onboarding.PathB,
		HasEmail:       true,
		NetworkExists:  true,
		ProductUpdates: true,
		NoOfferEmail:   onboarding.OfferEmailWithheld(true, platform, distribution, onboarding.PathB, appleEmailEnabled),
		Platform:       platform,
		Distribution:   distribution,
		WidgetBuild:    widgetBuild,
		HasConnected:   true,
	}
}

func TestOnboardingOfferEmailBillsAnUnknownRowByItsApp(t *testing.T) {
	// the row alone names no store: the rule that read it quoted every such
	// reader the Stripe price
	if onboarding.StoreBilledPlatform("", "") {
		t.Fatal("a row without a platform is billed by a store")
	}
	expires := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name        string
		build       *model.NetworkClientBuild
		storeBilled bool
	}{
		{"the Android app", &model.NetworkClientBuild{DeviceSpec: "16 Samsung Galaxy S24 Ultra", ClientVersion: "2026.10.15-1072765730"}, true},
		{"an older Android app", &model.NetworkClientBuild{DeviceSpec: "14 tecno/lh7n-ru/tecno-lh7n:14/up1a.231005.007/250416v782:user/release-keys", ClientVersion: "2026.9.1"}, true},
		{"the Windows app", &model.NetworkClientBuild{DeviceSpec: "Windows 11", ClientVersion: "2026.10.15-1072765730"}, false},
		{"a browser", &model.NetworkClientBuild{DeviceSpec: "Mozilla/5.0 Chrome/140", ClientVersion: ""}, false},
		{"no client", nil, false},
	} {
		decision := onboarding.Decide(onboarding.StepE4, unknownRowOfferFacts(c.build, false))
		if decision.Action != onboarding.ActionSend || decision.Template != onboarding.TemplateE4bOffer || !decision.IssueOffer {
			t.Fatalf("%s: E4b decision %+v, want the offer email", c.name, decision)
		}
		if decision.StoreBilled != c.storeBilled {
			t.Errorf("%s: store billed %t, want %t", c.name, decision.StoreBilled, c.storeBilled)
		}
		params := onboarding.BuildParams(onboarding.ParamsInput{
			Template:       decision.Template,
			Variant:        decision.Variant,
			StoreBilled:    decision.StoreBilled,
			OfferExpiresAt: &expires,
			OfferPriceUsd:  30,
			RegularYearUsd: 40,
			Location:       time.UTC,
			Token:          "synthetic-token",
			SiteUrl:        "https://ur.example",
		})
		want := ""
		if c.storeBilled {
			want = "1"
		}
		if params["store_billed"] != want {
			t.Errorf("%s: params.store_billed %q, want %q", c.name, params["store_billed"], want)
		}
	}
}

// An App Store reader can't redeem the emailed offer while no code reaches
// path B readers (offer.apple_email_enabled off), so E4b doesn't go to a row
// recorded without a platform whose app is the iPhone's, as it doesn't to a
// row recorded as ios; once the codes flow it goes, without a price.
func TestOnboardingOfferEmailWithheldFromAnUnknownRowOnTheAppStore(t *testing.T) {
	build := &model.NetworkClientBuild{DeviceSpec: "18.1 iPhone 16 Pro", ClientVersion: "2026.8.17"}
	if decision := onboarding.Decide(onboarding.StepE4, unknownRowOfferFacts(build, false)); decision.Action != onboarding.ActionSkip {
		t.Fatalf("E4b to an App Store reader without codes: %+v, want a skip", decision)
	}
	decision := onboarding.Decide(onboarding.StepE4, unknownRowOfferFacts(build, true))
	if decision.Action != onboarding.ActionSend || !decision.StoreBilled {
		t.Fatalf("E4b to an App Store reader with codes: %+v, want the store copy", decision)
	}
}
