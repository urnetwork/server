// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/reengagement"
	"github.com/urnetwork/server/session"
)

// The onboarding row decides every part it has, whatever else is on record.
func TestResolveReengagementContextOnboardingRowFirst(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	c := resolveReengagementContext(reengagementContextSources{
		Onboarding:      &model.NetworkOnboarding{Locale: "de-DE", TimeZone: "Europe/Berlin", Platform: "android", Country: "DE"},
		AppStoreCountry: "USA",
		PlayCountry:     "BR",
		StripeCountry:   "CA",
		Client: &model.NetworkClientContext{
			EventPlatform: "ios", EventPlatformAt: &now,
			EventLocale: "fr-FR", EventLocaleAt: &now,
			DeviceSpec: "macOS 26.0 arm64", DeviceSpecAt: &now,
		},
	})
	onb := reengagement.ContextSourceOnboarding
	want := ReengagementContext{
		Locale: "de-DE", LocaleSource: onb,
		TimeZone: "Europe/Berlin", TimeZoneSource: onb,
		Platform: "android", PlatformSource: onb,
		Country: "DE", CountrySource: onb,
	}
	if c != want {
		t.Fatalf("context = %+v, want %+v", c, want)
	}
}

// The country: the onboarding row's, else the App Store storefront, else the
// Play region, else the Stripe card's, normalized to alpha-2; a value that is
// no country is no value.
func TestResolveReengagementContextCountryOrder(t *testing.T) {
	cases := []struct {
		name    string
		sources reengagementContextSources
		country string
		source  string
	}{
		{"the App Store first", reengagementContextSources{AppStoreCountry: "USA", PlayCountry: "BR", StripeCountry: "CA"}, "US", reengagement.ContextSourceAppStore},
		{"an alpha-3 storefront in any case", reengagementContextSources{AppStoreCountry: "deu"}, "DE", reengagement.ContextSourceAppStore},
		{"Play next", reengagementContextSources{PlayCountry: "br", StripeCountry: "CA"}, "BR", reengagement.ContextSourcePlay},
		{"the Stripe card last", reengagementContextSources{StripeCountry: " ca "}, "CA", reengagement.ContextSourceStripe},
		{"values that are no country fall through", reengagementContextSources{AppStoreCountry: "ZZZ", PlayCountry: "B1", StripeCountry: "MX"}, "MX", reengagement.ContextSourceStripe},
		{"the onboarding row's country wins", reengagementContextSources{Onboarding: &model.NetworkOnboarding{Country: "jp"}, AppStoreCountry: "USA"}, "JP", reengagement.ContextSourceOnboarding},
		{"a row without a country falls through", reengagementContextSources{Onboarding: &model.NetworkOnboarding{Locale: "en"}, PlayCountry: "IN"}, "IN", reengagement.ContextSourcePlay},
		{"a row whose country is no country falls through", reengagementContextSources{Onboarding: &model.NetworkOnboarding{Country: "XXX"}, StripeCountry: "GB"}, "GB", reengagement.ContextSourceStripe},
		{"no source", reengagementContextSources{}, "", ""},
	}
	for _, c := range cases {
		got := resolveReengagementContext(c.sources)
		if got.Country != c.country || got.CountrySource != c.source {
			t.Errorf("%s: country %q from %q, want %q from %q", c.name, got.Country, got.CountrySource, c.country, c.source)
		}
	}
}

