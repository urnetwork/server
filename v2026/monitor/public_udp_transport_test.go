package monitor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	connectcore "github.com/urnetwork/connect/v2026"
)

type publicUdpTestPacket struct {
	data []byte
	from net.Addr
}

// Supplies one ordinary packet for each read, without allocating in the socket.
type publicUdpRepeatedPacket struct {
	net.PacketConn
	address *net.UDPAddr
}

// Returns a single payload byte from the pinned peer.
func (self *publicUdpRepeatedPacket) ReadFrom(buffer []byte) (int, net.Addr, error) {
	buffer[0] = 42
	return 1, self.address, nil
}

// A warmed-up read must reuse the full-size datagram buffer. The small
// protocol buffer still receives the payload and the wire counts stay exact.
func TestPublicUdpReadsReuseTheWireBuffer(t *testing.T) {
	peer := netip.MustParseAddrPort("192.0.2.1:443")
	wire := &publicUdpPacketConn{
		PacketConn: &publicUdpRepeatedPacket{address: net.UDPAddrFromAddrPort(peer)},
		peer:       peer, settings: DefaultPublicUdpProbeSettings(),
	}
	var buffer [1]byte
	allocations := testing.AllocsPerRun(32, func() {
		n, _, err := wire.ReadFrom(buffer[:])
		if err != nil || n != 1 || buffer[0] != 42 {
			t.Fatalf("read %d bytes, first byte %d, error %v", n, buffer[0], err)
		}
	})
	if allocations != 0 {
		t.Errorf("warmed-up UDP read allocated %.0f objects, want zero", allocations)
	}
	if got := wire.observation(); got.ReceivedBytes != 33 || got.ReceivedPackets != 33 {
		t.Errorf("received %d bytes in %d packets including warmup", got.ReceivedBytes, got.ReceivedPackets)
	}
}

// Exposes each borrowed destination while two socket reads are in progress.
type publicUdpHeldReads struct {
	net.PacketConn
	address *net.UDPAddr
	buffers chan []byte
	release chan struct{}
}

func (self *publicUdpHeldReads) ReadFrom(buffer []byte) (int, net.Addr, error) {
	self.buffers <- buffer
	<-self.release
	return 1, self.address, nil
}

func TestPublicUdpConcurrentReadsOwnTheirBuffers(t *testing.T) {
	peer := netip.MustParseAddrPort("192.0.2.1:443")
	socket := &publicUdpHeldReads{address: net.UDPAddrFromAddrPort(peer), buffers: make(chan []byte), release: make(chan struct{})}
	wire := &publicUdpPacketConn{PacketConn: socket, peer: peer, settings: DefaultPublicUdpProbeSettings()}
	results := make(chan byte, 2)
	for range 2 {
		go func() {
			var buffer [1]byte
			if n, _, err := wire.ReadFrom(buffer[:]); n != 1 || err != nil {
				t.Errorf("read %d bytes, error %v", n, err)
			}
			results <- buffer[0]
		}()
	}
	first := <-socket.buffers
	second := <-socket.buffers
	first[0], second[0] = 1, 2
	close(socket.release)
	a, b := <-results, <-results
	if !((a == 1 && b == 2) || (a == 2 && b == 1)) {
		t.Fatalf("concurrent readers returned %d and %d", a, b)
	}
}

// A bounded in-memory datagram network keeps native QUIC/TLS/codec controls
// off real sockets, hostnames and interfaces. Deadlines are only safety bounds;
// ordering controls use the first-write barrier or explicit packet delivery.
type publicUdpTestSocket struct {
	address      *net.UDPAddr
	peer         *publicUdpTestSocket
	incoming     chan publicUdpTestPacket
	closed       chan struct{}
	closeOnce    sync.Once
	wrote        chan struct{}
	writeOnce    sync.Once
	deadlineLock sync.Mutex
	readDeadline time.Time
	changed      chan struct{}
}

func publicUdpTestSocketPair(client string, server string) (*publicUdpTestSocket, *publicUdpTestSocket) {
	newSocket := func(address string) *publicUdpTestSocket {
		return &publicUdpTestSocket{
			address:  net.UDPAddrFromAddrPort(netip.MustParseAddrPort(address)),
			incoming: make(chan publicUdpTestPacket, DefaultPublicUdpProbeSettings().MaxPacketCount+1),
			closed:   make(chan struct{}), wrote: make(chan struct{}), changed: make(chan struct{}),
		}
	}
	a, b := newSocket(client), newSocket(server)
	a.peer, b.peer = b, a
	return a, b
}

