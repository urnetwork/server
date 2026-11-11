// SPDX-License-Identifier: MPL-2.0

package onboarding

// P6 (mmm/ONBOARDING-RETENTION.md, owner choice 2026-11-11): the App Store
// and direct-download Mac builds send the same device type, spec and version,
// so a Mac app got the App Store's rules: no offer email on path B while the
// App Store codes do not flow, the store-billed copy (no price) and the widget
// email, though the direct DMG bills through Stripe and ships no widgets. The
// apps report their distribution now (X-UR-ClientInfo), and the rules read it.

import (
	"testing"

	"github.com/urnetwork/server/session"
)

func TestBillingByDistribution(t *testing.T) {
	for _, c := range []struct {
		platform     string
		distribution string
		appStore     bool
		storeBilled  bool
	}{
		{"macos", session.ClientDistributionMacDirect, false, false},
		{"macos", session.ClientDistributionAppStore, true, true},
		// a Mac app from before the apps reported their distribution is the
		// App Store build, as all of them were taken to be
		{"macos", "", true, true},
		{"ios", session.ClientDistributionAppStore, true, true},
		{"ios", "", true, true},
		{"android", session.ClientDistributionPlay, false, true},
		{"android", "", false, true},
		// the flavors outside Play bill through Stripe
		{"android", session.ClientDistributionAndroidDirect, false, false},
		{"android", session.ClientDistributionSolanaDapp, false, false},
		{"android", session.ClientDistributionEthosDapp, false, false},
		{"windows", session.ClientDistributionWindows, false, false},
		{"linux", session.ClientDistributionLinuxAppimage, false, false},
		{"linux", "", false, false},
		{"web", session.ClientDistributionWeb, false, false},
		{"web", session.ClientDistributionExtensionChrome, false, false},
		{"", "", false, false},
	} {
		if got := AppStorePlatform(c.platform, c.distribution); got != c.appStore {
			t.Errorf("%q/%q: the App Store bills %t, want %t", c.platform, c.distribution, got, c.appStore)
		}
		if got := StoreBilledPlatform(c.platform, c.distribution); got != c.storeBilled {
			t.Errorf("%q/%q: store billed %t, want %t", c.platform, c.distribution, got, c.storeBilled)
		}
	}
}

// The App Store's offer rule (no offer email on path B while the App Store
// codes do not flow) holds the App Store Mac and the iPhone; the
// direct-download Mac gets the path B offers, as the web and the other
// Stripe-billed apps do.
func TestOfferEmailWithheldByDistribution(t *testing.T) {
	for _, c := range []struct {
		offerByCountry    bool
		platform          string
		distribution      string
		path              string
		appleEmailEnabled bool
		want              bool
	}{
		{true, "macos", session.ClientDistributionMacDirect, PathB, false, false},
		{true, "macos", session.ClientDistributionAppStore, PathB, false, true},
		{true, "macos", "", PathB, false, true},
		{true, "ios", session.ClientDistributionAppStore, PathB, false, true},
		{true, "android", session.ClientDistributionPlay, PathB, false, false},
		{true, "web", "", PathB, false, false},
		// the codes flow, or the network saw the in-app offer
		{true, "macos", session.ClientDistributionAppStore, PathB, true, false},
		{true, "macos", session.ClientDistributionAppStore, PathA, false, false},
		// a country without offers by email holds every platform
		{false, "macos", session.ClientDistributionMacDirect, PathB, false, true},
		{false, "windows", session.ClientDistributionWindows, PathA, true, true},
	} {
		if got := OfferEmailWithheld(c.offerByCountry, c.platform, c.distribution, c.path, c.appleEmailEnabled); got != c.want {
			t.Errorf("%+v: withheld %t", c, got)
		}
	}
}

// The direct-download Mac has no widget to point E2 at (its URnetworkDirect
// target embeds no widgets extension): it gets the desktop email, the app
// started with the computer, as Windows and Linux do. The App Store Mac, and
// a Mac app of unknown distribution, keep the widget email.
func TestE2DirectMacGetsTheDesktopEmail(t *testing.T) {
	for _, c := range []struct {
		distribution string
		widgetAdded  bool
		template     bool
		want         string
	}{
		{session.ClientDistributionMacDirect, false, true, VariantDesktop},
		// the desktop email does not wait for a widget the build lacks
		{session.ClientDistributionMacDirect, true, true, VariantDesktop},
		// not uploaded yet: skipped, as for Windows and Linux
		{session.ClientDistributionMacDirect, false, false, ""},
		{session.ClientDistributionAppStore, false, true, VariantActivated},
		{session.ClientDistributionAppStore, true, true, ""},
		{"", false, true, VariantActivated},
	} {
		f := healthy(PathB)
		f.Platform = "macos"
		f.Distribution = c.distribution
		f.HasConnected = true
		f.WidgetAdded = c.widgetAdded
		f.DesktopTemplate = c.template
		d := Decide(StepE2, f)
		if c.want == "" {
			if d.Action != ActionSkip || d.NextStep != StepE3 {
				t.Errorf("e2 on macos/%q (widget %t, desktop template %t): %+v, want a skip", c.distribution, c.widgetAdded, c.template, d)
			}
		} else if d.Action != ActionSend || d.Template != TemplateE2Widget || d.Variant != c.want || d.NextStep != StepE3 {
			t.Errorf("e2 on macos/%q (widget %t, desktop template %t): %+v, want %s", c.distribution, c.widgetAdded, c.template, d, c.want)
		}
	}
	for _, platform := range []string{"ios", "android", ""} {
		if !WidgetPlatform(platform, "") {
			t.Errorf("%q has a widget", platform)
		}
	}
	for _, platform := range []string{"windows", "linux", PlatformWeb} {
		if WidgetPlatform(platform, "") {
			t.Errorf("%q has no widget", platform)
		}
	}
}
