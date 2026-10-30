//go:build unix

// The shared service listener must keep a kernel liveness backstop for ordinary
// HTTP connections, including /status, which do not run transport heartbeats.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// Records the real route's response while forwarding it to the accepted socket.
type runStatusResponseCapture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

// Preserves the route's explicit response status.
func (self *runStatusResponseCapture) WriteHeader(status int) {
	self.status = status
	self.ResponseWriter.WriteHeader(status)
}

// Captures the actual status body without replacing the HTTP transport.
func (self *runStatusResponseCapture) Write(p []byte) (int, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	self.body.Write(p)
	return self.ResponseWriter.Write(p)
}

// Exercises runner options, the real HTTP helper, routing and the accepted
// socket together. Hijacking happens after /status has produced its response;
// a barrier keeps that same socket open for the kernel-option assertion.
func TestRunStatusAcceptedSocketKeepsTcpKeepAlive(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	for key, value := range map[string]string{
		"WARP_ENV": "test", "WARP_VERSION": "0.0.0-test",
		"WARP_HOST": "synthetic-host", "WARP_SERVICE": "connect", "WARP_BLOCK": "synthetic-block",
		"WARP_DOMAIN": "service.example", "WARP_HOST_IPV4": "127.0.0.1", "WARP_HOST_IPV6": "",
		"WARP_PORTS": fmt.Sprintf("8080:%d", port),
	} {
		t.Setenv(key, value)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	status := &router.WarpStatusState{}
	readinessErr := errors.New("synthetic startup refusal")
	type acceptedStatus struct {
		conn   net.Conn
		status int
		body   []byte
		err    error
	}
	accepted := make(chan acceptedStatus, 1)
	release := make(chan struct{})
	entered := make(chan struct{})
	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- runWithDependencies(ctx, RunOptions{
			Port:                     8080,
			WarpStatus:               status,
			PrivateHeapProfileTarget: "disabled",
		}, func(context.Context) error {
			status.SetNotReady(readinessErr)
			return readinessErr
		}, func(context.Context) func() {
			t.Error("refused startup published metrics")
			return func() {}
		}, func(listenCtx context.Context, addr string, handler http.Handler, reusePort bool, options server.HttpServerOptions) error {
			wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(handlerStarted)
				defer close(handlerDone)
				capture := &runStatusResponseCapture{ResponseWriter: w}
				handler.ServeHTTP(capture, r)
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					accepted <- acceptedStatus{err: err}
					return
				}
				defer conn.Close()
				accepted <- acceptedStatus{conn: conn, status: capture.status, body: capture.body.Bytes()}
				<-release
			})
			close(entered)
			return server.HttpListenAndServeWithReusePort(listenCtx, addr, wrapped, reusePort, options)
		})
	}()
	defer func() {
		close(release)
		cancel()
		if err := <-finished; err != nil {
			t.Error(err)
		}
		select {
		case <-handlerStarted:
			<-handlerDone
		default:
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("runner did not reach the real listener")
	}
	var clientConn net.Conn
	dialer := &net.Dialer{}
	for {
		clientConn, err = dialer.DialContext(ctx, "tcp4", address)
		if err == nil {
			break
		}
		// Startup only: the HTTP helper has no listener-ready callback.
		// All behavior assertions below are synchronized by the handler.
		select {
		case <-ctx.Done():
			t.Fatalf("HTTP listener did not start: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer clientConn.Close()
	if _, err := clientConn.Write([]byte("GET /status HTTP/1.1\r\nHost: status.example\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case observed := <-accepted:
		if observed.err != nil {
			t.Fatal(observed.err)
		}
		var response router.WarpStatusResult
		if err := json.Unmarshal(observed.body, &response); err != nil {
			t.Fatal(err)
		}
		if observed.status != http.StatusOK || response.Status != "error not ready: "+readinessErr.Error() {
			t.Fatalf("real /status response = %d %+v", observed.status, response)
		}
		tcpConn, ok := observed.conn.(*net.TCPConn)
		if !ok {
			t.Fatalf("accepted socket type = %T, want TCP", observed.conn)
		}
		if !tcpKeepAliveEnabled(t, tcpConn) {
			t.Fatal("ordinary /status socket lost its kernel liveness backstop on the shared listener")
		}
	case <-ctx.Done():
		t.Fatal("real /status handler did not expose its accepted socket")
	}
}
