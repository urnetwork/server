//go:build acklineagetrace

package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

const dohTransferPrefixBoundary = "request-source-pack-physical-prefix-before-cache-retirement-v1"

// The actual MultiClient evaluation/cping producer calls SendDetailedMessage
// with one empty IpPing, not a grouped IP payload. Exclude ONLY that distinct
// control type, not arbitrary non-workload/unknown frames or health markers.
// Every other all-Pack failure remains a conservative cohort stop condition.
func dohTransferIndependentPingFailure(event clientconnect.SendPackLifecycleObservation) bool {
	return event.Phase == clientconnect.SendPackLifecyclePhaseTerminal && event.Err != nil && event.MessageType == protocol.MessageType_IpIpPing
}

func TestDohTransferPrefixPingClassifierPinsEmptyProducerAndDataNegatives(t *testing.T) {
	if fields := (&protocol.IpPing{}).ProtoReflect().Descriptor().Fields().Len(); fields != 0 {
		t.Fatal("IpPing now carries fields; review request independence")
	}
	for _, version := range []int{1, 2} {
		frame, err := clientconnect.ToFrame(&protocol.IpPing{}, version)
		if err != nil || frame.MessageType != protocol.MessageType_IpIpPing || len(frame.MessageBytes) != 0 {
			t.Fatalf("actual ping producer frame=%+v err=%v", frame, err)
		}
		event := clientconnect.SendPackLifecycleObservation{Phase: clientconnect.SendPackLifecyclePhaseTerminal, MessageType: frame.MessageType, Err: errors.New("queued before serialization")}
		if !dohTransferIndependentPingFailure(event) || sendPackLifecycleWorkload(event) {
			t.Fatal("empty ping classified as DoH IP bytes")
		}
		for _, kind := range []protocol.MessageType{protocol.MessageType_IpIpPacketToProvider, protocol.MessageType_IpIpPacketFromProvider, protocol.MessageType_TransferExchangeSignals, protocol.MessageType_TransferPack} {
			event.MessageType = kind
			if dohTransferIndependentPingFailure(event) {
				t.Fatalf("non-ping failure %v waived", kind)
			}
		}
	}
}

type dohTransferCarrierPrefixObservation struct {
	Policy    string                                           `json:"policy"`
	WireScope string                                           `json:"wire_scope"`
	Attempts  int                                              `json:"attempts"`
	Links     map[string]directionalLinkPacketFenceObservation `json:"links"`
}

