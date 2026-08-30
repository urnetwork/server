// This file drives application TUN traffic through production route selection,
// provider NAT, real server carriers, and deterministic userspace networks.
package perfvar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
)

// A route name resolves all forced platform and P2P construction choices.
type fullTunRoute string

const (
	fullTunRouteExchangeH1   fullTunRoute = "exchange-h1"
	fullTunRouteExchangeH3   fullTunRoute = "exchange-h3"
	fullTunRouteExchangeAuto fullTunRoute = "exchange-auto"
	fullTunRouteP2pLegacy    fullTunRoute = "p2p-legacy"
	fullTunRouteP2pFast      fullTunRoute = "p2p-fast"

	fullTunProbePayloadByteCount = 16 * 1024
)

func fullTunRouteIsExchange(route fullTunRoute) bool {
	switch route {
	case fullTunRouteExchangeH1, fullTunRouteExchangeH3, fullTunRouteExchangeAuto:
		return true
	default:
		return false
	}
}

// The one-hop Pion fixture is link-oriented provider-left/device-right, while
// every scenario is application-oriented device-upload/device-download.
func oneHopP2pNetworkProfile(profile networkProfile) networkProfile {
	profile.Forward, profile.Reverse = profile.Reverse, profile.Forward
	return profile
}

// The composed path owns both application and provider sides of the tunnel.
type fullTunPath struct {
	t           testing.TB
	ctx         context.Context
	environment *routeEnvironment
	route       fullTunRoute
	p2pHopCount int

	appTun                       *clientconnect.Tun
	providerCarrierTun           *clientconnect.Tun
	deviceCarrierTun             *clientconnect.Tun
	deviceCarrierNode            string
	multiClient                  *clientconnect.RemoteUserNatMultiClient
	apiGenerator                 *clientconnect.ApiMultiClientGenerator
	deviceTransports             *platformTransportOwner
	deviceClient                 *atomic.Pointer[clientconnect.Client]
	deviceClientId               clientconnect.Id
	providerClient               *clientconnect.Client
	providerClientId             clientconnect.Id
	providerTransport            *clientconnect.PlatformTransport
	providerPlatformReceiveStats *clientconnect.PlatformTransportReceiveStats
	devicePlatformReceiveStats   *clientconnect.PlatformTransportReceiveStats
	providerH3DatagramStats      *clientconnect.H3DatagramStats
	deviceH3DatagramStats        *clientconnect.H3DatagramStats
	providerLocalNat             *clientconnect.LocalUserNat
	providerRemoteNat            *clientconnect.RemoteUserNatProvider
	p2pNetwork                   *p2pNetwork
	streamP2pNetwork             *streamP2pNetwork
	streamP2pClients             []*routeClient
	streamP2pStats               []*clientconnect.P2pDataPlaneStats
	streamP2pRouteTraces         []*p2pRouteStateTrace
	providerStats                *clientconnect.P2pDataPlaneStats
	deviceStats                  *clientconnect.P2pDataPlaneStats
	providerProbeTrace           *p2pProbeEventTrace
	deviceProbeTrace             *p2pProbeEventTrace
	providerSendRoutes           *platformSendRouteController
	deviceSendRoutes             *platformSendRouteController
	platformSendRoutes           []*platformSendRouteController
	providerNoAckSends           *noAckSendTracker
	deviceNoAckSends             *noAckSendTracker
	providerPackSends            *sendPackLifecycleTracker
	devicePackSends              *sendPackLifecycleTracker
	providerReturns              *providerReturnSendTracker
	bridgeSends                  *fullTunBridgeSendTracker

	bridgeWaitGroup sync.WaitGroup
	bridgeStarted   bool

	measurementLock                   sync.Mutex
	preparedCarrierStart              *perfvarCarrierBoundary
	carrierMeasurementStart           *perfvarCarrierBoundary
	carrierMeasurementEnd             *perfvarCarrierBoundary
	activePackFailureFloor            *perfvarPackFailureCounts
	allowProviderDatagramPackFailures bool
	carrierFencePackets               int
	readinessAppFence                 atomic.Bool
	readinessObservation              fullTunRouteReadinessObservation
	// Nil test seams can hold or drop an exact terminal-marker attempt.
	beforeUdpTerminalMarkerForTest      func(context.Context, bool, int) error
	afterUdpTerminalCarrierForTest      func(context.Context, bool, int) error
	afterUdpTerminalMarkerForTest       func(context.Context, bool, int) error
	beforeUdpTerminalReceiptForTest     func(context.Context, bool)
	afterUdpTerminalReceiptForTest      func(bool)
	beforeWarmedTcpAckForTest           func(context.Context, bool) error
	beforeWarmedTcpMeasuredForTest      func(bool)
	readinessProbePayloadForTest        []byte
	beforeReadinessClientWriteForTest   func()
	beforeReadinessServerCloseForTest   func()
	beforeWorkloadServerCloseForTest    func()
	workloadFlowServerSettingsForTest   *logicalTCPFlowServerSettings
	beforeWorkloadClientDialForTest     func(context.Context, string) error
	beforeWorkloadLoserDoneForTest      func()
	beforeWorkloadLoserWaitForTest      func()
	beforeLatencyProbeDoneForTest       func()
	beforeLatencyProbeWaitForTest       func()
	beforeLatencyBulkWaitForTest        func()
	beforeLatencyLoadedProbeForTest     func() error
	afterGeneratedClientMismatchForTest func()
	afterCarrierStartForTest            func(int)
	afterAccessCarrierStartForTest      func()
	afterCarrierEndCandidateForTest     func(int)
	// Nil construction seams expose exact cleanup completion and error
	// aggregation without changing production teardown.
	afterConstructionCleanupForTest func(fullTunConstructionResource)
	constructionCleanupErrorForTest func(fullTunConstructionResource) error
}

// One named resource identifies a construction acquisition and its matching
// rollback disposition in deterministic failure-injection tests.
type fullTunConstructionResource string

const (
	fullTunConstructionResourceAppTun              fullTunConstructionResource = "application TUN"
	fullTunConstructionResourceMultiClient         fullTunConstructionResource = "multi-client"
	fullTunConstructionResourceBridge              fullTunConstructionResource = "application bridge"
	fullTunConstructionResourceApiGenerator        fullTunConstructionResource = "device generator"
	fullTunConstructionResourceDeviceTransports    fullTunConstructionResource = "generated device transports"
	fullTunConstructionResourceProviderRemoteNat   fullTunConstructionResource = "provider remote NAT"
	fullTunConstructionResourceProviderLocalNat    fullTunConstructionResource = "provider local NAT"
	fullTunConstructionResourceProviderClient      fullTunConstructionResource = "provider client"
	fullTunConstructionResourceProviderTransport   fullTunConstructionResource = "provider transport"
	fullTunConstructionResourceIntermediaryClient  fullTunConstructionResource = "intermediary client"
	fullTunConstructionResourceIntermediaryRoute   fullTunConstructionResource = "intermediary transport"
	fullTunConstructionResourceIntermediaryTun     fullTunConstructionResource = "intermediary TUN"
	fullTunConstructionResourceP2pNetwork          fullTunConstructionResource = "P2P network"
	fullTunConstructionResourceStreamP2pNetwork    fullTunConstructionResource = "stream P2P network"
	fullTunConstructionResourceProviderCarrierTun  fullTunConstructionResource = "provider carrier TUN"
	fullTunConstructionResourceDeviceCarrierTun    fullTunConstructionResource = "device carrier TUN"
	fullTunConstructionResourceNoAckTracker        fullTunConstructionResource = "no-ack tracker"
	fullTunConstructionResourcePackTracker         fullTunConstructionResource = "Pack tracker"
	fullTunConstructionResourceReturnTracker       fullTunConstructionResource = "provider return tracker"
	fullTunConstructionResourceSendRouteController fullTunConstructionResource = "platform send-route controller"
)

// Construction stages sit immediately after each distinct acquisition or
// publication boundary. Tests inject failure at the stage, so rollback starts
// with exactly the same partially built ownership graph as a real error.
type fullTunConstructionStage string

const (
	fullTunConstructionStageProviderCarrierTun          fullTunConstructionStage = "provider-carrier-tun"
	fullTunConstructionStageDeviceCarrierTun            fullTunConstructionStage = "device-carrier-tun"
	fullTunConstructionStageP2pNetwork                  fullTunConstructionStage = "p2p-network"
	fullTunConstructionStageSendRouteControllers        fullTunConstructionStage = "send-route-controllers"
	fullTunConstructionStageSourceTrackers              fullTunConstructionStage = "source-trackers"
	fullTunConstructionStageProviderClient              fullTunConstructionStage = "provider-client"
	fullTunConstructionStageProviderTransport           fullTunConstructionStage = "provider-transport"
	fullTunConstructionStageProviderTransportReady      fullTunConstructionStage = "provider-transport-ready"
	fullTunConstructionStageProviderLocalNat            fullTunConstructionStage = "provider-local-nat"
	fullTunConstructionStageProviderTrackers            fullTunConstructionStage = "provider-trackers"
	fullTunConstructionStageProviderRemoteNat           fullTunConstructionStage = "provider-remote-nat"
	fullTunConstructionStageProviderRegistration        fullTunConstructionStage = "provider-registration"
	fullTunConstructionStageStreamIntermediaryClient    fullTunConstructionStage = "stream-intermediary-client"
	fullTunConstructionStageStreamIntermediaryTransport fullTunConstructionStage = "stream-intermediary-transport"
	fullTunConstructionStageStreamIntermediaryReady     fullTunConstructionStage = "stream-intermediary-ready"
	fullTunConstructionStageDeviceTransportOwner        fullTunConstructionStage = "device-transport-owner"
	fullTunConstructionStageDeviceGenerator             fullTunConstructionStage = "device-generator"
	fullTunConstructionStageApplicationTun              fullTunConstructionStage = "application-tun"
	fullTunConstructionStageMultiClient                 fullTunConstructionStage = "multi-client"
	fullTunConstructionStageBridge                      fullTunConstructionStage = "bridge"
	fullTunConstructionStageRouteReady                  fullTunConstructionStage = "route-ready"
)

// Optional hooks expose deterministic construction and measurement seams. A
// stage hook may install cleanup observers on the partial path before returning
// its error; settings hooks run before their corresponding object is built.
type fullTunConstructionTestHooks struct {
	afterStage                        func(fullTunConstructionStage, *fullTunPath) error
	configureConnectHandlerSettings   func(*connectserver.ConnectHandlerSettings)
	configureProviderClientSettings   func(*clientconnect.ClientSettings)
	configureProviderPlatformSettings func(*clientconnect.PlatformTransportSettings)
	configureDeviceClientSettings     func(*clientconnect.ClientSettings)
	configureDevicePlatformSettings   func(*clientconnect.PlatformTransportSettings)
}

// The transaction owns the partial path until commit transfers every acquired
// resource to the returned fullTunPath. Any return before commit rolls back the
// same graph through the normal dependency-ordered teardown.
type fullTunConstructionOwner struct {
	path         *fullTunPath
	committed    bool
	rollbackOnce sync.Once
	rollbackErr  error
}

// Construction starts with no acquired resource beyond borrowed fixture state.
func newFullTunConstructionOwner(path *fullTunPath) *fullTunConstructionOwner {
	return &fullTunConstructionOwner{path: path}
}

// A successful constructor transfers ownership exactly once.
func (self *fullTunConstructionOwner) commit() *fullTunPath {
	self.committed = true
	return self.path
}

// A failed constructor closes and joins every resource acquired so far.
func (self *fullTunConstructionOwner) rollback(ctx context.Context) error {
	if self.committed {
		return nil
	}
	self.rollbackOnce.Do(func() {
		self.rollbackErr = self.path.closeAndWait(ctx)
	})
	return self.rollbackErr
}

// A staged error preserves which exact source-to-carrier join failed while
// wrapping its cancellation or structural cause for callers.
type fullTunMeasurementBoundaryError struct {
	stage string
	err   error
}

// Error keeps the measurement failure compact in machine-readable records.
func (self *fullTunMeasurementBoundaryError) Error() string {
	return fmt.Sprintf("%s: %v", self.stage, self.err)
}

// Unwrap retains cancellation identity for errors.Is callers.
func (self *fullTunMeasurementBoundaryError) Unwrap() error {
	return self.err
}

// A unified source-to-carrier fixed point publishes one start for the next
// caller without reopening a wait/begin gap.
func (self *fullTunPath) setPreparedCarrierStart(boundary perfvarCarrierBoundary) {
	self.measurementLock.Lock()
	self.preparedCarrierStart = &boundary
	self.measurementLock.Unlock()
}

// The next carrier-begin call consumes the exact prepared generation once.
func (self *fullTunPath) takePreparedCarrierStart() *perfvarCarrierBoundary {
	self.measurementLock.Lock()
	defer self.measurementLock.Unlock()
	boundary := self.preparedCarrierStart
	self.preparedCarrierStart = nil
	return boundary
}

// The nil-by-default Connect observer gives PERFVAR a positive first-route-
// write boundary without changing production send behavior.
type noAckSendTracker struct {
	ctx            context.Context
	cancel         context.CancelFunc
	startedCount   atomic.Uint64
	completedCount atomic.Uint64
	failureCount   atomic.Uint64
	invalid        atomic.Bool
	nextInstance   atomic.Uint64
	events         chan noAckSendEvent
	progress       chan struct{}
	done           chan struct{}
	closeOnce      sync.Once
	// Nil in normal harness runs. Tests can hold the exact point after error
	// disposition but before terminal publication.
	beforeCompletionPublishForTest func()
	beforeObserverSendForTest      func()
}

const noAckSendEventCapacity = 64 * 1024

// Tokens are unique only within one Client instance. A fresh observer closure
// supplies the missing instance namespace when a logical ClientId is rebuilt.
type noAckSendKey struct {
	instance uint64
	token    uint64
}

// One immutable boundary retains the exact sends that had started when it was
// captured. A later completion can never stand in for an earlier held send.
type noAckSendBoundary struct {
	entries []*noAckSendEntry
}

// One owner-goroutine entry publishes terminal state atomically to waiters.
type noAckSendEntry struct {
	// 0 is pending, 1 is owned by one completing callback, and 2 is the final
	// release publication observed by boundaries.
	state atomic.Uint32
}

// Events and snapshot commands share one FIFO so a boundary follows every
// callback that finished enqueueing before the boundary request.
type noAckSendEvent struct {
	instance    uint64
	observation clientconnect.NoAckSendObservation
	boundary    chan noAckSendBoundary
}

// A bounded owner queue removes maps and allocations from Connect callbacks.
func newNoAckSendTracker() *noAckSendTracker {
	ctx, cancel := context.WithCancel(context.Background())
	tracker := &noAckSendTracker{
		ctx:      ctx,
		cancel:   cancel,
		events:   make(chan noAckSendEvent, noAckSendEventCapacity),
		progress: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	go tracker.run()
	return tracker
}

// The Connect callback only performs one bounded, zero-time channel send. The
// owner goroutine performs all map allocation and exact-pair validation away
// from the measured send path.
func (self *noAckSendTracker) newObserver() func(clientconnect.NoAckSendObservation) {
	instance := self.nextInstance.Add(1)
	return func(observation clientconnect.NoAckSendObservation) {
		if self.beforeObserverSendForTest != nil {
			self.beforeObserverSendForTest()
		}
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		select {
		case <-self.ctx.Done():
			return
		case self.events <- noAckSendEvent{instance: instance, observation: observation}:
		default:
			self.invalid.Store(true)
			select {
			case self.progress <- struct{}{}:
			default:
			}
		}
	}
}

// The single owner validates exact pairs, classifies failures, retires
// completed entries at boundaries, and performs the final release store.
func (self *noAckSendTracker) run() {
	defer close(self.done)
	entries := map[noAckSendKey]*noAckSendEntry{}
	for {
		var event noAckSendEvent
		select {
		case <-self.ctx.Done():
			return
		case event = <-self.events:
		}
		if event.boundary != nil {
			boundary := noAckSendBoundary{}
			for key, entry := range entries {
				if entry.state.Load() == 2 {
					delete(entries, key)
				} else {
					boundary.entries = append(boundary.entries, entry)
				}
			}
			event.boundary <- boundary
			continue
		}
		observation := event.observation
		key := noAckSendKey{instance: event.instance, token: observation.Token}
		switch observation.Phase {
		case clientconnect.NoAckSendPhaseStarted:
			if _, duplicate := entries[key]; duplicate {
				self.invalid.Store(true)
			} else {
				entries[key] = &noAckSendEntry{}
				self.startedCount.Add(1)
			}
		case clientconnect.NoAckSendPhaseCompleted:
			entry, ok := entries[key]
			if !ok || !entry.state.CompareAndSwap(0, 1) {
				self.invalid.Store(true)
			} else {
				if observation.Err != nil {
					self.failureCount.Add(1)
				}
				self.completedCount.Add(1)
				if self.beforeCompletionPublishForTest != nil {
					self.beforeCompletionPublishForTest()
				}
				entry.state.Store(2)
			}
		default:
			self.invalid.Store(true)
		}
		select {
		case self.progress <- struct{}{}:
		default:
		}
	}
}

// A snapshot request is ordered after every callback event that had completed
// its channel send before this call. Completed entries are retired here, so
// memory is bounded by the event queue and currently in-flight sends.
func (self *noAckSendTracker) boundary(ctx context.Context) (noAckSendBoundary, bool) {
	response := make(chan noAckSendBoundary, 1)
	select {
	case <-ctx.Done():
		return noAckSendBoundary{}, false
	case <-self.ctx.Done():
		return noAckSendBoundary{}, false
	case <-self.done:
		return noAckSendBoundary{}, false
	case self.events <- noAckSendEvent{boundary: response}:
	}
	select {
	case <-ctx.Done():
		return noAckSendBoundary{}, false
	case <-self.ctx.Done():
		return noAckSendBoundary{}, false
	case <-self.done:
		return noAckSendBoundary{}, false
	case boundary := <-response:
		return boundary, !self.invalid.Load()
	}
}

// Waiting checks every captured entry. Aggregate completion counts are not a
// correctness boundary because a later send could otherwise hide an earlier
// send that remains in flight. The context is only a liveness bound.
func (self *noAckSendTracker) waitThrough(
	ctx context.Context,
	boundary noAckSendBoundary,
) bool {
	complete := func() bool {
		if self.invalid.Load() {
			return false
		}
		for _, entry := range boundary.entries {
			if entry.state.Load() != 2 {
				return false
			}
		}
		return true
	}
	for !complete() && !self.invalid.Load() {
		select {
		case <-ctx.Done():
			return false
		case <-self.progress:
		}
	}
	return complete()
}

// Failure deltas make asynchronous route-write rejection a loud workload
// error rather than an apparent modeled packet loss.
func (self *noAckSendTracker) failures() uint64 {
	return self.failureCount.Load()
}

// Cancellation stops the owner without ever closing its producer-facing
// channel; a late callback therefore returns safely instead of panicking.
func (self *noAckSendTracker) close() {
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
	})
}

// A later completion cannot satisfy a boundary that captured an earlier send.
// The canceled context makes the negative assertion deterministic: the wait
// must fail immediately until the exact held identity completes.
func TestNoAckSendTrackerWaitsForExactCapturedIdentity(t *testing.T) {
	tracker := newNoAckSendTracker()
	defer tracker.close()
	clientId := clientconnect.NewId()
	observer := tracker.newObserver()
	started := func(token uint64) {
		observer(clientconnect.NoAckSendObservation{
			Phase:    clientconnect.NoAckSendPhaseStarted,
			ClientId: clientId,
			Token:    token,
		})
	}
	completed := func(token uint64) {
		observer(clientconnect.NoAckSendObservation{
			Phase:    clientconnect.NoAckSendPhaseCompleted,
			ClientId: clientId,
			Token:    token,
		})
	}

	started(1)
	ctx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	boundary, ok := tracker.boundary(ctx)
	if !ok {
		t.Fatalf("snapshot held boundary: %v", ctx.Err())
	}
	started(2)
	completed(2)
	if _, ok := tracker.boundary(ctx); !ok {
		t.Fatalf("flush later completion: %v", ctx.Err())
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if tracker.waitThrough(canceledCtx, boundary) {
		t.Fatal("later completion satisfied an earlier held send boundary")
	}

	completed(1)
	if !tracker.waitThrough(ctx, boundary) {
		t.Fatalf("exact completion did not satisfy boundary: %v", ctx.Err())
	}
}

// Error disposition precedes the release publication. A waiter cannot pass
// while the completing callback is held at that exact boundary and therefore
// cannot observe a stale failure count after success.
func TestNoAckSendTrackerPublishesFailureBeforeCompletion(t *testing.T) {
	tracker := newNoAckSendTracker()
	defer tracker.close()
	clientId := clientconnect.NewId()
	observer := tracker.newObserver()
	observer(clientconnect.NoAckSendObservation{
		Phase:    clientconnect.NoAckSendPhaseStarted,
		ClientId: clientId,
		Token:    1,
	})
	ctx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	boundary, ok := tracker.boundary(ctx)
	if !ok {
		t.Fatalf("snapshot failure boundary: %v", ctx.Err())
	}
	completionEntered := make(chan struct{})
	releaseCompletion := make(chan struct{})
	tracker.beforeCompletionPublishForTest = func() {
		close(completionEntered)
		<-releaseCompletion
	}
	observer(clientconnect.NoAckSendObservation{
		Phase:    clientconnect.NoAckSendPhaseCompleted,
		ClientId: clientId,
		Token:    1,
		Err:      errors.New("route write failed"),
	})
	<-completionEntered
	if tracker.failures() != 1 {
		t.Fatalf("failure disposition=%d before publication", tracker.failures())
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if tracker.waitThrough(canceledCtx, boundary) {
		t.Fatal("boundary passed before terminal completion publication")
	}
	close(releaseCompletion)
	if !tracker.waitThrough(ctx, boundary) || tracker.failures() != 1 {
		t.Fatalf(
			"published boundary complete=%t failures=%d err=%v",
			tracker.waitThrough(ctx, boundary),
			tracker.failures(),
			ctx.Err(),
		)
	}
}

// A generated Client can retain its logical ClientId while its local NoAck
// token counter restarts. Fresh observer closures keep those lifetimes
// independent and allow completed entries to be retired at each boundary.
func TestNoAckSendTrackerNamespacesRebuiltClientObservers(t *testing.T) {
	tracker := newNoAckSendTracker()
	defer tracker.close()
	clientId := clientconnect.NewId()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completeFirst := func(observer func(clientconnect.NoAckSendObservation)) {
		observer(clientconnect.NoAckSendObservation{
			Phase: clientconnect.NoAckSendPhaseStarted, ClientId: clientId, Token: 1,
		})
		observer(clientconnect.NoAckSendObservation{
			Phase: clientconnect.NoAckSendPhaseCompleted, ClientId: clientId, Token: 1,
		})
	}
	completeFirst(tracker.newObserver())
	first, ok := tracker.boundary(ctx)
	if !ok || !tracker.waitThrough(ctx, first) {
		t.Fatalf("first client-instance boundary failed: %v", ctx.Err())
	}

	secondObserver := tracker.newObserver()
	secondObserver(clientconnect.NoAckSendObservation{
		Phase: clientconnect.NoAckSendPhaseStarted, ClientId: clientId, Token: 1,
	})
	second, ok := tracker.boundary(ctx)
	if !ok {
		t.Fatalf("snapshot rebuilt client boundary: %v", ctx.Err())
	}
	secondObserver(clientconnect.NoAckSendObservation{
		Phase: clientconnect.NoAckSendPhaseCompleted, ClientId: clientId, Token: 1,
	})
	if !tracker.waitThrough(ctx, second) {
		t.Fatalf("rebuilt client-instance boundary failed: %v", ctx.Err())
	}
}

// Tracker shutdown never closes the producer-facing queue. A callback held
// before its send observes cancellation after release and returns without a
// send-on-closed-channel panic or a stranded tracker worker.
func TestNoAckSendTrackerCloseIsSafeWithHeldObserver(t *testing.T) {
	tracker := newNoAckSendTracker()
	observerEntered := make(chan struct{})
	releaseObserver := make(chan struct{})
	tracker.beforeObserverSendForTest = func() {
		close(observerEntered)
		<-releaseObserver
	}
	observer := tracker.newObserver()
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		observer(clientconnect.NoAckSendObservation{
			Phase:    clientconnect.NoAckSendPhaseStarted,
			ClientId: clientconnect.NewId(),
			Token:    1,
		})
	}()
	<-observerEntered
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		tracker.close()
	}()
	<-closeDone
	close(releaseObserver)
	<-observerDone
	if tracker.startedCount.Load() != 0 {
		t.Fatalf("held observer published %d sends after tracker close", tracker.startedCount.Load())
	}
}

// A workload stores its start after connection/NAT setup or warmup has been
// positively completed and all related source/carrier work is joined.
func (self *fullTunPath) setCarrierMeasurementStart(boundary perfvarCarrierBoundary) {
	self.measurementLock.Lock()
	self.carrierMeasurementStart = &boundary
	packFailureFloor := boundary.packFailures
	self.activePackFailureFloor = &packFailureFloor
	// A setup or warmup join can publish an earlier generic end. Starting the
	// measured interval invalidates that end while retaining an explicit end
	// published later by the workload.
	self.carrierMeasurementEnd = nil
	self.carrierFencePackets = 0
	self.measurementLock.Unlock()
}

// Standard workloads pass their start boundary directly to the observer, so
// their failure epoch is activated when that prepared boundary is consumed.
func (self *fullTunPath) setActivePackFailureFloor(boundary perfvarCarrierBoundary) {
	self.measurementLock.Lock()
	packFailureFloor := boundary.packFailures
	self.activePackFailureFloor = &packFailureFloor
	self.measurementLock.Unlock()
}

// Ownership-only tests and setup joins may run without an active measurement.
func (self *fullTunPath) activePackFailureFloorSnapshot() (
	*perfvarPackFailureCounts,
	bool,
	bool,
) {
	self.measurementLock.Lock()
	defer self.measurementLock.Unlock()
	if self.activePackFailureFloor == nil {
		return nil, false, self.allowProviderDatagramPackFailures
	}
	packFailureFloor := *self.activePackFailureFloor
	return &packFailureFloor, true, self.allowProviderDatagramPackFailures
}

// A successful boundary accounts every failure through that exact carrier
// end. Later enclosing boundaries must validate only newer failures instead
// of reclassifying already accepted probe loss after its narrow scope exits.
func (self *fullTunPath) advanceActivePackFailureFloor(
	expected perfvarPackFailureCounts,
	next perfvarPackFailureCounts,
) bool {
	self.measurementLock.Lock()
	defer self.measurementLock.Unlock()
	if self.activePackFailureFloor == nil ||
		*self.activePackFailureFloor != expected {
		return false
	}
	packFailureFloor := next
	self.activePackFailureFloor = &packFailureFloor
	return true
}

// Latency-under-load explicitly measures lossy UDP probes while a TCP bulk
// transfer owns the same route. Only that helper may account provider datagram
// Pack refusals through its attempt/failure sample contract.
func (self *fullTunPath) setAllowProviderDatagramPackFailures(allow bool) bool {
	self.measurementLock.Lock()
	defer self.measurementLock.Unlock()
	previous := self.allowProviderDatagramPackFailures
	self.allowProviderDatagramPackFailures = allow
	return previous
}

// The performance observer consumes a workload-specific start at most once.
func (self *fullTunPath) takeCarrierMeasurementStart() *perfvarCarrierBoundary {
	self.measurementLock.Lock()
	defer self.measurementLock.Unlock()
	boundary := self.carrierMeasurementStart
	self.carrierMeasurementStart = nil
	return boundary
}

// Correctness gates can require a workload-published start before consuming it
// through the shared carrier observer.
func (self *fullTunPath) hasCarrierMeasurementStart() bool {
	self.measurementLock.Lock()
	defer self.measurementLock.Unlock()
	return self.carrierMeasurementStart != nil
}

// UDP stores its end boundary after the positive same-flow application fence;
// the marker count explicitly records that conservative carrier inclusion.
func (self *fullTunPath) setCarrierMeasurementEnd(
	boundary perfvarCarrierBoundary,
	fencePacketCount int,
) {
	self.measurementLock.Lock()
	self.carrierMeasurementEnd = &boundary
	self.carrierFencePackets = fencePacketCount
	self.measurementLock.Unlock()
}

// A generic terminal fixed point freezes its candidate only when the workload
// has not already published a more precise application-fence boundary.
func (self *fullTunPath) setCarrierMeasurementEndIfAbsent(
	boundary perfvarCarrierBoundary,
) {
	self.measurementLock.Lock()
	if self.carrierMeasurementEnd == nil {
		self.carrierMeasurementEnd = &boundary
		self.carrierFencePackets = 0
	}
	self.measurementLock.Unlock()
}

// The performance observer consumes a workload-specific boundary at most once.
func (self *fullTunPath) takeCarrierMeasurementEnd() (*perfvarCarrierBoundary, int) {
	self.measurementLock.Lock()
	defer self.measurementLock.Unlock()
	boundary := self.carrierMeasurementEnd
	fencePacketCount := self.carrierFencePackets
	self.carrierMeasurementEnd = nil
	self.carrierFencePackets = 0
	return boundary, fencePacketCount
}

// Window construction exposes the generated client and its platform carrier.
type observedPlatformTransport struct {
	client    *clientconnect.Client
	transport *clientconnect.PlatformTransport
}

// Every newer generated window replaces both the device route manager and the
// provider's forced destination. A late older callback is a straggler: both
// controllers and the exposed pointer retain the newest locally minted id.
func observeGeneratedDeviceClient(
	deviceClient *atomic.Pointer[clientconnect.Client],
	providerSendRoutes *platformSendRouteController,
	deviceSendRoutes *platformSendRouteController,
	client *clientconnect.Client,
) {
	clientId := client.ClientId()
	if providerSendRoutes != nil {
		providerSendRoutes.observeDestinationId(clientId)
	}
	if deviceSendRoutes != nil {
		deviceSendRoutes.observeRouteManagerForDestination(
			clientId,
			client.RouteManager(),
		)
	}
	// Publish both ordered controller events before exposing the pointer. A
	// verifier that observes this client can then fence behind those events.
	for {
		current := deviceClient.Load()
		if current != nil {
			currentId := current.ClientId()
			if 0 <= bytes.Compare(currentId[:], clientId[:]) {
				return
			}
		}
		if deviceClient.CompareAndSwap(current, client) {
			return
		}
	}
}

// waitForCurrentGeneratedDeviceClient returns a generated client only at a
// linearization point where the callback pointer, retained transport owner,
// and applied device RouteManager all name the same local generation.
func waitForCurrentGeneratedDeviceClient(
	ctx context.Context,
	path *fullTunPath,
	observedTransports *platformTransportOwner,
) (*clientconnect.Client, error) {
	var rejected *clientconnect.Client
	for {
		candidate, err := observedTransports.waitCurrentClientAfter(
			ctx,
			path.deviceClient,
			rejected,
		)
		if err != nil {
			return nil, err
		}
		if !path.deviceSendRoutes.waitForIdle() {
			return nil, errors.New("device send-route controller closed before generated-client fence")
		}
		candidateId := candidate.ClientId()
		candidateRouteManager := candidate.RouteManager()
		consistent := func() bool {
			path.deviceSendRoutes.stateLock.Lock()
			defer path.deviceSendRoutes.stateLock.Unlock()
			return path.deviceClient.Load() == candidate &&
				path.deviceSendRoutes.routeManagerDestinationId == candidateId &&
				path.deviceSendRoutes.routeManager == candidateRouteManager
		}()
		if consistent {
			return candidate, nil
		}
		if path.afterGeneratedClientMismatchForTest != nil {
			path.afterGeneratedClientMismatchForTest()
		}
		rejected = candidate
	}
}

// One readiness trace separates connection establishment, the deliberate
// modeled-path warmup, and the exact request/response exchange.
type fullTunRouteReadinessObservation struct {
	Budget                 time.Duration
	DialDuration           time.Duration
	WarmupDuration         time.Duration
	WriteDuration          time.Duration
	ReadDuration           time.Duration
	ServerAcceptDuration   time.Duration
	ServerRequestDuration  time.Duration
	ServerResponseDuration time.Duration
	ServerStage            int32
	TotalDuration          time.Duration
}

