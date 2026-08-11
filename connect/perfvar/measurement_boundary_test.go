// This file verifies the unified upstream-to-carrier measurement start. Its
// barriers force source work to pause before SendPack without a database route.
package perfvar

import (
	"context"
	"net"
	"sync"
	"testing"

	clientconnect "github.com/urnetwork/connect"
)

// A minimal route retains every lifecycle object used by the fixed point but
// has no physical links, so source ordering is isolated from transport timing.
type fullTunMeasurementBoundaryTestFixture struct {
	ctx      context.Context
	cancel   context.CancelFunc
	network  *simulatedIPNetwork
	path     *fullTunPath
	packEnds []*sendPackLifecycleTracker
}

// Construction starts empty source and Pack owners with a positive readiness
// fence; each test injects the exact upstream generation it needs.
func newFullTunMeasurementBoundaryTestFixture() *fullTunMeasurementBoundaryTestFixture {
	ctx, cancel := context.WithCancel(context.Background())
	network := newSimulatedIPNetwork(ctx)
	devicePackSends := newSendPackLifecycleTracker()
	providerPackSends := newSendPackLifecycleTracker()
	path := &fullTunPath{
		ctx:               ctx,
		environment:       &routeEnvironment{network: network},
		deviceStats:       &clientconnect.P2pDataPlaneStats{},
		providerStats:     &clientconnect.P2pDataPlaneStats{},
		bridgeSends:       newFullTunBridgeSendTracker(),
		providerReturns:   newProviderReturnSendTracker(),
		devicePackSends:   devicePackSends,
		providerPackSends: providerPackSends,
	}
	path.readinessAppFence.Store(true)
	return &fullTunMeasurementBoundaryTestFixture{
		ctx:      ctx,
		cancel:   cancel,
		network:  network,
		path:     path,
		packEnds: []*sendPackLifecycleTracker{devicePackSends, providerPackSends},
	}
}

// Shutdown joins every owner created by the minimal fixture.
func (self *fullTunMeasurementBoundaryTestFixture) close() {
	for _, tracker := range self.packEnds {
		tracker.close()
	}
	self.path.providerReturns.close()
	self.network.close()
	self.cancel()
}

// One helper consumes the prepared start and verifies that no second carrier
// epoch was needed between the fixed point and its public begin call.
func requirePreparedCarrierStart(
	t *testing.T,
	fixture *fullTunMeasurementBoundaryTestFixture,
) {
	t.Helper()
	boundary, err := beginPerfvarCarrierMeasurement(fixture.path)
	if err != nil {
		t.Fatalf("consume prepared carrier start: %v", err)
	}
	if !perfvarCarrierGenerationStable(fixture.path, boundary) {
		t.Fatal("prepared carrier generation was not stable at consumption")
	}
}

// A bridge item held after source entry but before parsing or AppDelay keeps
// the baseline unpublished until that exact item reaches terminal ownership.
func TestFullTunMeasurementBoundaryJoinsHeldBridgePublication(t *testing.T) {
	fixture := newFullTunMeasurementBoundaryTestFixture()
	defer fixture.close()
	publishEntered := make(chan struct{})
	releasePublish := make(chan struct{})
	boundaryJoinedPublisher := make(chan struct{})
	releaseTerminal := make(chan struct{})
	startPublished := make(chan struct{})
	var publishOnce sync.Once
	fixture.path.bridgeSends.setBeforeStartPublishForTest(func() {
		publishOnce.Do(func() {
			close(publishEntered)
			<-releasePublish
		})
	})
	var boundaryOnce sync.Once
	fixture.path.bridgeSends.setBeforePublisherWaitForTest(func() {
		boundaryOnce.Do(func() { close(boundaryJoinedPublisher) })
	})
	go func() {
		entry := fixture.path.bridgeSends.start(0, fullTunBridgeFlowKey{}, 100)
		close(startPublished)
		<-releaseTerminal
		fixture.path.bridgeSends.terminal(entry, true)
	}()
	<-publishEntered
	boundaryResult := make(chan error, 1)
	go func() {
		boundaryResult <- fixture.path.waitForMeasurementBoundary(fixture.ctx)
	}()
	<-boundaryJoinedPublisher
	select {
	case err := <-boundaryResult:
		t.Fatalf("baseline passed held bridge publication: %v", err)
	default:
	}
	close(releasePublish)
	<-startPublished
	select {
	case err := <-boundaryResult:
		t.Fatalf("baseline passed live bridge source: %v", err)
	default:
	}
	close(releaseTerminal)
	if err := <-boundaryResult; err != nil {
		t.Fatal(err)
	}
	requirePreparedCarrierStart(t, fixture)
}

