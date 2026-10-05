package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// TestHasSubscriptionRenewalInTxSeesUncommittedRenewal: the in-tx read sees a renewal
// the tx wrote and has not committed. The pooled read runs outside the tx and cannot.
func TestHasSubscriptionRenewalInTxSeesUncommittedRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "renewalintx", server.NewId())

		now := server.NowUtc()
		var renewalErr error
		inTxActive := false
		var inTxMarket *string
		pooledActive := true
		server.Tx(ctx, func(tx server.PgTx) {
			renewalErr = AddSubscriptionRenewalInTx(tx, ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				StartTime:          now.Add(-24 * time.Hour),
				EndTime:            now.Add(29 * 24 * time.Hour),
				NetRevenue:         UsdToNanoCents(5),
				SubscriptionMarket: SubscriptionMarketApple,
			})
			if renewalErr != nil {
				return
			}
			inTxActive, inTxMarket = HasSubscriptionRenewalInTx(tx, ctx, networkId, SubscriptionTypeSupporter)
			pooledActive, _ = HasSubscriptionRenewal(ctx, networkId, SubscriptionTypeSupporter)
		})
		connect.AssertEqual(t, renewalErr, nil)
		connect.AssertEqual(t, inTxActive, true)
		connect.AssertNotEqual(t, inTxMarket, nil)
		connect.AssertEqual(t, *inTxMarket, string(SubscriptionMarketApple))
		connect.AssertEqual(t, pooledActive, false)

		// committed, the two reads agree
		active, market := HasSubscriptionRenewal(ctx, networkId, SubscriptionTypeSupporter)
		connect.AssertEqual(t, active, true)
		connect.AssertNotEqual(t, market, nil)
		connect.AssertEqual(t, *market, string(SubscriptionMarketApple))
	})
}
