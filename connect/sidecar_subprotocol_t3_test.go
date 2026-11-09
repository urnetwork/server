package connect

// §6's M1 exit scenarios on T3 (PROTO-M1-11, Q-TEST-1): two sidecars' dispatchers on Required clients
// that connect through server's in-process exchange, on the controller's own contracts and the private
// test stack. Hello's identity check, the binding against C15's session key and the admission, passes
// both ways; a HeadCheck exchange runs over the sealed sessions and witnesses; and a data RPC is refused
// PERMISSION_DENIED, since until CreateContract seals a class (PROTO-M4-02) every contract reads as
// control, the narrowest. A real resident's push reaches a sidecar's dispatcher over h1 and passes its
// source guard, which drops anything else the resident sends. The T2 half, on sidecartest's exchange, is
// sidecartest/subprotocol_test.go and sidecartest/push_test.go.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	connectlib "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One sidecar's state as its dispatcher reads it: the shared verified state, read for its own id as
// Sidecar.Admitted reads it.
type testT3Admissions struct {
	authority *sidecartest.FakeAuthority
	sidecarId sidecar.Id
}

func (self *testT3Admissions) Authority() sidecar.SidecarAuthority {
	return self.authority
}

func (self *testT3Admissions) Admitted() bool {
	admission := self.authority.Admission(self.sidecarId)
	return admission != nil && admission.Row.GetRevokedAt() == 0
}

// One sidecar on the exchange: its client, dispatcher and sources.
type testT3Sidecar struct {
	clientId    server.Id
	sidecarId   sidecar.Id
	subprotocol *sidecar.Subprotocol
	device      *sidecartest.FakeHeadCheckDevice
}

// 32 random bytes.
func testT3Seed(t testing.TB) []byte {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	return seed
}

