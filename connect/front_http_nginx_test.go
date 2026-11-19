package connect

// The front's tcp servers as the nginx front configures them: connect sets
// only the handler, the error log and the serving context, and the nginx
// front sets the rest (nginx.Front.ConfigureServer). Through connect's own
// public sockets and the real nginx front, nginx's keepalive_requests and
// keepalive_time close connections on h1 and h2, on the lb status route of
// an lb name, which no limit charges; and a client that stops reading over
// TLS, on h1 or h2, is cut at send_timeout without close_notify holding its
// handler. The public sockets bound a write left without a deadline by
// send_timeout, hijacked or not (frontTrackedConn).

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"

	"github.com/urnetwork/server/nginx"
)

// connect sets only its own fields on each public server, the handler, the
// counting error log and the serving context, and hands every one to the
// nginx front to configure (LBALT-IMPL.md §7), so their limits are the nginx
// front's, header bytes at twice nginx's buffers included.
func TestFrontHttpServersAreConfiguredByTheNginxFront(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	configuredServers := env.front.recordedConfiguredServers()
	if len(configuredServers) != 2*len(env.routeTable.Interfaces) || !slices.Equal(configuredServers, env.frontServer.httpServers) {
		t.Fatalf("the nginx front configured %d servers, want every one of connect's %d", len(configuredServers), len(env.frontServer.httpServers))
	}
	httpSettings := env.settings.Front.Http
	for _, httpServer := range configuredServers {
		if httpServer.Handler == nil || httpServer.ErrorLog == nil || httpServer.BaseContext == nil || httpServer.BaseContext(nil) != env.frontServer.ctx {
			t.Fatal("a public server lacks connect's handler, error log or serving context")
		}
		if httpServer.ReadHeaderTimeout != httpSettings.ClientHeaderTimeout ||
			httpServer.IdleTimeout != httpSettings.KeepaliveTimeout ||
			httpServer.MaxHeaderBytes != httpSettings.ServerHeaderBytesMultiple*httpSettings.MaxRequestHeadersTotalBytes ||
			httpServer.ConnContext == nil {
			t.Fatalf("a public server's limits are not the nginx front's: header timeout %s, idle timeout %s, header bytes %d", httpServer.ReadHeaderTimeout, httpServer.IdleTimeout, httpServer.MaxHeaderBytes)
		}
	}
}

// Connect names reach connect in-process only through connect's own routes,
// as a connect block's private listener routes what nginx forwards it: GET /
// reaches connect, its upgrade included, with the client's cookies dropped,
// while another path answers 404, an upgrade to it included, and another
// method on / 405 with Allow: GET, none of them reaching connect.
func TestFrontConnectNamesReachConnectOnlyThroughItsRoutes(t *testing.T) {
	t.Setenv("WARP_VERSION", "0.0.0-test")
	type connectRequest struct {
		method string
		path   string
		cookie bool
	}
	connectRequests := make(chan connectRequest, 16)
	connect := func(w http.ResponseWriter, r *http.Request) {
		connectRequests <- connectRequest{method: r.Method, path: r.URL.Path, cookie: r.Header.Get("Cookie") != ""}
		io.WriteString(w, "connect")
	}
	env := newUnstartedTestFrontEnv(t, nil)
	if err := env.frontServer.startWithConnectRoutes(env.ctx, connect, env.connectQuic); err != nil {
		t.Fatal(err)
	}
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	const upgradeHeader = "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	cases := []struct {
		method string
		path   string
		header string
		status int
		allow  string
	}{
		{method: http.MethodGet, path: "/favicon.ico", status: http.StatusNotFound},
		{method: http.MethodGet, path: "/other", header: upgradeHeader, status: http.StatusNotFound},
		{method: http.MethodPost, path: "/", header: "Content-Length: 0\r\n", status: http.StatusMethodNotAllowed, allow: http.MethodGet},
		{method: http.MethodDelete, path: "/x", status: http.StatusNotFound},
		{method: http.MethodGet, path: "/", header: "Cookie: session=synthetic\r\n", status: http.StatusOK},
		{method: http.MethodGet, path: "/", header: upgradeHeader, status: http.StatusOK},
	}
	for _, c := range cases {
		client := env.dialH1Client(env.bindAddress(address, frontPortHttps), testFrontConnectName)
		fmt.Fprintf(client.conn, "%s %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", c.method, c.path, testFrontConnectName, c.header)
		response, err := http.ReadResponse(client.reader, nil)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != c.status || response.Header.Get("Allow") != c.allow {
			t.Fatalf("%s %s = %d with Allow %q, want %d with Allow %q", c.method, c.path, response.StatusCode, response.Header.Get("Allow"), c.status, c.allow)
		}
	}
	close(connectRequests)
	got := []connectRequest{}
	for request := range connectRequests {
		got = append(got, request)
	}
	want := []connectRequest{{method: http.MethodGet, path: "/"}, {method: http.MethodGet, path: "/"}}
	if !slices.Equal(got, want) {
		t.Fatalf("connect saw %+v, want only the two GET / without a cookie", got)
	}
}

