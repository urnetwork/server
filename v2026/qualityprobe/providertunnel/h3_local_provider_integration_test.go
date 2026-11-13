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
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
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

// A URL's HTTP/3 leg rides the actual provider path: the probe tunnel's gVisor
// UDP socket, its multi-client, the platform, and a real local provider whose
// NAT sends the datagrams to a loopback HTTP/3 origin and carries its answers
// back. The same tunnel carries the URL's HTTPS leg to the same origin port
// over TCP first. Nothing but WebRTC candidate gathering (loopback only) and
// the provider's egress policy (which refuses loopback) is changed.
func TestH3LegThroughALocalProvider(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "API disabled", 503) }))
		defer trap.Close()
		seed := func(label string) (server.Id, string) {
			n, u, d, c := server.NewId(), server.NewId(), server.NewId(), server.NewId()
			model.Testing_CreateNetwork(ctx, n, label, u)
			model.Testing_CreateDevice(ctx, n, d, c, label, label)
			code, err := model.CreateBalanceCode(ctx, model.ByteCount(8<<30), 24*time.Hour, 0, label+server.NewId().String(), "", "")
			if err != nil {
				tb.Fatal(err)
			}
			redeemed, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: code.Secret, NetworkId: n}, ctx)
			if err != nil || redeemed.Error != nil {
				tb.Fatal("balance redemption", err)
			}
			return c, session.NewByJwt(n, u, label, false, false).Client(d, c).Testing_Sign()
		}
		providerClientId, providerJwt := seed("local-h3-provider")
		sourceClientId, sourceJwt := seed("local-h3-probe")
		pa, err := localclient.New(ctx, providerJwt, trap.URL)
		if err != nil {
			tb.Fatal(err)
		}
		defer pa.Close()
		sa, err := localclient.New(ctx, sourceJwt, trap.URL)
		if err != nil {
			tb.Fatal(err)
		}
		defer sa.Close()
		exchange, err := newPrivateTestExchange(ctx, "localquality")
		if err != nil {
			tb.Fatal(err)
		}
		hs := connectserver.DefaultConnectHandlerSettings()
		hs.ListenH3Port = 0
		hs.ListenDnsPort = 0
		hs.ConnectionAnnounceTimeout = 0
		handler := connectserver.NewConnectHandler(ctx, server.NewId(), exchange, hs)
		platform := httptest.NewServer(router.NewRouter(ctx, []*router.Route{router.NewRoute("GET", "/", handler.Connect)}))
		defer platform.Close()
		defer func() { handler.Close(); exchange.Close() }()
		strategySettings := connect.DefaultClientStrategySettings()
		strategySettings.EnableResilient = false
		strategy := connect.NewClientStrategy(ctx, strategySettings)
		defer strategy.Close()
		oob := connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, providerJwt, trap.URL, pa)
		provider := connect.NewClient(ctx, connect.Id(providerClientId), oob, hermeticWebRtcClientSettings(connect.DefaultClientSettings()))
		transport := connect.NewPlatformTransportWithDefaults(provider.Ctx(), strategy, provider.RouteManager(), "ws"+platform.URL[4:], &connect.ClientAuth{ByJwt: providerJwt, InstanceId: connect.NewId(), AppVersion: server.RequireVersion()})
		localNat := connect.NewLocalUserNatWithDefaults(provider.Ctx(), providerClientId.String())
		natSettings := connect.DefaultRemoteUserNatProviderSettings()
		natSettings.SecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
		nat := connect.NewRemoteUserNatProvider(provider, localNat, natSettings)
		closeProvider := func() {
			nat.Close()
			join, joinCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer joinCancel()
			_ = transport.CloseAndWait(join)
			_ = localNat.CloseAndWait(join)
			_ = provider.CloseAndWait(join)
			_ = oob.CloseAndWait(join)
		}
		defer closeProvider()
		provider.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{protocol.ProvideMode_Public: true, protocol.ProvideMode_Network: true})
		deadline := time.Now().Add(15 * time.Second)
		for {
			if modes, err := model.GetProvideModes(ctx, providerClientId); err == nil && len(modes) > 0 {
				break
			}
			if time.Now().After(deadline) {
				tb.Fatal("provider registration did not commit")
			}
			time.Sleep(20 * time.Millisecond)
		}

		// in-tunnel DoH that answers every name with the loopback origin
		dnsLeaf, dnsKey := issueLeaf(t, "127.0.0.1")
		{
			// a DoH origin addressed by IP is verified by its IP SAN
			copy := *dnsLeaf
			copy.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
			copy.DNSNames = nil
			der, e := x509.CreateCertificate(rand.Reader, &copy, testCaCert, &dnsKey.PublicKey, testCaKey)
			if e != nil {
				tb.Fatal(e)
			}
			if dnsLeaf, e = x509.ParseCertificate(der); e != nil {
				tb.Fatal(e)
			}
		}
		dns := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, e := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
			var message dnsmessage.Message
			if e != nil || message.Unpack(raw) != nil || len(message.Questions) != 1 {
				http.Error(w, "dns", 400)
				return
			}
			message.Header.Response, message.Header.Authoritative = true, true
			question := message.Questions[0]
			message.Answers = nil
			if question.Type == dnsmessage.TypeA {
				message.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}}}
			}
			raw, e = message.Pack()
			if e != nil {
				http.Error(w, "dns", 500)
				return
			}
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write(raw)
		}))
		dns.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{dnsLeaf.Raw}, PrivateKey: dnsKey}}, MinVersion: tls.VersionTLS12}
		dns.StartTLS()
		defer dns.Close()

		// one origin host, served over TCP and over UDP on the same port
		const originHost = "h3.localquality.example"
		leaf, key := issueLeaf(t, originHost)
		var httpsCalls, h3Calls atomic.Int64
		https := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpsCalls.Add(1)
			_, _ = io.WriteString(w, "origin over "+r.Proto)
		}))
		https.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
		https.StartTLS()
		defer https.Close()
		port := https.Listener.Addr().(*net.TCPAddr).Port
		udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			tb.Fatal("the origin's UDP port", err)
		}
		quicTransport := &quic.Transport{Conn: udp}
		listener, err := quicTransport.ListenEarly(http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}}), &quic.Config{})
		if err != nil {
			tb.Fatal(err)
		}
		h3Server := &http3.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h3Calls.Add(1)
			_, _ = io.WriteString(w, "origin over "+r.Proto)
		})}
		go h3Server.ServeListener(listener)
		defer func() {
			h3Server.Close()
			listener.Close()
			quicTransport.Close()
			udp.Close()
		}()

		defer useHermeticTunnelClientSettings()()
		cfg := Config{ApiUrl: trap.URL, PlatformUrl: "ws" + platform.URL[4:], ByJwt: sourceJwt, ClientId: connect.Id(sourceClientId), DataOnlyProbe: true, KeepClientKeysLocal: true, ClientCredentials: sa, ClientControl: sa, CloseTimeout: 15 * time.Second, DnsResolverSettings: &connect.DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{dns.URL + "/dns-query"}}, ContractReservationByteCount: 1 << 20}
		tunnel, err := Open(ctx, cfg, connect.Id(providerClientId))
		if err != nil {
			tb.Fatal(err)
		}
		defer tunnel.Close()
		client := tunnel.HttpClientForHosts(20*time.Second, []string{originHost})
		target := "https://" + net.JoinHostPort(originHost, strconv.Itoa(port)) + "/"
		get := func(client *http.Client) (string, int) {
			response, err := client.Get(target)
			if err != nil {
				tb.Fatal("leg through the provider", err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				tb.Fatal(err)
			}
			return string(body), response.ProtoMajor
		}
		if body, major := get(client); body != "origin over HTTP/1.1" || major != 1 {
			tb.Fatalf("HTTPS leg: %q HTTP/%d", body, major)
		}
		h3 := client.Transport.(*providerHttpTransport).ProviderUrlProbeH3RoundTripper()
		if h3 == nil {
			tb.Fatal("the tunnel offers no HTTP/3 leg")
		}
		if body, major := get(&http.Client{Transport: h3, Timeout: 20 * time.Second}); body != "origin over HTTP/3.0" || major != 3 {
			tb.Fatalf("HTTP/3 leg: %q HTTP/%d", body, major)
		}
		if httpsCalls.Load() != 1 || h3Calls.Load() != 1 {
			tb.Fatalf("origin saw https=%d h3=%d requests", httpsCalls.Load(), h3Calls.Load())
		}
		if err := tunnel.Close(); err != nil {
			tb.Fatal("joined retirement", err)
		}
	})
}
