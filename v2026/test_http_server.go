// Owns in-process test http servers through admission, handlers, hijacked
// connections and the serve loop, before a test's external resources are popped.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
)

// Retains handlers independently of sockets: Shutdown excludes hijacks and
// Close can return while a handler still uses a database. CloseAndWait is safe
// for concurrent callers and may be retried after a failed bounded join; each
// failed call remains a failure. The owner retains every hijacked connection
// until teardown, including sessions transferred out of their http handler.
type TestHttpServer struct {
	server                   *http.Server
	listener                 *testHttpListener
	serveDone                chan struct{}
	serveErr                 error
	stateLock                sync.Mutex
	closing                  bool
	active                   int
	idle                     chan struct{}
	connectionIdle           chan struct{}
	acceptedConnectionBools  map[net.Conn]bool
	hijackedConnectionCloses map[net.Conn]*testHttpHijackClose
	activeHijackCloses       int
	hijackCloseIdle          chan struct{}
	hijackErr                error
}

// Retains one underlying close attempt across concurrent bounded callers.
// A completed failed attempt stays in the connection map until a retry succeeds.
type testHttpHijackClose struct {
	done   chan struct{}
	failed bool
}

// Shares one listener close across Serve, Shutdown and pre-Serve tls failure.
// The retained result cannot disappear after the standard server forgets it.
type testHttpListener struct {
	net.Listener
	closeOnce sync.Once
	closeErr  error
}

// Retires the acquired listener once and preserves a failed close for every
// caller. A failed underlying close is never reported as completed ownership.
func (self *testHttpListener) Close() error {
	self.closeOnce.Do(func() {
		if err := self.Listener.Close(); err != nil && !testHttpOnlyClosed(err) {
			self.closeErr = err
		}
	})
	return self.closeErr
}

// Only an entirely closed error tree is benign. One closed cause cannot hide
// another failure inside a multi-error or an outer diagnostic wrapper.
func testHttpOnlyClosed(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !testHttpOnlyClosed(cause) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return testHttpOnlyClosed(cause)
	}
	return errors.Is(err, net.ErrClosed)
}

// Takes ownership of the listener and configured server. Callers configure
// Handler, timeouts and TLSConfig before this call and do not mutate them
// afterwards. A non-nil TLSConfig selects ServeTLS, exactly as the production
// tls assembly does. ConnState is preserved; BaseContext becomes the owner
// context so all requests retain its cancellation boundary.
func NewTestHttpServer(ctx context.Context, listener net.Listener, httpServer *http.Server) *TestHttpServer {
	owner := &TestHttpServer{
		server: httpServer, serveDone: make(chan struct{}), idle: make(chan struct{}),
		listener:                 &testHttpListener{Listener: listener},
		connectionIdle:           make(chan struct{}),
		acceptedConnectionBools:  map[net.Conn]bool{},
		hijackedConnectionCloses: map[net.Conn]*testHttpHijackClose{},
		hijackCloseIdle:          make(chan struct{}),
	}
	close(owner.idle)
	close(owner.connectionIdle)
	close(owner.hijackCloseIdle)
	handler := httpServer.Handler
	if handler == nil {
		handler = http.DefaultServeMux
	}
	httpServer.BaseContext = func(net.Listener) context.Context { return ctx }
	httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !owner.enter() {
			http.Error(w, "test server is closing", http.StatusServiceUnavailable)
			return
		}
		defer owner.leave()
		handler.ServeHTTP(w, r)
	})
	previousConnState := httpServer.ConnState
	httpServer.ConnState = func(conn net.Conn, state http.ConnState) {
		func() {
			owner.stateLock.Lock()
			defer owner.stateLock.Unlock()
			if state == http.StateNew {
				if len(owner.acceptedConnectionBools) == 0 {
					owner.connectionIdle = make(chan struct{})
				}
				owner.acceptedConnectionBools[conn] = true
			} else if state == http.StateHijacked {
				owner.hijackedConnectionCloses[conn] = nil
				if owner.closing {
					owner.startHijackCloseWithLock(conn)
				}
			}
		}()
		if previousConnState != nil {
			previousConnState(conn, state)
		}
		if state == http.StateClosed || state == http.StateHijacked {
			// StateClosed follows tls and connection cleanup, not just socket close.
			// A hijack transfers to the separately retained handler/socket owners.
			func() {
				owner.stateLock.Lock()
				defer owner.stateLock.Unlock()
				if owner.acceptedConnectionBools[conn] {
					delete(owner.acceptedConnectionBools, conn)
					if len(owner.acceptedConnectionBools) == 0 {
						close(owner.connectionIdle)
					}
				}
			}()
		}
	}
	go func() {
		defer close(owner.serveDone)
		defer owner.listener.Close()
		if httpServer.TLSConfig != nil {
			owner.serveErr = httpServer.ServeTLS(owner.listener, "", "")
		} else {
			owner.serveErr = httpServer.Serve(owner.listener)
		}
	}()
	return owner
}

