// Server-directed connection moves: the primitives of the source and the target
// exchange (connect/MOVE.md §5.1, §5.3, §5.6, §5.7). The mover (MOVE.md §5.5)
// decides balance moves and calls SendResidentMove, reads the residents through
// ResidentMoveCandidates and the target breaker through TargetBreakerOpen, and
// hears of new residents through SetResidentArrivalListener; Drain sends its
// own moves. The mover owns the sent and capable-resident metrics.
//
// Source. A move writes the client's move intent and the drain excuse markers
// before the `ResidentMigrate` frame is sent, so the arrival can take over the
// resident and the reconnect is excused (D16). A client that declared no move
// version gets only the legacy migrate (`migrate_time`), never an advisory move.
// While a move is outstanding the source watches it:
//   - A balance move (advisory or compulsory) re-reads the client's resident
//     record every MoveIntentPollInterval, and again whenever one of the
//     resident's transports closes, until the intent ends. It retires the
//     resident as soon as the record names another resident (Q7).
//   - A compulsory move has a deadline, `migrate_time` + MoveCompulsoryTimeout
//     (D12). If the record still names this resident then, the client's
//     transports on this process close with the moved close
//     (transport_moved_close.go) and the resident retires, so a transport of
//     the client elsewhere nominates afresh.
//   - Drain moves are compulsory moves to this host (D13) that any other
//     exchange takes over. Drains do not poll: the drain enforces each deadline
//     with one read of the record.
//
// A teardown caused by a move skips the network peer disconnect marker, like a
// drained one (§5.7).
//
// Target. A client transport that finds its resident record naming a resident
// elsewhere reads the intent before it forwards. A matching intent makes this
// exchange nominate a local resident in place of the moved one, through the
// compare-and-set nomination, and take the intent. Without one the transport
// forwards exactly as before.
package connect

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The lowest declared `resident_move_version` that honors move targets,
// reports results and prefers the target for reconnects (MOVE.md D2). A
// protocol fact, not a tunable.
const residentMoveTargetVersion uint32 = 1

// The H1 connect handshake header that declares the client's move version.
const HeaderResidentMoveVersion = "X-UR-ResidentMoveVersion"

// The edge carrier a client transport arrived on. The exchange header carries
// it to the resident, wherever the resident lives, so the mover can skip
// clients on a DNS-carried transport (MOVE.md §5.5).
type ResidentCarrier uint8

const (
	// from a peer exchange that predates carriers, or from an external front
	ResidentCarrierUnknown ResidentCarrier = 0
	ResidentCarrierH1      ResidentCarrier = 1
	ResidentCarrierH3      ResidentCarrier = 2
	ResidentCarrierH3Dns   ResidentCarrier = 3
)

// The carrier of a QUIC listener transport (connectListenerTransportH3 or
// connectListenerTransportH3Dns).
func residentCarrierForQuicListener(transport string) ResidentCarrier {
	switch transport {
	case connectListenerTransportH3:
		return ResidentCarrierH3
	case connectListenerTransportH3Dns:
		return ResidentCarrierH3Dns
	default:
		return ResidentCarrierUnknown
	}
}

// What an edge connection declared and how it arrived. The exchange header
// carries it to the resident; a peer that predates it sends the zero value.
type ResidentTransportEdge struct {
	// the highest `ResidentMigrate` move version the connection declared, from
	// the H1 header or the Auth `resident_move_version` (MOVE.md D2)
	ResidentMoveVersion uint32
	Carrier             ResidentCarrier
	// the family the client address was observed on: 4, 6, or 0 unknown
	IpFamily uint8
	// the connection declared provide intent (the H1 `X-UR-Provide-Intent`
	// header or the Auth `provide_intent`), so the load table can count
	// provider residents (MOVE.md §5.4)
	ProvideIntent bool
}

// The edge's family field of an observed ip version (4 or 6; else unknown).
func residentTransportIpFamily(ipVersion int) uint8 {
	switch ipVersion {
	case 4, 6:
		return uint8(ipVersion)
	default:
		return 0
	}
}

// Parses the H1 move version header. Anything but a decimal version is no
// declaration.
func residentMoveVersionFromHeader(value string) uint32 {
	version, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(version)
}

var (
	ErrResidentMoveDisabled    = errors.New("resident moves are disabled")
	ErrResidentMoveDraining    = errors.New("the exchange is draining")
	ErrResidentMoveInvalid     = errors.New("invalid resident move")
	ErrResidentMoveNoResident  = errors.New("no local resident for the client")
	ErrResidentMoveNotCapable  = errors.New("the client does not take advisory moves")
	ErrResidentMoveOutstanding = errors.New("a move of the resident is outstanding")
	ErrResidentMoveIntent      = errors.New("the move intent was not written")
	ErrResidentMoveNotSent     = errors.New("the move was not sent")
)

