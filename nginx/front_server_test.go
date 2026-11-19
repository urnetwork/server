package nginx

// Tests of the front's public TCP servers (front_server.go): the fields
// ConfigureServer sets, nginx's keepalive_requests and keepalive_time over
// real h1 and h2 servers, and send_timeout on an h2 connection whose client
// stops reading. The h2 tests read frames directly, so the GOAWAY and the
// connection's close are seen as the client sees them. The keepalive
// requests go to the lb status route on an lb name, which no limit charges.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// A real TLS server of the test front's TCP 443 handler, as
// newTestConfiguredServerOf builds it.
func newTestConfiguredServer(t *testing.T, testFront *testFront, enableHttp2 bool) *httptest.Server {
	t.Helper()
	return newTestConfiguredServerOf(t, testFront.front, enableHttp2)
}

// A real TLS server of front's TCP 443 handler for eth1, configured by
// ConfigureServer as connect configures its public servers, h2 when
// enableHttp2 is set. It closes when the test ends.
func newTestConfiguredServerOf(t *testing.T, front *Front, enableHttp2 bool) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(front.HttpsHandler("eth1"))
	front.ConfigureServer(server.Config)
	server.EnableHTTP2 = enableHttp2
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// A minimal h1 client over one TLS connection, so every request it sends
// goes over that connection, until the server closes it.
type testServerH1Client struct {
	t      *testing.T
	conn   *tls.Conn
	reader *bufio.Reader
}