// Through connect's https socket and the real nginx front, the
// connection's 1000th request, nginx's keepalive_requests, is its last over
// h1: every response before it keeps the connection, and its own carries
// Connection: close and closes it.
func TestFrontHttpsKeepaliveRequestsCloseTheConnectionOverH1(t *testing.T) {
	env := newTestNginxFrontEnv(t, nil, http.NotFoundHandler())
	if keepaliveRequests := env.settings.Front.Http.KeepaliveRequests; keepaliveRequests != 1000 {
		t.Fatalf("KeepaliveRequests %d, want nginx's 1000", keepaliveRequests)
	}
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	lbName := env.routeTable.Lb.Names[0]
	client := env.dialH1Client(env.bindAddress(address, frontPortHttps), lbName)
	for i := 1; i <= 1000; i += 1 {
		if response := client.get(lbName, env.routeTable.Lb.RoutePrefixes[0]+"/status"); response.Close != (i == 1000) {
			t.Fatalf("request %d: Connection close %t", i, response.Close)
		}
	}
	if !client.closedByServer() {
		t.Fatal("the server kept the connection after its 1000th request")
	}
}

// Over h2, the connection's 1000th stream is its last: every stream is
// served on the one connection, then the server sends a GOAWAY naming the
// 1000th as the last stream and closes the connection.
func TestFrontHttpsKeepaliveRequestsCloseTheConnectionOverH2(t *testing.T) {
	env := newTestNginxFrontEnv(t, nil, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	lbName := env.routeTable.Lb.Names[0]
	client := env.dialH2Client(env.bindAddress(address, frontPortHttps), lbName)
	for i := 1; i <= 1000; i += 1 {
		if status := client.get(lbName, env.routeTable.Lb.RoutePrefixes[0]+"/status"); status != strconv.Itoa(http.StatusOK) {
			t.Fatalf("stream %d: status %s", i, status)
		}
	}
	goAwayReceived, lastStreamId, errCode := client.readToClose()
	if !goAwayReceived || lastStreamId != testFrontH2StreamId(1000) || errCode != http2.ErrCodeNo {
		t.Fatalf("closed with a GOAWAY %t, last stream %d, code %s; want one naming stream %d, no error", goAwayReceived, lastStreamId, errCode, testFrontH2StreamId(1000))
	}
}

// A connection past nginx's keepalive_time is closed after its next request
// over h1. With a keepalive_time of 1 ns every connection is past it by its
// first request, which then carries Connection: close and closes it.
func TestFrontHttpsKeepaliveTimeClosesTheConnectionOverH1(t *testing.T) {
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.KeepaliveTime = time.Nanosecond
	}, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	lbName := env.routeTable.Lb.Names[0]
	client := env.dialH1Client(env.bindAddress(address, frontPortHttps), lbName)
	if !client.get(lbName, env.routeTable.Lb.RoutePrefixes[0]+"/status").Close || !client.closedByServer() {
		t.Fatal("a request past keepalive_time kept its connection")
	}
}

// Over h2, the first stream past keepalive_time is the connection's last:
// the server sends a GOAWAY naming it and closes the connection.
func TestFrontHttpsKeepaliveTimeClosesTheConnectionOverH2(t *testing.T) {
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.KeepaliveTime = time.Nanosecond
	}, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	lbName := env.routeTable.Lb.Names[0]
	client := env.dialH2Client(env.bindAddress(address, frontPortHttps), lbName)
	if status := client.get(lbName, env.routeTable.Lb.RoutePrefixes[0]+"/status"); status != strconv.Itoa(http.StatusOK) {
		t.Fatalf("the stream past keepalive_time: status %s", status)
	}
	goAwayReceived, lastStreamId, errCode := client.readToClose()
	if !goAwayReceived || lastStreamId != testFrontH2StreamId(1) || errCode != http2.ErrCodeNo {
		t.Fatalf("closed with a GOAWAY %t, last stream %d, code %s; want one naming stream %d, no error", goAwayReceived, lastStreamId, errCode, testFrontH2StreamId(1))
	}
}

