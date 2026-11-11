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

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// What a test's HTTP/3 leg dialed through the tunnel, and its sockets' fate.
type h3TestPacketPath struct {
	target    *net.UDPAddr
	stateLock sync.Mutex
	dials     []string
	conns     []*h3TestPacketConn
}

// The tunnel's packet dialer, standing in for Tun.DialContext: it records the
// network and address it was asked for and connects a real loopback UDP
// socket to the test's target instead, as the tunnel decides where datagrams
// go.
func (self *h3TestPacketPath) dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := net.DialUDP("udp4", nil, self.target)
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
	if config.HandshakeIdleTimeout != urlProbeTcpTimeout || config.MaxIdleTimeout != urlProbeReadIdleTimeout {
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

// The provider path drops UDP: nothing answers the QUIC Initial. The leg ends
// at the handshake's idle bound with the dial_quic stage, its datagrams each
// fit the tunnel link, and its socket is closed.
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
	if len(dials) != 1 || dials[0] != "udp4 203.0.113.7:443" || queries.Load() != 1 {
		t.Fatalf("dials=%v queries=%d; want one tunnel UDP socket to the resolved address on 443", dials, queries.Load())
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
