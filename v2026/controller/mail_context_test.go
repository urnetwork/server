// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/reengagement"
	"github.com/urnetwork/server/v2026/session"
)

// A device's zone counts only when mail can be placed in it: a zone the time
// package loads that is not UTC, which a server, an emulator or a hardened
// browser reports for a reader whose own zone has a name.
func TestUsableReportedTimeZone(t *testing.T) {
	for _, zone := range []string{"America/Chicago", "Asia/Kolkata", "Atlantic/Reykjavik", "Africa/Abidjan", "Etc/GMT+5", " Asia/Tokyo "} {
		if got := usableReportedTimeZone(zone); got == "" {
			t.Errorf("%q is not usable", zone)
		}
	}
	for _, zone := range []string{"", "UTC", "utc", "Etc/UTC", "Etc/UCT", "GMT", "Etc/GMT", "Etc/GMT+0", "Universal", "Zulu", "Factory", "Local", "Mars/Olympus_Mons", "America/" + string(make([]byte, 64))} {
		if got := usableReportedTimeZone(zone); got != "" {
			t.Errorf("%q is usable as %q", zone, got)
		}
	}
}

// The zone: the device's first, even where the onboarding row has exited and
// a purchase names another country; then the sign-up IP's, kept only where
// no purchase names another country; then the country's, the purchase's
// before the IP's; then the reader's last activity; then nothing.
func TestResolveMailReaderTimeZoneChain(t *testing.T) {
	signUp := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
	exited := signUp.Add(time.Hour)
	closedRow := &model.NetworkOnboarding{CreatedAt: signUp, ExitReason: onboarding.ExitHoldout, ExitedAt: &exited, Country: "DE", TimeZone: "Europe/Berlin"}
	cases := []struct {
		name     string
		sources  mailReaderSources
		timeZone string
		source   string
	}{
		{
			"the device's zone, though the onboarding row has exited",
			mailReaderSources{MailContext: &model.NetworkMailContext{TimeZone: "America/Chicago"}, Onboarding: closedRow, AppStoreCountry: "JPN"},
			"America/Chicago", reengagement.ContextSourceClient,
		},
		{
			"a device that reported UTC is no zone",
			mailReaderSources{MailContext: &model.NetworkMailContext{TimeZone: "Etc/UTC"}, Onboarding: closedRow},
			"Europe/Berlin", reengagement.ContextSourceOnboarding,
		},
		{
			"the sign-up IP's zone where the purchase agrees",
			mailReaderSources{Onboarding: closedRow, StripeCountry: "de"},
			"Europe/Berlin", reengagement.ContextSourceOnboarding,
		},
		{
			"a purchase in another country outranks the IP",
			mailReaderSources{Onboarding: closedRow, PlayCountry: "JP"},
			"Asia/Tokyo", reengagement.ContextSourceCountry,
		},
		{
			"the IP's country without a zone",
			mailReaderSources{Onboarding: &model.NetworkOnboarding{Country: "KR"}},
			"Asia/Seoul", reengagement.ContextSourceCountry,
		},
		{
			"a re-engagement row's country without an onboarding row",
			mailReaderSources{IpCountry: "US"},
			"America/New_York", reengagement.ContextSourceCountry,
		},
		{
			"the last activity when nothing names a zone",
			mailReaderSources{Onboarding: &model.NetworkOnboarding{CreatedAt: signUp}, ActivityAt: &signUp},
			"", reengagement.ContextSourceActivity,
		},
		{"nothing", mailReaderSources{}, "", ""},
	}
	for _, c := range cases {
		r := resolveMailReader(c.sources, 10, 0)
		if r.TimeZone != c.timeZone || r.TimeZoneSource != c.source {
			t.Errorf("%s: %q from %q, want %q from %q", c.name, r.TimeZone, r.TimeZoneSource, c.timeZone, c.source)
		}
	}
	// the country's zone keeps its narrowing
	if r := resolveMailReader(mailReaderSources{IpCountry: "US"}, 10, 0); r.SendZone.OpenLater != 6*time.Hour {
		t.Errorf("US zone %+v", r.SendZone)
	}
	// an activity frame places at the activity's time of day
	if r := resolveMailReader(mailReaderSources{ActivityAt: &signUp}, 10, 0); signUp.In(r.SendZone.Loc()).Hour() != 10 || !r.SendZone.Frame {
		t.Errorf("activity frame %+v", r.SendZone)
	}
}

