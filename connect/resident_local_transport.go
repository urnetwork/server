// In-process delivery of a transport whose resident lives in the same connect
// process (CPUOPT3 D1). ResidentTransport.Run resolves the client's resident
// record and, when it names this process's current resident and the switch
// allows this block, attaches the transport to that resident directly instead
// of dialing an exchange socket back into its own process. The rest of the
// loop is unchanged: the reconnect pacing, the nomination that replaces the
// old resident, and the client limit kick.
//
// An attach does what the accept path does after it reads a header, in the
// same order: it refuses a generation that is not current or a resident that
// is done, counts as an accepted exchange connection, publishes the
// resident's routes with the header's carrier properties and transport edge,
// and ends when the resident ends. Two pumps then stand in for the socket and
// its goroutines:
//   - the client pump moves the transport's send queue into the resident's
//     receive route, on the transport's Run goroutine;
//   - the resident pump moves the resident's send route into the transport's
//     receive queue, on one goroutine the link owns.
//
// Each pump keeps the contract of an exchange socket writer:
//   - it gathers up to ExchangeWriteBatchCount messages or
//     ExchangeWriteBatchByteCount bytes without blocking, and delivers them in
//     FIFO order;
//   - it checks the framer's limits for the whole batch before delivering any
//     of it. An oversized message ends the link (max_message_len) with the
//     batch returned, as a failed exchange write ends its connection;
//   - a zero-length message is a ping: it is returned and counts as activity;
//   - a batch has WriteTimeout from its start. A delivery that would block
//     waits on a timer made for the rest of that budget, and a batch that
//     cannot finish in it ends the link (write_timeout), as the exchange write
//     deadline ends a socket. A healthy batch keeps its whole budget;
//   - a delivered batch is resident activity.
//
// A socket's read deadline, pings and keepalive detect a dead peer process or
// path. Here both ends are contexts of this process: the resident or the
// transport ending ends the link at once. While a link is attached,
// Resident.CancelIfIdle treats the resident as active, standing in for the
// pings that kept an idle socket's resident from ExchangeResidentTtl.
//
// A detach mirrors closeExchangeTransportQueues: route removal joins every
// resident writer admitted to the old routes, then the link is canceled, both
// pumps are joined, and the messages left on the routes are returned. The
// transport's own queues outlive the link, as they outlive an exchange
// connection, so the next link takes the rest of the send queue in order. A
// pump gives each message it dequeued exactly one owner: a delivered message
// belongs to its destination, and the rest of a batch is returned to the pool
// and counted at the pump's receive queue boundary.
//
// The switch is the code ceiling EnableLocalTransport and the Redis move
// control's `localTransport` (model/connect_move_control.go), which chooses
// blocks and never widens past the ceiling. LocalTransportDefault applies while
// the control sets none, and ships off. Each exchange reads the control every
// LocalTransportControlRefreshInterval and keeps its last good read. When the
// control turns this block off, new attaches take TCP and live links detach,
// LocalTransportDisableDetachRate per second, each redialing its resident over
// the exchange at once.
//
// Safe for concurrent use.
package connect

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Frames that crossed a link instead of a socket, counted as the socket
// counted them, so exchange totals are exchange_io plus exchange_local. The
// labels are the closed sets of exchange_io; a link carries no handshake.
var exchangeLocalFramesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "exchange_local_frames_total",
		Help:      "Frames delivered in process by local transport links instead of an exchange socket, each counted once sent and once received as the socket counted it, partitioned by I/O direction and frame kind",
	},
	[]string{"direction", "kind"},
)

var exchangeLocalBytesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "exchange_local_bytes_total",
		Help:      "Bytes in frames delivered in process by local transport links, including the 4-byte frame header an exchange socket would have carried, partitioned by I/O direction and frame kind",
	},
	[]string{"direction", "kind"},
)

var exchangeLocalAttachmentsGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "exchange_local_attachments",
		Help:      "Transports attached in process to a resident of this process",
	},
)

var exchangeLocalDetachesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "exchange_local_detaches_total",
		Help:      "Local transport links detached, partitioned by reason",
	},
	[]string{"reason"},
)

var exchangeLocalBlockedSecondsHistogram = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "exchange_local_blocked_seconds",
		Help:      "Time a local transport delivery waited for queue capacity, observed only when a delivery would block",
		Buckets:   []float64{0.0001, 0.001, 0.01, 0.1, 1, 5, 10, 15},
	},
)

var exchangeLocalTransportEnabledGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "exchange_local_transport_enabled",
		Help:      "Whether this process's exchange attaches transports to its own residents in process: EnableLocalTransport as the Redis control last chose for its block, 1 on, 0 off",
	},
)

// Why a link detached, as the detaches metric labels it.
type localTransportDetachReason uint32

const (
	localTransportDetachNone localTransportDetachReason = iota
	localTransportDetachResidentDone
	localTransportDetachTransportDone
	localTransportDetachWriteTimeout
	localTransportDetachMaxMessageLen
	localTransportDetachDisabled
	// a pump raised; the batch it held is returned and counted as dropped
	localTransportDetachRaised
	localTransportDetachReasonCount
)

var localTransportDetachReasonLabels = [localTransportDetachReasonCount]string{
	"",
	"resident_done",
	"transport_done",
	"write_timeout",
	"max_message_len",
	"disabled",
	"raised",
}

// The direction of a pump: client to resident, or resident to client.
type localTransportDirection uint8

const (
	localTransportDirectionClient localTransportDirection = iota
	localTransportDirectionResident
)

// Pre-bound like exchange_io, for the same hot path.
var exchangeLocalIOCollectorsByLabel [exchangeIODirectionCount][exchangeIOFrameKindCount]exchangeIOCollectors
var exchangeLocalDetachesCounters [localTransportDetachReasonCount]prometheus.Counter

func init() {
	prometheus.MustRegister(exchangeLocalFramesCounter)
	prometheus.MustRegister(exchangeLocalBytesCounter)
	prometheus.MustRegister(exchangeLocalAttachmentsGauge)
	prometheus.MustRegister(exchangeLocalDetachesCounter)
	prometheus.MustRegister(exchangeLocalBlockedSecondsHistogram)
	prometheus.MustRegister(exchangeLocalTransportEnabledGauge)

	for direction := exchangeIODirection(0); direction < exchangeIODirectionCount; direction++ {
		for kind := exchangeIOFrameKind(0); kind < exchangeIOFrameKindCount; kind++ {
			exchangeLocalIOCollectorsByLabel[direction][kind] = exchangeIOCollectors{
				frames: exchangeLocalFramesCounter.WithLabelValues(
					exchangeIODirectionLabels[direction],
					exchangeIOFrameKindLabels[kind],
				),
				bytes: exchangeLocalBytesCounter.WithLabelValues(
					exchangeIODirectionLabels[direction],
					exchangeIOFrameKindLabels[kind],
				),
			}
		}
	}
	for reason := localTransportDetachResidentDone; reason < localTransportDetachReasonCount; reason++ {
		exchangeLocalDetachesCounters[reason] = exchangeLocalDetachesCounter.WithLabelValues(
			localTransportDetachReasonLabels[reason],
		)
	}
}

// A process runs one exchange, whose switch sets the gauge.
func setExchangeLocalTransportEnabledGauge(enabled bool) {
	if enabled {
		exchangeLocalTransportEnabledGauge.Set(1)
	} else {
		exchangeLocalTransportEnabledGauge.Set(0)
	}
}

