// This file applies PERFVAR's deterministic directional link model below
// Pion's UDP sockets while retaining the real ICE, DTLS, SRTP, and SCTP stack.
package perfvar

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/pion/transport/v4"
	"github.com/pion/transport/v4/vnet"
)

// PERFVAR's P2P topology is IPv4-only. Every UDP payload is charged for the
// standard 20-byte IPv4 header and 8-byte UDP header at the outer-link boundary.
const p2pIPv4UDPHeaderByteCount = 20 + 8

// Pion vnet silently drops a datagram when one UDP socket's fixed read queue
// already contains this many packets. Tracked credits stay one slot below this
// physical boundary.
const p2pVnetReceiveQueuePacketCount = 1024

// One queue slot is reserved for the single frame that can pass a router gate
// immediately before its destination generation closes. The rebound wrapper
// consumes that stale frame without exposing it or stranding a tracked token.
const p2pVnetReceiveCreditPacketCount = p2pVnetReceiveQueuePacketCount - 1

// The private vnet frame header carries one socket generation without changing
// the simulated outer packet size charged to the directional link.
const p2pVnetGenerationHeaderByteCount = 16

// Only ordinary UDP-sized scratch buffers are retained between framed reads.
const p2pMaximumPooledGenerationFrameByteCount = 128 * 1024

// The generation marker distinguishes harness frames from arbitrary UDP data.
const p2pVnetGenerationFrameMagic = "URPVGEN1"

// A live or serialized view makes every admitted receive datagram account for
// exactly one read, cancellation, or outstanding queue reservation.
type p2pReceiveCreditSnapshot struct {
	CapacityPacketCount        int    `json:"capacity_packet_count"`
	AdmittedPacketCount        uint64 `json:"admitted_packet_count"`
	ReadPacketCount            uint64 `json:"read_packet_count"`
	CanceledPacketCount        uint64 `json:"canceled_packet_count"`
	OutstandingPacketCount     int    `json:"outstanding_packet_count"`
	PendingAcquireCount        int    `json:"pending_acquire_count"`
	MaximumOutstandingPackets  int    `json:"maximum_outstanding_packets"`
	BlockedAcquireCount        uint64 `json:"blocked_acquire_count"`
	InvalidReleasePacketCount  uint64 `json:"invalid_release_packet_count"`
	LateReleaseAfterCloseCount uint64 `json:"late_release_after_close_count"`
	StaleGenerationDropCount   uint64 `json:"stale_generation_drop_count"`
	Closed                     bool   `json:"closed"`
	DestinationScoped          bool   `json:"destination_scoped"`
	TrackedReservationCount    int    `json:"tracked_reservation_count"`
	RouterPendingPacketCount   int    `json:"router_pending_packet_count"`

	measurementMaximumEpoch   *p2pReceiveCreditMaximumEpoch
	measurementMaximumPackets int
}

// One workload interval records receive-admission high water independently
// from the pool's lifetime diagnostic maximum.
type p2pReceiveCreditMaximumEpoch struct {
	maximumOutstandingPacket int
}

// A directional credit pool is shared by every sending socket and every
// receiving wrapper on the opposite vnet endpoint. Methods are safe for
// concurrent scheduler deliveries, socket reads, snapshots, and shutdown.
type p2pReceiveCredits struct {
	available chan struct{}
	closed    chan struct{}
	idle      chan struct{}
	progress  chan struct{}
	closeOnce sync.Once

	stateLock                 sync.Mutex
	closedState               bool
	outstandingPacketCount    int
	pendingAcquireCount       int
	maximumOutstandingPacket  int
	measurementMaximum        *p2pReceiveCreditMaximumEpoch
	admittedPacketCount       uint64
	readPacketCount           uint64
	canceledPacketCount       uint64
	blockedAcquireCount       uint64
	invalidReleaseCount       uint64
	lateReleaseCount          uint64
	staleGenerationDropCount  uint64
	trackedReservationCount   int
	routerPendingPacketCount  int
	destinationScoped         bool
	generationFramed          bool
	nextSocketGeneration      uint64
	receiveSockets            map[string]*p2pReceiveCreditSocket
	routerPendingReservations []*p2pReceiveCreditReservation
	routerIdle                chan struct{}
	frameBufferPool           sync.Pool

	// This test-only edge is nil in production measurement paths.
	beforeAcquireWaitForTest        func()
	afterSocketAcquireForTest       func()
	pendingAcquireObservedForTest   func()
	beforeQuiescentRetryWaitForTest func()
	quiescentRetryWaitForTest       <-chan struct{}
	beforeRouterCompletionForTest   func([]byte)
	routerPendingObservedForTest    func()
}

// One destination socket owns the ordered reservations for datagrams that
// can actually reach its vnet receive queue. All fields use the pool lock.
type p2pReceiveCreditSocket struct {
	credits      *p2pReceiveCredits
	addressKey   string
	remoteKey    string
	owner        *p2pLinkNet
	closedSignal chan struct{}
	closing      bool
	closed       bool
	generation   uint64
	reservations []*p2pReceiveCreditReservation
}

// One successful scheduled write has exactly one read, delegate-failure, or
// socket-close disposition. The active bit uses the owning pool lock.
type p2pReceiveCreditReservation struct {
	credits       *p2pReceiveCredits
	socket        *p2pReceiveCreditSocket
	active        bool
	routerPending bool
}

// Construction establishes one fixed capacity and starts at idle.
func newP2pReceiveCredits(capacityPacketCount int) *p2pReceiveCredits {
	if capacityPacketCount <= 0 {
		panic("P2P receive credit capacity must be positive")
	}
	idle := make(chan struct{})
	close(idle)
	routerIdle := make(chan struct{})
	close(routerIdle)
	credits := &p2pReceiveCredits{
		available:      make(chan struct{}, capacityPacketCount),
		closed:         make(chan struct{}),
		idle:           idle,
		routerIdle:     routerIdle,
		progress:       make(chan struct{}, 1),
		receiveSockets: map[string]*p2pReceiveCreditSocket{},
	}
	for range capacityPacketCount {
		credits.available <- struct{}{}
	}
	return credits
}

// Real vnet networks require destination-aware reservations. Focused pool
// tests retain the unscoped constructor so they can isolate semaphore logic.
func newP2pDestinationReceiveCredits(capacityPacketCount int) *p2pReceiveCredits {
	credits := newP2pReceiveCredits(capacityPacketCount)
	credits.destinationScoped = true
	return credits
}

// Real Pion vnet endpoints additionally frame the socket generation so a
// router-delayed packet cannot cross a close-and-rebind boundary.
func newP2pVnetReceiveCredits(capacityPacketCount int) *p2pReceiveCredits {
	credits := newP2pDestinationReceiveCredits(capacityPacketCount)
	credits.generationFramed = true
	return credits
}

// A canonical endpoint key lets a sender distinguish a live destination
// socket from a vnet write that its router will later discard.
func p2pReceiveCreditAddressKey(address net.Addr) (string, bool) {
	udpAddress, ok := address.(*net.UDPAddr)
	if !ok || udpAddress == nil || udpAddress.Port <= 0 {
		return "", false
	}
	if udpAddress.IP == nil || udpAddress.IP.IsUnspecified() {
		return net.JoinHostPort("", fmt.Sprintf("%d", udpAddress.Port)), true
	}
	addressPort := udpAddress.AddrPort()
	if !addressPort.IsValid() {
		return "", false
	}
	return netip.AddrPortFrom(addressPort.Addr().Unmap(), addressPort.Port()).String(), true
}

// A successfully created wrapper publishes its exact local endpoint before
// any remote scheduler can reserve space in that socket's receive queue.
func (self *p2pReceiveCredits) registerSocket(
	address net.Addr,
	remoteAddress net.Addr,
	owner *p2pLinkNet,
) *p2pReceiveCreditSocket {
	if self == nil {
		return nil
	}
	addressKey, ok := p2pReceiveCreditAddressKey(address)
	if !ok {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closedState {
		return nil
	}
	remoteKey, _ := p2pReceiveCreditAddressKey(remoteAddress)
	self.nextSocketGeneration += 1
	socket := &p2pReceiveCreditSocket{
		credits:      self,
		addressKey:   addressKey,
		remoteKey:    remoteKey,
		owner:        owner,
		closedSignal: make(chan struct{}),
		generation:   self.nextSocketGeneration,
	}
	if previous := self.receiveSockets[addressKey]; previous != nil && !previous.closed {
		panic("duplicate live P2P receive-credit socket")
	}
	self.receiveSockets[addressKey] = socket
	return socket
}

// A router accepts only a frame addressed to the currently registered socket
// generation and, for connected UDP, from that socket's authenticated peer.
func (self *p2pReceiveCredits) acceptRouterPayload(
	sourceAddress net.Addr,
	destinationAddress net.Addr,
	payload []byte,
) bool {
	if self == nil || !self.generationFramed {
		return true
	}
	generation, framed := p2pVnetPayloadGeneration(payload)
	self.stateLock.Lock()
	pending := false
	if framed {
		for _, reservation := range self.routerPendingReservations {
			if reservation.routerPending && reservation.socket != nil &&
				reservation.socket.generation == generation {
				pending = true
				break
			}
		}
	}
	self.stateLock.Unlock()
	if framed && pending && self.beforeRouterCompletionForTest != nil {
		self.beforeRouterCompletionForTest(payload)
	}
	self.stateLock.Lock()
	socket := self.socketForTransferWithLock(sourceAddress, destinationAddress)
	accepted := framed && pending && socket != nil && socket.generation == generation
	if !accepted {
		self.staleGenerationDropCount += 1
	}
	self.stateLock.Unlock()
	self.completeRouterPayload(generation)
	if !accepted {
		self.notifyProgress()
	}
	return accepted
}

// Router completion retires the oldest matching framed write only after every
// configured topology and test filter preceding this gate has finished.
func (self *p2pReceiveCredits) completeRouterPayload(generation uint64) {
	if self == nil || !self.generationFramed {
		return
	}
	self.stateLock.Lock()
	for reservationIndex, reservation := range self.routerPendingReservations {
		if reservation.routerPending && reservation.socket != nil &&
			reservation.socket.generation == generation {
			reservation.routerPending = false
			self.routerPendingReservations = append(
				self.routerPendingReservations[:reservationIndex],
				self.routerPendingReservations[reservationIndex+1:]...,
			)
			self.routerPendingPacketCount -= 1
			if self.routerPendingPacketCount == 0 {
				close(self.routerIdle)
			}
			self.stateLock.Unlock()
			self.notifyProgress()
			return
		}
	}
	if !self.closedState {
		self.invalidReleaseCount += 1
	}
	self.stateLock.Unlock()
	self.notifyProgress()
}

// A delegate failure removes the exact framed write because no router chunk
// exists to reach the completion gate.
func (self *p2pReceiveCreditReservation) cancelRouterPayload() {
	if self == nil || self.credits == nil {
		return
	}
	credits := self.credits
	credits.stateLock.Lock()
	if self.routerPending {
		self.routerPending = false
		for reservationIndex, reservation := range credits.routerPendingReservations {
			if reservation == self {
				credits.routerPendingReservations = append(
					credits.routerPendingReservations[:reservationIndex],
					credits.routerPendingReservations[reservationIndex+1:]...,
				)
				break
			}
		}
		credits.routerPendingPacketCount -= 1
		if credits.routerPendingPacketCount == 0 {
			close(credits.routerIdle)
		}
	}
	credits.stateLock.Unlock()
	credits.notifyProgress()
}

// One invalid generation observed after the router filter records the narrow
// filter-to-NIC race and is discarded before application delivery.
func (self *p2pReceiveCredits) recordStaleGenerationDrop() {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	self.staleGenerationDropCount += 1
	self.stateLock.Unlock()
	self.notifyProgress()
}

// A destination lookup accepts an exact address or a wildcard listener on the
// same port. Missing destinations need no queue reservation because vnet will
// discard them before any socket read queue is involved.
func (self *p2pReceiveCredits) socketForTransferWithLock(
	sourceAddress net.Addr,
	destinationAddress net.Addr,
) *p2pReceiveCreditSocket {
	addressKey, ok := p2pReceiveCreditAddressKey(destinationAddress)
	if !ok {
		return nil
	}
	sourceKey, _ := p2pReceiveCreditAddressKey(sourceAddress)
	acceptsSource := func(socket *p2pReceiveCreditSocket) bool {
		return socket.remoteKey == "" || socket.remoteKey == sourceKey
	}
	if socket := self.receiveSockets[addressKey]; socket != nil && !socket.closing &&
		!socket.closed && acceptsSource(socket) {
		return socket
	}
	udpAddress := destinationAddress.(*net.UDPAddr)
	wildcardKey := net.JoinHostPort("", fmt.Sprintf("%d", udpAddress.Port))
	if socket := self.receiveSockets[wildcardKey]; socket != nil && !socket.closing &&
		!socket.closed && acceptsSource(socket) {
		return socket
	}
	return nil
}

// Queue space is reserved only for a live destination. Socket replacement is
// rechecked after a capacity wait so close cannot strand a phantom admission.
func (self *p2pReceiveCredits) reserveForAddress(
	ctx context.Context,
	address net.Addr,
) (*p2pReceiveCreditReservation, bool, bool) {
	return self.reserveForTransfer(ctx, nil, address)
}

// Source-aware lookup rejects wrong-source traffic before connected vnet can
// silently dequeue it. The booleans report destination eligibility and global
// capacity admission independently.
func (self *p2pReceiveCredits) reserveForTransfer(
	ctx context.Context,
	sourceAddress net.Addr,
	destinationAddress net.Addr,
) (*p2pReceiveCreditReservation, bool, bool) {
	if self == nil {
		return nil, true, true
	}
	if !self.destinationScoped {
		if !self.acquire(ctx) {
			return nil, true, false
		}
		return &p2pReceiveCreditReservation{credits: self, active: true}, true, true
	}
	self.stateLock.Lock()
	socket := self.socketForTransferWithLock(sourceAddress, destinationAddress)
	self.stateLock.Unlock()
	if socket == nil {
		return nil, false, true
	}
	if !self.acquireWithAbort(ctx, socket.closedSignal) {
		return nil, true, false
	}
	if self.afterSocketAcquireForTest != nil {
		self.afterSocketAcquireForTest()
	}
	self.stateLock.Lock()
	currentSocket := self.socketForTransferWithLock(sourceAddress, destinationAddress)
	if self.closedState {
		self.stateLock.Unlock()
		return nil, true, false
	}
	if socket.closing || socket.closed || currentSocket != socket {
		self.releaseWithLock(false)
		self.stateLock.Unlock()
		self.notifyProgress()
		return nil, false, true
	}
	reservation := &p2pReceiveCreditReservation{credits: self, socket: socket, active: true}
	socket.reservations = append(socket.reservations, reservation)
	self.trackedReservationCount += 1
	self.stateLock.Unlock()
	return reservation, true, true
}

// A successful acquisition reserves one destination queue slot. The context
// and close signal are liveness exits for a scheduler blocked by a held reader.
func (self *p2pReceiveCredits) acquire(ctx context.Context) bool {
	return self.acquireWithAbort(ctx, nil)
}

// An optional socket-generation edge cancels capacity waiting as soon as its
// destination closes, even if another socket owns every available credit.
func (self *p2pReceiveCredits) acquireWithAbort(
	ctx context.Context,
	abort <-chan struct{},
) bool {
	self.stateLock.Lock()
	if self.closedState {
		self.stateLock.Unlock()
		return false
	}
	if self.outstandingPacketCount == 0 && self.pendingAcquireCount == 0 {
		self.idle = make(chan struct{})
	}
	self.pendingAcquireCount += 1
	select {
	case <-self.available:
		self.pendingAcquireCount -= 1
		self.outstandingPacketCount += 1
		self.maximumOutstandingPacket = max(
			self.maximumOutstandingPacket,
			self.outstandingPacketCount,
		)
		if self.measurementMaximum != nil {
			self.measurementMaximum.maximumOutstandingPacket = max(
				self.measurementMaximum.maximumOutstandingPacket,
				self.outstandingPacketCount,
			)
		}
		self.admittedPacketCount += 1
		self.stateLock.Unlock()
		self.notifyProgress()
		return true
	default:
	}
	self.blockedAcquireCount += 1
	self.stateLock.Unlock()
	self.notifyProgress()
	if self.beforeAcquireWaitForTest != nil {
		self.beforeAcquireWaitForTest()
	}
	select {
	case <-self.closed:
		self.cancelPendingAcquire()
		return false
	case <-abort:
		self.cancelPendingAcquire()
		return false
	case <-ctx.Done():
		self.cancelPendingAcquire()
		return false
	case <-self.available:
		return self.commitAcquire()
	}
}

// Converting a pending attempt into admission is serialized with shutdown; a
// close winner restores the token and leaves no untracked ownership gap.
func (self *p2pReceiveCredits) commitAcquire() bool {
	self.stateLock.Lock()
	self.pendingAcquireCount -= 1
	if self.closedState {
		self.available <- struct{}{}
		if self.outstandingPacketCount == 0 && self.pendingAcquireCount == 0 {
			close(self.idle)
		}
		self.stateLock.Unlock()
		self.notifyProgress()
		return false
	}
	self.outstandingPacketCount += 1
	self.maximumOutstandingPacket = max(
		self.maximumOutstandingPacket,
		self.outstandingPacketCount,
	)
	if self.measurementMaximum != nil {
		self.measurementMaximum.maximumOutstandingPacket = max(
			self.measurementMaximum.maximumOutstandingPacket,
			self.outstandingPacketCount,
		)
	}
	self.admittedPacketCount += 1
	self.stateLock.Unlock()
	self.notifyProgress()
	return true
}

// Context cancellation removes one registered attempt that never took a
// capacity token and closes idle after the final pending generation exits.
func (self *p2pReceiveCredits) cancelPendingAcquire() {
	self.stateLock.Lock()
	self.pendingAcquireCount -= 1
	if self.outstandingPacketCount == 0 && self.pendingAcquireCount == 0 {
		close(self.idle)
	}
	self.stateLock.Unlock()
	self.notifyProgress()
}

// A receive call releases one reservation only when it consumed a datagram.
func (self *p2pReceiveCredits) recordRead(readByteCount int, err error) {
	if err != nil && readByteCount <= 0 && !errors.Is(err, io.ErrShortBuffer) {
		return
	}
	self.release(true)
}

// A failed delegate write returns the reservation because no read can do so.
func (self *p2pReceiveCredits) cancelAdmission() {
	self.release(false)
}

// One terminal disposition wakes capacity waiters. Releases after close refer
// to reservations already classified as canceled and remain observable.
func (self *p2pReceiveCredits) release(read bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.releaseWithLock(read)
	}()
	self.notifyProgress()
}

// The caller holds stateLock and owns exactly one live global reservation.
func (self *p2pReceiveCredits) releaseWithLock(read bool) {
	if self.closedState {
		self.lateReleaseCount += 1
		return
	}
	if self.outstandingPacketCount <= 0 {
		self.invalidReleaseCount += 1
		return
	}
	self.outstandingPacketCount -= 1
	if read {
		self.readPacketCount += 1
	} else {
		self.canceledPacketCount += 1
	}
	self.available <- struct{}{}
	if self.outstandingPacketCount == 0 && self.pendingAcquireCount == 0 {
		close(self.idle)
	}
}

