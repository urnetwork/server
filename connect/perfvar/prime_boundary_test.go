// These regressions force an exact priming TCP request Pack to remain live
// while the P2P fixture approaches its route-transition and route-ready
// boundaries.
package perfvar

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// One asynchronous constructor result lets the test observe an interior
// priming boundary while retaining exact ownership of the completed fixture.
type primeBoundaryConstructionResult struct {
	path *fullTunPath
	err  error
}

// One private observer namespace and immutable Pack identity select the exact
// lifecycle that started after a named priming phase was armed.
type primeBoundaryPackIdentity struct {
	clientInstance uint64
	clientId       clientconnect.Id
	destinationId  clientconnect.Id
	token          uint64
	ackRequired    bool
	messageType    protocol.MessageType
}

// Forces one readiness probe's device request Pack terminal callback to remain
// live. Existing lifecycle and route-controller hooks distinguish the fixed
// join from the route transition that used to pass it without scheduler timing.
func testFullTunP2pPrimePackBoundary(
	t *testing.T,
	route fullTunRoute,
	p2pHopCount int,
	targetProbeNumber int32,
	expectJoinBeforeDisable bool,
) {
	t.Helper()
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		profile := initialNetworkProfiles(
			2026081121 + int64(targetProbeNumber) + int64(p2pHopCount),
		)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(
			ctx,
			t,
			profile,
			p2pHopCount == 1,
		)

		releaseTerminal := make(chan struct{})
		var releaseTerminalOnce sync.Once
		releasePackTerminal := func() {
			releaseTerminalOnce.Do(func() {
				close(releaseTerminal)
			})
		}
		terminalHeld := make(chan struct{})
		packBoundaryWaiting := make(chan struct{})
		deviceDisabled := make(chan struct{})
		providerDisabled := make(chan struct{})
		routeReady := make(chan struct{})
		phaseErrors := make(chan error, 1)
		var terminalHoldClaimed atomic.Bool
		var targetProbeArmed atomic.Bool
		var targetPackIdentity atomic.Pointer[primeBoundaryPackIdentity]
		var readinessProbeCount atomic.Int32
		var deviceDisabledApplied atomic.Bool
		var providerDisabledApplied atomic.Bool
		var packBoundaryWaitingOnce sync.Once
		var deviceDisabledOnce sync.Once
		var providerDisabledOnce sync.Once
		var routeReadyOnce sync.Once
		var heldObservation clientconnect.SendPackLifecycleObservation
		recordPhaseError := func(err error) {
			select {
			case phaseErrors <- err:
			default:
			}
		}

		hooks := &fullTunConstructionTestHooks{
			afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
				switch stage {
				case fullTunConstructionStageSendRouteControllers:
					path.deviceSendRoutes.afterEventApplyForTest = func(event platformSendRouteEvent) {
						if event.kind == platformSendRouteEventDisabled && event.disabled {
							deviceDisabledApplied.Store(true)
							deviceDisabledOnce.Do(func() {
								close(deviceDisabled)
							})
						}
					}
					path.providerSendRoutes.afterEventApplyForTest = func(event platformSendRouteEvent) {
						if event.kind == platformSendRouteEventDisabled && event.disabled {
							providerDisabledApplied.Store(true)
							providerDisabledOnce.Do(func() {
								close(providerDisabled)
							})
						}
					}
				case fullTunConstructionStageSourceTrackers:
					// Terminal publication may enter before boundary capture, or
					// capture may reach waitThrough first. Observe both orderings
					// with the same immutable Client-instance and Pack identity.
					markExactPackBoundaryWait := func(entry *sendPackLifecycleEntry) {
						identity := targetPackIdentity.Load()
						if identity == nil ||
							identity.clientInstance != entry.key.clientInstance ||
							identity.clientId != entry.clientId ||
							identity.destinationId != entry.destinationId ||
							identity.token != entry.key.token ||
							identity.ackRequired != entry.ackRequired ||
							identity.messageType != entry.messageType {
							return
						}
						packBoundaryWaitingOnce.Do(func() {
							close(packBoundaryWaiting)
						})
					}
					path.devicePackSends.setBeforeInstanceObserverPublishForTest(func(
						clientInstance uint64,
						observation clientconnect.SendPackLifecycleObservation,
					) {
						if !targetProbeArmed.Load() ||
							observation.AckRequired ||
							observation.MessageType != protocol.MessageType_IpIpPacketToProvider ||
							observation.DestinationId != path.providerClientId {
							return
						}
						if observation.Phase == clientconnect.SendPackLifecyclePhaseStarted {
							targetPackIdentity.CompareAndSwap(
								nil,
								&primeBoundaryPackIdentity{
									clientInstance: clientInstance,
									clientId:       observation.ClientId,
									destinationId:  observation.DestinationId,
									token:          observation.Token,
									ackRequired:    observation.AckRequired,
									messageType:    observation.MessageType,
								},
							)
						}
					})
					// The Connect observer is nonblocking by contract. Hold the
					// tracker's owner after it has consumed the exact terminal event;
					// blocking the observer itself would also freeze this destination's
					// SendSequence and strand the next inner-TCP Pack before first write.
					path.devicePackSends.setBeforeTerminalReleaseForTest(func(
						entry *sendPackLifecycleEntry,
						observation clientconnect.SendPackLifecycleObservation,
					) {
						identity := targetPackIdentity.Load()
						if identity == nil ||
							identity.clientInstance != entry.key.clientInstance ||
							identity.clientId != observation.ClientId ||
							identity.destinationId != observation.DestinationId ||
							identity.token != entry.key.token ||
							identity.ackRequired != observation.AckRequired ||
							identity.messageType != observation.MessageType ||
							!terminalHoldClaimed.CompareAndSwap(false, true) {
							return
						}
						heldObservation = observation
						close(terminalHeld)
						select {
						case <-releaseTerminal:
						case <-ctx.Done():
						}
					})
					path.devicePackSends.setAfterBoundaryEnqueueForTest(func(
						scope sendPackLifecycleBoundaryScope,
					) {
						if scope != sendPackLifecycleBoundaryScopeWorkload {
							return
						}
						if terminalHoldClaimed.Load() {
							packBoundaryWaitingOnce.Do(func() {
								close(packBoundaryWaiting)
							})
						}
					})
					path.devicePackSends.setBeforeEntryWaitForTest(markExactPackBoundaryWait)
				case fullTunConstructionStageBridge:
					// One byte keeps the exact request lifecycle small under race
					// instrumentation without turning this ordering test into a
					// 16 KiB retransmission-pressure workload.
					path.readinessProbePayloadForTest = []byte{1}
					// Arm immediately before the guaranteed request write. Arming
					// after the response is too late: the server closes after its
					// response write, so the inner TCP stack may have already sent
					// the FIN ACK before the application reaches client Close.
					path.beforeReadinessClientWriteForTest = func() {
						probeNumber := readinessProbeCount.Add(1)
						if probeNumber != targetProbeNumber {
							return
						}
						if expectJoinBeforeDisable {
							if deviceDisabledApplied.Load() || providerDisabledApplied.Load() {
								recordPhaseError(errors.New(
									"discovery probe reached request write after an exchange route was disabled",
								))
								return
							}
						} else if !deviceDisabledApplied.Load() || !providerDisabledApplied.Load() {
							recordPhaseError(errors.New(
								"forced probe reached request write before both exchange routes were disabled",
							))
							return
						}
						targetProbeArmed.Store(true)
					}
				case fullTunConstructionStageRouteReady:
					routeReadyOnce.Do(func() {
						close(routeReady)
					})
				}
				return nil
			},
		}

		constructionDone := make(chan primeBoundaryConstructionResult, 1)
		go func() {
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
			constructionDone <- primeBoundaryConstructionResult{path: path, err: err}
		}()

		var constructionResult primeBoundaryConstructionResult
		constructionJoined := false
		joinConstruction := func() primeBoundaryConstructionResult {
			if !constructionJoined {
				constructionResult = <-constructionDone
				constructionJoined = true
			}
			return constructionResult
		}
		defer func() {
			cancel()
			releasePackTerminal()
			result := joinConstruction()
			if result.path != nil {
				result.path.close()
			}
			environment.close()
		}()

		select {
		case <-terminalHeld:
		case err := <-phaseErrors:
			t.Fatalf("arm probe %d Pack phase: %v", targetProbeNumber, err)
		case result := <-constructionDone:
			constructionResult = result
			constructionJoined = true
			t.Fatalf(
				"constructor completed before probe %d Pack terminal was held: path=%p err=%v",
				targetProbeNumber,
				result.path,
				result.err,
			)
		case <-ctx.Done():
			t.Fatalf("hold probe %d Pack terminal: %v", targetProbeNumber, ctx.Err())
		}
		if heldObservation.Err != nil {
			t.Fatalf(
				"probe %d Pack reached terminal with error before release: %+v",
				targetProbeNumber,
				heldObservation,
			)
		}

		if expectJoinBeforeDisable {
			select {
			case <-packBoundaryWaiting:
			case err := <-phaseErrors:
				t.Fatalf("join discovery probe Pack phase: %v", err)
			case <-deviceDisabled:
				t.Fatal("device exchange route was disabled before the discovery probe Pack joined")
			case <-providerDisabled:
				t.Fatal("provider exchange route was disabled before the discovery probe Pack joined")
			case <-routeReady:
				t.Fatal("P2P route became ready before the discovery probe Pack joined")
			case result := <-constructionDone:
				constructionResult = result
				constructionJoined = true
				t.Fatalf(
					"constructor completed before the discovery probe Pack joined: path=%p err=%v",
					result.path,
					result.err,
				)
			case <-ctx.Done():
				t.Fatalf("join discovery probe Pack before route transition: %v", ctx.Err())
			}
			select {
			case <-deviceDisabled:
				t.Fatal("device exchange route changed while the discovery probe Pack was held")
			case <-providerDisabled:
				t.Fatal("provider exchange route changed while the discovery probe Pack was held")
			default:
			}
		} else {
			select {
			case <-packBoundaryWaiting:
			case err := <-phaseErrors:
				t.Fatalf("join forced probe Pack phase: %v", err)
			case <-routeReady:
				t.Fatal("P2P route became ready before the forced probe Pack joined")
			case result := <-constructionDone:
				constructionResult = result
				constructionJoined = true
				t.Fatalf(
					"constructor completed before the forced probe Pack joined: path=%p err=%v",
					result.path,
					result.err,
				)
			case <-ctx.Done():
				t.Fatalf("join forced probe Pack before route ready: %v", ctx.Err())
			}
			select {
			case <-routeReady:
				t.Fatal("route-ready publication crossed the held forced-probe Pack")
			default:
			}
		}

		releasePackTerminal()
		result := joinConstruction()
		if result.err != nil || result.path == nil {
			t.Fatalf(
				"complete P2P construction after releasing probe %d Pack: path=%p err=%v",
				targetProbeNumber,
				result.path,
				result.err,
			)
		}
		for name, event := range map[string]<-chan struct{}{
			"device disabled":   deviceDisabled,
			"provider disabled": providerDisabled,
			"route ready":       routeReady,
		} {
			select {
			case <-event:
			default:
				t.Errorf("construction completed without %s", name)
			}
		}
		if count := readinessProbeCount.Load(); count != 2 {
			t.Errorf("readiness probe count=%d, want=2", count)
		}
	})
}