// The languages: the device's list, the onboarding row's locale, the newest
// client event's, in that order, then the country, the purchase's before the
// IP's.
func TestResolveMailReaderLanguages(t *testing.T) {
	r := resolveMailReader(mailReaderSources{
		MailContext:   &model.NetworkMailContext{Languages: []string{"ca-ES", "es"}},
		Onboarding:    &model.NetworkOnboarding{Locale: "en-US", Country: "DE"},
		EventLocale:   "fr_FR",
		StripeCountry: "MX",
	}, 10, 0)
	tags := []string{}
	for _, candidate := range r.Languages.Candidates {
		tags = append(tags, candidate.Tag+"/"+candidate.Source)
	}
	if want := []string{"ca-ES/client", "es/client", "en-US/onboarding", "fr-FR/client_event"}; !slices.Equal(tags, want) {
		t.Errorf("candidates %v, want %v", tags, want)
	}
	if r.Languages.Country != "MX" || r.Languages.CountrySource != reengagement.ContextSourceCountry {
		t.Errorf("country %q from %q", r.Languages.Country, r.Languages.CountrySource)
	}
	settings := onboarding.DefaultMailLanguageSettings()
	if locale, source := onboarding.ChooseTemplateLocale(r.Languages, onboarding.EmailLocales, settings); locale != "es" || source != reengagement.ContextSourceClient {
		t.Errorf("the Catalan reader who lists Spanish reads %q from %q", locale, source)
	}
}

// Readers in Japan and Korea known only by country get Japanese and Korean;
// a reader in Switzerland gets English. Until 2026-11-13 the resolver never
// fell back to the country and every one of them got English.
func TestCountryOnlyReadersGetTheirLanguage(t *testing.T) {
	for _, c := range []struct {
		name    string
		sources reengagementContextSources
		locale  string
	}{
		{"Japan by the sign-up IP", reengagementContextSources{Onboarding: &model.NetworkOnboarding{Country: "JP"}}, "ja"},
		{"Japan by the App Store", reengagementContextSources{AppStoreCountry: "JPN"}, "ja"},
		{"Korea by the sign-up IP", reengagementContextSources{Onboarding: &model.NetworkOnboarding{Country: "KR"}}, "ko"},
		{"Korea by Play", reengagementContextSources{PlayCountry: "kr"}, "ko"},
	} {
		got := resolveReengagementContext(c.sources)
		if got.Locale != c.locale || got.LocaleSource != reengagement.ContextSourceCountry {
			t.Errorf("%s: %q from %q, want %q from the country", c.name, got.Locale, got.LocaleSource, c.locale)
		}
	}
	got := resolveReengagementContext(reengagementContextSources{Onboarding: &model.NetworkOnboarding{Country: "CH"}})
	if got.Locale != "" || got.LocaleSource != "" {
		t.Errorf("Switzerland: %q from %q, want English", got.Locale, got.LocaleSource)
	}
	reader := resolveMailReader(mailReaderSources{Onboarding: &model.NetworkOnboarding{Country: "CH"}}, 10, 0)
	if locale, _ := onboarding.ChooseTemplateLocale(reader.Languages, onboarding.EmailLocales, onboarding.DefaultMailLanguageSettings()); locale != "en" {
		t.Errorf("Switzerland reads %q", locale)
	}
}

