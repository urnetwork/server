package nginx

// The response side of a front request (owner: s-http). Every handler of the
// front passes the request on with a responseWriter over the server's own,
// which at the response head:
//
//   - adds what the request's location adds (response_headers.go);
//   - closes the client connection while the front drains (D12), and after
//     the request nginx's keepalive_requests or keepalive_time makes its
//     connection's last (front_server.go): Connection: close on h1, which
//     Go's h2 server turns into a GOAWAY. h3 gets none, since the field is
//     illegal there; connect sends the h3 GOAWAY itself;
//   - gzips as nginx's `gzip on` with its defaults does: text/html the
//     upstream didn't encode, for statuses 200, 403 and 404, of unknown
//     length or at least HttpSettings.GzipMinLength bytes, to an HTTP/1.1 or
//     later client whose Accept-Encoding offers gzip and whose request has
//     no Via. The Content-Length and Accept-Ranges go, and the server adds
//     no length of its own, a strong ETag turns weak, and no Vary is added,
//     as gzip_vary is off. A HEAD is decided as a GET and gets the same
//     head, with nothing compressed: nginx marks a HEAD header-only in its
//     last header filter, after gzip's has run (ngx_http_header_filter,
//     ngx_http_v2_header_filter);
//   - bounds each write to the client by HttpSettings.SendTimeout, as
//     nginx's send_timeout bounds a write only while it is blocked: the
//     write deadline is armed just before each write, flush and
//     informational head, and cleared when the call returns. A response
//     idle between two writes has none, which matters on h2, where Go's
//     server resets a stream whose deadline passes even with no write
//     pending, and a client that stops reading has the write cut. On h2 a
//     frame write blocked on the socket itself is cut by the server's write
//     byte timeout instead (Front.ConfigureServer), since a stream's reset
//     waits behind it. The deadline is armed again as the handler returns,
//     for what the server writes then (the rest of the buffered body, and
//     on h1 the end of the response); Go's h1 server clears it after the
//     request, and its h2 server stops it when the stream closes. On h1 the
//     first read of the body arms it too, for the 100 Continue Go's server
//     writes then. A hijack clears it, so the tunnel's owner starts with
//     none;
//   - on h1, has a response that ends its connection while the request's
//     body is unread end it with Go's half-close and its wait, so a client
//     still sending the body reads the answer before the close resets the
//     connection (finish);
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
	"errors"
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
	release func()
	// a connect upgrade's count in the concentration gauges
	// (frontConcentration), released with the slot; nil for any other
	// request. Set on the request's goroutine before any hijack, so before
	// the count of holders can reach zero elsewhere
	concentrationRelease func()
	holdCount            atomic.Int32
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
		if self.concentrationRelease != nil {
			self.concentrationRelease()
		}
	}
}

