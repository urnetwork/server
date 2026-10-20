package providertunnel

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
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
	"golang.org/x/net/dns/dnsmessage"
)

// Runs check when the generator begins retiring a derived client, before the
// identity is revoked, then delegates to the real local authority.
type retirementProbeAuthority struct {
	*localclient.Authority
	check func(connect.Id)
}

func (self *retirementProbeAuthority) RemoveNetworkClient(ctx context.Context, args *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error) {
	self.check(args.ClientId)
	return self.Authority.RemoveNetworkClient(ctx, args)
}

// Counts the origin's open client connections from the server's own view.
type originConnections struct {
	open atomic.Int64
}

func (self *originConnections) connState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		self.open.Add(1)
	case http.StateClosed, http.StateHijacked:
		self.open.Add(-1)
	}
}

// A provider, a DoH resolver and a streaming origin behind the real exchange,
// provider NAT and local authority, and a probe tunnel's config for them, with
// only WebRTC candidate gathering kept on loopback. When the generator begins
// revoking a derived client, every origin the tunnel reached must already see
// its connection closed, and the provider must hold no unacknowledged return
// for that client.
type teardownReturnStack struct {
	cfg              Config
	providerClientId server.Id
	payloadUrl       string
	payloadHost      string
	dnsConnections   *originConnections

	retirementLock sync.Mutex
	retirements    int
	retirementErr  error
}