// Counts delivered frames as an exchange socket counted them: each once sent
// and once received, with the 4-byte frame header it would have carried.
func recordLocalTransportFrames(dataFrameCount int, dataByteCount int, pingCount int) {
	if 0 < dataFrameCount {
		frames := float64(dataFrameCount)
		frameBytes := float64(dataFrameCount*exchangeIOFrameHeaderByteCount + dataByteCount)
		for direction := exchangeIODirection(0); direction < exchangeIODirectionCount; direction++ {
			collectors := &exchangeLocalIOCollectorsByLabel[direction][exchangeIOFrameKindData]
			collectors.frames.Add(frames)
			collectors.bytes.Add(frameBytes)
		}
	}
	if 0 < pingCount {
		frames := float64(pingCount)
		frameBytes := float64(pingCount * exchangeIOFrameHeaderByteCount)
		for direction := exchangeIODirection(0); direction < exchangeIODirectionCount; direction++ {
			collectors := &exchangeLocalIOCollectorsByLabel[direction][exchangeIOFrameKindPing]
			collectors.frames.Add(frames)
			collectors.bytes.Add(frameBytes)
		}
	}
}

// The switch and pacing of in-process delivery. Embedded in ExchangeSettings.
type ExchangeLocalTransportSettings struct {
	// The code ceiling. False: every transport dials its resident over an
	// exchange socket, and the control is not read. Off by default; the
	// connect service turns it on (run.go), and embedders such as alt keep it
	// off.
	EnableLocalTransport bool
	// The blocks that deliver in process while the Redis control sets no
	// `localTransport`, in the control's syntax: "off", "all", or blocks
	// separated by commas or spaces. Not a ceiling: an operator widens it with
	// the control, up to EnableLocalTransport. "off" ships the code dark.
	LocalTransportDefault string
	// How often the exchange reads the control's `localTransport`, so a
	// change reaches each process within about this long. Non-positive reads
	// it once, at start.
	LocalTransportControlRefreshInterval time.Duration
	// Bounds one read of the control.
	LocalTransportControlReadTimeout time.Duration
	// Live links detached per second after the control turns this block off.
	// Each then redials its resident over the exchange, so this paces that
	// wave of dials and handshakes. Non-positive detaches none: the change
	// then reaches only new attaches.
	LocalTransportDisableDetachRate int
}

func DefaultExchangeLocalTransportSettings() *ExchangeLocalTransportSettings {
	return &ExchangeLocalTransportSettings{
		EnableLocalTransport:                 false,
		LocalTransportDefault:                model.ConnectLocalTransportOff,
		LocalTransportControlRefreshInterval: 10 * time.Second,
		LocalTransportControlReadTimeout:     5 * time.Second,
		LocalTransportDisableDetachRate:      500,
	}
}

// The in-process delivery state of one exchange: the switch as the control
// last chose it for this block, and the live links a narrowing detaches.
type exchangeLocalTransport struct {
	ctx      context.Context
	settings *ExchangeSettings
	block    string
	read     func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error)

	// whether this block attaches, as of the last good read of the control,
	// and before the first as the code default chooses
	allowed atomic.Bool

	stateLock sync.Mutex
	// live links. An attach adds its link while the switch allows, under
	// stateLock, so the pacer the switch's turn off starts sees it.
	links map[*localTransportLink]struct{}
	// a pacer is detaching links. The switch's turn and the pacer's end are
	// decided under stateLock, so a turn off after a pacer ends starts another.
	pacing bool

	// workerLock closes admission before Exchange.WaitForIdle joins the
	// control loop, a pacer, and every attached link's owner until its detach
	// completes.
	workerLock    sync.Mutex
	workersClosed bool
	workers       sync.WaitGroup

	// owned by the control loop: the last read failed. A failure is logged
	// once until a read succeeds.
	failing bool

	// Nil in production; tests observe each pacer start.
	afterPacerStartForTest func()
}

