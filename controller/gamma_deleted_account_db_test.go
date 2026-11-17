package controller

// Database tests of Pay with Gamma and account deletion, end to end against
// the simulated chain (gamma_sim_chain_test.go). Account deletion once checked
// the Stripe, App Store and Google Play renewals only: the account's
// gamma_account row (automatic renewal and top-up on, its deposit hotkey)
// survived it, and the biller read neither the network nor whether it still
// existed. A renewal at the end of the term, a top-up once the balance ran out
// or expired, a seat upgrade answered after the deletion, an open purchase, a
// skipped charge re-armed by a deposit change, and the live batch's
// resubmission each spent the customer's gamma, and the credit then stopped on
// the missing network: the customer paid and got nothing. Now deletion ends the
// automatic charges and voids the open intents, waits for a charge in flight
// to be credited, and the biller charges no intent whose network is gone.
// Needs the test database and pro.yml.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The deletion outcome and alarm kinds the fix adds, by their wire values.
const (
	gammaDeletedTestInFlight        = model.RemoveNetworkOutcome("gamma_charge_in_flight")
	gammaDeletedTestCreditAlarm     = "credit_unfulfillable"
	gammaDeletedTestBatchAlarm      = "batch_unfulfillable"
	gammaDeletedTestDepositBalance  = uint64(100_000_000_000)
	gammaDeletedTestMonthlyPriceGam = uint64(5_000_000_000)
)

// gammaDeletedTestRemove deletes the signed-in account the way the API does
// after its store steps: one store snapshot, then the removal under the lock.
func gammaDeletedTestRemove(t testing.TB, ctx context.Context, clientSession *session.ClientSession) model.RemoveNetworkOutcome {
	networkId := clientSession.ByJwt.NetworkId
	userId := clientSession.ByJwt.UserId
	outcome, _ := model.RemoveNetworkWithStoreSnapshot(ctx, networkId, &userId, model.GetRemoveNetworkStoreSnapshot(ctx, networkId))
	return outcome
}

func gammaDeletedTestNetworkExists(t testing.TB, ctx context.Context, networkId server.Id) bool {
	return gammaDbCount(t, ctx, `SELECT COUNT(*) FROM network WHERE network_id = $1`, networkId) == 1
}

// gammaDeletedTestBuy buys item (a plan or a data pack) for the session's
// network with gamma and returns the deposit hotkey, once paid.
func gammaDeletedTestBuy(t testing.TB, f *gammaDbFixture, clientSession *session.ClientSession, args *CreateGammaPaymentIntentArgs) [32]byte {
	intent := gammaDbIntent(t, clientSession, args)
	if intent.Error != nil {
		t.Fatalf("intent refused: %+v", intent.Error)
	}
	hotkey := gammaTestAccount(t, intent.DepositHotkey)
	if f.sim.deposit(hotkey) == nil {
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: gammaDeletedTestDepositBalance})
	}
	f.run()
	f.sim.mine()
	f.run()
	connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
	return hotkey
}

// The deleted account's plan is not renewed at the end of its term: the
// renewal's charge would spend the deposit for a network the credit cannot
// reach.
func TestGammaDeletedAccountIsNeverRenewed(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammadeleterenew")
		f.run()
		hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		paidBalance := f.sim.deposit(hotkey).balance
		connect.AssertEqual(t, paidBalance, gammaDeletedTestDepositBalance-gammaDeletedTestMonthlyPriceGam)

		connect.AssertEqual(t, gammaDeletedTestRemove(t, ctx, clientSession), model.RemoveNetworkRemoved)
		connect.AssertEqual(t, gammaDeletedTestNetworkExists(t, ctx, networkId), false)

		// the end of the term, inside the renewal's lead time: nothing is
		// charged, and the deposit keeps its gamma
		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma)
		connect.AssertEqual(t, renewal.count, 1)
		termEnd := renewal.endTime.Add(-manualPaymentGracePeriod)
		f.now = termEnd.Add(-30 * time.Minute)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'renew'`, networkId), 0)
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, paidBalance)

		// the account's gamma row is kept, with its automatic charges off
		account := model.GetGammaAccount(ctx, networkId)
		if account == nil {
			t.Fatal("deletion removed the gamma account row")
		}
		connect.AssertEqual(t, account.AutoRenew, false)
		connect.AssertEqual(t, account.AutoTopup, false)
		// its deposit hotkey stays assigned, for support
		connect.AssertEqual(t, len(model.GetGammaDepositHotkeys(ctx, networkId)), 1)
	})
}

// A deleted account whose data balance runs out gets no top-up: its balance is
// zero from the deletion on (its data went to a code), so every check would
// buy one, an hour apart, until the deposit runs dry.
func TestGammaDeletedAccountIsNeverToppedUp(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammadeletetopup")
		f.run()
		// a data code: the account pays with gamma, and its own balance is empty
		hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib, Delivery: model.CryptoDeliveryCode})
		connect.AssertEqual(t, model.GetGammaAccount(ctx, networkId).AutoTopup, true)
		paidBalance := f.sim.deposit(hotkey).balance

		connect.AssertEqual(t, gammaDeletedTestRemove(t, ctx, clientSession), model.RemoveNetworkRemoved)
		for range 3 {
			f.autoNow()
			f.run()
			f.now = f.now.Add(time.Hour)
		}
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'topup'`, networkId), 0)
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, paidBalance)
	})
}