func (self *publicUdpTestSocket) LocalAddr() net.Addr { return self.address }

func (self *publicUdpTestSocket) ReadFrom(buffer []byte) (int, net.Addr, error) {
	for {
		deadline, changed := func() (time.Time, <-chan struct{}) {
			self.deadlineLock.Lock()
			defer self.deadlineLock.Unlock()
			return self.readDeadline, self.changed
		}()
		var expired <-chan time.Time
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return 0, nil, os.ErrDeadlineExceeded
			}
			expired = time.After(remaining)
		}
		select {
		case packet := <-self.incoming:
			return copy(buffer, packet.data), packet.from, nil
		case <-self.closed:
			return 0, nil, net.ErrClosed
		case <-changed:
		case <-expired:
			return 0, nil, os.ErrDeadlineExceeded
		}
	}
}

func (self *publicUdpTestSocket) WriteTo(buffer []byte, address net.Addr) (int, error) {
	if address.String() != self.peer.address.String() {
		return 0, errors.New("synthetic destination mismatch")
	}
	select {
	case <-self.closed:
		return 0, net.ErrClosed
	case <-self.peer.closed:
		return 0, net.ErrClosed
	default:
	}
	packet := publicUdpTestPacket{data: append([]byte(nil), buffer...), from: self.address}
	select {
	case self.peer.incoming <- packet:
		self.writeOnce.Do(func() { close(self.wrote) })
		return len(buffer), nil
	default:
		return 0, errors.New("synthetic datagram queue exhausted")
	}
}

func (self *publicUdpTestSocket) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

func (self *publicUdpTestSocket) SetReadDeadline(deadline time.Time) error {
	self.deadlineLock.Lock()
	defer self.deadlineLock.Unlock()
	self.readDeadline = deadline
	close(self.changed)
	self.changed = make(chan struct{})
	return nil
}

func (self *publicUdpTestSocket) SetWriteDeadline(time.Time) error { return nil }
func (self *publicUdpTestSocket) SetDeadline(deadline time.Time) error {
	return self.SetReadDeadline(deadline)
}

func publicUdpTestCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"connect.synthetic.example", "api.synthetic.example"},
		NotBefore: time.Unix(0, 0), NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return tls.Certificate{Certificate: [][]byte{encoded}, PrivateKey: private}, roots
}

func publicUdpTestNativeRequest(family string, front string, carrier string) PublicUdpRequest {
	request := PublicUdpRequest{
		AttemptId: "synthetic-native-attempt", Host: "edge-synthetic", Target: "synthetic-native", Family: family,
		Service: "alt", Front: front, Carrier: carrier, Address: "192.0.2.20", Port: 443, ServerName: "connect.synthetic.example",
	}
	if family == "ipv6" {
		request.Address = "2001:db8::20"
	}
	if front == "api" {
		request.ServerName = "api.synthetic.example"
	}
	if carrier != "quic" {
		request.Port, request.DnsTld = 53, "codec.synthetic.example."
	}
	return request
}

func publicUdpTestNativeHandshake(t *testing.T, request PublicUdpRequest, trusted bool) (PublicUdpObservation, error) {
	t.Helper()
	return publicUdpTestNativeAttempt(t, request, publicUdpTestNativeOptions{trusted: trusted})
}

// What a native attempt runs against, beyond its request.
type publicUdpTestNativeOptions struct {
	trusted bool
	// serves each connection the server accepts as HTTP/3; nil only
	// completes handshakes
	api http.Handler
	// nil takes the defaults
	probeSettings *PublicUdpProbeSettings
	// the attempt's own context; nil runs it under the fixture's bound
	attemptCtx context.Context
}

