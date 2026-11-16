package model

// The biller's void of the automatic Gamma intents whose switch the account
// turned off (VoidSwitchedOffGammaIntents), and what the settings change says
// is still being charged (SetGammaAccountSettings). The void never touches a
// charge in flight (in a spend batch that can still land, or spent and not
// yet credited): a spend that lands for a voided intent is never credited, and
// gamma cannot be refunded. Those charges are listed instead, so the customer
// is told they can still be charged once.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

func gammaSwitchTestIntentIds[T any](items []T, id func(T) server.Id) []server.Id {
	ids := []server.Id{}
	for _, item := range items {
		ids = append(ids, id(item))
	}
	slices.SortFunc(ids, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return ids
}

func gammaSwitchTestSortedIds(ids ...server.Id) []server.Id {
	sorted := slices.Clone(ids)
	slices.SortFunc(sorted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return sorted
}

// Renewal turned off: the renewal waiting for gamma, the one whose charge is
// in no batch, the one the chain skipped and the seat upgrade are voided; the
// renewal in a live batch and the one spent and not yet credited are left,
// and listed as still being charged. A top-up (automatic top-up is still on),
// a purchase, and another account's renewal are left. A voided intent's
// pending charge no longer holds its deposit, its skipped charge is never
// re-armed, and its period is free for the switch turned back on.
func TestVoidSwitchedOffGammaIntentsLeavesChargesInFlight(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		hotkey := gammaSwitchTestAccount(t, ctx, networkId, "synthetic-gamma-switch-void", now)
		otherNetworkId := server.NewId()
		otherHotkey := gammaSwitchTestAccount(t, ctx, otherNetworkId, "synthetic-gamma-switch-void-other", now)

		// spent from a batch that landed, not yet credited
		spent := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, spent, now), true)
		landed := gammaRemovalTestBatch(now)
		connect.AssertEqual(t, CreateGammaSpendBatch(ctx, landed, [][]byte{spent.Reference}), nil)
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, ResolveGammaChargeInTx(tx, ctx, spent.Reference, &GammaChargeResolution{
				State: GammaChargeSpent, BlockNumber: 1, BlockHash: "0x01", EventIndex: 0,
			}, now), true)
			connect.AssertEqual(t, IncludeGammaSpendBatchInTx(tx, ctx, landed.BatchId, &GammaBatchInclusion{BlockNumber: 1, BlockHash: "0x01"}, "", now), true)
		})
		// in the live batch
		inBatch := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, inBatch, now), true)
		connect.AssertEqual(t, CreateGammaSpendBatch(ctx, gammaRemovalTestBatch(now), [][]byte{inBatch.Reference}), nil)
		// not yet charged, charged in no batch, skipped by the chain
		waiting := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		unbatched := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, unbatched, now), true)
		skipped := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, skipped, now), true)
		gammaRemovalTestResolve(t, ctx, skipped.Reference, GammaChargeSkipped, now)
		seat := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeSeat, now)
		topup := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeTopup, now)
		purchase := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposePurchase, now)
		other := gammaRemovalTestIntent(t, ctx, otherNetworkId, otherHotkey, GammaPurposeRenew, now)
		// the unbatched renewal holds its price on the deposit
		connect.AssertEqual(t, SumPendingGammaCharges(ctx)[hotkey], inBatch.AmountGamma()+unbatched.AmountGamma())

		off := false
		inFlight := SetGammaAccountSettings(ctx, networkId, &GammaAccountSettings{AutoRenew: &off}, now)
		connect.AssertEqual(
			t,
			gammaSwitchTestIntentIds(inFlight, func(charge *GammaAutomaticChargeInFlight) server.Id { return charge.IntentId }),
			gammaSwitchTestSortedIds(spent.IntentId, inBatch.IntentId),
		)
		for _, charge := range inFlight {
			connect.AssertEqual(t, charge.Purpose, GammaPurposeRenew)
			connect.AssertEqual(t, charge.Item, SolanaPlanMonthly)
			connect.AssertEqual(t, charge.Amount, UsdToNanoCents(5))
			switch charge.IntentId {
			case spent.IntentId:
				connect.AssertEqual(t, charge.ChargeState, GammaChargeSpent)
			case inBatch.IntentId:
				connect.AssertEqual(t, charge.ChargeState, GammaChargePending)
			}
		}
		connect.AssertEqual(t, len(GetGammaSwitchedOffChargesInFlight(ctx, networkId)), 2)
		connect.AssertEqual(t, len(GetGammaSwitchedOffChargesInFlight(ctx, otherNetworkId)), 0)

		voided := VoidSwitchedOffGammaIntents(ctx, now)
		connect.AssertEqual(
			t,
			gammaSwitchTestIntentIds(voided, func(intent *GammaPaymentIntent) server.Id { return intent.IntentId }),
			gammaSwitchTestSortedIds(waiting.IntentId, unbatched.IntentId, skipped.IntentId, seat.IntentId),
		)
		for _, intent := range []*GammaPaymentIntent{waiting, unbatched, skipped, seat} {
			connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, intent.IntentId), true)
			connect.AssertEqual(t, GetGammaPaymentIntent(ctx, intent.IntentId).PeriodKey == nil, true)
		}
		for _, intent := range []*GammaPaymentIntent{spent, inBatch, topup, purchase, other} {
			connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, intent.IntentId), false)
		}
		// the charge in no batch is voided with its intent and holds nothing
		charge := GetGammaChargeForIntent(ctx, unbatched.IntentId)
		connect.AssertEqual(t, charge.State, GammaChargeVoided)
		connect.AssertEqual(t, charge.BatchId == nil, true)
		connect.AssertEqual(t, charge.ResolvedAt != nil, true)
		connect.AssertEqual(t, SumPendingGammaCharges(ctx)[hotkey], inBatch.AmountGamma())
		connect.AssertEqual(t, gammaSwitchTestListed(ctx, unbatched.IntentId), false)
		// the skipped charge stays skipped, and is never re-armed
		connect.AssertEqual(t, GetGammaChargeForIntent(ctx, skipped.IntentId).State, GammaChargeSkipped)
		connect.AssertEqual(t, ReArmGammaCharge(ctx, skipped.Reference), false)
		// the charges in flight are untouched
		connect.AssertEqual(t, GetGammaChargeForIntent(ctx, inBatch.IntentId).State, GammaChargePending)
		connect.AssertEqual(t, GetGammaChargeForIntent(ctx, inBatch.IntentId).BatchId != nil, true)
		connect.AssertEqual(t, GetGammaChargeForIntent(ctx, spent.IntentId).State, GammaChargeSpent)
		// voiding again voids nothing more
		connect.AssertEqual(t, len(VoidSwitchedOffGammaIntents(ctx, now)), 0)

		// the waiting renewal's period is free: the switch turned back on makes
		// a new renewal for it
		on := true
		SetGammaAccountSettings(ctx, networkId, &GammaAccountSettings{AutoRenew: &on}, now)
		again := *waiting
		again.IntentId = server.NewId()
		again.Reference = append(append([]byte{}, again.IntentId.Bytes()...), server.NewId().Bytes()...)
		again.StatusTokenHash = again.IntentId.Bytes()
		again.VoidedAt = nil
		connect.AssertEqual(t, again.PeriodKey != nil, true)
		connect.AssertEqual(t, CreateGammaPaymentIntent(ctx, &again), true)
		// a period still held is not made twice
		twice := again
		twice.IntentId = server.NewId()
		twice.Reference = append(append([]byte{}, twice.IntentId.Bytes()...), server.NewId().Bytes()...)
		connect.AssertEqual(t, CreateGammaPaymentIntent(ctx, &twice), false)
		connect.AssertEqual(t, len(VoidSwitchedOffGammaIntents(ctx, now)), 0)

		// top-up off: the top-up is voided, the renewal is not
		SetGammaAccountSettings(ctx, networkId, &GammaAccountSettings{AutoTopup: &off}, now)
		voided = VoidSwitchedOffGammaIntents(ctx, now)
		connect.AssertEqual(
			t,
			gammaSwitchTestIntentIds(voided, func(intent *GammaPaymentIntent) server.Id { return intent.IntentId }),
			[]server.Id{topup.IntentId},
		)
		connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, again.IntentId), false)
		connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, purchase.IntentId), false)
	})
}

