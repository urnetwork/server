package nginx

// The response side of a front request (owner: s-http). Every handler of the
// front passes the request on with a responseWriter over the server's own,
// which at the response head:
//
//   - adds what the request's location adds (response_headers.go);
//   - closes the client connection while the front drains (D12):
//     Connection: close on h1, which Go's h2 server turns into a GOAWAY. h3
//     gets none, since the field is illegal there; connect sends the h3
//     GOAWAY itself;
//   - gzips as nginx's `gzip on` with its defaults does: text/html the
//     upstream didn't encode, for statuses 200, 403 and 404, of unknown
//     length or at least HttpSettings.GzipMinLength bytes, to an HTTP/1.1 or
//     later client whose Accept-Encoding offers gzip and whose request has
//     no Via, never for HEAD. The Content-Length and Accept-Ranges go, a
//     strong ETag turns weak, and no Vary is added;
//   - records the status, the time of the head and the bytes for the
//     metrics and the access log.
//
// A writer over an h1 server's, which can hijack, hijacks too. The hijacked
// connection holds the request's limit slot until it closes, so an upgraded
// connection keeps its concurrency slot for the life of its tunnel, and the
// front tracks it so Close can cut it.

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Lets a forwarder name the block that answered, for the metrics and the
// access log (D17). The front's response writers implement it; a forwarder
// calls it through a type assertion of the writer it was given.
type upstreamBlockRecorder interface {
	recordUpstreamBlock(blockName string)
}

// A limit slot (frontLimits.acquireRequest) held by a request, and once the
// request hijacks its connection, by the connection: released when both have
// let go. Safe for concurrent use.
type responseSlot struct {
	release   func()
	holdCount atomic.Int32
}

// A slot held once, by the request.
func newResponseSlot(release func()) *responseSlot {
	slot := &responseSlot{
		release: release,
	}
	slot.holdCount.Store(1)
	return slot
}

// Adds a holder.
func (self *responseSlot) acquireHold() {
	self.holdCount.Add(1)
}

// Lets go of one hold, releasing the slot with the last.
func (self *responseSlot) releaseHold() {
	if self.holdCount.Add(-1) == 0 {
		self.release()
	}
}

// One request's response. Its methods run on the request's handler
// goroutine, as an http.ResponseWriter's do. A hijacked connection's close,
// which can run elsewhere, touches only the slot and the front's tracking.
type responseState struct {
	front *Front
	w     http.ResponseWriter
	r     *http.Request
	// what the request's location adds, set by routing before the head is
	// written
	headers responseHeaders
	// the limit slot, once the request has one
	slot *responseSlot

	statusCode  int
	wroteHeader bool
	hijacked    bool
	finished    bool
	// when the head was written or the connection hijacked
	headerTime time.Time
	// the gzip stream over the body, while nginx's gzip applies
	gzipWriter *gzip.Writer
	// body bytes written to the client, compressed when gzipped
	bodyBytes int64
	// the block that answered, as the forwarder named it
	upstreamBlock string
}

// The state of a new response over w.
func newResponseState(front *Front, w http.ResponseWriter, r *http.Request) *responseState {
	return &responseState{
		front: front,
		w:     w,
		r:     r,
	}
}

// The writer to pass on: one that also hijacks when the server's writer
// does, so an h2 or h3 handler sees no Hijacker, as with Go's own writers.
func (self *responseState) writer() http.ResponseWriter {
	responseWriter := &responseWriter{
		state: self,
	}
	if _, ok := self.w.(http.Hijacker); ok {
		return &responseHijackWriter{
			responseWriter: responseWriter,
		}
	}
	return responseWriter
}

// Holds a limit slot for the request, released when the request ends or,
// after a hijack, when the connection closes, whichever is later.
func (self *responseState) holdSlot(release func()) {
	self.slot = newResponseSlot(release)
}

// Lets go of the request's hold on its slot, once its handler has returned.
func (self *responseState) releaseSlot() {
	if self.slot != nil {
		self.slot.releaseHold()
	}
}

// Writes the head of a final response, or passes an informational one
// through.
func (self *responseState) writeHeader(statusCode int) {
	if self.wroteHeader || self.hijacked || self.finished {
		return
	}
	if http.StatusContinue <= statusCode && statusCode < http.StatusOK && statusCode != http.StatusSwitchingProtocols {
		self.w.WriteHeader(statusCode)
		return
	}
	self.wroteHeader = true
	self.statusCode = statusCode
	self.headerTime = self.front.now()

	header := self.w.Header()
	self.headers.apply(header, statusCode)
	if self.front.Draining() && statusCode != http.StatusSwitchingProtocols {
		switch self.r.ProtoMajor {
		case 1, 2:
			header.Set("Connection", "close")
		}
	}
	if self.gzips(statusCode) {
		self.startGzip()
	}
	self.w.WriteHeader(statusCode)
}