func newTeardownReturnStack(tb testing.TB, ctx context.Context) (stack *teardownReturnStack, closeStack func()) {
	closes := []func(){}
	closeStack = func() {
		for _, close := range slices.Backward(closes) {
			close()
		}
	}
	defer func() {
		if stack == nil {
			closeStack()
		}
	}()
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "API disabled", 503) }))
	closes = append(closes, trap.Close)
	seed := func(label string) (server.Id, string) {
		n, u, d, c := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, n, label, u)
		model.Testing_CreateDevice(ctx, n, d, c, label, label)
		code, err := model.CreateBalanceCode(ctx, model.ByteCount(8*1024*1024*1024), 24*time.Hour, 0, label+server.NewId().String(), "", "")
		if err != nil {
			tb.Fatal(err)
		}
		redeemed, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: code.Secret, NetworkId: n}, ctx)
		if err != nil || redeemed.Error != nil {
			tb.Fatal("balance redemption", err)
		}
		return c, session.NewByJwt(n, u, label, false, false).Client(d, c).Testing_Sign()
	}
	providerClientId, providerJwt := seed("drain-provider")
	sourceClientId, sourceJwt := seed("drain-probe")
	pa, err := localclient.New(ctx, providerJwt, trap.URL)
	if err != nil {
		tb.Fatal(err)
	}
	closes = append(closes, func() { pa.Close() })
	sa, err := localclient.New(ctx, sourceJwt, trap.URL)
	if err != nil {
		tb.Fatal(err)
	}
	closes = append(closes, func() { sa.Close() })

	host := "localdrain"
	exchange, err := newPrivateTestExchange(ctx, host)
	if err != nil {
		tb.Fatal(err)
	}
	hs := connectserver.DefaultConnectHandlerSettings()
	hs.ListenH3Port = 0
	hs.ListenDnsPort = 0
	hs.ConnectionAnnounceTimeout = 0
	handler := connectserver.NewConnectHandler(ctx, server.NewId(), exchange, hs)
	platform := httptest.NewServer(router.NewRouter(ctx, []*router.Route{router.NewRoute("GET", "/", handler.Connect)}))
	closes = append(closes, func() { handler.Close(); exchange.Close() }, platform.Close)
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategy := connect.NewClientStrategy(ctx, strategySettings)
	closes = append(closes, strategy.Close)
	oob := connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, providerJwt, trap.URL, pa)
	provider := connect.NewClient(ctx, connect.Id(providerClientId), oob, hermeticWebRtcClientSettings(connect.DefaultClientSettings()))
	transport := connect.NewPlatformTransportWithDefaults(provider.Ctx(), strategy, provider.RouteManager(), "ws"+platform.URL[4:], &connect.ClientAuth{ByJwt: providerJwt, InstanceId: connect.NewId(), AppVersion: server.RequireVersion()})
	localNat := connect.NewLocalUserNatWithDefaults(provider.Ctx(), providerClientId.String())
	natSettings := connect.DefaultRemoteUserNatProviderSettings()
	natSettings.SecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
	nat := connect.NewRemoteUserNatProvider(provider, localNat, natSettings)
	closes = append(closes, func() {
		nat.Close()
		join, joinCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer joinCancel()
		_ = transport.CloseAndWait(join)
		_ = localNat.CloseAndWait(join)
		_ = provider.CloseAndWait(join)
		_ = oob.CloseAndWait(join)
	})
	provider.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{protocol.ProvideMode_Public: true, protocol.ProvideMode_Network: true})
	registered := time.Now().Add(15 * time.Second)
	for {
		modes, err := model.GetProvideModes(ctx, providerClientId)
		if err == nil && len(modes) > 0 {
			break
		}
		if time.Now().After(registered) {
			tb.Fatal("provider registration did not commit")
		}
		time.Sleep(20 * time.Millisecond)
	}

	tlsOrigin := func(hostname string, connections *originConnections, serve http.Handler) *httptest.Server {
		leaf, key := issueLeaf(tb, hostname)
		if ip := net.ParseIP(hostname); ip != nil {
			template := *leaf
			template.IPAddresses = []net.IP{ip}
			template.DNSNames = nil
			der, err := x509.CreateCertificate(rand.Reader, &template, testCaCert, &key.PublicKey, testCaKey)
			if err != nil {
				tb.Fatal(err)
			}
			if leaf, err = x509.ParseCertificate(der); err != nil {
				tb.Fatal(err)
			}
		}
		origin := httptest.NewUnstartedServer(serve)
		origin.Config.ConnState = connections.connState
		origin.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
		origin.StartTLS()
		return origin
	}
	dnsConnections := &originConnections{}
	dns := tlsOrigin("127.0.0.1", dnsConnections, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		if err != nil {
			http.Error(w, "dns", 400)
			return
		}
		var message dnsmessage.Message
		if err = message.Unpack(raw); err != nil || len(message.Questions) != 1 {
			http.Error(w, "dns", 400)
			return
		}
		message.Header.Response = true
		message.Header.Authoritative = true
		question := message.Questions[0]
		message.Answers = nil
		if question.Type == dnsmessage.TypeA {
			message.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}}}
		}
		if raw, err = message.Pack(); err != nil {
			http.Error(w, "dns", 500)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(raw)
	}))
	closes = append(closes, dns.Close)

	// The body is far larger than the probe reads: like a sampled URL, the
	// probe stops early while the provider is still returning the rest.
	const payloadHost = "payload.localdrain.example"
	payloadConnections := &originConnections{}
	chunk := make([]byte, 64*1024)
	payload := tlsOrigin(payloadHost, payloadConnections, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		for range 128 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	closes = append(closes, payload.Close)
	_, port, _ := net.SplitHostPort(payload.Listener.Addr().String())

	stack = &teardownReturnStack{
		providerClientId: providerClientId,
		payloadUrl:       "https://" + net.JoinHostPort(payloadHost, port) + "/stream",
		payloadHost:      payloadHost,
		dnsConnections:   dnsConnections,
	}
	authority := &retirementProbeAuthority{Authority: sa, check: func(clientId connect.Id) {
		pending := 0
		for _, companion := range []bool{false, true} {
			for _, forceStream := range []bool{false, true} {
				count, _, _ := provider.ResendQueueSize(clientId, connect.MultiHopId{}, companion, forceStream)
				pending += count
			}
		}
		stack.retired(dnsConnections.open.Load(), payloadConnections.open.Load(), pending)
	}}
	restoreClientSettings := useHermeticTunnelClientSettings()
	closes = append(closes, restoreClientSettings)
	stack.cfg = Config{ApiUrl: trap.URL, PlatformUrl: "ws" + platform.URL[4:], ByJwt: sourceJwt, ClientId: connect.Id(sourceClientId), DataOnlyProbe: true, KeepClientKeysLocal: true,
		ClientCredentials: authority, ClientControl: authority, CloseTimeout: 15 * time.Second,
		DnsResolverSettings: &connect.DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{dns.URL + "/dns-query"}}, ContractReservationByteCount: 1024 * 1024}
	return stack, closeStack
}

