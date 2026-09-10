// This file implements the bounded, deterministic directional packet
// scheduler used by every userspace PERFVAR topology.
package perfvar

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errLinkClosed = errors.New("simulated link is closed")

// The synchronous error mode reports a datagram that cannot fit the outer MTU.
type packetTooLargeError struct {
	packetByteCount int
	mtu             int
}

// A concise error retains the observed and configured sizes.
func (self *packetTooLargeError) Error() string {
	return fmt.Sprintf("packet size %d exceeds simulated MTU %d", self.packetByteCount, self.mtu)
}

// An in-path terminal cause is selected before survivor-only stochastic
// duplication and reordering, but finishes only after wire scheduling.
type linkTerminalDropCause uint8

const (
	linkTerminalDropNone linkTerminalDropCause = iota
	linkTerminalDropLoss
	linkTerminalDropOutage
)

// A test-only immutable observation exposes exact scheduler arithmetic without
// turning wall-clock timing guesses into correctness conditions.
type linkScheduleObservation struct {
	sequence               uint64
	packetByteCount        int
	scheduleTime           time.Time
	rateReadyTime          time.Time
	releaseTime            time.Time
	terminalDropCause      linkTerminalDropCause
	duplicateScheduled     bool
	duplicateRateReadyTime time.Time
	duplicateReleaseTime   time.Time
}

// An admitted packet is owned by the link until delivery or drop.
type linkPacket struct {
	sequence            uint64
	packetBytes         []byte
	releaseTime         time.Time
	deliver             linkDeliver
	terminalDropCause   linkTerminalDropCause
	terminalDropAllowed bool
	// duplicatePacket follows a pending reorder candidate when its paired
	// packet moves the candidate's release boundary.
	duplicatePacket *linkPacket
	orderIndex      uint64
	heapIndex       int
}

// The release heap orders time first and admission order second.
type scheduledPacketHeap []*linkPacket

// Heap length is the number of pending releases.
func (self scheduledPacketHeap) Len() int {
	return len(self)
}

// Earlier release and order values sort first.
func (self scheduledPacketHeap) Less(i int, j int) bool {
	if self[i].releaseTime.Equal(self[j].releaseTime) {
		return self[i].orderIndex < self[j].orderIndex
	}
	return self[i].releaseTime.Before(self[j].releaseTime)
}

// Heap swaps keep each packet's heap-fix index synchronized.
func (self scheduledPacketHeap) Swap(i int, j int) {
	self[i], self[j] = self[j], self[i]
	self[i].heapIndex = i
	self[j].heapIndex = j
}

// Heap push accepts only owned link packets.
func (self *scheduledPacketHeap) Push(value any) {
	packet := value.(*linkPacket)
	packet.heapIndex = len(*self)
	*self = append(*self, packet)
}

// Heap pop clears the retained pointer before shrinking.
func (self *scheduledPacketHeap) Pop() any {
	packets := *self
	lastIndex := len(packets) - 1
	packet := packets[lastIndex]
	packets[lastIndex] = nil
	*self = packets[:lastIndex]
	packet.heapIndex = -1
	return packet
}

// Atomic counters let measurement snapshots avoid stopping the scheduler.
type directionalLinkCounters struct {
	admittedPacketCount             atomic.Uint64
	admittedByteCount               atomic.Uint64
	deliveredPacketCount            atomic.Uint64
	deliveredByteCount              atomic.Uint64
	wireByteCount                   atomic.Uint64
	lossDropPacketCount             atomic.Uint64
	mtuDropPacketCount              atomic.Uint64
	queueDropPacketCount            atomic.Uint64
	outageDropPacketCount           atomic.Uint64
	allowedLossDropPacketCount      atomic.Uint64
	unexpectedLossDropPacketCount   atomic.Uint64
	allowedMtuDropPacketCount       atomic.Uint64
	unexpectedMtuDropPacketCount    atomic.Uint64
	allowedQueueDropPacketCount     atomic.Uint64
	unexpectedQueueDropPacketCount  atomic.Uint64
	allowedOutageDropPacketCount    atomic.Uint64
	unexpectedOutageDropPacketCount atomic.Uint64
	receiverDropPacketCount         atomic.Uint64
	canceledDropPacketCount         atomic.Uint64
	duplicatePacketCount            atomic.Uint64
	reorderedPacketCount            atomic.Uint64
	profileUpdateCount              atomic.Uint64
	firstAdmissionUnixNano          atomic.Int64
	lastWireTerminalUnixNano        atomic.Int64
}

// A plain snapshot is serialized into result records and Markdown summaries.
type directionalLinkSnapshot struct {
	AdmittedPacketCount             uint64 `json:"admitted_packet_count"`
	AdmittedByteCount               uint64 `json:"admitted_byte_count"`
	DeliveredPacketCount            uint64 `json:"delivered_packet_count"`
	DeliveredByteCount              uint64 `json:"delivered_byte_count"`
	WireByteCount                   uint64 `json:"wire_byte_count"`
	LossDropPacketCount             uint64 `json:"loss_drop_packet_count"`
	MtuDropPacketCount              uint64 `json:"mtu_drop_packet_count"`
	QueueDropPacketCount            uint64 `json:"queue_drop_packet_count"`
	OutageDropPacketCount           uint64 `json:"outage_drop_packet_count"`
	AllowedLossDropPacketCount      uint64 `json:"allowed_loss_drop_packet_count"`
	UnexpectedLossDropPacketCount   uint64 `json:"unexpected_loss_drop_packet_count"`
	AllowedMtuDropPacketCount       uint64 `json:"allowed_mtu_drop_packet_count"`
	UnexpectedMtuDropPacketCount    uint64 `json:"unexpected_mtu_drop_packet_count"`
	AllowedQueueDropPacketCount     uint64 `json:"allowed_queue_drop_packet_count"`
	UnexpectedQueueDropPacketCount  uint64 `json:"unexpected_queue_drop_packet_count"`
	AllowedOutageDropPacketCount    uint64 `json:"allowed_outage_drop_packet_count"`
	UnexpectedOutageDropPacketCount uint64 `json:"unexpected_outage_drop_packet_count"`
	ReceiverDropPacketCount         uint64 `json:"receiver_drop_packet_count"`
	CanceledDropPacketCount         uint64 `json:"canceled_drop_packet_count"`
	DuplicatePacketCount            uint64 `json:"duplicate_packet_count"`
	ReorderedPacketCount            uint64 `json:"reordered_packet_count"`
	ProfileUpdateCount              uint64 `json:"profile_update_count"`
	QueuedPacketCount               int    `json:"queued_packet_count"`
	QueuedByteCount                 int    `json:"queued_byte_count"`
	MaximumQueuedPackets            int    `json:"maximum_queued_packets"`
	MaximumQueuedBytes              int    `json:"maximum_queued_bytes"`
	MaximumSubmittedPacketBytes     int    `json:"maximum_submitted_packet_bytes"`
	ConfiguredRateBits              int64  `json:"configured_rate_bits_per_second"`
	AchievedRateBits                int64  `json:"achieved_rate_bits_per_second"`
	ConfiguredAllowQueueDrops       bool   `json:"configured_allow_queue_drops"`

	measurementMaximumEpoch       *directionalLinkMaximumEpoch
	measurementMaximumPackets     int
	measurementMaximumBytes       int
	measurementMaximumPacketBytes int
	submittedPacketCount          uint64
	activeSubmissionCount         int
}

