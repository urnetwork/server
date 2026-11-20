package controller

// Database tests of Pay with Gamma end to end against the simulated chain of
// runtime 493 (gamma_sim_chain_test.go): an intent assigns the account's
// deposit hotkey; the biller charges only what the deposit can pay, signs each
// charge's spend as the deposit's own hotkey (the key of the deposit seed at
// the hotkey's index), has the chain judge it, records the charge and the
// signed spend before submitting, and credits only on the GammaSpent our own
// extrinsic emitted; a charge the deposit or the chain refuses is skipped
// before anything is sent and retried only once the deposit changes; a spend
// the chain stops taking after it was sent stays live until its era is over;
// an expired spend's charge goes out again with the same reference and is
// spent once; a moved nonce without our spend, a spend by one of our hotkeys
// we never issued, and an unreviewed runtime each stop and alarm, while a
// block a retired runtime ran is read while none of our spends is in flight;
// renewals, top-ups and seat upgrades are charged automatically, once per
// period, within the monthly limit, and never change the provide mode. Needs
// the test database and pro.yml.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type gammaDbFixture struct {
	t        testing.TB
	ctx      context.Context
	settings *gammaSettings
	sim      *gammaSimChain
	now      time.Time
	session  *session.ClientSession
}

func newGammaDbFixture(t testing.TB) *gammaDbFixture {
	settings := gammaTestSettings(t)
	fixture := &gammaDbFixture{
		t:        t,
		ctx:      context.Background(),
		settings: settings,
		sim:      newGammaSimChain(t, settings),
		// the database judges balances at its own clock: start an hour back,
		// so what the biller grants is already active when the test reads it
		now: server.NowUtc().Add(-time.Hour).Truncate(time.Second),
	}
	restoreSettings := setGammaSettingsForTest(settings, nil)
	previousChain := gammaChainFor
	chain := newGammaChain(fixture.sim)
	gammaChainFor = func(endpoints []string) *gammaChain { return chain }
	previousNow := gammaNow
	gammaNow = func() time.Time { return fixture.now }
	gammaAutoLock.Lock()
	gammaAutoLastRun = time.Time{}
	gammaAutoLock.Unlock()
	if testingT, ok := t.(*testing.T); ok {
		testingT.Cleanup(func() {
			restoreSettings()
			gammaChainFor = previousChain
			gammaNow = previousNow
		})
	}
	fixture.session = session.Testing_CreateClientSession(fixture.ctx, nil)
	return fixture
}

func (self *gammaDbFixture) run() *GammaBillerResult {
	result, err := RunGammaBiller(self.session)
	if err != nil {
		self.t.Fatal(err)
	}
	return result
}

// autoNow lets the next run make automatic intents.
func (self *gammaDbFixture) autoNow() {
	gammaAutoLock.Lock()
	gammaAutoLastRun = time.Time{}
	gammaAutoLock.Unlock()
}

func gammaDbIntent(t testing.TB, clientSession *session.ClientSession, args *CreateGammaPaymentIntentArgs) *GammaPaymentIntentResult {
	cryptoTestResetLimiters()
	result, err := CreateGammaPaymentIntent(args, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func gammaDbStatus(t testing.TB, clientSession *session.ClientSession, intent *GammaPaymentIntentResult) *GammaPaymentStatusResult {
	cryptoTestResetLimiters()
	result, err := GammaPaymentStatus(&GammaPaymentStatusArgs{IntentId: intent.IntentId, StatusToken: intent.StatusToken}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func gammaDbCount(t testing.TB, ctx context.Context, sql string, args ...any) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, sql, args...).Scan(&count))
	})
	return
}

const gammaTestColdkey = "5FHneW46xGXgs5mUiveU4sbTyGBzmstUspZC92UhjJM694ty"

