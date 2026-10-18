// Failure-path ownership for the native tcp writev fixture. These helpers are
// test-local; the production socket, writer and pool ownership remain intact.
package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// The test goroutine owns lifecycle and pool fields. Only accepted-peer
// publication is concurrent, and it cannot publish after teardown begins.
type exchangeWritevFixtureOwner struct {
	cancel     context.CancelFunc
	listener   *net.TCPListener
	connection *ExchangeConnection
	peerDone   chan struct{}
	peerErr    error

	stateLock sync.Mutex
	closing   bool
	peer      *net.TCPConn

	pending   [][]byte
	witnesses [][]byte
	closed    bool
	closeErr  error
}

// Registers teardown before any accept or socket worker can be launched.
func newExchangeWritevFixtureOwner(t testing.TB, cancel context.CancelFunc) *exchangeWritevFixtureOwner {
	t.Helper()
	owner := &exchangeWritevFixtureOwner{cancel: cancel}
	t.Cleanup(func() {
		if err := owner.close(); err != nil {
			t.Error(err)
		}
	})
	return owner
}

// Takes an accepted socket unless teardown already closed its publication.
func (self *exchangeWritevFixtureOwner) acceptPeer(peer *net.TCPConn) bool {
	accepted := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closing {
			return false
		}
		self.peer = peer
		return true
	}()
	if !accepted {
		_ = peer.Close()
	}
	return accepted
}

// Takes a newly created reference until it is passed to the consuming send.
func (self *exchangeWritevFixtureOwner) keepPending(message []byte) int {
	index := len(self.pending)
	self.pending = append(self.pending, message)
	return index
}

// Transfers ownership before the call: sendMessage consumes on every result.
func (self *exchangeWritevFixtureOwner) takePending(index int) []byte {
	message := self.pending[index]
	self.pending[index] = nil
	return message
}

// Cancels and closes every I/O boundary before joining; then returns only
// never-offered references and finally checks each retained witness once.
func (self *exchangeWritevFixtureOwner) close() error {
	if self.closed {
		return self.closeErr
	}
	self.closed = true
	peer := func() *net.TCPConn {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closing = true
		return self.peer
	}()
	self.cancel()
	if self.listener != nil {
		_ = self.listener.Close()
	}
	if peer != nil {
		_ = peer.Close()
	}
	if self.connection != nil {
		self.connection.Close()
	}
	if self.peerDone != nil {
		<-self.peerDone
	}
	for index, message := range self.pending {
		if message != nil {
			self.pending[index] = nil
			clientconnect.MessagePoolReturn(message)
		}
	}
	for index, witness := range self.witnesses {
		if witness == nil {
			continue
		}
		self.witnesses[index] = nil
		if !clientconnect.MessagePoolReturn(witness) {
			self.closeErr = errors.Join(self.closeErr, fmt.Errorf("native writev fixture witness %d was not the final pool owner", index))
		}
	}
	return self.closeErr
}
