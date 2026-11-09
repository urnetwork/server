package model

// NetworkTierOf (DESIGN.md 20.5, D-QUOTA-1): the highest in-window tier, on Pro's two
// cache tiers and its version order, under its own key. The database tests need the
// test database and redis (server.DefaultTestEnv), through .stack/server-test.sh.

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Adds a balance to networkId in its own transaction: Pro, or data only.
func testingAddTierBalance(t testing.TB, ctx context.Context, networkId server.Id, pro bool, startTime time.Time, endTime time.Time) {
	t.Helper()
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		if pro {
			err = AddProTransferBalanceInTx(tx, ctx, networkId, ByteCount(1024*1024*1024), startTime, endTime)
		} else {
			err = AddBasicTransferBalanceInTx(tx, ctx, networkId, ByteCount(1024*1024*1024), startTime, endTime)
		}
	})
	connect.AssertEqual(t, err, nil)
}

// The highest tier an in-window balance confers wins, by window and not by bytes: a
// free balance beside an in-window Pro one is pro, and a Pro balance not yet started
// or already ended confers nothing.
func TestNetworkTierOfPrecedence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		noBalance := server.NewId()
		connect.AssertEqual(t, NetworkTierOf(ctx, noBalance), NetworkTierFree)

		overlapping := server.NewId()
		testingAddTierBalance(t, ctx, overlapping, false, now.Add(-time.Hour), now.Add(30*24*time.Hour))
		testingAddTierBalance(t, ctx, overlapping, true, now.Add(-time.Hour), now.Add(time.Hour))
		connect.AssertEqual(t, NetworkTierOf(ctx, overlapping), NetworkTierPro)

		dataOnly := server.NewId()
		testingAddTierBalance(t, ctx, dataOnly, false, now.Add(-time.Hour), now.Add(30*24*time.Hour))
		connect.AssertEqual(t, NetworkTierOf(ctx, dataOnly), NetworkTierFree)

		notStarted := server.NewId()
		testingAddTierBalance(t, ctx, notStarted, false, now.Add(-time.Hour), now.Add(30*24*time.Hour))
		testingAddTierBalance(t, ctx, notStarted, true, now.Add(time.Hour), now.Add(30*24*time.Hour))
		connect.AssertEqual(t, NetworkTierOf(ctx, notStarted), NetworkTierFree)

		ended := server.NewId()
		testingAddTierBalance(t, ctx, ended, true, now.Add(-30*24*time.Hour), now.Add(-time.Second))
		connect.AssertEqual(t, NetworkTierOf(ctx, ended), NetworkTierFree)

		// IsProNetwork's readers agree with the tier
		for _, networkId := range []server.Id{noBalance, overlapping, dataOnly, notStarted, ended} {
			connect.AssertEqual(t, IsProNetwork(ctx, networkId), NetworkTierOf(ctx, networkId) != NetworkTierFree)
		}
	})
}

// Entitlement is time-based: a Pro balance with no bytes left is still pro until its
// window ends, since `active` is bytes left and never read.
func TestNetworkTierOfSpentProBalanceStaysPro(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		testingAddTierBalance(t, ctx, networkId, true, now.Add(-time.Hour), now.Add(time.Hour))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_balance SET balance_byte_count = 0 WHERE network_id = $1`,
				networkId,
			))
		})
		connect.AssertEqual(t, NetworkTierOfFresh(ctx, networkId), NetworkTierPro)
		connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierPro)
	})
}

// A read misses both tiers and loads free; before it writes the cache, an upgrade
// commits and its writer refreshes the cache. The read then writes what it loaded, and
// the refresh survives in both tiers of the tier's key.
func TestNetworkTierOfReadBeforeUpgradeDoesNotOverwriteRefresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		hold := testingHoldProNetworkLoad(networkId)
		defer hold.Release()
		readerTier := make(chan NetworkTier, 1)
		go func() {
			readerTier <- NetworkTierOf(ctx, networkId)
		}()
		hold.WaitLoaded(t)

		now := server.NowUtc()
		testingAddTierBalance(t, ctx, networkId, true, now, now.Add(30*24*time.Hour))
		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), true)

		hold.Release()
		select {
		case tier := <-readerTier:
			// the read answers with what it loaded...
			connect.AssertEqual(t, tier, NetworkTierFree)
		case <-time.After(30 * time.Second):
			t.Fatal("the held read did not finish")
		}

		// ...but the cache keeps the refresh
		localTier, localOk, cachedTier, cachedOk := Testing_NetworkTierCacheEntries(ctx, networkId)
		connect.AssertEqual(t, cachedOk, true)
		connect.AssertEqual(t, cachedTier, NetworkTierPro)
		connect.AssertEqual(t, localOk, true)
		connect.AssertEqual(t, localTier, NetworkTierPro)
		connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierPro)
	})
}

// A Pro balance that ends with no writer reads pro from the cache until both ttls run
// out, which bounds the staleness, and free afterwards.
func TestNetworkTierOfExpiringBalanceWithNoWriter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		testingAddTierBalance(t, ctx, networkId, true, now.Add(-time.Hour), now.Add(time.Hour))
		connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierPro)

		// the window ends and nothing refreshes the cache
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_balance SET end_time = $2 WHERE network_id = $1`,
				networkId,
				server.NowUtc().Add(-time.Second),
			))
		})
		connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierPro)

		// both ttls run out
		Testing_ExpireNetworkTierCache(ctx, networkId)
		connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierFree)
	})
}

