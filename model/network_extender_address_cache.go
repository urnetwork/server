package model

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
)

// The active extender addresses cached in process, so the connect path can tag
// a connection with the extender it arrived through (connect/EXTENDER.md J1).
//
// Address matching is the whole attribution today: an extender relays a client
// to the platform from its own address, so a caller address that is an active
// address of an active extender is that extender carrying that client. The set
// is one row per activated family and changes only on an activation, a
// revocation or a probe deactivation, so it is read once per refresh window
// rather than once per connection.
//
// The snapshot is immutable and swapped whole. First use loads it
// synchronously, because a missed tag is a permanently lost attribution on a
// row nothing revisits; after that a stale snapshot is served while a single
// background refresh runs, so no later connection waits on the directory. A
// read failure publishes nothing and the next connection retries.
//
// Generations follow the location directory: a reset cancels the previous
// generation, so a loader that started before a reset can neither publish into
// the replacement environment nor clear a newer loader's claim.

// How long a snapshot is served before a refresh is started. An extender that
// activates or is revoked is therefore attributed within a minute.
const extenderAddressCacheStaleAfter = 60 * time.Second

// Addresses are keyed unmapped, so a v4 caller seen through a v4-mapped v6
// socket matches the v4 address row that was activated.
type extenderAddressCacheSnapshot struct {
	addrExtenderIds map[netip.Addr]server.Id
	loadTime        time.Time
}

type extenderAddressCacheGeneration struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type extenderAddressCacheState struct {
	generation  atomic.Pointer[extenderAddressCacheGeneration]
	loading     atomic.Pointer[extenderAddressCacheGeneration]
	snapshot    atomic.Pointer[extenderAddressCacheSnapshot]
	publishLock sync.Mutex
}

func newExtenderAddressCacheState() *extenderAddressCacheState {
	state := &extenderAddressCacheState{}
	state.reset()
	return state
}

func (self *extenderAddressCacheState) reset() {
	generationCtx, generationCancel := context.WithCancel(context.Background())
	generation := &extenderAddressCacheGeneration{
		ctx:    generationCtx,
		cancel: generationCancel,
	}

	// Cancel the old generation before replacing it. Cancellation is
	// process-local and non-blocking; no external operation runs under this
	// lock.
	self.publishLock.Lock()
	defer self.publishLock.Unlock()

	if previous := self.generation.Load(); previous != nil {
		previous.cancel()
	}
	self.generation.Store(generation)
	self.snapshot.Store(nil)
	self.loading.Store(nil)
}

// Claims the single in-flight load, or reports that another caller holds it.
func (self *extenderAddressCacheState) startLoad() (*extenderAddressCacheGeneration, bool) {
	// Paired with reset's generation/loading swap. Only a missing or stale
	// snapshot reaches here, so the short lock is off the steady-state path.
	self.publishLock.Lock()
	defer self.publishLock.Unlock()

	generation := self.generation.Load()
	if generation == nil || generation.ctx.Err() != nil || !self.loading.CompareAndSwap(nil, generation) {
		return nil, false
	}
	return generation, true
}

func (self *extenderAddressCacheState) finishLoad(generation *extenderAddressCacheGeneration) {
	// An old generation must not clear a newer generation's claim.
	self.loading.CompareAndSwap(generation, nil)
}

func (self *extenderAddressCacheState) current(generation *extenderAddressCacheGeneration) bool {
	return generation != nil && generation.ctx.Err() == nil && self.generation.Load() == generation
}

func (self *extenderAddressCacheState) publish(
	generation *extenderAddressCacheGeneration,
	addrExtenderIds map[netip.Addr]server.Id,
) bool {
	self.publishLock.Lock()
	defer self.publishLock.Unlock()

	if !self.current(generation) {
		return false
	}
	self.snapshot.Store(&extenderAddressCacheSnapshot{
		addrExtenderIds: addrExtenderIds,
		loadTime:        server.NowUtc(),
	})
	return true
}

var currentExtenderAddressCache = newExtenderAddressCacheState()

func init() {
	server.OnReset(func() {
		currentExtenderAddressCache.reset()
	})
}

// ActiveExtenderIdForAddress is the active extender whose active address is ip,
// and false when the address belongs to no extender, which is the common case
// for a client dialing the platform directly.
func ActiveExtenderIdForAddress(ip netip.Addr) (server.Id, bool) {
	extenderId, found := activeExtenderAddrExtenderIds()[ip.Unmap()]
	return extenderId, found
}

// The cached address set, loading it on first use and starting one background
// refresh once it is stale. Nil only while the very first load is in flight or
// failing.
func activeExtenderAddrExtenderIds() map[netip.Addr]server.Id {
	state := currentExtenderAddressCache
	snapshot := state.snapshot.Load()
	if snapshot != nil && time.Since(snapshot.loadTime) < extenderAddressCacheStaleAfter {
		return snapshot.addrExtenderIds
	}

	generation, started := state.startLoad()
	if !started {
		// another caller holds the load; serve whatever is published now,
		// which on a first use may already be that caller's result
		if snapshot = state.snapshot.Load(); snapshot != nil {
			return snapshot.addrExtenderIds
		}
		return nil
	}
	if snapshot != nil {
		// stale: refresh behind the caller, which keeps serving the snapshot
		go server.HandleError(func() {
			defer state.finishLoad(generation)
			loadExtenderAddressCache(state, generation)
		})
		return snapshot.addrExtenderIds
	}

	// first use: pay the read here rather than leave the first connections of
	// a process untagged. A failure is contained, not fatal to the connect.
	server.HandleError(func() {
		defer state.finishLoad(generation)
		loadExtenderAddressCache(state, generation)
	})
	if snapshot = state.snapshot.Load(); snapshot != nil {
		return snapshot.addrExtenderIds
	}
	return nil
}

func loadExtenderAddressCache(
	state *extenderAddressCacheState,
	generation *extenderAddressCacheGeneration,
) {
	if !state.current(generation) {
		return
	}
	ctx := generation.ctx

	addrExtenderIds := map[netip.Addr]server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_extender_address.ip,
				network_extender_address.extender_id
			FROM network_extender_address
			INNER JOIN network_extender ON
				network_extender.extender_id = network_extender_address.extender_id AND
				network_extender.active
			WHERE network_extender_address.active
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var ip netip.Addr
				var extenderId server.Id
				server.Raise(result.Scan(&ip, &extenderId))
				addrExtenderIds[ip.Unmap()] = extenderId
			}
		})
	})
	state.publish(generation, addrExtenderIds)
}

// Testing_ResetExtenderAddressCache drops the cached addresses, so the next
// lookup reloads from the database.
func Testing_ResetExtenderAddressCache() {
	currentExtenderAddressCache.reset()
}

// Testing_RefreshExtenderAddressCache runs one refresh synchronously and
// publishes it, which is what a test asserting that the refresh picks up a
// directory change does instead of waiting out the staleness window.
func Testing_RefreshExtenderAddressCache() {
	state := currentExtenderAddressCache
	loadExtenderAddressCache(state, state.generation.Load())
}