// Writes body bytes, through gzip when it applies.
func (self *responseState) write(data []byte) (int, error) {
	if self.hijacked {
		return 0, http.ErrHijacked
	}
	if self.finished {
		return 0, http.ErrBodyNotAllowed
	}
	if !self.wroteHeader {
		self.writeHeader(http.StatusOK)
	}
	if self.gzipWriter != nil {
		return self.gzipWriter.Write(data)
	}
	return self.writeBody(data)
}

// Writes bytes to the client as they are, counting them.
func (self *responseState) writeBody(data []byte) (int, error) {
	n, err := self.w.Write(data)
	self.bodyBytes += int64(n)
	return n, err
}

// Sends what is buffered, through gzip's sync flush when it applies.
func (self *responseState) flush() error {
	if self.hijacked {
		return http.ErrHijacked
	}
	if self.finished {
		return nil
	}
	if !self.wroteHeader {
		self.writeHeader(http.StatusOK)
	}
	if self.gzipWriter != nil {
		if err := self.gzipWriter.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(self.w).Flush()
}

// Hijacks the client connection: the location's headers go into the header
// map first, for a forwarder that writes its 101 from it, and the connection
// returned holds the request's slot and is tracked until it closes.
func (self *responseState) hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := self.w.(http.Hijacker)
	if !ok || self.hijacked || self.finished {
		return nil, nil, http.ErrNotSupported
	}
	if !self.wroteHeader {
		self.headers.apply(self.w.Header(), http.StatusSwitchingProtocols)
	}
	conn, readWriter, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	self.hijacked = true
	if !self.wroteHeader {
		self.statusCode = http.StatusSwitchingProtocols
		self.headerTime = self.front.now()
	}
	hijackedConn := &responseHijackedConn{
		Conn:  conn,
		front: self.front,
		slot:  self.slot,
	}
	if self.slot != nil {
		self.slot.acquireHold()
	}
	self.front.trackHijackedConn(hijackedConn)
	return hijackedConn, readWriter, nil
}

// Completes a response whose handler returned normally: an empty 200 when
// nothing was written, and the end of the gzip stream.
func (self *responseState) finish() {
	if self.hijacked || self.finished {
		return
	}
	if !self.wroteHeader {
		self.writeHeader(http.StatusOK)
	}
	if self.gzipWriter != nil {
		self.gzipWriter.Close()
		self.front.gzipWriters.Put(self.gzipWriter)
		self.gzipWriter = nil
	}
	self.finished = true
}

// Whether nginx's gzip compresses this response (ngx_http_gzip_header_filter
// and ngx_http_gzip_ok with nginx's defaults).
func (self *responseState) gzips(statusCode int) bool {
	httpSettings := self.front.settings.Http
	if httpSettings.GzipLevel <= 0 {
		return false
	}
	switch statusCode {
	case http.StatusOK, http.StatusForbidden, http.StatusNotFound:
	default:
		return false
	}
	if self.r.Method == http.MethodHead || !self.r.ProtoAtLeast(1, 1) {
		return false
	}
	// gzip_proxied off
	if 0 < len(self.r.Header.Values("Via")) {
		return false
	}
	if !responseAcceptsGzip(self.r.Header.Get("Accept-Encoding")) {
		return false
	}
	header := self.w.Header()
	if header.Get("Content-Encoding") != "" {
		return false
	}
	mediaType, _, _ := strings.Cut(header.Get("Content-Type"), ";")
	if !strings.EqualFold(strings.TrimSpace(mediaType), "text/html") {
		return false
	}
	if contentLength := header.Get("Content-Length"); contentLength != "" {
		contentLengthBytes, err := strconv.ParseInt(contentLength, 10, 64)
		if err == nil && contentLengthBytes < int64(httpSettings.GzipMinLength) {
			return false
		}
	}
	return true
}

// Starts the gzip stream, changing the head as nginx's gzip filter does.
func (self *responseState) startGzip() {
	header := self.w.Header()
	header.Del("Content-Length")
	header.Del("Accept-Ranges")
	// ngx_http_weak_etag
	if etag := header.Get("ETag"); etag != "" {
		switch {
		case 2 < len(etag) && strings.HasPrefix(etag, "W/"):
		case strings.HasPrefix(etag, `"`):
			header.Set("ETag", "W/"+etag)
		default:
			header.Del("ETag")
		}
	}
	header.Set("Content-Encoding", "gzip")
	gzipWriter := self.front.gzipWriters.Get().(*gzip.Writer)
	gzipWriter.Reset(&responseBodyWriter{
		state: self,
	})
	self.gzipWriter = gzipWriter
}

// The client's writer under the gzip stream, counting what it sends.
type responseBodyWriter struct {
	state *responseState
}

var _ io.Writer = (*responseBodyWriter)(nil)

// See io.Writer.
func (self *responseBodyWriter) Write(data []byte) (int, error) {
	return self.state.writeBody(data)
}

