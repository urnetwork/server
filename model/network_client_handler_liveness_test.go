// The readers that join a connect handler on its heartbeat
// (network_client_handler_liveness.go) against the 2026-10-19 shape: the
// heartbeat path fails for every process while each process keeps stamping
// the provider liveness writer set. Each test reads the verdict where it
// shows: the published location, the provider audit feed, the native and
// named Quality reads, and the ARIN coverage readout. The controls keep a
// process that ended ended.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The handler a connection was registered under.
func liveStaleTestHandler(ctx context.Context, t testing.TB, connectionId server.Id) handlerExpiryTestHandler {
	t.Helper()
	handler := handlerExpiryTestHandler{
		connectionId: connectionId,
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT handler_id FROM network_client_connection WHERE connection_id = $1`,
			connectionId,
		).Scan(&handler.handlerId))
	})
	return handler
}

// Stops the handler's heartbeat ten minutes ago, past two heartbeat timeouts,
// as a failing heartbeat path leaves it, and when stamped has its process
// stamp the writer set, as a live process does throughout.
func liveStaleTestStopHeartbeat(ctx context.Context, t testing.TB, handler handlerExpiryTestHandler, stamped bool) {
	t.Helper()
	handlerExpiryTestHeartbeatAt(ctx, []handlerExpiryTestHandler{handler}, server.NowUtc().Add(-10*time.Minute))
	if stamped {
		handlerExpiryTestStamp(ctx, t, []handlerExpiryTestHandler{handler})
	}
	liveStaleTestReset()
}

// A live process on another handler that heartbeats and stamps, so the writer
// set vouches: an unstamped stale handler is then judged ended.
func liveStaleTestVouchingHandler(ctx context.Context, t testing.TB) {
	t.Helper()
	handlerExpiryTestStamp(ctx, t, handlerExpiryTestHandlers(ctx, t, 1, 20101))
	liveStaleTestReset()
}

// A provider whose location the location update published connected, as every
// provider's was before the outage.
func liveStaleTestPublishedProvider(ctx context.Context, t testing.TB) *egressTestProvider {
	t.Helper()
	city := egressTestCity(ctx, "Example City", "Example Region", "United States", "us")
	provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
	now := server.NowUtc()
	UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
	if !liveStaleTestLocationConnected(ctx, t, provider.clientId) {
		t.Fatal("the location update did not publish a heartbeating provider connected")
	}
	return provider
}

// Whether the client's published location reads connected.
func liveStaleTestLocationConnected(ctx context.Context, t testing.TB, clientId server.Id) (connected bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT connected FROM network_client_location_reliability WHERE client_id = $1`,
			clientId,
		).Scan(&connected))
	})
	return
}

// The provider's handler stopped heartbeating while its process kept stamping.
// Before, the location update published the location disconnected, which an
// outage did to every provider at once.
func TestLocationReliabilityKeepsAStampedStaleHandlerConnected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		provider := liveStaleTestPublishedProvider(ctx, t)

		liveStaleTestStopHeartbeat(ctx, t, liveStaleTestHandler(ctx, t, provider.connectionId), true)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		if !liveStaleTestLocationConnected(ctx, t, provider.clientId) {
			t.Fatal("the location update published the location of a live process as disconnected")
		}
	})
}

// Every heartbeat stopped and no process stamps, so the writer set cannot vouch
// and the fleet decides: fewer handlers heartbeat than went stale, so the
// stale handler is judged live and its location stays connected.
func TestLocationReliabilityKeepsStaleHandlersWhenNoHandlerHeartbeats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		provider := liveStaleTestPublishedProvider(ctx, t)

		liveStaleTestStopHeartbeat(ctx, t, liveStaleTestHandler(ctx, t, provider.connectionId), false)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		if !liveStaleTestLocationConnected(ctx, t, provider.clientId) {
			t.Fatal("the location update took a heartbeat path that failed for the whole fleet as every provider gone")
		}
	})
}

// The control: the writer set vouches and the provider's process does not
// stamp, so the process ended and its location reads disconnected.
func TestLocationReliabilityDisconnectsAnEndedHandler(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		provider := liveStaleTestPublishedProvider(ctx, t)

		liveStaleTestVouchingHandler(ctx, t)
		liveStaleTestStopHeartbeat(ctx, t, liveStaleTestHandler(ctx, t, provider.connectionId), false)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		if liveStaleTestLocationConnected(ctx, t, provider.clientId) {
			t.Fatal("the location update kept the location of an ended process connected")
		}
	})
}

// The provider's handler stopped heartbeating while its process kept stamping.
// Before, the sweep wrote an offline event for it, and for every provider in
// an outage.
func TestProviderAuditSweepKeepsAStampedStaleHandlerOnline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		provider := createSweepTestProvider(ctx, t, "live-stale-online")
		if onlineCount, _ := SweepProviderAuditEvents(ctx); onlineCount != 1 {
			t.Fatalf("the first sweep wrote %d online events, want 1", onlineCount)
		}

		liveStaleTestStopHeartbeat(ctx, t, liveStaleTestHandler(ctx, t, provider.connectionId), true)

		if _, offlineCount := SweepProviderAuditEvents(ctx); offlineCount != 0 {
			t.Fatalf("the sweep wrote %d offline events for a live process", offlineCount)
		}
	})
}

