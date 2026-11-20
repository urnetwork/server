// A URL request's HTTP/3 leg rides only the tunnel's UDP path, is admitted,
// resolved and verified as its HTTPS leg is, fits the tunnel's link, and fails
// fast and classified when the provider path drops UDP.
package providertunnel

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// What a test's HTTP/3 leg dialed through the tunnel, and its sockets' fate.
// The nth socket goes to targets[n] when the test names one, else to target:
// a path that loses one handshake and carries the next.
type h3TestPacketPath struct {
	target    *net.UDPAddr
	targets   []*net.UDPAddr
	stateLock sync.Mutex
	dials     []string
	conns     []*h3TestPacketConn
}

// The tunnel's packet dialer, standing in for Tun.DialContext: it records the
// network and address it was asked for and connects a real loopback UDP
// socket to the test's target instead, as the tunnel decides where datagrams
// go.
func (self *h3TestPacketPath) dial(ctx context.Context, network, address string) (net.Conn, error) {
	self.stateLock.Lock()
	target := self.target
	if n := len(self.dials); n < len(self.targets) {
		target = self.targets[n]
	}
	self.stateLock.Unlock()
	conn, err := net.DialUDP("udp4", nil, target)
	if err != nil {
		return nil, err
	}
	recorded := &h3TestPacketConn{Conn: conn}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.dials = append(self.dials, network+" "+address)
	self.conns = append(self.conns, recorded)
	return recorded, nil
}

func (self *h3TestPacketPath) snapshot() ([]string, []*h3TestPacketConn) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.dials...), append([]*h3TestPacketConn(nil), self.conns...)
}

type h3TestPacketConn struct {
	net.Conn
	closed atomic.Bool
}

func (self *h3TestPacketConn) Close() error {
	self.closed.Store(true)
	return self.Conn.Close()
}

// The tunnel's resolver, answering every name with one address.
func h3TestResolver(answer string, queries *atomic.Int32) *providerUrlResolver {
	return &providerUrlResolver{query: func(ctx context.Context, qtype string, host string) ([]netip.Addr, bool) {
		if queries != nil {
			queries.Add(1)
		}
		return []netip.Addr{netip.MustParseAddr(answer)}, true
	}}
}

// The HTTPS transport a tunnel builds, with its UDP path, and the HTTP/3
// client its URL requests' HTTP/3 legs use.
func h3TestClient(t *testing.T, path *h3TestPacketPath, resolver *providerUrlResolver, pins map[string][]string, hosts []string) (*providerHttpTransport, *http.Client) {
	t.Helper()
	https := httpClientOverDialerWithResolver(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("the HTTP/3 leg dialed TCP")
	}, resolver, pins, hosts, 10*time.Second)
	transport := https.Transport.(*providerHttpTransport)
	if path != nil {
		transport.dialPacket = path.dial
	}
	roundTripper := transport.ProviderUrlProbeH3RoundTripper()
	if roundTripper == nil {
		t.Fatal("a transport with a UDP path offers no HTTP/3 leg")
	}
	return transport, &http.Client{Transport: roundTripper, Timeout: 10 * time.Second}
}

// A loopback HTTP/3 origin presenting cert for its host.
func startH3TestOrigin(t *testing.T, cert *x509.Certificate, key *ecdsa.PrivateKey, handler http.Handler) *net.UDPAddr {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	transport := &quic.Transport{Conn: udp}
	listener, err := transport.ListenEarly(http3.ConfigureTLSConfig(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{cert.Raw}, PrivateKey: key}},
	}), &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	server := &http3.Server{Handler: handler}
	go server.ServeListener(listener)
	t.Cleanup(func() {
		server.Close()
		listener.Close()
		transport.Close()
		udp.Close()
	})
	return udp.LocalAddr().(*net.UDPAddr)
}

// A UDP port that reads every datagram and answers none: a provider path that
// does not carry UDP. It records the size of every datagram it was sent.
type h3TestBlackhole struct {
	conn      *net.UDPConn
	stateLock sync.Mutex
	sizes     []int
}

func startH3TestBlackhole(t *testing.T) *h3TestBlackhole {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	blackhole := &h3TestBlackhole{conn: conn}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 64*1024)
		for {
			n, _, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			blackhole.stateLock.Lock()
			blackhole.sizes = append(blackhole.sizes, n)
			blackhole.stateLock.Unlock()
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		<-done
	})
	return blackhole
}

