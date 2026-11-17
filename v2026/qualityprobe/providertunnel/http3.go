// A URL request's HTTP/3 leg: the same request as its HTTPS leg, over QUIC on
// a UDP socket of the provider tunnel's own stack. It is what tells a
// provider that carries UDP from one that carries only TCP.
package providertunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/connect/v2026"
)

// The outcomes of a retried QUIC handshake (dialQuic): answered, the site
// answered the second handshake, which is the first's loss the retry made up
// for; silent, the second heard nothing either, within its bound or what was
// left of the request's; canceled, the request ended under it.
const (
	providerQuicRetryAnswered = "answered"
	providerQuicRetrySilent   = "silent"
	providerQuicRetryCanceled = "canceled"
)

var providerQuicHandshakeRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_tunnel_quic_handshake_retries_total",
	Help: "URL probe HTTP/3 legs whose first QUIC handshake heard nothing back within its bound and was made once more on a fresh socket, by the leg's IP family (ipv4, ipv6) and how the second ended: answered (the site answered it, so the first was a lost handshake, not the provider's UDP), silent (no answer again; the leg reports silence), canceled (the request ended under it)",
}, []string{"family", "outcome"})

// Registers the retry counter with every label pair at zero.
func init() {
	for _, family := range []string{"ipv4", "ipv6"} {
		for _, outcome := range []string{providerQuicRetryAnswered, providerQuicRetrySilent, providerQuicRetryCanceled} {
			providerQuicHandshakeRetries.WithLabelValues(family, outcome)
		}
	}
	prometheus.MustRegister(providerQuicHandshakeRetries)
}

// Counts one retried handshake by its request's family and how it ended.
func observeProviderQuicHandshakeRetry(ctx context.Context, err error, received int64) {
	family := "ipv4"
	if ipVersion, _ := providerUrlRequestFamily(ctx); ipVersion == 6 {
		family = "ipv6"
	}
	outcome := providerQuicRetrySilent
	switch {
	case err == nil || 0 < received:
		outcome = providerQuicRetryAnswered
	case errors.Is(err, context.Canceled):
		outcome = providerQuicRetryCanceled
	}
	providerQuicHandshakeRetries.WithLabelValues(family, outcome).Inc()
}

// The leg's QUIC bounds, in line with its HTTPS leg's (url_phase_timeout.go).
//
// The handshake idle bound is the HTTPS leg's TCP connect bound. quic-go
// aborts a handshake that hears nothing from the peer for that long, and one
// that has not completed in twice it, which is the HTTPS leg's connect and
// TLS bounds together. A provider path that carries no UDP answers no QUIC
// Initial, so the leg fails at the bound of its second handshake (dialQuic
// makes a silent one once more), two bounds well inside its request timeout
// and the turn's slot. It is a var so a test can shorten it.
var urlProbeQuicHandshakeIdleTimeout = defaultProviderUrlPhaseSettings().TcpTimeout

// After the handshake a connection that hears nothing for the HTTPS leg's
// read-idle bound is over, as a stalled HTTPS read is.
const urlProbeQuicIdleTimeout = urlProbeReadIdleTimeout

// The largest QUIC datagram the probe tunnel's IPv4 link carries in one
// packet: its 1280-byte MTU (connect.DefaultMtu, server.CapServerTunIPv4) less
// the IPv4 and UDP headers. quic-go's default first packet (1280 bytes of UDP
// payload) would need IP fragmentation, and path MTU discovery would probe
// past the link; both are kept off the provider path. QUIC's 1200-byte
// minimum for a client's first datagram still holds.
const urlProbeQuicPacketSize = connect.DefaultMtu - 20 - 8

// The same over the dual-stack tunnel's IPv6 link (tunnel.go): the 1280-byte
// MTU less the 40-byte IPv6 and the UDP headers, still above QUIC's 1200.
// IPv6 is never fragmented by a router, so a datagram that did not fit would
// be lost on the path rather than split.
const urlProbeQuicIpv6PacketSize = connect.DefaultMtu - 40 - 8

// A fresh QUIC configuration per IPv4 leg: one version, as HTTP/3 dialing
// needs, no session resumption, no 0-RTT, no keep-alive, and nothing that
// outlives the one request.
func providerUrlProbeQuicConfig() *quic.Config {
	return providerUrlProbeQuicConfigFor(4)
}