func TestGammaPurchaseIsChargedOnceAndCreditedOnTheSpend(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammapurchase")

		// the first run starts the watcher at the head and judges readiness
		connect.AssertEqual(t, f.run().Readiness, gammaReady)
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		connect.AssertEqual(t, intent.Error == nil, true)
		connect.AssertEqual(t, intent.Netuid, uint16(25))
		// runtime 493: a deposit names no spender
		connect.AssertEqual(t, intent.Spender, "")
		connect.AssertEqual(t, intent.ServiceHotkey, f.settings.ServiceAddress)
		_, firstHotkey, _ := gammaDepositHotkeyAt(f.settings.DepositHotkeyMaster, 0)
		connect.AssertEqual(t, intent.DepositHotkey, firstHotkey)
		connect.AssertEqual(t, intent.AmountGamma, "40")
		connect.AssertEqual(t, intent.MinDailyCapGamma, "40")
		connect.AssertEqual(t, intent.Status.Status, gammaStatusPending)
		connect.AssertEqual(t, len(intent.Reference), 66)
		// the mint names the deposit hotkey only (bittensor 11.3.5)
		connect.AssertEqual(t, intent.MintCommand, "btcli tx mint-gamma --netuid 25 --hotkey "+firstHotkey+" --amount-tao "+intent.IndicativeTao+" -w <your_coldkey>")

		// the same purchase again is the same intent, with a new token
		again := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		connect.AssertEqual(t, again.IntentId, intent.IntentId)
		connect.AssertEqual(t, again.Reference, intent.Reference)
		if again.StatusToken == intent.StatusToken {
			t.Fatal("the reused intent handed out the same token")
		}

		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		coldkey := gammaTestAccount(t, gammaTestColdkey)
		// too little: seen, with what remains
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, balance: 30_000_000_000})
		f.run()
		status := gammaDbStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, gammaStatusSeen)
		connect.AssertEqual(t, status.RemainingGamma, "10")
		connect.AssertEqual(t, len(f.sim.submitted), 0)

		// enough: charged, the spend recorded before it is sent, signed by the
		// deposit's own hotkey (nonce 0), at our service, fee-free
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, balance: 50_000_000_000})
		result := f.run()
		connect.AssertEqual(t, result.Charged, 1)
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		spend := f.sim.parse(f.sim.submitted[0])
		connect.AssertEqual(t, *spend, gammaSimSpend{signer: hotkey, nonce: 0, service: f.settings.ServiceHotkey, netuid: 25, amount: 40_000_000_000})
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_spend_batch WHERE state = 'submitted' AND extrinsic_hash = $1`, gammaExtrinsicHash(f.sim.submitted[0]).Hex()), 1)
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusConfirming)
		// a run before it lands resubmits the same bytes and charges nothing more
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)

		f.now = f.now.Add(time.Minute)
		block := f.sim.mine()
		connect.AssertEqual(t, len(block.extrinsics), 1)
		f.run()
		status = gammaDbStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, gammaStatusPaid)
		connect.AssertEqual(t, *status.SpentBlock, int64(block.number))
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(10_000_000_000))
		// the chain's total of what the hotkey spent at our service
		connect.AssertEqual(t, f.sim.spentTotal(f.settings.ServiceHotkey, hotkey), uint64(40_000_000_000))
		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma)
		connect.AssertEqual(t, renewal.count, 1)
		connect.AssertEqual(t, renewal.netRevenue, model.UsdToNanoCents(40))
		connect.AssertEqual(t, renewal.endTime.Sub(renewal.startTime), SubscriptionYearDuration+manualPaymentGracePeriod)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), true)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE template_name = 'gamma_receipt'`), 1)

		// the account now pays with gamma: both automatic switches on
		account := model.GetGammaAccount(ctx, networkId)
		connect.AssertEqual(t, account.AutoRenew, true)
		connect.AssertEqual(t, account.AutoTopup, true)
		connect.AssertEqual(t, account.RenewPlan, model.SolanaPlanYearly)
		connect.AssertEqual(t, account.Hotkey, intent.DepositHotkey)

		// nothing is charged twice
		f.run()
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(10_000_000_000))
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).count, 1)
	})
}

