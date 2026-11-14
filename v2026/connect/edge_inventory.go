package connect

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// The edge inventory that server-directed moves target (connect/MOVE.md §2.6,
// D17): each edge host's public interfaces from the vault `services.yml`
// (server.ParseServicesLbInventory, the rules the monitor reads it with),
// reloaded every MoveInventoryRefreshInterval so a new host is picked up
// without a deploy, plus each host's optional move weight.
//
// The inventory never affects connect startup. A load that fails keeps the last
// good inventory, and before the first good load the inventory is empty, which
// only means there is nothing to target. Safe for concurrent use.
type EdgeInventory struct {
	ctx         context.Context
	processHost string
	// non-positive loads once
	refreshInterval time.Duration
	load            func() ([]byte, error)

	stateLock sync.Mutex
	// nil until the first good load
	inventory *server.ServicesLbInventory
	// the last load failed; a failure is logged once until a load succeeds
	failing bool
	// closed once after the first load attempt
	loaded     chan struct{}
	loadedOnce sync.Once
}

// One edge host of the inventory.
type EdgeInventoryHost struct {
	// the configured host without the services domain
	Host string
	// the optional `lb.move_weights` entry, 1 when absent (MOVE.md D3)
	MoveWeight float64
	// sorted by interface name
	Interfaces []EdgeInventoryInterface
}

// One public interface of an edge host.
type EdgeInventoryInterface struct {
	Interface string
	// the zero address when the interface has no address of the family
	Ipv4 netip.Addr
	Ipv6 netip.Addr
	// the weight of the interface's records in the route53 weighted sets
	DnsWeight int
}

// Reads the vault services.yml.
func loadVaultServicesYaml() ([]byte, error) {
	resource, err := server.Vault.SimpleResource("services.yml")
	if err != nil {
		return nil, err
	}
	return resource.BytesE()
}

// An inventory of this process's host (WARP_HOST) that loads now and then
// every refreshInterval until ctx ends.
func newEdgeInventory(
	ctx context.Context,
	processHost string,
	refreshInterval time.Duration,
	load func() ([]byte, error),
) *EdgeInventory {
	inventory := &EdgeInventory{
		ctx:             ctx,
		processHost:     processHost,
		refreshInterval: refreshInterval,
		load:            load,
		loaded:          make(chan struct{}),
	}
	go server.HandleError(inventory.run)
	return inventory
}

func (self *EdgeInventory) run() {
	defer self.loadedOnce.Do(func() {
		close(self.loaded)
	})
	for {
		self.reload()
		self.loadedOnce.Do(func() {
			close(self.loaded)
		})
		if self.refreshInterval <= 0 {
			return
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.refreshInterval):
		}
	}
}

// Loads the inventory once, keeping the last good one on a failure.
func (self *EdgeInventory) reload() {
	var servicesYamlBytes []byte
	var err error
	server.HandleError(func() {
		servicesYamlBytes, err = self.load()
	}, func(loadErr error) {
		err = loadErr
	})
	var inventory *server.ServicesLbInventory
	if err == nil {
		inventory, err = server.ParseServicesLbInventory(servicesYamlBytes)
	}
	logFailure := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if err != nil {
			logFailure := !self.failing
			self.failing = true
			return logFailure
		}
		self.inventory = inventory
		self.failing = false
		return false
	}()
	if logFailure {
		glog.Infof("[move]inventory unavailable, keeping the last one = %s\n", err)
	} else if err != nil && glog.V(1) {
		glog.Infof("[move]inventory unavailable = %s\n", err)
	}
}

// Closes after the first load attempt, good or not.
func (self *EdgeInventory) Loaded() <-chan struct{} {
	return self.loaded
}

func (self *EdgeInventory) currentInventory() *server.ServicesLbInventory {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.inventory
}

