package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The Play sales notice reads a purchase's earlier credits, oldest first,
// from its renewal rows: retention deletes a balance 7 days after it ends,
// and nothing prunes a renewal row. A grace period's hold (no revenue), every
// other purchase and every other network's rows are left out.
func TestGetPlayPurchaseCreditEndTimesInTx(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, networkId)
		otherNetworkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, otherNetworkId)
		day := 24 * time.Hour
		// the trial began 40 days ago: its balances are long deleted
		start := server.NowUtc().Truncate(time.Second).Add(-40 * day)
		token := "token-" + server.NewId().String()
		add := func(networkId server.Id, market string, purchaseToken string, end time.Time, netRevenue NanoCents) {
			t.Helper()
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				SubscriptionMarket: market,
				StartTime:          start,
				EndTime:            end,
				NetRevenue:         netRevenue,
				PurchaseToken:      purchaseToken,
			}); err != nil {
				t.Fatalf("add renewal: %v", err)
			}
		}
		// the trial, Play's extra day, and the grace period's hold
		add(networkId, SubscriptionMarketGoogle, token, start.Add(15*day), UsdToNanoCents(3.5))
		add(networkId, SubscriptionMarketGoogle, token, start.Add(16*day+time.Second), UsdToNanoCents(3.5))
		add(networkId, SubscriptionMarketGoogle, token, start.Add(22*day), 0)
		// another purchase, the same token on another network, another market
		add(networkId, SubscriptionMarketGoogle, "token-other-"+server.NewId().String(), start.Add(30*day), UsdToNanoCents(3.5))
		add(otherNetworkId, SubscriptionMarketGoogle, token, start.Add(31*day), UsdToNanoCents(3.5))
		add(networkId, SubscriptionMarketStripe, token, start.Add(32*day), UsdToNanoCents(3.5))

		var endTimes []time.Time
		server.Tx(ctx, func(tx server.PgTx) {
			endTimes = GetPlayPurchaseCreditEndTimesInTx(tx, ctx, networkId, token)
		})
		connect.AssertEqual(t, len(endTimes), 2)
		connect.AssertEqual(t, endTimes[0].Equal(start.Add(15*day)), true)
		connect.AssertEqual(t, endTimes[1].Equal(start.Add(16*day+time.Second)), true)

		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, len(GetPlayPurchaseCreditEndTimesInTx(tx, ctx, networkId, "")), 0)
		})
	})
}

// A credit an end path cut (a refund, a revocation, a terminal poll) no longer
// holds the window it was credited for, and is no history: a later credit of
// that window is owed again, and read against the cut row it would look moved
// out without a charge. A credit since paid_end_time ends a day past it, and a
// cut one sooner; an older credit cut within a day of its start ends before its
// start plus the day. Intact credits of both kinds are read.
func TestGetPlayPurchaseCreditEndTimesLeavesOutCutCredits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, networkId)
		day := 24 * time.Hour
		start := server.NowUtc().Truncate(time.Second).Add(-90 * day)
		token := "token-" + server.NewId().String()
		add := func(end time.Time, paidEnd *time.Time) {
			t.Helper()
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				SubscriptionMarket: SubscriptionMarketGoogle,
				StartTime:          start,
				EndTime:            end,
				NetRevenue:         UsdToNanoCents(3.5),
				PurchaseToken:      token,
				PaidEndTime:        paidEnd,
			}); err != nil {
				t.Fatalf("add renewal: %v", err)
			}
		}
		at := func(d time.Duration) *time.Time {
			at := start.Add(d)
			return &at
		}
		// an older credit cut half an hour after the start, and an intact one
		add(start.Add(30*time.Minute), nil)
		add(start.Add(31*day), nil)
		// a credit with a paid end, intact, and one cut ten days into its period
		add(start.Add(62*day), at(61*day))
		add(start.Add(72*day), at(92*day))

		var endTimes []time.Time
		server.Tx(ctx, func(tx server.PgTx) {
			endTimes = GetPlayPurchaseCreditEndTimesInTx(tx, ctx, networkId, token)
		})
		connect.AssertEqual(t, len(endTimes), 2)
		connect.AssertEqual(t, endTimes[0].Equal(start.Add(31*day)), true)
		connect.AssertEqual(t, endTimes[1].Equal(start.Add(62*day)), true)
	})
}

// A data plan writes no renewal row: its history is the purchase's balances
// that carry its data, oldest first. A zero-byte balance, a Pro one, a
// balance cut within a day of its start, every other purchase and every other
// network's balances are left out.
func TestGetPlayPurchaseDataCreditEndTimesInTx(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, networkId)
		otherNetworkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, otherNetworkId)
		day := 24 * time.Hour
		start := server.NowUtc().Truncate(time.Second).Add(-40 * day)
		token := "token-" + server.NewId().String()
		add := func(networkId server.Id, purchaseToken string, end time.Time, byteCount ByteCount, pro bool) {
			t.Helper()
			AddTransferBalance(ctx, &TransferBalance{
				NetworkId:             networkId,
				StartTime:             start,
				EndTime:               end,
				StartBalanceByteCount: byteCount,
				BalanceByteCount:      byteCount,
				PurchaseToken:         purchaseToken,
				Pro:                   pro,
			})
		}
		// the month's allowance, the day Play added (credited before the
		// gate held such days), and the next month's
		add(networkId, token, start.Add(31*day), 10*Tib, false)
		add(networkId, token, start.Add(32*day+time.Second), 10*Tib, false)
		add(networkId, token, start.Add(61*day), 10*Tib, false)
		// none of these
		add(networkId, token, start.Add(30*time.Minute), 10*Tib, false)
		add(networkId, token, start.Add(70*day), 0, false)
		add(networkId, token, start.Add(71*day), 0, true)
		add(networkId, token, start.Add(72*day), Gib, true)
		add(networkId, "token-other-"+server.NewId().String(), start.Add(73*day), 10*Tib, false)
		add(otherNetworkId, token, start.Add(74*day), 10*Tib, false)

		var endTimes []time.Time
		server.Tx(ctx, func(tx server.PgTx) {
			endTimes = GetPlayPurchaseDataCreditEndTimesInTx(tx, ctx, networkId, token)
		})
		connect.AssertEqual(t, len(endTimes), 3)
		connect.AssertEqual(t, endTimes[0].Equal(start.Add(31*day)), true)
		connect.AssertEqual(t, endTimes[1].Equal(start.Add(32*day+time.Second)), true)
		connect.AssertEqual(t, endTimes[2].Equal(start.Add(61*day)), true)

		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, len(GetPlayPurchaseDataCreditEndTimesInTx(tx, ctx, networkId, "")), 0)
		})
	})
}
