// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"testing"

	"github.com/urnetwork/server/model"
)

// 2026-11-12 on Main: 451 of the 509 App Store one-time offer codes drawn
// went to Android and web networks, which can never redeem one. Only a reader
// on an App Store build takes a code: the iOS app, and the Mac App Store
// build, which is the same App Store app with the same subscriptions; never
// the direct-download Mac (Stripe), Android, Windows, Linux or a browser. An
// app that declares no platform, its sdk older than X-UR-ClientInfo, is known
// by its client's device spec, of unknown distribution.
func TestAppStoreReaderIsOnlyAnAppStoreBuild(t *testing.T) {
	cases := []struct {
		platform     string
		distribution string
		deviceSpec   string
		want         bool
	}{
		{platform: "ios", distribution: "app_store", want: true},
		// an iOS app from before the distribution field
		{platform: "ios", distribution: "", want: true},
		{platform: "macos", distribution: "app_store", want: true},
		// a Mac app from before the distribution field is taken for the App
		// Store build, as the campaign rules take it
		{platform: "macos", distribution: "", want: true},
		{platform: "macos", distribution: "mac_direct", want: false},
		{platform: "android", distribution: "play", want: false},
		{platform: "android", distribution: "", want: false},
		{platform: "android", distribution: "android_direct", want: false},
		{platform: "android", distribution: "solana_dapp", want: false},
		{platform: "web", distribution: "web", want: false},
		{platform: "web", distribution: "extension_chrome", want: false},
		{platform: "windows", distribution: "windows", want: false},
		{platform: "linux", distribution: "linux_flatpak", want: false},
		// a declared platform decides over a spec that names another
		{platform: "android", distribution: "play", deviceSpec: "18.1 iphone 16 pro", want: false},
		{platform: "macos", distribution: "mac_direct", deviceSpec: "18.1 iphone 16 pro", want: false},
		// no declared platform: the client's device spec
		{deviceSpec: "18.1 iphone 16 pro", want: true},
		{deviceSpec: "17.6 ipad pro (m4)", want: true},
		{deviceSpec: "15.7.9 macbook air (m1, 2020)", want: true},
		{deviceSpec: "16 samsung galaxy s24 ultra", want: false},
		{deviceSpec: "14 synthetic/example/device:14/up1a.231005.007/1:user/release-keys", want: false},
		{deviceSpec: "urnetwork windows amd64", want: false},
		{deviceSpec: "socks5", want: false},
		{deviceSpec: "", want: false},
	}
	for _, c := range cases {
		deviceSpecReads := 0
		got := appStoreReaderOf(c.platform, c.distribution, func() string {
			deviceSpecReads += 1
			return c.deviceSpec
		})
		if got != c.want {
			t.Errorf("platform %q distribution %q device spec %q: App Store reader %t, want %t", c.platform, c.distribution, c.deviceSpec, got, c.want)
		}
		// the device spec is a database read: only for an app that declares
		// no platform
		if wantReads := map[bool]int{true: 0, false: 1}[c.platform != ""]; deviceSpecReads != wantReads {
			t.Errorf("platform %q: read the device spec %d times, want %d", c.platform, deviceSpecReads, wantReads)
		}
	}
	if appStoreReader(nil) {
		t.Error("no session is an App Store reader")
	}
}

// The draw outcome an offer shows: a one-time code, the custom code, or none.
func TestAppleOfferCodeResultNamesTheCode(t *testing.T) {
	code := func(value string) *model.OnboardingOffer {
		return &model.OnboardingOffer{AppleOfferCode: &value}
	}
	cases := []struct {
		offer      *model.OnboardingOffer
		customCode string
		want       string
	}{
		{offer: nil, want: appleOfferCodeResultEmpty},
		{offer: &model.OnboardingOffer{}, customCode: "SYNTHETIC-CUSTOM", want: appleOfferCodeResultEmpty},
		{offer: code("  "), want: appleOfferCodeResultEmpty},
		{offer: code("SYNTHETIC-ONE-TIME"), want: appleOfferCodeResultDrawn},
		{offer: code("SYNTHETIC-ONE-TIME"), customCode: "SYNTHETIC-CUSTOM", want: appleOfferCodeResultDrawn},
		{offer: code("SYNTHETIC-CUSTOM"), customCode: "SYNTHETIC-CUSTOM", want: appleOfferCodeResultCustom},
	}
	for i, c := range cases {
		if got := appleOfferCodeResult(c.offer, c.customCode); got != c.want {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
}