// This H1-only experiment joins a finite physical prefix AFTER the exact
// source and workload Pack terminals. Later control/maintenance cannot extend
// that prefix. It does not claim global idle or request-exclusive wire bytes.
// Source/Pack generations are recaptured after each fence, so newly published
// application work repeats the transaction; pending/failing request owners
// never become maintenance just because their publication races the fence.
func joinDohTransferCarrierPrefix(ctx context.Context, path *fullTunPath, observation *dohTransferCarrierPrefixObservation) error {
	if path.p2pNetwork != nil || path.streamP2pNetwork != nil || observation == nil {
		return fmt.Errorf("request physical-prefix boundary requires scoped H1 and observation")
	}
	observation.Policy, observation.WireScope = dohTransferPrefixBoundary, "inclusive physical counters through request prefix; later maintenance/backlog is reported, not required idle"
	for attempt := 1; ; attempt++ {
		observation.Attempts = attempt
		upstreamBefore, err := path.upstreamBoundary(ctx)
		if err != nil {
			return fmt.Errorf("capture request upstream: %w", err)
		}
		if !path.waitThroughUpstreamBoundary(ctx, upstreamBefore) {
			return fmt.Errorf("join request upstream: %v", ctx.Err())
		}
		trackers := []*sendPackLifecycleTracker{path.devicePackSends, path.providerPackSends}
		before := make([]sendPackLifecycleBoundary, len(trackers))
		for i, tracker := range trackers {
			if tracker == nil {
				return fmt.Errorf("nil request Pack tracker %d", i)
			}
			var ok bool
			before[i], ok = tracker.workloadBoundary(ctx)
			if !ok || !tracker.waitThrough(ctx, before[i]) {
				return fmt.Errorf("join request Pack tracker %d: %v", i, ctx.Err())
			}
		}
		type namedFence struct {
			name  string
			fence directionalLinkPacketFence
		}
		network := path.environment.network
		network.stateLock.Lock()
		names := make([]tunLinkKey, 0, len(network.links))
		for key := range network.links {
			names = append(names, key)
		}
		sort.Slice(names, func(i, j int) bool {
			if names[i].source == names[j].source {
				return names[i].destination < names[j].destination
			}
			return names[i].source < names[j].source
		})
		fences := make([]namedFence, 0, len(names))
		for _, key := range names {
			fence, captureErr := network.links[key].capturePacketFence()
			if captureErr != nil {
				network.stateLock.Unlock()
				return fmt.Errorf("capture physical prefix %s->%s: %w", key.source, key.destination, captureErr)
			}
			fences = append(fences, namedFence{key.source + "->" + key.destination, fence})
		}
		network.stateLock.Unlock()
		observation.Links = make(map[string]directionalLinkPacketFenceObservation, len(fences))
		for _, named := range fences {
			terminal, err := named.fence.wait(ctx)
			observation.Links[named.name] = terminal
			if err != nil {
				return fmt.Errorf("join physical prefix %s: %w", named.name, err)
			}
		}
		carrierEnd := snapshotPerfvarCarrier(path)
		if path.afterCarrierEndCandidateForTest != nil {
			path.afterCarrierEndCandidateForTest(attempt)
		}
		upstreamAfter, err := path.upstreamBoundary(ctx)
		if err != nil {
			return fmt.Errorf("recapture request upstream: %w", err)
		}
		stable := fullTunUpstreamBoundaryStable(upstreamBefore, upstreamAfter)
		for i, tracker := range trackers {
			after, ok := tracker.workloadBoundary(ctx)
			if !ok {
				return fmt.Errorf("recapture request Pack tracker %d: %v", i, ctx.Err())
			}
			stable = stable && before[i].startedCount == after.startedCount && len(after.entries) == 0
		}
		network.stateLock.Lock()
		linksStable := len(network.links) == len(fences)
		for i, key := range names {
			linksStable = linksStable && network.links[key] == fences[i].fence.link
		}
		network.stateLock.Unlock()
		if !linksStable {
			return fmt.Errorf("request physical-prefix topology changed")
		}
		if !stable {
			continue
		}
		if err := path.validateMeasuredPackFailures(carrierEnd); err != nil {
			return err
		}
		for _, named := range fences {
			observation.Links[named.name] = named.fence.snapshot()
		}
		path.setCarrierMeasurementEndIfAbsent(carrierEnd)
		return nil
	}
}

func TestDohTransferPrefixRecapturesSourceAndPackWithoutGap(t *testing.T) {
	for _, kind := range []string{"bridge-before", "provider-before", "pack-before", "bridge-after", "provider-after", "pack-after"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newFullTunMeasurementBoundaryTestFixture()
				defer fixture.close()
				var release func()
				admit := func() {
					switch kind {
					case "bridge-before", "bridge-after":
						entry := fixture.path.bridgeSends.start(0, fullTunBridgeFlowKey{}, 100)
						release = func() { fixture.path.bridgeSends.terminal(entry, true) }
					case "provider-before", "provider-after":
						flow := providerReturnTrackerTestFlow(47)
						observeProviderReturnStarted(fixture.path.providerReturns, 1, flow, 1, 100)
						release = func() { observeProviderReturnCompleted(fixture.path.providerReturns, 1, flow, 1, 100, true) }
					default:
						observer := fixture.path.devicePackSends.newObserver()
						event := clientconnect.SendPackLifecycleObservation{ClientId: clientconnect.NewId(), DestinationId: clientconnect.NewId(), Token: 1, AckRequired: true, MessageType: protocol.MessageType_IpIpPacketToProvider, Phase: clientconnect.SendPackLifecyclePhaseStarted}
						observer(event)
						event.Phase = clientconnect.SendPackLifecyclePhaseFirstRouteWrite
						observer(event)
						release = func() { event.Phase = clientconnect.SendPackLifecyclePhaseTerminal; observer(event) }
					}
				}
				after := kind[len(kind)-5:] == "after"
				if after {
					fixture.path.afterCarrierEndCandidateForTest = func(attempt int) {
						if attempt == 1 {
							admit()
						}
					}
				} else {
					admit()
				}
				var observation dohTransferCarrierPrefixObservation
				joined := make(chan error, 1)
				go func() { joined <- joinDohTransferCarrierPrefix(fixture.ctx, fixture.path, &observation) }()
				synctest.Wait()
				select {
				case err := <-joined:
					t.Fatalf("%s unfinished request escaped: %v", kind, err)
				default:
				}
				if release == nil {
					t.Fatal("source/Pack injection did not run")
				}
				release()
				if err := <-joined; err != nil {
					t.Fatal(err)
				}
				want := 1
				if after {
					want = 2
				}
				if observation.Attempts != want {
					t.Fatalf("attempts=%d want=%d", observation.Attempts, want)
				}
			})
		})
	}
}