// One immutable identity owns queue high-water marks for exactly one workload
// interval while lifetime maxima remain available for diagnostics.
type directionalLinkMaximumEpoch struct {
	maximumQueuedPacket        int
	maximumQueuedByte          int
	maximumSubmittedPacketByte int
}

// A serialized update records the actual application boundary.
type linkProfileUpdate struct {
	profile   linkProfile
	applied   chan time.Time
	eventName string
	scheduled time.Time
}

// A nonblocking destination accepts ownership only when it returns true.
type linkDeliver func([]byte) bool

// An atomically published test observer can safely be installed while a live
// topology continues to schedule unrelated carrier traffic.
type linkScheduleTestHook struct {
	call func(linkScheduleObservation)
}

// One goroutine owns random decisions, rate state, and release ordering.
type directionalLink struct {
	ctx       context.Context
	cancel    context.CancelFunc
	deliver   linkDeliver
	ingress   chan *linkPacket
	updates   chan linkProfileUpdate
	done      chan struct{}
	idle      chan struct{}
	progress  chan struct{}
	closeOnce sync.Once
	// submissionWaitGroup is registered under stateLock, which prevents an Add
	// after shutdown closes admission and begins its single Wait.
	submissionWaitGroup sync.WaitGroup

	stateLock                  sync.Mutex
	profile                    linkProfile
	closed                     bool
	activeSubmissionCount      int
	queuedPacketCount          int
	queuedByteCount            int
	maximumQueuedPacket        int
	maximumQueuedByte          int
	maximumSubmittedPacketByte int
	measurementMaximum         *directionalLinkMaximumEpoch

	nextSequence     atomic.Uint64
	submittedPackets atomic.Uint64
	counters         directionalLinkCounters
	releaseBatchSize int

	// Test-only barriers are nil in production measurement paths.
	beforeIngressForTest         func()
	beforeAdmissionForTest       func()
	afterImmediateDropForTest    func()
	beforeIdleWaitForTest        func()
	afterAdmissionsClosedForTest func()
	beforeSubmissionWaitForTest  func()
	afterReorderPairForTest      func()
	afterPacketScheduledForTest  atomic.Pointer[linkScheduleTestHook]
}

// A nil callback removes the observer without racing the scheduler goroutine.
func (self *directionalLink) setAfterPacketScheduledForTest(
	callback func(linkScheduleObservation),
) {
	if callback == nil {
		self.afterPacketScheduledForTest.Store(nil)
		return
	}
	self.afterPacketScheduledForTest.Store(&linkScheduleTestHook{call: callback})
}

// The first matching scheduler event is published before every matching
// packet is held. Closing release unblocks all carrier schedulers exactly.
func holdLinkScheduleForTest(
	links []*directionalLink,
	matches func(linkScheduleObservation) bool,
	release <-chan struct{},
) <-chan struct{} {
	reached := make(chan struct{})
	var reachedOnce sync.Once
	for _, link := range links {
		link.setAfterPacketScheduledForTest(func(observation linkScheduleObservation) {
			if !matches(observation) {
				return
			}
			reachedOnce.Do(func() {
				close(reached)
			})
			<-release
		})
	}
	return reached
}

// The scheduler starts fully running; nil delivery means every release drops.
func newDirectionalLink(
	ctx context.Context,
	profile linkProfile,
	seed int64,
	deliver linkDeliver,
) *directionalLink {
	linkCtx, cancel := context.WithCancel(ctx)
	ingressCapacity := max(profile.QueuePacketCount, 1)
	idle := make(chan struct{})
	close(idle)
	link := &directionalLink{
		ctx:              linkCtx,
		cancel:           cancel,
		deliver:          deliver,
		ingress:          make(chan *linkPacket, ingressCapacity),
		updates:          make(chan linkProfileUpdate),
		done:             make(chan struct{}),
		idle:             idle,
		progress:         make(chan struct{}, 1),
		profile:          profile,
		releaseBatchSize: 64,
	}
	go link.run(seed)
	return link
}

// Admission through the link's default destination never waits for the scheduler.
func (self *directionalLink) submit(packetBytes []byte) (int, error) {
	return self.submitWithDeliver(packetBytes, nil)
}

// Admission with a packet-specific destination lets one physical impairment
// scheduler serve multiple logical sockets while retaining one ownership path.
func (self *directionalLink) submitWithDeliver(
	packetBytes []byte,
	deliver linkDeliver,
) (int, error) {
	return self.submitPacket(packetBytes, deliver, false)
}

