// Decoded score pages are shared by every provider search in the process. A
// page is found by the sha256 of its bytes, which the reader has verified or
// computed for the bytes it just read, so a hit is the decode of exactly those
// bytes: a republished page is new bytes and a new decode.
package model

import (
	"container/list"
	"crypto/sha256"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
)

// The bound on the decoded pages one process retains, by their estimated heap.
const clientScorePageCacheMaxBytes = 64 * 1024 * 1024

// A decoded page retains about 5.3 to 5.8 times its gob bytes (scores with one
// to four lookbacks, measured), mostly in small maps. The estimate rounds up.
const clientScorePageDecodedBytesPerByte = 6

var clientScorePageCacheLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_client_score_page_cache_lookups_total",
	Help: "Decoded client score page cache lookups by result; each miss decodes its page",
}, []string{"result"})

var clientScorePageCacheEvictions = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_client_score_page_cache_evictions_total",
	Help: "Decoded client score pages evicted to keep the cache within its byte bound",
})

var clientScorePageCacheHits = clientScorePageCacheLookups.WithLabelValues("hit")
var clientScorePageCacheMisses = clientScorePageCacheLookups.WithLabelValues("miss")

type clientScorePageCacheItem struct {
	checksum [sha256.Size]byte
	sample   []*ClientScore
	size     int64
}

// A bounded LRU of decoded pages by checksum. Cached scores, their maps and
// their lookback scores are never modified: decodeClientScorePage gives each
// reader its own copy of every score.
type clientScorePageCache struct {
	lock     sync.Mutex
	maxBytes int64
	bytes    int64
	entries  map[[sha256.Size]byte]*list.Element
	recency  list.List
}

func newClientScorePageCache(maxBytes int64) *clientScorePageCache {
	return &clientScorePageCache{
		maxBytes: maxBytes,
		entries:  map[[sha256.Size]byte]*list.Element{},
	}
}

var clientScorePages = newClientScorePageCache(clientScorePageCacheMaxBytes)

func (self *clientScorePageCache) get(checksum [sha256.Size]byte) ([]*ClientScore, bool) {
	self.lock.Lock()
	defer self.lock.Unlock()
	element := self.entries[checksum]
	if element == nil {
		return nil, false
	}
	self.recency.MoveToFront(element)
	return element.Value.(*clientScorePageCacheItem).sample, true
}

// Retains a decoded page, evicting the least recently used past the bound. A
// page another reader decoded first is kept, and one larger than the bound is
// not retained.
func (self *clientScorePageCache) put(checksum [sha256.Size]byte, sample []*ClientScore, size int64) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if element := self.entries[checksum]; element != nil {
		self.recency.MoveToFront(element)
		return
	}
	if self.maxBytes < size {
		return
	}
	self.entries[checksum] = self.recency.PushFront(&clientScorePageCacheItem{checksum: checksum, sample: sample, size: size})
	self.bytes += size
	for self.maxBytes < self.bytes {
		self.remove(self.recency.Back())
		clientScorePageCacheEvictions.Inc()
	}
}

func (self *clientScorePageCache) remove(element *list.Element) {
	item := element.Value.(*clientScorePageCacheItem)
	delete(self.entries, item.checksum)
	self.recency.Remove(element)
	self.bytes -= item.size
}

func (self *clientScorePageCache) clear() {
	self.lock.Lock()
	defer self.lock.Unlock()
	clear(self.entries)
	self.recency.Init()
	self.bytes = 0
}

func (self *clientScorePageCache) size() (pages int, bytes int64) {
	self.lock.Lock()
	defer self.lock.Unlock()
	return len(self.entries), self.bytes
}

// The scores of a page whose bytes hash to checksum, decoded once per process
// and copied for this reader. A reader may set any field of its copies, but
// the maps and pointers are the cached page's: a reader that changes a map
// replaces it with a changed clone (filterPool).
func decodeClientScorePage(data []byte, checksum [sha256.Size]byte) ([]*ClientScore, error) {
	sample, ok := clientScorePages.get(checksum)
	if ok {
		clientScorePageCacheHits.Inc()
	} else {
		clientScorePageCacheMisses.Inc()
		var err error
		sample, err = decodeClientScoreSample(data)
		if err != nil {
			return nil, err
		}
		clientScorePages.put(checksum, sample, clientScorePageDecodedBytesPerByte*int64(len(data)))
	}
	copies := make([]ClientScore, len(sample))
	scores := make([]*ClientScore, len(sample))
	for i, score := range sample {
		copies[i] = *score
		scores[i] = &copies[i]
	}
	return scores, nil
}

func init() {
	prometheus.MustRegister(
		clientScorePageCacheLookups,
		clientScorePageCacheEvictions,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "urnetwork_client_score_page_cache_pages",
			Help: "Decoded client score pages the cache retains",
		}, func() float64 {
			pages, _ := clientScorePages.size()
			return float64(pages)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "urnetwork_client_score_page_cache_bytes",
			Help: "Estimated heap of the decoded client score pages the cache retains",
		}, func() float64 {
			_, bytes := clientScorePages.size()
			return float64(bytes)
		}),
	)
	server.OnReset(clientScorePages.clear)
}