func (self *h3TestBlackhole) datagramSizes() []int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]int(nil), self.sizes...)
}

// Only a transport built over the tunnel has a UDP path: one built over a bare
// dialer offers no HTTP/3 leg, so egresshealth records the leg not measured
// rather than reaching UDP some other way.
func TestH3LegNeedsTheTunnelUdpPath(t *testing.T) {
	bare := httpClientOverDialer(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unused")
	}, nil, time.Second)
	if bare.Transport.(*providerHttpTransport).ProviderUrlProbeH3RoundTripper() != nil {
		t.Fatal("a transport without the tunnel's UDP path offered an HTTP/3 leg")
	}
	withoutResolver := httpClientOverDialerWithResolver(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unused")
	}, nil, nil, []string{"h3.example"}, time.Second).Transport.(*providerHttpTransport)
	withoutResolver.dialPacket = (&h3TestPacketPath{}).dial
	if withoutResolver.ProviderUrlProbeH3RoundTripper() != nil {
		t.Fatal("an HTTP/3 leg without the tunnel's private resolver could resolve off the tunnel")
	}
}

// The quic-go settings fit the probe tunnel: one 1280-byte IPv4 link MTU
// datagram at most, no path MTU probing past it, the HTTPS leg's connect bound
// as the handshake's idle bound, its read-idle bound after, one version, and
// nothing that outlives the request.
func TestH3QuicConfigFitsTheProbeTunnel(t *testing.T) {
	config := providerUrlProbeQuicConfig()
	if config.InitialPacketSize != 1252 || config.InitialPacketSize+20+8 > connect.DefaultMtu || !config.DisablePathMTUDiscovery {
		t.Fatalf("QUIC datagrams do not fit the tunnel link: initial=%d pmtud_off=%t", config.InitialPacketSize, config.DisablePathMTUDiscovery)
	}
	if config.HandshakeIdleTimeout != defaultProviderUrlPhaseSettings().TcpTimeout || config.MaxIdleTimeout != urlProbeReadIdleTimeout {
		t.Fatalf("QUIC bounds are not the HTTPS leg's: handshake=%s idle=%s", config.HandshakeIdleTimeout, config.MaxIdleTimeout)
	}
	if len(config.Versions) != 1 || config.Versions[0] != quic.Version1 || config.Allow0RTT || config.TokenStore != nil || config.KeepAlivePeriod != 0 || config.EnableDatagrams {
		t.Fatalf("QUIC configuration keeps state past one request: %+v", config)
	}
	if providerUrlProbeQuicConfig() == config {
		t.Fatal("legs share one mutable QUIC configuration")
	}
}

// A host outside the tunnel's closed allowlist is refused before any socket is
// opened or any name resolved, exactly as its HTTPS leg would be.
func TestH3LegRefusesAHostOutsideTheAllowlistBeforeAnySocket(t *testing.T) {
	path := &h3TestPacketPath{target: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}}
	var queries atomic.Int32
	_, client := h3TestClient(t, path, h3TestResolver("203.0.113.7", &queries), nil, []string{"allowed.example"})
	response, err := client.Get("https://other.example/")
	if response != nil {
		response.Body.Close()
	}
	var staged interface{ ProviderHttpStage() string }
	if err == nil || !errors.As(err, &staged) || staged.ProviderHttpStage() != "policy" || !errors.Is(err, ErrPinHostUnknown) {
		t.Fatalf("err = %v; want a policy refusal of an unknown host", err)
	}
	if dials, _ := path.snapshot(); len(dials) != 0 || queries.Load() != 0 {
		t.Fatalf("an unknown host reached the tunnel: dials=%v queries=%d", dials, queries.Load())
	}
}

// A scoped URL request's HTTP/3 leg resolves through the tunnel's own
// resolver and refuses a nonpublic answer before any socket, as the HTTPS leg
// does; and its grant confines it to 443, as the HTTPS leg's.
func TestH3LegHonorsTheUrlProbeScope(t *testing.T) {
	path := &h3TestPacketPath{target: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}}
	_, client := h3TestClient(t, path, h3TestResolver("192.168.1.10", nil), nil, []string{"h3.example"})
	grant := client.Transport.(interface {
		ProviderUrlProbeContext(context.Context, *url.URL, bool) (context.Context, error)
	})
	target, _ := url.Parse("https://h3.example/")
	ctx, err := grant.ProviderUrlProbeContext(context.Background(), target, false)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
	}
	var staged interface{ ProviderHttpStage() string }
	if err == nil || !errors.As(err, &staged) || staged.ProviderHttpStage() != "policy" {
		t.Fatalf("err = %v; want a policy refusal of a nonpublic answer", err)
	}
	otherPort, _ := url.Parse("https://h3.example:8443/")
	if _, err := grant.ProviderUrlProbeContext(context.Background(), otherPort, false); err == nil {
		t.Fatal("a URL probe grant admitted a port other than 443")
	}
	if dials, _ := path.snapshot(); len(dials) != 0 {
		t.Fatalf("a refused leg opened sockets: %v", dials)
	}
}

