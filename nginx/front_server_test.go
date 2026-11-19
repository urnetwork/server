package nginx

// Tests of the front's public TCP servers (front_server.go): the fields
// ConfigureServer sets, and nginx's keepalive_requests and keepalive_time
// over real h1 and h2 servers. The h2 tests read frames directly, so the
// GOAWAY and the connection's close are seen as the client sees them. The
// requests go to the lb status route on an lb name, which no limit charges.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// A real TLS server of the front's TCP 443 handler, configured by
// ConfigureServer as connect configures its public servers, h2 when
// enableHttp2 is set. It closes when the test ends.
func newTestConfiguredServer(t *testing.T, testFront *testFront, enableHttp2 bool) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(testFront.front.HttpsHandler("eth1"))
	testFront.front.ConfigureServer(server.Config)
	server.EnableHTTP2 = enableHttp2
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// An h1 GET of the lb status route through client, and the connection it
// went over.
func getTestLbStatus(t *testing.T, server *httptest.Server, client *http.Client) (*http.Response, net.Conn) {
	t.Helper()
	var conn net.Conn
	r := newTestServerRequest(t, server, "GET", "test-lb.service.example", "/hidden-example/status", nil)
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
		GotConn: func(gotConnInfo httptrace.GotConnInfo) {
			conn = gotConnInfo.Conn
		},
	}))
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != testLbStatusBody {
		t.Fatalf("lb status = %d %q, %v", response.StatusCode, body, err)
	}
	return response, conn
}