// Pins the tier key's compare-and-set, as TestProNetworkCacheKeepsNewestVersion does for
// Pro's: an older version never replaces a newer one, the same or a newer one does,
// and an entry it cannot read is replaced.
func TestNetworkTierCacheKeepsNewestVersion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		set := func(tier NetworkTier, version int64) networkTierEntitlement {
			stored, ok := setNetworkTierCached(ctx, networkId, networkTierEntitlement{tier: tier, version: version})
			connect.AssertEqual(t, ok, true)
			return stored
		}

		connect.AssertEqual(t, set(NetworkTierPro, 200), networkTierEntitlement{tier: NetworkTierPro, version: 200})
		connect.AssertEqual(t, set(NetworkTierFree, 199), networkTierEntitlement{tier: NetworkTierPro, version: 200})
		connect.AssertEqual(t, set(NetworkTierFree, 99), networkTierEntitlement{tier: NetworkTierPro, version: 200})
		connect.AssertEqual(t, set(NetworkTierTeam, 200), networkTierEntitlement{tier: NetworkTierTeam, version: 200})
		connect.AssertEqual(t, set(NetworkTierFree, 1000), networkTierEntitlement{tier: NetworkTierFree, version: 1000})

		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, networkTierKey(networkId), "garbage", ProCacheTtl).Err())
		})
		connect.AssertEqual(t, set(NetworkTierPro, 5), networkTierEntitlement{tier: NetworkTierPro, version: 5})

		// a tier this build does not know is no entry
		_, ok := parseNetworkTierEntitlement("enterprise:7")
		connect.AssertEqual(t, ok, false)
	})
}

// A rolling deploy reads pro_versioned: from both releases. The tier changes nothing
// there: a refresh leaves the "<0|1>:<version>" value the old release's parse and
// script match, a value an old process wrote reads unharmed, and the tier rides its own
// key.
func TestNetworkTierLeavesProValueFormat(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		testingAddTierBalance(t, ctx, networkId, true, now.Add(-time.Hour), now.Add(time.Hour))

		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), true)

		oldFormat := regexp.MustCompile(`^[01]:([0-9]+)$`)
		var proValue string
		var tierValue string
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			proValue, err = r.Get(ctx, proNetworkKey(networkId)).Result()
			server.Raise(err)
			tierValue, err = r.Get(ctx, networkTierKey(networkId)).Result()
			server.Raise(err)
		})
		match := oldFormat.FindStringSubmatch(proValue)
		if match == nil || proValue[0] != '1' {
			t.Fatalf("pro_versioned holds %q, not the old release's format", proValue)
		}
		connect.AssertEqual(t, tierValue, "pro:"+match[1])

		// an old process writes its newer value; this release reads it, and the tier
		// key is not touched by it
		oldWritten := proEntitlement{pro: true, version: 1 << 60}
		stored, ok := setProNetworkCached(ctx, networkId, oldWritten)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, stored, oldWritten)
		cached, ok := getProNetworkCached(ctx, networkId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, cached, oldWritten)
		_, _, cachedTier, cachedOk := Testing_NetworkTierCacheEntries(ctx, networkId)
		connect.AssertEqual(t, cachedOk, true)
		connect.AssertEqual(t, cachedTier, NetworkTierPro)
	})
}

// A read inside a transaction loads through it and writes neither cache, since the
// transaction may hold a balance change a rollback would undo.
func TestNetworkTierOfInTxWritesNoCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		errRollBack := errors.New("roll back")
		var inTxTier NetworkTier
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(AddProTransferBalanceInTx(tx, ctx, networkId, ByteCount(1024), now.Add(-time.Hour), now.Add(time.Hour)))
				inTxTier = NetworkTierOfInTx(ctx, tx, networkId)
				server.Raise(errRollBack)
			}, server.OptNoRetry())
		})
		connect.AssertEqual(t, inTxTier, NetworkTierPro)
		_, localOk, _, cachedOk := Testing_NetworkTierCacheEntries(ctx, networkId)
		connect.AssertEqual(t, localOk, false)
		connect.AssertEqual(t, cachedOk, false)
		connect.AssertEqual(t, NetworkTierOf(ctx, networkId), NetworkTierFree)
	})
}
