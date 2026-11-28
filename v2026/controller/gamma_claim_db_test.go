package controller

// Database tests of the claim flow (gamma_claim.go) and of the daily spend cap
// a new deposit opens with, against the simulated chain of runtime 493
// (gamma_sim_chain_test.go). A direct payment, a hotkey's spend at our service
// from its own deposit, is recorded once and credited only to the network
// that proves it holds the hotkey, from the chain's own total, once. Needs the
// test database and pro.yml.

import (
	"sync"
	"testing"
	"time"

	subkey "github.com/vedhavyas/go-subkey/v2"
	"github.com/vedhavyas/go-subkey/v2/sr25519"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func gammaClaimTestResetLimiter() {
	gammaClaimLimiter.stateLock.Lock()
	gammaClaimLimiter.window = time.Time{}
	gammaClaimLimiter.stateLock.Unlock()
}

func gammaClaimTestChallenge(t testing.TB, clientSession *session.ClientSession, hotkey string) *GammaClaimChallengeResult {
	gammaClaimTestResetLimiter()
	cryptoTestResetLimiters()
	result, err := GammaClaimChallenge(&GammaClaimChallengeArgs{Hotkey: hotkey}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func gammaClaimTestClaim(t testing.TB, clientSession *session.ClientSession, args *GammaClaimArgs) *GammaClaimResult {
	gammaClaimTestResetLimiter()
	cryptoTestResetLimiters()
	result, err := GammaClaim(args, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// A hotkey pays our service straight from its own deposit: the watcher keeps
// one record of it, keyed by the hotkey and the block, and alarms nothing. The
// network that signs the challenge with that hotkey claims it, and the biller
// credits the data it buys, once: a second claim finds nothing to claim.
func TestGammaDirectPaymentIsRecordedAndClaimedOnce(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaclaimdata")
		f.run()
		keypair, payer := gammaTestPayer(t)
		payerKey := gammaTestAccount(t, payer)

		f.sim.directPayment(payerKey, 7_000_000_000, true)
		block := f.sim.mine()
		f.run()
		payments := model.ListGammaDirectPayments(ctx, payer)
		connect.AssertEqual(t, len(payments), 1)
		connect.AssertEqual(t, payments[0].BlockNumber, int64(block.number))
		connect.AssertEqual(t, payments[0].Amount, int64(7_000_000_000))
		// a record, not an alarm
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_alarm WHERE kind <> $1`, model.GammaRecordKindDirectPayment), 0)
		// read again (the cursor does not move back, but the record is keyed):
		// still one
		f.run()
		connect.AssertEqual(t, len(model.ListGammaDirectPayments(ctx, payer)), 1)

		challenge := gammaClaimTestChallenge(t, clientSession, payer)
		connect.AssertEqual(t, challenge.Error == nil, true)
		connect.AssertEqual(t, challenge.ClaimableGamma, "7")
		wantBytes, ok := gammaClaimDataBytes(7_000_000_000)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, challenge.ClaimableDataBytes, wantBytes)
		connect.AssertEqual(t, challenge.Message, gammaClaimMessage(networkId, payer, f.settings.ServiceAddress, f.now.Truncate(time.Second)))
		connect.AssertEqual(t, challenge.SignCommand, gammaClaimSignCommand(challenge.Message))

		claim := gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: gammaTestSign(t, keypair, challenge.Message)})
		if claim.Error != nil {
			t.Fatalf("claim refused: %+v", claim.Error)
		}
		connect.AssertEqual(t, claim.AmountGamma, "7")
		connect.AssertEqual(t, claim.DataBytes, wantBytes)
		connect.AssertEqual(t, claim.RemainingGamma, "0")
		connect.AssertEqual(t, claim.Status.Status, gammaStatusConfirming)
		f.run()
		status := gammaDbStatus(t, clientSession, &GammaPaymentIntentResult{IntentId: claim.IntentId, StatusToken: claim.StatusToken})
		connect.AssertEqual(t, status.Status, gammaStatusPaid)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), wantBytes)
		// the payer's hotkey is the customer's own: no automatic charge from it
		account := model.GetGammaAccount(ctx, networkId)
		connect.AssertEqual(t, account == nil || account.Hotkey == "", true)

		// claimed once
		again := gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: gammaTestSign(t, keypair, challenge.Message)})
		connect.AssertEqual(t, again.Error.Code, GammaErrorCodeNothingToClaim)
		connect.AssertEqual(t, gammaClaimTestChallenge(t, clientSession, payer).Error.Code, GammaErrorCodeNothingToClaim)
		f.run()
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), wantBytes)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE purpose = 'claim'`), 1)

		// a later payment from the same hotkey is claimable on its own
		f.sim.directPayment(payerKey, 3_000_000_000, true)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, len(model.ListGammaDirectPayments(ctx, payer)), 2)
		later := gammaClaimTestChallenge(t, clientSession, payer)
		connect.AssertEqual(t, later.ClaimableGamma, "3")
	})
}