// Through connect's port 80 and the real nginx front, an answer that ends
// the connection while the client is still sending the body it declared,
// here the default server's 403 to a client that asked to close, is
// followed at once by a FIN: net/http half-closes ahead of the wait it makes
// before the close, and the tracked connection passes that to the socket.
// The client reads the whole answer, then the end of the server's side, not
// the reset the close brings.
func TestFrontHttpHalfClosesAConnectionItEndsOverAnUnreadBody(t *testing.T) {
	env := newTestNginxFrontEnv(t, nil, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	conn, err := net.Dial("tcp", env.bindAddress(address, frontPortHttp))
	if err != nil {
		t.Fatal(err)
	}
	// bounds a broken test, never a passing one
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	const contentLength = 16 * 1024 * 1024
	chunk := make([]byte, 32*1024)
	// the head and the body's first bytes at once, so some of the body is
	// unread when the server closes
	head := fmt.Sprintf("POST /upload HTTP/1.1\r\nHost: unknown.example\r\nConnection: close\r\nContent-Length: %d\r\n\r\n", contentLength)
	if _, err := conn.Write(append([]byte(head), chunk...)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for written := len(chunk); written < contentLength; {
			n, err := conn.Write(chunk)
			written += n
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		// ends a write the server left blocked
		conn.Close()
		<-writerDone
	}()

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusForbidden || string(body) != http.StatusText(http.StatusForbidden)+"\n" || !response.Close {
		t.Fatalf("= %d %q, closing %t, %v; want the default server's 403, closing", response.StatusCode, body, response.Close, err)
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the answer, %v; want the server's FIN", err)
	}
}

// Once a write to a public tcp connection fails on its deadline, every later
// write fails at once with that error, whatever deadline is set after it,
// even with a reader ready: the connection is cut mid-write, and the
// close_notify crypto/tls would send next must not wait on a client that
// stopped reading.
func TestFrontTrackedConnFailsWritesAfterATimedOutWrite(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	trackedConn := &frontTrackedConn{
		Conn: serverConn,
	}
	// a deadline already passed fails the next write on it
	trackedConn.SetWriteDeadline(time.Now())
	if _, err := trackedConn.Write([]byte("a")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the first write = %v, want its deadline's error", err)
	}

	trackedConn.SetWriteDeadline(time.Time{})
	go io.Copy(io.Discard, clientConn)
	n, err := trackedConn.Write([]byte("b"))
	if n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a write after the timed-out one = %d, %v; want it failed at once with the deadline's error", n, err)
	}
}

// A raw connection over one end of a pipe, whose writes wait for the other
// end to read, or for their deadline. It reports the write deadline each
// write starts under, so a test sees a write's bound and can act while the
// write waits.
type testFrontDeadlineConn struct {
	net.Conn
	// the write deadline of each write as it starts
	writeStarts chan time.Time

	stateLock     sync.Mutex
	writeDeadline time.Time
}

// Over conn, one end of a pipe.
func newTestFrontDeadlineConn(conn net.Conn) *testFrontDeadlineConn {
	return &testFrontDeadlineConn{
		Conn:        conn,
		writeStarts: make(chan time.Time, 16),
	}
}

// See net.Conn.
func (self *testFrontDeadlineConn) SetDeadline(deadline time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeDeadline = deadline
	}()
	return self.Conn.SetDeadline(deadline)
}

// See net.Conn.
func (self *testFrontDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeDeadline = deadline
	}()
	return self.Conn.SetWriteDeadline(deadline)
}

// The write deadline set last, zero for none.
func (self *testFrontDeadlineConn) currentWriteDeadline() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.writeDeadline
}

// Reports the deadline the write starts under, then writes p.
func (self *testFrontDeadlineConn) Write(p []byte) (int, error) {
	self.writeStarts <- self.currentWriteDeadline()
	return self.Conn.Write(p)
}

// The deadline the next write started under.
func (self *testFrontDeadlineConn) nextWriteStart(t testing.TB) time.Time {
	t.Helper()
	select {
	case deadline := <-self.writeStarts:
		return deadline
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("no write started")
		return time.Time{}
	}
}

// A write deadline as a test reports it: none, or how far away it is.
func testFrontDeadlineText(deadline time.Time) string {
	if deadline.IsZero() {
		return "none"
	}
	return fmt.Sprintf("%s away", time.Until(deadline))
}

// A write its owner left without a deadline, on a connection net/http still
// owns, fails once none of it has gone out for send_timeout, as crypto/tls's
// answer to a KeyUpdate request does on a client that stopped reading; every
// later write fails at once; and the socket has no deadline again once the
// bounded write returned, so a connection idle between writes has none.
func TestFrontTrackedConnBoundsAWriteItsOwnerLeftWithoutADeadline(t *testing.T) {
	const sendTimeout = 50 * time.Millisecond
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	rawConn := newTestFrontDeadlineConn(serverConn)
	trackedConn := &frontTrackedConn{
		Conn:        rawConn,
		sendTimeout: sendTimeout,
	}
	writeStart := time.Now()
	written := make(chan error, 1)
	go func() {
		_, err := trackedConn.Write([]byte("a"))
		written <- err
	}()
	startDeadline := rawConn.nextWriteStart(t)
	if startDeadline.Before(writeStart.Add(sendTimeout)) || time.Now().Add(sendTimeout).Before(startDeadline) {
		t.Fatalf("the write started under the deadline %s, want send_timeout %s from its start", testFrontDeadlineText(startDeadline), sendTimeout)
	}
	select {
	case err := <-written:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the write the peer never reads = %v, want its bound's error", err)
		}
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("a write the peer never reads was not cut")
	}
	if deadline := rawConn.currentWriteDeadline(); !deadline.IsZero() {
		t.Fatalf("after the bounded write the socket's deadline is %s, want none", testFrontDeadlineText(deadline))
	}

	go io.Copy(io.Discard, clientConn)
	n, err := trackedConn.Write([]byte("b"))
	if n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a write after the cut one = %d, %v; want it failed at once with the bound's error", n, err)
	}
}

