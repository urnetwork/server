package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestSessionAuthorizationCloseWaitsForSoleWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writerDone := make(chan struct{})
		socketClosed := make(chan struct{})
		go closeH1AuthorizationSocket(writerDone, func() { close(socketClosed) })
		synctest.Wait()
		select {
		case <-socketClosed:
			t.Fatal("socket closed before the gated sole writer published retirement")
		default:
		}
		close(writerDone)
		synctest.Wait()
		select {
		case <-socketClosed:
		default:
			t.Fatal("completed writer did not release socket teardown")
		}
	})
}

func TestSessionAuthorizationCloseBoundsBlockedWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writerDone := make(chan struct{})
		socketClosed := make(chan struct{})
		start := time.Now()
		go closeH1AuthorizationSocket(writerDone, func() { close(socketClosed) })
		<-socketClosed
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("blocked writer held socket for %s, expected one second", elapsed)
		}
	})
}

func TestSessionOrdinaryCloseDoesNotWaitForAuthorizationWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		closed := false
		closeH1AuthorizationSocket(nil, func() { closed = true })
		if !closed || !time.Now().Equal(start) {
			t.Fatal("ordinary socket close waited for a nonexistent authorization writer")
		}
	})
}
