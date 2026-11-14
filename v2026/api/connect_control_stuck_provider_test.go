package api

// The api router reports a provider that keeps retrying a terminal refusal
// (controller/connect_control_stuck_provider.go) through the real route,
// credentials and Redis flag. A provider whose packs carry a sender role is
// never reported, and the router alt mounts reports nothing.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

// A clock the test steps.
type stuckProviderTestClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *stuckProviderTestClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *stuckProviderTestClock) Set(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = now
}

// Reports with the default thresholds on clock, with the switch on for every
// block while the control sets none.
func newStuckProviderTestRouter(clock *stuckProviderTestClock) func(ctx context.Context, uploadCtx context.Context) (*router.Router, func(), error) {
	return func(ctx context.Context, uploadCtx context.Context) (*router.Router, func(), error) {
		settings := controller.DefaultStuckProviderSignalSettings()
		settings.EnableStuckProviderSignal = true
		settings.StuckProviderSignalDefault = model.ConnectLocalTransportAll
		settings.Now = clock.Now
		return newRouterWithStuckProviderSignals(ctx, uploadCtx, settings)
	}
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

// With the default thresholds: a provider retrying a terminal refusal every
// 2 minutes is reported once its run has lasted 20 minutes, and not before.
func TestConnectControlRouteReportsAStuckProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		clock := &stuckProviderTestClock{now: start}
		fixture, closeFixture := newControlReplayFixtureWithRouter(t, ctx, newStuckProviderTestRouter(clock))
		defer closeFixture()
		retiredId := fixture.retiredIds[0]
		defaults := controller.DefaultStuckProviderSignalSettings()

		// a provider on a newer sdk names its lane: answered, never reported
		roleDeviceId, roleProviderId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, fixture.networkId, roleDeviceId, roleProviderId, "synthetic-role-provider", "synthetic")
		roleToken := fixture.token(roleDeviceId, roleProviderId, nil)
		token := fixture.token(fixture.deviceId, fixture.providerId, nil)
		post := func(token string, frame *protocol.Frame) {
			t.Helper()
			code, results, _ := fixture.post(t, token, frame)
			if code != http.StatusOK || !isReliabilityRefusal(results, 1) {
				t.Fatalf("a retry was not refused: status=%d results=%+v", code, results)
			}
		}

		// an old provider: the first request reads the lifecycle, and the
		// replays after it, every 2 minutes, make the run
		post(token, createContractFrame(t, retiredId))
		for minute := 0; minute < 20; minute += 2 {
			clock.Set(start.Add(time.Duration(minute) * time.Minute))
			post(roleToken, createContractFrameWithRole(t, retiredId, protocol.SequenceRole_SequenceRoleServer))
			post(token, createContractFrame(t, retiredId))
		}
		// ten replays over 18 minutes: not yet
		if waitForStuckProviderTestFlag(ctx, fixture.providerId, 1*time.Second) {
			t.Fatal("the provider was reported before its run lasted the minimum")
		}
		clock.Set(start.Add(defaults.MinRetryDuration))
		post(roleToken, createContractFrameWithRole(t, retiredId, protocol.SequenceRole_SequenceRoleServer))
		post(token, createContractFrame(t, retiredId))
		if !waitForStuckProviderTestFlag(ctx, fixture.providerId, 10*time.Second) {
			t.Fatal("the stuck provider was not reported")
		}
		if ttl := stuckProviderTestFlagTtl(ctx, fixture.providerId); ttl <= 0 || defaults.Cooldown+defaults.Cooldown/4 < ttl {
			t.Fatalf("the flag's ttl %s, want (0, %s]", ttl, defaults.Cooldown+defaults.Cooldown/4)
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
