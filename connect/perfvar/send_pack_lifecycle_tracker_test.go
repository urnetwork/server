// This file supplies PERFVAR's exact all-Pack producer boundary. It tracks
// original Pack identities across rebuilt clients and joins terminal resend
// ownership before a measurement snapshots the modeled physical carrier.
package perfvar

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

const sendPackLifecycleEventCapacity = 64 * 1024
const sendPackLifecycleFailureSampleCapacity = 16

// The completing state keeps terminal failure classification ordered before
// the release store observed by exact boundaries.
const sendPackLifecycleEntryPhaseCompleting = uint32(4)

// sendPackLifecycleTracker joins every original non-loopback Pack, including
// reliable ACK traffic that the older NoAck first-write seam omits.
type sendPackLifecycleTracker struct {
	ctx       context.Context
	cancel    context.CancelFunc
	events    chan sendPackLifecycleEvent
	progress  chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	invalid   atomic.Bool
	// One historical terminal failure poisons the fixture intentionally. A
	// measurement cannot attribute later bytes once any source Pack failed.
	failures       atomic.Uint64
	started        atomic.Uint64
	publishing     atomic.Int64
	nextClient     atomic.Uint64
	failureLock    sync.Mutex
	failureSamples []clientconnect.SendPackLifecycleObservation

	// Nil test barriers expose publication races without delaying production
	// measurement callbacks.
	beforeObserverPublishForTest atomic.Pointer[sendPackLifecycleObserverTestHook]
	beforeTerminalReleaseForTest atomic.Pointer[sendPackLifecycleObserverTestHook]
	beforePublisherWaitForTest   atomic.Pointer[sendPackLifecycleBoundaryTestHook]
}

// sendPackLifecycleObserverTestHook makes callback replacement race-free while
// live clients continue publishing unrelated control traffic.
type sendPackLifecycleObserverTestHook struct {
	call func(clientconnect.SendPackLifecycleObservation)
}

// A zero-argument test hook exposes entry into a publisher join without
// changing the observation callback surface.
type sendPackLifecycleBoundaryTestHook struct {
	call func()
}

// sendPackLifecycleKey namespaces per-Client tokens by observer registration.
type sendPackLifecycleKey struct {
	clientInstance uint64
	token          uint64
}

// sendPackLifecycleEntry retains immutable identity and terminal publication.
type sendPackLifecycleEntry struct {
	clientId      clientconnect.Id
	destinationId clientconnect.Id
	ackRequired   bool
	phase         atomic.Uint32
}

// sendPackLifecycleBoundary captures exact unfinished identities plus the
// number of Started callbacks that had entered publication.
type sendPackLifecycleBoundary struct {
	entries         []*sendPackLifecycleEntry
	startedCount    uint64
	failedAtCapture uint64
}

