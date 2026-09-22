//go:build unix

package connect

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	connectlib "github.com/urnetwork/connect"
)

// This fixture tests the real HTTP proxy/protocol seam only. It does not add a
// production H1+ endpoint or stand in for JWT/signed-proxy-id authentication.
const (
	h1plusNginxToken = "urnetwork-framer/1"
	h1plusNginxAuth  = "synthetic-h1plus-test-only"
	h1plusNginxHello = "framer bytes immediately after HTTP 101"
)

func h1plusNginxBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{
		filepath.Join("..", "..", "warp", "lb", "build", "nginx-local", "sbin", "nginx"),
		"/tmp/urnetwork-nginx-udp-v2-full/sbin/nginx",
	}
	if configured := os.Getenv("NGINX_H1PLUS_BINARY"); configured != "" {
		candidates = []string{configured}
	}
	for _, candidate := range candidates {
		binary, err := exec.LookPath(candidate)
		if err != nil {
			if os.Getenv("NGINX_H1PLUS_BINARY") != "" {
				t.Fatalf("NGINX_H1PLUS_BINARY=%q is not executable: %v", candidate, err)
			}
			continue
		}
		binary, err = filepath.Abs(binary)
		if err != nil {
			t.Fatal(err)
		}
		version, err := exec.Command(binary, "-V").CombinedOutput()
		if err != nil {
			t.Fatalf("NGINX prerequisite exists but is not runnable on this host; select a native pinned build with NGINX_H1PLUS_BINARY: %v: %s", err, version)
		}
		if !bytes.Contains(version, []byte("nginx/1.31.4")) ||
			!bytes.Contains(version, []byte("urnetwork-11d11b5f0d3d8ace5215e1a77918e9dc219ce7db")) {
			t.Fatalf("expected warp/lb pinned NGINX build; got: %s", version)
		}
		t.Logf("real NGINX: %s", strings.TrimSpace(string(version)))
		return binary
	}
	t.Skip("pinned NGINX absent; build `make nginx_local` in warp/lb or set NGINX_H1PLUS_BINARY to that native build")
	return ""
}

func h1plusNginxProxy(t *testing.T, binary, backend string) string {
	t.Helper()
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	front := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	config := fmt.Sprintf(`
worker_processes 1;
daemon off;
pid nginx.pid;
error_log stderr info;
events { worker_connections 128; }
http {
    access_log off;
    client_body_temp_path client_body;
    proxy_temp_path proxy_temp;
    map $http_upgrade $connection_upgrade {
        default upgrade;
        '' close;
    }
    server {
        listen %s;
        location / {
            proxy_pass %s;
            proxy_http_version 1.1;
            proxy_set_header Host $host;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection $connection_upgrade;
            proxy_set_header X-UR-Network-Auth $http_x_ur_network_auth;
            proxy_buffering off;
            proxy_read_timeout 1h;
            proxy_send_timeout 1h;
        }
    }
}
`, front, backend)
	configPath := filepath.Join(tempDir, "nginx.conf")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(tempDir, "nginx.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	cmd := exec.Command(binary, "-p", tempDir+string(os.PathSeparator), "-c", configPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var processErr error
	go func() {
		processErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGQUIT)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
			}
		}
		if t.Failed() {
			output, _ := os.ReadFile(logPath)
			t.Logf("nginx output:\n%s", output)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp4", front, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return "http://" + front
		}
		select {
		case <-done:
			t.Fatalf("nginx exited before readiness: %v", processErr)
		case <-time.After(5 * time.Millisecond):
		}
		if deadline.Before(time.Now()) {
			t.Fatal("nginx did not become ready")
		}
	}
}

type h1plusNginxRequest struct {
	path, upgrade, remote  string
	protoMajor, protoMinor int
}

type h1plusNginxBackend struct {
	mu       sync.Mutex
	requests []h1plusNginxRequest
	active   map[net.Conn]struct{}
	workers  sync.WaitGroup
}

func (b *h1plusNginxBackend) track(conn net.Conn) func() {
	b.mu.Lock()
	b.active[conn] = struct{}{}
	b.workers.Add(1)
	b.mu.Unlock()
	return func() {
		_ = conn.Close()
		b.mu.Lock()
		delete(b.active, conn)
		b.mu.Unlock()
		b.workers.Done()
	}
}

