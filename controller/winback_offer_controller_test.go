package controller

// The win-back offer (winback_offer_controller.go): the plan response's exact
// shape, the App Store eligibility read, the Stripe code checks, the Play
// "Resubscribe" binding, and, against the test database, the W1 issue on each
// store, the redemption on each store's purchase path and the landing's link.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"
	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/reengagement"
	"github.com/urnetwork/server/session"
)

func winbackJsonKeys(t *testing.T, value any) []string {
	t.Helper()
	data, err := json.Marshal(value)
	connect.AssertEqual(t, err, nil)
	fields := map[string]any{}
	connect.AssertEqual(t, json.Unmarshal(data, &fields), nil)
	keys := []string{}
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// The plan response's winback_offer is exactly the shape the apps code
// against: the store's own handle only, the expiry in RFC 3339, and nothing at
// all once the offer expired or was redeemed.
func TestWinbackOfferResultShape(t *testing.T) {
	issuedAt := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	tag, appleId, promo := "winback-monthly-50", "winback_monthly_50", "promo_1"
	offer := func(store string) *model.WinbackOffer {
		o := &model.WinbackOffer{
			OfferId: server.NewId(), NetworkId: server.NewId(), Store: store, Plan: model.PlanMonthly, PercentOff: 50,
			IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(14 * 24 * time.Hour),
		}
		switch store {
		case reengagement.WinbackStorePlay:
			o.PlayOfferTag = &tag
		case reengagement.WinbackStoreApple:
			o.AppleOfferId = &appleId
		case reengagement.WinbackStoreStripe:
			o.StripePromotionCodeId = &promo
		}
		return o
	}
	now := issuedAt.Add(time.Hour)

	play := winbackOfferResult(offer(reengagement.WinbackStorePlay), now)
	connect.AssertEqual(t, winbackJsonKeys(t, play), []string{"expires_at", "percent_off", "plan", "play_offer_tag", "store"})
	data, _ := json.Marshal(play)
	connect.AssertEqual(t, string(data), `{"expires_at":"2026-11-16T10:00:00Z","percent_off":50,"plan":"monthly","store":"play","play_offer_tag":"winback-monthly-50"}`)

	apple := winbackOfferResult(offer(reengagement.WinbackStoreApple), now)
	connect.AssertEqual(t, winbackJsonKeys(t, apple), []string{"apple_offer_id", "expires_at", "percent_off", "plan", "store"})
	connect.AssertEqual(t, apple.AppleOfferId, appleId)

	// the promotion code never leaves the server
	stripeResult := winbackOfferResult(offer(reengagement.WinbackStoreStripe), now)
	connect.AssertEqual(t, winbackJsonKeys(t, stripeResult), []string{"expires_at", "percent_off", "plan", "store"})

	expired := offer(reengagement.WinbackStorePlay)
	connect.AssertEqual(t, winbackOfferResult(expired, expired.ExpiresAt) == nil, true)
	redeemed := offer(reengagement.WinbackStorePlay)
	redeemedAt := now
	redeemed.RedeemedAt = &redeemedAt
	connect.AssertEqual(t, winbackOfferResult(redeemed, now) == nil, true)
	connect.AssertEqual(t, winbackOfferResult(nil, now) == nil, true)

	// the plan response leaves out a Stripe offer at a tier where Stripe
	// would not charge the half month, and keeps the stores' own offers
	standard := &model.ProPriceTier{Name: model.PriceTierStandard, MonthlyUsd: 5, YearlyUsd: 40}
	regional := &model.ProPriceTier{Name: "regional", MonthlyUsd: 0.5, YearlyUsd: 4}
	connect.AssertNotEqual(t, winbackPlanOfferResult(offer(reengagement.WinbackStoreStripe), standard, now), nil)
	connect.AssertEqual(t, winbackPlanOfferResult(offer(reengagement.WinbackStoreStripe), regional, now) == nil, true)
	connect.AssertNotEqual(t, winbackPlanOfferResult(offer(reengagement.WinbackStorePlay), regional, now), nil)
	connect.AssertNotEqual(t, winbackPlanOfferResult(offer(reengagement.WinbackStoreApple), regional, now), nil)
	connect.AssertEqual(t, winbackPlanOfferResult(expired, standard, expired.ExpiresAt) == nil, true)

	// omitted from the plan response when there is none, present beside
	// onboarding_offer when there is one
	balance := &SubscriptionBalanceResult{}
	connect.AssertEqual(t, slices.Contains(winbackJsonKeys(t, balance), "winback_offer"), false)
	connect.AssertEqual(t, slices.Contains(winbackJsonKeys(t, balance), "onboarding_offer"), true)
	balance.WinbackOffer = play
	connect.AssertEqual(t, slices.Contains(winbackJsonKeys(t, balance), "winback_offer"), true)
}

// The eligible offers are read from each subscription's renewal info, in
// Apple's order; a response Apple does not document lists none.
func TestAppleEligibleWinbackOfferIds(t *testing.T) {
	statuses := &appleSubscriptionStatusesResponse{Data: []*appleSubscriptionGroup{{
		SubscriptionGroupIdentifier: "21000000",
		LastTransactions: []*appleLastTransaction{
			{Status: 2, OriginalTransactionId: "1", SignedRenewalInfo: appleTestJws(t, map[string]any{
				"autoRenewStatus":         0,
				"eligibleWinBackOfferIds": []string{"winback_monthly_50", "winback_other"},
			})},
			{Status: 2, OriginalTransactionId: "2", SignedRenewalInfo: appleTestJws(t, map[string]any{"autoRenewStatus": 0})},
			{Status: 2, OriginalTransactionId: "3", SignedRenewalInfo: "not a jws"},
			{Status: 2, OriginalTransactionId: "4"},
			// the API reference names the transaction info; a repeat is listed once
			{Status: 2, OriginalTransactionId: "5", SignedTransactionInfo: appleTestJws(t, map[string]any{
				"eligibleWinBackOfferIds": []string{"winback_other", "winback_third"},
			})},
		},
	}}}
	connect.AssertEqual(t, appleEligibleWinbackOfferIds(statuses), []string{"winback_monthly_50", "winback_other", "winback_third"})

	statuses.Data[0].LastTransactions[1].Status = 9
	connect.AssertEqual(t, len(appleEligibleWinbackOfferIds(statuses)), 0)
	connect.AssertEqual(t, len(appleEligibleWinbackOfferIds(nil)), 0)
}

// A rate limit, Apple's 5xx or a transport failure retries the step; a 4xx or
// missing credentials mails W1 without the App Store line.
func TestAppleWinbackErrorPasses(t *testing.T) {
	connect.AssertEqual(t, appleWinbackErrorPasses(&server.HttpStatusError{StatusCode: http.StatusTooManyRequests}), true)
	connect.AssertEqual(t, appleWinbackErrorPasses(&server.HttpStatusError{StatusCode: http.StatusServiceUnavailable}), true)
	connect.AssertEqual(t, appleWinbackErrorPasses(errors.New("connection reset")), true)
	connect.AssertEqual(t, appleWinbackErrorPasses(&server.HttpStatusError{StatusCode: http.StatusUnauthorized}), false)
	connect.AssertEqual(t, appleWinbackErrorPasses(&server.HttpStatusError{StatusCode: http.StatusNotFound}), false)
	connect.AssertEqual(t, appleWinbackErrorPasses(errAppleWinbackNoCredentials), false)
}

// A promotion code is applied only while Stripe will honor it for the
// customer checking out; anything else is replaced.
func TestStripeWinbackPromotionCodeUsable(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	usable := func() *stripe.PromotionCode {
		return &stripe.PromotionCode{
			ID: "promo_1", Active: true, MaxRedemptions: 1, TimesRedeemed: 0,
			ExpiresAt: now.Add(time.Hour).Unix(), Customer: &stripe.Customer{ID: "cus_1"},
		}
	}
	connect.AssertEqual(t, stripeWinbackPromotionCodeUsable(usable(), "cus_1", now), true)
	for name, change := range map[string]func(p *stripe.PromotionCode){
		"inactive":          func(p *stripe.PromotionCode) { p.Active = false },
		"used up":           func(p *stripe.PromotionCode) { p.TimesRedeemed = 1 },
		"expired":           func(p *stripe.PromotionCode) { p.ExpiresAt = now.Unix() },
		"another customer":  func(p *stripe.PromotionCode) { p.Customer = &stripe.Customer{ID: "cus_2"} },
		"for every account": func(p *stripe.PromotionCode) { p.Customer = nil },
	} {
		p := usable()
		change(p)
		if stripeWinbackPromotionCodeUsable(p, "cus_1", now) {
			t.Errorf("%s: the code is applied", name)
		}
	}
	connect.AssertEqual(t, stripeWinbackPromotionCodeUsable(nil, "cus_1", now), false)
}

// A coupon under the configured id stands for the offer only when it is the
// offer: percent_off off one invoice.
func TestStripeWinbackCouponRefusal(t *testing.T) {
	connect.AssertEqual(t, stripeWinbackCouponRefusal(&stripe.Coupon{PercentOff: 50, Duration: stripe.CouponDurationOnce}, 50), "")
	for name, c := range map[string]*stripe.Coupon{
		"another percent": {PercentOff: 25, Duration: stripe.CouponDurationOnce},
		"repeating":       {PercentOff: 50, Duration: stripe.CouponDurationRepeating, DurationInMonths: 3},
		"forever":         {PercentOff: 50, Duration: stripe.CouponDurationForever},
		"an amount off":   {PercentOff: 50, AmountOff: 100, Duration: stripe.CouponDurationOnce},
	} {
		if stripeWinbackCouponRefusal(c, 50) == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

// resubscribedPurchase is a Play Store "Resubscribe": no account identifiers
// of its own, the expired subscription's in outOfAppPurchaseContext.
func (self *playBindingFakeEnv) resubscribedPurchase(purchaseToken string, expiredNetworkId server.Id) *PlaySubscription {
	startTime := playBindingNow.Add(-time.Hour)
	sub := &PlaySubscription{
		LineItems: []*PlaySubscriptionPurchaseLineItem{{
			ProductId:  playBindingTestProductId,
			ExpiryTime: startTime.Add(30 * 24 * time.Hour).Format(time.RFC3339),
		}},
		StartTime:            startTime.Format(time.RFC3339),
		SubscriptionState:    "SUBSCRIPTION_STATE_ACTIVE",
		AcknowledgementState: "ACKNOWLEDGEMENT_STATE_PENDING",
		OutOfAppPurchaseContext: &PlayOutOfAppPurchaseContext{
			ExpiredExternalAccountIdentifiers: &PlayExternalAccountIdentifiers{
				ObfuscatedExternalAccountId: expiredNetworkId.String(),
			},
			ExpiredPurchaseToken: "expired-" + purchaseToken,
		},
	}
	self.play.subscriptions[purchaseToken] = sub
	return sub
}

// A Play Store "Resubscribe" reported by the network the expired subscription
// was bought for binds through its account identifiers and is credited, with
// no welcome offer involved; reported by any other network it is that
// network's wrong network, and nothing is bound or credited.
func TestVerifyPlayResubscribeBindsThroughExpiredAccountId(t *testing.T) {
	env := installPlayBindingFakes(t)
	clientSession, networkId := env.addNetwork(false, 0)
	strangerSession, _ := env.addNetwork(false, 0)
	env.resubscribedPurchase("resubscribe-token-1", networkId)

	connect.AssertEqual(t, env.verify(t, strangerSession, "resubscribe-token-1"), VerifyPurchaseStatusWrongNetwork)
	connect.AssertEqual(t, len(env.state.bindings), 0)
	connect.AssertEqual(t, env.creditedNetworks(), []server.Id{})

	connect.AssertEqual(t, env.verify(t, clientSession, "resubscribe-token-1"), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, env.state.bindings["resubscribe-token-1"], playBindingFakeBinding{
		networkId: networkId,
		root:      "resubscribe-token-1",
		offer:     model.PlayPurchaseBindingResubscribe,
	})
	connect.AssertEqual(t, env.creditedNetworks(), []server.Id{networkId})
	// the retry is terminal and credits nothing twice
	connect.AssertEqual(t, env.verify(t, clientSession, "resubscribe-token-1"), VerifyPurchaseStatusAlreadyCredited)
	connect.AssertEqual(t, len(env.state.ledger), 1)

	// identifiers that do not parse are not a purchase this app launched
	sub := env.resubscribedPurchase("resubscribe-token-2", networkId)
	sub.OutOfAppPurchaseContext.ExpiredExternalAccountIdentifiers.ObfuscatedExternalAccountId = "not-an-id"
	connect.AssertEqual(t, env.verify(t, clientSession, "resubscribe-token-2"), VerifyPurchaseStatusInvalid)
}

// ----- against the test database -----

const winbackTestAppleUrl = "https://apps.apple.com/win-back/6755366859"

func winbackTestConfig() *model.ReengagementConfig {
	return &model.ReengagementConfig{
		Enabled: true,
		WinbackOffer: model.ReengagementWinbackOfferConfig{
			Enabled:        true,
			PercentOff:     50,
			Plan:           model.PlanMonthly,
			ValidityDays:   14,
			StripeCouponId: "winback_monthly_50",
			PlayOfferTag:   "winback-monthly-50",
			AppleOfferId:   "winback_monthly_50",
			AppleOfferUrl:  winbackTestAppleUrl,
		},
	}
}

type winbackTestEnv struct {
	promotionCodes   []time.Time
	promotionCodeErr error
	appleEligible    bool
	appleErr         error
	appleAsked       int
}

// winbackTestTiers are pro.yml's two tiers: the standard tier for a few
// named countries, the regional tier for every other one.
func winbackTestTiers() func() {
	return model.Testing_SetProPriceTiers(
		model.Testing_ProPriceTier(model.PriceTierStandard, 5, 40, "US", "CA", "DE", "GB"),
		model.Testing_ProPriceTier("regional", 0.5, 4),
	)
}

// installWinbackTestEnv turns the offer on (the accepted shape, at $5 / $40
// and $0.50 / $4) and replaces the two store calls the issue makes.
func installWinbackTestEnv(t testing.TB) *winbackTestEnv {
	env := &winbackTestEnv{}
	restoreConfig := model.Testing_SetReengagementConfig(winbackTestConfig())
	restorePrices := winbackTestTiers()
	previousPromotionCode := stripeWinbackPromotionCodeForNetworkFunc
	previousApple := appleWinbackOfferEligibleFunc
	t.Cleanup(func() {
		restoreConfig()
		restorePrices()
		stripeWinbackPromotionCodeForNetworkFunc = previousPromotionCode
		appleWinbackOfferEligibleFunc = previousApple
	})
	stripeWinbackPromotionCodeForNetworkFunc = func(ctx context.Context, networkId server.Id, expiresAt time.Time) (string, error) {
		if env.promotionCodeErr != nil {
			return "", env.promotionCodeErr
		}
		env.promotionCodes = append(env.promotionCodes, expiresAt)
		return "promo_" + networkId.String(), nil
	}
	appleWinbackOfferEligibleFunc = func(ctx context.Context, networkId server.Id, offerId string) (bool, error) {
		env.appleAsked++
		connect.AssertEqual(t, offerId, "winback_monthly_50")
		return env.appleEligible, env.appleErr
	}
	return env
}

// winbackPaidNetwork is a network whose last paid window is on `market`,
// lasting long enough to be a payment on every market.
func winbackPaidNetwork(t testing.TB, ctx context.Context, market string, now time.Time) server.Id {
	networkId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "winback-"+networkId.String(), server.NewId())
	start := now.Add(-120 * 24 * time.Hour)
	end := now.Add(-70 * 24 * time.Hour)
	transactionId := ""
	if market == model.SubscriptionMarketApple {
		transactionId = "apple-" + networkId.String()
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO subscription_renewal (
					network_id, subscription_type, start_time, end_time, net_revenue_nano_cents, purchase_token, market, transaction_id
				)
				VALUES ($1, 'supporter', $2, $3, $4, $5, $6, NULLIF($7, ''))
			`,
			networkId, start, end, model.UsdToNanoCents(4), "token-"+networkId.String(), market, transactionId,
		))
		if transactionId != "" {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO apple_subscription_transaction (transaction_id, notification_uuid, network_id, product_id, price_milliunits, currency)
					VALUES ($1, $2, $3, 'supporter_monthly_26', 4990, 'USD')
				`,
				transactionId, server.NewId(), networkId,
			))
		}
	})
	return networkId
}