// One native attempt against an in-memory QUIC server. With an api handler
// the server serves each connection it accepts as HTTP/3 and then closes it,
// as a front's api listener does (alt handleApiQuicConn). The server is torn
// down and its connection joined before this returns.
func publicUdpTestNativeAttempt(t *testing.T, request PublicUdpRequest, options publicUdpTestNativeOptions) (PublicUdpObservation, error) {
	t.Helper()
	trusted, api := options.trusted, options.api
	probeSettings := options.probeSettings
	if probeSettings == nil {
		probeSettings = DefaultPublicUdpProbeSettings()
	}
	clientAddress := "198.51.100.20:42000"
	if request.Family == "ipv6" {
		clientAddress = "[2001:db8:1::20]:42000"
	}
	client, server := publicUdpTestSocketPair(clientAddress, publicUdpTuple(request))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var serverPackets net.PacketConn = server
	var serverTransport *quic.Transport
	var listener *quic.Listener
	// the attempt must close the client socket itself, which the checks
	// below read before the deferred close
	var closeOnce sync.Once
	closeServer := func() {
		closeOnce.Do(func() {
			cancel()
			if listener != nil {
				listener.Close()
			}
			serverPackets.Close()
			if serverTransport != nil {
				serverTransport.Close()
			}
		})
	}
	defer func() {
		closeServer()
		client.Close()
	}()
	if request.Carrier != "quic" {
		settings := connectcore.DefaultPacketTranslationSettings()
		settings.Log = connectcore.NewNoopLogger()
		settings.DnsTlds = [][]byte{[]byte(request.DnsTld)}
		translated, err := connectcore.NewPacketTranslation(ctx, connectcore.PacketTranslationModeDecode53, server, settings)
		if err != nil {
			t.Fatal(err)
		}
		serverPackets = translated
	}
	certificate, roots := publicUdpTestCertificate(t)
	if !trusted {
		roots = x509.NewCertPool()
	}
	type helloSummary struct {
		name      string
		protocols []string
	}
	hellos := make(chan helloSummary, 1)
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case hellos <- helloSummary{name: hello.ServerName, protocols: append([]string(nil), hello.SupportedProtos...)}:
			default:
			}
			return nil, nil
		},
	}
	var expectedProtocols []string
	if request.Front == "api" {
		expectedProtocols, tlsConfig.NextProtos = []string{"h3"}, []string{"h3"}
	}
	serverTransport = &quic.Transport{Conn: serverPackets}
	var err error
	listener, err = serverTransport.Listen(tlsConfig, &quic.Config{InitialPacketSize: connectcore.H3InitialPacketByteCount, EnableDatagrams: true})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	if api == nil {
		close(served)
	} else {
		go func() {
			defer close(served)
			connection, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			defer connection.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeNoError), "")
			(&http3.Server{Handler: api}).ServeQUICConn(connection)
		}()
	}
	// Unsolicited bytes from another tuple must not spoil a later exact,
	// authenticated handshake or become return-path root-cause attribution.
	client.incoming <- publicUdpTestPacket{data: []byte("synthetic unsolicited datagram"), from: net.UDPAddrFromAddrPort(netip.MustParseAddrPort("203.0.113.99:8053"))}
	attemptCtx := options.attemptCtx
	if attemptCtx == nil {
		attemptCtx = ctx
	}
	listenCount := 0
	observed, observeErr := observePublicUdp(attemptCtx, request, probeSettings, func(_ context.Context, network string, _ string) (net.PacketConn, error) {
		listenCount++
		if network != "udp4" && request.Family == "ipv4" || network != "udp6" && request.Family == "ipv6" {
			t.Error("native socket family differs from explicit request")
		}
		return client, nil
	}, roots)
	// the attempt has closed its socket; closing the server ends a connection
	// its handler still holds, so the join below is bounded by teardown
	closeServer()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the synthetic api server did not release its connection")
	}
	if listenCount != 1 {
		t.Fatal("native attempt did not own exactly one fresh socket")
	}
	select {
	case hello := <-hellos:
		if hello.name != request.ServerName || !reflect.DeepEqual(hello.protocols, expectedProtocols) {
			t.Fatal("native QUIC used the wrong SNI or confused custom H3 with HTTP/3")
		}
	default:
		t.Fatal("native protocol did not reach the server TLS handshake")
	}
	select {
	case <-client.closed:
	default:
		t.Fatal("native attempt returned before closing its owned socket")
	}
	return observed, observeErr
}