// Owned admission transfers exclusive ownership of packetBytes on every
// return path. Callers must neither read nor mutate the slice after calling.
// It avoids copying a buffer that a test-only transport wrapper just created.
func (self *directionalLink) submitOwnedWithDeliver(
	packetBytes []byte,
	deliver linkDeliver,
) (int, error) {
	return self.submitPacket(packetBytes, deliver, true)
}

// A terminal cause is attributed to the policy active for that exact packet.
func recordLinkDropPolicy(
	allowed bool,
	allowedCounter *atomic.Uint64,
	unexpectedCounter *atomic.Uint64,
) {
	if allowed {
		allowedCounter.Add(1)
	} else {
		unexpectedCounter.Add(1)
	}
}

// Only validated nonzero loss models intentionally filter packets.
func linkLossDropAllowed(profile linkProfile) bool {
	switch profile.LossModel {
	case lossModelIndependent, lossModelEveryN, lossModelBurst:
		return true
	default:
		return false
	}
}

// One admission implementation keeps copied and transferred ownership paths
// identical after the explicit ownership boundary.
func (self *directionalLink) submitPacket(
	packetBytes []byte,
	deliver linkDeliver,
	owned bool,
) (int, error) {
	packetByteCount := len(packetBytes)
	if self.beforeAdmissionForTest != nil {
		self.beforeAdmissionForTest()
	}
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		return 0, errLinkClosed
	}
	if self.activeSubmissionCount == 0 && self.queuedPacketCount == 0 {
		self.idle = make(chan struct{})
	}
	self.activeSubmissionCount += 1
	self.maximumSubmittedPacketByte = max(self.maximumSubmittedPacketByte, packetByteCount)
	if self.measurementMaximum != nil {
		self.measurementMaximum.maximumSubmittedPacketByte = max(
			self.measurementMaximum.maximumSubmittedPacketByte,
			packetByteCount,
		)
	}
	self.submissionWaitGroup.Add(1)
	defer self.submissionWaitGroup.Done()
	defer self.finishSubmission()
	profile := self.profile
	if profile.OuterMtu < packetByteCount {
		self.stateLock.Unlock()
		if self.afterImmediateDropForTest != nil {
			self.afterImmediateDropForTest()
		}
		if profile.OversizeMode == oversizeModeError {
			self.recordSubmission()
			return 0, &packetTooLargeError{
				packetByteCount: packetByteCount,
				mtu:             profile.OuterMtu,
			}
		}
		self.counters.mtuDropPacketCount.Add(1)
		recordLinkDropPolicy(
			profile.AllowMtuDrops,
			&self.counters.allowedMtuDropPacketCount,
			&self.counters.unexpectedMtuDropPacketCount,
		)
		self.recordSubmission()
		return packetByteCount, nil
	}
	if profile.QueuePacketCount <= self.queuedPacketCount ||
		profile.QueueByteCount < self.queuedByteCount+packetByteCount {
		self.stateLock.Unlock()
		if self.afterImmediateDropForTest != nil {
			self.afterImmediateDropForTest()
		}
		self.counters.queueDropPacketCount.Add(1)
		recordLinkDropPolicy(
			profile.AllowQueueDrops,
			&self.counters.allowedQueueDropPacketCount,
			&self.counters.unexpectedQueueDropPacketCount,
		)
		self.recordSubmission()
		return packetByteCount, nil
	}
	self.queuedPacketCount += 1
	self.queuedByteCount += packetByteCount
	self.maximumQueuedPacket = max(self.maximumQueuedPacket, self.queuedPacketCount)
	self.maximumQueuedByte = max(self.maximumQueuedByte, self.queuedByteCount)
	if self.measurementMaximum != nil {
		self.measurementMaximum.maximumQueuedPacket = max(
			self.measurementMaximum.maximumQueuedPacket,
			self.queuedPacketCount,
		)
		self.measurementMaximum.maximumQueuedByte = max(
			self.measurementMaximum.maximumQueuedByte,
			self.queuedByteCount,
		)
	}
	self.stateLock.Unlock()
	// Publish the generation after reservation but before scheduler ingress.
	// An idle barrier therefore observes either the reservation or its generation.
	self.recordSubmission()
	if self.beforeIngressForTest != nil {
		self.beforeIngressForTest()
	}

	ownedPacketBytes := packetBytes
	if !owned {
		ownedPacketBytes = append([]byte(nil), packetBytes...)
	}
	packet := &linkPacket{
		sequence:    self.nextSequence.Add(1),
		packetBytes: ownedPacketBytes,
		deliver:     deliver,
		heapIndex:   -1,
	}
	admissionUnixNano := time.Now().UnixNano()
	select {
	case self.ingress <- packet:
		self.counters.admittedPacketCount.Add(1)
		self.counters.admittedByteCount.Add(uint64(packetByteCount))
		self.counters.firstAdmissionUnixNano.CompareAndSwap(0, admissionUnixNano)
		return packetByteCount, nil
	default:
		self.counters.queueDropPacketCount.Add(1)
		recordLinkDropPolicy(
			profile.AllowQueueDrops,
			&self.counters.allowedQueueDropPacketCount,
			&self.counters.unexpectedQueueDropPacketCount,
		)
		self.releaseQueue(packetByteCount)
		return packetByteCount, nil
	}
}

// Completion publishes every immediate drop and counter update before the
// live idle boundary can close its current submission generation.
func (self *directionalLink) finishSubmission() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.activeSubmissionCount -= 1
	if self.activeSubmissionCount < 0 {
		panic("simulated link active submission ownership became negative")
	}
	if self.activeSubmissionCount == 0 && self.queuedPacketCount == 0 {
		close(self.idle)
	}
}

// Every completed submit wakes count-based workload boundaries. Coalescing is
// safe because waiters always re-read the monotonic count before blocking.
func (self *directionalLink) recordSubmission() {
	self.submittedPackets.Add(1)
	select {
	case self.progress <- struct{}{}:
	default:
	}
}

// Sender-complete workloads wait until the network reader has submitted every
// emitted packet. The context is only a liveness bound, not completion proof.
func (self *directionalLink) waitForSubmissionCount(ctx context.Context, target uint64) bool {
	for self.submittedPackets.Load() < target {
		select {
		case <-ctx.Done():
			return false
		case <-self.progress:
		}
	}
	return true
}

