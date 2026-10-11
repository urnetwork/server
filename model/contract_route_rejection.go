package model

// A deterministic contract-route refusal, a missing or inactive destination or
// a companion without its reverse origin, repeats for every identical request
// until the route changes. An old client retrying a stale route asks again on
// its own interval, and each request read PostgreSQL to reach the same answer.
// The controller records the refusal here and replays it for a short timeout.
// The origin notification owner bounds the entries, and an origin edge for a
// route's reverse pair, or a subscription acknowledgement that may have missed
// such an edge, removes them before the timeout. Only the controller's verdict
// is stored; successes and operational errors never are.
//
// A destination refusal cannot go stale: a client is never reactivated. A
// missing origin ends when an origin commits, and its edge removes the
// refusal. The replay trades bounded staleness for the reads it saves: an
// origin edge this owner never receives, or a destination that starts to
// advertise the relationship mode, refuses an identical request for at most
// the timeout. The client then asks again and reads PostgreSQL as before.

import (
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// Why a route was refused.
type ContractRouteRejectionCause uint8

const (
	// The destination is missing or inactive.
	ContractRouteDestinationInactive ContractRouteRejectionCause = iota + 1
	// A companion found no open or lingering origin in the reverse direction.
	ContractRouteMissingOrigin
)

// A refusal and the bounded metric labels the controller reported it with.
type ContractRouteRejection struct {
	Cause          ContractRouteRejectionCause
	ResolutionPath string
	Relationship   ProvideRelationshipDetails
}

type contractRouteRejectionKey struct {
	// The reverse pair: the origin a companion on this route would ride, and
	// the pair its origin edges are published under.
	originPair [32]byte
	companion  bool
}

type contractRouteRejectionEntry struct {
	rejection  ContractRouteRejection
	validUntil time.Time
	sequence   uint64
}

type contractRouteRejectionSlot struct {
	key      contractRouteRejectionKey
	sequence uint64
}

// Every entry has the same timeout, so insertion order is expiry order. A ring
// of slots evicts the oldest entry first when the bound is reached; a slot
// whose entry was replaced or removed is discarded when it reaches the head.
// Safe for concurrent use. No method performs I/O while holding the lock.
type contractRouteRejections struct {
	stateLock  sync.Mutex
	maxEntries int
	timeout    time.Duration
	closed     bool
	entries    map[contractRouteRejectionKey]contractRouteRejectionEntry
	slots      []contractRouteRejectionSlot
	head       int
	count      int
	sequence   uint64
}

// A zero bound or timeout disables the registry.
func newContractRouteRejections(maxEntries int, timeout time.Duration) *contractRouteRejections {
	return &contractRouteRejections{
		maxEntries: maxEntries,
		timeout:    timeout,
		entries:    map[contractRouteRejectionKey]contractRouteRejectionEntry{},
	}
}

func newContractRouteRejectionKey(sourceId server.Id, destinationId server.Id, companion bool) contractRouteRejectionKey {
	return contractRouteRejectionKey{
		originPair: contractOriginPairHash(destinationId, sourceId),
		companion:  companion,
	}
}

func (self *contractRouteRejections) get(key contractRouteRejectionKey) (ContractRouteRejection, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return ContractRouteRejection{}, false
	}
	entry, ok := self.entries[key]
	if !ok {
		return ContractRouteRejection{}, false
	}
	if !time.Now().Before(entry.validUntil) {
		// its slot is discarded when it reaches the head
		delete(self.entries, key)
		return ContractRouteRejection{}, false
	}
	return entry.rejection, true
}

// A closed fence means an origin edge for the pair arrived after the verdict's
// read began, so the verdict may already be stale and is not stored. Origin
// edges close the fence before they remove entries (see `notifyBucket`), and
// the fence is checked under the same lock as the insert, so a verdict is
// either refused here or removed by that edge. A nil fence never closes.
func (self *contractRouteRejections) put(key contractRouteRejectionKey, rejection ContractRouteRejection, fence <-chan struct{}) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed || self.maxEntries <= 0 || self.timeout <= 0 {
		return false
	}
	select {
	case <-fence:
		return false
	default:
	}
	// read the clock under the lock so slot order stays expiry order
	now := time.Now()
	self.expireWithLock(now)
	if self.slots == nil {
		self.slots = make([]contractRouteRejectionSlot, self.maxEntries)
	}
	if self.count == self.maxEntries {
		self.popWithLock()
	}
	self.sequence += 1
	self.entries[key] = contractRouteRejectionEntry{
		rejection:  rejection,
		validUntil: now.Add(self.timeout),
		sequence:   self.sequence,
	}
	self.slots[(self.head+self.count)%self.maxEntries] = contractRouteRejectionSlot{
		key:      key,
		sequence: self.sequence,
	}
	self.count += 1
	return true
}

func (self *contractRouteRejections) popWithLock() {
	slot := self.slots[self.head]
	if entry, ok := self.entries[slot.key]; ok && entry.sequence == slot.sequence {
		delete(self.entries, slot.key)
	}
	self.slots[self.head] = contractRouteRejectionSlot{}
	self.head = (self.head + 1) % self.maxEntries
	self.count -= 1
}

func (self *contractRouteRejections) expireWithLock(now time.Time) {
	for 0 < self.count {
		slot := self.slots[self.head]
		if entry, ok := self.entries[slot.key]; ok && entry.sequence == slot.sequence && now.Before(entry.validUntil) {
			return
		}
		self.popWithLock()
	}
}

// Removes both request shapes of a route whose reverse pair had an origin edge.
func (self *contractRouteRejections) invalidate(originPair [32]byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.entries, contractRouteRejectionKey{originPair: originPair, companion: false})
	delete(self.entries, contractRouteRejectionKey{originPair: originPair, companion: true})
}

// Removes every route in a bucket whose subscription may have missed edges.
func (self *contractRouteRejections) invalidateBucket(index int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for key := range self.entries {
		if int(key.originPair[0])%contractOriginBucketCount == index {
			delete(self.entries, key)
		}
	}
}

// After close no edge can remove an entry, so none is stored or replayed.
func (self *contractRouteRejections) close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	clear(self.entries)
	self.slots = nil
	self.head = 0
	self.count = 0
}

// Returns a refusal recorded for an identical request whose timeout has not
// passed and whose reverse pair has had no origin edge since. A nil owner has
// no registry.
func (self *ContractOriginNotifications) RouteRejection(
	sourceId server.Id,
	destinationId server.Id,
	companion bool,
) (ContractRouteRejection, bool) {
	if self == nil {
		return ContractRouteRejection{}, false
	}
	return self.routeRejections.get(newContractRouteRejectionKey(sourceId, destinationId, companion))
}

// Records a deterministic refusal for identical requests. A missing-origin
// verdict passes the update channel its watch armed immediately before the
// final origin read (see `waitForCompanionOrigin`); a destination verdict has
// no fence. Returns whether the refusal was stored.
func (self *ContractOriginNotifications) RecordRouteRejection(
	sourceId server.Id,
	destinationId server.Id,
	companion bool,
	rejection ContractRouteRejection,
	fence <-chan struct{},
) bool {
	if self == nil {
		return false
	}
	return self.routeRejections.put(newContractRouteRejectionKey(sourceId, destinationId, companion), rejection, fence)
}
