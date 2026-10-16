// The handler join includes ownership transferred from a request to the hold.
package connect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/session"
)

// Terminal write and close stop at independent barriers without timing guesses.
type testingRefusalCleanupConn struct {
	testingHeldConn
	writeEntered chan struct{}
	writeRelease chan struct{}
	closeEntered chan struct{}
	closeRelease chan struct{}
	writeOnce    sync.Once
}

// The first terminal write remains owned by the hold until explicitly released.
func (self *testingRefusalCleanupConn) WriteMessage(messageType int, message []byte) error {
	self.writeOnce.Do(func() {
		close(self.writeEntered)
		<-self.writeRelease
	})
	return self.testingHeldConn.WriteMessage(messageType, message)
}

// Closing the socket is part of cleanup, even after its terminal frame is sent.
func (self *testingRefusalCleanupConn) Close() error {
	close(self.closeEntered)
	<-self.closeRelease
	return self.testingHeldConn.Close()
}

// A real constructor reserves the worker before any request transfers a socket.
func TestConnectRefusalHoldHandlerJoinWaitsForSocketCleanup(t *testing.T) {
	fixture := newTestingRefusalHoldHandler(t, 1)
	handler := fixture.handler
	conn := &testingRefusalCleanupConn{
		writeEntered: make(chan struct{}),
		writeRelease: make(chan struct{}),
		closeEntered: make(chan struct{}),
		closeRelease: make(chan struct{}),
	}
	var releaseWrite, releaseClose sync.Once
	defer releaseWrite.Do(func() { close(conn.writeRelease) })
	defer releaseClose.Do(func() { close(conn.closeRelease) })
	if !handler.beginHandle() {
		t.Fatal("request admission failed")
	}
	if !handler.refusalHold.hold(conn, session.AuthorizationRejectedCloseCode) {
		handler.endHandle()
		t.Fatal("request did not transfer its socket")
	}
	handler.endHandle()
	handler.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for _, barrier := range []<-chan struct{}{conn.writeEntered, conn.closeEntered} {
		select {
		case <-barrier:
		case <-ctx.Done():
			t.Fatal("socket cleanup did not reach its barrier")
		}
		handler.activeLock.Lock()
		activeZero := handler.activeZero
		handler.activeLock.Unlock()
		select {
		case <-activeZero:
			t.Fatal("handler reported idle while a transferred socket was still owned")
		default:
		}
		canceled, stop := context.WithCancel(ctx)
		stop()
		if handler.WaitForIdle(canceled) {
			t.Fatal("join succeeded while terminal socket cleanup was blocked")
		}
		releaseWrite.Do(func() { close(conn.writeRelease) })
	}
	releaseClose.Do(func() { close(conn.closeRelease) })
	if !handler.WaitForIdle(ctx) {
		t.Fatal("handler did not join completed socket cleanup")
	}
	if _, closed := conn.state(); !closed || handler.refusalHold.count() != 0 {
		t.Fatal("handler join left a live refused socket")
	}
	if handler.refusalHold.hold(&testingHeldConn{}, session.AuthorizationRejectedCloseCode) {
		t.Fatal("joined handler accepted another refused socket")
	}
}
