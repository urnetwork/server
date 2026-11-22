package nginx

// Go's HTTP/2 server takes over the TLS connection after the handshake and
// clears ReadHeaderTimeout. The public listener bounds header assembly there,
// before a request reaches its handler. net/http still owns the handshake,
// frame validation, flow control and graceful shutdown.
//
// The listener also reports each handshake that completes, with what its
// ClientHello offered (TlsClientOffer), which the front's TCP config reads
// into a slot the handshake's context carries. net/http runs the handshake
// through the connection's HandshakeContext, which it finds by interface
// (go1.27 net/http server.go, conn.serve), and a nil return means the
// client's Finished was verified. crypto/tls's VerifyConnection is
// earlier: a TLS 1.3 server calls it once its own flight is flushed, before
// it reads anything of the client's (readClientCertificate), and a TLS 1.2
// server resuming a session calls it right after its ServerHello
// (doResumeHandshake). Counted there, a client that then refused the
// certificate, hung up or stalled would read as a completed handshake as
// well as a failed one.

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"time"

	"golang.org/x/net/http2"
)

// A TLS listener using the front's shared TLS configuration and the public
// server's header timeout. Zero or negative disables the additional bound.
// handshakeObserver, when set, is given the state of each connection whose
// handshake completes and its ClientHello's offer, once, on the goroutine
// that ran the handshake, before the connection is served. The offer is
// read by the front's TCP config (Front.TlsConfig); under another config it
// is the zero offer.
func NewTlsListener(
	listener net.Listener,
	config *tls.Config,
	headerTimeout time.Duration,
	handshakeObserver func(connectionState tls.ConnectionState, clientOffer TlsClientOffer),
) net.Listener {
	return &frontTlsListener{
		Listener:          listener,
		config:            config,
		headerTimeout:     headerTimeout,
		handshakeObserver: handshakeObserver,
	}
}

// Creates a TLS connection for net/http to handshake on its serving goroutine.
type frontTlsListener struct {
	net.Listener
	config            *tls.Config
	headerTimeout     time.Duration
	handshakeObserver func(connectionState tls.ConnectionState, clientOffer TlsClientOffer)
}

// Accepts without waiting for TLS or an HTTP/2 preface.
func (self *frontTlsListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &frontTlsConn{
		Conn:              tls.Server(conn, self.config),
		headerTimeout:     self.headerTimeout,
		handshakeObserver: self.handshakeObserver,
	}, nil
}

// Only HTTP/2's single frame reader uses the framing state. It skips data
// payloads and tracks the fixed nine-byte frame prefix (RFC 9113 §4.1).
// net/http's serving goroutine is the only caller of HandshakeContext.
type frontTlsConn struct {
	*tls.Conn
	// non-positive bounds no header block
	headerTimeout time.Duration
	// nil when nothing observes the handshake
	handshakeObserver func(connectionState tls.ConnectionState, clientOffer TlsClientOffer)
	// set once the completed handshake was observed
	handshakeObserved bool
	// the ClientHello's offer, which the TCP config's GetConfigForClient
	// writes through the handshake's context on this goroutine
	clientOffer TlsClientOffer
	h2          bool
	prefaceLeft int
	frameHeader [9]byte
	headerBytes int
	payloadLeft int
	headerBlock bool
	endHeaders  bool
	deadlineSet bool
}

// net/http finishes the handshake before starting its HTTP/2 frame reader. A
// handshake that completes is observed once, however often this is called,
// with the offer its ClientHello made.
func (self *frontTlsConn) HandshakeContext(ctx context.Context) error {
	observe := self.handshakeObserver != nil && !self.handshakeObserved
	if observe {
		ctx = withTlsClientOffer(ctx, &self.clientOffer)
	}
	if err := self.Conn.HandshakeContext(ctx); err != nil {
		return err
	}
	connectionState := self.ConnectionState()
	if observe {
		self.handshakeObserved = true
		self.handshakeObserver(connectionState, self.clientOffer)
	}
	self.h2 = 0 < self.headerTimeout && connectionState.NegotiatedProtocol == http2.NextProtoTLS
	self.prefaceLeft = len(http2.ClientPreface)
	return nil
}

// Examines only the decrypted bytes actually delivered to the HTTP/2 server.
func (self *frontTlsConn) Read(p []byte) (int, error) {
	n, err := self.Conn.Read(p)
	if self.h2 && n > 0 {
		self.observeFrames(p[:n])
	}
	return n, err
}

// Holds one absolute deadline through HEADERS and its CONTINUATION frames.
// A partial frame prefix is bounded until its type is known. DATA payloads
// remain subject to the request's own body timeout.
func (self *frontTlsConn) observeFrames(p []byte) {
	skip := min(len(p), self.prefaceLeft)
	self.prefaceLeft -= skip
	p = p[skip:]
	for len(p) > 0 {
		if self.payloadLeft == 0 {
			n := copy(self.frameHeader[self.headerBytes:], p)
			self.headerBytes += n
			p = p[n:]
			if self.headerBytes < len(self.frameHeader) {
				break
			}
			self.headerBytes = 0
			self.payloadLeft = int(binary.BigEndian.Uint32(self.frameHeader[:4]) >> 8)
			if self.isHeaderFrame() {
				self.headerBlock = true
				self.endHeaders = http2.Flags(self.frameHeader[4]).Has(http2.FlagHeadersEndHeaders)
			}
		}
		n := min(len(p), self.payloadLeft)
		self.payloadLeft -= n
		p = p[n:]
		if self.payloadLeft == 0 && self.endHeaders {
			self.endHeaders = false
			self.headerBlock = false
			self.setHeaderDeadline(false)
		} else if !self.headerBlock {
			self.setHeaderDeadline(false)
		}
	}
	prefixPending := self.headerBytes > 0 && (self.headerBytes < 4 || self.isHeaderFrame())
	self.setHeaderDeadline(self.headerBlock || prefixPending)
}

// The fourth byte of a frame prefix identifies the frame type.
func (self *frontTlsConn) isHeaderFrame() bool {
	frameType := http2.FrameType(self.frameHeader[3])
	return frameType == http2.FrameHeaders || frameType == http2.FrameContinuation
}

// Changes the socket timer only at a header boundary. Fragment progress
// keeps the original deadline and the next independent block gets a fresh one.
func (self *frontTlsConn) setHeaderDeadline(active bool) {
	if active == self.deadlineSet {
		return
	}
	self.deadlineSet = active
	var deadline time.Time
	if active {
		deadline = time.Now().Add(self.headerTimeout)
	}
	self.Conn.SetReadDeadline(deadline)
}