const p2pProbeTraceEventCapacity = 512

// A test-only trace keeps atomic event totals plus a bounded nonblocking tail.
// Production settings leave the observer nil.
type p2pProbeEventTrace struct {
	counts  map[string]*atomic.Int64
	events  chan clientconnect.P2pStreamProbeEvent
	dropped atomic.Int64
}

// Preallocates every event counter so the transport callback only performs a
// read-only map lookup, one atomic add, and a nonblocking channel send.
func newP2pProbeEventTrace() *p2pProbeEventTrace {
	eventTypes := []string{
		clientconnect.P2pStreamProbeEventRouteReady,
		clientconnect.P2pStreamProbeEventRouteCleared,
		clientconnect.P2pStreamProbeEventRequestQueued,
		clientconnect.P2pStreamProbeEventRequestDropped,
		clientconnect.P2pStreamProbeEventRequestReceived,
		clientconnect.P2pStreamProbeEventResponseQueued,
		clientconnect.P2pStreamProbeEventResponseDropped,
		clientconnect.P2pStreamProbeEventResponseReceived,
		clientconnect.P2pStreamProbeEventResponseQueueFull,
		clientconnect.P2pStreamProbeEventResponseMatched,
		clientconnect.P2pStreamProbeEventResponseStale,
		clientconnect.P2pStreamProbeEventReadinessGranted,
		clientconnect.P2pStreamProbeEventReadinessWithdrawn,
		clientconnect.P2pStreamProbeEventCompatibilityBackoff,
	}
	trace := &p2pProbeEventTrace{
		counts: make(map[string]*atomic.Int64, len(eventTypes)),
		events: make(chan clientconnect.P2pStreamProbeEvent, p2pProbeTraceEventCapacity),
	}
	for _, eventType := range eventTypes {
		trace.counts[eventType] = &atomic.Int64{}
	}
	return trace
}

// observe is safe on a transport callback: a full diagnostic tail drops and
// counts the event instead of delaying packet handling.
func (self *p2pProbeEventTrace) observe(event clientconnect.P2pStreamProbeEvent) {
	if count := self.counts[event.Type]; count != nil {
		count.Add(1)
	}
	select {
	case self.events <- event:
	default:
		self.dropped.Add(1)
	}
}

// The failure boundary owns this destructive tail snapshot. Atomic totals
// remain available if another wrapper records the trace afterward.
func (self *p2pProbeEventTrace) snapshot() map[string]any {
	if self == nil {
		return nil
	}
	counts := make(map[string]int64, len(self.counts))
	for eventType, count := range self.counts {
		if value := count.Load(); 0 < value {
			counts[eventType] = value
		}
	}
	events := []clientconnect.P2pStreamProbeEvent{}
	for {
		select {
		case event := <-self.events:
			events = append(events, event)
		default:
			return map[string]any{
				"counts":  counts,
				"events":  events,
				"dropped": self.dropped.Load(),
			}
		}
	}
}

// One immutable snapshot makes destination replacement, forced state, and
// suppression one atomic route-matching decision.
type platformDataSendPolicy struct {
	destinationId       clientconnect.Id
	suppressedStreamIds map[clientconnect.Id]bool
	armed               bool
	suppressed          bool
}

// The harness transport delegates production priority and weight behavior,
// but can stop matching stream data while continuing to match control frames.
type platformDataSendTransport struct {
	clientconnect.Transport
	policy                        *atomic.Pointer[platformDataSendPolicy]
	rejectedDestinationMatchCount *atomic.Uint64
	fallbackViolationCount        *atomic.Uint64
}

// A route-manager probe invokes one test callback from the external
// MatchesSend call made while rematching an existing writer.
type platformRouteManagerLockProbeTransport struct {
	clientconnect.Transport
	beforeMatchesSend func()
}

// The embedded transport retains production matching after the lock probe.
func (self *platformRouteManagerLockProbeTransport) MatchesSend(
	destination clientconnect.TransferPath,
) bool {
	if self.beforeMatchesSend != nil {
		self.beforeMatchesSend()
	}
	return self.Transport.MatchesSend(destination)
}

// Once forced, the provider destination fails closed across P2P disconnects;
// unrelated destinations and the zero-id control destination remain available.
func (self *platformDataSendTransport) MatchesSend(destination clientconnect.TransferPath) bool {
	if !self.Transport.MatchesSend(destination) {
		return false
	}
	policy := self.policy.Load()
	if policy == nil || !policy.armed {
		return true
	}
	targetMatch := destination.DestinationId == policy.destinationId ||
		(destination.StreamId != (clientconnect.Id{}) &&
			policy.suppressedStreamIds[destination.StreamId])
	if targetMatch {
		if !policy.suppressed {
			self.fallbackViolationCount.Add(1)
		}
		self.rejectedDestinationMatchCount.Add(1)
		return false
	}
	return true
}

// Route controllers start one FIFO worker and are safe for concurrent
// connection replacement. Callback producers must stop before CloseAndWait;
// the worker then joins every admitted route mutation.
type platformSendRouteController struct {
	stateLock                     sync.Mutex
	appliedStateLock              sync.Mutex
	routeManager                  *clientconnect.RouteManager
	routes                        map[clientconnect.Transport]clientconnect.Route
	p2pSendRouteCounts            map[platformP2pSendRouteKey]int
	suppressedStreamIds           map[clientconnect.Id]bool
	destinationId                 clientconnect.Id
	routeManagerDestinationId     clientconnect.Id
	policy                        atomic.Pointer[platformDataSendPolicy]
	rejectedDestinationMatchCount atomic.Uint64
	fallbackViolationCount        atomic.Uint64
	disabled                      bool
	appliedRouteManager           *clientconnect.RouteManager
	appliedRoutes                 map[clientconnect.Transport]clientconnect.Route

	eventHead                *platformSendRouteEventNode
	eventTail                atomic.Pointer[platformSendRouteEventNode]
	eventReady               chan struct{}
	closeDone                chan struct{}
	admissionState           atomic.Uint64
	rejectedPublicationCount atomic.Uint64

	beforeEventAdmissionCasForTest func()
	afterEventAdmissionForTest     func()
	beforeEventLinkForTest         func(platformSendRouteEvent)
	afterEventLinkForTest          func(platformSendRouteEvent)
	beforeEventApplyForTest        func(platformSendRouteEvent)
	afterEventApplyForTest         func(platformSendRouteEvent)
	afterAdmissionClosedForTest    func()
}

// A P2P lifecycle can overlap a generated client replacement, so both the
// final peer and stream identify the suppression reference.
type platformP2pSendRouteKey struct {
	peerId   clientconnect.Id
	streamId clientconnect.Id
}

// One queue edge carries a complete immutable mutation or synchronization
// fence. Only the controller worker reads an admitted value.
type platformSendRouteEvent struct {
	kind          platformSendRouteEventKind
	routeManager  *clientconnect.RouteManager
	transport     clientconnect.Transport
	route         clientconnect.Route
	connected     bool
	p2pState      clientconnect.P2pRouteState
	destinationId clientconnect.Id
	disabled      bool
	done          chan struct{}
}

// Event kinds keep callback publications and synchronous commands in one
// total order.
type platformSendRouteEventKind uint8

const (
	platformSendRouteEventPlatformRoute platformSendRouteEventKind = iota + 1
	platformSendRouteEventP2pRoute
	platformSendRouteEventRouteManager
	platformSendRouteEventDestination
	platformSendRouteEventDisabled
	platformSendRouteEventFence

	// The high bit seals admission while the remaining bits count publishers
	// that have linearized but have not linked their event yet.
	platformSendRouteAdmissionClosed    uint64 = 9223372036854775808
	platformSendRouteAdmissionCountMask uint64 = 9223372036854775807
)

// A lock-free multi-producer link retains every admitted event. A producer
// may pause after swapping the tail; the consumer cannot pass that gap.
type platformSendRouteEventNode struct {
	event platformSendRouteEvent
	next  atomic.Pointer[platformSendRouteEventNode]
}

// A nonzero count means callback ownership outlived the producer join that
// must precede controller shutdown.
type platformSendRouteRejectedPublicationError struct {
	count uint64
}

// The exact count identifies how many callbacks violated close admission.
func (self *platformSendRouteRejectedPublicationError) Error() string {
	return fmt.Sprintf(
		"platform route controller rejected %d publications after close",
		self.count,
	)
}

// platformSendRouteDiagnostic captures the exact synchronous state used to
// explain a forced-route mismatch without relying on later log sampling.
type platformSendRouteDiagnostic struct {
	disabled                      bool
	destinationId                 clientconnect.Id
	policy                        platformDataSendPolicy
	hasPolicy                     bool
	hasLiveEndpointP2pRoute       bool
	routeManagerAddress           string
	appliedRouteManagerAddress    string
	rejectedDestinationMatchCount uint64
	fallbackViolationCount        uint64
	p2pSendRoutes                 []string
	observedPlatformRoutes        []string
	appliedPlatformRoutes         []string
	activeWriterRoutes            []string
	inactiveWriterRoutes          []string
}

// String keeps route diagnostics compact while retaining channel identity,
// transport type, transport id, and both destination-match decisions.
func (self platformSendRouteDiagnostic) String() string {
	return fmt.Sprintf(
		"disabled=%t destination=%s policy=%+v has_policy=%t has_live_endpoint_p2p=%t "+
			"route_manager=%s applied_route_manager=%s rejected=%d violations=%d "+
			"p2p=%v observed=%v applied=%v active=%v inactive=%v",
		self.disabled,
		self.destinationId,
		self.policy,
		self.hasPolicy,
		self.hasLiveEndpointP2pRoute,
		self.routeManagerAddress,
		self.appliedRouteManagerAddress,
		self.rejectedDestinationMatchCount,
		self.fallbackViolationCount,
		self.p2pSendRoutes,
		self.observedPlatformRoutes,
		self.appliedPlatformRoutes,
		self.activeWriterRoutes,
		self.inactiveWriterRoutes,
	)
}

// routeIdentity exposes only an in-process channel address. It is diagnostic
// metadata, not a synchronization primitive or a stable cross-run identity.
func routeIdentity(route clientconnect.Route) string {
	return fmt.Sprintf("%p", route)
}

// Diagnostics copies logical and completed applied state independently, then
// evaluates external transports after releasing both locks. The preceding
// event fence normally aligns the snapshots; concurrent later callbacks may
// advance either view but cannot create a data race.
func (self *platformSendRouteController) diagnostics(
	destination clientconnect.TransferPath,
	writer clientconnect.MultiRouteWriter,
) platformSendRouteDiagnostic {
	self.waitForIdle()
	type transportRoute struct {
		transport clientconnect.Transport
		route     clientconnect.Route
	}
	var observed []transportRoute
	var applied []transportRoute
	diagnostic := platformSendRouteDiagnostic{}
	self.stateLock.Lock()
	diagnostic.disabled = self.disabled
	diagnostic.destinationId = self.destinationId
	diagnostic.hasLiveEndpointP2pRoute = self.hasLiveEndpointP2pRouteWithLock()
	diagnostic.routeManagerAddress = fmt.Sprintf("%p", self.routeManager)
	for key, count := range self.p2pSendRouteCounts {
		diagnostic.p2pSendRoutes = append(
			diagnostic.p2pSendRoutes,
			fmt.Sprintf("peer=%s stream=%s count=%d", key.peerId, key.streamId, count),
		)
	}
	for transport, route := range self.routes {
		observed = append(observed, transportRoute{transport: transport, route: route})
	}
	self.stateLock.Unlock()
	self.appliedStateLock.Lock()
	diagnostic.appliedRouteManagerAddress = fmt.Sprintf("%p", self.appliedRouteManager)
	for transport, route := range self.appliedRoutes {
		applied = append(applied, transportRoute{transport: transport, route: route})
	}
	self.appliedStateLock.Unlock()
	if policy := self.policy.Load(); policy != nil {
		diagnostic.policy = *policy
		diagnostic.hasPolicy = true
	}
	diagnostic.rejectedDestinationMatchCount = self.rejectedDestinationMatchCount.Load()
	diagnostic.fallbackViolationCount = self.fallbackViolationCount.Load()
	describeTransportRoute := func(value transportRoute) string {
		return fmt.Sprintf(
			"type=%T id=%s route=%s destination_match=%t control_match=%t",
			value.transport,
			value.transport.TransportId(),
			routeIdentity(value.route),
			value.transport.MatchesSend(destination),
			value.transport.MatchesSend(clientconnect.DestinationId(clientconnect.ControlId)),
		)
	}
	for _, value := range observed {
		diagnostic.observedPlatformRoutes = append(
			diagnostic.observedPlatformRoutes,
			describeTransportRoute(value),
		)
	}
	for _, value := range applied {
		diagnostic.appliedPlatformRoutes = append(
			diagnostic.appliedPlatformRoutes,
			describeTransportRoute(value),
		)
	}
	for _, route := range writer.GetActiveRoutes() {
		diagnostic.activeWriterRoutes = append(diagnostic.activeWriterRoutes, routeIdentity(route))
	}
	for _, route := range writer.GetInactiveRoutes() {
		diagnostic.inactiveWriterRoutes = append(diagnostic.inactiveWriterRoutes, routeIdentity(route))
	}
	slices.Sort(diagnostic.p2pSendRoutes)
	slices.Sort(diagnostic.observedPlatformRoutes)
	slices.Sort(diagnostic.appliedPlatformRoutes)
	slices.Sort(diagnostic.activeWriterRoutes)
	slices.Sort(diagnostic.inactiveWriterRoutes)
	return diagnostic
}

// Reports whether this single-destination endpoint has a live P2P send route.
// In a multihop stream PeerId is the adjacent hop, not the logical endpoint,
// so the stream identity is the only routing alias shared with the platform.
// The caller holds stateLock.
func (self *platformSendRouteController) hasLiveEndpointP2pRouteWithLock() bool {
	for _, count := range self.p2pSendRouteCounts {
		if 0 < count {
			return true
		}
	}
	return false
}

// Every connection receives a fresh wrapper sharing the controller's
// immutable destination snapshot and the production receive route.
func (self *platformSendRouteController) newTransportPair() (
	sendTransport clientconnect.Transport,
	receiveTransport clientconnect.Transport,
) {
	return &platformDataSendTransport{
		Transport:                     clientconnect.NewSendGatewayTransport(),
		policy:                        &self.policy,
		rejectedDestinationMatchCount: &self.rejectedDestinationMatchCount,
		fallbackViolationCount:        &self.fallbackViolationCount,
	}, clientconnect.NewReceiveGatewayTransport()
}

// Applying a state rematches existing selectors against the same live route.
// Control destinations remain matched while stream data is disabled.
func (self *platformSendRouteController) apply(
	routeManager *clientconnect.RouteManager,
	transport clientconnect.Transport,
	route clientconnect.Route,
	disabled bool,
) {
	if _, ok := transport.(*platformDataSendTransport); ok {
		routeManager.UpdateTransport(transport, []clientconnect.Route{route})
		return
	}
	if disabled {
		routeManager.RemoveTransport(transport)
	} else {
		routeManager.UpdateTransport(transport, []clientconnect.Route{route})
	}
}

// The sole event worker serializes physical route-manager calls. Desired and
// previously applied state are copied under their locks, external calls run
// unlocked, and the completed applied snapshot is then published atomically
// with respect to diagnostics.
func (self *platformSendRouteController) applyCurrentState() {
	var routeManager *clientconnect.RouteManager
	var routes map[clientconnect.Transport]clientconnect.Route
	var disabled bool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		routeManager = self.routeManager
		disabled = self.disabled
		routes = make(map[clientconnect.Transport]clientconnect.Route, len(self.routes))
		for transport, route := range self.routes {
			routes[transport] = route
		}
	}()
	var appliedRouteManager *clientconnect.RouteManager
	var appliedRoutes map[clientconnect.Transport]clientconnect.Route
	func() {
		self.appliedStateLock.Lock()
		defer self.appliedStateLock.Unlock()
		appliedRouteManager = self.appliedRouteManager
		appliedRoutes = self.appliedRoutes
	}()
	if appliedRouteManager != nil {
		for transport := range appliedRoutes {
			_, retained := routes[transport]
			if appliedRouteManager != routeManager || !retained {
				appliedRouteManager.RemoveTransport(transport)
			}
		}
	}
	if routeManager != nil {
		for transport, route := range routes {
			self.apply(routeManager, transport, route, disabled)
		}
	}
	func() {
		self.appliedStateLock.Lock()
		defer self.appliedStateLock.Unlock()
		self.appliedRouteManager = nil
		self.appliedRoutes = nil
		if routeManager != nil {
			self.appliedRouteManager = routeManager
			self.appliedRoutes = routes
		}
	}()
}

// Applies one route-manager replacement from the controller event loop.
func (self *platformSendRouteController) applyRouteManagerEvent(
	routeManager *clientconnect.RouteManager,
	destinationId clientconnect.Id,
) {
	applied := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.routeManagerDestinationId != (clientconnect.Id{}) {
			if destinationId == (clientconnect.Id{}) ||
				bytes.Compare(destinationId[:], self.routeManagerDestinationId[:]) < 0 {
				return
			}
		}
		self.routeManager = routeManager
		self.routeManagerDestinationId = destinationId
		applied = true
	}()
	if applied {
		self.applyCurrentState()
	}
}

// Applies one generated-client identity replacement from the event loop.
func (self *platformSendRouteController) applyDestinationEvent(
	destinationId clientconnect.Id,
) {
	applied := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if bytes.Compare(destinationId[:], self.destinationId[:]) < 0 {
			return
		}
		self.destinationId = destinationId
		if self.disabled {
			self.resetSuppressedStreamIdsWithLock()
		}
		self.publishSuppressedDestinationWithLock()
		applied = true
	}()
	if applied {
		self.applyCurrentState()
	}
}

// Applies one platform route edge from the event loop.
func (self *platformSendRouteController) applyPlatformRouteEvent(
	transport clientconnect.Transport,
	route clientconnect.Route,
	connected bool,
) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if connected {
			self.routes[transport] = route
		} else {
			delete(self.routes, transport)
		}
	}()
	self.applyCurrentState()
}

// Applies one P2P route edge from the event loop.
func (self *platformSendRouteController) applyP2pRouteEvent(
	state clientconnect.P2pRouteState,
) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		key := platformP2pSendRouteKey{
			peerId:   state.PeerId,
			streamId: state.StreamId,
		}
		if state.Connected {
			self.p2pSendRouteCounts[key] += 1
			if self.disabled {
				self.suppressedStreamIds[state.StreamId] = true
			}
		} else if count := self.p2pSendRouteCounts[key]; count <= 1 {
			delete(self.p2pSendRouteCounts, key)
		} else {
			self.p2pSendRouteCounts[key] = count - 1
		}
		self.publishSuppressedDestinationWithLock()
	}()
	self.applyCurrentState()
}

// Publishing swaps one immutable policy so MatchesSend remains lock-free. The
// independent suppressed bit lets the transport count an inconsistent
// fail-open publication while still rejecting provider payload.
func (self *platformSendRouteController) publishSuppressedDestinationWithLock() {
	suppressedStreamIds := make(map[clientconnect.Id]bool, len(self.suppressedStreamIds))
	for streamId := range self.suppressedStreamIds {
		suppressedStreamIds[streamId] = true
	}
	self.policy.Store(&platformDataSendPolicy{
		destinationId:       self.destinationId,
		suppressedStreamIds: suppressedStreamIds,
		armed:               self.disabled,
		suppressed:          self.disabled,
	})
}

// Rebinding the generated destination replaces its alias set with every live
// stream on this single-destination endpoint. A multihop P2P route names its
// adjacent peer, not the final destination, while the authenticated stream id
// is the alias that selects it. Disconnects retain aliases until restoration
// so a route loss cannot silently reopen exchange fallback.
func (self *platformSendRouteController) resetSuppressedStreamIdsWithLock() {
	self.suppressedStreamIds = map[clientconnect.Id]bool{}
	for key, count := range self.p2pSendRouteCounts {
		if 0 < count {
			self.suppressedStreamIds[key.streamId] = true
		}
	}
}

// Applies one forced-route state change from the event loop.
func (self *platformSendRouteController) applyDisabledEvent(disabled bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if disabled && !self.disabled {
			self.resetSuppressedStreamIdsWithLock()
		} else if !disabled {
			self.suppressedStreamIds = map[clientconnect.Id]bool{}
		}
		self.disabled = disabled
		self.publishSuppressedDestinationWithLock()
	}()
	self.applyCurrentState()
}

// A capacity-one wake may coalesce because the linked queue retains every
// event. Producers never wait for the worker.
func (self *platformSendRouteController) notifyEventLoop() {
	select {
	case self.eventReady <- struct{}{}:
	default:
	}
}

// Publication is lock-free and unbounded: once admitted, an event is never
// dropped. Closing rejects new admission and waits for every link in flight.
func (self *platformSendRouteController) publish(event platformSendRouteEvent) bool {
	for {
		admissionState := self.admissionState.Load()
		if admissionState&platformSendRouteAdmissionClosed != 0 {
			self.rejectedPublicationCount.Add(1)
			return false
		}
		if self.beforeEventAdmissionCasForTest != nil {
			self.beforeEventAdmissionCasForTest()
		}
		if self.admissionState.CompareAndSwap(admissionState, admissionState+1) {
			if self.afterEventAdmissionForTest != nil {
				self.afterEventAdmissionForTest()
			}
			break
		}
	}
	node := &platformSendRouteEventNode{event: event}
	previous := self.eventTail.Swap(node)
	if self.beforeEventLinkForTest != nil {
		self.beforeEventLinkForTest(event)
	}
	previous.next.Store(node)
	if self.afterEventLinkForTest != nil {
		self.afterEventLinkForTest(event)
	}
	for {
		admissionState := self.admissionState.Load()
		if admissionState&platformSendRouteAdmissionCountMask == 0 {
			panic("platform route controller publication count underflow")
		}
		if self.admissionState.CompareAndSwap(admissionState, admissionState-1) {
			break
		}
	}
	self.notifyEventLoop()
	return true
}

// A command fence turns all earlier asynchronous callback edges into an exact
// completed boundary for setup and measurement code.
func (self *platformSendRouteController) publishAndWait(event platformSendRouteEvent) bool {
	event.done = make(chan struct{})
	if !self.publish(event) {
		return false
	}
	<-event.done
	return true
}

// The single consumer preserves the tail-swap publication order, including a
// producer paused between swapping the tail and linking its predecessor.
func (self *platformSendRouteController) run() {
	defer close(self.closeDone)
	for {
		if next := self.eventHead.next.Load(); next != nil {
			self.eventHead = next
			event := next.event
			if self.beforeEventApplyForTest != nil {
				self.beforeEventApplyForTest(event)
			}
			switch event.kind {
			case platformSendRouteEventPlatformRoute:
				self.applyPlatformRouteEvent(event.transport, event.route, event.connected)
			case platformSendRouteEventP2pRoute:
				self.applyP2pRouteEvent(event.p2pState)
			case platformSendRouteEventRouteManager:
				self.applyRouteManagerEvent(event.routeManager, event.destinationId)
			case platformSendRouteEventDestination:
				self.applyDestinationEvent(event.destinationId)
			case platformSendRouteEventDisabled:
				self.applyDisabledEvent(event.disabled)
			case platformSendRouteEventFence:
			}
			if self.afterEventApplyForTest != nil {
				self.afterEventApplyForTest(event)
			}
			if event.done != nil {
				close(event.done)
			}
			continue
		}
		admissionState := self.admissionState.Load()
		if admissionState&platformSendRouteAdmissionClosed != 0 &&
			admissionState&platformSendRouteAdmissionCountMask == 0 {
			// A publisher links before decrementing its count. Rechecking the
			// link after observing zero closes the only apparent-empty race.
			if self.eventHead.next.Load() == nil {
				return
			}
			continue
		}
		<-self.eventReady
	}
}

// The route manager can arrive after the generated platform transport starts.
// Setup callers wait for the ordered mutation before continuing.
func (self *platformSendRouteController) setRouteManager(
	routeManager *clientconnect.RouteManager,
) {
	self.publishAndWait(platformSendRouteEvent{
		kind:         platformSendRouteEventRouteManager,
		routeManager: routeManager,
	})
}

// Window callbacks publish a replacement without blocking setup.
func (self *platformSendRouteController) observeRouteManager(
	routeManager *clientconnect.RouteManager,
) {
	self.publish(platformSendRouteEvent{
		kind:         platformSendRouteEventRouteManager,
		routeManager: routeManager,
	})
}

// A generated route-manager replacement carries the same ordered client id as
// its provider destination, so a late older callback cannot regress either.
func (self *platformSendRouteController) observeRouteManagerForDestination(
	destinationId clientconnect.Id,
	routeManager *clientconnect.RouteManager,
) {
	self.publish(platformSendRouteEvent{
		kind:          platformSendRouteEventRouteManager,
		routeManager:  routeManager,
		destinationId: destinationId,
	})
}

// A generated window client has a distinct identity from its parent device.
// Setup waits until the new destination has rematched forced routes.
func (self *platformSendRouteController) setDestinationId(destinationId clientconnect.Id) {
	self.publishAndWait(platformSendRouteEvent{
		kind:          platformSendRouteEventDestination,
		destinationId: destinationId,
	})
}

// A generated-client callback publishes its replacement identity and returns;
// the ordered worker performs every route rematch outside the callback.
func (self *platformSendRouteController) observeDestinationId(destinationId clientconnect.Id) {
	self.publish(platformSendRouteEvent{
		kind:          platformSendRouteEventDestination,
		destinationId: destinationId,
	})
}

// The platform callback only publishes immutable route ownership and returns.
func (self *platformSendRouteController) observe(
	transport clientconnect.Transport,
	route clientconnect.Route,
	connected bool,
) {
	self.publish(platformSendRouteEvent{
		kind:      platformSendRouteEventPlatformRoute,
		transport: transport,
		route:     route,
		connected: connected,
	})
}

// P2P callbacks publish only complete send-route identities. The worker keeps
// the exact overlap refcount and fail-closed policy.
func (self *platformSendRouteController) observeP2pRoute(
	state clientconnect.P2pRouteState,
) {
	if !state.Send || state.StreamId == (clientconnect.Id{}) {
		return
	}
	self.publish(platformSendRouteEvent{
		kind:     platformSendRouteEventP2pRoute,
		p2pState: state,
	})
}

// Readiness is published only after the matching controller event is linked.
// A waiter can therefore use its trace edge before issuing an ordered command.
func observeControlledP2pRoute(
	controller *platformSendRouteController,
	trace *p2pRouteStateTrace,
	state clientconnect.P2pRouteState,
) {
	controller.observeP2pRoute(state)
	trace.Observe(state)
}

// A synchronous state command gives the next measurement boundary an exact
// forced-send state. Restoring before Flush preserves normal teardown.
func (self *platformSendRouteController) setDisabled(disabled bool) {
	self.publishAndWait(platformSendRouteEvent{
		kind:     platformSendRouteEventDisabled,
		disabled: disabled,
	})
}

// Joins all callback events admitted before the fence.
func (self *platformSendRouteController) waitForIdle() bool {
	return self.publishAndWait(platformSendRouteEvent{kind: platformSendRouteEventFence})
}

// Closing admission is idempotent. The worker exits only after every producer
// that passed admission has linked and every queued mutation has applied.
func (self *platformSendRouteController) CloseAndWait(ctx context.Context) error {
	for {
		admissionState := self.admissionState.Load()
		if admissionState&platformSendRouteAdmissionClosed != 0 {
			break
		}
		if self.admissionState.CompareAndSwap(
			admissionState,
			admissionState|platformSendRouteAdmissionClosed,
		) {
			if self.afterAdmissionClosedForTest != nil {
				self.afterAdmissionClosedForTest()
			}
			self.notifyEventLoop()
			break
		}
	}
	select {
	case <-self.closeDone:
		if rejectedPublicationCount := self.rejectedPublicationCount.Load(); rejectedPublicationCount != 0 {
			return &platformSendRouteRejectedPublicationError{
				count: rejectedPublicationCount,
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("join platform route controller: %w", ctx.Err())
	}
}

// verifyForcedDestination proves the current and historical fail-closed
// policy for one provider while independently checking ControlId reachability.
func (self *platformSendRouteController) verifyForcedDestination(
	expectedDestinationId clientconnect.Id,
) error {
	if !self.waitForIdle() {
		return errors.New("platform route controller closed before verification")
	}
	var disabled bool
	var destinationId clientconnect.Id
	var activeP2p bool
	var liveStreamIds []clientconnect.Id
	var platformTransports []*platformDataSendTransport
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		disabled = self.disabled
		destinationId = self.destinationId
		activeP2p = self.hasLiveEndpointP2pRouteWithLock()
		for key, count := range self.p2pSendRouteCounts {
			if 0 < count {
				liveStreamIds = append(liveStreamIds, key.streamId)
			}
		}
		for transport := range self.routes {
			if platformTransport, ok := transport.(*platformDataSendTransport); ok {
				platformTransports = append(platformTransports, platformTransport)
			}
		}
	}()
	policy := self.policy.Load()
	if !disabled || destinationId != expectedDestinationId || policy == nil ||
		!policy.armed || !policy.suppressed || policy.destinationId != expectedDestinationId {
		return fmt.Errorf(
			"forced platform policy disabled=%t destination=%s expected=%s policy=%+v",
			disabled,
			destinationId,
			expectedDestinationId,
			policy,
		)
	}
	if !activeP2p {
		return fmt.Errorf("forced destination %s has no active P2P send route", expectedDestinationId)
	}
	if len(platformTransports) == 0 {
		return fmt.Errorf("forced destination %s has no live platform control transport", expectedDestinationId)
	}
	for _, streamId := range liveStreamIds {
		if !policy.suppressedStreamIds[streamId] {
			return fmt.Errorf(
				"forced destination %s omitted live stream alias %s",
				expectedDestinationId,
				streamId,
			)
		}
	}
	for _, platformTransport := range platformTransports {
		if platformTransport.MatchesSend(clientconnect.DestinationId(expectedDestinationId)) {
			return fmt.Errorf("forced destination %s matched platform payload", expectedDestinationId)
		}
		if !platformTransport.MatchesSend(clientconnect.DestinationId(clientconnect.ControlId)) {
			return fmt.Errorf("forced destination %s removed ControlId platform route", expectedDestinationId)
		}
		for _, streamId := range liveStreamIds {
			if platformTransport.MatchesSend(clientconnect.StreamId(streamId)) {
				return fmt.Errorf(
					"forced destination %s matched platform stream alias %s",
					expectedDestinationId,
					streamId,
				)
			}
		}
	}
	if violationCount := self.fallbackViolationCount.Load(); violationCount != 0 {
		return fmt.Errorf(
			"forced destination %s observed %d fail-open platform matches",
			expectedDestinationId,
			violationCount,
		)
	}
	return nil
}

// Construction starts with production routing enabled.
func newPlatformSendRouteController(destinationId clientconnect.Id) *platformSendRouteController {
	eventRoot := &platformSendRouteEventNode{}
	controller := &platformSendRouteController{
		routes:              map[clientconnect.Transport]clientconnect.Route{},
		appliedRoutes:       map[clientconnect.Transport]clientconnect.Route{},
		p2pSendRouteCounts:  map[platformP2pSendRouteKey]int{},
		suppressedStreamIds: map[clientconnect.Id]bool{},
		destinationId:       destinationId,
		eventHead:           eventRoot,
		eventReady:          make(chan struct{}, 1),
		closeDone:           make(chan struct{}),
	}
	controller.eventTail.Store(eventRoot)
	controller.policy.Store(&platformDataSendPolicy{destinationId: destinationId})
	go controller.run()
	return controller
}

// Waits for one controller concurrency seam without allowing a deadlocked test
// to hold the package indefinitely.
func waitForPlatformRouteControllerSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatal(message)
	}
}

// Unit fixtures close the worker with a bounded liveness wait and report any
// callback that arrived after ownership ended.
func closePlatformSendRouteController(
	t testing.TB,
	controller *platformSendRouteController,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := controller.CloseAndWait(ctx); err != nil {
		t.Errorf("close platform send route controller: %v", err)
	}
}

