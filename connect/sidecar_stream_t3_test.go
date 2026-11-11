package connect

// §22.5's §6 stream rows on T3 (PROTO-M5-05; D-PROTO-4): two sidecars' dispatchers on Required clients through
// server's in-process exchange and the controller's own sealed contracts, a stream between them carrying a quarter
// MiB each way intact, half-closed each way, across many StreamWindows. The T2 half, lossy links, a lost pack and a
// slow reader beside a healthy sibling, is sidecartest/stream_test.go.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"testing"
	"time"

	connectlib "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Two sidecars of env's network on its exchange, each admitted in one verified state, their dispatchers on
// production's protocol settings as adjust leaves them. Everything they hold ends with ctx and the test.
func newTestT3SidecarPair(t testing.TB, ctx context.Context, env *peerDiscoveryEnv, adjust func(settings *sidecar.SidecarProtocolSettings)) (*testT3Sidecar, *testT3Sidecar) {
	t.Helper()
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
		t.Cleanup(client.Cancel)
		transport := env.newTransport(clientJwt, server.NewId(), client.RouteManager())
		t.Cleanup(transport.Close)
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
		if adjust != nil {
			adjust(protocolSettings)
		}
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
		t.Cleanup(subprotocol.Close)
		sides = append(sides, &testT3Sidecar{clientId: clientId, sidecarId: sidecarId, subprotocol: subprotocol})
	}
	return sides[0], sides[1]
}

// The test env with the self-signed test identity, not the environment's tls.yml.
func newTestT3StreamEnv(ctx context.Context, t testing.TB) *peerDiscoveryEnv {
	return testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) {
		settings.transportTlsLoader = func(tlsSettings *server.TransportTlsSettings) (*server.TransportTls, error) {
			return server.NewTransportTls(map[string]bool{}, tlsSettings), nil
		}
	})
}

// A stream through the exchange carries a quarter MiB each way intact, each side half-closing its write side, so
// both reads end cleanly.
func TestSidecarStreamT3BytesIntactBothWays(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := newTestT3StreamEnv(ctx, t)
		defer env.Close()
		laptop, desktop := newTestT3SidecarPair(t, ctx, env, nil)

		accepted := make(chan *sidecar.SubprotocolStream, 1)
		if _, err := desktop.subprotocol.Listen("t3-stream", func(ctx context.Context, incoming *sidecar.StreamIncoming) error {
			accepted <- incoming.Stream
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		openCtx, openCancel := context.WithTimeout(ctx, 60*time.Second)
		defer openCancel()
		opened, err := laptop.subprotocol.OpenStream(openCtx, sidecar.Id(desktop.clientId), &sidecar.StreamOpenRequest{Target: "t3-stream"})
		if err != nil {
			t.Fatalf("open through the exchange: %v", err)
		}
		defer opened.Close()
		var stream *sidecar.SubprotocolStream
		select {
		case stream = <-accepted:
		case <-openCtx.Done():
			t.Fatal("no stream accepted")
		}
		defer stream.Close()

		up, down := make([]byte, 256*1024), make([]byte, 256*1024)
		for _, data := range [][]byte{up, down} {
			if _, err := rand.Read(data); err != nil {
				t.Fatal(err)
			}
		}
		exchange := func(writer *sidecar.SubprotocolStream, reader *sidecar.SubprotocolStream, data []byte, what string) {
			written := make(chan error, 1)
			go func() {
				if _, err := writer.Write(data); err != nil {
					written <- err
					return
				}
				written <- writer.CloseWrite()
			}()
			if err := reader.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
				t.Fatal(err)
			}
			read, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(read, data) {
				t.Fatalf("%s: %d of %d bytes, %v", what, len(read), len(data), err)
			}
			if err := <-written; err != nil {
				t.Fatalf("%s: the write: %v", what, err)
			}
		}
		exchange(opened, stream, up, "up")
		exchange(stream, opened, down, "down")
	})
}
