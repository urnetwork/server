package model

// One Gamma plan charge at a time (mmm/BTCPAY.md §18.5): the state the biller
// holds a plan intent by, and the void a plan purchase's credit makes of the
// renewal or seat upgrade it replaces. The void never touches an intent whose
// charge is in flight: a spend that lands for a voided intent is never
// credited, and gamma cannot be refunded.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

var gammaPlanChargeTestPlans = []string{SolanaPlanMonthly, SolanaPlanYearly, SolanaPlanYearlyOnboarding}

func TestGammaPlanChargeStateAndTheReplacedVoid(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-gamma-plan-charge", server.NewId())
		now := server.NowUtc()
		hotkey := gammaRemovalTestHotkey(t, ctx, networkId, now)

		// a renewal with no charge, a seat upgrade whose charge was skipped, a
		// renewal whose charge is in a live batch, and the plan purchase
		waiting := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		skipped := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeSeat, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, skipped, now), true)
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, ResolveGammaChargeInTx(tx, ctx, skipped.Reference, &GammaChargeResolution{
				State: GammaChargeSkipped, BlockNumber: 1, BlockHash: "0x01", SkipReason: "insufficient_gamma",
			}, now), true)
		})
		inBatch := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposeRenew, now)
		connect.AssertEqual(t, CreateGammaCharge(ctx, inBatch, now), true)
		connect.AssertEqual(t, CreateGammaSpendBatch(ctx, gammaRemovalTestBatch(now), [][]byte{inBatch.Reference}), nil)
		purchase := gammaRemovalTestIntent(t, ctx, networkId, hotkey, GammaPurposePurchase, now.Add(time.Second))

		// the renewal in the batch is a plan charge in flight for every other
		// plan intent of the network, not for itself
		charging, paidSince := GetGammaPlanChargeState(ctx, networkId, gammaPlanChargeTestPlans, &purchase.IntentId, &purchase.CreateTime)
		connect.AssertEqual(t, charging, true)
		connect.AssertEqual(t, paidSince, false)
		charging, _ = GetGammaPlanChargeState(ctx, networkId, gammaPlanChargeTestPlans, nil, nil)
		connect.AssertEqual(t, charging, true)
		charging, _ = GetGammaPlanChargeState(ctx, networkId, gammaPlanChargeTestPlans, &inBatch.IntentId, nil)
		connect.AssertEqual(t, charging, false)
		// a data pack is no plan charge
		charging, _ = GetGammaPlanChargeState(ctx, networkId, []string{"data_1tib"}, nil, nil)
		connect.AssertEqual(t, charging, false)

		connect.AssertEqual(t, HasOpenGammaPlanPurchase(ctx, networkId, gammaPlanChargeTestPlans, now), true)
		connect.AssertEqual(t, HasOpenGammaPlanPurchase(ctx, networkId, gammaPlanChargeTestPlans, purchase.ExpiresAt), false)
		connect.AssertEqual(t, HasOpenGammaPlanPurchase(ctx, networkId, []string{"data_1tib"}, now), false)

		// the purchase's credit voids what it replaces, never a charge in flight
		var voided, inFlight int64
		server.Tx(ctx, func(tx server.PgTx) {
			voided, inFlight = VoidReplacedGammaPlanIntentsInTx(tx, ctx, networkId, gammaPlanChargeTestPlans, now)
		})
		connect.AssertEqual(t, voided, int64(2))
		connect.AssertEqual(t, inFlight, int64(1))
		connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, waiting.IntentId), true)
		connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, skipped.IntentId), true)
		connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, inBatch.IntentId), false)
		connect.AssertEqual(t, gammaRemovalTestVoided(t, ctx, purchase.IntentId), false)
		// a voided intent's skipped charge is never re-armed
		connect.AssertEqual(t, ReArmGammaCharge(ctx, skipped.Reference), false)

		// the renewal lands and is credited after the purchase was made: the
		// purchase is never charged for those days
		gammaRemovalTestResolve(t, ctx, inBatch.Reference, GammaChargeSpent, now)
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, MarkGammaPaymentIntentCreditedInTx(tx, ctx, inBatch.IntentId, now.Add(time.Minute)), true)
		})
		charging, paidSince = GetGammaPlanChargeState(ctx, networkId, gammaPlanChargeTestPlans, &purchase.IntentId, &purchase.CreateTime)
		connect.AssertEqual(t, charging, false)
		connect.AssertEqual(t, paidSince, true)
		// one made after that credit is not held by it
		later := purchase.CreateTime.Add(time.Hour)
		_, paidSince = GetGammaPlanChargeState(ctx, networkId, gammaPlanChargeTestPlans, &purchase.IntentId, &later)
		connect.AssertEqual(t, paidSince, false)
	})
}
