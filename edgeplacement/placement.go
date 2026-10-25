// Package edgeplacement chooses the edge a platform-hosted client dials
// (connect/MOVE.md §13): the hosted proxy's windows and the provider probes.
//
// These clients run on the platform, so they read the load model the mover
// reads -- the `{connect_load}` table every connect process publishes
// (model/connect_move_load.go) and the vault `services.yml` edge inventory --
// instead of leaving their edge to DNS. route53 answers one weighted record
// per query and large resolvers cache it, so a whole host of clients lands on
// whichever edge the cache held (MOVE.md §1). A client placed where the mover
// would send it never needs the move.
//
// An `EdgePlacement` keeps a snapshot of the model that its own goroutine
// refreshes every `LoadRefreshInterval`, and answers each
// `PlatformPlacementTargets` from that snapshot without blocking (select.go
// has the policy). It answers none, so the client resolves the name as
// before, for a host other than the platform host of the inventory's domain,
// while the control's `placement` is off, before the first read, and once the
// snapshot is older than `ModelMaxAge`. An address whose dial failed is held
// for `FailHold` for every client of the process.
//
// Safe for concurrent use.
package edgeplacement

import (
	"context"
	"math/rand/v2"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type EdgePlacementSettings struct {
	// The code switch. The control's `placement` can only turn it off.
	EnablePlacement bool
	// How often the load table and the control are read. Ten seconds, the
	// mover's tick: the arrivals between two reads follow one snapshot.
	// Non-positive reads once.
	LoadRefreshInterval time.Duration
	// Bounds one read of the load table and the control.
	LoadReadTimeout time.Duration
	// A load record older than this, by the Redis clock, is ignored. Sixty
	// seconds, the mover's `MoveLoadStaleTimeout`: three missed publishes.
	LoadStaleTimeout time.Duration
	// How often the inventory is reloaded from the vault. Five minutes, the
	// mover's: a new edge host takes arrivals without a deploy.
	InventoryRefreshInterval time.Duration
	// A snapshot older than this places nothing: placement does not act on a
	// model it cannot refresh. Two minutes, a dozen failed reads.
	// Non-positive never ages.
	ModelMaxAge time.Duration
	// The band around the fleet mean, the mover's `MoveBalanceTolerance`.
	Tolerance float64
	// Hosts at or above this CPU busy fraction take no arrivals while a host
	// with headroom is below it. 0.8 keeps arrivals off a host near
	// saturation, where open transactions stall (MOVE.md §1).
	MaxCpu float64
	// How long an address whose dial failed is not offered. Two minutes: a
	// broken interface costs each client of the process at most one failed
	// dial per hold, and a repaired one is offered again soon. Non-positive
	// holds none.
	FailHold time.Duration
}

func DefaultEdgePlacementSettings() *EdgePlacementSettings {
	return &EdgePlacementSettings{
		EnablePlacement:          true,
		LoadRefreshInterval:      10 * time.Second,
		LoadReadTimeout:          5 * time.Second,
		LoadStaleTimeout:         60 * time.Second,
		InventoryRefreshInterval: 5 * time.Minute,
		ModelMaxAge:              2 * time.Minute,
		Tolerance:                0.10,
		MaxCpu:                   0.8,
		FailHold:                 2 * time.Minute,
	}
}

// Where the placement reads the model. Tests replace it.
type placementStore interface {
	loads(ctx context.Context) ([]*model.ConnectMoveLoad, time.Time, error)
	control(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error)
	inventory() (*server.ServicesLbInventory, error)
}

// The model in Redis and the vault.
type modelPlacementStore struct{}

func (modelPlacementStore) loads(ctx context.Context) ([]*model.ConnectMoveLoad, time.Time, error) {
	return model.GetConnectMoveLoads(ctx)
}

func (modelPlacementStore) control(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
	return model.GetConnectMoveControl(ctx, ceiling)
}

func (modelPlacementStore) inventory() (*server.ServicesLbInventory, error) {
	resource, err := server.Vault.SimpleResource("services.yml")
	if err != nil {
		return nil, err
	}
	servicesYamlBytes, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	return server.ParseServicesLbInventory(servicesYamlBytes)
}

// math/rand/v2's top-level functions, which are safe for concurrent use.
type globalPlacementRandom struct{}

func (globalPlacementRandom) Float64() float64 {
	return rand.Float64()
}

func (globalPlacementRandom) IntN(n int) int {
	return rand.IntN(n)
}

// One read of the model.
type placementSnapshot struct {
	// the local clock at the read
	readTime time.Time
	// the control's placement
	placement bool
	// the platform host of the inventory's domain, lowercase
	platformHost string
	fleet        *placementFleet
}

// Chooses the edge for platform-hosted clients. Implements
// connect.PlatformPlacement. See the package header.
type EdgePlacement struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *EdgePlacementSettings
	store    placementStore
	random   placementRandom
	now      func() time.Time

	snapshot atomic.Pointer[placementSnapshot]

	// owned by the loop: the last good inventory and when it is next reloaded
	inventory         *server.ServicesLbInventory
	nextInventoryTime time.Time

	stateLock sync.Mutex
	// held address to the end of its hold
	heldIps map[netip.Addr]time.Time
}