// A provider return Started callback held before its queue publication keeps
// the download baseline unpublished until its matching Completed callback.
func TestFullTunMeasurementBoundaryJoinsHeldProviderPublication(t *testing.T) {
	fixture := newFullTunMeasurementBoundaryTestFixture()
	defer fixture.close()
	flowKey := providerReturnTrackerTestFlow(41)
	publishEntered := make(chan struct{})
	releasePublish := make(chan struct{})
	boundaryJoinedPublisher := make(chan struct{})
	releaseTerminal := make(chan struct{})
	startPublished := make(chan struct{})
	var publishOnce sync.Once
	fixture.path.providerReturns.setBeforeObserverPublishForTest(func(
		observation clientconnect.RemoteUserNatProviderReturnSendObservation,
	) {
		if observation.Phase == clientconnect.RemoteUserNatProviderReturnSendPhaseStarted {
			publishOnce.Do(func() {
				close(publishEntered)
				<-releasePublish
			})
		}
	})
	var boundaryOnce sync.Once
	fixture.path.providerReturns.setBeforePublisherWaitForTest(func() {
		boundaryOnce.Do(func() { close(boundaryJoinedPublisher) })
	})
	go func() {
		observeProviderReturnStarted(fixture.path.providerReturns, 1, flowKey, 1, 100)
		close(startPublished)
		<-releaseTerminal
		observeProviderReturnCompleted(fixture.path.providerReturns, 1, flowKey, 1, 100, true)
	}()
	<-publishEntered
	boundaryResult := make(chan error, 1)
	go func() {
		boundaryResult <- fixture.path.waitForMeasurementBoundary(fixture.ctx)
	}()
	<-boundaryJoinedPublisher
	select {
	case err := <-boundaryResult:
		t.Fatalf("baseline passed held provider publication: %v", err)
	default:
	}
	close(releasePublish)
	<-startPublished
	select {
	case err := <-boundaryResult:
		t.Fatalf("baseline passed live provider source: %v", err)
	default:
	}
	close(releaseTerminal)
	if err := <-boundaryResult; err != nil {
		t.Fatal(err)
	}
	requirePreparedCarrierStart(t, fixture)
}

// The post-workload fixed point also starts above SendPack, so a final bridge
// item held in an AppDelay-equivalent gap cannot cross carrier observation.
func TestFullTunPostWorkloadBoundaryJoinsHeldBridgeBeforePack(t *testing.T) {
	fixture := newFullTunMeasurementBoundaryTestFixture()
	defer fixture.close()
	publishEntered := make(chan struct{})
	releasePublish := make(chan struct{})
	boundaryJoinedPublisher := make(chan struct{})
	releaseTerminal := make(chan struct{})
	startPublished := make(chan struct{})
	var publishOnce sync.Once
	fixture.path.bridgeSends.setBeforeStartPublishForTest(func() {
		publishOnce.Do(func() {
			close(publishEntered)
			<-releasePublish
		})
	})
	var boundaryOnce sync.Once
	fixture.path.bridgeSends.setBeforePublisherWaitForTest(func() {
		boundaryOnce.Do(func() { close(boundaryJoinedPublisher) })
	})
	go func() {
		entry := fixture.path.bridgeSends.start(0, fullTunBridgeFlowKey{}, 100)
		close(startPublished)
		<-releaseTerminal
		fixture.path.bridgeSends.terminal(entry, true)
	}()
	<-publishEntered
	boundaryResult := make(chan error, 1)
	go func() {
		boundaryResult <- fixture.path.waitForPostWorkloadBoundary(fixture.ctx)
	}()
	<-boundaryJoinedPublisher
	select {
	case err := <-boundaryResult:
		t.Fatalf("post-workload boundary passed held bridge publication: %v", err)
	default:
	}
	close(releasePublish)
	<-startPublished
	select {
	case err := <-boundaryResult:
		t.Fatalf("post-workload boundary passed live bridge source: %v", err)
	default:
	}
	close(releaseTerminal)
	if err := <-boundaryResult; err != nil {
		t.Fatal(err)
	}
}