// Dials address over TLS for HTTP/1.1.
func dialTestServerH1Client(t *testing.T, address string) *testServerH1Client {
	t.Helper()
	conn, err := tls.Dial("tcp", address, &tls.Config{
		// the httptest server's certificate names no served name
		InsecureSkipVerify: true,
		NextProtos:         []string{"http/1.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
	})
	// bounds a broken test, never a passing one
	conn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
	return &testServerH1Client{
		t:      t,
		conn:   conn,
		reader: bufio.NewReader(conn),
	}
}

// GETs the lb status route on the connection and reads the response whole.
func (self *testServerH1Client) getLbStatus() *http.Response {
	self.t.Helper()
	fmt.Fprintf(self.conn, "GET /hidden-example/status HTTP/1.1\r\nHost: test-lb.service.example\r\n\r\n")
	response, err := http.ReadResponse(self.reader, nil)
	if err != nil {
		self.t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != testLbStatusBody {
		self.t.Fatalf("lb status = %d %q, %v", response.StatusCode, body, err)
	}
	return response
}

// Whether the server has closed the connection: the next read finds its end.
func (self *testServerH1Client) closedByServer() bool {
	self.t.Helper()
	_, err := self.reader.ReadByte()
	return errors.Is(err, io.EOF)
}

// A minimal h2 client over one TLS connection, which reads the server's
// frames itself, so a test sees the GOAWAY and the connection's close.
type testServerH2Client struct {
	t            *testing.T
	conn         *tls.Conn
	framer       *http2.Framer
	encoder      *hpack.Encoder
	headerBlock  *bytes.Buffer
	nextStreamId uint32
	// the GOAWAY the server sent, once it has
	goAwayReceived     bool
	goAwayLastStreamId uint32
	goAwayErrCode      http2.ErrCode
}

// Dials address over TLS, negotiating h2, and opens the connection.
func dialTestServerH2Client(t *testing.T, address string) *testServerH2Client {
	t.Helper()
	conn, err := tls.Dial("tcp", address, &tls.Config{
		// the httptest server's certificate names no served name
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
	})
	// bounds a broken test, never a passing one
	conn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
	if protocol := conn.ConnectionState().NegotiatedProtocol; protocol != "h2" {
		t.Fatalf("negotiated %q, want h2", protocol)
	}
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	headerBlock := &bytes.Buffer{}
	client := &testServerH2Client{
		t:            t,
		conn:         conn,
		framer:       http2.NewFramer(conn, conn),
		encoder:      hpack.NewEncoder(headerBlock),
		headerBlock:  headerBlock,
		nextStreamId: 1,
	}
	client.framer.ReadMetaHeaders = hpack.NewDecoder(4*1024, nil)
	if err := client.framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	return client
}

// Reads the next frame, answering the server's settings and pings, keeping
// the connection's flow-control window open and noting a GOAWAY. Nil once
// the server has closed the connection.
func (self *testServerH2Client) readFrame() http2.Frame {
	self.t.Helper()
	frame, err := self.framer.ReadFrame()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		self.t.Fatalf("read a frame: %v", err)
	}
	switch frame := frame.(type) {
	case *http2.SettingsFrame:
		if !frame.IsAck() {
			self.framer.WriteSettingsAck()
		}
	case *http2.PingFrame:
		if !frame.IsAck() {
			self.framer.WritePing(true, frame.Data)
		}
	case *http2.DataFrame:
		if dataBytes := len(frame.Data()); 0 < dataBytes {
			self.framer.WriteWindowUpdate(0, uint32(dataBytes))
		}
	case *http2.GoAwayFrame:
		self.goAwayReceived = true
		self.goAwayLastStreamId = frame.LastStreamID
		self.goAwayErrCode = frame.ErrCode
	}
	return frame
}

// GETs path at authority on a new stream and reads frames until its response
// ends. The response's status.
func (self *testServerH2Client) get(authority string, path string) string {
	self.t.Helper()
	streamId := self.startGet(authority, path)
	status := ""
	for {
		switch frame := self.readFrame().(type) {
		case nil:
			self.t.Fatalf("the connection closed before stream %d's response ended", streamId)
		case *http2.MetaHeadersFrame:
			if frame.StreamID == streamId {
				status = frame.PseudoValue("status")
				if frame.StreamEnded() {
					return status
				}
			}
		case *http2.DataFrame:
			if frame.StreamID == streamId && frame.StreamEnded() {
				return status
			}
		case *http2.RSTStreamFrame:
			if frame.StreamID == streamId {
				self.t.Fatalf("stream %d was reset: %s", streamId, frame.ErrCode)
			}
		}
	}
}

// Reads frames until the response head of streamId. Its status.
func (self *testServerH2Client) readResponseHead(streamId uint32) string {
	self.t.Helper()
	for {
		switch frame := self.readFrame().(type) {
		case nil:
			self.t.Fatalf("the connection closed before stream %d's response head", streamId)
		case *http2.MetaHeadersFrame:
			if frame.StreamID == streamId {
				return frame.PseudoValue("status")
			}
		case *http2.RSTStreamFrame:
			if frame.StreamID == streamId {
				self.t.Fatalf("stream %d was reset: %s", streamId, frame.ErrCode)
			}
		}
	}
}

// Opens a new stream that GETs path at authority, with extraFields after
// the pseudo-headers, sending no body. The stream's id.
func (self *testServerH2Client) startGet(authority string, path string, extraFields ...hpack.HeaderField) uint32 {
	self.t.Helper()
	streamId := self.nextStreamId
	self.nextStreamId += 2
	self.headerBlock.Reset()
	fields := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: authority},
		{Name: ":path", Value: path},
	}
	for _, field := range append(fields, extraFields...) {
		self.encoder.WriteField(field)
	}
	err := self.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamId,
		BlockFragment: self.headerBlock.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	})
	if err != nil {
		self.t.Fatalf("stream %d: %v", streamId, err)
	}
	return streamId
}

// Reads frames until the server closes the connection, and reports whether
// it sent a GOAWAY first, and its last stream and code.
func (self *testServerH2Client) readToClose() (goAwayReceived bool, lastStreamId uint32, errCode http2.ErrCode) {
	self.t.Helper()
	for self.readFrame() != nil {
	}
	return self.goAwayReceived, self.goAwayLastStreamId, self.goAwayErrCode
}

// The id of an h2 client's nth stream, from 1.
func testServerH2StreamId(n int) uint32 {
	return uint32(2*n - 1)
}