func TestPublicUdpNativeHandshakeBothFamiliesAllCodecsAndFronts(t *testing.T) {
	for _, family := range []string{"ipv4", "ipv6"} {
		for _, carrier := range []string{"quic", "dns", "dns-pump"} {
			for _, path := range []struct{ service, front string }{{service: "connect", front: "connect"}, {service: "alt", front: "connect"}, {service: "alt", front: "api"}} {
				request := publicUdpTestNativeRequest(family, path.front, carrier)
				request.Service = path.service
				observed, err := publicUdpTestNativeHandshake(t, request, true)
				if err != nil || observed.Failure != PublicUdpFailureNone || !observed.HandshakeComplete || !observed.TlsVerified || observed.UnexpectedPackets != 1 {
					t.Fatalf("native synthetic handshake failed for %s/%s/%s/%s: class=%s", family, path.service, path.front, carrier, observed.Failure)
				}
				if observed.ApplicationRequested || observed.ApplicationStatus != 0 {
					t.Fatalf("a handshake-only front made an api request for %s/%s/%s/%s", family, path.service, path.front, carrier)
				}
				for _, finding := range publicUdpFindings(request, observed, err, DefaultPublicUdpProbeSettings()) {
					if !finding.healthy {
						t.Fatal("authenticated exact native result did not establish only its transport path")
					}
				}
			}
		}
	}
}

func TestPublicUdpNativeRejectsUntrustedAndWrongNameCertificates(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		request := publicUdpTestNativeRequest("ipv4", "connect", "quic")
		if trusted {
			request.ServerName = "wrong.synthetic.example"
		}
		observed, err := publicUdpTestNativeHandshake(t, request, trusted)
		if err == nil || observed.TlsVerified || observed.Failure != PublicUdpFailureTls {
			t.Fatal("native transport did not enforce trust roots and exact SNI certificate verification")
		}
		findings := publicUdpFindings(request, observed, err, DefaultPublicUdpProbeSettings())
		if len(findings) != 1 || findings[0].healthy || findings[0].class != "public-udp-observation" {
			t.Fatal("unauthenticated tuple traffic established transport health")
		}
	}
}

func TestPublicUdpRawTupleFilterDropsWrongSourceBeforeProtocol(t *testing.T) {
	client, server := publicUdpTestSocketPair("198.51.100.20:42000", "192.0.2.20:443")
	defer client.Close()
	defer server.Close()
	wire := &publicUdpPacketConn{PacketConn: client, peer: netip.MustParseAddrPort("192.0.2.20:443"), settings: DefaultPublicUdpProbeSettings()}
	client.incoming <- publicUdpTestPacket{data: []byte("wrong port"), from: net.UDPAddrFromAddrPort(netip.MustParseAddrPort("192.0.2.20:8053"))}
	client.incoming <- publicUdpTestPacket{data: []byte("wrong address"), from: net.UDPAddrFromAddrPort(netip.MustParseAddrPort("192.0.2.21:443"))}
	client.incoming <- publicUdpTestPacket{data: []byte("exact"), from: server.address}
	buffer := make([]byte, 64)
	n, _, err := wire.ReadFrom(buffer)
	observed := wire.observation()
	if err != nil || string(buffer[:n]) != "exact" || observed.UnexpectedPackets != 2 || observed.ReceivedPackets != 1 || observed.PeerTuple != "192.0.2.20:443" {
		t.Fatal("wrong wire tuple escaped into the protocol or became exact-return evidence")
	}
	if _, err := wire.WriteTo([]byte("private"), net.UDPAddrFromAddrPort(netip.MustParseAddrPort("192.0.2.21:443"))); err == nil {
		t.Fatal("adapter wrote outside the requested pin")
	}
}