// A duplicate is admitted against the same hard queue bounds.
func (self *directionalLink) reserveDuplicate(packetByteCount int) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed || self.profile.QueuePacketCount <= self.queuedPacketCount ||
		self.profile.QueueByteCount < self.queuedByteCount+packetByteCount {
		return false
	}
	if self.activeSubmissionCount == 0 && self.queuedPacketCount == 0 {
		self.idle = make(chan struct{})
	}
	self.queuedPacketCount += 1
	self.queuedByteCount += packetByteCount
	self.maximumQueuedPacket = max(self.maximumQueuedPacket, self.queuedPacketCount)
	self.maximumQueuedByte = max(self.maximumQueuedByte, self.queuedByteCount)
	if self.measurementMaximum != nil {
		self.measurementMaximum.maximumQueuedPacket = max(
			self.measurementMaximum.maximumQueuedPacket,
			self.queuedPacketCount,
		)
		self.measurementMaximum.maximumQueuedByte = max(
			self.measurementMaximum.maximumQueuedByte,
			self.queuedByteCount,
		)
	}
	return true
}

// Every terminal delivery or drop releases one queue reservation.
func (self *directionalLink) releaseQueue(packetByteCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.queuedPacketCount -= 1
	self.queuedByteCount -= packetByteCount
	if self.queuedPacketCount < 0 || self.queuedByteCount < 0 {
		panic("simulated link queue ownership became negative")
	}
	if self.activeSubmissionCount == 0 && self.queuedPacketCount == 0 {
		close(self.idle)
	}
}

// A serialized update makes dynamic event boundaries observable and replayable.
func (self *directionalLink) updateProfile(
	profile linkProfile,
	eventName string,
	scheduled time.Time,
) (time.Time, error) {
	applied := make(chan time.Time, 1)
	update := linkProfileUpdate{
		profile:   profile,
		applied:   applied,
		eventName: eventName,
		scheduled: scheduled,
	}
	select {
	case <-self.ctx.Done():
		return time.Time{}, errLinkClosed
	case self.updates <- update:
	}
	select {
	case <-self.ctx.Done():
		return time.Time{}, errLinkClosed
	case actual := <-applied:
		return actual, nil
	}
}

// The lock-held snapshot keeps an interval reset and its monotonic baseline in
// one linearized operation. Callers must hold stateLock.
func (self *directionalLink) snapshotWithLock() directionalLinkSnapshot {
	queuedPacketCount := self.queuedPacketCount
	queuedByteCount := self.queuedByteCount
	maximumQueuedPacket := self.maximumQueuedPacket
	maximumQueuedByte := self.maximumQueuedByte
	maximumSubmittedPacketByte := self.maximumSubmittedPacketByte
	measurementMaximumEpoch := self.measurementMaximum
	measurementMaximumPackets := 0
	measurementMaximumBytes := 0
	measurementMaximumPacketBytes := 0
	if measurementMaximumEpoch != nil {
		measurementMaximumPackets = measurementMaximumEpoch.maximumQueuedPacket
		measurementMaximumBytes = measurementMaximumEpoch.maximumQueuedByte
		measurementMaximumPacketBytes = measurementMaximumEpoch.maximumSubmittedPacketByte
	}
	configuredRateBits := self.profile.RateBitsPerSecond
	configuredAllowQueueDrops := self.profile.AllowQueueDrops
	firstAdmission := self.counters.firstAdmissionUnixNano.Load()
	lastWireTerminal := self.counters.lastWireTerminalUnixNano.Load()
	achievedRateBits := int64(0)
	if firstAdmission != 0 && firstAdmission < lastWireTerminal {
		duration := time.Duration(lastWireTerminal - firstAdmission)
		achievedRateBits = int64(float64(self.counters.wireByteCount.Load()*8) / duration.Seconds())
	}
	return directionalLinkSnapshot{
		AdmittedPacketCount:             self.counters.admittedPacketCount.Load(),
		AdmittedByteCount:               self.counters.admittedByteCount.Load(),
		DeliveredPacketCount:            self.counters.deliveredPacketCount.Load(),
		DeliveredByteCount:              self.counters.deliveredByteCount.Load(),
		WireByteCount:                   self.counters.wireByteCount.Load(),
		LossDropPacketCount:             self.counters.lossDropPacketCount.Load(),
		MtuDropPacketCount:              self.counters.mtuDropPacketCount.Load(),
		QueueDropPacketCount:            self.counters.queueDropPacketCount.Load(),
		OutageDropPacketCount:           self.counters.outageDropPacketCount.Load(),
		AllowedLossDropPacketCount:      self.counters.allowedLossDropPacketCount.Load(),
		UnexpectedLossDropPacketCount:   self.counters.unexpectedLossDropPacketCount.Load(),
		AllowedMtuDropPacketCount:       self.counters.allowedMtuDropPacketCount.Load(),
		UnexpectedMtuDropPacketCount:    self.counters.unexpectedMtuDropPacketCount.Load(),
		AllowedQueueDropPacketCount:     self.counters.allowedQueueDropPacketCount.Load(),
		UnexpectedQueueDropPacketCount:  self.counters.unexpectedQueueDropPacketCount.Load(),
		AllowedOutageDropPacketCount:    self.counters.allowedOutageDropPacketCount.Load(),
		UnexpectedOutageDropPacketCount: self.counters.unexpectedOutageDropPacketCount.Load(),
		ReceiverDropPacketCount:         self.counters.receiverDropPacketCount.Load(),
		CanceledDropPacketCount:         self.counters.canceledDropPacketCount.Load(),
		DuplicatePacketCount:            self.counters.duplicatePacketCount.Load(),
		ReorderedPacketCount:            self.counters.reorderedPacketCount.Load(),
		ProfileUpdateCount:              self.counters.profileUpdateCount.Load(),
		QueuedPacketCount:               queuedPacketCount,
		QueuedByteCount:                 queuedByteCount,
		MaximumQueuedPackets:            maximumQueuedPacket,
		MaximumQueuedBytes:              maximumQueuedByte,
		MaximumSubmittedPacketBytes:     maximumSubmittedPacketByte,
		ConfiguredRateBits:              configuredRateBits,
		AchievedRateBits:                achievedRateBits,
		ConfiguredAllowQueueDrops:       configuredAllowQueueDrops,
		measurementMaximumEpoch:         measurementMaximumEpoch,
		measurementMaximumPackets:       measurementMaximumPackets,
		measurementMaximumBytes:         measurementMaximumBytes,
		measurementMaximumPacketBytes:   measurementMaximumPacketBytes,
		submittedPacketCount:            self.submittedPackets.Load(),
		activeSubmissionCount:           self.activeSubmissionCount,
	}
}

