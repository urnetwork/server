package controller

import (
	"testing"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
)

// E2's widget email is judged by the network's newest active client: its
// platform from the device spec, the app version it last connected with, and
// the row's distribution when the client is on the row's platform. A row
// recorded without a platform takes the client's; with no client, no widget
// build (the 2026-11-13 review: the platform alone sent the widget email to
// App Store iPhones, whose 2026.8.17 build has no widgets).
func TestOnboardingClientBuild(t *testing.T) {
	widgets := onboarding.DefaultWidgetSettings()
	row := func(platform string, distribution string) *model.NetworkOnboarding {
		return &model.NetworkOnboarding{Platform: platform, Distribution: distribution}
	}
	build := func(deviceSpec string, clientVersion string) *model.NetworkClientBuild {
		return &model.NetworkClientBuild{DeviceSpec: deviceSpec, ClientVersion: clientVersion}
	}
	for _, c := range []struct {
		name        string
		row         *model.NetworkOnboarding
		build       *model.NetworkClientBuild
		platform    string
		widgetBuild bool
	}{
		{"play with the widgets", row("android", session.ClientDistributionPlay), build("17.1 Pixel 8 Pro", "2026.10.15-1072765730"), "android", true},
		{"play before the widgets", row("android", session.ClientDistributionPlay), build("17.1 Pixel 8 Pro", "2026.8.31-1035000000"), "android", false},
		{"android of unknown distribution", row("android", ""), build("16 Samsung Galaxy S24 Ultra", "2026.10.15-1072765730"), "android", true},
		{"no version recorded yet", row("android", session.ClientDistributionPlay), build("17.1 Pixel 8 Pro", ""), "android", false},
		{"the App Store's iPhone build", row("ios", session.ClientDistributionAppStore), build("iPhone15,2 iOS 26.0", "2026.8.17"), "ios", false},
		{"a Mac", row("macos", session.ClientDistributionAppStore), build("15.7.9 MacBook Air (M1, 2020)", "2026.8.17"), "macos", false},
		// an unknown row takes the newest client's platform, and its build
		{"unknown row, android client", row("", ""), build("17.1 Pixel 8 Pro", "2026.10.15-1072765730"), "android", true},
		{"unknown row, mac client", row("", ""), build("15.7.9 MacBook Air (M1, 2020)", "2026.11.1"), "macos", false},
		{"unknown row, no client", row("", ""), nil, "", false},
		{"unknown row, a spec that names no platform", row("", ""), build("socks5", "2026.10.15"), "", false},
		// the row's platform stays; the client's build decides the widget
		{"ios row, newest client an android with the widgets", row("ios", session.ClientDistributionAppStore), build("17.1 Pixel 8 Pro", "2026.10.15-1072765730"), "ios", true},
		{"android row, newest client a windows app", row("android", session.ClientDistributionPlay), build("Windows 11", "2026.10.15-1072765730"), "android", false},
	} {
		platform, widgetBuild := onboardingClientBuild(c.row, c.build, widgets)
		if platform != c.platform || widgetBuild != c.widgetBuild {
			t.Errorf("%s: platform %q widget build %t, want %q %t", c.name, platform, widgetBuild, c.platform, c.widgetBuild)
		}
	}
}

// widgets.min_versions lays release records over the defaults, entry by
// entry: the App Store's first release with the widgets opens iOS without a
// server release, and an empty version removes a default.
func TestOnboardingConfigEffectiveWidgetSettings(t *testing.T) {
	c := &model.OnboardingConfig{Widgets: model.OnboardingWidgetsConfig{MinVersions: map[string]string{
		"ios/" + session.ClientDistributionAppStore:        "2026.11.20",
		" android/" + session.ClientDistributionSolanaDapp: " ",
	}}}
	s := c.EffectiveWidgetSettings()
	if !s.HasWidget("ios", session.ClientDistributionAppStore, "2026.11.20") {
		t.Error("the configured App Store release has the widget")
	}
	if s.HasWidget("android", session.ClientDistributionSolanaDapp, "2026.11.20") {
		t.Error("an empty version removes the default")
	}
	if !s.HasWidget("android", session.ClientDistributionPlay, "2026.10.15") {
		t.Error("the other defaults stay")
	}
	if (&model.OnboardingConfig{}).EffectiveWidgetSettings().HasWidget("ios", session.ClientDistributionAppStore, "2026.11.20") {
		t.Error("no App Store release has the widget by default")
	}
}
