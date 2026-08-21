// This file owns the TCP echo server used to prove a full TUN route is ready.
package perfvar

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Optional callbacks expose exact lifecycle boundaries to deterministic tests.
type readinessEchoServerSettings struct {
	beforeSuccessfulConnectionClose func()
	afterCompleteRequest            func()
	afterCompleteResponse           func()
	afterAcceptForTest              func(net.Conn)
	afterHandlerDoneForTest         func(net.Conn)
}

// One listener serves every connection created by the TUN dial race. The first
// complete request is authoritative; failed canceled attempts cannot replace it.
type readinessEchoServer struct {
	listener net.Listener
	payload  []byte
	settings *readinessEchoServerSettings

	result chan error
	done   chan struct{}

	stateLock   sync.Mutex
	connections map[net.Conn]bool
	firstError  error
	successful  bool
	closing     bool

	handlers     sync.WaitGroup
	shutdownOnce sync.Once
}

// Starts accepting before the caller begins its raced dial.
func newReadinessEchoServer(
	listener net.Listener,
	payload []byte,
	settings *readinessEchoServerSettings,
) *readinessEchoServer {
	if settings == nil {
		settings = &readinessEchoServerSettings{}
	}
	self := &readinessEchoServer{
		listener:    listener,
		payload:     payload,
		settings:    settings,
		result:      make(chan error, 1),
		done:        make(chan struct{}),
		connections: map[net.Conn]bool{},
	}
	go self.run()
	return self
}

// Records the first failure for diagnostics while allowing another raced dial
// to become the successful connection.
func (self *readinessEchoServer) recordError(err error) {
	if err == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.firstError == nil {
		self.firstError = err
	}
}

// Registers an accepted socket unless teardown already owns it.
func (self *readinessEchoServer) register(connection net.Conn) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closing {
		return false
	}
	self.connections[connection] = true
	return true
}

// Removes a completed socket from the set closed by teardown.
func (self *readinessEchoServer) unregister(connection net.Conn) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.connections, connection)
}

// Elects exactly one complete echo. A canceled loser can only contribute a
// diagnostic error and cannot overwrite this result.
func (self *readinessEchoServer) claimSuccess() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.successful || self.closing {
		return false
	}
	self.successful = true
	return true
}

// Stops admission and closes every accepted socket without holding state while
// calling external objects. Socket closure unblocks incomplete loser handlers.
func (self *readinessEchoServer) shutdown() {
	self.shutdownOnce.Do(func() {
		var connections []net.Conn
		self.stateLock.Lock()
		self.closing = true
		for connection := range self.connections {
			connections = append(connections, connection)
		}
		self.stateLock.Unlock()

		_ = self.listener.Close()
		for _, connection := range connections {
			_ = connection.Close()
		}
	})
}

// Reads and echoes one exact request. A successful handler closes the listener
// only after the pre-close test boundary has been released.
func (self *readinessEchoServer) serve(connection net.Conn) {
	defer self.handlers.Done()
	defer func() {
		if self.settings.afterHandlerDoneForTest != nil {
			self.settings.afterHandlerDoneForTest(connection)
		}
	}()
	defer self.unregister(connection)
	defer connection.Close()

	if noDelayConnection, ok := connection.(interface{ SetNoDelay(bool) error }); ok {
		if err := noDelayConnection.SetNoDelay(true); err != nil {
			self.recordError(fmt.Errorf("set readiness server no-delay: %w", err))
			return
		}
	}
	request := make([]byte, len(self.payload))
	if _, err := io.ReadFull(connection, request); err != nil {
		self.recordError(fmt.Errorf("read readiness request: %w", err))
		return
	}
	if !bytes.Equal(request, self.payload) {
		self.recordError(errors.New("readiness request content mismatch"))
		return
	}
	if self.settings.afterCompleteRequest != nil {
		self.settings.afterCompleteRequest()
	}
	if err := writeFullTunAll(connection, request); err != nil {
		self.recordError(fmt.Errorf("write readiness response: %w", err))
		return
	}
	if self.settings.afterCompleteResponse != nil {
		self.settings.afterCompleteResponse()
	}
	if !self.claimSuccess() {
		return
	}
	if self.settings.beforeSuccessfulConnectionClose != nil {
		self.settings.beforeSuccessfulConnectionClose()
	}
	self.shutdown()
}

// Accepts until one raced connection completes or teardown closes admission,
// then joins every handler before publishing the single final result.
func (self *readinessEchoServer) run() {
	defer close(self.done)
	defer close(self.result)
	for {
		connection, err := self.listener.Accept()
		if err != nil {
			self.stateLock.Lock()
			closing := self.closing
			self.stateLock.Unlock()
			if !closing {
				self.recordError(fmt.Errorf("accept readiness connection: %w", err))
				self.shutdown()
			}
			break
		}
		if !self.register(connection) {
			_ = connection.Close()
			continue
		}
		if self.settings.afterAcceptForTest != nil {
			self.settings.afterAcceptForTest(connection)
		}
		self.handlers.Add(1)
		go self.serve(connection)
	}

	self.handlers.Wait()
	self.stateLock.Lock()
	successful := self.successful
	resultErr := self.firstError
	self.stateLock.Unlock()
	if successful {
		resultErr = nil
	} else if resultErr == nil {
		resultErr = errors.New("readiness echo server closed without a complete request")
	}
	self.result <- resultErr
}

