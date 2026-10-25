package connect

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"
)

// A synthetic services.yml with two edge hosts and the optional move weights.
const edgeInventoryTestServicesYaml = `
domain: synthetic.example
versions:
-   lb:
        interfaces:
            alpha-edge-0.synthetic.example:
                eno1:
                    dns_weight: 10
                    ipv4: 192.0.2.10
                    ipv6: "2001:db8::10"
                eno2:
                    dns_weight: 30
                    ipv4: 192.0.2.11
                eno3:
                    ipv4: not-an-address
            alpha-edge-1.synthetic.example:
                eno1np0:
                    ipv4: 198.51.100.21
                    ipv6: "2001:db8::21"
        move_weights:
            alpha-edge-1.synthetic.example: 1.5
            alpha-edge-0.synthetic.example: -2
`

// A second version of the inventory: a host joined and one left.
const edgeInventoryTestServicesYamlNext = `
domain: synthetic.example
versions:
-   lb:
        interfaces:
            alpha-edge-1.synthetic.example:
                eno1np0:
                    ipv4: 198.51.100.21
            alpha-edge-7.synthetic.example:
                eno1:
                    ipv6: "2001:db8::70"
`

// A services.yml loader the test switches, counting loads.
type edgeInventoryTestLoader struct {
	stateLock    sync.Mutex
	servicesYaml string
	err          error
	loadCount    int
	loaded       chan struct{}
}

func newEdgeInventoryTestLoader(servicesYaml string) *edgeInventoryTestLoader {
	return &edgeInventoryTestLoader{
		servicesYaml: servicesYaml,
		loaded:       make(chan struct{}, 64),
	}
}

func (self *edgeInventoryTestLoader) set(servicesYaml string, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.servicesYaml = servicesYaml
	self.err = err
}

func (self *edgeInventoryTestLoader) load() ([]byte, error) {
	defer func() {
		self.loaded <- struct{}{}
	}()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.loadCount += 1
	if self.err != nil {
		return nil, self.err
	}
	return []byte(self.servicesYaml), nil
}