func edgeInventoryHost(inventory *server.ServicesLbInventory, host string) (EdgeInventoryHost, bool) {
	configuredInterfaces, ok := inventory.Interfaces[host]
	if !ok {
		return EdgeInventoryHost{}, false
	}
	inventoryHost := EdgeInventoryHost{
		Host:       host,
		MoveWeight: 1,
	}
	if moveWeight, ok := inventory.MoveWeights[host]; ok && 0 < moveWeight {
		inventoryHost.MoveWeight = moveWeight
	}
	for _, configuredInterface := range configuredInterfaces {
		inventoryInterface := EdgeInventoryInterface{
			Interface: configuredInterface.Interface,
			DnsWeight: configuredInterface.DnsWeight,
		}
		// an address of the wrong family or that does not parse is no target
		if ipv4, err := netip.ParseAddr(configuredInterface.Ipv4); err == nil && ipv4.Unmap().Is4() {
			inventoryInterface.Ipv4 = ipv4.Unmap()
		}
		if ipv6, err := netip.ParseAddr(configuredInterface.Ipv6); err == nil && ipv6.Is6() && !ipv6.Is4In6() {
			inventoryInterface.Ipv6 = ipv6
		}
		if !inventoryInterface.Ipv4.IsValid() && !inventoryInterface.Ipv6.IsValid() {
			continue
		}
		inventoryHost.Interfaces = append(inventoryHost.Interfaces, inventoryInterface)
	}
	return inventoryHost, true
}

// Every edge host of the inventory, sorted by host.
func (self *EdgeInventory) Hosts() []EdgeInventoryHost {
	inventory := self.currentInventory()
	if inventory == nil {
		return nil
	}
	hosts := make([]EdgeInventoryHost, 0, len(inventory.Interfaces))
	for host := range inventory.Interfaces {
		if inventoryHost, ok := edgeInventoryHost(inventory, host); ok {
			hosts = append(hosts, inventoryHost)
		}
	}
	slices.SortFunc(hosts, func(a EdgeInventoryHost, b EdgeInventoryHost) int {
		return strings.Compare(a.Host, b.Host)
	})
	return hosts
}

// One edge host by its inventory name.
func (self *EdgeInventory) Host(host string) (EdgeInventoryHost, bool) {
	inventory := self.currentInventory()
	if inventory == nil {
		return EdgeInventoryHost{}, false
	}
	return edgeInventoryHost(inventory, host)
}

// The inventory host of this process: the one WARP_HOST names, either as the
// inventory name or qualified with the services domain.
func (self *EdgeInventory) LocalHost() (EdgeInventoryHost, bool) {
	inventory := self.currentInventory()
	if inventory == nil {
		return EdgeInventoryHost{}, false
	}
	processHost := strings.ToLower(strings.TrimSpace(self.processHost))
	if processHost == "" {
		return EdgeInventoryHost{}, false
	}
	for host := range inventory.Interfaces {
		lowerHost := strings.ToLower(host)
		if processHost == lowerHost || (inventory.Domain != "" && processHost == lowerHost+"."+strings.ToLower(inventory.Domain)) {
			return edgeInventoryHost(inventory, host)
		}
	}
	return EdgeInventoryHost{}, false
}

// The host's public addresses by family, in interface order.
func (self EdgeInventoryHost) Addrs() (ipv4s []netip.Addr, ipv6s []netip.Addr) {
	for _, inventoryInterface := range self.Interfaces {
		if inventoryInterface.Ipv4.IsValid() {
			ipv4s = append(ipv4s, inventoryInterface.Ipv4)
		}
		if inventoryInterface.Ipv6.IsValid() {
			ipv6s = append(ipv6s, inventoryInterface.Ipv6)
		}
	}
	return
}

// One target per family the host has: an interface address picked with
// probability proportional to its dns_weight (MOVE.md §5.5, D4). int63n is the
// random source, rand.Int63n in production.
func (self EdgeInventoryHost) PickTargets(int63n func(int64) int64) []netip.Addr {
	pick := func(address func(EdgeInventoryInterface) netip.Addr) (netip.Addr, bool) {
		totalWeight := int64(0)
		for _, inventoryInterface := range self.Interfaces {
			if address(inventoryInterface).IsValid() {
				totalWeight += int64(max(1, inventoryInterface.DnsWeight))
			}
		}
		if totalWeight == 0 {
			return netip.Addr{}, false
		}
		r := int63n(totalWeight)
		for _, inventoryInterface := range self.Interfaces {
			if !address(inventoryInterface).IsValid() {
				continue
			}
			r -= int64(max(1, inventoryInterface.DnsWeight))
			if r < 0 {
				return address(inventoryInterface), true
			}
		}
		return netip.Addr{}, false
	}
	targets := []netip.Addr{}
	if ipv4, ok := pick(func(i EdgeInventoryInterface) netip.Addr { return i.Ipv4 }); ok {
		targets = append(targets, ipv4)
	}
	if ipv6, ok := pick(func(i EdgeInventoryInterface) netip.Addr { return i.Ipv6 }); ok {
		targets = append(targets, ipv6)
	}
	return targets
}