// The mover counts the moves it sends (moves_sent_total); this counts a
// drain's, by whether the client took targets.
var drainMovesSentCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "drain_moves_sent_total",
		Help:      "Compulsory moves a drain sent with intents, by whether the client takes move targets (capable) or got the legacy migrate",
	},
	[]string{"capable"},
)

var residentMovesArrivedCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "moves_arrived_total",
		Help:      "Residents taken over by move intent at the exchange the client arrived on",
	},
	[]string{"kind", "target_host"},
)

var residentMovesExpiredCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "moves_expired_total",
		Help:      "Moves whose resident was not taken over within the move's window, which covers declines and failures",
	},
	[]string{"kind"},
)

var residentMovesCompulsoryClosedCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "moves_compulsory_closed_total",
		Help:      "Clients closed with the moved close at a compulsory move's deadline because the resident was still here",
	},
)

// A target ip's breaker as the mover last read it: 1 open, 0 closed. Labeled
// by the inventory host (`target_host`, as the move counters name it),
// interface and family the ip belongs to. A label named `host` would collide
// with the stats pusher's grouping label, which refuses every later push of
// the process.
var residentMoveTargetBreakerOpenGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "move_target_breaker_open",
		Help:      "Whether the move target breaker of an inventory interface address was open at its last read",
	},
	[]string{"target_host", "interface", "family"},
)

func init() {
	prometheus.MustRegister(
		drainMovesSentCounter,
		residentMovesArrivedCounter,
		residentMovesExpiredCounter,
		residentMovesCompulsoryClosedCounter,
		residentMoveTargetBreakerOpenGauge,
	)
	for _, kind := range []model.ResidentMoveKind{
		model.ResidentMoveKindDrain,
		model.ResidentMoveKindAdvisory,
		model.ResidentMoveKindCompulsory,
	} {
		residentMovesExpiredCounter.WithLabelValues(string(kind)).Add(0)
	}
	for _, capable := range []string{"true", "false"} {
		drainMovesSentCounter.WithLabelValues(capable).Add(0)
	}
}

// One move a caller asks of a local resident.
type ResidentMove struct {
	// The move id the intent, the frame and the client's report carry. Zero
	// mints one; the mover passes the id it keyed the client's cooldown with.
	MoveId   server.Id
	ClientId server.Id
	// The resident generation the caller chose. Zero moves the client's
	// current local resident.
	ResidentId server.Id
	// advisory or compulsory; Drain sends its own moves
	Kind model.ResidentMoveKind
	// The inventory host the targets belong to (EdgeInventory).
	TargetHost string
	// At most one address per family, each an address of TargetHost's
	// inventory interfaces (MOVE.md §5.10).
	Targets []netip.Addr
	// When the client starts the move. Zero is now.
	MigrateTime time.Time
}

// One outstanding move of a resident generation, from its send until the
// resident is replaced or retired, or the move's windows end. Immutable after
// creation; the channels carry the events of its watcher.
type residentMove struct {
	moveId      server.Id
	kind        model.ResidentMoveKind
	targetHost  string
	targets     []netip.Addr
	migrateTime time.Time
	// the deadline of a compulsory or drain move; zero for an advisory move
	closeTime time.Time
	// the intent ends here, and with it the poll: MoveArrivalTimeout after the
	// migrate time, or after the send when the migrate time is past
	expireTime time.Time

	// a transport of the resident closed: re-read the resident record now
	transportClosed chan struct{}
	// closed when a drain move replaced this move
	superseded     chan struct{}
	supersededOnce sync.Once
}

// A move of kind; a zero moveId mints one.
func (self *Exchange) newResidentMove(
	moveId server.Id,
	kind model.ResidentMoveKind,
	targetHost string,
	targets []netip.Addr,
	migrateTime time.Time,
) *residentMove {
	if moveId == (server.Id{}) {
		moveId = server.NewId()
	}
	move := &residentMove{
		moveId:          moveId,
		kind:            kind,
		targetHost:      targetHost,
		targets:         slices.Clone(targets),
		migrateTime:     migrateTime,
		expireTime:      residentMoveExpireTime(time.Now(), migrateTime, self.settings.MoveArrivalTimeout),
		transportClosed: make(chan struct{}, 1),
		superseded:      make(chan struct{}),
	}
	if kind != model.ResidentMoveKindAdvisory {
		move.closeTime = migrateTime.Add(self.settings.MoveCompulsoryTimeout)
	}
	return move
}

// When a move's arrival window ends: arrivalTimeout after its migrate time,
// or after now when the migrate time is past. A client that starts its move at
// a drain's latest jittered migrate time gets the same window as one that
// starts at once.
func residentMoveExpireTime(now time.Time, migrateTime time.Time, arrivalTimeout time.Duration) time.Time {
	start := now
	if migrateTime.After(now) {
		start = migrateTime
	}
	return start.Add(arrivalTimeout)
}

