package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	connectcore "github.com/urnetwork/connect"
)

const (
	// the api route a connect api target requests: no auth and no database,
	// and a JSON body that names the address the api saw
	// (controller.HelloResult)
	publicUdpApiRequestPath = "/hello"
	// RFC 9114 §6.2 and RFC 9204 §4.2: an h3 client lets its peer open the
	// control stream and both QPACK streams, or the server cannot start
	publicUdpH3UniStreamCount = 3
)

var errPublicUdpBudget = errors.New("public UDP wire budget exhausted")

// The packet boundary is below DNS translation and QUIC. It never passes a
// datagram from a different wire tuple upward, even if its payload looks valid.
// Locks protect bookkeeping only, never a blocking transport operation.
type publicUdpPacketConn struct {
	net.PacketConn
	peer netip.AddrPort

	stateLock       sync.Mutex
	sentPackets     int
	sentBytes       int
	receivedPackets int
	receivedBytes   int
	unexpected      int
	writes          int
	writeBytes      int
	reads           int
	readBytes       int
	failure         PublicUdpFailure
}

func publicUdpAddress(addr net.Addr) (netip.AddrPort, bool) {
	if addr == nil {
		return netip.AddrPort{}, false
	}
	parsed, err := netip.ParseAddrPort(addr.String())
	if err != nil || parsed.Addr().Zone() != "" {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(parsed.Addr().Unmap(), parsed.Port()), true
}

func (self *publicUdpPacketConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	// Read the whole bounded UDP payload even when QUIC asks for a smaller
	// buffer. Otherwise oversized unsolicited datagrams could evade byte
	// accounting through socket truncation. No payload escapes this method.
	var datagram [65535]byte
	for {
		n, address, err := self.PacketConn.ReadFrom(datagram[:])
		if err != nil {
			return n, address, err
		}
		peer, valid := publicUdpAddress(address)
		accepted, exhausted := func() (bool, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if n < 0 || n > len(datagram) || self.reads >= publicUdpMaxPackets || publicUdpMaxBytes-self.readBytes < n {
				self.failure = PublicUdpFailureBudget
				return false, true
			}
			self.reads++
			self.readBytes += n
			if !valid || peer != self.peer {
				self.unexpected++
				return false, false
			}
			self.receivedPackets++
			self.receivedBytes += n
			return true, false
		}()
		if exhausted {
			self.PacketConn.Close()
			return 0, nil, errPublicUdpBudget
		}
		if accepted {
			return copy(buffer, datagram[:n]), address, nil
		}
	}
}

func (self *publicUdpPacketConn) WriteTo(buffer []byte, address net.Addr) (int, error) {
	peer, valid := publicUdpAddress(address)
	if !valid || peer != self.peer {
		return 0, errors.New("public UDP destination differs from pin")
	}
	exhausted := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.writes >= publicUdpMaxPackets || publicUdpMaxBytes-self.writeBytes < len(buffer) {
			self.failure = PublicUdpFailureBudget
			return true
		}
		// Reserve the budget before concurrent writes reach the socket.
		self.writes++
		self.writeBytes += len(buffer)
		return false
	}()
	if exhausted {
		self.PacketConn.Close()
		return 0, errPublicUdpBudget
	}
	n, err := self.PacketConn.WriteTo(buffer, address)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if n > 0 && n <= len(buffer) {
			self.sentPackets++
			self.sentBytes += n
		}
		if publicUdpRouteError(err) && self.failure == PublicUdpFailureNone {
			self.failure = PublicUdpFailureRoute
		}
	}()
	return n, err
}

func (self *publicUdpPacketConn) observation() PublicUdpObservation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	peer := ""
	if self.receivedPackets > 0 {
		peer = self.peer.String()
	}
	return PublicUdpObservation{
		PeerTuple: peer, SentPackets: self.sentPackets, SentBytes: self.sentBytes,
		ReceivedPackets: self.receivedPackets, ReceivedBytes: self.receivedBytes,
		UnexpectedPackets: self.unexpected, Failure: self.failure,
	}
}

func publicUdpRouteError(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT)
}