// The exchange's in-process delivery, which reads the control until ctx ends.
// Nil while EnableLocalTransport is off: nothing is read and every transport
// dials.
func newExchangeLocalTransport(ctx context.Context, settings *ExchangeSettings, block string) *exchangeLocalTransport {
	if !settings.EnableLocalTransport {
		return nil
	}
	localTransport := newExchangeLocalTransportWithStore(ctx, settings, block, model.GetConnectMoveControl)
	localTransport.start()
	return localTransport
}

// The state over an explicit store, without its control loop. Tests drive
// refresh directly, or start the loop.
func newExchangeLocalTransportWithStore(
	ctx context.Context,
	settings *ExchangeSettings,
	block string,
	read func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error),
) *exchangeLocalTransport {
	localTransport := &exchangeLocalTransport{
		ctx:      ctx,
		settings: settings,
		block:    block,
		read:     read,
		links:    map[*localTransportLink]struct{}{},
	}
	// until the first read, the code default chooses
	allowed := model.NarrowConnectMoveControl(nil, localTransport.ceiling()).LocalTransportAllows(block)
	localTransport.allowed.Store(allowed)
	setExchangeLocalTransportEnabledGauge(allowed)
	return localTransport
}

// The code settings the control narrows.
func (self *exchangeLocalTransport) ceiling() model.ConnectMoveControlCeiling {
	return model.ConnectMoveControlCeiling{
		EnableLocalTransport:  self.settings.EnableLocalTransport,
		LocalTransportDefault: self.settings.LocalTransportDefault,
	}
}

// Whether this block attaches new links, as of the last good read.
func (self *exchangeLocalTransport) allows() bool {
	return self.allowed.Load()
}

// Admits one worker for Exchange.WaitForIdle unless the exchange has closed.
func (self *exchangeLocalTransport) beginWorker() bool {
	self.workerLock.Lock()
	defer self.workerLock.Unlock()
	if self.workersClosed {
		return false
	}
	self.workers.Add(1)
	return true
}

// Releases one worker admitted by beginWorker.
func (self *exchangeLocalTransport) endWorker() {
	self.workers.Done()
}

// Closes worker admission, so Exchange.WaitForIdle's join is final.
func (self *exchangeLocalTransport) closeWorkers() {
	self.workerLock.Lock()
	defer self.workerLock.Unlock()
	self.workersClosed = true
}

// Starts the control loop. A pacer starts only when the switch turns off with
// live links, so a block left off runs only the loop.
func (self *exchangeLocalTransport) start() {
	if !self.beginWorker() {
		return
	}
	go server.HandleError(func() {
		defer self.endWorker()
		self.run()
	})
}

// Reads the control now, then every LocalTransportControlRefreshInterval
// until ctx ends, on one reused timer. A non-positive interval reads once.
func (self *exchangeLocalTransport) run() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		self.refresh()
		if self.settings.LocalTransportControlRefreshInterval <= 0 {
			return
		}
		timer.Reset(self.settings.LocalTransportControlRefreshInterval)
		select {
		case <-self.ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// Reads the control once. A read that fails or raises keeps the last value.
// Called only by the loop; tests call it in the loop's place.
func (self *exchangeLocalTransport) refresh() {
	var control *model.ConnectMoveControl
	var err error
	server.HandleError(func() {
		readCtx, readCancel := context.WithTimeout(self.ctx, self.settings.LocalTransportControlReadTimeout)
		defer readCancel()
		control, err = self.read(readCtx, self.ceiling())
	}, func(readErr error) {
		err = readErr
	})
	if err != nil {
		if self.ctx.Err() != nil {
			// the exchange is closing
			return
		}
		if !self.failing {
			self.failing = true
			glog.Infof("[exchange]local transport control unavailable, keeping attach=%t (%s)\n", self.allows(), err)
		} else if glog.V(1) {
			glog.Infof("[exchange]local transport control unavailable (%s)\n", err)
		}
		return
	}
	self.failing = false
	self.setAllowed(control.LocalTransportAllows(self.block))
}

// Publishes the switch for new attaches. Turning it off with live links starts
// a pacer that detaches them, unless one is pacing already or the rate
// detaches none.
func (self *exchangeLocalTransport) setAllowed(allowed bool) {
	changed, startPacer := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.allowed.Load() == allowed {
			return false, false
		}
		self.allowed.Store(allowed)
		if allowed || self.pacing || len(self.links) == 0 || self.settings.LocalTransportDisableDetachRate <= 0 {
			return true, false
		}
		self.pacing = true
		return true, true
	}()
	if !changed {
		return
	}
	setExchangeLocalTransportEnabledGauge(allowed)
	glog.Infof("[exchange]local transport control: block %s attach=%t\n", self.block, allowed)
	if startPacer {
		self.startPacer()
	}
}