func TestPublicUdpRawBudgetsCloseOwnedSocket(t *testing.T) {
	for _, inbound := range []bool{false, true} {
		client, server := publicUdpTestSocketPair("198.51.100.20:42000", "192.0.2.20:443")
		settings := DefaultPublicUdpProbeSettings()
		wire := &publicUdpPacketConn{PacketConn: client, peer: netip.MustParseAddrPort("192.0.2.20:443"), settings: settings}
		var err error
		if inbound {
			for range settings.MaxPacketCount + 1 {
				client.incoming <- publicUdpTestPacket{data: []byte{1}, from: net.UDPAddrFromAddrPort(netip.MustParseAddrPort("192.0.2.21:443"))}
			}
			_, _, err = wire.ReadFrom(make([]byte, 64))
		} else {
			_, err = wire.WriteTo(make([]byte, settings.MaxByteCount+1), server.address)
		}
		if !errors.Is(err, errPublicUdpBudget) || wire.observation().Failure != PublicUdpFailureBudget {
			t.Fatal("wire budget was not enforced before unbounded traffic")
		}
		select {
		case <-client.closed:
		default:
			t.Fatal("wire budget did not close the owned raw socket")
		}
		server.Close()
	}
}

func TestPublicUdpRawByteBudgetCountsWholeDatagramsBeforeProtocolTruncation(t *testing.T) {
	client, server := publicUdpTestSocketPair("198.51.100.20:42000", "192.0.2.20:443")
	defer client.Close()
	defer server.Close()
	settings := DefaultPublicUdpProbeSettings()
	wire := &publicUdpPacketConn{PacketConn: client, peer: netip.MustParseAddrPort("192.0.2.20:443"), settings: settings}
	payload := make([]byte, 65535)
	// The protocol requests only a tiny buffer. The raw boundary must still
	// count all bytes, including oversized unsolicited traffic, before drop.
	for range settings.MaxByteCount/len(payload) + 1 {
		client.incoming <- publicUdpTestPacket{data: payload, from: net.UDPAddrFromAddrPort(netip.MustParseAddrPort("192.0.2.21:443"))}
	}
	if _, _, err := wire.ReadFrom(make([]byte, 8)); !errors.Is(err, errPublicUdpBudget) {
		t.Fatal("socket truncation bypassed the complete raw-datagram byte budget")
	}
	if wire.observation().Failure != PublicUdpFailureBudget {
		t.Fatal("raw datagram overflow did not remain a fixed unknown class")
	}
}

func publicUdpTestNativeCancellation(t *testing.T, carrier string) {
	t.Helper()
	request := publicUdpTestNativeRequest("ipv4", "connect", carrier)
	client, server := publicUdpTestSocketPair("198.51.100.20:42000", publicUdpTuple(request))
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := observePublicUdp(ctx, request, DefaultPublicUdpProbeSettings(), func(context.Context, string, string) (net.PacketConn, error) { return client, nil }, x509.NewCertPool())
		done <- err
	}()
	select {
	case <-client.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("native first-write barrier not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("native cancellation did not retain cancellation authority")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native QUIC cancellation failed to join")
	}
	select {
	case <-client.closed:
	default:
		t.Fatal("native cancellation returned before socket cleanup")
	}
}

func TestPublicUdpNativeCancellationJoinsAfterFirstRawWrite(t *testing.T) {
	for _, carrier := range []string{"quic", "dns", "dns-pump"} {
		publicUdpTestNativeCancellation(t, carrier)
	}
}

func TestPublicUdpNativeRouteAndSocketPrerequisitesRemainUnknown(t *testing.T) {
	for _, cause := range []error{syscall.ENETUNREACH, syscall.EAFNOSUPPORT, errors.New("synthetic-private-socket-error")} {
		request := publicUdpTestNativeRequest("ipv6", "connect", "quic")
		calls := 0
		observed, err := observePublicUdp(context.Background(), request, DefaultPublicUdpProbeSettings(), func(context.Context, string, string) (net.PacketConn, error) {
			calls++
			return nil, cause
		}, nil)
		expected := PublicUdpFailureSocket
		if publicUdpRouteError(cause) {
			expected = PublicUdpFailureRoute
		}
		if err == nil || observed.FreshSocket || calls != 1 || observed.Failure != expected {
			t.Fatal("missing local socket/family route did not retain its bounded prerequisite class")
		}
		findings := publicUdpFindings(request, observed, err, DefaultPublicUdpProbeSettings())
		if len(findings) != 1 || findings[0].healthy || findings[0].class != "public-udp-observation" || !strings.Contains(findings[0].observed, string(expected)) {
			t.Fatal("missing local prerequisite was inferred as remote outage or healthy transport")
		}
	}
}

