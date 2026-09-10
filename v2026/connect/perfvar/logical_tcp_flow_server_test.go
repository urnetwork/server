// This file provides a test-only TCP server that separates one logical flow
// from the speculative sockets created by the production TUN dial race.
package perfvar

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

const (
	logicalTCPFlowPrefaceMagic   = "URTF"
	logicalTCPFlowPrefaceVersion = byte(1)
	logicalTCPFlowPrefaceLength  = len(logicalTCPFlowPrefaceMagic) + 1 + 8
)

var errLogicalTCPFlowServerClosed = errors.New("logical TCP flow server closed")

// A compact numeric identity lets the server distinguish distinct workload
// flows from duplicate dial candidates without depending on socket addresses.
type logicalTCPFlowId uint64

// Optional callbacks expose exact accept, read, claim, and join boundaries to
// deterministic lifecycle tests. They are called without internal locks held.
type logicalTCPFlowServerSettings struct {
	beforeAcceptForTest        func()
	afterAcceptForTest         func(net.Conn)
	beforePrefaceReadForTest   func(net.Conn)
	afterClaimForTest          func(net.Conn, logicalTCPFlowId, bool)
	beforeCandidateDoneForTest func(net.Conn, logicalTCPFlowId, bool)
	beforeHandlersWaitForTest  func()
	afterHandlersJoinedForTest func()
}

// One listener admits every speculative socket while each expected logical
// identity remains unclaimed. Methods are safe for concurrent use.
type logicalTCPFlowServer struct {
	ctx       context.Context
	cancel    context.CancelFunc
	listener  net.Listener
	flowCount int
	handler   func(logicalTCPFlowId, net.Conn) error
	settings  *logicalTCPFlowServerSettings

	done        chan struct{}
	ready       chan struct{}
	watcherDone chan struct{}

	stateLock         sync.Mutex
	connections       map[net.Conn]bool
	claimedFlowIds    map[logicalTCPFlowId]bool
	claimedFlowCount  int
	completeFlowCount int
	firstFlowError    error
	shutdownError     error
	admissionClosed   bool
	closing           bool
	finalError        error

	handlers      sync.WaitGroup
	admissionOnce sync.Once
	shutdownOnce  sync.Once
}

// Starts the accept loop before the caller dials. Expected identities are the
// contiguous values from zero through flowCount-1.
func newLogicalTCPFlowServer(
	ctx context.Context,
	listener net.Listener,
	flowCount int,
	handler func(logicalTCPFlowId, net.Conn) error,
	settings *logicalTCPFlowServerSettings,
) *logicalTCPFlowServer {
	if flowCount <= 0 {
		panic("logical TCP flow server requires at least one flow")
	}
	if settings == nil {
		settings = &logicalTCPFlowServerSettings{}
	}
	serverCtx, cancel := context.WithCancel(ctx)
	self := &logicalTCPFlowServer{
		ctx:            serverCtx,
		cancel:         cancel,
		listener:       listener,
		flowCount:      flowCount,
		handler:        handler,
		settings:       settings,
		done:           make(chan struct{}),
		ready:          make(chan struct{}),
		watcherDone:    make(chan struct{}),
		connections:    map[net.Conn]bool{},
		claimedFlowIds: map[logicalTCPFlowId]bool{},
	}
	go self.watchCancellation()
	go self.run()
	return self
}

// Writes the fixed magic, version, and logical identity before workload data.
func writeLogicalTCPFlowPreface(connection net.Conn, flowId logicalTCPFlowId) error {
	preface := make([]byte, logicalTCPFlowPrefaceLength)
	copy(preface, logicalTCPFlowPrefaceMagic)
	preface[len(logicalTCPFlowPrefaceMagic)] = logicalTCPFlowPrefaceVersion
	binary.BigEndian.PutUint64(
		preface[len(logicalTCPFlowPrefaceMagic)+1:],
		uint64(flowId),
	)
	if err := writeFullTunAll(connection, preface); err != nil {
		return fmt.Errorf("write logical TCP flow preface: %w", err)
	}
	return nil
}