// Starts the pacer setAllowed claimed.
func (self *exchangeLocalTransport) startPacer() {
	if !self.beginWorker() {
		// the exchange is closing, and its residents' ends detach every link
		self.endPacing()
		return
	}
	if afterStart := self.afterPacerStartForTest; afterStart != nil {
		afterStart()
	}
	go server.HandleError(func() {
		defer self.endWorker()
		self.detachPaced()
	}, func(error) {
		self.endPacing()
	})
}

// Detaches one live link per pace interval, on one reused timer, until the
// switch turns on again or no link is left. No link attaches while the switch
// is off, so the pacer ends.
func (self *exchangeLocalTransport) detachPaced() {
	interval := time.Second / time.Duration(self.settings.LocalTransportDisableDetachRate)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		link := self.takeLinkWhileOff()
		if link == nil {
			return
		}
		link.detach(localTransportDetachDisabled)
		if !self.continuePacing() {
			return
		}
		timer.Reset(interval)
		select {
		case <-self.ctx.Done():
			self.endPacing()
			return
		case <-timer.C:
		}
	}
}

// Withdraws a live link for the pacer while the switch is off. Otherwise ends
// the pacing in the same critical section as the decision, and returns nil.
func (self *exchangeLocalTransport) takeLinkWhileOff() *localTransportLink {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.allowed.Load() {
		for link := range self.links {
			delete(self.links, link)
			return link
		}
	}
	self.pacing = false
	return nil
}

// Whether another link is left to detach while the switch stays off.
// Otherwise ends the pacing in the same critical section, so the pacer does
// not wait out an interval with nothing to do.
func (self *exchangeLocalTransport) continuePacing() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.allowed.Load() && 0 < len(self.links) {
		return true
	}
	self.pacing = false
	return false
}

// Ends the pacing of a pacer that stops early. Only the pacing pacer calls
// it, and no other starts while it paces.
func (self *exchangeLocalTransport) endPacing() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pacing = false
}

// Whether a pacer is detaching links.
func (self *exchangeLocalTransport) isPacing() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.pacing
}

// Publishes a new link if the switch still allows attaching.
func (self *exchangeLocalTransport) addLink(link *localTransportLink) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.allowed.Load() {
		return false
	}
	self.links[link] = struct{}{}
	return true
}

// Withdraws a link that is detaching, so the pacer passes over it.
func (self *exchangeLocalTransport) removeLink(link *localTransportLink) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.links, link)
}

// The number of live links.
func (self *exchangeLocalTransport) linkCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.links)
}

// Attaches the transport described by header to this process's resident, or
// returns nil for the caller to dial the resident as before: the switch is
// off, the record's generation is not this process's current resident, or
// that resident is done. A draining exchange still attaches to a present
// resident, as its accept path accepts one. A returned link may already have
// ended, and its owner runs it with ResidentTransport.runLocal.
func (self *Exchange) attachLocalTransport(ctx context.Context, header ExchangeHeader) *localTransportLink {
	localTransport := self.localTransport
	if localTransport == nil || !localTransport.allows() {
		return nil
	}
	resident := self.localResident(header.ClientId)
	if resident == nil || resident.residentId != header.ResidentId || resident.IsDone() {
		return nil
	}
	if beforeAttach := self.beforeLocalTransportAttachForTest; beforeAttach != nil {
		beforeAttach(resident)
	}
	return self.attachLocalTransportRoutes(ctx, localTransport, resident, header, func() (chan []byte, chan []byte, func(), error) {
		return resident.addTransportWithEdge(
			header.transferCarrierProperties(),
			header.transportEdge(),
		)
	})
}