// The spend batch first: it holds the account's row (share) with the
// renewal's charge in it when the customer turns renewal off. The settings
// change waits for the row, then lists the charge as still being charged; the
// biller's void leaves it.
func TestGammaSettingsWaitForASpendBatchAndListItsCharge(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		networkId := server.NewId()
		now := server.NowUtc()
		hotkey := gammaSwitchTestAccount(t, ctx, networkId, "synthetic-gamma-switch-batch-first", now)
		renew := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, renew, now), true)

		batchId := server.NewId()
		batchLocked := make(chan struct{})
		releaseBatch := make(chan struct{})
		batchDone := runPaymentModelTest(func() error {
			server.Tx(ctx, func(tx server.PgTx) {
				// what CreateGammaSpendBatch takes and writes
				server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM gamma_account WHERE network_id = $1 FOR SHARE`, networkId))
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO gamma_spend_batch (batch_id, nonce, era_birth, era_period, extrinsic, extrinsic_hash, state, create_time)
						VALUES ($1, 0, 1, 64, $2, $3, 'signed', $4)
					`,
					batchId,
					[]byte{0x01},
					"0x"+batchId.String(),
					now,
				))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE gamma_charge SET batch_id = $1 WHERE reference = $2`, batchId, renew.Reference))
				close(batchLocked)
				select {
				case <-releaseBatch:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			return nil
		})
		select {
		case <-batchLocked:
		case <-ctx.Done():
			close(releaseBatch)
			t.Fatalf("the batch did not lock the account's row: %v", awaitPaymentModelTest(batchDone))
		}

		var inFlight []*GammaAutomaticChargeInFlight
		settingsDone := runPaymentModelTest(func() error {
			off := false
			inFlight = SetGammaAccountSettings(ctx, networkId, &GammaAccountSettings{AutoRenew: &off}, now)
			return nil
		})
		waitErr := waitForPaymentLockWaiter(ctx, "gamma-account-settings-lock")
		close(releaseBatch)
		batchErr := awaitPaymentModelTest(batchDone)
		settingsErr := awaitPaymentModelTest(settingsDone)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if batchErr != nil {
			t.Fatalf("batch: %v", batchErr)
		}
		if settingsErr != nil {
			t.Fatalf("settings change: %v", settingsErr)
		}
		connect.AssertEqual(t, len(inFlight), 1)
		connect.AssertEqual(t, inFlight[0].IntentId, renew.IntentId)
		connect.AssertEqual(t, inFlight[0].ChargeState, GammaChargePending)
		connect.AssertEqual(t, GetGammaAccount(ctx, networkId).AutoRenew, false)
		connect.AssertEqual(t, len(VoidSwitchedOffGammaIntents(ctx, now)), 0)
		connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, renew.IntentId), false)
		connect.AssertEqual(t, *GetGammaChargeForIntent(ctx, renew.IntentId).BatchId, batchId)
	})
}
