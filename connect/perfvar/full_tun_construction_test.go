// This file forces every full-TUN construction exit and proves the partial
// ownership transaction reaches one deterministic, pool-balanced fixed point.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// One homogeneous matrix row selects the partial topology and exact cleanup
// dispositions expected after its injected construction failure.
type fullTunConstructionFailureCase struct {
	name              string
	stage             fullTunConstructionStage
	route             fullTunRoute
	p2pHopCount       int
	expectedResources []fullTunConstructionResource
}

// A lightweight partial graph proves rollback continues after one error and
// remains exactly-once when a caller repeats the transaction disposition.
func TestFullTunConstructionOwnerRollbackClosesIndependentResourcesAfterError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	poolOutstandingBefore := routeMessagePoolOutstanding()
	linkProfile := testP2pLinkProfile(1500, oversizeModeDrop)
	path := &fullTunPath{
		t:                t,
		deviceTransports: newPlatformTransportOwner(),
		platformSendRoutes: []*platformSendRouteController{
			newPlatformSendRouteController(clientconnect.NewId()),
			newPlatformSendRouteController(clientconnect.NewId()),
		},
		providerNoAckSends: newNoAckSendTracker(),
		deviceNoAckSends:   newNoAckSendTracker(),
		providerPackSends:  newSendPackLifecycleTracker(),
		devicePackSends:    newSendPackLifecycleTracker(),
		providerReturns:    newProviderReturnSendTracker(),
		p2pNetwork: &p2pNetwork{
			forwardLink:           newDirectionalLink(ctx, linkProfile, 7313, nil),
			reverseLink:           newDirectionalLink(ctx, linkProfile, 7314, nil),
			forwardReceiveCredits: newP2pReceiveCredits(p2pVnetReceiveCreditPacketCount),
			reverseReceiveCredits: newP2pReceiveCredits(p2pVnetReceiveCreditPacketCount),
		},
	}
	cleanupErr := errors.New("injected independent cleanup failure")
	cleanupCounts := map[fullTunConstructionResource]int{}
	path.constructionCleanupErrorForTest = func(resource fullTunConstructionResource) error {
		if resource == fullTunConstructionResourceP2pNetwork {
			return cleanupErr
		}
		return nil
	}
	path.afterConstructionCleanupForTest = func(resource fullTunConstructionResource) {
		cleanupCounts[resource] += 1
	}
	owner := newFullTunConstructionOwner(path)
	if err := owner.rollback(ctx); !errors.Is(err, cleanupErr) {
		t.Fatalf("partial rollback error=%v, want %v", err, cleanupErr)
	}
	if err := owner.rollback(ctx); !errors.Is(err, cleanupErr) {
		t.Fatalf("repeated partial rollback error=%v, want retained %v", err, cleanupErr)
	}
	expectedResources := []fullTunConstructionResource{
		fullTunConstructionResourceDeviceTransports,
		fullTunConstructionResourceSendRouteController,
		fullTunConstructionResourceSendRouteController,
		fullTunConstructionResourceP2pNetwork,
		fullTunConstructionResourceNoAckTracker,
		fullTunConstructionResourceNoAckTracker,
		fullTunConstructionResourcePackTracker,
		fullTunConstructionResourcePackTracker,
		fullTunConstructionResourceReturnTracker,
	}
	assertFullTunConstructionRolledBack(
		t,
		"independent cleanup error",
		path,
		cleanupCounts,
		expectedResources,
	)
	poolSnapshotAfter, poolBalanced := routeMessagePoolBalance(poolOutstandingBefore)
	if !poolBalanced {
		t.Fatalf(
			"independent cleanup message-pool ownership did not reconcile: %d -> %d classes=%v",
			poolOutstandingBefore,
			poolSnapshotAfter.outstanding,
			poolSnapshotAfter.classes,
		)
	}
}

