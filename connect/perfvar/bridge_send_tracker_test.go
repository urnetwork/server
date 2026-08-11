// This file gives measured app-TUN uploads an exact flow-specific bridge
// lifecycle instead of sampling counters shared with unrelated TUN traffic.
package perfvar

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	clientconnect "github.com/urnetwork/connect"
)

const (
	fullTunBridgeSendEntryPending uint32 = iota
	fullTunBridgeSendEntryCompleting
	fullTunBridgeSendEntryTerminal
)

// A comparable full tuple separates the measured UDP socket from delayed,
// control, ICMP, and other application-TUN traffic.
type fullTunBridgeFlowKey struct {
	ipVersion       uint8
	protocol        clientconnect.IpProtocol
	sourceIp        [16]byte
	sourcePort      uint16
	destinationIp   [16]byte
	destinationPort uint16
	valid           bool
}

// One bridge admission retains immutable flow and byte accounting until its
// matching multi-client send publishes terminal ownership.
type fullTunBridgeSendEntry struct {
	windowId        uint64
	flowKey         fullTunBridgeFlowKey
	packetByteCount clientconnect.ByteCount
	state           atomic.Uint32
	sent            atomic.Bool
}

// A public window token prevents an old workload from finishing or observing
// a replacement window for the same socket tuple.
type fullTunBridgeFlowWindow struct {
	id      uint64
	flowKey fullTunBridgeFlowKey
}

// An immutable boundary captures the exact matching entries that contribute
// to one packet-and-byte target.
type fullTunBridgeSendBoundary struct {
	window          fullTunBridgeFlowWindow
	entries         []*fullTunBridgeSendEntry
	packetCount     int64
	packetByteCount clientconnect.ByteCount
}

// The active window is state-lock owned. Bridge producers publish their
// generation before parsing so begin/finish cannot adopt an older packet.
type fullTunBridgeFlowState struct {
	window          fullTunBridgeFlowWindow
	entries         []*fullTunBridgeSendEntry
	packetCount     int64
	packetByteCount clientconnect.ByteCount
	invalid         bool
}

// Concurrent bridge publication and workload boundaries share one short
// state lock; terminal state is atomic so waits never hold that lock.
type fullTunBridgeSendTracker struct {
	stateLock                    sync.Mutex
	active                       *fullTunBridgeFlowState
	liveEntries                  map[*fullTunBridgeSendEntry]struct{}
	nextWindowId                 uint64
	activeWindowId               atomic.Uint64
	startedCount                 atomic.Uint64
	publishingCount              atomic.Int64
	failureCount                 atomic.Uint64
	progress                     chan struct{}
	beforeStartPublishForTest    atomic.Pointer[fullTunBridgeStartTestHook]
	beforePublisherWaitForTest   atomic.Pointer[fullTunBridgeStartTestHook]
	beforeTerminalReleaseForTest atomic.Pointer[fullTunBridgeTerminalTestHook]
}

// An immutable optional hook exposes source entry and boundary publication.
type fullTunBridgeStartTestHook struct {
	call func()
}

// An immutable optional hook makes terminal publication ordering reproducible.
type fullTunBridgeTerminalTestHook struct {
	call func(*fullTunBridgeSendEntry)
}

// An exact all-item source boundary complements the measured-flow boundary.
// It prevents setup traffic held above SendPack from crossing a carrier start.
type fullTunBridgeLifecycleBoundary struct {
	entries      []*fullTunBridgeSendEntry
	startedCount uint64
}

// Construction initializes the coalesced waiter notification.
func newFullTunBridgeSendTracker() *fullTunBridgeSendTracker {
	return &fullTunBridgeSendTracker{
		liveEntries: map[*fullTunBridgeSendEntry]struct{}{},
		progress:    make(chan struct{}, 1),
	}
}

// A best-effort edge wakes waiters, which always re-read exact state.
func (self *fullTunBridgeSendTracker) notify() {
	select {
	case self.progress <- struct{}{}:
	default:
	}
}

// Address conversion keeps IPv4 and IPv4-mapped forms in one canonical key.
func fullTunBridgeAddressIp(address netip.Addr, target *[16]byte) uint8 {
	address = address.Unmap()
	if address.Is4() {
		value := address.As4()
		copy(target[:], value[:])
		return 4
	}
	value := address.As16()
	copy(target[:], value[:])
	return 6
}