// Publishes ownership before launching the sole close attempt. External Close
// runs outside stateLock and may outlive a caller's bound without being forgotten.
func (self *TestHttpServer) startHijackCloseWithLock(conn net.Conn) {
	previousFailure := false
	if attempt := self.hijackedConnectionCloses[conn]; attempt != nil {
		select {
		case <-attempt.done:
			previousFailure = attempt.failed
		default:
			return
		}
	}
	attempt := &testHttpHijackClose{done: make(chan struct{}), failed: previousFailure}
	self.hijackedConnectionCloses[conn] = attempt
	if self.activeHijackCloses == 0 {
		self.hijackCloseIdle = make(chan struct{})
	}
	self.activeHijackCloses++
	go func() {
		var err error
		returned := false
		defer func() {
			if recovered := recover(); recovered != nil {
				if cause, ok := recovered.(error); ok {
					err = fmt.Errorf("http hijack close panic: %w", cause)
				} else {
					err = fmt.Errorf("http hijack close panic: %v", recovered)
				}
			} else if !returned {
				err = errors.New("http hijack close exited without returning")
			}
			failed := !returned || (err != nil && !testHttpOnlyClosed(err))
			var closeErr error
			if failed {
				closeErr = fmt.Errorf("http hijacked connection: %w", err)
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if failed {
				attempt.failed = true
				self.hijackErr = errors.Join(self.hijackErr, closeErr)
			} else if err == nil || !attempt.failed {
				// TLS can return ErrClosed after a prior unsuccessful Close.
				// That flag alone cannot retire failed connection ownership.
				delete(self.hijackedConnectionCloses, conn)
			}
			self.activeHijackCloses--
			close(attempt.done)
			if self.activeHijackCloses == 0 {
				close(self.hijackCloseIdle)
			}
		}()
		err = conn.Close()
		returned = true
	}()
}

// Atomically refuses late requests or accounts their complete handler lifetime.
func (self *TestHttpServer) enter() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closing {
		return false
	}
	if self.active == 0 {
		self.idle = make(chan struct{})
	}
	self.active++
	return true
}

// Publishes idle only after the last admitted handler returns.
func (self *TestHttpServer) leave() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.active--
	if self.active == 0 {
		close(self.idle)
	}
}

// Closes admission and test-owned hijacks, then joins Serve, accepted connections
// and handlers under one deadline, then every hijack close attempt. Socket
// closure does not join a tls callback or another concurrent TLS Close call.
// This is a test ownership boundary, not the production process drain policy.
func (self *TestHttpServer) CloseAndWait(ctx context.Context) error {
	idle := func() <-chan struct{} {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closing = true
		for conn := range self.hijackedConnectionCloses {
			self.startHijackCloseWithLock(conn)
		}
		return self.idle
	}()
	var result error
	if err := self.server.Shutdown(ctx); err != nil {
		result = errors.Join(result, err, self.server.Close())
	}
	if err := self.listener.Close(); err != nil {
		result = errors.Join(result, fmt.Errorf("http listener close: %w", err))
	}
	wait := func(name string, done <-chan struct{}) {
		select {
		case <-done:
			return
		default:
		}
		select {
		case <-done:
		case <-ctx.Done():
			result = errors.Join(result, fmt.Errorf("%s did not join: %w", name, ctx.Err()))
		}
	}
	// Serve publishes StateNew before launching each connection. Waiting for it
	// first prevents a late accepted connection from escaping the idle snapshot.
	wait("http serve loop", self.serveDone)
	connectionIdle := func() <-chan struct{} {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.connectionIdle
	}()
	wait("http connections", connectionIdle)
	wait("http handlers", idle)
	// No accepted connection or handler can publish a later hijack after these
	// joins. A failed prior wait remains an explicit incomplete ownership error.
	hijackCloseIdle := func() <-chan struct{} {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.hijackCloseIdle
	}()
	wait("http hijack closes", hijackCloseIdle)
	select {
	case <-self.serveDone:
		if self.serveErr != nil && !errors.Is(self.serveErr, http.ErrServerClosed) {
			result = errors.Join(result, self.serveErr)
		}
	default:
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		result = errors.Join(result, self.hijackErr)
	}()
	return result
}