var _ connect.PlatformPlacement = (*EdgePlacement)(nil)

// Reads the model once before it returns, within LoadReadTimeout, so the
// first clients are placed, then every LoadRefreshInterval until ctx ends or
// Close.
func NewEdgePlacement(ctx context.Context, settings *EdgePlacementSettings) *EdgePlacement {
	placement := newEdgePlacementWithStore(ctx, settings, modelPlacementStore{}, globalPlacementRandom{}, time.Now)
	placement.tick()
	go server.HandleError(placement.run)
	return placement
}

// The placement over an explicit store, random source and clock, without its
// loop. Tests drive its reads directly.
func newEdgePlacementWithStore(
	ctx context.Context,
	settings *EdgePlacementSettings,
	store placementStore,
	random placementRandom,
	now func() time.Time,
) *EdgePlacement {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &EdgePlacement{
		ctx:      cancelCtx,
		cancel:   cancel,
		settings: settings,
		store:    store,
		random:   random,
		now:      now,
		heldIps:  map[netip.Addr]time.Time{},
	}
}

// Stops reading the model. The last snapshot keeps answering until it ages.
func (self *EdgePlacement) Close() {
	self.cancel()
}

// Reads the model every LoadRefreshInterval after the constructor's first
// read. A non-positive interval keeps the first read.
func (self *EdgePlacement) run() {
	defer self.cancel()
	if self.settings.LoadRefreshInterval <= 0 {
		return
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.LoadRefreshInterval):
		}
		self.tick()
	}
}

// One pass of the loop: reloads the inventory when it is due, keeping the last
// good one while a reload fails and retrying it each pass, then reads the
// model. Nothing is read before an inventory loads. Called only by the loop.
func (self *EdgePlacement) tick() {
	now := self.now()
	if self.inventory == nil || !now.Before(self.nextInventoryTime) {
		if loaded, err := self.store.inventory(); err == nil {
			self.inventory = loaded
			self.nextInventoryTime = now.Add(self.settings.InventoryRefreshInterval)
		} else {
			placementReadsCounter.WithLabelValues(placementReadInventoryFailed).Inc()
			if glog.V(1) {
				glog.Infof("[placement]inventory load failed; keeping the last (%s)\n", err)
			}
		}
	}
	if self.inventory != nil {
		self.refresh(self.inventory)
	}
}

