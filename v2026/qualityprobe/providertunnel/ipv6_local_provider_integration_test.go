package providertunnel

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
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

// One origin host served in both families on one port number: over IPv4 at
// 127.0.0.1 and over IPv6 at ::1, each over TCP and UDP, each answering with
// its own family, so a leg that crossed families would read the other's body.
type ipv6TestOrigin struct {
	port        int
	ipv4, ipv6  atomic.Int64
	h3ipv4      atomic.Int64
	h3ipv6      atomic.Int64
	closeOrigin func()
}

func startIpv6TestOrigin(tb testing.TB, host string) *ipv6TestOrigin {
	tb.Helper()
	leaf, key := issueLeaf(tb, host)
	certificates := []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}
	origin := &ipv6TestOrigin{}
	var v6Tcp, v4Tcp net.Listener
	for attempt := 0; ; attempt++ {
		var err error
		if v6Tcp, err = net.Listen("tcp6", "[::1]:0"); err != nil {
			tb.Fatal("the origin needs IPv6 loopback", err)
		}
		origin.port = v6Tcp.Addr().(*net.TCPAddr).Port
		if v4Tcp, err = net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(origin.port)); err == nil {
			break
		}
		v6Tcp.Close()
		if 10 <= attempt {
			tb.Fatal("no port free in both families", err)
		}
	}
	serve := func(listener net.Listener, family string, count *atomic.Int64) *httptest.Server {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			_, _ = io.WriteString(w, "origin over "+family+" "+r.Proto)
		}))
		server.Listener.Close()
		server.Listener = listener
		server.TLS = &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12}
		server.StartTLS()
		return server
	}
	v6Https := serve(v6Tcp, "IPv6", &origin.ipv6)
	v4Https := serve(v4Tcp, "IPv4", &origin.ipv4)
	quicOrigin := func(network string, ip net.IP, family string, count *atomic.Int64) func() {
		udp, err := net.ListenUDP(network, &net.UDPAddr{IP: ip, Port: origin.port})
		if err != nil {
			tb.Fatal("the origin's UDP port", err)
		}
		transport := &quic.Transport{Conn: udp}
		listener, err := transport.ListenEarly(http3.ConfigureTLSConfig(&tls.Config{Certificates: certificates}), &quic.Config{})
		if err != nil {
			tb.Fatal(err)
		}
		h3 := &http3.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			_, _ = io.WriteString(w, "origin over "+family+" "+r.Proto)
		})}
		go h3.ServeListener(listener)
		return func() {
			h3.Close()
			listener.Close()
			transport.Close()
			udp.Close()
		}
	}
	closeV6Quic := quicOrigin("udp6", net.IPv6loopback, "IPv6", &origin.h3ipv6)
	closeV4Quic := quicOrigin("udp4", net.IPv4(127, 0, 0, 1), "IPv4", &origin.h3ipv4)
	origin.closeOrigin = func() {
		closeV6Quic()
		closeV4Quic()
		v6Https.Close()
		v4Https.Close()
	}
	return origin
}

// In-tunnel DoH, over IPv4, answering A with the IPv4 origin and AAAA with
// the IPv6 one.
func startIpv6TestDoh(tb testing.TB) *httptest.Server {
	tb.Helper()
	dnsLeaf, dnsKey := issueLeaf(tb, "127.0.0.1")
	copy := *dnsLeaf
	copy.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	copy.DNSNames = nil
	der, err := x509.CreateCertificate(rand.Reader, &copy, testCaCert, &dnsKey.PublicKey, testCaKey)
	if err != nil {
		tb.Fatal(err)
	}
	if dnsLeaf, err = x509.ParseCertificate(der); err != nil {
		tb.Fatal(err)
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
		header := dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 60}
		message.Answers = nil
		switch question.Type {
		case dnsmessage.TypeA:
			message.Answers = []dnsmessage.Resource{{Header: header, Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}}}
		case dnsmessage.TypeAAAA:
			message.Answers = []dnsmessage.Resource{{Header: header, Body: &dnsmessage.AAAAResource{AAAA: netip.IPv6Loopback().As16()}}}
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
	return dns
}