// Synchronous rollback must have closed every lifecycle and published every
// expected disposition before the constructor returns its error.
func assertFullTunConstructionRolledBack(
	t testing.TB,
	caseName string,
	path *fullTunPath,
	cleanupCounts map[fullTunConstructionResource]int,
	expectedResources []fullTunConstructionResource,
) {
	t.Helper()
	expectedCounts := map[fullTunConstructionResource]int{}
	for _, resource := range expectedResources {
		expectedCounts[resource] += 1
	}
	for resource, expectedCount := range expectedCounts {
		if actualCount := cleanupCounts[resource]; actualCount != expectedCount {
			t.Fatalf(
				"%s cleanup %s count=%d, want %d; all=%v",
				caseName,
				resource,
				actualCount,
				expectedCount,
				cleanupCounts,
			)
		}
	}
	for resource, actualCount := range cleanupCounts {
		if expectedCount := expectedCounts[resource]; expectedCount != actualCount {
			t.Fatalf(
				"%s unexpected cleanup %s count=%d, want %d; all=%v",
				caseName,
				resource,
				actualCount,
				expectedCount,
				cleanupCounts,
			)
		}
	}
	assertClosed := func(name string, done <-chan struct{}) {
		select {
		case <-done:
		default:
			t.Fatalf("%s %s lifecycle remained live after synchronous rollback", caseName, name)
		}
	}
	for trackerIndex, tracker := range []*noAckSendTracker{
		path.providerNoAckSends,
		path.deviceNoAckSends,
	} {
		if tracker != nil {
			assertClosed(fmt.Sprintf("no-ack tracker %d", trackerIndex), tracker.done)
		}
	}
	for trackerIndex, tracker := range []*sendPackLifecycleTracker{
		path.providerPackSends,
		path.devicePackSends,
	} {
		if tracker != nil {
			assertClosed(fmt.Sprintf("Pack tracker %d", trackerIndex), tracker.done)
		}
	}
	if path.providerReturns != nil {
		assertClosed("provider return tracker", path.providerReturns.done)
	}
	assertClientClosed := func(name string, clientDone <-chan struct{}) {
		assertClosed(name, clientDone)
	}
	if path.providerClient != nil {
		assertClientClosed("provider client", path.providerClient.Done())
	}
	if path.deviceClient != nil {
		if deviceClient := path.deviceClient.Load(); deviceClient != nil {
			assertClientClosed("generated device client", deviceClient.Done())
		}
	}
	for intermediaryIndex, intermediary := range path.streamP2pClients {
		if intermediary.client != nil {
			assertClientClosed(
				fmt.Sprintf("intermediary client %d", intermediaryIndex),
				intermediary.client.Done(),
			)
		}
	}
	if path.deviceTransports != nil {
		if !path.deviceTransports.closing.Load() {
			t.Fatalf("%s generated transport owner did not enter closing state", caseName)
		}
		if publishingCount := path.deviceTransports.publishing.Load(); publishingCount != 0 {
			t.Fatalf(
				"%s generated transport publications=%d after rollback",
				caseName,
				publishingCount,
			)
		}
		for node := path.deviceTransports.head.Load(); node != nil; node = node.next {
			if node.client != nil {
				assertClientClosed("retained generated client", node.client.Done())
			}
		}
	}
	for controllerIndex, controller := range path.platformSendRoutes {
		if controller == nil {
			continue
		}
		assertClosed(
			fmt.Sprintf("platform send-route controller %d", controllerIndex),
			controller.closeDone,
		)
		admissionState := controller.admissionState.Load()
		closing := admissionState&platformSendRouteAdmissionClosed != 0
		publishingCount := admissionState & platformSendRouteAdmissionCountMask
		if !closing || publishingCount != 0 {
			t.Fatalf(
				"%s controller %d not quiescent: closing=%t publishing=%d",
				caseName,
				controllerIndex,
				closing,
				publishingCount,
			)
		}
	}
	assertLinkClosed := func(name string, link *directionalLink) {
		link.stateLock.Lock()
		closed := link.closed
		link.stateLock.Unlock()
		if !closed {
			t.Fatalf("%s %s still accepts packets after rollback", caseName, name)
		}
		assertClosed(name+" scheduler", link.done)
	}
	assertCreditsClosed := func(name string, credits *p2pReceiveCredits) {
		credits.stateLock.Lock()
		closed := credits.closedState
		outstandingPacketCount := credits.outstandingPacketCount
		pendingAcquireCount := credits.pendingAcquireCount
		credits.stateLock.Unlock()
		if !closed || outstandingPacketCount != 0 || pendingAcquireCount != 0 {
			t.Fatalf(
				"%s %s not quiescent: closed=%t outstanding=%d pending=%d",
				caseName,
				name,
				closed,
				outstandingPacketCount,
				pendingAcquireCount,
			)
		}
	}
	if path.p2pNetwork != nil {
		for linkIndex, link := range path.p2pNetwork.directionalLinks() {
			assertLinkClosed(fmt.Sprintf("direct P2P link %d", linkIndex), link)
		}
		for creditsIndex, credits := range path.p2pNetwork.receiveCreditPools() {
			assertCreditsClosed(fmt.Sprintf("direct P2P credits %d", creditsIndex), credits)
		}
	}
	if path.streamP2pNetwork != nil {
		for linkIndex, link := range path.streamP2pNetwork.directionalLinks() {
			assertLinkClosed(fmt.Sprintf("stream P2P link %d", linkIndex), link)
		}
		for creditsIndex, credits := range path.streamP2pNetwork.receiveCreditPools() {
			assertCreditsClosed(fmt.Sprintf("stream P2P credits %d", creditsIndex), credits)
		}
	}
	if path.bridgeSends != nil {
		path.bridgeSends.stateLock.Lock()
		liveEntryCount := len(path.bridgeSends.liveEntries)
		path.bridgeSends.stateLock.Unlock()
		if publishingCount := path.bridgeSends.publishingCount.Load(); publishingCount != 0 || liveEntryCount != 0 {
			t.Fatalf(
				"%s bridge not quiescent: publishing=%d live=%d",
				caseName,
				publishingCount,
				liveEntryCount,
			)
		}
	}
}

