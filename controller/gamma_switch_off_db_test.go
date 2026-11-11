package controller

// Database tests of the automatic Gamma charges turned off (POST
// /pay/gamma/settings, mmm/BTCPAY.md §18.5). The switches were read only when
// the biller made an automatic intent: one already made (a renewal an hour
// before the term ends, a seat upgrade at the client limit, a data top-up)
// stayed chargeable, so once the deposit could pay it, it was charged, though
// subscription details already said the plan would not renew. Now a switch
// turned off voids the open intents it governs that are not yet being charged
// (in the biller's own step, before it charges), a spend batch never takes
// their charge, and a charge already sent is listed in the answer, which says
// it can still be charged this once. Against the test database and the
// simulated chain. Needs pro.yml.
//
// These tests read the settings answer as JSON, the way a client does.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// gammaSwitchTestSet applies settings and returns the answer as a client
// reads it.
func gammaSwitchTestSet(t testing.TB, clientSession *session.ClientSession, args *GammaSettingsArgs) map[string]any {
	result, err := SetGammaSettings(args, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	if result.Error != nil {
		t.Fatalf("the settings were refused: %+v", result.Error)
	}
	return gammaSwitchTestJson(t, result)
}

func gammaSwitchTestGet(t testing.TB, clientSession *session.ClientSession) map[string]any {
	result, err := GetGammaSettings(clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return gammaSwitchTestJson(t, result)
}

func gammaSwitchTestJson(t testing.TB, result *GammaSettingsResult) map[string]any {
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	answer := map[string]any{}
	if err := json.Unmarshal(encoded, &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

// gammaSwitchTestCharging is the answer's list of automatic charges already
// being made when their switch was turned off.
func gammaSwitchTestCharging(t testing.TB, answer map[string]any) []map[string]any {
	raw, ok := answer["charging"]
	if !ok {
		t.Fatalf("the settings answer does not say what is still being charged (no charging): %v", answer)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("the settings answer's charging is not a list: %v", answer)
	}
	charging := []map[string]any{}
	for _, entry := range list {
		charge, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a charging entry is not an object: %v", answer)
		}
		charging = append(charging, charge)
	}
	return charging
}

func gammaSwitchTestBool(value bool) *bool {
	return &value
}

func gammaSwitchTestIntentCount(t testing.TB, ctx context.Context, networkId server.Id, purpose string) int {
	return gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = $2`, networkId, purpose)
}

func gammaSwitchTestVoided(t testing.TB, ctx context.Context, intentId server.Id) {
	intent := model.GetGammaPaymentIntent(ctx, intentId)
	if intent == nil || intent.VoidedAt == nil || intent.CreditedAt != nil {
		t.Fatalf("intent %s is not voided: %+v", intentId, intent)
	}
}

// The renewal waits for gamma at the end of the term, and the customer turns
// renewal off. Once they mint, the renewal is never charged: it is voided. On
// again inside the grace, the term renews, once.
func TestGammaRenewalOffVoidsThePendingRenewal(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammarenewoff")
		f.run()
		hotkey, renewal, termEnd := gammaReplaceTestPendingRenewal(t, f, networkId, clientSession)

		off := gammaSwitchTestSet(t, clientSession, &GammaSettingsArgs{AutoRenew: gammaSwitchTestBool(false)})
		connect.AssertEqual(t, off["auto_renew"], false)

		gammaReplaceTestDeposit(t, f, hotkey, gammaDeletedTestDepositBalance)
		f.run()
		f.run()
		if references := gammaReplaceTestSubmittedReferences(f); len(references) != 0 {
			t.Fatalf("spent %v after the customer turned renewal off; the renewal %s waited for gamma and must not be charged", references, gammaHex(renewal.Reference))
		}
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestDepositBalance)
		gammaSwitchTestVoided(t, ctx, renewal.IntentId)
		connect.AssertEqual(t, gammaReplaceTestCharges(t, ctx, renewal.IntentId), 0)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).count, 1)
		// nothing was being charged yet, so the answer listed nothing
		connect.AssertEqual(t, len(gammaSwitchTestCharging(t, off)), 0)

		// on again before the grace ends: the same term renews, from where it
		// ended, with a new renewal
		gammaSwitchTestSet(t, clientSession, &GammaSettingsArgs{AutoRenew: gammaSwitchTestBool(true)})
		f.autoNow()
		f.run()
		references := gammaReplaceTestSubmittedReferences(f)
		if len(references) != 1 || references[0] == gammaHex(renewal.Reference) {
			t.Fatalf("spent %v; want one new renewal, not the voided %s", references, gammaHex(renewal.Reference))
		}
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaSwitchTestIntentCount(t, ctx, networkId, model.GammaPurposeRenew), 2)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM subscription_renewal WHERE network_id = $1 AND market = 'gamma' AND start_time = $2`, networkId, termEnd), 1)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestDepositBalance-gammaDeletedTestMonthlyPriceGam)
	})
}

