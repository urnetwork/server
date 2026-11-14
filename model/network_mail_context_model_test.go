// SPDX-License-Identifier: MPL-2.0

package model

import (
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/onboarding"
)

// A report keeps the newest values; an unchanged one rewrites nothing until a
// value's time is a refresh interval old; an empty part keeps the stored one.
func TestMergeNetworkMailContext(t *testing.T) {
	networkId := server.NewId()
	settings := onboarding.DefaultMailContextSettings()
	settings.MaxClients = 2
	settings.MaxLanguages = 3
	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

	if _, changed := MergeNetworkMailContext(nil, networkId, &NetworkMailContextReport{}, t0, settings); changed {
		t.Fatal("an empty report wrote a row")
	}

	first, changed := MergeNetworkMailContext(nil, networkId, &NetworkMailContextReport{
		TimeZone:  "America/Chicago",
		Languages: []string{"es-MX", "en-US", "fr", "de"},
		Client:    &NetworkMailClient{Platform: "android", Distribution: "play", AppVersion: "2026.10.15"},
	}, t0, settings)
	if !changed || first.TimeZone != "America/Chicago" || !slices.Equal(first.Languages, []string{"es-MX", "en-US", "fr"}) {
		t.Fatalf("first report: %+v", first)
	}
	if len(first.Clients) != 1 || !first.Clients[0].Time.Equal(t0) || !first.UpdateTime.Equal(t0) {
		t.Fatalf("first client: %+v", first)
	}

	// the same report an hour later writes nothing
	if _, changed := MergeNetworkMailContext(first, networkId, &NetworkMailContextReport{
		TimeZone:  "America/Chicago",
		Languages: []string{"es-MX", "en-US", "fr"},
		Client:    &NetworkMailClient{Platform: "android", Distribution: "play", AppVersion: "2026.10.15"},
	}, t0.Add(time.Hour), settings); changed {
		t.Error("an unchanged report within the refresh interval rewrote the row")
	}

	// a day later it renews the times
	t1 := t0.Add(settings.RefreshInterval)
	renewed, changed := MergeNetworkMailContext(first, networkId, &NetworkMailContextReport{TimeZone: "America/Chicago"}, t1, settings)
	if !changed || !renewed.TimeZoneTime.Equal(t1) || !renewed.LanguagesTime.Equal(t0) || !slices.Equal(renewed.Languages, first.Languages) {
		t.Errorf("renewal: %+v", renewed)
	}

	// a new zone replaces the old at once; a report without languages keeps
	// the list; a new build of the same app replaces its entry, another app
	// goes first, and the oldest falls off past the bound
	t2 := t0.Add(2 * time.Hour)
	next, changed := MergeNetworkMailContext(first, networkId, &NetworkMailContextReport{
		TimeZone: "Asia/Tokyo",
		Client:   &NetworkMailClient{Platform: "android", Distribution: "play", AppVersion: "2026.11.13"},
	}, t2, settings)
	if !changed || next.TimeZone != "Asia/Tokyo" || !next.TimeZoneTime.Equal(t2) || !slices.Equal(next.Languages, first.Languages) {
		t.Fatalf("new zone: %+v", next)
	}
	if len(next.Clients) != 1 || next.Clients[0].AppVersion != "2026.11.13" {
		t.Fatalf("new build: %+v", next.Clients)
	}
	next, _ = MergeNetworkMailContext(next, networkId, &NetworkMailContextReport{Client: &NetworkMailClient{Platform: "windows", Distribution: "windows", AppVersion: "2026.11.1"}}, t2.Add(time.Minute), settings)
	next, _ = MergeNetworkMailContext(next, networkId, &NetworkMailContextReport{Client: &NetworkMailClient{Platform: "web", Distribution: "web"}}, t2.Add(2*time.Minute), settings)
	platforms := []string{}
	for _, client := range next.Clients {
		platforms = append(platforms, client.Platform)
	}
	if !slices.Equal(platforms, []string{"web", "windows"}) {
		t.Errorf("clients %v, want the newest two", platforms)
	}
	// the stored row is not the caller's to change
	if first.TimeZone != "America/Chicago" || len(first.Clients) != 1 {
		t.Error("the merge changed the current row")
	}
}

