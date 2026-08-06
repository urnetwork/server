package connect

import (
	"context"
	"net"
	"testing"
	"time"
)

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
