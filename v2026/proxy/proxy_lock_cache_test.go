package proxy

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestProxyLockCacheBoundsUniqueIdentifiers(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cache := newProxyLockCache(proxyLockCacheMaxEntries)
	extra := 257
	evictions := 0

	for i := 0; i < proxyLockCacheMaxEntries+extra; i++ {
		result := cache.put(proxyLockCacheTestID(i), proxyLockEntry{
			found:  true,
			expiry: now.Add(proxyLockCacheTtl),
		}, now)
		evictions += result.evicted
		if result.size > proxyLockCacheMaxEntries {
			t.Fatalf("cache exceeded hard capacity after insert %d: size=%d capacity=%d", i, result.size, proxyLockCacheMaxEntries)
		}
	}

	if got := cache.size(); got != proxyLockCacheMaxEntries {
		t.Fatalf("cache size=%d, want hard capacity %d", got, proxyLockCacheMaxEntries)
	}
	if evictions != extra {
		t.Fatalf("evictions=%d, want %d", evictions, extra)
	}
	if result := cache.get(proxyLockCacheTestID(0), now); result.found {
		t.Fatal("oldest unique identifier survived bounded churn")
	}
	if result := cache.get(proxyLockCacheTestID(proxyLockCacheMaxEntries+extra-1), now); !result.found {
		t.Fatal("newest unique identifier was not retained")
	}
}

func TestProxyLockCacheEvictsLeastRecentlyUsed(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 1, 0, 0, time.UTC)
	cache := newProxyLockCache(2)
	entry := proxyLockEntry{found: true, expiry: now.Add(time.Minute)}

	cache.put(proxyLockCacheTestID(1), entry, now)
	cache.put(proxyLockCacheTestID(2), entry, now)
	if result := cache.get(proxyLockCacheTestID(1), now); !result.found {
		t.Fatal("hot entry unexpectedly missing")
	}
	result := cache.put(proxyLockCacheTestID(3), entry, now)

	if result.size != 2 || result.evicted != 1 {
		t.Fatalf("third insert result=%+v, want size=2 evicted=1", result)
	}
	if result := cache.get(proxyLockCacheTestID(2), now); result.found {
		t.Fatal("least-recently-used entry survived capacity eviction")
	}
	if result := cache.get(proxyLockCacheTestID(1), now); !result.found {
		t.Fatal("recently used entry was evicted")
	}
}

func TestProxyLockCacheExpiresAndRemovesEntry(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 2, 0, 0, time.UTC)
	cache := newProxyLockCache(4)
	id := proxyLockCacheTestID(1)
	cache.put(id, proxyLockEntry{found: true, expiry: now.Add(30 * time.Second)}, now)

	if result := cache.get(id, now.Add(29*time.Second)); !result.found || result.expired != 0 || result.size != 1 {
		t.Fatalf("fresh lookup result=%+v", result)
	}
	if result := cache.get(id, now.Add(30*time.Second)); result.found || result.expired != 1 || result.size != 0 {
		t.Fatalf("expiry lookup result=%+v, want miss, one expiry, and empty cache", result)
	}
	if got := cache.size(); got != 0 {
		t.Fatalf("expired entry remained in cache: size=%d", got)
	}
}

func TestProxyLockCacheSweepsColdExpiredEntries(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 2, 30, 0, time.UTC)
	cache := newProxyLockCache(4)
	entry := proxyLockEntry{found: true, expiry: now.Add(proxyLockCacheTtl)}
	cache.put(proxyLockCacheTestID(1), entry, now)
	cache.put(proxyLockCacheTestID(2), entry, now)

	result := cache.get(proxyLockCacheTestID(3), now.Add(proxyLockCacheTtl))
	if result.found || result.expired != 2 || result.size != 0 {
		t.Fatalf("unrelated lookup result=%+v, want both cold expired keys swept", result)
	}
	if got := cache.size(); got != 0 {
		t.Fatalf("cold expired entries remained in cache: size=%d", got)
	}
}

func TestProxyLockCacheCopiesPrefixesAndPreservesFreshWinner(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 3, 0, 0, time.UTC)
	cache := newProxyLockCache(4)
	id := proxyLockCacheTestID(1)
	originalPrefix := netip.MustParsePrefix("192.0.2.0/24")
	prefixes := []netip.Prefix{originalPrefix}
	first := proxyLockEntry{
		found:       true,
		lockSubnets: prefixes,
		expiry:      now.Add(20 * time.Second),
	}
	cache.put(id, first, now)
	prefixes[0] = netip.MustParsePrefix("2001:db8::/32")

	second := proxyLockEntry{
		found:       false,
		lockSubnets: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")},
		expiry:      now.Add(time.Minute),
	}
	result := cache.put(id, second, now.Add(time.Second))

	if !result.found || !result.entry.found {
		t.Fatalf("fresh first loader did not win: %+v", result)
	}
	if len(result.entry.lockSubnets) != 1 || result.entry.lockSubnets[0] != originalPrefix {
		t.Fatalf("cached prefixes aliased caller storage or were overwritten: %v", result.entry.lockSubnets)
	}
	if result.entry.expiry != first.expiry {
		t.Fatalf("fresh cache winner expiry=%s, want %s", result.entry.expiry, first.expiry)
	}
}

func proxyLockCacheTestID(index int) server.Id {
	var id server.Id
	binary.BigEndian.PutUint64(id[8:], uint64(index+1))
	return id
}