// The exchange-carried discovery probe must fully relinquish source and route
// ownership before contract policy changes and the exchange route is disabled.
func TestFullTunP2pPrimeJoinsDiscoveryProbeBeforeRouteTransition(t *testing.T) {
	testFullTunP2pPrimePackBoundary(t, fullTunRouteP2pLegacy, 1, 1, true)
}

// The forced-P2P probe must fully relinquish source and route ownership before
// construction publishes route readiness to a measurement caller.
func TestFullTunP2pPrimeJoinsForcedProbeBeforeRouteReady(t *testing.T) {
	testFullTunP2pPrimePackBoundary(t, fullTunRouteP2pLegacy, 1, 2, false)
}

// A multihop stream must join the exchange-carried discovery tail before its
// platform routes are disabled for the end-to-end stream.
func TestFullTunStreamP2pPrimeJoinsDiscoveryProbeBeforeRouteTransition(t *testing.T) {
	testFullTunP2pPrimePackBoundary(t, fullTunRouteP2pFast, 2, 1, true)
}

// The promoted multihop stream must join its final probe tail before route
// readiness is made visible to a measurement caller.
func TestFullTunStreamP2pPrimeJoinsForcedProbeBeforeRouteReady(t *testing.T) {
	testFullTunP2pPrimePackBoundary(t, fullTunRouteP2pFast, 2, 2, false)
}
