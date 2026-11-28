package model

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
)

const subscriberNegativeTTL = time.Second
const subscriberNegativeCapacity = 65536

var providerSubscriberNegativeCache = newSubscriberNegativeCache(subscriberNegativeCapacity, time.Now)
var errSubscriberReadIncomplete = errors.New("subscriber eligibility read did not complete")

type subscriberNegativeEntry struct {
	id      server.Id
	expires time.Time
	index   int
}
type subscriberNegativeHeap []*subscriberNegativeEntry

func (h subscriberNegativeHeap) Len() int           { return len(h) }
func (h subscriberNegativeHeap) Less(i, j int) bool { return h[i].expires.Before(h[j].expires) }
func (h subscriberNegativeHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *subscriberNegativeHeap) Push(v any) {
	e := v.(*subscriberNegativeEntry)
	e.index = len(*h)
	*h = append(*h, e)
}
func (h *subscriberNegativeHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	e.index = -1
	return e
}

type subscriberNegativeFlight struct {
	done     chan struct{}
	excluded bool
	expires  time.Time
	err      error
}

// This cache stores refusals only. A positive concurrent result is never reused:
// its followers perform their own fresh read, so a new unknown connection can
// revoke Quality immediately. Capacity exhaustion only causes ordinary reads.
type subscriberNegativeCache struct {
	mu       sync.Mutex
	negative map[server.Id]*subscriberNegativeEntry
	expiry   subscriberNegativeHeap
	flights  map[server.Id]*subscriberNegativeFlight
	epoch    uint64
	capacity int
	now      func() time.Time
	observe  func(string, int)
}

func (c *subscriberNegativeCache) record(event string, count int) {
	if c.observe != nil && count > 0 {
		c.observe(event, count)
	}
}

func newSubscriberNegativeCache(capacity int, now func() time.Time) *subscriberNegativeCache {
	return &subscriberNegativeCache{
		negative: make(map[server.Id]*subscriberNegativeEntry),
		flights:  make(map[server.Id]*subscriberNegativeFlight),
		capacity: capacity,
		now:      now,
	}
}

func (c *subscriberNegativeCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.negative = make(map[server.Id]*subscriberNegativeEntry)
	c.expiry = nil
	c.flights = make(map[server.Id]*subscriberNegativeFlight)
}

// remember runs with mu held. observed is taken before the first fact query,
// not after query completion; a slow query cannot extend the one-second bound.
func (c *subscriberNegativeCache) remember(ids []server.Id, negative map[server.Id]bool, observed time.Time, epoch uint64) {
	expires := observed.Add(subscriberNegativeTTL)
	if epoch != c.epoch || c.capacity <= 0 || !c.now().Before(expires) {
		return
	}
	for _, id := range ids {
		if !negative[id] {
			continue
		}
		if e := c.negative[id]; e != nil {
			if e.expires.Before(expires) {
				e.expires = expires
				heap.Fix(&c.expiry, e.index)
			}
			continue
		}
		if len(c.negative) >= c.capacity {
			e := heap.Pop(&c.expiry).(*subscriberNegativeEntry)
			delete(c.negative, e.id)
		}
		e := &subscriberNegativeEntry{id: id, expires: expires}
		c.negative[id] = e
		heap.Push(&c.expiry, e)
	}
}

// The reader owns markObserved synchronously and calls it before dispatching
// its first fact query. Until then the conservative read-entry clock applies.
// Later chunks cannot renew the age of facts read by an earlier chunk.
type subscriberNegativeReader func(context.Context, []server.Id, func()) (map[server.Id]bool, error)

func (c *subscriberNegativeCache) lookup(ctx context.Context, ids []server.Id, read subscriberNegativeReader) (map[server.Id]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make(map[server.Id]bool)
	owned := make(map[server.Id]*subscriberNegativeFlight)
	waiting := make(map[server.Id]*subscriberNegativeFlight)
	load := make([]server.Id, 0, len(ids))
	seen := make(map[server.Id]bool, len(ids))
	hits, bypasses := 0, 0
	c.mu.Lock()
	epoch, now := c.epoch, c.now()
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if e := c.negative[id]; e != nil {
			if now.Before(e.expires) {
				result[id] = true
				hits++
				continue
			}
			heap.Remove(&c.expiry, e.index)
			delete(c.negative, id)
		}
		if f := c.flights[id]; f != nil {
			waiting[id] = f
			continue
		}
		load = append(load, id)
		if len(c.flights) < c.capacity {
			f := &subscriberNegativeFlight{done: make(chan struct{})}
			c.flights[id], owned[id] = f, f
		} else {
			bypasses++
		}
	}
	c.mu.Unlock()

	finish := func(negative map[server.Id]bool, observed time.Time, err error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err == nil {
			c.remember(load, negative, observed, epoch)
		}
		for id, f := range owned {
			f.excluded, f.expires, f.err = negative[id], observed.Add(subscriberNegativeTTL), err
			if c.flights[id] == f {
				delete(c.flights, id)
			}
			close(f.done)
		}
	}
	completed := false
	defer func() {
		// Database helpers may panic. Preserve that failure while releasing all
		// followers; neither partial rows nor a failed read enters the cache.
		if !completed {
			finish(nil, time.Time{}, errSubscriberReadIncomplete)
		}
	}()
	c.record("negative_hit", hits)
	c.record("negative_miss", len(load)+len(waiting))
	c.record("coalesced_wait", len(waiting))
	c.record("capacity_bypass", bypasses)
	readFacts := func(ids []server.Id) (map[server.Id]bool, time.Time, error) {
		observed := c.now()
		var firstDispatch sync.Once
		negative, err := read(ctx, ids, func() {
			firstDispatch.Do(func() { observed = c.now() })
		})
		return negative, observed, err
	}
	if len(load) > 0 {
		negative, observed, err := readFacts(load)
		finish(negative, observed, err)
		completed = true
		if err != nil {
			return nil, err
		}
		for _, id := range load {
			if negative[id] {
				result[id] = true
			}
		}
	} else {
		completed = true
	}

	fresh := make([]server.Id, 0, len(waiting))
	for id, f := range waiting {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.done:
		}
		if f.err != nil {
			return nil, f.err
		}
		c.mu.Lock()
		current := epoch == c.epoch
		c.mu.Unlock()
		if current && f.excluded && c.now().Before(f.expires) {
			result[id] = true
		} else {
			fresh = append(fresh, id)
		}
	}
	if len(fresh) > 0 {
		c.record("fresh_reread", len(fresh))
		negative, observed, err := readFacts(fresh)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.remember(fresh, negative, observed, epoch)
		c.mu.Unlock()
		for _, id := range fresh {
			if negative[id] {
				result[id] = true
			}
		}
	}
	return result, nil
}