// The configuration of a leg in one IP family, its first datagram sized to
// that family's link (urlProbeQuicPacketSize, urlProbeQuicIpv6PacketSize).
func providerUrlProbeQuicConfigFor(ipVersion int) *quic.Config {
	packetSize := uint16(urlProbeQuicPacketSize)
	if ipVersion == 6 {
		packetSize = urlProbeQuicIpv6PacketSize
	}
	return &quic.Config{
		Versions:                []quic.Version{quic.Version1},
		HandshakeIdleTimeout:    urlProbeQuicHandshakeIdleTimeout,
		MaxIdleTimeout:          urlProbeQuicIdleTimeout,
		InitialPacketSize:       packetSize,
		DisablePathMTUDiscovery: true,
	}
}

// Implements egresshealth's HTTP/3 transport hook: a round tripper for one
// URL request's HTTP/3 leg through this transport's tunnel, admitted,
// resolved and verified exactly as its HTTPS requests are. Nil when the
// transport was built over a bare dialer, which has no UDP path.
func (self *providerHttpTransport) ProviderUrlProbeH3RoundTripper() http.RoundTripper {
	if self == nil || self.dialPacket == nil || self.resolver == nil {
		return nil
	}
	return &providerH3RoundTripper{https: self}
}

// One URL request's HTTP/3 transport. Every RoundTrip dials its own QUIC
// connection on its own UDP socket, and releases both when its response body
// is closed or the request fails: no connection is reused, as the HTTPS
// leg's keep-alives are off. The probes egresshealth asks of a transport are
// answered by the HTTPS transport of the same tunnel.
type providerH3RoundTripper struct {
	https *providerHttpTransport
}

// Implements egresshealth's per-request URL grant, as the HTTPS leg's.
func (self *providerH3RoundTripper) ProviderUrlProbeContext(ctx context.Context, target *url.URL, redirect bool) (context.Context, error) {
	return self.https.ProviderUrlProbeContext(ctx, target, redirect)
}

// Implements egresshealth's per-request family grant, as the HTTPS leg's: the
// leg resolves only the family's record and opens a UDP socket of that family
// alone.
func (self *providerH3RoundTripper) ProviderUrlProbeFamilyContext(ctx context.Context, target *url.URL, redirect bool, ipVersion int) (context.Context, error) {
	return self.https.ProviderUrlProbeFamilyContext(ctx, target, redirect, ipVersion)
}

func (self *providerH3RoundTripper) ProviderMeasurementUnavailable() bool {
	return self.https.ProviderMeasurementUnavailable()
}

func (self *providerH3RoundTripper) ProviderContractAcquisitionUnavailable() bool {
	return self.https.ProviderContractAcquisitionUnavailable()
}

func (self *providerH3RoundTripper) ProviderLocalWriteUnavailable() bool {
	return self.https.ProviderLocalWriteUnavailable()
}

// Sends one request over a QUIC connection of its own. The transport's Dial is
// always set, so http3 never opens a socket of the host's: the leg's only
// datagrams are the tunnel's.
func (self *providerH3RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	session := newProviderH3Session(self.https)
	ipVersion, _ := providerUrlRequestFamily(req.Context())
	transport := &http3.Transport{
		Dial:               session.dial,
		QUICConfig:         providerUrlProbeQuicConfigFor(ipVersion),
		DisableCompression: true,
	}
	release := func() {
		transport.Close()
		session.close()
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		release()
		// A request deadline that ends the leg while its handshake still
		// waits returns the context's error, not the dial's. With nothing
		// back from the site that is the same silence the handshake's own
		// bound reports, and keeps its stage.
		var staged *providerHttpStageError
		var timed interface{ Timeout() bool }
		if !errors.As(err, &staged) && session.received() == 0 && session.dialed() &&
			(errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timed) && timed.Timeout())) {
			err = &providerHttpStageError{stage: "dial_quic", err: err}
		}
		return nil, err
	}
	response.Body = &providerH3Body{ReadCloser: response.Body, release: release}
	return response, nil
}

// A response body that releases its request's QUIC connection and UDP socket
// when it is closed.
type providerH3Body struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (self *providerH3Body) Close() error {
	err := self.ReadCloser.Close()
	self.once.Do(self.release)
	return err
}

// What one HTTP/3 request owns: the dials http3 makes for it (one, or two
// when it retries a refused request), and each dial's QUIC transport and UDP
// socket. Close cancels a dial still in flight, joins it, and closes what the
// dials opened, newest first.
type providerH3Session struct {
	https  *providerHttpTransport
	ctx    context.Context
	cancel context.CancelFunc

	stateLock   sync.Mutex
	closed      bool
	dials       sync.WaitGroup
	owned       []io.Closer
	packetConns []*providerH3PacketConn
}

// Whether a dial of the session opened a UDP socket to the site.
func (self *providerH3Session) dialed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return 0 < len(self.packetConns)
}