// Whether an Accept-Encoding value offers gzip as nginx reads it
// (ngx_http_gzip_accept_encoding): a "gzip" token, in any case, at the start
// or after a ',' or ' ', followed by the end, a ',', or a ";q=" whose value
// isn't zero.
func responseAcceptsGzip(acceptEncoding string) bool {
	const gzipToken = "gzip"
	if len(acceptEncoding) < len(gzipToken) {
		return false
	}
	if strings.HasPrefix(acceptEncoding, gzipToken+",") {
		return true
	}
	// the first "gzip" in any ascii case that starts a token. "gzip" can't
	// overlap itself, so testing every offset finds what nginx's search
	// does.
	tokenStart := -1
	for i := 0; i+len(gzipToken) <= len(acceptEncoding); i += 1 {
		if !strings.EqualFold(acceptEncoding[i:i+len(gzipToken)], gzipToken) {
			continue
		}
		if i == 0 || acceptEncoding[i-1] == ',' || acceptEncoding[i-1] == ' ' {
			tokenStart = i
			break
		}
	}
	if tokenStart < 0 {
		return false
	}
	i := tokenStart + len(gzipToken)

	// after the token: the end, a ',', or parameters
	for ; i < len(acceptEncoding); i += 1 {
		switch acceptEncoding[i] {
		case ',':
			return true
		case ' ':
			continue
		case ';':
			i += 1
			// after ';': the end, or q=
			for ; i < len(acceptEncoding); i += 1 {
				switch acceptEncoding[i] {
				case ' ':
					continue
				case 'q', 'Q':
					i += 1
					if len(acceptEncoding) < i+2 || acceptEncoding[i] != '=' {
						return false
					}
					return responseGzipQuantity(acceptEncoding[i+1:]) != 0
				default:
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	return true
}

// nginx's reading of a q value (ngx_http_gzip_quantity), which the front
// only compares with zero: the integer part, 0 or 1, times 100, plus each
// decimal digit as a unit, at most three of them; 0 for anything malformed
// or above 100.
func responseGzipQuantity(value string) int {
	if len(value) == 0 {
		return 0
	}
	c := value[0]
	if c != '0' && c != '1' {
		return 0
	}
	quantity := int(c-'0') * 100
	if len(value) == 1 {
		return quantity
	}
	c = value[1]
	if c == ',' || c == ' ' {
		return quantity
	}
	if c != '.' {
		return 0
	}
	digitCount := 0
	for i := 2; i < len(value); i += 1 {
		c = value[i]
		if c == ',' || c == ' ' {
			break
		}
		if c < '0' || '9' < c {
			return 0
		}
		quantity += int(c - '0')
		digitCount += 1
	}
	if 100 < quantity || 3 < digitCount {
		return 0
	}
	return quantity
}

// The http.ResponseWriter the front's handlers pass on. Not safe for
// concurrent use, as no http.ResponseWriter is.
type responseWriter struct {
	state *responseState
}

var (
	_ http.ResponseWriter   = (*responseWriter)(nil)
	_ http.Flusher          = (*responseWriter)(nil)
	_ upstreamBlockRecorder = (*responseWriter)(nil)
)

// See http.ResponseWriter.
func (self *responseWriter) Header() http.Header {
	return self.state.w.Header()
}

// See http.ResponseWriter.
func (self *responseWriter) WriteHeader(statusCode int) {
	self.state.writeHeader(statusCode)
}

// See http.ResponseWriter.
func (self *responseWriter) Write(data []byte) (int, error) {
	return self.state.write(data)
}

// See http.Flusher.
func (self *responseWriter) Flush() {
	self.state.flush()
}

// The flush http.ResponseController calls, with its error.
func (self *responseWriter) FlushError() error {
	return self.state.flush()
}

// The server's writer, for http.ResponseController's deadlines and full
// duplex. Writes through it skip the front's response handling.
func (self *responseWriter) Unwrap() http.ResponseWriter {
	return self.state.w
}

// See upstreamBlockRecorder.
func (self *responseWriter) recordUpstreamBlock(blockName string) {
	self.state.upstreamBlock = blockName
}

// A responseWriter over a server writer that hijacks: an h1 connection's.
type responseHijackWriter struct {
	*responseWriter
}

var _ http.Hijacker = (*responseHijackWriter)(nil)

// See http.Hijacker.
func (self *responseHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return self.state.hijack()
}

// A hijacked client connection. Its first close lets go of the request's
// limit slot and ends the front's tracking of it. Safe for concurrent use as
// the connection it wraps is.
type responseHijackedConn struct {
	net.Conn
	front *Front
	// nil when the request held none
	slot      *responseSlot
	closeOnce sync.Once
}

// See net.Conn.
func (self *responseHijackedConn) Close() error {
	err := self.Conn.Close()
	self.closeOnce.Do(func() {
		self.front.untrackHijackedConn(self)
		if self.slot != nil {
			self.slot.releaseHold()
		}
	})
	return err
}
