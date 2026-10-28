package connect

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"
	"time"

	connectlib "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func newClientCloseTestEnv(ctx context.Context, t testing.TB) *peerDiscoveryEnv {
	server.Testing_EnableKeyspaceNotifications(ctx)
	return testing_newPeerDiscoveryEnvWithSettings(ctx, t, func(settings *ExchangeSettings) {
		settings.KeyEventDelivery.Enabled = true
		// far outside the test window: delivery must come from key events
		settings.KeyEventDelivery.CorrectivePollInterval = 10 * time.Minute
		settings.KeyEventDelivery.ResyncSpreadTimeout = 1 * time.Second
	})
}

func waitFor(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !cond() {
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

func (self *Exchange) testingHoldsClient(clientId server.Id) (connections int, resident bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.connections[clientId]), self.residents[clientId] != nil
}

// Close requests added for a client reach it from its resident as one
// `CloseSequences` from the control id (CLIENTCLOSE.md section 2).
func TestExchangeClientCloseRequestsReachTheClient(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := newClientCloseTestEnv(ctx, t)
		defer env.Close()

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device a",
		})
		clientIdB, _ := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device b",
		})

		received := make(chan *protocol.CloseSequences, 16)
		clientA := env.newClient(clientIdA)
		defer clientA.Close()
		removeCallback := clientA.AddReceiveCallback(func(source connectlib.TransferPath, frames []*protocol.Frame, peer connectlib.Peer) {
			if !source.IsControlSource() {
				return
			}
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_TransferCloseSequences {
					continue
				}
				message, err := connectlib.FromFrame(frame)
				if err != nil {
					continue
				}
				select {
				case received <- message.(*protocol.CloseSequences):
				default:
				}
			}
		})
		defer removeCallback()
		transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
		defer transportA.Close()
		if !waitFor(ctx, 10*time.Second, func() bool {
			_, resident := env.exchange.testingHoldsClient(clientIdA)
			return resident
		}) {
			t.Fatal("client a has no resident")
		}
		// past the registration's own take, so the requests below arrive by
		// their key event
		select {
		case <-time.After(time.Second):
		}

		contractIds := []server.Id{server.NewId(), server.NewId()}
		peerId := server.NewId()
		model.AddClientCloseRequests(ctx, clientIdA, contractIds, []server.Id{peerId})
		// another client's requests are not a's
		model.AddClientCloseRequests(ctx, clientIdB, []server.Id{server.NewId()}, nil)

		select {
		case closeSequences := <-received:
			gotContractIds := []server.Id{}
			for _, contractIdBytes := range closeSequences.ContractIds {
				gotContractIds = append(gotContractIds, server.RequireIdFromBytes(contractIdBytes))
			}
			gotPeerIds := []server.Id{}
			for _, peerIdBytes := range closeSequences.PeerIds {
				gotPeerIds = append(gotPeerIds, server.RequireIdFromBytes(peerIdBytes))
			}
			slices.SortFunc(gotContractIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
			slices.SortFunc(contractIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
			if !slices.Equal(gotContractIds, contractIds) || !slices.Equal(gotPeerIds, []server.Id{peerId}) {
				t.Fatalf("client a got %d contracts and %d peers, want 2 and 1", len(gotContractIds), len(gotPeerIds))
			}
		case <-time.After(10 * time.Second):
			t.Fatal("client a got no CloseSequences")
		}

		// the requests were taken once
		select {
		case <-received:
			t.Fatal("client a got a second CloseSequences")
		case <-time.After(2 * time.Second):
		}
		if requests := model.TakeClientCloseRequests(ctx, clientIdA, model.ClientCloseTakeLimit); !requests.Empty() {
			t.Fatal("client a's requests were left behind")
		}
		if requests := model.TakeClientCloseRequests(ctx, clientIdB, model.ClientCloseTakeLimit); len(requests.ContractIds) != 1 {
			t.Fatal("client b's requests were taken without a resident")
		}
	})
}

// An api removal closes the removed client's transports with the typed
// authorization close and cancels its resident at once, well inside the 60 s
// lease recheck, and leaves other clients alone (CLIENTCLOSE.md section 4).
func TestExchangeClosesRemovedClient(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := newClientCloseTestEnv(ctx, t)
		defer env.Close()

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device a",
		})
		clientIdB, byClientJwtB := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device b",
		})

		ws, closeA, err := dialSessionAdmission(ctx, env, "h1plus", byClientJwtA, "")
		if err != nil {
			t.Fatal(err)
		}
		defer closeA()
		clientB := env.newClient(clientIdB)
		defer clientB.Close()
		transportB := env.newTransport(byClientJwtB, server.NewId(), clientB.RouteManager())
		defer transportB.Close()
		if !waitFor(ctx, 10*time.Second, func() bool {
			connectionsA, residentA := env.exchange.testingHoldsClient(clientIdA)
			connectionsB, residentB := env.exchange.testingHoldsClient(clientIdB)
			return 0 < connectionsA && residentA && 0 < connectionsB && residentB
		}) {
			t.Fatal("clients a and b are not both connected")
		}

		removedAt := time.Now()
		result, err := model.RemoveNetworkClient(&model.RemoveNetworkClientArgs{ClientId: clientIdA}, env.userSession)
		if err != nil || result.Error != nil {
			t.Fatalf("remove client a: %v", err)
		}

		want := uint32(session.AuthorizationRejectedCloseCode - 4000)
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			_, message, err := ws.ReadMessage()
			if err != nil {
				t.Fatalf("client a's transport closed without a typed close: %v", err)
			}
			if len(message) == 5 && message[0] == connectlib.TransportControlClose {
				if got := binary.BigEndian.Uint32(message[1:]); got != want {
					t.Fatalf("client a's transport closed with %d, want %d", got, want)
				}
				break
			}
		}
		if session.AuthorizationRecheckInterval <= time.Since(removedAt) {
			t.Fatal("the close came no sooner than the lease recheck")
		}
		if !waitFor(ctx, 10*time.Second, func() bool {
			connections, resident := env.exchange.testingHoldsClient(clientIdA)
			return connections == 0 && !resident
		}) {
			t.Fatal("the exchange still holds the removed client")
		}

		connectionsB, residentB := env.exchange.testingHoldsClient(clientIdB)
		if connectionsB == 0 || !residentB {
			t.Fatal("removing client a closed client b")
		}
	})
}