// The attach after its checks, over the routes publish adds to the resident.
// Tests publish queues they own in place of the resident's routes.
func (self *Exchange) attachLocalTransportRoutes(
	ctx context.Context,
	localTransport *exchangeLocalTransport,
	resident *Resident,
	header ExchangeHeader,
	publish func() (routeSend chan []byte, routeReceive chan []byte, closeTransport func(), err error),
) *localTransportLink {
	// The link's owner is an exchange worker until its detach completes, so
	// WaitForIdle covers the resident's side as it covers an accepted socket.
	if !localTransport.beginWorker() {
		return nil
	}
	linkCtx, linkCancel := context.WithCancel(ctx)
	link := &localTransportLink{
		ctx:      linkCtx,
		cancel:   linkCancel,
		exchange: self,
		settings: self.settings,
		resident: resident,
		header:   header,
	}
	// The pacer can detach the link from here on: the attach then completes,
	// and its owner detaches it at once.
	if !localTransport.addLink(link) {
		linkCancel()
		localTransport.endWorker()
		return nil
	}
	self.registerExchangeConnection(header.ClientId)
	routeSend, routeReceive, closeTransport, err := publish()
	if err != nil {
		self.unregisterExchangeConnection(header.ClientId)
		localTransport.removeLink(link)
		linkCancel()
		localTransport.endWorker()
		return nil
	}
	link.routeSend = routeSend
	link.routeReceive = routeReceive
	link.closeTransport = closeTransport
	resident.localAttachments.Add(1)
	exchangeLocalAttachmentsGauge.Inc()
	// Propagates the resident's end only; the pumps own every wait.
	link.stopResidentDone = context.AfterFunc(resident.ctx, func() {
		link.detach(localTransportDetachResidentDone)
	})
	// A resident replaced after the generation check is done by now, and its
	// edge above runs on another goroutine. Detach before either pump starts,
	// so nothing reaches it.
	select {
	case <-resident.ctx.Done():
		link.detach(localTransportDetachResidentDone)
	default:
	}
	return link
}

// One transport attached in process to a resident. The owner, the transport's
// Run goroutine, runs its client pump and its detach, and alone reads the
// routes after attach returns. The pacer and the resident's done edge only
// call detach.
type localTransportLink struct {
	ctx    context.Context
	cancel context.CancelFunc

	exchange *Exchange
	settings *ExchangeSettings
	resident *Resident
	header   ExchangeHeader

	// the resident's routes for this transport and their removal
	routeSend      chan []byte
	routeReceive   chan []byte
	closeTransport func()
	// stops the resident's done edge
	stopResidentDone func() bool

	// the first recorded reason, localTransportDetachNone until one is
	reason atomic.Uint32
}

// Ends the link, recording reason unless another was recorded first.
func (self *localTransportLink) detach(reason localTransportDetachReason) {
	self.reason.CompareAndSwap(uint32(localTransportDetachNone), uint32(reason))
	self.cancel()
}

