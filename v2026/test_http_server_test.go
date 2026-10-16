// Forces http fixture ownership boundaries with local sockets and explicit
// barriers, including hijacks that the standard server no longer tracks.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Publishes the first bounded wait without delaying it or changing cancellation.
type testHttpWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

// Observes closure of a real listener and optionally injects a retained error
// after retiring the underlying socket, without relying on a failed Accept.
type testHttpObservedListener struct {
	net.Listener
	closed     chan struct{}
	once       sync.Once
	closeCalls atomic.Int32
	closeErr   error
}

// Publishes the completed underlying close and its deliberately injected result.
func (self *testHttpObservedListener) Close() error {
	self.closeCalls.Add(1)
	err := self.Listener.Close()
	self.once.Do(func() { close(self.closed) })
	if self.closeErr != nil {
		return self.closeErr
	}
	return err
}

// Every acquisition retains a fresh bounded rescue join, including assertion
// failures before a request starts. Expected setup/close errors remain evidence;
// they do not permit the serve loop or admitted handlers to escape the test.
func testHttpRescueJoin(t *testing.T, owner *TestHttpServer, allowFailure bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := owner.CloseAndWait(ctx); err != nil && (!allowFailure || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
		t.Errorf("rescue close: %v", err)
	}
	owners := func() []<-chan struct{} {
		owner.stateLock.Lock()
		defer owner.stateLock.Unlock()
		return []<-chan struct{}{owner.serveDone, owner.connectionIdle, owner.idle}
	}()
	for _, done := range owners {
		select {
		case <-done:
			continue
		default:
		}
		select {
		case <-done:
		case <-ctx.Done():
			t.Error("rescue did not join an owned http worker")
		}
	}
	hijackCloseIdle := func() <-chan struct{} {
		owner.stateLock.Lock()
		defer owner.stateLock.Unlock()
		return owner.hijackCloseIdle
	}()
	select {
	case <-hijackCloseIdle:
		return
	default:
	}
	select {
	case <-hijackCloseIdle:
	case <-ctx.Done():
		t.Error("rescue did not join an owned hijack close")
	}
}

// ServeTLS can fail before it calls Serve or registers the listener. Taking
// listener ownership therefore requires closing it before publishing ServeDone.
func TestTestHttpServerClosesListenerAfterTlsSetupFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	observed := &testHttpObservedListener{Listener: listener, closed: make(chan struct{})}
	owner := NewTestHttpServer(ctx, observed, &http.Server{TLSConfig: &tls.Config{}, Handler: http.NotFoundHandler()})
	defer testHttpRescueJoin(t, owner, true)
	select {
	case <-owner.serveDone:
	case <-ctx.Done():
		t.Fatal("invalid tls setup did not finish")
	}
	if owner.serveErr == nil {
		t.Fatal("invalid tls config unexpectedly started")
	}
	select {
	case <-observed.closed:
	default:
		t.Error("ServeTLS published completion while its pre-Serve listener remained open")
	}
	if err := owner.CloseAndWait(ctx); err == nil {
		t.Error("tls setup failure was lost")
	}
	select {
	case <-observed.closed:
	default:
		t.Error("CloseAndWait did not retire the unregistered tls listener")
	}
}

// A listener close error must survive subsequent cleanup calls even when the
// standard server later reports only ErrServerClosed. Exactly one owner retires
// the supplied listener despite Shutdown, Serve's defer and explicit retries.
func TestTestHttpServerRetainsListenerCloseFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	sentinel := errors.New("synthetic listener close failure")
	observed := &testHttpObservedListener{Listener: listener, closed: make(chan struct{}), closeErr: sentinel}
	owner := NewTestHttpServer(ctx, observed, &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })})
	defer testHttpRescueJoin(t, owner, true)
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listener.Addr().String(), nil)
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if err := owner.CloseAndWait(ctx); !errors.Is(err, sentinel) {
		t.Errorf("first close did not retain listener failure: %v", err)
	}
	if err := owner.CloseAndWait(ctx); !errors.Is(err, sentinel) {
		t.Errorf("retry erased listener failure: %v", err)
	}
	if calls := observed.closeCalls.Load(); calls != 1 {
		t.Errorf("listener closed %d times, want one owned close", calls)
	}
}