// The renewal's charge was recorded but no batch carried it (the spend's fee
// was over the limit), and the customer turns renewal off. The charge never
// goes into a batch, and it no longer holds the deposit: a data pack the same
// deposit pays is charged.
func TestGammaRenewalOffVoidsARenewalChargeNotYetInABatch(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammarenewoffunbatched")
		f.run()
		hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		// the deposit holds the renewal's price, no more
		gammaReplaceTestDeposit(t, f, hotkey, gammaDeletedTestMonthlyPriceGam)
		termEnd := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).endTime.Add(-manualPaymentGracePeriod)
		f.now = termEnd.Add(-30 * time.Minute)
		f.autoNow()
		fee := f.sim.fee
		f.sim.fee = 2 * f.settings.MaxFeeRao
		f.run()
		f.sim.fee = fee
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		renewal := model.GetLatestGammaPaymentIntent(ctx, networkId, model.GammaPurposeRenew)
		if renewal == nil {
			t.Fatal("no renewal was made at the end of the term")
		}
		charge := model.GetGammaChargeForIntent(ctx, renewal.IntentId)
		if charge == nil || charge.State != model.GammaChargePending || charge.BatchId != nil {
			t.Fatalf("the renewal's charge is not pending in no batch: %+v", charge)
		}

		f.now = f.now.Add(time.Minute)
		off := gammaSwitchTestSet(t, clientSession, &GammaSettingsArgs{AutoRenew: gammaSwitchTestBool(false)})
		f.run()
		if references := gammaReplaceTestSubmittedReferences(f); len(references) != 0 {
			t.Fatalf("spent %v after the customer turned renewal off; the renewal's charge %s was in no batch and must not be sent", references, gammaHex(renewal.Reference))
		}
		gammaSwitchTestVoided(t, ctx, renewal.IntentId)
		charge = model.GetGammaChargeForIntent(ctx, renewal.IntentId)
		if charge == nil || charge.State == model.GammaChargePending || charge.BatchId != nil {
			t.Fatalf("the voided renewal's charge can still be batched: %+v", charge)
		}
		connect.AssertEqual(t, model.SumPendingGammaCharges(ctx)[renewal.Hotkey], int64(0))
		// a charge in no batch was not sent: it was cancelled, not charging
		connect.AssertEqual(t, len(gammaSwitchTestCharging(t, off)), 0)

		// the deposit pays a data pack
		data := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		if data.Error != nil {
			t.Fatalf("data pack refused: %+v", data.Error)
		}
		f.run()
		references := gammaReplaceTestSubmittedReferences(f)
		if len(references) != 1 || references[0] != data.Reference {
			t.Fatalf("spent %v; want the data pack %s", references, data.Reference)
		}
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, data).Status, gammaStatusPaid)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestMonthlyPriceGam-3_000_000_000)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).count, 1)
	})
}

// The renewal's spend is already in a submitted batch when the customer turns
// renewal off. It cannot be recalled, so it is not voided: the answer lists it
// and says plainly that it can still be charged this once. It lands and is
// credited, and the term after it does not renew.
func TestGammaRenewalOffWhileItsSpendIsSentSaysItIsChargedOnceMore(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammarenewoffsent")
		f.run()
		hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		termEnd := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).endTime.Add(-manualPaymentGracePeriod)
		f.now = termEnd.Add(-30 * time.Minute)
		f.autoNow()
		f.run()
		// the deposit pays the renewal at once: its spend is submitted
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		renewal := model.GetLatestGammaPaymentIntent(ctx, networkId, model.GammaPurposeRenew)
		if renewal == nil {
			t.Fatal("no renewal was made at the end of the term")
		}

		f.now = f.now.Add(time.Minute)
		off := gammaSwitchTestSet(t, clientSession, &GammaSettingsArgs{AutoRenew: gammaSwitchTestBool(false)})
		connect.AssertEqual(t, off["auto_renew"], false)
		charging := gammaSwitchTestCharging(t, off)
		if len(charging) != 1 {
			t.Fatalf("the answer lists %d charges still being made; want the renewal: %v", len(charging), off)
		}
		connect.AssertEqual(t, charging[0]["intent_id"], renewal.IntentId.String())
		connect.AssertEqual(t, charging[0]["purpose"], model.GammaPurposeRenew)
		connect.AssertEqual(t, charging[0]["plan"], model.SolanaPlanMonthly)
		connect.AssertEqual(t, charging[0]["amount_gamma"], "5")
		connect.AssertEqual(t, charging[0]["status"], gammaStatusConfirming)
		notice, _ := off["notice"].(string)
		if !strings.Contains(notice, "5 gamma") || !strings.Contains(notice, "charged this once") {
			t.Fatalf("the answer does not say plainly that the renewal can still be charged once: notice %q", notice)
		}
		// GET says the same until it lands
		connect.AssertEqual(t, len(gammaSwitchTestCharging(t, gammaSwitchTestGet(t, clientSession))), 1)

		// the biller leaves it in its batch, which lands: credited, once
		f.run()
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, renewal.IntentId).VoidedAt == nil, true)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, renewal.IntentId).CreditedAt != nil, true)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).count, 2)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestDepositBalance-2*gammaDeletedTestMonthlyPriceGam)
		after := gammaSwitchTestGet(t, clientSession)
		connect.AssertEqual(t, len(gammaSwitchTestCharging(t, after)), 0)
		connect.AssertEqual(t, after["notice"] == nil || after["notice"] == "", true)

		// the end of the term it paid for: no renewal
		nextEnd := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).endTime.Add(-manualPaymentGracePeriod)
		f.now = nextEnd.Add(-30 * time.Minute)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaSwitchTestIntentCount(t, ctx, networkId, model.GammaPurposeRenew), 1)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).count, 2)
	})
}

