package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	connectcore "github.com/urnetwork/connect"
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
	return observePublicUdp(attemptCtx, request, listen, nil)
}

// Test seams supply a private packet network and trust roots, not a different
// protocol. Production always uses a fresh UDP socket and normal system roots.
// Dial (not DialEarly) waits for the authenticated QUIC handshake. Connect's
// custom H3 offers no HTTP/3 ALPN; only the Alt API front offers h3.
func observePublicUdp(
	ctx context.Context,
	request PublicUdpRequest,
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
		request.Service == "connect" && request.Front != "connect" ||
		(request.Carrier != "quic" && request.Carrier != "dns" && request.Carrier != "dns-pump") ||
		request.Carrier != "quic" && (!strings.HasSuffix(request.DnsTld, ".") || !publicUdpServerName(strings.TrimSuffix(request.DnsTld, "."))) ||
		request.Carrier == "quic" && request.DnsTld != "" {
		observation.Failure = PublicUdpFailureConfiguration
		return observation, errors.New("public UDP request authority invalid")
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
	transport = &quic.Transport{Conn: packets}
	connection, err = transport.Dial(ctx, net.UDPAddrFromAddrPort(peer), tlsConfig, &quic.Config{
		HandshakeIdleTimeout: publicUdpAttemptTimeout, MaxIdleTimeout: publicUdpAttemptTimeout,
		InitialPacketSize:          connectcore.H3InitialPacketByteCount,
		InitialStreamReceiveWindow: 16 * 1024, MaxStreamReceiveWindow: 16 * 1024,
		InitialConnectionReceiveWindow: 32 * 1024, MaxConnectionReceiveWindow: 32 * 1024,
		MaxIncomingStreams: -1, MaxIncomingUniStreams: -1, EnableDatagrams: true,
	})
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
	return observation, nil
}