// A failed delayed delegate write owns the reservation returned by its exact
// destination lookup and cannot cancel a later packet on the same socket.
func (self *p2pReceiveCreditReservation) cancel() {
	if self == nil || self.credits == nil {
		return
	}
	credits := self.credits
	credits.stateLock.Lock()
	if self.active {
		self.active = false
		if self.socket != nil {
			credits.trackedReservationCount -= 1
			for reservationIndex, reservation := range self.socket.reservations {
				if reservation == self {
					self.socket.reservations = append(
						self.socket.reservations[:reservationIndex],
						self.socket.reservations[reservationIndex+1:]...,
					)
					break
				}
			}
		}
		credits.releaseWithLock(false)
	}
	credits.stateLock.Unlock()
	credits.notifyProgress()
}

// Framing reuses bytes already owned by the simulated outer IP/UDP header, so
// the scheduler still charges the exact original datagram size and makes no
// second send-side allocation.
func (self *p2pReceiveCreditReservation) framePayload(
	ownedOuterPacket []byte,
) ([]byte, bool) {
	if self == nil || self.credits == nil || self.socket == nil ||
		len(ownedOuterPacket) < p2pIPv4UDPHeaderByteCount {
		return nil, false
	}
	credits := self.credits
	credits.stateLock.Lock()
	active := self.active && !self.socket.closing && !self.socket.closed
	generation := self.socket.generation
	if active {
		if self.routerPending {
			credits.stateLock.Unlock()
			return nil, false
		}
		if credits.routerPendingPacketCount == 0 {
			credits.routerIdle = make(chan struct{})
		}
		self.routerPending = true
		credits.routerPendingPacketCount += 1
		credits.routerPendingReservations = append(credits.routerPendingReservations, self)
	}
	credits.stateLock.Unlock()
	if !active {
		return nil, false
	}
	headerOffset := p2pIPv4UDPHeaderByteCount - p2pVnetGenerationHeaderByteCount
	copy(
		ownedOuterPacket[headerOffset:headerOffset+len(p2pVnetGenerationFrameMagic)],
		p2pVnetGenerationFrameMagic,
	)
	binary.BigEndian.PutUint64(
		ownedOuterPacket[headerOffset+len(p2pVnetGenerationFrameMagic):p2pIPv4UDPHeaderByteCount],
		generation,
	)
	return ownedOuterPacket[headerOffset:], true
}

// Parsing is allocation-free and requires both the fixed marker and nonzero
// generation assigned by socket registration.
func p2pVnetPayloadGeneration(payload []byte) (uint64, bool) {
	if len(payload) < p2pVnetGenerationHeaderByteCount ||
		!bytes.Equal(payload[:len(p2pVnetGenerationFrameMagic)], []byte(p2pVnetGenerationFrameMagic)) {
		return 0, false
	}
	generation := binary.BigEndian.Uint64(
		payload[len(p2pVnetGenerationFrameMagic):p2pVnetGenerationHeaderByteCount],
	)
	return generation, generation != 0
}

// A successful or short-buffer read retires the oldest live reservation.
// Wrong-source traffic is rejected before vnet, so an empty error result never
// guesses that an invisible datagram was consumed.
func (self *p2pReceiveCreditSocket) recordRead(
	readByteCount int,
	err error,
) {
	if self == nil || self.credits == nil {
		return
	}
	consumed := err == nil || 0 < readByteCount || errors.Is(err, io.ErrShortBuffer)
	if !consumed {
		return
	}
	credits := self.credits
	credits.stateLock.Lock()
	if readByteCount <= 0 && err != nil && (self.closing || self.closed) {
		credits.stateLock.Unlock()
		return
	}
	for 0 < len(self.reservations) {
		reservation := self.reservations[0]
		self.reservations = self.reservations[1:]
		if reservation.active {
			reservation.active = false
			credits.trackedReservationCount -= 1
			credits.releaseWithLock(true)
			credits.stateLock.Unlock()
			credits.notifyProgress()
			return
		}
	}
	if !self.closing && !self.closed {
		credits.invalidReleaseCount += 1
	}
	credits.stateLock.Unlock()
	credits.notifyProgress()
}

// Socket close first withdraws destination eligibility. Reads already holding
// a buffered datagram may still complete; an error used only to wake a blocked
// read cannot consume a reservation after this point.
func (self *p2pReceiveCreditSocket) beginClose() {
	if self == nil || self.credits == nil {
		return
	}
	credits := self.credits
	credits.stateLock.Lock()
	self.beginCloseWithLock()
	credits.stateLock.Unlock()
	credits.notifyProgress()
}

// The caller holds the pool lock. The closed edge is one-shot and wakes any
// capacity acquisition still tied to this socket generation.
func (self *p2pReceiveCreditSocket) beginCloseWithLock() {
	if self.closing || self.closed {
		return
	}
	self.closing = true
	close(self.closedSignal)
	if self.credits.receiveSockets[self.addressKey] == self {
		delete(self.credits.receiveSockets, self.addressKey)
	}
}

// Closing a wrapped socket retires every queued reservation after the
// delegate has stopped accepting new datagrams for that endpoint.
func (self *p2pReceiveCreditSocket) close() {
	if self == nil || self.credits == nil {
		return
	}
	credits := self.credits
	credits.stateLock.Lock()
	self.closeWithLock()
	credits.stateLock.Unlock()
	credits.notifyProgress()
}

// The caller holds the owning pool lock. Retirement is idempotent across
// explicit address migration, delegate Close, and whole-network shutdown.
func (self *p2pReceiveCreditSocket) closeWithLock() {
	if self.closed {
		return
	}
	credits := self.credits
	self.beginCloseWithLock()
	self.closed = true
	if credits.receiveSockets[self.addressKey] == self {
		delete(credits.receiveSockets, self.addressKey)
	}
	for _, reservation := range self.reservations {
		if reservation.active {
			reservation.active = false
			credits.trackedReservationCount -= 1
			credits.releaseWithLock(false)
		}
	}
	self.reservations = nil
}

// Replacing one vnet endpoint retires its exact and wildcard wrappers. Owner
// identity is stronger than address parsing and covers every bound surface.
func (self *p2pReceiveCredits) retireOwner(owner *p2pLinkNet) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	for _, socket := range self.receiveSockets {
		if socket.owner == owner {
			socket.closeWithLock()
		}
	}
	self.stateLock.Unlock()
	self.notifyProgress()
}

// A coalesced edge wakes count-based deterministic barriers.
func (self *p2pReceiveCredits) notifyProgress() {
	select {
	case self.progress <- struct{}{}:
	default:
	}
}

// The lock-held copy lets interval reset and monotonic baseline share one
// linearization point. Callers must hold stateLock.
func (self *p2pReceiveCredits) snapshotWithLock() p2pReceiveCreditSnapshot {
	measurementMaximumPackets := 0
	if self.measurementMaximum != nil {
		measurementMaximumPackets = self.measurementMaximum.maximumOutstandingPacket
	}
	return p2pReceiveCreditSnapshot{
		CapacityPacketCount:        cap(self.available),
		AdmittedPacketCount:        self.admittedPacketCount,
		ReadPacketCount:            self.readPacketCount,
		CanceledPacketCount:        self.canceledPacketCount,
		OutstandingPacketCount:     self.outstandingPacketCount,
		PendingAcquireCount:        self.pendingAcquireCount,
		MaximumOutstandingPackets:  self.maximumOutstandingPacket,
		BlockedAcquireCount:        self.blockedAcquireCount,
		InvalidReleasePacketCount:  self.invalidReleaseCount,
		LateReleaseAfterCloseCount: self.lateReleaseCount,
		StaleGenerationDropCount:   self.staleGenerationDropCount,
		Closed:                     self.closedState,
		DestinationScoped:          self.destinationScoped,
		TrackedReservationCount:    self.trackedReservationCount,
		RouterPendingPacketCount:   self.routerPendingPacketCount,
		measurementMaximumEpoch:    self.measurementMaximum,
		measurementMaximumPackets:  measurementMaximumPackets,
	}
}

// A live snapshot holds the state lock only while copying scalar values.
func (self *p2pReceiveCredits) snapshot() p2pReceiveCreditSnapshot {
	if self == nil {
		return p2pReceiveCreditSnapshot{}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.snapshotWithLock()
}

// A fresh interval waits out an acquisition still coupled to a scheduler,
// then resets its high-water bucket and captures counters atomically. A stable
// unread UDP backlog is a valid baseline: real UDP sockets may retain ICE or
// DTLS datagrams that Pion never consumes, and the interval delta still
// rejects any backlog change that crosses the workload boundary.
func (self *p2pReceiveCredits) beginMeasurementSnapshot(
	ctx context.Context,
) (p2pReceiveCreditSnapshot, bool) {
	if self == nil {
		return p2pReceiveCreditSnapshot{}, true
	}
	for {
		self.stateLock.Lock()
		if self.pendingAcquireCount == 0 && self.routerPendingPacketCount == 0 {
			self.measurementMaximum = &p2pReceiveCreditMaximumEpoch{
				maximumOutstandingPacket: self.outstandingPacketCount,
			}
			snapshot := self.snapshotWithLock()
			self.stateLock.Unlock()
			return snapshot, true
		}
		self.stateLock.Unlock()
		select {
		case <-ctx.Done():
			return p2pReceiveCreditSnapshot{}, false
		case <-self.progress:
		}
	}
}

// Router idle joins every successful vnet write through the final generation
// gate, including a chunk already removed from Pion's internal router queue.
func (self *p2pReceiveCredits) waitRouterIdle(ctx context.Context) bool {
	if self == nil || !self.generationFramed {
		return true
	}
	self.stateLock.Lock()
	idle := self.routerPendingPacketCount == 0
	idleChannel := self.routerIdle
	self.stateLock.Unlock()
	if idle {
		return true
	}
	if self.routerPendingObservedForTest != nil {
		self.routerPendingObservedForTest()
	}
	select {
	case <-ctx.Done():
		return false
	case <-idleChannel:
		return true
	}
}

// A positive count edge lets tests hold the reader and observe exact capacity
// without queue-length polling or a negative timeout.
func (self *p2pReceiveCredits) waitForAdmittedPacketCount(
	ctx context.Context,
	target uint64,
) bool {
	for {
		if target <= self.snapshot().AdmittedPacketCount {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-self.progress:
		}
	}
}

// Idle means every admitted vnet write has reached one read or cancellation.
func (self *p2pReceiveCredits) waitIdle(ctx context.Context) bool {
	if self == nil {
		return true
	}
	self.stateLock.Lock()
	idle := self.outstandingPacketCount == 0 && self.pendingAcquireCount == 0
	idleChannel := self.idle
	self.stateLock.Unlock()
	if idle {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-idleChannel:
		return true
	}
}

// Shutdown rejects new admissions, classifies every outstanding reservation
// as canceled, and unblocks both capacity and idle waiters exactly once.
func (self *p2pReceiveCredits) close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			active := self.outstandingPacketCount != 0 || self.pendingAcquireCount != 0
			routerActive := self.routerPendingPacketCount != 0
			self.closedState = true
			close(self.closed)
			for _, socket := range self.receiveSockets {
				socket.beginCloseWithLock()
				socket.closed = true
				for _, reservation := range socket.reservations {
					reservation.active = false
				}
				socket.reservations = nil
			}
			self.receiveSockets = nil
			self.trackedReservationCount = 0
			for _, reservation := range self.routerPendingReservations {
				reservation.routerPending = false
			}
			self.routerPendingReservations = nil
			self.routerPendingPacketCount = 0
			if routerActive {
				close(self.routerIdle)
			}
			canceledPacketCount := self.outstandingPacketCount
			self.canceledPacketCount += uint64(canceledPacketCount)
			self.outstandingPacketCount = 0
			for range canceledPacketCount {
				self.available <- struct{}{}
			}
			if active && self.pendingAcquireCount == 0 {
				close(self.idle)
			}
		}()
		self.notifyProgress()
	})
}

// Closing an unused pool leaves its already-closed idle generation intact and
// remains idempotent rather than closing the same channel twice.
func TestP2pReceiveCreditsCloseWhileUnusedIsIdempotent(t *testing.T) {
	ctx := context.Background()
	for _, used := range []bool{false, true} {
		credits := newP2pReceiveCredits(1)
		if used {
			if !credits.acquire(ctx) {
				t.Fatal("used receive credit was not admitted")
			}
			credits.recordRead(1, nil)
		}
		credits.close()
		credits.close()
		snapshot := credits.snapshot()
		expectedPacketCount := uint64(0)
		if used {
			expectedPacketCount = 1
		}
		if !snapshot.Closed || snapshot.AdmittedPacketCount != expectedPacketCount ||
			snapshot.ReadPacketCount != expectedPacketCount ||
			snapshot.CanceledPacketCount != 0 || snapshot.OutstandingPacketCount != 0 ||
			snapshot.PendingAcquireCount != 0 {
			t.Fatalf("used=%t closed receive-credit snapshot=%+v", used, snapshot)
		}
	}
}

// IPv4-mapped and native IPv4 address surfaces identify the same live socket,
// so a value-address write cannot bypass the destination credit registry.
func TestP2pReceiveCreditsCanonicalizeMappedDestinationAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pDestinationReceiveCredits(1)
	defer credits.close()
	mappedAddress := &net.UDPAddr{IP: net.ParseIP("10.240.0.2"), Port: 4321}
	nativeAddress := net.UDPAddrFromAddrPort(netip.MustParseAddrPort("10.240.0.2:4321"))
	mappedKey, mappedOk := p2pReceiveCreditAddressKey(mappedAddress)
	nativeKey, nativeOk := p2pReceiveCreditAddressKey(nativeAddress)
	if !mappedOk || !nativeOk || mappedKey != nativeKey {
		t.Fatalf("canonical keys mapped=(%q,%t) native=(%q,%t)", mappedKey, mappedOk, nativeKey, nativeOk)
	}
	socket := credits.registerSocket(mappedAddress, nil, nil)
	reservation, hasDestination, admitted := credits.reserveForAddress(ctx, nativeAddress)
	if !hasDestination || !admitted || reservation == nil || reservation.socket != socket {
		t.Fatalf(
			"native reservation destination=%t admitted=%t reservation=%+v",
			hasDestination,
			admitted,
			reservation,
		)
	}
	reservation.cancel()
	socket.close()
	snapshot := credits.snapshot()
	if snapshot.AdmittedPacketCount != 1 || snapshot.CanceledPacketCount != 1 ||
		snapshot.OutstandingPacketCount != 0 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("canonical destination snapshot=%+v", snapshot)
	}
}

// Retiring an endpoint owner also removes its wildcard listeners. Reusing the
// same wildcard tuple binds a fresh owner generation, and exact-IP traffic can
// reserve only that replacement socket.
func TestP2pReceiveCreditsOwnerRetirementReplacesWildcardSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pDestinationReceiveCredits(1)
	defer credits.close()
	wildcardAddress := &net.UDPAddr{IP: net.IPv4zero, Port: 5004}
	exactAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 3), Port: 5004}
	oldOwner := &p2pLinkNet{}
	newOwner := &p2pLinkNet{}
	oldSocket := credits.registerSocket(wildcardAddress, nil, oldOwner)
	credits.retireOwner(oldOwner)
	newSocket := credits.registerSocket(wildcardAddress, nil, newOwner)
	reservation, hasDestination, admitted := credits.reserveForAddress(ctx, exactAddress)
	if !hasDestination || !admitted || reservation == nil || reservation.socket != newSocket {
		t.Fatalf(
			"replacement wildcard reservation destination=%t admitted=%t reservation=%+v",
			hasDestination,
			admitted,
			reservation,
		)
	}
	reservation.cancel()
	oldSocket.close()
	newSocket.close()
	snapshot := credits.snapshot()
	if snapshot.AdmittedPacketCount != 1 || snapshot.CanceledPacketCount != 1 ||
		snapshot.OutstandingPacketCount != 0 || snapshot.TrackedReservationCount != 0 ||
		snapshot.InvalidReleasePacketCount != 0 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("replacement wildcard receive credits=%+v", snapshot)
	}
}

// Closing one destination wakes its blocked capacity acquisition even while a
// different socket retains the only credit. A same-address replacement is a
// distinct generation and cannot inherit the canceled wait.
func TestP2pReceiveCreditsSocketCloseCancelsBlockedAcquisition(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pDestinationReceiveCredits(1)
	defer credits.close()
	firstAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 5001}
	blockingAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 5002}
	firstSocket := credits.registerSocket(firstAddress, nil, nil)
	blockingSocket := credits.registerSocket(blockingAddress, nil, nil)
	firstReservation, _, admitted := credits.reserveForAddress(ctx, firstAddress)
	if !admitted || firstReservation == nil {
		t.Fatal("first socket did not retain the only receive credit")
	}
	waitEntered := make(chan struct{})
	var waitEnteredOnce sync.Once
	credits.beforeAcquireWaitForTest = func() {
		waitEnteredOnce.Do(func() { close(waitEntered) })
	}
	type reservationResult struct {
		reservation    *p2pReceiveCreditReservation
		hasDestination bool
		admitted       bool
	}
	blockedResult := make(chan reservationResult, 1)
	go func() {
		reservation, hasDestination, acquired := credits.reserveForAddress(ctx, blockingAddress)
		blockedResult <- reservationResult{
			reservation:    reservation,
			hasDestination: hasDestination,
			admitted:       acquired,
		}
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for socket-scoped capacity block: %v", ctx.Err())
	case <-waitEntered:
	}
	blockingSocket.beginClose()
	replacementSocket := credits.registerSocket(
		blockingAddress,
		nil,
		nil,
	)
	select {
	case <-ctx.Done():
		t.Fatalf("closed socket did not cancel blocked acquisition: %v", ctx.Err())
	case result := <-blockedResult:
		if result.reservation != nil || result.admitted {
			t.Fatalf("closed generation reservation=%+v", result)
		}
	}
	blockedSnapshot := credits.snapshot()
	if blockedSnapshot.PendingAcquireCount != 0 || blockedSnapshot.OutstandingPacketCount != 1 {
		t.Fatalf("closed-generation capacity snapshot=%+v", blockedSnapshot)
	}
	firstReservation.cancel()
	replacementReservation, hasDestination, replacementAdmitted := credits.reserveForAddress(
		ctx,
		blockingAddress,
	)
	if !hasDestination || !replacementAdmitted || replacementReservation == nil ||
		replacementReservation.socket != replacementSocket {
		t.Fatalf(
			"replacement reservation destination=%t admitted=%t reservation=%+v",
			hasDestination,
			replacementAdmitted,
			replacementReservation,
		)
	}
	replacementReservation.cancel()
	firstSocket.close()
	blockingSocket.close()
	replacementSocket.close()
}

