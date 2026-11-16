package model

// Account deletion and Pay with Gamma (mmm/BTCPAY.md §18.5). Deletion once
// checked the Stripe, App Store and Google Play renewals only, so a deleted
// account's gamma_account row kept automatic renewal, top-up and its seat
// demand on, its open intents stayed chargeable, and a charge already in a
// spend batch could land after the network was gone, with nothing left to
// credit. The deletion now ends the automatic charges and voids the open
// intents, refuses while a charge is in flight, and a spend batch takes the
// network row lock the deletion takes, so the two are ordered.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// The outcome the fix adds, by its wire value.
const gammaRemovalTestInFlight = RemoveNetworkOutcome("gamma_charge_in_flight")

// gammaRemovalTestHotkey assigns the network a deposit hotkey of its own.
func gammaRemovalTestHotkey(t testing.TB, ctx context.Context, networkId server.Id, now time.Time) string {
	hotkey, err := AllocateGammaDepositHotkey(ctx, networkId, func(index int64) (string, error) {
		return "hk-" + networkId.String(), nil
	}, now)
	connect.AssertEqual(t, err, nil)
	return hotkey.Hotkey
}

// gammaRemovalTestIntent records an open intent of purpose for the network.
func gammaRemovalTestIntent(t testing.TB, ctx context.Context, networkId server.Id, hotkey string, purpose string, now time.Time) *GammaPaymentIntent {
	id := server.NewId()
	reference := append(append([]byte{}, id.Bytes()...), server.NewId().Bytes()...)
	var periodKey *string
	if purpose != GammaPurposePurchase {
		key := purpose + "/" + id.String()
		periodKey = &key
	}
	intent := &GammaPaymentIntent{
		IntentId:                 id,
		StatusTokenHash:          id.Bytes(),
		NetworkId:                networkId,
		Hotkey:                   hotkey,
		Purpose:                  purpose,
		PeriodKey:                periodKey,
		Reference:                reference,
		Delivery:                 CryptoDeliveryAccount,
		Item:                     SolanaPlanMonthly,
		PriceUsdNanoCents:        UsdToNanoCents(5),
		RegularPriceUsdNanoCents: UsdToNanoCents(5),
		CreateTime:               now,
		ExpiresAt:                now.Add(24 * time.Hour),
	}
	connect.AssertEqual(t, CreateGammaPaymentIntent(ctx, intent), true)
	return intent
}

func gammaRemovalTestBatch(now time.Time) *GammaSpendBatch {
	return &GammaSpendBatch{
		BatchId:       server.NewId(),
		Nonce:         0,
		EraBirth:      1,
		EraPeriod:     64,
		Extrinsic:     []byte{0x01},
		ExtrinsicHash: "0x" + server.NewId().String(),
		CreateTime:    now,
	}
}

func gammaRemovalTestResolve(t testing.TB, ctx context.Context, reference []byte, state string, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		connect.AssertEqual(t, ResolveGammaChargeInTx(tx, ctx, reference, &GammaChargeResolution{
			State:       state,
			BlockNumber: 1,
			BlockHash:   "0x01",
			EventIndex:  0,
		}, now), true)
	})
}

func gammaRemovalTestRemove(ctx context.Context, networkId server.Id, userId server.Id) RemoveNetworkOutcome {
	outcome, _ := RemoveNetworkWithStoreSnapshot(ctx, networkId, &userId, GetRemoveNetworkStoreSnapshot(ctx, networkId))
	return outcome
}

func gammaRemovalTestVoided(t testing.TB, ctx context.Context, intentId server.Id) bool {
	intent := GetGammaPaymentIntent(ctx, intentId)
	if intent == nil {
		t.Fatalf("intent %s is gone", intentId)
	}
	return intent.VoidedAt != nil
}

