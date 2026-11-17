//go:build !sidecar_pending

package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A release leg until MG6's Gn-out: transfer_balance has no tier column and no Team exists, so the
// downgrade clock answers as it did before QD-946, from renewals. A lapsed supporter left pro when
// its newest renewal ended, though a pro grant outlasted it, and a network in a pro balance's window
// has left nothing.
func TestNetworkDowngradeTimeReadsRenewalsWithoutMg6(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if server.SidecarMg6Linked() {
			t.Fatal("a release build links a pending group")
		}
		day := 24 * time.Hour
		now := server.NowUtc()
		renewalEnd := now.Add(-30 * day)
		renew := func(networkId server.Id, start time.Time, end time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(AddSubscriptionRenewalInTx(tx, ctx, &SubscriptionRenewal{
					NetworkId:          networkId,
					SubscriptionType:   SubscriptionTypeSupporter,
					StartTime:          start,
					EndTime:            end,
					SubscriptionMarket: SubscriptionMarketStripe,
				}))
			})
		}
		addProGrant := func(networkId server.Id, start time.Time, end time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				if err := AddGrantTransferBalanceInTx(tx, ctx, networkId, GrantKindPro, 10*Tib, start, end); err != nil {
					t.Fatal(err)
				}
			})
			UpdateProNetwork(ctx, networkId)
		}

		lapsed := server.NewId()
		Testing_CreateNetwork(ctx, lapsed, "lapsed-"+lapsed.String(), server.NewId())
		renew(lapsed, renewalEnd.AddDate(0, -1, 0), renewalEnd)
		addProGrant(lapsed, renewalEnd.AddDate(0, -1, 0), now.Add(-2*day))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, lapsed), NetworkTierFree)
		downgradeTime, fromTier, ok := NetworkDowngradeTime(ctx, lapsed)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, fromTier, NetworkTierPro)
		connect.AssertEqual(t, downgradeTime, renewalEnd)

		current := server.NewId()
		Testing_CreateNetwork(ctx, current, "current-"+current.String(), server.NewId())
		renew(current, renewalEnd.AddDate(0, -1, 0), renewalEnd)
		addProGrant(current, now.Add(-day), now.Add(29*day))
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, current), NetworkTierPro)
		_, _, ok = NetworkDowngradeTime(ctx, current)
		connect.AssertEqual(t, ok, false)
	})
}