// Parses one complete identity marker. Parsing failures belong to speculative
// candidates and are intentionally not promoted to the server result.
func readLogicalTCPFlowPreface(connection net.Conn) (logicalTCPFlowId, error) {
	preface := make([]byte, logicalTCPFlowPrefaceLength)
	if _, err := io.ReadFull(connection, preface); err != nil {
		return 0, err
	}
	magicEnd := len(logicalTCPFlowPrefaceMagic)
	if string(preface[:magicEnd]) != logicalTCPFlowPrefaceMagic {
		return 0, errors.New("logical TCP flow preface magic mismatch")
	}
	if preface[magicEnd] != logicalTCPFlowPrefaceVersion {
		return 0, fmt.Errorf(
			"logical TCP flow preface version=%d, want=%d",
			preface[magicEnd],
			logicalTCPFlowPrefaceVersion,
		)
	}
	return logicalTCPFlowId(binary.BigEndian.Uint64(preface[magicEnd+1:])), nil
}

// Wakes teardown when the caller's context ends.
func (self *logicalTCPFlowServer) watchCancellation() {
	defer close(self.watcherDone)
	<-self.ctx.Done()
	self.shutdown(self.ctx.Err())
}

// Registers a newly accepted candidate unless admission has already ended.
func (self *logicalTCPFlowServer) register(connection net.Conn) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.admissionClosed || self.closing {
		return false
	}
	self.connections[connection] = false
	return true
}

// Removes a candidate from the teardown set after its goroutine has finished
// all socket work.
func (self *logicalTCPFlowServer) unregister(connection net.Conn) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.connections, connection)
}

// Claims one in-range identity exactly once. Reaching the final distinct ID
// returns the unclaimed sockets that admission must close outside the lock.
func (self *logicalTCPFlowServer) claim(
	connection net.Conn,
	flowId logicalTCPFlowId,
) (bool, bool, []net.Conn) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.admissionClosed || self.closing ||
		uint64(flowId) >= uint64(self.flowCount) ||
		self.claimedFlowIds[flowId] {
		return false, false, nil
	}
	claimed, ok := self.connections[connection]
	if !ok || claimed {
		return false, false, nil
	}
	self.connections[connection] = true
	self.claimedFlowIds[flowId] = true
	self.claimedFlowCount++
	if self.claimedFlowCount != self.flowCount {
		return true, false, nil
	}
	self.admissionClosed = true
	var unclaimedConnections []net.Conn
	for activeConnection, activeClaimed := range self.connections {
		if !activeClaimed {
			unclaimedConnections = append(unclaimedConnections, activeConnection)
		}
	}
	return true, true, unclaimedConnections
}

// Stops new accepts and wakes every candidate that did not win an identity.
// External listener and socket methods are called without the state lock held.
func (self *logicalTCPFlowServer) closeAdmission(unclaimedConnections []net.Conn) {
	self.admissionOnce.Do(func() {
		_ = self.listener.Close()
		for _, connection := range unclaimedConnections {
			_ = connection.Close()
		}
	})
}

// Records the authoritative handler result. An error ends all flows; otherwise
// teardown starts only after every distinct flow handler has returned.
func (self *logicalTCPFlowServer) completeFlow(err error) {
	shouldShutdown := false
	self.stateLock.Lock()
	self.completeFlowCount++
	if err != nil && self.firstFlowError == nil {
		self.firstFlowError = err
	}
	if err != nil || self.completeFlowCount == self.flowCount {
		shouldShutdown = true
	}
	self.stateLock.Unlock()
	if shouldShutdown {
		self.shutdown(err)
	}
}

// Closes admission and every retained socket exactly once. State is copied
// under the lock; all external close calls happen after the lock is released.
func (self *logicalTCPFlowServer) shutdown(err error) {
	self.shutdownOnce.Do(func() {
		var connections []net.Conn
		self.stateLock.Lock()
		self.closing = true
		self.admissionClosed = true
		self.shutdownError = err
		for connection := range self.connections {
			connections = append(connections, connection)
		}
		self.stateLock.Unlock()

		self.cancel()
		_ = self.listener.Close()
		for _, connection := range connections {
			_ = connection.Close()
		}
	})
}