// Wakes the watcher to re-read the resident record. Does not block.
func (self *residentMove) notifyTransportClosed() {
	select {
	case self.transportClosed <- struct{}{}:
	default:
	}
}

func (self *residentMove) supersede() {
	self.supersededOnce.Do(func() {
		close(self.superseded)
	})
}

// The protocol kind the client sees. A drain move is compulsory.
func (self *residentMove) protocolKind() protocol.ResidentMoveKind {
	if self.kind == model.ResidentMoveKindAdvisory {
		return protocol.ResidentMoveKind_ResidentMoveKindAdvisory
	}
	return protocol.ResidentMoveKind_ResidentMoveKindCompulsory
}

// Identifies this exchange instance in move intents. A drain move matches any
// exchange but this one.
func (self *Exchange) ExchangeId() server.Id {
	return self.exchangeId
}

// The edge inventory moves target; nil while moves are disabled.
func (self *Exchange) EdgeInventory() *EdgeInventory {
	return self.edgeInventory
}

// The client's local resident, or nil.
func (self *Exchange) localResident(clientId server.Id) *Resident {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.residents[clientId]
}

// Whether residentId is the client's local resident.
func (self *Exchange) isLocalResident(clientId server.Id, residentId server.Id) bool {
	resident := self.localResident(clientId)
	return resident != nil && resident.residentId == residentId
}

// Whether host names this process's host, by WARP_HOST or by the inventory
// host matched to it.
func (self *Exchange) isLocalMoveHost(host string) bool {
	if host == "" {
		return false
	}
	if host == self.host {
		return true
	}
	if self.edgeInventory != nil {
		if localHost, ok := self.edgeInventory.LocalHost(); ok && localHost.Host == host {
			return true
		}
	}
	return false
}

// The name of this process's host in the edge inventory, which intents and
// arrival counts use, or WARP_HOST when the inventory has no match. The load
// table should key the host by it so the table joins the inventory.
func (self *Exchange) MoveHost() string {
	return self.localMoveHost()
}

// The inventory name of this process's host for intents, WARP_HOST when the
// inventory has no match.
func (self *Exchange) localMoveHost() string {
	if self.edgeInventory != nil {
		if localHost, ok := self.edgeInventory.LocalHost(); ok {
			return localHost.Host
		}
	}
	return self.host
}

// Checks the caller's move kind, and its targets against the inventory.
func (self *Exchange) validateResidentMove(move ResidentMove) error {
	switch move.Kind {
	case model.ResidentMoveKindAdvisory, model.ResidentMoveKindCompulsory:
	default:
		return fmt.Errorf("%w: kind %q", ErrResidentMoveInvalid, move.Kind)
	}
	if self.edgeInventory == nil {
		return fmt.Errorf("%w: no inventory", ErrResidentMoveInvalid)
	}
	host, ok := self.edgeInventory.Host(move.TargetHost)
	if !ok {
		return fmt.Errorf("%w: target host %q is not in the inventory", ErrResidentMoveInvalid, move.TargetHost)
	}
	if len(move.Targets) == 0 {
		return fmt.Errorf("%w: no targets", ErrResidentMoveInvalid)
	}
	ipv4s, ipv6s := host.Addrs()
	families := map[bool]bool{}
	for _, target := range move.Targets {
		target = target.Unmap()
		if !target.IsGlobalUnicast() {
			return fmt.Errorf("%w: target %s", ErrResidentMoveInvalid, target)
		}
		if families[target.Is4()] {
			return fmt.Errorf("%w: two targets of one family", ErrResidentMoveInvalid)
		}
		families[target.Is4()] = true
		if !slices.Contains(ipv4s, target) && !slices.Contains(ipv6s, target) {
			return fmt.Errorf("%w: target %s is not an address of %s", ErrResidentMoveInvalid, target, move.TargetHost)
		}
	}
	return nil
}

