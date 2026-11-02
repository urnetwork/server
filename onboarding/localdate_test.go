// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"strings"
	"testing"
	"time"
)

func TestLocaleDates(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	berlin, _ := time.LoadLocation("Europe/Berlin")
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	at := time.Date(2026, 9, 14, 22, 5, 0, 0, time.UTC)
	cases := []struct {
		locale string
		loc    *time.Location
		want   string
	}{
		// English is unchanged from FormatLocalDateTime
		{"en", la, "Sep 14, 2026 at 3:05 PM PDT"},
		{"", la, "Sep 14, 2026 at 3:05 PM PDT"},
		{"de", berlin, "15.09.2026, 00:05 CEST"},
		{"de-AT", berlin, "15.09.2026, 00:05 CEST"},
		{"zh", shanghai, "2026年9月15日 06:05 CST"},
		{"zh-TW", shanghai, "2026年9月15日 06:05 CST"},
		{"ja", time.UTC, "2026/09/14 22:05 UTC"},
		{"vi", time.UTC, "22:05 UTC, 14/09/2026"},
		{"pt-BR", time.UTC, "14/09/2026 22:05 UTC"},
		{"xx-YY", time.UTC, "Sep 14, 2026 at 10:05 PM UTC"},
	}
	for _, c := range cases {
		if got := FormatDateTimeLocale(at, c.loc, c.locale); got != c.want {
			t.Errorf("%q: got %q, want %q", c.locale, got, c.want)
		}
	}
	if got := FormatLocalDateTime(at, la); got != FormatDateTimeLocale(at, la, "en") {
		t.Errorf("english helpers disagree: %q", got)
	}
	if got := FormatDateLocale(at, time.UTC, "ko"); got != "2026. 9. 14." {
		t.Errorf("ko date: %q", got)
	}
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if got := FormatDayLocale(at, time.UTC, "en", now); got != "Sep 14" {
		t.Errorf("en day this year: %q", got)
	}
	if got := FormatDayLocale(at.AddDate(-1, 0, 0), time.UTC, "en", now); got != "Sep 14, 2025" {
		t.Errorf("en day last year: %q", got)
	}
	if got := FormatDayLocale(at, time.UTC, "ru", now); got != "14.09.2026" {
		t.Errorf("ru day: %q", got)
	}
	// every email locale has a complete style
	for _, l := range []string{"en", "ar", "cs", "de", "el", "es", "es-419", "es-MX", "fr", "he", "hi", "id", "it", "ja", "ko", "nl", "pl", "pt", "pt-BR", "pt-PT", "ru", "sv", "sw", "th", "uk", "vi", "zh", "zh-HK"} {
		s := styleFor(l)
		if s.date == "" || s.clock == "" || !strings.Contains(s.join, "%[1]s") || !strings.Contains(s.join, "%[2]s") || s.browser == "" || s.device == "" {
			t.Errorf("%s: incomplete style %+v", l, s)
		}
	}
}

func TestDeviceNames(t *testing.T) {
	if DeviceName("ios", "de") != "iPhone" || DeviceName("windows", "ja") != "Windows PC" {
		t.Error("product names stay as they are")
	}
	if DeviceName("web", "en") != "browser" || DeviceName("", "en") != "device" {
		t.Error("english words")
	}
	if DeviceName("web", "ru") != "браузере" || DeviceName("", "zh-HK") != "裝置" {
		t.Errorf("localized words: %q %q", DeviceName("web", "ru"), DeviceName("", "zh-HK"))
	}
}

func TestLandingSlugsAndReasons(t *testing.T) {
	for step, want := range map[string]string{
		TemplateE1Connect: "connect", TemplateE2Widget: "widgets", TemplateE3LastChance: "offer",
		TemplateE4bOffer: "offer", TemplateE4Feedback: "feedback",
		"keep": "keep", "update": "update", "learn": "learn", "winback": "winback", "nonsense": "connect",
	} {
		if got := LandingSlug(step); got != want {
			t.Errorf("slug %q: got %q, want %q", step, got, want)
		}
	}
	for in, want := range map[string]string{"1": "not_needed", "2": "not_working", "3": "trust", "4": "other", "slow": "slow", " other ": "other"} {
		if got := FeedbackReasonCode(in); got != want {
			t.Errorf("reason %q: got %q, want %q", in, got, want)
		}
	}
}
