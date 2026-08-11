// This file tracks exact provider-return items by authenticated UDP flow so
// unrelated traffic cannot satisfy a PERFVAR download source boundary.
package perfvar

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

const providerReturnSendEventCapacity = 64 * 1024

const (
	providerReturnSendEntryPending uint32 = iota
	providerReturnSendEntryCompleting
	providerReturnSendEntryTerminal
)

// The tracker keeps hot-path callbacks nonblocking while one owner validates
// token pairing, flow identity, counts, bytes, and window membership.
type providerReturnSendTracker struct {
	ctx       context.Context
	cancel    context.CancelFunc
	events    chan providerReturnSendEvent
	progress  chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	invalid   atomic.Bool

	observations atomic.Uint64
	publishing   atomic.Int64

	startedPacketCount   atomic.Int64
	startedByteCount     atomic.Int64
	startedEntryCount    atomic.Uint64
	completedPacketCount atomic.Int64
	completedByteCount   atomic.Int64
	failures             atomic.Int64
	failureByteCount     atomic.Int64
	retainedEntryCount   atomic.Int64

	beforeObserverPublishForTest atomic.Pointer[providerReturnObserverTestHook]
	beforeTerminalReleaseForTest atomic.Pointer[providerReturnObserverTestHook]
	beforePublisherWaitForTest   atomic.Pointer[providerReturnBoundaryTestHook]
}

// One immutable test hook can be replaced without racing live callbacks.
type providerReturnObserverTestHook struct {
	call func(clientconnect.RemoteUserNatProviderReturnSendObservation)
}

// A zero-argument test hook exposes entry into an upstream publisher join.
type providerReturnBoundaryTestHook struct {
	call func()
}

// An entry preserves every Started field and exposes one terminal publication
// that no later token or flow can substitute for.
type providerReturnSendEntry struct {
	token           uint64
	flowKey         clientconnect.RemoteUserNatProviderReturnFlowKey
	packetCount     int
	packetByteCount clientconnect.ByteCount
	inActiveWindow  bool
	state           atomic.Uint32
	sent            atomic.Bool
}

// A public window identifies one expected flow after a flushed start boundary.
type providerReturnFlowWindow struct {
	id      uint64
	flowKey clientconnect.RemoteUserNatProviderReturnFlowKey
}

// One immutable boundary retains exactly the tokens contributing to its
// packet and byte target.
type providerReturnFlowBoundary struct {
	window          providerReturnFlowWindow
	entries         []*providerReturnSendEntry
	packetCount     int64
	packetByteCount clientconnect.ByteCount
}

// An all-item source boundary joins return sends before their first SendPack.
type providerReturnLifecycleBoundary struct {
	entries      []*providerReturnSendEntry
	startedCount uint64
}

// The owner-only window accumulates matching Starts across monotonically
// increasing exact targets until the caller retires the whole flow window.
type providerReturnFlowState struct {
	window                  providerReturnFlowWindow
	entries                 []*providerReturnSendEntry
	packetCount             int64
	packetByteCount         clientconnect.ByteCount
	expectedPacketCount     int64
	expectedPacketByteCount clientconnect.ByteCount
	expectedSet             bool
	sealed                  bool
	invalid                 bool
}

// Request kinds serialize begin, boundary, and finish operations with observer
// events on the same FIFO.
type providerReturnRequestKind uint8

const (
	providerReturnRequestBegin providerReturnRequestKind = iota + 1
	providerReturnRequestBoundary
	providerReturnRequestFinish
	providerReturnRequestLifecycleBoundary
)

// A tracker request carries one response channel owned by its caller.
type providerReturnRequest struct {
	kind                    providerReturnRequestKind
	window                  providerReturnFlowWindow
	flowKey                 clientconnect.RemoteUserNatProviderReturnFlowKey
	expectedPacketCount     int64
	expectedPacketByteCount clientconnect.ByteCount
	response                chan providerReturnResponse
}

// A response distinguishes a valid under-target snapshot from an exact sealed
// boundary and from a structural or arithmetic mismatch.
type providerReturnResponse struct {
	window            providerReturnFlowWindow
	boundary          providerReturnFlowBoundary
	lifecycleBoundary providerReturnLifecycleBoundary
	exact             bool
	valid             bool
}

// Observer events and control requests share one owner queue.
type providerReturnSendEvent struct {
	observation clientconnect.RemoteUserNatProviderReturnSendObservation
	request     *providerReturnRequest
}