// The customer moves the gamma away after our spend was sent: the chain
// refuses the spend, which never enters a block, and could still land if the
// deposit were refilled within its era. So it stays live, and its charge is
// never signed again, until the era is over; then, with the deposit short,
// the charge is skipped before anything new is signed, and goes out again
// (same reference, a new spend) only once the deposit changes.
func TestGammaSkippedSpendFailsCleanlyAndRetriesOnlyAfterTheDepositChanges(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaskip")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		coldkey := gammaTestAccount(t, gammaTestColdkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, balance: 5_000_000_000})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		first := f.sim.submitted[0]
		// the customer moves the gamma away before the block: the spend no
		// longer passes and does not enter it
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, balance: 1_000_000_000})
		block := f.sim.mine()
		connect.AssertEqual(t, len(block.extrinsics), 0)
		f.run()
		// within its era the spend stays live: resubmitted (and refused), its
		// charge in flight, nothing credited, nothing else signed
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_spend_batch WHERE state = 'submitted'`), 1)
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusConfirming)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), model.ByteCount(0))

		// the era is over and every block it covered was read: the spend
		// expired; the deposit cannot pay, so the charge is skipped and nothing
		// new is signed
		f.sim.advance(int(f.settings.EraPeriod) + 2)
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_spend_batch WHERE state = 'expired'`), 1)
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		status := gammaDbStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, gammaStatusSeen)
		connect.AssertEqual(t, status.Error.Code, GammaErrorCodeInsufficientGamma)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).count, 0)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), model.ByteCount(0))

		// no retry while the deposit is unchanged
		f.run()
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_spend_batch`), 1)

		// the deposit changes: the same charge, same reference, goes out again
		// in a new spend, and is spent once
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, balance: 4_000_000_000})
		f.run()
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		if string(f.sim.submitted[0]) == string(first) {
			t.Fatal("the expired spend was sent again")
		}
		connect.AssertEqual(t, gammaReplaceTestSubmittedReferences(f), []string{intent.Reference})
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), model.Tib)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(1_000_000_000))
		connect.AssertEqual(t, f.sim.spentTotal(f.settings.ServiceHotkey, hotkey), uint64(3_000_000_000))
	})
}

// A charge the chain itself refuses at its judgment (a rule the biller does
// not mirror) is skipped before anything is recorded or sent, and goes out
// again only once its deposit changes.
func TestGammaChargeTheChainRefusesIsSkippedBeforeAnythingIsSent(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammarefused")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		coldkey := gammaTestAccount(t, gammaTestColdkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, balance: 5_000_000_000})
		f.sim.refuse = true
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_spend_batch`), 0)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_charge WHERE state = 'skipped' AND skip_reason = 'refused'`), 1)
		status := gammaDbStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, gammaStatusSeen)
		connect.AssertEqual(t, status.Error.Code, GammaErrorCodeInvalidAmount)

		// the chain takes it again, but the deposit is as it was: no retry
		f.sim.refuse = false
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		// the deposit changes: sent, and spent
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, balance: 6_000_000_000})
		f.run()
		connect.AssertEqual(t, gammaReplaceTestSubmittedReferences(f), []string{intent.Reference})
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
	})
}

func TestGammaExpiredBatchGoesOutAgainWithTheSameReference(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaexpire")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		connect.AssertEqual(t, intent.AmountGamma, "5")
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), balance: 20_000_000_000})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		first := f.sim.submitted[0]

		// the pool loses it, and its era runs out
		f.sim.drop = true
		f.sim.mine()
		f.sim.drop = false
		f.sim.advance(int(f.settings.EraPeriod) + 2)
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_spend_batch WHERE state = 'expired'`), 1)
		// the same run sent the charge again, in a new extrinsic, same reference,
		// at the hotkey's nonce the lost spend never used
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		second := f.sim.submitted[0]
		if string(first) == string(second) {
			t.Fatal("the expired extrinsic was sent again")
		}
		spend := f.sim.parse(second)
		connect.AssertEqual(t, spend.signer, hotkey)
		connect.AssertEqual(t, spend.nonce, uint64(0))
		connect.AssertEqual(t, gammaReplaceTestSubmittedReferences(f), []string{intent.Reference})
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(15_000_000_000))
	})
}

func TestGammaMovedNonceWithoutOurBatchHoldsForReview(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammanonce")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanMonthly})
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), balance: 20_000_000_000})
		f.run()
		f.sim.drop = true
		f.sim.mine()
		f.sim.drop = false
		// something else used the deposit hotkey's nonce
		f.sim.setNonce(hotkey, 1)
		f.sim.advance(int(f.settings.EraPeriod) + 2)
		result := f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_spend_batch WHERE state = 'review'`), 1)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm WHERE kind = $1`, gammaAlarmUnexpectedNonce), 1)
		connect.AssertEqual(t, result.Readiness, gammaBatchInReview)
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusConfirming)
	})
}