// The provider path drops UDP: nothing answers the QUIC Initial, on the first
// socket or on the one the handshake is retried on. The leg ends at the second
// handshake's idle bound with the dial_quic stage, its datagrams each fit the
// tunnel link, and both sockets are closed.
func TestH3LegFailsFastWhenTheProviderDropsUdp(t *testing.T) {
	saved := urlProbeQuicHandshakeIdleTimeout
	urlProbeQuicHandshakeIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { urlProbeQuicHandshakeIdleTimeout = saved })

	blackhole := startH3TestBlackhole(t)
	path := &h3TestPacketPath{target: blackhole.conn.LocalAddr().(*net.UDPAddr)}
	var queries atomic.Int32
	_, client := h3TestClient(t, path, h3TestResolver("203.0.113.7", &queries), nil, []string{"h3.example"})
	// the request's own deadline is far longer than the handshake bound
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://h3.example/", nil)
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
		t.Fatal("a provider that drops UDP answered HTTP/3")
	}
	var staged interface{ ProviderHttpStage() string }
	var handshakeTimeout *quic.HandshakeTimeoutError
	var idleTimeout *quic.IdleTimeoutError
	if !errors.As(err, &staged) || staged.ProviderHttpStage() != egresshealth.FailureStageDialQuic ||
		(!errors.As(err, &handshakeTimeout) && !errors.As(err, &idleTimeout)) || ctx.Err() != nil {
		t.Fatalf("err = %v; want the QUIC handshake's own idle bound, classified dial_quic", err)
	}
	dials, conns := path.snapshot()
	if len(dials) != 2 || dials[0] != "udp4 203.0.113.7:443" || dials[1] != dials[0] || queries.Load() != 1 {
		t.Fatalf("dials=%v queries=%d; want two tunnel UDP sockets, the handshake and its one retry, to the resolved address on 443", dials, queries.Load())
	}
	for _, conn := range conns {
		if !conn.closed.Load() {
			t.Fatal("a failed leg left its tunnel UDP socket open")
		}
	}
	sizes := blackhole.datagramSizes()
	if len(sizes) == 0 {
		t.Fatal("no QUIC datagram reached the provider path")
	}
	for _, size := range sizes {
		if size+20+8 > connect.DefaultMtu || size < 1200 {
			t.Fatalf("datagram of %d bytes does not fit the tunnel's 1280-byte IPv4 link or QUIC's 1200-byte floor", size)
		}
	}
}

// A certificate that does not authenticate the host over QUIC is the tls
// stage, and unwraps to the verification failure egresshealth classifies, on
// every platform: the issuing CA is trusted nowhere.
func TestH3LegUntrustedCertificateIsTls(t *testing.T) {
	untrustedCa, untrustedKey, err := generateTestCa()
	if err != nil {
		t.Fatal(err)
	}
	leaf, key := issueLeafSignedBy(t, "h3.example", untrustedCa, untrustedKey)
	origin := startH3TestOrigin(t, leaf, key, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an unauthenticated origin received the request")
	}))
	path := &h3TestPacketPath{target: origin}
	_, client := h3TestClient(t, path, h3TestResolver("203.0.113.7", nil), nil, []string{"h3.example"})
	response, err := client.Get("https://h3.example/")
	if response != nil {
		response.Body.Close()
		t.Fatal("an untrusted certificate was accepted over QUIC")
	}
	var staged interface{ ProviderHttpStage() string }
	if !errors.As(err, &staged) || staged.ProviderHttpStage() != "tls" || !egresshealth.IsTlsAuthenticationFailure(err) {
		t.Fatalf("err = %v; want the tls stage over a certificate verification failure", err)
	}
	if _, conns := path.snapshot(); len(conns) != 1 || !conns[0].closed.Load() {
		t.Fatal("a refused handshake left its tunnel UDP socket open")
	}
}