// The symmetric provider source fence prevents a final return item held before
// its first Pack from escaping the post-workload carrier observation.
func TestFullTunPostWorkloadBoundaryJoinsHeldProviderBeforePack(t *testing.T) {
	fixture := newFullTunMeasurementBoundaryTestFixture()
	defer fixture.close()
	flowKey := providerReturnTrackerTestFlow(45)
	publishEntered := make(chan struct{})
	releasePublish := make(chan struct{})
	boundaryJoinedPublisher := make(chan struct{})
	releaseTerminal := make(chan struct{})
	startPublished := make(chan struct{})
	var publishOnce sync.Once
	fixture.path.providerReturns.setBeforeObserverPublishForTest(func(
		observation clientconnect.RemoteUserNatProviderReturnSendObservation,
	) {
		if observation.Phase == clientconnect.RemoteUserNatProviderReturnSendPhaseStarted {
			publishOnce.Do(func() {
				close(publishEntered)
				<-releasePublish
			})
		}
	})
	var boundaryOnce sync.Once
	fixture.path.providerReturns.setBeforePublisherWaitForTest(func() {
		boundaryOnce.Do(func() { close(boundaryJoinedPublisher) })
	})
	go func() {
		observeProviderReturnStarted(fixture.path.providerReturns, 1, flowKey, 1, 100)
		close(startPublished)
		<-releaseTerminal
		observeProviderReturnCompleted(fixture.path.providerReturns, 1, flowKey, 1, 100, true)
	}()
	<-publishEntered
	boundaryResult := make(chan error, 1)
	go func() {
		boundaryResult <- fixture.path.waitForPostWorkloadBoundary(fixture.ctx)
	}()
	<-boundaryJoinedPublisher
	select {
	case err := <-boundaryResult:
		t.Fatalf("post-workload boundary passed held provider publication: %v", err)
	default:
	}
	close(releasePublish)
	<-startPublished
	select {
	case err := <-boundaryResult:
		t.Fatalf("post-workload boundary passed live provider source: %v", err)
	default:
	}
	close(releaseTerminal)
	if err := <-boundaryResult; err != nil {
		t.Fatal(err)
	}
}