// The datagrams that came back to the session's sockets.
func (self *providerH3Session) received() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var received int64
	for _, packetConn := range self.packetConns {
		received += packetConn.received.Load()
	}
	return received
}

func newProviderH3Session(https *providerHttpTransport) *providerH3Session {
	ctx, cancel := context.WithCancel(context.Background())
	return &providerH3Session{https: https, ctx: ctx, cancel: cancel}
}

// Implements http3.Transport.Dial for this request.
func (self *providerH3Session) dial(ctx context.Context, addr string, _ *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
	admitted := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.dials.Add(1)
		return true
	}()
	if !admitted {
		return nil, net.ErrClosed
	}
	defer self.dials.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(self.ctx, cancel)
	defer stop()
	return self.https.dialQuic(ctx, addr, quicConfig, self.own)
}

// Records something a dial opened, for close, and a socket also for its count
// of datagrams; closes it at once, and says so, when the session is closed.
func (self *providerH3Session) own(closer io.Closer) bool {
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		closer.Close()
		return false
	}
	self.owned = append(self.owned, closer)
	if packetConn, ok := closer.(*providerH3PacketConn); ok {
		self.packetConns = append(self.packetConns, packetConn)
	}
	self.stateLock.Unlock()
	return true
}

func (self *providerH3Session) close() {
	var owned []io.Closer
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
		owned = self.owned
		self.owned = nil
	}()
	self.cancel()
	self.dials.Wait()
	// a QUIC transport first, so its read loop stops before its socket closes
	for i := len(owned) - 1; 0 <= i; i-- {
		owned[i].Close()
	}
}

// Dials one QUIC connection to addr through the tunnel. It admits the host as
// the HTTPS leg's DialTLSContext does (admitTls), resolves it through the
// tunnel's own resolver to the same public answers of the request's family
// (IPv4 unless its grant says 6), and opens a UDP socket of the tunnel's stack
// in that family alone, connected to the first of them, on the URL's port: a
// URL probe's target is confined to 443 by its grant
// (ProviderUrlProbeContext), as the HTTPS leg's is. The TLS handshake
// verifies the chain by WebPKI and the same pins. The request's trace sees
// the target's DNS, then the handshake as both the connection and the TLS
// interval: QUIC sets up its transport and TLS together.
func (self *providerHttpTransport) dialQuic(ctx context.Context, addr string, quicConfig *quic.Config, own func(io.Closer) bool) (*quic.Conn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, &providerHttpStageError{stage: "policy", err: err}
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, &providerHttpStageError{stage: "policy", err: errors.New("provider HTTP/3 request has no valid port")}
	}
	tlsPins, _, err := self.admitTls(ctx, host)
	if err != nil {
		return nil, err
	}

	trace := httptrace.ContextClientTrace(ctx)
	resolveCtx, dialTrace := traceProviderHttpDial(ctx, "udp", addr)
	addrs, err := self.resolver.resolve(resolveCtx, addr, dialTrace)
	if stage := dialTrace.finish(err); err != nil {
		var staged *providerHttpStageError
		if errors.As(err, &staged) {
			return nil, err
		}
		return nil, &providerHttpStageError{stage: stage, err: err}
	}
	if len(addrs) == 0 {
		return nil, &providerHttpStageError{stage: "dial_dns", err: errors.New("provider URL resolution returned no address")}
	}
	remote := netip.AddrPortFrom(addrs[0], uint16(port))
	network := providerUrlDialNetwork(ctx, "udp4")

	// One handshake's own UDP socket of the tunnel's stack, connected to the
	// site, and the QUIC transport over it, both owned by the request.
	open := func() (*quic.Transport, *providerH3PacketConn, error) {
		raw, err := self.dialPacket(ctx, network, remote.String())
		if err != nil {
			return nil, nil, &providerHttpStageError{stage: "dial_dns_or_socket", err: err}
		}
		packetConn := &providerH3PacketConn{Conn: raw, remote: net.UDPAddrFromAddrPort(remote)}
		if !own(packetConn) {
			return nil, nil, net.ErrClosed
		}
		transport := &quic.Transport{Conn: packetConn}
		if !own(transport) {
			return nil, nil, net.ErrClosed
		}
		return transport, packetConn, nil
	}
	transport, packetConn, err := open()
	if err != nil {
		return nil, err
	}

	tlsConfig := PinnedTlsConfigForHost(tlsPins, host)
	tlsConfig.NextProtos = []string{http3.NextProtoH3}
	if trace != nil && trace.ConnectStart != nil {
		trace.ConnectStart("udp", remote.String())
	}
	if trace != nil && trace.TLSHandshakeStart != nil {
		trace.TLSHandshakeStart()
	}
	conn, err := transport.Dial(ctx, packetConn.remote, tlsConfig, quicConfig)
	if err != nil && providerQuicHandshakeUnanswered(ctx, err, packetConn.received.Load()) {
		// A first handshake that heard nothing back within its own bound is
		// made once more, on a fresh socket, so from a new source port, before
		// the leg reports silence: some QUIC edges lose a handshake now and
		// then on one path while the next one gets through (Cloudflare over
		// IPv6 left 11 of 535 unanswered from one vantage on 2026-11-11, with
		// none of 1494 over IPv4 and every TCP connect to the same addresses
		// answered). A provider path that carries no UDP is silent twice. The
		// retry has what is left of the leg's deadline, and the trace sees one
		// handshake that took both attempts.
		if retryTransport, retryPacketConn, retryErr := open(); retryErr == nil {
			transport.Close()
			packetConn.Close()
			conn, err = retryTransport.Dial(ctx, retryPacketConn.remote, tlsConfig, quicConfig)
			transport, packetConn = retryTransport, retryPacketConn
			observeProviderQuicHandshakeRetry(ctx, err, retryPacketConn.received.Load())
		}
	}
	var state tls.ConnectionState
	if conn != nil {
		state = conn.ConnectionState().TLS
	}
	if trace != nil && trace.TLSHandshakeDone != nil {
		trace.TLSHandshakeDone(state, err)
	}
	if trace != nil && trace.ConnectDone != nil {
		trace.ConnectDone("udp", remote.String(), err)
	}
	if err != nil {
		if stage := providerQuicDialStage(err, packetConn.received.Load()); stage != "" {
			return nil, &providerHttpStageError{stage: stage, err: err}
		}
		return nil, err
	}
	return conn, nil
}

