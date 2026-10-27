package connect

// The mover's view of an exchange (MOVE.md §5.4, §5.5): the residents it
// selects from, the counts its load record carries, the edge inventory, the
// move primitives it drives, the target breaker and the DB guard's pool
// reading. The connect service starts the mover and the load publisher beside
// its exchange (StartExchangeMover), and makes the Redis move control the
// drain's switch for drain moves (D14).
//
// Alt embeds an exchange for the clients that reach the platform through it,
// and starts neither: its hosts are not edges of the inventory, so they are
// never targets, and its clients are on alt by design.

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The inventory has not loaded yet, so the mover loads again at its next tick.
var errMoveInventoryNotLoaded = errors.New("the edge inventory is not loaded")

// What the mover needs from one exchange. Implements MoveResidents,
// MoveLoadSource, MoveInventoryLoader, MoveExecutor, MoveTargetBreaker and
// MoveDbWait.
type exchangeMoveDependencies struct {
	exchange *Exchange
	dbWait   *moveDbPoolWait
}

// The local residents, from the exchange's snapshot. Probes leave themselves
// out: they declare no move capability (qualityprobe/providertunnel).
func (self *exchangeMoveDependencies) MoveCandidates() []*MoveCandidate {
	residentCandidates := self.exchange.ResidentMoveCandidates()
	candidates := make([]*MoveCandidate, 0, len(residentCandidates))
	for _, residentCandidate := range residentCandidates {
		ipv4, ipv6 := residentCandidate.IpFamilies()
		candidates = append(candidates, &MoveCandidate{
			ClientId:          residentCandidate.ClientId,
			ResidentStartTime: residentCandidate.CreateTime,
			LastPayloadTime:   residentCandidate.ForwardActivityTime,
			MoveVersion:       int(residentCandidate.ResidentMoveVersion),
			DirectCarrier:     residentCandidate.DirectCarrier(),
			OpenStreamCount:   residentCandidate.StreamCount,
			Ipv4:              ipv4,
			Ipv6:              ipv6,
			ArrivedByMove:     residentCandidate.ArrivedByMove,
			MoveOutstanding:   residentCandidate.MoveOutstanding,
		})
	}
	return candidates
}

// The exchange's counts, and whether it drains.
func (self *exchangeMoveDependencies) MoveLoadCounts() MoveLoadCounts {
	residents, providerResidents, transports := self.exchange.ResidentMoveLoadCounts()
	return MoveLoadCounts{
		Residents:         residents,
		ProviderResidents: providerResidents,
		Transports:        transports,
		Draining:          self.exchange.IsDraining(),
	}
}

// The exchange's edge inventory, which reloads itself from the vault
// `services.yml` and already leaves out transparent interfaces. An inventory
// not loaded yet is an error, so the mover loads again at its next tick.
func (self *exchangeMoveDependencies) LoadMoveInventory(ctx context.Context) (map[string]MoveInventoryHost, error) {
	edgeInventory := self.exchange.EdgeInventory()
	if edgeInventory == nil {
		return nil, errMoveInventoryNotLoaded
	}
	edgeHosts := edgeInventory.Hosts()
	if edgeHosts == nil {
		return nil, errMoveInventoryNotLoaded
	}
	inventory := map[string]MoveInventoryHost{}
	for _, edgeHost := range edgeHosts {
		inventoryHost := MoveInventoryHost{
			MoveWeight: edgeHost.MoveWeight,
		}
		for _, edgeInterface := range edgeHost.Interfaces {
			inventoryHost.Interfaces = append(inventoryHost.Interfaces, MoveInventoryInterface{
				Name:      edgeInterface.Interface,
				Ipv4:      edgeInterface.Ipv4,
				Ipv6:      edgeInterface.Ipv6,
				DnsWeight: edgeInterface.DnsWeight,
			})
		}
		inventory[edgeHost.Host] = inventoryHost
	}
	return inventory, nil
}