// Records one derived client's revocation with what the provider still
// served it at that moment.
func (self *teardownReturnStack) retired(dnsOpen int64, payloadOpen int64, pending int) {
	self.retirementLock.Lock()
	defer self.retirementLock.Unlock()
	self.retirements++
	if dnsOpen != 0 || payloadOpen != 0 || pending != 0 {
		self.retirementErr = errors.Join(self.retirementErr, fmt.Errorf(
			"derived client retired while the provider still served it: open DoH connections=%d, open payload connections=%d, unacknowledged provider returns=%d",
			dnsOpen, payloadOpen, pending))
	}
}

// Opens a tunnel with ctx, starts the streamed payload through it and reads
// its first bytes, which also leaves a pooled DoH connection open.
func (self *teardownReturnStack) openStreaming(tb testing.TB, ctx context.Context) (*Tunnel, *http.Response) {
	tunnel, err := Open(ctx, self.cfg, connect.Id(self.providerClientId))
	if err != nil {
		tb.Fatal(err)
	}
	client := tunnel.HttpClientForHosts(20*time.Second, []string{self.payloadHost})
	response, err := client.Get(self.payloadUrl)
	if err != nil {
		tunnel.Close()
		tb.Fatal("provider DNS/TLS payload", err)
	}
	if _, err := io.ReadFull(response.Body, make([]byte, 16*1024)); err != nil {
		response.Body.Close()
		tunnel.Close()
		tb.Fatal("payload prefix", err)
	}
	if open := self.dnsConnections.open.Load(); open < 1 {
		response.Body.Close()
		tunnel.Close()
		tb.Fatalf("the tunnel holds no pooled DoH connection to retire (open=%d)", open)
	}
	return tunnel, response
}

// Every derived client Close revoked was revoked only after the provider's
// flows to it ended.
func (self *teardownReturnStack) requireRetiredAfterFlowsEnded(tb testing.TB) {
	self.retirementLock.Lock()
	defer self.retirementLock.Unlock()
	if self.retirements == 0 {
		tb.Fatal("Close never retired the derived client")
	}
	if self.retirementErr != nil {
		tb.Fatal(self.retirementErr)
	}
}

// A provider on SDK v2026.8.31 or later retains each TCP return it read from
// an origin until the derived client acknowledges it, and retransmits it to
// that client forever otherwise. Close used to stop the packet pump before
// anything else, so the tunnel's pooled DoH connection was closed only inside
// the stopped netstack and the early-stopped payload's late returns reached a
// canceled client: the provider kept both flows open and the client it would
// return to was revoked.
//
// The probe stops reading a streamed body early, as URL sampling does, and
// closes the tunnel.
func TestTunnelCloseRetiresDerivedClientOnlyAfterProviderFlowsEnd(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		stack, closeStack := newTeardownReturnStack(tb, ctx)
		defer closeStack()

		tunnel, response := stack.openStreaming(tb, ctx)
		defer tunnel.Close()
		response.Body.Close()
		if err := tunnel.Close(); err != nil {
			tb.Fatal("joined retirement", err)
		}
		stack.requireRetiredAfterFlowsEnded(tb)
	})
}

// A pass canceled by a deploy, a shutdown or its deadline cancels the context
// it opened its tunnels with while their providers are still returning. That
// cancellation used to end each tunnel's data path at once: Close then had no
// netstack to end the provider's flows with and no client to acknowledge its
// returns, and revoked the derived client while the provider held both. The
// tunnel's owner still closes it, and that Close does what it does for a live
// caller.
func TestTunnelCloseAfterItsCallerEndedRetiresOnlyAfterProviderFlowsEnd(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		stack, closeStack := newTeardownReturnStack(tb, ctx)
		defer closeStack()

		callerCtx, cancelCaller := context.WithCancel(ctx)
		tunnel, response := stack.openStreaming(tb, callerCtx)
		defer tunnel.Close()
		// the pass ends while the provider is still returning the payload
		cancelCaller()
		response.Body.Close()
		if err := tunnel.Close(); err != nil {
			tb.Fatal("joined retirement", err)
		}
		stack.requireRetiredAfterFlowsEnded(tb)
	})
}