// Whether a failed handshake is one the site never answered within the
// handshake's own bound, with time left on the request for another: not one
// datagram back, and a QUIC handshake or idle timeout rather than the
// request's own end.
func providerQuicHandshakeUnanswered(ctx context.Context, err error, received int64) bool {
	if received != 0 || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var handshakeTimeout *quic.HandshakeTimeoutError
	var idleTimeout *quic.IdleTimeoutError
	return errors.As(err, &handshakeTimeout) || errors.As(err, &idleTimeout)
}

// The stage a failed QUIC handshake reports, from the error and how many
// datagrams came back. Silence -- not one datagram back before the handshake
// timed out or ran out of its request's deadline -- is dial_quic: a provider
// path that does not carry UDP ends there, and nothing else does. Once the
// peer answered at all, the provider carried UDP to it: a TLS alert or a
// certificate that does not authenticate is tls, as on the HTTPS leg (its
// error still unwraps to the cause, the alert from 0x128 handshake_failure to
// 0x178 no_application_protocol), and any other refusal, reset or stall after
// an answer is quic. A canceled dial keeps its own classification.
func providerQuicDialStage(err error, received int64) string {
	var handshakeTimeout *quic.HandshakeTimeoutError
	var idleTimeout *quic.IdleTimeoutError
	var transportErr *quic.TransportError
	switch {
	case errors.Is(err, context.Canceled):
		return ""
	case errors.As(err, &transportErr) && transportErr.ErrorCode.IsCryptoError():
		return "tls"
	case received == 0 && (errors.As(err, &handshakeTimeout) || errors.As(err, &idleTimeout) || errors.Is(err, context.DeadlineExceeded)):
		return "dial_quic"
	default:
		return "quic"
	}
}

// A connected UDP socket of the tunnel's stack as the net.PacketConn quic-go
// reads and writes: every datagram goes to, and comes only from, the one
// address it was connected to. It counts the datagrams that came back, which
// is what tells a silent path from one the site answered.
type providerH3PacketConn struct {
	net.Conn
	remote   *net.UDPAddr
	received atomic.Int64
}

func (self *providerH3PacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := self.Conn.Read(p)
	if 0 < n {
		self.received.Add(1)
	}
	return n, self.remote, err
}

func (self *providerH3PacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return self.Conn.Write(p)
}

// quic-go asks for larger socket buffers. The tun sizes its UDP endpoints
// itself (connect.TunSettings), so the request is accepted as it stands.
func (self *providerH3PacketConn) SetReadBuffer(int) error  { return nil }
func (self *providerH3PacketConn) SetWriteBuffer(int) error { return nil }

var _ net.PacketConn = (*providerH3PacketConn)(nil)