// A US reader whose zone is unknown is mailed at 09:00-20:00 local in every
// populated US zone on every step of an episode, all year: the scan's first
// step (the scan runs at 02:30 UTC), the later steps on their days, and a step
// the one-email-a-week rule defers to any minute. 18% of re-engagement mail
// went at 10:00 UTC until 2026-11-13, 02:00-07:00 in the Americas.
func TestReengagementSendsKeepAUsReaderInDaytime(t *testing.T) {
	s := reengagement.DefaultSchedule
	zone := resolveMailReader(mailReaderSources{AppStoreCountry: "USA"}, s.SendHour, 0).SendZone
	locations := []*time.Location{}
	for _, name := range []string{"America/New_York", "America/Chicago", "America/Denver", "America/Phoenix", "America/Los_Angeles", "America/Anchorage", "Pacific/Honolulu"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		locations = append(locations, loc)
	}
	check := func(what string, at time.Time) {
		for _, loc := range locations {
			local := at.In(loc)
			if minutes := local.Hour()*60 + local.Minute(); minutes < 9*60 || 20*60 < minutes {
				t.Fatalf("%s goes at %s, %s in %s", what, at, local.Format("15:04"), loc)
			}
		}
	}
	for scan := time.Date(2026, 1, 1, 2, 30, 0, 0, time.UTC); scan.Year() == 2026; scan = scan.AddDate(0, 0, 1) {
		check("R1", reengagement.SendAtZone(scan, 0, zone, s, scan))
		check("R2", reengagement.SendAtZone(scan, s.R2AfterR1Days, zone, s, scan))
		check("R3", reengagement.SendAtZone(scan, s.R3Day, zone, s, scan))
		for minute := 0; minute < 24*60; minute += 37 {
			due := scan.Add(time.Duration(minute) * time.Minute)
			check("a deferred step", reengagement.SendAtZone(due, 0, zone, s, due))
		}
	}
	// what the program did before: UTC's 10:00
	before := reengagement.SendAt(time.Date(2026, 11, 18, 2, 30, 0, 0, time.UTC), 0, onboarding.LoadLocation(""), s, time.Date(2026, 11, 18, 2, 30, 0, 0, time.UTC))
	if local := before.In(locations[0]); 9 <= local.Hour() {
		t.Fatalf("the UTC placement reached New York at %s", local)
	}
}

// An onboarding row whose zone and country are unknown keeps the sign-up's
// own time of day: a sign-up at 01:00 UTC (20:00 in New York) gets its emails
// at 01:00 UTC, not at 08:00 UTC (03:00 in New York) as the UTC clamp placed
// them.
func TestOnboardingUnknownZoneKeepsTheSignUpTimeOfDay(t *testing.T) {
	signUp := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
	window := onboarding.DefaultSendWindow
	hour, minute := window.Middle()
	zone := resolveMailReader(mailReaderSources{
		Onboarding: &model.NetworkOnboarding{CreatedAt: signUp},
		ActivityAt: &signUp,
	}, hour, minute).SendZone
	for _, day := range []int{1, 3, 5} {
		at := onboarding.SendAt(signUp, day, zone.Loc(), zone.Window(window))
		if want := signUp.AddDate(0, 0, day); !at.Equal(want) {
			t.Errorf("day %d at %s, want %s", day, at, want)
		}
	}
	// a late step goes within half a window of that time of day, never at
	// the other side of the clock
	late := onboarding.DueSendAt(signUp.Add(12*time.Hour), zone.Loc(), zone.Window(window))
	if offset := late.Sub(signUp.Add(24 * time.Hour)); offset < -6*time.Hour-30*time.Minute || 0 < offset {
		t.Errorf("a step due at 13:00 UTC goes at %s", late)
	}
	// the dates in the emails stay in UTC
	if zone.DisplayLocation() != time.UTC {
		t.Error("a frame writes its dates in its own offset")
	}
	// the UTC clamp it replaces
	if before := onboarding.SendAt(signUp, 1, time.UTC, window); before.Hour() != 8 {
		t.Fatalf("the clamp placed it at %s", before)
	}
}

// A request reports its languages in order: the client's list, its locale,
// then a browser's whole Accept-Language; and its app build from
// X-UR-ClientInfo, else its device spec.
func TestMailReportLanguagesAndClient(t *testing.T) {
	clientSession := session.NewLocalClientSession(context.Background(), "", nil)
	defer clientSession.Cancel()
	clientSession.Header = map[string][]string{"Accept-Language": {"ca-ES,es;q=0.9"}}
	settings := onboarding.DefaultMailContextSettings()
	if got := mailReportLanguages(clientSession, []string{"pt_BR", "en"}, "pt-BR", settings); !slices.Equal(got, []string{"pt-BR", "en", "ca-ES", "es"}) {
		t.Errorf("languages %v", got)
	}
	if got := mailReportLanguages(nil, nil, "ja-JP", settings); !slices.Equal(got, []string{"ja-JP"}) {
		t.Errorf("an app's locale %v", got)
	}

	clientSession.ClientInfo = session.ParseClientInfo(`{"v":1,"device_type":"android","app_version":"2026.10.15-1072765730","distribution":"play"}`, "")
	if client := mailReportClient(clientSession, "16 samsung galaxy"); client == nil || client.Platform != "android" || client.Distribution != "play" || client.AppVersion != "2026.10.15-1072765730" {
		t.Errorf("client %+v", client)
	}
	clientSession.ClientInfo = session.UnknownClientInfo()
	if client := mailReportClient(clientSession, "17.1 iPhone"); client == nil || client.Platform != "ios" || client.Distribution != "" {
		t.Errorf("a spec's client %+v", client)
	}
	if client := mailReportClient(clientSession, "socks5"); client != nil {
		t.Errorf("a spec without a platform: %+v", client)
	}
}

