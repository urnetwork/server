package model

// An automatic Gamma charge whose switch the account turned off (POST
// /pay/gamma/settings, mmm/BTCPAY.md §18.5) never goes into a spend batch.
// The switches were read only when the biller made an automatic intent, so a
// renewal (or a top-up, or a seat upgrade) made before the switch was turned
// off was charged once its deposit could pay it. The biller now voids such an
// intent in its own step, and the spend batch itself reads the switches under
// a share lock on the account's row: a settings change that turns a switch off
// while a batch is being recorded is ordered with it, so a batch never carries
// a charge the customer was told is off.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// gammaSwitchTestAccount makes a network that pays with gamma, both automatic
// switches on, and returns its deposit hotkey.
func gammaSwitchTestAccount(t testing.TB, ctx context.Context, networkId server.Id, name string, now time.Time) string {
	Testing_CreateNetwork(ctx, networkId, name, server.NewId())
	hotkey := gammaRemovalTestHotkey(t, ctx, networkId, now)
	server.Tx(ctx, func(tx server.PgTx) {
		RecordGammaAccountPaymentInTx(tx, ctx, networkId, hotkey, SolanaPlanMonthly, PriceTierStandard, now)
	})
	account := GetGammaAccount(ctx, networkId)
	connect.AssertEqual(t, account.AutoRenew, true)
	connect.AssertEqual(t, account.AutoTopup, true)
	return hotkey
}

func gammaSwitchTestListed(ctx context.Context, intentId server.Id) bool {
	return slices.ContainsFunc(ListUnbatchedGammaCharges(ctx, 1000), func(charge *GammaCharge) bool {
		return charge.IntentId == intentId
	})
}

// A renewal's charge recorded before the customer turned renewal off: no batch
// takes it and nothing is written, and the next batch's list leaves it out. A
// top-up and a seat upgrade follow automatic top-up the same way; a purchase
// is never switched off.
func TestGammaSpendBatchRefusesAChargeWhoseSwitchIsOff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		hotkey := gammaSwitchTestAccount(t, ctx, networkId, "synthetic-gamma-switch-batch", now)

		renew := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, renew, now), true)
		purchase := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposePurchase, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, purchase, now), true)
		// a renewal with no charge yet
		waiting := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		chargeable := func(intentId server.Id) bool {
			return slices.ContainsFunc(ListChargeableGammaPaymentIntents(ctx, now, 1000), func(intent *GammaPaymentIntent) bool {
				return intent.IntentId == intentId
			})
		}
		connect.AssertEqual(t, chargeable(waiting.IntentId), true)

		off := false
		SetGammaAccountSettings(ctx, networkId, &GammaAccountSettings{AutoRenew: &off}, now)

		err := CreateGammaSpendBatch(ctx, gammaRemovalTestBatch(now), [][]byte{purchase.Reference, renew.Reference})
		if !errors.Is(err, ErrGammaChargeNotPayable) {
			t.Fatalf("batch with a renewal whose switch is off = %v, want %v", err, ErrGammaChargeNotPayable)
		}
		connect.AssertEqual(t, GetLiveGammaSpendBatch(ctx) == nil, true)
		connect.AssertEqual(t, GetGammaChargeForIntent(ctx, renew.IntentId).BatchId == nil, true)
		connect.AssertEqual(t, GetGammaChargeForIntent(ctx, purchase.IntentId).BatchId == nil, true)
		connect.AssertEqual(t, gammaSwitchTestListed(ctx, renew.IntentId), false)
		connect.AssertEqual(t, gammaSwitchTestListed(ctx, purchase.IntentId), true)
		connect.AssertEqual(t, chargeable(waiting.IntentId), false)

		// renewal on again, top-up off: the renewal goes out, a top-up and a
		// seat upgrade do not
		on := true
		SetGammaAccountSettings(ctx, networkId, &GammaAccountSettings{AutoRenew: &on, AutoTopup: &off}, now)
		topup := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeTopup, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, topup, now), true)
		seat := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeSeat, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, seat, now), true)
		for _, refused := range []*GammaPaymentIntent{topup, seat} {
			err := CreateGammaSpendBatch(ctx, gammaRemovalTestBatch(now), [][]byte{refused.Reference})
			if !errors.Is(err, ErrGammaChargeNotPayable) {
				t.Fatalf("batch with a %s whose switch is off = %v, want %v", refused.Purpose, err, ErrGammaChargeNotPayable)
			}
			connect.AssertEqual(t, gammaSwitchTestListed(ctx, refused.IntentId), false)
		}
		connect.AssertEqual(t, gammaSwitchTestListed(ctx, renew.IntentId), true)
		connect.AssertEqual(t, chargeable(waiting.IntentId), true)
		connect.AssertEqual(t, CreateGammaSpendBatch(ctx, gammaRemovalTestBatch(now), [][]byte{purchase.Reference, renew.Reference}), nil)
		connect.AssertEqual(t, GetGammaChargeForIntent(ctx, renew.IntentId).BatchId != nil, true)
	})
}

// The settings change first: it holds the account's row with renewal turned
// off when a spend batch for the renewal is recorded. The batch waits for the
// row and, once the change commits, reads the switch off and records nothing.
// (The batch first is the other order: the settings change waits for it and
// lists its charge as still being charged.)
func TestGammaSpendBatchWaitsForASwitchTurnedOffAndLeavesItsChargeOut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		networkId := server.NewId()
		now := server.NowUtc()
		hotkey := gammaSwitchTestAccount(t, ctx, networkId, "synthetic-gamma-switch-first", now)
		renew := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, renew, now), true)

		settingsLocked := make(chan struct{})
		releaseSettings := make(chan struct{})
		settingsDone := runPaymentModelTest(func() error {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE gamma_account SET auto_renew = false, update_time = $2 WHERE network_id = $1`,
					networkId,
					now,
				))
				close(settingsLocked)
				select {
				case <-releaseSettings:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			return nil
		})
		select {
		case <-settingsLocked:
		case <-ctx.Done():
			close(releaseSettings)
			t.Fatalf("the settings change did not lock the account's row: %v", awaitPaymentModelTest(settingsDone))
		}

		batch := gammaRemovalTestBatch(now)
		batchDone := runPaymentModelTest(func() error {
			return CreateGammaSpendBatch(ctx, batch, [][]byte{renew.Reference})
		})

		waitErr := waitForPaymentLockWaiter(ctx, "gamma-spend-batch-account-lock")
		close(releaseSettings)
		settingsErr := awaitPaymentModelTest(settingsDone)
		batchErr := awaitPaymentModelTest(batchDone)
		if waitErr != nil {
			t.Fatalf("the spend batch did not wait for the account's row the settings change holds: %v (batch: %v)", waitErr, batchErr)
		}
		if settingsErr != nil {
			t.Fatalf("settings change: %v", settingsErr)
		}
		if !errors.Is(batchErr, ErrGammaChargeNotPayable) {
			t.Fatalf("batch after renewal was turned off = %v, want %v", batchErr, ErrGammaChargeNotPayable)
		}
		connect.AssertEqual(t, GetLiveGammaSpendBatch(ctx) == nil, true)
		charge := GetGammaChargeForIntent(ctx, renew.IntentId)
		connect.AssertEqual(t, charge.State, GammaChargePending)
		connect.AssertEqual(t, charge.BatchId == nil, true)
		connect.AssertEqual(t, gammaSwitchTestListed(ctx, renew.IntentId), false)
	})
}
