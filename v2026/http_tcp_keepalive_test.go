//go:build unix

package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

// A server whose protocol runs its own heartbeat can leave TCP keepalive off on
// the connections it accepts; every other server keeps Go's default.
func TestHttpListenAndServeTcpKeepAliveOption(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(fmt.Sprintf("disable=%t", disable), func(t *testing.T) {
			enabled := acceptedHttpConnTcpKeepAlive(t, HttpServerOptions{
				ReadTimeout:         5 * time.Second,
				WriteTimeout:        5 * time.Second,
				IdleTimeout:         5 * time.Second,
				ShutdownTimeout:     time.Second,
				DisableTcpKeepAlive: disable,
			})
			if enabled == disable {
				t.Fatalf("accepted connection TCP keepalive = %t with DisableTcpKeepAlive = %t", enabled, disable)
			}
		})
	}
}

// Serves one request and reports whether the server side of its connection
// carries TCP keepalive.
func acceptedHttpConnTcpKeepAlive(t *testing.T, options HttpServerOptions) bool {
	t.Helper()
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := make(chan bool, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		rawConn, err := conn.(*net.TCPConn).SyscallConn()
		if err != nil {
			t.Error(err)
			return
		}
		var enabled int
		var getErr error
		if err := rawConn.Control(func(fd uintptr) {
			enabled, getErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
		}); err != nil || getErr != nil {
			t.Error(err, getErr)
			return
		}
		observed <- enabled != 0
		conn.Write([]byte("HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"))
	})
	served := make(chan error, 1)
	go func() {
		served <- HttpListenAndServeWithReusePort(ctx, addr, handler, false, options)
	}()

	var conn net.Conn
	for deadline := time.Now().Add(5 * time.Second); ; {
		conn, err = net.DialTimeout("tcp4", addr, time.Second)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	select {
	case enabled := <-observed:
		cancel()
		<-served
		return enabled
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not observe the connection")
		return false
	}
}