// Sends the mover's move through the exchange's move primitive, with the
// mover's move id, which also keys the client's cooldown.
func (self *exchangeMoveDependencies) SendMove(ctx context.Context, request *MoveRequest) error {
	kind := model.ResidentMoveKindAdvisory
	if request.Kind == protocol.ResidentMoveKind_ResidentMoveKindCompulsory {
		kind = model.ResidentMoveKindCompulsory
	}
	_, err := self.exchange.SendResidentMove(ctx, ResidentMove{
		MoveId:      request.MoveId,
		ClientId:    request.ClientId,
		Kind:        kind,
		TargetHost:  request.TargetHost,
		Targets:     request.Targets,
		MigrateTime: request.MigrateTime,
	})
	return err
}

// The exchange's target breaker.
func (self *exchangeMoveDependencies) TargetBreakerOpen(ctx context.Context, address netip.Addr) bool {
	return self.exchange.TargetBreakerOpen(ctx, address)
}

// The mean acquire wait of this process's main Postgres pool since the last
// tick.
func (self *exchangeMoveDependencies) PoolAcquireWait() time.Duration {
	return self.dbWait.PoolAcquireWait()
}

// Starts the mover and the load publisher beside the connect service's
// exchange, hears its arrivals for arrival-time moves (D9), and makes the
// Redis move control the drain's switch for drain moves (D14). A drain that
// cannot read the control keeps the code switches, since its clients must
// still leave.
//
// The mover and the publisher start once the edge inventory's first load
// ends, so the process keys its load record by its inventory host name
// (Exchange.MoveHost) from its first publish and the table joins the
// inventory. The returned stop ends both, and the publisher removes its
// record as it ends. The connect service stops them only after its drain, so
// through the drain the fleet reads this process as draining.
func StartExchangeMover(ctx context.Context, exchange *Exchange) func() {
	return startExchangeMover(ctx, exchange, newProcMoveCpuSampler())
}

// StartExchangeMover with the publisher's CPU sampler. Tests fix the sampler,
// since a host without /proc/stat measures no host CPU and is never a target.
func startExchangeMover(ctx context.Context, exchange *Exchange, sampler moveCpuSampler) func() {
	settings := exchange.settings
	moverCtx, moverCancel := context.WithCancel(ctx)
	dependencies := &exchangeMoveDependencies{
		exchange: exchange,
		dbWait:   newMoveDbPoolWait(),
	}
	exchange.SetDrainMovesAllowed(func(ctx context.Context) bool {
		control, err := model.GetConnectMoveControl(ctx, moveControlCeiling(settings))
		if err != nil {
			if glog.V(1) {
				glog.Infof("[move]drain control read failed; the code switches decide (%s)\n", err)
			}
			return true
		}
		return control.DrainMoves
	})
	go server.HandleError(func() {
		if edgeInventory := exchange.EdgeInventory(); edgeInventory != nil {
			select {
			case <-moverCtx.Done():
				return
			case <-edgeInventory.Loaded():
			}
		}
		identity := MoveProcessIdentity{
			Host:       exchange.MoveHost(),
			Block:      exchange.block,
			ExchangeId: exchange.ExchangeId(),
		}
		startMoveLoadPublisher(newMoveLoadPublisherWithStore(
			moverCtx,
			identity,
			settings,
			dependencies,
			sampler,
			moveModelStore{},
		))
		mover := NewMover(moverCtx, identity, settings, &MoverDependencies{
			Residents:     dependencies,
			LoadSource:    dependencies,
			Inventory:     dependencies,
			Executor:      dependencies,
			TargetBreaker: dependencies,
			DbWait:        dependencies,
		})
		select {
		case <-moverCtx.Done():
			return
		default:
			exchange.SetResidentArrivalListener(mover.OnResidentArrival)
		}
	})
	return func() {
		exchange.SetResidentArrivalListener(nil)
		moverCancel()
	}
}