// Pool shutdown between global admission and socket-token publication already
// owns that admission's cancellation; the resumed publisher cannot classify a
// second late release.
func TestP2pReceiveCreditsPoolCloseJoinsUnpublishedSocketReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pDestinationReceiveCredits(1)
	address := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 5005}
	credits.registerSocket(address, nil, nil)
	acquired := make(chan struct{})
	releasePublication := make(chan struct{})
	credits.afterSocketAcquireForTest = func() {
		close(acquired)
		<-releasePublication
	}
	result := make(chan bool, 1)
	go func() {
		_, _, admitted := credits.reserveForAddress(ctx, address)
		result <- admitted
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for unpublished socket reservation: %v", ctx.Err())
	case <-acquired:
	}
	credits.close()
	close(releasePublication)
	select {
	case <-ctx.Done():
		t.Fatalf("wait for closed unpublished socket reservation: %v", ctx.Err())
	case admitted := <-result:
		if admitted {
			t.Fatal("pool-close winner published a socket reservation")
		}
	}
	snapshot := credits.snapshot()
	if !snapshot.Closed || snapshot.AdmittedPacketCount != 1 ||
		snapshot.CanceledPacketCount != 1 || snapshot.OutstandingPacketCount != 0 ||
		snapshot.TrackedReservationCount != 0 || snapshot.InvalidReleasePacketCount != 0 ||
		snapshot.LateReleaseAfterCloseCount != 0 {
		t.Fatalf("closed unpublished reservation snapshot=%+v", snapshot)
	}
}

// Delegate failures remove their exact inactive queue nodes, so a live socket
// cannot retain an unbounded reservation slice across repeated failed writes.
func TestP2pReceiveCreditsCanceledReservationsDoNotAccumulate(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pDestinationReceiveCredits(1)
	defer credits.close()
	address := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 5006}
	socket := credits.registerSocket(address, nil, nil)
	for range 2 * p2pVnetReceiveQueuePacketCount {
		reservation, hasDestination, admitted := credits.reserveForAddress(ctx, address)
		if !hasDestination || !admitted || reservation == nil {
			t.Fatal("failed-write reservation was not admitted")
		}
		reservation.cancel()
	}
	credits.stateLock.Lock()
	queuedReservationCount := len(socket.reservations)
	credits.stateLock.Unlock()
	snapshot := credits.snapshot()
	if queuedReservationCount != 0 || snapshot.TrackedReservationCount != 0 ||
		snapshot.OutstandingPacketCount != 0 ||
		snapshot.AdmittedPacketCount != 2*p2pVnetReceiveQueuePacketCount ||
		snapshot.CanceledPacketCount != 2*p2pVnetReceiveQueuePacketCount ||
		!snapshot.isExactLiveTerminal() {
		t.Fatalf(
			"canceled reservation queue=%d snapshot=%+v",
			queuedReservationCount,
			snapshot,
		)
	}
	socket.close()
}

// A measured interval subtracts monotonic dispositions and uses the exact
// workload epoch high-water mark when both boundaries identify the same epoch.
func subtractP2pReceiveCreditSnapshots(
	before p2pReceiveCreditSnapshot,
	after p2pReceiveCreditSnapshot,
) p2pReceiveCreditSnapshot {
	maximumOutstandingPackets := 0
	if before.measurementMaximumEpoch != nil &&
		before.measurementMaximumEpoch == after.measurementMaximumEpoch {
		maximumOutstandingPackets = after.measurementMaximumPackets
	}
	return p2pReceiveCreditSnapshot{
		CapacityPacketCount:        after.CapacityPacketCount,
		AdmittedPacketCount:        after.AdmittedPacketCount - before.AdmittedPacketCount,
		ReadPacketCount:            after.ReadPacketCount - before.ReadPacketCount,
		CanceledPacketCount:        after.CanceledPacketCount - before.CanceledPacketCount,
		OutstandingPacketCount:     after.OutstandingPacketCount - before.OutstandingPacketCount,
		PendingAcquireCount:        after.PendingAcquireCount - before.PendingAcquireCount,
		MaximumOutstandingPackets:  maximumOutstandingPackets,
		BlockedAcquireCount:        after.BlockedAcquireCount - before.BlockedAcquireCount,
		InvalidReleasePacketCount:  after.InvalidReleasePacketCount - before.InvalidReleasePacketCount,
		LateReleaseAfterCloseCount: after.LateReleaseAfterCloseCount - before.LateReleaseAfterCloseCount,
		StaleGenerationDropCount:   after.StaleGenerationDropCount - before.StaleGenerationDropCount,
		Closed:                     after.Closed,
		DestinationScoped:          after.DestinationScoped,
		TrackedReservationCount:    after.TrackedReservationCount - before.TrackedReservationCount,
		RouterPendingPacketCount:   after.RouterPendingPacketCount - before.RouterPendingPacketCount,
	}
}

// A live terminal snapshot has no unconsumed, duplicate, or post-close
// disposition and balances every admitted datagram exactly.
func (self p2pReceiveCreditSnapshot) isExactLiveTerminal() bool {
	if self.Closed || self.OutstandingPacketCount != 0 || self.PendingAcquireCount != 0 ||
		self.RouterPendingPacketCount != 0 ||
		self.InvalidReleasePacketCount != 0 || self.LateReleaseAfterCloseCount != 0 {
		return false
	}
	return self.AdmittedPacketCount == self.ReadPacketCount+self.CanceledPacketCount &&
		(!self.DestinationScoped || self.TrackedReservationCount == 0)
}

// A quiescent socket backlog may remain unread, but every admitted datagram
// must still be represented exactly once as read, canceled, or outstanding.
func (self p2pReceiveCreditSnapshot) isExactLiveQuiescent() bool {
	if self.Closed || self.OutstandingPacketCount < 0 || self.PendingAcquireCount != 0 ||
		self.RouterPendingPacketCount != 0 ||
		self.InvalidReleasePacketCount != 0 || self.LateReleaseAfterCloseCount != 0 {
		return false
	}
	return self.AdmittedPacketCount == self.ReadPacketCount+
		self.CanceledPacketCount+uint64(self.OutstandingPacketCount) &&
		(!self.DestinationScoped ||
			self.TrackedReservationCount == self.OutstandingPacketCount)
}

// A measurement begins over a stable unread socket queue instead of waiting
// forever for Pion to consume optional control datagrams. The starting queue
// depth is retained in the interval high-water epoch.
func TestP2pReceiveCreditsBeginMeasurementRetainsStableSocketBacklog(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pReceiveCredits(4)
	defer credits.close()
	if !credits.acquire(ctx) {
		t.Fatal("stable socket backlog admission was rejected")
	}

	snapshot, ok := credits.beginMeasurementSnapshot(ctx)
	if !ok {
		t.Fatalf("stable socket backlog blocked measurement start: %v", ctx.Err())
	}
	if snapshot.OutstandingPacketCount != 1 || snapshot.PendingAcquireCount != 0 ||
		snapshot.measurementMaximumEpoch == nil || snapshot.measurementMaximumPackets != 1 ||
		!snapshot.isExactLiveQuiescent() {
		t.Fatalf("stable socket backlog measurement snapshot=%+v", snapshot)
	}
	credits.recordRead(1, nil)
}

// A receive/admission pair crossing the first quiescent candidate changes
// exact counters even when queue depth returns to one. The fixed point retries
// and accepts only the next unchanged generation.
func TestWaitForP2pCarrierQuiescentRetriesStableDepthGenerationChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pReceiveCredits(4)
	defer credits.close()
	if !credits.acquire(ctx) {
		t.Fatal("initial quiescent backlog admission was rejected")
	}
	attemptCount := 0
	joined := waitForP2pCarrierQuiescent(
		ctx,
		nil,
		[]*p2pReceiveCredits{credits},
		func() {
			attemptCount += 1
			if attemptCount == 1 {
				credits.recordRead(1, nil)
				if !credits.acquire(ctx) {
					t.Fatal("replacement quiescent backlog admission was rejected")
				}
			}
		},
	)
	if !joined || attemptCount != 2 {
		t.Fatalf("quiescent generation join=(%t, attempts=%d), want (true, 2)", joined, attemptCount)
	}
	snapshot := credits.snapshot()
	if snapshot.AdmittedPacketCount != 2 || snapshot.ReadPacketCount != 1 ||
		snapshot.OutstandingPacketCount != 1 || !snapshot.isExactLiveQuiescent() {
		t.Fatalf("replacement quiescent backlog snapshot=%+v", snapshot)
	}
	credits.recordRead(1, nil)
}

// Carrier quiescence cannot freeze while a framed write remains router-pending
// before final generation revalidation, even though its receive reservation is
// a valid stable unread backlog.
func TestWaitForP2pCarrierQuiescentJoinsRouterCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pVnetReceiveCredits(1)
	defer credits.close()
	localAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 5781}
	remoteAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 5782}
	socket := credits.registerSocket(localAddress, remoteAddress, nil)
	defer socket.close()
	reservation, hasDestination, admitted := credits.reserveForTransfer(
		ctx,
		remoteAddress,
		localAddress,
	)
	if !hasDestination || !admitted || reservation == nil {
		t.Fatal("router-completion carrier reservation was not admitted")
	}
	outerPacket := make([]byte, p2pIPv4UDPHeaderByteCount+1)
	if _, framed := reservation.framePayload(outerPacket); !framed {
		t.Fatal("router-completion carrier frame was not created")
	}
	waitEntered := make(chan struct{})
	var waitEnteredOnce sync.Once
	credits.routerPendingObservedForTest = func() {
		waitEnteredOnce.Do(func() { close(waitEntered) })
	}
	joined := make(chan bool, 1)
	go func() {
		joined <- waitForP2pCarrierQuiescent(
			ctx,
			nil,
			[]*p2pReceiveCredits{credits},
			nil,
		)
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for router-completion carrier barrier: %v", ctx.Err())
	case <-waitEntered:
	}
	select {
	case result := <-joined:
		t.Fatalf("carrier quiescence returned before router completion: %t", result)
	default:
	}
	credits.completeRouterPayload(socket.generation)
	select {
	case <-ctx.Done():
		t.Fatalf("join router-completion carrier barrier: %v", ctx.Err())
	case result := <-joined:
		if !result {
			t.Fatal("carrier quiescence rejected completed router generation")
		}
	}
	reservation.cancel()
}

// A permanently invalid stable generation waits on a rate-limited retry edge
// instead of spinning through fixed-point attempts until context expiration.
func TestWaitForP2pCarrierQuiescentRateLimitsInvalidGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	credits := newP2pReceiveCredits(1)
	defer credits.close()
	credits.recordRead(1, nil)
	retryEntered := make(chan struct{})
	releaseRetry := make(chan struct{})
	var retryOnce sync.Once
	credits.beforeQuiescentRetryWaitForTest = func() {
		retryOnce.Do(func() { close(retryEntered) })
	}
	credits.quiescentRetryWaitForTest = releaseRetry
	attemptCount := 0
	joined := make(chan bool, 1)
	go func() {
		joined <- waitForP2pCarrierQuiescent(
			ctx,
			nil,
			[]*p2pReceiveCredits{credits},
			func() { attemptCount += 1 },
		)
	}()
	select {
	case <-t.Context().Done():
		t.Fatalf("wait for invalid-generation retry barrier: %v", t.Context().Err())
	case <-retryEntered:
	}
	if attemptCount != 1 {
		t.Fatalf("attempts before retry release=%d, want 1", attemptCount)
	}
	cancel()
	select {
	case <-t.Context().Done():
		t.Fatalf("wait for canceled invalid-generation join: %v", t.Context().Err())
	case result := <-joined:
		if result {
			t.Fatal("invalid receive-credit generation was accepted")
		}
	}
}

// A stable receive generation ignores only the measurement epoch identity;
// all lifetime counts, queue ownership, and high-water observations must be
// unchanged across the carrier fixed point.
func p2pReceiveCreditGenerationStable(
	before p2pReceiveCreditSnapshot,
	after p2pReceiveCreditSnapshot,
) bool {
	before.measurementMaximumEpoch = nil
	before.measurementMaximumPackets = 0
	after.measurementMaximumEpoch = nil
	after.measurementMaximumPackets = 0
	return before == after && after.isExactLiveQuiescent()
}

// A failed fixed-point candidate always yields before retrying. Tests may
// replace the timer with an explicit barrier; production uses a short poll
// because one fixed point can span several independent credit pools.
func waitForP2pCarrierQuiescentRetry(
	ctx context.Context,
	creditPools []*p2pReceiveCredits,
) bool {
	var retryWait <-chan struct{}
	for _, credits := range creditPools {
		if credits == nil {
			continue
		}
		if credits.beforeQuiescentRetryWaitForTest != nil {
			credits.beforeQuiescentRetryWaitForTest()
		}
		if credits.quiescentRetryWaitForTest != nil {
			retryWait = credits.quiescentRetryWaitForTest
			break
		}
	}
	if retryWait != nil {
		select {
		case <-ctx.Done():
			return false
		case <-retryWait:
			return true
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Millisecond):
		return true
	}
}

// Physical carrier quiescence joins every link scheduler while allowing a
// stable unread UDP socket backlog. The caller's surrounding source and Pack
// generations detect work admitted after this local linearization point.
func waitForP2pCarrierQuiescent(
	ctx context.Context,
	links []*directionalLink,
	creditPools []*p2pReceiveCredits,
	afterIdle func(),
) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		linkSubmissions := make([]uint64, len(links))
		for linkIndex, link := range links {
			linkSubmissions[linkIndex] = link.submittedPackets.Load()
		}
		creditBefore := make([]p2pReceiveCreditSnapshot, len(creditPools))
		for creditIndex, credits := range creditPools {
			creditBefore[creditIndex] = credits.snapshot()
		}
		for _, link := range links {
			if !link.waitIdle(ctx) {
				return false
			}
		}
		for _, credits := range creditPools {
			if !credits.waitRouterIdle(ctx) {
				return false
			}
		}
		if afterIdle != nil {
			afterIdle()
		}
		stable := true
		for creditIndex, credits := range creditPools {
			if !p2pReceiveCreditGenerationStable(
				creditBefore[creditIndex],
				credits.snapshot(),
			) {
				stable = false
				break
			}
		}
		if !stable {
			if !waitForP2pCarrierQuiescentRetry(ctx, creditPools) {
				return false
			}
			continue
		}
		for linkIndex, link := range links {
			if !link.isTerminalAtSubmissionCount(linkSubmissions[linkIndex]) {
				stable = false
				break
			}
		}
		if !stable {
			if !waitForP2pCarrierQuiescentRetry(ctx, creditPools) {
				return false
			}
			continue
		}
		// A receive can race the link check without changing its submission
		// generation. Recheck every credit generation before accepting.
		for creditIndex, credits := range creditPools {
			if !p2pReceiveCreditGenerationStable(
				creditBefore[creditIndex],
				credits.snapshot(),
			) {
				stable = false
				break
			}
		}
		if stable {
			return true
		}
		if !waitForP2pCarrierQuiescentRetry(ctx, creditPools) {
			return false
		}
	}
}

// The fixed point joins every directional scheduler and destination credit
// pool, then retries if either admitted a new generation during that pass.
func waitForP2pTerminalIdle(
	ctx context.Context,
	links []*directionalLink,
	creditPools []*p2pReceiveCredits,
	afterIdle func(),
) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		linkSubmissions := make([]uint64, len(links))
		for linkIndex, link := range links {
			linkSubmissions[linkIndex] = link.submittedPackets.Load()
		}
		creditAdmissions := make([]uint64, len(creditPools))
		for creditIndex, credits := range creditPools {
			creditAdmissions[creditIndex] = credits.snapshot().AdmittedPacketCount
		}
		for _, link := range links {
			if !link.waitIdle(ctx) {
				return false
			}
		}
		for _, credits := range creditPools {
			if !credits.waitRouterIdle(ctx) {
				return false
			}
		}
		for _, credits := range creditPools {
			if !credits.waitIdle(ctx) {
				return false
			}
		}
		if afterIdle != nil {
			afterIdle()
		}
		stable := true
		for linkIndex, link := range links {
			if !link.isTerminalAtSubmissionCount(linkSubmissions[linkIndex]) {
				stable = false
				break
			}
		}
		if !stable {
			continue
		}
		for creditIndex, credits := range creditPools {
			snapshot := credits.snapshot()
			if snapshot.AdmittedPacketCount != creditAdmissions[creditIndex] {
				stable = false
				break
			}
			// A registered attempt can begin after waitIdle observed the old
			// generation but before this snapshot. Retry so the next pass joins
			// it instead of treating live capacity backpressure as corruption.
			if snapshot.PendingAcquireCount != 0 {
				if credits.pendingAcquireObservedForTest != nil {
					credits.pendingAcquireObservedForTest()
				}
				stable = false
				break
			}
			if !snapshot.isExactLiveTerminal() {
				return false
			}
		}
		if stable {
			return true
		}
	}
}

// The combined link-and-credit fixed point also retries a link reservation held
// before its submission count publication; empty credit pools cannot hide it.
func TestWaitForP2pTerminalIdleRetriesReservedUnpublishedLinkSubmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	link := newDirectionalLink(ctx, profile, 7002, nil)
	defer link.close()

	dropEntered := make(chan struct{})
	releaseDrop := make(chan struct{})
	link.afterImmediateDropForTest = func() {
		close(dropEntered)
		select {
		case <-releaseDrop:
		case <-ctx.Done():
		}
	}
	retryEntered := make(chan struct{})
	var retryEnteredOnce sync.Once
	link.beforeIdleWaitForTest = func() {
		retryEnteredOnce.Do(func() { close(retryEntered) })
	}
	var injectOnce sync.Once
	submitResult := make(chan error, 1)
	joined := make(chan bool, 1)
	go func() {
		joined <- waitForP2pTerminalIdle(
			ctx,
			[]*directionalLink{link},
			nil,
			func() {
				injectOnce.Do(func() {
					go func() {
						_, err := link.submit(make([]byte, profile.OuterMtu+1))
						submitResult <- err
					}()
					select {
					case <-dropEntered:
					case <-ctx.Done():
					}
				})
			},
		)
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("P2P terminal join did not retry reserved link submission: %v", ctx.Err())
	case <-retryEntered:
	}
	select {
	case terminal := <-joined:
		t.Fatalf("P2P terminal join returned through reserved link submission: %t", terminal)
	default:
	}
	close(releaseDrop)
	select {
	case <-ctx.Done():
		t.Fatalf("reserved P2P link submission did not finish: %v", ctx.Err())
	case err := <-submitResult:
		if err != nil {
			t.Fatalf("submit P2P immediate MTU drop: %v", err)
		}
	}
	select {
	case <-ctx.Done():
		t.Fatalf("P2P terminal join did not finish after exact submission: %v", ctx.Err())
	case terminal := <-joined:
		if !terminal {
			t.Fatal("P2P terminal join rejected completed immediate drop")
		}
	}
}