// A raw connection whose first write puts out one byte and fails on its
// deadline, as a write to a client that reads slowly does, and whose later
// writes go out whole. It records the write deadline each write starts
// under. Only its deadlines and writes are used.
type testFrontPartialWriteConn struct {
	net.Conn

	stateLock      sync.Mutex
	writeDeadline  time.Time
	startDeadlines []time.Time
	written        []byte
}

// See net.Conn.
func (self *testFrontPartialWriteConn) SetWriteDeadline(deadline time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writeDeadline = deadline
	return nil
}

// Puts out one byte of the first write and fails it on its deadline; puts
// out every later write whole.
func (self *testFrontPartialWriteConn) Write(p []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.startDeadlines = append(self.startDeadlines, self.writeDeadline)
	if len(self.startDeadlines) == 1 {
		self.written = append(self.written, p[0])
		return 1, os.ErrDeadlineExceeded
	}
	self.written = append(self.written, p...)
	return len(p), nil
}

// A bounded write that makes progress gets a new bound, as nginx's
// send_timeout bounds only a write that makes none: a write to a client
// that reads, however slowly, is never cut, and nothing latches.
func TestFrontTrackedConnRestartsTheBoundOfAWriteThatMakesProgress(t *testing.T) {
	rawConn := &testFrontPartialWriteConn{}
	trackedConn := &frontTrackedConn{
		Conn:        rawConn,
		sendTimeout: time.Hour,
	}
	if n, err := trackedConn.Write([]byte("ab")); n != 2 || err != nil {
		t.Fatalf("a write that made progress before its bound = %d, %v; want it written whole", n, err)
	}
	if n, err := trackedConn.Write([]byte("c")); n != 1 || err != nil {
		t.Fatalf("the next write = %d, %v; want it written, nothing latched", n, err)
	}
	if string(rawConn.written) != "abc" {
		t.Fatalf("the socket got %q, want abc", rawConn.written)
	}
	startDeadlines := rawConn.startDeadlines
	if len(startDeadlines) != 3 {
		t.Fatalf("%d attempts, want the first write's two and the next write's one", len(startDeadlines))
	}
	for i, startDeadline := range startDeadlines {
		if startDeadline.IsZero() {
			t.Fatalf("attempt %d started with no bound", i)
		}
	}
	if startDeadlines[1].Before(startDeadlines[0]) {
		t.Fatal("the attempt after the progress started under an earlier bound")
	}
}

// An owner's deadline set while a bounded write waits takes the write over
// and stands once it returns, so the bound never overwrites or clears it,
// and the writes under it get no bound; an owner that clears its deadline
// meanwhile leaves the write its bound.
func TestFrontTrackedConnKeepsAnOwnerDeadlineSetDuringABoundedWrite(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	rawConn := newTestFrontDeadlineConn(serverConn)
	trackedConn := &frontTrackedConn{
		Conn:        rawConn,
		sendTimeout: time.Hour,
	}
	written := make(chan error, 1)
	write := func(data string) {
		go func() {
			_, err := trackedConn.Write([]byte(data))
			written <- err
		}()
	}
	readWritten := func(what string) {
		t.Helper()
		if _, err := io.ReadFull(clientConn, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-written:
			if err != nil {
				t.Fatalf("%s = %v, want it written", what, err)
			}
		case <-time.After(testFrontProgressTimeout):
			t.Fatalf("%s did not return once read", what)
		}
	}

	write("a")
	bound := rawConn.nextWriteStart(t)
	if bound.IsZero() {
		t.Fatal("the write started with no bound")
	}
	trackedConn.SetWriteDeadline(time.Time{})
	if deadline := rawConn.currentWriteDeadline(); !deadline.Equal(bound) {
		t.Fatalf("an owner that cleared its deadline mid-write left the socket's deadline %s, want the write's bound", testFrontDeadlineText(deadline))
	}
	ownerDeadline := time.Now().Add(2 * time.Hour)
	trackedConn.SetWriteDeadline(ownerDeadline)
	if deadline := rawConn.currentWriteDeadline(); !deadline.Equal(ownerDeadline) {
		t.Fatalf("the owner's deadline set mid-write left the socket's %s, want the owner's", testFrontDeadlineText(deadline))
	}
	readWritten("the bounded write")
	if deadline := rawConn.currentWriteDeadline(); !deadline.Equal(ownerDeadline) {
		t.Fatalf("after the bounded write the socket's deadline is %s, want the owner's", testFrontDeadlineText(deadline))
	}

	write("b")
	if startDeadline := rawConn.nextWriteStart(t); !startDeadline.Equal(ownerDeadline) {
		t.Fatalf("a write under the owner's deadline started under %s, want the owner's", testFrontDeadlineText(startDeadline))
	}
	readWritten("the write under the owner's deadline")
}