// A deletion turns the account's automatic gamma charges off, as it cancels
// a store subscription, and voids every open intent, so none is charged after
// it; a credited intent and the deposit hotkey are history and stay.
func TestRemoveNetworkEndsGammaAutomaticCharges(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-gamma-removal", userId)
		now := server.NowUtc()
		hotkey := gammaRemovalTestHotkey(t, ctx, networkId, now)
		server.Tx(ctx, func(tx server.PgTx) {
			RecordGammaAccountPaymentInTx(tx, ctx, networkId, hotkey, SolanaPlanMonthly, PriceTierStandard, now)
		})
		connect.AssertEqual(t, NoteGammaSeatDemand(ctx, networkId, now), true)

		credited := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposePurchase, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, credited, now), true)
		gammaRemovalTestResolve(t, ctx, credited.Reference, GammaChargeSpent, now)
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, MarkGammaPaymentIntentCreditedInTx(tx, ctx, credited.IntentId, now), true)
		})
		// open: a purchase with no charge yet, a renewal whose charge is in no
		// batch, a top-up whose charge the chain skipped
		purchase := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposePurchase, now)
		renew := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, renew, now), true)
		topup := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeTopup, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, topup, now), true)
		gammaRemovalTestResolve(t, ctx, topup.Reference, GammaChargeSkipped, now)

		connect.AssertEqual(t, gammaRemovalTestRemove(ctx, networkId, userId), RemoveNetworkRemoved)
		connect.AssertEqual(t, removeNetworkTestNetworkExists(ctx, networkId), false)

		account := GetGammaAccount(ctx, networkId)
		if account == nil {
			t.Fatal("the gamma account row is gone")
		}
		connect.AssertEqual(t, account.AutoRenew, false)
		connect.AssertEqual(t, account.AutoTopup, false)
		connect.AssertEqual(t, account.SeatDemandAt == nil, true)
		connect.AssertEqual(t, account.Hotkey, hotkey)
		for _, intent := range []*GammaPaymentIntent{purchase, renew, topup} {
			connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, intent.IntentId), true)
		}
		connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, credited.IntentId), false)
		connect.AssertEqual(t, GetGammaPaymentIntent(ctx, credited.IntentId).CreditedAt != nil, true)
		connect.AssertEqual(t, len(GetGammaDepositHotkeys(ctx, networkId)), 1)

		// nothing of the account is left for the biller to charge
		for _, listed := range ListGammaAccounts(ctx, nil, 1000) {
			if listed.NetworkId == networkId {
				t.Fatal("the deleted account is still listed for automatic charges")
			}
		}
		for _, intent := range ListChargeableGammaPaymentIntents(ctx, now, 1000) {
			if intent.NetworkId == networkId {
				t.Fatalf("intent %s of the deleted account is chargeable", intent.IntentId)
			}
		}
		for _, charge := range ListUnbatchedGammaCharges(ctx, 1000) {
			if charge.IntentId == renew.IntentId {
				t.Fatal("the deleted account's pending charge can join a batch")
			}
		}
		for _, charge := range ListSkippedGammaCharges(ctx, now, 1000) {
			if charge.IntentId == topup.IntentId {
				t.Fatal("the deleted account's skipped charge can be re-armed")
			}
		}
		connect.AssertEqual(t, ReArmGammaCharge(ctx, topup.Reference), false)
	})
}