// One request's response. Its methods run on the request's handler
// goroutine, as an http.ResponseWriter's do. A hijacked connection's close,
// which can run elsewhere, touches only the slot and the front's tracking,
// and a read of an h1 body, which can too, only the write deadline and the
// body's own state.
type responseState struct {
	front *Front
	w     http.ResponseWriter
	r     *http.Request
	// over w, for its write deadline and its flush
	controller *http.ResponseController
	// what the request's location adds, set by routing before the head is
	// written
	headers responseHeaders
	// the request is its connection's last (Front.lastRequestOnConnection),
	// so its head closes the connection, as the drain's heads do
	closesConnection bool
	// the h1 request body, as the handlers read it; nil on h2 and h3, and
	// for a request without one
	requestBody *responseRequestBody
	// on h1, the connection ends after the response: its head says
	// Connection: close, the client asked to close, or keepalives are off.
	// Set with the head
	endsConnection bool
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
		front:      front,
		w:          w,
		r:          r,
		controller: http.NewResponseController(w),
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

// Holds a connect upgrade's count in the concentration gauges with the
// request's slot, released as the slot is: when the request ends or, after a
// hijack, when the connection closes, whichever is later. Called on the
// request's goroutine before any hijack. A request without a slot gets one
// that holds only the count.
func (self *responseState) holdConcentration(release func()) {
	if self.slot == nil {
		self.slot = newResponseSlot(func() {})
	}
	self.slot.concentrationRelease = release
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
		// the server writes an informational head at once
		self.armWriteDeadline()
		self.w.WriteHeader(statusCode)
		self.clearWriteDeadline()
		return
	}
	self.wroteHeader = true
	self.statusCode = statusCode
	self.headerTime = self.front.now()
	// a final head is written with the first body bytes, or after the
	// handler returns, so it takes no deadline of its own

	header := self.w.Header()
	self.headers.apply(header, statusCode)
	if (self.front.Draining() || self.closesConnection) && statusCode != http.StatusSwitchingProtocols {
		switch self.r.ProtoMajor {
		case 1, 2:
			header.Set("Connection", "close")
		}
	}
	if self.r.ProtoMajor == 1 {
		// as Go's h1 server decides to close after the response; r.Close is
		// a Connection: close, or HTTP/1.0 without keep-alive
		self.endsConnection = header.Get("Connection") == "close" || self.r.Close || self.front.settings.Http.KeepaliveTimeout <= 0
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
	self.armWriteDeadline()
	n, err := self.w.Write(data)
	self.clearWriteDeadline()
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
	self.armWriteDeadline()
	err := self.controller.Flush()
	self.clearWriteDeadline()
	return err
}

// Arms the client write deadline at SendTimeout from now, for a write about
// to start. A writer that can't take a deadline is left as it is. The
// deadline is real time, as the connection's are.
func (self *responseState) armWriteDeadline() {
	sendTimeout := self.front.settings.Http.SendTimeout
	if sendTimeout <= 0 {
		return
	}
	self.controller.SetWriteDeadline(time.Now().Add(sendTimeout))
}

// Clears the client write deadline once a write has returned, so the
// response has none while it waits for its next write.
func (self *responseState) clearWriteDeadline() {
	if self.front.settings.Http.SendTimeout <= 0 {
		return
	}
	self.controller.SetWriteDeadline(time.Time{})
}

// On h1, wraps a request body so the response knows whether it was read
// whole (finish), and so its first read arms the write deadline: Go's server
// writes the 100 Continue a request may expect then, and only before any
// response write, so the deadline stays armed after it. h1 allows that,
// since a connection's deadline fails only a write pending or started once
// it passes, and every later write arms its own. A bodiless request needs
// neither.
func (self *responseState) start() {
	if self.r.ProtoMajor != 1 || self.r.Body == nil || self.r.Body == http.NoBody {
		return
	}
	self.requestBody = &responseRequestBody{
		ReadCloser: self.r.Body,
		state:      self,
	}
	self.r.Body = self.requestBody
}

// Whether the h1 request has a body its handlers left unread, wholly or in
// part. False on h2 and h3, and for a request without a body.
func (self *responseState) requestBodyUnread() bool {
	return self.requestBody != nil && !self.requestBody.readWhole.Load()
}

// An h1 request body whose first read arms the client write deadline, for
// the 100 Continue, and which notes when it has been read whole
// (responseState.start). Read by one goroutine at a time, as a request body
// is; that goroutine may differ from the handler's, so the read touches only
// the deadline and the body's own state, and whether it was read whole is
// atomic, for the handler to look at while a reader may still run.
type responseRequestBody struct {
	io.ReadCloser
	state       *responseState
	readStarted bool
	// a read has returned io.EOF
	readWhole atomic.Bool
}

// See io.Reader.
func (self *responseRequestBody) Read(data []byte) (int, error) {
	if !self.readStarted {
		self.readStarted = true
		self.state.armWriteDeadline()
	}
	n, err := self.ReadCloser.Read(data)
	if err == io.EOF {
		self.readWhole.Store(true)
	}
	return n, err
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
	if 0 < self.front.settings.Http.SendTimeout {
		// the tunnel's owner sets its own
		conn.SetWriteDeadline(time.Time{})
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
	if !self.front.trackHijackedConn(hijackedConn) {
		// the front is cutting what outlived its drain
		hijackedConn.Close()
		return nil, nil, errResponseFrontCut
	}
	return hijackedConn, readWriter, nil
}

// A hijack after the front's cut started.
var errResponseFrontCut = errors.New("lbalt front: closed")

// Completes a response whose handler returned normally: an empty 200 when
// nothing was written, and the end of the gzip stream. The server writes
// the rest once the handler returns (what is still buffered, and on h1 the
// end of the response), so the write deadline is armed for that, fresh
// however long the response was idle. An h2 stream that has already closed,
// whose request context is done, gets none, since a deadline set on a
// closed h2 stream would still fire later, with a stray reset.
//
// On h1, a response that ends its connection while the request's body is
// unread is marked as one whose body ran over a limit: http.MaxBytesReader
// at 0 bytes, over a byte of the front's own rather than the socket, so
// nothing waits on the client. Go's server ends such a connection with a
// half-close, then rstAvoidanceDelay, 500 ms, before the close
// (closeWriteAndWait), so a client still sending its body has read the
// answer by the time the close resets the connection. Unmarked, it does that
// only when more than 256 KiB of the body is left, a declared body's rest
// unread or a chunked one's once 256 KiB of it is read, or when that read
// fails. When the client itself asked to close, it reads none of a declared
// body and closes at once, and a client that is still writing, such as Go's
// own, can report the reset in place of the answer; and a smaller body it
// reads whole after the answer, then closes at once over a pipelined request
// still arriving. Only the server's own writer can be marked. h2 and h3 need
// none: an h2 answer ends its stream and resets the request's side with
// NO_ERROR while the server reads on, and the vendored http3 stops the
// request's side with H3_NO_ERROR, so neither closes a connection over an
// unread body.
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
	if self.endsConnection && self.requestBodyUnread() {
		limitReader := http.MaxBytesReader(self.w, io.NopCloser(strings.NewReader("x")), 0)
		limitReader.Read(make([]byte, 1))
	}
	if self.r.ProtoMajor != 2 || self.r.Context().Err() == nil {
		self.armWriteDeadline()
	}
}

// Whether nginx's gzip compresses this response (ngx_http_gzip_header_filter
// and ngx_http_gzip_ok with nginx's defaults). A HEAD is decided as a GET,
// since nginx's header_only, which the filter checks, is set only later.
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
	// gzip_http_version 1.1
	if !self.r.ProtoAtLeast(1, 1) {
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
	// the part of the Content-Type nginx's gzip_types looks up: the whole
	// value, but for the bytes before a ';' that spaces and a charset=
	// parameter follow (ngx_http_upstream_copy_content_type), lowercased
	// and untrimmed (ngx_http_test_content_type)
	contentTypeMediaType := func(contentType string) string {
		for i := 0; i < len(contentType); i += 1 {
			if contentType[i] != ';' {
				continue
			}
			parameterStart := i + 1
			for parameterStart < len(contentType) && contentType[parameterStart] == ' ' {
				parameterStart += 1
			}
			if parameterStart == len(contentType) {
				return contentType
			}
			if parameter := contentType[parameterStart:]; len("charset=") <= len(parameter) && strings.EqualFold(parameter[:len("charset=")], "charset=") {
				return contentType[:i]
			}
			// nginx's scan goes on past the parameter's first byte
			i = parameterStart
		}
		return contentType
	}
	if !strings.EqualFold(contentTypeMediaType(header.Get("Content-Type")), "text/html") {
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

// Starts the gzip stream, changing the head as nginx's gzip filter does. The
// Content-Length is set to nil, which Go's h1, h2 and h3 servers take as one
// the handler suppressed, so none of them adds its own: on a GET, from the
// compressed bytes of a handler that returns before the head is written,
// where nginx sends no length; on a HEAD, from body bytes the handler wrote
// anyway, or on h3 as 0. A HEAD gets the head alone, with no body to
// compress.
func (self *responseState) startGzip() {
	header := self.w.Header()
	header["Content-Length"] = nil
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
	if self.r.Method == http.MethodHead {
		return
	}
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

// The server's writer, for http.ResponseController's read deadline and full
// duplex. Writes through it skip the front's response handling, and the
// write deadline is the front's: each write arms and clears its own.
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