// A tls config callback runs before handler admission. Socket and Serve closure
// must not release that accepted connection while its callback still owns work.
func TestTestHttpServerJoinsTlsCallbackAfterSocketClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release, callbackDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	closedState := make(chan struct{})
	httpServer := &http.Server{
		Handler:  http.NotFoundHandler(),
		ErrorLog: log.New(io.Discard, "", 0),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				close(closedState)
			}
		},
		TLSConfig: &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			defer close(callbackDone)
			close(entered)
			<-release
			return nil, errors.New("synthetic tls callback released")
		}},
	}
	owner := NewTestHttpServer(ctx, listener, httpServer)
	defer testHttpRescueJoin(t, owner, false)
	defer unblock()
	conn, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := tls.Client(conn, &tls.Config{ServerName: "fixture.example", InsecureSkipVerify: true})
	clientDone := make(chan struct{})
	go func() { defer close(clientDone); _ = client.HandshakeContext(ctx) }()
	defer func() {
		unblock()
		conn.Close()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for _, done := range []<-chan struct{}{callbackDone, clientDone, closedState} {
			select {
			case <-done:
			case <-joinCtx.Done():
				t.Error("tls callback, client or connection did not join")
			}
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("actual tls config callback did not start")
	}
	if err := httpServer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owner.serveDone:
	case <-ctx.Done():
		t.Fatal("closed tls Serve loop did not return")
	}
	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := owner.CloseAndWait(expired); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "http connections did not join") {
		t.Errorf("socket/Serve completion bypassed held tls config callback: %v", err)
	}
	unblock()
	if err := owner.CloseAndWait(ctx); err != nil {
		t.Errorf("released tls connection did not join: %v", err)
	}
}

// Preserved ConnState hooks remain part of the accepted connection's lifetime;
// terminal state publication cannot race ahead of a still-running prior hook.
func TestTestHttpServerJoinsPreservedClosedStateCallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release, callbackDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	httpServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				defer close(callbackDone)
				close(entered)
				<-release
			}
		},
	}
	owner := NewTestHttpServer(ctx, listener, httpServer)
	defer testHttpRescueJoin(t, owner, false)
	defer unblock()
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listener.Addr().String(), nil)
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("preserved terminal callback did not start")
	}
	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := owner.CloseAndWait(expired); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "http connections did not join") {
		t.Errorf("terminal state released a still-running preserved callback: %v", err)
	}
	unblock()
	if err := owner.CloseAndWait(ctx); err != nil {
		t.Error(err)
	}
	select {
	case <-callbackDone:
	case <-ctx.Done():
		t.Fatal("preserved callback did not join")
	}
}

// Implements the context contract while exposing the actual join boundary.
func (self *testHttpWaitContext) Done() <-chan struct{} {
	self.once.Do(func() { close(self.entered) })
	return self.Context.Done()
}

// Socket/Serve completion cannot certify the independent handler's completion.
func TestTestHttpServerJoinsHandlerAfterSocketClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release, handlerDone, clientDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	httpServer := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		defer close(handlerDone)
		close(entered)
		<-release
	})}
	owner := NewTestHttpServer(ctx, listener, httpServer)
	defer testHttpRescueJoin(t, owner, false)
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	go func() {
		defer close(clientDone)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listener.Addr().String(), nil)
		response, _ := (&http.Client{Transport: transport}).Do(request)
		if response != nil {
			response.Body.Close()
		}
	}()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := owner.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		for _, done := range []<-chan struct{}{handlerDone, clientDone} {
			select {
			case <-done:
			case <-joinCtx.Done():
				t.Error("http test child did not join")
			}
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler did not enter")
	}
	if err := httpServer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owner.serveDone:
	case <-ctx.Done():
		t.Fatal("Serve did not exit")
	}
	joinCtx, stopJoin := context.WithCancel(ctx)
	defer stopJoin()
	waitCtx := &testHttpWaitContext{Context: joinCtx, entered: make(chan struct{})}
	closed := make(chan error, 1)
	closeJoined := make(chan struct{})
	go func() { defer close(closeJoined); closed <- owner.CloseAndWait(waitCtx) }()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-closeJoined:
		case <-joinCtx.Done():
			t.Error("close caller did not join")
		}
	}()
	select {
	case <-waitCtx.entered:
	case err := <-closed:
		t.Errorf("socket/Serve exit bypassed held handler: %v", err)
		unblock()
		return
	case <-ctx.Done():
		t.Error("handler join not reached")
		unblock()
		return
	}
	stopJoin()
	select {
	case err := <-closed:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "http handlers did not join") {
			t.Errorf("held handler was reported joined: %v", err)
		}
	case <-ctx.Done():
		t.Error("close did not join")
	}
	unblock()
	if err := owner.CloseAndWait(ctx); err != nil {
		t.Error(err)
	}
}

