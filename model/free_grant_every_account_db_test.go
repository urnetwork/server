package model

// The daily free grant reaches every account without a Pro plan, whatever
// else it holds. The owner, 2026-11-11: "sign up codes should still get daily
// free data. all accounts should get the daily free data." pro.yml's free
// plan (free.data, daily) goes to every network without an active
// subscription; Pro's monthly grant replaces it. A redeemed balance code, a
// purchased data pack, a referral and a guest network from before guest
// sign-up was retired never take a network out of it. A lag.dev guest gets
// none (lag.dev/PLAN.md A1, lag_grant_test.go), nor does a prober shard's
// private network (prober_shard_free_grant_test.go). Needs the test database.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestDailyFreeGrantReachesEveryAccountWithoutAProPlan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		network := func(name string) server.Id {
			networkId := server.NewId()
			Testing_CreateNetwork(ctx, networkId, name+"-"+networkId.String()[:8], server.NewId())
			return networkId
		}

		plain := network("freeplain")

		// a balance code, redeemed: data only
		codeHolder := network("freecode")
		balanceCode, err := CreateBalanceCode(ctx, 1*Tib, 365*24*time.Hour, UsdToNanoCents(3), "", "", "")
		connect.AssertEqual(t, err, nil)
		redeemed, err := RedeemBalanceCode(&RedeemBalanceCodeArgs{Secret: balanceCode.Secret, NetworkId: codeHolder}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, redeemed.Error == nil, true)

		// a data pack bought to the network (Stripe, crypto, Gamma, x402): data
		// only, with its revenue
		dataPack := network("freedatapack")
		AddTransferBalance(ctx, &TransferBalance{
			NetworkId:             dataPack,
			StartTime:             now,
			EndTime:               now.Add(365 * 24 * time.Hour),
			StartBalanceByteCount: 1 * Tib,
			BalanceByteCount:      1 * Tib,
			NetRevenue:            UsdToNanoCents(3),
		})

		// a referral, on both sides
		referrer := network("freereferrer")
		referee := network("freereferee")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_referral (network_id, referral_network_id) VALUES ($1, $2)`,
				referee,
				referrer,
			))
		})

		// a guest network from before guest sign-up was retired
		legacyGuest := server.NewId()
		Testing_CreateLegacyGuestNetwork(ctx, legacyGuest, server.NewId())

		// Pro: the monthly Pro grant instead (pro.yml)
		pro := network("freepro")
		connect.AssertEqual(t, AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:          pro,
			SubscriptionType:   SubscriptionTypeSupporter,
			StartTime:          now.Add(-24 * time.Hour),
			EndTime:            now.Add(29 * 24 * time.Hour),
			NetRevenue:         UsdToNanoCents(5),
			SubscriptionMarket: SubscriptionMarketStripe,
			TransactionId:      "in_free_grant_" + pro.String(),
		}), nil)

		// a lag.dev guest
		lagGuest := CreateLagGuestNetwork(ctx, now, now.Add(24*time.Hour))

		startTime, endTime := FreeGrantWindow(now)
		added := AddFreeTransferBalanceToAllNetworks(ctx, startTime, endTime, 1*Gib)
		for _, granted := range []struct {
			name      string
			networkId server.Id
		}{
			{"a free network", plain},
			{"a network that redeemed a balance code", codeHolder},
			{"a network that bought a data pack", dataPack},
			{"a referrer", referrer},
			{"a referee", referee},
			{"a legacy guest network", legacyGuest},
		} {
			if added[granted.networkId] != 1*Gib {
				t.Errorf("%s: daily free grant %d, want %d", granted.name, added[granted.networkId], 1*Gib)
			}
			grants := 0
			for _, balance := range GetActiveTransferBalances(ctx, granted.networkId) {
				if balance.GrantKind == GrantKindFree {
					grants += 1
					connect.AssertEqual(t, balance.Pro, false)
				}
			}
			if grants != 1 {
				t.Errorf("%s: %d active daily free grants, want 1", granted.name, grants)
			}
		}
		for _, excluded := range []struct {
			name      string
			networkId server.Id
		}{
			{"a Pro network", pro},
			{"a lag.dev guest", lagGuest.NetworkId},
		} {
			if _, ok := added[excluded.networkId]; ok {
				t.Errorf("%s got the daily free grant", excluded.name)
			}
		}
	})
}
