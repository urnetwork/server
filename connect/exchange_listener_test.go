// This file verifies ownership and lifecycle behavior for already-bound
// exchange listeners.
package connect

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// An exchangeCloseRecordingListener exposes synchronous Close ownership while
// delegating the actual TCP listener contract.
type exchangeCloseRecordingListener struct {
	net.Listener
	closeCount atomic.Int64
}

// Close records the ownership action before releasing the socket.
func (self *exchangeCloseRecordingListener) Close() error {
	self.closeCount.Add(1)
	return self.Listener.Close()
}

func TestExchangeUsesPreboundListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	settings := DefaultExchangeSettings()
	settings.KeyEventDelivery.Enabled = false
	exchange := NewExchangeWithListeners(
		context.Background(),
		"host0",
		"connect",
		"test",
		map[int]int{port: port},
		map[string]string{"host0": "127.0.0.1"},
		settings,
		map[int]net.Listener{port: listener},
	)
	defer exchange.Close()

	select {
	case <-exchange.ctx.Done():
		t.Fatal("exchange canceled while adopting its prebound listener")
	case <-time.After(100 * time.Millisecond):
	}

	conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial prebound exchange listener: %v", err)
	}
	conn.Close()
}

func TestExchangeClosesPreboundListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	port := listener.Addr().(*net.TCPAddr).Port

	settings := DefaultExchangeSettings()
	settings.KeyEventDelivery.Enabled = false
	exchange := NewExchangeWithListeners(
		context.Background(),
		"host0",
		"connect",
		"test",
		map[int]int{port: port},
		map[string]string{"host0": "127.0.0.1"},
		settings,
		map[int]net.Listener{port: listener},
	)
	exchange.Close()

	deadline := time.Now().Add(time.Second)
	for {
		replacement, listenErr := net.Listen("tcp4", address)
		if listenErr == nil {
			replacement.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("prebound listener remained open after Exchange.Close: %v", listenErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Closing an Exchange must release a supplied listener even if Run has not
// admitted its listener worker. This is the exact constructor/Close race that
// otherwise leaves the socket with no lifecycle owner.
func TestExchangeCloseOwnsUnstartedPreboundListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	recordingListener := &exchangeCloseRecordingListener{Listener: listener}
	exchange := &Exchange{
		cancel:               func() {},
		servicePortListeners: map[int]net.Listener{1: recordingListener},
	}

	exchange.Close()
	if closeCount := recordingListener.closeCount.Load(); closeCount != 1 {
		t.Fatalf("prebound listener close count=%d, want=1", closeCount)
	}
	replacement, err := net.Listen("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatalf("rebind synchronously after Exchange.Close: %v", err)
	}
	_ = replacement.Close()
}