// A public listener that reports each connection it accepts, wrapped so a
// test can stall it.
type testFrontStallingListener struct {
	net.Listener
	accepted chan *testFrontStallingConn
}

// Accepts the next connection and reports it.
func (self *testFrontStallingListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	stallingConn := &testFrontStallingConn{
		Conn:   conn,
		closed: make(chan struct{}),
	}
	select {
	case self.accepted <- stallingConn:
	default:
	}
	return stallingConn, nil
}

// A connection whose writes, once it is stalled, wait out their deadline and
// fail with its error, as a write does to a client that stopped reading
// once the socket's buffers are full, whatever the kernel's buffer sizes.
// Once it is wedged, a write waits for the close alone, whatever its
// deadline: as close_notify does behind a write that holds TLS's write lock
// past every bound a close has, such as the answer crypto/tls writes on the
// reading goroutine to a client's KeyUpdate request, which only that write's
// own bound, send_timeout, ends (frontTrackedConn). It records the write
// deadline each write starts under. Safe for concurrent use as a net.Conn
// is.
type testFrontStallingConn struct {
	net.Conn
	stalled   atomic.Bool
	wedged    atomic.Bool
	closed    chan struct{}
	closeOnce sync.Once

	stateLock     sync.Mutex
	writeDeadline time.Time
	// the write deadline of each write as it started, in order
	writeStartDeadlines []time.Time
}

// See net.Conn.
func (self *testFrontStallingConn) SetDeadline(deadline time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeDeadline = deadline
	}()
	return self.Conn.SetDeadline(deadline)
}

// See net.Conn.
func (self *testFrontStallingConn) SetWriteDeadline(deadline time.Time) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeDeadline = deadline
	}()
	return self.Conn.SetWriteDeadline(deadline)
}

// Writes p, or once stalled waits for the write deadline, or the close, and
// fails; once wedged waits for the close and fails.
func (self *testFrontStallingConn) Write(p []byte) (int, error) {
	writeDeadline := func() time.Time {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeStartDeadlines = append(self.writeStartDeadlines, self.writeDeadline)
		return self.writeDeadline
	}()
	if self.wedged.Load() {
		<-self.closed
		return 0, net.ErrClosed
	}
	if !self.stalled.Load() {
		return self.Conn.Write(p)
	}
	var expired <-chan time.Time
	if !writeDeadline.IsZero() {
		expired = time.After(time.Until(writeDeadline))
	}
	select {
	case <-expired:
		return 0, os.ErrDeadlineExceeded
	case <-self.closed:
		return 0, net.ErrClosed
	}
}

// See net.Conn.
func (self *testFrontStallingConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return self.Conn.Close()
}

// The write deadline of every write so far as it started, in order.
func (self *testFrontStallingConn) startedWriteDeadlines() []time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.writeStartDeadlines)
}

// An idle keepalive connection whose client stopped reading closes once a
// write crypto/tls makes on its reading goroutine has stalled for
// send_timeout. net/http left that write no deadline, and its idle and
// header timeouts are read deadlines, which a goroutine blocked in a write
// never sees, so the connection would otherwise keep its goroutine, buffers
// and socket until the client closes it, where nginx's read timer closes it
// (frontTrackedConn). The write here is the alert answering a record that
// fails to decrypt; a client's KeyUpdate request asks for the same kind of
// write, as often as it likes.
func TestFrontHttpsClosesAnIdleConnectionWhoseTlsWriteStallsOnAClientThatStoppedReading(t *testing.T) {
	var idleConns *testFrontIdleConns
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		idleConns = newTestFrontIdleConns(env)
		env.settings.Front.Http.SendTimeout = 100 * time.Millisecond
	})
	client, stallingConn := idleConns.serveIdle(env)
	stallingConn.stalled.Store(true)
	// an application data record of 32 bytes that no key decrypts
	record := append([]byte{23, 3, 3, 0, 32}, make([]byte, 32)...)
	if _, err := client.conn.NetConn().Write(record); err != nil {
		t.Fatal(err)
	}
	testFrontWait(t, stallingConn.closed, "the close of the idle connection whose alert stalled")
	env.waitOpen(0)
}