func (b *h1plusNginxBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.requests = append(b.requests, h1plusNginxRequest{
		path: r.URL.Path, upgrade: r.Header.Get("Upgrade"), remote: r.RemoteAddr,
		protoMajor: r.ProtoMajor, protoMinor: r.ProtoMinor,
	})
	b.mu.Unlock()
	if r.Header.Get("X-UR-Network-Auth") != h1plusNginxAuth {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet || !h1plusNginxHeaderToken(r.Header, "Connection", "upgrade") {
		http.Error(w, "bad upgrade request", http.StatusBadRequest)
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		upgrader := websocket.Upgrader{}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer b.track(ws.UnderlyingConn())()
		_ = ws.UnderlyingConn().SetDeadline(time.Now().Add(10 * time.Second))
		ws.SetReadLimit(65535)
		for {
			kind, message, err := ws.ReadMessage()
			if err != nil || kind != websocket.BinaryMessage {
				return
			}
			if err := ws.WriteMessage(kind, message); err != nil {
				return
			}
		}
	}
	if r.Header.Get("Upgrade") != h1plusNginxToken {
		http.Error(w, "unsupported", http.StatusUpgradeRequired)
		return
	}
	switch r.URL.Path {
	case "/reject":
		http.Error(w, "unsupported", http.StatusUpgradeRequired)
		return
	case "/absent":
		_, _ = io.WriteString(w, "ordinary HTTP response")
		return
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer b.track(conn)()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	token := h1plusNginxToken
	if r.URL.Path == "/mismatch" {
		token = "not-urnetwork/1"
	}
	if _, err := fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", token); err != nil {
		return
	}
	framer := connectlib.NewFramer(connectlib.DefaultFramerSettings(65535))
	storage := make([]byte, 16*1024)
	// The first frame is in the same buffered flush as 101, not a later write.
	if err := framer.WriteBatchWithStorage(rw, [][]byte{[]byte(h1plusNginxHello)}, storage); err != nil {
		return
	}
	if err := rw.Flush(); err != nil || token != h1plusNginxToken {
		return
	}
	for {
		message, err := framer.Read(rw.Reader)
		if err != nil {
			return
		}
		err = framer.WriteBatchWithStorage(conn, [][]byte{message}, storage)
		connectlib.MessagePoolReturn(message)
		if err != nil {
			return
		}
	}
}

func h1plusNginxHeaderToken(header http.Header, key, token string) bool {
	for _, value := range header.Values(key) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

type h1plusNginxClient struct {
	conn                    net.Conn
	reader                  *bufio.Reader
	ws                      *websocket.Conn
	framer                  *connectlib.Framer
	firstLocal, secondLocal string
}

func h1plusNginxDial(rawURL, auth string) (*h1plusNginxClient, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, err
	}
	conn, err := net.DialTimeout("tcp4", u.Host, 3*time.Second)
	if err != nil {
		return nil, 0, err
	}
	firstLocal := conn.LocalAddr().String()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	request, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	request.Header.Set("Connection", "keep-alive, Upgrade")
	request.Header.Set("Upgrade", h1plusNginxToken)
	request.Header.Set("X-UR-Network-Auth", auth)
	if err := request.Write(conn); err != nil {
		_ = conn.Close()
		return nil, 0, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		_ = conn.Close()
		return nil, 0, err
	}
	status := response.StatusCode
	if status == http.StatusSwitchingProtocols &&
		h1plusNginxHeaderToken(response.Header, "Connection", "upgrade") &&
		response.Header.Get("Upgrade") == h1plusNginxToken {
		// Force the handoff with real prefetched payload bytes. Giving Framer
		// conn instead of reader here would lose the greeting deterministically.
		if _, err := reader.Peek(4 + len(h1plusNginxHello)); err != nil {
			_ = conn.Close()
			return nil, status, err
		}
		return &h1plusNginxClient{
			conn: conn, reader: reader, firstLocal: firstLocal,
			framer: connectlib.NewFramer(connectlib.DefaultFramerSettings(65535)),
		}, status, nil
	}
	// Never reuse or reinterpret the rejected socket. Do not drain an unbounded
	// rejected response body; closing first also unblocks response.Body.Close.
	_ = conn.Close()
	_ = response.Body.Close()
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, status, fmt.Errorf("terminal fixture authorization failure")
	}
	u.Scheme = "ws"
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	ws, wsResponse, err := dialer.Dial(u.String(), http.Header{"X-Ur-Network-Auth": {auth}})
	if err != nil {
		if wsResponse != nil && wsResponse.Body != nil {
			_ = wsResponse.Body.Close()
		}
		return nil, status, err
	}
	_ = ws.UnderlyingConn().SetDeadline(time.Now().Add(10 * time.Second))
	return &h1plusNginxClient{
		conn: ws.UnderlyingConn(), ws: ws, firstLocal: firstLocal,
		secondLocal: ws.LocalAddr().String(),
	}, status, nil
}