// The device's zone and language reach the re-engagement context after the
// network's onboarding row has exited. auth-client used to store them only in
// a running onboarding row: a holdout, a device-path network or one that
// finished onboarding kept neither, and was mailed at 10:00 UTC in English.
func TestAuthClientKeepsTheDeviceContextAfterOnboardingExits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-mail-context-exited", userId)
		now := server.NowUtc()
		if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{
			NetworkId: networkId, CreatedAt: now.Add(-60 * 24 * time.Hour), Email: true,
			ExitReason: onboarding.ExitHoldout, ExitedAt: &now,
		}) {
			t.Fatal("onboarding fixture")
		}
		clientSession := session.NewLocalClientSession(ctx, "", &session.ByJwt{NetworkId: networkId, UserId: userId})
		defer clientSession.Cancel()
		clientSession.ClientInfo = session.ParseClientInfo(`{"v":1,"device_type":"ios","app_version":"2026.9.9","distribution":"app_store"}`, "")

		recordAuthNetworkClientOnboarding(&model.AuthNetworkClientArgs{
			DeviceSpec: "18.1 iphone 16 pro",
			TimeZone:   "America/Chicago",
			Locale:     "ja-JP",
		}, clientSession)

		status := ReengagementNetworkStatus(ctx, networkId)
		if status.Context.TimeZone != "America/Chicago" || status.Context.TimeZoneSource != reengagement.ContextSourceClient {
			t.Errorf("time zone %q from %q, want the device's", status.Context.TimeZone, status.Context.TimeZoneSource)
		}
		if status.Context.Locale != "ja" || status.Context.LocaleSource != reengagement.ContextSourceClient {
			t.Errorf("locale %q from %q, want the device's", status.Context.Locale, status.Context.LocaleSource)
		}
		mailContext := model.GetNetworkMailContext(ctx, networkId)
		if mailContext == nil || len(mailContext.Clients) != 1 || mailContext.Clients[0].Platform != "ios" || mailContext.Clients[0].Distribution != "app_store" || mailContext.Clients[0].AppVersion != "2026.9.9" {
			t.Errorf("mail context %+v", mailContext)
		}
		// the exited row itself takes nothing, as before
		if row := model.GetNetworkOnboarding(ctx, networkId); row.TimeZone != "" || row.Locale != "" {
			t.Errorf("the exited row changed: %+v", row)
		}

		// a device that reports UTC leaves the zone as it was
		recordAuthNetworkClientOnboarding(&model.AuthNetworkClientArgs{DeviceSpec: "linux", TimeZone: "Etc/UTC"}, clientSession)
		if mailContext := model.GetNetworkMailContext(ctx, networkId); mailContext.TimeZone != "America/Chicago" {
			t.Errorf("UTC replaced the zone: %q", mailContext.TimeZone)
		}
	})
}