// The request goes out over HTTP/3 on the tunnel's UDP path to the address
// the tunnel resolved, the response is read, and closing its body releases
// the QUIC connection's socket.
func TestH3LegCarriesTheRequestOverTheTunnelUdpPath(t *testing.T) {
	requireInjectedSystemTrust(t)
	leaf, key := issueLeaf(t, "h3.example")
	var protocol atomic.Value
	origin := startH3TestOrigin(t, leaf, key, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol.Store(r.Proto)
		_, _ = w.Write([]byte("synthetic real content"))
	}))
	path := &h3TestPacketPath{target: origin}
	_, client := h3TestClient(t, path, h3TestResolver("203.0.113.7", nil), nil, []string{"h3.example"})
	response, err := client.Get("https://h3.example/")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "synthetic real content" || response.ProtoMajor != 3 || protocol.Load() != "HTTP/3.0" {
		t.Fatalf("response %s %q err=%v origin saw %v", response.Proto, body, err, protocol.Load())
	}
	dials, conns := path.snapshot()
	if len(dials) != 1 || dials[0] != "udp4 203.0.113.7:443" || conns[0].closed.Load() {
		t.Fatalf("dials=%v; the socket must stay open while the body is read", dials)
	}
	response.Body.Close()
	if !conns[0].closed.Load() {
		t.Fatal("closing the body left the leg's tunnel UDP socket open")
	}
}

// A pinned host keeps its pin over QUIC: a chain-valid certificate whose key
// the pin set does not trust is refused, as on the HTTPS leg.
func TestH3LegKeepsCertificatePins(t *testing.T) {
	requireInjectedSystemTrust(t)
	served, key := issueLeaf(t, "pinned.example")
	trusted, _ := issueLeaf(t, "pinned.example")
	origin := startH3TestOrigin(t, served, key, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a pin mismatch reached the origin")
	}))
	_, client := h3TestClient(t, &h3TestPacketPath{target: origin}, h3TestResolver("203.0.113.7", nil), map[string][]string{"pinned.example": {SpkiPin(trusted)}}, nil)
	response, err := client.Get("https://pinned.example/")
	if response != nil {
		response.Body.Close()
	}
	if err == nil || !errors.Is(err, ErrPinMismatch) || !egresshealth.IsTlsAuthenticationFailure(err) {
		t.Fatalf("err = %v; want the pin mismatch", err)
	}
}

// A site whose QUIC edge answers and refuses the hostname -- here with the
// no_application_protocol alert (0x178) a listener without h3 sends -- proves
// the provider carried UDP to it: the leg is the tls stage, not the silence
// that counts against a provider, and the datagrams that came back are seen.
func TestH3LegRefusalBySiteIsNotSilence(t *testing.T) {
	leaf, key := issueLeaf(t, "h3.example")
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	transport := &quic.Transport{Conn: udp}
	listener, err := transport.Listen(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}},
		NextProtos:   []string{"not-h3"},
	}, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
		transport.Close()
		udp.Close()
	})
	go func() {
		for {
			if _, err := listener.Accept(context.Background()); err != nil {
				return
			}
		}
	}()
	path := &h3TestPacketPath{target: udp.LocalAddr().(*net.UDPAddr)}
	_, client := h3TestClient(t, path, h3TestResolver("203.0.113.7", nil), nil, []string{"h3.example"})
	response, err := client.Get("https://h3.example/")
	if response != nil {
		response.Body.Close()
		t.Fatal("a QUIC edge without h3 answered HTTP/3")
	}
	var staged interface{ ProviderHttpStage() string }
	var transportErr *quic.TransportError
	if !errors.As(err, &staged) || staged.ProviderHttpStage() != "tls" || !errors.As(err, &transportErr) || !transportErr.Remote ||
		transportErr.ErrorCode != quic.TransportErrorCode(0x178) {
		t.Fatalf("err = %v; want the site's no_application_protocol alert as the tls stage", err)
	}
	if _, conns := path.snapshot(); len(conns) != 1 || !conns[0].closed.Load() {
		t.Fatal("a refused leg left its tunnel UDP socket open")
	}
}