// SendResidentMove asks the client of a local resident to reconnect to the
// move's targets (MOVE.md §5.1). The contract the mover relies on:
//   - The move is refused, with nothing written or sent, when moves are
//     disabled, the exchange drains, the move is invalid (its kind, or a target
//     outside the target host's inventory addresses), the client has
//     no local resident (or not the chosen generation), an advisory move
//     meets a client without a move version, or a move of the resident is
//     already outstanding.
//   - Otherwise the move intent is written, to live MoveArrivalTimeout past
//     the migrate time, then the drain excuse markers, then the frame is
//     sent. A client that declared no move version receives only
//     `migrate_time` (a compulsory move to such a client is the legacy
//     migrate plus the deadline). When the send fails the intent
//     is removed and ErrResidentMoveNotSent returned.
//   - On success the source watches the move as the file header describes and
//     the target breaker counts one send per target.
//
// Returns the move id the intent and the client's report carry, within ctx's
// deadline: the frame send waits at most DrainMigrateSendTimeout. Safe for
// concurrent use; one move per resident is outstanding at a time.
func (self *Exchange) SendResidentMove(ctx context.Context, move ResidentMove) (server.Id, error) {
	if !self.settings.EnableMove {
		return server.Id{}, ErrResidentMoveDisabled
	}
	if self.draining.Load() {
		return server.Id{}, ErrResidentMoveDraining
	}
	if err := self.validateResidentMove(move); err != nil {
		return server.Id{}, err
	}
	resident := self.localResident(move.ClientId)
	if resident == nil || resident.IsDone() || (move.ResidentId != (server.Id{}) && resident.residentId != move.ResidentId) {
		return server.Id{}, ErrResidentMoveNoResident
	}
	capable := residentMoveTargetVersion <= resident.moveVersion.Load()
	if move.Kind == model.ResidentMoveKindAdvisory && !capable {
		return server.Id{}, ErrResidentMoveNotCapable
	}
	migrateTime := move.MigrateTime
	if migrateTime.IsZero() {
		migrateTime = time.Now()
	}
	residentMove := self.newResidentMove(move.MoveId, move.Kind, move.TargetHost, move.Targets, migrateTime)
	if !resident.move.CompareAndSwap(nil, residentMove) {
		return server.Id{}, ErrResidentMoveOutstanding
	}
	sendTimeout := self.settings.DrainMigrateSendTimeout
	if deadline, ok := ctx.Deadline(); ok {
		// a negative send timeout waits without bound
		sendTimeout = max(0, min(sendTimeout, time.Until(deadline)))
	}
	if err := self.sendResidentMoveFrame(ctx, resident, residentMove, capable, true, sendTimeout); err != nil {
		resident.move.CompareAndSwap(residentMove, nil)
		return server.Id{}, err
	}
	if capable {
		for _, target := range residentMove.targets {
			// best effort: a lost count only weakens the breaker's sample
			if err := model.RecordResidentMoveTargetSend(ctx, target, self.settings.MoveTargetBreakerWindow); err != nil && glog.V(1) {
				glog.Infof("[move]breaker send %s = %s\n", target, err)
			}
		}
	}
	go server.HandleError(func() {
		self.watchResidentMove(resident, residentMove)
	})
	return residentMove.moveId, nil
}

// Writes the intent (and, unless the caller wrote them, the excuse markers),
// then sends the frame. Removes the intent when the frame is not sent.
func (self *Exchange) sendResidentMoveFrame(
	ctx context.Context,
	resident *Resident,
	move *residentMove,
	capable bool,
	writeExcuse bool,
	sendTimeout time.Duration,
) error {
	intent := &model.ResidentMoveIntent{
		MoveId:         move.moveId,
		Kind:           move.kind,
		FromResidentId: resident.residentId,
		FromExchangeId: self.exchangeId,
		TargetHost:     move.targetHost,
	}
	if capable {
		intent.Targets = move.targets
	}
	// the intent lives until the move's window ends, so a client whose
	// migrate time a drain jittered late still arrives within it
	if err := model.SetResidentMoveIntent(ctx, resident.clientId, intent, time.Until(move.expireTime)); err != nil {
		return fmt.Errorf("%w: %w", ErrResidentMoveIntent, err)
	}
	if writeExcuse && self.settings.EnableDrainExcuse {
		model.SetDrainExcuse(ctx, resident.clientId, resident.residentId, self.settings.DrainExcuseTtl)
		drainExcusesWrittenCounter.Inc()
	}

	residentMigrate := &protocol.ResidentMigrate{
		MigrateTime: uint64(move.migrateTime.UnixMilli()),
	}
	if capable {
		residentMigrate.MoveId = move.moveId.Bytes()
		residentMigrate.Kind = move.protocolKind()
		for _, target := range move.targets {
			residentMigrate.Targets = append(residentMigrate.Targets, &protocol.ResidentMoveTarget{
				Ip: target.Unmap().AsSlice(),
			})
		}
		if !move.closeTime.IsZero() {
			residentMigrate.CloseTime = uint64(move.closeTime.UnixMilli())
		}
		if preferDuration := self.settings.MovePreferDuration; 0 < preferDuration {
			residentMigrate.PreferDuration = uint64(preferDuration.Milliseconds())
		}
	}
	frame, err := connect.ToFrame(residentMigrate, connect.DefaultProtocolVersion)
	if err == nil && resident.client.SendWithTimeout(frame, connect.Id(resident.clientId), nil, sendTimeout) {
		if move.kind == model.ResidentMoveKindDrain {
			drainMovesSentCounter.WithLabelValues(fmt.Sprintf("%t", capable)).Inc()
		}
		return nil
	}
	if frame != nil && err == nil {
		// an unsent frame still belongs to the caller
		connect.MessagePoolReturn(frame.MessageBytes)
	}
	// no arrival is coming for this move
	if _, deleteErr := model.DeleteResidentMoveIntent(ctx, resident.clientId, move.moveId); deleteErr != nil && glog.V(1) {
		glog.Infof("[move]remove unsent intent %s = %s\n", resident.clientId, deleteErr)
	}
	return ErrResidentMoveNotSent
}

