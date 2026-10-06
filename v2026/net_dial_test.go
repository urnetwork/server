package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The shared dialer reaches both loopback families and carries the explicit
// happy-eyeballs delay (connect/IPV6.md C6). Loopback v6 is required, never
// skipped: the suite runs on dual-stack hosts.
func TestNewDialerDualStackLoopback(t *testing.T) {
	dialer := NewDialer(2 * time.Second)
	if dialer.FallbackDelay != DialFallbackDelay {
		t.Fatalf("FallbackDelay = %s, want %s", dialer.FallbackDelay, DialFallbackDelay)
	}
	if !dialer.KeepAliveConfig.Enable {
		t.Fatal("keepalive is not enabled")
	}

	for _, network := range []string{"tcp4", "tcp6"} {
		address := "127.0.0.1:0"
		if network == "tcp6" {
			address = "[::1]:0"
		}
		listener, err := net.Listen(network, address)
		if err != nil {
			t.Fatalf("%s loopback is required for dual-stack tests: %v", network, err)
		}
		accepted := make(chan struct{})
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				conn.Close()
			}
			close(accepted)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := dialer.DialContext(ctx, "tcp", listener.Addr().String())
		cancel()
		if err != nil {
			t.Fatalf("dial %s: %v", listener.Addr(), err)
		}
		conn.Close()
		<-accepted
		listener.Close()
	}
}

func TestNewHttpClientDualStackLoopback(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	for _, network := range []string{"tcp4", "tcp6"} {
		address := "127.0.0.1:0"
		if network == "tcp6" {
			address = "[::1]:0"
		}
		listener, err := net.Listen(network, address)
		if err != nil {
			t.Fatalf("%s loopback is required for dual-stack tests: %v", network, err)
		}
		testServer := httptest.NewUnstartedServer(handler)
		testServer.Listener = listener
		testServer.Start()

		client := NewHttpClient(5 * time.Second)
		response, err := client.Get(testServer.URL)
		if err != nil {
			t.Fatalf("get %s: %v", testServer.URL, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if string(body) != "ok" {
			t.Fatalf("body = %q", body)
		}
		testServer.Close()
	}
}