// Silence is only silence: once any datagram came back, a handshake that then
// times out is the quic stage, because the site answered; with nothing back it
// is dial_quic, the one stage that counts against the provider.
func TestH3DialStageTellsSilenceFromAnAnswer(t *testing.T) {
	timeout := &quic.HandshakeTimeoutError{}
	idle := &quic.IdleTimeoutError{}
	alert := &quic.TransportError{ErrorCode: quic.TransportErrorCode(0x150), Remote: true}
	for _, c := range []struct {
		err      error
		received int64
		want     string
	}{
		{timeout, 0, "dial_quic"},
		{idle, 0, "dial_quic"},
		{context.DeadlineExceeded, 0, "dial_quic"},
		{timeout, 1, "quic"},
		{idle, 3, "quic"},
		{alert, 1, "tls"},
		{alert, 0, "tls"},
		{&quic.StatelessResetError{}, 1, "quic"},
		{context.Canceled, 0, ""},
	} {
		if got := providerQuicDialStage(c.err, c.received); got != c.want {
			t.Errorf("stage of %v after %d datagrams = %q, want %q", c.err, c.received, got, c.want)
		}
	}
}

// A request deadline that ends the leg while its handshake still hears
// nothing is the same silence the handshake's own bound reports: dial_quic,
// not an answered leg's timeout.
func TestH3LegDeadlineDuringASilentHandshakeIsSilence(t *testing.T) {
	saved := urlProbeQuicHandshakeIdleTimeout
	urlProbeQuicHandshakeIdleTimeout = time.Minute
	t.Cleanup(func() { urlProbeQuicHandshakeIdleTimeout = saved })

	blackhole := startH3TestBlackhole(t)
	path := &h3TestPacketPath{target: blackhole.conn.LocalAddr().(*net.UDPAddr)}
	_, client := h3TestClient(t, path, h3TestResolver("203.0.113.7", nil), nil, []string{"h3.example"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://h3.example/", nil)
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
		t.Fatal("a provider that drops UDP answered HTTP/3")
	}
	var staged interface{ ProviderHttpStage() string }
	if !errors.As(err, &staged) || staged.ProviderHttpStage() != egresshealth.FailureStageDialQuic || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want the request deadline over a silent handshake, classified dial_quic", err)
	}
}

// A QUIC listener that answers every handshake and refuses it (ALPN not-h3,
// the no_application_protocol alert): a site that heard the provider.
func startH3TestRefusingListener(t *testing.T) *net.UDPAddr {
	t.Helper()
	leaf, key := issueLeaf(t, "h3.example")
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	transport := &quic.Transport{Conn: udp}
	listener, err := transport.Listen(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}},
		NextProtos:   []string{"not-h3"},
	}, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
		transport.Close()
		udp.Close()
	})
	go func() {
		for {
			if _, err := listener.Accept(context.Background()); err != nil {
				return
			}
		}
	}()
	return udp.LocalAddr().(*net.UDPAddr)
}

// One HTTP/3 request of the given family over path, as a URL leg grants it.
func h3TestFamilyRequest(t *testing.T, path *h3TestPacketPath, ipVersion int, timeout time.Duration) error {
	t.Helper()
	resolver := &providerUrlResolver{query: func(_ context.Context, recordType string, _ string) ([]netip.Addr, bool) {
		if recordType == "AAAA" {
			return []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111")}, true
		}
		return []netip.Addr{netip.MustParseAddr("8.8.4.4")}, true
	}}
	_, client := h3TestClient(t, path, resolver, nil, []string{"h3.example"})
	target, _ := url.Parse("https://h3.example/")
	ctx, err := client.Transport.(*providerH3RoundTripper).ProviderUrlProbeFamilyContext(context.Background(), target, false, ipVersion)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
		t.Fatal("the test origin answered HTTP/3")
	}
	return err
}