// Closing a full pool deterministically cancels its held reservation and wakes
// a sender parked at the exact capacity edge.
func TestP2pReceiveCreditsCloseUnblocksCapacityWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	credits := newP2pReceiveCredits(1)
	defer credits.close()
	if !credits.acquire(ctx) {
		t.Fatal("first receive credit was not admitted")
	}
	waitEntered := make(chan struct{})
	var waitEnteredOnce sync.Once
	credits.beforeAcquireWaitForTest = func() {
		waitEnteredOnce.Do(func() { close(waitEntered) })
	}
	acquired := make(chan bool, 1)
	go func() {
		acquired <- credits.acquire(ctx)
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for blocked receive-credit acquisition: %v", ctx.Err())
	case <-waitEntered:
	}
	beforeClose := credits.snapshot()
	if beforeClose.AdmittedPacketCount != 1 || beforeClose.OutstandingPacketCount != 1 ||
		beforeClose.PendingAcquireCount != 1 || beforeClose.BlockedAcquireCount != 1 {
		t.Fatalf("full receive-credit snapshot=%+v", beforeClose)
	}
	credits.close()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for close-unblocked acquisition: %v", ctx.Err())
	case admitted := <-acquired:
		if admitted {
			t.Fatal("receive credit was admitted after close")
		}
	}
	afterClose := credits.snapshot()
	if !afterClose.Closed || afterClose.AdmittedPacketCount != 1 ||
		afterClose.CanceledPacketCount != 1 || afterClose.OutstandingPacketCount != 0 ||
		afterClose.PendingAcquireCount != 0 || afterClose.InvalidReleasePacketCount != 0 {
		t.Fatalf("closed receive-credit snapshot=%+v", afterClose)
	}
}

// A credit admission injected after the first idle pass forces the combined
// terminal boundary to retry and join that exact generation.
func TestWaitForP2pTerminalIdleRetriesCreditGenerationRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, testP2pLinkProfile(1500, oversizeModeDrop), 7001, nil)
	defer link.close()
	credits := newP2pReceiveCredits(1)
	defer credits.close()
	injected := make(chan struct{})
	var injectOnce sync.Once
	afterIdle := func() {
		injectOnce.Do(func() {
			if !credits.acquire(ctx) {
				t.Error("injected receive credit was not admitted")
				return
			}
			close(injected)
		})
	}
	joined := make(chan bool, 1)
	go func() {
		joined <- waitForP2pTerminalIdle(
			ctx,
			[]*directionalLink{link},
			[]*p2pReceiveCredits{credits},
			afterIdle,
		)
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for terminal-race admission: %v", ctx.Err())
	case <-injected:
	}
	blockedCtx, blockedCancel := context.WithCancel(context.Background())
	blockedCancel()
	if credits.waitIdle(blockedCtx) {
		t.Fatal("injected receive credit was reported idle before its read")
	}
	credits.recordRead(0, nil)
	select {
	case <-ctx.Done():
		t.Fatalf("wait for combined terminal boundary: %v", ctx.Err())
	case terminal := <-joined:
		if !terminal {
			t.Fatal("combined terminal boundary rejected an exact read disposition")
		}
	}
	snapshot := credits.snapshot()
	if snapshot.AdmittedPacketCount != 1 || snapshot.ReadPacketCount != 1 ||
		snapshot.OutstandingPacketCount != 0 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("terminal-race receive-credit snapshot=%+v", snapshot)
	}
}

// A registered capacity wait that begins after the first idle pass is joined
// as a new generation instead of being rejected as a terminal imbalance.
func TestWaitForP2pTerminalIdleRetriesPendingCreditGenerationRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	credits := newP2pReceiveCredits(1)
	defer credits.close()

	acquireBlocked := make(chan struct{})
	var acquireBlockedOnce sync.Once
	credits.beforeAcquireWaitForTest = func() {
		acquireBlockedOnce.Do(func() { close(acquireBlocked) })
	}
	pendingObserved := make(chan struct{})
	var pendingObservedOnce sync.Once
	credits.pendingAcquireObservedForTest = func() {
		pendingObservedOnce.Do(func() { close(pendingObserved) })
	}
	injected := make(chan struct{})
	acquireResult := make(chan bool, 1)
	var injectOnce sync.Once
	afterIdle := func() {
		injectOnce.Do(func() {
			// Holding the semaphore token exposes the registered-before-admitted
			// edge exactly; no scheduler timing decides whether it is observed.
			<-credits.available
			go func() {
				if !credits.acquire(ctx) {
					acquireResult <- false
					return
				}
				credits.recordRead(1, nil)
				acquireResult <- true
			}()
			select {
			case <-ctx.Done():
			case <-acquireBlocked:
			}
			close(injected)
		})
	}
	joined := make(chan bool, 1)
	go func() {
		joined <- waitForP2pTerminalIdle(
			ctx,
			nil,
			[]*p2pReceiveCredits{credits},
			afterIdle,
		)
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for pending credit injection: %v", ctx.Err())
	case <-injected:
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for pending-generation retry: %v", ctx.Err())
	case terminal := <-joined:
		t.Fatalf("terminal boundary returned before joining pending generation: %t", terminal)
	case <-pendingObserved:
	}
	credits.available <- struct{}{}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for pending credit completion: %v", ctx.Err())
	case acquired := <-acquireResult:
		if !acquired {
			t.Fatal("pending receive credit was not admitted")
		}
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for pending-generation terminal boundary: %v", ctx.Err())
	case terminal := <-joined:
		if !terminal {
			t.Fatal("pending credit generation was rejected instead of joined")
		}
	}
	snapshot := credits.snapshot()
	if snapshot.AdmittedPacketCount != 1 || snapshot.ReadPacketCount != 1 ||
		snapshot.OutstandingPacketCount != 0 || snapshot.PendingAcquireCount != 0 ||
		snapshot.BlockedAcquireCount != 1 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("pending-generation receive-credit snapshot=%+v", snapshot)
	}
}

// The adapter holds 1,023 tracked datagrams below Pion's fixed 1,024-datagram
// queue and backpressures the next unique datagram instead of risking a silent
// drop. Releasing the reader then joins and verifies every submitted identity.
func TestP2pLinkNetBackpressuresBeforePionVnetReadQueueOverflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	profile.BurstByteCount = 4 * 1024 * 1024
	profile.QueueByteCount = 4 * 1024 * 1024
	profile.QueuePacketCount = 2 * p2pVnetReceiveQueuePacketCount
	network, err := newP2pNetwork(networkProfile{
		Name:    "vnet-receive-capacity",
		Seed:    7002,
		Forward: profile,
		Reverse: profile,
	})
	if err != nil {
		t.Fatalf("create P2P receive-capacity network: %v", err)
	}
	defer network.close()
	receiver, err := network.right.ListenUDP(
		"udp4",
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 0},
	)
	if err != nil {
		t.Fatalf("listen on P2P receive-capacity network: %v", err)
	}
	defer receiver.Close()
	sender, err := network.left.DialUDP(
		"udp4",
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 0},
		receiver.LocalAddr().(*net.UDPAddr),
	)
	if err != nil {
		t.Fatalf("dial P2P receive-capacity network: %v", err)
	}
	defer sender.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := receiver.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set P2P receive-capacity deadline: %v", err)
		}
	}

	capacityBlocked := make(chan struct{})
	var capacityBlockedOnce sync.Once
	network.forwardReceiveCredits.beforeAcquireWaitForTest = func() {
		capacityBlockedOnce.Do(func() { close(capacityBlocked) })
	}
	packetCount := p2pVnetReceiveCreditPacketCount + 1
	for packetIndex := range packetCount {
		payload := make([]byte, 8)
		binary.BigEndian.PutUint64(payload, uint64(packetIndex))
		writtenByteCount, writeErr := sender.Write(payload)
		if writeErr != nil || writtenByteCount != len(payload) {
			t.Fatalf(
				"submit P2P receive-capacity packet %d bytes=%d err=%v",
				packetIndex,
				writtenByteCount,
				writeErr,
			)
		}
	}
	if !network.forwardReceiveCredits.waitForAdmittedPacketCount(
		ctx,
		p2pVnetReceiveCreditPacketCount,
	) {
		t.Fatalf("wait for full P2P receive-credit pool: %v", ctx.Err())
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for sender backpressure at vnet capacity: %v", ctx.Err())
	case <-capacityBlocked:
	}
	fullSnapshot := network.forwardReceiveCredits.snapshot()
	if fullSnapshot.AdmittedPacketCount != p2pVnetReceiveCreditPacketCount ||
		fullSnapshot.ReadPacketCount != 0 ||
		fullSnapshot.OutstandingPacketCount != p2pVnetReceiveCreditPacketCount ||
		fullSnapshot.PendingAcquireCount != 1 ||
		fullSnapshot.MaximumOutstandingPackets != p2pVnetReceiveCreditPacketCount ||
		fullSnapshot.BlockedAcquireCount != 1 {
		t.Fatalf("full P2P receive-credit snapshot=%+v", fullSnapshot)
	}

	seen := make([]bool, packetCount)
	for readIndex := range packetCount {
		payload := make([]byte, 8)
		readByteCount, _, readErr := receiver.ReadFromUDP(payload)
		if readErr != nil || readByteCount != len(payload) {
			t.Fatalf(
				"read P2P receive-capacity packet %d bytes=%d err=%v",
				readIndex,
				readByteCount,
				readErr,
			)
		}
		packetIndex := binary.BigEndian.Uint64(payload)
		if uint64(packetCount) <= packetIndex || seen[packetIndex] {
			t.Fatalf("P2P receive-capacity packet identity=%d seen=%t", packetIndex, packetIndex < uint64(packetCount) && seen[packetIndex])
		}
		seen[packetIndex] = true
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join P2P receive-capacity terminal boundary: %v", ctx.Err())
	}
	terminalSnapshot := network.forwardReceiveCredits.snapshot()
	if terminalSnapshot.AdmittedPacketCount != uint64(packetCount) ||
		terminalSnapshot.ReadPacketCount != uint64(packetCount) ||
		terminalSnapshot.OutstandingPacketCount != 0 ||
		terminalSnapshot.InvalidReleasePacketCount != 0 ||
		!terminalSnapshot.isExactLiveTerminal() {
		t.Fatalf("terminal P2P receive-credit snapshot=%+v", terminalSnapshot)
	}
	linkSnapshot := network.forwardLink.snapshot()
	if linkSnapshot.DeliveredPacketCount != uint64(packetCount) ||
		linkSnapshot.ReceiverDropPacketCount != 0 || linkSnapshot.QueueDropPacketCount != 0 {
		t.Fatalf("terminal P2P receive-capacity link=%+v", linkSnapshot)
	}
}

// A real vnet destination with no listening socket is rejected before the
// router write, so repeated successful caller admissions cannot consume the
// shared receive capacity or arrive after a later bind.
func TestP2pLinkNetNoListenerCannotCreatePhantomReceiveCredits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	profile.BurstByteCount = 4 * 1024 * 1024
	profile.QueueByteCount = 4 * 1024 * 1024
	profile.QueuePacketCount = 2 * p2pVnetReceiveQueuePacketCount
	profile.BurstByteCount = 4 * 1024 * 1024
	profile.QueueByteCount = 4 * 1024 * 1024
	profile.QueuePacketCount = 2 * p2pVnetReceiveQueuePacketCount
	network, err := newP2pNetwork(networkProfile{
		Name:    "vnet-missing-destination",
		Seed:    7003,
		Forward: profile,
		Reverse: profile,
	})
	if err != nil {
		t.Fatalf("create missing-destination P2P network: %v", err)
	}
	defer network.close()
	destination := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 59999}
	sender, err := network.left.DialUDP(
		"udp4",
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 0},
		destination,
	)
	if err != nil {
		t.Fatalf("dial missing P2P destination: %v", err)
	}
	defer sender.Close()
	for packetIndex := range p2pVnetReceiveQueuePacketCount {
		payload := make([]byte, 8)
		binary.BigEndian.PutUint64(payload, uint64(packetIndex))
		if writtenByteCount, writeErr := sender.Write(payload); writeErr != nil ||
			writtenByteCount != len(payload) {
			t.Fatalf("write missing-destination packet %d bytes=%d err=%v", packetIndex, writtenByteCount, writeErr)
		}
	}
	if !network.forwardLink.waitIdle(ctx) {
		t.Fatalf("join missing-destination scheduler: %v", ctx.Err())
	}
	missingSnapshot := network.forwardReceiveCredits.snapshot()
	if missingSnapshot.AdmittedPacketCount != 0 || missingSnapshot.OutstandingPacketCount != 0 ||
		missingSnapshot.PendingAcquireCount != 0 {
		t.Fatalf("missing destination consumed receive credits: %+v", missingSnapshot)
	}
	linkSnapshot := network.forwardLink.snapshot()
	if linkSnapshot.ReceiverDropPacketCount != p2pVnetReceiveQueuePacketCount ||
		linkSnapshot.DeliveredPacketCount != 0 {
		t.Fatalf("missing-destination link disposition=%+v", linkSnapshot)
	}

	receiver, err := network.right.ListenUDP("udp4", destination)
	if err != nil {
		t.Fatalf("bind formerly missing P2P destination: %v", err)
	}
	defer receiver.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := receiver.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set rebound destination deadline: %v", err)
		}
	}
	expected := []byte("after-bind")
	if writtenByteCount, writeErr := sender.Write(expected); writeErr != nil ||
		writtenByteCount != len(expected) {
		t.Fatalf("write rebound destination bytes=%d err=%v", writtenByteCount, writeErr)
	}
	payload := make([]byte, len(expected))
	readByteCount, _, err := receiver.ReadFromUDP(payload)
	if err != nil || readByteCount != len(expected) || string(payload) != string(expected) {
		t.Fatalf("read rebound destination bytes=%d payload=%q err=%v", readByteCount, payload, err)
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join rebound destination: %v", ctx.Err())
	}
	terminalSnapshot := network.forwardReceiveCredits.snapshot()
	if terminalSnapshot.AdmittedPacketCount != 1 || terminalSnapshot.ReadPacketCount != 1 ||
		terminalSnapshot.OutstandingPacketCount != 0 || !terminalSnapshot.isExactLiveTerminal() {
		t.Fatalf("rebound destination receive credits=%+v", terminalSnapshot)
	}
}

// Wrapper Close removes a former live tuple before returning. A capacity-sized
// burst to that closed listener is rejected without credits, and an explicit
// same-tuple rebind receives only traffic sent after the new generation.
func TestP2pLinkNetClosedListenerCannotCreatePhantomReceiveCredits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	profile.BurstByteCount = 4 * 1024 * 1024
	profile.QueueByteCount = 4 * 1024 * 1024
	profile.QueuePacketCount = 2 * p2pVnetReceiveQueuePacketCount
	network, err := newP2pNetwork(networkProfile{
		Name:    "vnet-closed-destination",
		Seed:    7006,
		Forward: profile,
		Reverse: profile,
	})
	if err != nil {
		t.Fatalf("create closed-destination P2P network: %v", err)
	}
	defer network.close()
	destination := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 59998}
	receiver, err := network.right.ListenUDP("udp4", destination)
	if err != nil {
		t.Fatalf("listen destination before close: %v", err)
	}
	if err := receiver.Close(); err != nil {
		t.Fatalf("close destination generation: %v", err)
	}
	sender, err := network.left.DialUDP(
		"udp4",
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 0},
		destination,
	)
	if err != nil {
		t.Fatalf("dial closed P2P destination: %v", err)
	}
	defer sender.Close()
	for packetIndex := range p2pVnetReceiveQueuePacketCount {
		payload := make([]byte, 8)
		binary.BigEndian.PutUint64(payload, uint64(packetIndex))
		if writtenByteCount, writeErr := sender.Write(payload); writeErr != nil ||
			writtenByteCount != len(payload) {
			t.Fatalf("write closed-destination packet %d bytes=%d err=%v", packetIndex, writtenByteCount, writeErr)
		}
	}
	if !network.forwardLink.waitIdle(ctx) {
		t.Fatalf("join closed-destination scheduler: %v", ctx.Err())
	}
	closedSnapshot := network.forwardReceiveCredits.snapshot()
	if closedSnapshot.AdmittedPacketCount != 0 || closedSnapshot.OutstandingPacketCount != 0 ||
		closedSnapshot.TrackedReservationCount != 0 || closedSnapshot.PendingAcquireCount != 0 {
		t.Fatalf("closed destination consumed receive credits: %+v", closedSnapshot)
	}

	reboundReceiver, err := network.right.ListenUDP("udp4", destination)
	if err != nil {
		t.Fatalf("rebind closed P2P destination: %v", err)
	}
	defer reboundReceiver.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := reboundReceiver.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set rebound closed-destination deadline: %v", err)
		}
	}
	expected := []byte("after-close-rebind")
	if writtenByteCount, writeErr := sender.Write(expected); writeErr != nil ||
		writtenByteCount != len(expected) {
		t.Fatalf("write rebound closed destination bytes=%d err=%v", writtenByteCount, writeErr)
	}
	payload := make([]byte, len(expected))
	readByteCount, _, err := reboundReceiver.ReadFromUDP(payload)
	if err != nil || readByteCount != len(expected) || string(payload) != string(expected) {
		t.Fatalf("read rebound closed destination bytes=%d payload=%q err=%v", readByteCount, payload, err)
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join rebound closed destination: %v", ctx.Err())
	}
	terminalSnapshot := network.forwardReceiveCredits.snapshot()
	if terminalSnapshot.AdmittedPacketCount != 1 || terminalSnapshot.ReadPacketCount != 1 ||
		terminalSnapshot.OutstandingPacketCount != 0 || !terminalSnapshot.isExactLiveTerminal() {
		t.Fatalf("rebound closed-destination credits=%+v", terminalSnapshot)
	}
}

