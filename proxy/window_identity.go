package proxy

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// windowIdentityStore adapts the per-proxy redis persistence to
// `connect.MultiClientIdentityStore` for one hosted device
// (PROXYDRAIN1.md §3.5). Store/Load errors are swallowed (logged): identity
// persistence is a best-effort continuity optimization, and a redis outage
// must never break the device's window management.
type windowIdentityStore struct {
	ctx         context.Context
	proxyId     server.Id
	restoreGate *windowIdentityRestoreGate

	loadWindowIdentities  func(context.Context, server.Id) []*model.ProxyWindowClientIdentity
	storeWindowIdentities func(context.Context, server.Id, []*model.ProxyWindowClientIdentity)
	restoreHeldLogged     atomic.Bool
}

// A replacement accepts new public traffic before the old instance finishes
// draining. Identity restoration stays held across that overlap: lazy opens
// may form fresh windows, but they cannot run the old process's exact Connect
// client identities concurrently. Store mutations are buffered without a
// goroutine per device and flushed before the gate opens, so the replacement's
// fresh identities become the durable snapshot after drain completion.
type windowIdentityRestoreGate struct {
	stateLock   sync.Mutex
	releaseLock sync.Mutex
	released    bool
	pending     map[server.Id]pendingWindowIdentitySnapshot
}

// One proxy ID's latest buffered device generation.
type pendingWindowIdentitySnapshot struct {
	store      *windowIdentityStore
	identities []*connect.WindowClientIdentity
}

// Starts open for ordinary managers and held for deployment replacements.
func newWindowIdentityRestoreGate(held bool) *windowIdentityRestoreGate {
	return &windowIdentityRestoreGate{
		released: !held,
		pending:  map[server.Id]pendingWindowIdentitySnapshot{},
	}
}

// Owns a shallow copy; identity values contain immutable strings and value IDs.
func cloneWindowClientIdentities(identities []*connect.WindowClientIdentity) []*connect.WindowClientIdentity {
	cloned := make([]*connect.WindowClientIdentity, 0, len(identities))
	for _, identity := range identities {
		if identity == nil {
			cloned = append(cloned, nil)
			continue
		}
		clonedIdentity := *identity
		cloned = append(cloned, &clonedIdentity)
	}
	return cloned
}

// Reports whether a new device may consult the durable snapshot.
func (self *windowIdentityRestoreGate) restorationAllowed() bool {
	if self == nil {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.released
}

// Returns true when the snapshot was buffered behind the drain gate. Calls
// after release return false and the caller writes directly to persistence.
func (self *windowIdentityRestoreGate) buffer(
	store *windowIdentityStore,
	identities []*connect.WindowClientIdentity,
) bool {
	if self == nil {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.released {
		return false
	}
	self.pending[store.proxyId] = pendingWindowIdentitySnapshot{
		store:      store,
		identities: cloneWindowClientIdentities(identities),
	}
	return true
}

// Publishes every latest buffered snapshot before allowing restoration.
// Writes racing a flush are placed into the next batch, so none can remain
// stranded behind an already-open gate. External persistence runs lock-free.
func (self *windowIdentityRestoreGate) Release() {
	if self == nil {
		return
	}
	self.releaseLock.Lock()
	defer self.releaseLock.Unlock()
	for {
		pending := func() map[server.Id]pendingWindowIdentitySnapshot {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.released {
				return nil
			}
			if len(self.pending) == 0 {
				self.released = true
				return nil
			}
			pending := self.pending
			self.pending = map[server.Id]pendingWindowIdentitySnapshot{}
			return pending
		}()
		if pending == nil {
			return
		}
		for _, snapshot := range pending {
			if r := server.HandleError(func() {
				snapshot.store.storeNow(snapshot.identities)
			}); r != nil {
				glog.Infof("[pd][%s]window identity gate flush err=%v\n", snapshot.store.proxyId, r)
			}
		}
	}
}

// Adapts the production Redis functions and the process deployment gate.
func newWindowIdentityStore(
	ctx context.Context,
	proxyId server.Id,
	restoreGate *windowIdentityRestoreGate,
) *windowIdentityStore {
	return &windowIdentityStore{
		ctx:                   ctx,
		proxyId:               proxyId,
		restoreGate:           restoreGate,
		loadWindowIdentities:  model.GetProxyWindowIdentities,
		storeWindowIdentities: model.SetProxyWindowIdentities,
	}
}

func (self *windowIdentityStore) StoreWindowClientIdentities(identities []*connect.WindowClientIdentity) {
	if self.restoreGate.buffer(self, identities) {
		return
	}
	self.storeNow(identities)
}

// Converts and writes one snapshot after the deployment gate admits it.
func (self *windowIdentityStore) storeNow(identities []*connect.WindowClientIdentity) {
	modelIdentities := make([]*model.ProxyWindowClientIdentity, 0, len(identities))
	for _, identity := range identities {
		destinationIds := []server.Id{}
		for _, destinationId := range identity.Destination.Ids() {
			destinationIds = append(destinationIds, server.Id(destinationId))
		}
		modelIdentities = append(modelIdentities, &model.ProxyWindowClientIdentity{
			ClientId:       server.Id(identity.ClientId),
			ByJwt:          identity.ByJwt,
			InstanceId:     server.Id(identity.InstanceId),
			DestinationIds: destinationIds,
		})
	}
	if r := server.HandleError(func() {
		self.storeWindowIdentities(self.ctx, self.proxyId, modelIdentities)
	}); r != nil {
		glog.Infof("[pd][%s]window identity store err=%v\n", self.proxyId, r)
	}
}

func (self *windowIdentityStore) LoadWindowClientIdentities() []*connect.WindowClientIdentity {
	return self.LoadWindowClientIdentitiesContext(self.ctx)
}

// LoadWindowClientIdentitiesContext lets multi-client's optional restoration
// deadline cancel the Redis command itself. The legacy method above retains
// compatibility for callers that do not provide a narrower maintenance
// context.
func (self *windowIdentityStore) LoadWindowClientIdentitiesContext(ctx context.Context) []*connect.WindowClientIdentity {
	if !self.restoreGate.restorationAllowed() {
		if self.restoreHeldLogged.CompareAndSwap(false, true) {
			glog.Infof("[pd][%s]window identity restore held until drain completion\n", self.proxyId)
		}
		return nil
	}
	var modelIdentities []*model.ProxyWindowClientIdentity
	if r := server.HandleError(func() {
		modelIdentities = self.loadWindowIdentities(ctx, self.proxyId)
	}); r != nil {
		glog.Infof("[pd][%s]window identity load err=%v\n", self.proxyId, r)
		return nil
	}

	identities := make([]*connect.WindowClientIdentity, 0, len(modelIdentities))
	for _, modelIdentity := range modelIdentities {
		destinationIds := make([]connect.Id, 0, len(modelIdentity.DestinationIds))
		for _, destinationId := range modelIdentity.DestinationIds {
			destinationIds = append(destinationIds, connect.Id(destinationId))
		}
		destination, err := connect.NewMultiHopId(destinationIds...)
		if err != nil {
			continue
		}
		identities = append(identities, &connect.WindowClientIdentity{
			ClientId:    connect.Id(modelIdentity.ClientId),
			ByJwt:       modelIdentity.ByJwt,
			InstanceId:  connect.Id(modelIdentity.InstanceId),
			Destination: destination,
		})
	}
	if 0 < len(identities) {
		glog.Infof("[pd][%s]window identity restore: %d identities\n", self.proxyId, len(identities))
	}
	return identities
}
