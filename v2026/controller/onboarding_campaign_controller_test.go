// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
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
