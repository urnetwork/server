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

// The prefix an ipv6 caller is matched by when its exact address is no
// extender's: the /64, as the camouflage report decides a relay
// (controller/extender_camouflage_report.go) and the activation budget keys an
// address. An extender relays on the family its client arrived on (A7), but
// within that family the host picks the source of its onward dial, and an ipv6
// host's outbound source is commonly a temporary address of its /64 (RFC
// 8981), not the one it activated from, so a relay from it went untagged and
// earned nothing (research extender-health-2026-11-11 §2.3). A v4 caller is
// matched by its exact address, as before.
const extenderAddressCacheIpv6PrefixBitCount = 64

// Addresses are keyed unmapped, so a v4 caller seen through a v4-mapped v6
// socket matches the v4 address row that was activated.
type extenderAddressCacheSnapshot struct {
	addrExtenderIds map[netip.Addr]server.Id
	// the /64 of each active ipv6 address, by the extender whose addresses it
	// holds. A /64 holding the addresses of more than one extender is not
	// here: a caller in it that is none of their exact addresses could be any
	// of them, so it is tagged by none (extenderAddressCacheIpv6Prefixes)
	ipv6PrefixExtenderIds map[netip.Prefix]server.Id
	loadTime              time.Time
}

// The /64 of each ipv6 address of the snapshot by the one extender whose
// addresses are in it, leaving out a /64 that holds the addresses of more than
// one.
func extenderAddressCacheIpv6Prefixes(addrExtenderIds map[netip.Addr]server.Id) map[netip.Prefix]server.Id {
	prefixExtenderIds := map[netip.Prefix]server.Id{}
	ambiguousPrefixes := map[netip.Prefix]bool{}
	for addr, extenderId := range addrExtenderIds {
		addr = addr.Unmap()
		if !addr.Is6() {
			continue
		}
		prefix := netip.PrefixFrom(addr.WithZone(""), extenderAddressCacheIpv6PrefixBitCount).Masked()
		if ambiguousPrefixes[prefix] {
			continue
		}
		if prefixExtenderId, ok := prefixExtenderIds[prefix]; ok && prefixExtenderId != extenderId {
			delete(prefixExtenderIds, prefix)
			ambiguousPrefixes[prefix] = true
			continue
		}
		prefixExtenderIds[prefix] = extenderId
	}
	return prefixExtenderIds
}

// The active extender a caller address arrived through: the extender whose
// active address it is, and else, for an ipv6 address, the one extender whose
// active addresses share its /64. An address is the one it names, unmapped and
// without a zone. A nil snapshot, the first load in flight or failing, finds
// none.
func (self *extenderAddressCacheSnapshot) extenderIdForAddress(ip netip.Addr) (server.Id, bool) {
	if self == nil {
		return server.Id{}, false
	}
	ip = ip.Unmap().WithZone("")
	if extenderId, found := self.addrExtenderIds[ip]; found {
		return extenderId, true
	}
	if !ip.Is6() {
		return server.Id{}, false
	}
	extenderId, found := self.ipv6PrefixExtenderIds[netip.PrefixFrom(ip, extenderAddressCacheIpv6PrefixBitCount).Masked()]
	return extenderId, found
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
		addrExtenderIds:       addrExtenderIds,
		ipv6PrefixExtenderIds: extenderAddressCacheIpv6Prefixes(addrExtenderIds),
		loadTime:              server.NowUtc(),
	})
	return true
}

var currentExtenderAddressCache = newExtenderAddressCacheState()

func init() {
	server.OnReset(func() {
		currentExtenderAddressCache.reset()
	})
}

// ActiveExtenderIdForAddress is the active extender ip arrived through: the one
// whose active address it is, or for an ipv6 address the one whose active
// addresses share its /64 (extenderAddressCacheIpv6PrefixBitCount). False when
// it belongs to no extender, which is the common case for a client dialing the
// platform directly.
func ActiveExtenderIdForAddress(ip netip.Addr) (server.Id, bool) {
	return activeExtenderAddressCacheSnapshot().extenderIdForAddress(ip)
}

// The cached address set, loading it on first use and starting one background
// refresh once it is stale. Nil only while the very first load is in flight or
// failing.
func activeExtenderAddressCacheSnapshot() *extenderAddressCacheSnapshot {
	state := currentExtenderAddressCache
	snapshot := state.snapshot.Load()
	if snapshot != nil && time.Since(snapshot.loadTime) < extenderAddressCacheStaleAfter {
		return snapshot
	}

	generation, started := state.startLoad()
	if !started {
		// another caller holds the load; serve whatever is published now,
		// which on a first use may already be that caller's result
		return state.snapshot.Load()
	}
	if snapshot != nil {
		// stale: refresh behind the caller, which keeps serving the snapshot
		go server.HandleError(func() {
			defer state.finishLoad(generation)
			loadExtenderAddressCache(state, generation)
		})
		return snapshot
	}

	// first use: pay the read here rather than leave the first connections of
	// a process untagged. A failure is contained, not fatal to the connect.
	server.HandleError(func() {
		defer state.finishLoad(generation)
		loadExtenderAddressCache(state, generation)
	})
	return state.snapshot.Load()
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