// Construction starts the single map and window owner.
func newProviderReturnSendTracker() *providerReturnSendTracker {
	ctx, cancel := context.WithCancel(context.Background())
	tracker := &providerReturnSendTracker{
		ctx:      ctx,
		cancel:   cancel,
		events:   make(chan providerReturnSendEvent, providerReturnSendEventCapacity),
		progress: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	go tracker.run()
	return tracker
}

// A coalesced notification wakes publication, target, and terminal waiters.
func (self *providerReturnSendTracker) notify() {
	select {
	case self.progress <- struct{}{}:
	default:
	}
}

// The provider callback performs only atomics, an optional test barrier, and
// one bounded nonblocking queue publication.
func (self *providerReturnSendTracker) observe(
	observation clientconnect.RemoteUserNatProviderReturnSendObservation,
) {
	self.publishing.Add(1)
	self.observations.Add(1)
	defer func() {
		self.publishing.Add(-1)
		self.notify()
	}()
	if hook := self.beforeObserverPublishForTest.Load(); hook != nil {
		hook.call(observation)
	}
	select {
	case <-self.ctx.Done():
		return
	default:
	}
	select {
	case <-self.ctx.Done():
		return
	case self.events <- providerReturnSendEvent{observation: observation}:
	default:
		self.invalid.Store(true)
		self.notify()
	}
}

// The single owner validates exact phase pairs and the active flow window.
func (self *providerReturnSendTracker) run() {
	defer close(self.done)
	entries := map[uint64]*providerReturnSendEntry{}
	var active *providerReturnFlowState
	var nextWindowId uint64
	cleanTerminalEntries := func() {
		for token, entry := range entries {
			if entry.state.Load() == providerReturnSendEntryTerminal {
				delete(entries, token)
				self.retainedEntryCount.Add(-1)
			}
		}
	}
	for {
		var event providerReturnSendEvent
		select {
		case <-self.ctx.Done():
			return
		case event = <-self.events:
		}
		if event.request != nil {
			response := self.handleRequest(
				event.request,
				entries,
				&active,
				&nextWindowId,
				cleanTerminalEntries,
			)
			event.request.response <- response
			continue
		}
		observation := event.observation
		switch observation.Phase {
		case clientconnect.RemoteUserNatProviderReturnSendPhaseStarted:
			if observation.Token == 0 || observation.PacketCount <= 0 ||
				observation.PacketByteCount <= 0 || observation.Sent {
				self.invalid.Store(true)
				break
			}
			if _, duplicate := entries[observation.Token]; duplicate {
				self.invalid.Store(true)
				break
			}
			entry := &providerReturnSendEntry{
				token:           observation.Token,
				flowKey:         observation.FlowKey,
				packetCount:     observation.PacketCount,
				packetByteCount: observation.PacketByteCount,
			}
			entries[entry.token] = entry
			self.retainedEntryCount.Add(1)
			self.startedEntryCount.Add(1)
			self.startedPacketCount.Add(int64(entry.packetCount))
			self.startedByteCount.Add(int64(entry.packetByteCount))
			if active != nil && observation.FlowKey == active.window.flowKey {
				entry.inActiveWindow = true
				active.entries = append(active.entries, entry)
				active.packetCount += int64(entry.packetCount)
				active.packetByteCount += entry.packetByteCount
				active.sealed = false
			}
		case clientconnect.RemoteUserNatProviderReturnSendPhaseCompleted:
			entry, ok := entries[observation.Token]
			if !ok || observation.Token == 0 ||
				observation.FlowKey != entry.flowKey ||
				observation.PacketCount != entry.packetCount ||
				observation.PacketByteCount != entry.packetByteCount ||
				!entry.state.CompareAndSwap(
					providerReturnSendEntryPending,
					providerReturnSendEntryCompleting,
				) {
				self.invalid.Store(true)
				break
			}
			entry.sent.Store(observation.Sent)
			self.completedPacketCount.Add(int64(entry.packetCount))
			self.completedByteCount.Add(int64(entry.packetByteCount))
			if !observation.Sent {
				self.failures.Add(int64(entry.packetCount))
				self.failureByteCount.Add(int64(entry.packetByteCount))
			}
			if hook := self.beforeTerminalReleaseForTest.Load(); hook != nil {
				hook.call(observation)
			}
			entry.state.Store(providerReturnSendEntryTerminal)
			if !entry.inActiveWindow {
				delete(entries, observation.Token)
				self.retainedEntryCount.Add(-1)
			}
		default:
			self.invalid.Store(true)
		}
		self.notify()
	}
}

// Request handling mutates owner-only active-window state.
func (self *providerReturnSendTracker) handleRequest(
	request *providerReturnRequest,
	entries map[uint64]*providerReturnSendEntry,
	active **providerReturnFlowState,
	nextWindowId *uint64,
	cleanTerminalEntries func(),
) providerReturnResponse {
	switch request.kind {
	case providerReturnRequestBegin:
		if *active != nil || !request.flowKey.Valid ||
			request.flowKey.DestinationId == (clientconnect.Id{}) {
			self.invalid.Store(true)
			return providerReturnResponse{}
		}
		cleanTerminalEntries()
		*nextWindowId += 1
		if *nextWindowId == 0 {
			*nextWindowId += 1
		}
		window := providerReturnFlowWindow{id: *nextWindowId, flowKey: request.flowKey}
		*active = &providerReturnFlowState{window: window}
		return providerReturnResponse{window: window, valid: true}
	case providerReturnRequestBoundary:
		state := *active
		if state == nil || state.window != request.window || state.invalid ||
			request.expectedPacketCount <= 0 || request.expectedPacketByteCount <= 0 {
			self.invalid.Store(true)
			return providerReturnResponse{}
		}
		if !state.expectedSet {
			state.expectedSet = true
			state.expectedPacketCount = request.expectedPacketCount
			state.expectedPacketByteCount = request.expectedPacketByteCount
		} else if state.expectedPacketCount != request.expectedPacketCount ||
			state.expectedPacketByteCount != request.expectedPacketByteCount {
			if request.expectedPacketCount <= state.expectedPacketCount ||
				request.expectedPacketByteCount <= state.expectedPacketByteCount {
				state.invalid = true
				self.invalid.Store(true)
				return providerReturnResponse{}
			}
			state.expectedPacketCount = request.expectedPacketCount
			state.expectedPacketByteCount = request.expectedPacketByteCount
		}
		if state.expectedPacketCount < state.packetCount ||
			state.expectedPacketByteCount < state.packetByteCount {
			state.invalid = true
			self.invalid.Store(true)
			return providerReturnResponse{}
		}
		exact := state.packetCount == state.expectedPacketCount &&
			state.packetByteCount == state.expectedPacketByteCount
		state.sealed = exact
		boundary := providerReturnFlowBoundary{
			window:          state.window,
			entries:         append([]*providerReturnSendEntry(nil), state.entries...),
			packetCount:     state.packetCount,
			packetByteCount: state.packetByteCount,
		}
		return providerReturnResponse{boundary: boundary, exact: exact, valid: true}
	case providerReturnRequestFinish:
		state := *active
		if state == nil || state.window != request.window || state.invalid ||
			!state.sealed || !state.expectedSet ||
			state.packetCount != state.expectedPacketCount ||
			state.packetByteCount != state.expectedPacketByteCount {
			self.invalid.Store(true)
			return providerReturnResponse{}
		}
		for _, entry := range state.entries {
			if entry.state.Load() != providerReturnSendEntryTerminal || !entry.sent.Load() {
				return providerReturnResponse{valid: true}
			}
		}
		*active = nil
		cleanTerminalEntries()
		return providerReturnResponse{exact: true, valid: true}
	case providerReturnRequestLifecycleBoundary:
		boundary := providerReturnLifecycleBoundary{
			entries:      make([]*providerReturnSendEntry, 0, len(entries)),
			startedCount: self.startedEntryCount.Load(),
		}
		for _, entry := range entries {
			boundary.entries = append(boundary.entries, entry)
		}
		return providerReturnResponse{lifecycleBoundary: boundary, valid: true}
	default:
		self.invalid.Store(true)
		return providerReturnResponse{}
	}
}

// The callback-entry generation closes the zero-publisher race before a
// window command is inserted into the owner FIFO.
func (self *providerReturnSendTracker) waitForPublishers(ctx context.Context) bool {
	for {
		generation := self.observations.Load()
		for self.publishing.Load() != 0 {
			if hook := self.beforePublisherWaitForTest.Load(); hook != nil {
				hook.call()
			}
			select {
			case <-ctx.Done():
				return false
			case <-self.ctx.Done():
				return false
			case <-self.progress:
			}
		}
		if self.observations.Load() == generation {
			return !self.invalid.Load()
		}
	}
}

// A source boundary flushes callbacks that entered before their first
// SendPack and captures every still-live return-send identity.
func (self *providerReturnSendTracker) lifecycleBoundary(
	ctx context.Context,
) (providerReturnLifecycleBoundary, bool) {
	response, ok := self.request(ctx, &providerReturnRequest{
		kind: providerReturnRequestLifecycleBoundary,
	})
	return response.lifecycleBoundary, ok
}

// Waiting joins terminal ownership but leaves a fully terminal setup failure
// before the new carrier baseline instead of poisoning future measurements.
func (self *providerReturnSendTracker) waitLifecycleThrough(
	ctx context.Context,
	boundary providerReturnLifecycleBoundary,
) bool {
	for {
		if self.invalid.Load() {
			return false
		}
		complete := true
		for _, entry := range boundary.entries {
			if entry.state.Load() != providerReturnSendEntryTerminal {
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
		case <-self.ctx.Done():
			return false
		case <-self.progress:
		}
	}
}

// A command follows every observer callback that entered before its flushed
// publication boundary.
func (self *providerReturnSendTracker) request(
	ctx context.Context,
	request *providerReturnRequest,
) (providerReturnResponse, bool) {
	if !self.waitForPublishers(ctx) {
		return providerReturnResponse{}, false
	}
	request.response = make(chan providerReturnResponse, 1)
	select {
	case <-ctx.Done():
		return providerReturnResponse{}, false
	case <-self.ctx.Done():
		return providerReturnResponse{}, false
	case <-self.done:
		return providerReturnResponse{}, false
	case self.events <- providerReturnSendEvent{request: request}:
	}
	select {
	case <-ctx.Done():
		return providerReturnResponse{}, false
	case <-self.ctx.Done():
		return providerReturnResponse{}, false
	case <-self.done:
		return providerReturnResponse{}, false
	case response := <-request.response:
		return response, response.valid && !self.invalid.Load()
	}
}

// A flow window begins only after prior callback publications are flushed.
func (self *providerReturnSendTracker) beginFlowWindow(
	ctx context.Context,
	flowKey clientconnect.RemoteUserNatProviderReturnFlowKey,
) (providerReturnFlowWindow, bool) {
	response, ok := self.request(ctx, &providerReturnRequest{
		kind:    providerReturnRequestBegin,
		flowKey: flowKey,
	})
	return response.window, ok
}

// Boundary waits for exact matching-flow packet and byte totals, then seals
// the window against late overshoot.
func (self *providerReturnSendTracker) flowBoundary(
	ctx context.Context,
	window providerReturnFlowWindow,
	expectedPacketCount int64,
	expectedPacketByteCount clientconnect.ByteCount,
) (providerReturnFlowBoundary, bool) {
	for {
		response, ok := self.request(ctx, &providerReturnRequest{
			kind:                    providerReturnRequestBoundary,
			window:                  window,
			expectedPacketCount:     expectedPacketCount,
			expectedPacketByteCount: expectedPacketByteCount,
		})
		if !ok {
			return providerReturnFlowBoundary{}, false
		}
		if response.exact {
			return response.boundary, true
		}
		select {
		case <-ctx.Done():
			return providerReturnFlowBoundary{}, false
		case <-self.ctx.Done():
			return providerReturnFlowBoundary{}, false
		case <-self.progress:
		}
	}
}

// Waiting checks the exact entries captured for the expected flow and target.
func (self *providerReturnSendTracker) waitThrough(
	ctx context.Context,
	boundary providerReturnFlowBoundary,
) bool {
	status := func() (bool, bool) {
		if self.invalid.Load() {
			return false, true
		}
		for _, entry := range boundary.entries {
			if entry.state.Load() != providerReturnSendEntryTerminal {
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
		if complete {
			return true
		}
		if failed {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-self.ctx.Done():
			return false
		case <-self.progress:
		}
	}
}

// Finishing performs a second publication fence, rejects overshoot, proves
// every exact entry succeeded, and retires the closed window.
func (self *providerReturnSendTracker) finishFlowWindow(
	ctx context.Context,
	window providerReturnFlowWindow,
) bool {
	for {
		response, ok := self.request(ctx, &providerReturnRequest{
			kind:   providerReturnRequestFinish,
			window: window,
		})
		if !ok {
			return false
		}
		if response.exact {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-self.ctx.Done():
			return false
		case <-self.progress:
		}
	}
}

// One convenience boundary covers the normal begin-write-wait workload path.
func (self *providerReturnSendTracker) waitForFlow(
	ctx context.Context,
	window providerReturnFlowWindow,
	expectedPacketCount int64,
	expectedPacketByteCount clientconnect.ByteCount,
) bool {
	boundary, ok := self.flowBoundary(
		ctx,
		window,
		expectedPacketCount,
		expectedPacketByteCount,
	)
	return ok && self.waitThrough(ctx, boundary) && self.finishFlowWindow(ctx, window)
}

// Test barriers are installed through atomic immutable holders.
func (self *providerReturnSendTracker) setBeforeObserverPublishForTest(
	callback func(clientconnect.RemoteUserNatProviderReturnSendObservation),
) {
	if callback == nil {
		self.beforeObserverPublishForTest.Store(nil)
		return
	}
	self.beforeObserverPublishForTest.Store(&providerReturnObserverTestHook{call: callback})
}

// A nil-by-default barrier proves an entered source callback is joined before
// the carrier baseline can be published.
func (self *providerReturnSendTracker) setBeforePublisherWaitForTest(callback func()) {
	if callback == nil {
		self.beforePublisherWaitForTest.Store(nil)
		return
	}
	self.beforePublisherWaitForTest.Store(&providerReturnBoundaryTestHook{call: callback})
}

// The terminal barrier runs after success/failure classification and before
// the entry release store visible to exact waiters.
func (self *providerReturnSendTracker) setBeforeTerminalReleaseForTest(
	callback func(clientconnect.RemoteUserNatProviderReturnSendObservation),
) {
	if callback == nil {
		self.beforeTerminalReleaseForTest.Store(nil)
		return
	}
	self.beforeTerminalReleaseForTest.Store(&providerReturnObserverTestHook{call: callback})
}

// Shutdown cancels the owner without closing the producer-facing queue.
func (self *providerReturnSendTracker) close() {
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
	})
}

// A compact valid key keeps tracker regressions independent from packet parsing.
func providerReturnTrackerTestFlow(seed byte) clientconnect.RemoteUserNatProviderReturnFlowKey {
	flowKey := clientconnect.RemoteUserNatProviderReturnFlowKey{
		DestinationId:   clientconnect.NewId(),
		Protocol:        clientconnect.IpProtocolUdp,
		SourcePort:      uint16(10_000 + int(seed)),
		DestinationPort: uint16(20_000 + int(seed)),
		IpVersion:       4,
		Valid:           true,
	}
	flowKey.SourceIp[0] = 10
	flowKey.SourceIp[3] = seed
	flowKey.DestinationIp[0] = 192
	flowKey.DestinationIp[1] = 0
	flowKey.DestinationIp[2] = 2
	flowKey.DestinationIp[3] = seed
	return flowKey
}

// One helper publishes a structurally exact Started observation.
func observeProviderReturnStarted(
	tracker *providerReturnSendTracker,
	token uint64,
	flowKey clientconnect.RemoteUserNatProviderReturnFlowKey,
	packetCount int,
	packetByteCount clientconnect.ByteCount,
) {
	tracker.observe(clientconnect.RemoteUserNatProviderReturnSendObservation{
		Phase:           clientconnect.RemoteUserNatProviderReturnSendPhaseStarted,
		Token:           token,
		FlowKey:         flowKey,
		PacketCount:     packetCount,
		PacketByteCount: packetByteCount,
	})
}

// One helper publishes a structurally exact Completed observation.
func observeProviderReturnCompleted(
	tracker *providerReturnSendTracker,
	token uint64,
	flowKey clientconnect.RemoteUserNatProviderReturnFlowKey,
	packetCount int,
	packetByteCount clientconnect.ByteCount,
	sent bool,
) {
	tracker.observe(clientconnect.RemoteUserNatProviderReturnSendObservation{
		Phase:           clientconnect.RemoteUserNatProviderReturnSendPhaseCompleted,
		Token:           token,
		FlowKey:         flowKey,
		PacketCount:     packetCount,
		PacketByteCount: packetByteCount,
		Sent:            sent,
	})
}

// An unrelated flow cannot substitute for the independently expected tuple.
func TestProviderReturnFlowWindowRejectsFlowSubstitution(t *testing.T) {
	for substitutionIndex := range 2 {
		tracker := newProviderReturnSendTracker()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		expectedFlow := providerReturnTrackerTestFlow(byte(1 + substitutionIndex))
		unrelatedFlow := expectedFlow
		if substitutionIndex == 0 {
			unrelatedFlow.SourcePort += 1
		} else {
			unrelatedFlow.DestinationId = clientconnect.NewId()
		}
		window, ok := tracker.beginFlowWindow(ctx, expectedFlow)
		if !ok {
			t.Fatalf("substitution=%d begin expected flow: %v", substitutionIndex, ctx.Err())
		}
		observeProviderReturnStarted(tracker, 1, unrelatedFlow, 1, 100)
		observeProviderReturnCompleted(tracker, 1, unrelatedFlow, 1, 100, true)
		canceledCtx, canceled := context.WithCancel(context.Background())
		canceled()
		if _, ok := tracker.flowBoundary(canceledCtx, window, 1, 100); ok {
			t.Errorf("substitution=%d unrelated flow satisfied the target", substitutionIndex)
		}
		observeProviderReturnStarted(tracker, 2, expectedFlow, 1, 100)
		boundary, ok := tracker.flowBoundary(ctx, window, 1, 100)
		if !ok || len(boundary.entries) != 1 || boundary.entries[0].token != 2 {
			t.Errorf(
				"substitution=%d expected boundary=%+v ok=%t err=%v",
				substitutionIndex,
				boundary,
				ok,
				ctx.Err(),
			)
		} else {
			observeProviderReturnCompleted(tracker, 2, expectedFlow, 1, 100, true)
			if !tracker.waitThrough(ctx, boundary) || !tracker.finishFlowWindow(ctx, window) {
				t.Errorf("substitution=%d finish expected flow: %v", substitutionIndex, ctx.Err())
			}
		}
		cancel()
		tracker.close()
	}
}

// One Started/Completed pair may describe a batch, and both exact packet and
// byte totals must pass unchanged through the window.
func TestProviderReturnFlowWindowAcceptsExactMultiPacketBatch(t *testing.T) {
	tracker := newProviderReturnSendTracker()
	defer tracker.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flowKey := providerReturnTrackerTestFlow(3)
	window, ok := tracker.beginFlowWindow(ctx, flowKey)
	if !ok {
		t.Fatal("begin batch window")
	}
	observeProviderReturnStarted(tracker, 1, flowKey, 3, 384)
	boundary, ok := tracker.flowBoundary(ctx, window, 3, 384)
	if !ok || len(boundary.entries) != 1 || boundary.entries[0].packetCount != 3 {
		t.Fatalf("batch boundary=%+v ok=%t", boundary, ok)
	}
	observeProviderReturnCompleted(tracker, 1, flowKey, 3, 384, true)
	if !tracker.waitThrough(ctx, boundary) || !tracker.finishFlowWindow(ctx, window) {
		t.Fatalf("finish batch window: %v", ctx.Err())
	}
}

// One window remains active while exact cumulative targets advance, including
// when the next Started item arrives before its larger target is requested.
func TestProviderReturnFlowWindowAdvancesCumulativeTargetWithoutGap(t *testing.T) {
	tracker := newProviderReturnSendTracker()
	defer tracker.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flowKey := providerReturnTrackerTestFlow(4)
	window, ok := tracker.beginFlowWindow(ctx, flowKey)
	if !ok {
		t.Fatal("begin cumulative window")
	}
	observeProviderReturnStarted(tracker, 1, flowKey, 2, 200)
	observeProviderReturnCompleted(tracker, 1, flowKey, 2, 200, true)
	firstBoundary, ok := tracker.flowBoundary(ctx, window, 2, 200)
	if !ok || !tracker.waitThrough(ctx, firstBoundary) {
		t.Fatalf("first cumulative boundary=%+v ok=%t", firstBoundary, ok)
	}
	observeProviderReturnStarted(tracker, 2, flowKey, 1, 100)
	observeProviderReturnCompleted(tracker, 2, flowKey, 1, 100, true)
	secondBoundary, ok := tracker.flowBoundary(ctx, window, 3, 300)
	if !ok || len(secondBoundary.entries) != 2 || !tracker.waitThrough(ctx, secondBoundary) {
		t.Fatalf("second cumulative boundary=%+v ok=%t", secondBoundary, ok)
	}
	if !tracker.finishFlowWindow(ctx, window) {
		t.Fatalf("finish cumulative window: %v", ctx.Err())
	}
}

// Completed traffic outside the active measured tuple is discarded at once,
// so long warmed downloads retain only their exact matching-flow entries.
func TestProviderReturnFlowWindowBoundsUnrelatedTerminalRetention(t *testing.T) {
	tracker := newProviderReturnSendTracker()
	defer tracker.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	targetFlow := providerReturnTrackerTestFlow(43)
	window, ok := tracker.beginFlowWindow(ctx, targetFlow)
	if !ok {
		t.Fatal("begin bounded-retention window")
	}
	const unrelatedEntryCount = 4096
	for entryIndex := 0; entryIndex < unrelatedEntryCount; entryIndex += 1 {
		flowKey := providerReturnTrackerTestFlow(byte(44 + entryIndex%100))
		token := uint64(entryIndex + 1)
		observeProviderReturnStarted(tracker, token, flowKey, 1, 100)
		observeProviderReturnCompleted(tracker, token, flowKey, 1, 100, true)
	}
	targetToken := uint64(unrelatedEntryCount + 1)
	observeProviderReturnStarted(tracker, targetToken, targetFlow, 1, 100)
	boundary, ok := tracker.flowBoundary(ctx, window, 1, 100)
	if !ok || len(boundary.entries) != 1 || boundary.entries[0].token != targetToken {
		t.Fatalf("bounded-retention target boundary=%+v ok=%t", boundary, ok)
	}
	if retainedEntryCount := tracker.retainedEntryCount.Load(); retainedEntryCount != 1 {
		t.Fatalf("retained provider-return entries=%d, want only target entry", retainedEntryCount)
	}
	observeProviderReturnCompleted(tracker, targetToken, targetFlow, 1, 100, true)
	if !tracker.waitThrough(ctx, boundary) || !tracker.finishFlowWindow(ctx, window) {
		t.Fatalf("finish bounded-retention window: %v", ctx.Err())
	}
	if retainedEntryCount := tracker.retainedEntryCount.Load(); retainedEntryCount != 0 {
		t.Fatalf("retained provider-return entries after finish=%d", retainedEntryCount)
	}
}

// Zero, duplicate, and unpaired tokens are structural observer failures and
// cannot leave a seemingly valid exact flow window behind.
func TestProviderReturnFlowWindowRejectsInvalidTokenLifecycle(t *testing.T) {
	for lifecycleIndex := range 4 {
		tracker := newProviderReturnSendTracker()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		flowKey := providerReturnTrackerTestFlow(byte(10 + lifecycleIndex))
		window, ok := tracker.beginFlowWindow(ctx, flowKey)
		if !ok {
			t.Fatalf("lifecycle=%d begin window", lifecycleIndex)
		}
		switch lifecycleIndex {
		case 0:
			observeProviderReturnStarted(tracker, 0, flowKey, 1, 100)
		case 1:
			observeProviderReturnStarted(tracker, 1, flowKey, 1, 100)
			observeProviderReturnStarted(tracker, 1, flowKey, 1, 100)
		case 2:
			observeProviderReturnCompleted(tracker, 1, flowKey, 1, 100, true)
		case 3:
			observeProviderReturnStarted(tracker, 1, flowKey, 1, 100)
			observeProviderReturnCompleted(tracker, 1, flowKey, 1, 100, true)
			observeProviderReturnCompleted(tracker, 1, flowKey, 1, 100, true)
		}
		if _, ok := tracker.flowBoundary(ctx, window, 1, 100); ok || !tracker.invalid.Load() {
			t.Errorf("lifecycle=%d invalid token sequence passed", lifecycleIndex)
		}
		cancel()
		tracker.close()
	}
}

// The workload expectation uses the independently known UDP4 wire overhead,
// which prevents a payload-only byte target from passing the observer gate.
func TestFullTunUdp4ProviderReturnPacketByteCountIncludesHeaders(t *testing.T) {
	payloadByteCount := 1_200
	packetByteCount := fullTunUdp4ProviderReturnPacketByteCount(payloadByteCount)
	if packetByteCount != 1_228 || packetByteCount == clientconnect.ByteCount(payloadByteCount) {
		t.Fatalf("UDP4 packet bytes=%d, want 1228 including headers", packetByteCount)
	}
}

// Socket-derived expectations preserve the independent peer ID, source and
// destination tuple, protocol, ports, and normalized IP version.
func TestFullTunProviderReturnUdpFlowKeyUsesSocketTuple(t *testing.T) {
	destinationId := clientconnect.NewId()
	flowKey, err := fullTunProviderReturnUdpFlowKey(
		destinationId,
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 44_443},
		&net.UDPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 31_337},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !flowKey.Valid || flowKey.DestinationId != destinationId ||
		flowKey.Protocol != clientconnect.IpProtocolUdp || flowKey.IpVersion != 4 ||
		flowKey.SourcePort != 44_443 || flowKey.DestinationPort != 31_337 ||
		flowKey.SourceIp != [16]byte{127, 0, 0, 1} ||
		flowKey.DestinationIp != [16]byte{10, 0, 0, 7} {
		t.Fatalf("socket-derived flow=%+v", flowKey)
	}
	if _, err := fullTunProviderReturnUdpFlowKey(
		destinationId,
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		&net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 2},
	); err == nil {
		t.Fatal("mixed IP families produced a valid provider-return flow")
	}
}