// RouteManager rematching runs with neither controller state lock held. The
// synchronous MatchesSend callback probes both locks at the exact external
// call boundary, so a future lock expansion fails without scheduler timing.
func TestPlatformSendRouteControllerCallsRouteManagerWithoutStateLocks(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	destinationId := clientconnect.NewId()
	destination := clientconnect.DestinationId(destinationId)
	controller := newPlatformSendRouteController(destinationId)
	defer closePlatformSendRouteController(t, controller)
	routeManager := clientconnect.NewRouteManager(ctx, "platform controller lock probe")
	controller.setRouteManager(routeManager)
	writer := routeManager.OpenMultiRouteWriter(destination)
	defer routeManager.CloseMultiRouteWriter(writer)

	lockResult := make(chan string, 1)
	var matchOnce sync.Once
	transport := &platformRouteManagerLockProbeTransport{
		Transport: clientconnect.NewSendGatewayTransport(),
		beforeMatchesSend: func() {
			matchOnce.Do(func() {
				if !controller.stateLock.TryLock() {
					lockResult <- "logical state lock"
					return
				}
				controller.stateLock.Unlock()
				if !controller.appliedStateLock.TryLock() {
					lockResult <- "applied state lock"
					return
				}
				controller.appliedStateLock.Unlock()
				lockResult <- ""
			})
		},
	}
	controller.observe(transport, make(clientconnect.Route, 1), true)
	select {
	case lockName := <-lockResult:
		if lockName != "" {
			t.Fatalf("RouteManager callback ran under controller %s", lockName)
		}
	case <-ctx.Done():
		t.Fatalf("wait for RouteManager lock probe: %v", ctx.Err())
	}
	if !controller.waitForIdle() {
		t.Fatal("controller closed before RouteManager lock-probe fence")
	}
}

// Forced P2P suppression leaves unrelated peers and control traffic on the
// platform and fails closed after the final live route disconnects.
func TestPlatformSendRouteControllerMatchesOnlyLiveP2pDestinations(t *testing.T) {
	streamId := clientconnect.NewId()
	peerId := clientconnect.NewId()
	otherPeerId := clientconnect.NewId()
	controller := newPlatformSendRouteController(peerId)
	defer closePlatformSendRouteController(t, controller)
	sendTransport, _ := controller.newTransportPair()
	destination := clientconnect.DestinationId(peerId)
	otherDestination := clientconnect.DestinationId(otherPeerId)
	controlDestination := clientconnect.DestinationId(clientconnect.ControlId)
	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    peerId,
		StreamId:  streamId,
		Send:      true,
		Connected: true,
	})
	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    peerId,
		StreamId:  streamId,
		Send:      true,
		Connected: true,
	})
	controller.setDisabled(true)
	if sendTransport.MatchesSend(destination) {
		t.Fatal("platform still matched a destination with an active P2P send route")
	}
	if !sendTransport.MatchesSend(otherDestination) || !sendTransport.MatchesSend(controlDestination) {
		t.Fatal("platform stopped matching unrelated destination or control traffic")
	}
	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    peerId,
		StreamId:  streamId,
		Send:      true,
		Connected: false,
	})
	if sendTransport.MatchesSend(destination) {
		t.Fatal("one of two live P2P routes removed destination suppression")
	}
	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    peerId,
		StreamId:  streamId,
		Send:      true,
		Connected: false,
	})
	if sendTransport.MatchesSend(destination) {
		t.Fatal("final P2P disconnect restored forbidden platform fallback")
	}
	if !sendTransport.MatchesSend(controlDestination) {
		t.Fatal("final P2P disconnect also removed the ControlId platform route")
	}
	if violationCount := controller.fallbackViolationCount.Load(); violationCount != 0 {
		t.Fatalf("forced fallback policy violations=%d", violationCount)
	}
	controller.setDisabled(false)
	if !sendTransport.MatchesSend(destination) {
		t.Fatal("explicit forced-route teardown did not restore platform destination matching")
	}
}

// A final-destination writer is also matched through its authenticated stream
// alias. Forced P2P must remove the platform route for both keys, retain
// ControlId, and stay fail-closed after the P2P route disappears.
func TestPlatformSendRouteControllerSuppressesAuthenticatedStreamAlias(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	peerId := clientconnect.NewId()
	streamId := clientconnect.NewId()
	destination := clientconnect.DestinationId(peerId)
	streamAlias := clientconnect.StreamId(streamId)
	controller := newPlatformSendRouteController(peerId)
	defer closePlatformSendRouteController(t, controller)
	routeManager := clientconnect.NewRouteManager(ctx, "platform stream alias suppression")
	controller.setRouteManager(routeManager)

	platformTransport, _ := controller.newTransportPair()
	platformRoute := make(clientconnect.Route, 1)
	controller.observe(platformTransport, platformRoute, true)
	if !controller.waitForIdle() {
		t.Fatal("controller closed before initial platform route")
	}
	p2pTransport := clientconnect.NewSendClientTransport(destination, streamAlias)
	p2pRoute := make(clientconnect.Route, 1)
	routeManager.UpdateTransport(p2pTransport, []clientconnect.Route{p2pRoute})
	removeAlias := routeManager.AddWriterDestinationAlias(destination, streamAlias)
	defer removeAlias()

	peerWriter := routeManager.OpenMultiRouteWriter(destination)
	defer routeManager.CloseMultiRouteWriter(peerWriter)
	controlWriter := routeManager.OpenMultiRouteWriter(
		clientconnect.DestinationId(clientconnect.ControlId),
	)
	defer routeManager.CloseMultiRouteWriter(controlWriter)
	if len(peerWriter.GetActiveRoutes()) != 2 {
		t.Fatal("test writer did not initially match platform and P2P routes")
	}
	if len(controlWriter.GetActiveRoutes()) != 1 {
		t.Fatal("test ControlId writer did not initially match platform")
	}

	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    peerId,
		StreamId:  streamId,
		Send:      true,
		Connected: true,
	})
	controller.setDisabled(true)
	if len(peerWriter.GetActiveRoutes()) != 1 || peerWriter.GetActiveRoutes()[0] != p2pRoute {
		t.Fatal("authenticated stream alias retained the platform payload route")
	}
	if len(controlWriter.GetActiveRoutes()) != 1 || controlWriter.GetActiveRoutes()[0] != platformRoute {
		t.Fatal("stream alias suppression removed the ControlId platform route")
	}

	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    peerId,
		StreamId:  streamId,
		Send:      true,
		Connected: false,
	})
	routeManager.RemoveTransport(p2pTransport)
	if len(peerWriter.GetActiveRoutes()) != 0 {
		t.Fatal("P2P disconnect reopened platform fallback through the retained stream alias")
	}
	if platformTransport.MatchesSend(streamAlias) {
		t.Fatal("P2P disconnect removed the fail-closed stream-alias policy")
	}
	if len(controlWriter.GetActiveRoutes()) != 1 {
		t.Fatal("P2P disconnect removed ControlId reachability")
	}

	controller.setDisabled(false)
	if len(peerWriter.GetActiveRoutes()) != 1 || peerWriter.GetActiveRoutes()[0] != platformRoute {
		t.Fatal("explicit restoration did not reopen the platform destination route")
	}
}

// A multihop endpoint observes its adjacent peer in P2pRouteState while its
// application writer names the final destination. The authenticated stream
// alias must still remove exchange fallback, remain fail closed after route
// withdrawal, and leave ControlId available until explicit restoration.
func TestPlatformSendRouteControllerSuppressesMultihopStreamAlias(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logicalDestinationId := clientconnect.NewId()
	adjacentPeerId := clientconnect.NewId()
	streamId := clientconnect.NewId()
	destination := clientconnect.DestinationId(logicalDestinationId)
	streamAlias := clientconnect.StreamId(streamId)
	controller := newPlatformSendRouteController(logicalDestinationId)
	defer closePlatformSendRouteController(t, controller)
	routeManager := clientconnect.NewRouteManager(ctx, "multihop stream alias suppression")
	controller.setRouteManager(routeManager)

	platformTransport, _ := controller.newTransportPair()
	platformRoute := make(clientconnect.Route, 1)
	controller.observe(platformTransport, platformRoute, true)
	if !controller.waitForIdle() {
		t.Fatal("controller closed before initial multihop platform route")
	}
	p2pTransport := clientconnect.NewSendClientTransport(streamAlias)
	p2pRoute := make(clientconnect.Route, 1)
	routeManager.UpdateTransport(p2pTransport, []clientconnect.Route{p2pRoute})
	removeAlias := routeManager.AddWriterDestinationAlias(destination, streamAlias)
	defer removeAlias()

	destinationWriter := routeManager.OpenMultiRouteWriter(destination)
	defer routeManager.CloseMultiRouteWriter(destinationWriter)
	controlWriter := routeManager.OpenMultiRouteWriter(
		clientconnect.DestinationId(clientconnect.ControlId),
	)
	defer routeManager.CloseMultiRouteWriter(controlWriter)
	if routes := destinationWriter.GetActiveRoutes(); len(routes) != 2 {
		t.Fatalf("initial multihop destination routes=%d, want platform and P2P", len(routes))
	}
	if routes := controlWriter.GetActiveRoutes(); len(routes) != 1 || routes[0] != platformRoute {
		t.Fatal("initial multihop ControlId route was not platform")
	}

	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    adjacentPeerId,
		StreamId:  streamId,
		Send:      true,
		Connected: true,
	})
	controller.setDisabled(true)
	if routes := destinationWriter.GetActiveRoutes(); len(routes) != 1 || routes[0] != p2pRoute {
		t.Fatalf("forced multihop destination routes=%d, want P2P only", len(routes))
	}
	if err := controller.verifyForcedDestination(logicalDestinationId); err != nil {
		t.Fatalf("verify forced multihop destination: %v", err)
	}

	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    adjacentPeerId,
		StreamId:  streamId,
		Send:      true,
		Connected: false,
	})
	routeManager.RemoveTransport(p2pTransport)
	if routes := destinationWriter.GetActiveRoutes(); len(routes) != 0 {
		t.Fatalf("withdrawn multihop destination routes=%d, want fail closed", len(routes))
	}
	if platformTransport.MatchesSend(streamAlias) {
		t.Fatal("multihop route withdrawal reopened the platform stream alias")
	}
	if routes := controlWriter.GetActiveRoutes(); len(routes) != 1 || routes[0] != platformRoute {
		t.Fatal("multihop route withdrawal removed ControlId platform reachability")
	}

	controller.setDisabled(false)
	if routes := destinationWriter.GetActiveRoutes(); len(routes) != 1 || routes[0] != platformRoute {
		t.Fatal("explicit multihop restoration did not reopen the platform route")
	}
}

// Two multihop streams can share one adjacent peer while remaining distinct
// suppression generations. Withdrawing either route removes only its live
// refcount; both historical aliases stay fail closed until explicit restore.
func TestPlatformSendRouteControllerTracksStreamsForOneAdjacentPeerIndependently(
	t *testing.T,
) {
	logicalDestinationId := clientconnect.NewId()
	adjacentPeerId := clientconnect.NewId()
	firstStreamId := clientconnect.NewId()
	secondStreamId := clientconnect.NewId()
	controller := newPlatformSendRouteController(logicalDestinationId)
	defer closePlatformSendRouteController(t, controller)
	platformTransport, _ := controller.newTransportPair()

	observeStream := func(streamId clientconnect.Id, connected bool) {
		controller.observeP2pRoute(clientconnect.P2pRouteState{
			PeerId:    adjacentPeerId,
			StreamId:  streamId,
			Send:      true,
			Connected: connected,
		})
	}
	liveCount := func(streamId clientconnect.Id) int {
		controller.stateLock.Lock()
		defer controller.stateLock.Unlock()
		return controller.p2pSendRouteCounts[platformP2pSendRouteKey{
			peerId:   adjacentPeerId,
			streamId: streamId,
		}]
	}
	firstAlias := clientconnect.StreamId(firstStreamId)
	secondAlias := clientconnect.StreamId(secondStreamId)
	observeStream(firstStreamId, true)
	observeStream(secondStreamId, true)
	controller.setDisabled(true)
	if liveCount(firstStreamId) != 1 || liveCount(secondStreamId) != 1 {
		t.Fatal("same-peer multihop streams did not retain independent live refcounts")
	}
	if platformTransport.MatchesSend(firstAlias) || platformTransport.MatchesSend(secondAlias) {
		t.Fatal("same-peer multihop stream alias remained open on the platform")
	}

	observeStream(firstStreamId, false)
	if !controller.waitForIdle() {
		t.Fatal("controller closed before first same-peer stream withdrawal")
	}
	if liveCount(firstStreamId) != 0 || liveCount(secondStreamId) != 1 {
		t.Fatal("first same-peer stream withdrawal changed the second live refcount")
	}
	if platformTransport.MatchesSend(firstAlias) || platformTransport.MatchesSend(secondAlias) {
		t.Fatal("first withdrawal reopened one of the fail-closed stream aliases")
	}

	observeStream(secondStreamId, false)
	if !controller.waitForIdle() {
		t.Fatal("controller closed before second same-peer stream withdrawal")
	}
	if liveCount(firstStreamId) != 0 || liveCount(secondStreamId) != 0 {
		t.Fatal("final same-peer stream withdrawal retained a live refcount")
	}
	if platformTransport.MatchesSend(firstAlias) || platformTransport.MatchesSend(secondAlias) {
		t.Fatal("final same-peer withdrawal reopened a historical stream alias")
	}
	controller.setDisabled(false)
	if !platformTransport.MatchesSend(firstAlias) || !platformTransport.MatchesSend(secondAlias) {
		t.Fatal("explicit restore did not reopen both same-peer stream aliases")
	}
}

// Provider suppression follows the generated window client identity. The
// parent device identity remains available on the platform route.
func TestPlatformSendRouteControllerUsesDerivedClientDestination(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	parentClientId := clientconnect.NewId()
	derivedClientId := clientconnect.NewId()
	streamId := clientconnect.NewId()
	controller := newPlatformSendRouteController(parentClientId)
	defer closePlatformSendRouteController(t, controller)

	routeManager := clientconnect.NewRouteManager(ctx, "derived destination")
	controller.setRouteManager(routeManager)
	sendTransport, _ := controller.newTransportPair()
	route := make(clientconnect.Route, 1)
	controller.observe(sendTransport, route, true)
	if !controller.waitForIdle() {
		t.Fatal("controller closed before initial platform route")
	}
	routeManager.UpdateTransport(sendTransport, []clientconnect.Route{route})
	localConnection, remoteConnection := net.Pipe()
	defer localConnection.Close()
	defer remoteConnection.Close()
	p2pCtx, p2pCancel := context.WithCancel(ctx)
	defer p2pCancel()
	p2pTransport, p2pRoute := clientconnect.NewP2pSendTransportForPeer(
		p2pCtx,
		p2pCancel,
		localConnection,
		derivedClientId,
		streamId,
		clientconnect.DefaultP2pTransportSettings(),
	)
	p2pLifecycle, ok := p2pTransport.(clientconnect.P2pRouteLifecycle)
	if !ok {
		t.Fatalf("low-level P2P transport type=%T has no public lifecycle", p2pTransport)
	}
	routeManager.UpdateTransport(p2pTransport, []clientconnect.Route{p2pRoute})
	defer func() {
		routeManager.RemoveTransport(p2pTransport)
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := p2pLifecycle.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close low-level P2P send transport: %v", err)
		}
	}()

	parentWriter := routeManager.OpenMultiRouteWriter(clientconnect.DestinationId(parentClientId))
	defer routeManager.CloseMultiRouteWriter(parentWriter)
	derivedWriter := routeManager.OpenMultiRouteWriter(clientconnect.DestinationId(derivedClientId))
	defer routeManager.CloseMultiRouteWriter(derivedWriter)
	if len(parentWriter.GetActiveRoutes()) != 1 || len(derivedWriter.GetActiveRoutes()) != 2 {
		t.Fatal("P2P route did not match only the generated client identity")
	}

	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    derivedClientId,
		StreamId:  streamId,
		Send:      true,
		Connected: true,
	})
	controller.setDisabled(true)
	if len(derivedWriter.GetActiveRoutes()) != 2 {
		t.Fatal("stale parent identity suppressed the generated client route")
	}
	controller.setDestinationId(derivedClientId)
	if len(derivedWriter.GetActiveRoutes()) != 1 {
		t.Fatal("forced generated-client route did not retain exactly P2P")
	}
	if len(parentWriter.GetActiveRoutes()) != 1 {
		t.Fatal("generated client suppression also removed the parent device route")
	}
}

// The generated-window callback rebases provider suppression on every fresh
// client identity without waiting for the route worker that applies the change.
func TestGeneratedDeviceClientReplacementPublishesProviderDestinationNonblocking(t *testing.T) {
	oldClientId := clientconnect.NewId()
	newClientId := clientconnect.NewId()
	streamId := clientconnect.NewId()
	providerSendRoutes := newPlatformSendRouteController(oldClientId)
	defer closePlatformSendRouteController(t, providerSendRoutes)
	deviceSendRoutes := newPlatformSendRouteController(clientconnect.NewId())
	defer closePlatformSendRouteController(t, deviceSendRoutes)
	providerSendRoutes.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    newClientId,
		StreamId:  streamId,
		Send:      true,
		Connected: true,
	})
	providerSendRoutes.setDisabled(true)
	sendTransport, _ := providerSendRoutes.newTransportPair()
	if !sendTransport.MatchesSend(clientconnect.DestinationId(newClientId)) {
		t.Fatal("stale destination unexpectedly suppressed replacement before callback")
	}

	deviceClient := &atomic.Pointer[clientconnect.Client]{}
	linkEntered := make(chan struct{})
	releaseLink := make(chan struct{})
	var linkOnce sync.Once
	var releaseLinkOnce sync.Once
	defer releaseLinkOnce.Do(func() { close(releaseLink) })
	providerSendRoutes.beforeEventLinkForTest = func(event platformSendRouteEvent) {
		if event.kind == platformSendRouteEventDestination {
			linkOnce.Do(func() {
				close(linkEntered)
				<-releaseLink
			})
		}
	}
	applyEntered := make(chan struct{})
	releaseApply := make(chan struct{})
	var holdOnce sync.Once
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseApply) })
	providerSendRoutes.beforeEventApplyForTest = func(event platformSendRouteEvent) {
		if event.kind == platformSendRouteEventDestination {
			holdOnce.Do(func() {
				close(applyEntered)
				<-releaseApply
			})
		}
	}
	client := clientconnect.NewClient(
		t.Context(),
		newClientId,
		clientconnect.NewNoContractClientOob(),
		clientconnect.DefaultClientSettings(),
	)
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close replacement client: %v", err)
		}
	}()
	callbackReturned := make(chan struct{})
	go func() {
		observeGeneratedDeviceClient(
			deviceClient,
			providerSendRoutes,
			deviceSendRoutes,
			client,
		)
		close(callbackReturned)
	}()
	waitForPlatformRouteControllerSignal(t, linkEntered, "replacement destination did not reach publication link")
	if deviceClient.Load() != nil {
		t.Fatal("generated client became visible before its provider destination event linked")
	}
	releaseLinkOnce.Do(func() { close(releaseLink) })
	waitForPlatformRouteControllerSignal(t, applyEntered, "replacement destination did not reach route worker")
	waitForPlatformRouteControllerSignal(t, callbackReturned, "generated-client callback blocked on route application")
	releaseOnce.Do(func() { close(releaseApply) })
	if !providerSendRoutes.waitForIdle() || !deviceSendRoutes.waitForIdle() {
		t.Fatal("replacement route controllers closed before callback fence")
	}
	if deviceClient.Load() != client {
		t.Fatal("replacement callback did not publish the generated client")
	}
	if sendTransport.MatchesSend(clientconnect.DestinationId(newClientId)) {
		t.Fatal("provider platform route remained open for replacement destination")
	}
	if !sendTransport.MatchesSend(clientconnect.DestinationId(oldClientId)) {
		t.Fatal("replacement destination continued suppressing retired client identity")
	}
}

// A callback from an older generated window may arrive after its replacement;
// all three published identities must retain the newer local generation.
func TestGeneratedDeviceClientIgnoresLateOlderGeneration(t *testing.T) {
	olderClientId := clientconnect.NewId()
	newerClientId := clientconnect.NewId()
	if 0 <= bytes.Compare(olderClientId[:], newerClientId[:]) {
		t.Fatalf("generated ids are not ordered older=%s newer=%s", olderClientId, newerClientId)
	}
	providerSendRoutes := newPlatformSendRouteController(olderClientId)
	defer closePlatformSendRouteController(t, providerSendRoutes)
	deviceSendRoutes := newPlatformSendRouteController(clientconnect.NewId())
	defer closePlatformSendRouteController(t, deviceSendRoutes)
	newClient := func(clientId clientconnect.Id) *clientconnect.Client {
		return clientconnect.NewClient(
			t.Context(),
			clientId,
			clientconnect.NewNoContractClientOob(),
			clientconnect.DefaultClientSettings(),
		)
	}
	olderClient := newClient(olderClientId)
	newerClient := newClient(newerClientId)
	defer func() {
		closeClient := func(name string, client *clientconnect.Client) {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			if err := client.CloseAndWait(closeCtx); err != nil {
				t.Errorf("close %s generated client: %v", name, err)
			}
		}
		closeClient("older", olderClient)
		closeClient("newer", newerClient)
	}()
	deviceClient := &atomic.Pointer[clientconnect.Client]{}

	observeGeneratedDeviceClient(
		deviceClient,
		providerSendRoutes,
		deviceSendRoutes,
		newerClient,
	)
	observeGeneratedDeviceClient(
		deviceClient,
		providerSendRoutes,
		deviceSendRoutes,
		olderClient,
	)
	if !providerSendRoutes.waitForIdle() || !deviceSendRoutes.waitForIdle() {
		t.Fatal("generated-client controllers closed before stale-generation fence")
	}
	if deviceClient.Load() != newerClient {
		t.Fatal("late older callback replaced the exposed generated client")
	}
	func() {
		providerSendRoutes.stateLock.Lock()
		defer providerSendRoutes.stateLock.Unlock()
		if providerSendRoutes.destinationId != newerClientId {
			t.Fatalf(
				"provider destination=%s, want newer=%s",
				providerSendRoutes.destinationId,
				newerClientId,
			)
		}
	}()
	func() {
		deviceSendRoutes.stateLock.Lock()
		defer deviceSendRoutes.stateLock.Unlock()
		if deviceSendRoutes.routeManager != newerClient.RouteManager() ||
			deviceSendRoutes.routeManagerDestinationId != newerClientId {
			t.Fatalf(
				"device route manager=%p generation=%s, want=%p/%s",
				deviceSendRoutes.routeManager,
				deviceSendRoutes.routeManagerDestinationId,
				newerClient.RouteManager(),
				newerClientId,
			)
		}
	}()
}

// A newer RouteManager event is linked before its callback publishes the
// matching client pointer. Selection must reject the now-drained older manager
// at that exact gap and wait for the retained newer generation.
func TestCurrentGeneratedDeviceClientRejectsRouteManagerAheadOfPointer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	providerId := clientconnect.NewId()
	controller := newPlatformSendRouteController(providerId)
	defer closePlatformSendRouteController(t, controller)
	owner := newPlatformTransportOwner()
	newClient := func() *clientconnect.Client {
		return clientconnect.NewClient(
			ctx,
			clientconnect.NewId(),
			clientconnect.NewNoContractClientOob(),
			clientconnect.DefaultClientSettings(),
		)
	}
	olderClient := newClient()
	newerClient := newClient()
	olderCloser := newBlockingPlatformTransportCloser()
	newerCloser := newBlockingPlatformTransportCloser()
	close(olderCloser.release)
	close(newerCloser.release)
	defer func() {
		if err := owner.closeAndWait(ctx); err != nil {
			t.Errorf("close generated transport owner: %v", err)
		}
	}()
	deviceClient := &atomic.Pointer[clientconnect.Client]{}
	path := &fullTunPath{
		deviceClient:     deviceClient,
		deviceSendRoutes: controller,
	}
	platformTransport, _ := controller.newTransportPair()
	controller.observe(platformTransport, make(clientconnect.Route, 1), true)
	observeGeneratedDeviceClient(deviceClient, nil, controller, olderClient)
	owner.add(olderClient, nil, olderCloser)
	if !controller.waitForIdle() {
		t.Fatal("controller closed before older generated-client fence")
	}

	linkEntered := make(chan struct{})
	releaseLink := make(chan struct{})
	newerApplied := make(chan struct{})
	var linkOnce sync.Once
	var releaseOnce sync.Once
	var applyOnce sync.Once
	controller.afterEventLinkForTest = func(event platformSendRouteEvent) {
		if event.kind == platformSendRouteEventRouteManager &&
			event.destinationId == newerClient.ClientId() {
			linkOnce.Do(func() {
				close(linkEntered)
				<-releaseLink
			})
		}
	}
	controller.afterEventApplyForTest = func(event platformSendRouteEvent) {
		if event.kind == platformSendRouteEventRouteManager &&
			event.destinationId == newerClient.ClientId() {
			applyOnce.Do(func() { close(newerApplied) })
		}
	}
	callbackDone := make(chan struct{})
	go func() {
		defer close(callbackDone)
		observeGeneratedDeviceClient(deviceClient, nil, controller, newerClient)
		owner.add(newerClient, nil, newerCloser)
	}()
	defer func() {
		releaseOnce.Do(func() { close(releaseLink) })
		select {
		case <-callbackDone:
		case <-ctx.Done():
			t.Errorf("join held generated-client callback: %v", ctx.Err())
		}
	}()
	waitForPlatformRouteControllerSignal(t, linkEntered, "newer RouteManager event did not reach link barrier")
	controller.notifyEventLoop()
	waitForPlatformRouteControllerSignal(t, newerApplied, "newer RouteManager event was not applied")
	if deviceClient.Load() != olderClient {
		t.Fatal("held callback published the newer pointer before link release")
	}
	retiredWriter := olderClient.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(providerId),
	)
	retiredObserver := clientconnect.TestingObserveMultiRouteWriterRouteState(retiredWriter)
	retiredState := retiredObserver.Snapshot()
	retiredObserver.Close()
	olderClient.RouteManager().CloseMultiRouteWriter(retiredWriter)
	if retiredState.ActiveRouteCount != 0 || retiredState.Generation != 0 {
		t.Fatalf("retired RouteManager state=%+v, want zero routes at generation zero", retiredState)
	}

	mismatchObserved := make(chan struct{})
	var mismatchOnce sync.Once
	path.afterGeneratedClientMismatchForTest = func() {
		mismatchOnce.Do(func() { close(mismatchObserved) })
	}
	type selectionResult struct {
		client *clientconnect.Client
		err    error
	}
	selectionDone := make(chan selectionResult, 1)
	go func() {
		client, err := waitForCurrentGeneratedDeviceClient(ctx, path, owner)
		selectionDone <- selectionResult{client: client, err: err}
	}()
	waitForPlatformRouteControllerSignal(t, mismatchObserved, "selection did not reject the drained older RouteManager")
	releaseOnce.Do(func() { close(releaseLink) })
	waitForPlatformRouteControllerSignal(t, callbackDone, "newer generated-client callback did not complete")
	select {
	case result := <-selectionDone:
		if result.err != nil {
			t.Fatalf("select current generated client: %v", result.err)
		}
		if result.client != newerClient {
			t.Fatal("selection returned the retired client instead of the newer generation")
		}
	case <-ctx.Done():
		t.Fatalf("wait for current generated-client selection: %v", ctx.Err())
	}
}

// Once a generated RouteManager establishes an ordered generation, an
// unversioned event cannot erase that barrier and admit an older straggler.
func TestPlatformSendRouteControllerRejectsUnversionedGenerationRegression(t *testing.T) {
	controller := newPlatformSendRouteController(clientconnect.NewId())
	defer closePlatformSendRouteController(t, controller)
	olderId := clientconnect.NewId()
	newerId := clientconnect.NewId()
	olderRouteManager := clientconnect.NewRouteManager(t.Context(), "older generated client")
	newerRouteManager := clientconnect.NewRouteManager(t.Context(), "newer generated client")
	controller.observeRouteManagerForDestination(newerId, newerRouteManager)
	controller.observeRouteManager(nil)
	controller.observeRouteManagerForDestination(olderId, olderRouteManager)
	if !controller.waitForIdle() {
		t.Fatal("controller closed before generation-regression fence")
	}
	controller.stateLock.Lock()
	defer controller.stateLock.Unlock()
	if controller.routeManager != newerRouteManager ||
		controller.routeManagerDestinationId != newerId {
		t.Fatalf(
			"route manager=%p generation=%s, want newest=%p/%s",
			controller.routeManager,
			controller.routeManagerDestinationId,
			newerRouteManager,
			newerId,
		)
	}
}

// Every production callback returns after queue admission even while route
// application is held at an exact worker barrier.
func TestPlatformSendRouteControllerCallbacksReturnWhileApplyBlocked(t *testing.T) {
	controller := newPlatformSendRouteController(clientconnect.NewId())
	defer closePlatformSendRouteController(t, controller)
	applyEntered := make(chan struct{})
	releaseApply := make(chan struct{})
	var held atomic.Bool
	controller.beforeEventApplyForTest = func(event platformSendRouteEvent) {
		if held.CompareAndSwap(false, true) {
			close(applyEntered)
			<-releaseApply
		}
	}

	platformReturned := make(chan struct{})
	go func() {
		controller.observe(
			clientconnect.NewSendGatewayTransport(),
			make(clientconnect.Route, 1),
			true,
		)
		close(platformReturned)
	}()
	waitForPlatformRouteControllerSignal(t, applyEntered, "worker did not enter apply barrier")
	waitForPlatformRouteControllerSignal(t, platformReturned, "platform callback blocked on apply")

	routeManagerReturned := make(chan struct{})
	go func() {
		controller.observeRouteManager(nil)
		close(routeManagerReturned)
	}()
	waitForPlatformRouteControllerSignal(t, routeManagerReturned, "route-manager callback blocked on apply")

	p2pReturned := make(chan struct{})
	go func() {
		controller.observeP2pRoute(clientconnect.P2pRouteState{
			PeerId:    clientconnect.NewId(),
			StreamId:  clientconnect.NewId(),
			Send:      true,
			Connected: true,
		})
		close(p2pReturned)
	}()
	waitForPlatformRouteControllerSignal(t, p2pReturned, "P2P callback blocked on apply")
	close(releaseApply)
	if !controller.waitForIdle() {
		t.Fatal("controller closed before callback fence")
	}
}

// The readiness trace cannot wake setup in the gap between a production route
// edge and its controller publication.
func TestPlatformSendRouteControllerLinksBeforeReadinessTrace(t *testing.T) {
	controller := newPlatformSendRouteController(clientconnect.NewId())
	defer closePlatformSendRouteController(t, controller)
	trace := newP2pRouteStateTrace()
	linkCompleted := make(chan struct{})
	releaseLink := make(chan struct{})
	var held atomic.Bool
	controller.afterEventLinkForTest = func(event platformSendRouteEvent) {
		if event.kind == platformSendRouteEventP2pRoute && held.CompareAndSwap(false, true) {
			close(linkCompleted)
			<-releaseLink
		}
	}
	state := clientconnect.P2pRouteState{
		PeerId:    clientconnect.NewId(),
		StreamId:  clientconnect.NewId(),
		Send:      true,
		Connected: true,
	}
	callbackReturned := make(chan struct{})
	go func() {
		observeControlledP2pRoute(controller, trace, state)
		close(callbackReturned)
	}()
	waitForPlatformRouteControllerSignal(t, linkCompleted, "controller event did not link")
	if snapshot := trace.Snapshot(); snapshot.Generation != 0 {
		t.Fatalf("readiness published before controller link returned: %+v", snapshot)
	}
	close(releaseLink)
	waitForPlatformRouteControllerSignal(t, callbackReturned, "controlled route callback did not return")
	if snapshot := trace.Snapshot(); snapshot.Generation != 1 || snapshot.ActiveSendRoutes != 1 {
		t.Fatalf("readiness after controller publication=%+v", snapshot)
	}
	if !controller.waitForIdle() {
		t.Fatal("controller closed before controlled-route fence")
	}
	controller.stateLock.Lock()
	retainedCount := controller.p2pSendRouteCounts[platformP2pSendRouteKey{
		peerId:   state.PeerId,
		streamId: state.StreamId,
	}]
	controller.stateLock.Unlock()
	if retainedCount != 1 {
		t.Fatalf("controlled route refcount=%d, want 1", retainedCount)
	}
}

