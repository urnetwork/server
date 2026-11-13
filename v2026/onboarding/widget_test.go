// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"testing"

	"github.com/urnetwork/server/v2026/session"
)

// E2's widget email went to every phone whatever its build, until the
// 2026-11-13 review: the App Store's 2026.8.17 has no widgets, so iPhone
// readers were told to add one. A build has the widget only on a phone, in a
// platform and distribution with a first version, on that version or later.
func TestWidgetSettingsHasWidget(t *testing.T) {
	s := DefaultWidgetSettings()
	for _, c := range []struct {
		platform     string
		distribution string
		appVersion   string
		want         bool
	}{
		// every Android flavor from the first build with the widgets
		{"android", session.ClientDistributionPlay, "2026.10.15-1072765730", true},
		{"android", session.ClientDistributionPlay, "2026.9.3-1036629210", true},
		{"android", session.ClientDistributionPlay, "2026.9.2", false},
		{"android", session.ClientDistributionPlay, "2026.8.31-1035000000", false},
		{"android", session.ClientDistributionAndroidDirect, "2026.10.30", true},
		{"android", session.ClientDistributionSolanaDapp, "2026.11.1", true},
		{"android", session.ClientDistributionEthosDapp, "2026.9.1", false},
		// a build from before the distribution field
		{"android", "", "2026.9.17-1040000000", true},
		{"android", "", "2026.7.22", false},
		// a version the server never recorded, or not a calendar release
		{"android", session.ClientDistributionPlay, "", false},
		{"android", session.ClientDistributionPlay, "beta", false},
		// no App Store release has the widgets yet
		{"ios", session.ClientDistributionAppStore, "2026.8.17", false},
		{"ios", session.ClientDistributionAppStore, "2026.11.12", false},
		{"ios", "", "2026.11.12", false},
		// a Mac has no home screen, in either distribution
		{"macos", session.ClientDistributionAppStore, "2026.11.12", false},
		{"macos", session.ClientDistributionMacDirect, "2026.11.12", false},
		{"windows", session.ClientDistributionWindows, "2026.11.12", false},
		{"web", session.ClientDistributionWeb, "2026.11.12", false},
		{"", "", "2026.11.12", false},
	} {
		if got := s.HasWidget(c.platform, c.distribution, c.appVersion); got != c.want {
			t.Errorf("HasWidget(%q, %q, %q) = %t, want %t", c.platform, c.distribution, c.appVersion, got, c.want)
		}
	}
	// a release record for the App Store opens iOS, and a Mac stays out
	s.MinVersions["ios/"+session.ClientDistributionAppStore] = "2026.11.20"
	s.MinVersions["macos/"+session.ClientDistributionAppStore] = "2026.11.20"
	if !s.HasWidget("ios", session.ClientDistributionAppStore, "2026.11.20") || s.HasWidget("ios", session.ClientDistributionAppStore, "2026.11.19") {
		t.Error("ios at its first App Store version")
	}
	if s.HasWidget("macos", session.ClientDistributionAppStore, "2026.12.1") {
		t.Error("a Mac is never a widget platform")
	}
	// each call returns its own settings
	if _, ok := DefaultWidgetSettings().MinVersions["ios/"+session.ClientDistributionAppStore]; ok {
		t.Error("DefaultWidgetSettings shares its map")
	}
}

func TestAppVersionAtLeast(t *testing.T) {
	for _, c := range []struct {
		appVersion string
		minVersion string
		want       bool
	}{
		{"2026.9.3", "2026.9.3", true},
		{"2026.9.10", "2026.9.3", true},
		{"2026.10.1", "2026.9.30", true},
		{"2027.1.1", "2026.12.31", true},
		{"2026.9.2", "2026.9.3", false},
		{"2025.12.31", "2026.1.1", false},
		{"v2026.10.15-1072765730", "2026.9.3", true},
		{"2026.10.15+build.7", "2026.10.15", true},
		{" 2026.10.15 ", "2026.10.15", true},
		{"2026.10", "2026.9.3", false},
		{"2026.10.15.1", "2026.9.3", false},
		{"2026.x.15", "2026.9.3", false},
		{"", "2026.9.3", false},
		{"2026.10.15", "", false},
		{"2026.10.15", "soon", false},
	} {
		if got := AppVersionAtLeast(c.appVersion, c.minVersion); got != c.want {
			t.Errorf("AppVersionAtLeast(%q, %q) = %t, want %t", c.appVersion, c.minVersion, got, c.want)
		}
	}
}

// The desktop email's browser branch follows the platform E2 decided on, so a
// web reader resolved from the newest client gets the extension's branch.
func TestE2DesktopDecisionCarriesTheBrowserBranch(t *testing.T) {
	for platform, browser := range map[string]bool{"web": true, "windows": false, "linux": false, "macos": false} {
		f := healthy(PathB)
		f.Platform = platform
		f.HasConnected = true
		f.WidgetAdded = true
		f.DesktopTemplate = true
		d := Decide(StepE2, f)
		if d.Action != ActionSend || d.Variant != VariantDesktop || d.Browser != browser {
			t.Errorf("e2 on %q: %+v, want the desktop email with browser %t", platform, d, browser)
		}
	}
	f := healthy(PathB)
	f.Platform = "android"
	f.WidgetBuild = true
	f.HasConnected = true
	if d := Decide(StepE2, f); d.Variant != VariantActivated || d.Browser {
		t.Errorf("e2 on android: %+v", d)
	}
}