// A dual-stack provider's IPv6 legs ride the actual provider path: the probe
// tunnel's dual-stack gVisor stack, its multi-client, the platform, and a real
// local provider whose NAT dials the IPv6 origin with an IPv6 socket of its
// host and carries its answers back, over TCP and over UDP. Each request is
// forced to its family: the IPv4 one reaches only the IPv4 origin and the
// IPv6 one only the IPv6 origin, on one name that resolves in both. Nothing
// but WebRTC candidate gathering (loopback only), the provider's egress
// policy and the tunnel's URL-target policy (both refuse loopback) is
// changed.
func TestIpv6LegsThroughALocalDualStackProvider(t *testing.T) {
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
		providerClientId, providerJwt := seed("local-ipv6-provider")
		sourceClientId, sourceJwt := seed("local-ipv6-probe")
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
		exchange, err := newPrivateTestExchange(ctx, "localipv6")
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

		dns := startIpv6TestDoh(tb)
		defer dns.Close()
		const originHost = "dual.localipv6.example"
		origin := startIpv6TestOrigin(tb, originHost)
		defer origin.closeOrigin()

		defer useHermeticTunnelClientSettings()()
		cfg := Config{ApiUrl: trap.URL, PlatformUrl: "ws" + platform.URL[4:], ByJwt: sourceJwt, ClientId: connect.Id(sourceClientId), DataOnlyProbe: true, KeepClientKeysLocal: true, ClientCredentials: sa, ClientControl: sa, CloseTimeout: 15 * time.Second,
			DnsResolverSettings: &connect.DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{dns.URL + "/dns-query"}}, ContractReservationByteCount: 1 << 20,
			ProviderIpFamily: connect.IpFamilyDualstack}
		tunnel, err := Open(ctx, cfg, connect.Id(providerClientId))
		if err != nil {
			tb.Fatal(err)
		}
		defer tunnel.Close()
		if !tunnel.tun.Ipv6Enabled() {
			tb.Fatal("a dual-stack provider's tunnel carries no IPv6")
		}
		client := tunnel.HttpClientForHosts(20*time.Second, []string{originHost})
		https := client.Transport.(*providerHttpTransport)
		// the origins are on loopback, which no URL target may be
		https.resolver.publicAddress = func(netip.Addr, int) bool { return true }
		target, _ := url.Parse("https://" + net.JoinHostPort(originHost, strconv.Itoa(origin.port)) + "/")
		get := func(leg string, roundTripper http.RoundTripper, ipVersion int) string {
			// the grant ProviderUrlProbeFamilyContext makes, for the origin's
			// own port, which is outside a URL probe's 443
			requestCtx := context.WithValue(ctx, providerUrlProbeKey{}, providerUrlProbeTarget{host: originHost, ipVersion: ipVersion})
			request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target.String(), nil)
			if err != nil {
				tb.Fatal(err)
			}
			response, err := (&http.Client{Transport: roundTripper, Timeout: 20 * time.Second}).Do(request)
			if err != nil {
				tb.Fatal(fmt.Sprintf("%s leg through the provider", leg), err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				tb.Fatal(err)
			}
			return string(body)
		}
		h3 := https.ProviderUrlProbeH3RoundTripper().(*providerH3RoundTripper)
		for _, c := range []struct {
			leg  string
			got  func() string
			want string
		}{
			{"IPv4 HTTPS", func() string { return get("IPv4 HTTPS", https, 4) }, "origin over IPv4 HTTP/1.1"},
			{"IPv6 HTTPS", func() string { return get("IPv6 HTTPS", https, 6) }, "origin over IPv6 HTTP/1.1"},
			{"IPv4 HTTP/3", func() string { return get("IPv4 HTTP/3", h3, 4) }, "origin over IPv4 HTTP/3.0"},
			{"IPv6 HTTP/3", func() string { return get("IPv6 HTTP/3", h3, 6) }, "origin over IPv6 HTTP/3.0"},
		} {
			if body := c.got(); body != c.want {
				tb.Fatalf("%s leg: %q, want %q", c.leg, body, c.want)
			}
		}
		if origin.ipv4.Load() != 1 || origin.ipv6.Load() != 1 || origin.h3ipv4.Load() != 1 || origin.h3ipv6.Load() != 1 {
			tb.Fatalf("origins saw https ipv4=%d ipv6=%d h3 ipv4=%d ipv6=%d", origin.ipv4.Load(), origin.ipv6.Load(), origin.h3ipv4.Load(), origin.h3ipv6.Load())
		}
		if err := tunnel.Close(); err != nil {
			tb.Fatal("joined retirement", err)
		}
	})
}