// A producer paused after its tail swap cannot let a later producer overtake
// it. Both callbacks return once their own immutable link is complete.
func TestPlatformSendRouteControllerPreservesFifoAcrossPublicationGap(t *testing.T) {
	controller := newPlatformSendRouteController(clientconnect.NewId())
	defer closePlatformSendRouteController(t, controller)
	firstTransport := clientconnect.NewSendGatewayTransport()
	secondTransport := clientconnect.NewSendGatewayTransport()
	firstLinkEntered := make(chan struct{})
	releaseFirstLink := make(chan struct{})
	var held atomic.Bool
	controller.beforeEventLinkForTest = func(event platformSendRouteEvent) {
		if event.transport == firstTransport && held.CompareAndSwap(false, true) {
			close(firstLinkEntered)
			<-releaseFirstLink
		}
	}
	appliedTransports := []clientconnect.Transport{}
	controller.afterEventApplyForTest = func(event platformSendRouteEvent) {
		if event.kind == platformSendRouteEventPlatformRoute {
			appliedTransports = append(appliedTransports, event.transport)
		}
	}

	firstReturned := make(chan struct{})
	go func() {
		controller.observe(firstTransport, make(clientconnect.Route, 1), true)
		close(firstReturned)
	}()
	waitForPlatformRouteControllerSignal(t, firstLinkEntered, "first publisher did not enter link barrier")
	secondReturned := make(chan struct{})
	go func() {
		controller.observe(secondTransport, make(clientconnect.Route, 1), true)
		close(secondReturned)
	}()
	waitForPlatformRouteControllerSignal(t, secondReturned, "later publisher blocked behind queue gap")
	close(releaseFirstLink)
	waitForPlatformRouteControllerSignal(t, firstReturned, "first publisher did not complete its link")
	if !controller.waitForIdle() {
		t.Fatal("controller closed before FIFO fence")
	}
	if len(appliedTransports) != 2 ||
		appliedTransports[0] != firstTransport || appliedTransports[1] != secondTransport {
		t.Fatalf("applied transport order=%v, want first then second", appliedTransports)
	}
}

// The queue has no finite capacity. A burst larger than 64 Ki events remains
// fully retained while the sole consumer is held.
func TestPlatformSendRouteControllerRetainsBurstWithoutDrops(t *testing.T) {
	controller := newPlatformSendRouteController(clientconnect.NewId())
	defer closePlatformSendRouteController(t, controller)
	applyEntered := make(chan struct{})
	releaseApply := make(chan struct{})
	var held atomic.Bool
	controller.beforeEventApplyForTest = func(event platformSendRouteEvent) {
		if event.kind == platformSendRouteEventP2pRoute && held.CompareAndSwap(false, true) {
			close(applyEntered)
			<-releaseApply
		}
	}
	var appliedP2pEventCount atomic.Int64
	controller.afterEventApplyForTest = func(event platformSendRouteEvent) {
		if event.kind == platformSendRouteEventP2pRoute {
			appliedP2pEventCount.Add(1)
		}
	}
	primingState := clientconnect.P2pRouteState{
		PeerId:    clientconnect.NewId(),
		StreamId:  clientconnect.NewId(),
		Send:      true,
		Connected: true,
	}
	controller.observeP2pRoute(primingState)
	waitForPlatformRouteControllerSignal(t, applyEntered, "worker did not enter burst barrier")
	burstState := clientconnect.P2pRouteState{
		PeerId:    clientconnect.NewId(),
		StreamId:  clientconnect.NewId(),
		Send:      true,
		Connected: true,
	}
	const burstEventCount = 64*1024 + 1
	for eventIndex := 0; eventIndex < burstEventCount; eventIndex += 1 {
		controller.observeP2pRoute(burstState)
	}
	close(releaseApply)
	if !controller.waitForIdle() {
		t.Fatal("controller closed before burst fence")
	}
	if actualCount := appliedP2pEventCount.Load(); actualCount != burstEventCount+1 {
		t.Fatalf("applied P2P events=%d, want %d", actualCount, burstEventCount+1)
	}
	controller.stateLock.Lock()
	retainedCount := controller.p2pSendRouteCounts[platformP2pSendRouteKey{
		peerId:   burstState.PeerId,
		streamId: burstState.StreamId,
	}]
	controller.stateLock.Unlock()
	if retainedCount != burstEventCount {
		t.Fatalf("retained burst refcount=%d, want %d", retainedCount, burstEventCount)
	}
	if rejectedCount := controller.rejectedPublicationCount.Load(); rejectedCount != 0 {
		t.Fatalf("burst rejected %d admitted events", rejectedCount)
	}
}

// Closing behind an admitted producer waits through its tail-link gap, while
// a callback arriving after admission closes is rejected and counted.
func TestPlatformSendRouteControllerCloseJoinsAdmittedPublisher(t *testing.T) {
	controller := newPlatformSendRouteController(clientconnect.NewId())
	linkEntered := make(chan struct{})
	releaseLink := make(chan struct{})
	var held atomic.Bool
	controller.beforeEventLinkForTest = func(event platformSendRouteEvent) {
		if held.CompareAndSwap(false, true) {
			close(linkEntered)
			<-releaseLink
		}
	}
	admittedState := clientconnect.P2pRouteState{
		PeerId:    clientconnect.NewId(),
		StreamId:  clientconnect.NewId(),
		Send:      true,
		Connected: true,
	}
	publisherReturned := make(chan struct{})
	go func() {
		controller.observeP2pRoute(admittedState)
		close(publisherReturned)
	}()
	waitForPlatformRouteControllerSignal(t, linkEntered, "publisher did not enter tail-link gap")
	admissionClosed := make(chan struct{})
	controller.afterAdmissionClosedForTest = func() {
		close(admissionClosed)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- controller.CloseAndWait(ctx)
	}()
	waitForPlatformRouteControllerSignal(t, admissionClosed, "close did not seal admission")
	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    clientconnect.NewId(),
		StreamId:  clientconnect.NewId(),
		Send:      true,
		Connected: true,
	})
	close(releaseLink)
	waitForPlatformRouteControllerSignal(t, publisherReturned, "admitted publisher did not finish")
	var closeErr error
	select {
	case closeErr = <-closeResult:
	case <-ctx.Done():
		t.Fatalf("controller close did not join admitted publisher: %v", ctx.Err())
	}
	var rejectedErr *platformSendRouteRejectedPublicationError
	if !errors.As(closeErr, &rejectedErr) || rejectedErr.count != 1 {
		t.Fatalf("close error=%v, want one rejected late publication", closeErr)
	}
	controller.stateLock.Lock()
	retainedCount := controller.p2pSendRouteCounts[platformP2pSendRouteKey{
		peerId:   admittedState.PeerId,
		streamId: admittedState.StreamId,
	}]
	controller.stateLock.Unlock()
	if retainedCount != 1 {
		t.Fatalf("admitted publication refcount=%d, want 1", retainedCount)
	}
}

// Admission and close share one atomic linearization point. A callback held
// before its admission compare-and-swap loses to close and cannot resurrect a
// publisher count or enqueue an event after the worker exits.
func TestPlatformSendRouteControllerCloseWinsBeforePublisherAdmission(t *testing.T) {
	controller := newPlatformSendRouteController(clientconnect.NewId())
	beforeAdmission := make(chan struct{})
	releaseAdmission := make(chan struct{})
	var held atomic.Bool
	controller.beforeEventAdmissionCasForTest = func() {
		if held.CompareAndSwap(false, true) {
			close(beforeAdmission)
			<-releaseAdmission
		}
	}
	var admitted atomic.Bool
	controller.afterEventAdmissionForTest = func() {
		admitted.Store(true)
	}
	state := clientconnect.P2pRouteState{
		PeerId:    clientconnect.NewId(),
		StreamId:  clientconnect.NewId(),
		Send:      true,
		Connected: true,
	}
	publisherReturned := make(chan struct{})
	go func() {
		controller.observeP2pRoute(state)
		close(publisherReturned)
	}()
	waitForPlatformRouteControllerSignal(t, beforeAdmission, "publisher did not enter admission barrier")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := controller.CloseAndWait(ctx); err != nil {
		t.Fatalf("close before publisher admission: %v", err)
	}
	close(releaseAdmission)
	waitForPlatformRouteControllerSignal(t, publisherReturned, "publisher did not observe closed admission")
	if admitted.Load() {
		t.Fatal("publisher passed admission after close linearized")
	}
	admissionState := controller.admissionState.Load()
	if admissionState != platformSendRouteAdmissionClosed {
		t.Fatalf("final admission state=%d, want closed with zero publishers", admissionState)
	}
	controller.stateLock.Lock()
	retainedCount := controller.p2pSendRouteCounts[platformP2pSendRouteKey{
		peerId:   state.PeerId,
		streamId: state.StreamId,
	}]
	controller.stateLock.Unlock()
	if retainedCount != 0 {
		t.Fatalf("post-close publication refcount=%d, want 0", retainedCount)
	}
	var rejectedErr *platformSendRouteRejectedPublicationError
	if err := controller.CloseAndWait(ctx); !errors.As(err, &rejectedErr) || rejectedErr.count != 1 {
		t.Fatalf("repeated close error=%v, want one rejected publication", err)
	}
}

// The production generator still mints, configures, and tears down the window
// client; only provider enumeration is pinned to the explicit stream path.
type fixedMultiHopApiGenerator struct {
	*clientconnect.ApiMultiClientGenerator
	destination clientconnect.MultiHopId
}

// Explicit topology paths remain excluded after the window accepts one.
func (self *fixedMultiHopApiGenerator) NextDestinations(
	count int,
	excludeDestinations []clientconnect.MultiHopId,
	rankMode string,
) (map[clientconnect.MultiHopId]clientconnect.DestinationStats, error) {
	_ = rankMode
	if count <= 0 {
		return map[clientconnect.MultiHopId]clientconnect.DestinationStats{}, nil
	}
	for _, excluded := range excludeDestinations {
		if excluded == self.destination {
			return map[clientconnect.MultiHopId]clientconnect.DestinationStats{}, nil
		}
	}
	return map[clientconnect.MultiHopId]clientconnect.DestinationStats{
		self.destination: {},
	}, nil
}

// One explicit path uses the same single quality window as a fixed provider.
func (self *fixedMultiHopApiGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

// Gives race-instrumented route construction one shared wall-clock allowance;
// ordinary correctness runs use zero and retain their production deadlines.
func fullTunRaceInstrumentationAllowance() time.Duration {
	if !perfvarRaceEnabled {
		return 0
	}
	return 2 * fullTunMinimumDirectionalWorkloadTimeout()
}

// A full-TUN endpoint clones the production transport-budget capacity because
// PERFVAR hosts independent device and provider processes in one test process.
// Replacement transports still share their endpoint-local budget.
func newFullTunEndpointPlatformBudget() *clientconnect.PlatformTransportBudget {
	defaults := clientconnect.DefaultPlatformTransportBudget().Stats()
	return clientconnect.NewPlatformTransportBudget(
		defaults.TotalByteCount,
		defaults.MaxTransportCount,
	)
}

// Platform settings place H3 on the TUN. The Auto correctness candidate makes
// H1 and H3 equal-priority routes explicitly; production Auto deliberately
// treats H3 as a fallback and therefore does not keep both routes live.
func fullTunPlatformSettings(
	h3Port int,
	platformMode clientconnect.TransportMode,
	tun *clientconnect.Tun,
	receiveStats *clientconnect.PlatformTransportReceiveStats,
	platformBudget *clientconnect.PlatformTransportBudget,
) *clientconnect.PlatformTransportSettings {
	settings := clientconnect.DefaultPlatformTransportSettings()
	if platformMode == clientconnect.TransportModeAuto {
		settings.ModePreferences = map[clientconnect.TransportMode]int{
			clientconnect.TransportModeH1: 1,
			clientconnect.TransportModeH3: 1,
		}
	}
	settings.ReceiveStats = receiveStats
	settings.PlatformTransportBudget = platformBudget
	if allowance := fullTunRaceInstrumentationAllowance(); 0 < allowance {
		settings.HttpConnectTimeout = max(settings.HttpConnectTimeout, allowance)
		settings.WsHandshakeTimeout = max(settings.WsHandshakeTimeout, allowance)
		settings.QuicConnectTimeout = max(settings.QuicConnectTimeout, allowance)
		settings.QuicHandshakeTimeout = max(settings.QuicHandshakeTimeout, allowance)
		settings.AuthTimeout = max(settings.AuthTimeout, allowance)
		settings.PingTimeout = max(settings.PingTimeout, allowance)
		settings.WriteTimeout = max(settings.WriteTimeout, allowance)
		settings.ReadTimeout = max(settings.ReadTimeout, allowance)
		settings.InactiveDrainTimeout = max(settings.InactiveDrainTimeout, allowance)
	}
	settings.QuicTlsConfig.InsecureSkipVerify = true
	settings.H3Port = h3Port
	settings.DnsPort = 0
	settings.H3PacketConnFactory = func(ctx context.Context) (net.PacketConn, error) {
		return tun.ListenUDP(&net.UDPAddr{
			IP:   net.IP(tun.LocalAddresses()[0].AsSlice()),
			Port: 0,
		})
	}
	return settings
}

// Simulated endpoints must not consume each other's carrier working sets.
func TestFullTunPlatformSettingsUseIndependentEndpointBudgets(t *testing.T) {
	deviceBudget := newFullTunEndpointPlatformBudget()
	providerBudget := newFullTunEndpointPlatformBudget()
	deviceSettings := fullTunPlatformSettings(
		0,
		clientconnect.TransportModeAuto,
		nil,
		nil,
		deviceBudget,
	)
	providerSettings := fullTunPlatformSettings(
		0,
		clientconnect.TransportModeAuto,
		nil,
		nil,
		providerBudget,
	)
	if deviceSettings.PlatformTransportBudget == providerSettings.PlatformTransportBudget {
		t.Fatal("independent full-TUN endpoints shared one platform transport budget")
	}
	for name, settings := range map[string]*clientconnect.PlatformTransportSettings{
		"device":   deviceSettings,
		"provider": providerSettings,
	} {
		workingSet := settings.H1BudgetByteCount + settings.H3BudgetByteCount
		if total := settings.PlatformTransportBudget.Stats().TotalByteCount; total < workingSet {
			t.Errorf("%s platform budget=%d, below Auto working set=%d", name, total, workingSet)
		}
		if h1Priority, h3Priority := settings.ModePreferences[clientconnect.TransportModeH1], settings.ModePreferences[clientconnect.TransportModeH3]; h1Priority == 0 || h1Priority != h3Priority {
			t.Errorf(
				"%s Auto priorities H1=%d H3=%d, want equal nonzero priorities",
				name,
				h1Priority,
				h3Priority,
			)
		}
		if _, ok := settings.ModePreferences[clientconnect.TransportModeH3Dns]; ok {
			t.Errorf("%s Auto preferences unexpectedly enabled H3 DNS", name)
		}
	}
}

// Client settings select exactly one production P2P data plane when requested.
func fullTunClientSettings(
	route fullTunRoute,
	stats *clientconnect.P2pDataPlaneStats,
	noAckSends *noAckSendTracker,
	packSends *sendPackLifecycleTracker,
	logicalDataLaneCount int,
) *clientconnect.ClientSettings {
	settings := clientconnect.DefaultClientSettings()
	settings.SendBufferSettings.LogicalDataLaneCount = logicalDataLaneCount
	if noAckSends != nil {
		settings.SendBufferSettings.NoAckSendObserver = noAckSends.newObserver()
	}
	if packSends != nil {
		settings.SendBufferSettings.SendPackLifecycleObserver = packSends.newObserver()
	}
	settings.ControlPingTimeout = 10 * time.Second
	if allowance := fullTunRaceInstrumentationAllowance(); 0 < allowance {
		settings.ReadTimeout = max(settings.ReadTimeout, allowance)
		settings.BufferTimeout = max(settings.BufferTimeout, allowance)
		settings.ControlPingTimeout = max(settings.ControlPingTimeout, allowance)
		// The detector instruments the nested transfer and Pion workers too.
		// Raising only Client's outer queue deadlines left their production
		// 15-second route writes and stream probes able to retire a healthy
		// generated exit while one instrumented contract acquisition was still
		// running. These are fixture-only liveness guards; exact test ordering
		// continues to come from lifecycle barriers.
		settings.SendBufferSettings.AckTimeout = max(
			settings.SendBufferSettings.AckTimeout,
			allowance,
		)
		settings.SendBufferSettings.WriteTimeout = max(
			settings.SendBufferSettings.WriteTimeout,
			allowance,
		)
		settings.ReceiveBufferSettings.GapTimeout = max(
			settings.ReceiveBufferSettings.GapTimeout,
			allowance,
		)
		settings.ReceiveBufferSettings.IdleTimeout = max(
			settings.ReceiveBufferSettings.IdleTimeout,
			allowance,
		)
		settings.ReceiveBufferSettings.MaxPeerAuditDuration = max(
			settings.ReceiveBufferSettings.MaxPeerAuditDuration,
			allowance,
		)
		settings.ReceiveBufferSettings.WriteTimeout = max(
			settings.ReceiveBufferSettings.WriteTimeout,
			allowance,
		)
		settings.ForwardBufferSettings.WriteTimeout = max(
			settings.ForwardBufferSettings.WriteTimeout,
			allowance,
		)
		p2pSettings := settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings
		p2pSettings.WriteTimeout = max(p2pSettings.WriteTimeout, allowance)
		p2pSettings.ReadTimeout = max(p2pSettings.ReadTimeout, allowance)
		p2pSettings.ConnectTimeout = max(p2pSettings.ConnectTimeout, allowance)
		p2pSettings.AdmissionRetryTimeout = max(
			p2pSettings.AdmissionRetryTimeout,
			allowance,
		)
		p2pSettings.EndToEndProbeTimeout = max(
			p2pSettings.EndToEndProbeTimeout,
			allowance,
		)
		settings.WebRtcSettings.DisconnectedTimeout = max(
			settings.WebRtcSettings.DisconnectedTimeout,
			allowance,
		)
		settings.WebRtcSettings.FailedTimeout = max(
			settings.WebRtcSettings.FailedTimeout,
			allowance,
		)
		settings.WebRtcSettings.SctpNoProgressTimeout = max(
			settings.WebRtcSettings.SctpNoProgressTimeout,
			allowance,
		)
	}
	// A measured bulk run must not stop at the deliberately small one-MiB
	// production opening contract. Contract rollover is covered separately;
	// 256 MiB contains the largest 9-hop, one-second-RTT BDP warmup and the
	// maximum 32 MiB measured payload on one connection.
	settings.ContractManagerSettings.InitialContractTransferByteCount = perfvarPerformanceContractByteCount
	settings.ContractManagerSettings.InitialNetworkPeerContractTransferByteCount = perfvarPerformanceContractByteCount
	settings.ContractManagerSettings.StandardContractTransferByteCount = perfvarPerformanceContractByteCount
	p2pSettings := settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings
	p2pSettings.DataPlaneStats = stats
	switch route {
	case fullTunRouteP2pFast:
		p2pSettings.DataPlaneMode = clientconnect.P2pDataPlaneModeFastOnly
	case fullTunRouteP2pLegacy:
		p2pSettings.DataPlaneMode = clientconnect.P2pDataPlaneModeLegacyOnly
	case fullTunRouteExchangeH1, fullTunRouteExchangeH3, fullTunRouteExchangeAuto:
		// A zero-byte admission budget deterministically refuses every WebRTC
		// association while leaving the platform route available. Without this,
		// an explicitly selected provider on loopback can promote to P2P even
		// when server network-peer announcements are disabled.
		settings.WebRtcSettings.MemoryBudget = clientconnect.NewTransferMemoryBudget(0)
	}
	return settings
}

// Retains production reliability normally while keeping race instrumentation
// from classifying its own route-construction slowdown as a dead provider.
func fullTunMultiClientSettings(path *fullTunPath) *clientconnect.MultiClientSettings {
	settings := clientconnect.DefaultMultiClientSettings()
	if perfvarRaceEnabled {
		allowance := fullTunRouteReadinessTimeout(path)
		settings.PingWriteTimeout = max(settings.PingWriteTimeout, allowance)
		settings.CPingWriteTimeout = max(settings.CPingWriteTimeout, allowance)
		settings.PingTimeout = max(settings.PingTimeout, allowance)
		settings.CPingTimeout = max(settings.CPingTimeout, allowance)
		settings.AckTimeout = max(settings.AckTimeout, allowance)
		settings.BlackholeTimeout = max(settings.BlackholeTimeout, allowance)
		// Receive silence is a production health signal, not a concurrency
		// assertion. Its stats window cannot represent a multi-minute race
		// allowance, so disable it explicitly instead of setting an
		// unreachable duration that only looks bounded.
		settings.BlackholeReceiveTimeout = 0
		settings.BlackholeConnectTimeout = max(
			settings.BlackholeConnectTimeout,
			allowance,
		)
		settings.BlackholeConnectComparativeTimeout = 0
		settings.WindowGeneratorTimeout = max(
			settings.WindowGeneratorTimeout,
			allowance,
		)
		// The initial-ping evaluation belongs to its expand pass. Race
		// instrumentation must enlarge that pass boundary along with PingTimeout;
		// otherwise the correct terminal cleanup cancels a healthy candidate at
		// the unchanged production 15-second deadline.
		settings.WindowExpandTimeout = max(
			settings.WindowExpandTimeout,
			allowance,
		)
		settings.StatsWindowMaxUnhealthyDuration = max(
			settings.StatsWindowMaxUnhealthyDuration,
			allowance,
		)
		settings.StatsWindowWarnUnhealthyDuration = max(
			settings.StatsWindowWarnUnhealthyDuration,
			allowance,
		)
		settings.StatsWindowKeepUnhealthyDuration = max(
			settings.StatsWindowKeepUnhealthyDuration,
			allowance,
		)
		settings.SendStallTimeout = max(settings.SendStallTimeout, allowance)
	}
	return settings
}

// The complete fixture uses one fixed provider to keep route attribution exact.
func newFullTunPath(
	ctx context.Context,
	t testing.TB,
	environment *routeEnvironment,
	route fullTunRoute,
) *fullTunPath {
	return newFullTunPathWithExtender(ctx, t, environment, route, false)
}

// The extender variant keeps the application and provider topology identical
// while forcing each H1 client through one production extender server.
func newFullTunPathWithExtender(
	ctx context.Context,
	t testing.TB,
	environment *routeEnvironment,
	route fullTunRoute,
	useExtender bool,
) *fullTunPath {
	return newFullTunPathWithResources(
		ctx,
		t,
		environment,
		route,
		useExtender,
		defaultTunResourceProfile(),
	)
}

// Resource limits are applied at the application TUN boundary while carrier
// and server defaults remain production-identical.
func newFullTunPathWithResources(
	ctx context.Context,
	t testing.TB,
	environment *routeEnvironment,
	route fullTunRoute,
	useExtender bool,
	resources tunResourceProfile,
) *fullTunPath {
	path, err := tryNewFullTunPathWithResources(
		ctx,
		t,
		environment,
		route,
		useExtender,
		resources,
	)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// Measured scenarios return route-readiness failures so all requested fresh
// repetitions can be recorded. Infrastructure construction failures still use
// the fixture's fatal assertions because no meaningful record can follow.
func tryNewFullTunPathWithResources(
	ctx context.Context,
	t testing.TB,
	environment *routeEnvironment,
	route fullTunRoute,
	useExtender bool,
	resources tunResourceProfile,
) (*fullTunPath, error) {
	return tryNewFullTunPathWithTopology(
		ctx,
		t,
		environment,
		route,
		useExtender,
		resources,
		1,
	)
}

// Extended production streams reuse the complete application-TUN/provider-NAT
// fixture while replacing only the provider destination and adjacent Pion
// networks. One hop retains the established direct fixture unchanged.
func tryNewFullTunPathWithTopology(
	ctx context.Context,
	t testing.TB,
	environment *routeEnvironment,
	route fullTunRoute,
	useExtender bool,
	resources tunResourceProfile,
	p2pHopCount int,
) (*fullTunPath, error) {
	return tryNewFullTunPathWithTopologyHooks(
		ctx,
		t,
		environment,
		route,
		useExtender,
		resources,
		p2pHopCount,
		nil,
	)
}

// The hook-bearing constructor keeps production call sites free of test
// branching while exposing every ownership boundary to deterministic tests.
func tryNewFullTunPathWithTopologyHooks(
	ctx context.Context,
	t testing.TB,
	environment *routeEnvironment,
	route fullTunRoute,
	useExtender bool,
	resources tunResourceProfile,
	p2pHopCount int,
	hooks *fullTunConstructionTestHooks,
) (result *fullTunPath, resultErr error) {
	if useExtender && route != fullTunRouteExchangeH1 {
		return nil, fmt.Errorf("production extenders support exchange H1, not %s", route)
	}
	if p2pHopCount <= 0 || clientconnect.MaxMultihopLength+1 < p2pHopCount {
		return nil, fmt.Errorf(
			"full-TUN P2P hop count=%d is outside 1..%d",
			p2pHopCount,
			clientconnect.MaxMultihopLength+1,
		)
	}
	if 1 < p2pHopCount && route != fullTunRouteP2pFast {
		return nil, fmt.Errorf("extended P2P topology supports p2p-fast, not %s", route)
	}
	path := &fullTunPath{
		t:                            t,
		ctx:                          ctx,
		environment:                  environment,
		route:                        route,
		p2pHopCount:                  p2pHopCount,
		providerPlatformReceiveStats: &clientconnect.PlatformTransportReceiveStats{},
		devicePlatformReceiveStats:   &clientconnect.PlatformTransportReceiveStats{},
		providerH3DatagramStats:      &clientconnect.H3DatagramStats{},
		deviceH3DatagramStats:        &clientconnect.H3DatagramStats{},
	}
	owner := newFullTunConstructionOwner(path)
	defer func() {
		if result != nil {
			return
		}
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rollbackCancel()
		resultErr = errors.Join(resultErr, owner.rollback(rollbackCtx))
	}()
	afterStage := func(stage fullTunConstructionStage) error {
		if hooks == nil || hooks.afterStage == nil {
			return nil
		}
		if err := hooks.afterStage(stage, path); err != nil {
			return fmt.Errorf("full-TUN construction stage %s: %w", stage, err)
		}
		return nil
	}
	providerDeviceId := server.NewId()
	providerClientId := server.NewId()
	deviceDeviceId := server.NewId()
	deviceClientId := server.NewId()
	model.Testing_CreateDevice(
		ctx,
		environment.networkId,
		providerDeviceId,
		providerClientId,
		"perfvar provider",
		"perfvar provider",
	)
	model.Testing_CreateDevice(
		ctx,
		environment.networkId,
		deviceDeviceId,
		deviceClientId,
		"perfvar device",
		"perfvar device",
	)
	networkName := fmt.Sprintf("perfvar-%s", environment.networkId)
	providerJwt := jwt.NewByJwt(
		environment.networkId,
		environment.userId,
		networkName,
		false,
		false,
	).Client(providerDeviceId, providerClientId).Sign()
	deviceJwt := jwt.NewByJwt(
		environment.networkId,
		environment.userId,
		networkName,
		false,
		false,
	).Client(deviceDeviceId, deviceClientId).Sign()

	providerTun, providerStrategy := environment.newClientNodeWithProfileAt(
		useExtender,
		environment.providerAccessProfile,
		environment.providerEdgeName,
	)
	path.providerCarrierTun = providerTun
	if err := afterStage(fullTunConstructionStageProviderCarrierTun); err != nil {
		return nil, err
	}
	deviceCarrierTun, deviceStrategy := environment.newClientNodeWithProfileAt(
		useExtender,
		environment.deviceAccessProfile,
		environment.deviceEdgeName,
	)
	path.deviceCarrierTun = deviceCarrierTun
	deviceCarrierNode, ok := environment.network.nodeNameForTun(deviceCarrierTun)
	if !ok {
		return nil, errors.New("full-TUN device carrier has no simulated network node")
	}
	path.deviceCarrierNode = deviceCarrierNode
	if err := afterStage(fullTunConstructionStageDeviceCarrierTun); err != nil {
		return nil, err
	}
	deviceApiUrl := fmt.Sprintf("http://%s:%d", environment.edgeAddress, environment.apiPort)
	devicePlatformUrl := fmt.Sprintf("wss://%s:%d", environment.edgeAddress, environment.h1Port)
	providerApiUrl := fmt.Sprintf("http://%s:%d", environment.providerEdgeAddress, environment.providerApiPort)
	providerPlatformUrl := fmt.Sprintf("wss://%s:%d", environment.providerEdgeAddress, environment.providerH1Port)

	isP2p := route == fullTunRouteP2pFast || route == fullTunRouteP2pLegacy
	platformMode := clientconnect.TransportModeH1
	switch route {
	case fullTunRouteExchangeH3:
		platformMode = clientconnect.TransportModeH3
	case fullTunRouteExchangeAuto:
		platformMode = clientconnect.TransportModeAuto
	}
	var p2p *p2pNetwork
	var streamP2p *streamP2pNetwork
	var err error
	if isP2p {
		if p2pHopCount == 1 {
			p2p, err = newP2pNetwork(oneHopP2pNetworkProfile(environment.profile))
		} else {
			streamP2p, err = newStreamP2pNetwork(environment.profile, p2pHopCount)
		}
		if err != nil {
			return nil, fmt.Errorf("create full-TUN P2P network: %w", err)
		}
	}
	path.p2pNetwork = p2p
	path.streamP2pNetwork = streamP2p
	if err := afterStage(fullTunConstructionStageP2pNetwork); err != nil {
		return nil, err
	}
	var providerSendRoutes *platformSendRouteController
	var deviceSendRoutes *platformSendRouteController
	var providerProbeTrace *p2pProbeEventTrace
	var deviceProbeTrace *p2pProbeEventTrace
	if isP2p {
		providerSendRoutes = newPlatformSendRouteController(clientconnect.Id(deviceClientId))
		deviceSendRoutes = newPlatformSendRouteController(clientconnect.Id(providerClientId))
		path.providerSendRoutes = providerSendRoutes
		path.deviceSendRoutes = deviceSendRoutes
		path.platformSendRoutes = []*platformSendRouteController{
			providerSendRoutes,
			deviceSendRoutes,
		}
		providerProbeTrace = newP2pProbeEventTrace()
		deviceProbeTrace = newP2pProbeEventTrace()
		if err := afterStage(fullTunConstructionStageSendRouteControllers); err != nil {
			return nil, err
		}
	}

	providerStats := &clientconnect.P2pDataPlaneStats{}
	providerRouteStateTrace := newP2pRouteStateTrace()
	providerNoAckSends := newNoAckSendTracker()
	path.providerNoAckSends = providerNoAckSends
	deviceNoAckSends := newNoAckSendTracker()
	path.deviceNoAckSends = deviceNoAckSends
	providerPackSends := newSendPackLifecycleTracker()
	path.providerPackSends = providerPackSends
	devicePackSends := newSendPackLifecycleTracker()
	path.devicePackSends = devicePackSends
	if err := afterStage(fullTunConstructionStageSourceTrackers); err != nil {
		return nil, err
	}
	providerSettings := fullTunClientSettings(
		route,
		providerStats,
		providerNoAckSends,
		providerPackSends,
		resources.LogicalDataLaneCount,
	)
	if hooks != nil && hooks.configureProviderClientSettings != nil {
		hooks.configureProviderClientSettings(providerSettings)
	}
	if providerSendRoutes != nil {
		providerSettings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings.RouteStateObserver =
			func(state clientconnect.P2pRouteState) {
				observeControlledP2pRoute(
					providerSendRoutes,
					providerRouteStateTrace,
					state,
				)
			}
		providerSettings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings.EndToEndProbeObserver =
			providerProbeTrace.observe
	}
	if p2p != nil {
		providerSettings.WebRtcSettings.IceServerUrls = nil
		providerSettings.WebRtcSettings.Network = p2p.left
	} else if streamP2p != nil {
		providerSettings.WebRtcSettings.IceServerUrls = nil
		providerSettings.WebRtcSettings.Network = streamP2p.nets[p2pHopCount]
	} else {
		providerSettings.WebRtcSettings.UseLoopbackOnlyIceInterfaces = true
	}
	providerOob := clientconnect.NewApiOutOfBandControl(
		ctx,
		providerStrategy,
		providerJwt,
		providerApiUrl,
	)
	providerClient := clientconnect.NewClient(
		ctx,
		clientconnect.Id(providerClientId),
		providerOob,
		providerSettings,
	)
	path.providerClient = providerClient
	path.providerClientId = clientconnect.Id(providerClientId)
	if err := afterStage(fullTunConstructionStageProviderClient); err != nil {
		return nil, err
	}
	providerPlatformBudget := newFullTunEndpointPlatformBudget()
	providerPlatformSettings := fullTunPlatformSettings(
		environment.providerH3Port,
		platformMode,
		providerTun,
		path.providerPlatformReceiveStats,
		providerPlatformBudget,
	)
	providerPlatformSettings.H3DatagramStats = path.providerH3DatagramStats
	if hooks != nil && hooks.configureProviderPlatformSettings != nil {
		hooks.configureProviderPlatformSettings(providerPlatformSettings)
	}
	if providerSendRoutes != nil {
		providerSendRoutes.setRouteManager(providerClient.RouteManager())
		providerPlatformSettings.TransportGenerator = providerSendRoutes.newTransportPair
		providerPlatformSettings.SendRouteObserver = providerSendRoutes.observe
	}
	providerTransport := clientconnect.NewPlatformTransportWithTargetMode(
		ctx,
		providerStrategy,
		providerClient.RouteManager(),
		providerPlatformUrl,
		&clientconnect.ClientAuth{
			ByJwt:      providerJwt,
			InstanceId: clientconnect.NewId(),
			AppVersion: "perfvar",
		},
		platformMode,
		providerPlatformSettings,
	)
	path.providerTransport = providerTransport
	if err := afterStage(fullTunConstructionStageProviderTransport); err != nil {
		return nil, err
	}
	if !waitForPlatform(ctx, providerTransport) {
		return nil, errors.New("full-TUN provider platform did not connect")
	}
	if err := afterStage(fullTunConstructionStageProviderTransportReady); err != nil {
		return nil, err
	}
	providerLocalNat := clientconnect.NewLocalUserNatWithDefaults(
		providerClient.Ctx(),
		providerClientId.String(),
	)
	path.providerLocalNat = providerLocalNat
	if err := afterStage(fullTunConstructionStageProviderLocalNat); err != nil {
		return nil, err
	}
	providerReturns := newProviderReturnSendTracker()
	path.providerReturns = providerReturns
	bridgeSends := newFullTunBridgeSendTracker()
	path.bridgeSends = bridgeSends
	if err := afterStage(fullTunConstructionStageProviderTrackers); err != nil {
		return nil, err
	}
	providerRemoteNatSettings := clientconnect.DefaultRemoteUserNatProviderSettings()
	providerRemoteNatSettings.ReturnSendObserver = providerReturns.observe
	providerRemoteNat := clientconnect.NewRemoteUserNatProvider(
		providerClient,
		providerLocalNat,
		providerRemoteNatSettings,
	)
	path.providerRemoteNat = providerRemoteNat
	if err := afterStage(fullTunConstructionStageProviderRemoteNat); err != nil {
		return nil, err
	}
	if err := setRouteProvide(ctx, providerClient); err != nil {
		return nil, fmt.Errorf("register full-TUN provider: %w", err)
	}
	if err := afterStage(fullTunConstructionStageProviderRegistration); err != nil {
		return nil, err
	}
	intermediaryClients := []*routeClient{}
	pathIds := make([]clientconnect.Id, 0, p2pHopCount)
	if streamP2p != nil {
		for nodeIndex := 1; nodeIndex < p2pHopCount; nodeIndex += 1 {
			intermediary := environment.newClient(
				fmt.Sprintf("full-TUN stream intermediary %d", nodeIndex),
				clientconnect.P2pDataPlaneModeFastOnly,
				streamP2p.nets[nodeIndex],
				false,
			)
			intermediaryClients = append(intermediaryClients, intermediary)
			path.streamP2pClients = intermediaryClients
			if err := afterStage(fullTunConstructionStageStreamIntermediaryClient); err != nil {
				return nil, err
			}
			environment.connectPlatform(intermediary, clientconnect.TransportModeH1)
			if err := afterStage(fullTunConstructionStageStreamIntermediaryTransport); err != nil {
				return nil, err
			}
			if !waitForPlatform(ctx, intermediary.transport) {
				return nil, fmt.Errorf(
					"full-TUN stream intermediary %d platform did not connect",
					nodeIndex,
				)
			}
			if err := setProductionStreamProvide(ctx, intermediary.client); err != nil {
				return nil, fmt.Errorf(
					"full-TUN stream intermediary %d provide: %w",
					nodeIndex,
					err,
				)
			}
			pathIds = append(pathIds, clientconnect.Id(intermediary.clientId))
			if err := afterStage(fullTunConstructionStageStreamIntermediaryReady); err != nil {
				return nil, err
			}
		}
		pathIds = append(pathIds, clientconnect.Id(providerClientId))
	}

	deviceStats := &clientconnect.P2pDataPlaneStats{}
	deviceRouteStateTrace := newP2pRouteStateTrace()
	clientSettingsGenerator := func() *clientconnect.ClientSettings {
		settings := fullTunClientSettings(
			route,
			deviceStats,
			deviceNoAckSends,
			devicePackSends,
			resources.LogicalDataLaneCount,
		)
		if deviceSendRoutes != nil {
			settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings.RouteStateObserver =
				func(state clientconnect.P2pRouteState) {
					observeControlledP2pRoute(
						deviceSendRoutes,
						deviceRouteStateTrace,
						state,
					)
				}
			settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings.EndToEndProbeObserver =
				deviceProbeTrace.observe
		}
		if p2p != nil {
			settings.WebRtcSettings.IceServerUrls = nil
			settings.WebRtcSettings.Network = p2p.right
		} else if streamP2p != nil {
			settings.WebRtcSettings.IceServerUrls = nil
			settings.WebRtcSettings.Network = streamP2p.nets[0]
		} else {
			settings.WebRtcSettings.UseLoopbackOnlyIceInterfaces = true
		}
		if hooks != nil && hooks.configureDeviceClientSettings != nil {
			hooks.configureDeviceClientSettings(settings)
		}
		return settings
	}
	generatorSettings := clientconnect.DefaultApiMultiClientGeneratorSettings()
	generatorSettings.PlatformTransportMode = platformMode
	devicePlatformBudget := newFullTunEndpointPlatformBudget()
	generatorSettings.PlatformTransportSettingsGenerator = func() *clientconnect.PlatformTransportSettings {
		settings := fullTunPlatformSettings(
			environment.h3Port,
			platformMode,
			deviceCarrierTun,
			path.devicePlatformReceiveStats,
			devicePlatformBudget,
		)
		settings.H3DatagramStats = path.deviceH3DatagramStats
		if hooks != nil && hooks.configureDevicePlatformSettings != nil {
			hooks.configureDevicePlatformSettings(settings)
		}
		if deviceSendRoutes != nil {
			settings.TransportGenerator = deviceSendRoutes.newTransportPair
			settings.SendRouteObserver = deviceSendRoutes.observe
		}
		return settings
	}
	observedTransports := newPlatformTransportOwner()
	path.deviceTransports = observedTransports
	if err := afterStage(fullTunConstructionStageDeviceTransportOwner); err != nil {
		return nil, err
	}
	deviceClient := &atomic.Pointer[clientconnect.Client]{}
	path.deviceClient = deviceClient
	generatorSettings.PlatformTransportCreated = func(
		client *clientconnect.Client,
		transport *clientconnect.PlatformTransport,
	) {
		observeGeneratedDeviceClient(
			deviceClient,
			providerSendRoutes,
			deviceSendRoutes,
			client,
		)
		observedTransports.observe(client, transport)
	}
	providerId := clientconnect.Id(providerClientId)
	deviceId := clientconnect.Id(deviceClientId)
	apiGenerator := clientconnect.NewApiMultiClientGenerator(
		ctx,
		[]*clientconnect.ProviderSpec{{ClientId: &providerId}},
		deviceStrategy,
		nil,
		deviceApiUrl,
		deviceJwt,
		devicePlatformUrl,
		"perfvar device",
		"perfvar device",
		"perfvar",
		&deviceId,
		clientSettingsGenerator,
		generatorSettings,
	)
	path.apiGenerator = apiGenerator
	if err := afterStage(fullTunConstructionStageDeviceGenerator); err != nil {
		return nil, err
	}
	var generator clientconnect.MultiClientGenerator = apiGenerator
	if streamP2p != nil {
		destination, destinationErr := clientconnect.NewMultiHopId(pathIds...)
		if destinationErr != nil {
			return nil, fmt.Errorf(
				"create full-TUN %d-hop destination: %w",
				p2pHopCount,
				destinationErr,
			)
		}
		generator = &fixedMultiHopApiGenerator{
			ApiMultiClientGenerator: apiGenerator,
			destination:             destination,
		}
	}
	appSettings := clientconnect.DefaultTunSettingsWithBufferSize(resources.ChannelSize)
	appSettings.Mtu = resolvedFullTunApplicationMtu(environment.profile, resources)
	readinessPath := &fullTunPath{
		environment: environment,
		route:       route,
		p2pHopCount: p2pHopCount,
	}
	appSettings.DialTimeout = fullTunRouteReadinessTimeout(readinessPath)
	applyTunResourceProfile(appSettings, resources)
	appTun, err := clientconnect.CreateTun(ctx, appSettings)
	if err != nil {
		return nil, fmt.Errorf("create application TUN: %w", err)
	}
	path.appTun = appTun
	if err := afterStage(fullTunConstructionStageApplicationTun); err != nil {
		return nil, err
	}
	multiClient := clientconnect.NewRemoteUserNatMultiClient(
		ctx,
		generator,
		func(
			source clientconnect.TransferPath,
			provideMode protocol.ProvideMode,
			ipPath *clientconnect.IpPath,
			packet []byte,
		) {
			_, _ = appTun.Write(packet)
		},
		protocol.ProvideMode_Network,
		fullTunMultiClientSettings(readinessPath),
	)
	path.multiClient = multiClient
	multiClient.SetReceivePacketsCallback(func(
		source clientconnect.TransferPath,
		provideMode protocol.ProvideMode,
		ipPath *clientconnect.IpPath,
		packets [][]byte,
	) {
		_, _ = appTun.WriteBatch(packets)
	})
	if err := afterStage(fullTunConstructionStageMultiClient); err != nil {
		return nil, err
	}
	path.deviceClientId = deviceId
	path.providerStats = providerStats
	path.deviceStats = deviceStats
	path.providerProbeTrace = providerProbeTrace
	path.deviceProbeTrace = deviceProbeTrace
	if streamP2p != nil {
		path.streamP2pStats = append(path.streamP2pStats, deviceStats)
		path.streamP2pRouteTraces = append(path.streamP2pRouteTraces, deviceRouteStateTrace)
		for _, intermediary := range intermediaryClients {
			path.streamP2pStats = append(path.streamP2pStats, intermediary.stats)
			path.streamP2pRouteTraces = append(
				path.streamP2pRouteTraces,
				intermediary.routeStateTrace,
			)
		}
		path.streamP2pStats = append(path.streamP2pStats, providerStats)
		path.streamP2pRouteTraces = append(path.streamP2pRouteTraces, providerRouteStateTrace)
	}
	path.bridgeWaitGroup.Add(1)
	go func() {
		defer path.bridgeWaitGroup.Done()
		packets := make([][]byte, max(1, resources.BatchSize))
		sendPacketBatch := func(packetBatch [][]byte) int {
			return multiClient.SendPacketBatch(
				clientconnect.SourceId(deviceId),
				protocol.ProvideMode_Network,
				packetBatch,
				-1,
			)
		}
		if resources.SingularBridgeSend {
			sendPacketBatch = func(packetBatch [][]byte) int {
				sentPacketCount := 0
				for _, packet := range packetBatch {
					if multiClient.SendPacket(
						clientconnect.SourceId(deviceId),
						protocol.ProvideMode_Network,
						packet,
						-1,
					) {
						sentPacketCount += 1
					} else {
						clientconnect.MessagePoolReturn(packet)
					}
				}
				return sentPacketCount
			}
		}
		for {
			packetCount, readErr := appTun.ReadBatch(packets)
			if readErr != nil {
				return
			}
			sendFullTunBridgeBatch(
				bridgeSends,
				packets[:packetCount],
				resources.AppDelay,
				time.Sleep,
				sendPacketBatch,
			)
		}
	}()
	path.bridgeStarted = true
	if err := afterStage(fullTunConstructionStageBridge); err != nil {
		return nil, err
	}
	if isP2p {
		var primeErr error
		if streamP2p == nil {
			primeErr = primeFullTunP2p(
				ctx,
				path,
				clientconnect.Id(providerClientId),
				observedTransports,
			)
		} else {
			primeErr = primeFullTunStreamP2p(
				ctx,
				path,
				observedTransports,
			)
		}
		if primeErr != nil {
			p2pSnapshot := p2pNetworkSnapshot{}
			if path.p2pNetwork != nil {
				p2pSnapshot = path.p2pNetwork.snapshot()
			}
			setupErr := fmt.Errorf(
				"prime full-TUN %s route: %v; links=%+v p2p=%+v device=%+v provider=%+v device-packets=%+v provider-packets=%+v provider-congestion=%+v device-probes=%+v provider-probes=%+v",
				route,
				primeErr,
				environment.network.snapshotLinks(),
				p2pSnapshot,
				deviceStats.Snapshot(),
				providerStats.Snapshot(),
				multiClient.PacketStats(),
				providerRemoteNat.PacketStats(),
				providerRemoteNat.CongestionDropStats(),
				deviceProbeTrace.snapshot(),
				providerProbeTrace.snapshot(),
			)
			return nil, setupErr
		}
	} else if route == fullTunRouteExchangeAuto {
		if err := primeFullTunAuto(ctx, path, observedTransports); err != nil {
			return nil, fmt.Errorf("prime full-TUN %s route: %w", route, err)
		}
	} else {
		if err := probeFullTunPath(ctx, path); err != nil {
			setupErr := fmt.Errorf(
				"prime full-TUN %s route: %v; links=%+v device=%+v provider=%+v device-packets=%+v provider-packets=%+v provider-congestion=%+v",
				route,
				err,
				environment.network.snapshotLinks(),
				deviceStats.Snapshot(),
				providerStats.Snapshot(),
				multiClient.PacketStats(),
				providerRemoteNat.PacketStats(),
				providerRemoteNat.CongestionDropStats(),
			)
			return nil, setupErr
		}
		observedCtx, observedCancel := context.WithTimeout(ctx, 90*time.Second)
		_, observedErr := observedTransports.waitFirst(observedCtx)
		observedCancel()
		if observedErr != nil {
			return nil, fmt.Errorf("generated exchange client was not observed: %w", observedErr)
		}
	}
	t.Logf(
		"[perfvar] route-readiness-counters route=%s device-packets=%+v provider-packets=%+v provider-congestion=%+v",
		route,
		multiClient.PacketStats(),
		providerRemoteNat.PacketStats(),
		providerRemoteNat.CongestionDropStats(),
	)
	// Every construction branch above completed a bidirectional application
	// probe. Later carrier-idle joins can therefore use it as the positive Pion,
	// NAT, and TUN fence for all preceding setup traffic.
	path.readinessAppFence.Store(true)
	if err := afterStage(fullTunConstructionStageRouteReady); err != nil {
		return nil, err
	}
	return owner.commit(), nil
}

// Auto readiness is stronger than PlatformTransport.IsConnected: both direct
// modes must have published a send route in both directions before a campaign
// can attribute traffic distribution or failover. DNS transports are disabled
// in this direct-carrier fixture and remain a separate availability gate.
func primeFullTunAuto(
	ctx context.Context,
	path *fullTunPath,
	observedTransports *platformTransportOwner,
) error {
	if err := probeFullTunPath(ctx, path); err != nil {
		return err
	}
	if _, err := path.joinSourcePackCarrierBoundary(ctx); err != nil {
		return fmt.Errorf("join Auto discovery probe tail: %w", err)
	}
	observedCtx, observedCancel := context.WithTimeout(ctx, 90*time.Second)
	observedClient, observedErr := observedTransports.waitCurrentClient(
		observedCtx,
		path.deviceClient,
	)
	observedCancel()
	if observedErr != nil {
		return fmt.Errorf("current generated Auto client was not observed: %w", observedErr)
	}
	deviceWriter := observedClient.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(path.providerClientId),
	)
	defer observedClient.RouteManager().CloseMultiRouteWriter(deviceWriter)
	providerWriter := path.providerClient.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(observedClient.ClientId()),
	)
	defer path.providerClient.RouteManager().CloseMultiRouteWriter(providerWriter)
	deviceRoutes := clientconnect.TestingObserveMultiRouteWriterRouteState(deviceWriter)
	defer deviceRoutes.Close()
	providerRoutes := clientconnect.TestingObserveMultiRouteWriterRouteState(providerWriter)
	defer providerRoutes.Close()
	readinessCtx, readinessCancel := context.WithTimeout(
		ctx,
		max(30*time.Second, 20*fullTunOuterRoundTrip(path)),
	)
	defer readinessCancel()
	if err := waitForMinimumRouteCount(readinessCtx, deviceRoutes, 2); err != nil {
		return fmt.Errorf("wait for device H1+H3 Auto routes: %w", err)
	}
	if err := waitForMinimumRouteCount(readinessCtx, providerRoutes, 2); err != nil {
		return fmt.Errorf("wait for provider H1+H3 Auto routes: %w", err)
	}
	return nil
}

