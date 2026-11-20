package nginx

// The concentration of connect's connections by source address (LBALTOPT2
// R16): of the process's active connect connections, how many source
// addresses hold more than 10, 100 and 1,000, and how many connections those
// addresses hold. warpctl's split hashes the whole source address to one
// block of its host (warp warpctl/lbalt_split.go), so an address holding many
// connections lands wholly on one block. With every block's gauges side by
// side, a block whose surplus sits on a few addresses shows it.
//
// A connect connection is:
//   - an h1 upgrade to a connect name served in-process, from its hand-off
//     to connect's handler until the handler returns or, once hijacked,
//     until the connection closes, as its limit slot is held; and
//   - a QUIC connection admitted as connect's (Front.AcquireQuicConnection
//     with NameClassConnect), until its release.
//
// Each counts against its normalized source address (normalizeLimitAddr) on
// every interface of the process together, since the split keys on the
// address alone, and whatever the limits decided for the address: an address
// in exclude_subnets counts too.
//
// No address is ever a label. Each gauge has two labels from closed sets, the
// family and the bound, so 8 series per process. An address is kept only
// while it holds a connection, and the gauges move by the change each
// connection makes, so nothing walks the addresses.
//
// frontConcentration is safe for concurrent use.

import (
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// The bounds of the gauges' over label. An address counts at a bound while it
// holds more connections than the bound, so at 0 every address that holds
// one counts, with all of their connections.
var concentrationBounds = [...]int{0, 10, 100, 1000}

// The family label values, by concentrationFamilyIndex.
var concentrationFamilies = [...]string{"ipv4", "ipv6"}

// The index in concentrationFamilies of a normalized address.
func concentrationFamilyIndex(addr netip.Addr) int {
	if addr.Is4() {
		return 0
	}
	return 1
}

// What one address's count going from before to after adds at bound: to the
// addresses over the bound, and to the connections those addresses hold.
func concentrationBoundDelta(bound int, before int, after int) (addresses int, connections int) {
	if bound < before {
		addresses -= 1
		connections -= before
	}
	if bound < after {
		addresses += 1
		connections += after
	}
	return addresses, connections
}

// One family's gauges, by the index of their bound in concentrationBounds.
type concentrationSeries struct {
	addresses   [len(concentrationBounds)]prometheus.Gauge
	connections [len(concentrationBounds)]prometheus.Gauge
}

// The active connect connections of one front by source address, and the
// gauges they move. Safe for concurrent use.
type frontConcentration struct {
	stateLock sync.Mutex
	// the connections of each normalized address that holds one
	addrConnectionCounts map[netip.Addr]int
	// by concentrationFamilyIndex
	familySeries [len(concentrationFamilies)]concentrationSeries
}

// No connections, over metrics' gauges, every series resolved.
func newFrontConcentration(metrics *frontMetrics) *frontConcentration {
	concentration := &frontConcentration{
		addrConnectionCounts: map[netip.Addr]int{},
	}
	for familyIndex, family := range concentrationFamilies {
		series := &concentration.familySeries[familyIndex]
		for boundIndex, bound := range concentrationBounds {
			over := strconv.Itoa(bound)
			series.addresses[boundIndex] = metrics.connectAddresses.WithLabelValues(family, over)
			series.connections[boundIndex] = metrics.connectAddressConnections.WithLabelValues(family, over)
		}
	}
	return concentration
}

// Counts one connect connection from addr until the release it returns,
// which is safe to call more than once. An invalid address counts nowhere.
func (self *frontConcentration) acquire(addr netip.Addr) func() {
	addr = normalizeLimitAddr(addr)
	if !addr.IsValid() {
		return func() {}
	}
	self.add(addr, 1)
	var released atomic.Bool
	return func() {
		if released.CompareAndSwap(false, true) {
			self.add(addr, -1)
		}
	}
}

// Moves addr's count by delta, and the gauges by what that changes. The
// gauges move after the lock is let go: each change's moves add up to the
// same totals in any order.
func (self *frontConcentration) add(addr netip.Addr, delta int) {
	before, after := func() (int, int) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		before := self.addrConnectionCounts[addr]
		after := before + delta
		if after < 0 {
			panic("lbalt: concentration count became negative")
		}
		if after == 0 {
			delete(self.addrConnectionCounts, addr)
		} else {
			self.addrConnectionCounts[addr] = after
		}
		return before, after
	}()
	series := &self.familySeries[concentrationFamilyIndex(addr)]
	for boundIndex, bound := range concentrationBounds {
		addresses, connections := concentrationBoundDelta(bound, before, after)
		if addresses != 0 {
			series.addresses[boundIndex].Add(float64(addresses))
		}
		if connections != 0 {
			series.connections[boundIndex].Add(float64(connections))
		}
	}
}

// The number of addresses holding a connection.
func (self *frontConcentration) addrCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.addrConnectionCounts)
}