// A free account at its client limit waits for a seat upgrade the deposit
// cannot pay, and the customer turns renewal off: an upgrade is an automatic
// Pro charge. Once they mint, it is never charged, and no upgrade is made
// while renewal is off. On again, an upgrade is made at the next demand.
func TestGammaRenewalOffVoidsThePendingSeatUpgrade(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaseatoff")
		f.run()
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
		gammaSwitchTestSet(t, clientSession, &GammaSettingsArgs{AutoRenew: gammaSwitchTestBool(false)})
		gammaReplaceTestDeposit(t, f, hotkey, gammaDeletedTestDepositBalance)
		f.run()
		f.run()
		if references := gammaReplaceTestSubmittedReferences(f); len(references) != 0 {
			t.Fatalf("spent %v after the customer turned renewal off; the seat upgrade %s must not be charged", references, gammaHex(seat.Reference))
		}
		gammaSwitchTestVoided(t, ctx, seat.IntentId)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), false)

		// at the limit again, the next day: no upgrade while renewal is off
		f.now = f.now.Add(25 * time.Hour)
		model.NoteGammaSeatDemand(ctx, networkId, f.now)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaSwitchTestIntentCount(t, ctx, networkId, model.GammaPurposeSeat), 1)
		connect.AssertEqual(t, len(f.sim.submitted), 0)

		// renewal on: the next demand upgrades
		gammaSwitchTestSet(t, clientSession, &GammaSettingsArgs{AutoRenew: gammaSwitchTestBool(true)})
		connect.AssertEqual(t, model.NoteGammaSeatDemand(ctx, networkId, f.now), true)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		f.sim.mine()
		f.run()
		// (Pro starts at the test's clock, a day ahead of the database's: the
		// credited upgrade and its term say it)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'seat' AND credited_at IS NOT NULL`, networkId), 1)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).count, 1)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestDepositBalance-gammaDeletedTestMonthlyPriceGam)
	})
}

// A top-up and a seat upgrade wait for gamma, and the customer turns top-up
// off: both are voided, and neither is charged once they mint.
func TestGammaTopupOffVoidsThePendingTopupAndSeatUpgrade(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammatopupoff")
		f.run()
		// a data code: the account pays with gamma, and its own balance is empty
		hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib, Delivery: model.CryptoDeliveryCode})
		gammaReplaceTestDeposit(t, f, hotkey, 0)
		connect.AssertEqual(t, model.NoteGammaSeatDemand(ctx, networkId, f.now), true)
		f.autoNow()
		f.run()
		topup := model.GetLatestGammaPaymentIntent(ctx, networkId, model.GammaPurposeTopup)
		seat := model.GetLatestGammaPaymentIntent(ctx, networkId, model.GammaPurposeSeat)
		if topup == nil || seat == nil {
			t.Fatalf("the top-up (%v) or the seat upgrade (%v) was not made", topup, seat)
		}
		connect.AssertEqual(t, len(f.sim.submitted), 0)

		f.now = f.now.Add(time.Minute)
		gammaSwitchTestSet(t, clientSession, &GammaSettingsArgs{AutoTopup: gammaSwitchTestBool(false)})
		gammaReplaceTestDeposit(t, f, hotkey, gammaDeletedTestDepositBalance)
		f.run()
		f.run()
		if references := gammaReplaceTestSubmittedReferences(f); len(references) != 0 {
			t.Fatalf("spent %v after the customer turned top-up off; the top-up %s and the seat upgrade %s must not be charged", references, gammaHex(topup.Reference), gammaHex(seat.Reference))
		}
		gammaSwitchTestVoided(t, ctx, topup.IntentId)
		gammaSwitchTestVoided(t, ctx, seat.IntentId)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestDepositBalance)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), model.ByteCount(0))
	})
}
