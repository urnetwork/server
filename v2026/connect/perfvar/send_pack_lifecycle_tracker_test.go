// This file supplies PERFVAR's exact Pack producer boundaries. It observes
// every original Pack for diagnostics, while workload boundaries join only IP
// data Packs; independent signaling and maintenance remain reliable without
// becoming prerequisites for an application measurement boundary.
package perfvar

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

const sendPackLifecycleEventCapacity = 64 * 1024
const sendPackLifecycleFailureSampleCapacity = 16

// The completing state keeps terminal failure classification ordered before
// the release store observed by exact boundaries.
const sendPackLifecycleEntryPhaseCompleting = uint32(4)

// sendPackLifecycleTracker observes every original non-loopback Pack, including
// ACK-required sends that the older NoAck first-write seam omits. Workload
// counters and boundaries select only the two IP data directions.
type sendPackLifecycleTracker struct {
	ctx       context.Context
	cancel    context.CancelFunc
	events    chan sendPackLifecycleEvent
	progress  chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	invalid   atomic.Bool
	// Terminal failure counts are monotonic measurement watermarks. Exact
	// ownership joins accept a failed terminal; measured intervals compare
	// their start and end watermarks separately.
	failures                    atomic.Uint64
	recoverableFailures         atomic.Uint64
	started                     atomic.Uint64
	publishing                  atomic.Int64
	workloadFailures            atomic.Uint64
	workloadRecoverableFailures atomic.Uint64
	workloadDatagramFailures    atomic.Uint64
	workloadStarted             atomic.Uint64
	workloadPublishing          atomic.Int64
	nextClient                  atomic.Uint64
	failureLock                 sync.Mutex
	failureSamples              []clientconnect.SendPackLifecycleObservation
	workloadFailureSamples      []clientconnect.SendPackLifecycleObservation

	// Nil test barriers expose publication races without delaying production
	// measurement callbacks.
	beforeObserverPublishForTest         atomic.Pointer[sendPackLifecycleObserverTestHook]
	beforeInstanceObserverPublishForTest atomic.Pointer[sendPackLifecycleInstanceObserverTestHook]
	beforeTerminalReleaseForTest         atomic.Pointer[sendPackLifecycleTerminalTestHook]
	beforePublisherWaitForTest           atomic.Pointer[sendPackLifecycleBoundaryTestHook]
	afterBoundaryEnqueueForTest          atomic.Pointer[sendPackLifecycleBoundaryEnqueueTestHook]
	beforeEntryWaitForTest               atomic.Pointer[sendPackLifecycleEntryWaitTestHook]
}

// sendPackLifecycleObserverTestHook makes callback replacement race-free while
// live clients continue publishing unrelated control traffic.
type sendPackLifecycleObserverTestHook struct {
	call func(clientconnect.SendPackLifecycleObservation)
}

// An instance-aware hook exposes the private token namespace to tests that
// must retain one Pack across a generated Client replacement.
type sendPackLifecycleInstanceObserverTestHook struct {
	call func(uint64, clientconnect.SendPackLifecycleObservation)
}

// The owner-side terminal hook carries the private Client-instance namespace
// without blocking the Connect observer that produced the event.
type sendPackLifecycleTerminalTestHook struct {
	call func(*sendPackLifecycleEntry, clientconnect.SendPackLifecycleObservation)
}

// A zero-argument test hook exposes entry into a publisher join without
// changing the observation callback surface.
type sendPackLifecycleBoundaryTestHook struct {
	call func()
}

// One scoped boundary hook distinguishes application ownership joins from
// diagnostic snapshots while the owner is held on an exact terminal.
type sendPackLifecycleBoundaryEnqueueTestHook struct {
	call func(sendPackLifecycleBoundaryScope)
}

// An exact-entry hook distinguishes a boundary blocked on an already-captured
// Pack from one blocked on its observer callback publication.
type sendPackLifecycleEntryWaitTestHook struct {
	call func(*sendPackLifecycleEntry)
}

// sendPackLifecycleKey namespaces per-Client tokens by observer registration.
type sendPackLifecycleKey struct {
	clientInstance uint64
	token          uint64
}

// sendPackLifecycleEntry retains immutable identity and terminal publication.
type sendPackLifecycleEntry struct {
	key                 sendPackLifecycleKey
	clientId            clientconnect.Id
	destinationId       clientconnect.Id
	ackRequired         bool
	messageType         protocol.MessageType
	upstreamRecoverable bool
	phase               atomic.Uint32
}

// sendPackLifecycleBoundary captures exact unfinished identities plus the
// number of Started callbacks that had entered publication.
type sendPackLifecycleBoundary struct {
	entries         []*sendPackLifecycleEntry
	startedCount    uint64
	failedAtCapture uint64
}

// sendPackLifecycleBoundaryScope selects either the complete diagnostic stream
// or only traffic causally owned by a PERFVAR application workload.
type sendPackLifecycleBoundaryScope uint8

const (
	sendPackLifecycleBoundaryScopeAll sendPackLifecycleBoundaryScope = iota
	sendPackLifecycleBoundaryScopeWorkload
)