// A gamma charge in a batch that can still land, or spent and not yet
// credited, holds the deletion: the network must be there for its credit. A
// skipped or a pending charge in no batch can no longer land and does not.
func TestRemoveNetworkWaitsForAGammaChargeInFlight(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-gamma-in-flight", userId)
		now := server.NowUtc()
		hotkey := gammaRemovalTestHotkey(t, ctx, networkId, now)

		intent := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposePurchase, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, intent, now), true)
		batch := gammaRemovalTestBatch(now)
		connect.AssertEqual(t, CreateGammaSpendBatch(ctx, batch, [][]byte{intent.Reference}), nil)

		// signed into a live batch
		connect.AssertEqual(t, gammaRemovalTestRemove(ctx, networkId, userId), gammaRemovalTestInFlight)
		connect.AssertEqual(t, removeNetworkTestNetworkExists(ctx, networkId), true)
		connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, intent.IntentId), false)

		// landed and spent, not yet credited
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, ResolveGammaChargeInTx(tx, ctx, intent.Reference, &GammaChargeResolution{
				State: GammaChargeSpent, BlockNumber: 1, BlockHash: "0x01", EventIndex: 0,
			}, now), true)
			connect.AssertEqual(t, IncludeGammaSpendBatchInTx(tx, ctx, batch.BatchId, &GammaBatchInclusion{BlockNumber: 1, BlockHash: "0x01"}, "", now), true)
		})
		connect.AssertEqual(t, gammaRemovalTestRemove(ctx, networkId, userId), gammaRemovalTestInFlight)
		connect.AssertEqual(t, removeNetworkTestNetworkExists(ctx, networkId), true)

		// credited: the deletion goes ahead
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, MarkGammaPaymentIntentCreditedInTx(tx, ctx, intent.IntentId, now), true)
		})
		// a pending charge in no batch can no longer land: it does not hold
		pending := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, pending, now), true)
		connect.AssertEqual(t, gammaRemovalTestRemove(ctx, networkId, userId), RemoveNetworkRemoved)
		connect.AssertEqual(t, removeNetworkTestNetworkExists(ctx, networkId), false)
		connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, pending.IntentId), true)

		// and its charge never joins a batch
		err := CreateGammaSpendBatch(ctx, gammaRemovalTestBatch(now), [][]byte{pending.Reference})
		if !errors.Is(err, ErrGammaChargeNotPayable) {
			t.Fatalf("batch of a deleted account's charge = %v, want %v", err, ErrGammaChargeNotPayable)
		}
		connect.AssertEqual(t, GetLiveGammaSpendBatch(ctx) == nil, true)
	})
}

// The deletion first: a spend batch waits on the network row lock the
// deletion holds and, once the deletion commits, leaves the deleted account's
// charge out and records nothing. (The batch first is the refusal above: the
// deletion sees its charge in a live batch.)
func TestGammaSpendBatchWaitsForAccountDeletionAndLeavesItsChargeOut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-gamma-delete-first", userId)
		now := server.NowUtc()
		hotkey := gammaRemovalTestHotkey(t, ctx, networkId, now)
		intent := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, intent, now), true)

		deleteLocked := make(chan struct{})
		releaseDelete := make(chan struct{})
		deleteDone := runPaymentModelTest(func() error {
			server.Tx(ctx, func(tx server.PgTx) {
				_, found := lockPaymentNetworkForRemoveInTx(tx, ctx, networkId)
				if !found {
					server.Raise(fmt.Errorf("synthetic network missing before delete"))
				}
				close(deleteLocked)
				select {
				case <-releaseDelete:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network WHERE network_id = $1`, networkId))
			}, server.TxReadCommitted, server.OptNoRetry())
			return nil
		})
		select {
		case <-deleteLocked:
		case <-ctx.Done():
			if err := awaitPaymentModelTest(deleteDone); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("delete setup cleanup: %v", err)
			}
			t.Fatal("delete transaction did not acquire its row lock")
		}

		batch := gammaRemovalTestBatch(now)
		batchDone := runPaymentModelTest(func() error {
			return CreateGammaSpendBatch(ctx, batch, [][]byte{intent.Reference})
		})

		waitErr := waitForPaymentLockWaiter(ctx, "gamma-spend-batch-network-lock")
		close(releaseDelete)
		deleteErr := awaitPaymentModelTest(deleteDone)
		batchErr := awaitPaymentModelTest(batchDone)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		if deleteErr != nil {
			t.Fatalf("delete transaction: %v", deleteErr)
		}
		if !errors.Is(batchErr, ErrGammaChargeNotPayable) {
			t.Fatalf("batch after the deletion = %v, want %v", batchErr, ErrGammaChargeNotPayable)
		}
		connect.AssertEqual(t, GetLiveGammaSpendBatch(ctx) == nil, true)
		charge := GetGammaChargeForIntent(ctx, intent.IntentId)
		connect.AssertEqual(t, charge.State, GammaChargePending)
		connect.AssertEqual(t, charge.BatchId == nil, true)
		if slices.ContainsFunc(ListUnbatchedGammaCharges(ctx, 1000), func(c *GammaCharge) bool { return c.IntentId == intent.IntentId }) {
			t.Fatal("the deleted account's charge can still join a batch")
		}
	})
}
