// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

func TestNormalizeLocale(t *testing.T) {
	cases := map[string]string{
		"de-DE": "de-DE", "de_de": "de-DE", "EN": "en", "zh-Hant-HK": "zh-HK", "es-419": "es-419",
		"": "", "*": "", "1x": "", "toolongtoolongtoolongtoolongtoolong-XX": "", "pt-br": "pt-BR",
	}
	for in, want := range cases {
		if got := normalizeLocale(in); got != want {
			t.Errorf("normalizeLocale(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlatformFromDeviceSpec(t *testing.T) {
	cases := map[string]string{
		"iPhone15,2 iOS 26.0": "ios", "iPad": "ios", "Pixel 8 Android 15": "android", "macOS 26.0 arm64": "macos",
		"Windows 11": "windows", "Ubuntu 24.04": "linux", "Mozilla/5.0 (X11; Linux x86_64) Chrome": "linux",
		"Mozilla/5.0 Chrome/140": "web", "": "", "toaster": "",
	}
	for in, want := range cases {
		if got := platformFromDeviceSpec(in); got != want {
			t.Errorf("platformFromDeviceSpec(%q) = %q, want %q", in, got, want)
		}
	}
	if platformDeviceName("ios") != "iPhone" || platformDeviceName("") != "device" {
		t.Error("device names")
	}
}

// The specs the apps actually send on auth-client (device_spec on Main,
// 2026-11-10). The Android app sends "<os version> <make model>", and sent
// "<os version> <Build.FINGERPRINT>" before; neither names Android, so every
// Android network was left without a platform: its onboarding mail rendered
// "your device" and the web-billed branch with USD prices (store_billed off),
// though Google Play bills it. 1,832 Android networks got 5,465 onboarding
// sends in the 30 days to 2026-11-10 that way, 1,526 of them an offer email.
func TestPlatformFromDeviceSpecNamesTheAppsSpecs(t *testing.T) {
	cases := map[string]string{
		// Android, "<os version> <make model>" (android MainApplication.deviceSpec)
		"16 Samsung Galaxy S24 Ultra": "android",
		"17.1 Pixel 8 Pro":            "android",
		"10 Samsung SM-A125F":         "android",
		"8.1.0 Xiaomi Redmi 5 Plus":   "android",
		// Android, "<os version> <Build.FINGERPRINT>" (older app versions)
		"14 TECNO/LH7n-RU/TECNO-LH7n:14/UP1A.231005.007/250416V782:user/release-keys":                      "android",
		"11 Droidlogic/franklin/franklin:11/RD2A.211001.002/eng.ugoos.20231103.070631:userdebug/test-keys": "android",
		// a fingerprint whose build id contains "ios" is still Android
		"15 nubia/PQ82A04-UN/PQ82A04:15/AQ3A.240812.002/NebulaAIOS1.0.3_NX725J_GB:user/release-keys": "android",
		// iOS, "<os version> <model>" and the bare machine id
		"18.1 iPhone 16 Pro": "ios",
		"17.1 iPhone":        "ios",
		"iPhone14,5":         "ios",
		"iPad11,1":           "ios",
		// the other apps and the automation keep their answers
		"windows amd64":  "windows",
		"Windows 10 x64": "windows",
		"linux amd64":    "linux",
		"linux/amd64":    "linux",
		"macos":          "macos",
		"macOS Sonoma 14.6.1 (23G93) | Apple M3 8-Core | 24GB Unified Memory | 512GB Storage": "macos",
		"socks5":                  "",
		"resident":                "",
		"urnetwork/egress-prober": "",
		// a Mac model with only a version still names no platform: the App
		// Store and the direct-download build send the same spec, and the
		// platform decides the App Store offer rule (NoOfferEmail)
		"15.7.9 MacBook Air (M1, 2020)": "",
	}
	for in, want := range cases {
		if got := platformFromDeviceSpec(in); got != want {
			t.Errorf("platformFromDeviceSpec(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebhookTags(t *testing.T) {
	if tags := webhookTags(&BrevoWebhookArgs{Tags: []string{"onboarding", "e1_connect", "default"}}); len(tags) != 3 {
		t.Errorf("array form: %v", tags)
	}
	if tags := webhookTags(&BrevoWebhookArgs{Tag: `["onboarding","e2_widget","activated"]`}); len(tags) != 3 || tags[1] != "e2_widget" {
		t.Errorf("legacy json string form: %v", tags)
	}
	if tags := webhookTags(&BrevoWebhookArgs{Tag: "plain"}); len(tags) != 1 || tags[0] != "plain" {
		t.Errorf("plain string form: %v", tags)
	}
	if tags := webhookTags(&BrevoWebhookArgs{}); tags != nil {
		t.Errorf("none: %v", tags)
	}
}

func TestDecideAppleOfferCodeTopUp(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	cfg := model.OnboardingAppleOfferCodesConfig{OfferCodeId: "abc", BatchSize: 500, MinAvailable: 200, ExpiryDays: 5}
	if d := DecideAppleOfferCodeTopUp(model.OnboardingAppleOfferCodesConfig{}, true, 0, now); d.Generate || d.Reason == "" {
		t.Errorf("no offer code id: %+v", d)
	}
	if d := DecideAppleOfferCodeTopUp(cfg, false, 0, now); d.Generate || d.Reason == "" {
		t.Errorf("no credentials: %+v", d)
	}
	if d := DecideAppleOfferCodeTopUp(cfg, true, 200, now); d.Generate {
		t.Errorf("at the floor: %+v", d)
	}
	d := DecideAppleOfferCodeTopUp(cfg, true, 199, now)
	if !d.Generate || d.BatchSize != 500 || !d.ExpiresAt.Equal(now.Add(5*24*time.Hour)) {
		t.Errorf("below the floor: %+v", d)
	}
	// Apple's minimum batch and the defaults
	d = DecideAppleOfferCodeTopUp(model.OnboardingAppleOfferCodesConfig{OfferCodeId: "abc", BatchSize: 10}, true, 0, now)
	if !d.Generate || d.BatchSize != 500 || !d.ExpiresAt.Equal(now.Add(5*24*time.Hour)) {
		t.Errorf("defaults: %+v", d)
	}
}

func TestParseAppleOfferCodeValues(t *testing.T) {
	expires := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	codes := parseAppleOfferCodeValues("Code\r\nABCDEF\r\nGHIJKL,extra\n\n", expires)
	if len(codes) != 2 || codes[0].Code != "ABCDEF" || codes[1].Code != "GHIJKL" || !codes[1].ExpiresAt.Equal(expires) {
		t.Errorf("values: %+v", codes)
	}
}

func TestOnboardingScheduleFromConfig(t *testing.T) {
	// with no onboarding.yml in the test config the plan's table applies
	s := onboardingSchedule()
	if s.E1Day <= 0 || s.E5bDay <= s.E4bDay {
		t.Errorf("schedule: %+v", s)
	}
	w := onboardingSendWindow()
	if w.StartHour != 8 || w.EndHour != 21 {
		t.Errorf("window: %+v", w)
	}
}