// The row round-trips through PostgreSQL, merges reports in turn, and leaves
// with its network.
func TestNetworkMailContextStore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "mail-context", userId)
		settings := onboarding.DefaultMailContextSettings()
		now := server.NowUtc().Truncate(time.Microsecond)

		if GetNetworkMailContext(ctx, networkId) != nil {
			t.Fatal("a row before any report")
		}
		if !RecordNetworkMailContext(ctx, networkId, &NetworkMailContextReport{
			TimeZone:  "Asia/Seoul",
			Languages: []string{"ko-KR", "en"},
			Client:    &NetworkMailClient{Platform: "ios", Distribution: "app_store", AppVersion: "2026.8.17"},
		}, now, settings) {
			t.Fatal("the first report wrote nothing")
		}
		if RecordNetworkMailContext(ctx, networkId, &NetworkMailContextReport{TimeZone: "Asia/Seoul"}, now.Add(time.Minute), settings) {
			t.Error("an unchanged zone rewrote the row")
		}
		if !RecordNetworkMailContext(ctx, networkId, &NetworkMailContextReport{Languages: []string{"ja-JP"}}, now.Add(2*time.Minute), settings) {
			t.Error("a new language list wrote nothing")
		}
		row := GetNetworkMailContext(ctx, networkId)
		if row == nil || row.TimeZone != "Asia/Seoul" || !slices.Equal(row.Languages, []string{"ja-JP"}) {
			t.Fatalf("row %+v", row)
		}
		if len(row.Clients) != 1 || row.Clients[0].Platform != "ios" || row.Clients[0].Distribution != "app_store" || row.Clients[0].AppVersion != "2026.8.17" {
			t.Fatalf("clients %+v", row.Clients)
		}
		if row.TimeZoneTime == nil || !row.TimeZoneTime.Equal(now) || !row.UpdateTime.Equal(now.Add(2*time.Minute)) {
			t.Errorf("times %v %v", row.TimeZoneTime, row.UpdateTime)
		}

		if outcome, _ := RemoveNetworkWithRemovals(ctx, networkId, nil, &RemoveNetworkStoreSnapshot{}, nil); outcome != RemoveNetworkRemoved {
			t.Fatalf("removal %q", outcome)
		}
		if GetNetworkMailContext(ctx, networkId) != nil {
			t.Error("the mail context outlived its network")
		}
	})
}

// mail_language lays the owner's overrides and thresholds over the defaults;
// an entry that is no country or no language is ignored.
func TestEffectiveMailLanguageSettings(t *testing.T) {
	c := &OnboardingConfig{MailLanguage: OnboardingMailLanguageConfig{
		CountryOverrides:          map[string]string{"ua": "uk", "KZ": "ru", "USA": "en", "BY": "!!"},
		CountryLanguageMinPercent: 75,
	}}
	s := c.EffectiveMailLanguageSettings()
	if s.CountryOverrides["UA"] != "uk" || s.CountryOverrides["KZ"] != "ru" || len(s.CountryOverrides) != 2 {
		t.Errorf("overrides %v", s.CountryOverrides)
	}
	if s.CountryLanguageMinPercent != 75 || s.CountryOtherLanguageMaxPercent != onboarding.DefaultMailLanguageSettings().CountryOtherLanguageMaxPercent {
		t.Errorf("thresholds %v %v", s.CountryLanguageMinPercent, s.CountryOtherLanguageMaxPercent)
	}
	if got, _ := onboarding.CountryLanguage("UA", onboarding.EmailLocales, s); got != "uk" {
		t.Errorf("UA with the owner's override reads %q", got)
	}
	if got, _ := onboarding.CountryLanguage("UA", onboarding.EmailLocales, (&OnboardingConfig{}).EffectiveMailLanguageSettings()); got != "" {
		t.Errorf("UA without an override reads %q", got)
	}
}

// A reader's languages pick the template among the locales the variant has:
// the first one served, then the country, then English; a variant without
// its own templates reads the default variant's.
func TestTemplateIdForReader(t *testing.T) {
	c := &OnboardingBrevoConfig{Templates: map[string]map[string]map[string]int{
		"r1_news": {
			"lapsed": {"en": 10, "ja": 11, "ko": 12, "zh-HK": 13, "zh": 14, "es-419": 15, "es": 16, "de": 0},
		},
	}}
	settings := onboarding.DefaultMailLanguageSettings()
	for _, tc := range []struct {
		name      string
		variant   string
		languages onboarding.MailLanguages
		id        int
		locale    string
		source    string
	}{
		{"Japan by country", "lapsed", onboarding.MailLanguages{Country: "JP", CountrySource: "country"}, 11, "ja", "country"},
		{"Korea by country", "lapsed", onboarding.MailLanguages{Country: "KR", CountrySource: "country"}, 12, "ko", "country"},
		{"Taiwan reads Traditional", "lapsed", onboarding.MailLanguages{Candidates: []onboarding.LanguageCandidate{{Tag: "zh-TW", Source: "client"}}}, 13, "zh-HK", "client"},
		{"Argentina reads es-419", "lapsed", onboarding.MailLanguages{Candidates: []onboarding.LanguageCandidate{{Tag: "es-AR", Source: "onboarding"}}}, 15, "es-419", "onboarding"},
		{"an unuploaded locale is none", "lapsed", onboarding.MailLanguages{Candidates: []onboarding.LanguageCandidate{{Tag: "de-DE", Source: "client"}}}, 10, "en", ""},
		{"the default variant", "other", onboarding.MailLanguages{Country: "JP", CountrySource: "country"}, 0, "", ""},
	} {
		id, locale, source, ok := c.TemplateIdForReader("r1_news", tc.variant, tc.languages, settings)
		if tc.id == 0 {
			if ok {
				t.Errorf("%s: %d %q", tc.name, id, locale)
			}
			continue
		}
		if !ok || id != tc.id || locale != tc.locale || source != tc.source {
			t.Errorf("%s: %d %q from %q (%t), want %d %q from %q", tc.name, id, locale, source, ok, tc.id, tc.locale, tc.source)
		}
	}
}
