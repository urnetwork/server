package controller

// Database tests of a plan paid with Gamma beside another Pro subscription
// (the owner, 2026-11-11: never charge twice for the same period). The biller
// skipped a Gamma renewal only when another market's window ran past the
// Gamma term's whole grace, so an App Store or Play month (its window ends a
// day after the store's expiry) or a Stripe subscription retrying its renewal
// was charged beside it, and a renewal intent made before the other
// subscription started was charged later; Gamma renewal could be turned back
// on beside a card plan, and a Gamma plan bought beside a renewing one. In both
// orders the second charge is refused or skipped now, and a renewal held back
// by another subscription starts where that one ended. Against the test
// database, the simulated chain and a fake Stripe. Needs pro.yml.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// gammaOtherProTestWindow records a supporter window of another market, as
// that market's credit does: Stripe's invoice, the App Store's transaction or
// Play's purchase token.
func gammaOtherProTestWindow(t testing.TB, ctx context.Context, networkId server.Id, market string, start time.Time, end time.Time) {
	renewal := &model.SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          start,
		EndTime:            end,
		NetRevenue:         model.UsdToNanoCents(5),
		SubscriptionMarket: market,
	}
	switch market {
	case model.SubscriptionMarketGoogle:
		renewal.PurchaseToken = "play-" + server.NewId().String()
	default:
		renewal.TransactionId = market + "-" + server.NewId().String()
	}
	connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, renewal), nil)
}

func gammaOtherProTestSetAutoRenew(t testing.TB, clientSession *session.ClientSession, autoRenew bool) *GammaSettingsResult {
	settings, err := SetGammaSettings(&GammaSettingsArgs{AutoRenew: &autoRenew}, clientSession)
	connect.AssertEqual(t, err, nil)
	return settings
}

// The recurring markets another Pro subscription is billed in.
var gammaOtherProTestStores = []string{model.SubscriptionMarketApple, model.SubscriptionMarketGoogle, model.SubscriptionMarketStripe}

// Gamma first: with Gamma renewal off the customer buys a card plan, then
// asks for Gamma renewal back on. Both would bill the same days, so it stays
// off, with the reason.
func TestGammaRenewalCannotBeTurnedBackOnBesideACardPlan(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaofftheon")
		f.run()
		gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		off := gammaOtherProTestSetAutoRenew(t, clientSession, false)
		connect.AssertEqual(t, off.Error == nil, true)
		connect.AssertEqual(t, off.AutoRenew, false)

		// the card plan's first invoice is paid: its window, with Stripe's grace
		now := server.NowUtc()
		gammaOtherProTestWindow(t, ctx, networkId, model.SubscriptionMarketStripe, now.Add(-time.Minute), now.Add(60*24*time.Hour))

		on := gammaOtherProTestSetAutoRenew(t, clientSession, true)
		if on.Error == nil || on.Error.Code != PurchaseErrorCodeAlreadySubscribed || on.Error.Message == "" {
			t.Fatalf("Gamma renewal turned back on beside a card plan: %+v", on)
		}
		connect.AssertEqual(t, on.AutoRenew, false)
		connect.AssertEqual(t, model.GetGammaAccount(ctx, networkId).AutoRenew, false)
		// turning it off again is always allowed
		connect.AssertEqual(t, gammaOtherProTestSetAutoRenew(t, clientSession, false).Error == nil, true)
	})
}