// Exact target arithmetic rejects packet or byte overshoot instead of clipping
// the window to a plausible aggregate count.
func TestProviderReturnFlowWindowRejectsOvershoot(t *testing.T) {
	tracker := newProviderReturnSendTracker()
	defer tracker.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flowKey := providerReturnTrackerTestFlow(3)
	window, ok := tracker.beginFlowWindow(ctx, flowKey)
	if !ok {
		t.Fatal("begin overshoot window")
	}
	observeProviderReturnStarted(tracker, 1, flowKey, 2, 200)
	if _, ok := tracker.flowBoundary(ctx, window, 1, 100); ok || !tracker.invalid.Load() {
		t.Fatal("packet and byte overshoot was accepted")
	}
}

// Completion must repeat the exact token, flow, packet count, and byte count
// captured at Started.
func TestProviderReturnFlowWindowRejectsCompletionMismatch(t *testing.T) {
	for mismatchIndex := range 3 {
		tracker := newProviderReturnSendTracker()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		flowKey := providerReturnTrackerTestFlow(byte(4 + mismatchIndex))
		window, ok := tracker.beginFlowWindow(ctx, flowKey)
		if !ok {
			t.Fatalf("mismatch=%d begin window", mismatchIndex)
		}
		observeProviderReturnStarted(tracker, 1, flowKey, 1, 100)
		boundary, ok := tracker.flowBoundary(ctx, window, 1, 100)
		if !ok {
			t.Fatalf("mismatch=%d boundary", mismatchIndex)
		}
		completedFlow := flowKey
		completedPacketCount := 1
		completedByteCount := clientconnect.ByteCount(100)
		switch mismatchIndex {
		case 0:
			completedFlow = providerReturnTrackerTestFlow(9)
		case 1:
			completedPacketCount = 2
		case 2:
			completedByteCount = 101
		}
		observeProviderReturnCompleted(
			tracker,
			1,
			completedFlow,
			completedPacketCount,
			completedByteCount,
			true,
		)
		if tracker.waitThrough(ctx, boundary) || !tracker.invalid.Load() {
			t.Errorf("mismatch=%d completion passed", mismatchIndex)
		}
		cancel()
		tracker.close()
	}
}