// The current counters include in-flight queue occupancy and achieved rate.
func (self *directionalLink) snapshot() directionalLinkSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.snapshotWithLock()
}

// A fresh interval waits out any post-boundary admission that won the state
// lock, then swaps its maximum identity and captures counters under that lock.
func (self *directionalLink) beginMeasurementSnapshot(
	ctx context.Context,
) (directionalLinkSnapshot, bool) {
	for {
		self.stateLock.Lock()
		if self.activeSubmissionCount == 0 && self.queuedPacketCount == 0 {
			self.measurementMaximum = &directionalLinkMaximumEpoch{}
			snapshot := self.snapshotWithLock()
			self.stateLock.Unlock()
			return snapshot, true
		}
		idleChannel := self.idle
		self.stateLock.Unlock()
		select {
		case <-ctx.Done():
			return directionalLinkSnapshot{}, false
		case <-idleChannel:
		}
	}
}

// Idle means no admitted packet remains in ingress or release queues. Callers
// with live admission compare the submission generation around this barrier.
func (self *directionalLink) waitIdle(ctx context.Context) bool {
	self.stateLock.Lock()
	idle := self.activeSubmissionCount == 0 && self.queuedPacketCount == 0
	idleChannel := self.idle
	self.stateLock.Unlock()
	if idle {
		return true
	}
	if self.beforeIdleWaitForTest != nil {
		self.beforeIdleWaitForTest()
	}
	select {
	case <-ctx.Done():
		return false
	case <-idleChannel:
		return true
	}
}

// A final generation check also joins a submission that reserved ownership but
// has not yet published its monotonic submission count.
func (self *directionalLink) isTerminalAtSubmissionCount(submittedPacketCount uint64) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.submittedPackets.Load() == submittedPacketCount &&
		self.activeSubmissionCount == 0 && self.queuedPacketCount == 0 &&
		self.queuedByteCount == 0
}

// A network-wide terminal boundary repeats whenever any link admitted work
// during the idle pass. The optional hook deterministically injects that race
// in tests and is nil in measurement code.
func waitForDirectionalLinksTerminalIdle(
	ctx context.Context,
	links []*directionalLink,
	afterIdle func(),
) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		before := make([]uint64, len(links))
		for linkIndex, link := range links {
			before[linkIndex] = link.submittedPackets.Load()
		}
		for _, link := range links {
			if !link.waitIdle(ctx) {
				return false
			}
		}
		if afterIdle != nil {
			afterIdle()
		}
		stable := true
		for linkIndex, link := range links {
			if !link.isTerminalAtSubmissionCount(before[linkIndex]) {
				stable = false
				break
			}
		}
		if stable {
			return true
		}
	}
}

// A submission reserved after the first idle pass but held before generation
// publication forces another pass and remains joined through its exact terminal.
func TestDirectionalLinksTerminalIdleRetriesReservedUnpublishedSubmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
	link := newDirectionalLink(ctx, profile, 20260816, nil)
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
		joined <- waitForDirectionalLinksTerminalIdle(
			ctx,
			[]*directionalLink{link},
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
		t.Fatalf("terminal join did not retry reserved unpublished submission: %v", ctx.Err())
	case <-retryEntered:
	}
	select {
	case terminal := <-joined:
		t.Fatalf("terminal join returned through reserved unpublished submission: %t", terminal)
	default:
	}
	close(releaseDrop)
	select {
	case <-ctx.Done():
		t.Fatalf("reserved unpublished submission did not finish: %v", ctx.Err())
	case err := <-submitResult:
		if err != nil {
			t.Fatalf("submit immediate MTU drop: %v", err)
		}
	}
	select {
	case <-ctx.Done():
		t.Fatalf("terminal join did not finish after exact submission: %v", ctx.Err())
	case terminal := <-joined:
		if !terminal {
			t.Fatal("terminal join rejected completed immediate drop")
		}
	}
	if snapshot := link.snapshot(); snapshot.MtuDropPacketCount != 1 ||
		snapshot.submittedPacketCount != 1 || snapshot.activeSubmissionCount != 0 ||
		snapshot.QueuedPacketCount != 0 || snapshot.QueuedByteCount != 0 {
		t.Fatalf("reserved unpublished terminal snapshot=%+v", snapshot)
	}
}

// stopAdmissions serializes shutdown with submit registration. Its test
// barrier exposes the exact closed-admission edge without changing production.
func (self *directionalLink) stopAdmissions() {
	closedNow := false
	self.stateLock.Lock()
	if !self.closed {
		self.closed = true
		closedNow = true
	}
	self.stateLock.Unlock()
	if closedNow && self.afterAdmissionsClosedForTest != nil {
		self.afterAdmissionsClosedForTest()
	}
}

// Close stops admission, cancels, drains, and joins the scheduler goroutine.
func (self *directionalLink) close() {
	self.closeOnce.Do(func() {
		self.stopAdmissions()
		self.cancel()
		<-self.done
	})
}