// The control: the writer set vouches and the provider's process does not
// stamp, so the sweep takes it offline.
func TestProviderAuditSweepTakesAnEndedHandlerOffline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		provider := createSweepTestProvider(ctx, t, "live-stale-offline")
		if onlineCount, _ := SweepProviderAuditEvents(ctx); onlineCount != 1 {
			t.Fatalf("the first sweep wrote %d online events, want 1", onlineCount)
		}

		liveStaleTestVouchingHandler(ctx, t)
		liveStaleTestStopHeartbeat(ctx, t, liveStaleTestHandler(ctx, t, provider.connectionId), false)

		if _, offlineCount := SweepProviderAuditEvents(ctx); offlineCount != 1 {
			t.Fatalf("the sweep wrote %d offline events for an ended process, want 1", offlineCount)
		}
	})
}

// A Quality-verified provider for the Quality reads.
func liveStaleTestQualityProvider(ctx context.Context, t testing.TB) *egressTestProvider {
	t.Helper()
	enableSubscriberQualityPolicy(t)
	city := egressTestCity(ctx, "Example City", "Example Region", "United States", "us")
	return egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
}

// The provider's handler stopped heartbeating while its process kept stamping.
// Before, native Quality refused it as offline, as it did every candidate in
// an outage.
func TestQualitySubscriberReadKeepsAStampedStaleHandlerLive(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		provider := liveStaleTestQualityProvider(ctx, t)

		liveStaleTestStopHeartbeat(ctx, t, liveStaleTestHandler(ctx, t, provider.connectionId), true)

		negative, risky, offline, err := getProviderSubscriberExclusions(ctx, []server.Id{provider.clientId})
		if err != nil {
			t.Fatal(err)
		}
		if offline[provider.clientId] || negative[provider.clientId] || risky[provider.clientId] {
			t.Fatal("native Quality refused the provider of a live process")
		}
	})
}

// The control for native Quality: an ended process's provider is offline.
func TestQualitySubscriberReadRefusesAnEndedHandler(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		provider := liveStaleTestQualityProvider(ctx, t)

		liveStaleTestVouchingHandler(ctx, t)
		liveStaleTestStopHeartbeat(ctx, t, liveStaleTestHandler(ctx, t, provider.connectionId), false)

		_, _, offline, err := getProviderSubscriberExclusions(ctx, []server.Id{provider.clientId})
		if err != nil {
			t.Fatal(err)
		}
		if !offline[provider.clientId] {
			t.Fatal("native Quality admitted the provider of an ended process")
		}
	})
}

// The provider's handler stopped heartbeating while its process kept stamping.
// Before, a named Quality request refused it as offline, so a redial that
// named its exit changed the user's IP.
func TestNamedQualityReadKeepsAStampedStaleHandlerLive(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		provider := liveStaleTestQualityProvider(ctx, t)

		liveStaleTestStopHeartbeat(ctx, t, liveStaleTestHandler(ctx, t, provider.connectionId), true)

		offline, risky, err := getProviderNamedSubscriberExclusions(ctx, []server.Id{provider.clientId})
		if err != nil {
			t.Fatal(err)
		}
		if offline[provider.clientId] || risky[provider.clientId] {
			t.Fatal("named Quality refused the provider of a live process")
		}
	})
}

// The control for named Quality: an ended process's provider is offline.
func TestNamedQualityReadRefusesAnEndedHandler(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		provider := liveStaleTestQualityProvider(ctx, t)

		liveStaleTestVouchingHandler(ctx, t)
		liveStaleTestStopHeartbeat(ctx, t, liveStaleTestHandler(ctx, t, provider.connectionId), false)

		offline, _, err := getProviderNamedSubscriberExclusions(ctx, []server.Id{provider.clientId})
		if err != nil {
			t.Fatal(err)
		}
		if !offline[provider.clientId] {
			t.Fatal("named Quality admitted the provider of an ended process")
		}
	})
}

// The provider's handler stopped heartbeating while its process kept stamping.
// Before, the ARIN coverage readout dropped its connection, and in an outage
// read an empty fleet.
func TestArinCoverageCountsAStampedStaleHandler(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Example City", "Example Region", "United States", "us")
		provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)

		liveStaleTestStopHeartbeat(ctx, t, liveStaleTestHandler(ctx, t, provider.connectionId), true)

		coverage := GetProviderArinClassificationCoverage(ctx, 0, server.NowUtc())
		if coverage.Connections != 1 || coverage.Providers != 1 {
			t.Fatalf("coverage=%+v: the readout dropped the connection of a live process", coverage)
		}
	})
}