func TestH1PlusNginxUpgradeAndFreshWebSocketFallback(t *testing.T) {
	binary := h1plusNginxBinary(t)
	backend := &h1plusNginxBackend{active: make(map[net.Conn]struct{})}
	server := httptest.NewServer(backend)
	t.Cleanup(func() {
		server.Close()
		backend.mu.Lock()
		for conn := range backend.active {
			_ = conn.Close()
		}
		backend.mu.Unlock()
		backend.workers.Wait()
	})
	front := h1plusNginxProxy(t, binary, server.URL)
	for _, tc := range []struct {
		path     string
		fallback bool
		status   int
	}{{"/accept", false, 101}, {"/reject", true, 426}, {"/absent", true, 200}, {"/mismatch", true, 101}} {
		t.Run(strings.TrimPrefix(tc.path, "/"), func(t *testing.T) {
			client, status, err := h1plusNginxDial(front+tc.path, h1plusNginxAuth)
			if err != nil {
				t.Fatal(err)
			}
			defer client.conn.Close()
			if status != tc.status || (client.ws != nil) != tc.fallback {
				t.Fatalf("status=%d WS=%v, want status=%d WS=%v", status, client.ws != nil, tc.status, tc.fallback)
			}
			if tc.fallback && client.firstLocal == client.secondLocal {
				t.Fatal("fallback reused the failed connection")
			}
			if !tc.fallback {
				if client.reader.Buffered() < 4+len(h1plusNginxHello) {
					t.Fatal("fixture failed to prefetch the initial frame")
				}
				message, err := client.framer.Read(client.reader)
				if err != nil || string(message) != h1plusNginxHello {
					t.Fatalf("bytes after 101 were lost: %q: %v", message, err)
				}
				connectlib.MessagePoolReturn(message)
			}
			messages := [][]byte{nil, {1}, bytes.Repeat([]byte{2}, 1200), bytes.Repeat([]byte{3}, 16380), bytes.Repeat([]byte{4}, 65535)}
			for i := range 33 {
				messages = append(messages, bytes.Repeat([]byte{byte(i + 5)}, 1200))
			}
			writeDone := make(chan error, 1)
			writerFinished := make(chan struct{})
			go func() {
				defer close(writerFinished)
				storage := make([]byte, 16*1024)
				for _, message := range messages {
					var err error
					if client.ws != nil {
						err = client.ws.WriteMessage(websocket.BinaryMessage, message)
					} else {
						err = client.framer.WriteBatchWithStorage(client.conn, [][]byte{message}, storage)
					}
					if err != nil {
						writeDone <- err
						return
					}
				}
				writeDone <- nil
			}()
			defer func() {
				_ = client.conn.Close()
				<-writerFinished
			}()
			for i, want := range messages {
				var got []byte
				var err error
				if client.ws != nil {
					var kind int
					kind, got, err = client.ws.ReadMessage()
					if err == nil && kind != websocket.BinaryMessage {
						err = fmt.Errorf("unexpected WebSocket message type %d", kind)
					}
				} else {
					got, err = client.framer.Read(client.reader)
				}
				correct := bytes.Equal(got, want)
				if client.ws == nil {
					connectlib.MessagePoolReturn(got)
				}
				if err != nil || !correct {
					t.Fatalf("message %d changed through nginx: %v", i, err)
				}
			}
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			backend.mu.Lock()
			var requests []h1plusNginxRequest
			for _, request := range backend.requests {
				if request.path == tc.path {
					requests = append(requests, request)
				}
			}
			backend.mu.Unlock()
			wantRequests := 1
			if tc.fallback {
				wantRequests = 2
			}
			if len(requests) != wantRequests || requests[0].upgrade != h1plusNginxToken {
				t.Fatalf("wrong upstream handshake sequence: %+v", requests)
			}
			for _, request := range requests {
				if request.protoMajor != 1 || request.protoMinor != 1 {
					t.Fatal("nginx did not use HTTP/1.1 upstream")
				}
			}
			if tc.fallback && (requests[1].upgrade != "websocket" || requests[0].remote == requests[1].remote) {
				t.Fatal("fallback did not use a distinct upstream RFC WebSocket connection")
			}
		})
	}
	t.Run("auth-before-101", func(t *testing.T) {
		client, status, err := h1plusNginxDial(front+"/unauthorized", "wrong-fixture-auth")
		if client != nil {
			_ = client.conn.Close()
		}
		if err == nil || client != nil || status != http.StatusUnauthorized {
			t.Fatalf("unauthorized custom upgrade accepted: status=%d err=%v", status, err)
		}
		ws, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front, "http")+"/unauthorized-ws", nil)
		if ws != nil {
			_ = ws.Close()
		}
		if response != nil && response.Body != nil {
			defer response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatal("unauthorized WebSocket control accepted")
		}
		backend.mu.Lock()
		defer backend.mu.Unlock()
		attempts := 0
		for _, request := range backend.requests {
			if request.path == "/unauthorized" {
				attempts++
			}
		}
		if attempts != 1 {
			t.Fatal("terminal auth failure triggered fallback")
		}
	})
}