// A small exact echo proves the lazy generated client, selected platform, NAT,
// and return path are ready before any measurement boundary.
func probeFullTunPath(
	ctx context.Context,
	path *fullTunPath,
) error {
	probeStartTime := time.Now()
	observation := fullTunRouteReadinessObservation{}
	var serverStage atomic.Int32
	var serverAcceptNanos atomic.Int64
	var serverRequestNanos atomic.Int64
	var serverResponseNanos atomic.Int64
	snapshotServer := func() {
		observation.ServerAcceptDuration = time.Duration(serverAcceptNanos.Load())
		observation.ServerRequestDuration = time.Duration(serverRequestNanos.Load())
		observation.ServerResponseDuration = time.Duration(serverResponseNanos.Load())
		observation.ServerStage = serverStage.Load()
	}
	failure := func(stage string, err error) error {
		snapshotServer()
		observation.TotalDuration = time.Since(probeStartTime)
		return fmt.Errorf(
			"%s after %s: %w; readiness=%+v",
			stage,
			observation.TotalDuration,
			err,
			observation,
		)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return failure("listen for readiness", err)
	}
	defer listener.Close()
	// A multi-segment request proves more than SYN reachability and avoids
	// turning an extreme-RTT measurement into a test of one tiny delayed TCP
	// write. Web-like tiny exchanges are measured as their own workload.
	payload := append([]byte(nil), deterministicPayload()[:fullTunProbePayloadByteCount]...)
	if path.readinessProbePayloadForTest != nil {
		payload = append([]byte(nil), path.readinessProbePayloadForTest...)
	}
	server := newReadinessEchoServer(
		listener,
		payload,
		&readinessEchoServerSettings{
			beforeSuccessfulConnectionClose: path.beforeReadinessServerCloseForTest,
			afterCompleteRequest: func() {
				serverRequestNanos.Store(int64(time.Since(probeStartTime)))
				serverStage.Store(2)
			},
			afterCompleteResponse: func() {
				serverResponseNanos.Store(int64(time.Since(probeStartTime)))
				serverStage.Store(3)
			},
			afterAcceptForTest: func(net.Conn) {
				serverAcceptNanos.CompareAndSwap(0, int64(time.Since(probeStartTime)))
				serverStage.CompareAndSwap(0, 1)
			},
		},
	)
	defer server.CloseAndWait()
	probeTimeout := fullTunRouteReadinessTimeout(path)
	observation.Budget = probeTimeout
	dialStartTime := time.Now()
	dialCtx, dialCancel := context.WithTimeout(ctx, probeTimeout)
	connection, err := path.appTun.DialContext(dialCtx, "tcp", listener.Addr().String())
	dialCancel()
	observation.DialDuration = time.Since(dialStartTime)
	if err != nil {
		return failure("dial readiness path", err)
	}
	if noDelayConnection, ok := connection.(interface{ SetNoDelay(bool) error }); ok {
		if err := noDelayConnection.SetNoDelay(true); err != nil {
			connection.Close()
			return failure("set readiness no-delay", err)
		}
	}
	warmupStartTime := time.Now()
	warmupTimer := time.NewTimer(2 * fullTunOuterRoundTrip(path))
	select {
	case <-warmupTimer.C:
		observation.WarmupDuration = time.Since(warmupStartTime)
	case <-ctx.Done():
		warmupTimer.Stop()
		connection.Close()
		observation.WarmupDuration = time.Since(warmupStartTime)
		return failure("warm readiness path", ctx.Err())
	}
	if err := connection.SetDeadline(time.Now().Add(probeTimeout)); err != nil {
		connection.Close()
		return failure("set readiness deadline", err)
	}
	if path.beforeReadinessClientWriteForTest != nil {
		path.beforeReadinessClientWriteForTest()
	}
	writeStartTime := time.Now()
	if err := writeFullTunAll(connection, payload); err != nil {
		connection.Close()
		observation.WriteDuration = time.Since(writeStartTime)
		return failure("write readiness request", err)
	}
	observation.WriteDuration = time.Since(writeStartTime)
	response := make([]byte, len(payload))
	readStartTime := time.Now()
	if _, err := io.ReadFull(connection, response); err != nil {
		connection.Close()
		observation.ReadDuration = time.Since(readStartTime)
		return failure("read readiness response", err)
	}
	observation.ReadDuration = time.Since(readStartTime)
	connection.Close()
	if !bytes.Equal(response, payload) {
		return failure("verify readiness response", fmt.Errorf("content mismatch"))
	}
	select {
	case err := <-server.result:
		if err != nil {
			return failure("complete readiness server", err)
		}
	case <-ctx.Done():
		return failure("join readiness server", ctx.Err())
	}
	snapshotServer()
	observation.TotalDuration = time.Since(probeStartTime)
	path.readinessObservation = observation
	path.t.Logf("[perfvar] route-readiness route=%s observation=%+v", path.route, observation)
	return nil
}

// The exchange-carried probe triggers peer discovery before forcing P2P.
func primeFullTunP2p(
	ctx context.Context,
	path *fullTunPath,
	providerClientId clientconnect.Id,
	observedTransports *platformTransportOwner,
) error {
	if err := probeFullTunPath(ctx, path); err != nil {
		return err
	}
	// The successful application response does not synchronously finish the
	// inner TCP close. Join its source, Pack, and carrier tail before changing
	// contract policy or disabling the exchange route; otherwise a late FIN or
	// ACK can cross the route transition and lose every accepting writer.
	if _, err := path.joinSourcePackCarrierBoundary(ctx); err != nil {
		return fmt.Errorf("join exchange discovery probe tail: %w", err)
	}
	observedCtx, observedCancel := context.WithTimeout(ctx, 90*time.Second)
	observedClient, observedErr := waitForCurrentGeneratedDeviceClient(
		observedCtx,
		path,
		observedTransports,
	)
	observedCancel()
	if observedErr != nil {
		return fmt.Errorf("current generated platform client was not observed: %w", observedErr)
	}
	writer := observedClient.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(providerClientId),
	)
	defer observedClient.RouteManager().CloseMultiRouteWriter(writer)
	deviceRouteStateObserver := clientconnect.TestingObserveMultiRouteWriterRouteState(writer)
	defer deviceRouteStateObserver.Close()
	path.providerSendRoutes.setDestinationId(observedClient.ClientId())
	providerWriter := path.providerClient.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(observedClient.ClientId()),
	)
	defer path.providerClient.RouteManager().CloseMultiRouteWriter(providerWriter)
	providerRouteStateObserver := clientconnect.TestingObserveMultiRouteWriterRouteState(providerWriter)
	defer providerRouteStateObserver.Close()
	if err := waitForRouteCount(ctx, deviceRouteStateObserver, 2); err != nil {
		return fmt.Errorf("wait for device promotion: %w", err)
	}
	if err := waitForRouteCount(ctx, providerRouteStateObserver, 2); err != nil {
		return fmt.Errorf("wait for provider promotion: %w", err)
	}
	observedClient.ContractManager().AddNoContractPeer(providerClientId)
	path.providerClient.ContractManager().AddNoContractPeer(observedClient.ClientId())
	deviceRouteBefore := path.deviceSendRoutes.diagnostics(
		clientconnect.DestinationId(providerClientId),
		writer,
	)
	providerRouteBefore := path.providerSendRoutes.diagnostics(
		clientconnect.DestinationId(observedClient.ClientId()),
		providerWriter,
	)
	deviceForcedBarrier := deviceRouteStateObserver.Snapshot()
	providerForcedBarrier := providerRouteStateObserver.Snapshot()
	if deviceForcedBarrier.ActiveRouteCount != 2 ||
		providerForcedBarrier.ActiveRouteCount != 2 {
		return fmt.Errorf(
			"forced P2P transition started from device=%+v provider=%+v",
			deviceForcedBarrier,
			providerForcedBarrier,
		)
	}
	for _, sendRoutes := range path.platformSendRoutes {
		sendRoutes.setDisabled(true)
	}
	if _, err := waitForRouteCountAfter(
		ctx,
		deviceRouteStateObserver,
		deviceForcedBarrier,
		1,
	); err != nil {
		return fmt.Errorf("wait for forced device P2P route: %w", err)
	}
	if _, err := waitForRouteCountAfter(
		ctx,
		providerRouteStateObserver,
		providerForcedBarrier,
		1,
	); err != nil {
		return fmt.Errorf("wait for forced provider P2P route: %w", err)
	}
	deviceRouteAfter := path.deviceSendRoutes.diagnostics(
		clientconnect.DestinationId(providerClientId),
		writer,
	)
	providerRouteAfter := path.providerSendRoutes.diagnostics(
		clientconnect.DestinationId(observedClient.ClientId()),
		providerWriter,
	)
	if len(deviceRouteAfter.activeWriterRoutes) != 1 {
		return fmt.Errorf(
			"force device P2P after promotion: route count=%d, expected=1; before={%s}; after={%s}",
			len(deviceRouteAfter.activeWriterRoutes),
			deviceRouteBefore,
			deviceRouteAfter,
		)
	}
	if len(providerRouteAfter.activeWriterRoutes) != 1 {
		return fmt.Errorf(
			"force provider P2P after promotion: route count=%d, expected=1; before={%s}; after={%s}",
			len(providerRouteAfter.activeWriterRoutes),
			providerRouteBefore,
			providerRouteAfter,
		)
	}
	deviceStatsBefore := path.deviceStats.Snapshot()
	providerStatsBefore := path.providerStats.Snapshot()
	if err := probeFullTunPath(ctx, path); err != nil {
		return fmt.Errorf("probe forced one-hop P2P route: %w", err)
	}
	deviceStatsAfter := path.deviceStats.Snapshot()
	providerStatsAfter := path.providerStats.Snapshot()
	deviceDelta := subtractP2pStats(deviceStatsBefore, deviceStatsAfter)
	providerDelta := subtractP2pStats(providerStatsBefore, providerStatsAfter)
	if path.route == fullTunRouteP2pFast {
		if deviceDelta.FastSendMessageCount == 0 || deviceDelta.FastReceiveMessageCount == 0 ||
			providerDelta.FastSendMessageCount == 0 || providerDelta.FastReceiveMessageCount == 0 ||
			deviceDelta.LegacySendMessageCount != 0 || providerDelta.LegacySendMessageCount != 0 {
			return fmt.Errorf(
				"forced fast P2P probe used wrong lane: device=%+v provider=%+v",
				deviceDelta,
				providerDelta,
			)
		}
	} else if deviceDelta.LegacySendMessageCount == 0 || deviceDelta.LegacyReceiveMessageCount == 0 ||
		providerDelta.LegacySendMessageCount == 0 || providerDelta.LegacyReceiveMessageCount == 0 ||
		deviceDelta.FastSendMessageCount != 0 || providerDelta.FastSendMessageCount != 0 {
		return fmt.Errorf(
			"forced legacy P2P probe used wrong lane: device=%+v provider=%+v",
			deviceDelta,
			providerDelta,
		)
	}
	// Carrier queues alone are not an application boundary: the inner TCP
	// close can publish another Pack after they look empty. Join the complete
	// source-to-carrier generation before construction publishes RouteReady.
	if _, err := path.joinSourcePackCarrierBoundary(ctx); err != nil {
		return fmt.Errorf("join forced one-hop P2P probe tail: %w", err)
	}
	return nil
}

// The generated source, every intermediary, and provider use independent
// production WebRTC associations. Every directed adjacency must be active
// before a bidirectional application probe verifies P2P carrier use.
func primeFullTunStreamP2p(
	ctx context.Context,
	path *fullTunPath,
	observedTransports *platformTransportOwner,
) error {
	if err := probeFullTunPath(ctx, path); err != nil {
		return err
	}
	// A stream promotion has the same exchange-carried discovery tail as a
	// one-hop route. Join it before publishing destinations or disabling any
	// platform writer used by that tail.
	if _, err := path.joinSourcePackCarrierBoundary(ctx); err != nil {
		return fmt.Errorf("join exchange stream discovery probe tail: %w", err)
	}
	observedCtx, observedCancel := context.WithTimeout(ctx, 90*time.Second)
	observedClient, observedErr := waitForCurrentGeneratedDeviceClient(
		observedCtx,
		path,
		observedTransports,
	)
	observedCancel()
	if observedErr != nil {
		return fmt.Errorf("current generated stream platform client was not observed: %w", observedErr)
	}
	path.providerSendRoutes.setDestinationId(observedClient.ClientId())
	clients := make([]*clientconnect.Client, 0, len(path.streamP2pClients)+2)
	clients = append(clients, observedClient)
	for _, intermediary := range path.streamP2pClients {
		clients = append(clients, intermediary.client)
	}
	clients = append(clients, path.providerClient)
	streamStatsSnapshots := func() []clientconnect.P2pDataPlaneStatsSnapshot {
		snapshots := make([]clientconnect.P2pDataPlaneStatsSnapshot, len(path.streamP2pStats))
		for nodeIndex, stats := range path.streamP2pStats {
			snapshots[nodeIndex] = stats.Snapshot()
		}
		return snapshots
	}
	deviceWriter := observedClient.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(path.providerClientId),
	)
	defer observedClient.RouteManager().CloseMultiRouteWriter(deviceWriter)
	providerWriter := path.providerClient.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(observedClient.ClientId()),
	)
	defer path.providerClient.RouteManager().CloseMultiRouteWriter(providerWriter)
	deviceRouteStateObserver := clientconnect.TestingObserveMultiRouteWriterRouteState(deviceWriter)
	defer deviceRouteStateObserver.Close()
	providerRouteStateObserver := clientconnect.TestingObserveMultiRouteWriterRouteState(providerWriter)
	defer providerRouteStateObserver.Close()
	readinessCtx, readinessCancel := context.WithTimeout(
		ctx,
		max(30*time.Second, 20*fullTunOuterRoundTrip(path)),
	)
	defer readinessCancel()
	for nodeIndex, routeStateTrace := range path.streamP2pRouteTraces {
		minimumRouteCount := 2
		if nodeIndex == 0 || nodeIndex == len(path.streamP2pRouteTraces)-1 {
			minimumRouteCount = 1
		}
		if _, err := routeStateTrace.WaitForMinimumRoutes(
			readinessCtx,
			minimumRouteCount,
			minimumRouteCount,
		); err != nil {
			return fmt.Errorf(
				"wait for stream node %d P2P routes: %w: stats=%+v",
				nodeIndex,
				err,
				streamStatsSnapshots(),
			)
		}
	}
	if err := waitForMinimumRouteCount(readinessCtx, deviceRouteStateObserver, 2); err != nil {
		return fmt.Errorf("wait for device end-to-end stream readiness: %w", err)
	}
	if err := waitForMinimumRouteCount(readinessCtx, providerRouteStateObserver, 2); err != nil {
		return fmt.Errorf("wait for provider end-to-end stream readiness: %w", err)
	}
	deviceForcedBarrier := deviceRouteStateObserver.Snapshot()
	providerForcedBarrier := providerRouteStateObserver.Snapshot()
	if deviceForcedBarrier.ActiveRouteCount < 2 ||
		providerForcedBarrier.ActiveRouteCount < 2 {
		return fmt.Errorf(
			"forced stream transition started from device=%+v provider=%+v",
			deviceForcedBarrier,
			providerForcedBarrier,
		)
	}
	for _, sendRoutes := range path.platformSendRoutes {
		sendRoutes.setDisabled(true)
	}
	if _, err := waitForRouteCountAfter(
		readinessCtx,
		deviceRouteStateObserver,
		deviceForcedBarrier,
		1,
	); err != nil {
		return fmt.Errorf(
			"wait for forced device stream route: %w; controller={%s}",
			err,
			path.deviceSendRoutes.diagnostics(
				clientconnect.DestinationId(path.providerClientId),
				deviceWriter,
			),
		)
	}
	if _, err := waitForRouteCountAfter(
		readinessCtx,
		providerRouteStateObserver,
		providerForcedBarrier,
		1,
	); err != nil {
		return fmt.Errorf(
			"wait for forced provider stream route: %w; controller={%s}",
			err,
			path.providerSendRoutes.diagnostics(
				clientconnect.DestinationId(observedClient.ClientId()),
				providerWriter,
			),
		)
	}
	promotionCtx, promotionCancel := context.WithTimeout(
		ctx,
		max(30*time.Second, 20*fullTunOuterRoundTrip(path)),
	)
	defer promotionCancel()
	hopsBeforeProbe := path.streamP2pNetwork.snapshot()
	if err := probeFullTunPath(promotionCtx, path); err != nil {
		return fmt.Errorf(
			"probe promoted stream: %w: stats=%+v hops=%+v device-packets=%+v provider-packets=%+v",
			err,
			streamStatsSnapshots(),
			path.streamP2pNetwork.snapshot(),
			path.multiClient.PacketStats(),
			path.providerRemoteNat.PacketStats(),
		)
	}
	if _, err := path.joinSourcePackCarrierBoundary(promotionCtx); err != nil {
		return fmt.Errorf(
			"join promoted stream probe tail: %w; stats=%+v hops=%+v",
			err,
			streamStatsSnapshots(),
			path.streamP2pNetwork.snapshot(),
		)
	}
	hopsAfterProbe := path.streamP2pNetwork.snapshot()
	for hopIndex, after := range hopsAfterProbe {
		before := hopsBeforeProbe[hopIndex]
		if after.Forward.PacketCount == before.Forward.PacketCount ||
			after.Forward.PacketByteCount == before.Forward.PacketByteCount ||
			after.Reverse.PacketCount == before.Reverse.PacketCount ||
			after.Reverse.PacketByteCount == before.Reverse.PacketByteCount {
			return fmt.Errorf(
				"stream hop %d did not carry bidirectional probe traffic: before=%+v after=%+v",
				hopIndex,
				before,
				after,
			)
		}
	}
	return nil
}

