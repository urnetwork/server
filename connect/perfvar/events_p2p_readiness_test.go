// Path-change readiness must follow physical P2P replacement, not unrelated
// platform count transitions or an already-withdrawn historical promotion.
package perfvar

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

// Two synthetic carriers exercise the production writer's exact publication
// history while explicit callbacks control each P2P endpoint direction.
type liveP2pReadinessFixture struct {
	route     *liveP2pRoute
	manager   *clientconnect.RouteManager
	platform  clientconnect.Transport
	direct    clientconnect.Transport
	peerState clientconnect.P2pRouteState
}

// No sockets, traffic, or scheduler timing are needed to publish these routes.
func newLiveP2pReadinessFixture(t *testing.T) *liveP2pReadinessFixture {
	manager := clientconnect.NewRouteManager(t.Context(), "p2p-readiness")
	writer := manager.OpenMultiRouteWriter(clientconnect.DestinationId(clientconnect.NewId()))
	fixture := &liveP2pReadinessFixture{
		route: &liveP2pRoute{
			source:             &routeClient{routeStateTrace: newP2pRouteStateTrace()},
			destination:        &routeClient{routeStateTrace: newP2pRouteStateTrace()},
			writer:             writer,
			routeStateObserver: clientconnect.TestingObserveMultiRouteWriterRouteState(writer),
		},
		manager:  manager,
		platform: clientconnect.NewSendGatewayTransport(),
		direct:   clientconnect.NewSendGatewayTransport(),
		peerState: clientconnect.P2pRouteState{
			PeerId:    clientconnect.NewId(),
			StreamId:  clientconnect.NewId(),
			Connected: true,
		},
	}
	t.Cleanup(func() {
		fixture.route.routeStateObserver.Close()
		manager.CloseMultiRouteWriter(writer)
		manager.RemoveTransport(fixture.platform)
		manager.RemoveTransport(fixture.direct)
	})
	manager.UpdateTransport(fixture.platform, []clientconnect.Route{make(clientconnect.Route, 1)})
	manager.UpdateTransport(fixture.direct, []clientconnect.Route{make(clientconnect.Route, 1)})
	for _, client := range []*routeClient{fixture.route.source, fixture.route.destination} {
		for _, send := range []bool{false, true} {
			state := fixture.peerState
			state.Send = send
			client.routeStateTrace.Observe(state)
		}
	}
	return fixture
}

// Every replacement callback is published before the readiness consumer runs.
func (self *liveP2pReadinessFixture) replaceDirect() {
	self.manager.RemoveTransport(self.direct)
	for _, client := range []*routeClient{self.route.source, self.route.destination} {
		for _, send := range []bool{false, true} {
			state := self.peerState
			state.Send = send
			state.Connected = false
			client.routeStateTrace.Observe(state)
			state.Connected = true
			client.routeStateTrace.Observe(state)
		}
	}
	self.manager.UpdateTransport(self.direct, []clientconnect.Route{make(clientconnect.Route, 1)})
}

// A complete platform-only 2 -> 1 -> 2 history leaves the old P2P generation
// untouched. Cancellation ends the unavailable proof without a timing guess.
func TestLiveP2pReadinessRejectsPlatformOnlyReplacement(t *testing.T) {
	fixture := newLiveP2pReadinessFixture(t)
	barrier := fixture.route.routeState()
	fixture.manager.RemoveTransport(fixture.platform)
	fixture.manager.UpdateTransport(fixture.platform, []clientconnect.Route{make(clientconnect.Route, 1)})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := fixture.route.waitForRebuiltDirectRoute(ctx, barrier); !errors.Is(err, context.Canceled) {
		t.Fatalf("platform-only reconnect satisfied P2P replacement: err=%v", err)
	}
}

// A real P2P rebuild followed by withdrawal before the consumer runs is
// historical evidence, not a currently usable route for the next workload.
func TestLiveP2pReadinessRejectsWithdrawnPromotion(t *testing.T) {
	fixture := newLiveP2pReadinessFixture(t)
	barrier := fixture.route.routeState()
	fixture.replaceDirect()
	fixture.manager.RemoveTransport(fixture.direct)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := fixture.route.waitForRebuiltDirectRoute(ctx, barrier); !errors.Is(err, context.Canceled) {
		t.Fatalf("withdrawn historical promotion satisfied current readiness: err=%v", err)
	}
}

// A complete direct replacement remains observable even when every callback
// and selector publication precedes the consumer's first read.
func TestLiveP2pReadinessRetainsFastReplacement(t *testing.T) {
	fixture := newLiveP2pReadinessFixture(t)
	barrier := fixture.route.routeState()
	fixture.replaceDirect()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := fixture.route.waitForRebuiltDirectRoute(ctx, barrier); err != nil {
		t.Fatalf("complete direct replacement was lost: %v", err)
	}
}