// A datagram can pause inside the router filter immediately before final
// generation revalidation. Closing and rebinding its tuple while paused must
// make that revalidation reject the old-generation payload.
func TestP2pLinkNetRouterInflightPacketCannotCrossSocketGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	profile.BurstByteCount = 4 * 1024 * 1024
	profile.QueueByteCount = 4 * 1024 * 1024
	profile.QueuePacketCount = 2 * p2pVnetReceiveQueuePacketCount
	network, err := newP2pNetwork(networkProfile{
		Name:    "router-inflight-generation",
		Seed:    7007,
		Forward: profile,
		Reverse: profile,
	})
	if err != nil {
		t.Fatalf("create router-inflight P2P network: %v", err)
	}
	defer network.close()
	destination := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 5532}
	source := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 5531}
	oldPayload := []byte("old-generation")
	filterEntered := make(chan struct{})
	releaseFilter := make(chan struct{})
	var filterOnce sync.Once
	network.forwardReceiveCredits.beforeRouterCompletionForTest = func(payload []byte) {
		if len(payload) == p2pVnetGenerationHeaderByteCount+len(oldPayload) &&
			bytes.Equal(payload[p2pVnetGenerationHeaderByteCount:], oldPayload) {
			filterOnce.Do(func() { close(filterEntered) })
			<-releaseFilter
		}
	}
	oldReceiver, err := network.right.ListenUDP("udp4", destination)
	if err != nil {
		close(releaseFilter)
		t.Fatalf("listen old router-inflight destination: %v", err)
	}
	sender, err := network.left.DialUDP("udp4", source, destination)
	if err != nil {
		_ = oldReceiver.Close()
		close(releaseFilter)
		t.Fatalf("dial router-inflight destination: %v", err)
	}
	defer sender.Close()
	if writtenByteCount, writeErr := sender.Write(oldPayload); writeErr != nil ||
		writtenByteCount != len(oldPayload) {
		_ = oldReceiver.Close()
		close(releaseFilter)
		t.Fatalf("write old-generation packet bytes=%d err=%v", writtenByteCount, writeErr)
	}
	select {
	case <-ctx.Done():
		_ = oldReceiver.Close()
		close(releaseFilter)
		t.Fatalf("wait for router-inflight filter: %v", ctx.Err())
	case <-filterEntered:
	}
	if err := oldReceiver.Close(); err != nil {
		close(releaseFilter)
		t.Fatalf("close old router-inflight destination: %v", err)
	}
	newReceiver, err := network.right.ListenUDP("udp4", destination)
	if err != nil {
		close(releaseFilter)
		t.Fatalf("rebind router-inflight destination: %v", err)
	}
	defer newReceiver.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := newReceiver.SetReadDeadline(deadline); err != nil {
			close(releaseFilter)
			t.Fatalf("set replacement router-inflight deadline: %v", err)
		}
	}
	routerWaitEntered := make(chan struct{})
	var routerWaitOnce sync.Once
	network.forwardReceiveCredits.routerPendingObservedForTest = func() {
		routerWaitOnce.Do(func() { close(routerWaitEntered) })
	}
	terminalJoined := make(chan bool, 1)
	go func() {
		terminalJoined <- network.waitForTerminalIdle(ctx)
	}()
	select {
	case <-ctx.Done():
		close(releaseFilter)
		t.Fatalf("wait for router-pending terminal barrier: %v", ctx.Err())
	case <-routerWaitEntered:
	}
	select {
	case joined := <-terminalJoined:
		close(releaseFilter)
		t.Fatalf("terminal barrier returned before router completion: %t", joined)
	default:
	}
	capacityBlocked := make(chan struct{})
	var capacityBlockedOnce sync.Once
	network.forwardReceiveCredits.beforeAcquireWaitForTest = func() {
		capacityBlockedOnce.Do(func() { close(capacityBlocked) })
	}
	close(releaseFilter)
	select {
	case <-ctx.Done():
		t.Fatalf("join released router-pending terminal barrier: %v", ctx.Err())
	case joined := <-terminalJoined:
		if !joined {
			t.Fatal("released router-pending terminal barrier rejected exact state")
		}
	}
	currentPacketCount := p2pVnetReceiveQueuePacketCount
	for packetIndex := range currentPacketCount {
		currentPayload := make([]byte, 8)
		binary.BigEndian.PutUint64(currentPayload, uint64(packetIndex))
		if writtenByteCount, writeErr := sender.Write(currentPayload); writeErr != nil ||
			writtenByteCount != len(currentPayload) {
			t.Fatalf(
				"write current-generation packet %d bytes=%d err=%v",
				packetIndex,
				writtenByteCount,
				writeErr,
			)
		}
	}
	if !network.forwardReceiveCredits.waitForAdmittedPacketCount(
		ctx,
		1+p2pVnetReceiveCreditPacketCount,
	) {
		t.Fatalf("wait for rebound generation credit headroom: %v", ctx.Err())
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for rebound generation backpressure: %v", ctx.Err())
	case <-capacityBlocked:
	}
	heldSnapshot := network.forwardReceiveCredits.snapshot()
	if heldSnapshot.AdmittedPacketCount != uint64(1+p2pVnetReceiveCreditPacketCount) ||
		heldSnapshot.CanceledPacketCount != 1 ||
		heldSnapshot.OutstandingPacketCount != p2pVnetReceiveCreditPacketCount ||
		heldSnapshot.PendingAcquireCount != 1 || heldSnapshot.BlockedAcquireCount != 1 {
		t.Fatalf("router-inflight headroom credits=%+v", heldSnapshot)
	}
	seen := make([]bool, currentPacketCount)
	for readIndex := range currentPacketCount {
		payload := make([]byte, 8)
		readByteCount, _, readErr := newReceiver.ReadFromUDP(payload)
		if readErr != nil || readByteCount != len(payload) {
			t.Fatalf(
				"replacement generation read %d bytes=%d err=%v",
				readIndex,
				readByteCount,
				readErr,
			)
		}
		packetIndex := binary.BigEndian.Uint64(payload)
		if uint64(currentPacketCount) <= packetIndex || seen[packetIndex] {
			t.Fatalf(
				"replacement generation packet identity=%d seen=%t",
				packetIndex,
				packetIndex < uint64(currentPacketCount) && seen[packetIndex],
			)
		}
		seen[packetIndex] = true
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join router-inflight generation transfer: %v", ctx.Err())
	}
	snapshot := network.forwardReceiveCredits.snapshot()
	if snapshot.AdmittedPacketCount != uint64(1+currentPacketCount) ||
		snapshot.ReadPacketCount != uint64(currentPacketCount) ||
		snapshot.CanceledPacketCount != 1 || snapshot.StaleGenerationDropCount != 1 ||
		snapshot.OutstandingPacketCount != 0 || snapshot.TrackedReservationCount != 0 ||
		snapshot.InvalidReleasePacketCount != 0 || snapshot.LateReleaseAfterCloseCount != 0 ||
		!snapshot.isExactLiveTerminal() {
		t.Fatalf("router-inflight generation credits=%+v", snapshot)
	}
}

// One frame can pass final generation revalidation immediately before close
// and pause in Pion's filter-to-NIC handoff. The reserved physical slot and
// receiver frame check preserve all 1,024 current-generation packets while
// 1,023 tracked credits backpressure the final packet.
func TestP2pLinkNetPostGateStaleFramePreservesCurrentQueueCapacity(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	profile.BurstByteCount = 4 * 1024 * 1024
	profile.QueueByteCount = 4 * 1024 * 1024
	profile.QueuePacketCount = 2 * p2pVnetReceiveQueuePacketCount
	network, err := newP2pNetwork(networkProfile{
		Name:    "post-gate-generation-headroom",
		Seed:    7008,
		Forward: profile,
		Reverse: profile,
	})
	if err != nil {
		t.Fatalf("create post-gate generation network: %v", err)
	}
	defer network.close()
	source := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 5551}
	destination := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 5552}
	oldPayload := make([]byte, 8)
	binary.BigEndian.PutUint64(oldPayload, ^uint64(0))
	filterEntered := make(chan struct{})
	releaseFilter := make(chan struct{})
	var filterOnce sync.Once
	network.router.AddChunkFilter(func(chunk vnet.Chunk) bool {
		payload := chunk.UserData()
		if len(payload) == p2pVnetGenerationHeaderByteCount+len(oldPayload) &&
			bytes.Equal(payload[p2pVnetGenerationHeaderByteCount:], oldPayload) {
			filterOnce.Do(func() { close(filterEntered) })
			<-releaseFilter
		}
		return true
	})
	oldReceiver, err := network.right.ListenUDP("udp4", destination)
	if err != nil {
		close(releaseFilter)
		t.Fatalf("listen post-gate old generation: %v", err)
	}
	sender, err := network.left.DialUDP("udp4", source, destination)
	if err != nil {
		_ = oldReceiver.Close()
		close(releaseFilter)
		t.Fatalf("dial post-gate generation: %v", err)
	}
	defer sender.Close()
	if writtenByteCount, writeErr := sender.Write(oldPayload); writeErr != nil ||
		writtenByteCount != len(oldPayload) {
		_ = oldReceiver.Close()
		close(releaseFilter)
		t.Fatalf("write post-gate old generation bytes=%d err=%v", writtenByteCount, writeErr)
	}
	select {
	case <-ctx.Done():
		_ = oldReceiver.Close()
		close(releaseFilter)
		t.Fatalf("wait for post-gate filter: %v", ctx.Err())
	case <-filterEntered:
	}
	if err := oldReceiver.Close(); err != nil {
		close(releaseFilter)
		t.Fatalf("close post-gate old generation: %v", err)
	}
	newReceiver, err := network.right.ListenUDP("udp4", destination)
	if err != nil {
		close(releaseFilter)
		t.Fatalf("rebind post-gate generation: %v", err)
	}
	defer newReceiver.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := newReceiver.SetReadDeadline(deadline); err != nil {
			close(releaseFilter)
			t.Fatalf("set post-gate generation deadline: %v", err)
		}
	}
	capacityBlocked := make(chan struct{})
	var capacityBlockedOnce sync.Once
	network.forwardReceiveCredits.beforeAcquireWaitForTest = func() {
		capacityBlockedOnce.Do(func() { close(capacityBlocked) })
	}
	close(releaseFilter)
	currentPacketCount := p2pVnetReceiveQueuePacketCount
	for packetIndex := range currentPacketCount {
		payload := make([]byte, 8)
		binary.BigEndian.PutUint64(payload, uint64(packetIndex))
		if writtenByteCount, writeErr := sender.Write(payload); writeErr != nil ||
			writtenByteCount != len(payload) {
			t.Fatalf(
				"write post-gate current packet %d bytes=%d err=%v",
				packetIndex,
				writtenByteCount,
				writeErr,
			)
		}
	}
	if !network.forwardReceiveCredits.waitForAdmittedPacketCount(
		ctx,
		1+p2pVnetReceiveCreditPacketCount,
	) {
		t.Fatalf("wait for post-gate receive headroom: %v", ctx.Err())
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for post-gate receive backpressure: %v", ctx.Err())
	case <-capacityBlocked:
	}
	if !network.forwardReceiveCredits.waitRouterIdle(ctx) {
		t.Fatalf("join post-gate current router frames: %v", ctx.Err())
	}
	heldSnapshot := network.forwardReceiveCredits.snapshot()
	if heldSnapshot.CanceledPacketCount != 1 ||
		heldSnapshot.OutstandingPacketCount != p2pVnetReceiveCreditPacketCount ||
		heldSnapshot.PendingAcquireCount != 1 || heldSnapshot.RouterPendingPacketCount != 0 {
		t.Fatalf("post-gate receive headroom credits=%+v", heldSnapshot)
	}
	seen := make([]bool, currentPacketCount)
	for readIndex := range currentPacketCount {
		payload := make([]byte, 8)
		readByteCount, _, readErr := newReceiver.ReadFromUDP(payload)
		if readErr != nil || readByteCount != len(payload) {
			t.Fatalf(
				"read post-gate current packet %d bytes=%d err=%v",
				readIndex,
				readByteCount,
				readErr,
			)
		}
		packetIndex := binary.BigEndian.Uint64(payload)
		if uint64(currentPacketCount) <= packetIndex || seen[packetIndex] {
			t.Fatalf(
				"post-gate current packet identity=%d seen=%t",
				packetIndex,
				packetIndex < uint64(currentPacketCount) && seen[packetIndex],
			)
		}
		seen[packetIndex] = true
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join post-gate generation transfer: %v", ctx.Err())
	}
	snapshot := network.forwardReceiveCredits.snapshot()
	if snapshot.AdmittedPacketCount != uint64(1+currentPacketCount) ||
		snapshot.ReadPacketCount != uint64(currentPacketCount) ||
		snapshot.CanceledPacketCount != 1 || snapshot.StaleGenerationDropCount != 1 ||
		snapshot.OutstandingPacketCount != 0 || snapshot.TrackedReservationCount != 0 ||
		snapshot.RouterPendingPacketCount != 0 || snapshot.InvalidReleasePacketCount != 0 ||
		!snapshot.isExactLiveTerminal() {
		t.Fatalf("post-gate generation credits=%+v", snapshot)
	}
}

// A connected vnet socket accepts reservations only from its authenticated
// remote tuple. Wrong-source packets are receiver drops before vnet can
// silently dequeue them and strand the shared capacity.
func TestP2pLinkNetConnectedSocketRejectsWrongSourceBeforeVnet(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	profile.BurstByteCount = 4 * 1024 * 1024
	profile.QueueByteCount = 4 * 1024 * 1024
	profile.QueuePacketCount = 2 * p2pVnetReceiveQueuePacketCount
	network, err := newP2pNetwork(networkProfile{
		Name:    "vnet-connected-source",
		Seed:    7004,
		Forward: profile,
		Reverse: profile,
	})
	if err != nil {
		t.Fatalf("create connected-source P2P network: %v", err)
	}
	defer network.close()
	sourceA, err := network.left.ListenUDP(
		"udp4",
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 51001},
	)
	if err != nil {
		t.Fatalf("listen connected source A: %v", err)
	}
	defer sourceA.Close()
	sourceB, err := network.left.ListenUDP(
		"udp4",
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 51002},
	)
	if err != nil {
		t.Fatalf("listen connected source B: %v", err)
	}
	defer sourceB.Close()
	receiver, err := network.right.DialUDP(
		"udp4",
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 51003},
		sourceA.LocalAddr().(*net.UDPAddr),
	)
	if err != nil {
		t.Fatalf("dial connected receiver: %v", err)
	}
	defer receiver.Close()
	for packetIndex := range p2pVnetReceiveQueuePacketCount {
		payload := make([]byte, 8)
		binary.BigEndian.PutUint64(payload, uint64(packetIndex))
		if writtenByteCount, writeErr := sourceB.WriteToUDP(
			payload,
			receiver.LocalAddr().(*net.UDPAddr),
		); writeErr != nil || writtenByteCount != len(payload) {
			t.Fatalf("write wrong-source packet %d bytes=%d err=%v", packetIndex, writtenByteCount, writeErr)
		}
	}
	if !network.forwardLink.waitIdle(ctx) {
		t.Fatalf("join wrong-source scheduler: %v", ctx.Err())
	}
	wrongSourceCredits := network.forwardReceiveCredits.snapshot()
	if wrongSourceCredits.AdmittedPacketCount != 0 ||
		wrongSourceCredits.OutstandingPacketCount != 0 ||
		wrongSourceCredits.TrackedReservationCount != 0 {
		t.Fatalf("wrong source consumed receive credits: %+v", wrongSourceCredits)
	}
	expected := []byte("expected-source")
	if writtenByteCount, writeErr := sourceA.WriteToUDP(
		expected,
		receiver.LocalAddr().(*net.UDPAddr),
	); writeErr != nil || writtenByteCount != len(expected) {
		t.Fatalf("write expected-source packet bytes=%d err=%v", writtenByteCount, writeErr)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := receiver.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set connected receiver deadline: %v", err)
		}
	}
	payload := make([]byte, len(expected))
	readByteCount, _, err := receiver.ReadFromUDP(payload)
	if err != nil || readByteCount != len(expected) || string(payload) != string(expected) {
		t.Fatalf("read expected source bytes=%d payload=%q err=%v", readByteCount, payload, err)
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join connected expected source: %v", ctx.Err())
	}
	terminalCredits := network.forwardReceiveCredits.snapshot()
	if terminalCredits.AdmittedPacketCount != 1 || terminalCredits.ReadPacketCount != 1 ||
		terminalCredits.OutstandingPacketCount != 0 || !terminalCredits.isExactLiveTerminal() {
		t.Fatalf("connected expected-source credits=%+v", terminalCredits)
	}
	linkSnapshot := network.forwardLink.snapshot()
	if linkSnapshot.ReceiverDropPacketCount != p2pVnetReceiveQueuePacketCount ||
		linkSnapshot.DeliveredPacketCount != 1 {
		t.Fatalf("connected source link disposition=%+v", linkSnapshot)
	}
}

// A wildcard-bound sender must use its concrete vnet interface address for a
// connected receiver's source match. Both the direct and generalized stream
// constructors inject the endpoint address without probing transport.Net.
func TestP2pLinkNetWildcardSourceMatchesConnectedReceiver(t *testing.T) {
	type wildcardSourceFixture struct {
		name                string
		senderNet           transport.Net
		receiverNet         transport.Net
		senderIP            net.IP
		receiverIP          net.IP
		forwardCredits      *p2pReceiveCredits
		waitForTerminalIdle func(context.Context) bool
		close               func()
	}
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	directNetwork, err := newP2pNetwork(networkProfile{
		Name:    "wildcard-source-direct",
		Seed:    7005,
		Forward: profile,
		Reverse: profile,
	})
	if err != nil {
		t.Fatalf("create direct wildcard-source network: %v", err)
	}
	streamNetwork, err := newStreamP2pNetwork(networkProfile{
		Name:    "wildcard-source-stream",
		Seed:    7006,
		Forward: profile,
		Reverse: profile,
	}, 1)
	if err != nil {
		directNetwork.close()
		t.Fatalf("create stream wildcard-source network: %v", err)
	}
	fixtures := []wildcardSourceFixture{
		{
			name:                "direct",
			senderNet:           directNetwork.left,
			receiverNet:         directNetwork.right,
			senderIP:            net.IPv4(10, 240, 0, 1),
			receiverIP:          net.IPv4(10, 240, 0, 2),
			forwardCredits:      directNetwork.forwardReceiveCredits,
			waitForTerminalIdle: directNetwork.waitForTerminalIdle,
			close:               directNetwork.close,
		},
		{
			name:                "stream",
			senderNet:           streamNetwork.nets[0],
			receiverNet:         streamNetwork.nets[1],
			senderIP:            net.IPv4(10, 241, 0, 1),
			receiverIP:          net.IPv4(10, 241, 0, 2),
			forwardCredits:      streamNetwork.hopForwardReceiveCredits[0],
			waitForTerminalIdle: streamNetwork.waitForTerminalIdle,
			close:               streamNetwork.close,
		},
	}
	for fixtureIndex, fixture := range fixtures {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		senderPort := 5501 + 2*fixtureIndex
		receiverPort := senderPort + 1
		senderAddress := &net.UDPAddr{IP: net.IPv4zero, Port: senderPort}
		receiverAddress := &net.UDPAddr{IP: net.IPv4zero, Port: receiverPort}
		expectedSource := &net.UDPAddr{IP: fixture.senderIP, Port: senderPort}
		destination := &net.UDPAddr{IP: fixture.receiverIP, Port: receiverPort}
		receiver, listenErr := fixture.receiverNet.DialUDP(
			"udp4",
			receiverAddress,
			expectedSource,
		)
		if listenErr != nil {
			cancel()
			fixture.close()
			t.Fatalf("%s dial connected wildcard receiver: %v", fixture.name, listenErr)
		}
		sender, listenErr := fixture.senderNet.ListenUDP("udp4", senderAddress)
		if listenErr != nil {
			_ = receiver.Close()
			cancel()
			fixture.close()
			t.Fatalf("%s listen wildcard sender: %v", fixture.name, listenErr)
		}
		if deadline, ok := ctx.Deadline(); ok {
			if deadlineErr := receiver.SetReadDeadline(deadline); deadlineErr != nil {
				_ = sender.Close()
				_ = receiver.Close()
				cancel()
				fixture.close()
				t.Fatalf("%s set receiver deadline: %v", fixture.name, deadlineErr)
			}
		}
		payload := []byte("wildcard-source-" + fixture.name)
		if writtenByteCount, writeErr := sender.WriteToUDP(payload, destination); writeErr != nil ||
			writtenByteCount != len(payload) {
			_ = sender.Close()
			_ = receiver.Close()
			cancel()
			fixture.close()
			t.Fatalf(
				"%s wildcard-source write bytes=%d err=%v",
				fixture.name,
				writtenByteCount,
				writeErr,
			)
		}
		readPayload := make([]byte, len(payload))
		readByteCount, readErr := receiver.Read(readPayload)
		if readErr != nil || readByteCount != len(payload) || string(readPayload) != string(payload) {
			_ = sender.Close()
			_ = receiver.Close()
			cancel()
			fixture.close()
			t.Fatalf(
				"%s wildcard-source read bytes=%d payload=%q err=%v",
				fixture.name,
				readByteCount,
				readPayload,
				readErr,
			)
		}
		if !fixture.waitForTerminalIdle(ctx) {
			_ = sender.Close()
			_ = receiver.Close()
			cancel()
			fixture.close()
			t.Fatalf("%s join wildcard-source transfer: %v", fixture.name, ctx.Err())
		}
		snapshot := fixture.forwardCredits.snapshot()
		if snapshot.AdmittedPacketCount != 1 || snapshot.ReadPacketCount != 1 ||
			snapshot.OutstandingPacketCount != 0 || snapshot.TrackedReservationCount != 0 ||
			snapshot.InvalidReleasePacketCount != 0 || !snapshot.isExactLiveTerminal() {
			_ = sender.Close()
			_ = receiver.Close()
			cancel()
			fixture.close()
			t.Fatalf("%s wildcard-source credits=%+v", fixture.name, snapshot)
		}
		_ = sender.Close()
		_ = receiver.Close()
		cancel()
		fixture.close()
	}
}