// A failed bound closes admission but does not discard ownership. Both a retry
// and simultaneous successful closes must join the same final handler.
func TestTestHttpServerDeadlineAdmissionAndConcurrentRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	httpServer := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release })}
	owner := NewTestHttpServer(ctx, listener, httpServer)
	defer testHttpRescueJoin(t, owner, false)
	go func() {
		defer close(done)
		httpServer.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	defer func() {
		unblock()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := owner.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-joinCtx.Done():
			t.Error("direct handler did not join")
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler did not enter")
	}
	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := owner.CloseAndWait(expired); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "http handlers did not join") {
		t.Errorf("failed join was reported as clean: %v", err)
	}
	response := httptest.NewRecorder()
	httpServer.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("late admission = %d", response.Code)
	}
	results := make(chan error, 2)
	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	waitContexts := []*testHttpWaitContext{
		{Context: firstCtx, entered: make(chan struct{})},
		{Context: secondCtx, entered: make(chan struct{})},
	}
	var callers sync.WaitGroup
	for i := 0; i < 2; i++ {
		callers.Go(func() { results <- owner.CloseAndWait(waitContexts[i]) })
	}
	defer func() { unblock(); callers.Wait() }()
	for _, waitCtx := range waitContexts {
		select {
		case <-waitCtx.entered:
		case err := <-results:
			t.Fatalf("concurrent close bypassed held handler: %v", err)
		case <-ctx.Done():
			t.Fatal("concurrent caller did not reach the actual handler wait")
		}
	}
	stopFirst()
	stopSecond()
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "http handlers did not join") {
				t.Errorf("concurrent cleanup reported held handler joined: %v", err)
			}
		case <-ctx.Done():
			t.Error("concurrent close did not join")
		}
	}
	unblock()
	if err := owner.CloseAndWait(ctx); err != nil {
		t.Error(err)
	}
}

// Checks both an established hijack and the transition after admission closes.
func testHttpHijackOwnership(t *testing.T, late bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, allowHijack, hijacked, socketClosed, release, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	permitHijack := sync.OnceFunc(func() { close(allowHijack) })
	unblock := sync.OnceFunc(func() { close(release) })
	observedHijack := make(chan struct{})
	httpServer := &http.Server{
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateHijacked {
				close(observedHijack)
			}
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			defer close(handlerDone)
			close(entered)
			<-allowHijack
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer conn.Close()
			close(hijacked)
			var b [1]byte
			_, _ = conn.Read(b[:])
			close(socketClosed)
			<-release
		}),
	}
	shutdownEntered := make(chan struct{})
	httpServer.RegisterOnShutdown(sync.OnceFunc(func() { close(shutdownEntered) }))
	owner := NewTestHttpServer(ctx, listener, httpServer)
	defer testHttpRescueJoin(t, owner, false)
	defer func() { permitHijack(); unblock() }()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		permitHijack()
		unblock()
		client.Close()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := owner.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		select {
		case <-handlerDone:
		case <-joinCtx.Done():
			t.Error("hijacked handler did not join")
		}
	}()
	if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: fixture.example\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler did not enter")
	}
	if !late {
		permitHijack()
		select {
		case <-hijacked:
		case <-ctx.Done():
			t.Fatal("hijack did not complete")
		}
	}
	closed := make(chan error, 1)
	closeJoined := make(chan struct{})
	joinCtx, stopJoin := context.WithCancel(ctx)
	defer stopJoin()
	go func() { defer close(closeJoined); closed <- owner.CloseAndWait(joinCtx) }()
	defer func() {
		permitHijack()
		unblock()
		client.Close()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-closeJoined:
		case <-joinCtx.Done():
			t.Error("hijack close caller did not join")
		}
	}()
	if late {
		select {
		case <-shutdownEntered:
		case <-ctx.Done():
			t.Fatal("shutdown did not begin")
		}
		permitHijack()
	}
	select {
	case <-socketClosed:
	case err := <-closed:
		t.Errorf("owned socket escaped cleanup: %v", err)
		return
	case <-ctx.Done():
		t.Error("owned hijacked socket was not closed")
		unblock()
		return
	}
	select {
	case <-observedHijack:
	default:
		t.Error("prior ConnState callback was lost")
	}
	stopJoin()
	select {
	case err := <-closed:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "http handlers did not join") {
			t.Errorf("closed hijack was mistaken for joined handler: %v", err)
		}
	case <-ctx.Done():
		t.Error("hijack close did not join")
	}
	unblock()
	if err := owner.CloseAndWait(ctx); err != nil {
		t.Error(err)
	}
}

