// A native snapshot's manifest is named by its pointer: the slot, the
// generation and the sha256 of the manifest's bytes
// (parseClientScoreNativePointer). Each process keeps the manifests it has
// read and verified, by pointer key, pointer and manifest facets, and reads a
// slot's manifest again only for a pointer it has not verified. A new
// publication is a new pointer, with a new generation, so a cached manifest is
// exactly the one the slot's read would return while its pointer is current.
//
// Nothing else is taken on trust: every page is still read with its slot's
// generation and verified against the manifest's checksum
// (clientScoreCursor.readWithClient), so a slot reused, damaged or expired since
// its manifest was verified is unavailable at the page read, as it was at the
// manifest read before.
//
// Every provider search reads its pool's snapshots again, and each manifest
// read is one more sequential Redis round trip per snapshot and target: two per
// selector-mode pool, which reads the family and the capability snapshot. A
// hosted proxy's in-process searches run on CPU-saturated processes, where
// each round trip waits for the scheduler.
package model

import (
	"container/list"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
)

// The bound on the verified manifests one process retains, by their estimated
// heap.
const clientScoreNativeManifestCacheMaxBytes = 8 * 1024 * 1024

// A decoded manifest's page infos take about their gob bytes; this covers its
// keys, maps and list element.
const clientScoreNativeManifestCacheEntryBytes = 512

var clientScoreNativeManifestCacheLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_client_score_native_manifest_cache_lookups_total",
	Help: "Native client score manifest cache lookups by result; each miss reads and verifies the slot's manifest",
}, []string{"result"})

var clientScoreNativeManifestCacheHits = clientScoreNativeManifestCacheLookups.WithLabelValues("hit")
var clientScoreNativeManifestCacheMisses = clientScoreNativeManifestCacheLookups.WithLabelValues("miss")

type clientScoreNativeManifestCacheItem struct {
	key      string
	manifest *clientScoreNativeManifest
	size     int64
}

// A bounded LRU of verified manifests by clientScoreNativeManifestCacheKey. A
// cached manifest is shared by every reader and never modified. Safe for
// concurrent use.
type clientScoreNativeManifestCache struct {
	stateLock sync.Mutex
	maxBytes  int64
	bytes     int64
	entries   map[string]*list.Element
	recency   list.List
}

func newClientScoreNativeManifestCache(maxBytes int64) *clientScoreNativeManifestCache {
	return &clientScoreNativeManifestCache{
		maxBytes: maxBytes,
		entries:  map[string]*list.Element{},
	}
}

var clientScoreNativeManifests = newClientScoreNativeManifestCache(clientScoreNativeManifestCacheMaxBytes)

// The cache key of a pointer read under key whose manifest must list exactly
// manifestFacets. The pointer key names the target and the kind of snapshot,
// and the raw pointer the slot, generation and manifest checksum.
func clientScoreNativeManifestCacheKey(key string, rawPointer string, manifestFacets []ipFamilyFacet) string {
	var b strings.Builder
	b.WriteString(key)
	b.WriteByte('\n')
	b.WriteString(rawPointer)
	for _, facet := range manifestFacets {
		b.WriteByte('\n')
		b.WriteString(string(facet))
	}
	return b.String()
}

func (self *clientScoreNativeManifestCache) get(key string) (*clientScoreNativeManifest, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	element := self.entries[key]
	if element == nil {
		return nil, false
	}
	self.recency.MoveToFront(element)
	return element.Value.(*clientScoreNativeManifestCacheItem).manifest, true
}

// Retains a verified manifest, evicting the least recently used past the
// bound. A manifest another reader verified first is kept, and one larger
// than the bound is not retained.
func (self *clientScoreNativeManifestCache) put(key string, manifest *clientScoreNativeManifest, size int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if element := self.entries[key]; element != nil {
		self.recency.MoveToFront(element)
		return
	}
	if self.maxBytes < size {
		return
	}
	self.entries[key] = self.recency.PushFront(&clientScoreNativeManifestCacheItem{key: key, manifest: manifest, size: size})
	self.bytes += size
	for self.maxBytes < self.bytes {
		self.remove(self.recency.Back())
	}
}

func (self *clientScoreNativeManifestCache) remove(element *list.Element) {
	item := element.Value.(*clientScoreNativeManifestCacheItem)
	delete(self.entries, item.key)
	self.recency.Remove(element)
	self.bytes -= item.size
}

func (self *clientScoreNativeManifestCache) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clear(self.entries)
	self.recency.Init()
	self.bytes = 0
}

func (self *clientScoreNativeManifestCache) size() (manifests int, bytes int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.entries), self.bytes
}

func init() {
	prometheus.MustRegister(clientScoreNativeManifestCacheLookups)
	server.OnReset(clientScoreNativeManifests.clear)
}
