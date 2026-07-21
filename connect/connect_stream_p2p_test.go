package connect

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// TestExchangeStreamP2pPeerTraffic drives the stream-open -> p2p ->
// route-registration flow end to end through a real exchange:
//
//   - a force-stream contract between two clients of the same network adds the
//     pair to a stream, and the residents deliver `StreamOpen` to both endpoints
//   - the endpoints negotiate a real webrtc conn with signaling relayed
//     client-to-client over the platform
//   - when the conn connects, the p2p transports register with each client's
//     route manager, and a destination addressed to the peer (with no stream
//     id) matches the stream transport
//   - with the platform transports closed, contract-less traffic addressed to
//     the peer flows over the p2p conn in both directions: a->b and b->a each
//     deliver a message and return its ack
//
// Requires the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.
func TestExchangeStreamP2pPeerTraffic(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnv(ctx, t, 8099, 9019)
		defer env.Close()

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device a",
			DeviceSpec:  "spec a",
		})
		clientIdB, byClientJwtB := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device b",
			DeviceSpec:  "spec b",
		})

		clientA := env.newClient(clientIdA)
		defer clientA.Close()
		clientB := env.newClient(clientIdB)
		defer clientB.Close()

		receiveB := recordPeerReceives(clientB, clientIdA)
		receiveA := recordPeerReceives(clientA, clientIdB)

		transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
		defer transportA.Close()
		transportB := env.newTransport(byClientJwtB, server.NewId(), clientB.RouteManager())
		defer transportB.Close()

		fmt.Printf("[progress]stream p2p: provide\n")
		env.setProvideModes(clientA, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})
		env.setProvideModes(clientB, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})

		// force a stream for the a->b pair. the contract creation adds the pair
		// to a stream, and the residents deliver StreamOpen to both endpoints
		fmt.Printf("[progress]stream p2p: force stream message\n")
		sendSimpleMessage(t, clientA, clientIdB, connect.ForceStream())
		select {
		case <-receiveB:
		case <-time.After(60 * time.Second):
			t.Fatal("timeout waiting for the stream message over the platform")
		}

		// the stream endpoints negotiate a webrtc p2p conn with signaling
		// relayed over the platform. when it connects, the p2p send transport
		// registers with the route manager and matches the peer destination
		// with no stream id: the platform route plus the p2p route
		fmt.Printf("[progress]stream p2p: wait for p2p route\n")
		writerA := clientA.RouteManager().OpenMultiRouteWriter(connect.DestinationId(connect.Id(clientIdB)))
		defer clientA.RouteManager().CloseMultiRouteWriter(writerA)
		waitForActiveRoutes(ctx, t, writerA, 2)

		// contract-less traffic addressed to the peer uses the plain
		// {DestinationId: b} destination in the send sequence
		clientA.ContractManager().AddNoContractPeer(connect.Id(clientIdB))
		clientB.ContractManager().AddNoContractPeer(connect.Id(clientIdA))

		// with the platform transports closed, the p2p conn is the only route
		fmt.Printf("[progress]stream p2p: close platform transports\n")
		transportA.Close()
		transportB.Close()
		waitForActiveRoutes(ctx, t, writerA, 1)

		// contract-less messages flow over the p2p conn in both directions,
		// and the acks return over the p2p conn
		sendPeerMessage := func(from *connect.Client, toClientId server.Id, receive chan connect.Peer, direction string) {
			frame, err := connect.ToFrame(&protocol.SimpleMessage{
				Content: "hello",
			}, connect.DefaultProtocolVersion)
			connect.AssertEqual(t, err, nil)
			ack := make(chan error, 1)
			sent := from.SendWithTimeout(
				frame,
				connect.DestinationId(connect.Id(toClientId)),
				func(err error) {
					select {
					case ack <- err:
					default:
					}
				},
				60*time.Second,
			)
			connect.AssertEqual(t, sent, true)
			fmt.Printf("[progress]stream p2p: %s sent\n", direction)

			select {
			case <-receive:
				fmt.Printf("[progress]stream p2p: %s received\n", direction)
			case <-time.After(60 * time.Second):
				t.Fatalf("timeout waiting for the %s message over the p2p conn", direction)
			}
			select {
			case err := <-ack:
				connect.AssertEqual(t, err, nil)
				fmt.Printf("[progress]stream p2p: %s acked\n", direction)
			case <-time.After(60 * time.Second):
				t.Fatalf("timeout waiting for the %s ack over the p2p conn", direction)
			}
		}

		fmt.Printf("[progress]stream p2p: wait for p2p receive and ack\n")
		sendPeerMessage(clientA, clientIdB, receiveB, "a->b")
		sendPeerMessage(clientB, clientIdA, receiveA, "b->a")
		fmt.Printf("[progress]stream p2p: done\n")
	})
}

// waitForActiveRoutes polls the writer until the active route count settles at
// `routeCount`
func waitForActiveRoutes(ctx context.Context, t testing.TB, writer connect.MultiRouteWriter, routeCount int) {
	endTime := time.Now().Add(120 * time.Second)
	for {
		activeRoutes := writer.GetActiveRoutes()
		if len(activeRoutes) == routeCount {
			return
		}
		if endTime.Before(time.Now()) {
			t.Fatalf("timeout waiting for %d active routes (have %d)", routeCount, len(activeRoutes))
		}
		select {
		case <-ctx.Done():
			t.Fatal("context done waiting for active routes")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