// The real Hijacker may remain inside its handler after the socket is closed.
func TestTestHttpServerJoinsHijackedHandler(t *testing.T) { testHttpHijackOwnership(t, false) }

// The closing state and ConnState callback cover a hijack racing shutdown.
func TestTestHttpServerClosesLateHijack(t *testing.T) { testHttpHijackOwnership(t, true) }

// Unexpected Serve failure remains visible even though no handler was admitted.
func TestTestHttpServerRetainsServeFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	owner := NewTestHttpServer(ctx, listener, &http.Server{Handler: http.NotFoundHandler()})
	defer testHttpRescueJoin(t, owner, true)
	select {
	case <-owner.serveDone:
	case <-ctx.Done():
		t.Fatal("failed Serve did not terminate")
	}
	if err := owner.CloseAndWait(ctx); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Serve failure lost: %v", err)
	}
}

// Injects a close failure without changing the underlying net.Conn behavior.
type testHttpCloseFailureConn struct {
	net.Conn
	stateLock sync.Mutex
	failures  int
	err       error
}

// Fails the first close and permits the next attempt to retire the real pipe.
func (self *testHttpCloseFailureConn) Close() error {
	fail := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if 0 < self.failures {
			self.failures--
			return true
		}
		return false
	}()
	if fail {
		return self.err
	}
	return self.Conn.Close()
}

// A close error cannot drop an owned connection from the retry set or disappear
// from the evidence returned by later callers.
func TestTestHttpServerRetainsHijackCloseFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: http.NotFoundHandler()}
	owner := NewTestHttpServer(ctx, listener, httpServer)
	defer testHttpRescueJoin(t, owner, true)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	sentinel := errors.New("synthetic hijack close failure")
	conn := &testHttpCloseFailureConn{Conn: left, failures: 1, err: sentinel}
	httpServer.ConnState(conn, http.StateHijacked)
	if err := owner.CloseAndWait(ctx); !errors.Is(err, sentinel) {
		t.Errorf("initial close error lost: %v", err)
	}
	retained := func() bool {
		owner.stateLock.Lock()
		defer owner.stateLock.Unlock()
		_, retained := owner.hijackedConnectionCloses[conn]
		return retained
	}()
	if !retained {
		t.Error("failed close dropped still-owned connection")
	}
	if err := owner.CloseAndWait(ctx); !errors.Is(err, sentinel) {
		t.Errorf("retry erased close failure: %v", err)
	}
	retained = func() bool {
		owner.stateLock.Lock()
		defer owner.stateLock.Unlock()
		_, retained := owner.hijackedConnectionCloses[conn]
		return retained
	}()
	if retained {
		t.Error("successful retry did not retire connection")
	}
}

// Wraps an accepted raw socket only at its real Close tail. The TLS connection
// remains the actual object acquired through http.Hijacker.
type testHttpHeldCloseConn struct {
	net.Conn
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
	failure error
	err     error
	exit    func()
}

// Retires the socket before holding completion and publishing a retained error.
func (self *testHttpHeldCloseConn) Close() error {
	self.once.Do(func() {
		self.err = errors.Join(self.Conn.Close(), self.failure)
		close(self.entered)
		<-self.release
		if self.exit != nil {
			self.exit()
		}
	})
	return self.err
}