// A spend by one of our deposit hotkeys that is none of our spends, at our
// service or at another, raises an alarm (we hold those keys, so it is the
// first sign of a leaked deposit seed) and credits nothing. A spend at our
// service from a hotkey we never assigned is no alarm and no credit: it stays
// in that hotkey's GammaSpent total.
func TestGammaSpendWeNeverIssuedRaisesAnAlarm(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaunknownspend")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		ours := gammaTestAccount(t, intent.DepositHotkey)
		elsewhere := gammaTestAccount(t, "5FLSigC9HGRKVhB9FiEo4Y3koPsNmBmLJbpXg2mp1hXcS59Y")
		foreign := gammaTestAccount(t, gammaTestColdkey)
		f.sim.extraEvents = append(f.sim.extraEvents,
			f.sim.spentEvent(ours, f.settings.ServiceHotkey, 1_000_000_000),
			f.sim.spentEvent(ours, elsewhere, 2_000_000_000),
			f.sim.spentEvent(foreign, f.settings.ServiceHotkey, 3_000_000_000),
		)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm WHERE kind = $1`, gammaAlarmUnknownReference), 2)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm`), 2)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE credited_at IS NOT NULL`), 0)
		// the cursor moved on
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_charge WHERE state = 'spent'`), 0)
		connect.AssertEqual(t, f.run().Readiness, gammaReady)
	})
}

func TestGammaUnreviewedRuntimePausesTheBillerWithoutCrashing(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		f.run()
		start := *model.GetGammaChainState(ctx).ScannedBlock
		// the next block runs a runtime whose GammaSpent is not as reviewed
		f.sim.unsupportedParent[uint64(start)+1] = true
		f.sim.mine()
		f.sim.mine()
		result := f.run()
		connect.AssertEqual(t, result.Readiness, gammaRuntimeUnsupported)
		connect.AssertEqual(t, *model.GetGammaChainState(ctx).ScannedBlock, start)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm WHERE kind = $1`, gammaAlarmRuntimeUnsupported), 1)
		// again: still paused, still no crash, one alarm
		result = f.run()
		connect.AssertEqual(t, result.Readiness, gammaRuntimeUnsupported)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm WHERE kind = $1`, gammaAlarmRuntimeUnsupported), 1)
		// a reviewed runtime resumes the cursor
		delete(f.sim.unsupportedParent, uint64(start)+1)
		result = f.run()
		connect.AssertEqual(t, result.Readiness, gammaReady)
		connect.AssertEqual(t, *model.GetGammaChainState(ctx).ScannedBlock, start+2)
	})
}

// The last block of an older runtime (the spender model, as block 9,549,412
// was under 492 before 493 took over) is read for what kept its names while
// none of our spends is in flight, and the cursor moves past it. With one of
// our spends in flight, the same block holds the cursor and pauses, with an
// alarm: a spend could not be resolved from it.
func TestGammaRetiredRuntimeBlockIsReadOnlyWithNothingInFlight(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaretired")
		f.run()
		start := *model.GetGammaChainState(ctx).ScannedBlock
		f.sim.retiredParent[uint64(start)+1] = true
		f.sim.mine()
		f.sim.mine()
		result := f.run()
		connect.AssertEqual(t, result.Readiness, gammaReady)
		connect.AssertEqual(t, *model.GetGammaChainState(ctx).ScannedBlock, start+2)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm`), 0)

		// a spend in flight: the next retired block holds the cursor
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), balance: 5_000_000_000})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		held := *model.GetGammaChainState(ctx).ScannedBlock
		f.sim.retiredParent[uint64(held)+1] = true
		f.sim.drop = true
		f.sim.mine()
		f.sim.drop = false
		result = f.run()
		connect.AssertEqual(t, result.Readiness, gammaRuntimeUnsupported)
		connect.AssertEqual(t, *model.GetGammaChainState(ctx).ScannedBlock, held)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm WHERE kind = $1`, gammaAlarmRuntimeUnsupported), 1)
	})
}

