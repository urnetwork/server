package connect

// The registry retains only an existing announcement pointer. It performs no
// lookup, database read, capture or policy installation at startup. An explicit
// reviewed capture supplies at most256 durable connection keys at a time.
import (
	"sync"

	"github.com/urnetwork/server/v2026"
)

const arinShadowOwnerCapacity = 1 << 20

type arinShadowOwnerEntry struct {
	owner     *ConnectionAnnounce
	refs      int
	ambiguous bool
}

type arinShadowOwnerRegistry struct {
	mu       sync.Mutex
	entries  map[server.Id]*arinShadowOwnerEntry
	handlers map[server.Id]int
	capacity int
	overflow int
}

type arinShadowOwnerLease struct {
	registry  *arinShadowOwnerRegistry
	id        server.Id
	entry     *arinShadowOwnerEntry
	handlerId server.Id
	once      sync.Once
}

// Counts are local-process metadata, not fleet or public-provider coverage.
type ArinShadowOwnerRegistryStats struct {
	TrackedConnections  int `json:"tracked_connections"`
	OverflowConnections int `json:"overflow_connections"`
}

var currentArinShadowOwners = newArinShadowOwnerRegistry(arinShadowOwnerCapacity)

func newArinShadowOwnerRegistry(capacity int) *arinShadowOwnerRegistry {
	return &arinShadowOwnerRegistry{entries: make(map[server.Id]*arinShadowOwnerEntry), handlers: map[server.Id]int{}, capacity: capacity}
}

func (r *arinShadowOwnerRegistry) add(id server.Id, owner *ConnectionAnnounce) *arinShadowOwnerLease {
	r.mu.Lock()
	defer r.mu.Unlock()
	lease := &arinShadowOwnerLease{registry: r, id: id, handlerId: owner.handlerId}
	r.handlers[owner.handlerId]++
	if entry := r.entries[id]; entry != nil {
		// Even after one duplicate leaves, this generation remains ambiguous.
		// This avoids assigning an existing durable identity to a replacement.
		entry.refs++
		entry.ambiguous = true
		lease.entry = entry
	} else if id == (server.Id{}) || len(r.entries) >= r.capacity {
		r.overflow++
	} else {
		lease.entry = &arinShadowOwnerEntry{owner: owner, refs: 1}
		r.entries[id] = lease.entry
	}
	return lease
}

func (l *arinShadowOwnerLease) close() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		r := l.registry
		r.mu.Lock()
		defer r.mu.Unlock()
		r.handlers[l.handlerId]--
		if r.handlers[l.handlerId] == 0 {
			delete(r.handlers, l.handlerId)
		}
		if l.entry == nil {
			r.overflow--
			return
		}
		l.entry.refs--
		if l.entry.refs == 0 && r.entries[l.id] == l.entry {
			delete(r.entries, l.id)
		}
	})
}

func (l *arinShadowOwnerLease) current() bool {
	r := l.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	return l.entry != nil && r.entries[l.id] == l.entry && l.entry.refs == 1 && !l.entry.ambiguous
}

// No registry mutex is held while waiting for an announcement mutex. Writers
// may take stateLock then registry.mu; readers never reverse that lock order.
func (l *arinShadowOwnerLease) ArinShadowCurrentConnection() (server.ArinShadowOwnerSnapshot, bool) {
	if !l.current() {
		return server.ArinShadowOwnerSnapshot{}, false
	}
	owner := l.entry.owner
	owner.stateLock.Lock()
	valid := owner.ctx != nil && owner.ctx.Err() == nil && owner.connectionId != nil && *owner.connectionId == l.id && owner.shadowCaptureLease == l
	snapshot := server.ArinShadowOwnerSnapshot{ConnectionId: l.id, ClientId: owner.clientId, HandlerId: owner.handlerId}
	address, err := server.ParseClientAddress(owner.clientAddress)
	owner.stateLock.Unlock()
	if !valid || err != nil || !l.current() {
		return server.ArinShadowOwnerSnapshot{}, false
	}
	snapshot.Address = address.Addr().Unmap()
	snapshot.At = server.NowUtc()
	return snapshot, true
}

func (r *arinShadowOwnerRegistry) targets(ids []server.Id) ([]server.ArinShadowCaptureTarget, ArinShadowOwnerRegistryStats, error) {
	if len(ids) == 0 || len(ids) > server.ArinShadowCaptureBatchLimit {
		return nil, ArinShadowOwnerRegistryStats{}, server.ErrArinShadowInput
	}
	seen := make(map[server.Id]bool, len(ids))
	for _, id := range ids {
		if id == (server.Id{}) || seen[id] {
			return nil, ArinShadowOwnerRegistryStats{}, server.ErrArinShadowInput
		}
		seen[id] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	targets := make([]server.ArinShadowCaptureTarget, len(ids))
	for i, id := range ids {
		targets[i].ConnectionId = id
		if entry := r.entries[id]; entry != nil && entry.refs == 1 && !entry.ambiguous {
			// A lightweight view is bound to the exact lease generation below.
			targets[i].Owner = &arinShadowOwnerView{registry: r, id: id, entry: entry}
		}
	}
	return targets, ArinShadowOwnerRegistryStats{TrackedConnections: len(r.entries), OverflowConnections: r.overflow}, nil
}

type arinShadowOwnerView struct {
	registry *arinShadowOwnerRegistry
	id       server.Id
	entry    *arinShadowOwnerEntry
}

func (v *arinShadowOwnerView) ArinShadowCurrentConnection() (server.ArinShadowOwnerSnapshot, bool) {
	// The stored owner is immutable. Copy its lease under stateLock, then the
	// lease validates both map generation and current announcement identity.
	owner := v.entry.owner
	owner.stateLock.Lock()
	lease := owner.shadowCaptureLease
	owner.stateLock.Unlock()
	if lease == nil || lease.registry != v.registry || lease.id != v.id || lease.entry != v.entry {
		return server.ArinShadowOwnerSnapshot{}, false
	}
	return lease.ArinShadowCurrentConnection()
}

// CaptureArinShadowOwners is an explicit local-process point lookup. Missing
// owners remain input rows with no authority; callers must never drop them.
func CaptureArinShadowOwners(ids []server.Id) ([]server.ArinShadowCaptureTarget, ArinShadowOwnerRegistryStats, error) {
	return currentArinShadowOwners.targets(ids)
}

func (self *ConnectionAnnounce) releaseArinShadowOwner() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.shadowCaptureLease.close()
	self.shadowCaptureLease = nil
}
