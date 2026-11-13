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

// E2's activated email points at the widget on the home screen and the lock
// screen, which a Mac has in neither distribution: the App Store Mac got it
// until the 2026-11-13 review, and so did a reader of unknown platform. A Mac
// gets the desktop email now, the app started with the computer ("Launch
// URnetwork on system startup" is in its Settings), as Windows and Linux do;
// an unknown platform gets neither.
func TestE2MacGetsTheDesktopEmail(t *testing.T) {
	for _, c := range []struct {
		platform    string
		widgetAdded bool
		template    bool
		want        string
	}{
		{"macos", false, true, VariantDesktop},
		// the desktop email does not wait for a widget
		{"macos", true, true, VariantDesktop},
		// not uploaded yet: skipped, as for Windows and Linux
		{"macos", false, false, ""},
		// an unknown platform: the widget copy would be wrong on a computer
		// and the desktop copy on a phone
		{"", false, true, ""},
		{"", false, false, ""},
		{"", true, true, ""},
	} {
		f := healthy(PathB)
		f.Platform = c.platform
		// even a build with the widget: a Mac has no home screen
		f.WidgetBuild = true
		f.HasConnected = true
		f.WidgetAdded = c.widgetAdded
		f.DesktopTemplate = c.template
		d := Decide(StepE2, f)
		if c.want == "" {
			if d.Action != ActionSkip || d.NextStep != StepE3 {
				t.Errorf("e2 on %q (widget %t, desktop template %t): %+v, want a skip", c.platform, c.widgetAdded, c.template, d)
			}
		} else if d.Action != ActionSend || d.Template != TemplateE2Widget || d.Variant != c.want || d.NextStep != StepE3 {
			t.Errorf("e2 on %q (widget %t, desktop template %t): %+v, want %s", c.platform, c.widgetAdded, c.template, d, c.want)
		}
		// a reader who never connected gets the connect nudge, which names
		// no widget, on every platform
		f.HasConnected = false
		if d := Decide(StepE2, f); d.Action != ActionSend || d.Variant != VariantNotActivated {
			t.Errorf("e2 not activated on %q: %+v", c.platform, d)
		}
	}
	for _, platform := range []string{"ios", "android"} {
		if !WidgetPlatform(platform) || DesktopPlatform(platform) {
			t.Errorf("%q is a widget platform", platform)
		}
	}
	for _, platform := range []string{"macos", "windows", "linux", PlatformWeb} {
		if WidgetPlatform(platform) || !DesktopPlatform(platform) {
			t.Errorf("%q is a desktop platform", platform)
		}
	}
	if WidgetPlatform("") || DesktopPlatform("") {
		t.Errorf("an unknown platform is neither")
	}
}