func TestGammaUnavailableUntilTheOnChainSetupExists(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammasetup")

		// a serving copy of gamma.yml (no deposit seed) never bills, but
		// serves: an api host does not need the deposit seed
		serving, err := parseGammaSettings(gammaTestYaml(func(y *gammaYaml) {
			y.Gamma.DepositHotkeySeed = ""
			y.Gamma.RpcUrls = []string{"sim://gamma"}
		}))
		if err != nil {
			t.Fatal(err)
		}
		restoreServing := setGammaSettingsForTest(serving, nil)
		result := f.run()
		connect.AssertEqual(t, result.Configured, false)
		connect.AssertEqual(t, result.Readiness, gammaNotConfigured)
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		restoreServing()

		for _, test := range []struct {
			setup func()
			want  string
		}{
			// a spend at a hotkey with no service is refused: our service must
			// be published; nothing else (no spender, proxy or fee float)
			{func() { f.sim.serviceOk = false }, gammaServiceNotPublished},
			{func() { f.sim.serviceOk = true }, gammaReady},
		} {
			test.setup()
			connect.AssertEqual(t, f.run().Readiness, test.want)
			terms, err := GammaTerms(clientSession)
			if err != nil {
				t.Fatal(err)
			}
			connect.AssertEqual(t, terms.Available, test.want == gammaReady)
			intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
			if test.want == gammaReady {
				connect.AssertEqual(t, intent.Error == nil, true)
			} else {
				connect.AssertEqual(t, intent.Error.Code, PurchaseErrorCodeAssetUnavailable)
			}
		}
		// the api's copy of gamma.yml names another deposit master than the
		// biller signs for (a rollout half done): nothing is sold, since a
		// hotkey it assigned could not be spent from
		otherMaster, err := parseGammaSettings(gammaTestYaml(func(y *gammaYaml) {
			y.Gamma.DepositHotkeySeed = ""
			y.Gamma.DepositHotkeyMaster = gammaTestColdkey
			y.Gamma.RpcUrls = []string{"sim://gamma"}
		}))
		if err != nil {
			t.Fatal(err)
		}
		restoreOther := setGammaSettingsForTest(otherMaster, nil)
		split, err := GammaTerms(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, split.Available, false)
		connect.AssertEqual(t, split.UnavailableReason, gammaNotConfigured)
		refused := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		connect.AssertEqual(t, refused.Error.Code, PurchaseErrorCodeAssetUnavailable)
		connect.AssertEqual(t, refused.Error.Message, gammaUnavailableMessage)
		restoreOther()
		agreed, err := GammaTerms(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, agreed.Available, true)

		// minting off (a stale oracle): terms say so, and a new purchase the
		// deposit cannot already pay is refused as asset_unavailable
		f.sim.priceAge = 301
		f.run()
		terms, _ := GammaTerms(clientSession)
		connect.AssertEqual(t, terms.MintAvailable, false)
		connect.AssertEqual(t, terms.MintUnavailableReason, gammaMintPriceStale)
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData10Tib})
		connect.AssertEqual(t, intent.Error.Code, PurchaseErrorCodeAssetUnavailable)
		connect.AssertEqual(t, intent.MintUnavailableReason, gammaMintPriceStale)
	})
}

// A payer whose coldkey cannot open a deposit at the account's hotkey gets a
// second hotkey, derived at the next index.
func TestGammaSecondHotkeyForAnotherColdkey(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammacoldkey")
		f.run()
		first := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib, Coldkey: gammaTestColdkey})
		hotkey := gammaTestAccount(t, first.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), balance: 1})
		f.run()
		other := "5FLSigC9HGRKVhB9FiEo4Y3koPsNmBmLJbpXg2mp1hXcS59Y"
		second := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData10Tib, Coldkey: other})
		if second.DepositHotkey == first.DepositHotkey {
			t.Fatal("a coldkey that cannot open the deposit got the same hotkey")
		}
		hotkeys := model.GetGammaDepositHotkeys(ctx, networkId)
		connect.AssertEqual(t, len(hotkeys), 2)
		_, want, _ := gammaDepositHotkeyAt(f.settings.DepositHotkeyMaster, hotkeys[1].DerivationIndex)
		connect.AssertEqual(t, second.DepositHotkey, want)
		// the first coldkey still gets its own
		third := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData10Tib, Coldkey: gammaTestColdkey})
		connect.AssertEqual(t, third.DepositHotkey, first.DepositHotkey)
	})
}