// Whether another resident replaced resident in its client's record.
func (self *Exchange) residentReplaced(resident *Resident) bool {
	current := model.GetResidentForClient(resident.ctx, resident.clientId, 0)
	return current != nil && current.ResidentId != resident.residentId
}

// Retires a resident its move replaced or closed. The teardown skips the
// network peer disconnect marker (MOVE.md §5.7).
func (self *Exchange) retireMovedResident(resident *Resident) {
	resident.moved.Store(true)
	resident.Cancel()
	if glog.V(1) {
		glog.Infof("[move]retire %s/%s\n", resident.clientId, resident.residentId)
	}
}

// Closes the client's transports on this process with the moved close.
// Returns how many it closed.
func (self *Exchange) closeClientTransportsMoved(clientId server.Id) int {
	closes := func() []func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		closes := []func(){}
		for connectionId, cancel := range self.connections[clientId] {
			if closeMoved := self.connectionMovedCloses[connectionId]; closeMoved != nil {
				closes = append(closes, closeMoved)
			} else {
				closes = append(closes, cancel)
			}
		}
		return closes
	}()
	for _, closeTransport := range closes {
		closeTransport()
	}
	return len(closes)
}

// Enforces a compulsory or drain move's deadline (MOVE.md §5.6). One read of
// the resident record decides: replaced, the resident only idles here and
// retires; still here, the client's transports on this process close with the
// moved close first, so none of them nominates here again, then the resident
// retires so a transport of the client elsewhere nominates afresh.
func (self *Exchange) enforceResidentMoveDeadline(resident *Resident, move *residentMove) {
	if resident.IsDone() || resident.move.Load() != move {
		return
	}
	if self.residentReplaced(resident) {
		self.retireMovedResident(resident)
		return
	}
	self.closeClientTransportsMoved(resident.clientId)
	residentMovesCompulsoryClosedCounter.Inc()
	residentMovesExpiredCounter.WithLabelValues(string(move.kind)).Inc()
	self.retireMovedResident(resident)
}

// Watches one outstanding balance move (advisory or compulsory) until the
// resident is replaced or ends, a drain supersedes the move, or the move's
// windows end. The poll runs while the intent lives; a compulsory move's
// deadline is enforced even after it.
func (self *Exchange) watchResidentMove(resident *Resident, move *residentMove) {
	defer resident.move.CompareAndSwap(move, nil)
	pollInterval := self.settings.MoveIntentPollInterval
	expired := false
	for {
		var pollAfter <-chan time.Time
		var transportClosed <-chan struct{}
		var expireAfter <-chan time.Time
		if !expired {
			remaining := time.Until(move.expireTime)
			if remaining <= 0 {
				expired = true
			} else {
				expireAfter = time.After(remaining)
				transportClosed = move.transportClosed
				if 0 < pollInterval {
					pollAfter = time.After(min(pollInterval, remaining))
				}
			}
		}
		var closeAfter <-chan time.Time
		if !move.closeTime.IsZero() {
			closeAfter = time.After(time.Until(move.closeTime))
		}
		if expired && closeAfter == nil {
			// the window ended with the resident still here
			if self.residentReplaced(resident) {
				self.retireMovedResident(resident)
			} else {
				residentMovesExpiredCounter.WithLabelValues(string(move.kind)).Inc()
			}
			return
		}

		select {
		case <-resident.Done():
			return
		case <-move.superseded:
			return
		case <-closeAfter:
			self.enforceResidentMoveDeadline(resident, move)
			return
		case <-expireAfter:
			expired = true
			continue
		case <-pollAfter:
		case <-transportClosed:
		}
		if self.residentReplaced(resident) {
			self.retireMovedResident(resident)
			return
		}
	}
}