// Pion ReadFromUDP wraps an empty deadline result in a non-timeout address
// error. That result consumes no queued token, both with an empty socket and
// with a deliberately held reservation.
func TestP2pLinkNetReadFromUDPDeadlineDoesNotReleaseReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	network, err := newP2pNetwork(networkProfile{
		Name:    "vnet-read-deadline",
		Seed:    7005,
		Forward: profile,
		Reverse: profile,
	})
	if err != nil {
		t.Fatalf("create read-deadline P2P network: %v", err)
	}
	defer network.close()
	source, err := network.left.ListenUDP(
		"udp4",
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 51101},
	)
	if err != nil {
		t.Fatalf("listen read-deadline source: %v", err)
	}
	defer source.Close()
	receiver, err := network.right.DialUDP(
		"udp4",
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 51102},
		source.LocalAddr().(*net.UDPAddr),
	)
	if err != nil {
		t.Fatalf("dial read-deadline receiver: %v", err)
	}
	defer receiver.Close()
	readTimeout := func() {
		if err := receiver.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
			t.Fatalf("set expired read deadline: %v", err)
		}
		readByteCount, _, readErr := receiver.ReadFromUDP(make([]byte, 1))
		if readErr == nil || 0 < readByteCount {
			t.Fatalf("expired ReadFromUDP bytes=%d err=%v", readByteCount, readErr)
		}
	}
	readTimeout()
	emptySnapshot := network.forwardReceiveCredits.snapshot()
	if emptySnapshot.InvalidReleasePacketCount != 0 || emptySnapshot.ReadPacketCount != 0 {
		t.Fatalf("empty timeout changed receive credits: %+v", emptySnapshot)
	}
	reservation, hasDestination, admitted := network.forwardReceiveCredits.reserveForTransfer(
		ctx,
		source.LocalAddr(),
		receiver.LocalAddr(),
	)
	if !hasDestination || !admitted || reservation == nil {
		t.Fatal("held timeout reservation was not admitted")
	}
	readTimeout()
	heldSnapshot := network.forwardReceiveCredits.snapshot()
	if heldSnapshot.ReadPacketCount != 0 || heldSnapshot.OutstandingPacketCount != 1 ||
		heldSnapshot.TrackedReservationCount != 1 || heldSnapshot.InvalidReleasePacketCount != 0 {
		t.Fatalf("held timeout changed receive credits: %+v", heldSnapshot)
	}
	reservation.cancel()
}

// A link-backed Net owns no sockets or link lifecycle. It only delays copied
// UDP writes through one direction's scheduler; all other operations delegate.
type p2pLinkNet struct {
	transport.Net
	link                         *directionalLink
	outboundCredits              *p2pReceiveCredits
	inboundCredits               *p2pReceiveCredits
	untrackedDestinationObserver func(net.Addr, net.Addr, []byte)
	sourceIPv4                   netip.Addr
}

// The caller owns the transport and link lifetimes.
func newP2pLinkNet(
	network transport.Net,
	link *directionalLink,
	receiveCredits ...*p2pReceiveCredits,
) *p2pLinkNet {
	return newP2pLinkNetWithUntrackedDestinationObserver(
		network,
		link,
		nil,
		receiveCredits...,
	)
}

// Extended topologies classify a rejected non-adjacent packet at the same
// pre-vnet boundary that prevents an unreserved router write.
func newP2pLinkNetWithUntrackedDestinationObserver(
	network transport.Net,
	link *directionalLink,
	untrackedDestinationObserver func(net.Addr, net.Addr, []byte),
	receiveCredits ...*p2pReceiveCredits,
) *p2pLinkNet {
	var outboundCredits *p2pReceiveCredits
	var inboundCredits *p2pReceiveCredits
	switch len(receiveCredits) {
	case 0:
	case 2:
		outboundCredits = receiveCredits[0]
		inboundCredits = receiveCredits[1]
	default:
		panic("P2P link net requires both outbound and inbound receive credits")
	}
	return &p2pLinkNet{
		Net:                          network,
		link:                         link,
		outboundCredits:              outboundCredits,
		inboundCredits:               inboundCredits,
		untrackedDestinationObserver: untrackedDestinationObserver,
	}
}

// Wildcard-bound vnet sockets emit from their endpoint's configured address.
// Construction paths inject that known address without probing arbitrary Net
// implementations or accidentally selecting vnet's loopback interface.
func (self *p2pLinkNet) setSourceIPv4ForWildcardBinds(sourceIPv4 netip.Addr) {
	sourceIPv4 = sourceIPv4.Unmap()
	if !sourceIPv4.Is4() || sourceIPv4.IsLoopback() || sourceIPv4.IsUnspecified() {
		panic(fmt.Sprintf("invalid P2P endpoint source IPv4 address %s", sourceIPv4))
	}
	self.sourceIPv4 = sourceIPv4
}

// Packet listeners preserve UDP impairment even when Pion uses net.PacketConn.
func (self *p2pLinkNet) ListenPacket(network string, address string) (net.PacketConn, error) {
	connection, err := self.Net.ListenPacket(network, address)
	if err != nil || !p2pUDPNetwork(network) {
		return connection, err
	}
	return self.wrapPacketConn(connection), nil
}

// A returned packet socket retains its most specific available interface.
func (self *p2pLinkNet) wrapPacketConn(connection net.PacketConn) net.PacketConn {
	if udpConnection, ok := connection.(transport.UDPConn); ok {
		return self.wrapUDP(udpConnection)
	}
	var receiveSocket *p2pReceiveCreditSocket
	if self.inboundCredits != nil {
		receiveSocket = self.inboundCredits.registerSocket(
			connection.LocalAddr(),
			nil,
			self,
		)
	}
	return &p2pLinkPacketConn{
		PacketConn:                   connection,
		link:                         self.link,
		outboundCredits:              self.outboundCredits,
		inboundCredits:               self.inboundCredits,
		receiveSocket:                receiveSocket,
		untrackedDestinationObserver: self.untrackedDestinationObserver,
		sourceIPv4:                   self.sourceIPv4,
	}
}

// UDP listeners retain the exact transport.UDPConn surface Pion expects.
func (self *p2pLinkNet) ListenUDP(
	network string,
	localAddress *net.UDPAddr,
) (transport.UDPConn, error) {
	connection, err := self.Net.ListenUDP(network, localAddress)
	if err != nil {
		return nil, err
	}
	if !p2pUDPNetwork(network) {
		return connection, nil
	}
	return self.wrapUDP(connection), nil
}

// Generic UDP dialing cannot bypass the directional scheduler.
func (self *p2pLinkNet) Dial(network string, address string) (net.Conn, error) {
	connection, err := self.Net.Dial(network, address)
	if err != nil || !p2pUDPNetwork(network) {
		return connection, err
	}
	udpConnection, ok := connection.(transport.UDPConn)
	if !ok {
		_ = connection.Close()
		return nil, transport.ErrNotSupported
	}
	return self.wrapUDP(udpConnection), nil
}

// Explicit UDP dialing uses the same wrapper as listeners.
func (self *p2pLinkNet) DialUDP(
	network string,
	localAddress *net.UDPAddr,
	remoteAddress *net.UDPAddr,
) (transport.UDPConn, error) {
	connection, err := self.Net.DialUDP(network, localAddress, remoteAddress)
	if err != nil {
		return nil, err
	}
	if !p2pUDPNetwork(network) {
		return connection, nil
	}
	return self.wrapUDP(connection), nil
}

// A Pion dialer routes future UDP sockets back through this Net.
func (self *p2pLinkNet) CreateDialer(dialer *net.Dialer) transport.Dialer {
	return &p2pLinkDialer{
		network:  self,
		delegate: self.Net.CreateDialer(dialer),
	}
}

// A Pion ListenConfig routes packet listeners back through this Net while
// preserving the delegate for unused stream-listener operations.
func (self *p2pLinkNet) CreateListenConfig(config *net.ListenConfig) transport.ListenConfig {
	return &p2pLinkListenConfig{
		network:  self,
		delegate: self.Net.CreateListenConfig(config),
	}
}

// Every socket receives the same directional link and opposing admission pools.
func (self *p2pLinkNet) wrapUDP(connection transport.UDPConn) *p2pLinkUDPConn {
	var receiveSocket *p2pReceiveCreditSocket
	if self.inboundCredits != nil {
		receiveSocket = self.inboundCredits.registerSocket(
			connection.LocalAddr(),
			connection.RemoteAddr(),
			self,
		)
	}
	return &p2pLinkUDPConn{
		UDPConn:                      connection,
		link:                         self.link,
		outboundCredits:              self.outboundCredits,
		inboundCredits:               self.inboundCredits,
		receiveSocket:                receiveSocket,
		untrackedDestinationObserver: self.untrackedDestinationObserver,
		sourceIPv4:                   self.sourceIPv4,
	}
}

// Only UDP carries Pion's packet path in this IPv4 topology.
func p2pUDPNetwork(network string) bool {
	return network == "udp" || network == "udp4"
}

// A transport dialer retains the caller's configuration for documentation;
// vnet's Dialer also resolves the operation through its owning Net.
type p2pLinkDialer struct {
	network  *p2pLinkNet
	delegate transport.Dialer
}

// Dial uses the wrapper identity so UDP cannot bypass impairment.
func (self *p2pLinkDialer) Dial(network string, address string) (net.Conn, error) {
	connection, err := self.delegate.Dial(network, address)
	if err != nil || !p2pUDPNetwork(network) {
		return connection, err
	}
	udpConnection, ok := connection.(transport.UDPConn)
	if !ok {
		_ = connection.Close()
		return nil, transport.ErrNotSupported
	}
	return self.network.wrapUDP(udpConnection), nil
}

// Packet listeners use the wrapper; stream listeners retain delegate behavior.
type p2pLinkListenConfig struct {
	network  *p2pLinkNet
	delegate transport.ListenConfig
}

// TCP is outside the P2P packet model.
func (self *p2pLinkListenConfig) Listen(
	ctx context.Context,
	network string,
	address string,
) (net.Listener, error) {
	return self.delegate.Listen(ctx, network, address)
}

// UDP packet listeners use the link-backed network.
func (self *p2pLinkListenConfig) ListenPacket(
	ctx context.Context,
	network string,
	address string,
) (net.PacketConn, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	connection, err := self.delegate.ListenPacket(ctx, network, address)
	if err != nil || !p2pUDPNetwork(network) {
		return connection, err
	}
	return self.network.wrapPacketConn(connection), nil
}

// A UDPConn delegates reads and socket controls but admits every write through
// one deterministic link. The directional link owns its copied bytes.
type p2pLinkUDPConn struct {
	transport.UDPConn
	link                         *directionalLink
	outboundCredits              *p2pReceiveCredits
	inboundCredits               *p2pReceiveCredits
	receiveSocket                *p2pReceiveCreditSocket
	untrackedDestinationObserver func(net.Addr, net.Addr, []byte)
	sourceIPv4                   netip.Addr
	closeOnce                    sync.Once
	closeErr                     error
}

// A wildcard bind uses the owning vnet endpoint's concrete IPv4 source, which
// is the tuple the router places on the emitted datagram.
func (self *p2pLinkUDPConn) sourceAddress() net.Addr {
	return p2pEffectiveSourceAddress(self.UDPConn.LocalAddr(), self.sourceIPv4)
}

// Destination eligibility is withdrawn before the delegate wakes blocked
// reads; exact reservations are canceled after the delegate stops accepting.
func (self *p2pLinkUDPConn) Close() error {
	self.closeOnce.Do(func() {
		self.receiveSocket.beginClose()
		self.closeErr = self.UDPConn.Close()
		self.receiveSocket.close()
	})
	return self.closeErr
}

// Connected writes retain the connected destination in the delegate.
func (self *p2pLinkUDPConn) Write(payload []byte) (int, error) {
	var destination net.Addr
	if self.outboundCredits != nil && self.outboundCredits.destinationScoped {
		destination = cloneP2pNetAddress(self.UDPConn.RemoteAddr())
	}
	return submitP2pUDPPayload(
		self.link,
		self.outboundCredits,
		destination,
		payload,
		self.untrackedDestinationObserver,
		self.sourceAddress,
		func(ownedPayload []byte) bool {
			writtenByteCount, err := self.UDPConn.Write(ownedPayload)
			return err == nil && writtenByteCount == len(ownedPayload)
		},
	)
}

// Generic-address writes capture an immutable destination for delayed release.
func (self *p2pLinkUDPConn) WriteTo(payload []byte, address net.Addr) (int, error) {
	ownedAddress := cloneP2pNetAddress(address)
	return submitP2pUDPPayload(
		self.link,
		self.outboundCredits,
		ownedAddress,
		payload,
		self.untrackedDestinationObserver,
		self.sourceAddress,
		func(ownedPayload []byte) bool {
			writtenByteCount, err := self.UDPConn.WriteTo(ownedPayload, ownedAddress)
			return err == nil && writtenByteCount == len(ownedPayload)
		},
	)
}

// UDP-address writes capture an immutable address for delayed release.
func (self *p2pLinkUDPConn) WriteToUDP(payload []byte, address *net.UDPAddr) (int, error) {
	ownedAddress := cloneP2pUDPAddress(address)
	return submitP2pUDPPayload(
		self.link,
		self.outboundCredits,
		ownedAddress,
		payload,
		self.untrackedDestinationObserver,
		self.sourceAddress,
		func(ownedPayload []byte) bool {
			writtenByteCount, err := self.UDPConn.WriteToUDP(ownedPayload, ownedAddress)
			return err == nil && writtenByteCount == len(ownedPayload)
		},
	)
}

// Message writes copy both the payload and ancillary bytes before returning.
func (self *p2pLinkUDPConn) WriteMsgUDP(
	payload []byte,
	oob []byte,
	address *net.UDPAddr,
) (int, int, error) {
	ownedAddress := cloneP2pUDPAddress(address)
	reservationAddress := ownedAddress
	if reservationAddress == nil && self.outboundCredits != nil &&
		self.outboundCredits.destinationScoped {
		if remoteAddress, ok := self.UDPConn.RemoteAddr().(*net.UDPAddr); ok {
			reservationAddress = cloneP2pUDPAddress(remoteAddress)
		}
	}
	ownedOob := append([]byte(nil), oob...)
	writtenByteCount, err := submitP2pUDPPayload(
		self.link,
		self.outboundCredits,
		reservationAddress,
		payload,
		self.untrackedDestinationObserver,
		self.sourceAddress,
		func(ownedPayload []byte) bool {
			writtenPayloadByteCount, writtenOobByteCount, writeErr := self.UDPConn.WriteMsgUDP(
				ownedPayload,
				ownedOob,
				ownedAddress,
			)
			return writeErr == nil && writtenPayloadByteCount == len(ownedPayload) &&
				writtenOobByteCount == len(ownedOob)
		},
	)
	if err != nil {
		return 0, 0, err
	}
	return writtenByteCount, len(oob), nil
}

// ICE's value-address write stays allocation-free while retaining the same
// delayed outer-link ownership and accounting as legacy address forms.
func (self *p2pLinkUDPConn) WriteToAddrPort(
	payload []byte,
	address netip.AddrPort,
) (int, error) {
	return submitP2pUDPPayload(
		self.link,
		self.outboundCredits,
		net.UDPAddrFromAddrPort(address),
		payload,
		self.untrackedDestinationObserver,
		self.sourceAddress,
		func(ownedPayload []byte) bool {
			if connection, ok := self.UDPConn.(interface {
				WriteToAddrPort([]byte, netip.AddrPort) (int, error)
			}); ok {
				writtenByteCount, err := connection.WriteToAddrPort(ownedPayload, address)
				return err == nil && writtenByteCount == len(ownedPayload)
			}
			writtenByteCount, err := self.UDPConn.WriteToUDP(
				ownedPayload,
				net.UDPAddrFromAddrPort(address),
			)
			return err == nil && writtenByteCount == len(ownedPayload)
		},
	)
}

// Newer concrete UDP callers use the same value-address impairment path.
func (self *p2pLinkUDPConn) WriteToUDPAddrPort(
	payload []byte,
	address netip.AddrPort,
) (int, error) {
	return submitP2pUDPPayload(
		self.link,
		self.outboundCredits,
		net.UDPAddrFromAddrPort(address),
		payload,
		self.untrackedDestinationObserver,
		self.sourceAddress,
		func(ownedPayload []byte) bool {
			if connection, ok := self.UDPConn.(interface {
				WriteToUDPAddrPort([]byte, netip.AddrPort) (int, error)
			}); ok {
				writtenByteCount, err := connection.WriteToUDPAddrPort(ownedPayload, address)
				return err == nil && writtenByteCount == len(ownedPayload)
			}
			writtenByteCount, err := self.UDPConn.WriteToUDP(
				ownedPayload,
				net.UDPAddrFromAddrPort(address),
			)
			return err == nil && writtenByteCount == len(ownedPayload)
		},
	)
}

// Registered real sockets retire their exact destination reservation. Focused
// delegate tests without a registry retain the pool-level compatibility path.
func (self *p2pLinkUDPConn) recordRead(
	readByteCount int,
	err error,
) {
	if self.receiveSocket != nil {
		self.receiveSocket.recordRead(readByteCount, err)
		return
	}
	if self.inboundCredits != nil {
		self.inboundCredits.recordRead(readByteCount, err)
	}
}

// Framed vnet reads borrow enough scratch space for the private generation
// header while credit-only unit delegates retain their original buffer.
func (self *p2pReceiveCreditSocket) readBuffer(payload []byte) ([]byte, bool) {
	if self == nil || self.credits == nil || !self.credits.generationFramed {
		return payload, false
	}
	byteCount := len(payload) + p2pVnetGenerationHeaderByteCount
	if pooled := self.credits.frameBufferPool.Get(); pooled != nil {
		buffer := pooled.([]byte)
		if byteCount <= cap(buffer) {
			return buffer[:byteCount], true
		}
	}
	return make([]byte, byteCount), true
}

// Scratch ownership returns only ordinary UDP-sized buffers to the pool.
func (self *p2pReceiveCreditSocket) releaseReadBuffer(buffer []byte, framed bool) {
	if !framed || self == nil || self.credits == nil ||
		p2pMaximumPooledGenerationFrameByteCount < cap(buffer) {
		return
	}
	self.credits.frameBufferPool.Put(buffer[:0])
}