// Reads one candidate preface and calls application code only after that
// identity wins. Candidate failures and duplicates remain private to teardown.
func (self *logicalTCPFlowServer) serve(connection net.Conn) {
	flowId := logicalTCPFlowId(0)
	claimed := false
	defer self.handlers.Done()
	defer func() {
		if self.settings.beforeCandidateDoneForTest != nil {
			self.settings.beforeCandidateDoneForTest(connection, flowId, claimed)
		}
	}()
	defer self.unregister(connection)
	defer connection.Close()

	if self.settings.beforePrefaceReadForTest != nil {
		self.settings.beforePrefaceReadForTest(connection)
	}
	parsedFlowId, err := readLogicalTCPFlowPreface(connection)
	if err != nil {
		return
	}
	flowId = parsedFlowId
	closeAdmission := false
	var unclaimedConnections []net.Conn
	claimed, closeAdmission, unclaimedConnections = self.claim(connection, flowId)
	if self.settings.afterClaimForTest != nil {
		self.settings.afterClaimForTest(connection, flowId, claimed)
	}
	if closeAdmission {
		self.closeAdmission(unclaimedConnections)
		// Closing the listener and every unclaimed candidate first makes the
		// final distinct claim the exact zero-warmup measurement boundary: no
		// speculative preface can enter the route after readiness is visible.
		close(self.ready)
	}
	if !claimed {
		return
	}
	self.completeFlow(self.handler(flowId, connection))
}

