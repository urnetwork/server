// The verified claims cache: the claims of the tokens whose signature this
// process verified, so a token presented again skips its decode and its
// signature check.
//
// JWT verification is about 15% of an api process (CPUOPT5 C #4): an es256
// verify and a claims decode on every authenticated request, every control
// replay among them, and a client presents one token for many requests.
//
// Only what is a fixed property of the token's bytes under the held keys is
// remembered: that its signature verified with a held key under an allowed
// signing method, and the claims it decodes to. Everything that depends on
// time, state or the request still runs on every request, after the lookup:
//   - in ParseByJwtForAudience, both gates, the session horizon, the
//     expiration, not-before and issued-at checks with their leeway, the
//     audience and identity claims, and the network fix (fixByJwt);
//   - in every caller, the state checks: the remembered refusals, the session
//     revocation marker, and the Redis revocation record or the SQL state
//     query.
//
// The rules of an entry:
//   - Key: SHA-256 of the whole token as presented, so two tokens that differ
//     in any byte, a signature byte among them, never share an entry.
//   - Successes only: a malformed token, a bad signature, a refused signing
//     method or an unknown kid is never remembered, so a retry verifies again
//     (and verifies once a reread gains its key).
//   - Lifetime: an entry answers until the token's expiration or for the
//     maximum age after it was stored, whichever is first, on the keyring's
//     clock. A token already past its expiration, or with none, is not
//     remembered. The verifier accepts a token until its expiration plus the
//     leeway, so an entry never outlives what the verifier accepts.
//   - Keys: an entry answers only under the removal generation it was stored
//     under. A key set that drops a key the held set verified with raises the
//     generation (`byJwtKeyring.setKeys`) and ends every entry at once. The
//     keyring only gains keys today, so a rotation ends none: the uncached
//     verifier keeps verifying a previous key's tokens until restart too, and
//     a restart (the response to a compromised key) starts empty.
//   - Bounds: a sharded lru. Storing past a shard's share of the entry bound
//     drops its least recently used entry.
//   - A verification's answer is a deep copy (`ByJwt.clone`), so no caller
//     changes what another reads.
//
// The unverified claim readers of the request gates (ByJwtLagGuestUnverified,
// ByJwtNamesClientUnverified, byJwtNetworkIdUnverified) read a remembered
// entry's claims instead of decoding the token: the same bytes through the
// same decoder give the same claims. A lookup that finds nothing decodes, as
// before, and never verifies.
//
// Safe for concurrent use.
package session

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type byJwtVerifiedCacheKey [sha256.Size]byte

type byJwtVerifiedCache struct {
	maxEntryAge time.Duration
	now         func() time.Time
	shards      []*byJwtVerifiedCacheShard

	hitCount   atomic.Uint64
	missCount  atomic.Uint64
	storeCount atomic.Uint64
	evictCount atomic.Uint64
}

// One independently locked part of the remembered verifications.
type byJwtVerifiedCacheShard struct {
	maxEntryCount int

	stateLock sync.Mutex
	// most recently used first; each value is a *byJwtVerifiedEntry
	entries    *list.List
	keyEntries map[byJwtVerifiedCacheKey]*list.Element
}

// One remembered verification.
type byJwtVerifiedEntry struct {
	key byJwtVerifiedCacheKey
	// never changed after it is stored; every answer is a copy
	byJwt             *ByJwt
	removalGeneration uint64
	// the token's expiration or the store time plus the maximum age, whichever
	// is first; the entry answers before it
	endTime time.Time
}

// The cache the settings describe, or nil when they remember nothing.
func newByJwtVerifiedCache(settings *byJwtKeyringSettings) *byJwtVerifiedCache {
	if settings.verifiedCacheMaxEntryCount <= 0 || settings.verifiedCacheMaxEntryAge <= 0 {
		return nil
	}
	shardCount := max(settings.verifiedCacheShardCount, 1)
	shardMaxEntryCount := max((settings.verifiedCacheMaxEntryCount+shardCount-1)/shardCount, 1)
	cache := &byJwtVerifiedCache{
		maxEntryAge: settings.verifiedCacheMaxEntryAge,
		now:         settings.now,
	}
	for range shardCount {
		cache.shards = append(cache.shards, &byJwtVerifiedCacheShard{
			maxEntryCount: shardMaxEntryCount,
			entries:       list.New(),
			keyEntries:    map[byJwtVerifiedCacheKey]*list.Element{},
		})
	}
	return cache
}

func newByJwtVerifiedCacheKey(jwtSigned string) byJwtVerifiedCacheKey {
	return sha256.Sum256([]byte(jwtSigned))
}

func (self *byJwtVerifiedCache) shard(key byJwtVerifiedCacheKey) *byJwtVerifiedCacheShard {
	return self.shards[binary.BigEndian.Uint64(key[:8])%uint64(len(self.shards))]
}

// The remembered claims of jwtSigned when they answer under removalGeneration
// now, counted as a hit or a miss. The claims are shared and never change: a
// caller copies them before it changes or returns them.
func (self *byJwtVerifiedCache) lookup(jwtSigned string, removalGeneration uint64) *ByJwt {
	if self == nil {
		return nil
	}
	byJwt := self.read(newByJwtVerifiedCacheKey(jwtSigned), removalGeneration)
	if byJwt == nil {
		self.missCount.Add(1)
	} else {
		self.hitCount.Add(1)
	}
	return byJwt
}