// The locale and the platform fall back to the clients' own reports, part by
// part; the time zone has no source but the onboarding row.
func TestResolveReengagementContextClientReports(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	older := now.Add(-48 * time.Hour)

	c := resolveReengagementContext(reengagementContextSources{
		Client: &model.NetworkClientContext{EventLocale: "pt_br", EventLocaleAt: &older, EventPlatform: "android", EventPlatformAt: &older},
	})
	if c.Locale != "pt-BR" || c.LocaleSource != reengagement.ContextSourceClientEvent {
		t.Errorf("event locale: %+v", c)
	}
	if c.Platform != "android" || c.PlatformSource != reengagement.ContextSourceClientEvent {
		t.Errorf("event platform: %+v", c)
	}
	if c.TimeZone != "" || c.TimeZoneSource != "" {
		t.Errorf("a client report carries no time zone: %+v", c)
	}

	// a row with only a time zone (and a platform outside the events'
	// vocabulary) keeps its zone and takes the rest from the clients
	c = resolveReengagementContext(reengagementContextSources{
		Onboarding: &model.NetworkOnboarding{TimeZone: "America/Sao_Paulo", Platform: "toaster"},
		Client:     &model.NetworkClientContext{EventLocale: "pt-BR", EventLocaleAt: &older, DeviceSpec: "17.1 iPhone", DeviceSpecAt: &older},
	})
	want := ReengagementContext{
		Locale: "pt-BR", LocaleSource: reengagement.ContextSourceClientEvent,
		TimeZone: "America/Sao_Paulo", TimeZoneSource: reengagement.ContextSourceOnboarding,
		Platform: "ios", PlatformSource: reengagement.ContextSourceDevice,
	}
	if c != want {
		t.Errorf("partial row: %+v, want %+v", c, want)
	}

	// a locale that does not normalize is no locale
	c = resolveReengagementContext(reengagementContextSources{Client: &model.NetworkClientContext{EventLocale: "1x", EventLocaleAt: &now}})
	if c.Locale != "" || c.LocaleSource != "" {
		t.Errorf("junk locale: %+v", c)
	}
}

// The platform is the more recent of the newest client event's and the one
// the newest client's device spec names.
func TestResolveReengagementContextPlatformRecency(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	older := now.Add(-48 * time.Hour)
	cases := []struct {
		name     string
		client   model.NetworkClientContext
		platform string
		source   string
	}{
		{"an event only", model.NetworkClientContext{EventPlatform: "android", EventPlatformAt: &older}, "android", reengagement.ContextSourceClientEvent},
		{"a device only", model.NetworkClientContext{DeviceSpec: "17.1 iPhone", DeviceSpecAt: &older}, "ios", reengagement.ContextSourceDevice},
		{"the device's report is newer", model.NetworkClientContext{EventPlatform: "android", EventPlatformAt: &older, DeviceSpec: "macOS 26.0 arm64", DeviceSpecAt: &now}, "macos", reengagement.ContextSourceDevice},
		{"the event is newer", model.NetworkClientContext{EventPlatform: "android", EventPlatformAt: &now, DeviceSpec: "17.1 iPhone", DeviceSpecAt: &older}, "android", reengagement.ContextSourceClientEvent},
		{"a tie goes to the event", model.NetworkClientContext{EventPlatform: "android", EventPlatformAt: &now, DeviceSpec: "17.1 iPhone", DeviceSpecAt: &now}, "android", reengagement.ContextSourceClientEvent},
		{"a spec that names no platform is no report", model.NetworkClientContext{EventPlatform: "android", EventPlatformAt: &older, DeviceSpec: "15.7.9 MacBook Air (M1, 2020)", DeviceSpecAt: &now}, "android", reengagement.ContextSourceClientEvent},
		// the Android app's "<os version> <make model>" names Android
		{"the Android app's spec is a report", model.NetworkClientContext{EventPlatform: "ios", EventPlatformAt: &older, DeviceSpec: "16 Samsung Galaxy S24 Ultra", DeviceSpecAt: &now}, "android", reengagement.ContextSourceDevice},
		{"an event platform outside the vocabulary is no report", model.NetworkClientContext{EventPlatform: "toaster", EventPlatformAt: &now}, "", ""},
		{"no report", model.NetworkClientContext{}, "", ""},
	}
	for _, c := range cases {
		client := c.client
		got := resolveReengagementContext(reengagementContextSources{Client: &client})
		if got.Platform != c.platform || got.PlatformSource != c.source {
			t.Errorf("%s: platform %q from %q, want %q from %q", c.name, got.Platform, got.PlatformSource, c.platform, c.source)
		}
	}
	// the onboarding row's platform wins over a newer client report
	got := resolveReengagementContext(reengagementContextSources{
		Onboarding: &model.NetworkOnboarding{Platform: "web"},
		Client:     &model.NetworkClientContext{EventPlatform: "android", EventPlatformAt: &now},
	})
	if got.Platform != "web" || got.PlatformSource != reengagement.ContextSourceOnboarding {
		t.Errorf("row platform: %+v", got)
	}
}