func TestSidecarSubprotocolT3HelloHeadCheckAndClosedData(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) {
			// the self-signed test identity, not the environment's tls.yml
			settings.transportTlsLoader = func(tlsSettings *server.TransportTlsSettings) (*server.TransportTls, error) {
				return server.NewTransportTls(map[string]bool{}, tlsSettings), nil
			}
		})
		defer env.Close()

		networkId := sidecar.Id(env.networkId)
		authority := sidecartest.NewFakeAuthority()
		api := sidecartest.NewFakeHeadCheckApi(networkId)
		api.Append(5)
		sides := []*testT3Sidecar{}
		for _, name := range []string{"laptop", "desktop"} {
			clientId, clientJwt := env.authClient(&model.AuthNetworkClientArgs{Description: name, DeviceSpec: "test"})
			clientKeySeed := testT3Seed(t)
			identitySeed := testT3Seed(t)
			clientSettings := newPeerDiscoveryClientSettings()
			sidecar.ApplySidecarClientSettings(clientSettings)
			clientSettings.ClientKeySeed = bytes.Clone(clientKeySeed)
			client := connectlib.NewClient(
				ctx,
				connectlib.Id(clientId),
				Testing_NewControllerOutOfBandControl(ctx, clientId, clientSettings.ContractManagerSettings),
				clientSettings,
			)
			defer client.Cancel()
			transport := env.newTransport(clientJwt, server.NewId(), client.RouteManager())
			defer transport.Close()
			env.setProvideModes(client, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})

			sidecarId := sidecar.NewId()
			binding, err := sidecar.NewSidecarIdentityBinding(&sidecar.SidecarIdentityBinding{
				NetworkId:  networkId,
				SidecarId:  sidecarId,
				ClientId:   sidecar.Id(clientId),
				Generation: 1,
				IssuedAt:   time.Now(),
			}, identitySeed, clientKeySeed)
			if err != nil {
				t.Fatal(err)
			}
			at := authority.AdmitSidecar(&sidecartest.FakeAuthorityAdmission{
				SidecarId:         sidecarId,
				MemberId:          sidecar.NewId(),
				IdentityPublicKey: ed25519.NewKeyFromSeed(identitySeed).Public().(ed25519.PublicKey),
			})
			chain := sha256.Sum256([]byte(name))
			authority.SetPin(&bridge.AuthPin{Version: at, Chain: chain[:]})
			protocolSettings := sidecar.DefaultSidecarProtocolSettings()
			subprotocol, err := sidecar.NewSubprotocol(ctx, &sidecar.SubprotocolEnv{
				Client:     client,
				Store:      sidecartest.NewFakeStore().Root(),
				Settings:   protocolSettings,
				Budget:     sidecar.NewSubprotocolBudget(protocolSettings, 64*1024*1024),
				Inbound:    sidecar.SubprotocolInboundWithoutPolicy,
				NetworkId:  networkId,
				Binding:    binding,
				Admissions: &testT3Admissions{authority: authority, sidecarId: sidecarId},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer subprotocol.Close()
			device := sidecartest.NewFakeHeadCheckDevice(sidecarId, api)
			if err := device.Auth.ReachFromApi(5); err != nil {
				t.Fatal(err)
			}
			sides = append(sides, &testT3Sidecar{clientId: clientId, sidecarId: sidecarId, subprotocol: subprotocol, device: device})
		}
		laptop, desktop := sides[0], sides[1]
		laptop.device.Peers.Admit(desktop.sidecarId)
		desktop.device.Peers.Admit(laptop.sidecarId)
		for _, side := range sides {
			stop, err := sidecar.NewHeadStopState(ctx, &sidecar.HeadStopStateEnv{
				SidecarId:         side.sidecarId,
				NetworkId:         networkId,
				Sources:           side.device.Sources(),
				Store:             sidecartest.NewFakeStore().Root(),
				Settings:          sidecar.DefaultSidecarProtocolSettings(),
				Clock:             sidecar.RealClock{},
				Log:               sidecar.NewNoopLogger(),
				WitnessBoundHours: func() (uint32, bool) { return sidecar.WitnessBoundDefaultHours, true },
				SharedGroup:       func(groupId sidecar.Id) bool { return true },
				Alarm:             func(event sidecar.SidecarEvent) {},
				RequestRound:      func() {},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stop.Close()
			detach, err := side.subprotocol.AttachHeadCheck(&sidecar.HeadCheckEnv{
				Stop:       stop,
				Candidates: func() []sidecar.HeadCheckCandidate { return nil },
				Identity: func(ctx context.Context, sidecarId sidecar.Id) (*sidecar.SidecarIdentityRecord, error) {
					return nil, context.Canceled
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer detach()
		}

		helloCtx, helloCancel := context.WithTimeout(ctx, 60*time.Second)
		defer helloCancel()
		if err := laptop.subprotocol.EnsureHello(helloCtx, sidecar.Id(desktop.clientId)); err != nil {
			t.Fatalf("Hello through the exchange: %v", err)
		}
		for _, c := range []struct {
			from *testT3Sidecar
			to   *testT3Sidecar
		}{{from: laptop, to: desktop}, {from: desktop, to: laptop}} {
			peer, ok := c.from.subprotocol.PeerIdentity(sidecar.Id(c.to.clientId))
			if !ok || peer.SidecarId != c.to.sidecarId || peer.Admission == nil {
				t.Errorf("%s holds %+v", c.from.sidecarId, peer)
			}
		}
		witnessed, err := laptop.subprotocol.HeadCheckNow(helloCtx, sidecar.Id(desktop.clientId))
		if err != nil || !witnessed {
			t.Errorf("HeadCheck through the exchange: witnessed %t: %v", witnessed, err)
		}
		// no contract carries a class yet, so a data RPC is the closed direction
		if _, err := desktop.subprotocol.Handle(sidecarprotocol.MessageType(0x8031), func(ctx context.Context, request *sidecar.SubprotocolRequest) ([]byte, error) {
			t.Errorf("a data RPC reached its handler on an unclassed contract")
			return nil, nil
		}, nil); err != nil {
			t.Fatal(err)
		}
		_, err = laptop.subprotocol.Call(helloCtx, sidecar.Id(desktop.clientId), sidecarprotocol.MessageType(0x8031), []byte("ask"), nil)
		if code, _ := sidecar.SubprotocolErrorCode(err); code != sidecarprotocol.ErrorCode_ERR_PERMISSION_DENIED {
			t.Errorf("a data RPC on an unclassed contract: %v", err)
		}
	})
}

// §6's row "ControlId push kept over h1" on T3: a real resident's send, the one its publisher uses
// (Resident.sendPublisherFrame), carries a push frame to a sidecar's client over server's h1 transport,
// and the sidecar's own dispatcher, registered as production registers it, takes it past its source
// guard; a frame of another type from the same resident over the same carrier is dropped there,
// unanswered. Each wait is on the frame's ack, which the client sends once its receive callbacks, the
// dispatcher's inline checks among them, have run. The P2P half, a forged ControlId over a P2P carrier,
// is T2's (sidecartest/push_test.go): the in-process exchange runs no P2P carrier a peer could forge
// on, and FS-126's real P2P runs at M6.
func TestSidecarPushFromARealResidentKeptByTheDispatcher(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) {
			// the self-signed test identity, not the environment's tls.yml
			settings.transportTlsLoader = func(tlsSettings *server.TransportTlsSettings) (*server.TransportTls, error) {
				return server.NewTransportTls(map[string]bool{}, tlsSettings), nil
			}
		})
		defer env.Close()
		started := make(chan *Resident, 8)
		env.exchange.afterResidentWorkersStartedForTest = func(resident *Resident) {
			select {
			case started <- resident:
			default:
			}
		}

		clientId, clientJwt := env.authClient(&model.AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test"})
		clientSettings := newPeerDiscoveryClientSettings()
		sidecar.ApplySidecarClientSettings(clientSettings)
		client := connectlib.NewClient(
			ctx,
			connectlib.Id(clientId),
			Testing_NewControllerOutOfBandControl(ctx, clientId, clientSettings.ContractManagerSettings),
			clientSettings,
		)
		defer client.Cancel()
		protocolSettings := sidecar.DefaultSidecarProtocolSettings()
		subprotocol, err := sidecar.NewSubprotocol(ctx, &sidecar.SubprotocolEnv{
			Client:    client,
			Store:     sidecartest.NewFakeStore().Root(),
			Settings:  protocolSettings,
			Budget:    sidecar.NewSubprotocolBudget(protocolSettings, 64*1024*1024),
			Inbound:   sidecar.SubprotocolInboundWithoutPolicy,
			NetworkId: sidecar.Id(env.networkId),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer subprotocol.Close()
		transport := env.newTransport(clientJwt, server.NewId(), client.RouteManager())
		defer transport.Close()

		var resident *Resident
		for resident == nil {
			select {
			case startedResident := <-started:
				if startedResident.clientId == clientId {
					resident = startedResident
				}
			case <-time.After(30 * time.Second):
				t.Fatal("no resident started")
			}
		}
		sendAcked := func(frame *protocol.Frame) {
			t.Helper()
			acked := make(chan error, 1)
			if !resident.sendPublisherFrame(frame, func(err error) {
				select {
				case acked <- err:
				default:
				}
			}) {
				connectlib.MessagePoolReturn(frame.MessageBytes)
				t.Fatal("the resident's send refused the frame")
			}
			select {
			case err := <-acked:
				if err != nil {
					t.Fatalf("the frame's ack: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("no ack")
			}
		}
		subprotocolId := connectlib.SubprotocolId(sidecar.SubprotocolId)

		// a push, as the resident's publisher sends it: delivered to the dispatcher and queued past its guard
		before := subprotocol.Stats()
		receivedBefore := client.SubprotocolStats().ReceivedById[subprotocolId]
		frame, err := pushservice.PushFrame(sidecarprotocol.MessageType_PUSH_HINT, &sidecarprotocol.PushHint{Seq: 1, Topic: sidecar.PushTopicTeam, Version: 3}, 1)
		if err != nil {
			t.Fatal(err)
		}
		sendAcked(frame)
		after := subprotocol.Stats()
		if after.QueuedCount != before.QueuedCount+1 || after.SourceDroppedCount != before.SourceDroppedCount {
			t.Errorf("a real resident's push over h1: queued %d, dropped %d", after.QueuedCount-before.QueuedCount, after.SourceDroppedCount-before.SourceDroppedCount)
		}
		if received := client.SubprotocolStats().ReceivedById[subprotocolId]; received != receivedBefore+1 {
			t.Errorf("connect delivered %d of the resident's frames to 21842", received-receivedBefore)
		}

		// another type from the same resident over the same carrier: connect hands it on, the guard drops it
		envelopeBytes, err := proto.Marshal(&sidecarprotocol.Envelope{Version: sidecar.WireVersionMajor, Type: uint32(sidecarprotocol.MessageType_PING), RequestId: 2})
		if err != nil {
			t.Fatal(err)
		}
		pingFrame, err := connectlib.ToFrame(&protocol.SubprotocolMessage{SubprotocolId: sidecar.SubprotocolId, MessageBytes: envelopeBytes}, connectlib.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		sendAcked(pingFrame)
		last := subprotocol.Stats()
		if last.SourceDroppedCount != after.SourceDroppedCount+1 || last.QueuedCount != after.QueuedCount {
			t.Errorf("a Ping from ControlId: dropped %d, queued %d", last.SourceDroppedCount-after.SourceDroppedCount, last.QueuedCount-after.QueuedCount)
		}
	})
}
