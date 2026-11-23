//go:build sidecar_e2e && sidecar_localstack && sidecar_pending

// T3Network.Sync's wait on a device that Retire takes out of the syncs (world_team.go's retired devices row). Run it
// with the private stores through the build's wrapper, -tags 'sidecar_e2e sidecar_pending sidecar_localstack', as
// world_evs_e2e_test.go's cases run.

package sidecart3

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// A device that follows nothing more, retired between Sync's look at it and its wait on it, ends that wait: the
// statement's sync answers once every device it still waits on stands, not at SyncTimeout. api refuses the device's
// reads of the log, as it refuses a removed member's deactivated client, so the device stays behind the statement.
// This is the window in which the control-plane case's member removal met another burst's sync ("team-member-1-0 did
// not follow team's log to 26: context deadline exceeded"), when Sync asked whether a device was retired only before
// it began to wait on it.
func TestT3NetworkSyncEndsItsWaitOnADeviceRetiredMeanwhile(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		world, err := NewT3World(tb, nil)
		if err != nil {
			tb.Fatal(err)
		}
		defer world.Close()
		network, err := world.AddNetwork(ctx, "a")
		if err != nil {
			tb.Fatal(err)
		}
		devices, err := network.AddSidecars(ctx, 2, nil)
		if err != nil {
			tb.Fatal(err)
		}
		owner, leaving := devices[0], devices[1]
		if err := network.Bootstrap(ctx, owner); err != nil {
			tb.Fatal(err)
		}
		if err := network.Admit(ctx, []*T3Device{leaving}); err != nil {
			tb.Fatal(err)
		}

		// api refuses the leaving device's reads of the log from now on
		bearer := leaving.Bearer()
		world.Api().SetApiHook(func(request *http.Request, serve func() (*http.Response, error)) (*http.Response, error) {
			if request.Header.Get("Authorization") != bearer || !strings.HasPrefix(request.URL.Path, "/network/config/projection") {
				return serve()
			}
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Status:     "401 Unauthorized",
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader("a deactivated client")),
				Request:    request,
			}, nil
		})
		defer world.Api().SetApiHook(nil)
		// retired as the sync begins to wait on it
		var looks atomic.Int64
		func() {
			network.stateLock.Lock()
			defer network.stateLock.Unlock()
			network.syncWaiting = func(device *T3Device) {
				if device == leaving {
					looks.Add(1)
					network.Retire(device)
				}
			}
		}()

		started := time.Now()
		pin, err := network.FileStatement(ctx, owner, bridge.SignedType_SIGNED_SIDECAR_REVOKE, &bridge.SidecarRevoke{
			SidecarId: leaving.Sidecar().Identity().SidecarId.Bytes(),
			Reason:    bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED,
		})
		if err != nil {
			tb.Fatalf("the revocation's sync, its device retired as the sync began to wait on it, after %s: %v", time.Since(started).Round(time.Millisecond), err)
		}
		if looks.Load() != 1 {
			tb.Errorf("the sync began to wait on the leaving device %d times, not once", looks.Load())
		}
		if version := leaving.Follower().Client().Pin().GetVersion(); pin.GetVersion() <= version {
			tb.Errorf("the leaving device followed the log to %d, the revocation's %d, though api refused its reads", version, pin.GetVersion())
		}
		if !network.Retired(leaving) || len(network.LiveDevices()) != 1 {
			tb.Errorf("the leaving device is not retired, or the network holds %d live devices", len(network.LiveDevices()))
		}
	})
}
