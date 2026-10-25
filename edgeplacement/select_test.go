package edgeplacement

import (
	"math"
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A random source that answers queued values, so a draw's position in the
// weights is set by the test.
type testPlacementRandom struct {
	floats []float64
	ints   []int
}

func (self *testPlacementRandom) Float64() float64 {
	value := self.floats[0]
	self.floats = self.floats[1:]
	return value
}

func (self *testPlacementRandom) IntN(n int) int {
	value := self.ints[0]
	self.ints = self.ints[1:]
	return value % n
}

// Synthetic edge hosts on documentation addresses. test-edge-2 has a second,
// IPv4-only interface with half the DNS weight.
func testPlacementInventory() *server.ServicesLbInventory {
	return &server.ServicesLbInventory{
		Domain: "space.example",
		Interfaces: map[string][]server.ServicesLbInterface{
			"test-edge-1": {
				{Interface: "eth0", Ipv4: "192.0.2.1", Ipv6: "2001:db8::1", DnsWeight: 100},
			},
			"test-edge-2": {
				{Interface: "eth1", Ipv4: "198.51.100.2", DnsWeight: 50},
				{Interface: "eth0", Ipv4: "192.0.2.2", Ipv6: "2001:db8::2", DnsWeight: 100},
			},
			"test-edge-3": {
				{Interface: "eth0", Ipv4: "192.0.2.3", DnsWeight: 100},
			},
			"test-edge-4": {
				{Interface: "eth0", Ipv4: "192.0.2.4", DnsWeight: 100},
			},
		},
		MoveWeights: map[string]float64{},
	}
}

func testPlacementLoad(host string, residents int, hostCores int, hostCpuBusy float64, at time.Time) *model.ConnectMoveLoad {
	return &model.ConnectMoveLoad{
		Host:        host,
		Block:       "g1",
		ExchangeId:  server.NewId(),
		Time:        at,
		Residents:   residents,
		HostCores:   hostCores,
		HostCpuBusy: hostCpuBusy,
	}
}

func testPlacementHostNames(fleet *placementFleet) []string {
	hosts := []string{}
	for _, host := range fleet.hosts {
		hosts = append(hosts, host.host)
	}
	return hosts
}

func notHeld(netip.Addr) bool {
	return false
}

// Four equal hosts: two far below the fleet mean, two far above it. The mean
// is 1000 residents per core and the band edge 1100.
func testImbalancedPlacementFleet(nodeTime time.Time) *placementFleet {
	return computePlacementFleet(
		[]*model.ConnectMoveLoad{
			testPlacementLoad("test-edge-1", 20000, 100, 0.5, nodeTime),
			testPlacementLoad("test-edge-2", 40000, 100, 0.5, nodeTime),
			testPlacementLoad("test-edge-3", 160000, 100, 0.5, nodeTime),
			testPlacementLoad("test-edge-4", 180000, 100, 0.5, nodeTime),
		},
		nodeTime,
		time.Minute,
		testPlacementInventory(),
		map[string]bool{},
		0.10,
	)
}

// The fleet reads the load table as the mover does: fresh, non-draining
// records summed per host, capacity as cores times the move weight, the mean
// over the hosts that are not evacuating; only an inventory host with
// capacity and a placeable interface that is not evacuating takes arrivals.
func TestComputePlacementFleetReadsTheLoadTable(t *testing.T) {
	nodeTime := time.Now()
	inventory := testPlacementInventory()
	inventory.MoveWeights["test-edge-2"] = 2
	// no positive DNS weight: no placeable interface
	inventory.Interfaces["test-edge-4"] = []server.ServicesLbInterface{
		{Interface: "eth0", Ipv4: "192.0.2.4", DnsWeight: -1},
	}
	fleet := computePlacementFleet(
		[]*model.ConnectMoveLoad{
			testPlacementLoad("test-edge-1", 10000, 100, 0.4, nodeTime),
			testPlacementLoad("test-edge-1", 5000, 100, 0.5, nodeTime.Add(-30*time.Second)),
			// stale
			testPlacementLoad("test-edge-1", 99000, 100, 0.9, nodeTime.Add(-2*time.Minute)),
			testPlacementLoad("test-edge-2", 20000, 100, -1, nodeTime),
			// draining
			func() *model.ConnectMoveLoad {
				load := testPlacementLoad("test-edge-2", 77000, 100, 0.9, nodeTime)
				load.Draining = true
				return load
			}(),
			// evacuating: residents in the mean, capacity not
			testPlacementLoad("test-edge-3", 30000, 100, 0.3, nodeTime),
			// no placeable interface: in the mean, takes no arrivals
			testPlacementLoad("test-edge-4", 15000, 100, 0.3, nodeTime),
			// outside the inventory: in the mean, takes no arrivals
			testPlacementLoad("test-edge-9", 20000, 100, 0.3, nodeTime),
			// no cores: no capacity, in nothing
			testPlacementLoad("test-edge-8", 50000, 0, 0.3, nodeTime),
		},
		nodeTime,
		time.Minute,
		inventory,
		map[string]bool{"test-edge-3": true},
		0.10,
	)
	if got := testPlacementHostNames(fleet); !slices.Equal(got, []string{"test-edge-1", "test-edge-2"}) {
		t.Fatalf("placeable hosts = %v", got)
	}
	edge1, edge2 := fleet.hosts[0], fleet.hosts[1]
	if edge1.residents != 15000 || edge1.capacity != 100 || edge1.cpuBusy != 0.5 {
		t.Fatalf("test-edge-1 = %+v, want the fresh records summed at the higher cpu", edge1)
	}
	if edge2.residents != 20000 || edge2.capacity != 200 || edge2.cpuBusy != model.ConnectMoveLoadCpuUnknown {
		t.Fatalf("test-edge-2 = %+v, want the move weight in the capacity and an unknown cpu", edge2)
	}
	// residents 15k+20k+30k+15k+20k over the capacity of edge-1, edge-2, edge-4 and edge-9
	if want := 100000.0 / 500.0; math.Abs(fleet.mean-want) > 1e-9 || math.Abs(fleet.bandEdge-want*1.1) > 1e-9 {
		t.Fatalf("mean = %v band edge = %v, want %v and %v", fleet.mean, fleet.bandEdge, want, want*1.1)
	}
	// interfaces in name order
	if got := edge2.interfaces; len(got) != 2 || got[0].ipv4 != netip.MustParseAddr("192.0.2.2") || got[1].ipv4 != netip.MustParseAddr("198.51.100.2") || got[1].ipv6.IsValid() {
		t.Fatalf("test-edge-2 interfaces = %+v", got)
	}
	for address, host := range map[string]string{
		"192.0.2.1":    "test-edge-1",
		"2001:db8::1":  "test-edge-1",
		"198.51.100.2": "test-edge-2",
	} {
		if got := fleet.addressHosts[netip.MustParseAddr(address)]; got != host {
			t.Errorf("address %s belongs to %q, want %s", address, got, host)
		}
	}
	if _, ok := fleet.addressHosts[netip.MustParseAddr("192.0.2.3")]; ok {
		t.Error("an evacuating host's address is placeable")
	}
}

// Arrivals go to the hosts below the band edge in proportion to their
// headroom, and never to a host above it.
func TestChoosePlacementHostFillsHeadroom(t *testing.T) {
	fleet := testImbalancedPlacementFleet(time.Now())
	// headroom: test-edge-1 90k, test-edge-2 70k, the others none
	for _, c := range []struct {
		draw float64
		host string
	}{
		{draw: 0, host: "test-edge-1"},
		{draw: 0.5, host: "test-edge-1"},
		{draw: 0.56, host: "test-edge-1"},
		{draw: 0.57, host: "test-edge-2"},
		{draw: 0.999, host: "test-edge-2"},
	} {
		host, _ := choosePlacementHost(fleet, 0.8, notHeld, &testPlacementRandom{floats: []float64{c.draw}})
		if host == nil || host.host != c.host {
			t.Errorf("draw %v placed on %v, want %s", c.draw, host, c.host)
		}
	}

	random := rand.New(rand.NewPCG(1, 2))
	counts := map[string]int{}
	drawCount := 20000
	for i := 0; i < drawCount; i += 1 {
		host, _ := choosePlacementHost(fleet, 0.8, notHeld, random)
		counts[host.host] += 1
	}
	if counts["test-edge-3"] != 0 || counts["test-edge-4"] != 0 {
		t.Fatalf("hosts above the band took arrivals: %v", counts)
	}
	if share := float64(counts["test-edge-1"]) / float64(drawCount); math.Abs(share-90.0/160.0) > 0.02 {
		t.Fatalf("test-edge-1 share = %.3f, want %.3f", share, 90.0/160.0)
	}
}

// A fleet within the band places in proportion to capacity.
func TestChoosePlacementHostKeepsABalancedFleetBalanced(t *testing.T) {
	nodeTime := time.Now()
	inventory := testPlacementInventory()
	inventory.MoveWeights["test-edge-2"] = 3
	fleet := computePlacementFleet(
		[]*model.ConnectMoveLoad{
			testPlacementLoad("test-edge-1", 10000, 100, 0.5, nodeTime),
			testPlacementLoad("test-edge-2", 30000, 100, 0.5, nodeTime),
		},
		nodeTime,
		time.Minute,
		inventory,
		map[string]bool{},
		0.10,
	)
	random := rand.New(rand.NewPCG(3, 4))
	counts := map[string]int{}
	drawCount := 20000
	for i := 0; i < drawCount; i += 1 {
		host, _ := choosePlacementHost(fleet, 0.8, notHeld, random)
		counts[host.host] += 1
	}
	if share := float64(counts["test-edge-2"]) / float64(drawCount); math.Abs(share-0.75) > 0.02 {
		t.Fatalf("test-edge-2 share = %.3f, want its capacity share 0.75", share)
	}
}

// A host at or above the cpu ceiling, or whose cpu is unknown, takes no
// arrivals while a host with headroom is below the ceiling; with none below
// it, headroom decides; with no headroom anywhere, capacity does.
func TestChoosePlacementHostKeepsArrivalsOffHotHosts(t *testing.T) {
	nodeTime := time.Now()
	fleetWithCpu := func(cpu1 float64, cpu2 float64, tolerance float64, residents1 int, residents2 int) *placementFleet {
		return computePlacementFleet(
			[]*model.ConnectMoveLoad{
				testPlacementLoad("test-edge-1", residents1, 100, cpu1, nodeTime),
				testPlacementLoad("test-edge-2", residents2, 100, cpu2, nodeTime),
				testPlacementLoad("test-edge-3", 160000, 100, 0.5, nodeTime),
			},
			nodeTime,
			time.Minute,
			testPlacementInventory(),
			map[string]bool{},
			tolerance,
		)
	}
	for _, c := range []struct {
		name  string
		fleet *placementFleet
		// draws across the whole range must land only here
		hosts []string
	}{
		{name: "a hot host with headroom yields to a cool one", fleet: fleetWithCpu(0.9, 0.5, 0.10, 20000, 40000), hosts: []string{"test-edge-2"}},
		{name: "the ceiling itself is hot", fleet: fleetWithCpu(0.8, 0.5, 0.10, 20000, 40000), hosts: []string{"test-edge-2"}},
		{name: "an unknown cpu yields to a measured one", fleet: fleetWithCpu(-1, 0.5, 0.10, 20000, 40000), hosts: []string{"test-edge-2"}},
		{name: "every host with headroom is hot: headroom decides", fleet: fleetWithCpu(0.9, 0.95, 0.10, 20000, 40000), hosts: []string{"test-edge-1", "test-edge-2"}},
	} {
		seen := map[string]bool{}
		for _, draw := range []float64{0, 0.25, 0.5, 0.75, 0.999} {
			host, _ := choosePlacementHost(c.fleet, 0.8, notHeld, &testPlacementRandom{floats: []float64{draw}})
			if host == nil || !slices.Contains(c.hosts, host.host) {
				t.Errorf("%s: draw %v placed on %v, want one of %v", c.name, draw, host, c.hosts)
				continue
			}
			seen[host.host] = true
		}
		if len(seen) != len(c.hosts) {
			t.Errorf("%s: placed on %v, want each of %v", c.name, seen, c.hosts)
		}
	}

	// no headroom anywhere (a zero band over an exact balance): capacity
	balanced := computePlacementFleet(
		[]*model.ConnectMoveLoad{
			testPlacementLoad("test-edge-1", 10000, 100, 0.9, nodeTime),
			testPlacementLoad("test-edge-2", 10000, 100, 0.9, nodeTime),
		},
		nodeTime,
		time.Minute,
		testPlacementInventory(),
		map[string]bool{},
		0,
	)
	for _, c := range []struct {
		draw float64
		host string
	}{
		{draw: 0.25, host: "test-edge-1"},
		{draw: 0.75, host: "test-edge-2"},
	} {
		host, _ := choosePlacementHost(balanced, 0.8, notHeld, &testPlacementRandom{floats: []float64{c.draw}})
		if host == nil || host.host != c.host {
			t.Errorf("no headroom: draw %v placed on %v, want %s by capacity", c.draw, host, c.host)
		}
	}
}

// A held address is never offered: a host with every address held takes no
// arrivals, and one with only its IPv4 address held is still dialed on IPv6.
func TestChoosePlacementHostSkipsHeldAddresses(t *testing.T) {
	fleet := testImbalancedPlacementFleet(time.Now())
	allHeld := func(address netip.Addr) bool {
		return address == netip.MustParseAddr("192.0.2.1") || address == netip.MustParseAddr("2001:db8::1")
	}
	for _, draw := range []float64{0, 0.5, 0.999} {
		host, _ := choosePlacementHost(fleet, 0.8, allHeld, &testPlacementRandom{floats: []float64{draw}})
		if host == nil || host.host != "test-edge-2" {
			t.Fatalf("draw %v with test-edge-1 held placed on %v", draw, host)
		}
	}

	ipv4Held := func(address netip.Addr) bool {
		return address == netip.MustParseAddr("192.0.2.1")
	}
	host, interfaces := choosePlacementHost(fleet, 0.8, ipv4Held, &testPlacementRandom{floats: []float64{0}})
	if host == nil || host.host != "test-edge-1" {
		t.Fatalf("with only its ipv4 held, placed on %v, want test-edge-1", host)
	}
	if got := choosePlacementAddresses(interfaces, &testPlacementRandom{ints: []int{0, 0}}); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("2001:db8::1")}) {
		t.Fatalf("addresses with the ipv4 held = %v, want the ipv6 only", got)
	}

	everyHeld := func(netip.Addr) bool { return true }
	if host, _ := choosePlacementHost(fleet, 0.8, everyHeld, &testPlacementRandom{floats: []float64{0}}); host != nil {
		t.Fatalf("with every address held, placed on %v", host)
	}
}

// One address per family, each chosen in proportion to its interface's DNS
// weight, IPv4 first.
func TestChoosePlacementAddressesWeighsByDnsWeight(t *testing.T) {
	fleet := testImbalancedPlacementFleet(time.Now())
	edge2 := fleet.hosts[1]
	for _, c := range []struct {
		// the IPv4 draw over weights 100 (eth0) and 50 (eth1), then the IPv6 draw
		ints []int
		want []netip.Addr
	}{
		{ints: []int{0, 0}, want: []netip.Addr{netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("2001:db8::2")}},
		{ints: []int{99, 0}, want: []netip.Addr{netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("2001:db8::2")}},
		{ints: []int{100, 0}, want: []netip.Addr{netip.MustParseAddr("198.51.100.2"), netip.MustParseAddr("2001:db8::2")}},
		{ints: []int{149, 0}, want: []netip.Addr{netip.MustParseAddr("198.51.100.2"), netip.MustParseAddr("2001:db8::2")}},
	} {
		if got := choosePlacementAddresses(edge2.interfaces, &testPlacementRandom{ints: c.ints}); !slices.Equal(got, c.want) {
			t.Errorf("draws %v: addresses = %v, want %v", c.ints, got, c.want)
		}
	}
}