// The same once the deleted account's data balance expires: the rows of a
// deleted network stay, and a balance that ends reads as empty.
func TestGammaDeletedAccountIsNeverToppedUpWhenItsBalanceExpires(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammadeleteexpire")
		f.run()
		hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), model.Tib)
		paidBalance := f.sim.deposit(hotkey).balance

		connect.AssertEqual(t, gammaDeletedTestRemove(t, ctx, clientSession), model.RemoveNetworkRemoved)
		// a full balance: no top-up is due either way
		f.autoNow()
		f.run()
		// it expires
		cryptoDbExec(ctx, `UPDATE transfer_balance SET end_time = $2 WHERE network_id = $1`, networkId, server.NowUtc().Add(-time.Minute))
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), model.ByteCount(0))
		f.now = f.now.Add(10 * time.Minute)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'topup'`, networkId), 0)
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, paidBalance)
	})
}

// A seat demand the account raised at its client limit is not answered once
// the account is deleted: the upgrade would buy Pro for a network that is gone.
func TestGammaDeletedAccountIsNeverUpgradedAtItsClientLimit(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammadeleteseat")
		f.run()
		hotkey := gammaDeletedTestBuy(t, f, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib, Delivery: model.CryptoDeliveryCode})
		paidBalance := f.sim.deposit(hotkey).balance
		// at its client limit, before the biller's next automatic pass
		connect.AssertEqual(t, model.NoteGammaSeatDemand(ctx, networkId, f.now), true)

		connect.AssertEqual(t, gammaDeletedTestRemove(t, ctx, clientSession), model.RemoveNetworkRemoved)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'seat'`, networkId), 0)
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, paidBalance)
	})
}

// A purchase the customer started before deleting the account is voided: a
// mint into the deposit afterwards is not spent for it.
func TestGammaDeletedAccountsOpenPurchaseIsNeverCharged(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammadeletepurchase")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		connect.AssertEqual(t, intent.Error == nil, true)

		connect.AssertEqual(t, gammaDeletedTestRemove(t, ctx, clientSession), model.RemoveNetworkRemoved)
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: gammaDeletedTestDepositBalance})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND voided_at IS NOT NULL`, networkId), 1)
		status := gammaDbStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, gammaStatusReview)
		connect.AssertEqual(t, status.ReviewReason, "voided")
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, gammaDeletedTestDepositBalance)
	})
}

// A charge the chain skipped before the deletion is not re-armed when the
// deposit changes after it.
func TestGammaDeletedAccountsSkippedChargeIsNeverRetried(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammadeleteskip")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		coldkey := gammaTestAccount(t, gammaTestColdkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 5_000_000_000})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		// moved away in the block the spend lands in: skipped
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 1_000_000_000})
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Error.Code, GammaErrorCodeInsufficientGamma)

		// a skipped charge is not in flight: the deletion goes ahead
		connect.AssertEqual(t, gammaDeletedTestRemove(t, ctx, clientSession), model.RemoveNetworkRemoved)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 4_000_000_000})
		f.run()
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(4_000_000_000))
	})
}

// A charge in a batch that can still land holds the deletion: once it lands it
// is credited to the account, and the retried deletion goes ahead.
func TestGammaDeletionWaitsForAChargeInFlight(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammadeleteinflight")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: 20_000_000_000})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)

		// submitted, not landed
		connect.AssertEqual(t, gammaDeletedTestRemove(t, ctx, clientSession), gammaDeletedTestInFlight)
		connect.AssertEqual(t, gammaDeletedTestNetworkExists(t, ctx, networkId), true)
		// nothing of the account's gamma was switched off by the refusal
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND voided_at IS NOT NULL`, networkId), 0)

		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), true)

		connect.AssertEqual(t, gammaDeletedTestRemove(t, ctx, clientSession), model.RemoveNetworkRemoved)
		connect.AssertEqual(t, gammaDeletedTestNetworkExists(t, ctx, networkId), false)
		connect.AssertEqual(t, model.GetGammaAccount(ctx, networkId).AutoRenew, false)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm WHERE kind = $1`, gammaDeletedTestCreditAlarm), 0)
	})
}

// A network removed some other way than account deletion (which waits for the
// batch) while its charge is in the live batch: the batch is never sent again,
// and once its era is over the charge stays out of the next one.
func TestGammaLiveBatchIsNeverResubmittedForANetworkThatIsGone(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammagonebatch")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: 20_000_000_000})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)

		cryptoDbExec(ctx, `DELETE FROM network WHERE network_id = $1`, networkId)
		// the pool loses the batch
		f.sim.drop = true
		f.sim.mine()
		f.sim.drop = false
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm WHERE kind = $1`, gammaDeletedTestBatchAlarm), 1)

		// its era runs out: the batch expires and its charge goes nowhere
		f.sim.advance(int(f.settings.EraPeriod) + 2)
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_spend_batch WHERE state = 'expired'`), 1)
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(20_000_000_000))
	})
}

// A spend that lands for a network removed another way cannot be credited:
// the biller raises one alarm for it, so an operator can decide what the
// customer is owed (gamma cannot be refunded on chain).
func TestGammaSpendForANetworkThatIsGoneRaisesAnAlarm(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammagonecredit")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: 20_000_000_000})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)

		cryptoDbExec(ctx, `DELETE FROM network WHERE network_id = $1`, networkId)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(15_000_000_000))
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND credited_at IS NOT NULL`, networkId), 0)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm WHERE kind = $1`, gammaDeletedTestCreditAlarm), 1)
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm WHERE kind = $1`, gammaDeletedTestCreditAlarm), 1)
	})
}