// All random and timing decisions are serialized in this lifecycle loop.
func (self *directionalLink) run(seed int64) {
	defer close(self.done)
	random := rand.New(rand.NewSource(seed))
	packets := &scheduledPacketHeap{}
	heap.Init(packets)
	// Keep one explicitly disarmed timer. Resetting an expired or active timer
	// without first stopping and draining it can lose the only wake-up for a
	// packet that arrives after an idle interval on runtimes using asynchronous
	// timer channels. A lost wake-up leaves the packet owned by the link forever.
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var timerChannel <-chan time.Time
	stopTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerChannel = nil
	}
	resetTimer := func(wait time.Duration) {
		stopTimer()
		timer.Reset(max(time.Duration(0), wait))
		timerChannel = timer.C
	}
	var rateCursor time.Time
	var fifoReleaseTime time.Time
	var orderIndex uint64
	var pendingReorderPacket *linkPacket
	var highestReleasedSequence uint64
	burstBad := false

	dropForLoss := func(packet *linkPacket, profile linkProfile) bool {
		switch profile.LossModel {
		case lossModelNone:
			return false
		case lossModelIndependent:
			return random.Float64() < profile.LossProbability
		case lossModelEveryN:
			return packet.sequence%profile.DropEveryPacketCount == 0
		case lossModelBurst:
			burst := profile.BurstLoss
			if burstBad {
				if random.Float64() < burst.BadToGoodProbability {
					burstBad = false
				}
			} else if random.Float64() < burst.GoodToBadProbability {
				burstBad = true
			}
			if burstBad {
				return random.Float64() < burst.BadLossProbability
			}
			return random.Float64() < burst.GoodLossProbability
		default:
			return true
		}
	}

	releasePacket := func(packet *linkPacket) {
		finishWireTerminal := func() {
			self.counters.lastWireTerminalUnixNano.Store(time.Now().UnixNano())
			self.releaseQueue(len(packet.packetBytes))
		}
		switch packet.terminalDropCause {
		case linkTerminalDropLoss:
			self.counters.lossDropPacketCount.Add(1)
			recordLinkDropPolicy(
				packet.terminalDropAllowed,
				&self.counters.allowedLossDropPacketCount,
				&self.counters.unexpectedLossDropPacketCount,
			)
			finishWireTerminal()
			return
		case linkTerminalDropOutage:
			self.counters.outageDropPacketCount.Add(1)
			recordLinkDropPolicy(
				packet.terminalDropAllowed,
				&self.counters.allowedOutageDropPacketCount,
				&self.counters.unexpectedOutageDropPacketCount,
			)
			finishWireTerminal()
			return
		}
		if packet.sequence < highestReleasedSequence {
			self.counters.reorderedPacketCount.Add(1)
		} else if highestReleasedSequence < packet.sequence {
			highestReleasedSequence = packet.sequence
		}
		accepted := false
		deliver := packet.deliver
		if deliver == nil {
			deliver = self.deliver
		}
		if deliver != nil {
			accepted = deliver(packet.packetBytes)
		}
		if accepted {
			self.counters.deliveredPacketCount.Add(1)
			self.counters.deliveredByteCount.Add(uint64(len(packet.packetBytes)))
		} else {
			self.counters.receiverDropPacketCount.Add(1)
		}
		finishWireTerminal()
	}

	processPacket := func(packet *linkPacket) {
		self.stateLock.Lock()
		profile := self.profile
		self.stateLock.Unlock()
		terminalDropCause := linkTerminalDropNone
		terminalDropAllowed := false
		if profile.Blackhole {
			terminalDropCause = linkTerminalDropOutage
			terminalDropAllowed = true
		} else if dropForLoss(packet, profile) {
			terminalDropCause = linkTerminalDropLoss
			terminalDropAllowed = linkLossDropAllowed(profile)
		}

		now := time.Now()
		byteRate := float64(profile.RateBitsPerSecond) / 8
		burstDuration := time.Duration(float64(time.Second) * float64(profile.BurstByteCount) / byteRate)
		serializationDuration := time.Duration(float64(time.Second) * float64(len(packet.packetBytes)) / byteRate)
		minimumCursor := now.Add(-burstDuration)
		if rateCursor.Before(minimumCursor) {
			rateCursor = minimumCursor
		}
		rateCursor = rateCursor.Add(serializationDuration)
		rateReady := now
		if rateReady.Before(rateCursor) {
			rateReady = rateCursor
		}
		jitter := time.Duration(0)
		reorderCandidate := false
		// Loss and outage are classified before the survivor-only stochastic
		// stages. They still receive fixed propagation and processing delay, but
		// skip later random draws so existing seeded loss vectors stay stable.
		if terminalDropCause == linkTerminalDropNone {
			if 0 < profile.Jitter {
				jitterRange := int64(2*profile.Jitter) + 1
				jitter = time.Duration(random.Int63n(jitterRange) - int64(profile.Jitter))
			}
			reorderCandidate = pendingReorderPacket == nil &&
				random.Float64() < profile.ReorderProbability
		}
		releaseTime := rateReady.Add(profile.BaseDelay + profile.ProcessingDelay + jitter)
		if releaseTime.Before(now) {
			releaseTime = now
		}
		// One directional link is FIFO unless its explicit reorder stage
		// selects this packet. Independent jitter varies latency without
		// silently adding a second source of packet reordering.
		if releaseTime.Before(fifoReleaseTime) {
			releaseTime = fifoReleaseTime
		}
		fifoReleaseTime = releaseTime
		if reorderCandidate {
			releaseTime = releaseTime.Add(max(profile.BaseDelay/2, time.Millisecond))
		}
		orderIndex += 1
		packet.releaseTime = releaseTime
		packet.orderIndex = orderIndex
		packet.terminalDropCause = terminalDropCause
		packet.terminalDropAllowed = terminalDropAllowed
		heap.Push(packets, packet)
		if terminalDropCause == linkTerminalDropNone {
			if pendingReorderPacket != nil {
				if !packet.releaseTime.Before(pendingReorderPacket.releaseTime) {
					pendingReorderPacket.releaseTime = packet.releaseTime.Add(time.Microsecond)
					heap.Fix(packets, pendingReorderPacket.heapIndex)
					if duplicate := pendingReorderPacket.duplicatePacket; duplicate != nil &&
						0 <= duplicate.heapIndex {
						duplicate.releaseTime = pendingReorderPacket.releaseTime.Add(time.Microsecond)
						heap.Fix(packets, duplicate.heapIndex)
					}
				}
				pendingReorderPacket = nil
				if self.afterReorderPairForTest != nil {
					self.afterReorderPairForTest()
				}
			} else if reorderCandidate {
				pendingReorderPacket = packet
			}
		}
		// Every admitted original reaches this in-path wire boundary. MTU and
		// ingress-queue rejections returned before scheduler ingress do not.
		self.counters.wireByteCount.Add(uint64(len(packet.packetBytes)))

		duplicateScheduled := false
		var duplicateRateReady time.Time
		var duplicateReleaseTime time.Time
		if terminalDropCause == linkTerminalDropNone &&
			random.Float64() < profile.DuplicateProbability &&
			self.reserveDuplicate(len(packet.packetBytes)) {
			// A duplicate is another physical outer packet. It consumes the next
			// token serialization without drawing another stochastic decision.
			rateCursor = rateCursor.Add(serializationDuration)
			duplicateRateReady = now
			if duplicateRateReady.Before(rateCursor) {
				duplicateRateReady = rateCursor
			}
			duplicateReleaseTime = duplicateRateReady.Add(
				profile.BaseDelay + profile.ProcessingDelay + jitter,
			)
			if duplicateReleaseTime.Before(now) {
				duplicateReleaseTime = now
			}
			if !packet.releaseTime.Before(duplicateReleaseTime) {
				duplicateReleaseTime = packet.releaseTime.Add(time.Microsecond)
			}
			orderIndex += 1
			duplicate := &linkPacket{
				sequence:    packet.sequence,
				packetBytes: append([]byte(nil), packet.packetBytes...),
				deliver:     packet.deliver,
				releaseTime: duplicateReleaseTime,
				orderIndex:  orderIndex,
				heapIndex:   -1,
			}
			heap.Push(packets, duplicate)
			packet.duplicatePacket = duplicate
			self.counters.duplicatePacketCount.Add(1)
			self.counters.wireByteCount.Add(uint64(len(duplicate.packetBytes)))
			duplicateScheduled = true
		}
		if hook := self.afterPacketScheduledForTest.Load(); hook != nil {
			hook.call(linkScheduleObservation{
				sequence:               packet.sequence,
				packetByteCount:        len(packet.packetBytes),
				scheduleTime:           now,
				rateReadyTime:          rateReady,
				releaseTime:            releaseTime,
				terminalDropCause:      terminalDropCause,
				duplicateScheduled:     duplicateScheduled,
				duplicateRateReadyTime: duplicateRateReady,
				duplicateReleaseTime:   duplicateReleaseTime,
			})
		}
	}

	for {
		now := time.Now()
		releasedCount := 0
		for 0 < packets.Len() && releasedCount < self.releaseBatchSize {
			packet := (*packets)[0]
			if now.Before(packet.releaseTime) {
				break
			}
			heap.Pop(packets)
			if pendingReorderPacket == packet {
				pendingReorderPacket = nil
			}
			releasePacket(packet)
			releasedCount += 1
		}
		if 0 < packets.Len() {
			resetTimer(time.Until((*packets)[0].releaseTime))
		} else {
			stopTimer()
		}

		select {
		case <-self.ctx.Done():
			self.stopAdmissions()
			if self.beforeSubmissionWaitForTest != nil {
				self.beforeSubmissionWaitForTest()
			}
			self.submissionWaitGroup.Wait()
			for {
				select {
				case packet := <-self.ingress:
					self.counters.canceledDropPacketCount.Add(1)
					self.releaseQueue(len(packet.packetBytes))
				default:
					for 0 < packets.Len() {
						packet := heap.Pop(packets).(*linkPacket)
						self.counters.canceledDropPacketCount.Add(1)
						self.counters.lastWireTerminalUnixNano.Store(time.Now().UnixNano())
						self.releaseQueue(len(packet.packetBytes))
					}
					return
				}
			}
		case update := <-self.updates:
			actual := time.Now()
			self.stateLock.Lock()
			self.profile = update.profile
			self.stateLock.Unlock()
			self.counters.profileUpdateCount.Add(1)
			update.applied <- actual
		case packet := <-self.ingress:
			processPacket(packet)
		case <-timerChannel:
		}
	}
}