// The survey's finding (factury reports/Quality probe IPv6 support.md):
// Cloudflare's QUIC edge left about one IPv6 handshake in 50 unanswered,
// though quic-go resent the Initial, while the next handshake got through. A
// first handshake that hears nothing within its bound is therefore made once
// more on a fresh socket, so from a new source port, in either family, before
// the leg reports silence: here the first socket's path is a blackhole and
// the second's reaches the site, so the leg is the site's answer (the tls
// stage of its refusal), not the silence that counts against a provider.
func TestH3LegRetriesAnUnansweredHandshakeOnceOnAFreshSocket(t *testing.T) {
	saved := urlProbeQuicHandshakeIdleTimeout
	urlProbeQuicHandshakeIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { urlProbeQuicHandshakeIdleTimeout = saved })

	for _, c := range []struct {
		ipVersion int
		family    string
		dial      string
	}{
		{4, "ipv4", "udp4 8.8.4.4:443"},
		{6, "ipv6", "udp6 [2606:4700:4700::1111]:443"},
	} {
		blackhole := startH3TestBlackhole(t)
		site := startH3TestRefusingListener(t)
		path := &h3TestPacketPath{targets: []*net.UDPAddr{blackhole.conn.LocalAddr().(*net.UDPAddr), site}, target: site}
		answered := testutil.ToFloat64(providerQuicHandshakeRetries.WithLabelValues(c.family, providerQuicRetryAnswered))
		err := h3TestFamilyRequest(t, path, c.ipVersion, 30*time.Second)
		var staged interface{ ProviderHttpStage() string }
		if !errors.As(err, &staged) || staged.ProviderHttpStage() != "tls" {
			t.Fatalf("%s: err = %v; want the retried handshake's answer (the site's tls refusal), not silence", c.family, err)
		}
		dials, conns := path.snapshot()
		if len(dials) != 2 || dials[0] != c.dial || dials[1] != c.dial || conns[0] == conns[1] {
			t.Fatalf("%s: dials=%v; want the handshake and one retry, each on a socket of its own, to the same address", c.family, dials)
		}
		for _, conn := range conns {
			if !conn.closed.Load() {
				t.Fatalf("%s: a socket of the leg was left open", c.family)
			}
		}
		if len(blackhole.datagramSizes()) == 0 {
			t.Fatalf("%s: the first handshake never reached the path that lost it", c.family)
		}
		if got := testutil.ToFloat64(providerQuicHandshakeRetries.WithLabelValues(c.family, providerQuicRetryAnswered)) - answered; got != 1 {
			t.Fatalf("%s: answered retries counted %v, want 1", c.family, got)
		}
	}
}

// The retry is made once, and only for silence within the handshake's own
// bound: a path that drops UDP is silent twice and the leg reports dial_quic
// after two sockets, never a third; a handshake the site answered is never
// retried; and a request deadline that ends the first handshake leaves no
// time for another.
func TestH3LegRetriesASilentHandshakeOnceAndOnlyForSilence(t *testing.T) {
	saved := urlProbeQuicHandshakeIdleTimeout
	urlProbeQuicHandshakeIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { urlProbeQuicHandshakeIdleTimeout = saved })
	var staged interface{ ProviderHttpStage() string }

	blackhole := startH3TestBlackhole(t)
	dark := &h3TestPacketPath{target: blackhole.conn.LocalAddr().(*net.UDPAddr)}
	silent := testutil.ToFloat64(providerQuicHandshakeRetries.WithLabelValues("ipv6", providerQuicRetrySilent))
	err := h3TestFamilyRequest(t, dark, 6, 30*time.Second)
	if !errors.As(err, &staged) || staged.ProviderHttpStage() != egresshealth.FailureStageDialQuic {
		t.Fatalf("a dark path: err = %v; want dial_quic", err)
	}
	if dials, _ := dark.snapshot(); len(dials) != 2 {
		t.Fatalf("a dark path: dials=%v; want the handshake and exactly one retry", dials)
	}
	if got := testutil.ToFloat64(providerQuicHandshakeRetries.WithLabelValues("ipv6", providerQuicRetrySilent)) - silent; got != 1 {
		t.Fatalf("silent retries counted %v, want 1", got)
	}

	site := startH3TestRefusingListener(t)
	answering := &h3TestPacketPath{target: site}
	if err := h3TestFamilyRequest(t, answering, 4, 30*time.Second); !errors.As(err, &staged) || staged.ProviderHttpStage() != "tls" {
		t.Fatalf("an answering site: err = %v; want its tls refusal", err)
	}
	if dials, _ := answering.snapshot(); len(dials) != 1 {
		t.Fatalf("an answered handshake was retried: dials=%v", dials)
	}

	urlProbeQuicHandshakeIdleTimeout = time.Minute
	cut := &h3TestPacketPath{target: blackhole.conn.LocalAddr().(*net.UDPAddr)}
	if err := h3TestFamilyRequest(t, cut, 4, 300*time.Millisecond); !errors.As(err, &staged) || staged.ProviderHttpStage() != egresshealth.FailureStageDialQuic || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a request deadline over a silent handshake: err = %v; want dial_quic at the deadline", err)
	}
	if dials, _ := cut.snapshot(); len(dials) != 1 {
		t.Fatalf("a handshake the request's deadline ended was retried: dials=%v", dials)
	}
}