// Socket addresses independently define the expected upload direction before
// any bridge observation is allowed to select it.
func fullTunBridgeUdpFlowKey(
	sourceAddress net.Addr,
	destinationAddress net.Addr,
) (fullTunBridgeFlowKey, error) {
	parseAddress := func(name string, address net.Addr) (netip.AddrPort, error) {
		if address == nil {
			return netip.AddrPort{}, fmt.Errorf("%s address is nil", name)
		}
		addressPort, err := netip.ParseAddrPort(address.String())
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("parse %s address %q: %w", name, address, err)
		}
		return netip.AddrPortFrom(addressPort.Addr().Unmap(), addressPort.Port()), nil
	}
	source, err := parseAddress("source", sourceAddress)
	if err != nil {
		return fullTunBridgeFlowKey{}, err
	}
	destination, err := parseAddress("destination", destinationAddress)
	if err != nil {
		return fullTunBridgeFlowKey{}, err
	}
	if source.Addr().Is4() != destination.Addr().Is4() {
		return fullTunBridgeFlowKey{}, fmt.Errorf(
			"bridge flow mixes source %s and destination %s IP families",
			source,
			destination,
		)
	}
	flowKey := fullTunBridgeFlowKey{
		protocol:        clientconnect.IpProtocolUdp,
		sourcePort:      source.Port(),
		destinationPort: destination.Port(),
		valid:           true,
	}
	flowKey.ipVersion = fullTunBridgeAddressIp(source.Addr(), &flowKey.sourceIp)
	if destinationVersion := fullTunBridgeAddressIp(
		destination.Addr(),
		&flowKey.destinationIp,
	); destinationVersion != flowKey.ipVersion {
		return fullTunBridgeFlowKey{}, fmt.Errorf(
			"bridge flow mixes source IPv%d and destination IPv%d",
			flowKey.ipVersion,
			destinationVersion,
		)
	}
	return flowKey, nil
}

// Parsed app-TUN packets use the same canonical tuple representation as the
// independently derived socket key.
func fullTunBridgeFlowKeyFromIpPath(ipPath *clientconnect.IpPath) fullTunBridgeFlowKey {
	if ipPath == nil || ipPath.SourcePort < 0 || 65535 < ipPath.SourcePort ||
		ipPath.DestinationPort < 0 || 65535 < ipPath.DestinationPort {
		return fullTunBridgeFlowKey{}
	}
	sourceAddress, sourceOk := netip.AddrFromSlice(ipPath.SourceIp)
	destinationAddress, destinationOk := netip.AddrFromSlice(ipPath.DestinationIp)
	if !sourceOk || !destinationOk {
		return fullTunBridgeFlowKey{}
	}
	flowKey := fullTunBridgeFlowKey{
		protocol:        ipPath.Protocol,
		sourcePort:      uint16(ipPath.SourcePort),
		destinationPort: uint16(ipPath.DestinationPort),
		valid:           true,
	}
	flowKey.ipVersion = fullTunBridgeAddressIp(sourceAddress, &flowKey.sourceIp)
	if fullTunBridgeAddressIp(destinationAddress, &flowKey.destinationIp) != flowKey.ipVersion {
		return fullTunBridgeFlowKey{}
	}
	return flowKey
}

// Opening publishes a unique active generation before measured application
// writes begin. Only one workload can own the bridge at a time.
func (self *fullTunBridgeSendTracker) beginFlowWindow(
	flowKey fullTunBridgeFlowKey,
) (fullTunBridgeFlowWindow, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !flowKey.valid || self.active != nil {
		return fullTunBridgeFlowWindow{}, false
	}
	self.nextWindowId += 1
	if self.nextWindowId == 0 {
		self.nextWindowId += 1
	}
	window := fullTunBridgeFlowWindow{id: self.nextWindowId, flowKey: flowKey}
	self.active = &fullTunBridgeFlowState{window: window}
	self.activeWindowId.Store(window.id)
	self.notify()
	return window, true
}