func winbackTestRow(networkId server.Id, arm string, now time.Time) *model.NetworkReengagement {
	return &model.NetworkReengagement{
		NetworkId:    networkId,
		Stream:       reengagement.StreamWinback,
		Episode:      1,
		StartedAt:    now.Add(-time.Hour),
		Country:      "US",
		ExperimentId: "reengage_winback_1",
		Variant:      arm,
		NextStep:     reengagement.StepW1,
	}
}

var winbackTestDecision = reengagement.Decision{Action: reengagement.ActionSend, Template: reengagement.TemplateW1Winback, Variant: reengagement.VariantOffer}

// W1 issues the offer right before its email, on the store the network paid
// through: Google Play and Stripe to the offer arm, the App Store's to the
// networks Apple lists; a replayed step mails the offer it issued, and every
// constraint sends the email without one.
func TestIssueWinbackOfferForSend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := installWinbackTestEnv(t)
		now := server.NowUtc()

		// Google Play, the offer arm: the offer variant and the Play tag
		playNetworkId := winbackPaidNetwork(t, ctx, model.SubscriptionMarketGoogle, now)
		playRow := winbackTestRow(playNetworkId, reengagement.ArmOffer, now)
		connect.AssertEqual(t, winbackComebackOfferAllowed(winbackOfferFacts(ctx, playRow, now)), true)
		offer, decision, err := issueWinbackOfferForSend(ctx, playRow, winbackTestDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, decision.Variant, reengagement.VariantOffer)
		connect.AssertEqual(t, offer.Store, reengagement.WinbackStorePlay)
		connect.AssertEqual(t, *offer.PlayOfferTag, "winback-monthly-50")
		connect.AssertEqual(t, offer.ExpiresAt.Sub(offer.IssuedAt), 14*24*time.Hour)
		connect.AssertEqual(t, offer.IssuedAt.Equal(offer.IssuedAt.Truncate(time.Second)), true)
		// the plan response now carries it
		connect.AssertEqual(t, winbackOfferResult(model.GetActiveWinbackOffer(ctx, playNetworkId, now), now).PlayOfferTag, "winback-monthly-50")
		// a replayed W1 (its send failed) mails the same offer
		replayed, decision, err := issueWinbackOfferForSend(ctx, playRow, winbackTestDecision, now.Add(time.Minute))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, replayed.OfferId, offer.OfferId)
		connect.AssertEqual(t, decision.Variant, reengagement.VariantOffer)
		// the next episode, inside the year, gets none
		nextRow := winbackTestRow(playNetworkId, reengagement.ArmOffer, now.Add(200*24*time.Hour))
		none, decision, err := issueWinbackOfferForSend(ctx, nextRow, winbackTestDecision, now.Add(200*24*time.Hour))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, none == nil, true)
		connect.AssertEqual(t, decision.Variant, reengagement.VariantDefault)

		// Google Play, the control arm: mailed without an offer
		controlNetworkId := winbackPaidNetwork(t, ctx, model.SubscriptionMarketGoogle, now)
		controlRow := winbackTestRow(controlNetworkId, reengagement.ArmControl, now)
		connect.AssertEqual(t, winbackComebackOfferAllowed(winbackOfferFacts(ctx, controlRow, now)), false)
		none, decision, err = issueWinbackOfferForSend(ctx, controlRow, winbackTestDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, none == nil, true)
		connect.AssertEqual(t, decision.Variant, reengagement.VariantDefault)
		connect.AssertEqual(t, model.GetLatestWinbackOffer(ctx, controlNetworkId) == nil, true)

		// Stripe: a failure to make the promotion code retries the step and
		// records nothing; then the code expires with the offer
		stripeNetworkId := winbackPaidNetwork(t, ctx, model.SubscriptionMarketStripe, now)
		stripeRow := winbackTestRow(stripeNetworkId, reengagement.ArmOffer, now)
		env.promotionCodeErr = errors.New("stripe is down")
		_, _, err = issueWinbackOfferForSend(ctx, stripeRow, winbackTestDecision, now)
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, model.GetLatestWinbackOffer(ctx, stripeNetworkId) == nil, true)
		env.promotionCodeErr = nil
		offer, decision, err = issueWinbackOfferForSend(ctx, stripeRow, winbackTestDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, decision.Variant, reengagement.VariantOffer)
		connect.AssertEqual(t, offer.Store, reengagement.WinbackStoreStripe)
		connect.AssertEqual(t, *offer.StripePromotionCodeId, "promo_"+stripeNetworkId.String())
		connect.AssertEqual(t, len(env.promotionCodes), 1)
		connect.AssertEqual(t, env.promotionCodes[0].Equal(offer.ExpiresAt), true)

		// App Store: every mailed arm, only when Apple lists the offer; the
		// email is the default variant with the App Store line
		appleNetworkId := winbackPaidNetwork(t, ctx, model.SubscriptionMarketApple, now)
		appleRow := winbackTestRow(appleNetworkId, reengagement.ArmControl, now)
		connect.AssertEqual(t, winbackComebackOfferAllowed(winbackOfferFacts(ctx, appleRow, now)), false)
		appleDecision := winbackTestDecision
		appleDecision.Variant = reengagement.VariantDefault
		env.appleEligible = false
		none, decision, err = issueWinbackOfferForSend(ctx, appleRow, appleDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, none == nil, true)
		connect.AssertEqual(t, decision.Variant, reengagement.VariantDefault)
		// a failure that may pass retries; a final one mails without the line
		env.appleErr = &server.HttpStatusError{StatusCode: http.StatusServiceUnavailable}
		_, _, err = issueWinbackOfferForSend(ctx, appleRow, appleDecision, now)
		connect.AssertNotEqual(t, err, nil)
		env.appleErr = &server.HttpStatusError{StatusCode: http.StatusUnauthorized}
		none, _, err = issueWinbackOfferForSend(ctx, appleRow, appleDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, none == nil, true)
		env.appleErr = nil
		env.appleEligible = true
		offer, decision, err = issueWinbackOfferForSend(ctx, appleRow, appleDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, decision.Variant, reengagement.VariantDefault)
		connect.AssertEqual(t, offer.Store, reengagement.WinbackStoreApple)
		connect.AssertEqual(t, *offer.AppleOfferId, "winback_monthly_50")
		// the landing opens Apple's link for that network only
		connect.AssertEqual(t, winbackLandingOpenUrl(ctx, appleNetworkId, now), winbackTestAppleUrl)
		connect.AssertEqual(t, winbackLandingOpenUrl(ctx, playNetworkId, now), "")
		// the holdout is never asked about
		asked := env.appleAsked
		holdoutRow := winbackTestRow(winbackPaidNetwork(t, ctx, model.SubscriptionMarketApple, now), reengagement.ArmHoldout, now)
		none, _, err = issueWinbackOfferForSend(ctx, holdoutRow, appleDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, none == nil, true)
		connect.AssertEqual(t, env.appleAsked, asked)

		// crypto, an unknown or no-offer country, a Stripe network on the
		// regional tier (half of 0.50 USD is below Stripe's minimum charge),
		// and a trial alone: no offer
		for name, row := range map[string]*model.NetworkReengagement{
			"crypto":           winbackTestRow(winbackPaidNetwork(t, ctx, model.SubscriptionMarketSolana, now), reengagement.ArmOffer, now),
			"unknown":          winbackTestRow(winbackPaidNetwork(t, ctx, model.SubscriptionMarketGoogle, now), reengagement.ArmOffer, now),
			"Iran":             winbackTestRow(winbackPaidNetwork(t, ctx, model.SubscriptionMarketStripe, now), reengagement.ArmOffer, now),
			"Brazil on Stripe": winbackTestRow(winbackPaidNetwork(t, ctx, model.SubscriptionMarketStripe, now), reengagement.ArmOffer, now),
		} {
			switch name {
			case "unknown":
				row.Country = ""
			case "Iran":
				row.Country = "IR"
			case "Brazil on Stripe":
				row.Country = "BR"
				facts := winbackOfferFacts(ctx, row, now)
				connect.AssertEqual(t, reengagement.WinbackOfferRefusal(facts), reengagement.WinbackRefusalBelowMinimumCharge)
			}
			none, decision, err := issueWinbackOfferForSend(ctx, row, winbackTestDecision, now)
			connect.AssertEqual(t, err, nil)
			if none != nil || decision.Variant != reengagement.VariantDefault {
				t.Errorf("%s: issued %+v", name, none)
			}
		}
		// Play prices its own regions: a Brazilian Play network gets the offer
		brazilPlayRow := winbackTestRow(winbackPaidNetwork(t, ctx, model.SubscriptionMarketGoogle, now), reengagement.ArmOffer, now)
		brazilPlayRow.Country = "BR"
		offer, _, err = issueWinbackOfferForSend(ctx, brazilPlayRow, winbackTestDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, offer, nil)
		connect.AssertEqual(t, offer.Store, reengagement.WinbackStorePlay)

		trialNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, trialNetworkId, "winbacktrial", server.NewId())
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO subscription_renewal (network_id, subscription_type, start_time, end_time, net_revenue_nano_cents, purchase_token, market)
					VALUES ($1, 'supporter', $2, $3, $4, 'trial-token', 'google')
				`,
				trialNetworkId, now.Add(-90*24*time.Hour), now.Add(-75*24*time.Hour), model.UsdToNanoCents(4),
			))
		})
		none, _, err = issueWinbackOfferForSend(ctx, winbackTestRow(trialNetworkId, reengagement.ArmOffer, now), winbackTestDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, none == nil, true)

		// turned off: nothing, whatever the arm
		model.Testing_SetReengagementConfig(&model.ReengagementConfig{Enabled: true})
		offNetworkId := winbackPaidNetwork(t, ctx, model.SubscriptionMarketGoogle, now)
		none, decision, err = issueWinbackOfferForSend(ctx, winbackTestRow(offNetworkId, reengagement.ArmOffer, now), winbackTestDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, none == nil, true)
		connect.AssertEqual(t, decision.Variant, reengagement.VariantDefault)
	})
}

// Each store's purchase path stamps the offer redeemed: a Stripe invoice of a
// subscription started with it, a Play credit under its tag, an App Store
// transaction of offer type 4 with its identifier. A purchase on another
// store, or before the offer, stamps nothing, and a renewal counts nothing.
func TestWinbackRedemptionOnEachStore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := installWinbackTestEnv(t)
		env.appleEligible = true
		now := server.NowUtc()
		issue := func(market string, arm string) (server.Id, *model.WinbackOffer) {
			networkId := winbackPaidNetwork(t, ctx, market, now)
			decision := winbackTestDecision
			if market == model.SubscriptionMarketApple {
				decision.Variant = reengagement.VariantDefault
			}
			offer, _, err := issueWinbackOfferForSend(ctx, winbackTestRow(networkId, arm, now), decision, now)
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, offer, nil)
			return networkId, offer
		}

		// Stripe: the subscription's metadata names the offer
		stripeNetworkId, stripeOffer := issue(model.SubscriptionMarketStripe, reengagement.ArmOffer)
		sub := &stripe.Subscription{
			ID:      "sub_winback",
			Created: now.Add(time.Minute).Unix(),
			Metadata: map[string]string{
				stripeMetadataNetworkId:    stripeNetworkId.String(),
				stripeMetadataWinbackOffer: stripeOffer.OfferId.String(),
			},
		}
		stripeRecordWinbackForInvoice(ctx, sub)
		stored := model.GetWinbackOffer(ctx, stripeOffer.OfferId)
		connect.AssertEqual(t, stored.RedeemedAt != nil, true)
		connect.AssertEqual(t, stored.RedeemedAt.Unix(), sub.Created)
		// the renewal invoice changes nothing
		stripeRecordWinbackForInvoice(ctx, sub)
		connect.AssertEqual(t, model.GetWinbackOffer(ctx, stripeOffer.OfferId).RedeemedAt.Unix(), sub.Created)

		// Google Play: a credit bought under the tag, at Google's startTime
		playNetworkId, playOffer := issue(model.SubscriptionMarketGoogle, reengagement.ArmOffer)
		taggedSub := &PlaySubscription{LineItems: []*PlaySubscriptionPurchaseLineItem{{
			ProductId:    "supporter",
			OfferDetails: &PlayOfferDetails{BasePlanId: "supporter-base", OfferId: "winback-monthly-50", OfferTags: []string{"winback-monthly-50"}},
		}}}
		untaggedSub := &PlaySubscription{LineItems: []*PlaySubscriptionPurchaseLineItem{{ProductId: "supporter"}}}
		credit := func(sub *PlaySubscription, networkId server.Id, startTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				playRecordWinbackInTx(tx, ctx, sub, &model.SubscriptionRenewal{NetworkId: networkId, StartTime: startTime})
			})
		}
		credit(untaggedSub, playNetworkId, now.Add(time.Minute))
		connect.AssertEqual(t, model.GetWinbackOffer(ctx, playOffer.OfferId).RedeemedAt == nil, true)
		credit(taggedSub, playNetworkId, now.Add(-time.Hour))
		connect.AssertEqual(t, model.GetWinbackOffer(ctx, playOffer.OfferId).RedeemedAt == nil, true)
		credit(taggedSub, playNetworkId, now.Add(time.Minute))
		connect.AssertEqual(t, model.GetWinbackOffer(ctx, playOffer.OfferId).RedeemedAt != nil, true)
		connect.AssertEqual(t, model.GetActiveWinbackOffer(ctx, playNetworkId, now.Add(2*time.Minute)) == nil, true)
		// a purchase under the tag by a network never offered one is credited
		// all the same (the credit is untouched) and only counted
		strangerNetworkId := winbackPaidNetwork(t, ctx, model.SubscriptionMarketGoogle, now)
		credit(taggedSub, strangerNetworkId, now)
		connect.AssertEqual(t, model.GetLatestWinbackOffer(ctx, strangerNetworkId) == nil, true)

		// App Store: a win-back transaction with the offer's identifier
		appleNetworkId, appleOffer := issue(model.SubscriptionMarketApple, reengagement.ArmControl)
		redeemApple := func(offerType int64, identifier string) {
			server.Tx(ctx, func(tx server.PgTx) {
				appleRecordWinbackInTx(tx, ctx, &validatedAppleTransaction{
					networkId:       appleNetworkId,
					transactionId:   "apple-winback-" + identifier,
					purchaseTime:    now.Add(time.Minute),
					offerType:       offerType,
					offerIdentifier: identifier,
				})
			})
		}
		redeemApple(appleOfferTypeOfferCode, "winback_monthly_50")
		redeemApple(appleOfferTypeWinBack, "another_offer")
		connect.AssertEqual(t, model.GetWinbackOffer(ctx, appleOffer.OfferId).RedeemedAt == nil, true)
		redeemApple(appleOfferTypeWinBack, "winback_monthly_50")
		connect.AssertEqual(t, model.GetWinbackOffer(ctx, appleOffer.OfferId).RedeemedAt != nil, true)
	})
}

// A redemption stamp that cannot be written (here: the table is gone) rolls
// back to its savepoint, and the credit's own writes in the same transaction
// commit.
func TestWinbackRedemptionFailureNeverFailsTheCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installWinbackTestEnv(t)
		now := server.NowUtc()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "winbackfail", server.NewId())
		taggedSub := &PlaySubscription{LineItems: []*PlaySubscriptionPurchaseLineItem{{
			ProductId:    "supporter",
			OfferDetails: &PlayOfferDetails{OfferTags: []string{"winback-monthly-50"}},
		}}}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_winback_offer RENAME TO network_winback_offer_gone`))
			playRecordWinbackInTx(tx, ctx, taggedSub, &model.SubscriptionRenewal{NetworkId: networkId, StartTime: now})
			// the credit goes on in the same transaction
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO subscription_renewal (network_id, subscription_type, start_time, end_time, net_revenue_nano_cents, purchase_token, market)
					VALUES ($1, 'supporter', $2, $3, 1, 'credit-token', 'google')
				`,
				networkId, now, now.Add(30*24*time.Hour),
			))
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_winback_offer_gone RENAME TO network_winback_offer`))
		})
		_, market, ok := model.NetworkLastPaidWindow(ctx, networkId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, market, model.SubscriptionMarketGoogle)
	})
}