// Immediate MTU and queue rejections retain the policy active at admission.
func TestDirectionalLinkSnapshotsImmediateDropPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
	profile.AllowMtuDrops = false
	profile.AllowQueueDrops = false
	profile.QueuePacketCount = 0
	link := newDirectionalLink(ctx, profile, 20260811, nil)
	defer link.close()
	if _, err := link.submit(make([]byte, profile.OuterMtu+1)); err != nil {
		t.Fatalf("submit MTU diagnostic: %v", err)
	}
	if _, err := link.submit([]byte{1}); err != nil {
		t.Fatalf("submit queue diagnostic: %v", err)
	}
	snapshot := link.snapshot()
	if snapshot.MtuDropPacketCount != 1 || snapshot.UnexpectedMtuDropPacketCount != 1 ||
		snapshot.AllowedMtuDropPacketCount != 0 {
		t.Fatalf("MTU policy attribution=%+v", snapshot)
	}
	if snapshot.QueueDropPacketCount != 1 || snapshot.UnexpectedQueueDropPacketCount != 1 ||
		snapshot.AllowedQueueDropPacketCount != 0 {
		t.Fatalf("queue policy attribution=%+v", snapshot)
	}
}

// An immediate MTU disposition remains active until its counters and submitted
// generation are published, so terminal idle cannot pass through that gap.
func TestDirectionalLinkTerminalIdleJoinsHeldImmediateDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
	link := newDirectionalLink(ctx, profile, 20260813, nil)
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
	idleWaitEntered := make(chan struct{})
	var idleWaitEnteredOnce sync.Once
	link.beforeIdleWaitForTest = func() {
		idleWaitEnteredOnce.Do(func() { close(idleWaitEntered) })
	}
	submitResult := make(chan error, 1)
	go func() {
		_, err := link.submit(make([]byte, profile.OuterMtu+1))
		submitResult <- err
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for held immediate drop: %v", ctx.Err())
	case <-dropEntered:
	}
	joined := make(chan bool, 1)
	go func() {
		joined <- waitForDirectionalLinksTerminalIdle(
			ctx,
			[]*directionalLink{link},
			nil,
		)
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for immediate-drop idle join: %v", ctx.Err())
	case terminal := <-joined:
		t.Fatalf("terminal idle returned through active immediate drop: %t", terminal)
	case <-idleWaitEntered:
	}
	close(releaseDrop)
	select {
	case <-ctx.Done():
		t.Fatalf("wait for immediate-drop submission: %v", ctx.Err())
	case err := <-submitResult:
		if err != nil {
			t.Fatalf("submit immediate MTU drop: %v", err)
		}
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for immediate-drop terminal boundary: %v", ctx.Err())
	case terminal := <-joined:
		if !terminal {
			t.Fatal("terminal idle rejected completed immediate drop")
		}
	}
	snapshot := link.snapshot()
	if snapshot.MtuDropPacketCount != 1 || snapshot.submittedPacketCount != 1 {
		t.Fatalf("immediate-drop terminal snapshot=%+v", snapshot)
	}
}

