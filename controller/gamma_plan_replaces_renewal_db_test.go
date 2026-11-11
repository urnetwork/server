package controller

// Database tests of a Gamma plan bought beside another Gamma plan charge (the
// owner, 2026-11-11: never charge twice for the same period). Purchases
// ignore the network's own Gamma plans, so that a plan can be upgraded, and
// the biller charged every open intent its deposit could pay, oldest first:
// a renewal waiting at the end of a term (or a seat upgrade) and a plan the
// customer bought then were both charged for the same days, a renewal charged
// before a purchase was paid made the purchase charge again on top of it, and
// two plan purchases opened together were both charged. Now a plan purchase
// replaces the waiting automatic plan charge (held while the purchase is
// open, voided once it is paid), a plan purchase is refused while another
// Gamma plan charge is being made, and a plan intent opened before another
// Gamma plan was paid is never charged. Against the test database and the
// simulated chain. Needs pro.yml.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const gammaReplaceTestYearlyPriceGam = uint64(40_000_000_000)

// gammaReplaceTestPendingRenewal pays a monthly plan, empties the deposit and
// moves to the end of the term, where the biller makes the renewal it cannot
// charge yet. It returns the deposit hotkey, the renewal and the term's end.
func gammaReplaceTestPendingRenewal(t testing.TB, f *gammaDbFixture, networkId server.Id, clientSession *session.ClientSession) ([32]byte, *model.GammaPaymentIntent, time.Time) {
	ctx := f.ctx
	hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
	f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: 0})
	termEnd := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).endTime.Add(-manualPaymentGracePeriod)
	f.now = termEnd.Add(-30 * time.Minute)
	f.autoNow()
	f.run()
	connect.AssertEqual(t, len(f.sim.submitted), 0)
	renewal := model.GetLatestGammaPaymentIntent(ctx, networkId, model.GammaPurposeRenew)
	if renewal == nil {
		t.Fatal("no renewal was made at the end of the term")
	}
	// the customer comes to it a minute later
	f.now = f.now.Add(time.Minute)
	return hotkey, renewal, termEnd
}

func gammaReplaceTestDeposit(t testing.TB, f *gammaDbFixture, hotkey [32]byte, balance uint64) {
	f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: balance})
}

// gammaReplaceTestSubmittedReferences are the references of the spend entries
// submitted since the last mine.
func gammaReplaceTestSubmittedReferences(f *gammaDbFixture) []string {
	references := []string{}
	for _, encoded := range f.sim.submitted {
		_, _, entries := f.sim.parse(encoded)
		for _, entry := range entries {
			references = append(references, gammaHex(entry.Reference[:]))
		}
	}
	return references
}