// A bridge producer records matching immutable metadata before transferring
// packet ownership to the multi-client sender.
func (self *fullTunBridgeSendTracker) startPacket(packet []byte) *fullTunBridgeSendEntry {
	self.publishingCount.Add(1)
	self.startedCount.Add(1)
	defer func() {
		self.publishingCount.Add(-1)
		self.notify()
	}()
	if hook := self.beforeStartPublishForTest.Load(); hook != nil {
		hook.call()
	}
	windowId := self.activeWindowId.Load()
	if windowId == 0 {
		return self.startWithMetadata(0, fullTunBridgeFlowKey{}, clientconnect.ByteCount(len(packet)))
	}
	ipPath, err := clientconnect.ParseIpPath(packet)
	if err != nil {
		return self.startWithMetadata(windowId, fullTunBridgeFlowKey{}, clientconnect.ByteCount(len(packet)))
	}
	return self.startWithMetadata(
		windowId,
		fullTunBridgeFlowKeyFromIpPath(ipPath),
		clientconnect.ByteCount(len(packet)),
	)
}

// The metadata form supports deterministic lifecycle tests without building a
// synthetic IP packet; production bridge calls use startPacket.
func (self *fullTunBridgeSendTracker) start(
	windowId uint64,
	flowKey fullTunBridgeFlowKey,
	packetByteCount clientconnect.ByteCount,
) *fullTunBridgeSendEntry {
	self.publishingCount.Add(1)
	self.startedCount.Add(1)
	defer func() {
		self.publishingCount.Add(-1)
		self.notify()
	}()
	if hook := self.beforeStartPublishForTest.Load(); hook != nil {
		hook.call()
	}
	return self.startWithMetadata(windowId, flowKey, packetByteCount)
}

// State publication retains every bridge item for the route-wide lifecycle,
// while only an exact active tuple enters measured-flow accounting.
func (self *fullTunBridgeSendTracker) startWithMetadata(
	windowId uint64,
	flowKey fullTunBridgeFlowKey,
	packetByteCount clientconnect.ByteCount,
) *fullTunBridgeSendEntry {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry := &fullTunBridgeSendEntry{
		windowId:        windowId,
		flowKey:         flowKey,
		packetByteCount: packetByteCount,
	}
	self.liveEntries[entry] = struct{}{}
	if self.active != nil && self.active.window.id == windowId &&
		self.active.window.flowKey == flowKey {
		self.active.entries = append(self.active.entries, entry)
		self.active.packetCount += 1
		self.active.packetByteCount += packetByteCount
	}
	self.notify()
	return entry
}

// Terminal publication pairs exactly one admitted packet with the
// multi-client ownership result; unrelated packets have a nil entry.
func (self *fullTunBridgeSendTracker) terminal(
	entry *fullTunBridgeSendEntry,
	sent bool,
) {
	if entry == nil {
		return
	}
	if !entry.state.CompareAndSwap(
		fullTunBridgeSendEntryPending,
		fullTunBridgeSendEntryCompleting,
	) {
		self.stateLock.Lock()
		if self.active != nil && self.active.window.id == entry.windowId {
			self.active.invalid = true
		}
		self.stateLock.Unlock()
		self.notify()
		return
	}
	entry.sent.Store(sent)
	if !sent {
		self.failureCount.Add(1)
	}
	if hook := self.beforeTerminalReleaseForTest.Load(); hook != nil {
		hook.call(entry)
	}
	entry.state.Store(fullTunBridgeSendEntryTerminal)
	self.stateLock.Lock()
	delete(self.liveEntries, entry)
	self.stateLock.Unlock()
	self.notify()
}

