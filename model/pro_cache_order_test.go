package model

// Checks that Pro cache writes are ordered by version: a read or refresh that loaded the
// entitlement before a commit cannot write it over the refresh made after the commit,
// in redis or in this process's local tier. The database tests need the test database
// and redis (server.DefaultTestEnv).

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// proLoadHold parks the first refresh of one network between its load and its cache
// write (testingProNetworkLoaded), so a test can land a commit and a refresh while it
// waits. Refreshes of other networks, and later refreshes of this one, pass through.
type proLoadHold struct {
	loaded      chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func testingHoldProNetworkLoad(networkId server.Id) *proLoadHold {
	hold := &proLoadHold{
		loaded:  make(chan struct{}),
		release: make(chan struct{}),
	}
	var held atomic.Bool
	hook := func(loadedNetworkId server.Id) {
		if loadedNetworkId == networkId && held.CompareAndSwap(false, true) {
			close(hold.loaded)
			<-hold.release
		}
	}
	testingProNetworkLoaded.Store(&hook)
	return hold
}

// WaitLoaded returns once the held refresh has loaded.
func (self *proLoadHold) WaitLoaded(t testing.TB) {
	t.Helper()
	select {
	case <-self.loaded:
	case <-time.After(30 * time.Second):
		t.Fatal("the held refresh did not load")
	}
}

// Release lets the held refresh write the cache, and stops holding.
func (self *proLoadHold) Release() {
	self.releaseOnce.Do(func() {
		testingProNetworkLoaded.Store(nil)
		close(self.release)
	})
}

func testingReceiveProResult(t testing.TB, results chan bool) bool {
	t.Helper()
	select {
	case pro := <-results:
		return pro
	case <-time.After(30 * time.Second):
		t.Fatal("the held refresh did not finish")
		return false
	}
}

// testingPoisonProNetworkCache plants an entitlement in both tiers as if it had been
// cached just before the network's last change: older than any load since, and written
// over whatever the tiers hold.
func testingPoisonProNetworkCache(ctx context.Context, networkId server.Id, pro bool) {
	entitlement := proEntitlement{pro: pro, version: 1}
	func() {
		proLocalCacheMutex.Lock()
		defer proLocalCacheMutex.Unlock()
		proLocalCache[networkId] = proLocalEntry{
			entitlement: entitlement,
			expiry:      server.NowUtc().Add(ProLocalCacheTtl),
		}
	}()
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, proNetworkKey(networkId), formatProEntitlement(entitlement), ProCacheTtl).Err())
	})
}

// TestIsProNetworkReadBeforeUpgradeDoesNotOverwriteRefresh: a hot-path read misses both
// tiers and loads "not Pro"; before it writes the cache, the upgrade commits and its
// writer refreshes the cache. The read then writes what it loaded. The refresh survives
// in both tiers.
func TestIsProNetworkReadBeforeUpgradeDoesNotOverwriteRefresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		hold := testingHoldProNetworkLoad(networkId)
		defer hold.Release()
		readerPro := make(chan bool, 1)
		go func() {
			readerPro <- IsProNetwork(ctx, networkId)
		}()
		hold.WaitLoaded(t)

		now := server.NowUtc()
		var err error
		server.Tx(ctx, func(tx server.PgTx) {
			err = AddProTransferBalanceInTx(
				tx, ctx, networkId, ByteCount(1024*1024*1024), now, now.Add(30*24*time.Hour),
			)
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), true)

		hold.Release()
		// the read answers with what it loaded...
		connect.AssertEqual(t, testingReceiveProResult(t, readerPro), false)

		// ...but the cache keeps the refresh
		localPro, localOk, cachedPro, cachedOk := Testing_ProNetworkCacheEntries(ctx, networkId)
		connect.AssertEqual(t, cachedOk, true)
		connect.AssertEqual(t, cachedPro, true)
		connect.AssertEqual(t, localOk, true)
		connect.AssertEqual(t, localPro, true)
		connect.AssertEqual(t, IsProNetwork(ctx, networkId), true)
	})
}