// Accepts until every identity is claimed or cancellation closes admission,
// then joins every candidate before publishing the final outcome.
func (self *logicalTCPFlowServer) run() {
	defer close(self.done)
	for {
		if self.settings.beforeAcceptForTest != nil {
			self.settings.beforeAcceptForTest()
		}
		connection, err := self.listener.Accept()
		if err != nil {
			self.stateLock.Lock()
			expectedClose := self.admissionClosed || self.closing
			self.stateLock.Unlock()
			if !expectedClose {
				self.shutdown(fmt.Errorf("accept logical TCP flow candidate: %w", err))
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

	if self.settings.beforeHandlersWaitForTest != nil {
		self.settings.beforeHandlersWaitForTest()
	}
	self.handlers.Wait()
	if self.settings.afterHandlersJoinedForTest != nil {
		self.settings.afterHandlersJoinedForTest()
	}
	self.shutdown(nil)
	<-self.watcherDone

	self.stateLock.Lock()
	if self.completeFlowCount == self.flowCount && self.firstFlowError == nil {
		self.finalError = nil
	} else if self.firstFlowError != nil {
		self.finalError = self.firstFlowError
	} else if self.shutdownError != nil {
		self.finalError = self.shutdownError
	} else {
		self.finalError = errLogicalTCPFlowServerClosed
	}
	self.stateLock.Unlock()
}

// Notifies callers only after the accept loop and every candidate have joined.
func (self *logicalTCPFlowServer) Done() <-chan struct{} {
	return self.done
}

// Notifies callers when all identities are claimed and preface traffic can no
// longer cross into a measured workload interval.
func (self *logicalTCPFlowServer) Ready() <-chan struct{} {
	return self.ready
}

// Waits for the final distinct identity claim, caller cancellation, or the
// authoritative server result when lifecycle completion wins the race.
func (self *logicalTCPFlowServer) WaitReady(ctx context.Context) error {
	select {
	case <-self.ready:
		return nil
	default:
	}
	select {
	case <-self.ready:
		return nil
	case <-self.done:
		return self.Wait()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Joins every owned goroutine and returns the authoritative flow outcome.
func (self *logicalTCPFlowServer) Wait() error {
	<-self.done
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.finalError
}

// Requests teardown and does not return while any accepted candidate remains.
func (self *logicalTCPFlowServer) CloseAndWait() {
	self.shutdown(errLogicalTCPFlowServerClosed)
	<-self.done
}

// A dormant accepted candidate cannot steal a one-flow workload from the
// later socket that carries the matching identity preface.
func TestLogicalTCPFlowServerWinnerAfterDormantAcceptedLoser(t *testing.T) {
	safetyCtx, safetyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer safetyCancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 2)
	prefaceReads := make(chan net.Conn, 2)
	handled := make(chan struct{}, 1)
	server := newLogicalTCPFlowServer(
		safetyCtx,
		listener,
		1,
		func(flowId logicalTCPFlowId, connection net.Conn) error {
			if flowId != 0 {
				return fmt.Errorf("logical flow id=%d, want=0", flowId)
			}
			body := make([]byte, 1)
			if _, readErr := io.ReadFull(connection, body); readErr != nil {
				return readErr
			}
			if body[0] != 41 {
				return fmt.Errorf("logical flow body=%d, want=41", body[0])
			}
			handled <- struct{}{}
			return nil
		},
		&logicalTCPFlowServerSettings{
			afterAcceptForTest: func(connection net.Conn) {
				accepted <- connection
			},
			beforePrefaceReadForTest: func(connection net.Conn) {
				prefaceReads <- connection
			},
		},
	)
	defer server.CloseAndWait()

	dialCandidate := func() net.Conn {
		connection, dialErr := (&net.Dialer{}).DialContext(
			safetyCtx,
			"tcp4",
			listener.Addr().String(),
		)
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		select {
		case <-accepted:
		case <-safetyCtx.Done():
			t.Fatalf("candidate was not accepted: %v", safetyCtx.Err())
		}
		select {
		case <-prefaceReads:
		case <-safetyCtx.Done():
			t.Fatalf("candidate did not reach preface read: %v", safetyCtx.Err())
		}
		return connection
	}
	loser := dialCandidate()
	defer loser.Close()
	loserRead := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, readErr := loser.Read(buffer)
		loserRead <- readErr
	}()
	winner := dialCandidate()
	defer winner.Close()
	if err := writeLogicalTCPFlowPreface(winner, 0); err != nil {
		t.Fatal(err)
	}
	if err := writeFullTunAll(winner, []byte{41}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-server.Done():
	case <-safetyCtx.Done():
		t.Fatalf("logical flow server did not finish: %v", safetyCtx.Err())
	}
	if err := server.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handled:
	default:
		t.Fatal("winning logical flow was not handled")
	}
	select {
	case readErr := <-loserRead:
		if readErr == nil {
			t.Fatal("dormant loser read unexpectedly succeeded")
		}
	case <-safetyCtx.Done():
		t.Fatalf("dormant loser was not closed: %v", safetyCtx.Err())
	}
}

// Completion remains unavailable while a canceled loser is held immediately
// before its handler releases its final lifecycle credit.
func TestLogicalTCPFlowServerCompletionWaitsForHeldLoserJoin(t *testing.T) {
	safetyCtx, safetyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer safetyCancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 2)
	prefaceReads := make(chan net.Conn, 2)
	loserHeld := make(chan struct{})
	releaseLoser := make(chan struct{})
	beforeWait := make(chan struct{}, 1)
	var stateLock sync.Mutex
	var loserServerConnection net.Conn
	server := newLogicalTCPFlowServer(
		safetyCtx,
		listener,
		1,
		func(_ logicalTCPFlowId, _ net.Conn) error {
			return nil
		},
		&logicalTCPFlowServerSettings{
			afterAcceptForTest: func(connection net.Conn) {
				accepted <- connection
			},
			beforePrefaceReadForTest: func(connection net.Conn) {
				prefaceReads <- connection
			},
			beforeCandidateDoneForTest: func(connection net.Conn, _ logicalTCPFlowId, claimed bool) {
				stateLock.Lock()
				isLoser := connection == loserServerConnection
				stateLock.Unlock()
				if isLoser && !claimed {
					close(loserHeld)
					select {
					case <-releaseLoser:
					case <-safetyCtx.Done():
					}
				}
			},
			beforeHandlersWaitForTest: func() {
				beforeWait <- struct{}{}
			},
		},
	)
	defer func() {
		select {
		case <-releaseLoser:
		default:
			close(releaseLoser)
		}
		server.CloseAndWait()
	}()

	dialAndWait := func() (net.Conn, net.Conn) {
		connection, dialErr := (&net.Dialer{}).DialContext(
			safetyCtx,
			"tcp4",
			listener.Addr().String(),
		)
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		var serverConnection net.Conn
		select {
		case serverConnection = <-accepted:
		case <-safetyCtx.Done():
			t.Fatalf("candidate was not accepted: %v", safetyCtx.Err())
		}
		select {
		case <-prefaceReads:
		case <-safetyCtx.Done():
			t.Fatalf("candidate did not reach preface read: %v", safetyCtx.Err())
		}
		return connection, serverConnection
	}
	loser, acceptedLoser := dialAndWait()
	defer loser.Close()
	stateLock.Lock()
	loserServerConnection = acceptedLoser
	stateLock.Unlock()
	winner, _ := dialAndWait()
	defer winner.Close()
	if err := writeLogicalTCPFlowPreface(winner, 0); err != nil {
		t.Fatal(err)
	}

	select {
	case <-loserHeld:
	case <-safetyCtx.Done():
		t.Fatalf("loser cleanup was not held: %v", safetyCtx.Err())
	}
	select {
	case <-beforeWait:
	case <-safetyCtx.Done():
		t.Fatalf("accept loop did not reach handler join: %v", safetyCtx.Err())
	}
	select {
	case <-server.Done():
		t.Fatal("server completed before held loser released its lifecycle credit")
	default:
	}
	close(releaseLoser)
	select {
	case <-server.Done():
	case <-safetyCtx.Done():
		t.Fatalf("server did not finish after loser release: %v", safetyCtx.Err())
	}
	if err := server.Wait(); err != nil {
		t.Fatal(err)
	}
}

// Context cancellation closes a blocked accept and a candidate blocked in its
// preface read, and Wait observes both lifecycle joins before returning.
func TestLogicalTCPFlowServerCancellationJoinsAcceptAndCandidateRead(t *testing.T) {
	safetyCtx, safetyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer safetyCancel()
	serverCtx, serverCancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptCalls := make(chan struct{}, 2)
	accepted := make(chan struct{}, 1)
	prefaceRead := make(chan struct{}, 1)
	candidateDone := make(chan struct{}, 1)
	var handlerCalls int
	server := newLogicalTCPFlowServer(
		serverCtx,
		listener,
		1,
		func(_ logicalTCPFlowId, _ net.Conn) error {
			handlerCalls++
			return nil
		},
		&logicalTCPFlowServerSettings{
			beforeAcceptForTest: func() {
				acceptCalls <- struct{}{}
			},
			afterAcceptForTest: func(_ net.Conn) {
				accepted <- struct{}{}
			},
			beforePrefaceReadForTest: func(_ net.Conn) {
				prefaceRead <- struct{}{}
			},
			beforeCandidateDoneForTest: func(_ net.Conn, _ logicalTCPFlowId, _ bool) {
				candidateDone <- struct{}{}
			},
		},
	)
	defer server.CloseAndWait()

	waitBarrier := func(name string, barrier <-chan struct{}) {
		select {
		case <-barrier:
		case <-safetyCtx.Done():
			t.Fatalf("%s was not reached: %v", name, safetyCtx.Err())
		}
	}
	waitBarrier("initial accept", acceptCalls)
	candidate, err := (&net.Dialer{}).DialContext(
		safetyCtx,
		"tcp4",
		listener.Addr().String(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	waitBarrier("candidate accept", accepted)
	waitBarrier("candidate preface read", prefaceRead)
	waitBarrier("second blocked accept", acceptCalls)
	candidateRead := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, readErr := candidate.Read(buffer)
		candidateRead <- readErr
	}()

	serverCancel()
	select {
	case <-server.Done():
	case <-safetyCtx.Done():
		t.Fatalf("canceled server did not join: %v", safetyCtx.Err())
	}
	if err := server.WaitReady(safetyCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled server readiness error=%v, want context canceled", err)
	}
	if err := server.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled server error=%v, want context canceled", err)
	}
	waitBarrier("candidate handler completion", candidateDone)
	select {
	case readErr := <-candidateRead:
		if readErr == nil {
			t.Fatal("canceled candidate read unexpectedly succeeded")
		}
	case <-safetyCtx.Done():
		t.Fatalf("candidate peer read was not canceled: %v", safetyCtx.Err())
	}
	if handlerCalls != 0 {
		t.Fatalf("canceled candidate handler calls=%d, want=0", handlerCalls)
	}
}

// Readiness remains unpublished while the final claim callback is held, then
// succeeds only after admission has closed around that committed claim.
func TestLogicalTCPFlowServerWaitReadyFollowsFinalClaim(t *testing.T) {
	safetyCtx, safetyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer safetyCancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	finalClaimed := make(chan struct{})
	releaseFinalClaim := make(chan struct{})
	server := newLogicalTCPFlowServer(
		safetyCtx,
		listener,
		1,
		func(_ logicalTCPFlowId, _ net.Conn) error {
			return nil
		},
		&logicalTCPFlowServerSettings{
			afterAcceptForTest: func(_ net.Conn) {
				accepted <- struct{}{}
			},
			afterClaimForTest: func(_ net.Conn, _ logicalTCPFlowId, claimed bool) {
				if claimed {
					close(finalClaimed)
					select {
					case <-releaseFinalClaim:
					case <-safetyCtx.Done():
					}
				}
			},
		},
	)
	defer func() {
		select {
		case <-releaseFinalClaim:
		default:
			close(releaseFinalClaim)
		}
		server.CloseAndWait()
	}()
	connection, err := (&net.Dialer{}).DialContext(
		safetyCtx,
		"tcp4",
		listener.Addr().String(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	select {
	case <-accepted:
	case <-safetyCtx.Done():
		t.Fatalf("candidate was not accepted: %v", safetyCtx.Err())
	}
	if err := writeLogicalTCPFlowPreface(connection, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finalClaimed:
	case <-safetyCtx.Done():
		t.Fatalf("final claim was not observed: %v", safetyCtx.Err())
	}
	select {
	case <-server.Ready():
		t.Fatal("readiness was published before final-claim admission cleanup")
	default:
	}
	close(releaseFinalClaim)
	if err := server.WaitReady(safetyCtx); err != nil {
		t.Fatalf("wait for claimed server readiness: %v", err)
	}
	if err := server.Wait(); err != nil {
		t.Fatal(err)
	}
}

// Canceling one readiness waiter does not cancel the server or invent a
// successful claim boundary.
func TestLogicalTCPFlowServerWaitReadyReturnsCallerCancellation(t *testing.T) {
	safetyCtx, safetyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer safetyCancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := newLogicalTCPFlowServer(
		safetyCtx,
		listener,
		1,
		func(_ logicalTCPFlowId, _ net.Conn) error {
			return nil
		},
		nil,
	)
	defer server.CloseAndWait()
	waitCtx, waitCancel := context.WithCancel(context.Background())
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- server.WaitReady(waitCtx)
	}()
	waitCancel()
	select {
	case waitErr := <-waitResult:
		if !errors.Is(waitErr, context.Canceled) {
			t.Fatalf("readiness cancellation error=%v, want context canceled", waitErr)
		}
	case <-safetyCtx.Done():
		t.Fatalf("readiness waiter did not observe cancellation: %v", safetyCtx.Err())
	}
	select {
	case <-server.Done():
		t.Fatal("caller readiness cancellation stopped the server")
	default:
	}
	select {
	case <-server.Ready():
		t.Fatal("caller readiness cancellation published readiness")
	default:
	}
}

// Distinct identities may win out of order while one dormant loser precedes
// each winner. A duplicate identity cannot create another handler result.
func TestLogicalTCPFlowServerClaimsExactlyEachExpectedFlow(t *testing.T) {
	safetyCtx, safetyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer safetyCancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const flowCount = 3
	accepted := make(chan net.Conn, 2*flowCount+1)
	prefaceReads := make(chan net.Conn, 2*flowCount+1)
	claimResults := make(chan struct {
		flowId  logicalTCPFlowId
		claimed bool
	}, 2*flowCount+1)
	var resultLock sync.Mutex
	handlerResultCounts := map[logicalTCPFlowId]int{}
	candidateDoneCount := 0
	server := newLogicalTCPFlowServer(
		safetyCtx,
		listener,
		flowCount,
		func(flowId logicalTCPFlowId, connection net.Conn) error {
			body := make([]byte, 1)
			if _, readErr := io.ReadFull(connection, body); readErr != nil {
				return readErr
			}
			if logicalTCPFlowId(body[0]) != flowId {
				return fmt.Errorf("flow %d body=%d", flowId, body[0])
			}
			resultLock.Lock()
			handlerResultCounts[flowId]++
			resultLock.Unlock()
			return nil
		},
		&logicalTCPFlowServerSettings{
			afterAcceptForTest: func(connection net.Conn) {
				accepted <- connection
			},
			beforePrefaceReadForTest: func(connection net.Conn) {
				prefaceReads <- connection
			},
			afterClaimForTest: func(_ net.Conn, flowId logicalTCPFlowId, claimed bool) {
				claimResults <- struct {
					flowId  logicalTCPFlowId
					claimed bool
				}{
					flowId:  flowId,
					claimed: claimed,
				}
			},
			beforeCandidateDoneForTest: func(_ net.Conn, _ logicalTCPFlowId, _ bool) {
				resultLock.Lock()
				candidateDoneCount++
				resultLock.Unlock()
			},
		},
	)
	defer server.CloseAndWait()

	dialAndWait := func() net.Conn {
		connection, dialErr := (&net.Dialer{}).DialContext(
			safetyCtx,
			"tcp4",
			listener.Addr().String(),
		)
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		select {
		case <-accepted:
		case <-safetyCtx.Done():
			t.Fatalf("candidate was not accepted: %v", safetyCtx.Err())
		}
		select {
		case <-prefaceReads:
		case <-safetyCtx.Done():
			t.Fatalf("candidate did not reach preface read: %v", safetyCtx.Err())
		}
		return connection
	}
	waitClaim := func(flowId logicalTCPFlowId, claimed bool) {
		select {
		case result := <-claimResults:
			if result.flowId != flowId || result.claimed != claimed {
				t.Fatalf("claim=%+v, want flow=%d claimed=%t", result, flowId, claimed)
			}
		case <-safetyCtx.Done():
			t.Fatalf("flow %d claim was not observed: %v", flowId, safetyCtx.Err())
		}
	}
	var connections []net.Conn
	for winnerIndex, flowId := range []logicalTCPFlowId{2, 0, 1} {
		loser := dialAndWait()
		connections = append(connections, loser)
		winner := dialAndWait()
		connections = append(connections, winner)
		if err := writeLogicalTCPFlowPreface(winner, flowId); err != nil {
			t.Fatal(err)
		}
		if err := writeFullTunAll(winner, []byte{byte(flowId)}); err != nil {
			t.Fatal(err)
		}
		waitClaim(flowId, true)
		if winnerIndex == 0 {
			duplicate := dialAndWait()
			connections = append(connections, duplicate)
			if err := writeLogicalTCPFlowPreface(duplicate, flowId); err != nil {
				t.Fatal(err)
			}
			waitClaim(flowId, false)
		}
	}
	defer func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()

	select {
	case <-server.Done():
	case <-safetyCtx.Done():
		t.Fatalf("multi-flow server did not finish: %v", safetyCtx.Err())
	}
	if err := server.Wait(); err != nil {
		t.Fatal(err)
	}
	resultLock.Lock()
	defer resultLock.Unlock()
	if len(handlerResultCounts) != flowCount {
		t.Fatalf("handler result identities=%d, want=%d", len(handlerResultCounts), flowCount)
	}
	if candidateDoneCount != 2*flowCount+1 {
		t.Fatalf("joined candidates=%d, want=%d", candidateDoneCount, 2*flowCount+1)
	}
	for flowId := logicalTCPFlowId(0); flowId < flowCount; flowId++ {
		if handlerResultCounts[flowId] != 1 {
			t.Fatalf("flow %d handler results=%d, want=1", flowId, handlerResultCounts[flowId])
		}
	}
}