// The country sources are the episode event's evidence vocabulary, and a scan
// candidate counts once per part, an unknown part as unknown.
func TestReengagementContextSourceVocabulary(t *testing.T) {
	if !slices.Equal(reengagement.CountrySources, model.ReengagementCountrySourceValues) {
		t.Fatalf("country sources %v, event vocabulary %v", reengagement.CountrySources, model.ReengagementCountrySourceValues)
	}
	for _, source := range reengagement.CountrySources {
		props := map[string]any{"stream": reengagement.StreamLapsed, "country": "US", "country_source": source}
		if _, err := model.ValidateServerEvent(model.EventReengageEpisodeStarted, props); err != nil {
			t.Errorf("source %s: %s", source, err)
		}
	}
	for _, props := range []map[string]any{
		{"stream": reengagement.StreamLapsed, "country": "USA", "country_source": reengagement.ContextSourceAppStore},
		{"stream": reengagement.StreamLapsed, "country": "US", "country_source": "ip"},
	} {
		if _, err := model.ValidateServerEvent(model.EventReengageEpisodeStarted, props); err == nil {
			t.Errorf("accepted %v", props)
		}
	}

	parts := []struct {
		part   string
		source string
	}{
		{"country", reengagement.ContextSourceStripe},
		{"locale", reengagement.ContextSourceUnknown},
		{"platform", reengagement.ContextSourceUnknown},
		{"time_zone", reengagement.ContextSourceUnknown},
	}
	stream := "vocabulary_test"
	before := make([]float64, len(parts))
	for i, c := range parts {
		before[i] = testutil.ToFloat64(reengagementContextSourcesTotal.WithLabelValues(stream, c.part, c.source))
	}
	countReengagementContextSources(stream, ReengagementContext{Country: "US", CountrySource: reengagement.ContextSourceStripe})
	for i, c := range parts {
		if got := testutil.ToFloat64(reengagementContextSourcesTotal.WithLabelValues(stream, c.part, c.source)) - before[i]; got != 1 {
			t.Errorf("%s from %s counted %v", c.part, c.source, got)
		}
	}
}