// A terminal send failure rejects its exact entry immediately instead of
// waiting for an unrelated progress notification or the context guard.
func TestProviderReturnFlowWindowRejectsFailedCompletion(t *testing.T) {
	tracker := newProviderReturnSendTracker()
	defer tracker.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flowKey := providerReturnTrackerTestFlow(6)
	window, ok := tracker.beginFlowWindow(ctx, flowKey)
	if !ok {
		t.Fatal("begin failed-completion window")
	}
	observeProviderReturnStarted(tracker, 1, flowKey, 1, 100)
	boundary, ok := tracker.flowBoundary(ctx, window, 1, 100)
	if !ok {
		t.Fatal("capture failed-completion boundary")
	}
	observeProviderReturnCompleted(tracker, 1, flowKey, 1, 100, false)
	if tracker.waitThrough(ctx, boundary) {
		t.Fatal("failed terminal completion passed")
	}
}

// A later token on the same flow cannot complete an earlier captured entry.
func TestProviderReturnFlowWindowRejectsTokenSubstitution(t *testing.T) {
	tracker := newProviderReturnSendTracker()
	defer tracker.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flowKey := providerReturnTrackerTestFlow(7)
	window, ok := tracker.beginFlowWindow(ctx, flowKey)
	if !ok {
		t.Fatal("begin token window")
	}
	observeProviderReturnStarted(tracker, 1, flowKey, 1, 100)
	boundary, ok := tracker.flowBoundary(ctx, window, 1, 100)
	if !ok {
		t.Fatal("capture token boundary")
	}
	otherFlow := providerReturnTrackerTestFlow(8)
	observeProviderReturnStarted(tracker, 2, otherFlow, 1, 100)
	observeProviderReturnCompleted(tracker, 2, otherFlow, 1, 100, true)
	canceledCtx, canceled := context.WithCancel(context.Background())
	canceled()
	if tracker.waitThrough(canceledCtx, boundary) {
		t.Fatal("later token satisfied an earlier exact entry")
	}
	observeProviderReturnCompleted(tracker, 1, flowKey, 1, 100, true)
	if !tracker.waitThrough(ctx, boundary) || !tracker.finishFlowWindow(ctx, window) {
		t.Fatalf("exact token did not finish: %v", ctx.Err())
	}
}