// Waits for the next completed load and then for the inventory to apply it.
func (self *edgeInventoryTestLoader) waitForLoad(t testing.TB, inventory *EdgeInventory, applied func() bool) {
	t.Helper()
	endTime := time.Now().Add(10 * time.Second)
	for !applied() {
		if endTime.Before(time.Now()) {
			t.Fatal("the inventory did not apply the load")
		}
		select {
		case <-self.loaded:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestEdgeInventoryExposesHostsAddressesAndWeights(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loader := newEdgeInventoryTestLoader(edgeInventoryTestServicesYaml)
	inventory := newEdgeInventory(ctx, "alpha-edge-0", 0, loader.load)
	<-inventory.Loaded()

	hosts := inventory.Hosts()
	if len(hosts) != 2 || hosts[0].Host != "alpha-edge-0" || hosts[1].Host != "alpha-edge-1" {
		t.Fatalf("hosts = %+v", hosts)
	}
	alpha0 := hosts[0]
	// an invalid move weight keeps the default
	if alpha0.MoveWeight != 1 || hosts[1].MoveWeight != 1.5 {
		t.Fatalf("move weights = %v, %v", alpha0.MoveWeight, hosts[1].MoveWeight)
	}
	// an interface whose address does not parse is no target
	wantInterfaces := []EdgeInventoryInterface{
		{Interface: "eno1", Ipv4: netip.MustParseAddr("192.0.2.10"), Ipv6: netip.MustParseAddr("2001:db8::10"), DnsWeight: 10},
		{Interface: "eno2", Ipv4: netip.MustParseAddr("192.0.2.11"), DnsWeight: 30},
	}
	if !reflect.DeepEqual(alpha0.Interfaces, wantInterfaces) {
		t.Fatalf("interfaces = %+v, want %+v", alpha0.Interfaces, wantInterfaces)
	}
	ipv4s, ipv6s := alpha0.Addrs()
	if !reflect.DeepEqual(ipv4s, []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")}) ||
		!reflect.DeepEqual(ipv6s, []netip.Addr{netip.MustParseAddr("2001:db8::10")}) {
		t.Fatalf("addrs = %v, %v", ipv4s, ipv6s)
	}

	host, ok := inventory.Host("alpha-edge-1")
	if !ok || host.Host != "alpha-edge-1" {
		t.Fatalf("host = %+v, %t", host, ok)
	}
	if _, ok := inventory.Host("alpha-edge-9"); ok {
		t.Fatal("an unknown host was found")
	}
}

// WARP_HOST names its inventory host by the inventory name or qualified with
// the services domain.
func TestEdgeInventoryMatchesProcessHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, test := range []struct {
		processHost string
		want        string
	}{
		{processHost: "alpha-edge-1", want: "alpha-edge-1"},
		{processHost: "alpha-edge-1.synthetic.example", want: "alpha-edge-1"},
		{processHost: "ALPHA-EDGE-0", want: "alpha-edge-0"},
		{processHost: "alpha-edge-1.other.example", want: ""},
		{processHost: "beta-api-0", want: ""},
		{processHost: "", want: ""},
	} {
		inventory := newEdgeInventory(ctx, test.processHost, 0, newEdgeInventoryTestLoader(edgeInventoryTestServicesYaml).load)
		<-inventory.Loaded()
		host, ok := inventory.LocalHost()
		if test.want == "" {
			if ok {
				t.Fatalf("process host %q matched %q", test.processHost, host.Host)
			}
			continue
		}
		if !ok || host.Host != test.want {
			t.Fatalf("process host %q matched %q, %t, want %q", test.processHost, host.Host, ok, test.want)
		}
	}
}

// The inventory reloads on its interval: a host that joins is picked up and
// one that leaves is dropped, a failed load keeps the last good inventory, and
// a load that never succeeds leaves the inventory empty without failing.
func TestEdgeInventoryReloadsAndToleratesFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failing := newEdgeInventoryTestLoader("")
	failing.set("", errors.New("services.yml unavailable"))
	empty := newEdgeInventory(ctx, "alpha-edge-0", 0, failing.load)
	<-empty.Loaded()
	if hosts := empty.Hosts(); len(hosts) != 0 {
		t.Fatalf("a failed first load produced hosts %+v", hosts)
	}
	if _, ok := empty.LocalHost(); ok {
		t.Fatal("a failed first load matched a local host")
	}

	loader := newEdgeInventoryTestLoader(edgeInventoryTestServicesYaml)
	inventory := newEdgeInventory(ctx, "alpha-edge-1", 10*time.Millisecond, loader.load)
	<-inventory.Loaded()
	if _, ok := inventory.Host("alpha-edge-0"); !ok {
		t.Fatal("the first load is missing a host")
	}

	// a load that fails, then one that does not parse, keep the last good one
	loader.set("", errors.New("services.yml unavailable"))
	loader.waitForLoad(t, inventory, func() bool {
		loader.stateLock.Lock()
		defer loader.stateLock.Unlock()
		return 3 <= loader.loadCount
	})
	if _, ok := inventory.Host("alpha-edge-0"); !ok {
		t.Fatal("a failed load dropped the last good inventory")
	}
	loader.set("versions: [", nil)
	loader.waitForLoad(t, inventory, func() bool {
		loader.stateLock.Lock()
		defer loader.stateLock.Unlock()
		return 6 <= loader.loadCount
	})
	if _, ok := inventory.Host("alpha-edge-0"); !ok {
		t.Fatal("an unparseable load dropped the last good inventory")
	}

	loader.set(edgeInventoryTestServicesYamlNext, nil)
	loader.waitForLoad(t, inventory, func() bool {
		_, joined := inventory.Host("alpha-edge-7")
		return joined
	})
	if _, ok := inventory.Host("alpha-edge-0"); ok {
		t.Fatal("a host that left the inventory is still a target")
	}
	if host, ok := inventory.LocalHost(); !ok || host.Host != "alpha-edge-1" {
		t.Fatalf("local host after the reload = %+v, %t", host, ok)
	}
}

// One target per family, picked in proportion to dns_weight.
func TestEdgeInventoryHostPicksTargetsByDnsWeight(t *testing.T) {
	host := EdgeInventoryHost{
		Host: "alpha-edge-0",
		Interfaces: []EdgeInventoryInterface{
			{Interface: "eno1", Ipv4: netip.MustParseAddr("192.0.2.10"), Ipv6: netip.MustParseAddr("2001:db8::10"), DnsWeight: 10},
			{Interface: "eno2", Ipv4: netip.MustParseAddr("192.0.2.11"), DnsWeight: 30},
		},
	}
	// the v4 weights are 10 then 30 over [0, 40); the only v6 address takes any draw
	for _, test := range []struct {
		draw int64
		want []netip.Addr
	}{
		{draw: 0, want: []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")}},
		{draw: 9, want: []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")}},
		{draw: 10, want: []netip.Addr{netip.MustParseAddr("192.0.2.11"), netip.MustParseAddr("2001:db8::10")}},
		{draw: 39, want: []netip.Addr{netip.MustParseAddr("192.0.2.11"), netip.MustParseAddr("2001:db8::10")}},
	} {
		draws := []int64{}
		targets := host.PickTargets(func(n int64) int64 {
			draws = append(draws, n)
			return min(test.draw, n-1)
		})
		if !reflect.DeepEqual(targets, test.want) {
			t.Fatalf("draw %d picked %v, want %v", test.draw, targets, test.want)
		}
		if !reflect.DeepEqual(draws, []int64{40, 10}) {
			t.Fatalf("weight totals = %v, want [40 10]", draws)
		}
	}
	if targets := (EdgeInventoryHost{}).PickTargets(func(n int64) int64 { return 0 }); len(targets) != 0 {
		t.Fatalf("a host without interfaces picked %v", targets)
	}
}