// Observes a real accepted socket without substituting the TLS or HTTP layers.
type testHttpHeldCloseListener struct {
	net.Listener
	accepted chan *testHttpHeldCloseConn
	release  <-chan struct{}
	failure  error
	exit     func()
}

// Installs the deterministic raw-close boundary under the actual TLS server.
func (self *testHttpHeldCloseListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	held := &testHttpHeldCloseConn{Conn: conn, entered: make(chan struct{}), release: self.release, failure: self.failure, exit: self.exit}
	self.accepted <- held
	return held, nil
}

// TLS marks itself closed before its first Close completes. A second cleanup
// cannot interpret net.ErrClosed as proof that the first close owner has joined.
func testHttpTlsHijackCloseOwnership(t *testing.T, late bool) {
	for _, failedClose := range []bool{false, true} {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pair := newTestTlsPair(t, "fixture.example")
			certificate, err := tls.X509KeyPair(pair.certPemBytes, pair.keyPemBytes)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			var failure error
			if failedClose {
				failure = errors.New("synthetic retired raw socket close failure")
			}
			observed := &testHttpHeldCloseListener{Listener: listener, accepted: make(chan *testHttpHeldCloseConn, 1), release: release, failure: failure}
			handlerDone := make(chan struct{})
			handlerEntered, allowHijack := make(chan struct{}), make(chan struct{})
			permitHijack := sync.OnceFunc(func() { close(allowHijack) })
			hijacked := make(chan net.Conn, 1)
			httpServer := &http.Server{
				TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}},
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					defer close(handlerDone)
					close(handlerEntered)
					<-allowHijack
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("actual TLS hijack: %v", err)
						return
					}
					hijacked <- conn
				}),
			}
			shutdownEntered := make(chan struct{})
			httpServer.RegisterOnShutdown(sync.OnceFunc(func() { close(shutdownEntered) }))
			owner := NewTestHttpServer(ctx, observed, httpServer)
			defer testHttpRescueJoin(t, owner, failedClose)
			defer unblock()
			defer permitHijack()
			rawClient, err := net.Dial("tcp4", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer rawClient.Close()
			deadline, _ := ctx.Deadline()
			if err := rawClient.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			client := tls.Client(rawClient, &tls.Config{ServerName: "fixture.example", InsecureSkipVerify: true})
			if err := client.HandshakeContext(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: fixture.example\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			var held *testHttpHeldCloseConn
			select {
			case held = <-observed.accepted:
			case <-ctx.Done():
				t.Fatal("actual TLS socket was not accepted")
			}
			select {
			case <-handlerEntered:
			case <-ctx.Done():
				t.Fatal("actual TLS handler did not enter")
			}
			firstCtx, stopFirst := context.WithCancel(ctx)
			defer stopFirst()
			firstDone := make(chan struct{})
			firstResult := make(chan error, 1)
			firstStarted := false
			startClose := func() {
				firstStarted = true
				go func() { defer close(firstDone); firstResult <- owner.CloseAndWait(firstCtx) }()
			}
			defer func() {
				permitHijack()
				unblock()
				stopFirst()
				if firstStarted {
					joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					select {
					case <-firstDone:
					case <-joinCtx.Done():
						t.Error("first TLS cleanup caller did not join")
					}
				}
			}()
			if late {
				startClose()
				select {
				case <-shutdownEntered:
				case <-ctx.Done():
					t.Fatal("late hijack did not enter the actual shutdown boundary")
				}
			}
			permitHijack()
			select {
			case <-handlerDone:
			case <-ctx.Done():
				t.Fatal("actual hijacked handler did not return")
			}
			var heldTlsConnection net.Conn
			select {
			case conn := <-hijacked:
				heldTlsConnection = conn
				if _, ok := conn.(*tls.Conn); !ok {
					t.Fatalf("hijack did not return the real TLS connection: %T", conn)
				}
			default:
				t.Fatal("handler returned without its hijacked connection")
			}
			idle := func() <-chan struct{} {
				owner.stateLock.Lock()
				defer owner.stateLock.Unlock()
				return owner.idle
			}()
			select {
			case <-idle:
			case <-ctx.Done():
				t.Fatal("independent handler ownership did not retire")
			}
			if err := httpServer.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-owner.serveDone:
			case <-ctx.Done():
				t.Fatal("accept loop did not join before the isolated hijack proof")
			}
			if !late {
				startClose()
			}
			select {
			case <-held.entered:
			case <-ctx.Done():
				t.Fatal("first TLS Close did not reach the actual raw-close tail")
			}
			expired, stop := context.WithCancel(context.Background())
			stop()
			if err := owner.CloseAndWait(expired); !errors.Is(err, context.Canceled) {
				t.Errorf("second cleanup certified a still-held actual TLS Close: %v", err)
			}
			stopFirst()
			select {
			case err := <-firstResult:
				if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "http hijack closes did not join") {
					t.Errorf("first canceled caller bypassed its held close attempt: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("first canceled TLS cleanup did not finish while its close remained held")
			}
			unblock()
			if err := owner.CloseAndWait(ctx); !errors.Is(err, failure) || (failure == nil && err != nil) {
				t.Errorf("retry erased the actual first TLS close result: %v", err)
			}
			// The first call may only join the released attempt. This next call
			// must exercise the completed-failure to actual TLS ErrClosed retry.
			if err := owner.CloseAndWait(ctx); !errors.Is(err, failure) || (failure == nil && err != nil) {
				t.Errorf("completed-attempt retry erased the TLS close result: %v", err)
			}
			if failedClose {
				retained := func() bool {
					owner.stateLock.Lock()
					defer owner.stateLock.Unlock()
					_, retained := owner.hijackedConnectionCloses[heldTlsConnection]
					return retained
				}()
				if !retained {
					t.Error("TLS net.ErrClosed retired ownership after an unsuccessful close")
				}
			}
		}()
	}
}