// A callback held before its queue publication is included in the target, and
// a completion held before terminal release cannot satisfy the boundary.
func TestProviderReturnFlowWindowJoinsPublicationBarriers(t *testing.T) {
	tracker := newProviderReturnSendTracker()
	defer tracker.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flowKey := providerReturnTrackerTestFlow(10)
	window, ok := tracker.beginFlowWindow(ctx, flowKey)
	if !ok {
		t.Fatal("begin publication window")
	}
	publishEntered := make(chan struct{})
	releasePublish := make(chan struct{})
	var publishOnce sync.Once
	tracker.setBeforeObserverPublishForTest(func(
		observation clientconnect.RemoteUserNatProviderReturnSendObservation,
	) {
		if observation.Phase == clientconnect.RemoteUserNatProviderReturnSendPhaseStarted {
			publishOnce.Do(func() {
				close(publishEntered)
				<-releasePublish
			})
		}
	})
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		observeProviderReturnStarted(tracker, 1, flowKey, 1, 100)
	}()
	<-publishEntered
	canceledCtx, canceled := context.WithCancel(context.Background())
	canceled()
	if boundary, boundaryOk := tracker.flowBoundary(canceledCtx, window, 1, 100); boundaryOk {
		t.Fatalf("boundary passed held Started publication: %+v", boundary)
	}
	close(releasePublish)
	<-observerDone
	boundary, boundaryOk := tracker.flowBoundary(ctx, window, 1, 100)
	if !boundaryOk || len(boundary.entries) != 1 {
		t.Fatalf("published boundary=%+v ok=%t", boundary, boundaryOk)
	}
	terminalEntered := make(chan struct{})
	releaseTerminal := make(chan struct{})
	tracker.setBeforeTerminalReleaseForTest(func(
		clientconnect.RemoteUserNatProviderReturnSendObservation,
	) {
		close(terminalEntered)
		<-releaseTerminal
	})
	observeProviderReturnCompleted(tracker, 1, flowKey, 1, 100, true)
	<-terminalEntered
	canceledCtx, canceled = context.WithCancel(context.Background())
	canceled()
	if tracker.waitThrough(canceledCtx, boundary) {
		t.Fatal("boundary passed held terminal publication")
	}
	close(releaseTerminal)
	if !tracker.waitThrough(ctx, boundary) ||
		!tracker.finishFlowWindow(ctx, window) {
		t.Fatalf("terminal publication did not finish: %v", ctx.Err())
	}
}

// Formatting one exact boundary aids failure diagnostics without exposing
// internal entry pointers to workload code.
func (self providerReturnFlowBoundary) String() string {
	return fmt.Sprintf(
		"flow=%+v packets=%d bytes=%d tokens=%d",
		self.window.flowKey,
		self.packetCount,
		self.packetByteCount,
		len(self.entries),
	)
}
