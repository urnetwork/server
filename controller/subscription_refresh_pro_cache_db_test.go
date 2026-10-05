package controller

// Database-backed checks that the refresh grant (AddRefreshTransferBalance: network
// creation, the initial-balance backfill, bringyourctl upgrade-plan) publishes the Pro
// entitlement only once it commits. The Pro cache refresh used to run inside the
// grant's transaction; it reads on its own connection, so it cached the entitlement
// from before the grant (not Pro) for up to ProCacheTtl, and a grant that rolled back
// still wrote the cache. Needs the test database and redis (server.DefaultTestEnv).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A network with an active supporter renewal, so its refresh grant is the
// monthly Pro grant.
func testingCreateSupporterNetwork(t testing.TB, ctx context.Context) server.Id {
	networkId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "refreshpro", server.NewId())

	now := server.NowUtc()
	err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          now.Add(-24 * time.Hour),
		EndTime:            now.Add(29 * 24 * time.Hour),
		NetRevenue:         model.UsdToNanoCents(5),
		SubscriptionMarket: model.SubscriptionMarketManual,
	})
	connect.AssertEqual(t, err, nil)
	return networkId
}

// Once the Pro refresh grant commits, both cache tiers hold Pro, so this process and
// every process reading the shared redis tier see the upgrade at once, not after
// ProCacheTtl.
func TestAddRefreshTransferBalanceProReadsTrueAfterCommit(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := testingCreateSupporterNetwork(t, ctx)

		// prime the cache with the "not pro" answer, the way a hot path would
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)

		connect.AssertEqual(t, AddRefreshTransferBalance(ctx, networkId), nil)

		transferBalances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(transferBalances), 1)
		connect.AssertEqual(t, transferBalances[0].Pro, true)

		localPro, localOk, cachedPro, cachedOk := model.Testing_ProNetworkCacheEntries(ctx, networkId)
		connect.AssertEqual(t, localOk, true)
		connect.AssertEqual(t, localPro, true)
		connect.AssertEqual(t, cachedOk, true)
		connect.AssertEqual(t, cachedPro, true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	})
}

// A Pro refresh grant whose transaction rolls back writes neither cache tier. The
// refresh belongs to whoever commits the transaction.
func TestAddRefreshTransferBalanceInTxRollbackCachesNothing(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := testingCreateSupporterNetwork(t, ctx)

		type rollback struct{}
		proGranted := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					if _, ok := r.(rollback); !ok {
						panic(r)
					}
				}
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				proGranted, err = AddRefreshTransferBalanceInTx(tx, ctx, networkId)
				connect.AssertEqual(t, err, nil)
				// a panic is the tx helper's rollback path
				panic(rollback{})
			})
		}()

		// it was the Pro grant, and it did not commit
		connect.AssertEqual(t, proGranted, true)
		connect.AssertEqual(t, len(model.GetActiveTransferBalances(ctx, networkId)), 0)

		_, localOk, _, cachedOk := model.Testing_ProNetworkCacheEntries(ctx, networkId)
		connect.AssertEqual(t, localOk, false)
		connect.AssertEqual(t, cachedOk, false)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
	})
}