// A minimal h2 client over one TLS connection, which reads the server's
// frames itself, so a test sees the GOAWAY and the connection's close.
type testH2Client struct {
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
func dialTestH2Client(t *testing.T, address string) *testH2Client {
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
	client := &testH2Client{
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
func (self *testH2Client) readFrame() http2.Frame {
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
func (self *testH2Client) get(authority string, path string) string {
	self.t.Helper()
	streamId := self.nextStreamId
	self.nextStreamId += 2
	self.headerBlock.Reset()
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: authority},
		{Name: ":path", Value: path},
	} {
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

// Reads frames until the server closes the connection, and reports whether
// it sent a GOAWAY first, and its last stream and code.
func (self *testH2Client) readToClose() (goAwayReceived bool, lastStreamId uint32, errCode http2.ErrCode) {
	self.t.Helper()
	for self.readFrame() != nil {
	}
	return self.goAwayReceived, self.goAwayLastStreamId, self.goAwayErrCode
}

// The id of an h2 client's nth stream, from 1.
func testH2StreamId(n int) uint32 {
	return uint32(2*n - 1)
}

// ConfigureServer sets every field that derives from HttpSettings and
// nothing the caller owns: Handler, ErrorLog and BaseContext stay, no
// TLSConfig, ReadTimeout or WriteTimeout is set, a ConnContext already set
// still runs ahead of the front's, and an HTTP2 config the caller set keeps
// its other fields without being written. Non-positive settings leave Go's
// defaults.
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
	if want := 2 * httpSettings.MaxRequestHeadersTotalBytes; httpServer.MaxHeaderBytes != want {
		t.Errorf("MaxHeaderBytes %d, want twice MaxRequestHeadersTotalBytes, %d", httpServer.MaxHeaderBytes, want)
	}
	if httpServer.HTTP2 == nil || httpServer.HTTP2.MaxConcurrentStreams != httpSettings.Http2MaxConcurrentStreams || httpServer.HTTP2.MaxReceiveBufferPerConnection != 1024*1024 {
		t.Errorf("HTTP2 %+v, want the stream limit %d with the caller's receive buffer", httpServer.HTTP2, httpSettings.Http2MaxConcurrentStreams)
	}
	if callerHttp2Config.MaxConcurrentStreams != 0 {
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
	zeroFront := newTestFront(t, loadTestRouteTable(t), zeroSettings)
	zeroServer := &http.Server{}
	zeroFront.front.ConfigureServer(zeroServer)
	if zeroServer.ReadHeaderTimeout != 0 || zeroServer.MaxHeaderBytes != 0 || zeroServer.HTTP2 != nil {
		t.Errorf("non-positive settings set ReadHeaderTimeout %s, MaxHeaderBytes %d, HTTP2 %+v", zeroServer.ReadHeaderTimeout, zeroServer.MaxHeaderBytes, zeroServer.HTTP2)
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
// connection, its own carries Connection: close, and the next request goes
// over a new connection.
func TestFrontKeepaliveRequestsCloseTheConnectionOverH1(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	if keepaliveRequests := testFront.front.settings.Http.KeepaliveRequests; keepaliveRequests != 1000 {
		t.Fatalf("KeepaliveRequests %d, want nginx's 1000", keepaliveRequests)
	}
	server := newTestConfiguredServer(t, testFront, false)
	client := server.Client()

	var firstConn net.Conn
	for i := 1; i <= 1001; i += 1 {
		response, conn := getTestLbStatus(t, server, client)
		switch {
		case i == 1:
			firstConn = conn
		case i <= 1000 && conn != firstConn:
			t.Fatalf("request %d went over another connection", i)
		case i == 1001 && conn == firstConn:
			t.Fatal("the 1001st request went over the closed connection")
		}
		if wantClose := i == 1000; response.Close != wantClose {
			t.Fatalf("request %d: Connection close %t, want %t", i, response.Close, wantClose)
		}
	}
}

// Over h2, the connection's 1000th stream is its last: every stream is
// served on the one connection, and after the 1000th the server sends a
// GOAWAY naming it as the last stream and closes the connection.
func TestFrontKeepaliveRequestsCloseTheConnectionOverH2(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	server := newTestConfiguredServer(t, testFront, true)
	client := dialTestH2Client(t, server.Listener.Addr().String())
	for i := 1; i <= 1000; i += 1 {
		if status := client.get("test-lb.service.example", "/hidden-example/status"); status != strconv.Itoa(http.StatusOK) {
			t.Fatalf("stream %d: status %s", i, status)
		}
	}
	goAwayReceived, lastStreamId, errCode := client.readToClose()
	if !goAwayReceived || lastStreamId != testH2StreamId(1000) || errCode != http2.ErrCodeNo {
		t.Fatalf("closed with a GOAWAY %t, last stream %d, code %s; want one naming stream %d, no error", goAwayReceived, lastStreamId, errCode, testH2StreamId(1000))
	}
}

// Over h1, a request more than keepalive_time after the connection was
// accepted, by the front's clock, is its last, and one at exactly
// keepalive_time isn't; all of them go over the one connection.
func TestFrontKeepaliveTimeClosesTheConnectionOverH1(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	keepaliveTime := testFront.front.settings.Http.KeepaliveTime
	server := newTestConfiguredServer(t, testFront, false)
	client := server.Client()

	response, firstConn := getTestLbStatus(t, server, client)
	if response.Close {
		t.Fatal("the first request closed its connection")
	}
	testFront.clock.advance(keepaliveTime)
	response, conn := getTestLbStatus(t, server, client)
	if response.Close || conn != firstConn {
		t.Fatalf("a request at exactly keepalive_time: close %t, same connection %t", response.Close, conn == firstConn)
	}
	testFront.clock.advance(time.Nanosecond)
	response, conn = getTestLbStatus(t, server, client)
	if !response.Close || conn != firstConn {
		t.Fatalf("a request past keepalive_time: close %t, same connection %t; want its connection's last", response.Close, conn == firstConn)
	}
}

// Over h2, the first stream more than keepalive_time after the connection
// was accepted is its last: the GOAWAY names it, and the connection closes.
func TestFrontKeepaliveTimeClosesTheConnectionOverH2(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	keepaliveTime := testFront.front.settings.Http.KeepaliveTime
	server := newTestConfiguredServer(t, testFront, true)
	client := dialTestH2Client(t, server.Listener.Addr().String())

	client.get("test-lb.service.example", "/hidden-example/status")
	testFront.clock.advance(keepaliveTime)
	client.get("test-lb.service.example", "/hidden-example/status")
	testFront.clock.advance(time.Nanosecond)
	if status := client.get("test-lb.service.example", "/hidden-example/status"); status != strconv.Itoa(http.StatusOK) {
		t.Fatalf("the stream past keepalive_time: status %s", status)
	}
	goAwayReceived, lastStreamId, errCode := client.readToClose()
	if !goAwayReceived || lastStreamId != testH2StreamId(3) || errCode != http2.ErrCodeNo {
		t.Fatalf("closed with a GOAWAY %t, last stream %d, code %s; want one naming stream %d, no error", goAwayReceived, lastStreamId, errCode, testH2StreamId(3))
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
	if response, _ := getTestLbStatus(t, h1Server, h1Server.Client()); !response.Close {
		t.Error("an h1 response kept its connection with keepalive_timeout 0")
	}

	h2Server := newTestConfiguredServer(t, testFront, true)
	client := dialTestH2Client(t, h2Server.Listener.Addr().String())
	client.get("test-lb.service.example", "/hidden-example/status")
	goAwayReceived, lastStreamId, _ := client.readToClose()
	if !goAwayReceived || lastStreamId != testH2StreamId(1) {
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