// Takes over the client's resident by its move intent (MOVE.md §5.3). current
// is the client's resident record. A local resident replaces the moved one when
// the intent matches: a balance move names this host and the resident current
// names, and a drain move matches any exchange but its source. Returns whether
// a local resident replaced it; otherwise the caller forwards as before.
func (self *Exchange) takeOverMovedResident(
	ctx context.Context,
	clientId server.Id,
	instanceId server.Id,
	current *model.NetworkClientResident,
	provideIntent bool,
) bool {
	if !self.settings.EnableMove || current == nil || self.isLocalResident(clientId, current.ResidentId) {
		return false
	}
	intent, err := model.GetResidentMoveIntent(ctx, clientId)
	if err != nil || intent == nil || !self.matchesResidentMoveIntent(intent, current) {
		return false
	}
	nominated, _ := self.nominateLocalResident(ctx, clientId, instanceId, &intent.FromResidentId, residentNominationArgs{
		provideIntent: provideIntent,
		arrivedMoveId: intent.MoveId,
	})
	if !nominated {
		return false
	}
	// best effort: an intent left behind only ends with its ttl
	if _, err := model.TakeResidentMoveIntent(ctx, clientId); err != nil && glog.V(1) {
		glog.Infof("[move]take intent %s = %s\n", clientId, err)
	}
	residentMovesArrivedCounter.WithLabelValues(string(intent.Kind), self.localMoveHost()).Inc()
	if glog.V(1) {
		glog.Infof("[move]arrived %s %s replaced %s\n", clientId, intent.Kind, intent.FromResidentId)
	}
	return true
}

// Whether this exchange takes over current by the intent.
func (self *Exchange) matchesResidentMoveIntent(intent *model.ResidentMoveIntent, current *model.NetworkClientResident) bool {
	if intent.FromExchangeId == self.exchangeId || intent.FromResidentId != current.ResidentId {
		return false
	}
	switch intent.Kind {
	case model.ResidentMoveKindDrain:
		return true
	case model.ResidentMoveKindAdvisory, model.ResidentMoveKindCompulsory:
		return self.isLocalMoveHost(intent.TargetHost)
	default:
		return false
	}
}

// A local resident as the mover sees it (MOVE.md §5.5, D7).
type ResidentMoveCandidate struct {
	ClientId   server.Id
	ResidentId server.Id
	// when this exchange created the resident
	CreateTime time.Time
	// the highest move version any of the client's transports declared
	ResidentMoveVersion uint32
	// what each transport attached now declared and how it arrived, in no
	// order
	Transports []ResidentTransportEdge
	// the client's open stream hops, from the latest stream snapshot
	StreamCount int
	// the latest payload activity across the resident's forwards; zero when
	// it has none
	ForwardActivityTime time.Time
	// a move of the resident is outstanding
	MoveOutstanding bool
	// the resident took over by a move intent; the mover never moves such an
	// arrival again
	ArrivedByMove bool
}

// Whether the client honors move targets (an advisory move needs this).
func (self ResidentMoveCandidate) Capable() bool {
	return residentMoveTargetVersion <= self.ResidentMoveVersion
}

// Whether the client reaches this resident only over direct carriers (H1 or
// H3): at least one transport, none DNS-carried or of unknown carrier.
func (self ResidentMoveCandidate) DirectCarrier() bool {
	if len(self.Transports) == 0 {
		return false
	}
	for _, transport := range self.Transports {
		if transport.Carrier != ResidentCarrierH1 && transport.Carrier != ResidentCarrierH3 {
			return false
		}
	}
	return true
}

// The families the client's transports here were observed on. Neither is
// unknown.
func (self ResidentMoveCandidate) IpFamilies() (ipv4 bool, ipv6 bool) {
	for _, transport := range self.Transports {
		switch transport.IpFamily {
		case 4:
			ipv4 = true
		case 6:
			ipv6 = true
		}
	}
	return
}

// The local residents with what the mover selects on. A snapshot: a resident
// can change or end right after. Safe for concurrent use.
func (self *Exchange) ResidentMoveCandidates() []ResidentMoveCandidate {
	residents := func() []*Resident {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		residents := make([]*Resident, 0, len(self.residents))
		for _, resident := range self.residents {
			residents = append(residents, resident)
		}
		return residents
	}()
	candidates := make([]ResidentMoveCandidate, 0, len(residents))
	for _, resident := range residents {
		if resident.IsDone() {
			continue
		}
		candidates = append(candidates, resident.moveCandidate())
	}
	return candidates
}

func (self *Resident) moveCandidate() ResidentMoveCandidate {
	candidate := ResidentMoveCandidate{
		ClientId:            self.clientId,
		ResidentId:          self.residentId,
		CreateTime:          self.createTime,
		ResidentMoveVersion: self.moveVersion.Load(),
		StreamCount:         int(self.streamHopCount.Load()),
		MoveOutstanding:     self.move.Load() != nil,
		ArrivedByMove:       self.arrivedMoveId != (server.Id{}),
	}
	func() {
		self.stateLock.RLock()
		defer self.stateLock.RUnlock()
		for transport := range self.transports {
			candidate.Transports = append(candidate.Transports, transport.edge)
		}
		lastActivityNanos := int64(0)
		for _, forward := range self.forwards {
			lastActivityNanos = max(lastActivityNanos, forward.lastActivityNanos.Load())
		}
		if 0 < lastActivityNanos {
			candidate.ForwardActivityTime = time.Unix(0, lastActivityNanos)
		}
	}()
	return candidate
}