// A connect websocket, upgraded as connect's handler upgrades it, whose
// client sends no auth frame and stops reading closes once a write
// crypto/tls makes on the reading goroutine has stalled for send_timeout.
// gorilla's upgrade clears the write deadline after its 101, as connect's
// H1+ upgrade does, and the auth frame's read deadline never reaches a
// goroutine blocked in a write, so an unauthenticated client would otherwise
// keep the connection and its limit slot until it closes them, where nginx
// bounds an upgraded connection's every blocked client write by send_timeout
// (frontTrackedConn). The write here is the alert answering a record that
// fails to decrypt; a client's KeyUpdate request asks for the same kind of
// write, as often as it likes.
func TestFrontClosesAnUnauthenticatedWebsocketWhoseTlsWriteStallsOnAClientThatStoppedReading(t *testing.T) {
	readEnded := make(chan error, 1)
	connectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			HandshakeTimeout: testFrontProgressTimeout,
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		// the auth frame's deadline, far past the test, so only the bound
		// of the write the read makes can end it
		ws.SetReadDeadline(time.Now().Add(time.Hour))
		_, _, err = ws.ReadMessage()
		readEnded <- err
	})
	accepted := make(chan *testFrontStallingConn, 16)
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.SendTimeout = 100 * time.Millisecond
		listen := env.settings.Listen
		env.settings.Listen = func(network string, address string) (net.Listener, error) {
			listener, err := listen(network, address)
			if err != nil {
				return listener, err
			}
			return &testFrontStallingListener{
				Listener: listener,
				accepted: accepted,
			}, nil
		}
	}, connectHandler)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	client := env.dialH1Client(env.bindAddress(address, frontPortHttps), testFrontConnectName)
	var stallingConn *testFrontStallingConn
	select {
	case stallingConn = <-accepted:
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("no connection was accepted")
	}
	// a front that never bounds the stalled write would otherwise hold the
	// test's own close of the front
	t.Cleanup(func() {
		stallingConn.Close()
	})
	fmt.Fprintf(client.conn, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", testFrontConnectName)
	response, err := http.ReadResponse(client.reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade = %d", response.StatusCode)
	}
	stallingConn.stalled.Store(true)
	// an application data record of 32 bytes that no key decrypts
	record := append([]byte{23, 3, 3, 0, 32}, make([]byte, 32)...)
	if _, err := client.conn.NetConn().Write(record); err != nil {
		t.Fatal(err)
	}
	testFrontWait(t, stallingConn.closed, "the close of the unauthenticated websocket whose alert stalled")
	select {
	case err := <-readEnded:
		if err == nil {
			t.Fatal("the auth read returned a message")
		}
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("the auth read never ended")
	}
	env.waitOpen(0)
}

// A response net/http writes for a handler that set no deadline is bounded
// by send_timeout, over TLS on 443 and plain on 80; so is a hijacked
// connection's write its owner left without one, while one its owner gave a
// deadline is written under that deadline alone (frontTrackedConn).
func TestFrontPublicTcpWritesWithoutADeadlineAreBoundedBySendTimeout(t *testing.T) {
	const sendTimeout = time.Hour
	ownerDeadline := time.Now().Add(2 * time.Hour)
	hijackingHandler := func(interfaceName string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/upgrade" {
				io.WriteString(w, "plain")
				return
			}
			conn, readWriter, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			readWriter.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test-owned\r\nConnection: Upgrade\r\n\r\n")
			if err := readWriter.Flush(); err != nil {
				return
			}
			conn.SetWriteDeadline(ownerDeadline)
			if _, err := io.WriteString(conn, "owned\n"); err != nil {
				return
			}
			// until the client closes
			io.Copy(io.Discard, readWriter)
		})
	}
	accepted := make(chan *testFrontStallingConn, 16)
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.SendTimeout = sendTimeout
		env.front.httpsHandler = hijackingHandler
		env.front.httpHandler = hijackingHandler
		listen := env.settings.Listen
		env.settings.Listen = func(network string, address string) (net.Listener, error) {
			listener, err := listen(network, address)
			if err != nil {
				return listener, err
			}
			return &testFrontStallingListener{
				Listener: listener,
				accepted: accepted,
			}, nil
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	bounded := func(deadline time.Time) bool {
		return sendTimeout/2 < time.Until(deadline) && !deadline.After(time.Now().Add(sendTimeout))
	}
	cases := []struct {
		portKind string
		dial     func() (net.Conn, error)
	}{
		{
			portKind: frontPortHttps,
			dial: func() (net.Conn, error) {
				return tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontConnectName, []string{"http/1.1"}))
			},
		},
		{
			portKind: frontPortHttp,
			dial: func() (net.Conn, error) {
				return net.Dial("tcp", env.bindAddress(address, frontPortHttp))
			},
		},
	}
	for _, c := range cases {
		conn, err := c.dial()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			conn.Close()
		})
		// bounds a broken test, never a passing one
		conn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
		var rawConn *testFrontStallingConn
		select {
		case rawConn = <-accepted:
		case <-time.After(testFrontProgressTimeout):
			t.Fatalf("%s: no connection was accepted", c.portKind)
		}
		reader := bufio.NewReader(conn)

		// past the TLS handshake, whose writes have its own deadline
		from := len(rawConn.startedWriteDeadlines())
		fmt.Fprintf(conn, "GET /plain HTTP/1.1\r\nHost: %s\r\n\r\n", testFrontConnectName)
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.portKind, err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: the plain request = %d", c.portKind, response.StatusCode)
		}
		responseDeadlines := rawConn.startedWriteDeadlines()[from:]
		if len(responseDeadlines) == 0 {
			t.Fatalf("%s: the response was never written", c.portKind)
		}
		for _, deadline := range responseDeadlines {
			if !bounded(deadline) {
				t.Errorf("%s: net/http's response started under the deadline %s, want send_timeout's bound, %s", c.portKind, testFrontDeadlineText(deadline), sendTimeout)
			}
		}

		from = len(rawConn.startedWriteDeadlines())
		fmt.Fprintf(conn, "GET /upgrade HTTP/1.1\r\nHost: %s\r\nUpgrade: test-owned\r\nConnection: Upgrade\r\n\r\n", testFrontConnectName)
		response, err = http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.portKind, err)
		}
		if response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("%s: the upgrade = %d", c.portKind, response.StatusCode)
		}
		if line, err := reader.ReadString('\n'); line != "owned\n" || err != nil {
			t.Fatalf("%s: the tunnel sent %q, %v", c.portKind, line, err)
		}
		tunnelDeadlines := rawConn.startedWriteDeadlines()[from:]
		if len(tunnelDeadlines) != 2 {
			t.Fatalf("%s: the tunnel made %d writes, want its 101 and its owned line", c.portKind, len(tunnelDeadlines))
		}
		if deadline := tunnelDeadlines[0]; !bounded(deadline) {
			t.Errorf("%s: the hijacked 101, left without a deadline, started under the deadline %s, want send_timeout's bound, %s", c.portKind, testFrontDeadlineText(deadline), sendTimeout)
		}
		if deadline := tunnelDeadlines[1]; !deadline.Equal(ownerDeadline) {
			t.Errorf("%s: the hijacked write its owner gave a deadline started under the deadline %s, want the owner's alone", c.portKind, testFrontDeadlineText(deadline))
		}
	}
}

