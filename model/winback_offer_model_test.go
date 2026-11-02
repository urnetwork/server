package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/reengagement"
)

// The pure parts: an offer is active until it is redeemed or expires.
func TestWinbackOfferActive(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	offer := &WinbackOffer{IssuedAt: now, ExpiresAt: now.Add(14 * 24 * time.Hour)}
	connect.AssertEqual(t, true, offer.Active(now))
	connect.AssertEqual(t, false, offer.Active(offer.ExpiresAt))
	redeemed := now.Add(time.Hour)
	offer.RedeemedAt = &redeemed
	connect.AssertEqual(t, false, offer.Active(now))
	var none *WinbackOffer
	connect.AssertEqual(t, false, none.Active(now))
}

// The comeback offer's config: off unless enabled, refused whole when its
// shape breaks the rule over every offer, each store's handle, and Apple's
// direct link only in its documented form.
func TestReengagementWinbackOfferConfig(t *testing.T) {
	tiers := []*ProPriceTier{
		{Name: PriceTierStandard, MonthlyUsd: 5, YearlyUsd: 40},
		{Name: "regional", MonthlyUsd: 0.5, YearlyUsd: 4},
	}
	accepted := ReengagementWinbackOfferConfig{
		Enabled:        true,
		PercentOff:     50,
		Plan:           PlanMonthly,
		ValidityDays:   14,
		StripeCouponId: "winback_monthly_50",
		PlayOfferTag:   "winback-monthly-50",
		AppleOfferId:   "winback_monthly_50",
		AppleOfferUrl:  "https://apps.apple.com/win-back/6755366859",
	}
	c := NewReengagementConfigForTest(&ReengagementConfig{WinbackOffer: accepted})
	connect.AssertEqual(t, "", c.winbackOfferRefusal(25, tiers))
	connect.AssertEqual(t, 14*24*time.Hour, c.WinbackOfferValidity())
	connect.AssertEqual(t, "winback_monthly_50", c.WinbackStoreHandle(reengagement.WinbackStoreStripe))
	connect.AssertEqual(t, "winback-monthly-50", c.WinbackStoreHandle(reengagement.WinbackStorePlay))
	connect.AssertEqual(t, "winback_monthly_50", c.WinbackStoreHandle(reengagement.WinbackStoreApple))
	connect.AssertEqual(t, "", c.WinbackStoreHandle("solana"))
	connect.AssertEqual(t, "https://apps.apple.com/win-back/6755366859", c.WinbackAppleOfferUrl())

	for name, change := range map[string]func(w *ReengagementWinbackOfferConfig){
		"disabled":         func(w *ReengagementWinbackOfferConfig) { w.Enabled = false },
		"no deadline":      func(w *ReengagementWinbackOfferConfig) { w.ValidityDays = 0 },
		"yearly":           func(w *ReengagementWinbackOfferConfig) { w.Plan = PlanYearly },
		"more than half":   func(w *ReengagementWinbackOfferConfig) { w.PercentOff = 75 },
		"no plan at all":   func(w *ReengagementWinbackOfferConfig) { w.Plan = "" },
		"negative percent": func(w *ReengagementWinbackOfferConfig) { w.PercentOff = -50 },
	} {
		w := accepted
		change(&w)
		changed := NewReengagementConfigForTest(&ReengagementConfig{WinbackOffer: w})
		if changed.winbackOfferRefusal(25, tiers) == "" {
			t.Errorf("%s: the offer is not refused", name)
		}
	}
	// a tier where half a month is worth a quarter of a year breaks the rule
	dear := []*ProPriceTier{{Name: "dear", MonthlyUsd: 20, YearlyUsd: 40}}
	if c.winbackOfferRefusal(25, dear) == "" {
		t.Error("an offer equal to the welcome offer is not refused")
	}
	// an unpriced environment (no pro.yml) issues nothing
	if c.winbackOfferRefusal(25, []*ProPriceTier{{Name: PriceTierStandard}}) == "" {
		t.Error("an unpriced tier is not refused")
	}

	for _, link := range []string{
		"",
		"https://apps.apple.com/win-back/",
		"https://apps.apple.com/win-back/abc",
		"http://apps.apple.com/win-back/123",
		"https://apps.apple.com/redeem?ctx=offercodes&id=1&code=X",
		"https://example.com/win-back/123",
	} {
		w := accepted
		w.AppleOfferUrl = link
		if got := NewReengagementConfigForTest(&ReengagementConfig{WinbackOffer: w}).WinbackAppleOfferUrl(); got != "" {
			t.Errorf("link %q accepted as %q", link, got)
		}
	}
}