// Every acquisition and publication boundary rolls back the exact partial
// graph, including one-hop and multihop modeled networks.
func TestFullTunConstructionRollbackClosesEveryAcquisitionStage(t *testing.T) {
	if testing.Short() {
		return
	}
	appendResources := func(parts ...[]fullTunConstructionResource) []fullTunConstructionResource {
		var resources []fullTunConstructionResource
		for _, part := range parts {
			resources = append(resources, part...)
		}
		return resources
	}
	providerCarrier := []fullTunConstructionResource{
		fullTunConstructionResourceProviderCarrierTun,
	}
	carrierTuns := appendResources(providerCarrier, []fullTunConstructionResource{
		fullTunConstructionResourceDeviceCarrierTun,
	})
	sourceTrackers := appendResources(carrierTuns, []fullTunConstructionResource{
		fullTunConstructionResourceNoAckTracker,
		fullTunConstructionResourceNoAckTracker,
		fullTunConstructionResourcePackTracker,
		fullTunConstructionResourcePackTracker,
	})
	providerClient := appendResources(sourceTrackers, []fullTunConstructionResource{
		fullTunConstructionResourceProviderClient,
	})
	providerTransport := appendResources(providerClient, []fullTunConstructionResource{
		fullTunConstructionResourceProviderTransport,
	})
	providerLocalNat := appendResources(providerTransport, []fullTunConstructionResource{
		fullTunConstructionResourceProviderLocalNat,
	})
	providerTrackers := appendResources(providerLocalNat, []fullTunConstructionResource{
		fullTunConstructionResourceReturnTracker,
	})
	providerRemoteNat := appendResources(providerTrackers, []fullTunConstructionResource{
		fullTunConstructionResourceProviderRemoteNat,
	})
	deviceTransportOwner := appendResources(providerRemoteNat, []fullTunConstructionResource{
		fullTunConstructionResourceDeviceTransports,
	})
	deviceGenerator := appendResources(deviceTransportOwner, []fullTunConstructionResource{
		fullTunConstructionResourceApiGenerator,
	})
	applicationTun := appendResources(deviceGenerator, []fullTunConstructionResource{
		fullTunConstructionResourceAppTun,
	})
	multiClient := appendResources(applicationTun, []fullTunConstructionResource{
		fullTunConstructionResourceMultiClient,
	})
	bridge := appendResources(multiClient, []fullTunConstructionResource{
		fullTunConstructionResourceBridge,
	})
	directRouteControllers := appendResources(carrierTuns, []fullTunConstructionResource{
		fullTunConstructionResourceP2pNetwork,
		fullTunConstructionResourceSendRouteController,
		fullTunConstructionResourceSendRouteController,
	})
	streamRouteControllers := appendResources(carrierTuns, []fullTunConstructionResource{
		fullTunConstructionResourceStreamP2pNetwork,
		fullTunConstructionResourceSendRouteController,
		fullTunConstructionResourceSendRouteController,
	})
	directProviderRemoteNat := appendResources(providerRemoteNat, []fullTunConstructionResource{
		fullTunConstructionResourceP2pNetwork,
		fullTunConstructionResourceSendRouteController,
		fullTunConstructionResourceSendRouteController,
	})
	streamProviderRemoteNat := appendResources(providerRemoteNat, []fullTunConstructionResource{
		fullTunConstructionResourceStreamP2pNetwork,
		fullTunConstructionResourceSendRouteController,
		fullTunConstructionResourceSendRouteController,
	})
	streamIntermediaryClient := appendResources(streamProviderRemoteNat, []fullTunConstructionResource{
		fullTunConstructionResourceIntermediaryClient,
		fullTunConstructionResourceIntermediaryTun,
	})
	streamIntermediaryTransport := appendResources(streamIntermediaryClient, []fullTunConstructionResource{
		fullTunConstructionResourceIntermediaryRoute,
	})
	cases := []fullTunConstructionFailureCase{
		{
			name:              "provider carrier TUN",
			stage:             fullTunConstructionStageProviderCarrierTun,
			route:             fullTunRouteExchangeH1,
			p2pHopCount:       1,
			expectedResources: providerCarrier,
		},
		{
			name:              "device carrier TUN",
			stage:             fullTunConstructionStageDeviceCarrierTun,
			route:             fullTunRouteExchangeH1,
			p2pHopCount:       1,
			expectedResources: carrierTuns,
		},
		{
			name:              "one-hop P2P network",
			stage:             fullTunConstructionStageP2pNetwork,
			route:             fullTunRouteP2pFast,
			p2pHopCount:       1,
			expectedResources: appendResources(carrierTuns, []fullTunConstructionResource{fullTunConstructionResourceP2pNetwork}),
		},
		{
			name:              "multihop P2P network",
			stage:             fullTunConstructionStageP2pNetwork,
			route:             fullTunRouteP2pFast,
			p2pHopCount:       2,
			expectedResources: appendResources(carrierTuns, []fullTunConstructionResource{fullTunConstructionResourceStreamP2pNetwork}),
		},
		{name: "one-hop send-route controllers", stage: fullTunConstructionStageSendRouteControllers, route: fullTunRouteP2pFast, p2pHopCount: 1, expectedResources: directRouteControllers},
		{name: "multihop send-route controllers", stage: fullTunConstructionStageSendRouteControllers, route: fullTunRouteP2pFast, p2pHopCount: 2, expectedResources: streamRouteControllers},
		{name: "source trackers", stage: fullTunConstructionStageSourceTrackers, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: sourceTrackers},
		{name: "provider client", stage: fullTunConstructionStageProviderClient, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: providerClient},
		{name: "provider transport", stage: fullTunConstructionStageProviderTransport, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: providerTransport},
		{name: "provider transport ready", stage: fullTunConstructionStageProviderTransportReady, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: providerTransport},
		{name: "provider local NAT", stage: fullTunConstructionStageProviderLocalNat, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: providerLocalNat},
		{name: "provider trackers", stage: fullTunConstructionStageProviderTrackers, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: providerTrackers},
		{name: "provider remote NAT", stage: fullTunConstructionStageProviderRemoteNat, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: providerRemoteNat},
		{name: "provider registration", stage: fullTunConstructionStageProviderRegistration, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: providerRemoteNat},
		{name: "multihop intermediary client", stage: fullTunConstructionStageStreamIntermediaryClient, route: fullTunRouteP2pFast, p2pHopCount: 2, expectedResources: streamIntermediaryClient},
		{name: "multihop intermediary transport", stage: fullTunConstructionStageStreamIntermediaryTransport, route: fullTunRouteP2pFast, p2pHopCount: 2, expectedResources: streamIntermediaryTransport},
		{name: "multihop intermediary ready", stage: fullTunConstructionStageStreamIntermediaryReady, route: fullTunRouteP2pFast, p2pHopCount: 2, expectedResources: streamIntermediaryTransport},
		{name: "device transport owner", stage: fullTunConstructionStageDeviceTransportOwner, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: deviceTransportOwner},
		{name: "device generator", stage: fullTunConstructionStageDeviceGenerator, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: deviceGenerator},
		{name: "application TUN", stage: fullTunConstructionStageApplicationTun, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: applicationTun},
		{name: "multi-client", stage: fullTunConstructionStageMultiClient, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: multiClient},
		{name: "bridge", stage: fullTunConstructionStageBridge, route: fullTunRouteExchangeH1, p2pHopCount: 1, expectedResources: bridge},
		{name: "one-hop P2P provider graph", stage: fullTunConstructionStageProviderRemoteNat, route: fullTunRouteP2pFast, p2pHopCount: 1, expectedResources: directProviderRemoteNat},
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for _, testCase := range cases {
			func() {
				caseCtx, caseCancel := context.WithTimeout(context.Background(), 4*time.Minute)
				defer caseCancel()
				poolOutstandingBefore := routeMessagePoolOutstanding()
				profile := initialNetworkProfiles(7311)["clean-lan"]
				environment := newRouteEnvironmentWithNetworkPeers(caseCtx, t, profile, true)
				environmentClosed := false
				defer func() {
					if !environmentClosed {
						environment.close()
					}
				}()
				primaryErr := errors.New("injected construction failure")
				stageReached := false
				var capturedPath *fullTunPath
				cleanupCounts := map[fullTunConstructionResource]int{}
				var cleanupLock sync.Mutex
				hooks := &fullTunConstructionTestHooks{
					afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
						if stage != testCase.stage {
							return nil
						}
						stageReached = true
						capturedPath = path
						path.afterConstructionCleanupForTest = func(resource fullTunConstructionResource) {
							cleanupLock.Lock()
							cleanupCounts[resource] += 1
							cleanupLock.Unlock()
						}
						return primaryErr
					},
				}
				path, err := tryNewFullTunPathWithTopologyHooks(
					caseCtx,
					t,
					environment,
					testCase.route,
					false,
					defaultTunResourceProfile(),
					testCase.p2pHopCount,
					hooks,
				)
				if path != nil || !stageReached || capturedPath == nil || !errors.Is(err, primaryErr) {
					t.Fatalf(
						"%s result path=%p reached=%t captured=%p err=%v",
						testCase.name,
						path,
						stageReached,
						capturedPath,
						err,
					)
				}
				cleanupLock.Lock()
				finalCleanupCounts := make(map[fullTunConstructionResource]int, len(cleanupCounts))
				for resource, count := range cleanupCounts {
					finalCleanupCounts[resource] = count
				}
				cleanupLock.Unlock()
				assertFullTunConstructionRolledBack(
					t,
					testCase.name,
					capturedPath,
					finalCleanupCounts,
					testCase.expectedResources,
				)
				environment.close()
				environmentClosed = true
				poolSnapshotAfter, poolBalanced := routeMessagePoolBalance(poolOutstandingBefore)
				if !poolBalanced {
					t.Fatalf(
						"%s message-pool ownership did not reconcile: %d -> %d classes=%v",
						testCase.name,
						poolOutstandingBefore,
						poolSnapshotAfter.outstanding,
						poolSnapshotAfter.classes,
					)
				}
			}()
		}
	})
}

