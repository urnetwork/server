package connect

// Cancellation must reach resident work before any synchronous owner teardown
// can block. A listener close barrier observes that ordering without I/O.

import (
	"context"
	"net"
	"testing"
)

type exchangeCloseBarrierListener struct {
	net.Listener
	entered chan struct{}
	release chan struct{}
}

func (self *exchangeCloseBarrierListener) Close() error {
	close(self.entered)
	<-self.release
	return nil
}

func TestExchangeCloseCancelsBeforeJoiningOwners(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener := &exchangeCloseBarrierListener{entered: make(chan struct{}), release: make(chan struct{})}
	exchange := &Exchange{ctx: ctx, cancel: cancel, servicePortListeners: map[int]net.Listener{1: listener}}
	joined := make(chan struct{})
	go func() { exchange.Close(); close(joined) }()
	<-listener.entered
	canceledBeforeJoin := ctx.Err() != nil
	close(listener.release)
	<-joined
	if !canceledBeforeJoin {
		t.Fatal("synchronous owner teardown delayed exchange cancellation")
	}
}