// sendPackLifecycleEvent serializes observations and boundary commands on one
// owner goroutine.
type sendPackLifecycleEvent struct {
	clientInstance uint64
	observation    clientconnect.SendPackLifecycleObservation
	boundary       chan sendPackLifecycleBoundary
	boundaryScope  sendPackLifecycleBoundaryScope
}

// PERFVAR workloads enter transfer through these two TUN data directions.
// Signaling, contract maintenance, and health probes are independent traffic:
// they remain visible to diagnostics but cannot own an application boundary.
func sendPackLifecycleWorkloadMessageType(messageType protocol.MessageType) bool {
	switch messageType {
	case protocol.MessageType_IpIpPacketToProvider,
		protocol.MessageType_IpIpPacketFromProvider:
		return true
	default:
		return false
	}
}

// newSendPackLifecycleTracker starts the single state owner.
func newSendPackLifecycleTracker() *sendPackLifecycleTracker {
	ctx, cancel := context.WithCancel(context.Background())
	tracker := &sendPackLifecycleTracker{
		ctx:      ctx,
		cancel:   cancel,
		events:   make(chan sendPackLifecycleEvent, sendPackLifecycleEventCapacity),
		progress: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	go tracker.run()
	return tracker
}

// notify wakes boundary and terminal waiters without blocking a send callback.
func (self *sendPackLifecycleTracker) notify() {
	select {
	case self.progress <- struct{}{}:
	default:
	}
}

// newObserver supplies a fresh namespace because tokens restart when the API
// generator rebuilds a Client with the same logical ClientId.
func (self *sendPackLifecycleTracker) newObserver() func(clientconnect.SendPackLifecycleObservation) {
	clientInstance := self.nextClient.Add(1)
	return func(observation clientconnect.SendPackLifecycleObservation) {
		workload := sendPackLifecycleWorkloadMessageType(observation.MessageType)
		self.publishing.Add(1)
		if workload {
			self.workloadPublishing.Add(1)
		}
		defer func() {
			if workload {
				self.workloadPublishing.Add(-1)
			}
			self.publishing.Add(-1)
			self.notify()
		}()
		if observation.Phase == clientconnect.SendPackLifecyclePhaseStarted {
			self.started.Add(1)
			if workload {
				self.workloadStarted.Add(1)
			}
		}
		if hook := self.beforeObserverPublishForTest.Load(); hook != nil {
			hook.call(observation)
		}
		if hook := self.beforeInstanceObserverPublishForTest.Load(); hook != nil {
			hook.call(clientInstance, observation)
		}
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		select {
		case <-self.ctx.Done():
			return
		case self.events <- sendPackLifecycleEvent{
			clientInstance: clientInstance,
			observation:    observation,
		}:
		default:
			self.invalid.Store(true)
			self.notify()
		}
	}
}

// setBeforeObserverPublishForTest installs a race-safe nil-by-default barrier.
func (self *sendPackLifecycleTracker) setBeforeObserverPublishForTest(
	callback func(clientconnect.SendPackLifecycleObservation),
) {
	if callback == nil {
		self.beforeObserverPublishForTest.Store(nil)
		return
	}
	self.beforeObserverPublishForTest.Store(&sendPackLifecycleObserverTestHook{call: callback})
}

// An instance-aware barrier distinguishes token reuse by rebuilt Clients.
func (self *sendPackLifecycleTracker) setBeforeInstanceObserverPublishForTest(
	callback func(uint64, clientconnect.SendPackLifecycleObservation),
) {
	if callback == nil {
		self.beforeInstanceObserverPublishForTest.Store(nil)
		return
	}
	self.beforeInstanceObserverPublishForTest.Store(
		&sendPackLifecycleInstanceObserverTestHook{call: callback},
	)
}

// A nil-by-default barrier proves a boundary observed an entered publisher
// before a test cancels or releases that exact generation.
func (self *sendPackLifecycleTracker) setBeforePublisherWaitForTest(callback func()) {
	if callback == nil {
		self.beforePublisherWaitForTest.Store(nil)
		return
	}
	self.beforePublisherWaitForTest.Store(&sendPackLifecycleBoundaryTestHook{call: callback})
}

// A nil-by-default hook exposes a scoped boundary request after it joins
// callback entry and enters the owner queue, but before the owner publishes a
// snapshot.
func (self *sendPackLifecycleTracker) setAfterBoundaryEnqueueForTest(
	callback func(sendPackLifecycleBoundaryScope),
) {
	if callback == nil {
		self.afterBoundaryEnqueueForTest.Store(nil)
		return
	}
	self.afterBoundaryEnqueueForTest.Store(
		&sendPackLifecycleBoundaryEnqueueTestHook{call: callback},
	)
}

// A nil-by-default barrier exposes an exact entry after boundary capture and
// before waitThrough sleeps for its terminal publication.
func (self *sendPackLifecycleTracker) setBeforeEntryWaitForTest(
	callback func(*sendPackLifecycleEntry),
) {
	if callback == nil {
		self.beforeEntryWaitForTest.Store(nil)
		return
	}
	self.beforeEntryWaitForTest.Store(&sendPackLifecycleEntryWaitTestHook{call: callback})
}

// run validates exact phase order and owns the live identity map.
func (self *sendPackLifecycleTracker) run() {
	defer close(self.done)
	entries := map[sendPackLifecycleKey]*sendPackLifecycleEntry{}
	for {
		var event sendPackLifecycleEvent
		select {
		case <-self.ctx.Done():
			return
		case event = <-self.events:
		}
		if event.boundary != nil {
			boundary := sendPackLifecycleBoundary{
				startedCount:    self.started.Load(),
				failedAtCapture: self.failures.Load(),
			}
			if event.boundaryScope == sendPackLifecycleBoundaryScopeWorkload {
				boundary.startedCount = self.workloadStarted.Load()
				boundary.failedAtCapture = self.workloadFailures.Load()
			}
			for key, entry := range entries {
				if entry.phase.Load() == uint32(clientconnect.SendPackLifecyclePhaseTerminal) {
					delete(entries, key)
				} else if event.boundaryScope == sendPackLifecycleBoundaryScopeAll ||
					sendPackLifecycleWorkloadMessageType(entry.messageType) {
					boundary.entries = append(boundary.entries, entry)
				}
			}
			event.boundary <- boundary
			continue
		}

		observation := event.observation
		key := sendPackLifecycleKey{
			clientInstance: event.clientInstance,
			token:          observation.Token,
		}
		switch observation.Phase {
		case clientconnect.SendPackLifecyclePhaseStarted:
			if _, duplicate := entries[key]; duplicate || observation.Err != nil {
				self.invalid.Store(true)
				break
			}
			entries[key] = &sendPackLifecycleEntry{
				key:                 key,
				clientId:            observation.ClientId,
				destinationId:       observation.DestinationId,
				ackRequired:         observation.AckRequired,
				messageType:         observation.MessageType,
				upstreamRecoverable: observation.UpstreamRecoverable,
				phase:               atomic.Uint32{},
			}
		case clientconnect.SendPackLifecyclePhaseFirstRouteWrite:
			entry, ok := entries[key]
			if !ok || !sameSendPackLifecycleIdentity(entry, observation) ||
				!entry.phase.CompareAndSwap(0, uint32(clientconnect.SendPackLifecyclePhaseFirstRouteWrite)) {
				self.invalid.Store(true)
				break
			}
		case clientconnect.SendPackLifecyclePhaseTerminal:
			entry, ok := entries[key]
			if !ok || !sameSendPackLifecycleIdentity(entry, observation) ||
				!entry.phase.CompareAndSwap(
					uint32(clientconnect.SendPackLifecyclePhaseFirstRouteWrite),
					sendPackLifecycleEntryPhaseCompleting,
				) {
				self.invalid.Store(true)
				break
			}
			if observation.Err != nil {
				recoverableAttempt := observation.UpstreamRecoverable &&
					errors.Is(observation.Err, clientconnect.ErrSendPackNotAdmitted)
				self.failures.Add(1)
				if recoverableAttempt {
					self.recoverableFailures.Add(1)
				}
				self.failureLock.Lock()
				if len(self.failureSamples) < sendPackLifecycleFailureSampleCapacity {
					self.failureSamples = append(self.failureSamples, observation)
				}
				if sendPackLifecycleWorkloadMessageType(observation.MessageType) {
					self.workloadFailures.Add(1)
					if recoverableAttempt {
						self.workloadRecoverableFailures.Add(1)
					}
					if !observation.AckRequired &&
						observation.MessageType == protocol.MessageType_IpIpPacketFromProvider {
						self.workloadDatagramFailures.Add(1)
					}
					if len(self.workloadFailureSamples) < sendPackLifecycleFailureSampleCapacity {
						self.workloadFailureSamples = append(
							self.workloadFailureSamples,
							observation,
						)
					}
				}
				self.failureLock.Unlock()
			}
			if hook := self.beforeTerminalReleaseForTest.Load(); hook != nil {
				hook.call(entry, observation)
			}
			entry.phase.Store(uint32(clientconnect.SendPackLifecyclePhaseTerminal))
		default:
			self.invalid.Store(true)
		}
		self.notify()
	}
}

// failureSnapshot returns bounded immutable diagnostic samples without
// putting formatting work on the observer callback or state-owner goroutine.
func (self *sendPackLifecycleTracker) failureSnapshot() []clientconnect.SendPackLifecycleObservation {
	self.failureLock.Lock()
	defer self.failureLock.Unlock()
	return append([]clientconnect.SendPackLifecycleObservation(nil), self.failureSamples...)
}

// workloadFailureSnapshot excludes independent control-plane failures from a
// measured application's terminal-failure epoch.
func (self *sendPackLifecycleTracker) workloadFailureSnapshot() []clientconnect.SendPackLifecycleObservation {
	self.failureLock.Lock()
	defer self.failureLock.Unlock()
	return append(
		[]clientconnect.SendPackLifecycleObservation(nil),
		self.workloadFailureSamples...,
	)
}

// setBeforeTerminalReleaseForTest installs an owner-side barrier after the
// exact terminal event is consumed (and any error is classified) but before
// the release store observed by waiters.
func (self *sendPackLifecycleTracker) setBeforeTerminalReleaseForTest(
	callback func(*sendPackLifecycleEntry, clientconnect.SendPackLifecycleObservation),
) {
	if callback == nil {
		self.beforeTerminalReleaseForTest.Store(nil)
		return
	}
	self.beforeTerminalReleaseForTest.Store(&sendPackLifecycleTerminalTestHook{call: callback})
}

// sameSendPackLifecycleIdentity rejects phase pairs that reuse a token for a
// different logical Pack.
func sameSendPackLifecycleIdentity(
	entry *sendPackLifecycleEntry,
	observation clientconnect.SendPackLifecycleObservation,
) bool {
	return entry.clientId == observation.ClientId &&
		entry.destinationId == observation.DestinationId &&
		entry.ackRequired == observation.AckRequired &&
		entry.messageType == observation.MessageType &&
		entry.upstreamRecoverable == observation.UpstreamRecoverable
}

// waitForPublishers joins callbacks that entered before a boundary request.
func (self *sendPackLifecycleTracker) waitForPublishers(
	ctx context.Context,
	scope sendPackLifecycleBoundaryScope,
) bool {
	publishing := &self.publishing
	if scope == sendPackLifecycleBoundaryScopeWorkload {
		publishing = &self.workloadPublishing
	}
	for publishing.Load() != 0 {
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
	return true
}

// boundary flushes prior callback publications and captures exact unfinished
// Pack identities. A generation retry closes the callback-entry race around
// the zero-publisher observation.
func (self *sendPackLifecycleTracker) captureBoundary(
	ctx context.Context,
	scope sendPackLifecycleBoundaryScope,
) (sendPackLifecycleBoundary, bool) {
	started := &self.started
	publishing := &self.publishing
	if scope == sendPackLifecycleBoundaryScopeWorkload {
		started = &self.workloadStarted
		publishing = &self.workloadPublishing
	}
	for {
		startedBefore := started.Load()
		if !self.waitForPublishers(ctx, scope) {
			return sendPackLifecycleBoundary{}, false
		}
		response := make(chan sendPackLifecycleBoundary, 1)
		select {
		case <-ctx.Done():
			return sendPackLifecycleBoundary{}, false
		case <-self.ctx.Done():
			return sendPackLifecycleBoundary{}, false
		case self.events <- sendPackLifecycleEvent{
			boundary:      response,
			boundaryScope: scope,
		}:
		}
		if hook := self.afterBoundaryEnqueueForTest.Load(); hook != nil {
			hook.call(scope)
		}
		var boundary sendPackLifecycleBoundary
		select {
		case <-ctx.Done():
			return sendPackLifecycleBoundary{}, false
		case <-self.ctx.Done():
			return sendPackLifecycleBoundary{}, false
		case boundary = <-response:
		}
		if publishing.Load() == 0 &&
			startedBefore == started.Load() &&
			boundary.startedCount == started.Load() {
			return boundary, !self.invalid.Load()
		}
	}
}

// boundary captures every Pack for diagnostics and tracker unit tests.
func (self *sendPackLifecycleTracker) boundary(
	ctx context.Context,
) (sendPackLifecycleBoundary, bool) {
	return self.captureBoundary(ctx, sendPackLifecycleBoundaryScopeAll)
}

// workloadBoundary captures only application IP Packs. Reliable signaling may
// legitimately remain unacknowledged across route establishment and therefore
// cannot be part of the application's source-to-carrier ownership fence.
func (self *sendPackLifecycleTracker) workloadBoundary(
	ctx context.Context,
) (sendPackLifecycleBoundary, bool) {
	return self.captureBoundary(ctx, sendPackLifecycleBoundaryScopeWorkload)
}

// waitThrough joins terminal publication for every identity in one boundary.
func (self *sendPackLifecycleTracker) waitThrough(
	ctx context.Context,
	boundary sendPackLifecycleBoundary,
) bool {
	for {
		if self.invalid.Load() {
			return false
		}
		complete := true
		for _, entry := range boundary.entries {
			if entry.phase.Load() != uint32(clientconnect.SendPackLifecyclePhaseTerminal) {
				if hook := self.beforeEntryWaitForTest.Load(); hook != nil {
					hook.call(entry)
				}
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

// close stops the owner without closing the producer-facing event channel.
func (self *sendPackLifecycleTracker) close() {
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
	})
}

// A reliable signaling Pack may remain in resend ownership after route
// establishment. That independent control lifetime must stay visible to the
// diagnostic boundary without blocking a completed application boundary.
func TestSendPackLifecycleWorkloadBoundaryIgnoresLiveReliableControl(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	control := clientconnect.SendPackLifecycleObservation{
		ClientId:      clientconnect.NewId(),
		DestinationId: clientconnect.NewId(),
		Token:         1,
		AckRequired:   true,
		MessageType:   protocol.MessageType_TransferExchangeSignals,
	}
	for _, phase := range []clientconnect.SendPackLifecyclePhase{
		clientconnect.SendPackLifecyclePhaseStarted,
		clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
	} {
		observation := control
		observation.Phase = phase
		observer(observation)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	workloadBoundary, ok := tracker.workloadBoundary(ctx)
	if !ok || workloadBoundary.startedCount != 0 ||
		len(workloadBoundary.entries) != 0 {
		t.Fatalf("workload boundary included live signaling: %+v", workloadBoundary)
	}
	if !tracker.waitThrough(ctx, workloadBoundary) {
		t.Fatalf("live signaling blocked workload boundary: %v", ctx.Err())
	}
	allBoundary, ok := tracker.boundary(ctx)
	if !ok || allBoundary.startedCount != 1 || len(allBoundary.entries) != 1 {
		t.Fatalf("diagnostic boundary lost live signaling: %+v", allBoundary)
	}
	canceledCtx, canceled := context.WithCancel(context.Background())
	canceled()
	if tracker.waitThrough(canceledCtx, allBoundary) {
		t.Fatal("live reliable signaling satisfied the diagnostic terminal boundary")
	}

	terminal := control
	terminal.Phase = clientconnect.SendPackLifecyclePhaseTerminal
	observer(terminal)
	if !tracker.waitThrough(ctx, allBoundary) {
		t.Fatalf("signaling terminal did not satisfy diagnostic boundary: %v", ctx.Err())
	}
}

// Filtering control traffic must not weaken the exact application fence: one
// IP Pack at first write remains owned until that same identity is terminal.
func TestSendPackLifecycleWorkloadBoundaryJoinsExactDataTerminal(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	data := clientconnect.SendPackLifecycleObservation{
		ClientId:      clientconnect.NewId(),
		DestinationId: clientconnect.NewId(),
		Token:         1,
		AckRequired:   true,
		MessageType:   protocol.MessageType_IpIpPacketToProvider,
	}
	for _, phase := range []clientconnect.SendPackLifecyclePhase{
		clientconnect.SendPackLifecyclePhaseStarted,
		clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
	} {
		observation := data
		observation.Phase = phase
		observer(observation)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boundary, ok := tracker.workloadBoundary(ctx)
	if !ok || boundary.startedCount != 1 || len(boundary.entries) != 1 ||
		boundary.entries[0].messageType != protocol.MessageType_IpIpPacketToProvider {
		t.Fatalf("workload boundary=%+v, want one live IP Pack", boundary)
	}
	canceledCtx, canceled := context.WithCancel(context.Background())
	canceled()
	if tracker.waitThrough(canceledCtx, boundary) {
		t.Fatal("first route write satisfied the workload terminal boundary")
	}
	terminal := data
	terminal.Phase = clientconnect.SendPackLifecyclePhaseTerminal
	observer(terminal)
	if !tracker.waitThrough(ctx, boundary) {
		t.Fatalf("exact IP Pack terminal did not satisfy workload boundary: %v", ctx.Err())
	}
}

// A test barrier can identify the exact Pack after capture but before its
// terminal callback begins. Here the boundary waits on the captured entry,
// rather than an observer callback already in flight.
func TestSendPackLifecycleTrackerExposesExactCapturedEntryWait(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	identity := clientconnect.SendPackLifecycleObservation{
		ClientId:      clientconnect.NewId(),
		DestinationId: clientconnect.NewId(),
		Token:         37,
		AckRequired:   false,
		MessageType:   protocol.MessageType_IpIpPacketToProvider,
	}
	for _, phase := range []clientconnect.SendPackLifecyclePhase{
		clientconnect.SendPackLifecyclePhaseStarted,
		clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
	} {
		observation := identity
		observation.Phase = phase
		observer(observation)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boundary, ok := tracker.workloadBoundary(ctx)
	if !ok || len(boundary.entries) != 1 {
		t.Fatalf("capture exact entry boundary: boundary=%+v err=%v", boundary, ctx.Err())
	}
	entryWaiting := make(chan struct{})
	var entryWaitingOnce sync.Once
	tracker.setBeforeEntryWaitForTest(func(entry *sendPackLifecycleEntry) {
		if entry.key.token == identity.Token &&
			entry.clientId == identity.ClientId &&
			entry.destinationId == identity.DestinationId &&
			entry.ackRequired == identity.AckRequired &&
			entry.messageType == identity.MessageType {
			entryWaitingOnce.Do(func() {
				close(entryWaiting)
			})
		}
	})
	defer tracker.setBeforeEntryWaitForTest(nil)
	waitResult := make(chan bool, 1)
	go func() {
		waitResult <- tracker.waitThrough(ctx, boundary)
	}()
	select {
	case <-entryWaiting:
	case <-ctx.Done():
		t.Fatalf("exact captured entry did not enter terminal wait: %v", ctx.Err())
	}
	select {
	case result := <-waitResult:
		t.Fatalf("unterminated exact entry completed boundary wait: %t", result)
	default:
	}
	terminal := identity
	terminal.Phase = clientconnect.SendPackLifecyclePhaseTerminal
	observer(terminal)
	select {
	case result := <-waitResult:
		if !result {
			t.Fatalf("exact entry terminal did not complete boundary: %v", ctx.Err())
		}
	case <-ctx.Done():
		t.Fatalf("join exact entry terminal: %v", ctx.Err())
	}
}

// A boundary joins a Started callback held before event publication and then
// retains that exact Pack until both remaining phases are published.
func TestSendPackLifecycleTrackerJoinsHeldPublicationAndExactTerminal(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	startedPublication := make(chan struct{})
	releasePublication := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releasePublication)
		})
	}
	defer release()
	tracker.setBeforeObserverPublishForTest(func(
		observation clientconnect.SendPackLifecycleObservation,
	) {
		if observation.Phase == clientconnect.SendPackLifecyclePhaseStarted {
			close(startedPublication)
			<-releasePublication
		}
	})
	identity := clientconnect.SendPackLifecycleObservation{
		ClientId:      clientconnect.NewId(),
		DestinationId: clientconnect.NewId(),
		Token:         1,
		AckRequired:   true,
	}
	go func() {
		observation := identity
		observation.Phase = clientconnect.SendPackLifecyclePhaseStarted
		observer(observation)
	}()
	<-startedPublication

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probeEntered := make(chan struct{})
	var probeEnteredOnce sync.Once
	tracker.setBeforePublisherWaitForTest(func() {
		probeEnteredOnce.Do(func() { close(probeEntered) })
	})
	probeCtx, probeCancel := context.WithCancel(context.Background())
	probeResult := make(chan *sendPackLifecycleBoundary, 1)
	go func() {
		boundary, ok := tracker.boundary(probeCtx)
		if !ok {
			probeResult <- nil
			return
		}
		probeResult <- &boundary
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("probe boundary did not enter publisher join: %v", ctx.Err())
	case <-probeEntered:
	}
	probeCancel()
	select {
	case <-ctx.Done():
		t.Fatalf("canceled publisher join did not return: %v", ctx.Err())
	case boundary := <-probeResult:
		if boundary != nil {
			t.Fatalf("canceled publisher join returned a boundary: %+v", *boundary)
		}
	}

	boundaryEntered := make(chan struct{})
	var boundaryEnteredOnce sync.Once
	tracker.setBeforePublisherWaitForTest(func() {
		boundaryEnteredOnce.Do(func() { close(boundaryEntered) })
	})
	defer tracker.setBeforePublisherWaitForTest(nil)
	boundaryResult := make(chan *sendPackLifecycleBoundary, 1)
	go func() {
		boundary, ok := tracker.boundary(ctx)
		if !ok {
			boundaryResult <- nil
			return
		}
		boundaryResult <- &boundary
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("boundary did not enter exact publisher join: %v", ctx.Err())
	case <-boundaryEntered:
	}
	release()
	var boundaryPointer *sendPackLifecycleBoundary
	select {
	case <-ctx.Done():
		t.Fatalf("boundary did not return after held publication release: %v", ctx.Err())
	case boundaryPointer = <-boundaryResult:
	}
	if boundaryPointer == nil {
		t.Fatalf("boundary failed after held publication release: %v", ctx.Err())
	}
	boundary := *boundaryPointer
	if len(boundary.entries) != 1 || boundary.startedCount != 1 {
		t.Fatalf("held publication boundary=%+v", boundary)
	}

	firstWrite := identity
	firstWrite.Phase = clientconnect.SendPackLifecyclePhaseFirstRouteWrite
	observer(firstWrite)
	canceledCtx, canceled := context.WithCancel(context.Background())
	canceled()
	if tracker.waitThrough(canceledCtx, boundary) {
		t.Fatal("first route write satisfied an exact terminal boundary")
	}
	terminal := identity
	terminal.Phase = clientconnect.SendPackLifecyclePhaseTerminal
	observer(terminal)
	if !tracker.waitThrough(ctx, boundary) {
		t.Fatalf("terminal publication did not satisfy exact boundary: %v", ctx.Err())
	}
}

// A later rebuilt Client may reuse token one without satisfying the earlier
// Client instance's held identity.
func TestSendPackLifecycleTrackerNamespacesRebuiltClients(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	first := tracker.newObserver()
	second := tracker.newObserver()
	clientId := clientconnect.NewId()
	destinationId := clientconnect.NewId()
	started := clientconnect.SendPackLifecycleObservation{
		Phase:         clientconnect.SendPackLifecyclePhaseStarted,
		ClientId:      clientId,
		DestinationId: destinationId,
		Token:         1,
	}
	first(started)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boundary, ok := tracker.boundary(ctx)
	if !ok {
		t.Fatalf("capture first Client boundary: %v", ctx.Err())
	}
	second(started)
	for _, phase := range []clientconnect.SendPackLifecyclePhase{
		clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
		clientconnect.SendPackLifecyclePhaseTerminal,
	} {
		observation := started
		observation.Phase = phase
		second(observation)
	}
	if _, ok := tracker.boundary(ctx); !ok {
		t.Fatalf("flush second Client lifecycle: %v", ctx.Err())
	}
	canceledCtx, canceled := context.WithCancel(context.Background())
	canceled()
	if tracker.waitThrough(canceledCtx, boundary) {
		t.Fatal("rebuilt Client token satisfied the earlier instance boundary")
	}
	for _, phase := range []clientconnect.SendPackLifecyclePhase{
		clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
		clientconnect.SendPackLifecyclePhaseTerminal,
	} {
		observation := started
		observation.Phase = phase
		first(observation)
	}
	if !tracker.waitThrough(ctx, boundary) {
		t.Fatalf("first Client terminal did not satisfy its boundary: %v", ctx.Err())
	}
}

// A later Pack's terminal cannot stand in for an earlier final ACK whose
// observer publication is held. The canceled wait is an exact state check,
// not a timing-based assertion.
func TestSendPackLifecycleTrackerHeldTerminalRejectsLaterCompletion(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	clientId := clientconnect.NewId()
	destinationId := clientconnect.NewId()
	first := clientconnect.SendPackLifecycleObservation{
		ClientId:      clientId,
		DestinationId: destinationId,
		Token:         1,
		AckRequired:   true,
	}
	for _, phase := range []clientconnect.SendPackLifecyclePhase{
		clientconnect.SendPackLifecyclePhaseStarted,
		clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
	} {
		observation := first
		observation.Phase = phase
		observer(observation)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boundary, ok := tracker.boundary(ctx)
	if !ok || len(boundary.entries) != 1 {
		t.Fatalf("capture first Pack boundary: boundary=%+v err=%v", boundary, ctx.Err())
	}

	terminalHeld := make(chan struct{})
	releaseTerminal := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseTerminal)
		})
	}
	defer release()
	tracker.setBeforeObserverPublishForTest(func(
		observation clientconnect.SendPackLifecycleObservation,
	) {
		if observation.Token == first.Token &&
			observation.Phase == clientconnect.SendPackLifecyclePhaseTerminal {
			close(terminalHeld)
			<-releaseTerminal
		}
	})
	go func() {
		observation := first
		observation.Phase = clientconnect.SendPackLifecyclePhaseTerminal
		observer(observation)
	}()
	<-terminalHeld

	later := first
	later.Token = 2
	for _, phase := range []clientconnect.SendPackLifecyclePhase{
		clientconnect.SendPackLifecyclePhaseStarted,
		clientconnect.SendPackLifecyclePhaseFirstRouteWrite,
		clientconnect.SendPackLifecyclePhaseTerminal,
	} {
		observation := later
		observation.Phase = phase
		observer(observation)
	}
	canceledCtx, canceled := context.WithCancel(context.Background())
	canceled()
	if tracker.waitThrough(canceledCtx, boundary) {
		t.Fatal("later Pack terminal satisfied the held final ACK boundary")
	}

	release()
	if !tracker.waitThrough(ctx, boundary) {
		t.Fatalf("held final ACK did not satisfy its exact boundary: %v", ctx.Err())
	}
}