func TestDohTransferPrefixJoinsUnpublishedSource(t *testing.T) {
	for _, provider := range []bool{false, true} {
		t.Run(fmt.Sprint("provider=", provider), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newFullTunMeasurementBoundaryTestFixture()
				defer fixture.close()
				entered, releasePublish, published, releaseTerminal := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				waiting := make(chan struct{})
				var once sync.Once
				if provider {
					fixture.path.providerReturns.setBeforeObserverPublishForTest(func(event clientconnect.RemoteUserNatProviderReturnSendObservation) {
						if event.Phase == clientconnect.RemoteUserNatProviderReturnSendPhaseStarted {
							close(entered)
							<-releasePublish
						}
					})
					fixture.path.providerReturns.setBeforePublisherWaitForTest(func() { once.Do(func() { close(waiting) }) })
				} else {
					fixture.path.bridgeSends.setBeforeStartPublishForTest(func() { close(entered); <-releasePublish })
					fixture.path.bridgeSends.setBeforePublisherWaitForTest(func() { once.Do(func() { close(waiting) }) })
				}
				go func() {
					if provider {
						flow := providerReturnTrackerTestFlow(53)
						observeProviderReturnStarted(fixture.path.providerReturns, 1, flow, 1, 100)
						close(published)
						<-releaseTerminal
						observeProviderReturnCompleted(fixture.path.providerReturns, 1, flow, 1, 100, true)
					} else {
						entry := fixture.path.bridgeSends.start(0, fullTunBridgeFlowKey{}, 100)
						close(published)
						<-releaseTerminal
						fixture.path.bridgeSends.terminal(entry, true)
					}
				}()
				<-entered
				joined := make(chan error, 1)
				go func() {
					joined <- joinDohTransferCarrierPrefix(fixture.ctx, fixture.path, &dohTransferCarrierPrefixObservation{})
				}()
				<-waiting
				synctest.Wait()
				select {
				case err := <-joined:
					t.Fatalf("unpublished source escaped: %v", err)
				default:
				}
				close(releasePublish)
				<-published
				synctest.Wait()
				select {
				case err := <-joined:
					t.Fatalf("published but unfinished source escaped: %v", err)
				default:
				}
				close(releaseTerminal)
				if err := <-joined; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestDohTransferPrefixLaterPingDoesNotOwnCompletedRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newFullTunMeasurementBoundaryTestFixture()
		defer fixture.close()
		profile := newLinkProfile(1_000_000_000, time.Hour, 0, 0, time.Millisecond)
		link := newDirectionalLink(fixture.ctx, profile, 37, func([]byte) bool { return true })
		if err := link.enablePacketFences(); err != nil {
			t.Fatal(err)
		}
		fixture.network.links[tunLinkKey{"device", "edge"}] = link
		observer := fixture.path.devicePackSends.newObserver()
		event := clientconnect.SendPackLifecycleObservation{ClientId: clientconnect.NewId(), DestinationId: clientconnect.NewId(), Token: 1, AckRequired: true, MessageType: protocol.MessageType_IpIpPing}
		fixture.path.afterCarrierEndCandidateForTest = func(attempt int) {
			if attempt != 1 {
				t.Fatal("independent ping changed request generation")
			}
			event.Phase = clientconnect.SendPackLifecyclePhaseStarted
			observer(event)
			event.Phase = clientconnect.SendPackLifecyclePhaseFirstRouteWrite
			observer(event)
			_, _ = link.submit([]byte{2})
		}
		var observation dohTransferCarrierPrefixObservation
		if err := joinDohTransferCarrierPrefix(fixture.ctx, fixture.path, &observation); err != nil {
			t.Fatal(err)
		}
		got := observation.Links["device->edge"]
		if got.PendingOwners != 0 || got.PostFenceQueuedPackets != 1 || got.PostFenceSubmissions != 1 {
			t.Fatalf("reported maintenance tail=%+v", got)
		}
		all, ok := fixture.path.devicePackSends.boundary(fixture.ctx)
		if !ok || len(all.entries) != 1 || fixture.path.devicePackSends.workloadFailures.Load() != 0 {
			t.Fatal("independent ping vanished from all-Pack diagnostics")
		}
		oldCtx, cancel := context.WithTimeout(fixture.ctx, time.Second)
		defer cancel()
		if fixture.path.waitForCarrierQuiescent(oldCtx) {
			t.Fatal("old global-idle semantics changed")
		}
		event.Phase = clientconnect.SendPackLifecyclePhaseTerminal
		observer(event)
	})
}