// lookup, uncounted, for the unverified claim readers.
func (self *byJwtVerifiedCache) peek(jwtSigned string, removalGeneration uint64) *ByJwt {
	if self == nil {
		return nil
	}
	return self.read(newByJwtVerifiedCacheKey(jwtSigned), removalGeneration)
}

func (self *byJwtVerifiedCache) read(key byJwtVerifiedCacheKey, removalGeneration uint64) *ByJwt {
	now := self.now()
	shard := self.shard(key)
	shard.stateLock.Lock()
	defer shard.stateLock.Unlock()
	element := shard.keyEntries[key]
	if element == nil {
		return nil
	}
	entry := element.Value.(*byJwtVerifiedEntry)
	if entry.removalGeneration != removalGeneration || !now.Before(entry.endTime) {
		shard.entries.Remove(element)
		delete(shard.keyEntries, key)
		return nil
	}
	shard.entries.MoveToFront(element)
	return entry.byJwt
}

// Remembers that jwtSigned verified under removalGeneration with the claims
// byJwt, which the cache copies. A token past its expiration, or with none, is
// not remembered.
func (self *byJwtVerifiedCache) store(jwtSigned string, byJwt *ByJwt, removalGeneration uint64) {
	if self == nil || byJwt.ExpiresAt == nil {
		return
	}
	now := self.now()
	endTime := now.Add(self.maxEntryAge)
	if expireTime := byJwt.ExpiresAt.Time; expireTime.Before(endTime) {
		endTime = expireTime
	}
	if !now.Before(endTime) {
		return
	}
	key := newByJwtVerifiedCacheKey(jwtSigned)
	entry := &byJwtVerifiedEntry{
		key:               key,
		byJwt:             byJwt.clone(),
		removalGeneration: removalGeneration,
		endTime:           endTime,
	}
	evictCount := 0
	func() {
		shard := self.shard(key)
		shard.stateLock.Lock()
		defer shard.stateLock.Unlock()
		if element := shard.keyEntries[key]; element != nil {
			element.Value = entry
			shard.entries.MoveToFront(element)
		} else {
			shard.keyEntries[key] = shard.entries.PushFront(entry)
		}
		for shard.maxEntryCount < shard.entries.Len() {
			oldest := shard.entries.Back()
			shard.entries.Remove(oldest)
			delete(shard.keyEntries, oldest.Value.(*byJwtVerifiedEntry).key)
			evictCount += 1
		}
	}()
	self.storeCount.Add(1)
	if 0 < evictCount {
		self.evictCount.Add(uint64(evictCount))
	}
}

// The remembered verifications, ended ones not yet dropped included.
func (self *byJwtVerifiedCache) entryCount() int {
	if self == nil {
		return 0
	}
	count := 0
	for _, shard := range self.shards {
		func() {
			shard.stateLock.Lock()
			defer shard.stateLock.Unlock()
			count += shard.entries.Len()
		}()
	}
	return count
}

// The process keyring's verified claims cache, as metrics.
type byJwtVerifiedCacheCollector struct {
	lookups   *prometheus.Desc
	stores    *prometheus.Desc
	evictions *prometheus.Desc
	entries   *prometheus.Desc
}

func newByJwtVerifiedCacheCollector() *byJwtVerifiedCacheCollector {
	return &byJwtVerifiedCacheCollector{
		lookups: prometheus.NewDesc(
			"urnetwork_auth_jwt_verified_cache_lookups_total",
			"Token verifications by whether a remembered verification of the same token answered (hit) or the token was verified in full (miss)",
			[]string{"result"},
			nil,
		),
		stores: prometheus.NewDesc(
			"urnetwork_auth_jwt_verified_cache_stores_total",
			"Token verifications remembered",
			nil,
			nil,
		),
		evictions: prometheus.NewDesc(
			"urnetwork_auth_jwt_verified_cache_evictions_total",
			"Remembered token verifications dropped to stay within the entry bound",
			nil,
			nil,
		),
		entries: prometheus.NewDesc(
			"urnetwork_auth_jwt_verified_cache_entries",
			"Remembered token verifications held now",
			nil,
			nil,
		),
	}
}

func (self *byJwtVerifiedCacheCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.lookups
	ch <- self.stores
	ch <- self.evictions
	ch <- self.entries
}

func (self *byJwtVerifiedCacheCollector) Collect(ch chan<- prometheus.Metric) {
	cache := defaultByJwtKeyring.verifiedCache
	hitCount, missCount, storeCount, evictCount := uint64(0), uint64(0), uint64(0), uint64(0)
	if cache != nil {
		hitCount = cache.hitCount.Load()
		missCount = cache.missCount.Load()
		storeCount = cache.storeCount.Load()
		evictCount = cache.evictCount.Load()
	}
	ch <- prometheus.MustNewConstMetric(self.lookups, prometheus.CounterValue, float64(hitCount), "hit")
	ch <- prometheus.MustNewConstMetric(self.lookups, prometheus.CounterValue, float64(missCount), "miss")
	ch <- prometheus.MustNewConstMetric(self.stores, prometheus.CounterValue, float64(storeCount))
	ch <- prometheus.MustNewConstMetric(self.evictions, prometheus.CounterValue, float64(evictCount))
	ch <- prometheus.MustNewConstMetric(self.entries, prometheus.GaugeValue, float64(cache.entryCount()))
}

func init() {
	prometheus.MustRegister(newByJwtVerifiedCacheCollector())
}
