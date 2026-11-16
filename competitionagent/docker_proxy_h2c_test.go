//go:build linux

// BuildKit compatibility tests exercise real native HTTP/2, legacy upgrade and
// reverse-session byte transport with fake local servers, never Docker itself.
package competitionagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The fixed batch caller uses false/zero or omits stdin. Alternate true
// spellings, duplicates and malformed flags cannot bypass read-only attach.
func TestDockerProxyAttachRequiresUnambiguousDisabledStdin(t *testing.T) {
	fixture := newDockerProxyTest(t)
	id := strings.Repeat("c", 64)
	var inspections atomic.Int32
	var upgrades atomic.Int32
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			upgrades.Add(1)
			http.Error(w, "synthetic upgrade endpoint unavailable", http.StatusServiceUnavailable)
			return
		}
		inspections.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": id,
			"Config":     map[string]any{"Labels": map[string]string{cleanupJobLabel: fixture.plan.JobId, cleanupRoundLabel: fixture.plan.RoundId, dockerProxyRunLabel: fixture.plan.RunId}},
			"HostConfig": map[string]string{"CgroupParent": fixture.plan.CgroupNames[0]}})
	})
	for _, query := range []string{"", "stdin=0", "stdin=false", "stdin=1", "stdin=true", "stdin=TRUE", "stdin=t", "stdin=", "stdin=invalid", "stdin=0&stdin=true", "stdin=false&stdin=false"} {
		before, beforeUpgrades := inspections.Load(), upgrades.Load()
		request := httptest.NewRequest(http.MethodPost, "http://worker/containers/"+id+"/attach?"+query, nil)
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "tcp")
		response := httptest.NewRecorder()
		fixture.proxy.workerHandler(response, request)
		allowed := query == "" || query == "stdin=0" || query == "stdin=false"
		if allowed {
			if response.Code != http.StatusBadGateway || inspections.Load() != before+1 || upgrades.Load() != beforeUpgrades+1 {
				t.Fatal("read-only attach never reached its owned upgrade", query, response.Code)
			}
		} else if response.Code != http.StatusForbidden || inspections.Load() != before || upgrades.Load() != beforeUpgrades {
			t.Fatal("interactive/ambiguous attach crossed admission", query, response.Code)
		}
	}
}

// A client may already have buffered server settings after the upgrade prelude.
type dockerProxyTestBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

// The already-read prefix must remain visible to native HTTP/2.
func (self *dockerProxyTestBufferedConn) Read(content []byte) (int, error) {
	return self.reader.Read(content)
}

// A fake real-CLI connection follows the same /grpc Upgrade:h2c prelude as
// Docker 29's buildx docker driver, then uses stdlib HTTP/2 on that connection.
func dockerProxyTestLegacySolve(ctx context.Context, address string) (string, error) {
	transport := &http.Transport{Protocols: &http.Protocols{}}
	transport.Protocols.SetUnencryptedHTTP2(true)
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://owner/grpc", nil)
		if err != nil {
			connection.Close()
			return nil, err
		}
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "h2c")
		if err := request.Write(connection); err != nil {
			connection.Close()
			return nil, err
		}
		reader := bufio.NewReader(connection)
		response, err := http.ReadResponse(reader, request)
		if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
			connection.Close()
			return nil, errors.New("legacy gRPC upgrade failed")
		}
		return &dockerProxyTestBufferedConn{Conn: connection, reader: reader}, nil
	}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://owner/moby.buildkit.v1.Control/Solve", bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/grpc")
	request.Header.Set("Te", "trailers")
	response, err := transport.RoundTrip(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected fake Solve status %d", response.StatusCode)
	}
	return response.Trailer.Get("Grpc-Status"), nil
}

// Both native h2c and upgraded clients reach the same fake daemon. No test can
// select or accidentally reach the host's real Docker socket.
func configureDockerProxyH2Daemon(t *testing.T, fixture *dockerProxyTest, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.Protocols = &http.Protocols{}
	server.Config.Protocols.SetHTTP1(true)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	t.Cleanup(server.Close)
	fixture.proxy.dialDaemon = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	return server
}