func TestDohTransferPrefixRejectsRequestFailureAndCancellation(t *testing.T) {
	for _, kind := range []string{"pack", "queue", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newFullTunMeasurementBoundaryTestFixture()
			defer fixture.close()
			if err := fixture.path.waitForMeasurementBoundary(fixture.ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := beginPerfvarCarrierMeasurement(fixture.path); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "pack":
				observeFailedSendPackLifecycle(fixture.path.devicePackSends, 1)
			case "queue":
				profile := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
				profile.QueuePacketCount = 0
				link := newDirectionalLink(fixture.ctx, profile, 41, func([]byte) bool { return true })
				if err := link.enablePacketFences(); err != nil {
					t.Fatal(err)
				}
				fixture.network.links[tunLinkKey{"device", "edge"}] = link
				_, _ = link.submit([]byte{1})
			case "cancel":
				fixture.cancel()
			}
			if err := joinDohTransferCarrierPrefix(fixture.ctx, fixture.path, &dohTransferCarrierPrefixObservation{}); err == nil {
				t.Fatalf("%s request failure was accepted", kind)
			}
		})
	}
}

func TestDohTransferPrefixIdentityAndLegacyPreservation(t *testing.T) {
	for _, suffix := range []string{"", "-burst-1"} {
		old, err := dohTransferScenarioNamed("established-rtt3s-keepalive" + suffix)
		if err != nil {
			t.Fatal(err)
		}
		current, err := dohTransferScenarioNamed("established-rtt3s-request-prefix" + suffix)
		if err != nil || old.Version != 4 || old.RequestBoundaryPolicy != dohTransferKeepaliveBoundary || current.Version != 5 || current.RequestBoundaryPolicy != dohTransferPrefixBoundary {
			t.Fatalf("versions changed: %+v %+v %v", old, current, err)
		}
		normalized := current
		normalized.Name, normalized.Version, normalized.RequestBoundaryPolicy = old.Name, old.Version, old.RequestBoundaryPolicy
		if !reflect.DeepEqual(normalized, old) {
			t.Fatal("scoped fence changed route/loss/timer/ACK settings")
		}
		for run := 23; run <= 32; run++ {
			trace, err := dohTransferTrace(current, run)
			if err != nil {
				t.Fatal(err)
			}
			if os.Getenv("CONNECT_PERFVAR_DOH_TRANSFER_EMIT_IDENTITIES") == "1" {
				encoded, err := json.Marshal(struct {
					Scenario     dohTransferScenario `json:"scenario"`
					ScenarioHash string              `json:"scenario_hash"`
					ProfileHash  string              `json:"profile_hash"`
					Trace        perfvarTrace        `json:"trace"`
					RunIndex     int                 `json:"run_index"`
				}{current, tcpRecoveryHash(current), tcpRecoveryHash([]networkProfile{current.DeviceAccess, current.ProviderAccess}), trace, run})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("[doh-prefix-identity] %s", encoded)
			}
		}
	}
	if _, err := dohTransferScenarioNamed("established-rtt3s-request-prefix-ignore-loss"); err == nil {
		t.Fatal("unknown scoped profile became clean")
	}
}