// Waits until a logical destination has at least the platform route and one
// end-to-end-ready stream route. Additional live aliases are also acceptable.
func waitForMinimumRouteCount(
	ctx context.Context,
	observer *clientconnect.TestingMultiRouteWriterRouteStateObserver,
	minimumRouteCount int,
) error {
	state := observer.Snapshot()
	for {
		if minimumRouteCount <= state.ActiveRouteCount {
			return nil
		}
		next, err := observer.WaitAfter(ctx, state.Generation)
		if err != nil {
			return fmt.Errorf(
				"route count=%d generation=%d, expected at least %d: %w",
				state.ActiveRouteCount,
				state.Generation,
				minimumRouteCount,
				err,
			)
		}
		state = next
	}
}

// closeAndWait stops every Pack producer, joins all retained platform
// carriers, and continues through every independent cleanup after an error.
// It is nil-safe because construction rollback owns a partially built path.
func (self *fullTunPath) closeAndWait(ctx context.Context) error {
	var closeErr error
	complete := func(resource fullTunConstructionResource, err error) {
		if err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("join %s during full-TUN teardown: %w", resource, err))
		}
		if self.constructionCleanupErrorForTest != nil {
			if injectedErr := self.constructionCleanupErrorForTest(resource); injectedErr != nil {
				closeErr = errors.Join(
					closeErr,
					fmt.Errorf("join %s during full-TUN teardown: %w", resource, injectedErr),
				)
			}
		}
		if self.afterConstructionCleanupForTest != nil {
			self.afterConstructionCleanupForTest(resource)
		}
	}
	if self.appTun != nil {
		complete(fullTunConstructionResourceAppTun, self.appTun.Close())
	}
	for _, sendRoutes := range self.platformSendRoutes {
		sendRoutes.setDisabled(false)
	}
	// Closing the application TUN stops bridge admission. Join the bridge
	// before flushing or closing its multi-client consumer so every pooled
	// packet read before Close has one live ownership handoff or local return.
	self.bridgeWaitGroup.Wait()
	if self.bridgeStarted {
		complete(fullTunConstructionResourceBridge, nil)
	}
	if self.deviceClient != nil {
		if deviceClient := self.deviceClient.Load(); deviceClient != nil {
			deviceClient.Flush()
		}
	}
	if self.multiClient != nil {
		self.multiClient.Close()
		complete(fullTunConstructionResourceMultiClient, nil)
	}
	if self.apiGenerator != nil {
		complete(
			fullTunConstructionResourceApiGenerator,
			self.apiGenerator.CloseTransportCreationAndWait(ctx),
		)
	}
	if self.deviceTransports != nil {
		complete(
			fullTunConstructionResourceDeviceTransports,
			self.deviceTransports.closeAndWait(ctx),
		)
	}
	if self.providerRemoteNat != nil {
		self.providerRemoteNat.Close()
		complete(fullTunConstructionResourceProviderRemoteNat, nil)
	}
	if self.providerLocalNat != nil {
		self.providerLocalNat.Close()
		complete(fullTunConstructionResourceProviderLocalNat, nil)
	}
	if self.providerClient != nil {
		complete(
			fullTunConstructionResourceProviderClient,
			closeClientAndOutOfBandWait(ctx, self.providerClient),
		)
	}
	if self.providerTransport != nil {
		complete(
			fullTunConstructionResourceProviderTransport,
			self.providerTransport.CloseAndWait(ctx),
		)
	}
	for _, intermediary := range self.streamP2pClients {
		if intermediary.client != nil {
			intermediary.client.Flush()
			complete(
				fullTunConstructionResourceIntermediaryClient,
				intermediary.client.CloseAndWait(ctx),
			)
		}
		if intermediary.transport != nil {
			complete(
				fullTunConstructionResourceIntermediaryRoute,
				intermediary.transport.CloseAndWait(ctx),
			)
		}
		if intermediary.tun != nil {
			complete(fullTunConstructionResourceIntermediaryTun, intermediary.tun.Close())
		}
	}
	for _, sendRoutes := range self.platformSendRoutes {
		if sendRoutes != nil {
			complete(
				fullTunConstructionResourceSendRouteController,
				sendRoutes.CloseAndWait(ctx),
			)
		}
	}
	if self.p2pNetwork != nil {
		self.p2pNetwork.close()
		complete(fullTunConstructionResourceP2pNetwork, nil)
	}
	if self.streamP2pNetwork != nil {
		self.streamP2pNetwork.close()
		complete(fullTunConstructionResourceStreamP2pNetwork, nil)
	}
	if self.providerCarrierTun != nil {
		complete(fullTunConstructionResourceProviderCarrierTun, self.providerCarrierTun.Close())
	}
	if self.deviceCarrierTun != nil {
		complete(fullTunConstructionResourceDeviceCarrierTun, self.deviceCarrierTun.Close())
	}
	for _, tracker := range []*noAckSendTracker{self.deviceNoAckSends, self.providerNoAckSends} {
		if tracker != nil {
			tracker.close()
			complete(fullTunConstructionResourceNoAckTracker, nil)
		}
	}
	for _, tracker := range []*sendPackLifecycleTracker{self.devicePackSends, self.providerPackSends} {
		if tracker != nil {
			tracker.close()
			complete(fullTunConstructionResourcePackTracker, nil)
		}
	}
	if self.providerReturns != nil {
		self.providerReturns.close()
		complete(fullTunConstructionResourceReturnTracker, nil)
	}
	return closeErr
}

// The normal fixture close reports aggregated teardown failures without
// weakening rollback callers that need the same error as a returned value.
func (self *fullTunPath) close() {
	joinCtx, joinCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer joinCancel()
	if err := self.closeAndWait(joinCtx); err != nil {
		self.t.Errorf("full-TUN teardown: %v", err)
	}
}

// Route-wide carrier idle joins exchange/access and P2P schedulers in one
// generation-stable pass. P2P UDP sockets may retain a stable unread control
// backlog; that backlog becomes part of the baseline, while any later change
// invalidates the surrounding measurement generation.
func (self *fullTunPath) waitForCarrierQuiescent(ctx context.Context) bool {
	links := self.environment.network.directionalLinks()
	creditPools := []*p2pReceiveCredits{}
	if self.p2pNetwork != nil {
		links = append(links, self.p2pNetwork.directionalLinks()...)
		creditPools = append(creditPools, self.p2pNetwork.receiveCreditPools()...)
	}
	if self.streamP2pNetwork != nil {
		links = append(links, self.streamP2pNetwork.directionalLinks()...)
		creditPools = append(creditPools, self.streamP2pNetwork.receiveCreditPools()...)
	}
	return waitForP2pCarrierQuiescent(ctx, links, creditPools, nil)
}

// The route-wide post-workload fixed point begins above SendPack, then joins
// exact IP data Pack terminals and carrier quiescence. Reliable signaling and
// maintenance remain in all-Pack diagnostics but do not own the application
// boundary. A stable owned Pion socket backlog may remain; its candidate end
// is accepted only when second source, workload-Pack, and carrier generations
// are unchanged.
func (self *fullTunPath) waitForPackAndCarrierTerminalIdle(
	ctx context.Context,
) (perfvarCarrierBoundary, string, bool) {
	trackers := []*sendPackLifecycleTracker{self.devicePackSends, self.providerPackSends}
	lastInstability := ""
	stageAfterInstability := func(stage string) string {
		if lastInstability == "" {
			return stage
		}
		return fmt.Sprintf("%s after unstable candidate: %s", stage, lastInstability)
	}
	for attempt := 1; ; attempt += 1 {
		upstreamBefore, err := self.upstreamBoundary(ctx)
		if err != nil {
			return perfvarCarrierBoundary{}, stageAfterInstability("capture upstream boundary"), false
		}
		if !self.waitThroughUpstreamBoundary(ctx, upstreamBefore) {
			return perfvarCarrierBoundary{}, stageAfterInstability("join upstream boundary"), false
		}
		before := make([]sendPackLifecycleBoundary, len(trackers))
		for trackerIndex, tracker := range trackers {
			if tracker == nil {
				return perfvarCarrierBoundary{}, fmt.Sprintf(
					"capture Pack boundary %d with nil tracker",
					trackerIndex,
				), false
			}
			boundary, ok := tracker.workloadBoundary(ctx)
			if !ok {
				return perfvarCarrierBoundary{}, fmt.Sprintf(
					"%s %d",
					stageAfterInstability("capture Pack boundary"),
					trackerIndex,
				), false
			}
			before[trackerIndex] = boundary
		}
		for trackerIndex, tracker := range trackers {
			if !tracker.waitThrough(ctx, before[trackerIndex]) {
				entryDiagnostics := make([]string, 0, len(before[trackerIndex].entries))
				for _, entry := range before[trackerIndex].entries {
					entryDiagnostics = append(entryDiagnostics, fmt.Sprintf(
						"%s->%s ack=%t type=%s phase=%d",
						entry.clientId,
						entry.destinationId,
						entry.ackRequired,
						entry.messageType,
						entry.phase.Load(),
					))
				}
				return perfvarCarrierBoundary{}, fmt.Sprintf(
					"%s %d entries=%d started=%d live=%v",
					stageAfterInstability("join Pack boundary"),
					trackerIndex,
					len(before[trackerIndex].entries),
					before[trackerIndex].startedCount,
					entryDiagnostics,
				), false
			}
		}
		if !self.waitForCarrierQuiescent(ctx) {
			return perfvarCarrierBoundary{}, stageAfterInstability("join carrier boundary"), false
		}
		carrierEnd := snapshotPerfvarCarrier(self)
		if self.afterCarrierEndCandidateForTest != nil {
			self.afterCarrierEndCandidateForTest(attempt)
		}

		upstreamAfter, err := self.upstreamBoundary(ctx)
		if err != nil {
			return perfvarCarrierBoundary{}, stageAfterInstability("recapture upstream boundary"), false
		}
		upstreamStable := fullTunUpstreamBoundaryStable(upstreamBefore, upstreamAfter)
		packStable := true
		packInstability := ""
		for trackerIndex, tracker := range trackers {
			after, ok := tracker.workloadBoundary(ctx)
			if !ok {
				return perfvarCarrierBoundary{}, fmt.Sprintf(
					"%s %d",
					stageAfterInstability("recapture Pack boundary"),
					trackerIndex,
				), false
			}
			if after.startedCount != before[trackerIndex].startedCount || 0 < len(after.entries) {
				packStable = false
				packInstability = fmt.Sprintf(
					"Pack tracker %d changed started=%d->%d live=%d",
					trackerIndex,
					before[trackerIndex].startedCount,
					after.startedCount,
					len(after.entries),
				)
			}
		}
		carrierInstability := perfvarCarrierSnapshotInstability(self, carrierEnd, false)
		if upstreamStable && packStable && carrierInstability == "" {
			return carrierEnd, "complete", true
		}
		switch {
		case !upstreamStable:
			lastInstability = fmt.Sprintf(
				"upstream changed bridge=%d->%d provider=%d->%d live=%d/%d",
				upstreamBefore.bridge.startedCount,
				upstreamAfter.bridge.startedCount,
				upstreamBefore.provider.startedCount,
				upstreamAfter.provider.startedCount,
				len(upstreamAfter.bridge.entries),
				len(upstreamAfter.provider.entries),
			)
		case !packStable:
			lastInstability = packInstability
		default:
			lastInstability = carrierInstability
		}
	}
}

// joinSourcePackCarrierBoundary returns one exact application-source-to-carrier
// fixed point without deciding whether a caller may publish it as a
// measurement end. Priming and workload boundaries deliberately share this
// ownership transaction so neither can miss IP data work still above the
// simulated carrier.
func (self *fullTunPath) joinSourcePackCarrierBoundary(
	ctx context.Context,
) (perfvarCarrierBoundary, error) {
	carrierEnd, stage, ok := self.waitForPackAndCarrierTerminalIdle(ctx)
	if ok {
		return carrierEnd, nil
	}
	p2pSnapshot := p2pNetworkSnapshot{}
	if self.p2pNetwork != nil {
		p2pSnapshot = self.p2pNetwork.snapshot()
	}
	streamSnapshot := []streamP2pHopSnapshot{}
	if self.streamP2pNetwork != nil {
		streamSnapshot = self.streamP2pNetwork.snapshot()
	}
	return perfvarCarrierBoundary{}, fmt.Errorf(
		"join source, workload Pack, and carrier boundary at %s: context=%v device-invalid=%t device-workload-started=%d device-workload-terminal-failures=%d device-all-terminal-failures=%d device-failure-samples=%+v provider-invalid=%t provider-workload-started=%d provider-workload-terminal-failures=%d provider-all-terminal-failures=%d provider-failure-samples=%+v access-links=%+v p2p=%+v stream=%+v",
		stage,
		ctx.Err(),
		self.devicePackSends.invalid.Load(),
		self.devicePackSends.workloadStarted.Load(),
		self.devicePackSends.workloadFailures.Load(),
		self.devicePackSends.failures.Load(),
		self.devicePackSends.failureSnapshot(),
		self.providerPackSends.invalid.Load(),
		self.providerPackSends.workloadStarted.Load(),
		self.providerPackSends.workloadFailures.Load(),
		self.providerPackSends.failures.Load(),
		self.providerPackSends.failureSnapshot(),
		self.environment.network.snapshotLinks(),
		p2pSnapshot,
		streamSnapshot,
	)
}

// A measured interval rejects terminal failures newer than its exact
// workload-local start unless the Pack's caller identified an enclosing TCP
// state that retains the exact bytes or can regenerate the control packet.
// Setup candidate failures remain before the floor; UDP outside an explicitly
// lossy probe, public callbacks, and unclassified failures remain fatal.
func (self *fullTunPath) validateMeasuredPackFailures(
	carrierEnd perfvarCarrierBoundary,
) error {
	packFailureFloor, active, allowProviderDatagramFailures :=
		self.activePackFailureFloorSnapshot()
	if !active {
		return nil
	}
	packFailures := carrierEnd.packFailures
	if packFailures.deviceFailureCount < packFailureFloor.deviceFailureCount ||
		packFailures.providerFailureCount < packFailureFloor.providerFailureCount ||
		packFailures.providerRecoverableFailureCount <
			packFailureFloor.providerRecoverableFailureCount ||
		packFailures.providerDatagramFailureCount <
			packFailureFloor.providerDatagramFailureCount {
		return fmt.Errorf(
			"Pack failure counters moved backward: start=%+v end=%+v",
			*packFailureFloor,
			packFailures,
		)
	}
	deviceFailureCount := packFailures.deviceFailureCount -
		packFailureFloor.deviceFailureCount
	providerFailureCount := packFailures.providerFailureCount -
		packFailureFloor.providerFailureCount
	providerRecoverableFailureCount :=
		packFailures.providerRecoverableFailureCount -
			packFailureFloor.providerRecoverableFailureCount
	providerDatagramFailureCount :=
		packFailures.providerDatagramFailureCount -
			packFailureFloor.providerDatagramFailureCount
	providerAllowedFailureCount := providerRecoverableFailureCount
	if allowProviderDatagramFailures {
		providerAllowedFailureCount += providerDatagramFailureCount
	}
	if providerFailureCount < providerAllowedFailureCount {
		return fmt.Errorf(
			"provider allowed Pack failures exceeded all failures: total=%d recoverable=%d datagram=%d allow-datagram=%t start=%+v end=%+v",
			providerFailureCount,
			providerRecoverableFailureCount,
			providerDatagramFailureCount,
			allowProviderDatagramFailures,
			*packFailureFloor,
			packFailures,
		)
	}
	providerUnrecoverableFailureCount :=
		providerFailureCount - providerAllowedFailureCount
	if deviceFailureCount == 0 && providerUnrecoverableFailureCount == 0 {
		if !self.advanceActivePackFailureFloor(*packFailureFloor, packFailures) {
			return fmt.Errorf(
				"Pack failure floor changed while validating: start=%+v end=%+v",
				*packFailureFloor,
				packFailures,
			)
		}
		return nil
	}
	deviceFailureSamples := []clientconnect.SendPackLifecycleObservation{}
	if self.devicePackSends != nil {
		deviceFailureSamples = self.devicePackSends.workloadFailureSnapshot()
	}
	providerFailureSamples := []clientconnect.SendPackLifecycleObservation{}
	if self.providerPackSends != nil {
		providerFailureSamples = self.providerPackSends.workloadFailureSnapshot()
	}
	return fmt.Errorf(
		"measured Pack terminal failures device=%d provider=%d provider-recoverable=%d provider-datagram=%d allow-provider-datagram=%t provider-unrecoverable=%d start=%+v end=%+v lifetime-device-samples=%+v lifetime-provider-samples=%+v",
		deviceFailureCount,
		providerFailureCount,
		providerRecoverableFailureCount,
		providerDatagramFailureCount,
		allowProviderDatagramFailures,
		providerUnrecoverableFailureCount,
		*packFailureFloor,
		packFailures,
		deviceFailureSamples,
		providerFailureSamples,
	)
}

// Workload completion joins ownership first, then applies the active failure
// epoch without changing the shared setup/priming transaction.
func (self *fullTunPath) joinPostWorkloadBoundary(
	ctx context.Context,
) (perfvarCarrierBoundary, error) {
	carrierEnd, err := self.joinSourcePackCarrierBoundary(ctx)
	if err != nil {
		return perfvarCarrierBoundary{}, err
	}
	if err := self.validateMeasuredPackFailures(carrierEnd); err != nil {
		return perfvarCarrierBoundary{}, err
	}
	return carrierEnd, nil
}

// Setup and warmup traffic is joined before its terminal failure counts are
// captured as the next workload-local baseline.
func (self *fullTunPath) waitForSetupBoundary(ctx context.Context) error {
	_, err := self.joinSourcePackCarrierBoundary(ctx)
	return err
}

// waitForPostWorkloadBoundary publishes a generic terminal end for workloads
// whose application completion is already known at this fixed point.
func (self *fullTunPath) waitForPostWorkloadBoundary(ctx context.Context) error {
	carrierEnd, err := self.joinPostWorkloadBoundary(ctx)
	if err != nil {
		return err
	}
	self.setCarrierMeasurementEndIfAbsent(carrierEnd)
	return nil
}

// waitForIntermediateWorkloadBoundary joins route ownership without freezing
// an end before a later application-level fence, such as a UDP terminal marker.
func (self *fullTunPath) waitForIntermediateWorkloadBoundary(ctx context.Context) error {
	_, err := self.joinPostWorkloadBoundary(ctx)
	return err
}

// One upstream snapshot covers traffic that has entered before its first
// SendPack, so no setup item can hide above the Pack lifecycle boundary.
type fullTunUpstreamBoundary struct {
	bridge   fullTunBridgeLifecycleBoundary
	provider providerReturnLifecycleBoundary
}

// Source publication is flushed in deterministic device/provider order.
func (self *fullTunPath) upstreamBoundary(
	ctx context.Context,
) (fullTunUpstreamBoundary, error) {
	if self.bridgeSends == nil || self.providerReturns == nil {
		return fullTunUpstreamBoundary{}, errors.New("source lifecycle tracker is nil")
	}
	bridge, ok := self.bridgeSends.lifecycleBoundary(ctx)
	if !ok {
		return fullTunUpstreamBoundary{}, errors.New("bridge source boundary rejected")
	}
	provider, ok := self.providerReturns.lifecycleBoundary(ctx)
	if !ok {
		return fullTunUpstreamBoundary{}, errors.New("provider-return source boundary rejected")
	}
	return fullTunUpstreamBoundary{bridge: bridge, provider: provider}, nil
}

// Exact upstream entries remain joined through terminal source ownership.
func (self *fullTunPath) waitThroughUpstreamBoundary(
	ctx context.Context,
	boundary fullTunUpstreamBoundary,
) bool {
	return self.bridgeSends.waitLifecycleThrough(ctx, boundary.bridge) &&
		self.providerReturns.waitLifecycleThrough(ctx, boundary.provider)
}

// Terminal bridge entries may be captured while their final map deletion is
// pending; their atomic terminal phase proves they no longer own source work.
func fullTunBridgeLifecycleBoundaryTerminal(
	boundary fullTunBridgeLifecycleBoundary,
) bool {
	for _, entry := range boundary.entries {
		if entry.state.Load() != fullTunBridgeSendEntryTerminal {
			return false
		}
	}
	return true
}

// Active provider flow windows intentionally retain terminal entries until
// the workload seals the window, so terminal phase—not slice length—defines
// whether their source ownership is complete.
func providerReturnLifecycleBoundaryTerminal(
	boundary providerReturnLifecycleBoundary,
) bool {
	for _, entry := range boundary.entries {
		if entry.state.Load() != providerReturnSendEntryTerminal {
			return false
		}
	}
	return true
}

// A stable source generation has no new or still-live item after the carrier
// baseline was created. Retained terminal flow-window entries are complete.
func fullTunUpstreamBoundaryStable(
	before fullTunUpstreamBoundary,
	after fullTunUpstreamBoundary,
) bool {
	return before.bridge.startedCount == after.bridge.startedCount &&
		before.provider.startedCount == after.provider.startedCount &&
		fullTunBridgeLifecycleBoundaryTerminal(after.bridge) &&
		providerReturnLifecycleBoundaryTerminal(after.provider)
}

// A provider download keeps its exact terminal entries until the marker closes
// the active flow window; that retention must not prevent the pre-marker
// route-wide terminal boundary from completing.
func TestFullTunUpstreamBoundaryStableAcceptsRetainedTerminalProviderFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tracker := newProviderReturnSendTracker()
	defer tracker.close()
	flowKey := providerReturnTrackerTestFlow(31)
	window, ok := tracker.beginFlowWindow(ctx, flowKey)
	if !ok {
		t.Fatalf("begin provider-return flow window: %v", ctx.Err())
	}
	observeProviderReturnStarted(tracker, 1, flowKey, 1, 800)
	flowBoundary, ok := tracker.flowBoundary(ctx, window, 1, 800)
	if !ok {
		t.Fatalf("capture provider-return flow boundary: %v", ctx.Err())
	}
	observeProviderReturnCompleted(tracker, 1, flowKey, 1, 800, true)
	if !tracker.waitThrough(ctx, flowBoundary) {
		t.Fatalf("join provider-return flow boundary: %v", ctx.Err())
	}
	before, ok := tracker.lifecycleBoundary(ctx)
	if !ok {
		t.Fatalf("capture first lifecycle boundary: %v", ctx.Err())
	}
	after, ok := tracker.lifecycleBoundary(ctx)
	if !ok {
		t.Fatalf("capture second lifecycle boundary: %v", ctx.Err())
	}
	if len(after.entries) != 1 ||
		after.entries[0].state.Load() != providerReturnSendEntryTerminal {
		t.Fatalf("active flow did not retain its terminal entry: %+v", after)
	}
	if !fullTunUpstreamBoundaryStable(
		fullTunUpstreamBoundary{provider: before},
		fullTunUpstreamBoundary{provider: after},
	) {
		t.Fatal("retained terminal provider flow made the upstream generation unstable")
	}
}

// An unchanged generation with a pending entry still owns work and therefore
// cannot be accepted merely because no new source item started.
func TestFullTunUpstreamBoundaryStableRejectsPendingProviderFlow(t *testing.T) {
	entry := &providerReturnSendEntry{}
	boundary := fullTunUpstreamBoundary{
		provider: providerReturnLifecycleBoundary{
			entries:      []*providerReturnSendEntry{entry},
			startedCount: 1,
		},
	}
	if fullTunUpstreamBoundaryStable(boundary, boundary) {
		t.Fatal("pending provider flow was accepted as terminal")
	}
}