// Native HTTP/2 trailers positively identify a complete ordinary compile error.
// HTTP headers, body log text and CLI exit code alone do not establish this.
func TestDockerProxyLegacyBuildxUpgradeCapturesTerminalCompileFailure(t *testing.T) {
	fixture := newDockerProxyTest(t)
	configureDockerProxyH2Daemon(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.URL.Path != "/moby.buildkit.v1.Control/Solve" || r.Header.Get("Te") != "trailers" {
			t.Error("owned build did not preserve native gRPC transport")
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0, 0, 0, 0, 0})
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "2")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "process%20synthetic-compiler%20did%20not%20complete%20successfully:%20exit%20code:%201")
	})
	buildServer := httptest.NewServer(http.HandlerFunc(fixture.proxy.buildHandler))
	defer buildServer.Close()
	fixture.proxy.buildRunner = func(ctx context.Context, build *dockerProxyBuild) DockerBuildResult {
		status, err := dockerProxyTestLegacySolve(ctx, buildServer.Listener.Addr().String())
		fixture.proxy.stateLock.Lock()
		known := build.solveObserved && build.solveAck
		fixture.proxy.stateLock.Unlock()
		if err != nil {
			return DockerBuildResult{Acknowledged: false, ExitCode: 1, ErrorCode: "synthetic_transport_failure"}
		}
		return DockerBuildResult{Acknowledged: known, ExitCode: 1, GrpcStatus: status, Stderr: []byte("synthetic compile failure")}
	}
	result, err := fixture.proxy.RunOwnedBuild(context.Background(), fixture.buildRequest)
	if err != nil || !result.Acknowledged || result.GrpcStatus != "2" || result.ExitCode != 1 {
		t.Fatalf("complete compiler response was not acknowledged: %+v %v", result, err)
	}
	if err := fixture.proxy.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Cancellation of the evaluator's wait after gRPC headers must not close the
// real build's independent RPC or turn an incomplete response into success.
func TestDockerProxyGrpcHeadersDoNotCompleteDisconnectedBuildWaiter(t *testing.T) {
	fixture := newDockerProxyTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	configureDockerProxyH2Daemon(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(entered)
		<-release
		_, _ = w.Write([]byte{0, 0, 0, 0, 0})
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "2")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "process%20synthetic-compiler%20did%20not%20complete%20successfully:%20exit%20code:%201")
	})
	buildServer := httptest.NewServer(http.HandlerFunc(fixture.proxy.buildHandler))
	defer buildServer.Close()
	fixture.proxy.buildRunner = func(ctx context.Context, build *dockerProxyBuild) DockerBuildResult {
		status, err := dockerProxyTestLegacySolve(ctx, buildServer.Listener.Addr().String())
		if err != nil {
			return DockerBuildResult{ExitCode: 1, ErrorCode: "synthetic_transport_failure"}
		}
		fixture.proxy.stateLock.Lock()
		known := build.solveObserved && build.solveAck
		fixture.proxy.stateLock.Unlock()
		return DockerBuildResult{Acknowledged: known, ExitCode: 1, GrpcStatus: status}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := fixture.proxy.RunOwnedBuild(ctx, fixture.buildRequest); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("evaluator cancellation: %v", err)
	}
	quiesceCtx, stop := context.WithCancel(context.Background())
	stop()
	if err := fixture.proxy.Quiesce(quiesceCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("headers certified complete gRPC custody: %v", err)
	}
	if snapshot := fixture.proxy.Snapshot(); snapshot.Quiesced || !snapshot.AdmissionsClosed || snapshot.BuildAcknowledged {
		t.Fatalf("closed admissions masqueraded as terminal proof: %+v", snapshot)
	}
	close(release)
	result, err := fixture.proxy.RunOwnedBuild(context.Background(), fixture.buildRequest)
	if err != nil || !result.Acknowledged || result.GrpcStatus != "2" {
		t.Fatalf("original RPC did not drain: %+v %v", result, err)
	}
	if err := fixture.proxy.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fixture.proxy.Snapshot().Quiesced {
		t.Fatal("joined durable terminal proof was not exposed")
	}
}