func winbackTestNetwork(t testing.TB, ctx context.Context) server.Id {
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "winback-"+networkId.String(), server.NewId())
	return networkId
}

func issueWinbackTestOffer(t testing.TB, ctx context.Context, networkId server.Id, store string, issuedAt time.Time) (*WinbackOffer, bool) {
	args := &IssueWinbackOfferArgs{
		NetworkId:  networkId,
		Store:      store,
		Plan:       PlanMonthly,
		PercentOff: 50,
		IssuedAt:   issuedAt,
		ExpiresAt:  issuedAt.Add(14 * 24 * time.Hour),
	}
	switch store {
	case reengagement.WinbackStoreStripe:
		args.StripePromotionCodeId = "promo_" + networkId.String()
	case reengagement.WinbackStorePlay:
		args.PlayOfferTag = "winback-monthly-50"
	case reengagement.WinbackStoreApple:
		args.AppleOfferId = "winback_monthly_50"
	}
	offer, created, err := IssueWinbackOffer(ctx, args)
	if err != nil {
		t.Fatalf("issue: %s", err)
	}
	return offer, created
}

// One offer per network per 365 days, whatever became of the first: a second
// issue inside the interval returns the first unchanged, one after it records
// a new offer; a deleted network gets none.
func TestIssueWinbackOfferOncePerInterval(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := winbackTestNetwork(t, ctx)
		issuedAt := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

		first, created := issueWinbackTestOffer(t, ctx, networkId, reengagement.WinbackStorePlay, issuedAt)
		connect.AssertEqual(t, true, created)
		connect.AssertEqual(t, reengagement.WinbackStorePlay, first.Store)
		connect.AssertEqual(t, PlanMonthly, first.Plan)
		connect.AssertEqual(t, 50, first.PercentOff)
		connect.AssertEqual(t, true, first.PlayOfferTag != nil && *first.PlayOfferTag == "winback-monthly-50")
		connect.AssertEqual(t, true, first.StripePromotionCodeId == nil && first.AppleOfferId == nil)
		connect.AssertEqual(t, true, first.ExpiresAt.Equal(issuedAt.Add(14*24*time.Hour)))

		again, created := issueWinbackTestOffer(t, ctx, networkId, reengagement.WinbackStoreStripe, issuedAt.Add(time.Minute))
		connect.AssertEqual(t, false, created)
		connect.AssertEqual(t, first.OfferId, again.OfferId)
		connect.AssertEqual(t, reengagement.WinbackStorePlay, again.Store)

		// expired, still inside the year: nothing new
		late, created := issueWinbackTestOffer(t, ctx, networkId, reengagement.WinbackStorePlay, issuedAt.Add(364*24*time.Hour))
		connect.AssertEqual(t, false, created)
		connect.AssertEqual(t, first.OfferId, late.OfferId)
		connect.AssertEqual(t, false, late.Active(issuedAt.Add(364*24*time.Hour)))

		next, created := issueWinbackTestOffer(t, ctx, networkId, reengagement.WinbackStoreStripe, issuedAt.Add(reengagement.WinbackOfferInterval))
		connect.AssertEqual(t, true, created)
		connect.AssertEqual(t, true, next.OfferId != first.OfferId)
		connect.AssertEqual(t, next.OfferId, GetLatestWinbackOffer(ctx, networkId).OfferId)

		_, _, err := IssueWinbackOffer(ctx, &IssueWinbackOfferArgs{
			NetworkId: server.NewId(), Store: reengagement.WinbackStorePlay, Plan: PlanMonthly, PercentOff: 50,
			IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Hour), PlayOfferTag: "winback-monthly-50",
		})
		connect.AssertEqual(t, ErrWinbackNetworkNotFound, err)
	})
}

