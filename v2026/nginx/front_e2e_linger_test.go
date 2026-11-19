package nginx

// End-to-end tests of how an h1 connection ends when its answer leaves the
// request's body unread while the client is still sending it: the front's
// own answers (its errors, redirects, the CORS preflight and the lb status
// routes), connect in-process, the drain's and keepalive_requests'
// Connection: close, and a client that asked to close. Go's server must end
// such a connection with a half-close and a wait before the close
// (closeWriteAndWait), never close it at once, so the client has read the
// whole answer by the time the close resets the connection. A client that
// holds the rest of its body back must still get connect's in-process
// answer, and the end of the server's side, once LingeringTimeout cuts Go's
// read of the body.
//
// The servers are built as connect builds its public ones: the front's
// handlers under Front.ConfigureServer, TLS through tls.NewListener with the
// front's one config, plain TCP for port 80, over listeners whose
// connections record how the server ends them. Each client writes its head
// and the first 64 KiB of the body it keeps sending at once, so some of it
// is unread whenever the server closes, then keeps writing while it reads;
// or the head and a body's first bytes, holding the rest back.

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// How long Go's h1 server waits between the half-close and the close of a
// connection it ends with a request body unread: net/http's
// rstAvoidanceDelay. A close at once comes within microseconds of the last
// write, so half of it tells the two apart.
const e2eRstAvoidanceDelay = 500 * time.Millisecond

// A listener whose connections record how the server ends them, each known
// by the client's address. Safe for concurrent use.
type e2eLingerListener struct {
	net.Listener

	stateLock sync.Mutex
	addrConns map[string]*e2eLingerConn
}

// See net.Listener.
func (self *e2eLingerListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	lingerConn := &e2eLingerConn{
		Conn:   conn,
		closed: make(chan struct{}),
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.addrConns[conn.RemoteAddr().String()] = lingerConn
	}()
	return lingerConn, nil
}

// The server's end of one connection, recording when its last write
// returned, when it half-closed and when it closed. A half-close reaches the
// socket, as a *net.TCPConn's does; under TLS nothing calls it, the
// half-close being a close_notify write. Safe for concurrent use as a
// net.Conn is.
type e2eLingerConn struct {
	net.Conn
	closed    chan struct{}
	closeOnce sync.Once

	stateLock      sync.Mutex
	lastWriteTime  time.Time
	closeWriteTime time.Time
	closeTime      time.Time
}

// See net.Conn.
func (self *e2eLingerConn) Write(p []byte) (int, error) {
	n, err := self.Conn.Write(p)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastWriteTime = time.Now()
	}()
	return n, err
}

// Closes the write side, as net/http's closeWriteAndWait asks of a socket
// that can.
func (self *e2eLingerConn) CloseWrite() error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closeWriteTime = time.Now()
	}()
	closeWriter, ok := self.Conn.(interface{ CloseWrite() error })
	if !ok {
		return nil
	}
	return closeWriter.CloseWrite()
}

// See net.Conn.
func (self *e2eLingerConn) Close() error {
	self.closeOnce.Do(func() {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.closeTime = time.Now()
		}()
		close(self.closed)
	})
	return self.Conn.Close()
}

// Serves handler on a new recording listener on loopback, as connect serves
// a public port: TLS through tls.NewListener with the front's one config
// when overTls is set. The listener and the server close when the test ends.
func serveE2eLinger(t *testing.T, e2e *e2eFront, handler http.Handler, overTls bool) (*e2eLingerListener, *http.Server) {
	t.Helper()
	tcpListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &e2eLingerListener{
		Listener:  tcpListener,
		addrConns: map[string]*e2eLingerConn{},
	}
	httpServer := newE2eHttpServer(t.Context(), e2e.front, handler, e2e.errorLog)
	t.Cleanup(func() {
		httpServer.Close()
		tcpListener.Close()
	})
	var served net.Listener = listener
	if overTls {
		served = tls.NewListener(listener, e2e.front.TlsConfig())
	}
	go httpServer.Serve(served)
	return listener, httpServer
}