// A connection's flow-control window when it opens, before any
// WINDOW_UPDATE (RFC 9113, section 6.9.2).
const testServerH2InitialWindowBytes = 65535

// Opens the client's flow-control windows, the connection's and every
// stream's, to the largest h2 allows, far beyond any socket buffer, so the
// server's writes to a client that stops reading block on the socket, never
// on flow control.
func (self *testServerH2Client) openWindows() {
	self.t.Helper()
	err := self.framer.WriteSettings(http2.Setting{
		ID:  http2.SettingInitialWindowSize,
		Val: math.MaxInt32,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	if err := self.framer.WriteWindowUpdate(0, math.MaxInt32-testServerH2InitialWindowBytes); err != nil {
		self.t.Fatal(err)
	}
}

// A listener that reports each connection it accepts, wrapped so a test can
// stall it.
type testServerStallingListener struct {
	net.Listener
	accepted chan *testServerStallingConn
}

// Accepts the next connection and reports it, when accepted has room.
func (self *testServerStallingListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	stallingConn := &testServerStallingConn{
		Conn:   conn,
		closed: make(chan struct{}),
	}
	select {
	case self.accepted <- stallingConn:
	default:
	}
	return stallingConn, nil
}

// A server connection whose writes, once it is stalled, wait out their
// deadline and fail with its error, as a write does to a client that stopped
// reading once the socket's buffers are full, whatever the kernel's buffer
// sizes; a write with no deadline waits for the close. Once a write has
// failed on its deadline, every later write fails at once, as on connect's
// public sockets (connect/front_http.go, frontTrackedConn), so crypto/tls's
// close_notify doesn't wait out a deadline of its own. Safe for concurrent
// use as a net.Conn is.
type testServerStallingConn struct {
	net.Conn
	stalled       atomic.Bool
	writeTimedOut atomic.Bool
	closed        chan struct{}
	closeOnce     sync.Once

	stateLock     sync.Mutex
	writeDeadline time.Time
}

// See net.Conn.
func (self *testServerStallingConn) SetDeadline(deadline time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeDeadline = deadline
	}()
	return self.Conn.SetDeadline(deadline)
}

// See net.Conn.
func (self *testServerStallingConn) SetWriteDeadline(deadline time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeDeadline = deadline
	}()
	return self.Conn.SetWriteDeadline(deadline)
}

// Writes p, or once stalled waits for the write deadline, or the close, and
// fails. After a write failed on its deadline, fails at once.
func (self *testServerStallingConn) Write(p []byte) (int, error) {
	if self.writeTimedOut.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	if !self.stalled.Load() {
		n, err := self.Conn.Write(p)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			self.writeTimedOut.Store(true)
		}
		return n, err
	}
	writeDeadline := func() time.Time {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.writeDeadline
	}()
	var expired <-chan time.Time
	if !writeDeadline.IsZero() {
		expired = time.After(time.Until(writeDeadline))
	}
	select {
	case <-expired:
		self.writeTimedOut.Store(true)
		return 0, os.ErrDeadlineExceeded
	case <-self.closed:
		return 0, net.ErrClosed
	}
}

// See net.Conn.
func (self *testServerStallingConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return self.Conn.Close()
}

