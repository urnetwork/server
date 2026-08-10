package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// TestX402GrantProMonthRollingWindow pins the S9 window fix: pro_1month buys a
// ROLLING month from the purchase, not the remainder of the calendar month. The
// old ProGrantWindow(now) grant meant paying full price on the 28th bought ~3
// days.
func TestX402GrantProMonthRollingWindow(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "x402rolling", userId)

		sku := &X402Sku{
			SkuId:     X402SkuProMonth,
			PriceUsd:  5,
			Pro:       true,
			ByteCount: model.Pro().DataAmount(true),
		}

		before := server.NowUtc()
		err := x402GrantProMonth(ctx, networkId, sku, model.UsdToNanoCents(sku.PriceUsd), &X402SettleResponse{
			Success:     true,
			Transaction: "0xrolling1",
			Network:     "base",
		})
		connect.AssertEqual(t, err, nil)
		after := server.NowUtc()

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].Pro, true)
		// a full month (plus grace) from the PURCHASE time, wherever in the
		// calendar month it falls
		connect.AssertEqual(t, balances[0].EndTime.Sub(balances[0].StartTime), x402ProMonthDuration+SubscriptionGracePeriod)
		// the window starts at the purchase (a second of slack absorbs timestamp
		// truncation in the db), not at the top of the calendar month
		connect.AssertEqual(t, balances[0].StartTime.After(before.Add(-time.Second)), true)
		connect.AssertEqual(t, balances[0].StartTime.Before(after.Add(time.Second)), true)

		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)

		active, market := model.HasSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeSupporter)
		connect.AssertEqual(t, active, true)
		connect.AssertEqual(t, *market, model.SubscriptionMarketX402)
	})
}

// TestX402GrantIdempotentOnSettleTransaction pins the settle->grant idempotency:
// an agent whose settlement succeeded but whose response was lost retries with the
// same signed payment; the facilitator settles it to the SAME transaction, and the
// grant keyed on that transaction must land exactly once. A purchase settled to a
// DIFFERENT transaction is a real second purchase and EXTENDS: its own renewal row
// (the key includes market, and the rolling windows are distinct) and its own
// month of balance.
func TestX402GrantIdempotentOnSettleTransaction(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "x402idem", userId)

		sku := &X402Sku{
			SkuId:     X402SkuProMonth,
			PriceUsd:  5,
			Pro:       true,
			ByteCount: model.Pro().DataAmount(true),
		}
		netRevenue := model.UsdToNanoCents(sku.PriceUsd)

		settle := &X402SettleResponse{
			Success:     true,
			Transaction: "0xidem1",
			Network:     "base",
		}

		err := x402GrantProMonth(ctx, networkId, sku, netRevenue, settle)
		connect.AssertEqual(t, err, nil)

		// the retry with the same settle transaction grants nothing new
		err = x402GrantProMonth(ctx, networkId, sku, netRevenue, settle)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 1)

		// a SECOND purchase (a new settle transaction) extends: a second balance
		// and a second renewal row
		err = x402GrantProMonth(ctx, networkId, sku, netRevenue, &X402SettleResponse{
			Success:     true,
			Transaction: "0xidem2",
			Network:     "base",
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 2)

		var renewalCount int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT COUNT(*) FROM subscription_renewal
				WHERE network_id = $1 AND market = $2
				`,
				networkId,
				model.SubscriptionMarketX402,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&renewalCount))
				}
			})
		})
		connect.AssertEqual(t, renewalCount, 2)

		// the data grant is idempotent the same way, keyed on its purchase token
		dataNetworkId := server.NewId()
		dataUserId := server.NewId()
		model.Testing_CreateNetwork(ctx, dataNetworkId, "x402idemdata", dataUserId)

		dataSku := &X402Sku{
			SkuId:     X402SkuData1Tib,
			PriceUsd:  5,
			Pro:       false,
			ByteCount: 1 * model.Tib,
		}
		dataSettle := &X402SettleResponse{
			Success:     true,
			Transaction: "0xidemdata1",
			Network:     "base",
		}
		for i := 0; i < 2; i += 1 {
			err = x402GrantData(ctx, dataNetworkId, dataSku, model.UsdToNanoCents(dataSku.PriceUsd), dataSettle)
			connect.AssertEqual(t, err, nil)
		}
		dataBalances := model.GetActiveTransferBalances(ctx, dataNetworkId)
		connect.AssertEqual(t, len(dataBalances), 1)
		connect.AssertEqual(t, dataBalances[0].BalanceByteCount, 1*model.Tib)
		connect.AssertEqual(t, dataBalances[0].Pro, false)
	})
}

// TestX402SecondPurchaseInOneCalendarMonthExtends pins the collapse half of S9
// end to end at the window level: two purchases made the same day used to share
// the calendar-month window exactly and upsert onto one renewal row, so the
// second full-price payment extended NOTHING. With rolling windows (distinct
// start/end per purchase) and market in the renewal key, the entitlement now
// runs to the LATER window's end.
func TestX402SecondPurchaseInOneCalendarMonthExtends(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "x402extend", userId)

		sku := &X402Sku{
			SkuId:     X402SkuProMonth,
			PriceUsd:  5,
			Pro:       true,
			ByteCount: model.Pro().DataAmount(true),
		}
		netRevenue := model.UsdToNanoCents(sku.PriceUsd)

		err := x402GrantProMonth(ctx, networkId, sku, netRevenue, &X402SettleResponse{
			Success: true, Transaction: "0xextend1", Network: "base",
		})
		connect.AssertEqual(t, err, nil)

		// time passes within the same calendar month
		time.Sleep(10 * time.Millisecond)

		err = x402GrantProMonth(ctx, networkId, sku, netRevenue, &X402SettleResponse{
			Success: true, Transaction: "0xextend2", Network: "base",
		})
		connect.AssertEqual(t, err, nil)

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 2)

		// the second purchase's window ends AFTER the first's: it extended
		var minEnd, maxEnd time.Time
		for _, balance := range balances {
			if minEnd.IsZero() || balance.EndTime.Before(minEnd) {
				minEnd = balance.EndTime
			}
			if balance.EndTime.After(maxEnd) {
				maxEnd = balance.EndTime
			}
		}
		connect.AssertEqual(t, minEnd.Before(maxEnd), true)
	})
}