// sendPackLifecycleEvent serializes observations and boundary commands on one
// owner goroutine.
type sendPackLifecycleEvent struct {
	clientInstance uint64
	observation    clientconnect.SendPackLifecycleObservation
	boundary       chan sendPackLifecycleBoundary
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
		self.publishing.Add(1)
		defer func() {
			self.publishing.Add(-1)
			self.notify()
		}()
		if observation.Phase == clientconnect.SendPackLifecyclePhaseStarted {
			self.started.Add(1)
		}
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

// A nil-by-default barrier proves a boundary observed an entered publisher
// before a test cancels or releases that exact generation.
func (self *sendPackLifecycleTracker) setBeforePublisherWaitForTest(callback func()) {
	if callback == nil {
		self.beforePublisherWaitForTest.Store(nil)
		return
	}
	self.beforePublisherWaitForTest.Store(&sendPackLifecycleBoundaryTestHook{call: callback})
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
			for key, entry := range entries {
				if entry.phase.Load() == uint32(clientconnect.SendPackLifecyclePhaseTerminal) {
					delete(entries, key)
				} else {
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
				clientId:      observation.ClientId,
				destinationId: observation.DestinationId,
				ackRequired:   observation.AckRequired,
				phase:         atomic.Uint32{},
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
				self.failures.Add(1)
				self.failureLock.Lock()
				if len(self.failureSamples) < sendPackLifecycleFailureSampleCapacity {
					self.failureSamples = append(self.failureSamples, observation)
				}
				self.failureLock.Unlock()
			}
			if hook := self.beforeTerminalReleaseForTest.Load(); hook != nil {
				hook.call(observation)
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

// setBeforeTerminalReleaseForTest installs a barrier after terminal error
// classification and before the release store observed by waiters.
func (self *sendPackLifecycleTracker) setBeforeTerminalReleaseForTest(
	callback func(clientconnect.SendPackLifecycleObservation),
) {
	if callback == nil {
		self.beforeTerminalReleaseForTest.Store(nil)
		return
	}
	self.beforeTerminalReleaseForTest.Store(&sendPackLifecycleObserverTestHook{call: callback})
}

// sameSendPackLifecycleIdentity rejects phase pairs that reuse a token for a
// different logical Pack.
func sameSendPackLifecycleIdentity(
	entry *sendPackLifecycleEntry,
	observation clientconnect.SendPackLifecycleObservation,
) bool {
	return entry.clientId == observation.ClientId &&
		entry.destinationId == observation.DestinationId &&
		entry.ackRequired == observation.AckRequired
}

// waitForPublishers joins callbacks that entered before a boundary request.
func (self *sendPackLifecycleTracker) waitForPublishers(ctx context.Context) bool {
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
	return true
}

// boundary flushes prior callback publications and captures exact unfinished
// Pack identities. A generation retry closes the callback-entry race around
// the zero-publisher observation.
func (self *sendPackLifecycleTracker) boundary(
	ctx context.Context,
) (sendPackLifecycleBoundary, bool) {
	for {
		startedBefore := self.started.Load()
		if !self.waitForPublishers(ctx) {
			return sendPackLifecycleBoundary{}, false
		}
		response := make(chan sendPackLifecycleBoundary, 1)
		select {
		case <-ctx.Done():
			return sendPackLifecycleBoundary{}, false
		case <-self.ctx.Done():
			return sendPackLifecycleBoundary{}, false
		case self.events <- sendPackLifecycleEvent{boundary: response}:
		}
		var boundary sendPackLifecycleBoundary
		select {
		case <-ctx.Done():
			return sendPackLifecycleBoundary{}, false
		case <-self.ctx.Done():
			return sendPackLifecycleBoundary{}, false
		case boundary = <-response:
		}
		if self.publishing.Load() == 0 &&
			startedBefore == self.started.Load() &&
			boundary.startedCount == self.started.Load() {
			return boundary, !self.invalid.Load()
		}
	}
}

// waitThrough joins terminal publication for every identity in one boundary.
func (self *sendPackLifecycleTracker) waitThrough(
	ctx context.Context,
	boundary sendPackLifecycleBoundary,
) bool {
	for {
		if self.invalid.Load() || boundary.failedAtCapture != 0 || self.failures.Load() != 0 {
			return false
		}
		complete := true
		for _, entry := range boundary.entries {
			if entry.phase.Load() != uint32(clientconnect.SendPackLifecyclePhaseTerminal) {
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

// Error publication precedes terminal state, so a failed Pack can never pass
// an already captured boundary.
func TestSendPackLifecycleTrackerRejectsTerminalFailure(t *testing.T) {
	tracker := newSendPackLifecycleTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	identity := clientconnect.SendPackLifecycleObservation{
		Phase:         clientconnect.SendPackLifecyclePhaseStarted,
		ClientId:      clientconnect.NewId(),
		DestinationId: clientconnect.NewId(),
		Token:         1,
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
	tracker.setBeforeTerminalReleaseForTest(func(
		observation clientconnect.SendPackLifecycleObservation,
	) {
		close(failureClassified)
		<-releaseTerminal
	})
	go func() {
		terminal := identity
		terminal.Phase = clientconnect.SendPackLifecyclePhaseTerminal
		terminal.Err = errors.New("terminal failure")
		observer(terminal)
	}()
	<-failureClassified
	if tracker.failures.Load() != 1 {
		t.Fatalf("terminal failure was not classified before release: %d", tracker.failures.Load())
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
	if tracker.waitThrough(ctx, boundary) {
		t.Fatal("terminal failure passed an exact lifecycle boundary")
	}
}