// ConfigureServer sets every field that derives from HttpSettings and
// nothing the caller owns: Handler, ErrorLog and BaseContext stay, no
// TLSConfig, ReadTimeout or WriteTimeout is set, a ConnContext already set
// still runs ahead of the front's, and an HTTP2 config the caller set keeps
// its other fields without being written. The h2 config carries the stream
// limit and the write byte timeout, SendTimeout, and is set when either is.
// Non-positive settings leave Go's defaults.
func TestFrontConfigureServerSetsTheHttpSettingsFields(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	httpSettings := testFront.front.settings.Http
	type testContextKey struct{}
	handler := &testConnectHandler{}
	errorLog := log.New(io.Discard, "", 0)
	callerHttp2Config := &http.HTTP2Config{
		MaxReceiveBufferPerConnection: 1024 * 1024,
	}
	httpServer := &http.Server{
		Handler:  handler,
		ErrorLog: errorLog,
		BaseContext: func(net.Listener) context.Context {
			return context.WithValue(context.Background(), testContextKey{}, "base")
		},
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, testContextKey{}, "caller")
		},
		HTTP2: callerHttp2Config,
	}
	testFront.front.ConfigureServer(httpServer)

	if httpServer.ReadHeaderTimeout != httpSettings.ClientHeaderTimeout {
		t.Errorf("ReadHeaderTimeout %s, want ClientHeaderTimeout %s", httpServer.ReadHeaderTimeout, httpSettings.ClientHeaderTimeout)
	}
	if httpServer.IdleTimeout != httpSettings.KeepaliveTimeout {
		t.Errorf("IdleTimeout %s, want KeepaliveTimeout %s", httpServer.IdleTimeout, httpSettings.KeepaliveTimeout)
	}
	// twice nginx's buffers, as LBALT-IMPL.md §7 decides
	if httpSettings.ServerHeaderBytesMultiple != 2 {
		t.Errorf("ServerHeaderBytesMultiple %d, want 2", httpSettings.ServerHeaderBytesMultiple)
	}
	if want := 2 * httpSettings.MaxRequestHeadersTotalBytes; httpServer.MaxHeaderBytes != want {
		t.Errorf("MaxHeaderBytes %d, want twice MaxRequestHeadersTotalBytes, %d", httpServer.MaxHeaderBytes, want)
	}
	if httpServer.HTTP2 == nil ||
		httpServer.HTTP2.MaxConcurrentStreams != httpSettings.Http2MaxConcurrentStreams ||
		httpServer.HTTP2.WriteByteTimeout != httpSettings.SendTimeout ||
		httpServer.HTTP2.MaxReceiveBufferPerConnection != 1024*1024 {
		t.Errorf("HTTP2 %+v, want the stream limit %d and the write byte timeout %s with the caller's receive buffer", httpServer.HTTP2, httpSettings.Http2MaxConcurrentStreams, httpSettings.SendTimeout)
	}
	if callerHttp2Config.MaxConcurrentStreams != 0 || callerHttp2Config.WriteByteTimeout != 0 {
		t.Error("the caller's HTTP2 config was written")
	}
	if !httpServer.DisableGeneralOptionsHandler {
		t.Error("OPTIONS * would not reach the front")
	}
	if httpServer.Handler != handler || httpServer.ErrorLog != errorLog || httpServer.BaseContext(nil).Value(testContextKey{}) != "base" {
		t.Error("the caller's Handler, ErrorLog or BaseContext changed")
	}
	if httpServer.TLSConfig != nil || httpServer.ReadTimeout != 0 || httpServer.WriteTimeout != 0 {
		t.Errorf("TLSConfig %v, ReadTimeout %s, WriteTimeout %s, want none", httpServer.TLSConfig, httpServer.ReadTimeout, httpServer.WriteTimeout)
	}
	ctx := httpServer.ConnContext(context.Background(), nil)
	if ctx.Value(testContextKey{}) != "caller" {
		t.Error("the caller's ConnContext no longer runs")
	}
	if _, ok := ctx.Value(frontConnectionContextKey{}).(*frontConnection); !ok {
		t.Error("a connection gets no keepalive state")
	}

	zeroSettings := testFrontSettings()
	zeroSettings.Http.ClientHeaderTimeout = 0
	zeroSettings.Http.MaxRequestHeadersTotalBytes = 0
	zeroSettings.Http.Http2MaxConcurrentStreams = 0
	zeroSettings.Http.SendTimeout = 0
	zeroFront := newTestFront(t, loadTestRouteTable(t), zeroSettings)
	zeroServer := &http.Server{}
	zeroFront.front.ConfigureServer(zeroServer)
	if zeroServer.ReadHeaderTimeout != 0 || zeroServer.MaxHeaderBytes != 0 || zeroServer.HTTP2 != nil {
		t.Errorf("non-positive settings set ReadHeaderTimeout %s, MaxHeaderBytes %d, HTTP2 %+v", zeroServer.ReadHeaderTimeout, zeroServer.MaxHeaderBytes, zeroServer.HTTP2)
	}

	// the server's header bytes follow the multiple, and a non-positive one
	// leaves Go's default
	for _, serverHeaderBytesMultiple := range []int{3, 0} {
		multipleSettings := testFrontSettings()
		multipleSettings.Http.ServerHeaderBytesMultiple = serverHeaderBytesMultiple
		multipleFront := newTestFront(t, loadTestRouteTable(t), multipleSettings)
		multipleServer := &http.Server{}
		multipleFront.front.ConfigureServer(multipleServer)
		if want := serverHeaderBytesMultiple * multipleSettings.Http.MaxRequestHeadersTotalBytes; multipleServer.MaxHeaderBytes != want {
			t.Errorf("multiple %d: MaxHeaderBytes %d, want %d", serverHeaderBytesMultiple, multipleServer.MaxHeaderBytes, want)
		}
	}

	// a send timeout alone still bounds h2's writes, under Go's stream limit
	sendTimeoutSettings := testFrontSettings()
	sendTimeoutSettings.Http.Http2MaxConcurrentStreams = 0
	sendTimeoutFront := newTestFront(t, loadTestRouteTable(t), sendTimeoutSettings)
	sendTimeoutServer := &http.Server{}
	sendTimeoutFront.front.ConfigureServer(sendTimeoutServer)
	if sendTimeoutServer.HTTP2 == nil || sendTimeoutServer.HTTP2.WriteByteTimeout != sendTimeoutSettings.Http.SendTimeout || sendTimeoutServer.HTTP2.MaxConcurrentStreams != 0 {
		t.Errorf("HTTP2 %+v with no stream limit, want only the write byte timeout %s", sendTimeoutServer.HTTP2, sendTimeoutSettings.Http.SendTimeout)
	}
}

