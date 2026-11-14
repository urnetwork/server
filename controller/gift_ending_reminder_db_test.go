package controller

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/reengagement"
	"github.com/urnetwork/server/session"
)

// A network for the gift-ending notice, signed in with an email: its sign-up
// row names its country, zone and locale, and its product-updates preference
// carries the source the mail policy reads (consentSource; "" leaves the
// sign-up default with no row).
func giftEndingTestNetwork(t testing.TB, ctx context.Context, country string, locale string, timeZone string, consentSource string, productUpdates bool) (networkId server.Id, adminUserId server.Id, userAuth string) {
	t.Helper()
	networkId = server.NewId()
	adminUserId = server.NewId()
	userAuth = model.Testing_CreateNetwork(ctx, networkId, "gift-ending-"+networkId.String(), adminUserId)
	model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{
		NetworkId: networkId,
		CreatedAt: server.NowUtc().AddDate(-1, 0, -1),
		Email:     true,
		TimeZone:  timeZone,
		Locale:    locale,
		Country:   country,
		ExitedAt:  func() *time.Time { at := server.NowUtc().AddDate(-1, 0, 0); return &at }(),
	})
	if consentSource != "" || !productUpdates {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO account_preferences (network_id, product_updates, product_updates_source, product_updates_source_time)
					VALUES ($1, $2, $3, $4)
				`,
				networkId,
				productUpdates,
				consentSource,
				server.NowUtc().AddDate(-1, 0, 0),
			))
		})
	}
	return
}

// A redeemed gift year of the network ending at `end`, its code its
// renewal's transaction id; returns the year as the listing reads it.
func giftEndingTestYear(t testing.TB, ctx context.Context, networkId server.Id, end time.Time) *model.GiftYearEnding {
	t.Helper()
	codeId := server.NewId().String()
	if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          end.AddDate(-1, 0, 0),
		EndTime:            end,
		NetRevenue:         model.UsdToNanoCents(29.99),
		SubscriptionMarket: model.SubscriptionMarketGift,
		TransactionId:      codeId,
		PaidEndTime:        &end,
	}); err != nil {
		t.Fatal(err)
	}
	return &model.GiftYearEnding{NetworkId: networkId, EndTime: end, TransactionId: codeId}
}

// The gift-ending notice (GIFTOFPRIVACY.md §9.2): one email 14 days before a
// redeemed gift year ends, when nothing else will carry Pro on, to an email
// login the mail policy lets an offer reach, in the reader's language with
// the date in their zone; once per gift year, behind its switch; and never a
// campaign: no re-engagement event and no episode.
func TestGiftEndingReminder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		day := 24 * time.Hour
		end := dayStart(now).Add(14*day + 12*time.Hour)

		// sent: a US reader with the sign-up switch, a Mexican one in Spanish
		// with the sign-up default (no evidence needed there), a German one
		// who set the preference in settings
		us, _, usAuth := giftEndingTestNetwork(t, ctx, "US", "en-US", "America/Chicago", onboarding.ConsentSourceSignupSwitch, true)
		usYear := giftEndingTestYear(t, ctx, us, end)
		mx, _, mxAuth := giftEndingTestNetwork(t, ctx, "MX", "es-MX", "America/Mexico_City", "", true)
		mxYear := giftEndingTestYear(t, ctx, mx, end)
		de, _, deAuth := giftEndingTestNetwork(t, ctx, "DE", "de-DE", "Europe/Berlin", onboarding.ConsentSourceSettings, true)
		deYear := giftEndingTestYear(t, ctx, de, end)

		// kept back by the mail policy, each for its reason
		refused := map[string]server.Id{}
		year := func(networkId server.Id) server.Id {
			giftEndingTestYear(t, ctx, networkId, end)
			return networkId
		}
		refused[onboarding.ExitConsentEvidence] = year(func() server.Id {
			networkId, _, _ := giftEndingTestNetwork(t, ctx, "DE", "de-DE", "Europe/Berlin", onboarding.ConsentSourceSignupDefault, true)
			return networkId
		}())
		refused[giftEndingRefusalProductUpdates] = year(func() server.Id {
			networkId, _, _ := giftEndingTestNetwork(t, ctx, "US", "en-US", "America/Chicago", onboarding.ConsentSourceSettings, false)
			return networkId
		}())
		refused[giftEndingRefusalSuppressed] = year(func() server.Id {
			networkId, _, _ := giftEndingTestNetwork(t, ctx, "US", "en-US", "America/Chicago", onboarding.ConsentSourceSignupSwitch, true)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE account_preferences SET marketing_suppressed_at = $2, marketing_suppressed_reason = 'complaint' WHERE network_id = $1`, networkId, now))
			})
			return networkId
		}())
		// a country nobody knows: consent evidence, but no offer may go
		refused[giftEndingRefusalNoOffer] = year(func() server.Id {
			networkId, _, _ := giftEndingTestNetwork(t, ctx, "", "", "", onboarding.ConsentSourceOptIn, true)
			return networkId
		}())
		refused[onboarding.ExitCountryNoMarketing] = year(func() server.Id {
			networkId, _, _ := giftEndingTestNetwork(t, ctx, "RU", "ru-RU", "Europe/Moscow", onboarding.ConsentSourceSignupSwitch, true)
			return networkId
		}())
		refused[onboarding.ExitCanadaConsent] = year(func() server.Id {
			networkId, _, _ := giftEndingTestNetwork(t, ctx, "CA", "en-CA", "America/Toronto", onboarding.ConsentSourceSignupSwitch, true)
			return networkId
		}())
		refused[giftEndingRefusalEpisode] = year(func() server.Id {
			networkId, _, _ := giftEndingTestNetwork(t, ctx, "US", "en-US", "America/Chicago", onboarding.ConsentSourceSignupSwitch, true)
			nextSendAt := now.Add(day)
			if !model.StartNetworkReengagement(ctx, &model.NetworkReengagement{
				NetworkId:  networkId,
				Stream:     reengagement.StreamLapsed,
				StartedAt:  now,
				Country:    "US",
				NextStep:   "r1_news",
				NextSendAt: &nextSendAt,
			}, now, now) {
				t.Fatal("no episode started")
			}
			return networkId
		}())
		refused[giftEndingRefusalNoEmail] = year(func() server.Id {
			networkId, adminUserId, _ := giftEndingTestNetwork(t, ctx, "US", "en-US", "America/Chicago", onboarding.ConsentSourceSignupSwitch, true)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET user_auth = '+15555550123' WHERE user_id = $1`, adminUserId))
			})
			return networkId
		}())

		// never listed: Pro goes on past the year, by a current card
		// subscription, by a gift year queued after it, or the year ends later
		renewing, _, _ := giftEndingTestNetwork(t, ctx, "US", "en-US", "America/Chicago", onboarding.ConsentSourceSignupSwitch, true)
		giftEndingTestYear(t, ctx, renewing, end)
		if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          renewing,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-10 * day),
			EndTime:            now.Add(20 * day),
			NetRevenue:         model.UsdToNanoCents(5),
			SubscriptionMarket: model.SubscriptionMarketStripe,
			TransactionId:      "in_gift_ending_" + server.NewId().String(),
		}); err != nil {
			t.Fatal(err)
		}
		queued, _, _ := giftEndingTestNetwork(t, ctx, "US", "en-US", "America/Chicago", onboarding.ConsentSourceSignupSwitch, true)
		giftEndingTestYear(t, ctx, queued, end)
		giftEndingTestYear(t, ctx, queued, end.AddDate(1, 0, 0))
		later, _, _ := giftEndingTestNetwork(t, ctx, "US", "en-US", "America/Chicago", onboarding.ConsentSourceSignupSwitch, true)
		giftEndingTestYear(t, ctx, later, end.Add(10*day))

		wantRefused := map[string]int{}
		for reason := range refused {
			wantRefused[reason] = 1
		}
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		run := func(giftEnding bool, dryRun bool) *ServiceRemindersResult {
			t.Helper()
			defer model.Testing_SetReengagementConfig(&model.ReengagementConfig{
				ServiceReminders: model.ReengagementServiceRemindersConfig{GiftEnding: giftEnding},
			})()
			result, err := ServiceReminders(&ServiceRemindersArgs{DryRun: dryRun}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(result.GiftEndingRefused, wantRefused) {
				t.Fatalf("kept back %v, want %v", result.GiftEndingRefused, wantRefused)
			}
			return result
		}
		queuedNotice := func(year *model.GiftYearEnding) *model.AccountMessage {
			return model.GetAccountMessageByKey(ctx, giftEndingTemplateName, giftEndingKey(year))
		}

		// off: counted, nothing queued
		if result := run(false, false); result.GiftEnding != 3 {
			t.Fatalf("switched off, %d counted, want 3", result.GiftEnding)
		}
		if queuedNotice(usYear) != nil {
			t.Fatal("switched off, a notice was queued")
		}
		// a dry run with it on queues nothing either
		if result := run(true, true); result.GiftEnding != 3 || queuedNotice(usYear) != nil {
			t.Fatalf("dry run: %d counted, notice %v", result.GiftEnding, queuedNotice(usYear))
		}

		// on: three notices, each to its reader's email in their language and
		// zone; a second run adds none
		if result := run(true, false); result.GiftEnding != 3 {
			t.Fatalf("switched on, %d queued, want 3", result.GiftEnding)
		}
		for _, c := range []struct {
			year      *model.GiftYearEnding
			networkId server.Id
			userAuth  string
			language  string
			timeZone  string
		}{
			{usYear, us, usAuth, "en", "America/Chicago"},
			{mxYear, mx, mxAuth, "es", "America/Mexico_City"},
			{deYear, de, deAuth, "de", "Europe/Berlin"},
		} {
			message := queuedNotice(c.year)
			if message == nil || message.UserAuth != c.userAuth || message.NetworkId == nil || *message.NetworkId != c.networkId {
				t.Fatalf("%s: notice %+v", c.language, message)
			}
			var stored GiftEndingTemplate
			if err := json.Unmarshal([]byte(message.TemplateJson), &stored); err != nil || stored.Lang != c.language || stored.TimeZone != c.timeZone || !stored.EndTime.Equal(end) {
				t.Fatalf("%s: stored %s", c.language, message.TemplateJson)
			}
		}
		if result := run(true, false); result.GiftEnding != 0 {
			t.Fatalf("a second run queued %d", result.GiftEnding)
		}

		// they go out as emails
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, now.Add(time.Minute))
		languages := []string{}
		for _, send := range sender.sent() {
			if template, ok := send.template.(*GiftEndingTemplate); ok {
				languages = append(languages, template.Language())
			}
		}
		slices.Sort(languages)
		if !slices.Equal(languages, []string{"de", "en", "es"}) {
			t.Fatalf("sent in %v", languages)
		}

		// no campaign: no re-engagement event for any of these networks, and
		// the one episode is the one opened above
		networkIds := []server.Id{us, mx, de, renewing, queued, later}
		for _, networkId := range refused {
			networkIds = append(networkIds, networkId)
		}
		var events int
		var episodes int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM network_onboarding_event WHERE network_id = ANY($1) AND name LIKE 'reengage.%'`,
				networkIds,
			).Scan(&events))
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM network_reengagement WHERE network_id = ANY($1)`,
				networkIds,
			).Scan(&episodes))
		})
		if events != 0 || episodes != 1 {
			t.Fatalf("%d re-engagement events and %d episodes, want none and the one opened", events, episodes)
		}
	})
}