// The load of this process's residents, for the load table (MOVE.md §5.4):
// the local residents not done, those with at least one attached transport
// whose edge declared provide intent (each counted once), and the attached
// client transports of those residents. In memory and non-blocking: the
// resident list is taken under the exchange lock and each resident's
// transports under its own, never both at once. Safe for concurrent use.
func (self *Exchange) ResidentMoveLoadCounts() (residentCount int, providerResidentCount int, transportCount int) {
	residents := func() []*Resident {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		residents := make([]*Resident, 0, len(self.residents))
		for _, resident := range self.residents {
			residents = append(residents, resident)
		}
		return residents
	}()
	for _, resident := range residents {
		if resident.IsDone() {
			continue
		}
		residentCount += 1
		provider := false
		func() {
			resident.stateLock.RLock()
			defer resident.stateLock.RUnlock()
			transportCount += len(resident.transports)
			for transport := range resident.transports {
				if transport.edge.ProvideIntent {
					provider = true
					break
				}
			}
		}()
		if provider {
			providerResidentCount += 1
		}
	}
	return
}

// Records the highest move version any transport's edge declared over the
// resident's life.
func (self *Resident) observeTransportEdge(edge ResidentTransportEdge) {
	for {
		version := self.moveVersion.Load()
		if edge.ResidentMoveVersion <= version || self.moveVersion.CompareAndSwap(version, edge.ResidentMoveVersion) {
			return
		}
	}
}

// Whether moves to a target interface address are held off by its breaker
// (MOVE.md §5.5), under MoveTargetBreakerRatio, MoveTargetBreakerMinSends and
// MoveTargetBreakerTimeout. A read that cannot tell answers true, so moves
// avoid a target whose state is unknown. Sets the breaker gauge of an inventory
// address. Returns within ctx's deadline; safe for concurrent use.
func (self *Exchange) TargetBreakerOpen(ctx context.Context, address netip.Addr) bool {
	open, err := model.IsResidentMoveTargetBreakerOpen(ctx, address, model.ResidentMoveTargetBreakerSettings{
		Ratio:    self.settings.MoveTargetBreakerRatio,
		MinSends: self.settings.MoveTargetBreakerMinSends,
		Timeout:  self.settings.MoveTargetBreakerTimeout,
	})
	if err != nil {
		if glog.V(1) {
			glog.Infof("[move]breaker read %s = %s\n", address, err)
		}
		open = true
	}
	if self.edgeInventory != nil {
		address = address.Unmap()
		for _, host := range self.edgeInventory.Hosts() {
			for _, edgeInterface := range host.Interfaces {
				family := ""
				if edgeInterface.Ipv4 == address {
					family = "4"
				} else if edgeInterface.Ipv6 == address {
					family = "6"
				}
				if family != "" {
					value := float64(0)
					if open {
						value = 1
					}
					residentMoveTargetBreakerOpenGauge.WithLabelValues(host.Host, edgeInterface.Interface, family).Set(value)
				}
			}
		}
	}
	return open
}

// Installs the listener a new local resident is announced to, with whether it
// took over by a move intent (MOVE.md D9: such an arrival is never moved
// again). The mover installs its arrival hook here. It runs on the nominating
// transport's goroutine, outside every lock, and must not block. Nil removes it.
func (self *Exchange) SetResidentArrivalListener(listener func(clientId server.Id, byIntent bool)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.residentArrivalListener = listener
}

// Announces a published resident to the arrival listener.
func (self *Exchange) notifyResidentArrival(resident *Resident) {
	listener := func() func(server.Id, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.residentArrivalListener
	}()
	if listener != nil {
		listener(resident.clientId, resident.arrivedMoveId != (server.Id{}))
	}
}

// Installs the decision drains consult before sending moves, after the code
// switches allow them (EnableMove, EnableDrainMoves). The mover's Redis move
// control installs one so an operator can switch drain moves off without a
// deploy (MOVE.md D14). Nil, the default, allows. It runs on the draining
// goroutine with a context bounded by DrainMigrateSendTimeout; a raise allows.
func (self *Exchange) SetDrainMovesAllowed(allowed func(context.Context) bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.drainMovesAllowed = allowed
}

// Whether this drain sends moves (MOVE.md §5.6, D13).
func (self *Exchange) drainMovesEnabled() bool {
	if !self.settings.EnableMove || !self.settings.EnableDrainMoves {
		return false
	}
	allowed := func() func(context.Context) bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.drainMovesAllowed
	}()
	if allowed == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.DrainMigrateSendTimeout)
	defer cancel()
	return server.HandleError1(func() bool {
		return allowed(ctx)
	}, func() bool {
		return true
	})
}