// The request that reaches keepalive_requests is its connection's last, and
// so is every later one; a request more than keepalive_time after the
// connection was accepted is too, but not one at exactly keepalive_time, as
// nginx compares them. Non-positive limits close nothing, and a request on a
// connection ConfigureServer didn't set up is never a last one.
func TestFrontLastRequestOnConnectionFollowsNginxKeepalive(t *testing.T) {
	newConnectionRequest := func(front *Front) func() bool {
		httpServer := &http.Server{}
		front.ConfigureServer(httpServer)
		ctx := httpServer.ConnContext(t.Context(), nil)
		return func() bool {
			return front.lastRequestOnConnection(httptest.NewRequest("GET", "/", nil).WithContext(ctx))
		}
	}

	settings := testFrontSettings()
	settings.Http.KeepaliveRequests = 3
	settings.Http.KeepaliveTime = time.Hour
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	request := newConnectionRequest(testFront.front)
	for i, want := range []bool{false, false, true, true} {
		if last := request(); last != want {
			t.Errorf("request %d last %t, want %t", i+1, last, want)
		}
	}
	request = newConnectionRequest(testFront.front)
	testFront.clock.advance(time.Hour)
	if request() {
		t.Error("a request at exactly keepalive_time was its connection's last")
	}
	testFront.clock.advance(time.Nanosecond)
	if !request() {
		t.Error("a request past keepalive_time was not its connection's last")
	}
	if testFront.front.lastRequestOnConnection(httptest.NewRequest("GET", "/", nil)) {
		t.Error("a request without the front's connection state was a last one")
	}

	unlimitedSettings := testFrontSettings()
	unlimitedSettings.Http.KeepaliveRequests = 0
	unlimitedSettings.Http.KeepaliveTime = 0
	unlimitedFront := newTestFront(t, loadTestRouteTable(t), unlimitedSettings)
	request = newConnectionRequest(unlimitedFront.front)
	unlimitedFront.clock.advance(365 * 24 * time.Hour)
	for i := range 2000 {
		if request() {
			t.Fatalf("request %d was a last one with no keepalive limits", i+1)
		}
	}
}