// A payment from before anything recorded it (it is in the chain's total, not
// in any block the watcher read) is claimable; a claim can buy Pro when it
// covers the price, and leaves the rest claimable as data.
func TestGammaClaimIsRetroactiveAndCanBuyAPlan(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaclaimplan")
		f.run()
		keypair, payer := gammaTestPayer(t)
		f.sim.directPayment(gammaTestAccount(t, payer), 12_000_000_000, false)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, len(model.ListGammaDirectPayments(ctx, payer)), 0)

		challenge := gammaClaimTestChallenge(t, clientSession, payer)
		connect.AssertEqual(t, challenge.ClaimableGamma, "12")
		signature := gammaTestSign(t, keypair, challenge.Message)
		// the year is more than is claimable
		yearly := gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: signature, Plan: model.SolanaPlanYearly})
		connect.AssertEqual(t, yearly.Error.Code, GammaErrorCodeInsufficientGamma)
		monthly := gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: signature, Plan: model.SolanaPlanMonthly})
		if monthly.Error != nil {
			t.Fatalf("claim refused: %+v", monthly.Error)
		}
		connect.AssertEqual(t, monthly.AmountGamma, "5")
		connect.AssertEqual(t, monthly.RemainingGamma, "7")
		f.run()
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), true)
		connect.AssertEqual(t, cryptoDbRenewals(t, ctx, networkId, model.SubscriptionMarketGamma).count, 1)

		rest := gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: signature})
		if rest.Error != nil {
			t.Fatalf("claim refused: %+v", rest.Error)
		}
		connect.AssertEqual(t, rest.AmountGamma, "7")
		f.run()
		wantBytes, _ := gammaClaimDataBytes(7_000_000_000)
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, &GammaPaymentIntentResult{IntentId: rest.IntentId, StatusToken: rest.StatusToken}).Status, gammaStatusPaid)
		connect.AssertEqual(t, rest.DataBytes, wantBytes)
		connect.AssertEqual(t, gammaClaimTestChallenge(t, clientSession, payer).Error.Code, GammaErrorCodeNothingToClaim)
	})
}

// Two networks that both hold the hotkey claim at once: one gets it, and the
// other finds nothing to claim.
func TestGammaConcurrentClaimsTakeTheGammaOnce(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		_, _, firstSession := cryptoDbTestNetwork(t, ctx, "gammaclaimracea")
		_, _, secondSession := cryptoDbTestNetwork(t, ctx, "gammaclaimraceb")
		f.run()
		keypair, payer := gammaTestPayer(t)
		f.sim.directPayment(gammaTestAccount(t, payer), 4_000_000_000, true)
		f.sim.mine()
		f.run()
		sessions := []*session.ClientSession{firstSession, secondSession}
		args := []*GammaClaimArgs{}
		for _, clientSession := range sessions {
			challenge := gammaClaimTestChallenge(t, clientSession, payer)
			args = append(args, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: gammaTestSign(t, keypair, challenge.Message)})
		}
		gammaClaimTestResetLimiter()
		results := make([]*GammaClaimResult, 2)
		var wait sync.WaitGroup
		for i := range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				result, err := GammaClaim(args[i], sessions[i])
				if err != nil {
					t.Error(err)
					return
				}
				results[i] = result
			}()
		}
		wait.Wait()
		claimed := 0
		for _, result := range results {
			if result.Error == nil {
				claimed++
				connect.AssertEqual(t, result.AmountGamma, "4")
			} else {
				connect.AssertEqual(t, result.Error.Code, GammaErrorCodeNothingToClaim)
			}
		}
		connect.AssertEqual(t, claimed, 1)
		connect.AssertEqual(t, model.GetGammaClaimedTotal(ctx, payer), int64(4_000_000_000))
	})
}

