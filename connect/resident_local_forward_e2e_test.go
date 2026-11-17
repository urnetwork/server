package connect

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Local forwards end to end, through the real connect handler, nomination,
// contracts and the residents' internal clients (resident_local_forward.go).
//
// Require the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.

// Two clients whose residents share one exchange exchange messages in each
// delivery mode. A message from one to the other crosses its resident's
// forward to the other's resident: in process, that forward attaches to the
// destination's resident and opens no forward socket; over tcp, it dials one.
// In both modes every message arrives and is acknowledged. Catches a forward
// that silently dials in in_process mode, or attaches in tcp mode, on the
// real production path.
func TestLocalForwardEndToEndThroughTheHandler(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the test DB env")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, mode := range localDeliveryTestModes {
			func() {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, mode.apply, nil)
				defer env.Close()
				mode.requireExchange(t, env.exchange)

				clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
					Description: "local forward a",
					DeviceSpec:  "spec a",
				})
				clientIdB, byClientJwtB := env.authClient(&model.AuthNetworkClientArgs{
					Description: "local forward b",
					DeviceSpec:  "spec b",
				})
				clientA := env.newClient(clientIdA)
				defer clientA.Close()
				clientB := env.newClient(clientIdB)
				defer clientB.Close()
				receiveB := recordPeerReceives(clientB, clientIdA)

				outboundBefore := testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", "forward"))
				attachmentsBefore := testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward])
				localFramesBefore := snapshotLocalForwardIO("received", "data").frames
				transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
				defer transportA.Close()
				transportB := env.newTransport(byClientJwtB, server.NewId(), clientB.RouteManager())
				defer transportB.Close()
				env.setProvideModes(clientA, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
				env.setProvideModes(clientB, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})

				const messageCount = 20
				acked := make(chan error, messageCount)
				for range messageCount {
					frame, err := connect.ToFrame(&protocol.SimpleMessage{Content: "local forward"}, connect.DefaultProtocolVersion)
					connect.AssertEqual(t, nil, err)
					sent := clientA.SendWithTimeout(frame, connect.Id(clientIdB), func(err error) {
						acked <- err
					}, 60*time.Second)
					connect.AssertEqual(t, true, sent)
				}
				for index := range messageCount {
					select {
					case <-receiveB:
					case <-time.After(60 * time.Second):
						t.Fatalf("mode %s: b received %d of %d", mode.name, index, messageCount)
					}
				}
				for index := range messageCount {
					select {
					case err := <-acked:
						if err != nil {
							t.Fatalf("mode %s: message %d failed: %v", mode.name, index, err)
						}
					case <-time.After(60 * time.Second):
						t.Fatalf("mode %s: a acknowledged %d of %d", mode.name, index, messageCount)
					}
				}

				attached := testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward]) - attachmentsBefore
				outbound := testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", "forward")) - outboundBefore
				localFrames := snapshotLocalForwardIO("received", "data").frames - localFramesBefore
				t.Logf("mode %s: forward attachments=%v outbound forward sockets=%v local forward frames=%v", mode.name, attached, outbound, localFrames)
				if mode.local {
					// a's forward to b, and b's acknowledgements back to a
					if attached < 1 || env.exchange.localForward.linkCount() < 1 || outbound != 0 || localFrames < messageCount {
						t.Errorf("mode %s: the forwards did not attach in process: attachments=%v links=%d outbound=%v local_frames=%v", mode.name, attached, env.exchange.localForward.linkCount(), outbound, localFrames)
					}
				} else {
					if attached != 0 || outbound < 1 || localFrames != 0 {
						t.Errorf("mode %s: the forwards did not dial: attachments=%v outbound=%v local_frames=%v", mode.name, attached, outbound, localFrames)
					}
				}
			}()
		}
	})
}