func gammaReplaceTestCharges(t testing.TB, ctx context.Context, intentId server.Id) int {
	return gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_charge WHERE intent_id = $1`, intentId)
}

// The customer buys a yearly plan while the monthly plan's renewal waits for
// gamma, and mints the yearly price. Only the purchase is charged: the
// renewal would buy days the year pays for. It is voided once the year is
// paid, and a deposit filled again later never pays it.
func TestGammaPlanPurchaseReplacesThePendingRenewal(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammareplacerenewal")
		f.run()
		hotkey, renewal, _ := gammaReplaceTestPendingRenewal(t, f, networkId, clientSession)

		yearly := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		if yearly.Error != nil {
			t.Fatalf("the yearly plan was refused beside a waiting renewal: %+v", yearly.Error)
		}
		gammaReplaceTestDeposit(t, f, hotkey, gammaReplaceTestYearlyPriceGam)
		f.run()
		references := gammaReplaceTestSubmittedReferences(f)
		if len(references) != 1 || references[0] != yearly.Reference {
			t.Fatalf("spent %v; want only the yearly plan %s, not the renewal %s", references, yearly.Reference, gammaHex(renewal.Reference))
		}
		connect.AssertEqual(t, gammaReplaceTestCharges(t, ctx, renewal.IntentId), 0)

		f.now = f.now.Add(time.Minute)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, yearly).Status, gammaStatusPaid)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(0))
		replaced := model.GetGammaPaymentIntent(ctx, renewal.IntentId)
		if replaced.VoidedAt == nil || replaced.CreditedAt != nil {
			t.Fatalf("the replaced renewal is not voided: %+v", replaced)
		}
		renewals := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma)
		connect.AssertEqual(t, renewals.count, 2)
		connect.AssertEqual(t, renewals.endTime.Sub(renewals.startTime), SubscriptionYearDuration+manualPaymentGracePeriod)
		connect.AssertEqual(t, model.GetGammaAccount(ctx, networkId).RenewPlan, model.SolanaPlanYearly)

		// gamma minted later pays nothing for the replaced renewal
		gammaReplaceTestDeposit(t, f, hotkey, gammaDeletedTestDepositBalance)
		f.autoNow()
		f.run()
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestDepositBalance)
		connect.AssertEqual(t, gammaReplaceTestCharges(t, ctx, renewal.IntentId), 0)
	})
}

// A renewal waits while a plan purchase is open, though the deposit can pay
// the renewal alone: the purchase replaces it once paid. The purchase expires
// unpaid, and the renewal then goes ahead from where the term ended, so a
// quote the customer never paid does not cost them the renewal.
func TestGammaPendingRenewalWaitsForAnOpenPlanPurchaseAndRenewsIfItExpires(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammarenewalwaits")
		f.run()
		hotkey, renewal, termEnd := gammaReplaceTestPendingRenewal(t, f, networkId, clientSession)

		yearly := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		connect.AssertEqual(t, yearly.Error == nil, true)
		// enough for the renewal, not for the year
		gammaReplaceTestDeposit(t, f, hotkey, 10_000_000_000)
		f.run()
		f.run()
		if references := gammaReplaceTestSubmittedReferences(f); len(references) != 0 {
			t.Fatalf("spent %v while a plan purchase that replaces the renewal is open", references)
		}
		connect.AssertEqual(t, gammaReplaceTestCharges(t, ctx, renewal.IntentId), 0)

		// the purchase expires unpaid; the renewal is still open, and goes ahead
		f.now = yearly.ExpiresAt.Add(time.Minute)
		if !f.now.Before(renewal.ExpiresAt) {
			t.Fatalf("the renewal expires (%s) before the purchase (%s)", renewal.ExpiresAt, yearly.ExpiresAt)
		}
		f.run()
		references := gammaReplaceTestSubmittedReferences(f)
		if len(references) != 1 || references[0] != gammaHex(renewal.Reference) {
			t.Fatalf("spent %v; want the renewal %s once the purchase expired", references, gammaHex(renewal.Reference))
		}
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, yearly).Status, gammaStatusExpired)
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, renewal.IntentId).CreditedAt != nil, true)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM subscription_renewal WHERE network_id = $1 AND market = 'gamma' AND start_time = $2`, networkId, termEnd), 1)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(5_000_000_000))
	})
}