// Over h1, the connection's 1000th request, nginx's default
// keepalive_requests, is its last: every response before it keeps the
// connection, and its own carries Connection: close and closes it.
func TestFrontKeepaliveRequestsCloseTheConnectionOverH1(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	if keepaliveRequests := testFront.front.settings.Http.KeepaliveRequests; keepaliveRequests != 1000 {
		t.Fatalf("KeepaliveRequests %d, want nginx's 1000", keepaliveRequests)
	}
	server := newTestConfiguredServer(t, testFront, false)
	client := dialTestServerH1Client(t, server.Listener.Addr().String())
	for i := 1; i <= 1000; i += 1 {
		if response := client.getLbStatus(); response.Close != (i == 1000) {
			t.Fatalf("request %d: Connection close %t", i, response.Close)
		}
	}
	if !client.closedByServer() {
		t.Fatal("the server kept the connection after its 1000th request")
	}
}

// Over h2, the connection's 1000th stream is its last: every stream is
// served on the one connection, and after the 1000th the server sends a
// GOAWAY naming it as the last stream and closes the connection.
func TestFrontKeepaliveRequestsCloseTheConnectionOverH2(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	server := newTestConfiguredServer(t, testFront, true)
	client := dialTestServerH2Client(t, server.Listener.Addr().String())
	for i := 1; i <= 1000; i += 1 {
		if status := client.get("test-lb.service.example", "/hidden-example/status"); status != strconv.Itoa(http.StatusOK) {
			t.Fatalf("stream %d: status %s", i, status)
		}
	}
	goAwayReceived, lastStreamId, errCode := client.readToClose()
	if !goAwayReceived || lastStreamId != testServerH2StreamId(1000) || errCode != http2.ErrCodeNo {
		t.Fatalf("closed with a GOAWAY %t, last stream %d, code %s; want one naming stream %d, no error", goAwayReceived, lastStreamId, errCode, testServerH2StreamId(1000))
	}
}

// Over h2, a stream Go's server answers itself, here its 400 to a
// connection-specific header, never reaches the front's handlers, so it
// doesn't count toward keepalive_requests and leaves the connection open,
// where nginx counts every stream (LBALT-IMPL.md §4): with a limit of 2,
// the GOAWAY follows the second stream the front served, the connection's
// third, not its second.
func TestFrontKeepaliveRequestsOverH2CountOnlyTheStreamsTheFrontServes(t *testing.T) {
	settings := testFrontSettings()
	settings.Http.KeepaliveRequests = 2
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	server := newTestConfiguredServer(t, testFront, true)
	client := dialTestServerH2Client(t, server.Listener.Addr().String())

	streamId := client.startGet("test-lb.service.example", "/hidden-example/status", hpack.HeaderField{
		Name:  "connection",
		Value: "keep-alive",
	})
	if status := client.readResponseHead(streamId); status != strconv.Itoa(http.StatusBadRequest) {
		t.Fatalf("a stream with a connection-specific header: status %s, want Go's 400", status)
	}
	for i := 2; i <= 3; i += 1 {
		if status := client.get("test-lb.service.example", "/hidden-example/status"); status != strconv.Itoa(http.StatusOK) {
			t.Fatalf("stream %d: status %s", i, status)
		}
	}
	goAwayReceived, lastStreamId, errCode := client.readToClose()
	if !goAwayReceived || lastStreamId != testServerH2StreamId(3) || errCode != http2.ErrCodeNo {
		t.Fatalf("closed with a GOAWAY %t, last stream %d, code %s; want one naming stream %d, no error", goAwayReceived, lastStreamId, errCode, testServerH2StreamId(3))
	}
}