// Publisher entry is fenced before the live-item snapshot, closing the zero-
// publisher race around a bridge item held before packet parsing or SendPack.
func (self *fullTunBridgeSendTracker) lifecycleBoundary(
	ctx context.Context,
) (fullTunBridgeLifecycleBoundary, bool) {
	for {
		startedBefore := self.startedCount.Load()
		for self.publishingCount.Load() != 0 {
			if hook := self.beforePublisherWaitForTest.Load(); hook != nil {
				hook.call()
			}
			select {
			case <-ctx.Done():
				return fullTunBridgeLifecycleBoundary{}, false
			case <-self.progress:
			}
		}
		self.stateLock.Lock()
		boundary := fullTunBridgeLifecycleBoundary{
			entries:      make([]*fullTunBridgeSendEntry, 0, len(self.liveEntries)),
			startedCount: self.startedCount.Load(),
		}
		for entry := range self.liveEntries {
			boundary.entries = append(boundary.entries, entry)
		}
		self.stateLock.Unlock()
		if self.publishingCount.Load() == 0 &&
			startedBefore == self.startedCount.Load() &&
			boundary.startedCount == self.startedCount.Load() {
			return boundary, true
		}
	}
}

// Waiting retains the concrete live entries through terminal ownership. A
// fully terminal setup failure remains before, rather than poisoning, baseline.
func (self *fullTunBridgeSendTracker) waitLifecycleThrough(
	ctx context.Context,
	boundary fullTunBridgeLifecycleBoundary,
) bool {
	for {
		complete := true
		for _, entry := range boundary.entries {
			if entry.state.Load() != fullTunBridgeSendEntryTerminal {
				complete = false
				break
			}
		}
		if complete {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-self.progress:
		}
	}
}

// A nil-by-default source barrier forces pre-SendPack publication ordering.
func (self *fullTunBridgeSendTracker) setBeforeStartPublishForTest(callback func()) {
	if callback == nil {
		self.beforeStartPublishForTest.Store(nil)
		return
	}
	self.beforeStartPublishForTest.Store(&fullTunBridgeStartTestHook{call: callback})
}

// A nil-by-default boundary barrier proves the source publisher was joined.
func (self *fullTunBridgeSendTracker) setBeforePublisherWaitForTest(callback func()) {
	if callback == nil {
		self.beforePublisherWaitForTest.Store(nil)
		return
	}
	self.beforePublisherWaitForTest.Store(&fullTunBridgeStartTestHook{call: callback})
}

// A boundary waits until the active flow reaches exactly the requested packet
// and full-IP byte target, then freezes those concrete entries.
func (self *fullTunBridgeSendTracker) flowBoundary(
	ctx context.Context,
	window fullTunBridgeFlowWindow,
	expectedPacketCount int64,
	expectedPacketByteCount clientconnect.ByteCount,
) (fullTunBridgeSendBoundary, bool) {
	for {
		var boundary fullTunBridgeSendBoundary
		var exact bool
		var valid bool
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.active == nil || self.active.window != window || self.active.invalid {
				return
			}
			valid = true
			if expectedPacketCount < self.active.packetCount ||
				expectedPacketByteCount < self.active.packetByteCount {
				self.active.invalid = true
				valid = false
				return
			}
			if self.active.packetCount == expectedPacketCount &&
				self.active.packetByteCount == expectedPacketByteCount {
				boundary = fullTunBridgeSendBoundary{
					window:          window,
					entries:         append([]*fullTunBridgeSendEntry{}, self.active.entries...),
					packetCount:     self.active.packetCount,
					packetByteCount: self.active.packetByteCount,
				}
				exact = true
			}
		}()
		if !valid {
			return fullTunBridgeSendBoundary{}, false
		}
		if exact {
			return boundary, true
		}
		select {
		case <-ctx.Done():
			return fullTunBridgeSendBoundary{}, false
		case <-self.progress:
		}
	}
}

// Waiting joins every exact entry and rejects any terminal send failure. The
// context is only a liveness bound.
func (self *fullTunBridgeSendTracker) waitThrough(
	ctx context.Context,
	boundary fullTunBridgeSendBoundary,
) bool {
	status := func() (complete bool, failed bool) {
		for _, entry := range boundary.entries {
			if entry.state.Load() != fullTunBridgeSendEntryTerminal {
				return false, false
			}
			if !entry.sent.Load() {
				return false, true
			}
		}
		return true, false
	}
	for {
		complete, failed := status()
		if failed {
			return false
		}
		if complete {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-self.progress:
		}
	}
}

