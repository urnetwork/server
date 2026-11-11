package connect

// FS-126's T3 half (PROTO-M6-02; D-PROTO-10): two sidecars with SidecarProtocolSettings.EnablePeerP2p on, on Required
// clients through server's in-process exchange and the controller's own contracts. After a relayed Hello, their
// sends ride connect's stream lane, which CreateContract binds to the pair's stream, and Hello's identity check and a
// data RPC hold on it; with the setting off, as by default, no contract between a pair is bound to a stream. The
// in-process exchange has no P2P carrier, so the move onto a P2P route and back is T2's (sidecartest/p2p_test.go),
// where each link's carrier is the test's.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
	"time"

	connectlib "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// How many of the contracts from source to destination the controller bound to a stream, and how many it made.
func testP2pStreamContracts(t testing.TB, ctx context.Context, source server.Id, destination server.Id) (streamCount int, count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT stream_id IS NOT NULL FROM transfer_contract WHERE source_id = $1 AND destination_id = $2`, source, destination)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var streamBound bool
				server.Raise(result.Scan(&streamBound))
				count += 1
				if streamBound {
					streamCount += 1
				}
			}
		})
	})
	return
}

func TestSidecarP2pT3StreamLaneOnlyWhenEnabled(t *testing.T) {
	if testing.Short() {
		return
	}
	for _, enable := range []bool{true, false} {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) {
				settings.transportTlsLoader = func(tlsSettings *server.TransportTlsSettings) (*server.TransportTls, error) {
					return server.NewTransportTls(map[string]bool{}, tlsSettings), nil
				}
			})
			defer env.Close()

			networkId := sidecar.Id(env.networkId)
			authority := sidecartest.NewFakeAuthority()
			sides := []*testT3Sidecar{}
			for _, name := range []string{"laptop", "desktop"} {
				clientId, clientJwt := env.authClient(&model.AuthNetworkClientArgs{Description: name, DeviceSpec: "test"})
				clientKeySeed := testT3Seed(t)
				identitySeed := testT3Seed(t)
				clientSettings := newPeerDiscoveryClientSettings()
				sidecar.ApplySidecarClientSettings(clientSettings)
				clientSettings.ClientKeySeed = bytes.Clone(clientKeySeed)
				client := connectlib.NewClient(ctx, connectlib.Id(clientId), Testing_NewControllerOutOfBandControl(ctx, clientId, clientSettings.ContractManagerSettings), clientSettings)
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
				protocolSettings.EnablePeerP2p = enable
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
				sides = append(sides, &testT3Sidecar{clientId: clientId, sidecarId: sidecarId, subprotocol: subprotocol})
			}
			laptop, desktop := sides[0], sides[1]

			callCtx, callCancel := context.WithTimeout(ctx, 60*time.Second)
			defer callCancel()
			if err := laptop.subprotocol.EnsureHello(callCtx, sidecar.Id(desktop.clientId)); err != nil {
				t.Fatalf("Hello through the exchange: %v", err)
			}
			if _, err := desktop.subprotocol.Handle(sidecarprotocol.MessageType(0x8031), func(ctx context.Context, request *sidecar.SubprotocolRequest) ([]byte, error) {
				return []byte("answer"), nil
			}, nil); err != nil {
				t.Fatal(err)
			}
			answer, err := laptop.subprotocol.Call(callCtx, sidecar.Id(desktop.clientId), sidecarprotocol.MessageType(0x8031), []byte("ask"), nil)
			if err != nil || string(answer) != "answer" {
				t.Fatalf("P2P %t: a data RPC: %q, %v", enable, answer, err)
			}
			if _, ok := laptop.subprotocol.PeerIdentity(sidecar.Id(desktop.clientId)); !ok {
				t.Errorf("P2P %t: the Hello did not hold", enable)
			}
			streamCount, count := testP2pStreamContracts(t, ctx, laptop.clientId, desktop.clientId)
			switch {
			case count == 0:
				t.Errorf("P2P %t: no contract from laptop to desktop", enable)
			case enable && streamCount == 0:
				t.Errorf("P2P on: none of %d contracts rides the pair's stream", count)
			case !enable && streamCount != 0:
				t.Errorf("P2P off: %d of %d contracts ride a stream", streamCount, count)
			}
		})
	}
}