// A sign-up keeps the zone and the whole language list it reported, before
// the enrollment that places E1, so E1 goes at the sign-up's time of day in
// that zone; a sign-in keeps its own for the network its token names.
func TestSignUpAndSignInKeepTheMailContext(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, "synthetic-mail-context-signup", userId)
		createSession := session.NewLocalClientSession(ctx, "", nil)
		defer createSession.Cancel()
		createSession.Header = map[string][]string{"Accept-Language": {"ca-ES,es;q=0.9,en;q=0.8"}}
		result := &model.NetworkCreateResult{Network: &model.NetworkCreateResultNetwork{NetworkId: networkId}, UserAuth: &userAuth}

		recordNetworkCreateMailContextPostPrimary(result, &model.NetworkCreateArgs{TimeZone: "Europe/Madrid"}, createSession)
		mailContext := model.GetNetworkMailContext(ctx, networkId)
		if mailContext == nil || mailContext.TimeZone != "Europe/Madrid" || !slices.Equal(mailContext.Languages, []string{"ca-ES", "es", "en"}) {
			t.Fatalf("sign-up mail context %+v", mailContext)
		}
		enrollNetworkCreateOnboardingPostPrimary(result, createSession)
		row := model.GetNetworkOnboarding(ctx, networkId)
		if row == nil || row.NextSendAt == nil {
			t.Fatalf("enrollment %+v", row)
		}
		madrid, _ := time.LoadLocation("Europe/Madrid")
		hour, minute := onboarding.DefaultSendWindow.Middle()
		if local := row.NextSendAt.In(madrid); local.Hour() < 8 || 21 < local.Hour() || (local.Hour() == 21 && 0 < local.Minute()) {
			t.Errorf("E1 at %s in Madrid (window middle %02d:%02d)", local, hour, minute)
		}
		reader := onboardingMailReader(ctx, row)
		if reader.TimeZone != "Europe/Madrid" || reader.TimeZoneSource != reengagement.ContextSourceClient {
			t.Errorf("onboarding reader zone %q from %q", reader.TimeZone, reader.TimeZoneSource)
		}

		signInSession := session.NewLocalClientSession(ctx, "", nil)
		defer signInSession.Cancel()
		token := session.NewByJwt(networkId, userId, "synthetic-mail-context-signup", false, false).Testing_Sign()
		recordSignInMailContext(signInSession, token, "Asia/Tokyo", []string{"ja-JP"})
		mailContext = model.GetNetworkMailContext(ctx, networkId)
		if mailContext.TimeZone != "Asia/Tokyo" || !slices.Equal(mailContext.Languages, []string{"ja-JP"}) {
			t.Errorf("sign-in mail context %+v", mailContext)
		}
		otherId := server.NewId()
		recordSignInMailContext(signInSession, "synthetic-invalid-token", "Asia/Seoul", []string{"ko"})
		if model.GetNetworkMailContext(ctx, otherId) != nil {
			t.Error("an invalid token recorded a mail context")
		}
		if mailContext := model.GetNetworkMailContext(ctx, networkId); mailContext.TimeZone != "Asia/Tokyo" {
			t.Errorf("an invalid token changed the network's zone: %q", mailContext.TimeZone)
		}
	})
}