// The targets of one drain move: one address per family of this host's
// inventory interfaces, weighted by dns_weight (D13). None when the inventory
// has no entry for this host, so the client migrates by name.
func (self *Exchange) drainMoveTargets() []netip.Addr {
	if self.edgeInventory == nil {
		return nil
	}
	localHost, ok := self.edgeInventory.LocalHost()
	if !ok {
		return nil
	}
	return localHost.PickTargets(rand.Int63n)
}

// Sends one drain move: the intent, then the frame (compulsory with this
// host's targets, or the legacy migrate). The drain wrote the excuse markers.
// The deadline is no later than the drain's own, which enforces it. The move
// replaces any outstanding balance move of the resident. Returns the move, or
// nil when none was sent; then sent reports whether the legacy migrate went
// instead.
func (self *Exchange) sendDrainResidentMove(
	resident *Resident,
	migrateTime time.Time,
	drainEndTime time.Time,
	targets []netip.Addr,
	sendTimeout time.Duration,
) (move *residentMove, sent bool) {
	move = self.newResidentMove(
		server.Id{},
		model.ResidentMoveKindDrain,
		self.localMoveHost(),
		targets,
		migrateTime,
	)
	if drainEndTime.Before(move.closeTime) {
		move.closeTime = drainEndTime
	}
	if previous := resident.move.Swap(move); previous != nil {
		previous.supersede()
	}
	capable := residentMoveTargetVersion <= resident.moveVersion.Load()
	ctx, cancel := context.WithTimeout(self.ctx, sendTimeout)
	defer cancel()
	if err := self.sendResidentMoveFrame(ctx, resident, move, capable, false, sendTimeout); err != nil {
		resident.move.CompareAndSwap(move, nil)
		if errors.Is(err, ErrResidentMoveIntent) {
			// without an intent the legacy migrate still moves the client;
			// the straggler sweep covers it
			return nil, self.sendResidentMigrate(resident, migrateTime, sendTimeout)
		}
		return nil, false
	}
	return move, true
}

// Broadcasts drain moves with migrate times jittered across the drain migrate
// window (broadcastResidentMigrates), then enforces each move's deadline in
// deadline order. Returns when every moved client has left this process or the
// last deadline passed, which is no later than the drain deadline; whatever
// remains falls to the straggler sweep. Drains do not poll the resident
// record.
func (self *Exchange) moveResidentsForDrain(residents []*Resident, drainEndTime time.Time) {
	type drainMove struct {
		resident *Resident
		move     *residentMove
	}
	var movesLock sync.Mutex
	moves := []drainMove{}
	attemptedCount, sentClientIds, _ := self.broadcastResidentMigrates(
		residents,
		drainEndTime,
		func(resident *Resident, migrateTime time.Time, sendTimeout time.Duration) bool {
			move, sent := self.sendDrainResidentMove(resident, migrateTime, drainEndTime, self.drainMoveTargets(), sendTimeout)
			if move != nil {
				func() {
					movesLock.Lock()
					defer movesLock.Unlock()
					moves = append(moves, drainMove{resident: resident, move: move})
				}()
			}
			return sent
		},
	)
	if len(sentClientIds) == 0 {
		return
	}
	glog.Infof(
		"[c]drain move broadcast to %d residents, %d with intents (%d/%d attempted)\n",
		len(sentClientIds),
		len(moves),
		attemptedCount,
		len(residents),
	)
	if len(moves) == 0 {
		return
	}

	slices.SortFunc(moves, func(a drainMove, b drainMove) int {
		return a.move.closeTime.Compare(b.move.closeTime)
	})
	nextIndex := 0
	for {
		now := time.Now()
		for nextIndex < len(moves) && !now.Before(moves[nextIndex].move.closeTime) {
			resident, move := moves[nextIndex].resident, moves[nextIndex].move
			server.HandleError(func() {
				self.enforceResidentMoveDeadline(resident, move)
			})
			resident.move.CompareAndSwap(move, nil)
			nextIndex += 1
		}
		remainingCount := func() int {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			n := 0
			for _, drainMove := range moves {
				clientId := drainMove.resident.clientId
				if self.residents[clientId] != nil || 0 < len(self.connections[clientId]) {
					n += 1
				}
			}
			return n
		}()
		drainResidentsRemainingGauge.Set(float64(remainingCount))
		if remainingCount == 0 || nextIndex == len(moves) || drainEndTime.Before(now) {
			return
		}
		timeout := time.Until(moves[nextIndex].move.closeTime)
		timeout = max(min(timeout, time.Second), time.Millisecond)
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(timeout):
		}
	}
}