// A purchase redeems the newest unredeemed offer of its store when it falls
// inside the offer (from the issue less the skew to the expiry), once; the
// plan response's active offer is gone after it.
func TestRedeemWinbackOffer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := winbackTestNetwork(t, ctx)
		issuedAt := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
		offer, _ := issueWinbackTestOffer(t, ctx, networkId, reengagement.WinbackStoreStripe, issuedAt)
		connect.AssertEqual(t, offer.OfferId, GetActiveWinbackOffer(ctx, networkId, issuedAt.Add(time.Hour)).OfferId)

		redeem := func(store string, offerId *server.Id, at time.Time) (redeemed *server.Id) {
			server.Tx(ctx, func(tx server.PgTx) {
				redeemed = RedeemWinbackOfferInTx(tx, ctx, networkId, store, offerId, at)
			})
			return
		}
		// another store's purchase, one before the issue, one after the expiry
		connect.AssertEqual(t, true, redeem(reengagement.WinbackStorePlay, nil, issuedAt.Add(time.Hour)) == nil)
		connect.AssertEqual(t, true, redeem(reengagement.WinbackStoreStripe, nil, issuedAt.Add(-WinbackOfferIssueSkew-time.Second)) == nil)
		connect.AssertEqual(t, true, redeem(reengagement.WinbackStoreStripe, nil, offer.ExpiresAt) == nil)
		other := server.NewId()
		connect.AssertEqual(t, true, redeem(reengagement.WinbackStoreStripe, &other, issuedAt.Add(time.Hour)) == nil)

		// a store clock a little behind ours still redeems
		at := issuedAt.Add(-time.Minute)
		redeemed := redeem(reengagement.WinbackStoreStripe, &offer.OfferId, at)
		connect.AssertEqual(t, true, redeemed != nil && *redeemed == offer.OfferId)
		connect.AssertEqual(t, true, redeem(reengagement.WinbackStoreStripe, nil, issuedAt.Add(time.Hour)) == nil)

		stored := GetWinbackOffer(ctx, offer.OfferId)
		connect.AssertEqual(t, true, stored.RedeemedAt != nil && stored.RedeemedAt.Equal(at))
		connect.AssertEqual(t, true, GetActiveWinbackOffer(ctx, networkId, issuedAt.Add(time.Hour)) == nil)
		var latest *WinbackOffer
		server.Tx(ctx, func(tx server.PgTx) {
			latest = LatestWinbackOfferForStoreInTx(tx, ctx, networkId, reengagement.WinbackStoreStripe)
		})
		connect.AssertEqual(t, offer.OfferId, latest.OfferId)
		// a redeemed offer's promotion code is never replaced
		connect.AssertEqual(t, false, SetWinbackOfferStripePromotionCode(ctx, offer.OfferId, "promo_replacement", issuedAt.Add(time.Hour)))
	})
}

// A used-up promotion code is replaced while the offer is active, never after.
func TestSetWinbackOfferStripePromotionCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := winbackTestNetwork(t, ctx)
		issuedAt := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
		offer, _ := issueWinbackTestOffer(t, ctx, networkId, reengagement.WinbackStoreStripe, issuedAt)
		connect.AssertEqual(t, true, SetWinbackOfferStripePromotionCode(ctx, offer.OfferId, "promo_second", issuedAt.Add(time.Hour)))
		connect.AssertEqual(t, "promo_second", *GetWinbackOffer(ctx, offer.OfferId).StripePromotionCodeId)
		connect.AssertEqual(t, false, SetWinbackOfferStripePromotionCode(ctx, offer.OfferId, "promo_third", offer.ExpiresAt))

		playNetworkId := winbackTestNetwork(t, ctx)
		playOffer, _ := issueWinbackTestOffer(t, ctx, playNetworkId, reengagement.WinbackStorePlay, issuedAt)
		connect.AssertEqual(t, false, SetWinbackOfferStripePromotionCode(ctx, playOffer.OfferId, "promo_play", issuedAt.Add(time.Hour)))
	})
}