// A carrier-only packet that enters after the first terminal-idle pass forces
// a new end candidate. Traffic entering after the accepted candidate remains
// outside the frozen observation instead of leaking through a later snapshot.
func TestFullTunPostWorkloadBoundaryRetriesAndFreezesCarrierGeneration(t *testing.T) {
	fixture := newFullTunMeasurementBoundaryTestFixture()
	defer fixture.close()
	link := newDirectionalLink(
		fixture.ctx,
		testP2pLinkProfile(1500, oversizeModeDrop),
		8101,
		func([]byte) bool { return true },
	)
	fixture.network.links[tunLinkKey{source: "source", destination: "destination"}] = link
	start, err := beginPerfvarCarrierMeasurementNow(fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	attemptCount := 0
	var injectionErr error
	fixture.path.afterCarrierEndCandidateForTest = func(attempt int) {
		attemptCount = attempt
		if attempt != 1 {
			return
		}
		if _, submitErr := link.submit([]byte{1}); submitErr != nil {
			injectionErr = submitErr
			return
		}
		if !waitForDirectionalLinksTerminalIdle(fixture.ctx, []*directionalLink{link}, nil) {
			injectionErr = fixture.ctx.Err()
		}
	}
	if err := fixture.path.waitForPostWorkloadBoundary(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if injectionErr != nil {
		t.Fatalf("inject crossing carrier packet: %v", injectionErr)
	}
	if attemptCount != 2 {
		t.Fatalf("carrier end attempts=%d, want 2", attemptCount)
	}
	if _, err := link.submit([]byte{2}); err != nil {
		t.Fatalf("submit post-boundary carrier packet: %v", err)
	}
	if !waitForDirectionalLinksTerminalIdle(fixture.ctx, []*directionalLink{link}, nil) {
		t.Fatalf("join post-boundary carrier packet: %v", fixture.ctx.Err())
	}
	observation := observePerfvarWorkloadCarrier(fixture.path, start)
	linkObservation := observation.Links["source->destination"]
	if linkObservation.AdmittedPacketCount != 1 || linkObservation.AdmittedByteCount != 1 {
		t.Fatalf("frozen carrier interval included later traffic: %+v", linkObservation)
	}
}

// A bridge generation crossing the first carrier start forces a second pass;
// its terminal setup failure remains entirely before the accepted baseline.
func TestFullTunMeasurementBoundaryRetriesBridgeGenerationAfterFailure(t *testing.T) {
	fixture := newFullTunMeasurementBoundaryTestFixture()
	defer fixture.close()
	attemptCount := 0
	fixture.path.afterCarrierStartForTest = func(attempt int) {
		attemptCount = attempt
		if attempt == 1 {
			entry := fixture.path.bridgeSends.start(0, fullTunBridgeFlowKey{}, 100)
			fixture.path.bridgeSends.terminal(entry, false)
		}
	}
	boundary, err := beginPerfvarCarrierMeasurement(fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	if attemptCount != 2 {
		t.Fatalf("bridge carrier-start attempts=%d, want 2", attemptCount)
	}
	if !perfvarCarrierGenerationStable(fixture.path, boundary) {
		t.Fatal("direct unprepared begin returned an unstable carrier generation")
	}
}

// A provider generation crossing the first carrier start also retries, while
// a fully terminal failed setup return cannot poison the replacement baseline.
func TestFullTunMeasurementBoundaryRetriesProviderGenerationAfterFailure(t *testing.T) {
	fixture := newFullTunMeasurementBoundaryTestFixture()
	defer fixture.close()
	flowKey := providerReturnTrackerTestFlow(42)
	attemptCount := 0
	fixture.path.afterCarrierStartForTest = func(attempt int) {
		attemptCount = attempt
		if attempt == 1 {
			observeProviderReturnStarted(fixture.path.providerReturns, 1, flowKey, 1, 100)
			observeProviderReturnCompleted(fixture.path.providerReturns, 1, flowKey, 1, 100, false)
		}
	}
	if err := fixture.path.waitForMeasurementBoundary(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 2 {
		t.Fatalf("provider carrier-start attempts=%d, want 2", attemptCount)
	}
	requirePreparedCarrierStart(t, fixture)
}

// A direct-carrier packet injected after access-link resets but before P2P
// resets would be absorbed by the later baseline without the pre-reset view.
func TestFullTunMeasurementBoundaryRetriesCarrierCrossingBeforeLaterReset(t *testing.T) {
	fixture := newFullTunMeasurementBoundaryTestFixture()
	defer fixture.close()
	profile := initialNetworkProfiles(20260814)["clean-lan"]
	p2pNetwork, err := newP2pNetwork(profile)
	if err != nil {
		t.Fatal(err)
	}
	defer p2pNetwork.close()
	fixture.path.p2pNetwork = p2pNetwork
	attemptCount := 0
	var hookErr error
	fixture.path.afterAccessCarrierStartForTest = func() {
		attemptCount += 1
		if attemptCount != 1 {
			return
		}
		if _, submitErr := p2pNetwork.forwardLink.submit([]byte{1}); submitErr != nil {
			hookErr = submitErr
			return
		}
		if !p2pNetwork.waitForTerminalIdle(fixture.ctx) {
			hookErr = fixture.ctx.Err()
		}
	}
	boundary, err := beginPerfvarCarrierMeasurement(fixture.path)
	if hookErr != nil {
		t.Fatalf("inject between-object carrier submission: %v", hookErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if attemptCount != 2 {
		t.Fatalf("between-object carrier-start attempts=%d, want 2", attemptCount)
	}
	if !perfvarCarrierGenerationStable(fixture.path, boundary) {
		t.Fatal("replacement carrier generation was unstable")
	}
}

// A stable destination-scoped UDP backlog crosses the complete measurement
// lifecycle as an absolute baseline. A read-and-replacement generation during
// the first start candidate forces an exact retry; the frozen workload delta
// then remains zero and passes receive-credit validation.
func TestFullTunMeasurementBoundaryRetainsStableP2pSocketBacklog(t *testing.T) {
	fixture := newFullTunMeasurementBoundaryTestFixture()
	defer fixture.close()
	profile := initialNetworkProfiles(20260815)["clean-lan"]
	p2pNetwork, err := newP2pNetwork(profile)
	if err != nil {
		t.Fatalf("create stable-backlog P2P network: %v", err)
	}
	defer p2pNetwork.close()
	fixture.path.p2pNetwork = p2pNetwork
	credits := p2pNetwork.forwardReceiveCredits
	destination := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 53301}
	socket := credits.registerSocket(destination, nil, nil)
	reservation, hasDestination, admitted := credits.reserveForAddress(fixture.ctx, destination)
	if !hasDestination || !admitted || reservation == nil {
		t.Fatal("initial stable socket backlog was not admitted")
	}
	attemptCount := 0
	var replacement *p2pReceiveCreditReservation
	fixture.path.afterCarrierStartForTest = func(attempt int) {
		attemptCount = attempt
		if attempt != 1 {
			return
		}
		socket.recordRead(1, nil)
		var replacementDestination bool
		var replacementAdmitted bool
		replacement, replacementDestination, replacementAdmitted = credits.reserveForAddress(
			fixture.ctx,
			destination,
		)
		if !replacementDestination || !replacementAdmitted || replacement == nil {
			t.Error("replacement stable socket backlog was not admitted")
		}
	}
	if err := fixture.path.waitForMeasurementBoundary(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 2 {
		t.Fatalf("stable-backlog carrier-start attempts=%d, want 2", attemptCount)
	}
	start, err := beginPerfvarCarrierMeasurement(fixture.path)
	if err != nil {
		t.Fatalf("consume stable-backlog carrier start: %v", err)
	}
	if err := fixture.path.waitForPostWorkloadBoundary(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	observation := observePerfvarWorkloadCarrier(fixture.path, start)
	delta := observation.P2PNetwork.ForwardReceiveCredits
	if delta.AdmittedPacketCount != 0 || delta.ReadPacketCount != 0 ||
		delta.CanceledPacketCount != 0 || delta.OutstandingPacketCount != 0 ||
		delta.PendingAcquireCount != 0 || delta.TrackedReservationCount != 0 ||
		delta.RouterPendingPacketCount != 0 ||
		delta.MaximumOutstandingPackets != 1 {
		t.Fatalf("stable-backlog workload delta=%+v", delta)
	}
	if reason := perfvarReceiveCreditReason("stable backlog", delta); reason != "" {
		t.Fatalf("stable-backlog validation: %s", reason)
	}
	if replacement == nil {
		t.Fatal("stable-backlog replacement was not retained")
	}
	replacement.cancel()
	socket.close()
}
