package proxy

import (
	"container/list"
	"net/netip"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// proxyLockCacheMaxEntries bounds authenticated and formerly-authenticated
// proxy ids retained by one process. It is larger than the observed hot device
// set while keeping even an all-unique stale-token stream at a small fixed heap
// boundary.
const proxyLockCacheMaxEntries = 16_384

type proxyLockCacheItem struct {
	proxyID server.Id
	entry   proxyLockEntry
}

type proxyLockCacheResult struct {
	entry   proxyLockEntry
	found   bool
	size    int
	expired int
	evicted int
}

// proxyLockCache is a bounded TTL cache with LRU capacity eviction. Expired
// entries are removed on lookup, while capacity eviction protects against a
// stream of unique valid or formerly-valid credentials. The list retains only
// the same ids and entries already owned by the map; it never owns device
// configs or request objects.
type proxyLockCache struct {
	lock       sync.Mutex
	maxEntries int
	entries    map[server.Id]*list.Element
	recency    list.List
	nextSweep  time.Time
}

func newProxyLockCache(maxEntries int) *proxyLockCache {
	if maxEntries <= 0 {
		maxEntries = 1
	}
	return &proxyLockCache{
		maxEntries: maxEntries,
		entries:    map[server.Id]*list.Element{},
	}
}

func (self *proxyLockCache) get(proxyID server.Id, now time.Time) proxyLockCacheResult {
	self.lock.Lock()
	defer self.lock.Unlock()
	expired := self.sweepExpired(now)

	element := self.entries[proxyID]
	if element == nil {
		return proxyLockCacheResult{size: len(self.entries), expired: expired}
	}
	item := element.Value.(*proxyLockCacheItem)
	if !now.Before(item.entry.expiry) {
		self.remove(element)
		return proxyLockCacheResult{size: len(self.entries), expired: expired + 1}
	}
	self.recency.MoveToFront(element)
	return proxyLockCacheResult{
		entry:   item.entry,
		found:   true,
		size:    len(self.entries),
		expired: expired,
	}
}

// put preserves a fresh value installed by a concurrent loader. Both callers
// observed the same miss, but a slower database result must not overwrite the
// cache entry with an earlier expiry or older configuration snapshot.
func (self *proxyLockCache) put(
	proxyID server.Id,
	entry proxyLockEntry,
	now time.Time,
) proxyLockCacheResult {
	self.lock.Lock()
	defer self.lock.Unlock()

	expired := self.sweepExpired(now)
	if element := self.entries[proxyID]; element != nil {
		item := element.Value.(*proxyLockCacheItem)
		if now.Before(item.entry.expiry) {
			self.recency.MoveToFront(element)
			return proxyLockCacheResult{
				entry: item.entry,
				found: true,
				size:  len(self.entries),
			}
		}
		self.remove(element)
		expired = 1
	}

	// Copy the slice to retain exactly its live prefixes rather than a larger
	// model/config backing array. Prefix values themselves contain no pointers.
	entry.lockSubnets = append([]netip.Prefix(nil), entry.lockSubnets...)
	item := &proxyLockCacheItem{proxyID: proxyID, entry: entry}
	self.entries[proxyID] = self.recency.PushFront(item)

	evicted := 0
	for self.maxEntries < len(self.entries) {
		self.remove(self.recency.Back())
		evicted++
	}
	return proxyLockCacheResult{
		entry:   entry,
		found:   true,
		size:    len(self.entries),
		expired: expired,
		evicted: evicted,
	}
}

// sweepExpired removes cold stale keys too. Without this amortized scan, TTL
// would release only a key that happened to be requested again and the cache
// would remain at its high-water cardinality until LRU pressure. One bounded
// scan per TTL under continuing traffic keeps stale memory short-lived without
// a manager goroutine or a timer per entry.
func (self *proxyLockCache) sweepExpired(now time.Time) int {
	if now.Before(self.nextSweep) {
		return 0
	}
	self.nextSweep = now.Add(proxyLockCacheTtl)
	expired := 0
	for _, element := range self.entries {
		item := element.Value.(*proxyLockCacheItem)
		if !now.Before(item.entry.expiry) {
			self.remove(element)
			expired++
		}
	}
	return expired
}

func (self *proxyLockCache) remove(element *list.Element) {
	if element == nil {
		return
	}
	item := element.Value.(*proxyLockCacheItem)
	delete(self.entries, item.proxyID)
	self.recency.Remove(element)
}

func (self *proxyLockCache) size() int {
	self.lock.Lock()
	defer self.lock.Unlock()
	return len(self.entries)
}
