// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"testing"
	"time"
)

// A sign-up's request makes it likely bot when a farm's user agent sent it, or
// when a browser sent it without an Accept-Language header; an app's sign-up
// (its own user agent, no language on iOS) and a browser's with a language are
// likely human.
func TestSignupCohortFromTheRequest(t *testing.T) {
	settings := DefaultSignupCohortSettings()
	for _, c := range []struct {
		name           string
		userAgent      string
		acceptLanguage string
		cohort         string
		reason         string
	}{
		{"the farm agent", "urnet/1.1 (+https://farm.example/ur-network-farm)", "en-US", CohortBot, CohortReasonFarmUserAgent},
		{"the farm mark anywhere", "Mozilla/5.0 reformboss-agent", "en-US", CohortBot, CohortReasonFarmUserAgent},
		{"synx", "synx/1.0", "", CohortBot, CohortReasonFarmUserAgent},
		{"urnet-safe", "URNET-SAFE/1.0", "ja", CohortBot, CohortReasonFarmUserAgent},
		{"a browser without a language", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/140.0", "", CohortBot, CohortReasonNoAcceptLanguage},
		{"a phone browser without a language", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X)", " ", CohortBot, CohortReasonNoAcceptLanguage},
		{"a browser with a language", "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_1) Safari/605.1.15", "de-DE,de;q=0.9", CohortHuman, ""},
		{"the iOS app, which sends no language", "URnetwork/2026.8.17 CFNetwork/3826.400 Darwin/25.1.0", "", CohortHuman, ""},
		{"the Android app", "okhttp/4.12.0", "ru-RU", CohortHuman, ""},
		{"our own name is not the farm's", "urnetwork/2026.11.1", "", CohortHuman, ""},
		{"no user agent", "", "", CohortHuman, ""},
	} {
		cohort, reason := SignupCohort(c.userAgent, c.acceptLanguage, settings)
		if cohort != c.cohort || reason != c.reason {
			t.Errorf("%s: %s %q, want %s %q", c.name, cohort, reason, c.cohort, c.reason)
		}
	}
}

// A burst is BurstThreshold or more email sign-ups in one UTC hour with the
// same platform, language and country: every one of them is likely bot, the
// first included. Other hours, signatures and sign-ups without an email don't
// count toward it.
func TestSignatureBursts(t *testing.T) {
	settings := DefaultSignupCohortSettings()
	hour := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	signups := []CohortSignup{}
	burst := func(n int, at time.Time, platform string, locale string, country string, email bool) {
		for i := 0; i < n; i++ {
			signups = append(signups, CohortSignup{Email: email, CreatedAt: at.Add(time.Duration(i) * time.Minute), Platform: platform, Locale: locale, Country: country})
		}
	}
	burst(settings.BurstThreshold, hour, PlatformWeb, "", "JP", true)  // 0..9: a burst
	burst(settings.BurstThreshold-1, hour, "ios", "", "US", true)      // under the threshold
	burst(5, hour, "android", "zh-CN", "US", true)                     // half in this hour
	burst(5, hour.Add(time.Hour), "android", "zh-CN", "US", true)      // half in the next
	burst(settings.BurstThreshold+5, hour, "android", "", "KR", false) // no email: not counted
	bursts := SignatureBursts(signups, settings)
	for i, s := range signups {
		want := i < settings.BurstThreshold
		if bursts[i] != want {
			t.Errorf("sign-up %d (%s %q %s email=%t at %s): burst %t, want %t", i, s.Platform, s.Locale, s.Country, s.Email, s.CreatedAt.Format(time.Kitchen), bursts[i], want)
		}
	}
}

// A burst or a likely-bot request makes the sign-up likely bot; a likely-human
// request without a burst, likely human. A sign-up from before the request
// labels is likely bot only when a browser signed up without a language, and
// otherwise unknown.
func TestCohortPrecedence(t *testing.T) {
	for _, c := range []struct {
		name   string
		signup CohortSignup
		burst  bool
		cohort string
	}{
		{"human request", CohortSignup{RequestCohort: CohortHuman, Platform: "ios"}, false, CohortHuman},
		{"a burst outranks a human request", CohortSignup{RequestCohort: CohortHuman, Platform: "ios"}, true, CohortBot},
		{"bot request", CohortSignup{RequestCohort: CohortBot, Platform: "android", Locale: "zh-CN"}, false, CohortBot},
		{"unlabeled browser without a language", CohortSignup{Platform: PlatformWeb}, false, CohortBot},
		{"unlabeled browser with a language", CohortSignup{Platform: PlatformWeb, Locale: "de-DE"}, false, CohortUnknown},
		{"unlabeled app", CohortSignup{Platform: "ios"}, false, CohortUnknown},
		{"unlabeled burst", CohortSignup{Platform: "android"}, true, CohortBot},
	} {
		if got := Cohort(c.signup, c.burst); got != c.cohort {
			t.Errorf("%s: %s, want %s", c.name, got, c.cohort)
		}
	}
	if CohortExperimentId("email_sequence_2", CohortHuman) != "email_sequence_2@human" {
		t.Error("cohort experiment id")
	}
}

// The crypto line goes only in the card branch of the offer emails, and only
// for the crypto arm: a store-billed reader never sees it.
func TestOfferParamsCryptoLine(t *testing.T) {
	expires := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	for _, template := range []string{TemplateE3LastChance, TemplateE4bOffer} {
		for _, c := range []struct {
			crypto, storeBilled bool
			want                string
		}{
			{true, false, "1"},
			{false, false, ""},
			{true, true, ""},
			{false, true, ""},
		} {
			p := BuildParams(ParamsInput{Template: template, Variant: VariantDefault, CryptoLine: c.crypto, StoreBilled: c.storeBilled,
				OfferExpiresAt: &expires, OfferPriceUsd: 30, RegularYearUsd: 40, Location: time.UTC, Token: "synthetic", SiteUrl: "https://ur.example"})
			if got, ok := p["crypto_line"]; !ok || got != c.want {
				t.Errorf("%s crypto=%t store=%t: crypto_line %q (present %t), want %q", template, c.crypto, c.storeBilled, got, ok, c.want)
			}
		}
	}
}