// The context read from what the server keeps: the store countries the
// purchase paths now record, the Stripe card country, the clients' event and
// device reports, and the onboarding row; and the mail policy's verdict on it.
func TestReengagementContextFromStoredSources(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		day := 24 * time.Hour

		newNetwork := func(name string) (server.Id, *session.ClientSession) {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, name, userId)
			return networkId, session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, name, false, false))
		}
		addRenewal := func(renewal *model.SubscriptionRenewal, record func(tx server.PgTx)) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(model.AddSubscriptionRenewalInTx(tx, ctx, renewal))
				record(tx)
			})
		}
		addApple := func(networkId server.Id, transactionId string, start time.Time, storefront string) {
			renewal := &model.SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   model.SubscriptionTypeSupporter,
				StartTime:          start,
				EndTime:            start.Add(30 * day),
				SubscriptionMarket: model.SubscriptionMarketApple,
				TransactionId:      transactionId,
			}
			addRenewal(renewal, func(tx server.PgTx) {
				appleRecordOnboardingInTx(tx, ctx, &validatedAppleTransaction{
					networkId:     networkId,
					transactionId: transactionId,
					storefront:    storefront,
				})
			})
		}
		addPlay := func(networkId server.Id, clientSession *session.ClientSession, purchaseToken string, start time.Time, region string) {
			renewal := &model.SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   model.SubscriptionTypeSupporter,
				StartTime:          start,
				EndTime:            start.Add(30 * day),
				PurchaseToken:      purchaseToken,
				SubscriptionMarket: model.SubscriptionMarketGoogle,
			}
			addRenewal(renewal, func(tx server.PgTx) {
				playRecordOnboardingInTx(tx, clientSession, &PlaySubscription{RegionCode: region}, renewal)
			})
		}
		addStripe := func(networkId server.Id, clientSession *session.ClientSession, country string) {
			customerId := "cus_" + networkId.String()
			server.Raise(model.CreateStripeCustomer(customerId, clientSession))
			if !model.SetStripeCustomerBillingCountry(ctx, customerId, country) {
				t.Fatalf("billing country %s not recorded", country)
			}
		}
		addEvent := func(networkId server.Id, name string, at time.Time, platform string, locale string) {
			server.Raise(model.AddOnboardingEvent(ctx, &model.OnboardingEvent{
				NetworkId: networkId,
				Name:      name,
				At:        at,
				Platform:  platform,
				Locale:    locale,
			}))
		}
		addDevice := func(networkId server.Id, spec string, createTime time.Time, authTime time.Time) {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "device", spec)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE network_client SET create_time = $2, auth_time = $3 WHERE client_id = $1`,
					clientId,
					createTime,
					authTime,
				))
			})
		}
		expect := func(name string, networkId server.Id, want ReengagementContext) {
			if got := reengagementContext(ctx, networkId); got != want {
				t.Errorf("%s: context %+v, want %+v", name, got, want)
			}
		}
		onb := reengagement.ContextSourceOnboarding

		// enrolled in onboarding: the row decides, though an App Store row
		// names another country
		enrolledId, _ := newNetwork("enrolled")
		if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{
			NetworkId: enrolledId, CreatedAt: now, Email: true,
			TimeZone: "Europe/Berlin", Platform: "android", Locale: "de-DE", Country: "DE",
		}) {
			t.Fatal("onboarding row not created")
		}
		addApple(enrolledId, "enrolled-1", now.Add(-10*day), "USA")
		expect("enrolled", enrolledId, ReengagementContext{
			Locale: "de-DE", LocaleSource: onb,
			TimeZone: "Europe/Berlin", TimeZoneSource: onb,
			Platform: "android", PlatformSource: onb,
			Country: "DE", CountrySource: onb,
		})
		// the onboarding campaign's own row is untouched, and the purchase's
		// price tier is still recorded beside its country
		if row := model.GetNetworkOnboarding(ctx, enrolledId); row == nil || row.Country != "DE" || row.Locale != "de-DE" || row.TimeZone != "Europe/Berlin" || row.Platform != "android" {
			t.Errorf("onboarding row changed: %+v", row)
		}
		if tier := model.GetLatestSubscriptionPriceTier(ctx, enrolledId); tier == "" {
			t.Error("the App Store purchase lost its price tier")
		}

		// older than the program, with every purchase source: the latest App
		// Store row that carries a storefront, though Play and Stripe are on
		// record, Play's row is the newest purchase, and the newest App Store
		// row was credited without one
		storesId, storesSession := newNetwork("stores")
		addApple(storesId, "stores-1", now.Add(-90*day), "DEU")
		addApple(storesId, "stores-2", now.Add(-60*day), "usa")
		addApple(storesId, "stores-3", now.Add(-30*day), "")
		addPlay(storesId, storesSession, "stores-play", now.Add(-5*day), "BR")
		addStripe(storesId, storesSession, "ca")
		if appStore, play := model.NetworkStoreCountries(ctx, storesId); appStore != "US" || play != "BR" {
			t.Errorf("store countries %q, %q", appStore, play)
		}
		expect("stores", storesId, ReengagementContext{Country: "US", CountrySource: reengagement.ContextSourceAppStore})

		// Play, then the Stripe card
		playId, playSession := newNetwork("play")
		addPlay(playId, playSession, "play-1", now.Add(-40*day), "br")
		addStripe(playId, playSession, "CA")
		expect("play", playId, ReengagementContext{Country: "BR", CountrySource: reengagement.ContextSourcePlay})

		stripeId, stripeSession := newNetwork("stripe")
		addStripe(stripeId, stripeSession, "ca")
		expect("stripe", stripeId, ReengagementContext{Country: "CA", CountrySource: reengagement.ContextSourceStripe})

		// the App Store webhook's credit records the signed transaction's
		// storefront (alpha-3) on the renewal row it writes
		_, restoreNotices := model.Testing_CaptureSalesPaymentNotices()
		t.Cleanup(restoreNotices)
		webhookId, _ := newNetwork("webhook")
		claims := appleSandboxRevenueClaims(webhookId, "2000000000009901", "Production", now.Truncate(time.Millisecond))
		claims["storefront"] = "DEU"
		processed, err := ProcessAppleNotification(ctx, appleSandboxRevenueNotification(claims, now.Truncate(time.Millisecond)), appleSandboxRevenueProductIds)
		if err != nil || !processed {
			t.Fatalf("the App Store purchase was not credited: processed=%t err=%v", processed, err)
		}
		expect("webhook", webhookId, ReengagementContext{Country: "DE", CountrySource: reengagement.ContextSourceAppStore})

		// nothing on record: every part unknown
		unknownId, _ := newNetwork("unknown")
		expect("unknown", unknownId, ReengagementContext{})

		// the clients' reports: the newest client event with a locale, and the
		// device spec of the newest client, which reported after the newest
		// event; the server's own events are not client reports
		devicesId, _ := newNetwork("devices")
		addEvent(devicesId, model.EventPurchaseStarted, now.Add(-10*day), "android", "pt_BR")
		addEvent(devicesId, model.EventFeedbackSubmitted, now.Add(-5*day), "android", "")
		addEvent(devicesId, model.EventReengageExit, now.Add(-1*day), "windows", "")
		addDevice(devicesId, "17.1 iPhone", now.Add(-30*day), now.Add(-30*day))
		addDevice(devicesId, "macOS 26.0 arm64", now.Add(-20*day), now.Add(-2*day))
		expect("devices", devicesId, ReengagementContext{
			Locale: "pt-BR", LocaleSource: reengagement.ContextSourceClientEvent,
			Platform: "macos", PlatformSource: reengagement.ContextSourceDevice,
		})

		// an event newer than the newest client's last authentication wins
		eventsId, _ := newNetwork("events")
		addDevice(eventsId, "17.1 iPhone", now.Add(-40*day), now.Add(-20*day))
		addEvent(eventsId, model.EventOfferScreenShown, now.Add(-2*day), "android", "es-419")
		expect("events", eventsId, ReengagementContext{
			Locale: "es-419", LocaleSource: reengagement.ContextSourceClientEvent,
			Platform: "android", PlatformSource: reengagement.ContextSourceClientEvent,
		})

		// the newest client's spec names no platform: an older device's
		// does not stand in for it
		newestId, _ := newNetwork("newest")
		addDevice(newestId, "17.1 iPhone", now.Add(-40*day), now.Add(-1*day))
		addDevice(newestId, "15.7.9 MacBook Air (M1, 2020)", now.Add(-3*day), now.Add(-3*day))
		expect("newest", newestId, ReengagementContext{})

		// an onboarding row with only a time zone keeps it; the rest falls back
		partialId, partialSession := newNetwork("partial")
		if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{
			NetworkId: partialId, CreatedAt: now, Email: true, TimeZone: "America/Sao_Paulo",
		}) {
			t.Fatal("onboarding row not created")
		}
		addPlay(partialId, partialSession, "partial-play", now.Add(-20*day), "BR")
		addEvent(partialId, model.EventConnectFirst, now.Add(-3*day), "android", "pt-BR")
		expect("partial", partialId, ReengagementContext{
			Locale: "pt-BR", LocaleSource: reengagement.ContextSourceClientEvent,
			TimeZone: "America/Sao_Paulo", TimeZoneSource: onb,
			Platform: "android", PlatformSource: reengagement.ContextSourceClientEvent,
			Country: "BR", CountrySource: reengagement.ContextSourcePlay,
		})

		// the mail policy: a pre-program network without consent evidence may
		// be mailed once a purchase names a country outside the evidence
		// regions; with no country it is refused
		if exit := reengagementPolicyExit(ctx, storesId, reengagement.StreamLapsed, reengagementContext(ctx, storesId).Country, now); exit != "" {
			t.Errorf("a store country outside the evidence regions refused: %s", exit)
		}
		if exit := reengagementPolicyExit(ctx, unknownId, reengagement.StreamLapsed, reengagementContext(ctx, unknownId).Country, now); exit != onboarding.ExitConsentEvidence {
			t.Errorf("an unknown country: exit %q, want %q", exit, onboarding.ExitConsentEvidence)
		}
	})
}