// Gamma first, renewing: the customer then subscribes in the App Store, on
// Play, or a Stripe subscription is still retrying its renewal. Each window
// covers the end of the Gamma term without running past its grace. The biller
// renews nothing while it covers; once it ends, inside the Gamma term's grace,
// the renewal is charged and its term starts where the other one ended.
func TestGammaRenewalSkipsATermAnotherProSubscriptionBills(t *testing.T) {
	skipWithoutProYml(t)
	for _, market := range gammaOtherProTestStores {
		t.Logf("market %s", market)
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			f := newGammaDbFixture(t)
			ctx := f.ctx
			networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaotherbills")
			f.run()
			hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
			connect.AssertEqual(t, gammaAccountRenews(model.GetGammaAccount(ctx, networkId)), true)
			paidBalance := f.sim.deposit(hotkey).balance

			gammaRenewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma)
			termEnd := gammaRenewal.endTime.Add(-manualPaymentGracePeriod)
			otherEnd := termEnd.Add(20 * 24 * time.Hour)
			gammaOtherProTestWindow(t, ctx, networkId, market, termEnd.Add(-10*24*time.Hour), otherEnd)

			// the end of the Gamma term, while the other one covers it
			f.now = termEnd.Add(-30 * time.Minute)
			f.autoNow()
			f.run()
			connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'renew'`, networkId), 0)
			connect.AssertEqual(t, len(f.sim.submitted), 0)
			f.now = termEnd.Add(10 * 24 * time.Hour)
			f.autoNow()
			f.run()
			connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'renew'`, networkId), 0)
			connect.AssertEqual(t, len(f.sim.submitted), 0)
			connect.AssertEqual(t, f.sim.deposit(hotkey).balance, paidBalance)

			// it ended, inside the grace: the renewal starts where it ended
			f.now = otherEnd.Add(time.Hour)
			f.autoNow()
			f.run()
			connect.AssertEqual(t, len(f.sim.submitted), 1)
			f.sim.mine()
			f.run()
			connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'renew' AND credited_at IS NOT NULL`, networkId), 1)
			renewed := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma)
			connect.AssertEqual(t, renewed.count, 2)
			connect.AssertEqual(t, renewed.startTime, otherEnd)
			connect.AssertEqual(t, renewed.endTime, otherEnd.Add(30*24*time.Hour+manualPaymentGracePeriod))
			connect.AssertEqual(t, f.sim.deposit(hotkey).balance, paidBalance-gammaDeletedTestMonthlyPriceGam)
		})
	}
}

// A renewal made before another subscription started is not charged while
// that one covers the term it would buy, though the deposit can pay it.
func TestGammaOpenRenewalIsNotChargedOnceAnotherProSubscriptionStarts(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaopenrenewal")
		f.run()
		hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		coldkey := gammaTestAccount(t, gammaTestColdkey)
		gammaRenewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma)
		termEnd := gammaRenewal.endTime.Add(-manualPaymentGracePeriod)

		// the renewal is made while the deposit is empty
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 0})
		f.now = termEnd.Add(-30 * time.Minute)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'renew'`, networkId), 1)
		connect.AssertEqual(t, len(f.sim.submitted), 0)

		// an App Store subscription starts, then the customer mints
		gammaOtherProTestWindow(t, ctx, networkId, model.SubscriptionMarketApple, termEnd.Add(time.Hour), termEnd.Add(31*24*time.Hour))
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: gammaDeletedTestDepositBalance})
		f.now = termEnd.Add(2 * time.Hour)
		f.run()
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_charge INNER JOIN gamma_payment_intent USING (intent_id) WHERE gamma_payment_intent.network_id = $1 AND gamma_payment_intent.purpose = 'renew'`, networkId), 0)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestDepositBalance)
	})
}

// Another Pro subscription first: a Gamma plan is refused beside one that
// renews, in the App Store, on Play or by card, as card checkout is refused
// beside a renewing Gamma plan. A data pack is not Pro and is sold, and a
// one-time crypto year, which never renews, refuses nothing.
func TestGammaProPurchaseRefusedBesideARenewingSubscription(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		f.run()
		now := server.NowUtc()
		for _, market := range gammaOtherProTestStores {
			networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammabuybeside"+market)
			gammaOtherProTestWindow(t, ctx, networkId, market, now.Add(-time.Hour), now.Add(30*24*time.Hour))
			for _, plan := range []string{model.SolanaPlanMonthly, model.SolanaPlanYearly} {
				refused := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: plan})
				if refused.Error == nil || refused.Error.Code != PurchaseErrorCodeAlreadySubscribed || refused.Error.Message == "" {
					t.Fatalf("%s: a Gamma %s plan beside a renewing subscription was not refused: %+v", market, plan, refused)
				}
			}
			connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1`, networkId), 0)
			data := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
			if data.Error != nil {
				t.Fatalf("%s: a data pack beside a renewing subscription was refused: %+v", market, data.Error)
			}
		}
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammabuybesidetao")
		gammaOtherProTestWindow(t, ctx, networkId, model.SubscriptionMarketBittensor, now.Add(-time.Hour), now.Add(395*24*time.Hour))
		beside := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		if beside.Error != nil {
			t.Fatalf("a Gamma plan beside a prepaid TAO year was refused: %+v", beside.Error)
		}
	})
}

