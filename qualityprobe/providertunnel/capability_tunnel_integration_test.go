// A capability measurement tunnel end to end: through the real Open path,
// with the in-process credential and control authority, to a real provider
// over a local platform.
package providertunnel

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Waits until tunnel's window admits a client, and returns its id. The
// window admits a capability tunnel's client only after the platform has
// processed its key registration (ClientKeyRegistrationRequired).
func waitCapabilityTunnelClientAdmitted(t testing.TB, ctx context.Context, tunnel *Tunnel) connect.Id {
	t.Helper()
	admitted := make(chan connect.Id, 1)
	offer := func(events map[connect.Id]*connect.ProviderEvent) {
		for clientId, event := range events {
			if event != nil && event.State.IsActive() {
				select {
				case admitted <- clientId:
				default:
				}
			}
		}
	}
	monitor := tunnel.multiClient.Monitor()
	unsubscribe := monitor.AddMonitorEventCallback(func(_ *connect.WindowExpandEvent, events map[connect.Id]*connect.ProviderEvent, _ bool) {
		offer(events)
	})
	defer unsubscribe()
	// a client admitted before the subscription
	offer(monitor.ProviderEvents())
	select {
	case clientId := <-admitted:
		return clientId
	case <-ctx.Done():
		t.Fatal("the capability tunnel's window admitted no client")
		return connect.Id{}
	}
}

// Two capability measurement tunnels at once, as two measurements of a pass
// run them, each with a recorder and a platform transport budget of its own,
// through the real Open path with the in-process credential and control
// authority (localclient) and no control API, to a provider that runs the
// current app's opportunistic sessions. Each window admits its client only
// after the platform processed the client's key registration through that
// local control: GetClientPublicKey, the read behind /key/<client_id> and
// capprobe's own peer key fetcher, resolves the derived client's published
// key. Each recorder sees the provider's responder answer the client's
// ClientHello with the post-quantum hybrid group, and its acknowledgements
// advertise a receive window. Close retires both derived clients.
func TestCapabilityTunnelRegistersItsClientKeyThroughTheLocalAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		stack, closeStack := newCapabilityTestStack(tb, ctx)
		defer closeStack()

		defer useHermeticTunnelClientSettings()()
		defaultBudget := connect.DefaultPlatformTransportSettings().PlatformTransportBudget.Stats()
		type capabilityTunnel struct {
			tunnel   *Tunnel
			recorder *connect.PeerCapabilityRecorder
		}
		tunnels := []capabilityTunnel{}
		for range 2 {
			recorder := connect.NewPeerCapabilityRecorder(8)
			cfg := Config{
				ApiUrl:                       stack.ApiUrl,
				PlatformUrl:                  stack.PlatformUrl,
				ByJwt:                        stack.SourceJwt,
				ClientId:                     connect.Id(stack.SourceClientId),
				ClientCredentials:            stack.Credentials(),
				ClientControl:                stack.Control(),
				CloseTimeout:                 15 * time.Second,
				ContractReservationByteCount: 1024 * 1024,
				PlatformTransportBudget:      connect.NewPlatformTransportBudget(defaultBudget.TotalByteCount, defaultBudget.MaxTransportCount),
				CapabilityObservation: &CapabilityObservation{
					Recorder: recorder,
					NewPeerClientPublicKeyFetcher: func(peerId connect.Id) func(context.Context) ([]byte, error) {
						return func(ctx context.Context) ([]byte, error) {
							return model.GetClientPublicKey(ctx, server.Id(peerId))
						}
					},
				},
			}
			tunnel, err := Open(ctx, cfg, connect.Id(stack.ProviderClientId))
			if err != nil {
				tb.Fatal(err)
			}
			defer tunnel.Close()
			tunnels = append(tunnels, capabilityTunnel{tunnel: tunnel, recorder: recorder})
		}

		derivedClientIds := []connect.Id{}
		for index, opened := range tunnels {
			derivedClientId := waitCapabilityTunnelClientAdmitted(tb, ctx, opened.tunnel)
			for _, earlier := range derivedClientIds {
				if earlier == derivedClientId {
					tb.Fatalf("tunnels %d and an earlier one share the derived client %s", index, derivedClientId)
				}
			}
			derivedClientIds = append(derivedClientIds, derivedClientId)
			publicKey, err := model.GetClientPublicKey(ctx, server.Id(derivedClientId))
			if err != nil || len(publicKey) != ed25519.PublicKeySize {
				tb.Fatalf("tunnel %d: the platform resolves %d bytes for its admitted client's key (%v), want its %d-byte published key", index, len(publicKey), err, ed25519.PublicKeySize)
			}
			var sourceId server.Id
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT source_client_id FROM network_client WHERE client_id=$1`, server.Id(derivedClientId)).Scan(&sourceId))
			})
			if sourceId != stack.SourceClientId {
				tb.Fatalf("tunnel %d: the admitted client is not derived from the probe's client", index)
			}
			// the provider's responder answers the window's ClientHello with
			// the hybrid group, and its acknowledgements carry a window
			for evidenced := false; !evidenced; {
				observation, found := opened.recorder.Get(connect.Id(stack.ProviderClientId))
				evidenced = found && observation.E2eNegotiated && observation.PostQuantum && observation.KeyExchange == "X25519MLKEM768" &&
					observation.AckObserved && observation.WindowAdvertised
				if !evidenced {
					select {
					case <-ctx.Done():
						tb.Fatalf("tunnel %d: the recorder saw no post-quantum session and window from the provider: %+v", index, observation)
					case <-time.After(20 * time.Millisecond):
					}
				}
			}
		}
		for index, opened := range tunnels {
			if err := opened.tunnel.Close(); err != nil {
				tb.Fatalf("tunnel %d: joined retirement: %v", index, err)
			}
		}
		if mint, control, retire := stack.AuthorityCalls(); mint < 2 || control == 0 || retire < 2 {
			tb.Fatalf("missing local mint, control or retirement: mint=%d control=%d retire=%d", mint, control, retire)
		}
		var liveDerivedCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client WHERE source_client_id=$1 AND active`, stack.SourceClientId).Scan(&liveDerivedCount))
		})
		if liveDerivedCount != 0 {
			tb.Fatalf("%d derived clients are live after both tunnels closed", liveDerivedCount)
		}
		if apiCalls := stack.ApiCalls(); apiCalls != 0 {
			tb.Fatalf("the control API was used %d times", apiCalls)
		}
	})
}
