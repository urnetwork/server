package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The reconciler's Play data plans: a purchase-token balance with data, no
// Pro and no net revenue, whose window is open now, with no renewal row for
// its token, on a network that still exists, latest window end first. A Pro
// period (renewal row), an x402 data grant (net revenue), a code grant (no
// token), a Pro marker, a window that ended, and a network with no row are
// left out.
func TestGetActivePlayDataPlansInConn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		day := 24 * time.Hour
		start := now.Add(-40 * day)

		add := func(networkId server.Id, purchaseToken string, end time.Time, pro bool, netRevenue NanoCents) {
			t.Helper()
			AddTransferBalance(ctx, &TransferBalance{
				NetworkId:             networkId,
				StartTime:             start,
				EndTime:               end,
				StartBalanceByteCount: 10 * Gib,
				BalanceByteCount:      10 * Gib,
				NetRevenue:            netRevenue,
				SubsidyNetRevenue:     UsdToNanoCents(3.5),
				PurchaseToken:         purchaseToken,
				Pro:                   pro,
			})
		}
		network := func() server.Id {
			networkId := server.NewId()
			testingCreatePaymentNetworkRow(ctx, networkId)
			return networkId
		}

		// two data plans, an earlier and a later window end; the later plan
		// also has an ended window
		planNetworkId := network()
		planToken := "token-plan-" + server.NewId().String()
		add(planNetworkId, planToken, now.Add(10*day), false, 0)
		laterNetworkId := network()
		laterToken := "token-later-" + server.NewId().String()
		add(laterNetworkId, laterToken, now.Add(-20*day), false, 0)
		add(laterNetworkId, laterToken, now.Add(20*day), false, 0)

		// none of these
		proNetworkId := network()
		proToken := "token-pro-" + server.NewId().String()
		add(proNetworkId, proToken, now.Add(10*day), false, 0)
		if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:          proNetworkId,
			SubscriptionType:   SubscriptionTypeSupporter,
			SubscriptionMarket: SubscriptionMarketGoogle,
			StartTime:          start,
			EndTime:            now.Add(10 * day),
			NetRevenue:         UsdToNanoCents(3.5),
			PurchaseToken:      proToken,
		}); err != nil {
			t.Fatalf("add renewal: %v", err)
		}
		add(network(), "x402-"+server.NewId().String(), now.Add(10*day), false, UsdToNanoCents(95))
		add(network(), "", now.Add(10*day), false, 0)
		add(network(), "token-marker-"+server.NewId().String(), now.Add(10*day), true, 0)
		add(network(), "token-ended-"+server.NewId().String(), now.Add(-day), false, 0)
		add(server.NewId(), "token-no-network-"+server.NewId().String(), now.Add(10*day), false, 0)

		var plans []*ActivePlayDataPlan
		server.Db(ctx, func(conn server.PgConn) {
			plans = GetActivePlayDataPlansInConn(conn, ctx, now, 100)
		})
		connect.AssertEqual(t, len(plans), 2)
		connect.AssertEqual(t, plans[0].NetworkId, laterNetworkId)
		connect.AssertEqual(t, plans[0].PurchaseToken, laterToken)
		connect.AssertEqual(t, plans[0].EndTime.Equal(now.Add(20*day)), true)
		connect.AssertEqual(t, plans[1].NetworkId, planNetworkId)
		connect.AssertEqual(t, plans[1].PurchaseToken, planToken)
	})
}
