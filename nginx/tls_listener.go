package nginx

// Go's HTTP/2 server takes over the TLS connection after the handshake and
// clears ReadHeaderTimeout. The public listener bounds header assembly there,
// before a request reaches its handler. net/http still owns the handshake,
// frame validation, flow control and graceful shutdown.

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
func NewTlsListener(listener net.Listener, config *tls.Config, headerTimeout time.Duration) net.Listener {
	if headerTimeout <= 0 {
		return tls.NewListener(listener, config)
	}
	return &frontTlsListener{Listener: listener, config: config, headerTimeout: headerTimeout}
}

// Creates a TLS connection for net/http to handshake on its serving goroutine.
type frontTlsListener struct {
	net.Listener
	config        *tls.Config
	headerTimeout time.Duration
}

// Accepts without waiting for TLS or an HTTP/2 preface.
func (self *frontTlsListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &frontTlsConn{Conn: tls.Server(conn, self.config), headerTimeout: self.headerTimeout}, nil
}

// Only HTTP/2's single frame reader uses the framing state. It skips data
// payloads and tracks the fixed nine-byte frame prefix (RFC 9113 §4.1).
type frontTlsConn struct {
	*tls.Conn
	headerTimeout time.Duration
	h2            bool
	prefaceLeft   int
	frameHeader   [9]byte
	headerBytes   int
	payloadLeft   int
	headerBlock   bool
	endHeaders    bool
	deadlineSet   bool
}

// net/http finishes the handshake before starting its HTTP/2 frame reader.
func (self *frontTlsConn) HandshakeContext(ctx context.Context) error {
	if err := self.Conn.HandshakeContext(ctx); err != nil {
		return err
	}
	self.h2 = self.ConnectionState().NegotiatedProtocol == http2.NextProtoTLS
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
