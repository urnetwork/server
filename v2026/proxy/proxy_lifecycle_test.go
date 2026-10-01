// Proxy harness lifecycle tests force each in-process connect owner to remain
// live at teardown so no Redis or database cleanup can escape the harness.
package proxy

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Provides exact close and idle barriers for the harness lifecycle helper.
type blockingProxyConnectLifecycle struct {
	closeOnce   sync.Once
	waitOnce    sync.Once
	closed      chan struct{}
	waitEntered chan struct{}
	release     <-chan struct{}
}

// Records that admission was closed.
func (self *blockingProxyConnectLifecycle) Close() {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
}

// Holds the cleanup owner until the test releases it or the deadline expires.
func (self *blockingProxyConnectLifecycle) WaitForIdle(ctx context.Context) bool {
	self.waitOnce.Do(func() {
		close(self.waitEntered)
	})
	select {
	case <-ctx.Done():
		return false
	case <-self.release:
		return true
	}
}

// Teardown closes both owners before waiting and cannot return while the
// exchange still retains its final Redis cleanup.
func TestCloseProxyConnectLifecyclesJoinsHandlerAndExchange(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	handlerRelease := make(chan struct{})
	close(handlerRelease)
	exchangeRelease := make(chan struct{})
	handler := &blockingProxyConnectLifecycle{
		closed:      make(chan struct{}),
		waitEntered: make(chan struct{}),
		release:     handlerRelease,
	}
	exchange := &blockingProxyConnectLifecycle{
		closed:      make(chan struct{}),
		waitEntered: make(chan struct{}),
		release:     exchangeRelease,
	}
	closeDone := make(chan struct{})
	go func() {
		closeProxyConnectLifecycles(t, handler, exchange, func() {})
		close(closeDone)
	}()

	for _, closed := range []<-chan struct{}{handler.closed, exchange.closed} {
		select {
		case <-closed:
		case <-testCtx.Done():
			close(exchangeRelease)
			t.Fatal("proxy connect owner did not close admission")
		}
	}
	select {
	case <-exchange.waitEntered:
	case <-testCtx.Done():
		close(exchangeRelease)
		t.Fatal("proxy teardown did not enter exchange cleanup join")
	}
	select {
	case <-closeDone:
		close(exchangeRelease)
		t.Fatal("proxy teardown returned before exchange cleanup")
	default:
	}

	close(exchangeRelease)
	select {
	case <-closeDone:
	case <-testCtx.Done():
		t.Fatal("proxy teardown did not join exchange cleanup")
	}
}
