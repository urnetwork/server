// A capability measurement tunnel end to end: through the real Open path,
// with the in-process credential and control authority, to a real provider
// over a local platform.
package providertunnel

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/localclient"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
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
// authority (localclient) and no control API. Each window admits its client
// only after the platform processed the client's key registration through
// that local control: GetClientPublicKey, the read behind /key/<client_id>
// and capprobe's own peer key fetcher, resolves the derived client's
// published key, and each recorder sees the provider acknowledge the
// window's pings. Close retires both derived clients.
func TestCapabilityTunnelRegistersItsClientKeyThroughTheLocalAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		var apiCalls atomic.Int64
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiCalls.Add(1)
			http.Error(w, "API disabled", 503)
		}))
		defer trap.Close()
		seed := func(label string) (server.Id, string) {
			networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, label, userId)
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, label, label)
			code, err := model.CreateBalanceCode(ctx, model.ByteCount(8*1024*1024*1024), 24*time.Hour, 0, label+server.NewId().String(), "", "")
			if err != nil {
				tb.Fatal(err)
			}
			redeemed, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: code.Secret, NetworkId: networkId}, ctx)
			if err != nil || redeemed.Error != nil {
				tb.Fatal("balance redemption", err)
			}
			return clientId, session.NewByJwt(networkId, userId, label, false, false).Client(deviceId, clientId).Testing_Sign()
		}
		providerClientId, providerJwt := seed("capability-provider")
		sourceClientId, sourceJwt := seed("capability-probe")
		providerAuthority, err := localclient.New(ctx, providerJwt, trap.URL)
		if err != nil {
			tb.Fatal(err)
		}
		defer providerAuthority.Close()
		sourceAuthority, err := localclient.New(ctx, sourceJwt, trap.URL)
		if err != nil {
			tb.Fatal(err)
		}
		defer sourceAuthority.Close()
		counted := &countedProbeAuthority{Authority: sourceAuthority, rejectingApiUrl: trap.URL}

		host := "localcapability"
		exchange := connectserver.NewExchange(ctx, host, "connect", "test", map[int]int{7300: 7300}, map[string]string{host: "127.0.0.1"}, connectserver.DefaultExchangeSettings())
		handlerSettings := connectserver.DefaultConnectHandlerSettings()
		handlerSettings.ListenH3Port = 0
		handlerSettings.ListenDnsPort = 0
		handlerSettings.ConnectionAnnounceTimeout = 0
		handler := connectserver.NewConnectHandler(ctx, server.NewId(), exchange, handlerSettings)
		platform := httptest.NewServer(router.NewRouter(ctx, []*router.Route{router.NewRoute("GET", "/", handler.Connect)}))
		defer platform.Close()
		defer func() { handler.Close(); exchange.Close() }()

		strategySettings := connect.DefaultClientStrategySettings()
		strategySettings.EnableResilient = false
		strategy := connect.NewClientStrategy(ctx, strategySettings)
		defer strategy.Close()
		oob := connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, providerJwt, trap.URL, providerAuthority)
		provider := connect.NewClient(ctx, connect.Id(providerClientId), oob, hermeticWebRtcClientSettings(connect.DefaultClientSettings()))
		transport := connect.NewPlatformTransportWithDefaults(provider.Ctx(), strategy, provider.RouteManager(), "ws"+platform.URL[4:], &connect.ClientAuth{ByJwt: providerJwt, InstanceId: connect.NewId(), AppVersion: server.RequireVersion()})
		localNat := connect.NewLocalUserNatWithDefaults(provider.Ctx(), providerClientId.String())
		natSettings := connect.DefaultRemoteUserNatProviderSettings()
		natSettings.SecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
		nat := connect.NewRemoteUserNatProvider(provider, localNat, natSettings)
		closeProvider := func() {
			nat.Close()
			joinCtx, joinCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer joinCancel()
			_ = transport.CloseAndWait(joinCtx)
			_ = localNat.CloseAndWait(joinCtx)
			_ = provider.CloseAndWait(joinCtx)
			_ = oob.CloseAndWait(joinCtx)
		}
		defer closeProvider()
		provider.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{protocol.ProvideMode_Public: true, protocol.ProvideMode_Network: true})
		registered := false
		for !registered {
			modes, err := model.GetProvideModes(ctx, providerClientId)
			registered = err == nil && 0 < len(modes)
			if !registered {
				select {
				case <-ctx.Done():
					tb.Fatal("provider registration did not commit")
				case <-time.After(20 * time.Millisecond):
				}
			}
		}

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
				ApiUrl:                       trap.URL,
				PlatformUrl:                  "ws" + platform.URL[4:],
				ByJwt:                        sourceJwt,
				ClientId:                     connect.Id(sourceClientId),
				ClientCredentials:            counted,
				ClientControl:                counted,
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
			tunnel, err := Open(ctx, cfg, connect.Id(providerClientId))
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
			if sourceId != sourceClientId {
				tb.Fatalf("tunnel %d: the admitted client is not derived from the probe's client", index)
			}
			// the window pings the provider, and the recorder sees it answer
			acknowledged := false
			for !acknowledged {
				observation, found := opened.recorder.Get(connect.Id(providerClientId))
				acknowledged = found && observation.AckObserved
				if !acknowledged {
					select {
					case <-ctx.Done():
						tb.Fatalf("tunnel %d: the recorder saw no acknowledgement from the provider: %+v", index, observation)
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
		if counted.mint.Load() < 2 || counted.control.Load() == 0 || counted.retire.Load() < 2 {
			tb.Fatalf("missing local mint, control or retirement: mint=%d control=%d retire=%d", counted.mint.Load(), counted.control.Load(), counted.retire.Load())
		}
		var liveDerivedCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client WHERE source_client_id=$1 AND active`, sourceClientId).Scan(&liveDerivedCount))
		})
		if liveDerivedCount != 0 {
			tb.Fatalf("%d derived clients are live after both tunnels closed", liveDerivedCount)
		}
		if apiCalls.Load() != 0 {
			tb.Fatalf("the control API was used %d times", apiCalls.Load())
		}
	})
}