// Another Pro subscription first: Gamma renewal is not turned on beside one
// that renews, and nothing else in the request is applied.
func TestGammaRenewalCannotBeTurnedOnBesideARenewingSubscription(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		for _, market := range gammaOtherProTestStores {
			networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaonbeside"+market)
			gammaOtherProTestWindow(t, ctx, networkId, market, now.Add(-time.Hour), now.Add(30*24*time.Hour))
			autoRenew, autoTopup := true, false
			result, err := SetGammaSettings(&GammaSettingsArgs{AutoRenew: &autoRenew, AutoTopup: &autoTopup}, clientSession)
			connect.AssertEqual(t, err, nil)
			if result.Error == nil || result.Error.Code != PurchaseErrorCodeAlreadySubscribed || result.Error.Message == "" {
				t.Fatalf("%s: Gamma renewal turned on beside a renewing subscription: %+v", market, result)
			}
			if account := model.GetGammaAccount(ctx, networkId); account != nil {
				t.Fatalf("%s: the refused request was applied: %+v", market, account)
			}
			connect.AssertEqual(t, gammaOtherProTestSetAutoRenew(t, clientSession, false).Error == nil, true)
		}
	})
}

// A Stripe subscription whose last window ended while Stripe still retries
// its renewal still bills: Gamma renewal and a Gamma plan wait for it, and
// are open once Stripe has given up on it.
func TestGammaRenewalRefusedWhileStripeStillBillsItsLastWindow(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		f.run()
		stripeEnv := newStripeSubscriptionTestEnv(t)
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammastriperetry")
		now := server.NowUtc()
		invoiceId := "in_gamma_retry_" + networkId.String()
		connect.AssertEqual(t, model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-61 * 24 * time.Hour),
			EndTime:            now.Add(-time.Hour),
			NetRevenue:         model.UsdToNanoCents(5),
			SubscriptionMarket: model.SubscriptionMarketStripe,
			TransactionId:      invoiceId,
		}), nil)
		sub := stripeEnv.addSubscription("", "sub_gamma_retry", stripeSubscriptionStatusPastDue, false, now.Add(-31*24*time.Hour), "month")
		stripeEnv.invoiceSubscription[invoiceId] = "sub_gamma_retry"

		on := gammaOtherProTestSetAutoRenew(t, clientSession, true)
		if on.Error == nil || on.Error.Code != PurchaseErrorCodeAlreadySubscribed {
			t.Fatalf("Gamma renewal turned on while Stripe retries: %+v", on)
		}
		plan := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		if plan.Error == nil || plan.Error.Code != PurchaseErrorCodeAlreadySubscribed {
			t.Fatalf("a Gamma plan was started while Stripe retries: %+v", plan)
		}

		sub["status"] = "canceled"
		connect.AssertEqual(t, gammaOtherProTestSetAutoRenew(t, clientSession, true).Error == nil, true)
		connect.AssertEqual(t, gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly}).Error == nil, true)
	})
}

// A free account at its client limit is not upgraded with Gamma while another
// Pro subscription pays for now, though its entitlement does not show yet.
func TestGammaSeatUpgradeSkippedWhileAnotherProSubscriptionBills(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaseatbeside")
		f.run()
		gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib, Delivery: model.CryptoDeliveryCode})
		gammaOtherProTestWindow(t, ctx, networkId, model.SubscriptionMarketApple, f.now.Add(-time.Hour), f.now.Add(30*24*time.Hour))
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), false)

		connect.AssertEqual(t, model.NoteGammaSeatDemand(ctx, networkId, f.now), true)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'seat'`, networkId), 0)
		connect.AssertEqual(t, model.GetGammaAccount(ctx, networkId).SeatDemandAt == nil, true)
	})
}

// A Gamma plan the customer started before another subscription began is not
// charged while that one covers the days it would buy: its status says why. It
// is charged if the other one ends while it is still open.
func TestGammaOpenPurchaseIsNotChargedOnceAnotherProSubscriptionStarts(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaopenpurchase")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		connect.AssertEqual(t, intent.Error == nil, true)

		gammaOtherProTestWindow(t, ctx, networkId, model.SubscriptionMarketStripe, f.now.Add(-time.Hour), f.now.Add(60*24*time.Hour))
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: gammaDeletedTestDepositBalance})
		f.run()
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		status := gammaDbStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, gammaStatusSeen)
		if status.Error == nil || status.Error.Code != PurchaseErrorCodeAlreadySubscribed {
			t.Fatalf("status of a covered purchase: %+v", status)
		}

		// the other subscription ends while the purchase is open
		cryptoDbExec(ctx, `UPDATE subscription_renewal SET end_time = $2 WHERE network_id = $1 AND market = 'stripe'`, networkId, f.now.Add(-time.Minute))
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
	})
}
