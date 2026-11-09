//go:build sidecar_pending

package connect

// §6's M1 exit row "push resyncs after a resident restart" on T3 (PROTO-M1-11), on a real exchange and
// the private test stack, which needs MG0's network_client_owner for the strict_exp mark: a sidecar's
// client takes a reset and hints from its resident's publisher; its resident is killed while the owners
// keep writing; the client reconnects to a new resident, whose publisher sends a reset of a new epoch
// holding every version written meanwhile, then hints whose seq only rises; and the state the client
// holds from the reset and hints equals what the owners wrote.

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

func TestSidecarPushResyncsAfterAResidentRestart(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server.Testing_EnableKeyspaceNotifications(ctx)

		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, func(settings *ExchangeSettings) {
			settings.KeyEventDelivery.Enabled = true
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

		clientId, clientJwt := env.authClient(&model.AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true})
		received := make(chan *testSidecarPushMessage, 64)
		client := env.newClient(clientId)
		defer client.Close()
		unregister, err := connectlib.RegisterSubprotocol(
			client,
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
					t.Errorf("a push frame dropped by the test")
				}
			},
			connectlib.WithSubprotocolControlSource(),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer unregister()
		transport := env.newTransport(clientJwt, server.NewId(), client.RouteManager())
		defer transport.Close()

		awaitStart := func() {
			select {
			case startedId := <-started:
				if startedId != clientId {
					t.Fatalf("a publisher started for %s", startedId)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("no publisher started")
			}
		}
		await := func(what string) *testSidecarPushMessage {
			select {
			case message := <-received:
				return message
			case <-time.After(30 * time.Second):
				t.Fatalf("no %s", what)
				return nil
			}
		}
		// what the client holds: each topic's version, from resets and hints, as push_state.go keeps it
		held := map[string]uint64{}
		applyReset := func(reset *sidecarprotocol.PushReset) {
			if !reset.Complete {
				t.Fatalf("a partial reset %+v", reset)
			}
			for _, version := range reset.Versions {
				held[version.Topic] = max(held[version.Topic], version.Version)
			}
		}

		awaitStart()
		first := await("first reset")
		if first.reset == nil || first.reset.Epoch == 0 {
			t.Fatalf("the first frame is %+v", first)
		}
		applyReset(first.reset)

		writer, err := pushservice.NewHintWriter(model.NewSidecarPushRedis(nil), nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		written := uint64(0)
		write := func() {
			written += 1
			if _, err := writer.HintClient(ctx, connectlib.Id(clientId), &sidecar.ClientHint{Topic: sidecar.PushTopicTeam, Version: written}); err != nil {
				t.Fatal(err)
			}
		}
		lastSeq := uint64(0)
		applyHint := func(hint *sidecarprotocol.PushHint) {
			if hint.Seq <= lastSeq {
				t.Errorf("seq %d after %d", hint.Seq, lastSeq)
			}
			lastSeq = hint.Seq
			held[hint.Topic] = max(held[hint.Topic], hint.Version)
		}
		for range 3 {
			write()
			message := await("hint")
			if message.hint == nil {
				t.Fatalf("a hint arrived as %+v", message)
			}
			applyHint(message.hint)
		}

		// the resident dies mid-stream, and the owners keep writing while no resident holds the client
		resident := env.exchange.testingResident(clientId)
		if resident == nil {
			t.Fatal("no resident")
		}
		resident.Cancel()
		for range 2 {
			write()
		}
		awaitStart()
		// what the dying publisher sent before it ended is the stream's; the new one opens with a reset
		var reset *sidecarprotocol.PushReset
		for reset == nil {
			message := await("the new resident's reset")
			if message.hint != nil {
				applyHint(message.hint)
				continue
			}
			reset = message.reset
		}
		if reset.Epoch == first.reset.Epoch {
			t.Errorf("the new publisher kept the old epoch %d", reset.Epoch)
		}
		applyReset(reset)
		if held[sidecar.PushTopicTeam] != written {
			t.Errorf("after the reset the client holds team %d, the owners wrote %d", held[sidecar.PushTopicTeam], written)
		}
		// hints after the reset: seq only rises, and the state follows the owners'
		lastSeq = 0
		for range 2 {
			write()
			message := await("hint after the reset")
			if message.hint == nil {
				t.Fatalf("a frame after the reset is %+v", message)
			}
			applyHint(message.hint)
		}
		if held[sidecar.PushTopicTeam] != written {
			t.Errorf("the client holds team %d, the owners wrote %d", held[sidecar.PushTopicTeam], written)
		}
	})
}