// An onboarding step that waits for the offer's last day is placed in the
// zone the device reported, though the row recorded none: the step used the
// row's zone alone and placed the reader by UTC.
func TestOnboardingStepPlacesInTheDeviceZone(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-mail-context-step", server.NewId())
		now := server.NowUtc()
		createdAt := now.Add(-4 * 24 * time.Hour)
		if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{NetworkId: networkId, CreatedAt: createdAt,
			Email: true, Path: onboarding.PathA, NextStep: onboarding.StepE3, NextSendAt: &now}) {
			t.Fatal("campaign fixture missing")
		}
		if !model.RecordNetworkMailContext(ctx, networkId, &model.NetworkMailContextReport{TimeZone: "Asia/Tokyo"}, now, onboarding.DefaultMailContextSettings()) {
			t.Fatal("mail context fixture missing")
		}
		if _, created, err := model.IssueOnboardingOffer(ctx, &model.IssueOnboardingOfferArgs{
			NetworkId: networkId, IssuedBy: model.OnboardingOfferIssuedByInApp, Surface: "final_screen",
			Tier: "standard", PercentOff: 25, MonthsFree: 3, Validity: 3 * 24 * time.Hour,
		}); err != nil || !created {
			t.Fatal("offer fixture", err)
		}
		// the offer ends at 18:00 UTC three days on, 03:00 in Tokyo: its last
		// chance is 18:00 the day before in UTC, and 08:00 (the window's
		// opening) in Tokyo, whatever the hour the test runs at
		day := time.Date(now.Year(), now.Month(), now.Day(), 18, 0, 0, 0, time.UTC)
		expiresAt := day.AddDate(0, 0, 3)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_onboarding_offer SET expires_at = $2 WHERE network_id = $1`, networkId, expiresAt))
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()

		result, err := OnboardingCampaignStep(&OnboardingCampaignStepArgs{NetworkId: networkId, Step: onboarding.StepE3}, owner)
		if err != nil {
			t.Fatal(err)
		}
		tokyo, _ := time.LoadLocation("Asia/Tokyo")
		want := onboarding.LastChanceAt(expiresAt, tokyo, onboarding.DefaultSendWindow)
		if !want.Equal(time.Date(day.Year(), day.Month(), day.Day()+2, 23, 0, 0, 0, time.UTC)) {
			t.Fatalf("Tokyo's last chance is %s", want)
		}
		if result.Action != onboarding.ActionDefer || result.NextSendAt == nil || !result.NextSendAt.Equal(want) {
			t.Fatalf("e3a deferral %+v, want %s (08:00 in Tokyo)", result, want)
		}
		if utc := onboarding.LastChanceAt(expiresAt, time.UTC, onboarding.DefaultSendWindow); utc.Equal(want) {
			t.Fatal("the fixture cannot tell Tokyo from UTC")
		}
	})
}

// A re-engagement step for a US reader whose zone is unknown places the next
// step in daytime across the US; one whose device reported a zone places it
// in that zone at the program's hour. The step read the episode row's zone
// alone, which was empty, and placed the reader at 10:00 UTC.
func TestReengagementStepPlacesInTheReadersDay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		restore := model.Testing_SetReengagementConfig(&model.ReengagementConfig{Enabled: false})
		defer restore()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		now := server.NowUtc()
		s := model.Reengagement().ScheduleValue()

		open := func(name string) server.Id {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, name, userId)
			lastConnect := now.Add(-31 * 24 * time.Hour)
			row := &model.NetworkReengagement{
				NetworkId: networkId, Stream: reengagement.StreamLapsed, StartedAt: now.Add(-time.Hour),
				LastConnectAt: &lastConnect, Country: "US", NextStep: reengagement.StepR1, NextSendAt: &now,
			}
			if !model.StartNetworkReengagement(ctx, row, now.Add(-time.Duration(s.CooldownDays)*24*time.Hour), now) {
				t.Fatal("episode fixture")
			}
			return networkId
		}
		step := func(networkId server.Id) *ReengagementStepResult {
			result, err := ReengagementStep(&ReengagementStepArgs{NetworkId: networkId, Episode: 1, Step: reengagement.StepR1}, owner)
			if err != nil {
				t.Fatal(err)
			}
			// nothing changed for the network: R1 hands its day to R2
			if result.Action != reengagement.ActionSkip || result.NextStep != reengagement.StepR2 || result.NextSendAt == nil {
				t.Fatalf("R1 %+v", result)
			}
			return result
		}

		unknownId := open("synthetic-mail-context-us")
		at := *step(unknownId).NextSendAt
		for _, name := range []string{"America/New_York", "America/Chicago", "America/Los_Angeles", "America/Anchorage", "Pacific/Honolulu"} {
			loc, _ := time.LoadLocation(name)
			if local := at.In(loc); local.Hour() < 9 || 20 < local.Hour() || (local.Hour() == 20 && 0 < local.Minute()) {
				t.Errorf("R2 at %s, %s in %s", at, local.Format("15:04"), name)
			}
		}

		deviceId := open("synthetic-mail-context-device")
		if !model.RecordNetworkMailContext(ctx, deviceId, &model.NetworkMailContextReport{TimeZone: "Asia/Tokyo"}, now, onboarding.DefaultMailContextSettings()) {
			t.Fatal("mail context fixture missing")
		}
		tokyo, _ := time.LoadLocation("Asia/Tokyo")
		row := model.GetNetworkReengagement(ctx, deviceId)
		want := reengagement.SendAt(row.StartedAt, 0, tokyo, s, server.NowUtc())
		if got := *step(deviceId).NextSendAt; got.Sub(want) < -time.Minute || time.Minute < got.Sub(want) {
			t.Errorf("R2 at %s, want %s in Tokyo", got, want)
		}
	})
}