// Over h1, a request more than keepalive_time after the connection was
// accepted, by the front's clock, is its last and closes it, and one at
// exactly keepalive_time isn't.
func TestFrontKeepaliveTimeClosesTheConnectionOverH1(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	keepaliveTime := testFront.front.settings.Http.KeepaliveTime
	server := newTestConfiguredServer(t, testFront, false)
	client := dialTestServerH1Client(t, server.Listener.Addr().String())

	if client.getLbStatus().Close {
		t.Fatal("the first request closed its connection")
	}
	testFront.clock.advance(keepaliveTime)
	if client.getLbStatus().Close {
		t.Fatal("a request at exactly keepalive_time closed its connection")
	}
	testFront.clock.advance(time.Nanosecond)
	if !client.getLbStatus().Close || !client.closedByServer() {
		t.Fatal("a request past keepalive_time kept its connection")
	}
}

// Over h2, the first stream more than keepalive_time after the connection
// was accepted is its last: the GOAWAY names it, and the connection closes.
func TestFrontKeepaliveTimeClosesTheConnectionOverH2(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	keepaliveTime := testFront.front.settings.Http.KeepaliveTime
	server := newTestConfiguredServer(t, testFront, true)
	client := dialTestServerH2Client(t, server.Listener.Addr().String())

	client.get("test-lb.service.example", "/hidden-example/status")
	testFront.clock.advance(keepaliveTime)
	client.get("test-lb.service.example", "/hidden-example/status")
	testFront.clock.advance(time.Nanosecond)
	if status := client.get("test-lb.service.example", "/hidden-example/status"); status != strconv.Itoa(http.StatusOK) {
		t.Fatalf("the stream past keepalive_time: status %s", status)
	}
	goAwayReceived, lastStreamId, errCode := client.readToClose()
	if !goAwayReceived || lastStreamId != testServerH2StreamId(3) || errCode != http2.ErrCodeNo {
		t.Fatalf("closed with a GOAWAY %t, last stream %d, code %s; want one naming stream %d, no error", goAwayReceived, lastStreamId, errCode, testServerH2StreamId(3))
	}
}

// keepalive_timeout 0 turns keepalives off: an h1 response closes its
// connection, and an h2 connection is sent a GOAWAY once its first stream
// has ended.
func TestFrontKeepaliveTimeoutZeroClosesEveryConnection(t *testing.T) {
	settings := testFrontSettings()
	settings.Http.KeepaliveTimeout = 0
	testFront := newTestFront(t, loadTestRouteTable(t), settings)

	h1Server := newTestConfiguredServer(t, testFront, false)
	h1Client := dialTestServerH1Client(t, h1Server.Listener.Addr().String())
	if !h1Client.getLbStatus().Close || !h1Client.closedByServer() {
		t.Error("an h1 response kept its connection with keepalive_timeout 0")
	}

	h2Server := newTestConfiguredServer(t, testFront, true)
	client := dialTestServerH2Client(t, h2Server.Listener.Addr().String())
	client.get("test-lb.service.example", "/hidden-example/status")
	goAwayReceived, lastStreamId, _ := client.readToClose()
	if !goAwayReceived || lastStreamId != testServerH2StreamId(1) {
		t.Errorf("an h2 connection closed with a GOAWAY %t, last stream %d; want one after the first stream", goAwayReceived, lastStreamId)
	}
}