// A current-generation frame is copied without its private header. Stale or
// malformed frames are consumed by the harness and the caller continues its
// blocking read for a packet owned by this socket generation.
func (self *p2pReceiveCreditSocket) finishRead(
	payload []byte,
	readBuffer []byte,
	readByteCount int,
	err error,
	framed bool,
) (int, error, bool) {
	if !framed || readByteCount <= 0 {
		return readByteCount, err, true
	}
	if len(readBuffer) < readByteCount {
		self.credits.recordStaleGenerationDrop()
		return 0, nil, false
	}
	generation, valid := p2pVnetPayloadGeneration(readBuffer[:readByteCount])
	if !valid || generation != self.generation {
		self.credits.recordStaleGenerationDrop()
		return 0, nil, false
	}
	payloadByteCount := readByteCount - p2pVnetGenerationHeaderByteCount
	copy(
		payload[:payloadByteCount],
		readBuffer[p2pVnetGenerationHeaderByteCount:readByteCount],
	)
	return payloadByteCount, err, true
}

// Connected reads release one directional vnet receive reservation.
func (self *p2pLinkUDPConn) Read(payload []byte) (int, error) {
	for {
		readBuffer, framed := self.receiveSocket.readBuffer(payload)
		readByteCount, err := self.UDPConn.Read(readBuffer)
		readByteCount, err, accepted := self.receiveSocket.finishRead(
			payload,
			readBuffer,
			readByteCount,
			err,
			framed,
		)
		self.receiveSocket.releaseReadBuffer(readBuffer, framed)
		if !accepted {
			continue
		}
		self.recordRead(readByteCount, err)
		return readByteCount, err
	}
}

// Generic packet reads preserve the delegate address and release exactly once.
func (self *p2pLinkUDPConn) ReadFrom(payload []byte) (int, net.Addr, error) {
	for {
		readBuffer, framed := self.receiveSocket.readBuffer(payload)
		readByteCount, address, err := self.UDPConn.ReadFrom(readBuffer)
		readByteCount, err, accepted := self.receiveSocket.finishRead(
			payload,
			readBuffer,
			readByteCount,
			err,
			framed,
		)
		self.receiveSocket.releaseReadBuffer(readBuffer, framed)
		if !accepted {
			continue
		}
		self.recordRead(readByteCount, err)
		return readByteCount, address, err
	}
}

// UDP-address reads share the same receive-admission disposition.
func (self *p2pLinkUDPConn) ReadFromUDP(payload []byte) (int, *net.UDPAddr, error) {
	for {
		readBuffer, framed := self.receiveSocket.readBuffer(payload)
		readByteCount, address, err := self.UDPConn.ReadFromUDP(readBuffer)
		readByteCount, err, accepted := self.receiveSocket.finishRead(
			payload,
			readBuffer,
			readByteCount,
			err,
			framed,
		)
		self.receiveSocket.releaseReadBuffer(readBuffer, framed)
		if !accepted {
			continue
		}
		self.recordRead(readByteCount, err)
		return readByteCount, address, err
	}
}

// Message reads consume one datagram regardless of ancillary-data length.
func (self *p2pLinkUDPConn) ReadMsgUDP(
	payload []byte,
	oob []byte,
) (int, int, int, *net.UDPAddr, error) {
	for {
		readBuffer, framed := self.receiveSocket.readBuffer(payload)
		readByteCount, readOobByteCount, flags, address, err := self.UDPConn.ReadMsgUDP(
			readBuffer,
			oob,
		)
		readByteCount, err, accepted := self.receiveSocket.finishRead(
			payload,
			readBuffer,
			readByteCount,
			err,
			framed,
		)
		self.receiveSocket.releaseReadBuffer(readBuffer, framed)
		if !accepted {
			continue
		}
		self.recordRead(readByteCount, err)
		return readByteCount, readOobByteCount, flags, address, err
	}
}

// Allocation-free ICE reads delegate when supported and otherwise adapt the
// transport UDP address while releasing one receive reservation.
func (self *p2pLinkUDPConn) ReadFromAddrPort(
	payload []byte,
) (int, netip.AddrPort, error) {
	if connection, ok := self.UDPConn.(interface {
		ReadFromAddrPort([]byte) (int, netip.AddrPort, error)
	}); ok {
		for {
			readBuffer, framed := self.receiveSocket.readBuffer(payload)
			readByteCount, address, err := connection.ReadFromAddrPort(readBuffer)
			readByteCount, err, accepted := self.receiveSocket.finishRead(
				payload,
				readBuffer,
				readByteCount,
				err,
				framed,
			)
			self.receiveSocket.releaseReadBuffer(readBuffer, framed)
			if !accepted {
				continue
			}
			self.recordRead(readByteCount, err)
			return readByteCount, address, err
		}
	}
	for {
		readBuffer, framed := self.receiveSocket.readBuffer(payload)
		readByteCount, address, err := self.UDPConn.ReadFromUDP(readBuffer)
		readByteCount, err, accepted := self.receiveSocket.finishRead(
			payload,
			readBuffer,
			readByteCount,
			err,
			framed,
		)
		self.receiveSocket.releaseReadBuffer(readBuffer, framed)
		if !accepted {
			continue
		}
		self.recordRead(readByteCount, err)
		if err != nil || address == nil {
			return readByteCount, netip.AddrPort{}, err
		}
		return readByteCount, address.AddrPort(), nil
	}
}

// The concrete UDP value-address read shares the ICE adapter above.
func (self *p2pLinkUDPConn) ReadFromUDPAddrPort(
	payload []byte,
) (int, netip.AddrPort, error) {
	if connection, ok := self.UDPConn.(interface {
		ReadFromUDPAddrPort([]byte) (int, netip.AddrPort, error)
	}); ok {
		for {
			readBuffer, framed := self.receiveSocket.readBuffer(payload)
			readByteCount, address, err := connection.ReadFromUDPAddrPort(readBuffer)
			readByteCount, err, accepted := self.receiveSocket.finishRead(
				payload,
				readBuffer,
				readByteCount,
				err,
				framed,
			)
			self.receiveSocket.releaseReadBuffer(readBuffer, framed)
			if !accepted {
				continue
			}
			self.recordRead(readByteCount, err)
			return readByteCount, address, err
		}
	}
	return self.ReadFromAddrPort(payload)
}

// A generic PacketConn fallback covers alternate Pion packet-listener paths.
type p2pLinkPacketConn struct {
	net.PacketConn
	link                         *directionalLink
	outboundCredits              *p2pReceiveCredits
	inboundCredits               *p2pReceiveCredits
	receiveSocket                *p2pReceiveCreditSocket
	untrackedDestinationObserver func(net.Addr, net.Addr, []byte)
	sourceIPv4                   netip.Addr
	closeOnce                    sync.Once
	closeErr                     error
}

// PacketConn wildcard binds share the same concrete vnet source resolution.
func (self *p2pLinkPacketConn) sourceAddress() net.Addr {
	return p2pEffectiveSourceAddress(self.PacketConn.LocalAddr(), self.sourceIPv4)
}

// Generic packet sockets share the same exact close disposition as UDPConn.
func (self *p2pLinkPacketConn) Close() error {
	self.closeOnce.Do(func() {
		self.receiveSocket.beginClose()
		self.closeErr = self.PacketConn.Close()
		self.receiveSocket.close()
	})
	return self.closeErr
}

// Generic packet reads release the receive reservation held by the sender.
func (self *p2pLinkPacketConn) ReadFrom(payload []byte) (int, net.Addr, error) {
	for {
		readBuffer, framed := self.receiveSocket.readBuffer(payload)
		readByteCount, address, err := self.PacketConn.ReadFrom(readBuffer)
		readByteCount, err, accepted := self.receiveSocket.finishRead(
			payload,
			readBuffer,
			readByteCount,
			err,
			framed,
		)
		self.receiveSocket.releaseReadBuffer(readBuffer, framed)
		if !accepted {
			continue
		}
		if self.receiveSocket != nil {
			self.receiveSocket.recordRead(readByteCount, err)
		} else if self.inboundCredits != nil {
			self.inboundCredits.recordRead(readByteCount, err)
		}
		return readByteCount, address, err
	}
}

// Delayed generic packet writes capture the destination before returning.
func (self *p2pLinkPacketConn) WriteTo(payload []byte, address net.Addr) (int, error) {
	ownedAddress := cloneP2pNetAddress(address)
	return submitP2pUDPPayload(
		self.link,
		self.outboundCredits,
		ownedAddress,
		payload,
		self.untrackedDestinationObserver,
		self.sourceAddress,
		func(ownedPayload []byte) bool {
			writtenByteCount, err := self.PacketConn.WriteTo(ownedPayload, ownedAddress)
			return err == nil && writtenByteCount == len(ownedPayload)
		},
	)
}

// Admission copies the payload, charges the full outer IPv4 UDP datagram, and
// translates the scheduler result back to net.Conn payload-byte semantics.
func submitP2pUDPPayload(
	link *directionalLink,
	outboundCredits *p2pReceiveCredits,
	destination net.Addr,
	payload []byte,
	untrackedDestinationObserver func(net.Addr, net.Addr, []byte),
	sourceAddress func() net.Addr,
	deliverPayload func([]byte) bool,
) (int, error) {
	packetByteCount := p2pIPv4UDPHeaderByteCount + len(payload)
	outerPacket := make([]byte, packetByteCount)
	copy(outerPacket[p2pIPv4UDPHeaderByteCount:], payload)
	_, err := link.submitOwnedWithDeliver(outerPacket, func(ownedOuterPacket []byte) bool {
		var ownedSourceAddress net.Addr
		if (outboundCredits != nil && outboundCredits.destinationScoped) ||
			untrackedDestinationObserver != nil {
			ownedSourceAddress = sourceAddress()
		}
		reservation, hasDestination, admitted := outboundCredits.reserveForTransfer(
			link.ctx,
			ownedSourceAddress,
			destination,
		)
		if !admitted {
			return false
		}
		if !hasDestination {
			if untrackedDestinationObserver != nil {
				untrackedDestinationObserver(
					ownedSourceAddress,
					destination,
					ownedOuterPacket[p2pIPv4UDPHeaderByteCount:],
				)
			}
			return false
		}
		deliveryPayload := ownedOuterPacket[p2pIPv4UDPHeaderByteCount:]
		if outboundCredits != nil && outboundCredits.generationFramed {
			var framed bool
			deliveryPayload, framed = reservation.framePayload(ownedOuterPacket)
			if !framed {
				reservation.cancel()
				return false
			}
		}
		accepted := deliverPayload(deliveryPayload)
		if !accepted {
			reservation.cancelRouterPayload()
			reservation.cancel()
		}
		return accepted
	})
	if err != nil {
		return 0, err
	}
	return len(payload), nil
}

// vnet selects its endpoint interface for a wildcard bind before emitting the
// chunk. Mirroring that selection keeps connected-source registry matching on
// the same tuple the receiver will observe.
func p2pEffectiveSourceAddress(address net.Addr, sourceIPv4 netip.Addr) net.Addr {
	udpAddress, ok := address.(*net.UDPAddr)
	if !ok || udpAddress == nil {
		return cloneP2pNetAddress(address)
	}
	ownedAddress := cloneP2pUDPAddress(udpAddress)
	if sourceIPv4.IsValid() && (ownedAddress.IP == nil || ownedAddress.IP.IsUnspecified()) {
		ownedAddress.IP = net.IP(sourceIPv4.AsSlice())
	}
	return ownedAddress
}

// UDP addresses are mutable structs containing mutable IP slices.
func cloneP2pUDPAddress(address *net.UDPAddr) *net.UDPAddr {
	if address == nil {
		return nil
	}
	ownedAddress := *address
	ownedAddress.IP = append(net.IP(nil), address.IP...)
	return &ownedAddress
}

// Pion uses UDP addresses; immutable value-like alternatives can pass through.
func cloneP2pNetAddress(address net.Addr) net.Addr {
	if udpAddress, ok := address.(*net.UDPAddr); ok {
		return cloneP2pUDPAddress(udpAddress)
	}
	return address
}

// A close/read delegate exposes exact barriers around the error or successful
// read that races wrapper Close. Unused UDP methods come from the embedding.
type p2pCloseReadUDPConn struct {
	transport.UDPConn
	localAddress  *net.UDPAddr
	remoteAddress *net.UDPAddr
	readByteCount int
	readErr       error
	readEntered   chan struct{}
	releaseRead   chan struct{}
	closeEntered  chan struct{}
	releaseClose  chan struct{}
	readOnce      sync.Once
	closeOnce     sync.Once
}

// A generic packet delegate exposes the same exact close barrier without the
// transport.UDPConn methods that would select the specialized wrapper.
type p2pCloseBarrierPacketConn struct {
	net.PacketConn
	localAddress *net.UDPAddr
	closeEntered chan struct{}
	releaseClose chan struct{}
	closeOnce    sync.Once
}

// The configured result is released independently from delegate Close return.
func (self *p2pCloseReadUDPConn) Read([]byte) (int, error) {
	self.readOnce.Do(func() { close(self.readEntered) })
	<-self.releaseRead
	return self.readByteCount, self.readErr
}

// The wrapper has already withdrawn destination eligibility when this begins.
func (self *p2pCloseReadUDPConn) Close() error {
	self.closeOnce.Do(func() { close(self.closeEntered) })
	<-self.releaseClose
	return nil
}

// The exact local tuple is registered in the destination credit pool.
func (self *p2pCloseReadUDPConn) LocalAddr() net.Addr {
	return self.localAddress
}

// The connected source tuple protects the receiver from wrong-source writes.
func (self *p2pCloseReadUDPConn) RemoteAddr() net.Addr {
	return self.remoteAddress
}

// Delegate close remains blocked until the test releases its lifecycle edge.
func (self *p2pCloseBarrierPacketConn) Close() error {
	self.closeOnce.Do(func() { close(self.closeEntered) })
	<-self.releaseClose
	return nil
}

// The exact local tuple is registered in the destination credit pool.
func (self *p2pCloseBarrierPacketConn) LocalAddr() net.Addr {
	return self.localAddress
}

// Both socket wrappers must withdraw destination eligibility before calling a
// delegate Close that can wake readers or block on external socket teardown.
func TestP2pLinkSocketCloseWithdrawsEligibilityBeforeDelegateClose(t *testing.T) {
	cases := []struct {
		name string
		wrap func(
			*p2pLinkNet,
			*net.UDPAddr,
			*net.UDPAddr,
			chan struct{},
			chan struct{},
		) io.Closer
	}{
		{
			name: "UDPConn",
			wrap: func(
				network *p2pLinkNet,
				localAddress *net.UDPAddr,
				remoteAddress *net.UDPAddr,
				closeEntered chan struct{},
				releaseClose chan struct{},
			) io.Closer {
				return network.wrapUDP(&p2pCloseReadUDPConn{
					localAddress:  localAddress,
					remoteAddress: remoteAddress,
					closeEntered:  closeEntered,
					releaseClose:  releaseClose,
				})
			},
		},
		{
			name: "PacketConn",
			wrap: func(
				network *p2pLinkNet,
				localAddress *net.UDPAddr,
				_ *net.UDPAddr,
				closeEntered chan struct{},
				releaseClose chan struct{},
			) io.Closer {
				return network.wrapPacketConn(&p2pCloseBarrierPacketConn{
					localAddress: localAddress,
					closeEntered: closeEntered,
					releaseClose: releaseClose,
				})
			},
		},
	}
	for caseIndex, testCase := range cases {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		credits := newP2pDestinationReceiveCredits(1)
		localAddress := &net.UDPAddr{
			IP:   net.IPv4(10, 240, 0, 2),
			Port: 53407 + caseIndex,
		}
		remoteAddress := &net.UDPAddr{
			IP:   net.IPv4(10, 240, 0, 1),
			Port: 53409 + caseIndex,
		}
		closeEntered := make(chan struct{})
		releaseClose := make(chan struct{})
		wrapper := testCase.wrap(
			&p2pLinkNet{inboundCredits: credits},
			localAddress,
			remoteAddress,
			closeEntered,
			releaseClose,
		)

		registered := func() bool {
			credits.stateLock.Lock()
			defer credits.stateLock.Unlock()
			return credits.socketForTransferWithLock(remoteAddress, localAddress) != nil
		}()
		if !registered {
			close(releaseClose)
			cancel()
			credits.close()
			t.Fatalf("%s wrapper did not register its destination", testCase.name)
		}

		closeDone := make(chan error, 1)
		go func() {
			closeDone <- wrapper.Close()
		}()
		select {
		case <-closeEntered:
		case <-ctx.Done():
			close(releaseClose)
			cancel()
			credits.close()
			t.Fatalf("%s delegate Close was not entered: %v", testCase.name, ctx.Err())
		}

		reservation, hasDestination, admitted := credits.reserveForTransfer(
			ctx,
			remoteAddress,
			localAddress,
		)
		if reservation != nil {
			reservation.cancel()
		}
		close(releaseClose)
		select {
		case err := <-closeDone:
			if err != nil {
				cancel()
				credits.close()
				t.Fatalf("%s wrapper Close: %v", testCase.name, err)
			}
		case <-ctx.Done():
			cancel()
			credits.close()
			t.Fatalf("%s wrapper Close did not return: %v", testCase.name, ctx.Err())
		}
		if reservation != nil || hasDestination || !admitted {
			cancel()
			credits.close()
			t.Fatalf(
				"%s close-entry reservation=(%v, destination=%t, admitted=%t), want (nil, false, true)",
				testCase.name,
				reservation,
				hasDestination,
				admitted,
			)
		}
		snapshot := credits.snapshot()
		if snapshot.AdmittedPacketCount != 0 || snapshot.OutstandingPacketCount != 0 ||
			snapshot.TrackedReservationCount != 0 || snapshot.PendingAcquireCount != 0 {
			cancel()
			credits.close()
			t.Fatalf("%s close-entry receive credits=%+v", testCase.name, snapshot)
		}
		cancel()
		credits.close()
	}
}

// A close-wakeup error consumes no datagram. The wrapper cancels its exact
// reservation only after the delegate has stopped, with no invalid release.
func TestP2pLinkUDPConnCloseWakeupDoesNotConsumeReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pDestinationReceiveCredits(1)
	defer credits.close()
	localAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 53401}
	remoteAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 53402}
	delegate := &p2pCloseReadUDPConn{
		localAddress:  localAddress,
		remoteAddress: remoteAddress,
		readErr:       errors.New("delegate close wakeup"),
		readEntered:   make(chan struct{}),
		releaseRead:   make(chan struct{}),
		closeEntered:  make(chan struct{}),
		releaseClose:  make(chan struct{}),
	}
	wrapper := (&p2pLinkNet{inboundCredits: credits}).wrapUDP(delegate)
	reservation, hasDestination, admitted := credits.reserveForTransfer(
		ctx,
		remoteAddress,
		localAddress,
	)
	if !hasDestination || !admitted || reservation == nil {
		t.Fatal("close-wakeup reservation was not admitted")
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := wrapper.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for blocked close-wakeup read: %v", ctx.Err())
	case <-delegate.readEntered:
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- wrapper.Close() }()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for delegate close entry: %v", ctx.Err())
	case <-delegate.closeEntered:
	}
	close(delegate.releaseRead)
	select {
	case <-ctx.Done():
		t.Fatalf("wait for close-wakeup read result: %v", ctx.Err())
	case err := <-readDone:
		if !errors.Is(err, delegate.readErr) {
			t.Fatalf("close-wakeup read error=%v", err)
		}
	}
	close(delegate.releaseClose)
	select {
	case <-ctx.Done():
		t.Fatalf("wait for wrapper close: %v", ctx.Err())
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("wrapper close: %v", err)
		}
	}
	snapshot := credits.snapshot()
	if snapshot.AdmittedPacketCount != 1 || snapshot.ReadPacketCount != 0 ||
		snapshot.CanceledPacketCount != 1 || snapshot.OutstandingPacketCount != 0 ||
		snapshot.TrackedReservationCount != 0 || snapshot.InvalidReleasePacketCount != 0 ||
		snapshot.LateReleaseAfterCloseCount != 0 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("close-wakeup receive credits=%+v", snapshot)
	}
}