// A producer paused before link admission linearizes after the workload epoch
// begins, so its packet count and packet-size maximum stay in the same interval.
func TestDirectionalLinkIntervalIncludesProducerAdmittedAfterBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
	link := newDirectionalLink(ctx, profile, 20260814, func([]byte) bool { return true })
	defer link.close()

	producerEntered := make(chan struct{})
	releaseProducer := make(chan struct{})
	link.beforeAdmissionForTest = func() {
		close(producerEntered)
		select {
		case <-releaseProducer:
		case <-ctx.Done():
		}
	}
	submitResult := make(chan error, 1)
	const packetByteCount = 321
	go func() {
		_, err := link.submit(make([]byte, packetByteCount))
		submitResult <- err
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for pre-admission producer: %v", ctx.Err())
	case <-producerEntered:
	}
	if !waitForDirectionalLinksTerminalIdle(ctx, []*directionalLink{link}, nil) {
		t.Fatalf("join pre-admission carrier boundary: %v", ctx.Err())
	}
	start, ok := link.beginMeasurementSnapshot(ctx)
	if !ok {
		t.Fatalf("begin post-producer interval: %v", ctx.Err())
	}
	close(releaseProducer)
	select {
	case <-ctx.Done():
		t.Fatalf("wait for post-boundary admission: %v", ctx.Err())
	case err := <-submitResult:
		if err != nil {
			t.Fatalf("submit post-boundary packet: %v", err)
		}
	}
	if !waitForDirectionalLinksTerminalIdle(ctx, []*directionalLink{link}, nil) {
		t.Fatalf("join post-boundary packet: %v", ctx.Err())
	}
	delta := subtractDirectionalLinkSnapshot(start, link.snapshot(), time.Second)
	if delta.AdmittedPacketCount != 1 || delta.AdmittedByteCount != packetByteCount ||
		delta.MaximumQueuedPackets != 1 || delta.MaximumQueuedBytes != packetByteCount ||
		delta.MaximumSubmittedPacketBytes != packetByteCount {
		t.Fatalf("post-boundary producer interval=%+v", delta)
	}
}

// A canceled measurement start exits an occupied link deterministically
// instead of converting the boundary failure into a process-wide test abort.
func TestDirectionalLinkBeginMeasurementSnapshotHonorsCanceledContext(t *testing.T) {
	linkCtx, linkCancel := context.WithCancel(context.Background())
	defer linkCancel()
	profile := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
	link := newDirectionalLink(linkCtx, profile, 20260815, func([]byte) bool { return true })
	defer link.close()

	ingressEntered := make(chan struct{})
	releaseIngress := make(chan struct{})
	link.beforeIngressForTest = func() {
		close(ingressEntered)
		select {
		case <-releaseIngress:
		case <-linkCtx.Done():
		}
	}
	submitResult := make(chan error, 1)
	go func() {
		_, err := link.submit([]byte{1})
		submitResult <- err
	}()
	select {
	case <-linkCtx.Done():
		t.Fatalf("wait for occupied measurement-start link: %v", linkCtx.Err())
	case <-ingressEntered:
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := link.beginMeasurementSnapshot(canceledCtx); ok {
		t.Fatal("canceled measurement start returned a baseline")
	}
	close(releaseIngress)
	select {
	case <-linkCtx.Done():
		t.Fatalf("wait for occupied-link submission: %v", linkCtx.Err())
	case err := <-submitResult:
		if err != nil {
			t.Fatalf("submit occupied-link packet: %v", err)
		}
	}
}

// A later profile update cannot reclassify a packet dropped under an earlier policy.
func TestDirectionalLinkSnapshotRetainsSchedulingPolicyAcrossUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
	profile.LossModel = lossModelEveryN
	profile.DropEveryPacketCount = 1
	link := newDirectionalLink(ctx, profile, 20260812, nil)
	defer link.close()
	if _, err := link.submit([]byte{1}); err != nil {
		t.Fatalf("submit configured loss: %v", err)
	}
	if !waitForDirectionalLinksTerminalIdle(ctx, []*directionalLink{link}, nil) {
		t.Fatalf("configured loss terminal: %v", ctx.Err())
	}
	cleanProfile := profile
	cleanProfile.LossModel = lossModelNone
	cleanProfile.DropEveryPacketCount = 0
	if _, err := link.updateProfile(cleanProfile, "clean-after-loss", time.Now()); err != nil {
		t.Fatalf("update profile: %v", err)
	}
	snapshot := link.snapshot()
	if snapshot.LossDropPacketCount != 1 || snapshot.AllowedLossDropPacketCount != 1 ||
		snapshot.UnexpectedLossDropPacketCount != 0 {
		t.Fatalf("loss policy attribution after update=%+v", snapshot)
	}
}