// A hotkey of an earlier deposit master, which the configured seed does not
// derive, is never handed out again: nothing here could spend from it. The
// account gets a hotkey of the current master, at the next index, and that is
// the one charged.
func TestGammaHotkeyOfAnEarlierMasterIsNotHandedOutAgain(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaoldmaster")
		f.run()
		_, old, err := gammaDepositHotkeyAt(gammaTestAccount(t, "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG"), 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := model.AllocateGammaDepositHotkey(ctx, networkId, func(index int64) (string, error) { return old, nil }, f.now); err != nil {
			t.Fatal(err)
		}
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		connect.AssertEqual(t, intent.Error == nil, true)
		if intent.DepositHotkey == old {
			t.Fatal("a hotkey of an earlier master was handed out")
		}
		hotkeys := model.GetGammaDepositHotkeys(ctx, networkId)
		connect.AssertEqual(t, len(hotkeys), 2)
		_, want, _ := gammaDepositHotkeyAt(f.settings.DepositHotkeyMaster, hotkeys[1].DerivationIndex)
		connect.AssertEqual(t, intent.DepositHotkey, want)

		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), balance: 5_000_000_000})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		connect.AssertEqual(t, f.sim.parse(f.sim.submitted[0]).signer, hotkey)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
	})
}

func TestGammaRenewsTopsUpAndUpgradesAutomatically(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaauto")
		f.run()
		coldkey := gammaTestAccount(t, gammaTestColdkey)

		// a free account buys a data code: it pays with gamma from now on
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib, Delivery: model.CryptoDeliveryCode})
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, balance: 100_000_000_000})
		f.run()
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
		connect.AssertEqual(t, model.GetGammaAccount(ctx, networkId).AutoTopup, true)

		// a provider client: an automatic upgrade never changes how it provides
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "gamma device", "test")
		model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{model.ProvideModeNetwork: make([]byte, 32)})
		before, err := model.GetProvideModes(ctx, clientId)
		if err != nil {
			t.Fatal(err)
		}

		// its data runs out (the code went elsewhere): a top-up, once an hour
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'topup'`, networkId), 1)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), model.Tib)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'topup'`, networkId), 1)

		// at its client limit: upgraded to Pro monthly, once a day
		connect.AssertEqual(t, model.NoteGammaSeatDemand(ctx, networkId, f.now), true)
		f.autoNow()
		f.run()
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'seat' AND credited_at IS NOT NULL`, networkId), 1)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), true)
		after, err := model.GetProvideModes(ctx, clientId)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, after, before)

		// the month ends: the plan renews from where the term ends
		renewal := cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma)
		termEnd := renewal.endTime.Add(-manualPaymentGracePeriod)
		f.now = termEnd.Add(-30 * time.Minute)
		f.autoNow()
		f.run()
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'renew' AND credited_at IS NOT NULL`, networkId), 1)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM subscription_renewal WHERE network_id = $1 AND market = 'gamma' AND start_time = $2`, networkId, termEnd), 1)
		// subscription details say it renews
		var renewalIntentId string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT transaction_id FROM subscription_renewal WHERE network_id = $1 AND market = 'gamma' ORDER BY end_time DESC LIMIT 1`, networkId).Scan(&renewalIntentId))
		})
		state, err := gammaLookupSubscriptionState(ctx, renewalIntentId)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, *state.AutoRenew, true)
		connect.AssertEqual(t, state.CancelAtPeriodEnd, false)

		// a monthly limit below the price: the next renewal is not made
		limit := "1"
		settings, err := SetGammaSettings(&GammaSettingsArgs{MonthlyLimitGamma: &limit}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, settings.AutoRenew, true)
		connect.AssertEqual(t, settings.MonthlyLimitGamma, "1")
		var lastEnd time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT MAX(end_time) FROM subscription_renewal WHERE network_id = $1 AND market = 'gamma'`, networkId).Scan(&lastEnd))
		})
		f.now = lastEnd.Add(-manualPaymentGracePeriod).Add(-30 * time.Minute)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'renew'`, networkId), 1)

		// no limit, renewal off: still nothing
		autoRenew := false
		noLimit := "0"
		settings, err = SetGammaSettings(&GammaSettingsArgs{AutoRenew: &autoRenew, MonthlyLimitGamma: &noLimit}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, settings.AutoRenew, false)
		connect.AssertEqual(t, settings.MonthlyLimitGamma, "")
		// and now that it runs out at the end of the term
		state, err = gammaLookupSubscriptionState(ctx, renewalIntentId)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, *state.AutoRenew, false)
		connect.AssertEqual(t, state.CancelAtPeriodEnd, true)
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'renew'`, networkId), 1)

		// on again: the renewal is made for that term, once
		autoRenew = true
		SetGammaSettings(&GammaSettingsArgs{AutoRenew: &autoRenew}, clientSession)
		f.autoNow()
		f.run()
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'renew'`, networkId), 2)
	})
}
