// The placement policy: the fleet from the load table and the inventory, and
// the host and addresses one arrival is placed on.
//
// Everything here is pure. Every input is an argument, nothing reads a clock,
// Redis or the vault, and randomness comes from an injected source, so a table
// test with a seeded source reproduces every choice. Hosts are kept in sorted
// order, so a draw over them is reproducible.
//
// The fleet reads the load table the way the mover does (connect/MOVE.md
// §5.4, D3): a host's residents are those of its fresh, non-draining records,
// its capacity is its cores times its inventory `move_weight`, and the fleet
// mean is all residents over the capacity of the hosts that are not
// evacuating. An evacuating host keeps its residents in the mean but takes no
// arrivals.
//
// An arrival goes to a host with headroom below the band's upper edge, mean ×
// (1 + tolerance), with probability proportional to that headroom in
// residents. A fleet within the band places in proportion to capacity, and a
// host far above the band gets nothing until the others fill. Hosts at or
// above the CPU ceiling get no arrivals while a host with headroom is below
// it; with none below it, headroom alone decides, and with no headroom
// anywhere, capacity does.
package edgeplacement

import (
	"math"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A source of randomness, safe for concurrent use. math/rand/v2's top-level
// functions satisfy it.
type placementRandom interface {
	// in [0, 1)
	Float64() float64
	// in [0, n)
	IntN(n int) int
}

// One public interface of a host, as placement dials it: a valid address of at
// least one family and a positive DNS weight.
type placementInterface struct {
	ipv4      netip.Addr
	ipv6      netip.Addr
	dnsWeight int
}

// One host that can take arrivals.
type placementHost struct {
	host      string
	residents int
	// cores times `move_weight`
	capacity float64
	// the highest busy fraction over the host's records, or
	// model.ConnectMoveLoadCpuUnknown when none measured it
	cpuBusy float64
	// in interface name order
	interfaces []placementInterface
}

// The fleet as one read of the load table saw it.
type placementFleet struct {
	// the hosts that can take arrivals, sorted by host
	hosts []*placementHost
	// all residents over the capacity of the hosts that are not evacuating;
	// zero without capacity
	mean float64
	// mean × (1 + tolerance)
	bandEdge float64
	// every placeable address and its host, to name a failed dial's host
	addressHosts map[netip.Addr]string
}

// The capacity multiplier of a host: its inventory `move_weight`, or 1 when it
// sets none that is positive and finite.
func placementHostWeight(inventory *server.ServicesLbInventory, host string) float64 {
	if weight, ok := inventory.MoveWeights[host]; ok && 0 < weight && !math.IsInf(weight, 1) {
		return weight
	}
	return 1
}

// The placeable interfaces of an inventory host, in interface name order.
func placementInterfaces(interfaces []server.ServicesLbInterface) []placementInterface {
	sorted := slices.Clone(interfaces)
	slices.SortFunc(sorted, func(a server.ServicesLbInterface, b server.ServicesLbInterface) int {
		return strings.Compare(a.Interface, b.Interface)
	})
	placeable := []placementInterface{}
	for _, inventoryInterface := range sorted {
		if inventoryInterface.DnsWeight <= 0 {
			continue
		}
		candidate := placementInterface{
			dnsWeight: inventoryInterface.DnsWeight,
		}
		if ipv4, err := netip.ParseAddr(strings.TrimSpace(inventoryInterface.Ipv4)); err == nil && ipv4.Unmap().Is4() {
			candidate.ipv4 = ipv4.Unmap()
		}
		if ipv6, err := netip.ParseAddr(strings.TrimSpace(inventoryInterface.Ipv6)); err == nil && ipv6.Is6() && !ipv6.Is4In6() {
			candidate.ipv6 = ipv6
		}
		if candidate.ipv4.IsValid() || candidate.ipv6.IsValid() {
			placeable = append(placeable, candidate)
		}
	}
	return placeable
}

// The fleet from the table as read at nodeTime: records within staleTimeout of
// nodeTime that are not draining, grouped by host and joined with the
// inventory. A host without capacity (no cores reported), outside the
// inventory, evacuating, or without a placeable interface takes no arrivals.
// Non-positive staleTimeout leaves every record out.
func computePlacementFleet(
	loads []*model.ConnectMoveLoad,
	nodeTime time.Time,
	staleTimeout time.Duration,
	inventory *server.ServicesLbInventory,
	evacuateHosts map[string]bool,
	tolerance float64,
) *placementFleet {
	fleet := &placementFleet{
		addressHosts: map[netip.Addr]string{},
	}
	hostResidents := map[string]int{}
	hostCores := map[string]int{}
	hostCpuBusy := map[string]float64{}
	for _, load := range loads {
		if load.Draining || staleTimeout <= 0 || staleTimeout < nodeTime.Sub(load.Time) {
			continue
		}
		if _, ok := hostCpuBusy[load.Host]; !ok {
			hostCpuBusy[load.Host] = model.ConnectMoveLoadCpuUnknown
		}
		hostResidents[load.Host] += max(0, load.Residents)
		hostCores[load.Host] = max(hostCores[load.Host], load.HostCores)
		if 0 <= load.HostCpuBusy {
			hostCpuBusy[load.Host] = max(hostCpuBusy[load.Host], load.HostCpuBusy)
		}
	}

	hosts := make([]string, 0, len(hostCpuBusy))
	for host := range hostCpuBusy {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)

	totalResidents := 0
	meanCapacity := float64(0)
	for _, host := range hosts {
		capacity := float64(hostCores[host]) * placementHostWeight(inventory, host)
		if !(0 < capacity) {
			continue
		}
		totalResidents += hostResidents[host]
		if evacuateHosts[host] {
			continue
		}
		meanCapacity += capacity
		interfaces := placementInterfaces(inventory.Interfaces[host])
		if len(interfaces) == 0 {
			continue
		}
		fleet.hosts = append(fleet.hosts, &placementHost{
			host:       host,
			residents:  hostResidents[host],
			capacity:   capacity,
			cpuBusy:    hostCpuBusy[host],
			interfaces: interfaces,
		})
		for _, placeable := range interfaces {
			if placeable.ipv4.IsValid() {
				fleet.addressHosts[placeable.ipv4] = host
			}
			if placeable.ipv6.IsValid() {
				fleet.addressHosts[placeable.ipv6] = host
			}
		}
	}
	if 0 < meanCapacity {
		fleet.mean = float64(totalResidents) / meanCapacity
	}
	if math.IsNaN(tolerance) || math.IsInf(tolerance, 0) || tolerance < 0 {
		tolerance = 0
	}
	fleet.bandEdge = fleet.mean * (1 + tolerance)
	return fleet
}

// The interfaces of the host with an address that is not held.
func unheldPlacementInterfaces(interfaces []placementInterface, held func(netip.Addr) bool) []placementInterface {
	unheld := []placementInterface{}
	for _, placeable := range interfaces {
		candidate := placeable
		if candidate.ipv4.IsValid() && held(candidate.ipv4) {
			candidate.ipv4 = netip.Addr{}
		}
		if candidate.ipv6.IsValid() && held(candidate.ipv6) {
			candidate.ipv6 = netip.Addr{}
		}
		if candidate.ipv4.IsValid() || candidate.ipv6.IsValid() {
			unheld = append(unheld, candidate)
		}
	}
	return unheld
}

// A host for one arrival and the interfaces it can be dialed on, or nil when
// no host has an address that is not held. See the file header for the rule.
func choosePlacementHost(
	fleet *placementFleet,
	maxCpu float64,
	held func(netip.Addr) bool,
	random placementRandom,
) (*placementHost, []placementInterface) {
	type candidate struct {
		host       *placementHost
		interfaces []placementInterface
		headroom   float64
	}
	candidates := []candidate{}
	for _, host := range fleet.hosts {
		interfaces := unheldPlacementInterfaces(host.interfaces, held)
		if len(interfaces) == 0 {
			continue
		}
		headroom := fleet.bandEdge*host.capacity - float64(host.residents)
		if math.IsNaN(headroom) || math.IsInf(headroom, 0) {
			headroom = 0
		}
		candidates = append(candidates, candidate{
			host:       host,
			interfaces: interfaces,
			headroom:   max(0, headroom),
		})
	}
	draw := func(weight func(candidate) float64) (*placementHost, []placementInterface, bool) {
		total := float64(0)
		for _, c := range candidates {
			total += weight(c)
		}
		if !(0 < total) {
			return nil, nil, false
		}
		r := random.Float64() * total
		var last *candidate
		for i := range candidates {
			w := weight(candidates[i])
			if !(0 < w) {
				continue
			}
			last = &candidates[i]
			r -= w
			if r < 0 {
				return candidates[i].host, candidates[i].interfaces, true
			}
		}
		// rounding left r at zero past the last
		return last.host, last.interfaces, true
	}
	coolHeadroom := func(c candidate) float64 {
		if c.host.cpuBusy < 0 || !(c.host.cpuBusy < maxCpu) {
			return 0
		}
		return c.headroom
	}
	if host, interfaces, ok := draw(coolHeadroom); ok {
		return host, interfaces
	}
	if host, interfaces, ok := draw(func(c candidate) float64 { return c.headroom }); ok {
		return host, interfaces
	}
	if host, interfaces, ok := draw(func(c candidate) float64 { return c.host.capacity }); ok {
		return host, interfaces
	}
	return nil, nil
}

// One address per family among the interfaces, each chosen with probability
// proportional to its interface's DNS weight: at most one IPv4 and one IPv6
// address, IPv4 first.
func choosePlacementAddresses(interfaces []placementInterface, random placementRandom) []netip.Addr {
	addresses := []netip.Addr{}
	choose := func(address func(placementInterface) netip.Addr) {
		total := 0
		for _, placeable := range interfaces {
			if address(placeable).IsValid() {
				total += placeable.dnsWeight
			}
		}
		if total <= 0 {
			return
		}
		r := random.IntN(total)
		for _, placeable := range interfaces {
			if !address(placeable).IsValid() {
				continue
			}
			r -= placeable.dnsWeight
			if r < 0 {
				addresses = append(addresses, address(placeable))
				return
			}
		}
	}
	choose(func(placeable placementInterface) netip.Addr { return placeable.ipv4 })
	choose(func(placeable placementInterface) netip.Addr { return placeable.ipv6 })
	return addresses
}