func TestPublicUdpNativePreCanceledAttemptNeverOpensSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := observePublicUdp(ctx, publicUdpTestNativeRequest("ipv4", "connect", "quic"), DefaultPublicUdpProbeSettings(), func(context.Context, string, string) (net.PacketConn, error) {
		calls++
		return nil, errors.New("synthetic forbidden socket")
	}, nil)
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal("pre-canceled attempt opened a socket")
	}
}

// What the synthetic api saw of one request a front forwarded.
type publicUdpTestApiRequest struct {
	method    string
	path      string
	authority string
}

// Records every request the synthetic api serves.
type publicUdpTestApiRecorder struct {
	stateLock   sync.Mutex
	apiRequests []publicUdpTestApiRequest
}

func (self *publicUdpTestApiRecorder) record(r *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.apiRequests = append(self.apiRequests, publicUdpTestApiRequest{method: r.Method, path: r.URL.Path, authority: r.Host})
}

func (self *publicUdpTestApiRecorder) requests() []publicUdpTestApiRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]publicUdpTestApiRequest(nil), self.apiRequests...)
}

// The api's /hello as the front forwards it: the answer names the peer the
// server accepted, rendered as the api's ingress renders it.
func (self *publicUdpTestApiRecorder) hello() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		self.record(r)
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "unreadable peer", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"client_address":"` + netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port()).String() + `"}`))
	})
}

func publicUdpTestConnectApiRequest(family string, carrier string) PublicUdpRequest {
	request := publicUdpTestNativeRequest(family, "api", carrier)
	request.Service = "connect"
	return request
}

// Connect's front forwards api requests to the api blocks, so a connect api
// target follows its handshake with GET /hello over HTTP/3, on every carrier
// and in both families. The request names the api host, and only the api's
// answer completes the attempt. The h3 server opens its control stream first,
// so this also needs the client to allow it.
func TestPublicUdpConnectApiFrontCompletesHelloOverHttp3(t *testing.T) {
	for _, family := range []string{"ipv4", "ipv6"} {
		for _, carrier := range []string{"quic", "dns", "dns-pump"} {
			request := publicUdpTestConnectApiRequest(family, carrier)
			recorder := &publicUdpTestApiRecorder{}
			observed, err := publicUdpTestNativeAttempt(t, request, publicUdpTestNativeOptions{trusted: true, api: recorder.hello()})
			if err != nil || observed.Failure != PublicUdpFailureNone || !observed.HandshakeComplete || !observed.TlsVerified || observed.NegotiatedProtocol != "h3" {
				t.Fatalf("%s/%s: the connect api attempt failed: class=%s err=%v", family, carrier, observed.Failure, err)
			}
			if !observed.ApplicationRequested || !observed.ApplicationComplete || observed.ApplicationStatus != http.StatusOK ||
				!observed.ApplicationContentTypeJson || !observed.ApplicationBodyValid {
				t.Fatalf("%s/%s: the api request did not complete: %+v", family, carrier, observed)
			}
			want := []publicUdpTestApiRequest{{method: http.MethodGet, path: "/hello", authority: request.ServerName}}
			if apiRequests := recorder.requests(); !reflect.DeepEqual(apiRequests, want) {
				t.Fatalf("%s/%s: the api saw %+v, want %+v", family, carrier, apiRequests, want)
			}
			for _, finding := range publicUdpFindings(request, observed, err, DefaultPublicUdpProbeSettings()) {
				if !finding.healthy {
					t.Fatalf("%s/%s: a completed api request was not healthy: %s", family, carrier, finding.class)
				}
			}
		}
	}
}

// Only a 200 JSON object whose client_address is an address and port of the
// probed family completes the request. Every other answer is a fixed
// application failure that the signal cannot read as healthy, and the
// address in the body is never retained.
func TestPublicUdpConnectApiFrontRefusesAnswersOutsideTheApiShape(t *testing.T) {
	const bodyAddress = "203.0.113.200:41000"
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{name: "an unavailable api", status: http.StatusServiceUnavailable, contentType: "application/json", body: `{"client_address":"203.0.113.200:41000"}`},
		{name: "a redirect", status: http.StatusMovedPermanently, contentType: "application/json", body: `{"client_address":"203.0.113.200:41000"}`},
		{name: "a text body", status: http.StatusOK, contentType: "text/plain", body: `{"client_address":"203.0.113.200:41000"}`},
		{name: "a json array", status: http.StatusOK, contentType: "application/json", body: `["203.0.113.200:41000"]`},
		{name: "a json null", status: http.StatusOK, contentType: "application/json", body: `null`},
		{name: "no client address", status: http.StatusOK, contentType: "application/json", body: `{"gossip_peer_id":"synthetic"}`},
		{name: "a client address that is not one", status: http.StatusOK, contentType: "application/json", body: `{"client_address":"synthetic"}`},
		{name: "a client address with no port", status: http.StatusOK, contentType: "application/json", body: `{"client_address":"203.0.113.200"}`},
		{name: "a client address of the other family", status: http.StatusOK, contentType: "application/json", body: `{"client_address":"[2001:db8::200]:41000"}`},
		{name: "trailing data", status: http.StatusOK, contentType: "application/json", body: `{"client_address":"203.0.113.200:41000"} {}`},
	}
	for _, c := range cases {
		request := publicUdpTestConnectApiRequest("ipv4", "quic")
		recorder := &publicUdpTestApiRecorder{}
		api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			recorder.record(r)
			w.Header().Set("Content-Type", c.contentType)
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		})
		observed, err := publicUdpTestNativeAttempt(t, request, publicUdpTestNativeOptions{trusted: true, api: api})
		if err != nil || observed.Failure != PublicUdpFailureApplication || !observed.ApplicationRequested || observed.ApplicationComplete ||
			observed.ApplicationStatus != c.status || !observed.HandshakeComplete || !observed.TlsVerified {
			t.Errorf("%s: the answer was not a fixed application failure: %+v err=%v", c.name, observed, err)
		}
		if len(recorder.requests()) != 1 {
			t.Errorf("%s: the api saw %d requests", c.name, len(recorder.requests()))
		}
		if strings.Contains(fmt.Sprintf("%+v", observed), bodyAddress) {
			t.Errorf("%s: the observation retained the address in the body", c.name)
		}
		// the verified handshake resolves the row's path, never its application
		findings := publicUdpFindings(request, observed, err, DefaultPublicUdpProbeSettings())
		if len(findings) == 0 || findings[0].healthy || findings[0].class != "public-udp-application" {
			t.Errorf("%s: an invalid api answer was not an application failure: %+v", c.name, findings)
		}
		for _, result := range findings[1:] {
			if !result.healthy || result.class == "public-udp-application" {
				t.Errorf("%s: an invalid api answer established health: %s healthy=%t", c.name, result.class, result.healthy)
			}
		}
	}
}

// The body is read through its byte bound: an answer of the api's shape that
// runs past it is not complete, while the same answer under the default
// bound is.
func TestPublicUdpConnectApiFrontBoundsTheHelloBody(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := netip.MustParseAddrPort(r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"client_address":"` + peer.String() + `","gossip_peer_id":"` + strings.Repeat("a", 256) + `"}`))
	})
	bounded := DefaultPublicUdpProbeSettings()
	bounded.ApiRequest.ResponseBodyByteLimit = 64
	for _, c := range []struct {
		settings *PublicUdpProbeSettings
		complete bool
	}{
		{settings: bounded, complete: false},
		{settings: DefaultPublicUdpProbeSettings(), complete: true},
	} {
		request := publicUdpTestConnectApiRequest("ipv4", "quic")
		observed, err := publicUdpTestNativeAttempt(t, request, publicUdpTestNativeOptions{trusted: true, api: api, probeSettings: c.settings})
		if err != nil || observed.ApplicationComplete != c.complete || observed.ApplicationBodyValid != c.complete || observed.ApplicationStatus != http.StatusOK {
			t.Fatalf("a %d byte bound: %+v err=%v", c.settings.ApiRequest.ResponseBodyByteLimit, observed, err)
		}
		if !c.complete && observed.Failure != PublicUdpFailureApplication {
			t.Fatalf("a body past its bound was not an application failure: %s", observed.Failure)
		}
	}
}

