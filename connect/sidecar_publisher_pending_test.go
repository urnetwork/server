//go:build sidecar_pending

package connect

// §6.8's resident publishers (PROTO-M1-09) on a real exchange and the private test stack, which needs
// MG0's network_client_owner for the strict_exp mark. A sidecar's client that connects gets a reset
// from ControlId over 21842; a hint an owner writes reaches it as a PushHint through the exchange's one
// key-event subscription; and the frame's ack raises {scp_<C>}ack once the test has set tok. A client
// without the mark reads none and gets no publisher.

import (
	"context"
	"fmt"
	"testing"
	"time"

	connectlib "github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The resident this exchange holds for clientId, nil for none.
func (self *Exchange) testingResident(clientId server.Id) *Resident {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.residents[clientId]
}

func TestExchangeResidentPublishesHintsToAStrictExpClient(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server.Testing_EnableKeyspaceNotifications(ctx)

		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, func(settings *ExchangeSettings) {
			settings.KeyEventDelivery.Enabled = true
			// far outside the test window: delivery must come from key events
			settings.KeyEventDelivery.CorrectivePollInterval = 10 * time.Minute
			settings.KeyEventDelivery.ResyncSpreadTimeout = 1 * time.Second
			settings.EnableSidecarPush = true
		}, func(settings *ConnectHandlerSettings) {
			// the self-signed test identity, not the environment's tls.yml
			settings.transportTlsLoader = func(tlsSettings *server.TransportTlsSettings) (*server.TransportTls, error) {
				return server.NewTransportTls(map[string]bool{}, tlsSettings), nil
			}
		})
		defer env.Close()
		started := make(chan server.Id, 8)
		env.exchange.afterSidecarPublisherStartForTest = func(resident *Resident, publisher *pushservice.SidecarPublisher) {
			started <- resident.clientId
		}
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  push: {enabled: true}\n  preview_networks: [%q]\n", env.networkId))()

		sidecarClientId, sidecarJwt := env.authClient(&model.AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true})
		deviceClientId, deviceJwt := env.authClient(&model.AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})

		// §18.7's projection, which api writes from M8: with it the frame's ack raises {scp_<C>}ack
		server.Redis(ctx, func(client server.RedisClient) {
			if err := client.Set(ctx, pushservice.HintTokenKey(connectlib.Id(sidecarClientId)), "token", 0).Err(); err != nil {
				t.Fatal(err)
			}
		})

		received := make(chan *testSidecarPushMessage, 64)
		sidecarClient := env.newClient(sidecarClientId)
		defer sidecarClient.Close()
		unregister, err := connectlib.RegisterSubprotocol(
			sidecarClient,
			connectlib.SubprotocolId(sidecar.SubprotocolId),
			connectlib.ProtoCodec[*sidecarprotocol.Envelope](),
			func(source connectlib.TransferPath, envelope *sidecarprotocol.Envelope, peer connectlib.Peer) {
				if !source.IsControlSource() {
					t.Errorf("a push frame from %s", source.SourceId)
					return
				}
				message, err := decodeTestSidecarPushEnvelope(envelope)
				if err != nil {
					t.Errorf("a push frame the client cannot read: %v", err)
					return
				}
				select {
				case received <- message:
				default:
				}
			},
			connectlib.WithSubprotocolControlSource(),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer unregister()
		sidecarTransport := env.newTransport(sidecarJwt, server.NewId(), sidecarClient.RouteManager())
		defer sidecarTransport.Close()

		deviceClient := env.newClient(deviceClientId)
		defer deviceClient.Close()
		deviceTransport := env.newTransport(deviceJwt, server.NewId(), deviceClient.RouteManager())
		defer deviceTransport.Close()

		select {
		case clientId := <-started:
			if clientId != sidecarClientId {
				t.Fatalf("a publisher started for %s, not the sidecar's client", clientId)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("no publisher started for the sidecar's client")
		}
		select {
		case message := <-received:
			if message.reset == nil || !message.reset.Complete || message.reset.Epoch == 0 || len(message.reset.Versions) != 0 {
				t.Fatalf("the first frame is %+v", message)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the sidecar's client got no reset")
		}

		writer, err := pushservice.NewHintWriter(model.NewSidecarPushRedis(nil), nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := writer.HintClient(ctx, connectlib.Id(sidecarClientId), &sidecar.ClientHint{Topic: sidecar.PushTopicTeam, Version: 7, Kind: 0x0201, Body: []byte{7}})
		if err != nil {
			t.Fatal(err)
		}
		// the device's hint has no publisher to carry it
		if _, err := writer.HintClient(ctx, connectlib.Id(deviceClientId), &sidecar.ClientHint{Topic: sidecar.PushTopicTeam, Version: 7}); err != nil {
			t.Fatal(err)
		}
		select {
		case message := <-received:
			hint := message.hint
			if hint == nil || hint.Seq != 1 || hint.Topic != sidecar.PushTopicTeam || hint.Version != 7 || hint.Kind != 0x0201 || string(hint.Hint) != "\x07" {
				t.Fatalf("the hint arrived as %+v", message)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the hint did not reach the sidecar's client")
		}
		ackRaised := waitFor(ctx, 10*time.Second, func() bool {
			ack := ""
			server.Redis(ctx, func(client server.RedisClient) {
				ack, _ = client.Get(ctx, pushservice.HintAckKey(connectlib.Id(sidecarClientId))).Result()
			})
			return ack == result.StreamId
		})
		if !ackRaised {
			t.Errorf("{scp_<C>}ack was not raised to %s", result.StreamId)
		}

		// each resident read its client's marks with its profile: only the sidecar's is strict_exp
		if !waitFor(ctx, 10*time.Second, func() bool { return env.exchange.testingResident(deviceClientId) != nil }) {
			t.Fatal("the device has no resident")
		}
		if marks := env.exchange.testingResident(sidecarClientId).sidecarPushMarks; !marks.StrictExp || marks.AclEnforcing {
			t.Errorf("the sidecar's marks read %+v", marks)
		}
		if marks := env.exchange.testingResident(deviceClientId).sidecarPushMarks; marks != (model.SidecarPushMarks{}) {
			t.Errorf("the device's marks read %+v", marks)
		}
		select {
		case clientId := <-started:
			t.Errorf("a second publisher started, for %s", clientId)
		default:
		}
	})
}