// A fresh database and route fixture isolate expensive readiness from the
// earlier acquisition matrix. Race instrumentation can then consume the same
// context-bounded liveness budget without accumulated fixture churn.
func testFullTunConstructionReadyRouteRollback(
	t *testing.T,
	route fullTunRoute,
	profileSeed int64,
	p2pHopCount int,
) {
	t.Helper()
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		poolOutstandingBefore := routeMessagePoolOutstanding()
		profile := initialNetworkProfiles(profileSeed)["clean-lan"]
		enableNetworkPeers := p2pHopCount == 1
		environment := newRouteEnvironmentWithNetworkPeers(
			ctx,
			t,
			profile,
			enableNetworkPeers,
		)
		environmentClosed := false
		defer func() {
			if !environmentClosed {
				environment.close()
			}
		}()
		primaryErr := errors.New("injected ready-route construction failure")
		var capturedPath *fullTunPath
		cleanupCounts := map[fullTunConstructionResource]int{}
		var cleanupLock sync.Mutex
		hooks := &fullTunConstructionTestHooks{
			afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
				if stage != fullTunConstructionStageRouteReady {
					return nil
				}
				capturedPath = path
				path.afterConstructionCleanupForTest = func(resource fullTunConstructionResource) {
					cleanupLock.Lock()
					cleanupCounts[resource] += 1
					cleanupLock.Unlock()
				}
				return primaryErr
			},
		}
		path, err := tryNewFullTunPathWithTopologyHooks(
			ctx,
			t,
			environment,
			route,
			false,
			defaultTunResourceProfile(),
			p2pHopCount,
			hooks,
		)
		if path != nil || capturedPath == nil || !errors.Is(err, primaryErr) {
			t.Fatalf(
				"ready route %s result path=%p captured=%p err=%v",
				route,
				path,
				capturedPath,
				err,
			)
		}
		cleanupLock.Lock()
		finalCleanupCounts := make(map[fullTunConstructionResource]int, len(cleanupCounts))
		for resource, count := range cleanupCounts {
			finalCleanupCounts[resource] = count
		}
		cleanupLock.Unlock()
		expectedResources := []fullTunConstructionResource{
			fullTunConstructionResourceAppTun,
			fullTunConstructionResourceMultiClient,
			fullTunConstructionResourceBridge,
			fullTunConstructionResourceApiGenerator,
			fullTunConstructionResourceDeviceTransports,
			fullTunConstructionResourceProviderRemoteNat,
			fullTunConstructionResourceProviderLocalNat,
			fullTunConstructionResourceProviderClient,
			fullTunConstructionResourceProviderTransport,
			fullTunConstructionResourceProviderCarrierTun,
			fullTunConstructionResourceDeviceCarrierTun,
			fullTunConstructionResourceNoAckTracker,
			fullTunConstructionResourceNoAckTracker,
			fullTunConstructionResourcePackTracker,
			fullTunConstructionResourcePackTracker,
			fullTunConstructionResourceReturnTracker,
		}
		if route == fullTunRouteP2pFast && p2pHopCount == 1 {
			expectedResources = append(
				expectedResources,
				fullTunConstructionResourceP2pNetwork,
				fullTunConstructionResourceSendRouteController,
				fullTunConstructionResourceSendRouteController,
			)
		} else if route == fullTunRouteP2pFast {
			expectedResources = append(
				expectedResources,
				fullTunConstructionResourceStreamP2pNetwork,
				fullTunConstructionResourceSendRouteController,
				fullTunConstructionResourceSendRouteController,
			)
			for intermediaryIndex := 1; intermediaryIndex < p2pHopCount; intermediaryIndex += 1 {
				expectedResources = append(
					expectedResources,
					fullTunConstructionResourceIntermediaryClient,
					fullTunConstructionResourceIntermediaryRoute,
					fullTunConstructionResourceIntermediaryTun,
				)
			}
		}
		assertFullTunConstructionRolledBack(
			t,
			fmt.Sprintf("ready route %s", route),
			capturedPath,
			finalCleanupCounts,
			expectedResources,
		)
		environment.close()
		environmentClosed = true
		poolSnapshotAfter, poolBalanced := routeMessagePoolBalance(poolOutstandingBefore)
		if !poolBalanced {
			t.Fatalf(
				"ready route %s message-pool ownership did not reconcile: %d -> %d classes=%v",
				route,
				poolOutstandingBefore,
				poolSnapshotAfter.outstanding,
				poolSnapshotAfter.classes,
			)
		}
	})
}

