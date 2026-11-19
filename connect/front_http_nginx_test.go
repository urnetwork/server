package connect

// The front's tcp servers as the nginx front configures them: connect sets
// only the handler, the error log and the serving context, and the nginx
// front sets the rest (nginx.Front.ConfigureServer). Through connect's own
// public sockets and the real nginx front, nginx's keepalive_requests and
// keepalive_time close connections on h1 and h2, on the lb status route of
// an lb name, which no limit charges; and a client that stops reading over
// TLS, on h1 or h2, is cut at send_timeout without close_notify holding its
// handler.

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
// Safe for concurrent use as a net.Conn is.
type testFrontStallingConn struct {
	net.Conn
	stalled   atomic.Bool
	closed    chan struct{}
	closeOnce sync.Once

	stateLock     sync.Mutex
	writeDeadline time.Time
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
// fails.
func (self *testFrontStallingConn) Write(p []byte) (int, error) {
	if !self.stalled.Load() {
		return self.Conn.Write(p)
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