// The premeasurement fixed point has no wait/begin gap: upstream sources and
// Packs are joined, carrier epochs begin, and every generation is rechecked.
// A crossing item invalidates that candidate start and retries the whole pass.
func (self *fullTunPath) waitForMeasurementBoundary(ctx context.Context) error {
	failure := func(stage string, err error) error {
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = errors.New("boundary rejected")
		}
		return &fullTunMeasurementBoundaryError{stage: stage, err: err}
	}
	if !self.readinessAppFence.Load() {
		return failure("application readiness", errors.New("readiness fence is absent"))
	}
	packTrackers := []*sendPackLifecycleTracker{self.devicePackSends, self.providerPackSends}
	packFailure := func(trackerIndex int, tracker *sendPackLifecycleTracker) error {
		return fmt.Errorf(
			"tracker=%d invalid=%t workload-started=%d workload-failures=%d all-started=%d all-failures=%d samples=%+v",
			trackerIndex,
			tracker.invalid.Load(),
			tracker.workloadStarted.Load(),
			tracker.workloadFailures.Load(),
			tracker.started.Load(),
			tracker.failures.Load(),
			tracker.failureSnapshot(),
		)
	}
	for attempt := 1; ; attempt += 1 {
		select {
		case <-ctx.Done():
			return failure("source-to-carrier fixed point", ctx.Err())
		default:
		}
		upstreamBefore, err := self.upstreamBoundary(ctx)
		if err != nil {
			return failure("capture upstream source boundary", err)
		}
		if !self.waitThroughUpstreamBoundary(ctx, upstreamBefore) {
			return failure("join upstream source boundary", nil)
		}

		packBefore := make([]sendPackLifecycleBoundary, len(packTrackers))
		for trackerIndex, tracker := range packTrackers {
			if tracker == nil {
				return failure("capture Pack boundary", errors.New("Pack lifecycle tracker is nil"))
			}
			boundary, ok := tracker.workloadBoundary(ctx)
			if !ok {
				return failure("capture Pack boundary", packFailure(trackerIndex, tracker))
			}
			packBefore[trackerIndex] = boundary
		}
		for trackerIndex, tracker := range packTrackers {
			if !tracker.waitThrough(ctx, packBefore[trackerIndex]) {
				return failure("join Pack boundary", packFailure(trackerIndex, tracker))
			}
		}
		if !self.waitForCarrierQuiescent(ctx) {
			return failure("join carrier terminal ownership", nil)
		}
		carrierStart, err := beginPerfvarCarrierMeasurementNow(self)
		if err != nil {
			if errors.Is(err, errPerfvarCarrierStartCrossed) {
				select {
				case <-ctx.Done():
					return failure("retry carrier baseline reset pass", ctx.Err())
				case <-time.After(time.Millisecond):
				}
				continue
			}
			return failure("begin carrier generation", err)
		}
		if self.afterCarrierStartForTest != nil {
			self.afterCarrierStartForTest(attempt)
		}

		upstreamAfter, err := self.upstreamBoundary(ctx)
		if err != nil {
			return failure("verify upstream source generation", err)
		}
		packStable := true
		for trackerIndex, tracker := range packTrackers {
			after, ok := tracker.workloadBoundary(ctx)
			if !ok {
				return failure("verify Pack generation", packFailure(trackerIndex, tracker))
			}
			if after.startedCount != packBefore[trackerIndex].startedCount ||
				0 < len(after.entries) {
				packStable = false
			}
		}
		if fullTunUpstreamBoundaryStable(upstreamBefore, upstreamAfter) &&
			packStable && perfvarCarrierGenerationStable(self, carrierStart) {
			self.setPreparedCarrierStart(carrierStart)
			return nil
		}
		select {
		case <-ctx.Done():
			return failure("retry source-to-carrier generation", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

// A blocking connection write is completed even when the stack accepts a prefix.
func writeFullTunAll(connection net.Conn, payload []byte) error {
	for 0 < len(payload) {
		writtenByteCount, err := connection.Write(payload)
		if 0 < writtenByteCount {
			payload = payload[writtenByteCount:]
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// The effective application-direction rate is the slowest physical segment.
// Scenario profile directions remain device upload/download across every
// topology even where a link-oriented fixture places the device on the right.
func fullTunEffectiveRateBitsPerSecond(path *fullTunPath, upload bool) int64 {
	rates := []int64{}
	if fullTunRouteIsExchange(path.route) {
		if upload {
			rates = append(
				rates,
				path.environment.deviceAccessProfile.Forward.RateBitsPerSecond,
				path.environment.providerAccessProfile.Reverse.RateBitsPerSecond,
			)
			if internal := path.environment.internalExchangeProfile; internal != nil {
				rates = append(rates, internal.Forward.RateBitsPerSecond)
			}
		} else {
			rates = append(
				rates,
				path.environment.providerAccessProfile.Forward.RateBitsPerSecond,
				path.environment.deviceAccessProfile.Reverse.RateBitsPerSecond,
			)
			if internal := path.environment.internalExchangeProfile; internal != nil {
				rates = append(rates, internal.Reverse.RateBitsPerSecond)
			}
		}
	} else {
		if upload {
			rates = append(rates, path.environment.profile.Forward.RateBitsPerSecond)
		} else {
			rates = append(rates, path.environment.profile.Reverse.RateBitsPerSecond)
		}
	}
	rateBitsPerSecond := int64(0)
	for _, candidate := range rates {
		if 0 < candidate && (rateBitsPerSecond == 0 || candidate < rateBitsPerSecond) {
			rateBitsPerSecond = candidate
		}
	}
	return rateBitsPerSecond
}

// Preserves the ordinary 30-second floor and gives the instrumented userspace
// network enough wall time to exercise concurrency without a false timeout.
func fullTunMinimumDirectionalWorkloadTimeout() time.Duration {
	if perfvarRaceEnabled {
		return 2 * time.Minute
	}
	return 30 * time.Second
}

// A directional workload deadline scales with modeled RTT, bottleneck rate,
// and the total bytes sharing that bottleneck.
func fullTunDirectionalWorkloadTimeout(
	path *fullTunPath,
	upload bool,
	byteCount int64,
) time.Duration {
	roundTrip := fullTunOuterRoundTrip(path)
	rateBitsPerSecond := fullTunEffectiveRateBitsPerSecond(path, upload)
	rateDuration := time.Duration(0)
	if 0 < rateBitsPerSecond {
		rateDuration = time.Duration(float64(time.Second) * float64(byteCount*8) / float64(rateBitsPerSecond))
	}
	return max(
		fullTunMinimumDirectionalWorkloadTimeout(),
		60*roundTrip,
		60*rateDuration,
	)
}

// Bidirectional and event workloads retain the larger directional allowance.
func fullTunWorkloadTimeout(path *fullTunPath, byteCount int64) time.Duration {
	return max(
		fullTunDirectionalWorkloadTimeout(path, true, byteCount),
		fullTunDirectionalWorkloadTimeout(path, false, byteCount),
	)
}

// Route readiness writes an exact request and reads its echo sequentially, so
// its deadline is the sum of the independently modeled directional budgets.
// A one-way workload maximum is insufficient when both legs consume the same
// connection deadline.
func fullTunRouteReadinessTimeout(path *fullTunPath) time.Duration {
	return fullTunDirectionalWorkloadTimeout(path, true, fullTunProbePayloadByteCount) +
		fullTunDirectionalWorkloadTimeout(path, false, fullTunProbePayloadByteCount)
}

// The race runtime gets enough liveness headroom to complete a valid route
// probe, while ordinary correctness runs retain the production watchdogs.
func TestFullTunMultiClientSettingsBoundRaceConstruction(t *testing.T) {
	path := &fullTunPath{
		environment: &routeEnvironment{
			profile: initialNetworkProfiles(20260811)["clean-lan"],
		},
		route:       fullTunRouteP2pFast,
		p2pHopCount: 1,
	}
	settings := fullTunMultiClientSettings(path)
	defaults := clientconnect.DefaultMultiClientSettings()
	clientSettings := fullTunClientSettings(fullTunRouteP2pFast, nil, nil, nil, 0)
	clientDefaults := clientconnect.DefaultClientSettings()
	p2pSettings := clientSettings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings
	platformSettings := fullTunPlatformSettings(
		0,
		clientconnect.TransportModeH1,
		nil,
		nil,
		newFullTunEndpointPlatformBudget(),
	)
	platformDefaults := clientconnect.DefaultPlatformTransportSettings()
	if !perfvarRaceEnabled {
		if settings.SendStallTimeout != defaults.SendStallTimeout ||
			settings.BlackholeReceiveTimeout != defaults.BlackholeReceiveTimeout ||
			settings.WindowExpandTimeout != defaults.WindowExpandTimeout ||
			clientSettings.ReadTimeout != clientDefaults.ReadTimeout ||
			clientSettings.BufferTimeout != clientDefaults.BufferTimeout ||
			clientSettings.ControlPingTimeout != 10*time.Second ||
			platformSettings.PingTimeout != platformDefaults.PingTimeout ||
			platformSettings.ReadTimeout != platformDefaults.ReadTimeout ||
			platformSettings.InactiveDrainTimeout != platformDefaults.InactiveDrainTimeout {
			t.Fatalf(
				"ordinary route settings multi=%s/%s/%s client=%s/%s/%s platform=%s/%s/%s",
				settings.SendStallTimeout,
				settings.BlackholeReceiveTimeout,
				settings.WindowExpandTimeout,
				clientSettings.ReadTimeout,
				clientSettings.BufferTimeout,
				clientSettings.ControlPingTimeout,
				platformSettings.PingTimeout,
				platformSettings.ReadTimeout,
				platformSettings.InactiveDrainTimeout,
			)
		}
		return
	}
	allowance := fullTunRouteReadinessTimeout(path)
	fixedAllowance := fullTunRaceInstrumentationAllowance()
	if settings.PingTimeout < allowance ||
		settings.AckTimeout < allowance ||
		settings.BlackholeTimeout < allowance ||
		settings.BlackholeReceiveTimeout != 0 ||
		settings.BlackholeConnectTimeout < allowance ||
		settings.WindowGeneratorTimeout < allowance ||
		settings.WindowExpandTimeout < allowance ||
		settings.StatsWindowMaxUnhealthyDuration < allowance ||
		settings.SendStallTimeout < allowance ||
		clientSettings.ReadTimeout < fixedAllowance ||
		clientSettings.BufferTimeout < fixedAllowance ||
		clientSettings.ControlPingTimeout < fixedAllowance ||
		clientSettings.SendBufferSettings.AckTimeout < fixedAllowance ||
		clientSettings.SendBufferSettings.WriteTimeout < fixedAllowance ||
		clientSettings.ReceiveBufferSettings.GapTimeout < fixedAllowance ||
		clientSettings.ReceiveBufferSettings.IdleTimeout < fixedAllowance ||
		clientSettings.ReceiveBufferSettings.MaxPeerAuditDuration < fixedAllowance ||
		clientSettings.ReceiveBufferSettings.WriteTimeout < fixedAllowance ||
		clientSettings.ForwardBufferSettings.WriteTimeout < fixedAllowance ||
		p2pSettings.WriteTimeout < fixedAllowance ||
		p2pSettings.ReadTimeout < fixedAllowance ||
		p2pSettings.ConnectTimeout < fixedAllowance ||
		p2pSettings.AdmissionRetryTimeout < fixedAllowance ||
		p2pSettings.EndToEndProbeTimeout < fixedAllowance ||
		clientSettings.WebRtcSettings.DisconnectedTimeout < fixedAllowance ||
		clientSettings.WebRtcSettings.FailedTimeout < fixedAllowance ||
		clientSettings.WebRtcSettings.SctpNoProgressTimeout < fixedAllowance ||
		platformSettings.PingTimeout < fixedAllowance ||
		platformSettings.ReadTimeout < fixedAllowance ||
		platformSettings.InactiveDrainTimeout < fixedAllowance {
		t.Fatalf(
			"race route settings multi=%s/%s/%s/%s/%s/%s/%s/%s/%s client=%s/%s/%s send=%s/%s receive=%s/%s/%s/%s forward=%s p2p=%s/%s/%s/%s/%s webrtc=%s/%s/%s platform=%s/%s/%s, want path allowance=%s fixed allowance=%s and receive disabled",
			settings.PingTimeout,
			settings.AckTimeout,
			settings.BlackholeTimeout,
			settings.BlackholeReceiveTimeout,
			settings.BlackholeConnectTimeout,
			settings.WindowGeneratorTimeout,
			settings.WindowExpandTimeout,
			settings.StatsWindowMaxUnhealthyDuration,
			settings.SendStallTimeout,
			clientSettings.ReadTimeout,
			clientSettings.BufferTimeout,
			clientSettings.ControlPingTimeout,
			clientSettings.SendBufferSettings.AckTimeout,
			clientSettings.SendBufferSettings.WriteTimeout,
			clientSettings.ReceiveBufferSettings.GapTimeout,
			clientSettings.ReceiveBufferSettings.IdleTimeout,
			clientSettings.ReceiveBufferSettings.MaxPeerAuditDuration,
			clientSettings.ReceiveBufferSettings.WriteTimeout,
			clientSettings.ForwardBufferSettings.WriteTimeout,
			p2pSettings.WriteTimeout,
			p2pSettings.ReadTimeout,
			p2pSettings.ConnectTimeout,
			p2pSettings.AdmissionRetryTimeout,
			p2pSettings.EndToEndProbeTimeout,
			clientSettings.WebRtcSettings.DisconnectedTimeout,
			clientSettings.WebRtcSettings.FailedTimeout,
			clientSettings.WebRtcSettings.SctpNoProgressTimeout,
			platformSettings.PingTimeout,
			platformSettings.ReadTimeout,
			platformSettings.InactiveDrainTimeout,
			allowance,
			fixedAllowance,
		)
	}
}

func TestFullTunClientSettingsApplyLogicalLaneCount(t *testing.T) {
	for _, laneCount := range []int{0, 1, 4, 8} {
		settings := fullTunClientSettings(
			fullTunRouteExchangeH3,
			nil,
			nil,
			nil,
			laneCount,
		)
		if got := settings.SendBufferSettings.LogicalDataLaneCount; got != laneCount {
			t.Fatalf("logical lane count=%d, want %d", got, laneCount)
		}
	}
}

// Direction and topology determine the physical bottleneck used for timeout
// sizing, including a separately impaired internal exchange link.
func TestFullTunEffectiveRateAndAggregateTimeout(t *testing.T) {
	profiles := initialNetworkProfiles(3299)
	lte := profiles["lte"]
	direct := &fullTunPath{
		environment: &routeEnvironment{profile: lte},
		route:       fullTunRouteP2pFast,
		p2pHopCount: 1,
	}
	if rate := fullTunEffectiveRateBitsPerSecond(direct, true); rate != lte.Forward.RateBitsPerSecond {
		t.Fatalf("direct P2P upload rate=%d", rate)
	}
	if rate := fullTunEffectiveRateBitsPerSecond(direct, false); rate != lte.Reverse.RateBitsPerSecond {
		t.Fatalf("direct P2P download rate=%d", rate)
	}
	extended := &fullTunPath{
		environment: &routeEnvironment{profile: lte},
		route:       fullTunRouteP2pFast,
		p2pHopCount: 3,
	}
	if rate := fullTunEffectiveRateBitsPerSecond(extended, true); rate != lte.Forward.RateBitsPerSecond {
		t.Fatalf("extended P2P upload rate=%d", rate)
	}
	if rate := fullTunEffectiveRateBitsPerSecond(extended, false); rate != lte.Reverse.RateBitsPerSecond {
		t.Fatalf("extended P2P download rate=%d", rate)
	}
	clean := profiles["clean-lan"]
	internal := clean
	internal.Name = "internal-10mbps"
	internal.Forward.RateBitsPerSecond = 10_000_000
	internal.Reverse.RateBitsPerSecond = 10_000_000
	split := &fullTunPath{
		environment: &routeEnvironment{
			profile:                 clean,
			deviceAccessProfile:     clean,
			providerAccessProfile:   clean,
			internalExchangeProfile: &internal,
		},
		route: fullTunRouteExchangeH3,
	}
	if rate := fullTunEffectiveRateBitsPerSecond(split, true); rate != 10_000_000 {
		t.Fatalf("split exchange upload rate=%d", rate)
	}
	if rate := fullTunEffectiveRateBitsPerSecond(split, false); rate != 10_000_000 {
		t.Fatalf("split exchange download rate=%d", rate)
	}
	rateProfile := clean
	rateProfile.Forward.RateBitsPerSecond = 8 * 1024 * 1024
	rateProfile.Reverse.RateBitsPerSecond = 16 * 1024 * 1024
	aggregate := &fullTunPath{
		environment: &routeEnvironment{profile: rateProfile},
		route:       fullTunRouteP2pFast,
		p2pHopCount: 3,
	}
	perFlow := fullTunDirectionalWorkloadTimeout(aggregate, true, 1024*1024)
	fourFlows := fullTunDirectionalWorkloadTimeout(aggregate, true, 4*1024*1024)
	wantPerFlow := max(fullTunMinimumDirectionalWorkloadTimeout(), time.Minute)
	if perFlow != wantPerFlow || fourFlows != 4*time.Minute {
		t.Fatalf("aggregate timeout per-flow=%s four-flows=%s", perFlow, fourFlows)
	}
}

// Composed access paths retain separate upload and download allowances at the
// route-readiness boundary instead of inheriting the old fixed 30-second cap.
func TestFullTunRouteReadinessTimeoutUsesComposedRoundTrip(t *testing.T) {
	profiles := initialNetworkProfiles(3300)
	providerProfile := profiles["clean-lan"]
	testCases := []struct {
		name                   string
		profileName            string
		route                  fullTunRoute
		wantRoundTrip          time.Duration
		wantDirectionalTimeout time.Duration
		wantReadinessTimeout   time.Duration
	}{
		{
			name:                   "exchange 500 ms",
			profileName:            "single-region-500ms-rtt",
			route:                  fullTunRouteExchangeH1,
			wantRoundTrip:          502 * time.Millisecond,
			wantDirectionalTimeout: 30*time.Second + 120*time.Millisecond,
			wantReadinessTimeout:   60*time.Second + 240*time.Millisecond,
		},
		{
			name:                   "exchange 1000 ms",
			profileName:            "single-region-1000ms-rtt",
			route:                  fullTunRouteExchangeH3,
			wantRoundTrip:          time.Second + 2*time.Millisecond,
			wantDirectionalTimeout: 60*time.Second + 120*time.Millisecond,
			wantReadinessTimeout:   120*time.Second + 240*time.Millisecond,
		},
		{
			name:                   "direct 500 ms",
			profileName:            "single-region-500ms-rtt",
			route:                  fullTunRouteP2pLegacy,
			wantRoundTrip:          500 * time.Millisecond,
			wantDirectionalTimeout: 30 * time.Second,
			wantReadinessTimeout:   60 * time.Second,
		},
		{
			name:                   "direct 1000 ms",
			profileName:            "single-region-1000ms-rtt",
			route:                  fullTunRouteP2pFast,
			wantRoundTrip:          time.Second,
			wantDirectionalTimeout: 60 * time.Second,
			wantReadinessTimeout:   120 * time.Second,
		},
	}
	for _, testCase := range testCases {
		wantDirectionalTimeout := max(
			testCase.wantDirectionalTimeout,
			fullTunMinimumDirectionalWorkloadTimeout(),
		)
		wantReadinessTimeout := testCase.wantReadinessTimeout
		if testCase.wantDirectionalTimeout < wantDirectionalTimeout {
			wantReadinessTimeout = 2 * wantDirectionalTimeout
		}
		profile := profiles[testCase.profileName]
		path := &fullTunPath{
			environment: &routeEnvironment{
				profile:               profile,
				deviceAccessProfile:   profile,
				providerAccessProfile: providerProfile,
			},
			route:       testCase.route,
			p2pHopCount: 1,
		}
		if roundTrip := fullTunOuterRoundTrip(path); roundTrip != testCase.wantRoundTrip {
			t.Errorf("%s round trip=%s, want=%s", testCase.name, roundTrip, testCase.wantRoundTrip)
		}
		uploadTimeout := fullTunDirectionalWorkloadTimeout(
			path,
			true,
			fullTunProbePayloadByteCount,
		)
		downloadTimeout := fullTunDirectionalWorkloadTimeout(
			path,
			false,
			fullTunProbePayloadByteCount,
		)
		readinessTimeout := fullTunRouteReadinessTimeout(path)
		if uploadTimeout != wantDirectionalTimeout ||
			downloadTimeout != wantDirectionalTimeout {
			t.Errorf(
				"%s directional timeouts upload=%s download=%s, want=%s",
				testCase.name,
				uploadTimeout,
				downloadTimeout,
				wantDirectionalTimeout,
			)
		}
		if readinessTimeout != wantReadinessTimeout {
			t.Errorf(
				"%s readiness timeout=%s, want=%s",
				testCase.name,
				readinessTimeout,
				wantReadinessTimeout,
			)
		}
		if readinessTimeout != uploadTimeout+downloadTimeout {
			t.Errorf(
				"%s readiness timeout=%s, want directional sum=%s",
				testCase.name,
				readinessTimeout,
				uploadTimeout+downloadTimeout,
			)
		}
		oldTimeout := max(30*time.Second, 20*fullTunOuterRoundTrip(path))
		if readinessTimeout <= oldTimeout {
			t.Errorf(
				"%s readiness timeout=%s, want greater than old one-way cap=%s",
				testCase.name,
				readinessTimeout,
				oldTimeout,
			)
		}
	}
}

// Scheduler processing belongs to every traversed direction just like base
// propagation delay, including internal exchange links and repeated P2P hops.
func TestFullTunOuterRoundTripIncludesProcessingDelay(t *testing.T) {
	device := networkProfile{
		Forward: linkProfile{BaseDelay: 10 * time.Millisecond, ProcessingDelay: 2 * time.Millisecond},
		Reverse: linkProfile{BaseDelay: 20 * time.Millisecond, ProcessingDelay: 3 * time.Millisecond},
	}
	provider := networkProfile{
		Forward: linkProfile{BaseDelay: time.Millisecond, ProcessingDelay: 4 * time.Millisecond},
		Reverse: linkProfile{BaseDelay: 2 * time.Millisecond, ProcessingDelay: 5 * time.Millisecond},
	}
	internal := networkProfile{
		Forward: linkProfile{BaseDelay: 3 * time.Millisecond, ProcessingDelay: 6 * time.Millisecond},
		Reverse: linkProfile{BaseDelay: 4 * time.Millisecond, ProcessingDelay: 7 * time.Millisecond},
	}
	exchange := &fullTunPath{
		environment: &routeEnvironment{
			profile:                 device,
			deviceAccessProfile:     device,
			providerAccessProfile:   provider,
			internalExchangeProfile: &internal,
		},
		route: fullTunRouteExchangeH1,
	}
	if roundTrip := fullTunOuterRoundTrip(exchange); roundTrip != 67*time.Millisecond {
		t.Fatalf("processed exchange round trip=%s, want=67ms", roundTrip)
	}
	direct := &fullTunPath{
		environment: &routeEnvironment{profile: device},
		route:       fullTunRouteP2pFast,
		p2pHopCount: 3,
	}
	if roundTrip := fullTunOuterRoundTrip(direct); roundTrip != 105*time.Millisecond {
		t.Fatalf("processed three-hop round trip=%s, want=105ms", roundTrip)
	}
}

// Readiness cannot become a measurement boundary until the echo server has
// closed its accepted socket and every resulting source send and physical
// carrier submission has reached a terminal state. The callback is an exact
// pre-Close barrier, so this regression does not infer ordering from sleeps.
func TestFullTunReadinessWaitsForServerSocketClose(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(3301)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()

		beforeServerClose := make(chan struct{})
		releaseServerClose := make(chan struct{})
		var releaseServerOnce sync.Once
		releaseServer := func() {
			releaseServerOnce.Do(func() {
				close(releaseServerClose)
			})
		}
		defer releaseServer()
		terminalHeld := make(chan struct{})
		releaseTerminal := make(chan struct{})
		var releaseTerminalOnce sync.Once
		releasePackTerminal := func() {
			releaseTerminalOnce.Do(func() {
				close(releaseTerminal)
			})
		}
		defer releasePackTerminal()
		var armPackTerminal atomic.Bool
		var heldPackTerminal atomic.Bool
		holdFirstTerminal := func(observation clientconnect.SendPackLifecycleObservation) {
			if armPackTerminal.Load() &&
				observation.Phase == clientconnect.SendPackLifecyclePhaseTerminal &&
				heldPackTerminal.CompareAndSwap(false, true) {
				close(terminalHeld)
				select {
				case <-releaseTerminal:
				case <-ctx.Done():
				}
			}
		}
		path.devicePackSends.setBeforeObserverPublishForTest(holdFirstTerminal)
		path.providerPackSends.setBeforeObserverPublishForTest(holdFirstTerminal)
		path.beforeReadinessServerCloseForTest = func() {
			armPackTerminal.Store(true)
			close(beforeServerClose)
			select {
			case <-releaseServerClose:
			case <-ctx.Done():
			}
		}

		readinessResult := make(chan error, 1)
		go func() {
			if err := probeFullTunPath(ctx, path); err != nil {
				readinessResult <- err
				return
			}
			if err := path.waitForMeasurementBoundary(ctx); err != nil {
				readinessResult <- fmt.Errorf("wait for post-probe measurement boundary: %w", err)
				return
			}
			readinessResult <- nil
		}()

		select {
		case <-beforeServerClose:
		case err := <-readinessResult:
			t.Fatalf("readiness returned before the server reached Close: %v", err)
		case <-ctx.Done():
			t.Fatalf("readiness server did not reach Close: %v", ctx.Err())
		}
		select {
		case err := <-readinessResult:
			t.Fatalf("readiness returned while the server socket Close was held: %v", err)
		default:
		}

		releaseServer()
		select {
		case <-terminalHeld:
		case err := <-readinessResult:
			t.Fatalf("readiness returned before a post-Close Pack reached terminal publication: %v", err)
		case <-ctx.Done():
			t.Fatalf("post-Close Pack did not reach terminal publication: %v", ctx.Err())
		}
		select {
		case err := <-readinessResult:
			t.Fatalf("readiness returned while final Pack terminal publication was held: %v", err)
		default:
		}
		releasePackTerminal()
		select {
		case err := <-readinessResult:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatalf("readiness did not join the released server Close: %v", ctx.Err())
		}
		path.beforeReadinessServerCloseForTest = nil

		for trackerName, tracker := range map[string]*sendPackLifecycleTracker{
			"device":   path.devicePackSends,
			"provider": path.providerPackSends,
		} {
			boundary, ok := tracker.boundary(ctx)
			if !ok || !tracker.waitThrough(ctx, boundary) {
				t.Fatalf("%s source sends retained a post-readiness tail: %v", trackerName, ctx.Err())
			}
		}
		if !path.waitForCarrierQuiescent(ctx) {
			t.Fatalf("carrier retained a post-readiness tail: %v", ctx.Err())
		}
		for linkName, snapshot := range environment.network.snapshotLinks() {
			if snapshot.QueuedPacketCount != 0 || snapshot.QueuedByteCount != 0 {
				t.Errorf("link %s retained queued work after readiness: %+v", linkName, snapshot)
			}
		}
	})
}

// A completed TCP payload is not a terminal carrier boundary. This regression
// arms at the server's exact pre-Close edge, holds the first subsequent Pack
// terminal publication, and proves the workload cannot return until that exact
// identity and all resulting carrier work are joined.
func TestFullTunTCPWorkloadWaitsForHeldFinalPackTerminal(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(3302)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()

		serverCloseReached := make(chan struct{})
		terminalHeld := make(chan struct{})
		releaseTerminal := make(chan struct{})
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() {
				close(releaseTerminal)
			})
		}
		defer release()
		var terminalArmed atomic.Bool
		var terminalHeldOnce atomic.Bool
		path.beforeWorkloadServerCloseForTest = func() {
			terminalArmed.Store(true)
			close(serverCloseReached)
		}
		holdFirstTerminal := func(observation clientconnect.SendPackLifecycleObservation) {
			if terminalArmed.Load() &&
				observation.Phase == clientconnect.SendPackLifecyclePhaseTerminal &&
				terminalHeldOnce.CompareAndSwap(false, true) {
				close(terminalHeld)
				select {
				case <-releaseTerminal:
				case <-ctx.Done():
				}
			}
		}
		path.devicePackSends.setBeforeObserverPublishForTest(holdFirstTerminal)
		path.providerPackSends.setBeforeObserverPublishForTest(holdFirstTerminal)

		type workloadCompletion struct {
			result workloadResult
			err    error
		}
		workloadDone := make(chan workloadCompletion, 1)
		go func() {
			result, err := measureFullTunUpload(ctx, path, 64*1024)
			workloadDone <- workloadCompletion{result: result, err: err}
		}()
		select {
		case <-serverCloseReached:
		case completion := <-workloadDone:
			t.Fatalf("workload returned before server Close: %+v", completion)
		case <-ctx.Done():
			t.Fatalf("workload server did not reach Close: %v", ctx.Err())
		}
		select {
		case <-terminalHeld:
		case completion := <-workloadDone:
			t.Fatalf("workload returned before final Pack terminal publication: %+v", completion)
		case <-ctx.Done():
			t.Fatalf("final Pack terminal publication was not observed: %v", ctx.Err())
		}
		select {
		case completion := <-workloadDone:
			t.Fatalf("workload returned while final Pack terminal was held: %+v", completion)
		default:
		}

		release()
		select {
		case completion := <-workloadDone:
			if completion.err != nil {
				t.Fatal(completion.err)
			}
			if completion.result.UsefulByteCount != 64*1024 {
				t.Fatalf("workload result=%+v", completion.result)
			}
		case <-ctx.Done():
			t.Fatalf("workload did not join released terminal: %v", ctx.Err())
		}
	})
}

// One direct socket reaches the server's preface read before the production
// TUN dial. A successful result must include that losing handler's join.
func testFullTunTCPWorkloadAfterDormantAcceptedCandidate(
	t *testing.T,
	seed int64,
	measure func(context.Context, *fullTunPath) (workloadResult, error),
) workloadResult {
	t.Helper()
	result, err := measureFullTunTCPWorkloadAfterDormantAcceptedCandidate(t, seed, measure)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// The error-returning form lets lifecycle regressions inspect cleanup after a
// measured workload fails without converting that expected error into Fatal.
func measureFullTunTCPWorkloadAfterDormantAcceptedCandidate(
	t *testing.T,
	seed int64,
	measure func(context.Context, *fullTunPath) (workloadResult, error),
) (workloadResult, error) {
	t.Helper()
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	var result workloadResult
	var resultErr error
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(seed)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()

		acceptedConnection := make(chan net.Conn, 1)
		prefaceReadConnection := make(chan net.Conn, 1)
		loserJoined := make(chan bool, 1)
		var acceptedOnce sync.Once
		var prefaceReadOnce sync.Once
		var stateLock sync.Mutex
		var loserServerConnection net.Conn
		path.workloadFlowServerSettingsForTest = &logicalTCPFlowServerSettings{
			afterAcceptForTest: func(connection net.Conn) {
				acceptedOnce.Do(func() {
					acceptedConnection <- connection
				})
			},
			beforePrefaceReadForTest: func(connection net.Conn) {
				prefaceReadOnce.Do(func() {
					prefaceReadConnection <- connection
				})
			},
			beforeCandidateDoneForTest: func(connection net.Conn, _ logicalTCPFlowId, claimed bool) {
				isInjectedLoser := func() bool {
					stateLock.Lock()
					defer stateLock.Unlock()
					return connection == loserServerConnection
				}()
				if isInjectedLoser {
					loserJoined <- claimed
				}
			},
		}
		var loserConnection net.Conn
		var loserReader sync.WaitGroup
		defer func() {
			if loserConnection != nil {
				_ = loserConnection.Close()
			}
			if path.beforeWorkloadLoserWaitForTest != nil {
				path.beforeWorkloadLoserWaitForTest()
			}
			loserReader.Wait()
		}()
		loserRead := make(chan error, 1)
		path.beforeWorkloadClientDialForTest = func(hookCtx context.Context, address string) error {
			connection, dialErr := (&net.Dialer{}).DialContext(hookCtx, "tcp4", address)
			if dialErr != nil {
				return fmt.Errorf("dial dormant workload candidate: %w", dialErr)
			}
			loserConnection = connection
			var serverConnection net.Conn
			select {
			case serverConnection = <-acceptedConnection:
			case <-hookCtx.Done():
				return fmt.Errorf("wait for dormant workload candidate accept: %w", hookCtx.Err())
			}
			var readingConnection net.Conn
			select {
			case readingConnection = <-prefaceReadConnection:
			case <-hookCtx.Done():
				return fmt.Errorf("wait for dormant workload candidate preface read: %w", hookCtx.Err())
			}
			if readingConnection != serverConnection ||
				serverConnection.RemoteAddr().String() != connection.LocalAddr().String() {
				return fmt.Errorf(
					"dormant workload candidate accepted=%s reading=%s client=%s",
					serverConnection.RemoteAddr(),
					readingConnection.RemoteAddr(),
					connection.LocalAddr(),
				)
			}
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				loserServerConnection = serverConnection
			}()
			loserReader.Add(1)
			go func() {
				defer loserReader.Done()
				buffer := make([]byte, 1)
				_, readErr := connection.Read(buffer)
				if path.beforeWorkloadLoserDoneForTest != nil {
					path.beforeWorkloadLoserDoneForTest()
				}
				loserRead <- readErr
			}()
			return nil
		}

		result, resultErr = measure(ctx, path)
		if resultErr != nil {
			return
		}
		select {
		case claimed := <-loserJoined:
			if claimed {
				t.Fatal("dormant workload candidate claimed the logical flow")
			}
		default:
			t.Fatal("workload returned before the dormant candidate handler joined")
		}
		select {
		case readErr := <-loserRead:
			if readErr == nil {
				t.Fatal("dormant workload candidate read unexpectedly succeeded")
			}
		case <-ctx.Done():
			t.Fatalf("dormant workload candidate was not closed: %v", ctx.Err())
		}
	})
	return result, resultErr
}

// An expected workload error still closes the injected loser and waits for its
// reader to release the final goroutine lifecycle credit before returning.
func TestFullTunDormantCandidateErrorJoinsLoserReader(t *testing.T) {
	if testing.Short() {
		return
	}
	safetyCtx, safetyCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer safetyCancel()
	expectedErr := errors.New("stop after dormant-candidate admission")
	loserReaderHeld := make(chan struct{})
	loserReaderWaitReached := make(chan struct{})
	releaseLoserReader := make(chan struct{})
	helperReturned := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseLoserReader)
		})
	}
	defer release()
	controllerResult := make(chan error, 1)
	go func() {
		waitBarrier := func(name string, barrier <-chan struct{}) error {
			select {
			case <-barrier:
				return nil
			case <-helperReturned:
				return fmt.Errorf("helper returned before %s", name)
			case <-safetyCtx.Done():
				return fmt.Errorf("wait for %s: %w", name, safetyCtx.Err())
			}
		}
		if err := waitBarrier("held loser reader", loserReaderHeld); err != nil {
			release()
			controllerResult <- err
			return
		}
		if err := waitBarrier("loser-reader join", loserReaderWaitReached); err != nil {
			release()
			controllerResult <- err
			return
		}
		select {
		case <-helperReturned:
			release()
			controllerResult <- errors.New("helper returned while loser reader was held")
			return
		default:
		}
		release()
		controllerResult <- nil
	}()
	_, err := measureFullTunTCPWorkloadAfterDormantAcceptedCandidate(
		t,
		3305,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			path.beforeWorkloadLoserDoneForTest = func() {
				close(loserReaderHeld)
				<-releaseLoserReader
			}
			path.beforeWorkloadLoserWaitForTest = func() {
				close(loserReaderWaitReached)
			}
			return measureFullTunUploadWithStartHook(
				ctx,
				path,
				64*1024,
				64*1024,
				func() error {
					return expectedErr
				},
			)
		},
	)
	close(helperReturned)
	if controllerErr := <-controllerResult; controllerErr != nil {
		t.Fatal(controllerErr)
	}
	if !errors.Is(err, expectedErr) {
		t.Fatalf("dormant-candidate workload error=%v, want=%v", err, expectedErr)
	}
}

// A warmed upload uses the later identified TUN socket when a dormant direct
// socket has already consumed the listener's first accept.
func TestMeasureFullTunUploadWithWarmupAndStartHookAfterDormantAcceptedCandidate(t *testing.T) {
	if testing.Short() {
		return
	}
	const warmupByteCount = 16 * 1024
	const byteCount = 32 * 1024
	var startHookCount atomic.Int64
	result := testFullTunTCPWorkloadAfterDormantAcceptedCandidate(
		t,
		3303,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunUploadWithWarmupAndStartHook(
				ctx,
				path,
				warmupByteCount,
				byteCount,
				warmupByteCount+byteCount,
				func() error {
					startHookCount.Add(1)
					return nil
				},
			)
		},
	)
	if startHookCount.Load() != 1 {
		t.Fatalf("upload start hook count=%d, want=1", startHookCount.Load())
	}
	if result.UsefulByteCount != byteCount ||
		result.WarmupByteCount != warmupByteCount ||
		result.ContentHash != deterministicPayloadHash(byteCount) {
		t.Fatalf("upload result=%+v", result)
	}
}