// TestProNetworkRefreshBeforeRevocationDoesNotOverwriteRefresh: the other direction,
// between two refreshes. A fresh read (as at token issue) loads Pro; before it writes
// the cache, the Pro balance is ended and that writer refreshes the cache. "Not Pro"
// survives in both tiers.
func TestProNetworkRefreshBeforeRevocationDoesNotOverwriteRefresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		now := server.NowUtc()
		var err error
		server.Tx(ctx, func(tx server.PgTx) {
			err = AddProTransferBalanceInTx(
				tx, ctx, networkId, ByteCount(1024*1024*1024), now.Add(-time.Hour), now.Add(30*24*time.Hour),
			)
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), true)

		hold := testingHoldProNetworkLoad(networkId)
		defer hold.Release()
		freshPro := make(chan bool, 1)
		go func() {
			freshPro <- IsProNetworkFresh(ctx, networkId)
		}()
		hold.WaitLoaded(t)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_balance SET end_time = $2 WHERE network_id = $1 AND pro = true`,
				networkId,
				server.NowUtc().Add(-time.Second),
			))
		})
		connect.AssertEqual(t, UpdateProNetwork(ctx, networkId), false)

		hold.Release()
		// the fresh read answers with what it loaded...
		connect.AssertEqual(t, testingReceiveProResult(t, freshPro), true)

		// ...but the cache keeps the later refresh
		localPro, localOk, cachedPro, cachedOk := Testing_ProNetworkCacheEntries(ctx, networkId)
		connect.AssertEqual(t, cachedOk, true)
		connect.AssertEqual(t, cachedPro, false)
		connect.AssertEqual(t, localOk, true)
		connect.AssertEqual(t, localPro, false)
		connect.AssertEqual(t, IsProNetwork(ctx, networkId), false)
	})
}

// TestProNetworkCacheKeepsNewestVersion pins the redis compare-and-set: an older version
// never replaces a newer one, the same or a newer version does, an entry it cannot read
// is replaced, and it reports the entry the key holds afterwards.
func TestProNetworkCacheKeepsNewestVersion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		set := func(pro bool, version int64) proEntitlement {
			stored, ok := setProNetworkCached(ctx, networkId, proEntitlement{pro: pro, version: version})
			connect.AssertEqual(t, ok, true)
			return stored
		}

		connect.AssertEqual(t, set(true, 200), proEntitlement{pro: true, version: 200})
		// older, including fewer digits: refused, and the newer entry comes back
		connect.AssertEqual(t, set(false, 199), proEntitlement{pro: true, version: 200})
		connect.AssertEqual(t, set(false, 99), proEntitlement{pro: true, version: 200})
		// the same version replaces
		connect.AssertEqual(t, set(false, 200), proEntitlement{pro: false, version: 200})
		// newer, including more digits, replaces
		connect.AssertEqual(t, set(true, 1000), proEntitlement{pro: true, version: 1000})

		cached, ok := getProNetworkCached(ctx, networkId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, cached, proEntitlement{pro: true, version: 1000})

		var ttl time.Duration
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			ttl, err = r.PTTL(ctx, proNetworkKey(networkId)).Result()
			server.Raise(err)
		})
		connect.AssertEqual(t, 0 < ttl && ttl <= ProCacheTtl, true)

		// an entry in another format reads as a miss and is replaced
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, proNetworkKey(networkId), "1", ProCacheTtl).Err())
		})
		_, ok = getProNetworkCached(ctx, networkId)
		connect.AssertEqual(t, ok, false)
		connect.AssertEqual(t, set(false, 5), proEntitlement{pro: false, version: 5})
	})
}

// TestProLocalCacheKeepsNewestVersion pins the same rule in the local tier: an older
// version does not replace a live newer entry, the same or a newer version does, and
// once the entry has expired any version does.
func TestProLocalCacheKeepsNewestVersion(t *testing.T) {
	networkId := server.NewId()

	get := func() proEntitlement {
		entitlement, ok := getProNetworkLocal(networkId)
		connect.AssertEqual(t, ok, true)
		return entitlement
	}

	setProNetworkLocal(networkId, proEntitlement{pro: true, version: 200})
	setProNetworkLocal(networkId, proEntitlement{pro: false, version: 199})
	connect.AssertEqual(t, get(), proEntitlement{pro: true, version: 200})

	setProNetworkLocal(networkId, proEntitlement{pro: false, version: 200})
	connect.AssertEqual(t, get(), proEntitlement{pro: false, version: 200})

	setProNetworkLocal(networkId, proEntitlement{pro: true, version: 201})
	connect.AssertEqual(t, get(), proEntitlement{pro: true, version: 201})

	func() {
		proLocalCacheMutex.Lock()
		defer proLocalCacheMutex.Unlock()
		entry := proLocalCache[networkId]
		entry.expiry = server.NowUtc()
		proLocalCache[networkId] = entry
	}()
	setProNetworkLocal(networkId, proEntitlement{pro: false, version: 100})
	connect.AssertEqual(t, get(), proEntitlement{pro: false, version: 100})
}
