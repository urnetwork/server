package model

import (
	"sync"
	"sync/atomic"

	"github.com/urnetwork/server"
)

// A process-local map keyed by client id, with a bound on its entries. Client
// ids churn (window clients are per-stream), so a full map is cleared
// wholesale rather than evicted entry by entry. Every caller treats a missing
// entry as unknown and asks the database, so a clear costs at most one probe
// per client.
type boundedClientMap[V any] struct {
	entries  sync.Map
	count    atomic.Int64
	maxCount int64
}

func newBoundedClientMap[V any](maxCount int64) *boundedClientMap[V] {
	return &boundedClientMap[V]{maxCount: maxCount}
}

// Returns the value stored for the client, if any.
func (self *boundedClientMap[V]) Load(clientId server.Id) (value V, ok bool) {
	if v, found := self.entries.Load(clientId); found {
		return v.(V), true
	}
	return
}

// Stores the value. A new client that would exceed the bound clears the map
// first; a store racing the clear may be lost, which reads as unknown.
func (self *boundedClientMap[V]) Store(clientId server.Id, value V) {
	if _, loaded := self.entries.Swap(clientId, value); loaded {
		return
	}
	if self.maxCount < self.count.Add(1) {
		self.entries.Clear()
		self.count.Store(1)
		self.entries.Store(clientId, value)
	}
}

// Forgets every entry.
func (self *boundedClientMap[V]) Clear() {
	self.entries.Clear()
	self.count.Store(0)
}

// The number of clients stored since the last clear.
func (self *boundedClientMap[V]) Len() int64 {
	return self.count.Load()
}