// Concurrent canceled callers retain a real TLS close tail after its handler
// and accept loop have both finished.
func TestTestHttpServerConcurrentTlsHijackCloseJoinsAttempt(t *testing.T) {
	testHttpTlsHijackCloseOwnership(t, false)
}

// A hijack that arrives after admission closes transfers to the same retained
// close worker even after its accepted-connection and handler owners retire.
func TestTestHttpServerLateTlsHijackCloseJoinsAttempt(t *testing.T) {
	testHttpTlsHijackCloseOwnership(t, true)
}

// An acquired Hijacker connection may panic or call Goexit from Close. The
// asynchronous owner must publish a failed attempt after unwinding, not strand
// its completion counter or allow a panic to terminate unrelated fixtures.
func testHttpHijackCloseAbnormal(t *testing.T, panicExit bool, causes ...error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	release := make(chan struct{})
	close(release)
	sentinel := errors.New("synthetic hijacked close panic")
	if 0 < len(causes) {
		sentinel = causes[0]
	}
	var closeArmed atomic.Bool
	exit := func() {
		if !closeArmed.Load() {
			return
		}
		if panicExit {
			panic(sentinel)
		}
		runtime.Goexit()
	}
	observed := &testHttpHeldCloseListener{Listener: listener, accepted: make(chan *testHttpHeldCloseConn, 1), release: release, exit: exit}
	handlerDone := make(chan struct{})
	hijacked := make(chan struct{})
	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(handlerDone)
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Errorf("actual abnormal-close hijack: %v", err)
			return
		}
		close(hijacked)
	})}
	owner := NewTestHttpServer(ctx, observed, httpServer)
	defer testHttpRescueJoin(t, owner, true)
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := client.SetDeadline(deadline); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: fixture.example\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handlerDone:
	case <-ctx.Done():
		t.Fatal("abnormal-close handler did not return")
	}
	select {
	case <-hijacked:
		closeArmed.Store(true)
	default:
		t.Fatal("abnormal close was not acquired through the actual Hijacker")
	}
	if err := httpServer.Close(); err != nil {
		t.Fatal(err)
	}
	for retry := 0; retry < 2; retry++ {
		err := owner.CloseAndWait(ctx)
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("retry=%d abnormal Close stranded ownership or became success: %v", retry, err)
		} else if panicExit && !errors.Is(err, sentinel) {
			t.Errorf("retry=%d lost Close panic cause: %v", retry, err)
		} else if !panicExit && !strings.Contains(err.Error(), "without returning") {
			t.Errorf("retry=%d lost Close Goexit evidence: %v", retry, err)
		}
	}
}

