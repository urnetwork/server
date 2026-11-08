package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The Play sales notice reads a purchase's earlier credits, oldest first,
// leaving out a grace period's zero-byte hold and every other purchase.
func TestGetPurchaseTokenCreditEndTimesInTx(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, networkId)
		start := server.NowUtc().Truncate(time.Second).Add(-20 * 24 * time.Hour)
		token := "token-" + server.NewId().String()
		add := func(purchaseToken string, end time.Time, byteCount ByteCount) {
			server.Tx(ctx, func(tx server.PgTx) {
				AddTransferBalanceInTx(ctx, tx, &TransferBalance{
					NetworkId:             networkId,
					StartTime:             start,
					EndTime:               end,
					StartBalanceByteCount: byteCount,
					BalanceByteCount:      byteCount,
					SubsidyNetRevenue:     UsdToNanoCents(3.5),
					PurchaseToken:         purchaseToken,
					Pro:                   true,
				})
			})
		}
		// the trial, Play's extra day, and the grace period's hold
		add(token, start.Add(15*24*time.Hour), 600*Gib)
		add(token, start.Add(16*24*time.Hour+time.Second), 600*Gib)
		add(token, start.Add(22*24*time.Hour), 0)
		add("token-other-"+server.NewId().String(), start.Add(30*24*time.Hour), 600*Gib)

		var endTimes []time.Time
		server.Tx(ctx, func(tx server.PgTx) {
			endTimes = GetPurchaseTokenCreditEndTimesInTx(tx, ctx, token)
		})
		connect.AssertEqual(t, len(endTimes), 2)
		connect.AssertEqual(t, endTimes[0].Equal(start.Add(15*24*time.Hour)), true)
		connect.AssertEqual(t, endTimes[1].Equal(start.Add(16*24*time.Hour+time.Second)), true)

		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, len(GetPurchaseTokenCreditEndTimesInTx(tx, ctx, "")), 0)
		})
	})
}
