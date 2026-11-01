package api

// The api router reports a provider that keeps retrying a terminal refusal
// (controller/connect_control_stuck_provider.go) through the real route,
// credentials and Redis flag. A provider whose packs carry a sender role is
// never reported, and the router alt mounts reports nothing.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Reports on the third answered retry of a run, at any span, with the switch
// on for every block while the control sets none.
func newStuckProviderTestRouter(ctx context.Context, uploadCtx context.Context) (*router.Router, func(), error) {
	settings := controller.DefaultStuckProviderSignalSettings()
	settings.EnableStuckProviderSignal = true
	settings.StuckProviderSignalDefault = model.ConnectLocalTransportAll
	settings.MinRetryDuration = 0
	settings.MinRetryCount = 3
	return newRouterWithStuckProviderSignals(ctx, uploadCtx, settings)
}

func createContractFrameWithRole(t testing.TB, destinationId server.Id, role protocol.SequenceRole) *protocol.Frame {
	t.Helper()
	frame, err := connect.ToFrame(&protocol.CreateContract{
		DestinationId:     destinationId.Bytes(),
		TransferByteCount: uint64(1024 * 1024),
		SenderRole:        &role,
	}, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func stuckProviderTestFlagTtl(ctx context.Context, clientId server.Id) time.Duration {
	var ttl time.Duration
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		ttl, err = r.PTTL(ctx, "{"+clientId.String()+"}c_recycle").Result()
		server.Raise(err)
	})
	return ttl
}

func waitForStuckProviderTestFlag(ctx context.Context, clientId server.Id, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for stuckProviderTestFlagTtl(ctx, clientId) <= 0 {
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return true
}

func TestConnectControlRouteReportsAStuckProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture, closeFixture := newControlReplayFixtureWithRouter(t, ctx, newStuckProviderTestRouter)
		defer closeFixture()
		retiredId := fixture.retiredIds[0]

		// a provider on a newer sdk names its lane: answered, never reported
		roleDeviceId, roleProviderId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, fixture.networkId, roleDeviceId, roleProviderId, "synthetic-role-provider", "synthetic")
		roleToken := fixture.token(roleDeviceId, roleProviderId, nil)
		for range 6 {
			code, results, _ := fixture.post(t, roleToken, createContractFrameWithRole(t, retiredId, protocol.SequenceRole_SequenceRoleServer))
			if code != http.StatusOK || !isReliabilityRefusal(results, 1) {
				t.Fatalf("a retry with a sender role was not refused: status=%d results=%+v", code, results)
			}
		}

		// an old provider: the first request reads the lifecycle, the next
		// three are replays, and the third replay crosses the run
		token := fixture.token(fixture.deviceId, fixture.providerId, nil)
		for range 4 {
			code, results, _ := fixture.post(t, token, createContractFrame(t, retiredId))
			if code != http.StatusOK || !isReliabilityRefusal(results, 1) {
				t.Fatalf("a retry was not refused: status=%d results=%+v", code, results)
			}
		}
		if !waitForStuckProviderTestFlag(ctx, fixture.providerId, 10*time.Second) {
			t.Fatal("the stuck provider was not reported")
		}
		if ttl := stuckProviderTestFlagTtl(ctx, fixture.providerId); controller.DefaultStuckProviderSignalSettings().Cooldown < ttl {
			t.Fatalf("the flag's ttl %s is past the cooldown", ttl)
		}
		if 0 < stuckProviderTestFlagTtl(ctx, roleProviderId) {
			t.Fatal("a provider with a sender role was reported")
		}
	})
}

// The router alt mounts keeps the ceiling off: the same retries report nothing.
func TestConnectControlSharedRouterReportsNoStuckProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture, closeFixture := newControlReplayFixture(t, ctx)
		defer closeFixture()
		retiredId := fixture.retiredIds[0]

		token := fixture.token(fixture.deviceId, fixture.providerId, nil)
		for range 6 {
			code, results, _ := fixture.post(t, token, createContractFrame(t, retiredId))
			if code != http.StatusOK || !isReliabilityRefusal(results, 1) {
				t.Fatalf("a retry was not refused: status=%d results=%+v", code, results)
			}
		}
		if waitForStuckProviderTestFlag(ctx, fixture.providerId, 2*time.Second) {
			t.Fatal("the shared router reported a stuck provider")
		}
	})
}