// The server reads twice nginx's header buffers before the handler, so the
// front answers nginx's own 400 to a head between one and two times
// MaxRequestHeadersTotalBytes, where a server limit of one times would have
// Go answer 431; past twice the buffers and net/http's 4 KiB slack, Go
// answers 431. Each request opens its own connection: on a reused one,
// net/http buffers up to 4 KiB of the next head before it applies the limit,
// so the bound moves up by that much.
func TestFrontConfigureServerLetsTheFrontAnswerHeadsWithinTwiceTheBuffers(t *testing.T) {
	settings := testFrontSettings()
	settings.Http.MaxRequestHeadersTotalBytes = 4 * 1024
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	server := newTestConfiguredServer(t, testFront, false)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	client := &http.Client{
		Transport: transport,
	}
	statusWithHeaders := func(fillCount int) int {
		t.Helper()
		r := newTestServerRequest(t, server, "GET", "test-lb.service.example", "/hidden-example/status", nil)
		// each header well under nginx's 8 KiB line
		for i := range fillCount {
			r.Header.Set("X-Fill-"+strconv.Itoa(i), strings.Repeat("a", 1024))
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}
	cases := []struct {
		fillCount  int
		statusCode int
	}{
		{fillCount: 2, statusCode: http.StatusOK},
		// about 10 KiB: over the buffers, under twice them plus the slack
		{fillCount: 10, statusCode: http.StatusBadRequest},
		{fillCount: 16, statusCode: http.StatusRequestHeaderFieldsTooLarge},
	}
	for _, c := range cases {
		if statusCode := statusWithHeaders(c.fillCount); statusCode != c.statusCode {
			t.Errorf("%d KiB of headers: status %d, want %d", c.fillCount, statusCode, c.statusCode)
		}
	}
}

// Over h2, a client that stops reading at the socket while its flow-control
// windows stay open has its connection closed once a frame write has been
// blocked for SendTimeout, so the handler's write fails then, as nginx
// closes an h2 connection whose output stays blocked past send_timeout. A
// stream's own deadline can't do it: its reset waits behind the blocked
// write. The client stops reading once it has the response head, and the
// handler writes only after that; the connection stalls then, so every
// write to it waits out its deadline. The connection fails its writes after
// a timed-out one, as connect's public sockets do, so the bound is well
// under crypto/tls's 5 s close_notify deadline.
func TestFrontConfigureServerClosesAnH2ConnectionWhoseClientStopsReading(t *testing.T) {
	const sendTimeout = 200 * time.Millisecond
	const releaseBound = 3 * time.Second
	settings := testFrontSettings()
	settings.Http.SendTimeout = sendTimeout
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	type cut struct {
		err     error
		blocked time.Duration
	}
	stalled := make(chan struct{})
	cuts := make(chan cut, 1)
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		select {
		case <-stalled:
		case <-r.Context().Done():
			return
		}
		chunk := make([]byte, 16*1024)
		// at most 1 GiB, far past any buffer, so a write never cut still
		// ends
		for range 64 * 1024 {
			writeStart := time.Now()
			if _, err := w.Write(chunk); err != nil {
				cuts <- cut{
					err:     err,
					blocked: time.Since(writeStart),
				}
				return
			}
		}
		cuts <- cut{}
	}
	accepted := make(chan *testServerStallingConn, 4)
	server := httptest.NewUnstartedServer(testFront.front.HttpsHandler("eth1"))
	testFront.front.ConfigureServer(server.Config)
	server.EnableHTTP2 = true
	server.Listener = &testServerStallingListener{
		Listener: server.Listener,
		accepted: accepted,
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	client := dialTestServerH2Client(t, server.Listener.Addr().String())
	client.openWindows()

	streamId := client.startGet("api.service.example", "/download")
	if status := client.readResponseHead(streamId); status != strconv.Itoa(http.StatusOK) {
		t.Fatalf("the download: status %s", status)
	}
	select {
	case conn := <-accepted:
		conn.stalled.Store(true)
	default:
		t.Fatal("no connection was accepted")
	}
	close(stalled)
	select {
	case cutWrite := <-cuts:
		if cutWrite.err == nil {
			t.Fatal("1 GiB was written to a client that reads nothing")
		}
		if cutWrite.blocked < sendTimeout || releaseBound <= cutWrite.blocked {
			t.Fatalf("the cut write returned after %s, want SendTimeout %s or a little more", cutWrite.blocked, sendTimeout)
		}
	case <-time.After(releaseBound):
		t.Fatalf("after %s the handler's write to a client that reads nothing is still blocked", releaseBound)
	}
	// the server closed the connection: past what it sent before the stall,
	// the client finds the connection's end, and no read deadline
	for {
		if _, err := client.framer.ReadFrame(); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("the connection is still open after the cut: %v", err)
			}
			break
		}
	}
}