// A warmed download uses the later identified TUN socket when a dormant
// direct socket has already consumed the listener's first accept.
func TestMeasureFullTunDownloadWithWarmupAfterDormantAcceptedCandidate(t *testing.T) {
	if testing.Short() {
		return
	}
	const warmupByteCount = 16 * 1024
	const byteCount = 32 * 1024
	var startHookCount atomic.Int64
	result := testFullTunTCPWorkloadAfterDormantAcceptedCandidate(
		t,
		3304,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunDownloadWithWarmupAndStartHook(
				ctx,
				path,
				warmupByteCount,
				byteCount,
				warmupByteCount+byteCount,
				func() error {
					startHookCount.Add(1)
					return nil
				},
			)
		},
	)
	if startHookCount.Load() != 1 {
		t.Fatalf("download start hook count=%d, want=1", startHookCount.Load())
	}
	if result.UsefulByteCount != byteCount ||
		result.WarmupByteCount != warmupByteCount ||
		result.ContentHash != deterministicPayloadHash(byteCount) {
		t.Fatalf("download result=%+v", result)
	}
}

// Upload verification hashes an exact deterministic stream at provider egress.
func measureFullTunUpload(
	ctx context.Context,
	path *fullTunPath,
	byteCount int64,
) (workloadResult, error) {
	result, err := measureFullTunUploadWithDeadlineBytes(ctx, path, byteCount, byteCount)
	if err == nil {
		err = path.waitForPostWorkloadBoundary(ctx)
	}
	return result, err
}

// Warmed upload primes one route-local BDP on the measured connection, then
// resets content verification, timing, and carrier accounting for the payload.
func measureFullTunWarmedUpload(
	ctx context.Context,
	path *fullTunPath,
	warmupByteCount int64,
	byteCount int64,
) (workloadResult, error) {
	result, err := measureFullTunUploadWithWarmupAndStartHook(
		ctx,
		path,
		warmupByteCount,
		byteCount,
		warmupByteCount+byteCount,
		nil,
	)
	if err == nil {
		err = path.waitForPostWorkloadBoundary(ctx)
	}
	return result, err
}

// Parallel flows use their aggregate bytes to allow for fair sharing at one
// route bottleneck while still hashing each flow's exact payload independently.
func measureFullTunUploadWithDeadlineBytes(
	ctx context.Context,
	path *fullTunPath,
	byteCount int64,
	deadlineByteCount int64,
) (workloadResult, error) {
	return measureFullTunUploadWithStartHook(
		ctx,
		path,
		byteCount,
		deadlineByteCount,
		nil,
	)
}

// The optional hook gives cancellation tests an exact post-handshake boundary.
func measureFullTunUploadWithStartHook(
	ctx context.Context,
	path *fullTunPath,
	byteCount int64,
	deadlineByteCount int64,
	startHook func() error,
) (workloadResult, error) {
	return measureFullTunUploadWithWarmupAndStartHook(
		ctx,
		path,
		0,
		byteCount,
		deadlineByteCount,
		startHook,
	)
}

// One full-route upload implementation keeps cold and warmed validation on
// the same production path while placing the hook after any warmup barrier.
func measureFullTunUploadWithWarmupAndStartHook(
	ctx context.Context,
	path *fullTunPath,
	warmupByteCount int64,
	byteCount int64,
	deadlineByteCount int64,
	startHook func() error,
) (workloadResult, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return workloadResult{}, err
	}
	const flowId = logicalTCPFlowId(0)
	expectedWarmupHash := deterministicPayloadHash(warmupByteCount)
	flowServer := newLogicalTCPFlowServer(
		ctx,
		listener,
		1,
		func(receivedFlowId logicalTCPFlowId, connection net.Conn) error {
			defer func() {
				if path.beforeWorkloadServerCloseForTest != nil {
					path.beforeWorkloadServerCloseForTest()
				}
			}()
			if receivedFlowId != flowId {
				return fmt.Errorf("upload flow id=%d, want=%d", receivedFlowId, flowId)
			}
			deadline := boundedWorkloadDeadline(
				ctx,
				fullTunDirectionalWorkloadTimeout(path, true, deadlineByteCount),
			)
			if deadlineErr := connection.SetDeadline(deadline); deadlineErr != nil {
				return deadlineErr
			}
			stopInterrupt := interruptDeadlineOnContext(ctx, connection)
			defer stopInterrupt()
			if 0 < warmupByteCount {
				warmupHash := sha256.New()
				readByteCount, readErr := io.CopyN(warmupHash, connection, warmupByteCount)
				if readErr != nil {
					return readErr
				}
				if readByteCount != warmupByteCount ||
					hex.EncodeToString(warmupHash.Sum(nil)) != expectedWarmupHash {
					return fmt.Errorf("upload warmup content mismatch bytes=%d", readByteCount)
				}
				if path.beforeWarmedTcpAckForTest != nil {
					if err := path.beforeWarmedTcpAckForTest(ctx, true); err != nil {
						return err
					}
				}
				if err := writeFullTunAll(connection, []byte{1}); err != nil {
					return err
				}
			}
			hash := sha256.New()
			readByteCount, readErr := io.CopyN(hash, connection, byteCount)
			if readErr != nil {
				return readErr
			}
			if readByteCount != byteCount {
				return fmt.Errorf("upload server read %d/%d bytes", readByteCount, byteCount)
			}
			_, writeErr := connection.Write(hash.Sum(nil))
			return writeErr
		},
		path.workloadFlowServerSettingsForTest,
	)
	defer flowServer.CloseAndWait()
	if path.beforeWorkloadClientDialForTest != nil {
		if err := path.beforeWorkloadClientDialForTest(ctx, listener.Addr().String()); err != nil {
			return workloadResult{}, err
		}
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, fullTunRouteReadinessTimeout(path))
	setupStart := time.Now()
	connection, err := path.appTun.DialContext(dialCtx, "tcp", listener.Addr().String())
	dialCancel()
	if err != nil {
		return workloadResult{}, err
	}
	defer connection.Close()
	deadline := boundedWorkloadDeadline(
		ctx,
		fullTunDirectionalWorkloadTimeout(path, true, deadlineByteCount),
	)
	if err := connection.SetDeadline(deadline); err != nil {
		return workloadResult{}, err
	}
	stopInterrupt := interruptDeadlineOnContext(ctx, connection)
	defer stopInterrupt()
	if err := writeLogicalTCPFlowPreface(connection, flowId); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	select {
	case <-flowServer.Ready():
	case <-flowServer.Done():
		if serverErr := flowServer.Wait(); serverErr != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, serverErr)
		}
		select {
		case <-flowServer.Ready():
		default:
			return workloadResult{}, errors.New("full-TUN upload flow completed before readiness")
		}
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	setupDuration := time.Since(setupStart)
	warmupDuration := time.Duration(0)
	var warmupStart time.Time
	if 0 < warmupByteCount {
		warmupStart = time.Now()
		if err := writeDeterministicWorkloadPayload(
			ctx,
			connection,
			0,
			warmupByteCount,
		); err != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, err)
		}
		ack := make([]byte, 1)
		if _, err := io.ReadFull(connection, ack); err != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, err)
		}
	}
	if err := path.waitForSetupBoundary(ctx); err != nil {
		return workloadResult{}, fmt.Errorf("join upload setup/warmup boundary: %w", err)
	}
	carrierMeasurementStart, err := beginPerfvarCarrierMeasurement(path)
	if err != nil {
		return workloadResult{}, err
	}
	path.setCarrierMeasurementStart(carrierMeasurementStart)
	if 0 < warmupByteCount {
		warmupDuration = time.Since(warmupStart)
	}
	if startHook != nil {
		if err := startHook(); err != nil {
			return workloadResult{}, err
		}
	}
	if 0 < warmupByteCount && path.beforeWarmedTcpMeasuredForTest != nil {
		path.beforeWarmedTcpMeasuredForTest(true)
	}
	payload := deterministicPayload()
	expectedHash := sha256.New()
	var memoryBefore runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	startTime := time.Now()
	for remaining := byteCount; 0 < remaining; {
		chunk := payload
		if remaining < int64(len(chunk)) {
			chunk = payload[:remaining]
		}
		if err := writeFullTunAll(connection, chunk); err != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, err)
		}
		_, _ = expectedHash.Write(chunk)
		remaining -= int64(len(chunk))
	}
	actualHash := make([]byte, sha256.Size)
	if _, err := io.ReadFull(connection, actualHash); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	if !bytes.Equal(actualHash, expectedHash.Sum(nil)) {
		return workloadResult{}, fmt.Errorf("full-TUN upload hash mismatch")
	}
	if err := flowServer.Wait(); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	duration := time.Since(startTime)
	var memoryAfter runtime.MemStats
	runtime.ReadMemStats(&memoryAfter)
	return finishWorkloadResult(workloadResult{
		UsefulByteCount:        byteCount,
		WarmupByteCount:        warmupByteCount,
		WarmupDuration:         warmupDuration,
		DeliveredPacketCount:   1,
		Duration:               duration,
		SetupDuration:          setupDuration,
		ContentHash:            fmt.Sprintf("%x", actualHash),
		AllocatedByteCount:     memoryAfter.TotalAlloc - memoryBefore.TotalAlloc,
		AllocationCount:        memoryAfter.Mallocs - memoryBefore.Mallocs,
		GarbageCollectionCount: memoryAfter.NumGC - memoryBefore.NumGC,
		GarbageCollectionPause: time.Duration(memoryAfter.PauseTotalNs - memoryBefore.PauseTotalNs),
	}), nil
}

// Download verification hashes an exact deterministic provider-origin stream.
func measureFullTunDownload(
	ctx context.Context,
	path *fullTunPath,
	byteCount int64,
) (workloadResult, error) {
	result, err := measureFullTunDownloadWithDeadlineBytes(ctx, path, byteCount, byteCount)
	if err == nil {
		err = path.waitForPostWorkloadBoundary(ctx)
	}
	return result, err
}

// Warmed download primes the provider-to-application connection before an
// in-process barrier starts measured sending and carrier accounting exactly.
func measureFullTunWarmedDownload(
	ctx context.Context,
	path *fullTunPath,
	warmupByteCount int64,
	byteCount int64,
) (workloadResult, error) {
	result, err := measureFullTunDownloadWithWarmup(
		ctx,
		path,
		warmupByteCount,
		byteCount,
		warmupByteCount+byteCount,
	)
	if err == nil {
		err = path.waitForPostWorkloadBoundary(ctx)
	}
	return result, err
}

// Parallel downloads receive the same aggregate bottleneck allowance as
// parallel uploads.
func measureFullTunDownloadWithDeadlineBytes(
	ctx context.Context,
	path *fullTunPath,
	byteCount int64,
	deadlineByteCount int64,
) (workloadResult, error) {
	return measureFullTunDownloadWithWarmup(
		ctx,
		path,
		0,
		byteCount,
		deadlineByteCount,
	)
}

// One full-route download implementation shares the exact measured body while
// an optional local barrier prevents measured bytes racing ahead of the timer.
func measureFullTunDownloadWithWarmup(
	ctx context.Context,
	path *fullTunPath,
	warmupByteCount int64,
	byteCount int64,
	deadlineByteCount int64,
) (workloadResult, error) {
	return measureFullTunDownloadWithWarmupAndStartHook(
		ctx,
		path,
		warmupByteCount,
		byteCount,
		deadlineByteCount,
		nil,
	)
}

// The optional hook starts a live network trace after setup and any warmup,
// immediately before the measured provider-origin payload is released.
func measureFullTunDownloadWithWarmupAndStartHook(
	ctx context.Context,
	path *fullTunPath,
	warmupByteCount int64,
	byteCount int64,
	deadlineByteCount int64,
	startHook func() error,
) (workloadResult, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return workloadResult{}, err
	}
	const flowId = logicalTCPFlowId(0)
	payload := deterministicPayload()
	expectedWarmupHash := deterministicPayloadHash(warmupByteCount)
	expectedHash := sha256.New()
	for remaining := byteCount; 0 < remaining; {
		chunk := payload
		if remaining < int64(len(chunk)) {
			chunk = payload[:remaining]
		}
		_, _ = expectedHash.Write(chunk)
		remaining -= int64(len(chunk))
	}
	warmupAcknowledged := make(chan struct{})
	startMeasured := make(chan struct{})
	flowServer := newLogicalTCPFlowServer(
		ctx,
		listener,
		1,
		func(receivedFlowId logicalTCPFlowId, connection net.Conn) error {
			defer func() {
				if path.beforeWorkloadServerCloseForTest != nil {
					path.beforeWorkloadServerCloseForTest()
				}
			}()
			if receivedFlowId != flowId {
				return fmt.Errorf("download flow id=%d, want=%d", receivedFlowId, flowId)
			}
			deadline := boundedWorkloadDeadline(
				ctx,
				fullTunDirectionalWorkloadTimeout(path, false, deadlineByteCount),
			)
			if deadlineErr := connection.SetDeadline(deadline); deadlineErr != nil {
				return deadlineErr
			}
			stopInterrupt := interruptDeadlineOnContext(ctx, connection)
			defer stopInterrupt()
			if 0 < warmupByteCount {
				if err := writeDeterministicWorkloadPayload(
					ctx,
					connection,
					0,
					warmupByteCount,
				); err != nil {
					return err
				}
				ack := make([]byte, 1)
				if _, err := io.ReadFull(connection, ack); err != nil {
					return err
				}
				close(warmupAcknowledged)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-startMeasured:
			}
			for remaining := byteCount; 0 < remaining; {
				chunk := payload
				if remaining < int64(len(chunk)) {
					chunk = payload[:remaining]
				}
				if writeErr := writeFullTunAll(connection, chunk); writeErr != nil {
					return writeErr
				}
				remaining -= int64(len(chunk))
			}
			ack := make([]byte, 1)
			_, readErr := io.ReadFull(connection, ack)
			return readErr
		},
		path.workloadFlowServerSettingsForTest,
	)
	defer flowServer.CloseAndWait()
	if path.beforeWorkloadClientDialForTest != nil {
		if err := path.beforeWorkloadClientDialForTest(ctx, listener.Addr().String()); err != nil {
			return workloadResult{}, err
		}
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, fullTunRouteReadinessTimeout(path))
	setupStart := time.Now()
	connection, err := path.appTun.DialContext(dialCtx, "tcp", listener.Addr().String())
	dialCancel()
	if err != nil {
		return workloadResult{}, err
	}
	defer connection.Close()
	deadline := boundedWorkloadDeadline(
		ctx,
		fullTunDirectionalWorkloadTimeout(path, false, deadlineByteCount),
	)
	if err := connection.SetDeadline(deadline); err != nil {
		return workloadResult{}, err
	}
	stopInterrupt := interruptDeadlineOnContext(ctx, connection)
	defer stopInterrupt()
	if err := writeLogicalTCPFlowPreface(connection, flowId); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	select {
	case <-flowServer.Ready():
	case <-flowServer.Done():
		if serverErr := flowServer.Wait(); serverErr != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, serverErr)
		}
		select {
		case <-flowServer.Ready():
		default:
			return workloadResult{}, errors.New("full-TUN download flow completed before readiness")
		}
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	setupDuration := time.Since(setupStart)
	warmupDuration := time.Duration(0)
	var warmupStart time.Time
	if 0 < warmupByteCount {
		warmupStart = time.Now()
		warmupHash := sha256.New()
		readByteCount, readErr := io.CopyN(warmupHash, connection, warmupByteCount)
		if readErr != nil {
			return workloadResult{}, fmt.Errorf(
				"download warmup read %d/%d bytes: %w",
				readByteCount,
				warmupByteCount,
				contextBoundWorkloadError(ctx, readErr),
			)
		}
		if readByteCount != warmupByteCount ||
			hex.EncodeToString(warmupHash.Sum(nil)) != expectedWarmupHash {
			return workloadResult{}, fmt.Errorf("full-TUN download warmup content mismatch bytes=%d", readByteCount)
		}
		if path.beforeWarmedTcpAckForTest != nil {
			if err := path.beforeWarmedTcpAckForTest(ctx, false); err != nil {
				return workloadResult{}, err
			}
		}
		if err := writeFullTunAll(connection, []byte{1}); err != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, err)
		}
		select {
		case <-warmupAcknowledged:
		case <-flowServer.Done():
			return workloadResult{}, contextBoundWorkloadError(ctx, flowServer.Wait())
		case <-ctx.Done():
			return workloadResult{}, ctx.Err()
		}
	}
	if err := path.waitForSetupBoundary(ctx); err != nil {
		return workloadResult{}, fmt.Errorf("join download setup/warmup boundary: %w", err)
	}
	carrierMeasurementStart, err := beginPerfvarCarrierMeasurement(path)
	if err != nil {
		return workloadResult{}, err
	}
	path.setCarrierMeasurementStart(carrierMeasurementStart)
	if 0 < warmupByteCount {
		warmupDuration = time.Since(warmupStart)
	}
	var memoryBefore runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	if 0 < warmupByteCount && path.beforeWarmedTcpMeasuredForTest != nil {
		path.beforeWarmedTcpMeasuredForTest(false)
	}
	if startHook != nil {
		if err := startHook(); err != nil {
			// Release the server-side measurement barrier before deferred
			// connection close and flow-server join.
			close(startMeasured)
			return workloadResult{}, err
		}
	}
	startTime := time.Now()
	close(startMeasured)
	actualHash := sha256.New()
	readByteCount, err := io.CopyN(actualHash, connection, byteCount)
	if err != nil {
		return workloadResult{}, fmt.Errorf("download read %d/%d bytes: %w", readByteCount, byteCount, err)
	}
	if readByteCount != byteCount || !bytes.Equal(actualHash.Sum(nil), expectedHash.Sum(nil)) {
		return workloadResult{}, fmt.Errorf("full-TUN download content mismatch bytes=%d", readByteCount)
	}
	if err := writeFullTunAll(connection, []byte{1}); err != nil {
		return workloadResult{}, err
	}
	if err := flowServer.Wait(); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	duration := time.Since(startTime)
	var memoryAfter runtime.MemStats
	runtime.ReadMemStats(&memoryAfter)
	return finishWorkloadResult(workloadResult{
		UsefulByteCount:        byteCount,
		WarmupByteCount:        warmupByteCount,
		WarmupDuration:         warmupDuration,
		DeliveredPacketCount:   1,
		Duration:               duration,
		SetupDuration:          setupDuration,
		ContentHash:            fmt.Sprintf("%x", actualHash.Sum(nil)),
		AllocatedByteCount:     memoryAfter.TotalAlloc - memoryBefore.TotalAlloc,
		AllocationCount:        memoryAfter.Mallocs - memoryBefore.Mallocs,
		GarbageCollectionCount: memoryAfter.NumGC - memoryBefore.NumGC,
		GarbageCollectionPause: time.Duration(memoryAfter.PauseTotalNs - memoryBefore.PauseTotalNs),
	}), nil
}

// P2P statistics prove that the TUN payload used the requested production lane.
func (self *fullTunPath) verifyRoute() error {
	provider := self.providerStats.Snapshot()
	device := self.deviceStats.Snapshot()
	if fullTunRouteIsExchange(self.route) {
		if provider != (clientconnect.P2pDataPlaneStatsSnapshot{}) ||
			device != (clientconnect.P2pDataPlaneStatsSnapshot{}) {
			return fmt.Errorf("forced exchange used P2P provider=%+v device=%+v", provider, device)
		}
		return nil
	}
	if provider.FastFallbackCount != 0 || device.FastFallbackCount != 0 {
		return fmt.Errorf("full-TUN P2P fallback provider=%+v device=%+v", provider, device)
	}
	if self.deviceClient == nil || self.deviceClient.Load() == nil ||
		self.deviceSendRoutes == nil || self.providerSendRoutes == nil {
		return fmt.Errorf("full-TUN P2P route is missing forced platform controllers")
	}
	if err := self.deviceSendRoutes.verifyForcedDestination(self.providerClientId); err != nil {
		return fmt.Errorf("verify device forced route: %w", err)
	}
	if err := self.providerSendRoutes.verifyForcedDestination(
		self.deviceClient.Load().ClientId(),
	); err != nil {
		return fmt.Errorf("verify provider forced route: %w", err)
	}
	if self.route == fullTunRouteP2pFast &&
		(device.FastSendMessageCount == 0 || device.FastReceiveMessageCount == 0) {
		return fmt.Errorf("full-TUN fast route counters provider=%+v device=%+v", provider, device)
	}
	if self.route == fullTunRouteP2pLegacy &&
		(device.LegacySendMessageCount == 0 || device.LegacyReceiveMessageCount == 0) {
		return fmt.Errorf("full-TUN legacy route counters provider=%+v device=%+v", provider, device)
	}
	return nil
}

// One helper keeps the four top-level correctness tests identical and fresh.
func testFullTunRouteCorrectness(t *testing.T, route fullTunRoute) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4001)["clean-lan"]
		enableNetworkPeers := route == fullTunRouteP2pFast || route == fullTunRouteP2pLegacy
		environment := newRouteEnvironmentWithNetworkPeers(
			ctx,
			t,
			profile,
			enableNetworkPeers,
		)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, route)
		defer path.close()
		upload, err := measureFullTunUpload(ctx, path, 256*1024)
		if err != nil {
			t.Fatalf("full-TUN %s upload: %v", route, err)
		}
		download, err := measureFullTunDownload(ctx, path, 256*1024)
		if err != nil {
			t.Fatalf("full-TUN %s download: %v", route, err)
		}
		if upload.UsefulByteCount == 0 || download.UsefulByteCount == 0 {
			t.Fatalf("full-TUN %s empty result upload=%+v download=%+v", route, upload, download)
		}
		if route == fullTunRouteExchangeAuto {
			devicePackets := path.multiClient.PacketStats()
			providerPackets := path.providerRemoteNat.PacketStats()
			deviceAffinity := clientconnect.DirectCarrierAffinityStats{}
			if currentDeviceClient := path.deviceClient.Load(); currentDeviceClient != nil {
				deviceAffinity = currentDeviceClient.RouteManager().DirectCarrierAffinityStats()
			}
			providerAffinity := path.providerClient.RouteManager().DirectCarrierAffinityStats()
			t.Logf(
				"[perfvar] auto-packet-stats device-h1=%+v device-h3=%+v provider-h1=%+v provider-h3=%+v device-affinity=%+v provider-affinity=%+v",
				devicePackets.TransportStats[clientconnect.TransportTypeH1],
				devicePackets.TransportStats[clientconnect.TransportTypeH3],
				providerPackets.TransportStats[clientconnect.TransportTypeH1],
				providerPackets.TransportStats[clientconnect.TransportTypeH3],
				deviceAffinity,
				providerAffinity,
			)
		}
		if err := path.verifyRoute(); err != nil {
			t.Fatal(err)
		}
	})
}

// Exchange H1 carries exact bidirectional full-TUN TCP traffic.
func TestFullTunExchangeH1Correctness(t *testing.T) {
	testFullTunRouteCorrectness(t, fullTunRouteExchangeH1)
}

// Exchange H3 carries exact bidirectional full-TUN TCP traffic.
func TestFullTunExchangeH3Correctness(t *testing.T) {
	testFullTunRouteCorrectness(t, fullTunRouteExchangeH3)
}

// Exchange Auto keeps both equal-priority direct carriers live while carrying
// exact bidirectional full-TUN TCP traffic.
func TestFullTunExchangeAutoCorrectness(t *testing.T) {
	testFullTunRouteCorrectness(t, fullTunRouteExchangeAuto)
}

// Legacy WebRTC DataChannel carries exact bidirectional full-TUN TCP traffic.
func TestFullTunP2pLegacyCorrectness(t *testing.T) {
	testFullTunRouteCorrectness(t, fullTunRouteP2pLegacy)
}

// Fast WebRTC media carries exact bidirectional full-TUN TCP traffic.
func TestFullTunP2pFastCorrectness(t *testing.T) {
	testFullTunRouteCorrectness(t, fullTunRouteP2pFast)
}

// Exchange and fast P2P both carry an acknowledged BDP warmup and a separately
// hashed measured phase over one production full-TUN TCP connection.
func TestFullTunWarmedTCPRouteCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4003)["clean-lan"]
		for _, route := range []fullTunRoute{fullTunRouteExchangeH1, fullTunRouteP2pFast} {
			enableNetworkPeers := route == fullTunRouteP2pFast
			environment := newRouteEnvironmentWithNetworkPeers(
				ctx,
				t,
				profile,
				enableNetworkPeers,
			)
			path := newFullTunPath(ctx, t, environment, route)
			for _, direction := range []perfvarDirection{
				perfvarDirectionUpload,
				perfvarDirectionDownload,
			} {
				scenario := perfvarScenario{
					Route:                 route,
					Profile:               profile,
					ProviderAccessProfile: profile,
					Workload:              perfvarWorkloadTCPWarmed,
					Direction:             direction,
					Topology:              perfvarTopologyOneHop,
					Resource:              perfvarResourceDefault,
					PayloadByteCount:      128 * 1024,
					FlowCount:             1,
				}
				scenario.WarmupByteCount = perfvarDirectionalBandwidthDelayByteCount(scenario)
				result, err := measurePerfvarFullTun(ctx, path, scenario)
				if err != nil {
					path.close()
					environment.close()
					t.Fatalf("%s/%s warmed TCP: %v", route, direction, err)
				}
				if result.WarmupByteCount != scenario.WarmupByteCount ||
					result.WarmupDuration <= 0 ||
					result.UsefulByteCount != scenario.PayloadByteCount ||
					result.ContentHash != deterministicPayloadHash(scenario.PayloadByteCount) {
					path.close()
					environment.close()
					t.Fatalf("%s/%s warmed result=%+v scenario=%+v", route, direction, result, scenario)
				}
				if path.takeCarrierMeasurementStart() == nil {
					path.close()
					environment.close()
					t.Fatalf("%s/%s did not publish a measured carrier boundary", route, direction)
				}
			}
			if err := path.verifyRoute(); err != nil {
				path.close()
				environment.close()
				t.Fatal(err)
			}
			path.close()
			environment.close()
		}
	})
}

// The measured phase cannot begin or freeze its carrier counters while the
// exact final warmup Pack still owns terminal lifecycle work.
func TestFullTunWarmedTCPWaitsForFinalWarmupPackTerminal(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4004)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()
		for _, direction := range []perfvarDirection{
			perfvarDirectionUpload,
			perfvarDirectionDownload,
		} {
			terminalHeld := make(chan struct{})
			releaseTerminal := make(chan struct{})
			measuredStarted := make(chan struct{})
			var holdOnce sync.Once
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(releaseTerminal) })
			path.beforeWarmedTcpAckForTest = func(ctx context.Context, upload bool) error {
				if _, _, ok := path.waitForPackAndCarrierTerminalIdle(ctx); !ok {
					return fmt.Errorf("join pre-ACK warmup Pack and carrier boundary: %v", ctx.Err())
				}
				tracker := path.devicePackSends
				if upload {
					tracker = path.providerPackSends
				}
				tracker.setBeforeTerminalReleaseForTest(func(
					_ *sendPackLifecycleEntry,
					_ clientconnect.SendPackLifecycleObservation,
				) {
					holdOnce.Do(func() {
						close(terminalHeld)
						<-releaseTerminal
					})
				})
				return nil
			}
			path.beforeWarmedTcpMeasuredForTest = func(bool) {
				close(measuredStarted)
			}
			scenario := perfvarScenario{
				Route:                 fullTunRouteExchangeH1,
				Profile:               profile,
				ProviderAccessProfile: profile,
				Workload:              perfvarWorkloadTCPWarmed,
				Direction:             direction,
				Topology:              perfvarTopologyOneHop,
				Resource:              perfvarResourceDefault,
				PayloadByteCount:      64 * 1024,
				FlowCount:             1,
			}
			scenario.WarmupByteCount = perfvarDirectionalBandwidthDelayByteCount(scenario)
			workloadDone := make(chan struct {
				result workloadResult
				err    error
			}, 1)
			go func() {
				result, err := measurePerfvarFullTun(ctx, path, scenario)
				workloadDone <- struct {
					result workloadResult
					err    error
				}{result: result, err: err}
			}()
			select {
			case <-terminalHeld:
			case <-ctx.Done():
				t.Fatalf("%s warmup terminal was not held: %v", direction, ctx.Err())
			}
			select {
			case <-measuredStarted:
				t.Fatalf("%s measured body started while warmup terminal was held", direction)
			default:
			}
			path.measurementLock.Lock()
			carrierStartFrozen := path.carrierMeasurementStart != nil
			path.measurementLock.Unlock()
			if carrierStartFrozen {
				t.Fatalf("%s carrier start froze while warmup terminal was held", direction)
			}
			select {
			case completion := <-workloadDone:
				t.Fatalf("%s workload returned while warmup terminal was held: %+v", direction, completion)
			default:
			}
			releaseOnce.Do(func() { close(releaseTerminal) })
			var completion struct {
				result workloadResult
				err    error
			}
			select {
			case completion = <-workloadDone:
			case <-ctx.Done():
				t.Fatalf("%s warmed workload did not complete after terminal release: %v", direction, ctx.Err())
			}
			if completion.err != nil {
				t.Fatalf("%s warmed workload: %v", direction, completion.err)
			}
			if completion.result.UsefulByteCount != scenario.PayloadByteCount ||
				completion.result.WarmupByteCount != scenario.WarmupByteCount {
				t.Fatalf("%s warmed result=%+v", direction, completion.result)
			}
			if path.takeCarrierMeasurementStart() == nil {
				t.Fatalf("%s carrier start was not published after terminal release", direction)
			}
			path.providerPackSends.setBeforeTerminalReleaseForTest(nil)
			path.devicePackSends.setBeforeTerminalReleaseForTest(nil)
			path.beforeWarmedTcpAckForTest = nil
			path.beforeWarmedTcpMeasuredForTest = nil
		}
	})
}

// Established full-route TCP is held at an exact post-blackhole P2P carrier
// submission, then explicit cancellation must stop its blocked application I/O.
func TestFullTunTCPCancelsAfterCarrierBlackhole(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		routeCtx, routeCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer routeCancel()
		profile := initialNetworkProfiles(4002)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(routeCtx, t, profile, true)
		defer environment.close()
		path := newFullTunPath(routeCtx, t, environment, fullTunRouteP2pFast)
		defer path.close()
		livenessCtx, livenessCancel := context.WithTimeout(routeCtx, 2*time.Minute)
		defer livenessCancel()
		workloadCtx, workloadCancel := context.WithCancel(livenessCtx)
		defer workloadCancel()
		barrierPublished := make(chan (<-chan struct{}), 1)
		completion := make(chan error, 1)
		go func() {
			_, err := measureFullTunUploadWithStartHook(
				workloadCtx,
				path,
				8*1024*1024,
				8*1024*1024,
				func() error {
					if err := path.p2pNetwork.setBlackhole(true, true); err != nil {
						return err
					}
					barrierPublished <- holdLinkScheduleForTest(
						path.p2pNetwork.directionalLinks(),
						func(observation linkScheduleObservation) bool {
							return observation.terminalDropCause == linkTerminalDropOutage &&
								1000 <= observation.packetByteCount
						},
						workloadCtx.Done(),
					)
					return nil
				},
			)
			completion <- err
		}()
		var carrierHeld <-chan struct{}
		select {
		case carrierHeld = <-barrierPublished:
		case err := <-completion:
			t.Fatalf("full-route TCP returned before publishing its P2P carrier barrier: %v", err)
		case <-livenessCtx.Done():
			t.Fatalf("publish full-route TCP P2P carrier barrier: %v", livenessCtx.Err())
		}
		select {
		case <-carrierHeld:
		case err := <-completion:
			t.Fatalf("full-route TCP returned before a post-blackhole P2P submission: %v", err)
		case <-livenessCtx.Done():
			t.Fatalf("wait for full-route TCP post-blackhole P2P submission: %v", livenessCtx.Err())
		}
		workloadCancel()
		select {
		case err := <-completion:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("full-route TCP blackhole error=%v, want explicit context cancellation", err)
			}
		case <-livenessCtx.Done():
			t.Fatalf("full-route TCP did not stop after explicit cancellation: %v", livenessCtx.Err())
		}
	})
}

// Request payloads for future UDP fixtures use a stable network byte order.
func fullTunSequencePayload(sequence uint64, byteCount int) []byte {
	payload := make([]byte, byteCount)
	binary.BigEndian.PutUint64(payload, sequence)
	for index := 8; index < len(payload); index += 1 {
		payload[index] = byte((int(sequence) + index) % 251)
	}
	return payload
}