// A fully published exchange graph rolls back through its generated client and
// retained platform transport after the readiness probe succeeds.
func TestFullTunConstructionRollbackClosesReadyExchangeRoute(t *testing.T) {
	if testing.Short() {
		return
	}
	testFullTunConstructionReadyRouteRollback(t, fullTunRouteExchangeH1, 7314, 1)
}

// A ready HTTP/3 exchange graph owns a QUIC platform transport and must reach
// the same synchronous fixed point as the HTTP/1 graph during rollback.
func TestFullTunConstructionRollbackClosesReadyExchangeH3Route(t *testing.T) {
	if testing.Short() {
		return
	}
	testFullTunConstructionReadyRouteRollback(t, fullTunRouteExchangeH3, 7316, 1)
}

// A fully promoted direct graph also closes both Pion directions and receive
// credit pools after generated device ownership has been published.
func TestFullTunConstructionRollbackClosesReadyOneHopP2pRoute(t *testing.T) {
	if testing.Short() {
		return
	}
	testFullTunConstructionReadyRouteRollback(t, fullTunRouteP2pFast, 7315, 1)
}

// A fully promoted three-hop graph must synchronously join both intermediary
// clients, their platform transports and TUNs, and every modeled P2P link.
func TestFullTunConstructionRollbackClosesReadyThreeHopP2pRoute(t *testing.T) {
	if testing.Short() {
		return
	}
	testFullTunConstructionReadyRouteRollback(t, fullTunRouteP2pFast, 7317, 3)
}