// Finishing clears only the exact current generation after all captured sends
// have reached a terminal state.
func (self *fullTunBridgeSendTracker) finishFlowWindow(
	window fullTunBridgeFlowWindow,
) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.active == nil || self.active.window != window || self.active.invalid {
		return false
	}
	for _, entry := range self.active.entries {
		if entry.state.Load() != fullTunBridgeSendEntryTerminal || !entry.sent.Load() {
			return false
		}
	}
	self.active = nil
	self.activeWindowId.Store(0)
	self.notify()
	return true
}

// Incidental traffic deterministically increments the former global failure
// view but cannot enter or fail the measured full-tuple boundary.
func TestFullTunBridgeSendTrackerIgnoresIncidentalGlobalDrop(t *testing.T) {
	tracker := newFullTunBridgeSendTracker()
	targetFlow := fullTunBridgeFlowKey{
		ipVersion: 4, protocol: clientconnect.IpProtocolUdp,
		sourcePort: 41000, destinationPort: 42000, valid: true,
	}
	targetFlow.sourceIp[0] = 10
	targetFlow.destinationIp[0] = 127
	window, ok := tracker.beginFlowWindow(targetFlow)
	if !ok {
		t.Fatal("begin target bridge flow window")
	}

	var formerGlobalProcessed atomic.Uint64
	var formerGlobalDrops atomic.Uint64
	incidentalFlows := []fullTunBridgeFlowKey{}
	for _, mutate := range []func(*fullTunBridgeFlowKey){
		func(flowKey *fullTunBridgeFlowKey) { flowKey.sourceIp[1] += 1 },
		func(flowKey *fullTunBridgeFlowKey) { flowKey.destinationIp[1] += 1 },
		func(flowKey *fullTunBridgeFlowKey) { flowKey.sourcePort += 1 },
		func(flowKey *fullTunBridgeFlowKey) { flowKey.destinationPort += 1 },
		func(flowKey *fullTunBridgeFlowKey) { flowKey.protocol = clientconnect.IpProtocolTcp },
	} {
		incidentalFlow := targetFlow
		mutate(&incidentalFlow)
		incidentalFlows = append(incidentalFlows, incidentalFlow)
	}
	for _, incidentalFlow := range incidentalFlows {
		incidentalEntry := tracker.start(window.id, incidentalFlow, 128)
		tracker.terminal(incidentalEntry, false)
		formerGlobalProcessed.Add(1)
		formerGlobalDrops.Add(1)
		if incidentalEntry == nil {
			t.Fatalf("incidental flow %+v omitted all-item source ownership", incidentalFlow)
		}
	}

	targetEntry := tracker.start(window.id, targetFlow, 1028)
	if targetEntry == nil {
		t.Fatal("target flow did not enter measured bridge window")
	}
	boundary, ok := tracker.flowBoundary(t.Context(), window, 1, 1028)
	if !ok {
		t.Fatal("capture exact target bridge boundary")
	}
	terminalEntered := make(chan struct{})
	releaseTerminal := make(chan struct{})
	tracker.beforeTerminalReleaseForTest.Store(&fullTunBridgeTerminalTestHook{
		call: func(entry *fullTunBridgeSendEntry) {
			if entry == targetEntry {
				close(terminalEntered)
				<-releaseTerminal
			}
		},
	})
	terminalDone := make(chan struct{})
	go func() {
		tracker.terminal(targetEntry, true)
		close(terminalDone)
	}()
	<-terminalEntered
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if tracker.waitThrough(canceledCtx, boundary) {
		t.Fatal("bridge boundary passed before exact terminal publication")
	}
	close(releaseTerminal)
	<-terminalDone
	if !tracker.waitThrough(t.Context(), boundary) {
		t.Fatal("exact target terminal did not satisfy bridge boundary")
	}
	formerGlobalProcessed.Add(1)
	if formerGlobalProcessed.Load() != uint64(len(incidentalFlows)+1) ||
		formerGlobalDrops.Load() != uint64(len(incidentalFlows)) {
		t.Fatal("regression did not reproduce former global-counter contamination")
	}
	if tracker.failureCount.Load() != uint64(len(incidentalFlows)) {
		t.Fatal("regression did not reproduce former global failure view")
	}
	if !tracker.finishFlowWindow(window) {
		t.Fatal("finish exact target bridge flow window")
	}
}