// Detaches the link in closeExchangeTransportQueues order after the client
// pump has returned, joins the resident pump, and returns the reason. Called
// once, by the owner, which then stops being an exchange worker.
func (self *localTransportLink) close(pumps *sync.WaitGroup) localTransportDetachReason {
	defer self.exchange.localTransport.endWorker()
	self.exchange.localTransport.removeLink(self)
	self.stopResidentDone()
	// Route removal joins every resident writer still admitted to the old
	// routes. One blocked on a full send route ends at its own write timeout,
	// as when a socket closes.
	self.closeTransport()
	self.cancel()
	pumps.Wait()
	returnReadyPooledMessages(self.routeSend)
	returnReadyPooledMessages(self.routeReceive)
	// The link was activity up to now, as an idle socket's last ping was at
	// most ExchangePingTimeout before it closed.
	self.resident.UpdateActivity()
	self.resident.localAttachments.Add(-1)
	self.exchange.unregisterExchangeConnection(self.header.ClientId)
	exchangeLocalAttachmentsGauge.Dec()

	reason := localTransportDetachReason(self.reason.Load())
	if reason == localTransportDetachNone {
		// only the transport's own end records no reason
		reason = localTransportDetachTransportDone
	}
	exchangeLocalDetachesCounters[reason].Inc()
	return reason
}

// One direction of a link: the batch writer of an exchange socket, with a
// queue in place of the socket. Owned by its pump goroutine.
type localTransportPump struct {
	link        *localTransportLink
	direction   localTransportDirection
	source      <-chan []byte
	destination chan<- []byte
	// the receive queue boundary that counts the messages this pump drops
	dropBoundary string
	// ends the link when the source queue is closed
	sourceClosed func()

	// reused batch index; its references are cleared after each batch
	batch [][]byte
}

// Delivers batches until the link ends.
func (self *localTransportPump) run() {
	for {
		// A link that has ended takes nothing more from its source, so the
		// next link receives the rest of the source in order.
		select {
		case <-self.link.ctx.Done():
			return
		default:
		}
		select {
		case <-self.link.ctx.Done():
			return
		case message, ok := <-self.source:
			if !ok {
				self.sourceClosed()
				return
			}
			if !self.deliverBatch(message) {
				return
			}
		}
	}
}

// Gathers the ready messages after first, checks the batch, and delivers it
// in order within WriteTimeout of its start. Returns whether the link
// continues. Each message of the batch ends with one owner: its destination,
// or the pool.
func (self *localTransportPump) deliverBatch(first []byte) bool {
	link := self.link
	settings := link.settings
	self.batch = append(self.batch[:0], first)
	// The pump owns batch[owned:]. Whatever it still owns when the batch
	// ends, on an early return or a raise, is returned and counted as
	// dropped. Then the stale references go too, so a later smaller batch
	// keeps no payload reachable.
	owned := 0
	defer func() {
		self.drop(self.batch[owned:])
		clear(self.batch)
		self.batch = self.batch[:0]
	}()
	if beforeGather := link.exchange.beforeLocalTransportGatherForTest; beforeGather != nil {
		beforeGather(link, self.direction)
	}
	batchByteCount := ByteCount(len(first))
	open := true
gather:
	for open && len(self.batch) < settings.ExchangeWriteBatchCount && batchByteCount < settings.ExchangeWriteBatchByteCount {
		// A link that has ended takes no more from its source, so the next
		// link carries the rest.
		select {
		case <-link.ctx.Done():
			break gather
		default:
		}
		select {
		case next, nextOk := <-self.source:
			if !nextOk {
				open = false
			} else {
				self.batch = append(self.batch, next)
				batchByteCount += ByteCount(len(next))
			}
		default:
			break gather
		}
	}
	if afterGather := link.exchange.afterLocalTransportGatherForTest; afterGather != nil {
		afterGather(link, self.direction, len(self.batch))
	}

	// The framer's limits for the whole batch before any delivery, as
	// ExchangeBuffer.WriteMessages checks them before its write.
	for _, message := range self.batch {
		messageLen := len(message)
		if settings.FramerSettings.MaxMessageLen < messageLen || math.MaxUint16 < messageLen {
			if glog.V(1) {
				glog.Infof(
					"[rtl]max message len exceeded (%d<%d) %s/%s\n",
					settings.FramerSettings.MaxMessageLen,
					messageLen,
					link.header.ClientId,
					link.header.ResidentId,
				)
			}
			link.detach(localTransportDetachMaxMessageLen)
			return false
		}
	}

	// The whole batch has WriteTimeout from its start, as one exchange write.
	deadline := time.Now().Add(settings.WriteTimeout)
	var timer *time.Timer
	dataFrameCount := 0
	dataByteCount := 0
	pingCount := 0
	for messageIndex, message := range self.batch {
		if len(message) == 0 {
			// a ping: activity, not a delivery
			owned = messageIndex + 1
			connect.MessagePoolReturn(message)
			pingCount += 1
			continue
		}
		select {
		case self.destination <- message:
			owned = messageIndex + 1
			dataFrameCount += 1
			dataByteCount += len(message)
			continue
		default:
		}
		// The delivery would block. One timer serves the rest of the batch,
		// for exactly the budget it has left.
		if timer == nil {
			timer = time.NewTimer(time.Until(deadline))
		}
		if beforeBlock := link.exchange.beforeLocalTransportBlockForTest; beforeBlock != nil {
			beforeBlock(link, self.direction)
		}
		blockStartTime := time.Now()
		select {
		case self.destination <- message:
			owned = messageIndex + 1
			exchangeLocalBlockedSecondsHistogram.Observe(time.Since(blockStartTime).Seconds())
			dataFrameCount += 1
			dataByteCount += len(message)
		case <-timer.C:
			exchangeLocalBlockedSecondsHistogram.Observe(time.Since(blockStartTime).Seconds())
			recordLocalTransportFrames(dataFrameCount, dataByteCount, pingCount)
			link.detach(localTransportDetachWriteTimeout)
			return false
		case <-link.ctx.Done():
			timer.Stop()
			recordLocalTransportFrames(dataFrameCount, dataByteCount, pingCount)
			return false
		}
	}
	if timer != nil {
		timer.Stop()
	}
	recordLocalTransportFrames(dataFrameCount, dataByteCount, pingCount)
	link.resident.UpdateActivity()
	if !open {
		self.sourceClosed()
		return false
	}
	return true
}