// A successful buffered read that wins after close admission still retires its
// exact token once; subsequent socket cancellation finds no live ownership.
func TestP2pLinkUDPConnSuccessfulReadWinsCloseRaceExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pDestinationReceiveCredits(1)
	defer credits.close()
	localAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 53403}
	remoteAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 53404}
	delegate := &p2pCloseReadUDPConn{
		localAddress:  localAddress,
		remoteAddress: remoteAddress,
		readByteCount: 1,
		readEntered:   make(chan struct{}),
		releaseRead:   make(chan struct{}),
		closeEntered:  make(chan struct{}),
		releaseClose:  make(chan struct{}),
	}
	wrapper := (&p2pLinkNet{inboundCredits: credits}).wrapUDP(delegate)
	reservation, hasDestination, admitted := credits.reserveForTransfer(
		ctx,
		remoteAddress,
		localAddress,
	)
	if !hasDestination || !admitted || reservation == nil {
		t.Fatal("successful close-race reservation was not admitted")
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := wrapper.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for blocked successful read: %v", ctx.Err())
	case <-delegate.readEntered:
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- wrapper.Close() }()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for successful-read close entry: %v", ctx.Err())
	case <-delegate.closeEntered:
	}
	close(delegate.releaseRead)
	select {
	case <-ctx.Done():
		t.Fatalf("wait for successful close-race read: %v", ctx.Err())
	case err := <-readDone:
		if err != nil {
			t.Fatalf("successful close-race read: %v", err)
		}
	}
	close(delegate.releaseClose)
	select {
	case <-ctx.Done():
		t.Fatalf("wait for successful-read wrapper close: %v", ctx.Err())
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("successful-read wrapper close: %v", err)
		}
	}
	snapshot := credits.snapshot()
	if snapshot.AdmittedPacketCount != 1 || snapshot.ReadPacketCount != 1 ||
		snapshot.CanceledPacketCount != 0 || snapshot.OutstandingPacketCount != 0 ||
		snapshot.TrackedReservationCount != 0 || snapshot.InvalidReleasePacketCount != 0 ||
		snapshot.LateReleaseAfterCloseCount != 0 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("successful close-race receive credits=%+v", snapshot)
	}
}

// Once wrapper Close has returned and canceled the socket's token, a delayed
// successful delegate read cannot consume that retired generation a second
// time or create an invalid release.
func TestP2pLinkUDPConnCloseWinsBeforeDelayedSuccessfulRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pDestinationReceiveCredits(1)
	defer credits.close()
	localAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 53405}
	remoteAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 53406}
	delegate := &p2pCloseReadUDPConn{
		localAddress:  localAddress,
		remoteAddress: remoteAddress,
		readByteCount: 1,
		readEntered:   make(chan struct{}),
		releaseRead:   make(chan struct{}),
		closeEntered:  make(chan struct{}),
		releaseClose:  make(chan struct{}),
	}
	wrapper := (&p2pLinkNet{inboundCredits: credits}).wrapUDP(delegate)
	reservation, hasDestination, admitted := credits.reserveForTransfer(
		ctx,
		remoteAddress,
		localAddress,
	)
	if !hasDestination || !admitted || reservation == nil {
		t.Fatal("close-first successful-read reservation was not admitted")
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := wrapper.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for delayed successful read: %v", ctx.Err())
	case <-delegate.readEntered:
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- wrapper.Close() }()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for close-first delegate close: %v", ctx.Err())
	case <-delegate.closeEntered:
	}
	close(delegate.releaseClose)
	select {
	case <-ctx.Done():
		t.Fatalf("wait for close-first wrapper close: %v", ctx.Err())
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close-first wrapper close: %v", err)
		}
	}
	closedSnapshot := credits.snapshot()
	if closedSnapshot.CanceledPacketCount != 1 || closedSnapshot.ReadPacketCount != 0 ||
		closedSnapshot.OutstandingPacketCount != 0 ||
		closedSnapshot.TrackedReservationCount != 0 {
		t.Fatalf("close-first pre-read credits=%+v", closedSnapshot)
	}
	close(delegate.releaseRead)
	select {
	case <-ctx.Done():
		t.Fatalf("wait for post-close successful read: %v", ctx.Err())
	case err := <-readDone:
		if err != nil {
			t.Fatalf("post-close successful read: %v", err)
		}
	}
	snapshot := credits.snapshot()
	if snapshot.AdmittedPacketCount != 1 || snapshot.ReadPacketCount != 0 ||
		snapshot.CanceledPacketCount != 1 || snapshot.OutstandingPacketCount != 0 ||
		snapshot.TrackedReservationCount != 0 || snapshot.InvalidReleasePacketCount != 0 ||
		snapshot.LateReleaseAfterCloseCount != 0 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("close-first successful-read credits=%+v", snapshot)
	}
}

// A recording socket embeds the unused transport methods and exposes exact
// write ownership to focused wrapper tests.
type recordingP2pUDPConn struct {
	transport.UDPConn
	entered chan struct{}
	release chan struct{}
	writes  chan []byte
	once    sync.Once
}

// Connected writes are enough to validate link ownership and disposition.
func (self *recordingP2pUDPConn) Write(payload []byte) (int, error) {
	if self.entered != nil {
		self.once.Do(func() { close(self.entered) })
	}
	if self.release != nil {
		<-self.release
	}
	self.writes <- append([]byte(nil), payload...)
	return len(payload), nil
}

// One exact held release proves the caller buffer is copied and idle joins the
// delayed destination rather than merely the scheduler heap.
func TestP2pLinkUDPConnCopiesPayloadAndJoinsHeldDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	link := newDirectionalLink(ctx, profile, 7101, nil)
	defer link.close()
	recorder := &recordingP2pUDPConn{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		writes:  make(chan []byte, 1),
	}
	var releaseOnce sync.Once
	releaseDelivery := func() { releaseOnce.Do(func() { close(recorder.release) }) }
	defer releaseDelivery()
	connection := &p2pLinkUDPConn{UDPConn: recorder, link: link}
	payload := []byte("owned-before-return")
	expected := append([]byte(nil), payload...)
	writtenByteCount, err := connection.Write(payload)
	if err != nil || writtenByteCount != len(payload) {
		t.Fatalf("write bytes=%d err=%v", writtenByteCount, err)
	}
	for index := range payload {
		payload[index] = 0
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for held destination entry: %v", ctx.Err())
	case <-recorder.entered:
	}
	blockedCtx, blockedCancel := context.WithCancel(context.Background())
	blockedCancel()
	if link.waitIdle(blockedCtx) {
		t.Fatal("held destination was reported idle before release")
	}
	releaseDelivery()
	if !link.waitIdle(ctx) {
		t.Fatalf("link did not become idle after destination release: %v", ctx.Err())
	}
	var delivered []byte
	select {
	case <-ctx.Done():
		t.Fatalf("wait for delivered payload: %v", ctx.Err())
	case delivered = <-recorder.writes:
	}
	if string(delivered) != string(expected) {
		t.Fatalf("delivered payload=%q expected=%q", delivered, expected)
	}
	snapshot := link.snapshot()
	expectedOuterByteCount := uint64(p2pIPv4UDPHeaderByteCount + len(expected))
	if snapshot.AdmittedPacketCount != 1 || snapshot.DeliveredPacketCount != 1 ||
		snapshot.AdmittedByteCount != expectedOuterByteCount ||
		snapshot.DeliveredByteCount != expectedOuterByteCount ||
		snapshot.WireByteCount != expectedOuterByteCount ||
		snapshot.QueuedPacketCount != 0 || snapshot.QueuedByteCount != 0 {
		t.Fatalf("held delivery snapshot=%+v", snapshot)
	}
}

// Exact full-outer accounting makes the MTU boundary deterministic.
func TestP2pLinkUDPConnUsesFullIPv4UDPOuterMtuAndByteAccounting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const outerMtu = 1280
	link := newDirectionalLink(ctx, testP2pLinkProfile(outerMtu, oversizeModeDrop), 7102, nil)
	defer link.close()
	recorder := &recordingP2pUDPConn{writes: make(chan []byte, 1)}
	connection := &p2pLinkUDPConn{
		UDPConn: recorder,
		link:    link,
	}
	boundaryPayload := make([]byte, outerMtu-p2pIPv4UDPHeaderByteCount)
	writtenByteCount, err := connection.Write(boundaryPayload)
	if err != nil || writtenByteCount != len(boundaryPayload) {
		t.Fatalf("boundary write bytes=%d err=%v", writtenByteCount, err)
	}
	if !link.waitIdle(ctx) {
		t.Fatal("boundary packet did not reach idle")
	}
	var delivered []byte
	select {
	case <-ctx.Done():
		t.Fatalf("wait for boundary payload: %v", ctx.Err())
	case delivered = <-recorder.writes:
	}
	if len(delivered) != len(boundaryPayload) {
		t.Fatal("boundary payload length changed")
	}
	oversizePayload := make([]byte, len(boundaryPayload)+1)
	writtenByteCount, err = connection.Write(oversizePayload)
	if err != nil || writtenByteCount != len(oversizePayload) {
		t.Fatalf("silent oversize write bytes=%d err=%v", writtenByteCount, err)
	}
	snapshot := link.snapshot()
	if snapshot.AdmittedPacketCount != 1 || snapshot.AdmittedByteCount != outerMtu ||
		snapshot.DeliveredPacketCount != 1 || snapshot.DeliveredByteCount != outerMtu ||
		snapshot.WireByteCount != outerMtu || snapshot.MtuDropPacketCount != 1 ||
		snapshot.MaximumQueuedBytes != outerMtu {
		t.Fatalf("MTU boundary snapshot=%+v", snapshot)
	}
	if snapshot.MaximumSubmittedPacketBytes != outerMtu+1 {
		t.Fatalf(
			"maximum outer bytes=%d expected=%d",
			snapshot.MaximumSubmittedPacketBytes,
			outerMtu+1,
		)
	}
}

// Error mode reports the full outer packet size without admitting ownership.
func TestP2pLinkUDPConnReportsSynchronousOuterMtuError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const outerMtu = 1280
	link := newDirectionalLink(ctx, testP2pLinkProfile(outerMtu, oversizeModeError), 7103, nil)
	defer link.close()
	connection := &p2pLinkUDPConn{
		UDPConn: &recordingP2pUDPConn{writes: make(chan []byte, 1)},
		link:    link,
	}
	payload := make([]byte, outerMtu-p2pIPv4UDPHeaderByteCount+1)
	writtenByteCount, err := connection.Write(payload)
	if writtenByteCount != 0 {
		t.Fatalf("oversize error bytes=%d", writtenByteCount)
	}
	var tooLarge *packetTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("oversize error=%v", err)
	}
	if tooLarge.packetByteCount != outerMtu+1 || tooLarge.mtu != outerMtu {
		t.Fatalf("oversize detail=%+v", tooLarge)
	}
	snapshot := link.snapshot()
	if snapshot.AdmittedPacketCount != 0 || snapshot.MtuDropPacketCount != 0 ||
		snapshot.QueuedPacketCount != 0 || snapshot.QueuedByteCount != 0 {
		t.Fatalf("oversize error snapshot=%+v", snapshot)
	}
}

// Directional schedulers do not share an idle generation or blocked receiver.
func TestP2pLinkUDPConnDirectionsAreIndependent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	forward := newDirectionalLink(ctx, profile, 7104, nil)
	reverse := newDirectionalLink(ctx, profile, 7105, nil)
	defer forward.close()
	defer reverse.close()
	forwardRecorder := &recordingP2pUDPConn{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		writes:  make(chan []byte, 1),
	}
	var releaseOnce sync.Once
	releaseForward := func() { releaseOnce.Do(func() { close(forwardRecorder.release) }) }
	defer releaseForward()
	reverseRecorder := &recordingP2pUDPConn{writes: make(chan []byte, 1)}
	forwardConnection := &p2pLinkUDPConn{UDPConn: forwardRecorder, link: forward}
	reverseConnection := &p2pLinkUDPConn{UDPConn: reverseRecorder, link: reverse}
	if _, err := forwardConnection.Write([]byte("forward-held")); err != nil {
		t.Fatalf("forward write: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for held forward destination: %v", ctx.Err())
	case <-forwardRecorder.entered:
	}
	if _, err := reverseConnection.Write([]byte("reverse-free")); err != nil {
		t.Fatalf("reverse write: %v", err)
	}
	if !reverse.waitIdle(ctx) {
		t.Fatal("reverse link did not become idle")
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for reverse payload: %v", ctx.Err())
	case payload := <-reverseRecorder.writes:
		if string(payload) != "reverse-free" {
			t.Fatalf("reverse payload=%q", payload)
		}
	}
	blockedCtx, blockedCancel := context.WithCancel(context.Background())
	blockedCancel()
	if forward.waitIdle(blockedCtx) {
		t.Fatal("forward link followed independent reverse completion")
	}
	releaseForward()
	if !forward.waitIdle(ctx) {
		t.Fatalf("forward link did not become idle after release: %v", ctx.Err())
	}
}

// Focused wrapper tests use an explicit valid profile with no timing guesses.
func testP2pLinkProfile(outerMtu int, mode oversizeMode) linkProfile {
	return linkProfile{
		RateBitsPerSecond: 1_000_000_000,
		BurstByteCount:    1024 * 1024,
		QueueByteCount:    1024 * 1024,
		QueuePacketCount:  1024,
		LossModel:         lossModelNone,
		OuterMtu:          outerMtu,
		OversizeMode:      mode,
	}
}

// Delegate fakes expose whether configured Pion construction paths were used.
type recordingP2pDialer struct {
	connection net.Conn
	called     chan struct{}
}

// Dial returns the delegate-owned connection and records exact invocation.
func (self *recordingP2pDialer) Dial(network string, address string) (net.Conn, error) {
	_ = network
	_ = address
	close(self.called)
	return self.connection, nil
}

// A configured listener fake records construction and returns one exact socket.
type recordingP2pListenConfig struct {
	connection net.PacketConn
	called     chan struct{}
}

// Stream listening is outside the UDP wrapper regression.
func (self *recordingP2pListenConfig) Listen(
	ctx context.Context,
	network string,
	address string,
) (net.Listener, error) {
	_ = ctx
	_ = network
	_ = address
	return nil, transport.ErrNotSupported
}

// Packet listening returns the delegate-owned socket and records invocation.
func (self *recordingP2pListenConfig) ListenPacket(
	ctx context.Context,
	network string,
	address string,
) (net.PacketConn, error) {
	_ = ctx
	_ = network
	_ = address
	close(self.called)
	return self.connection, nil
}

// A delegate fake retains constructor inputs and caller-selected implementations.
type recordingP2pDelegateNet struct {
	transport.Net
	dialer                 transport.Dialer
	listenConfig           transport.ListenConfig
	configuredDialer       *net.Dialer
	configuredListenConfig *net.ListenConfig
}

// The exact net.Dialer pointer proves its LocalAddr/deadline/control settings
// remain owned by the underlying transport implementation.
func (self *recordingP2pDelegateNet) CreateDialer(dialer *net.Dialer) transport.Dialer {
	self.configuredDialer = dialer
	return self.dialer
}

// The exact net.ListenConfig pointer proves its Control settings are retained.
func (self *recordingP2pDelegateNet) CreateListenConfig(config *net.ListenConfig) transport.ListenConfig {
	self.configuredListenConfig = config
	return self.listenConfig
}

// Configured dialers must invoke the delegate and wrap its returned UDP socket.
func TestP2pLinkNetCreateDialerPreservesDelegate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, testP2pLinkProfile(1500, oversizeModeDrop), 7106, nil)
	defer link.close()
	socket := &recordingP2pUDPConn{writes: make(chan []byte, 1)}
	delegateDialer := &recordingP2pDialer{connection: socket, called: make(chan struct{})}
	delegateNet := &recordingP2pDelegateNet{dialer: delegateDialer}
	network := newP2pLinkNet(delegateNet, link)
	configuredDialer := &net.Dialer{Timeout: 37 * time.Second}
	dialer := network.CreateDialer(configuredDialer)
	if delegateNet.configuredDialer != configuredDialer {
		t.Fatal("configured dialer was not passed unchanged to delegate")
	}
	connection, err := dialer.Dial("udp4", "10.240.0.2:10000")
	if err != nil {
		t.Fatalf("configured UDP dial: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for configured dial delegate: %v", ctx.Err())
	case <-delegateDialer.called:
	}
	if _, ok := connection.(*p2pLinkUDPConn); !ok {
		t.Fatalf("configured dial returned %T, expected link-backed UDP", connection)
	}
}

// Configured listeners must invoke the delegate and wrap its returned socket.
func TestP2pLinkNetCreateListenConfigPreservesDelegate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, testP2pLinkProfile(1500, oversizeModeDrop), 7107, nil)
	defer link.close()
	socket := &recordingP2pUDPConn{writes: make(chan []byte, 1)}
	delegateListenConfig := &recordingP2pListenConfig{
		connection: socket,
		called:     make(chan struct{}),
	}
	delegateNet := &recordingP2pDelegateNet{listenConfig: delegateListenConfig}
	network := newP2pLinkNet(delegateNet, link)
	configuredListenConfig := &net.ListenConfig{}
	listenConfig := network.CreateListenConfig(configuredListenConfig)
	if delegateNet.configuredListenConfig != configuredListenConfig {
		t.Fatal("configured ListenConfig was not passed unchanged to delegate")
	}
	connection, err := listenConfig.ListenPacket(ctx, "udp4", "10.240.0.1:0")
	if err != nil {
		t.Fatalf("configured UDP listen: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for configured listen delegate: %v", ctx.Err())
	case <-delegateListenConfig.called:
	}
	if _, ok := connection.(*p2pLinkUDPConn); !ok {
		t.Fatalf("configured listen returned %T, expected link-backed UDP", connection)
	}
}

var _ transport.Net = (*p2pLinkNet)(nil)
var _ transport.UDPConn = (*p2pLinkUDPConn)(nil)
var _ net.PacketConn = (*p2pLinkPacketConn)(nil)

// Pion discovers this optional value-address surface with a type assertion.
type p2pAddrPortReaderWriter interface {
	ReadFromAddrPort([]byte) (int, netip.AddrPort, error)
	WriteToAddrPort([]byte, netip.AddrPort) (int, error)
}

var _ p2pAddrPortReaderWriter = (*p2pLinkUDPConn)(nil)