// A TLS h1 client that stops reading has the nginx front's blocked write cut
// at send_timeout, and the handler returns, letting go of its limit slot,
// within about that (LBALT-IMPL.md §7). net/http then closes the TLS
// connection, and the close_notify crypto/tls sends first fails at once on
// the public connection, rather than wait out its own 5 s deadline. The
// client stops reading once it has the response head: its connection
// stalls, so every write to it waits out its deadline.
func TestFrontHttpsSendTimeoutReleasesTheHandlerOfAClientThatStopsReading(t *testing.T) {
	const sendTimeout = 100 * time.Millisecond
	// far under crypto/tls's 5 s close_notify deadline
	const releaseBound = 3 * time.Second
	type cut struct {
		err     error
		blocked time.Duration
	}
	stalled := make(chan struct{})
	cuts := make(chan cut, 1)
	connectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download" {
			io.WriteString(w, "ok")
			return
		}
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		select {
		case <-stalled:
		case <-r.Context().Done():
			return
		}
		writeStart := time.Now()
		_, err := w.Write(make([]byte, 16*1024))
		cuts <- cut{
			err:     err,
			blocked: time.Since(writeStart),
		}
	})
	accepted := make(chan *testFrontStallingConn, 16)
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.SendTimeout = sendTimeout
		// one request in flight per address, so a held slot refuses the next
		for _, routeInterface := range env.routeTable.Interfaces {
			routeInterface.RateLimit.NetConnections = 1
		}
		listen := env.settings.Listen
		env.settings.Listen = func(network string, address string) (net.Listener, error) {
			listener, err := listen(network, address)
			if err != nil {
				return listener, err
			}
			return &testFrontStallingListener{
				Listener: listener,
				accepted: accepted,
			}, nil
		}
	}, connectHandler)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	request := func(path string) *http.Response {
		t.Helper()
		tlsConn, err := tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontConnectName, []string{"http/1.1"}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			tlsConn.Close()
		})
		tlsConn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
		fmt.Fprintf(tlsConn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, testFrontConnectName)
		response, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	if response := request("/download"); response.StatusCode != http.StatusOK {
		t.Fatalf("the download = %d", response.StatusCode)
	}
	select {
	case conn := <-accepted:
		conn.stalled.Store(true)
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("no connection was accepted")
	}
	close(stalled)
	var cutWrite cut
	select {
	case cutWrite = <-cuts:
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("a write the client never reads was not cut")
	}
	if cutWrite.err == nil || cutWrite.blocked < sendTimeout || releaseBound <= cutWrite.blocked {
		t.Fatalf("the cut write returned %v after %s, want an error after SendTimeout %s, well under close_notify's 5 s", cutWrite.err, cutWrite.blocked, sendTimeout)
	}

	// the slot goes as the handler returns
	releaseDeadline := time.Now().Add(releaseBound)
	for {
		response := request("/ok")
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			break
		}
		if response.StatusCode != http.StatusTooManyRequests || releaseDeadline.Before(time.Now()) {
			t.Fatalf("the next request = %d, want 200 once the cut handler's slot is let go", response.StatusCode)
		}
	}
}