// A panic from a close boundary is retained locally after its acquired socket
// has been retired; it cannot crash the process or become successful cleanup.
func TestTestHttpServerHijackClosePanicRetainsFailure(t *testing.T) {
	testHttpHijackCloseAbnormal(t, true)
}

// Goexit executes ownership finalization even though Close never returns.
func TestTestHttpServerHijackCloseGoexitRetainsFailure(t *testing.T) {
	testHttpHijackCloseAbnormal(t, false)
}

// A benign closed cause cannot hide a second listener-close failure. Wrapped
// ordinary closed errors remain compatible with idempotent teardown.
func TestTestHttpServerClassifiesListenerCloseCauses(t *testing.T) {
	sentinel := errors.New("synthetic mixed listener close failure")
	for _, c := range []struct {
		cause  error
		failed bool
	}{
		{cause: fmt.Errorf("synthetic wrapped closed: %w", net.ErrClosed)},
		{cause: errors.Join(net.ErrClosed, fmt.Errorf("synthetic wrapped closed: %w", net.ErrClosed))},
		{cause: errors.Join(net.ErrClosed, sentinel), failed: true},
		{cause: fmt.Errorf("synthetic wrapper: %w", errors.Join(net.ErrClosed, sentinel)), failed: true},
	} {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			observed := &testHttpObservedListener{Listener: listener, closed: make(chan struct{}), closeErr: c.cause}
			owner := NewTestHttpServer(ctx, observed, &http.Server{Handler: http.NotFoundHandler()})
			defer testHttpRescueJoin(t, owner, c.failed)
			for retry := 0; retry < 2; retry++ {
				err := owner.CloseAndWait(ctx)
				if c.failed && !errors.Is(err, sentinel) {
					t.Errorf("listener retry=%d closed cause hid a real failure: %v", retry, err)
				} else if !c.failed && err != nil {
					t.Errorf("listener retry=%d rejected only-closed causes: %v", retry, err)
				}
			}
		}()
	}
}

// Exercises error classification on a connection obtained from the real
// Hijacker, not by calling the accounting callback directly.
func TestTestHttpServerClassifiesHijackCloseCauses(t *testing.T) {
	sentinel := errors.New("synthetic mixed hijack close failure")
	for _, c := range []struct {
		cause  error
		failed bool
	}{
		{cause: fmt.Errorf("synthetic wrapped closed: %w", net.ErrClosed)},
		{cause: errors.Join(net.ErrClosed, fmt.Errorf("synthetic wrapped closed: %w", net.ErrClosed))},
		{cause: errors.Join(net.ErrClosed, sentinel), failed: true},
		{cause: fmt.Errorf("synthetic wrapper: %w", errors.Join(net.ErrClosed, sentinel)), failed: true},
	} {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			release := make(chan struct{})
			close(release)
			observed := &testHttpHeldCloseListener{Listener: listener, accepted: make(chan *testHttpHeldCloseConn, 1), release: release, failure: c.cause}
			handlerDone := make(chan struct{})
			httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				defer close(handlerDone)
				if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
					t.Errorf("actual mixed-close hijack: %v", err)
				}
			})}
			owner := NewTestHttpServer(ctx, observed, httpServer)
			defer testHttpRescueJoin(t, owner, c.failed)
			client, err := net.Dial("tcp4", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			deadline, _ := ctx.Deadline()
			if err := client.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: fixture.example\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-handlerDone:
			case <-ctx.Done():
				t.Fatal("mixed-close handler did not return")
			}
			for retry := 0; retry < 2; retry++ {
				err := owner.CloseAndWait(ctx)
				if c.failed && !errors.Is(err, sentinel) {
					t.Errorf("hijack retry=%d closed cause hid a real failure: %v", retry, err)
				} else if !c.failed && err != nil {
					t.Errorf("hijack retry=%d rejected only-closed causes: %v", retry, err)
				}
			}
		}()
	}
}

// An abnormal return is always a failed close attempt, even if its panic cause
// would have been benign when returned normally.
func TestTestHttpServerHijackCloseClosedPanicRetainsFailure(t *testing.T) {
	testHttpHijackCloseAbnormal(t, true, net.ErrClosed)
}
