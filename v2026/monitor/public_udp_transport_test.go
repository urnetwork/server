package monitor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	connectcore "github.com/urnetwork/connect/v2026"
)

type publicUdpTestPacket struct {
	data []byte
	from net.Addr
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
			incoming: make(chan publicUdpTestPacket, publicUdpMaxPackets+1),
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
	clientAddress := "198.51.100.20:42000"
	if request.Family == "ipv6" {
		clientAddress = "[2001:db8:1::20]:42000"
	}
	client, server := publicUdpTestSocketPair(clientAddress, publicUdpTuple(request))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var serverPackets net.PacketConn = server
	var serverTransport *quic.Transport
	var listener *quic.Listener
	defer func() {
		cancel()
		if listener != nil {
			listener.Close()
		}
		serverPackets.Close()
		if serverTransport != nil {
			serverTransport.Close()
		}
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
	// Unsolicited bytes from another tuple must not spoil a later exact,
	// authenticated handshake or become return-path root-cause attribution.
	client.incoming <- publicUdpTestPacket{data: []byte("synthetic unsolicited datagram"), from: net.UDPAddrFromAddrPort(netip.MustParseAddrPort("203.0.113.99:8053"))}
	listenCount := 0
	observed, observeErr := observePublicUdp(ctx, request, func(_ context.Context, network string, _ string) (net.PacketConn, error) {
		listenCount++
		if network != "udp4" && request.Family == "ipv4" || network != "udp6" && request.Family == "ipv6" {
			t.Error("native socket family differs from explicit request")
		}
		return client, nil
	}, roots)
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
				for _, finding := range publicUdpFindings(request, observed, err) {
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
		findings := publicUdpFindings(request, observed, err)
		if len(findings) != 1 || findings[0].healthy || findings[0].class != "public-udp-observation" {
			t.Fatal("unauthenticated tuple traffic established transport health")
		}
	}
}

func TestPublicUdpRawTupleFilterDropsWrongSourceBeforeProtocol(t *testing.T) {
	client, server := publicUdpTestSocketPair("198.51.100.20:42000", "192.0.2.20:443")
	defer client.Close()
	defer server.Close()
	wire := &publicUdpPacketConn{PacketConn: client, peer: netip.MustParseAddrPort("192.0.2.20:443")}
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
		wire := &publicUdpPacketConn{PacketConn: client, peer: netip.MustParseAddrPort("192.0.2.20:443")}
		var err error
		if inbound {
			for range publicUdpMaxPackets + 1 {
				client.incoming <- publicUdpTestPacket{data: []byte{1}, from: net.UDPAddrFromAddrPort(netip.MustParseAddrPort("192.0.2.21:443"))}
			}
			_, _, err = wire.ReadFrom(make([]byte, 64))
		} else {
			_, err = wire.WriteTo(make([]byte, publicUdpMaxBytes+1), server.address)
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
	wire := &publicUdpPacketConn{PacketConn: client, peer: netip.MustParseAddrPort("192.0.2.20:443")}
	payload := make([]byte, 65535)
	// The protocol requests only a tiny buffer. The raw boundary must still
	// count all bytes, including oversized unsolicited traffic, before drop.
	for range publicUdpMaxBytes/len(payload) + 1 {
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
		_, err := observePublicUdp(ctx, request, func(context.Context, string, string) (net.PacketConn, error) { return client, nil }, x509.NewCertPool())
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
		observed, err := observePublicUdp(context.Background(), request, func(context.Context, string, string) (net.PacketConn, error) {
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
		findings := publicUdpFindings(request, observed, err)
		if len(findings) != 1 || findings[0].healthy || findings[0].class != "public-udp-observation" || !strings.Contains(findings[0].observed, string(expected)) {
			t.Fatal("missing local prerequisite was inferred as remote outage or healthy transport")
		}
	}
}

func TestPublicUdpNativePreCanceledAttemptNeverOpensSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := observePublicUdp(ctx, publicUdpTestNativeRequest("ipv4", "connect", "quic"), func(context.Context, string, string) (net.PacketConn, error) {
		calls++
		return nil, errors.New("synthetic forbidden socket")
	}, nil)
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal("pre-canceled attempt opened a socket")
	}
}