// One h1 request whose answer leaves the body it declares unread.
type e2eLingerCase struct {
	what string
	// the request line and headers, but the Content-Length
	headLines []string
	// the declared body's length; -1 for none, for a body whose framing
	// headLines declare, such as a chunked one, which only a client that
	// holds its body back sends
	contentLength int64
	// the declared body goes whole with the head, followed by a pipelined
	// request whose own body the client keeps sending; otherwise the client
	// keeps sending the declared body
	pipelined bool
	// the client sends the head and heldBackBodyPrefix, then holds the rest
	// of its body back: it writes nothing more while it reads the answer and
	// the end of the server's side
	heldBack bool
	// with heldBack, the body's first bytes, framed as the head declares
	heldBackBodyPrefix string
	// the answer's handler reads the body whole, so the connection closes
	// at once, with no half-close, as nothing of it is left to arrive
	readWhole  bool
	statusCode int
	// the answer's whole body
	body string
}

// The body a pipelined request declares, which its client keeps sending.
const e2eLingerPipelinedContentLength = 16 * 1024 * 1024

// What a case's client read.
type e2eLingerOutcome struct {
	// the client's end of the connection, the address the server's listener
	// knows it by
	localAddr string
	response  *http.Response
	body      []byte
	// the read after the answer: io.EOF once the server ended its side
	// cleanly, with a FIN or a TLS close_notify
	endErr error
	err    error
}

// Sends every case at once, each on a connection of its own, then fails the
// test unless each client read its whole answer, Connection: close
// included, and the end of the server's side, and the server ended each
// connection with Go's wait, at least half of its rstAvoidanceDelay after
// its last write, the answer or the TLS close_notify, half-closing first
// when socketHalfCloses is set, as on plain TCP. A case whose body is read
// whole must close with no half-close there.
//
// Each client writes the head and the first 64 KiB of the body it keeps
// sending at once, then keeps writing that body on its own goroutine while
// it reads the answer, its body, and once more, for the end of the server's
// side. One that holds its body back writes the head and the body's first
// bytes, and nothing more.
func runE2eLingerCases(t *testing.T, listener *e2eLingerListener, dial func(ctx context.Context) (net.Conn, error), cases []*e2eLingerCase, socketHalfCloses bool) {
	t.Helper()
	// on a goroutine of its own, so it reports rather than fails
	send := func(c *e2eLingerCase) *e2eLingerOutcome {
		outcome := &e2eLingerOutcome{}
		conn, err := dial(t.Context())
		if err != nil {
			outcome.err = err
			return outcome
		}
		// bounds a broken test, never a passing one
		conn.SetDeadline(time.Now().Add(e2eWaitTimeout))
		outcome.localAddr = conn.LocalAddr().String()

		headBytes := func(headLines []string, contentLength int64) []byte {
			head := strings.Join(headLines, "\r\n") + "\r\n"
			if 0 <= contentLength {
				head += "Content-Length: " + strconv.FormatInt(contentLength, 10) + "\r\n"
			}
			return []byte(head + "\r\n")
		}
		firstWrite := headBytes(c.headLines, c.contentLength)
		// the body the client keeps sending
		sentBytes := c.contentLength
		switch {
		case c.heldBack:
			firstWrite = append(firstWrite, c.heldBackBodyPrefix...)
			sentBytes = 0
		case c.pipelined:
			firstWrite = append(firstWrite, e2eBody(int(c.contentLength))...)
			firstWrite = append(firstWrite, headBytes([]string{"POST /pipelined HTTP/1.1", "Host: test-lb.service.example"}, e2eLingerPipelinedContentLength)...)
			sentBytes = e2eLingerPipelinedContentLength
		}
		firstBytes := min(64*1024, sentBytes)
		if _, err := conn.Write(append(firstWrite, e2eBody(int(firstBytes))...)); err != nil {
			conn.Close()
			outcome.err = err
			return outcome
		}
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			chunk := e2eBody(32 * 1024)
			for written := firstBytes; written < sentBytes; {
				n, err := conn.Write(chunk[:min(int64(len(chunk)), sentBytes-written)])
				written += int64(n)
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
			outcome.err = err
			return outcome
		}
		outcome.response = response
		outcome.body, err = io.ReadAll(response.Body)
		if err != nil {
			outcome.err = err
			return outcome
		}
		_, outcome.endErr = reader.ReadByte()
		return outcome
	}
	outcomes := make([]*e2eLingerOutcome, len(cases))
	var sends sync.WaitGroup
	for i, c := range cases {
		sends.Add(1)
		go func() {
			defer sends.Done()
			outcomes[i] = send(c)
		}()
	}
	sends.Wait()

	for i, c := range cases {
		outcome := outcomes[i]
		if outcome.err != nil {
			t.Errorf("%s: %v", c.what, outcome.err)
			continue
		}
		if outcome.response.StatusCode != c.statusCode || string(outcome.body) != c.body || !outcome.response.Close {
			t.Errorf("%s = %d %q, closing %t; want %d %q, closing", c.what, outcome.response.StatusCode, outcome.body, outcome.response.Close, c.statusCode, c.body)
		}
		if !errors.Is(outcome.endErr, io.EOF) {
			t.Errorf("%s: after the answer, %v; want the server's side to end cleanly", c.what, outcome.endErr)
		}
		// a client that read an answer was accepted before it was answered
		serverConn := func() *e2eLingerConn {
			listener.stateLock.Lock()
			defer listener.stateLock.Unlock()
			return listener.addrConns[outcome.localAddr]
		}()
		if serverConn == nil {
			t.Errorf("%s: the server's listener has no connection from %s", c.what, outcome.localAddr)
			continue
		}
		select {
		case <-serverConn.closed:
		case <-time.After(e2eWaitTimeout):
			t.Errorf("%s: the server never closed the connection", c.what)
			continue
		}
		var lastWriteTime, closeWriteTime, closeTime time.Time
		func() {
			serverConn.stateLock.Lock()
			defer serverConn.stateLock.Unlock()
			lastWriteTime = serverConn.lastWriteTime
			closeWriteTime = serverConn.closeWriteTime
			closeTime = serverConn.closeTime
		}()
		if c.readWhole {
			// Go's server closes at once a connection whose request left
			// nothing unread, unless something marked it
			if socketHalfCloses && !closeWriteTime.IsZero() {
				t.Errorf("%s: the server half-closed and waited, though the body was read whole", c.what)
			}
			continue
		}
		if socketHalfCloses && closeWriteTime.IsZero() {
			t.Errorf("%s: the server closed with no half-close first", c.what)
		}
		if wait := closeTime.Sub(lastWriteTime); wait < e2eRstAvoidanceDelay/2 {
			t.Errorf("%s: the server closed %s after its last write; want Go's half-close, then its %s wait", c.what, wait, e2eRstAvoidanceDelay)
		}
	}
}