// Error publication precedes terminal state. Ownership joins wait for the
// exact failed terminal, while its monotonic watermark remains available to
// the measured-interval policy.
func TestSendPackLifecycleTrackerJoinsFailedTerminalAndRetainsWatermark(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	identity := clientconnect.SendPackLifecycleObservation{
		Phase:               clientconnect.SendPackLifecyclePhaseStarted,
		ClientId:            clientconnect.NewId(),
		DestinationId:       clientconnect.NewId(),
		Token:               1,
		AckRequired:         true,
		UpstreamRecoverable: true,
	}
	observer(identity)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boundary, ok := tracker.boundary(ctx)
	if !ok || len(boundary.entries) != 1 {
		t.Fatalf("capture failed Pack boundary: boundary=%+v err=%v", boundary, ctx.Err())
	}
	firstWrite := identity
	firstWrite.Phase = clientconnect.SendPackLifecyclePhaseFirstRouteWrite
	observer(firstWrite)

	failureClassified := make(chan struct{})
	releaseTerminal := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseTerminal)
		})
	}
	defer release()
	var terminalHoldOnce sync.Once
	tracker.setBeforeTerminalReleaseForTest(func(
		_ *sendPackLifecycleEntry,
		observation clientconnect.SendPackLifecycleObservation,
	) {
		if observation.Token != identity.Token {
			return
		}
		terminalHoldOnce.Do(func() {
			close(failureClassified)
			<-releaseTerminal
		})
	})
	terminalPublisherReturned := make(chan struct{})
	go func() {
		terminal := identity
		terminal.Phase = clientconnect.SendPackLifecyclePhaseTerminal
		terminal.Err = clientconnect.ErrSendPackNotAdmitted
		observer(terminal)
		close(terminalPublisherReturned)
	}()
	<-failureClassified
	select {
	case <-terminalPublisherReturned:
	case <-ctx.Done():
		t.Fatalf("terminal producer blocked behind tracker owner: %v", ctx.Err())
	}
	// A later Pack and boundary request must also enter while the exact earlier
	// terminal remains held by the tracker owner. This is the ordering used by
	// source-to-carrier joins: owner-side observation cannot feed backpressure
	// into Connect's synchronous SendSequence callback.
	laterIdentity := identity
	laterIdentity.Token = 2
	observer(laterIdentity)
	laterFirstWrite := laterIdentity
	laterFirstWrite.Phase = clientconnect.SendPackLifecyclePhaseFirstRouteWrite
	observer(laterFirstWrite)
	laterTerminal := laterIdentity
	laterTerminal.Phase = clientconnect.SendPackLifecyclePhaseTerminal
	observer(laterTerminal)
	boundaryEnqueued := make(chan struct{})
	var boundaryEnqueuedOnce sync.Once
	tracker.setAfterBoundaryEnqueueForTest(func(sendPackLifecycleBoundaryScope) {
		boundaryEnqueuedOnce.Do(func() { close(boundaryEnqueued) })
	})
	laterBoundaryResult := make(chan sendPackLifecycleBoundary, 1)
	go func() {
		laterBoundary, _ := tracker.boundary(ctx)
		laterBoundaryResult <- laterBoundary
	}()
	select {
	case <-boundaryEnqueued:
	case <-ctx.Done():
		t.Fatalf("later boundary did not enter behind held tracker owner: %v", ctx.Err())
	}
	select {
	case laterBoundary := <-laterBoundaryResult:
		t.Fatalf("later boundary crossed held terminal: %+v", laterBoundary)
	default:
	}
	if tracker.failures.Load() != 1 {
		t.Fatalf("terminal failure was not classified before release: %d", tracker.failures.Load())
	}
	if tracker.recoverableFailures.Load() != 1 {
		t.Fatalf(
			"recoverable terminal failure count=%d, want one",
			tracker.recoverableFailures.Load(),
		)
	}
	failureSamples := tracker.failureSnapshot()
	if len(failureSamples) != 1 ||
		failureSamples[0].Token != identity.Token ||
		failureSamples[0].Err == nil {
		t.Fatalf("terminal failure samples=%+v", failureSamples)
	}
	if phase := boundary.entries[0].phase.Load(); phase != sendPackLifecycleEntryPhaseCompleting {
		t.Fatalf("failed terminal phase=%d before release, want completing", phase)
	}
	canceledCtx, canceled := context.WithCancel(context.Background())
	canceled()
	if tracker.waitThrough(canceledCtx, boundary) {
		t.Fatal("classified failure passed before terminal release")
	}
	release()
	if !tracker.waitThrough(ctx, boundary) {
		t.Fatalf("failed terminal did not satisfy ownership boundary: %v", ctx.Err())
	}
	var laterBoundary sendPackLifecycleBoundary
	select {
	case laterBoundary = <-laterBoundaryResult:
	case <-ctx.Done():
		t.Fatalf("capture boundary after failed terminal: %v", ctx.Err())
	}
	if len(laterBoundary.entries) != 0 || laterBoundary.failedAtCapture != 1 {
		t.Fatalf("later failed-terminal boundary=%+v, want empty with watermark one", laterBoundary)
	}
	if !tracker.waitThrough(ctx, laterBoundary) {
		t.Fatalf("historical failure poisoned later ownership boundary: %v", ctx.Err())
	}
}