// Over h2, on a forwarded name, a client that stops reading at the socket
// while its flow-control windows stay open has its connection closed at
// send_timeout through connect's public sockets (LBALT-IMPL.md §7): the
// nginx front sets the h2 server's write byte timeout, and the public
// connection fails the close_notify crypto/tls sends next at once. The
// forwarded request lets go of its block's connection and its limit slot
// within about SendTimeout, where a stream's own deadline couldn't, since
// its reset waits behind the blocked write. The block sends its response
// head, and its body only once the client has the head and has stopped
// reading; the client's connection stalls then, so every write to it waits
// out its deadline.
func TestFrontHttpsSendTimeoutClosesAnH2ConnectionWhoseClientStopsReading(t *testing.T) {
	const sendTimeout = 100 * time.Millisecond
	// far under crypto/tls's 5 s close_notify deadline
	const releaseBound = 3 * time.Second
	type cut struct {
		err     error
		blocked time.Duration
	}
	stalled := make(chan struct{})
	upstreamCuts := make(chan cut, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		select {
		case <-stalled:
		case <-r.Context().Done():
			return
		}
		chunk := make([]byte, 16*1024)
		writeStart := time.Now()
		// at most 1 GiB, far past any buffer, so a body never cut still ends
		for range 64 * 1024 {
			if _, err := w.Write(chunk); err != nil {
				upstreamCuts <- cut{
					err:     err,
					blocked: time.Since(writeStart),
				}
				return
			}
		}
		upstreamCuts <- cut{}
	}))
	t.Cleanup(upstream.Close)
	upstreamPort := upstream.Listener.Addr().(*net.TCPAddr).Port
	connectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	accepted := make(chan *testFrontStallingConn, 16)
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.SendTimeout = sendTimeout
		// one request in flight per address, so a held slot refuses the next
		for _, routeInterface := range env.routeTable.Interfaces {
			routeInterface.RateLimit.NetConnections = 1
		}
		// the forwarded service's one block on loopback; a parsed route table
		// refuses the services bridge on an interface's address, as the
		// test's 127.0.0.1 is, so it is set after the parse
		env.routeTable.Upstream.Ipv4 = "127.0.0.1"
		forwardedService, ok := env.routeTable.Service("mcp")
		if !ok {
			t.Fatal("the route fixture has no mcp service")
		}
		forwardedService.Blocks = []*nginx.RouteBlock{
			{Name: "g1", Weight: 1, Port: upstreamPort},
		}
		listen := env.settings.Listen
		env.settings.Listen = func(network string, address string) (net.Listener, error) {
			listener, err := listen(network, address)
			if err != nil {
				return listener, err
			}
			return &testFrontStallingListener{
				Listener: listener,
				accepted: accepted,
			}, nil
		}
	}, connectHandler)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	request := func(path string) *http.Response {
		t.Helper()
		tlsConn, err := tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontConnectName, []string{"http/1.1"}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			tlsConn.Close()
		})
		tlsConn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
		fmt.Fprintf(tlsConn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, testFrontConnectName)
		response, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	client := env.dialH2Client(env.bindAddress(address, frontPortHttps), testFrontForwardedName)
	client.openWindows()
	streamId := client.startGet(testFrontForwardedName, "/download")
	if status := client.readResponseHead(streamId); status != strconv.Itoa(http.StatusOK) {
		t.Fatalf("the download: status %s", status)
	}
	select {
	case conn := <-accepted:
		conn.stalled.Store(true)
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("no connection was accepted")
	}
	close(stalled)
	select {
	case upstreamCut := <-upstreamCuts:
		if upstreamCut.err == nil {
			t.Fatal("1 GiB was relayed to a client that reads nothing")
		}
		if releaseBound <= upstreamCut.blocked {
			t.Fatalf("the block's connection was cut after %s, want about SendTimeout %s", upstreamCut.blocked, sendTimeout)
		}
	case <-time.After(releaseBound):
		t.Fatalf("after %s the response to a client that reads nothing still holds its block's connection", releaseBound)
	}
	// past what the server sent before the stall, the client finds its
	// connection's end, and no read deadline
	for {
		if _, err := client.framer.ReadFrame(); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("the connection is still open after the cut: %v", err)
			}
			break
		}
	}

	// the slot goes as the handler returns
	releaseDeadline := time.Now().Add(releaseBound)
	for {
		response := request("/ok")
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			break
		}
		if response.StatusCode != http.StatusTooManyRequests || releaseDeadline.Before(time.Now()) {
			t.Fatalf("the next request = %d, want 200 once the cut handler's slot is let go", response.StatusCode)
		}
	}
}