// The text of the front's own answer for a status.
func e2eOwnAnswerBody(statusCode int) string {
	return http.StatusText(statusCode) + "\n"
}

// Every answer the front writes itself on h1 without reading the body, to a
// client that asked to close the connection and keeps sending its body:
// over TLS its errors 400, 403, 404, 405, 413, 414 and 429, a redirect
// alias's 301, the CORS preflight's 204, the lb status json, connect
// in-process, and an HTTP/1.0 request, with send_timeout on and off; over
// plain TCP, port 80's 301, 403, 413 and lb status. Each client reads the
// whole answer and the end of the server's side, and the server half-closes
// and waits before it closes, but for a forwarded request whose body was
// read whole, which has nothing left to arrive and closes at once. Go's
// server reads none of the body of a client that asked to close, and
// without the front's mark it closes at once, a reset that a client still
// writing, such as Go's own, can report in place of the answer.
func TestFrontE2eOwnAnswersLingerForABodyStillArriving(t *testing.T) {
	e2e := newE2eFront(t, nil, func(routeTable *RouteTable) {
		// eth2 admits one request a minute, so a second gets the 429
		for _, routeInterface := range routeTable.Interfaces {
			if routeInterface.Name == "eth2" {
				routeInterface.RateLimit.RequestsPerMinute = 1
				routeInterface.RateLimit.Burst = 0
			}
		}
	})
	const contentLength = 16 * 1024 * 1024
	const overTheCapContentLength = 32 * 1024 * 1024
	dialTls := func(e2e *e2eFront, listener *e2eLingerListener) func(ctx context.Context) (net.Conn, error) {
		return func(ctx context.Context) (net.Conn, error) {
			return e2e.dialTls(ctx, listener.Addr().String(), "", []string{"http/1.1"})
		}
	}

	httpsListener, _ := serveE2eLinger(t, e2e, e2e.front.HttpsHandler("eth1"), true)
	limitedListener, _ := serveE2eLinger(t, e2e, e2e.front.HttpsHandler("eth2"), true)
	// the one request eth2 admits
	if response, _ := e2e.dialRaw(t, limitedListener.Addr().String(), "").exchange(t, "GET /hidden-example/missing HTTP/1.1", "Host: test-lb.service.example"); response.StatusCode != http.StatusNotFound {
		t.Fatalf("eth2's first request = %d, want 404", response.StatusCode)
	}
	runE2eLingerCases(t, limitedListener, dialTls(e2e, limitedListener), []*e2eLingerCase{
		{what: "tls 429", headLines: []string{"POST /hidden-example/missing HTTP/1.1", "Host: test-lb.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusTooManyRequests, body: e2eOwnAnswerBody(http.StatusTooManyRequests)},
	}, false)
	runE2eLingerCases(t, httpsListener, dialTls(e2e, httpsListener), []*e2eLingerCase{
		{what: "tls 400", headLines: []string{"Post /upload HTTP/1.1", "Host: api.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusBadRequest, body: e2eOwnAnswerBody(http.StatusBadRequest)},
		{what: "tls 403", headLines: []string{"POST /status HTTP/1.1", "Host: api.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusForbidden, body: e2eOwnAnswerBody(http.StatusForbidden)},
		{what: "tls 404", headLines: []string{"POST /hidden-example/missing HTTP/1.1", "Host: test-lb.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusNotFound, body: e2eOwnAnswerBody(http.StatusNotFound)},
		{what: "tls 405", headLines: []string{"POST /hidden-example/status HTTP/1.1", "Host: test-lb.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusMethodNotAllowed, body: e2eOwnAnswerBody(http.StatusMethodNotAllowed)},
		{what: "tls 413", headLines: []string{"POST /upload HTTP/1.1", "Host: api.service.example", "Connection: close"}, contentLength: overTheCapContentLength, statusCode: http.StatusRequestEntityTooLarge, body: e2eOwnAnswerBody(http.StatusRequestEntityTooLarge)},
		{what: "tls 414", headLines: []string{"POST /" + strings.Repeat("u", 9*1024) + " HTTP/1.1", "Host: api.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusRequestURITooLong, body: e2eOwnAnswerBody(http.StatusRequestURITooLong)},
		{what: "tls 301", headLines: []string{"POST /upload HTTP/1.1", "Host: old-api.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusMovedPermanently, body: e2eOwnAnswerBody(http.StatusMovedPermanently)},
		{what: "tls preflight", headLines: []string{"OPTIONS /upload HTTP/1.1", "Host: api.service.example", "Origin: https://app.service.example", "Access-Control-Request-Method: POST", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusNoContent, body: ""},
		{what: "tls lb status", headLines: []string{"GET /hidden-example/status HTTP/1.1", "Host: test-lb.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusOK, body: e2eLbStatusBody},
		{what: "tls in-process", headLines: []string{"POST /upload HTTP/1.1", "Host: connect.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusOK, body: "connect"},
		{what: "tls http/1.0", headLines: []string{"POST /hidden-example/missing HTTP/1.0", "Host: test-lb.service.example"}, contentLength: contentLength, statusCode: http.StatusNotFound, body: e2eOwnAnswerBody(http.StatusNotFound)},
	}, false)

	httpListener, _ := serveE2eLinger(t, e2e, e2e.front.HttpHandler("eth1"), false)
	dialPlain := func(ctx context.Context) (net.Conn, error) {
		dialer := &net.Dialer{}
		return dialer.DialContext(ctx, "tcp", httpListener.Addr().String())
	}
	runE2eLingerCases(t, httpListener, dialPlain, []*e2eLingerCase{
		{what: "http 301", headLines: []string{"POST /upload HTTP/1.1", "Host: api.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusMovedPermanently, body: e2eOwnAnswerBody(http.StatusMovedPermanently)},
		{what: "http 403", headLines: []string{"POST /upload HTTP/1.1", "Host: unknown.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusForbidden, body: e2eOwnAnswerBody(http.StatusForbidden)},
		{what: "http 413", headLines: []string{"POST /upload HTTP/1.1", "Host: test-lb.service.example", "Connection: close"}, contentLength: overTheCapContentLength, statusCode: http.StatusRequestEntityTooLarge, body: e2eOwnAnswerBody(http.StatusRequestEntityTooLarge)},
		{what: "http lb status", headLines: []string{"GET /hidden-example/status HTTP/1.1", "Host: test-lb.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusOK, body: e2eLbStatusBody},
		// forwarded, its body read whole on the way
		{what: "http block status", headLines: []string{"POST /hidden-example/by/b/api/g1/status HTTP/1.1", "Host: test-lb.service.example", "Connection: close"}, contentLength: 10, readWhole: true, statusCode: http.StatusOK, body: "api/g1"},
	}, true)

	// the body is watched with send_timeout off too
	noSendTimeoutSettings := e2eFrontSettings(t)
	noSendTimeoutSettings.Http.SendTimeout = 0
	noSendTimeoutE2e := newE2eFront(t, noSendTimeoutSettings, nil)
	noSendTimeoutListener, _ := serveE2eLinger(t, noSendTimeoutE2e, noSendTimeoutE2e.front.HttpsHandler("eth1"), true)
	runE2eLingerCases(t, noSendTimeoutListener, dialTls(noSendTimeoutE2e, noSendTimeoutListener), []*e2eLingerCase{
		{what: "tls 404 with no send_timeout", headLines: []string{"POST /hidden-example/missing HTTP/1.1", "Host: test-lb.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusNotFound, body: e2eOwnAnswerBody(http.StatusNotFound)},
	}, false)
}

// An answer the front closes the connection with, to a client that asked
// for nothing, ends it with Go's half-close and wait too, whether a body the
// answer left unread is still arriving or is whole and followed by a
// pipelined request whose body is: the front's own closing statuses, the
// drain's close, keepalive_requests', keepalive_timeout 0, and the closed
// front's 503 to a client that asked to close. Go's server reads none of
// the body before such an answer; it waits after it for a body over 256 KiB
// unread, but a smaller one it reads whole and then closes at once, with the
// pipelined request's bytes unread, unless the front marks the answer.
func TestFrontE2eClosingAnswersLingerForABodyStillArriving(t *testing.T) {
	const contentLength = 16 * 1024 * 1024
	const wholeContentLength = 10
	dialTls := func(e2e *e2eFront, listener *e2eLingerListener) func(ctx context.Context) (net.Conn, error) {
		return func(ctx context.Context) (net.Conn, error) {
			return e2e.dialTls(ctx, listener.Addr().String(), "", []string{"http/1.1"})
		}
	}

	e2e := newE2eFront(t, nil, nil)
	listener, _ := serveE2eLinger(t, e2e, e2e.front.HttpsHandler("eth1"), true)
	runE2eLingerCases(t, listener, dialTls(e2e, listener), []*e2eLingerCase{
		{what: "413", headLines: []string{"POST /upload HTTP/1.1", "Host: api.service.example"}, contentLength: 32 * 1024 * 1024, statusCode: http.StatusRequestEntityTooLarge, body: e2eOwnAnswerBody(http.StatusRequestEntityTooLarge)},
		{what: "400", headLines: []string{"Post /upload HTTP/1.1", "Host: api.service.example"}, contentLength: contentLength, statusCode: http.StatusBadRequest, body: e2eOwnAnswerBody(http.StatusBadRequest)},
		{what: "400, then pipelined", headLines: []string{"Post /upload HTTP/1.1", "Host: api.service.example"}, contentLength: wholeContentLength, pipelined: true, statusCode: http.StatusBadRequest, body: e2eOwnAnswerBody(http.StatusBadRequest)},
	}, false)
	httpListener, _ := serveE2eLinger(t, e2e, e2e.front.HttpHandler("eth1"), false)
	dialPlain := func(ctx context.Context) (net.Conn, error) {
		dialer := &net.Dialer{}
		return dialer.DialContext(ctx, "tcp", httpListener.Addr().String())
	}
	runE2eLingerCases(t, httpListener, dialPlain, []*e2eLingerCase{
		{what: "http 413", headLines: []string{"POST /upload HTTP/1.1", "Host: test-lb.service.example"}, contentLength: 32 * 1024 * 1024, statusCode: http.StatusRequestEntityTooLarge, body: e2eOwnAnswerBody(http.StatusRequestEntityTooLarge)},
		{what: "http 400, then pipelined", headLines: []string{"Post /upload HTTP/1.1", "Host: test-lb.service.example"}, contentLength: wholeContentLength, pipelined: true, statusCode: http.StatusBadRequest, body: e2eOwnAnswerBody(http.StatusBadRequest)},
	}, true)

	keepaliveSettings := e2eFrontSettings(t)
	keepaliveSettings.Http.KeepaliveRequests = 1
	keepaliveE2e := newE2eFront(t, keepaliveSettings, nil)
	keepaliveListener, _ := serveE2eLinger(t, keepaliveE2e, keepaliveE2e.front.HttpsHandler("eth1"), true)
	runE2eLingerCases(t, keepaliveListener, dialTls(keepaliveE2e, keepaliveListener), []*e2eLingerCase{
		{what: "keepalive_requests lb status", headLines: []string{"GET /hidden-example/status HTTP/1.1", "Host: test-lb.service.example"}, contentLength: contentLength, statusCode: http.StatusOK, body: e2eLbStatusBody},
		{what: "keepalive_requests lb status, then pipelined", headLines: []string{"GET /hidden-example/status HTTP/1.1", "Host: test-lb.service.example"}, contentLength: wholeContentLength, pipelined: true, statusCode: http.StatusOK, body: e2eLbStatusBody},
	}, false)

	keepaliveOffSettings := e2eFrontSettings(t)
	keepaliveOffSettings.Http.KeepaliveTimeout = 0
	keepaliveOffE2e := newE2eFront(t, keepaliveOffSettings, nil)
	keepaliveOffListener, _ := serveE2eLinger(t, keepaliveOffE2e, keepaliveOffE2e.front.HttpsHandler("eth1"), true)
	runE2eLingerCases(t, keepaliveOffListener, dialTls(keepaliveOffE2e, keepaliveOffListener), []*e2eLingerCase{
		{what: "keepalive_timeout 0 404, then pipelined", headLines: []string{"POST /hidden-example/missing HTTP/1.1", "Host: test-lb.service.example"}, contentLength: wholeContentLength, pipelined: true, statusCode: http.StatusNotFound, body: e2eOwnAnswerBody(http.StatusNotFound)},
	}, false)

	drainE2e := newE2eFront(t, nil, nil)
	drainListener, drainServer := serveE2eLinger(t, drainE2e, drainE2e.front.HttpsHandler("eth1"), true)
	// connect's drain: the front's, and the server's keepalives off
	drainE2e.front.BeginDrain()
	drainServer.SetKeepAlivesEnabled(false)
	runE2eLingerCases(t, drainListener, dialTls(drainE2e, drainListener), []*e2eLingerCase{
		{what: "draining 404", headLines: []string{"POST /hidden-example/missing HTTP/1.1", "Host: test-lb.service.example"}, contentLength: contentLength, statusCode: http.StatusNotFound, body: e2eOwnAnswerBody(http.StatusNotFound)},
		{what: "draining 404, then pipelined", headLines: []string{"POST /hidden-example/missing HTTP/1.1", "Host: test-lb.service.example"}, contentLength: wholeContentLength, pipelined: true, statusCode: http.StatusNotFound, body: e2eOwnAnswerBody(http.StatusNotFound)},
	}, false)
	drainE2e.front.Close()
	runE2eLingerCases(t, drainListener, dialTls(drainE2e, drainListener), []*e2eLingerCase{
		{what: "closed 503", headLines: []string{"POST /hidden-example/missing HTTP/1.1", "Host: test-lb.service.example", "Connection: close"}, contentLength: contentLength, statusCode: http.StatusServiceUnavailable, body: e2eOwnAnswerBody(http.StatusServiceUnavailable)},
	}, false)
}

// Connect in-process answers without reading a body it has no use for, and
// a client that holds the rest of its body back still gets the answer and
// the end of the server's side: Go's server reads what is left of the body
// before an answer that keeps the connection, or after one that closes it,
// and the front bounds that read by LingeringTimeout once connect's handler
// returns, as before its own answers. Over TLS, as connect serves its names:
// keep-alive requests, with a declared body and a chunked one, answered once
// the bound cuts the read, with Connection: close; and answers that close the
// connection, whose ends follow the bound: a client that expects
// 100-continue, one that asked to close with a chunked body, of which Go
// reads up to 256 KiB where it reads none of a declared one, the drain's
// close and keepalive_requests'. Each server ends the connection with Go's
// half-close and wait, the read having been cut. Unbounded, the answer, or
// the end, would wait on the client for as long as it stays connected.
func TestFrontE2eConnectInProcessBoundsTheReadOfAHeldBackBody(t *testing.T) {
	const declaredContentLength = 1000
	const declaredPrefix = "0123456789"
	// one chunk of ten bytes
	const chunkedPrefix = "a\r\n0123456789\r\n"
	newSettings := func() *FrontSettings {
		settings := e2eFrontSettings(t)
		// so each client waits little for the bound
		settings.Http.LingeringTimeout = 200 * time.Millisecond
		return settings
	}
	dialTls := func(e2e *e2eFront, listener *e2eLingerListener) func(ctx context.Context) (net.Conn, error) {
		return func(ctx context.Context) (net.Conn, error) {
			return e2e.dialTls(ctx, listener.Addr().String(), "", []string{"http/1.1"})
		}
	}

	e2e := newE2eFront(t, newSettings(), nil)
	listener, _ := serveE2eLinger(t, e2e, e2e.front.HttpsHandler("eth1"), true)
	runE2eLingerCases(t, listener, dialTls(e2e, listener), []*e2eLingerCase{
		{what: "keep-alive", headLines: []string{"POST /upload HTTP/1.1", "Host: connect.service.example"}, contentLength: declaredContentLength, heldBack: true, heldBackBodyPrefix: declaredPrefix, statusCode: http.StatusOK, body: "connect"},
		{what: "keep-alive chunked", headLines: []string{"POST /upload HTTP/1.1", "Host: connect.service.example", "Transfer-Encoding: chunked"}, contentLength: -1, heldBack: true, heldBackBodyPrefix: chunkedPrefix, statusCode: http.StatusOK, body: "connect"},
		{what: "100-continue", headLines: []string{"POST /upload HTTP/1.1", "Host: connect.service.example", "Expect: 100-continue"}, contentLength: declaredContentLength, heldBack: true, statusCode: http.StatusOK, body: "connect"},
		{what: "client close chunked", headLines: []string{"POST /upload HTTP/1.1", "Host: connect.service.example", "Connection: close", "Transfer-Encoding: chunked"}, contentLength: -1, heldBack: true, heldBackBodyPrefix: chunkedPrefix, statusCode: http.StatusOK, body: "connect"},
	}, false)

	keepaliveSettings := newSettings()
	keepaliveSettings.Http.KeepaliveRequests = 1
	keepaliveE2e := newE2eFront(t, keepaliveSettings, nil)
	keepaliveListener, _ := serveE2eLinger(t, keepaliveE2e, keepaliveE2e.front.HttpsHandler("eth1"), true)
	runE2eLingerCases(t, keepaliveListener, dialTls(keepaliveE2e, keepaliveListener), []*e2eLingerCase{
		{what: "keepalive_requests", headLines: []string{"POST /upload HTTP/1.1", "Host: connect.service.example"}, contentLength: declaredContentLength, heldBack: true, heldBackBodyPrefix: declaredPrefix, statusCode: http.StatusOK, body: "connect"},
	}, false)

	drainE2e := newE2eFront(t, newSettings(), nil)
	drainListener, drainServer := serveE2eLinger(t, drainE2e, drainE2e.front.HttpsHandler("eth1"), true)
	// connect's drain: the front's, and the server's keepalives off
	drainE2e.front.BeginDrain()
	drainServer.SetKeepAlivesEnabled(false)
	runE2eLingerCases(t, drainListener, dialTls(drainE2e, drainListener), []*e2eLingerCase{
		{what: "draining", headLines: []string{"POST /upload HTTP/1.1", "Host: connect.service.example"}, contentLength: declaredContentLength, heldBack: true, heldBackBodyPrefix: declaredPrefix, statusCode: http.StatusOK, body: "connect"},
	}, false)
}