// The /hello response header is read through its bound too: an answer of the
// api's shape whose header block runs past it never completes, while the same
// answer under the default bound does, so only the bound refused it. No
// header text is retained.
func TestPublicUdpConnectApiFrontBoundsTheHelloHeader(t *testing.T) {
	const marker = "synthetic-header-marker"
	recorder := &publicUdpTestApiRecorder{}
	hello := recorder.hello()
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Synthetic-Padding", marker+strings.Repeat("p", 4*1024))
		hello.ServeHTTP(w, r)
	})
	bounded := DefaultPublicUdpProbeSettings()
	bounded.ApiRequest.ResponseHeaderByteLimit = 1024
	request := publicUdpTestConnectApiRequest("ipv4", "quic")
	observed, err := publicUdpTestNativeAttempt(t, request, publicUdpTestNativeOptions{trusted: true, api: api, probeSettings: bounded})
	if err == nil || !observed.ApplicationRequested || observed.ApplicationComplete || observed.ApplicationStatus != 0 ||
		observed.Failure != PublicUdpFailureApplication || !observed.HandshakeComplete || !observed.TlsVerified {
		t.Fatalf("a header past its bound gave %+v err=%v", observed, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", observed), marker) || strings.Contains(err.Error(), marker) {
		t.Fatalf("a diagnostic carried the header: %+v err=%v", observed, err)
	}
	if len(recorder.requests()) != 1 {
		t.Fatalf("the api saw %d requests", len(recorder.requests()))
	}
	observed, err = publicUdpTestNativeAttempt(t, request, publicUdpTestNativeOptions{trusted: true, api: api})
	if err != nil || !observed.ApplicationComplete || observed.Failure != PublicUdpFailureNone {
		t.Fatalf("the same answer under the default bound gave %+v err=%v", observed, err)
	}
}