// A Play Store "Resubscribe" delivered by RTDN, with no account identifiers
// of its own, is credited to the network its expired subscription named, and
// bound, so the renewal Google delivers after the acknowledgement removed the
// context is credited to the same network.
func TestPlayWebhookResubscribeBindsThroughExpiredAccountId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter_monthly": {FeeFraction: 0.3, PriceAmountUsd: 5.0, Supporter: true},
		})
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "playresubscribe", userId)
		webhookSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId})

		purchaseToken := "play-resubscribe-1"
		sub := playTestSubscription(networkId, "supporter_monthly", server.NowUtc().Add(-time.Minute), server.NowUtc().Add(30*24*time.Hour))
		sub.ExternalAccountIdentifiers = nil
		sub.OutOfAppPurchaseContext = &PlayOutOfAppPurchaseContext{
			ExpiredExternalAccountIdentifiers: &PlayExternalAccountIdentifiers{ObfuscatedExternalAccountId: networkId.String()},
			ExpiredPurchaseToken:              "play-expired-1",
		}
		env.subscriptions[purchaseToken] = sub

		_, err := PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, env.acknowledgeCount(purchaseToken), int64(1))
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
		boundNetworkId, _, bound := model.GetPlayPurchaseBinding(ctx, purchaseToken)
		connect.AssertEqual(t, bound, true)
		connect.AssertEqual(t, boundNetworkId, networkId)

		// after the acknowledgement Google drops the context; the next period
		// resolves through the binding
		sub.OutOfAppPurchaseContext = nil
		sub.AcknowledgementState = "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED"
		sub.LineItems[0].ExpiryTime = server.NowUtc().Add(60 * 24 * time.Hour).UTC().Format(time.RFC3339)
		_, err = PlayWebhook(env.webhookArgs(t, "supporter_monthly", purchaseToken), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 2)
	})
}