// Closes all sockets and waits until no accepted handler can retain work.
func (self *readinessEchoServer) CloseAndWait() {
	self.shutdown()
	<-self.done
}

// A dormant first connection models the canceled loser from Tun.DialContext.
// The second connection must still complete, and the first one's read failure
// must not replace the successful result.
func TestReadinessEchoServerServesWinnerAfterAcceptedLoser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte(nil), deterministicPayload()[:fullTunProbePayloadByteCount]...)
	accepted := make(chan net.Conn, 2)
	loserHandlerHeld := make(chan struct{})
	releaseLoserHandler := make(chan struct{})
	winnerHandlerDone := make(chan struct{})
	var releaseLoserHandlerOnce sync.Once
	releaseLoser := func() {
		releaseLoserHandlerOnce.Do(func() {
			close(releaseLoserHandler)
		})
	}
	var stateLock sync.Mutex
	var dormantConnection net.Conn
	var handlerDoneCount atomic.Int32
	server := newReadinessEchoServer(
		listener,
		payload,
		&readinessEchoServerSettings{
			afterAcceptForTest: func(connection net.Conn) {
				accepted <- connection
			},
			afterHandlerDoneForTest: func(connection net.Conn) {
				stateLock.Lock()
				isDormant := connection == dormantConnection
				stateLock.Unlock()
				if isDormant {
					close(loserHandlerHeld)
					select {
					case <-releaseLoserHandler:
					case <-ctx.Done():
					}
				}
				handlerDoneCount.Add(1)
				if !isDormant {
					close(winnerHandlerDone)
				}
			},
		},
	)
	defer server.CloseAndWait()
	defer releaseLoser()

	dial := func() net.Conn {
		connection, dialErr := (&net.Dialer{}).DialContext(
			ctx,
			"tcp4",
			listener.Addr().String(),
		)
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		return connection
	}
	loser := dial()
	defer loser.Close()
	select {
	case acceptedConnection := <-accepted:
		if acceptedConnection.RemoteAddr().String() != loser.LocalAddr().String() {
			t.Fatalf(
				"first accepted connection=%s, want dormant loser=%s",
				acceptedConnection.RemoteAddr(),
				loser.LocalAddr(),
			)
		}
		stateLock.Lock()
		dormantConnection = acceptedConnection
		stateLock.Unlock()
	case <-ctx.Done():
		t.Fatalf("dormant loser was not accepted: %v", ctx.Err())
	}
	loserRead := make(chan error, 1)
	go func() {
		oneByte := make([]byte, 1)
		_, readErr := loser.Read(oneByte)
		loserRead <- readErr
	}()

	winner := dial()
	defer winner.Close()
	if err := writeFullTunAll(winner, payload); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(payload))
	if _, err := io.ReadFull(winner, response); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response, payload) {
		t.Fatal("winner received mismatched readiness echo")
	}

	waitBarrier := func(name string, barrier <-chan struct{}) {
		select {
		case <-barrier:
		case <-ctx.Done():
			t.Fatalf("%s was not reached: %v", name, ctx.Err())
		}
	}
	waitBarrier("dormant loser handler boundary", loserHandlerHeld)
	waitBarrier("winner handler completion", winnerHandlerDone)
	select {
	case resultErr, ok := <-server.result:
		t.Fatalf(
			"readiness result published before dormant loser joined: ok=%t err=%v",
			ok,
			resultErr,
		)
	default:
	}
	if handlerDoneCount.Load() != 1 {
		t.Fatalf("completed handlers while loser held=%d, want=1", handlerDoneCount.Load())
	}
	releaseLoser()

	select {
	case resultErr, ok := <-server.result:
		if !ok {
			t.Fatal("readiness result closed without its authoritative value")
		}
		if resultErr != nil {
			t.Fatal(resultErr)
		}
	case <-ctx.Done():
		t.Fatalf("readiness server did not publish success: %v", ctx.Err())
	}
	if _, ok := <-server.result; ok {
		t.Fatal("readiness server published more than one result")
	}
	if handlerDoneCount.Load() != 2 {
		t.Fatalf("joined handlers=%d, want=2", handlerDoneCount.Load())
	}
	select {
	case readErr := <-loserRead:
		if readErr == nil {
			t.Fatal("dormant loser read unexpectedly succeeded")
		}
	case <-ctx.Done():
		t.Fatalf("dormant loser was not closed: %v", ctx.Err())
	}
}