// A reverse filesync/session is duplex, not a finite HTTP response. The real
// independently owned CLI keeps it alive until its own session lifetime ends.
func TestDockerProxyBuildxReverseSessionPreservesDuplexUpgrade(t *testing.T) {
	fixture := newDockerProxyTest(t)
	payload := []byte("synthetic-buildkit-session")
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session" || r.Header.Get("X-Docker-Expose-Session-Uuid") != "synthetic-session" {
			t.Error("reverse-session metadata was lost")
		}
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		response := &http.Response{StatusCode: http.StatusSwitchingProtocols, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Connection": []string{"Upgrade"}, "Upgrade": []string{"h2c"}}}
		if err := response.Write(connection); err != nil {
			t.Error(err)
			return
		}
		incoming := make([]byte, len(payload))
		if _, err := io.ReadFull(buffer.Reader, incoming); err != nil {
			t.Error(err)
			return
		}
		if !bytes.Equal(incoming, payload) {
			t.Error("reverse-session payload changed")
		}
		_, _ = connection.Write(incoming)
	})
	buildServer := httptest.NewServer(http.HandlerFunc(fixture.proxy.buildHandler))
	defer buildServer.Close()
	fixture.proxy.buildRunner = func(ctx context.Context, _ *dockerProxyBuild) DockerBuildResult {
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", buildServer.Listener.Addr().String())
		if err != nil {
			return DockerBuildResult{ExitCode: 1}
		}
		defer connection.Close()
		request, _ := http.NewRequest(http.MethodPost, "http://owner/session", nil)
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "h2c")
		request.Header.Set("X-Docker-Expose-Session-Uuid", "synthetic-session")
		if request.Write(connection) != nil {
			return DockerBuildResult{ExitCode: 1}
		}
		reader := bufio.NewReader(connection)
		response, err := http.ReadResponse(reader, request)
		if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
			return DockerBuildResult{ExitCode: 1}
		}
		if _, err := connection.Write(payload); err != nil {
			return DockerBuildResult{ExitCode: 1}
		}
		received := make([]byte, len(payload))
		if _, err := io.ReadFull(reader, received); err != nil || !bytes.Equal(received, payload) {
			return DockerBuildResult{ExitCode: 1}
		}
		return DockerBuildResult{Acknowledged: true, ExitCode: 0}
	}
	result, err := fixture.proxy.RunOwnedBuild(context.Background(), fixture.buildRequest)
	if err != nil || !result.Acknowledged || result.ExitCode != 0 {
		t.Fatalf("reverse session was treated as finite HTTP: %+v %v", result, err)
	}
	if err := fixture.proxy.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A partial or escaping build input is never a partially admitted daemon build.
func TestDockerProxyContextEscapeFailsBeforeCliAndRecoversNotIssued(t *testing.T) {
	fixture := newDockerProxyTest(t)
	if err := os.Symlink(filepath.Join(t.TempDir(), "synthetic-outside"), filepath.Join(fixture.buildRequest.ContextDirectory, "escape")); err != nil {
		t.Fatal(err)
	}
	called := false
	fixture.proxy.buildRunner = func(context.Context, *dockerProxyBuild) DockerBuildResult { called = true; return DockerBuildResult{} }
	result, err := fixture.proxy.RunOwnedBuild(context.Background(), fixture.buildRequest)
	if err == nil || !result.Acknowledged || called {
		t.Fatal("escaping context reached the CLI")
	}
	if err := fixture.proxy.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = fixture.proxy.Close()
	recovered, err := newDockerProxy(fixture.plan, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if recovered.Snapshot().UnknownOperations != 0 {
		t.Fatal("provably unissued context manufactured unknown daemon work")
	}
}

// A complete error status is distinct from incomplete/transport/server failure.
func TestDockerProxyGrpcTerminalStatusClassificationIsConservative(t *testing.T) {
	for _, status := range []string{"", "1", "4", "8", "10", "13", "14", "15", "NaN", "4294967296"} {
		if definitiveDockerGrpcStatus(status) {
			t.Fatalf("uncertain gRPC status accepted: %s", status)
		}
	}
	if !definitiveDockerGrpcStatus("0") || !definitiveDockerGrpcResponse("2", "process synthetic-compiler did not complete successfully: exit code: 1") {
		t.Fatal("complete success or ordinary compiler error was treated as unknown")
	}
	if definitiveDockerGrpcResponse("2", "") || definitiveDockerGrpcResponse("2", "synthetic internal error") {
		t.Fatal("unknown server response was mistaken for a positive compile failure")
	}
}