// The W1 landing's click answers with Apple's direct link for a network whose
// offer is the App Store's, and with no link otherwise.
func TestOnboardingClickWinbackOpensAppleLink(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := installWinbackTestEnv(t)
		env.appleEligible = true
		now := server.NowUtc()
		appleDecision := winbackTestDecision
		appleDecision.Variant = reengagement.VariantDefault
		appleNetworkId := winbackPaidNetwork(t, ctx, model.SubscriptionMarketApple, now)
		offer, _, err := issueWinbackOfferForSend(ctx, winbackTestRow(appleNetworkId, reengagement.ArmControl, now), appleDecision, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, offer, nil)
		playNetworkId := winbackPaidNetwork(t, ctx, model.SubscriptionMarketGoogle, now)

		click := func(networkId server.Id) *OnboardingClickResult {
			token, err := onboarding.NewToken(&onboarding.TokenClaims{
				NetworkId: networkId,
				Step:      onboarding.SlugWinback,
				FlowStep:  reengagement.StepW1,
				ExpiresAt: now.Add(30 * 24 * time.Hour).Unix(),
			})
			connect.AssertEqual(t, err, nil)
			clickSession := session.Testing_CreateClientSession(ctx, nil)
			result, err := OnboardingClick(&OnboardingClickArgs{Token: token}, clickSession)
			connect.AssertEqual(t, err, nil)
			return result
		}
		apple := click(appleNetworkId)
		connect.AssertEqual(t, apple.Ok, true)
		connect.AssertEqual(t, apple.Step, onboarding.SlugWinback)
		connect.AssertEqual(t, apple.OpenUrl, winbackTestAppleUrl)
		play := click(playNetworkId)
		connect.AssertEqual(t, play.Ok, true)
		connect.AssertEqual(t, play.OpenUrl, "")
		connect.AssertEqual(t, strings.HasPrefix(play.Destination, "onboarding/"), true)
	})
}