// Provider NoAck data failures are tracked separately so only the
// latency-under-load probe scope can account them as measured loss.
func TestSendPackLifecycleTrackerClassifiesProviderDatagramFailure(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	identity := clientconnect.SendPackLifecycleObservation{
		Phase:         clientconnect.SendPackLifecyclePhaseStarted,
		ClientId:      clientconnect.NewId(),
		DestinationId: clientconnect.NewId(),
		Token:         1,
		MessageType:   protocol.MessageType_IpIpPacketFromProvider,
	}
	observer(identity)
	firstWrite := identity
	firstWrite.Phase = clientconnect.SendPackLifecyclePhaseFirstRouteWrite
	firstWrite.Err = clientconnect.ErrSendPackNotAdmitted
	observer(firstWrite)
	terminal := firstWrite
	terminal.Phase = clientconnect.SendPackLifecyclePhaseTerminal
	observer(terminal)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boundary, ok := tracker.workloadBoundary(ctx)
	if !ok || len(boundary.entries) != 0 || boundary.failedAtCapture != 1 {
		t.Fatalf("provider datagram boundary=%+v ok=%t", boundary, ok)
	}
	if got := tracker.workloadDatagramFailures.Load(); got != 1 {
		t.Fatalf("provider datagram failures=%d, want one", got)
	}
	if got := tracker.workloadRecoverableFailures.Load(); got != 0 {
		t.Fatalf("provider datagram counted as recoverable: %d", got)
	}
}

