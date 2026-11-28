package providertunnel

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
	"github.com/urnetwork/server/v2026/localclient"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// One URL turn that measures post-quantum, through a real local provider: the
// tunnel the URL probe opens for such a turn (data-only, its other clients'
// keys local, the turn's capability observation with a 3 s establish hold and
// the tunnel kept on a replacement), an HTTPS request to a loopback origin,
// and whether the session had sealed when the request was written. The
// provider's end-to-end responder is on or off by responder.
type postQuantumUrlTurn struct {
	body             string
	sealedAtRequest  bool
	sessionAtStart   connect.PeerCapabilitySession
	sessionAtRequest connect.PeerCapabilitySession
	observation      connect.PeerCapabilityObservation
	found            bool
	clientReplaced   bool
	derivedKeys      int
}

func runPostQuantumUrlTurn(tb testing.TB, ctx context.Context, responder bool) postQuantumUrlTurn {
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "API disabled", 503) }))
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
	providerClientId, providerJwt := seed("local-pq-provider")
	sourceClientId, sourceJwt := seed("local-pq-probe")
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
	exchange, err := newPrivateTestExchange(ctx, "localpq")
	if err != nil {
		tb.Fatal(err)
	}
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
	providerSettings := hermeticWebRtcClientSettings(connect.DefaultClientSettings())
	// a copy, so no settings value another client shares changes
	encryption := *providerSettings.EncryptionSettings
	encryption.Mode = connect.EncryptionModeOff
	if responder {
		encryption.Mode = connect.EncryptionModeOpportunistic
		encryption.NewPeerClientPublicKeyFetcher = func(peerId connect.Id) func(context.Context) ([]byte, error) {
			return func(ctx context.Context) ([]byte, error) {
				return model.GetClientPublicKey(ctx, server.Id(peerId))
			}
		}
	}
	providerSettings.EncryptionSettings = &encryption
	provider := connect.NewClient(ctx, connect.Id(providerClientId), oob, providerSettings)
	transport := connect.NewPlatformTransportWithDefaults(provider.Ctx(), strategy, provider.RouteManager(), "ws"+platform.URL[4:], &connect.ClientAuth{ByJwt: providerJwt, InstanceId: connect.NewId(), AppVersion: server.RequireVersion()})
	localNat := connect.NewLocalUserNatWithDefaults(provider.Ctx(), providerClientId.String())
	natSettings := connect.DefaultRemoteUserNatProviderSettings()
	natSettings.SecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
	nat := connect.NewRemoteUserNatProvider(provider, localNat, natSettings)
	defer func() {
		nat.Close()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer joinCancel()
		_ = transport.CloseAndWait(joinCtx)
		_ = localNat.CloseAndWait(joinCtx)
		_ = provider.CloseAndWait(joinCtx)
		_ = oob.CloseAndWait(joinCtx)
	}()
	provider.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{protocol.ProvideMode_Public: true, protocol.ProvideMode_Network: true})
	for registered := false; !registered; {
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

	// in-tunnel DoH that answers every name with the loopback origin
	dnsLeaf, dnsKey := issueLeaf(tb, "127.0.0.1")
	{
		// a DoH origin addressed by IP is verified by its IP SAN
		copied := *dnsLeaf
		copied.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
		copied.DNSNames = nil
		der, err := x509.CreateCertificate(rand.Reader, &copied, testCaCert, &dnsKey.PublicKey, testCaKey)
		if err != nil {
			tb.Fatal(err)
		}
		if dnsLeaf, err = x509.ParseCertificate(der); err != nil {
			tb.Fatal(err)
		}
	}
	dns := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		var message dnsmessage.Message
		if err != nil || message.Unpack(raw) != nil || len(message.Questions) != 1 {
			http.Error(w, "dns", 400)
			return
		}
		message.Header.Response, message.Header.Authoritative = true, true
		question := message.Questions[0]
		message.Answers = nil
		if question.Type == dnsmessage.TypeA {
			message.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}}}
		}
		raw, err = message.Pack()
		if err != nil {
			http.Error(w, "dns", 500)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(raw)
	}))
	dns.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{dnsLeaf.Raw}, PrivateKey: dnsKey}}, MinVersion: tls.VersionTLS12}
	dns.StartTLS()
	defer dns.Close()

	const originHost = "pq.localquality.example"
	leaf, key := issueLeaf(tb, originHost)
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "synthetic origin content")
	}))
	origin.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	origin.StartTLS()
	defer origin.Close()
	port := origin.Listener.Addr().(*net.TCPAddr).Port

	defer useHermeticTunnelClientSettings()()
	recorder := connect.NewPeerCapabilityRecorder(8)
	cfg := Config{
		ApiUrl: trap.URL, PlatformUrl: "ws" + platform.URL[4:], ByJwt: sourceJwt, ClientId: connect.Id(sourceClientId),
		DataOnlyProbe: true, KeepClientKeysLocal: true, ClientCredentials: sourceAuthority, ClientControl: sourceAuthority,
		CloseTimeout: 15 * time.Second, ContractReservationByteCount: 1024 * 1024,
		DnsResolverSettings: &connect.DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{dns.URL + "/dns-query"}},
		CapabilityObservation: &CapabilityObservation{
			Recorder: recorder,
			NewPeerClientPublicKeyFetcher: func(peerId connect.Id) func(context.Context) ([]byte, error) {
				return func(ctx context.Context) ([]byte, error) {
					return model.GetClientPublicKey(ctx, server.Id(peerId))
				}
			},
			EstablishHold:           3 * time.Second,
			KeepTunnelOnReplacement: true,
		},
	}
	tunnel, err := Open(ctx, cfg, connect.Id(providerClientId))
	if err != nil {
		tb.Fatal(err)
	}
	defer tunnel.Close()

	turn := postQuantumUrlTurn{}
	client := tunnel.HttpClientForHosts(30*time.Second, []string{originHost})
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			observation, _ := recorder.Get(connect.Id(providerClientId))
			turn.sessionAtStart = observation.Session
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				observation, found := recorder.Get(connect.Id(providerClientId))
				turn.sealedAtRequest = found && observation.E2eSealed
				turn.sessionAtRequest = observation.Session
			}
		},
	}), http.MethodGet, "https://"+net.JoinHostPort(originHost, strconv.Itoa(port))+"/", nil)
	if err != nil {
		tb.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		tb.Fatal("the URL request through the provider", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		tb.Fatal(err)
	}
	turn.body = string(body)
	turn.observation, turn.found = recorder.Get(connect.Id(providerClientId))
	turn.clientReplaced = tunnel.ClientReplaced()
	// the turn's derived clients published their keys: a post-quantum turn
	// reads its key, whatever KeepClientKeysLocal says for other turns
	var derivedClientIds []server.Id
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT client_id FROM network_client WHERE source_client_id = $1`, sourceClientId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var derivedClientId server.Id
				server.Raise(result.Scan(&derivedClientId))
				derivedClientIds = append(derivedClientIds, derivedClientId)
			}
		})
	})
	for _, derivedClientId := range derivedClientIds {
		if publicKey, err := model.GetClientPublicKey(ctx, derivedClientId); err == nil && 0 < len(publicKey) {
			turn.derivedKeys++
		}
	}
	if err := tunnel.Close(); err != nil {
		tb.Fatal("joined retirement", err)
	}
	return turn
}

// A provider with the end-to-end responder seals a post-quantum session
// before the turn's request is written, the hold keeping the first sends for
// it, and carries the request sealed: positive evidence. Its acknowledgements
// advertise a window.
func TestPostQuantumUrlTurnThroughALocalProvider(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		turn := runPostQuantumUrlTurn(tb, ctx, true)
		if turn.body != "synthetic origin content" {
			tb.Fatalf("the sealed turn's request read %q", turn.body)
		}
		observation := turn.observation
		if !turn.found || !observation.E2eSealed || !observation.PostQuantum || observation.KeyExchange != "X25519MLKEM768" || !observation.WindowAdvertised {
			tb.Fatalf("the recorder saw %+v", observation)
		}
		if !turn.sealedAtRequest {
			tb.Fatal("the session had not sealed when the request was written: the hold did not keep the first sends for the seal")
		}
		if turn.sessionAtStart.EpochId == (connect.Id{}) || !turn.sessionAtStart.PostQuantum || turn.sessionAtStart != turn.sessionAtRequest || turn.sessionAtRequest != observation.Session {
			tb.Fatalf("the real seal did not bind request and completion to one live sending epoch: start %+v written %+v final %+v", turn.sessionAtStart, turn.sessionAtRequest, observation.Session)
		}
		if turn.clientReplaced {
			tb.Fatal("the turn's window client was replaced")
		}
		if turn.derivedKeys == 0 {
			tb.Fatal("the post-quantum turn's derived client published no key")
		}
	})
}

// A provider without the responder carries the same request in plaintext
// once the hold ends: the URL outcome does not depend on the session, and
// the unanswered ClientHello is no negotiated session.
func TestPostQuantumUrlTurnWithoutAResponderCarriesTheSameRequest(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		turn := runPostQuantumUrlTurn(tb, ctx, false)
		if turn.body != "synthetic origin content" {
			tb.Fatalf("the turn's request read %q", turn.body)
		}
		if turn.sealedAtRequest || (turn.found && (turn.observation.E2eNegotiated || turn.observation.E2eSealed)) {
			tb.Fatalf("a provider without the responder negotiated a session: %+v", turn.observation)
		}
		if turn.found && !turn.observation.E2eAttempted {
			tb.Fatalf("the turn sent no ClientHello: %+v", turn.observation)
		}
	})
}