// Neither another endpoint nor repeated replacement of the opposite local
// direction can discharge the unchanged direction's retirement obligation.
func TestLiveP2pReadinessRequiresBothEndpointsAndDirections(t *testing.T) {
	for unchangedEndpoint := range 2 {
		for _, unchangedSend := range []bool{false, true} {
			fixture := newLiveP2pReadinessFixture(t)
			barrier := fixture.route.routeState()
			fixture.manager.RemoveTransport(fixture.direct)
			for endpoint, client := range []*routeClient{fixture.route.source, fixture.route.destination} {
				for _, send := range []bool{false, true} {
					if endpoint == unchangedEndpoint && send == unchangedSend {
						continue
					}
					state := fixture.peerState
					state.Send = send
					for range 2 {
						state.Connected = false
						client.routeStateTrace.Observe(state)
						state.Connected = true
						client.routeStateTrace.Observe(state)
					}
				}
			}
			fixture.manager.UpdateTransport(fixture.direct, []clientconnect.Route{make(clientconnect.Route, 1)})
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := fixture.route.waitForRebuiltDirectRoute(ctx, barrier); !errors.Is(err, context.Canceled) {
				t.Fatalf("unchanged endpoint=%d send=%t satisfied P2P replacement: err=%v", unchangedEndpoint, unchangedSend, err)
			}
		}
	}
}

// A blackhole has already retired both directions. Restoration must require
// new live routes but must not wait for another nonexistent old connection.
func TestLiveP2pReadinessRestoresAbsentDirections(t *testing.T) {
	fixture := newLiveP2pReadinessFixture(t)
	fixture.manager.RemoveTransport(fixture.direct)
	for _, client := range []*routeClient{fixture.route.source, fixture.route.destination} {
		for _, send := range []bool{false, true} {
			state := fixture.peerState
			state.Send = send
			state.Connected = false
			client.routeStateTrace.Observe(state)
		}
	}
	barrier := fixture.route.routeState()
	fixture.replaceDirect()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := fixture.route.waitForRebuiltDirectRoute(ctx, barrier); err != nil {
		t.Fatalf("restored absent P2P directions were rejected: %v", err)
	}
}

// Fallback is not proven until the surviving physical direction retires;
// an unrelated platform route-count change is never part of this condition.
func TestP2pRouteStateTraceWithdrawalRequiresBothDirections(t *testing.T) {
	for _, survivingSend := range []bool{false, true} {
		fixture := newLiveP2pReadinessFixture(t)
		state := fixture.peerState
		state.Send = !survivingSend
		state.Connected = false
		trace := fixture.route.source.routeStateTrace
		trace.Observe(state)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := trace.WaitForNoRoutes(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("surviving direction send=%t satisfied withdrawal: err=%v", survivingSend, err)
		}
		state.Send = survivingSend
		trace.Observe(state)
		if _, err := trace.WaitForNoRoutes(t.Context()); err != nil {
			t.Fatalf("complete withdrawal was rejected: %v", err)
		}
	}
}

// An exact wait-boundary hook publishes a finite route history only after the
// caller has read its initial level. Both old and current observer APIs use it.
type routeCountReadinessObserver struct {
	*clientconnect.TestingMultiRouteWriterRouteStateObserver
	publish     func()
	publishOnce sync.Once
}

// Current-level waits receive a historical edge after the forced publication.
func (self *routeCountReadinessObserver) WaitAfter(
	ctx context.Context,
	generation uint64,
) (clientconnect.TestingMultiRouteWriterRouteState, error) {
	self.publishOnce.Do(self.publish)
	return self.TestingMultiRouteWriterRouteStateObserver.WaitAfter(ctx, generation)
}

// Matching-history waits cross the same barrier, exposing the pre-fix false
// readiness even when publication and withdrawal both precede consumption.
func (self *routeCountReadinessObserver) WaitForActiveRouteCountAfter(
	ctx context.Context,
	generation uint64,
	activeRouteCount int,
) (clientconnect.TestingMultiRouteWriterRouteState, error) {
	self.publishOnce.Do(self.publish)
	return self.TestingMultiRouteWriterRouteStateObserver.WaitForActiveRouteCountAfter(ctx, generation, activeRouteCount)
}

// Force 1 -> 2 -> 1 strictly between the initial read and the wait. Closing
// the observer bounds the unavailable future proof without a negative timeout.
func TestRouteCountReadinessRejectsWithdrawnPublication(t *testing.T) {
	fixture := newLiveP2pReadinessFixture(t)
	fixture.manager.RemoveTransport(fixture.direct)
	observer := &routeCountReadinessObserver{
		TestingMultiRouteWriterRouteStateObserver: fixture.route.routeStateObserver,
		publish: func() {
			fixture.manager.UpdateTransport(fixture.direct, []clientconnect.Route{make(clientconnect.Route, 1)})
			fixture.manager.RemoveTransport(fixture.direct)
			fixture.route.routeStateObserver.Close()
		},
	}
	if err := waitForRouteCount(t.Context(), observer, 2); err == nil {
		t.Fatal("withdrawn publication satisfied current route readiness")
	}
}

// The same boundary still admits a promotion that remains current.
func TestRouteCountReadinessAcceptsCurrentPublication(t *testing.T) {
	fixture := newLiveP2pReadinessFixture(t)
	fixture.manager.RemoveTransport(fixture.direct)
	observer := &routeCountReadinessObserver{
		TestingMultiRouteWriterRouteStateObserver: fixture.route.routeStateObserver,
		publish: func() {
			fixture.manager.UpdateTransport(fixture.direct, []clientconnect.Route{make(clientconnect.Route, 1)})
			fixture.route.routeStateObserver.Close()
		},
	}
	if err := waitForRouteCount(t.Context(), observer, 2); err != nil {
		t.Fatalf("current publication did not satisfy readiness: %v", err)
	}
}