// A same-flow packet beyond the requested exact count or byte target is a
// structural measurement error, never a later item to ignore.
func TestFullTunBridgeSendTrackerRejectsSameFlowOvershoot(t *testing.T) {
	tracker := newFullTunBridgeSendTracker()
	flowKey := fullTunBridgeFlowKey{
		ipVersion: 4, protocol: clientconnect.IpProtocolUdp,
		sourcePort: 43000, destinationPort: 44000, valid: true,
	}
	window, ok := tracker.beginFlowWindow(flowKey)
	if !ok {
		t.Fatal("begin overshoot bridge flow window")
	}
	first := tracker.start(window.id, flowKey, 100)
	second := tracker.start(window.id, flowKey, 100)
	tracker.terminal(first, true)
	tracker.terminal(second, true)
	if _, ok := tracker.flowBoundary(t.Context(), window, 1, 100); ok {
		t.Fatal("same-flow overshoot satisfied smaller exact boundary")
	}
	if tracker.finishFlowWindow(window) {
		t.Fatal("invalid overshoot window finished successfully")
	}
}

// A matching terminal rejection is reported immediately and prevents a
// failed measured window from being finalized as successful.
func TestFullTunBridgeSendTrackerRejectsFailedTerminal(t *testing.T) {
	tracker := newFullTunBridgeSendTracker()
	flowKey := fullTunBridgeFlowKey{
		ipVersion: 4, protocol: clientconnect.IpProtocolUdp,
		sourcePort: 45000, destinationPort: 46000, valid: true,
	}
	window, ok := tracker.beginFlowWindow(flowKey)
	if !ok {
		t.Fatal("begin failed-terminal bridge flow window")
	}
	entry := tracker.start(window.id, flowKey, 200)
	boundary, ok := tracker.flowBoundary(t.Context(), window, 1, 200)
	if !ok {
		t.Fatal("capture failed-terminal bridge boundary")
	}
	tracker.terminal(entry, false)
	if tracker.waitThrough(t.Context(), boundary) {
		t.Fatal("failed matching terminal satisfied bridge boundary")
	}
	if tracker.failureCount.Load() != 1 {
		t.Fatalf("failed matching terminal count=%d, want 1", tracker.failureCount.Load())
	}
	if tracker.finishFlowWindow(window) {
		t.Fatal("failed-terminal window finished successfully")
	}
}

// A duplicate callback from a fully retired generation cannot invalidate a
// replacement window, even when the replacement reuses the same flow tuple.
func TestFullTunBridgeSendTrackerStaleDuplicateDoesNotPoisonReplacement(t *testing.T) {
	tracker := newFullTunBridgeSendTracker()
	flowKey := fullTunBridgeFlowKey{
		ipVersion: 4, protocol: clientconnect.IpProtocolUdp,
		sourcePort: 47000, destinationPort: 48000, valid: true,
	}
	firstWindow, ok := tracker.beginFlowWindow(flowKey)
	if !ok {
		t.Fatal("begin first bridge flow window")
	}
	firstEntry := tracker.start(firstWindow.id, flowKey, 300)
	firstBoundary, ok := tracker.flowBoundary(t.Context(), firstWindow, 1, 300)
	if !ok {
		t.Fatal("capture first bridge boundary")
	}
	tracker.terminal(firstEntry, true)
	if !tracker.waitThrough(t.Context(), firstBoundary) || !tracker.finishFlowWindow(firstWindow) {
		t.Fatal("complete first bridge flow window")
	}

	secondWindow, ok := tracker.beginFlowWindow(flowKey)
	if !ok {
		t.Fatal("begin replacement bridge flow window")
	}
	tracker.terminal(firstEntry, true)
	secondEntry := tracker.start(secondWindow.id, flowKey, 400)
	secondBoundary, ok := tracker.flowBoundary(t.Context(), secondWindow, 1, 400)
	if !ok {
		t.Fatal("stale duplicate invalidated replacement bridge window")
	}
	tracker.terminal(secondEntry, true)
	if !tracker.waitThrough(t.Context(), secondBoundary) ||
		!tracker.finishFlowWindow(secondWindow) {
		t.Fatal("replacement bridge window did not survive stale duplicate")
	}
}
