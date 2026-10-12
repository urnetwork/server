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
//
// A refusal whose lifecycle read found the destination row present and
// inactive is terminal, so it is kept apart in two bounded generations with a
// long timeout instead of the ring (see `contractTerminalRouteRejections`).
// Old sdks retry such a route every 5s for as long as they hold return
// traffic, often hours, and a load balancer spreads the retries over several
// api processes, so a process sees one route only every 25-40s. The ring's
// timeout bounds a missing origin's staleness and cannot hold a route that
// long. A missing row, a refusal from the write boundary's own read, and a
// missing origin stay in the ring.
//
// The api also answers a control request from these refusals before its
// client state query, for the client they were recorded for
// (controller/connect_control_refusal_replay.go).

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

// Whether the lifecycle read found the destination row present and inactive.
// No production path sets network_client.active back to true and client ids
// are never reused (session/state_refusal_cache.go relies on the same
// invariant), so the route stays refused. A missing row is not terminal here,
// and neither is a write-boundary refusal, whose labels carry the earlier
// read's active lifecycle.
func (self ContractRouteRejection) terminal() bool {
	if self.Cause != ContractRouteDestinationInactive {
		return false
	}
	switch self.Relationship.DestinationLifecycle {
	case NetworkClientLifecycleInactiveTop, NetworkClientLifecycleInactiveDerived:
		return true
	default:
		return false
	}
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

type contractTerminalRouteRejection struct {
	rejection  ContractRouteRejection
	validUntil time.Time
}

// Terminal refusals (see `ContractRouteRejection.terminal`), keyed like the
// ring. An origin edge cannot open such a route, so edges and acknowledgements
// leave these entries alone. Two bounded generations: a refusal enters the
// current generation; a full current generation becomes the previous one and
// the old previous one is dropped whole. A refusal served from the previous
// generation moves back to the current one with its original expiry, so a
// route an old client keeps retrying survives the turnover while one it
// stopped retrying ages out. The timeout only bounds the cost of a mistake.
// Safe for concurrent use. No method performs I/O while holding the lock.
type contractTerminalRouteRejections struct {
	stateLock             sync.Mutex
	generationCount       int
	timeout               time.Duration
	closed                bool
	currentKeyRejections  map[contractRouteRejectionKey]contractTerminalRouteRejection
	previousKeyRejections map[contractRouteRejectionKey]contractTerminalRouteRejection
}

// A zero generation bound or timeout disables the registry. At most twice the
// generation bound is held.
func newContractTerminalRouteRejections(generationCount int, timeout time.Duration) *contractTerminalRouteRejections {
	return &contractTerminalRouteRejections{
		generationCount:       generationCount,
		timeout:               timeout,
		currentKeyRejections:  map[contractRouteRejectionKey]contractTerminalRouteRejection{},
		previousKeyRejections: map[contractRouteRejectionKey]contractTerminalRouteRejection{},
	}
}

func (self *contractTerminalRouteRejections) get(key contractRouteRejectionKey) (ContractRouteRejection, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return ContractRouteRejection{}, false
	}
	now := time.Now()
	if entry, ok := self.currentKeyRejections[key]; ok {
		if now.Before(entry.validUntil) {
			return entry.rejection, true
		}
		delete(self.currentKeyRejections, key)
		return ContractRouteRejection{}, false
	}
	entry, ok := self.previousKeyRejections[key]
	if !ok {
		return ContractRouteRejection{}, false
	}
	delete(self.previousKeyRejections, key)
	if !now.Before(entry.validUntil) {
		return ContractRouteRejection{}, false
	}
	self.putWithLock(key, entry)
	return entry.rejection, true
}

func (self *contractTerminalRouteRejections) put(key contractRouteRejectionKey, rejection ContractRouteRejection) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed || self.generationCount <= 0 || self.timeout <= 0 {
		return false
	}
	delete(self.previousKeyRejections, key)
	self.putWithLock(key, contractTerminalRouteRejection{
		rejection:  rejection,
		validUntil: time.Now().Add(self.timeout),
	})
	return true
}

func (self *contractTerminalRouteRejections) putWithLock(key contractRouteRejectionKey, entry contractTerminalRouteRejection) {
	if _, ok := self.currentKeyRejections[key]; !ok && self.generationCount <= len(self.currentKeyRejections) {
		self.previousKeyRejections = self.currentKeyRejections
		self.currentKeyRejections = map[contractRouteRejectionKey]contractTerminalRouteRejection{}
	}
	self.currentKeyRejections[key] = entry
}

// A closed owner neither stores nor replays, as for the ring.
func (self *contractTerminalRouteRejections) close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	clear(self.currentKeyRejections)
	clear(self.previousKeyRejections)
}

// Returns a refusal recorded for an identical request: a terminal one whose
// timeout has not passed, or one from the ring whose timeout has not passed
// and whose reverse pair has had no origin edge since. A nil owner has no
// registry.
func (self *ContractOriginNotifications) RouteRejection(
	sourceId server.Id,
	destinationId server.Id,
	companion bool,
) (ContractRouteRejection, bool) {
	if self == nil {
		return ContractRouteRejection{}, false
	}
	key := newContractRouteRejectionKey(sourceId, destinationId, companion)
	if rejection, ok := self.routeRejections.get(key); ok {
		return rejection, true
	}
	return self.terminalRouteRejections.get(key)
}

// Records a deterministic refusal for identical requests. A missing-origin
// verdict passes the update channel its watch armed immediately before the
// final origin read (see `waitForCompanionOrigin`); a destination verdict has
// no fence. A terminal refusal goes to its own registry, everything else to
// the ring. Returns whether the refusal was stored.
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
	key := newContractRouteRejectionKey(sourceId, destinationId, companion)
	if rejection.terminal() {
		return self.terminalRouteRejections.put(key, rejection)
	}
	return self.routeRejections.put(key, rejection, fence)
}
