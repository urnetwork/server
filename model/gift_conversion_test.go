package model

// A redeemed gift year carries its giver's payment (gift_redeem_model.go), so it
// funds the recipient's traffic as paid traffic. It is never the recipient's
// purchase: the re-engagement and onboarding results, the email tracker, the
// win-back and CASL's purchase window count only what a network bought.

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/reengagement"
)

// A re-engaged network that redeems a gift inside its episode's window is no
// conversion (no paid start, no pro_restart_30d), while a card purchase in the
// same window still is.
func TestReengagementGiftIsNoConversion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		step := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		episode := func(name string) server.Id {
			networkId := giftTestNetwork(t, ctx, name)
			startedAt := step.Add(-time.Minute)
			server.Raise(AddOnboardingEvent(ctx, &OnboardingEvent{
				NetworkId: networkId,
				Name:      EventReengageEpisodeStarted,
				At:        startedAt,
				Props:     map[string]any{"stream": reengagement.StreamLapsed},
			}))
			server.Raise(AddOnboardingEvent(ctx, &OnboardingEvent{
				NetworkId: networkId,
				Name:      EventReengageEmailSent,
				At:        step,
				Platform:  "android",
				Props:     map[string]any{"step": "template", "flow_step": reengagement.StepR1, "variant": "default"},
			}))
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_reengagement (network_id, stream, started_at, country) VALUES ($1, $2, $3, $4)`,
					networkId,
					reengagement.StreamLapsed,
					startedAt,
					"US",
				))
			})
			return networkId
		}
		giftNetworkId := episode("synthetic-reengaged-gift")
		cardNetworkId := episode("synthetic-reengaged-card")

		// the gift, redeemed inside the window: a renewal with revenue
		_, codes := giftTestCodes(t, ctx, 1, nil)
		if result := giftTestRedeem(t, ctx, giftNetworkId, codes[0].Secret); result.Error != nil || result.Pro == nil || result.Pro.Queued {
			t.Fatalf("redeem answered %+v", result)
		}
		if renewal := giftTestRenewalOf(t, ctx, giftNetworkId, codes[0].BalanceCodeId); renewal == nil || renewal.netRevenue <= 0 {
			t.Fatalf("the gift renewal is %+v, want one with its revenue", renewal)
		}
		// a card purchase in the same window, as the Stripe credit records it
		cardStart := server.NowUtc()
		paidEnd := cardStart.Add(30 * 24 * time.Hour)
		if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:          cardNetworkId,
			SubscriptionType:   SubscriptionTypeSupporter,
			StartTime:          cardStart,
			EndTime:            paidEnd.Add(30 * 24 * time.Hour),
			NetRevenue:         UsdToNanoCents(5),
			SubscriptionMarket: SubscriptionMarketStripe,
			TransactionId:      "synthetic-invoice-" + cardNetworkId.String(),
			PaidEndTime:        &paidEnd,
		}); err != nil {
			t.Fatal(err)
		}

		facts := map[server.Id]reengagement.EpisodeFacts{}
		for _, e := range ListReengagementCohortEpisodes(ctx, step) {
			facts[e.NetworkId] = e.Facts
		}
		gift, giftFound := facts[giftNetworkId]
		card, cardFound := facts[cardNetworkId]
		if !giftFound || !cardFound {
			t.Fatalf("the day's cohort holds the gift episode %t and the card episode %t", giftFound, cardFound)
		}
		if len(gift.PaidStarts) != 0 {
			t.Fatalf("the gift network's paid starts are %v, want none", gift.PaidStarts)
		}
		if len(card.PaidStarts) != 1 || !card.PaidStarts[0].Equal(cardStart.Truncate(time.Microsecond)) {
			t.Fatalf("the card network's paid starts are %v, want its purchase at %s", card.PaidStarts, cardStart)
		}
		matured := step.Add(reengagement.WindowProRestart30d + time.Hour)
		if reengagement.ComputeEpisodeOutcomes(gift, matured).ProRestart30d {
			t.Fatal("a redeemed gift counts as the re-engaged network's Pro restart")
		}
		if !reengagement.ComputeEpisodeOutcomes(card, matured).ProRestart30d {
			t.Fatal("a card purchase in the window is no Pro restart")
		}
	})
}

// The other places a purchase is counted leave a gift year out, while it still
// marks when Pro stopped: the last paid end that CASL's purchase window reads,
// the last paid window a win-back offer's store comes from, the win-back
// candidates (a gift-only recipient is none; a payer whose gift year ended is
// one, on the grace markets' timing), the onboarding results' first Pro, and
// the email tracker's pro_started.
func TestGiftRedemptionIsNoPurchaseOfTheNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Second)

		// a recipient that never paid, its gift year ended 35 days ago
		giftEnd := now.Add(-35 * 24 * time.Hour)
		giftOnly := winbackTestNetwork(t, ctx)
		addWinbackTestRenewal(t, ctx, giftOnly, SubscriptionMarketGift, giftEnd.Add(-giftTestYear), giftEnd, UsdToNanoCents(39.99), server.NewId().String())
		// a network that paid by card, then had a gift year to the same end
		stripeStart := giftEnd.Add(-giftTestYear - 60*24*time.Hour)
		stripeEnd := giftEnd.Add(-giftTestYear)
		paidThenGift := winbackTestNetwork(t, ctx)
		addWinbackTestRenewal(t, ctx, paidThenGift, SubscriptionMarketStripe, stripeStart, stripeEnd, UsdToNanoCents(5), "synthetic-invoice-"+paidThenGift.String())
		addWinbackTestRenewal(t, ctx, paidThenGift, SubscriptionMarketGift, stripeEnd, giftEnd, UsdToNanoCents(29.99), server.NewId().String())

		if end := NetworkLastPaidEnd(ctx, giftOnly); end != nil {
			t.Fatalf("a gift-only network's last paid end is %s", end)
		}
		if end := NetworkLastPaidEnd(ctx, paidThenGift); end == nil || !end.Equal(stripeEnd) {
			t.Fatalf("the payer's last paid end is %v, want its card window's %s", end, stripeEnd)
		}
		if _, _, ok := NetworkLastPaidWindow(ctx, giftOnly); ok {
			t.Fatal("a gift-only network has a paid window")
		}
		if end, market, ok := NetworkLastPaidWindow(ctx, paidThenGift); !ok || market != SubscriptionMarketStripe || !end.Equal(stripeEnd) {
			t.Fatalf("the payer's last paid window is %s %s %t, want its card window", end, market, ok)
		}
		if end, ok := NetworkLastGiftWindowEnd(ctx, paidThenGift); !ok || !end.Equal(giftEnd) {
			t.Fatalf("the payer's gift year ends %s %t, want %s", end, ok, giftEnd)
		}

		// CASL: a purchase within two years is implied consent; a gift is none
		policy := onboarding.MailPolicy{}
		created := now.Add(-3 * 365 * 24 * time.Hour)
		if exit := policy.Exit(onboarding.PurposeReengagement, onboarding.MailFacts{Country: "CA", CreatedAt: created, LastPaidEnd: NetworkLastPaidEnd(ctx, giftOnly), Now: now}); exit != onboarding.ExitCanadaConsent {
			t.Fatalf("a Canadian gift-only network may be mailed (%q)", exit)
		}
		if exit := policy.Exit(onboarding.PurposeReengagement, onboarding.MailFacts{Country: "CA", CreatedAt: created, LastPaidEnd: NetworkLastPaidEnd(ctx, paidThenGift), Now: now}); exit != "" {
			t.Fatalf("a Canadian payer within two years of its purchase is refused (%q)", exit)
		}

		candidates := func(grace bool) map[server.Id]bool {
			found := map[server.Id]bool{}
			for _, c := range ListWinbackReengagementCandidates(ctx, giftEnd.Add(-time.Hour), giftEnd.Add(time.Hour), grace, 100) {
				found[c.NetworkId] = true
			}
			return found
		}
		graceCandidates, storeCandidates := candidates(true), candidates(false)
		if graceCandidates[giftOnly] || storeCandidates[giftOnly] {
			t.Fatal("a gift-only recipient is a win-back candidate")
		}
		if !graceCandidates[paidThenGift] || storeCandidates[paidThenGift] {
			t.Fatalf("the payer whose gift year ended is a grace candidate %t and a store candidate %t, want the grace timing alone", graceCandidates[paidThenGift], storeCandidates[paidThenGift])
		}

		// the onboarding results' first Pro is the network's own
		facts := LoadOnboardingCohortFacts(ctx, []*NetworkOnboarding{{NetworkId: giftOnly}, {NetworkId: paidThenGift}})
		if f := facts[giftOnly]; f == nil || f.FirstProAt != nil {
			t.Fatalf("a gift-only network's first Pro is %+v", f)
		}
		if f := facts[paidThenGift]; f == nil || f.FirstProAt == nil || !f.FirstProAt.Equal(stripeStart) {
			t.Fatalf("the payer's first Pro is %+v, want its card window's start %s", f, stripeStart)
		}

		// the email tracker: one E1 to each of two networks; one redeems a
		// gift after it, the other buys by card
		day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		trackedGift := server.NewId()
		trackedCard := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, email := range []*NetworkOnboardingEmail{
				{MessageId: "synthetic-gift-e1", NetworkId: trackedGift, Step: onboarding.StepE1, Template: onboarding.TemplateE1Connect, Variant: onboarding.VariantDefault, SentAt: day.Add(8 * time.Hour)},
				{MessageId: "synthetic-card-e1", NetworkId: trackedCard, Step: onboarding.StepE1, Template: onboarding.TemplateE1Connect, Variant: onboarding.VariantDefault, SentAt: day.Add(8 * time.Hour)},
			} {
				AddNetworkOnboardingEmailInTx(tx, ctx, email)
			}
		})
		addWinbackTestRenewal(t, ctx, trackedGift, SubscriptionMarketGift, day.Add(2*24*time.Hour), day.Add(2*24*time.Hour+giftTestYear), UsdToNanoCents(39.99), server.NewId().String())
		addWinbackTestRenewal(t, ctx, trackedCard, SubscriptionMarketStripe, day.Add(2*24*time.Hour), day.Add(32*24*time.Hour), UsdToNanoCents(5), "synthetic-invoice-"+trackedCard.String())
		if _, err := RebuildOnboardingEmailTracker(ctx, day, day.Add(9*24*time.Hour), day.Add(30*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		e1 := SumOnboardingEmailTracker(ctx, day, day.Add(9*24*time.Hour))[onboarding.StepE1]
		if e1 == nil || e1.Sent != 2 || e1.ProStarted != 1 {
			t.Fatalf("the E1 sends tally %+v, want 2 sent and 1 Pro started (the card's)", e1)
		}
	})
}