func (self *runner) publicUdp(ctx context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
	timeout := publicUdpAttemptTimeout
	if self.cfg.commandTimeout > 0 && self.cfg.commandTimeout < timeout {
		timeout = self.cfg.commandTimeout
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	listen := func(ctx context.Context, network string, address string) (net.PacketConn, error) {
		return (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	}
	return observePublicUdp(attemptCtx, request, DefaultPublicUdpApiRequestSettings(), listen, nil)
}

// Test seams supply a private packet network and trust roots, not a different
// protocol. Production always uses a fresh UDP socket and normal system roots.
// Dial (not DialEarly) waits for the authenticated QUIC handshake. Connect's
// custom H3 offers no HTTP/3 ALPN; an api front offers h3.
//
// A connect api front then answers one GET /hello over HTTP/3, within the same
// attempt deadline (LBALT.md D17). That front forwards api requests to the api
// blocks, so its handshake proves nothing about the api behind it; alt's api
// front embeds the api, and alt's fronts keep ending at the handshake. Only a
// 200 with a JSON body in the api's shape completes the request, and anything
// else is a fixed application failure. The h3 client lets the server open its
// control and QPACK streams, which the handshake-only fronts never allow.
func observePublicUdp(
	ctx context.Context,
	request PublicUdpRequest,
	apiRequestSettings *PublicUdpApiRequestSettings,
	listen func(context.Context, string, string) (net.PacketConn, error),
	roots *x509.CertPool,
) (observation PublicUdpObservation, resultErr error) {
	observation.AttemptId = request.AttemptId
	observation.RequestedTuple = publicUdpTuple(request)
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	peer, err := netip.ParseAddrPort(observation.RequestedTuple)
	if err != nil || request.AttemptId == "" || !publicUdpServerName(request.ServerName) ||
		(request.Family != "ipv4" && request.Family != "ipv6") || (request.Family == "ipv4") != peer.Addr().Is4() ||
		(request.Service != "connect" && request.Service != "alt") || (request.Front != "connect" && request.Front != "api") ||
		(request.Carrier != "quic" && request.Carrier != "dns" && request.Carrier != "dns-pump") ||
		request.Carrier != "quic" && (!strings.HasSuffix(request.DnsTld, ".") || !publicUdpServerName(strings.TrimSuffix(request.DnsTld, "."))) ||
		request.Carrier == "quic" && request.DnsTld != "" {
		observation.Failure = PublicUdpFailureConfiguration
		return observation, errors.New("public UDP request authority invalid")
	}
	apiRequest := request.Service == "connect" && request.Front == "api"
	if apiRequest && (apiRequestSettings == nil || apiRequestSettings.ResponseBodyByteLimit <= 0) {
		observation.Failure = PublicUdpFailureConfiguration
		return observation, errors.New("public UDP api request bounds invalid")
	}
	network, bind := "udp4", "0.0.0.0:0"
	if request.Family == "ipv6" {
		network, bind = "udp6", "[::]:0"
	}
	raw, err := listen(ctx, network, bind)
	if err != nil {
		observation.Failure = PublicUdpFailureSocket
		if publicUdpRouteError(err) {
			observation.Failure = PublicUdpFailureRoute
		}
		return observation, errors.New("public UDP socket unavailable")
	}
	observation.FreshSocket = true
	wire := &publicUdpPacketConn{PacketConn: raw, peer: peer}
	var packets net.PacketConn = wire
	var transport *quic.Transport
	var connection *quic.Conn
	defer func() {
		if connection != nil {
			connection.CloseWithError(0, "bounded transport observation complete")
		}
		// Closing the owned packet boundary releases pending reads first. DNS
		// translation Close joins its bounded workers and returns pooled data.
		packets.Close()
		if transport != nil {
			transport.Close()
		}
		wireObservation := wire.observation()
		observation.PeerTuple = wireObservation.PeerTuple
		observation.SentPackets, observation.SentBytes = wireObservation.SentPackets, wireObservation.SentBytes
		observation.ReceivedPackets, observation.ReceivedBytes = wireObservation.ReceivedPackets, wireObservation.ReceivedBytes
		observation.UnexpectedPackets = wireObservation.UnexpectedPackets
		if wireObservation.Failure != PublicUdpFailureNone {
			observation.Failure = wireObservation.Failure
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err := raw.SetDeadline(deadline); err != nil {
			observation.Failure = PublicUdpFailureSocket
			return observation, errors.New("public UDP deadline unavailable")
		}
	}
	if request.Carrier != "quic" {
		settings := connectcore.DefaultPacketTranslationSettings()
		settings.Log = connectcore.NewNoopLogger()
		settings.DnsTlds = [][]byte{[]byte(request.DnsTld)}
		mode := connectcore.PacketTranslationModeDns
		if request.Carrier == "dns-pump" {
			mode = connectcore.PacketTranslationModeDnsPump
		}
		translated, err := connectcore.NewPacketTranslation(ctx, mode, wire, settings)
		if err != nil {
			observation.Failure = PublicUdpFailureConfiguration
			return observation, errors.New("public UDP codec unavailable")
		}
		packets = translated
	}
	tlsConfig := &tls.Config{ServerName: request.ServerName, RootCAs: roots, MinVersion: tls.VersionTLS13}
	if request.Front == "api" {
		tlsConfig.NextProtos = []string{http3.NextProtoH3}
	}
	quicConfig := &quic.Config{
		HandshakeIdleTimeout: publicUdpAttemptTimeout, MaxIdleTimeout: publicUdpAttemptTimeout,
		InitialPacketSize:          connectcore.H3InitialPacketByteCount,
		InitialStreamReceiveWindow: 16 * 1024, MaxStreamReceiveWindow: 16 * 1024,
		InitialConnectionReceiveWindow: 32 * 1024, MaxConnectionReceiveWindow: 32 * 1024,
		MaxIncomingStreams: -1, MaxIncomingUniStreams: -1, EnableDatagrams: true,
	}
	if apiRequest {
		// quic-go reads a negative limit as none, and an h3 server that cannot
		// open its control stream closes the connection before any request
		quicConfig.MaxIncomingUniStreams = publicUdpH3UniStreamCount
	}
	transport = &quic.Transport{Conn: packets}
	connection, err = transport.Dial(ctx, net.UDPAddrFromAddrPort(peer), tlsConfig, quicConfig)
	if err != nil {
		observation.Failure = PublicUdpFailureHandshake
		var certificateErr *tls.CertificateVerificationError
		var timeoutErr net.Error
		if errors.As(err, &certificateErr) {
			observation.Failure = PublicUdpFailureTls
		} else if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
			observation.Failure = PublicUdpFailureTimeout
		}
		if ctx.Err() != nil {
			return observation, ctx.Err()
		}
		return observation, errors.New("public UDP handshake incomplete")
	}
	state := connection.ConnectionState().TLS
	observation.HandshakeComplete = state.HandshakeComplete
	observation.TlsVerified = len(state.VerifiedChains) > 0
	observation.NegotiatedProtocol = state.NegotiatedProtocol
	if !observation.HandshakeComplete || !observation.TlsVerified {
		observation.Failure = PublicUdpFailureTls
	}
	if actualPeer, ok := publicUdpAddress(connection.RemoteAddr()); !ok || actualPeer != peer {
		observation.Failure = PublicUdpFailureHandshake
	}
	if !apiRequest || observation.Failure != PublicUdpFailureNone {
		return observation, nil
	}

	// Only an authenticated handshake from the exact pin carries the request.
	// The failure stands until a valid answer clears it.
	observation.ApplicationRequested = true
	observation.Failure = PublicUdpFailureApplication
	apiUrl := url.URL{Scheme: "https", Host: request.ServerName, Path: publicUdpApiRequestPath}
	apiHttpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, apiUrl.String(), nil)
	if err != nil {
		return observation, errors.New("public UDP api request unavailable")
	}
	apiTransport := &http3.Transport{
		// an encoded body could exceed the byte bound only once decoded
		DisableCompression:     true,
		MaxResponseHeaderBytes: apiRequestSettings.ResponseHeaderByteLimit,
	}
	response, err := apiTransport.NewClientConn(connection).RoundTrip(apiHttpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return observation, ctx.Err()
		}
		return observation, errors.New("public UDP api request incomplete")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(apiRequestSettings.ResponseBodyByteLimit)+1))
	response.Body.Close()
	observation.ApplicationStatus = response.StatusCode
	observation.ApplicationContentTypeJson = publicUdpJsonContentType(response.Header)
	if err != nil {
		if ctx.Err() != nil {
			return observation, ctx.Err()
		}
		return observation, errors.New("public UDP api response incomplete")
	}
	// The api's shape: one JSON object whose client_address is an address and
	// port of the probed family, as the api renders the peer its ingress saw.
	// The address is the observer's own, read only for its shape and never
	// retained.
	apiResponseValid := func() bool {
		if apiRequestSettings.ResponseBodyByteLimit < len(body) {
			return false
		}
		fields := map[string]json.RawMessage{}
		if json.Unmarshal(body, &fields) != nil || fields == nil {
			return false
		}
		var clientAddress string
		if json.Unmarshal(fields["client_address"], &clientAddress) != nil {
			return false
		}
		addrPort, err := netip.ParseAddrPort(clientAddress)
		if err != nil || addrPort.Addr().Zone() != "" || addrPort.Port() == 0 {
			return false
		}
		return addrPort.Addr().Is4() == (request.Family == "ipv4")
	}
	observation.ApplicationBodyValid = apiResponseValid()
	observation.ApplicationComplete = response.StatusCode == http.StatusOK && observation.ApplicationContentTypeJson && observation.ApplicationBodyValid
	if observation.ApplicationComplete {
		observation.Failure = PublicUdpFailureNone
	}
	return observation, nil
}

// Whether a response carries a Content-Type and every one it carries is JSON.
// A proxy can add the header beside its upstream's, as nginx's lb status
// routes do, so each value counts.
func publicUdpJsonContentType(header http.Header) bool {
	values := header.Values("Content-Type")
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		mediaType, _, err := mime.ParseMediaType(value)
		if err != nil || mediaType != "application/json" {
			return false
		}
	}
	return true
}