// Upstream ownership makes a refused attempt retryable; it cannot recover a
// Pack that was admitted and later exhausted Transfer's terminal reliability.
func TestSendPackLifecycleTrackerRejectsRecoverablePostAdmissionError(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	identity := clientconnect.SendPackLifecycleObservation{
		Phase:               clientconnect.SendPackLifecyclePhaseStarted,
		ClientId:            clientconnect.NewId(),
		DestinationId:       clientconnect.NewId(),
		Token:               1,
		AckRequired:         true,
		MessageType:         protocol.MessageType_IpIpPacketFromProvider,
		UpstreamRecoverable: true,
	}
	observer(identity)
	firstWrite := identity
	firstWrite.Phase = clientconnect.SendPackLifecyclePhaseFirstRouteWrite
	observer(firstWrite)
	terminal := firstWrite
	terminal.Phase = clientconnect.SendPackLifecyclePhaseTerminal
	terminal.Err = context.DeadlineExceeded
	observer(terminal)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boundary, ok := tracker.workloadBoundary(ctx)
	if !ok || len(boundary.entries) != 0 || boundary.failedAtCapture != 1 {
		t.Fatalf("post-admission failure boundary=%+v ok=%t", boundary, ok)
	}
	if got := tracker.workloadRecoverableFailures.Load(); got != 0 {
		t.Fatalf("post-admission failure counted as recoverable: %d", got)
	}
}