// One cleanup failure is aggregated with the primary stage error while every
// later resource still receives its synchronous rollback disposition.
func TestFullTunConstructionRollbackContinuesAfterCleanupError(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		poolOutstandingBefore := routeMessagePoolOutstanding()
		profile := initialNetworkProfiles(7312)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, true)
		environmentClosed := false
		defer func() {
			if !environmentClosed {
				environment.close()
			}
		}()
		primaryErr := errors.New("injected primary construction failure")
		cleanupErr := errors.New("injected provider-client cleanup failure")
		var capturedPath *fullTunPath
		cleanupCounts := map[fullTunConstructionResource]int{}
		var cleanupLock sync.Mutex
		hooks := &fullTunConstructionTestHooks{
			afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
				if stage != fullTunConstructionStageRouteReady {
					return nil
				}
				capturedPath = path
				path.constructionCleanupErrorForTest = func(resource fullTunConstructionResource) error {
					if resource == fullTunConstructionResourceProviderClient {
						return cleanupErr
					}
					return nil
				}
				path.afterConstructionCleanupForTest = func(resource fullTunConstructionResource) {
					cleanupLock.Lock()
					cleanupCounts[resource] += 1
					cleanupLock.Unlock()
				}
				return primaryErr
			},
		}
		path, err := tryNewFullTunPathWithTopologyHooks(
			ctx,
			t,
			environment,
			fullTunRouteExchangeH1,
			false,
			defaultTunResourceProfile(),
			1,
			hooks,
		)
		if path != nil || capturedPath == nil || !errors.Is(err, primaryErr) || !errors.Is(err, cleanupErr) {
			t.Fatalf(
				"cleanup aggregation path=%p captured=%p primary=%t cleanup=%t err=%v",
				path,
				capturedPath,
				errors.Is(err, primaryErr),
				errors.Is(err, cleanupErr),
				err,
			)
		}
		cleanupLock.Lock()
		finalCleanupCounts := make(map[fullTunConstructionResource]int, len(cleanupCounts))
		for resource, count := range cleanupCounts {
			finalCleanupCounts[resource] = count
		}
		cleanupLock.Unlock()
		expectedResources := []fullTunConstructionResource{
			fullTunConstructionResourceAppTun,
			fullTunConstructionResourceMultiClient,
			fullTunConstructionResourceBridge,
			fullTunConstructionResourceApiGenerator,
			fullTunConstructionResourceDeviceTransports,
			fullTunConstructionResourceProviderRemoteNat,
			fullTunConstructionResourceProviderLocalNat,
			fullTunConstructionResourceProviderClient,
			fullTunConstructionResourceProviderTransport,
			fullTunConstructionResourceProviderCarrierTun,
			fullTunConstructionResourceDeviceCarrierTun,
			fullTunConstructionResourceNoAckTracker,
			fullTunConstructionResourceNoAckTracker,
			fullTunConstructionResourcePackTracker,
			fullTunConstructionResourcePackTracker,
			fullTunConstructionResourceReturnTracker,
		}
		assertFullTunConstructionRolledBack(
			t,
			"cleanup error aggregation",
			capturedPath,
			finalCleanupCounts,
			expectedResources,
		)
		environment.close()
		environmentClosed = true
		poolSnapshotAfter, poolBalanced := routeMessagePoolBalance(poolOutstandingBefore)
		if !poolBalanced {
			t.Fatalf(
				"cleanup error aggregation message-pool ownership did not reconcile: %d -> %d classes=%v",
				poolOutstandingBefore,
				poolSnapshotAfter.outstanding,
				poolSnapshotAfter.classes,
			)
		}
	})
}