// A claim fails closed: a signature not the hotkey's, a challenge of another
// account or an expired one, one of our own deposit hotkeys, Gamma not
// selling, or a chain read that fails, each claims nothing.
func TestGammaClaimFailsClosed(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammaclaimclosed")
		_, _, otherSession := cryptoDbTestNetwork(t, ctx, "gammaclaimother")
		f.run()
		keypair, payer := gammaTestPayer(t)
		f.sim.directPayment(gammaTestAccount(t, payer), 5_000_000_000, true)
		f.sim.mine()
		f.run()
		challenge := gammaClaimTestChallenge(t, clientSession, payer)
		signature := gammaTestSign(t, keypair, challenge.Message)
		claims := func() int {
			return gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE purpose = 'claim'`)
		}

		// another key's signature of this hotkey's challenge
		other, err := subkey.DeriveKeyPair(sr25519.Scheme{}, gammaTestAliceSeed+"//someone-else")
		if err != nil {
			t.Fatal(err)
		}
		forged := gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: gammaTestSign(t, other, challenge.Message)})
		connect.AssertEqual(t, forged.Error.Code, GammaErrorCodeSignatureInvalid)
		connect.AssertEqual(t, gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: "0x1234"}).Error.Code, GammaErrorCodeSignatureInvalid)
		// this account's challenge, sent by another account
		connect.AssertEqual(t, gammaClaimTestClaim(t, otherSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: signature}).Error.Code, PurchaseErrorCodeInvalidRequest)
		// a challenge for another service
		elsewhere := gammaClaimMessage(networkId, payer, gammaTestColdkey, f.now)
		connect.AssertEqual(t, gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: elsewhere, Signature: gammaTestSign(t, keypair, elsewhere)}).Error.Code, PurchaseErrorCodeInvalidRequest)
		// expired
		f.now = f.now.Add(gammaClaimTtl + time.Minute)
		f.run()
		connect.AssertEqual(t, gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: signature}).Error.Code, GammaErrorCodeClaimExpired)
		challenge = gammaClaimTestChallenge(t, clientSession, payer)
		signature = gammaTestSign(t, keypair, challenge.Message)
		// one of our deposit hotkeys
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		connect.AssertEqual(t, gammaClaimTestChallenge(t, clientSession, intent.DepositHotkey).Error.Code, GammaErrorCodeDepositHotkey)
		// the chain read fails
		f.sim.failSpentReads = true
		connect.AssertEqual(t, gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: signature}).Error.Code, PurchaseErrorCodeAssetUnavailable)
		connect.AssertEqual(t, gammaClaimTestChallenge(t, clientSession, payer).Error.Code, PurchaseErrorCodeAssetUnavailable)
		f.sim.failSpentReads = false
		// Gamma not selling (our service is not listed)
		f.sim.serviceOk = false
		f.run()
		connect.AssertEqual(t, gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: signature}).Error.Code, PurchaseErrorCodeAssetUnavailable)
		f.sim.serviceOk = true
		f.run()
		connect.AssertEqual(t, claims(), 0)
		// and then it goes through
		ok := gammaClaimTestClaim(t, clientSession, &GammaClaimArgs{Hotkey: payer, Message: challenge.Message, Signature: signature})
		if ok.Error != nil {
			t.Fatalf("claim refused: %+v", ok.Error)
		}
		connect.AssertEqual(t, claims(), 1)
	})
}

// A purchase whose new deposit's cap allows less than the price now waits:
// its status says how long the cap takes to refill, nothing is sent, and the
// charge goes once the cap allows it.
func TestGammaChargeWaitsForTheDailyCapToRefill(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "gammacapwait")
		f.run()
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		connect.AssertEqual(t, intent.MintDailyCapGamma, "50")
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		capUnits := uint64(50_000_000_000)
		allowed := uint64(10_000_000_000)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), balance: 100_000_000_000, cap: &capUnits, available: &allowed})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		status := gammaDbStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, gammaStatusSeen)
		connect.AssertEqual(t, status.Error.Code, GammaErrorCodeDailyCapExceeded)
		connect.AssertEqual(t, status.Error.Message, "The deposit's daily spend cap allows 10 gamma now and refills over a day, so this payment goes through in about 15 hours, on its own. To pay sooner, raise the cap, which refills faster, or remove it.")
		// a mint into the existing deposit keeps its cap
		again := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib})
		connect.AssertEqual(t, again.MintDailyCapGamma, "")
		// refilled
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), balance: 100_000_000_000, cap: &capUnits})
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
	})
}