// Returns messages this pump dequeued and did not deliver, counting each data
// message as dropped at the pump's boundary.
func (self *localTransportPump) drop(messages [][]byte) {
	for _, message := range messages {
		if 0 < len(message) {
			recordReceiveQueueDrop(self.dropBoundary, len(message))
		}
		connect.MessagePoolReturn(message)
	}
}

// Runs an attached link until it detaches, and returns why. The client pump
// runs here, on the Run goroutine; the resident pump on one goroutine the link
// owns. A raise in either pump detaches the link with raised, after the pump
// returned the batch it held, and the loop continues as after a socket's end.
// On return the link is detached and both pumps are joined.
func (self *ResidentTransport) runLocal(link *localTransportLink) (reason localTransportDetachReason) {
	var pumps sync.WaitGroup
	defer func() {
		reason = link.close(&pumps)
	}()
	raised := func(error) {
		link.detach(localTransportDetachRaised)
	}

	residentPump := &localTransportPump{
		link:         link,
		direction:    localTransportDirectionResident,
		source:       link.routeSend,
		destination:  self.receive,
		dropBoundary: receiveQueueBoundaryLocalTransportReceive,
		sourceClosed: func() {
			link.detach(localTransportDetachResidentDone)
		},
	}
	pumps.Add(1)
	go server.HandleError(func() {
		defer pumps.Done()
		residentPump.run()
	}, raised)

	clientPump := &localTransportPump{
		link:         link,
		direction:    localTransportDirectionClient,
		source:       self.send,
		destination:  link.routeReceive,
		dropBoundary: receiveQueueBoundaryLocalTransportSend,
		sourceClosed: func() {
			// a closed send queue closes the transport, as it does an
			// exchange connection's
			self.cancel()
			link.detach(localTransportDetachTransportDone)
		},
	}
	server.HandleError(clientPump.run, raised)
	return
}
