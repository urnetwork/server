package model

// Pay with Gamma in the re-engagement and win-back market lists. A plan paid
// with gamma records its end with the 30-day grace in it (controller
// gamma_credit.go adds manualPaymentGracePeriod), like Stripe's and the crypto
// markets', and only ever records a payment as its revenue. The lists named
// every other paid market but gamma: a lapsed gamma payer was timed as an App
// Store one (the store's retry days on top of a grace already past), and was
// never a payer the win-back offer may go to.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Every market whose window ends with the grace in it is a grace market; the
// stores' are not.
func TestReengagementGraceMarketsTakeEveryMarketWhoseEndHoldsTheGrace(t *testing.T) {
	graceMarkets := ReengagementGraceMarkets()
	for _, market := range slices.Concat([]string{SubscriptionMarketStripe, SubscriptionMarketGamma}, SubscriptionOneTimeCryptoMarkets) {
		if !slices.Contains(graceMarkets, market) {
			t.Errorf("grace markets %v leave out %s", graceMarkets, market)
		}
	}
	for _, store := range []string{SubscriptionMarketApple, SubscriptionMarketGoogle} {
		if slices.Contains(graceMarkets, store) {
			t.Errorf("grace markets %v take the store %s", graceMarkets, store)
		}
	}
}

// The win-back design (mmm/reengagement/PLAN.md, stream W) takes every network
// that paid for Pro, whatever the market: every subscription market but the
// two stores, whose revenue is not what was charged, counts a window with
// revenue as a payment, and a refund event of any of them counts.
func TestWinbackMarketListsTakeEveryPaidMarket(t *testing.T) {
	for _, market := range SubscriptionMarkets {
		if !slices.Contains(winbackRefundEventStores, market) {
			t.Errorf("win-back refund stores %v leave out %s", winbackRefundEventStores, market)
		}
		store := market == SubscriptionMarketApple || market == SubscriptionMarketGoogle
		if slices.Contains(winbackChargedRevenueMarkets, market) == store {
			t.Errorf("win-back charged-revenue markets %v: %s in = %t, want %t", winbackChargedRevenueMarkets, market, !store, store)
		}
	}
}

// A network that paid for Pro with gamma is a former payer: win-back takes it,
// on the grace markets' timing.
func TestWinbackTakesAGammaPayer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		// a month and its grace, as the gamma credit records it
		end := start.Add(60 * 24 * time.Hour)
		paid := winbackTestNetwork(t, ctx)
		addWinbackTestRenewal(t, ctx, paid, SubscriptionMarketGamma, start, end, UsdToNanoCents(5), server.NewId().String())
		connect.AssertEqual(t, true, NetworkHasPaidSupporterPeriod(ctx, paid))

		inGrace := func(grace bool) bool {
			for _, c := range ListWinbackReengagementCandidates(ctx, end.Add(-time.Hour), end.Add(time.Hour), grace, 100) {
				if c.NetworkId == paid {
					return true
				}
			}
			return false
		}
		connect.AssertEqual(t, true, inGrace(true))
		connect.AssertEqual(t, false, inGrace(false))
	})
}