// Reads the control and the load table once and replaces the snapshot. A
// failed read keeps the last snapshot, which ages out after ModelMaxAge.
func (self *EdgePlacement) refresh(inventory *server.ServicesLbInventory) {
	readCtx, readCancel := context.WithTimeout(self.ctx, self.settings.LoadReadTimeout)
	defer readCancel()
	control, err := self.store.control(readCtx, model.ConnectMoveControlCeiling{
		EnablePlacement: self.settings.EnablePlacement,
		Tolerance:       self.settings.Tolerance,
	})
	if err != nil {
		placementReadsCounter.WithLabelValues(placementReadControlFailed).Inc()
		if glog.V(1) {
			glog.Infof("[placement]control read failed (%s)\n", err)
		}
		return
	}
	loads, nodeTime, err := self.store.loads(readCtx)
	if err != nil {
		placementReadsCounter.WithLabelValues(placementReadLoadFailed).Inc()
		if glog.V(1) {
			glog.Infof("[placement]load read failed (%s)\n", err)
		}
		return
	}
	fleet := computePlacementFleet(
		loads,
		nodeTime,
		self.settings.LoadStaleTimeout,
		inventory,
		control.EvacuateHosts,
		self.settings.Tolerance,
	)
	platformHost := ""
	if inventory.Domain != "" {
		platformHost = "connect." + strings.ToLower(inventory.Domain)
	}
	self.snapshot.Store(&placementSnapshot{
		readTime:     self.now(),
		placement:    control.Placement,
		platformHost: platformHost,
		fleet:        fleet,
	})
	placementReadsCounter.WithLabelValues(placementReadOk).Inc()
	placementHostsGauge.Set(float64(len(fleet.hosts)))
}

// Whether the address is held, as of now. Call with stateLock.
func (self *EdgePlacement) heldWithLock(address netip.Addr, now time.Time) bool {
	endTime, ok := self.heldIps[address]
	if !ok {
		return false
	}
	if !now.Before(endTime) {
		delete(self.heldIps, address)
		return false
	}
	return true
}

// The fresh snapshot that places for `host`, or why there is none.
func (self *EdgePlacement) placingSnapshot(host string, now time.Time) (*placementSnapshot, string) {
	snapshot := self.snapshot.Load()
	if snapshot == nil {
		return nil, placementMissModel
	}
	if 0 < self.settings.ModelMaxAge && self.settings.ModelMaxAge < now.Sub(snapshot.readTime) {
		return nil, placementMissModel
	}
	if !snapshot.placement {
		return nil, placementMissOff
	}
	normalizedHost := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if snapshot.platformHost == "" || normalizedHost != snapshot.platformHost {
		return nil, placementMissHost
	}
	return snapshot, ""
}

// connect.PlatformPlacement: one host's addresses for an arrival, at most one
// per family, or none.
func (self *EdgePlacement) PlatformPlacementTargets(host string) []netip.Addr {
	now := self.now()
	snapshot, miss := self.placingSnapshot(host, now)
	if snapshot == nil {
		placementMissesCounter.WithLabelValues(miss).Inc()
		return nil
	}
	chosenHost, interfaces := func() (*placementHost, []placementInterface) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return choosePlacementHost(snapshot.fleet, self.settings.MaxCpu, func(address netip.Addr) bool {
			return self.heldWithLock(address, now)
		}, self.random)
	}()
	if chosenHost == nil {
		placementMissesCounter.WithLabelValues(placementMissNoHost).Inc()
		return nil
	}
	targetIps := choosePlacementAddresses(interfaces, self.random)
	if len(targetIps) == 0 {
		placementMissesCounter.WithLabelValues(placementMissNoHost).Inc()
		return nil
	}
	placementsCounter.WithLabelValues(chosenHost.host).Inc()
	return targetIps
}

// connect.PlatformPlacement: holds each address for FailHold.
func (self *EdgePlacement) PlatformPlacementFailed(host string, targetIps []netip.Addr) {
	now := self.now()
	snapshot := self.snapshot.Load()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.settings.FailHold <= 0 {
			return
		}
		for _, targetIp := range targetIps {
			self.heldIps[targetIp.Unmap()] = now.Add(self.settings.FailHold)
		}
	}()
	for _, targetIp := range targetIps {
		failedHost := placementUnknownHost
		if snapshot != nil {
			if addressHost, ok := snapshot.fleet.addressHosts[targetIp.Unmap()]; ok {
				failedHost = addressHost
			}
		}
		placementFailuresCounter.WithLabelValues(failedHost).Inc()
	}
}