// The api request rides the attempt's own context: canceling the attempt
// while the api holds the request returns the cancellation at once, with the
// failure standing and the socket closed. The handler's entry is the barrier.
func TestPublicUdpConnectApiFrontCancellationEndsTheApiRequest(t *testing.T) {
	attemptCtx, cancelAttempt := context.WithCancel(context.Background())
	defer cancelAttempt()
	entered := make(chan struct{})
	var enterOnce sync.Once
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() { close(entered) })
		cancelAttempt()
		<-r.Context().Done()
	})
	request := publicUdpTestConnectApiRequest("ipv4", "quic")
	observed, err := publicUdpTestNativeAttempt(t, request, publicUdpTestNativeOptions{trusted: true, api: api, attemptCtx: attemptCtx})
	select {
	case <-entered:
	default:
		t.Fatal("the api request never reached the api")
	}
	if !errors.Is(err, context.Canceled) || observed.Failure != PublicUdpFailureApplication || observed.ApplicationComplete || observed.ApplicationStatus != 0 {
		t.Fatalf("a canceled api request kept or lost authority: %+v err=%v", observed, err)
	}
}

// A connect api target passes the transport's own authority check and opens
// its socket; before LBALT it was refused there as configuration-invalid.
// Its request also needs a positive body bound before any socket opens.
func TestPublicUdpTransportAdmitsAConnectApiRequest(t *testing.T) {
	request := publicUdpTestConnectApiRequest("ipv4", "quic")
	listens := 0
	observed, err := observePublicUdp(context.Background(), request, DefaultPublicUdpProbeSettings(), func(context.Context, string, string) (net.PacketConn, error) {
		listens++
		return nil, errors.New("synthetic-private-socket-error")
	}, nil)
	if err == nil || listens != 1 || observed.Failure != PublicUdpFailureSocket {
		t.Fatalf("a connect api request did not reach its socket: listens=%d class=%s", listens, observed.Failure)
	}
	unboundedBody := DefaultPublicUdpProbeSettings()
	unboundedBody.ApiRequest.ResponseBodyByteLimit = 0
	for _, settings := range []*PublicUdpProbeSettings{nil, unboundedBody} {
		listens = 0
		observed, err = observePublicUdp(context.Background(), request, settings, func(context.Context, string, string) (net.PacketConn, error) {
			listens++
			return nil, errors.New("synthetic-private-socket-error")
		}, nil)
		if err == nil || listens != 0 || observed.Failure != PublicUdpFailureConfiguration {
			t.Fatalf("an unbounded api request opened a socket: listens=%d class=%s", listens, observed.Failure)
		}
	}
}