func addWinbackTestRenewal(t testing.TB, ctx context.Context, networkId server.Id, market string, start time.Time, end time.Time, revenue NanoCents, transactionId string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO subscription_renewal (
					network_id, subscription_type, start_time, end_time, net_revenue_nano_cents, purchase_token, market, transaction_id
				)
				VALUES ($1, 'supporter', $2, $3, $4, $5, $6, NULLIF($7, ''))
			`,
			networkId,
			start,
			end,
			revenue,
			"token-"+networkId.String(),
			market,
			transactionId,
		))
	})
}

// Paid means charged at least once and nothing given back: a Stripe trial
// invoice, a Play window no longer than a free phase and an App Store
// transaction priced zero are not payments, and any refund, dispute or store
// revocation makes the network unpaid.
func TestNetworkHasPaidSupporterPeriod(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		revenue := UsdToNanoCents(4)

		never := winbackTestNetwork(t, ctx)
		connect.AssertEqual(t, false, NetworkHasPaidSupporterPeriod(ctx, never))

		stripeTrial := winbackTestNetwork(t, ctx)
		addWinbackTestRenewal(t, ctx, stripeTrial, SubscriptionMarketStripe, start, start.Add(44*24*time.Hour), 0, "in_trial")
		connect.AssertEqual(t, false, NetworkHasPaidSupporterPeriod(ctx, stripeTrial))

		stripePaid := winbackTestNetwork(t, ctx)
		addWinbackTestRenewal(t, ctx, stripePaid, SubscriptionMarketStripe, start, start.Add(61*24*time.Hour), revenue, "in_paid")
		connect.AssertEqual(t, true, NetworkHasPaidSupporterPeriod(ctx, stripePaid))

		// a Play trial records the plan's price over the 14 days and a day of slack
		playTrial := winbackTestNetwork(t, ctx)
		addWinbackTestRenewal(t, ctx, playTrial, SubscriptionMarketGoogle, start, start.Add(15*24*time.Hour), revenue, "")
		connect.AssertEqual(t, false, NetworkHasPaidSupporterPeriod(ctx, playTrial))

		playPaid := winbackTestNetwork(t, ctx)
		addWinbackTestRenewal(t, ctx, playPaid, SubscriptionMarketGoogle, start, start.Add(32*24*time.Hour), revenue, "")
		connect.AssertEqual(t, true, NetworkHasPaidSupporterPeriod(ctx, playPaid))

		addAppleLedger := func(networkId server.Id, transactionId string, price *int64) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO apple_subscription_transaction (transaction_id, notification_uuid, network_id, product_id, price_milliunits, currency)
						VALUES ($1, $2, $3, 'supporter_monthly_26', $4, 'USD')
					`,
					transactionId,
					server.NewId(),
					networkId,
					price,
				))
			})
		}
		// an App Store trial records a paid marker; its ledger price is zero
		appleTrial := winbackTestNetwork(t, ctx)
		zero := int64(0)
		trialTransaction := "apple-trial-" + appleTrial.String()
		addWinbackTestRenewal(t, ctx, appleTrial, SubscriptionMarketApple, start, start.Add(8*24*time.Hour), UsdToNanoCents(0.01), trialTransaction)
		addAppleLedger(appleTrial, trialTransaction, &zero)
		connect.AssertEqual(t, false, NetworkHasPaidSupporterPeriod(ctx, appleTrial))

		applePaid := winbackTestNetwork(t, ctx)
		price := int64(4990)
		paidTransaction := "apple-paid-" + applePaid.String()
		addWinbackTestRenewal(t, ctx, applePaid, SubscriptionMarketApple, start, start.Add(31*24*time.Hour), UsdToNanoCents(0.01), paidTransaction)
		addAppleLedger(applePaid, paidTransaction, &price)
		connect.AssertEqual(t, true, NetworkHasPaidSupporterPeriod(ctx, applePaid))

		// a refund, a dispute or a revocation on any market makes the network unpaid
		refunded := winbackTestNetwork(t, ctx)
		addWinbackTestRenewal(t, ctx, refunded, SubscriptionMarketStripe, start, start.Add(61*24*time.Hour), revenue, "in_refunded")
		connect.AssertEqual(t, true, NetworkHasPaidSupporterPeriod(ctx, refunded))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO stripe_refund (refund_id, charge_id, network_id, action) VALUES ($1, $2, $3, 'refunded')`,
				"re_"+refunded.String(), "ch_"+refunded.String(), refunded,
			))
		})
		connect.AssertEqual(t, false, NetworkHasPaidSupporterPeriod(ctx, refunded))

		revoked := winbackTestNetwork(t, ctx)
		addWinbackTestRenewal(t, ctx, revoked, SubscriptionMarketGoogle, start, start.Add(32*24*time.Hour), revenue, "")
		connect.AssertEqual(t, true, NetworkHasPaidSupporterPeriod(ctx, revoked))
		// a dry run's event is no revocation
		connect.AssertEqual(t, nil, AddPaymentReconciliationEvent(ctx, &PaymentReconciliationEvent{
			RunId: server.NewId(), Store: SubscriptionMarketGoogle, NetworkId: &revoked, Action: PaymentReconcileActionRevoked, Evidence: "token", DryRun: true,
		}))
		connect.AssertEqual(t, true, NetworkHasPaidSupporterPeriod(ctx, revoked))
		connect.AssertEqual(t, nil, AddPaymentReconciliationEvent(ctx, &PaymentReconciliationEvent{
			RunId: server.NewId(), Store: SubscriptionMarketGoogle, NetworkId: &revoked, Action: PaymentReconcileActionRevoked, Evidence: "token",
		}))
		connect.AssertEqual(t, false, NetworkHasPaidSupporterPeriod(ctx, revoked))

		// the App Store eligibility lookup reads the latest App Store transaction
		connect.AssertEqual(t, paidTransaction, NetworkLastAppleTransactionId(ctx, applePaid))
		connect.AssertEqual(t, "", NetworkLastAppleTransactionId(ctx, stripePaid))
	})
}