// A renewal already being charged cannot be voided safely: its spend may
// land. A plan bought then is refused with already_subscribed, and nothing is
// started; once the renewal is paid, a plan can be bought again (an upgrade
// starts at once, as before).
func TestGammaPlanPurchaseRefusedWhileARenewalIsBeingCharged(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammarenewalinflight")
		f.run()
		hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		termEnd := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).endTime.Add(-manualPaymentGracePeriod)
		f.now = termEnd.Add(-30 * time.Minute)
		f.autoNow()
		f.run()
		// the deposit pays the renewal at once: its spend is submitted
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		renewal := model.GetLatestGammaPaymentIntent(ctx, networkId, model.GammaPurposeRenew)
		connect.AssertEqual(t, gammaReplaceTestCharges(t, ctx, renewal.IntentId), 1)

		f.now = f.now.Add(time.Minute)
		for _, plan := range []string{model.SolanaPlanYearly, model.SolanaPlanMonthly} {
			refused := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: plan})
			if refused.Error == nil || refused.Error.Code != PurchaseErrorCodeAlreadySubscribed || refused.Error.Message == "" {
				t.Fatalf("a Gamma %s plan was started while the renewal is being charged: %+v", plan, refused)
			}
		}
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'purchase' AND create_time > $2`, networkId, termEnd.Add(-time.Hour)), 0)
		// a data pack is not Pro, and is sold
		data := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		if data.Error != nil {
			t.Fatalf("a data pack was refused beside a renewal being charged: %+v", data.Error)
		}

		f.sim.mine()
		f.run()
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, renewal.IntentId).CreditedAt != nil, true)
		// the data pack went out once the renewal's batch was done
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, data).Status, gammaStatusPaid)

		// the upgrade, once the renewal is paid
		f.now = f.now.Add(time.Minute)
		yearly := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		if yearly.Error != nil {
			t.Fatalf("the upgrade was refused once the renewal was paid: %+v", yearly.Error)
		}
		f.run()
		references := gammaReplaceTestSubmittedReferences(f)
		if len(references) != 1 || references[0] != yearly.Reference {
			t.Fatalf("spent %v; want the upgrade %s", references, yearly.Reference)
		}
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, yearly).Status, gammaStatusPaid)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestDepositBalance-2*gammaDeletedTestMonthlyPriceGam-gammaReplaceTestYearlyPriceGam-3_000_000_000)
	})
}

// A plan purchase that got past the refusal as the renewal's charge was being
// recorded (the API and the biller run apart) waits while the renewal is
// charged, and once the renewal is paid it is never charged: the renewal
// paid for those days.
func TestGammaPlanPurchaseIsNotChargedOnceARenewalIsPaidAfterIt(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammapurchaserace")
		f.run()
		hotkey, renewal, _ := gammaReplaceTestPendingRenewal(t, f, networkId, clientSession)
		gammaReplaceTestDeposit(t, f, hotkey, 10_000_000_000)
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		connect.AssertEqual(t, gammaReplaceTestCharges(t, ctx, renewal.IntentId), 1)

		// the purchase the API recorded as the renewal's spend went out
		tier := model.Pro().DefaultPriceTier()
		priceUsd, ok := solanaPlanPriceUsdForTier(model.SolanaPlanYearly, tier)
		connect.AssertEqual(t, ok, true)
		token, tokenHash := cryptoNewStatusToken()
		purchase := &model.GammaPaymentIntent{
			IntentId:                 server.NewId(),
			StatusTokenHash:          tokenHash,
			NetworkId:                networkId,
			Hotkey:                   renewal.Hotkey,
			Purpose:                  model.GammaPurposePurchase,
			Reference:                gammaNewReference(),
			Delivery:                 model.CryptoDeliveryAccount,
			Item:                     model.SolanaPlanYearly,
			Tier:                     tier.Name,
			PriceUsdNanoCents:        model.UsdToNanoCents(priceUsd),
			RegularPriceUsdNanoCents: model.UsdToNanoCents(priceUsd),
			CreateTime:               f.now,
			ExpiresAt:                f.now.Add(f.settings.IntentTtl),
		}
		connect.AssertEqual(t, model.CreateGammaPaymentIntent(ctx, purchase), true)
		handle := &GammaPaymentIntentResult{IntentId: purchase.IntentId.String(), StatusToken: token}

		// the customer mints the year; the renewal lands and is credited
		gammaReplaceTestDeposit(t, f, hotkey, gammaDeletedTestDepositBalance)
		f.now = f.now.Add(time.Minute)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, renewal.IntentId).CreditedAt != nil, true)
		if references := gammaReplaceTestSubmittedReferences(f); len(references) != 0 {
			t.Fatalf("spent %v after the renewal paid for the same days", references)
		}
		status := gammaDbStatus(t, clientSession, handle)
		connect.AssertEqual(t, status.Status, gammaStatusSeen)
		if status.Error == nil || status.Error.Code != PurchaseErrorCodeAlreadySubscribed || status.Error.Message == "" {
			t.Fatalf("status of a purchase the renewal paid for: %+v", status)
		}
		f.run()
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaReplaceTestCharges(t, ctx, purchase.IntentId), 0)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestDepositBalance-gammaDeletedTestMonthlyPriceGam)
	})
}

// A free account at its client limit waits for a seat upgrade the deposit
// cannot pay, and the customer buys a yearly plan. Only the year is charged:
// the seat upgrade would buy a month of the same days. It is voided once the
// year is paid.
func TestGammaPlanPurchaseReplacesAPendingSeatUpgrade(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammareplaceseat")
		f.run()
		// data to the account: it pays with gamma, and needs no top-up
		hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		gammaReplaceTestDeposit(t, f, hotkey, 0)
		connect.AssertEqual(t, model.NoteGammaSeatDemand(ctx, networkId, f.now), true)
		f.autoNow()
		f.run()
		seat := model.GetLatestGammaPaymentIntent(ctx, networkId, model.GammaPurposeSeat)
		if seat == nil {
			t.Fatal("no seat upgrade was made at the client limit")
		}
		connect.AssertEqual(t, len(f.sim.submitted), 0)

		f.now = f.now.Add(time.Minute)
		yearly := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		connect.AssertEqual(t, yearly.Error == nil, true)
		gammaReplaceTestDeposit(t, f, hotkey, gammaReplaceTestYearlyPriceGam)
		f.run()
		references := gammaReplaceTestSubmittedReferences(f)
		if len(references) != 1 || references[0] != yearly.Reference {
			t.Fatalf("spent %v; want only the yearly plan %s, not the seat upgrade %s", references, yearly.Reference, gammaHex(seat.Reference))
		}
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, yearly).Status, gammaStatusPaid)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), true)
		replaced := model.GetGammaPaymentIntent(ctx, seat.IntentId)
		if replaced.VoidedAt == nil || replaced.CreditedAt != nil {
			t.Fatalf("the replaced seat upgrade is not voided: %+v", replaced)
		}
		connect.AssertEqual(t, gammaReplaceTestCharges(t, ctx, seat.IntentId), 0)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(0))
	})
}

// Two plans opened together from one deposit, the monthly first: once the
// deposit can pay both, only the first is charged, the yearly waits while it
// is, and once it is paid the yearly is never charged. Asking again for the
// purchase being charged hands it back; another plan is refused meanwhile.
func TestGammaSecondPlanPurchaseIsNotChargedBesideTheFirst(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammatwoplans")
		f.run()
		monthly := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		connect.AssertEqual(t, monthly.Error == nil, true)
		f.now = f.now.Add(time.Minute)
		yearly := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		connect.AssertEqual(t, yearly.Error == nil, true)
		connect.AssertEqual(t, yearly.DepositHotkey, monthly.DepositHotkey)
		hotkey := gammaTestAccount(t, monthly.DepositHotkey)
		gammaReplaceTestDeposit(t, f, hotkey, gammaDeletedTestMonthlyPriceGam+gammaReplaceTestYearlyPriceGam)
		f.run()
		references := gammaReplaceTestSubmittedReferences(f)
		if len(references) != 1 || references[0] != monthly.Reference {
			t.Fatalf("spent %v; want only the first plan %s", references, monthly.Reference)
		}

		again := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		connect.AssertEqual(t, again.Error == nil, true)
		connect.AssertEqual(t, again.IntentId, monthly.IntentId)
		refused := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		if refused.Error == nil || refused.Error.Code != PurchaseErrorCodeAlreadySubscribed {
			t.Fatalf("another plan was handed out while the first is being charged: %+v", refused)
		}

		f.now = f.now.Add(time.Minute)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, monthly).Status, gammaStatusPaid)
		if references := gammaReplaceTestSubmittedReferences(f); len(references) != 0 {
			t.Fatalf("spent %v after the first plan was paid", references)
		}
		status := gammaDbStatus(t, clientSession, yearly)
		connect.AssertEqual(t, status.Status, gammaStatusSeen)
		if status.Error == nil || status.Error.Code != PurchaseErrorCodeAlreadySubscribed {
			t.Fatalf("status of the second plan: %+v", status)
		}
		f.run()
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaReplaceTestYearlyPriceGam)
	})
}
