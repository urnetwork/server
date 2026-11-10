//go:build linux

// Docker 29's default buildx driver uses /grpc Upgrade:h2c plus a reverse
// /session connection. Only the independently owned real build CLI may use
// these channels. Go's native unencrypted HTTP/2 parses terminal Solve trailers.
package competitionagent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Native and legacy buildx transports share the same private ownership gate.
func (self *DockerProxy) buildHandler(w http.ResponseWriter, r *http.Request) {
	if !self.authorized(r, true) {
		http.Error(w, "owned build role rejected", http.StatusForbidden)
		return
	}
	self.stateLock.Lock()
	active := self.build != nil && self.build.issued && !self.build.finished
	self.stateLock.Unlock()
	if !active {
		http.Error(w, "no committed owned build", http.StatusForbidden)
		return
	}
	path := dockerProxyApiPath(r.URL.Path)
	if path == "/grpc" {
		if r.Method != http.MethodPost || !strings.EqualFold(r.Header.Get("Upgrade"), "h2c") {
			http.Error(w, "owned build requires h2c", http.StatusBadRequest)
			return
		}
		self.serveGrpcUpgrade(w, r)
		return
	}
	if path == "/session" {
		if r.Method != http.MethodPost || !strings.EqualFold(r.Header.Get("Upgrade"), "h2c") {
			http.Error(w, "owned session requires h2c", http.StatusBadRequest)
			return
		}
		self.relayUpgrade(w, r, true)
		return
	}
	if r.Method == http.MethodPost && path == "/images/load" {
		self.forwardMutation(w, r, "image-load", true)
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		if !strings.HasPrefix(path, "/moby.buildkit.") && !strings.HasPrefix(path, "/opentelemetry.proto.collector.trace.") {
			http.Error(w, "unapproved build RPC", http.StatusForbidden)
			return
		}
		if strings.HasSuffix(path, "/Prune") || strings.HasSuffix(path, "/UpdateBuildHistory") {
			http.Error(w, "daemon-wide build mutation rejected", http.StatusForbidden)
			return
		}
		if strings.HasSuffix(path, "/Solve") {
			self.forwardMutation(w, r, "grpc", true)
			return
		}
		self.forwardGrpcRead(w, r)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		self.forwardRead(w, r)
		return
	}
	if !allowedDockerMutation(r.Method, path) {
		http.Error(w, "owned build API rejected", http.StatusForbidden)
		return
	}
	self.forwardMutation(w, r, "http", true)
}

// The HTTP/1 upgrade is only a protocol prelude. A new native h2c server parses
// the bytes after it; no custom HPACK/frame parser or legacy-builder fallback.
func (self *DockerProxy) serveGrpcUpgrade(w http.ResponseWriter, r *http.Request) {
	admission, err := self.admit(true)
	if err != nil {
		http.Error(w, "owned build upgrade admission is closed", http.StatusServiceUnavailable)
		return
	}
	defer admission()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "h2c upgrade unavailable", http.StatusInternalServerError)
		self.buildTransportFault()
		return
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		self.buildTransportFault()
		return
	}
	connection.SetDeadline(time.Time{})
	response := &http.Response{StatusCode: http.StatusSwitchingProtocols, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Connection": []string{"Upgrade"}, "Upgrade": []string{"h2c"}}}
	if err := response.Write(connection); err != nil {
		connection.Close()
		self.buildTransportFault()
		return
	}
	done := make(chan struct{})
	wrapped := &dockerProxyBufferedConn{Conn: connection, reader: buffer.Reader, done: done}
	release := self.trackSession(wrapped, true)
	defer release()
	listener := &dockerProxySingleListener{connection: wrapped, done: done}
	server := self.httpServer(http.HandlerFunc(self.buildHandler), true)
	server.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, dockerProxyPeerKey{}, uint32(0))
	}
	server.BaseContext = func(net.Listener) context.Context { return self.ownerCtx }
	_ = server.Serve(listener)
	_ = server.Close()
	_ = wrapped.Close()
}

// Auxiliary RPCs are owned by the pinned CLI. Its reverse filesync session has
// a fully snapshotted logical build body and cannot outlive the owner's budget.
func (self *DockerProxy) forwardGrpcRead(w http.ResponseWriter, r *http.Request) {
	release, err := self.admit(true)
	if err != nil {
		http.Error(w, "owned build read admission is closed", http.StatusServiceUnavailable)
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), self.settings.OperationTimeout)
	defer cancel()
	request := r.Clone(ctx)
	request.RequestURI = ""
	request.URL.Scheme, request.URL.Host = "http", "docker"
	request.Host = "docker"
	request.Header = r.Header.Clone()
	removeDockerHopHeaders(request.Header)
	request.Header.Set("Te", "trailers")
	response, err := self.grpcTransport.RoundTrip(request)
	if err != nil {
		self.buildTransportFault()
		http.Error(w, "owned build RPC failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 500 {
		self.buildTransportFault()
	}
	declared := prepareDockerResponseHeaders(w.Header(), response)
	w.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	buffer := make([]byte, self.settings.CopyBufferBytes)
	total := int64(0)
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > self.settings.MaximumResponseBytes {
				self.buildTransportFault()
				return
			}
			_ = controller.SetWriteDeadline(self.settings.Now().Add(self.settings.ClientWriteTimeout))
			if _, err := w.Write(buffer[:count]); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			if r.Context().Err() == nil {
				self.buildTransportFault()
			}
			return
		}
	}
	finishDockerResponseTrailers(w.Header(), response, declared)
}

// A disconnected independent CLI is different from a disconnected evaluator:
// the former can destroy filesync custody and must not manufacture a known ACK.
func (self *DockerProxy) buildTransportFault() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.build != nil {
		self.build.transportFault = true
	}
}

// Interactive stdin is not part of the evaluator role. Read-only attach still
// preserves Docker's binary multiplexing so ordinary docker run remains valid.
func (self *DockerProxy) attach(w http.ResponseWriter, r *http.Request) {
	path := dockerProxyApiPath(r.URL.Path)
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	stdinValues := r.URL.Query()["stdin"]
	if len(parts) != 3 || parts[0] != "containers" || parts[2] != "attach" || len(stdinValues) > 1 ||
		len(stdinValues) == 1 && stdinValues[0] != "0" && stdinValues[0] != "false" {
		http.Error(w, "interactive Docker attach rejected", http.StatusForbidden)
		return
	}
	if _, err := self.inspectOwnedDocker(r.Context(), parts[1], false); err != nil {
		http.Error(w, "Docker attach scope rejected", http.StatusForbidden)
		return
	}
	self.relayUpgrade(w, r, false)
}

// Session upgrades are opaque reverse gRPC by design: the real pinned CLI
// remains their server. They are not admitted on the worker socket.
func (self *DockerProxy) relayUpgrade(w http.ResponseWriter, r *http.Request, build bool) {
	admission, err := self.admit(build)
	if err != nil {
		http.Error(w, "owned session admission is closed", http.StatusServiceUnavailable)
		return
	}
	defer admission()
	if r.ContentLength > 0 {
		http.Error(w, "upgrade body rejected", http.StatusBadRequest)
		return
	}
	ctx, cancel := self.operationContext()
	defer cancel()
	upstream, err := self.dialDaemon(ctx)
	if err != nil {
		if build {
			self.buildTransportFault()
		}
		http.Error(w, "owned daemon upgrade failed", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	if deadline, present := self.ownerCtx.Deadline(); present {
		_ = upstream.SetDeadline(deadline)
	}
	request := r.Clone(ctx)
	request.RequestURI = ""
	request.URL.Scheme, request.URL.Host = "http", "docker"
	request.Host = "docker"
	request.Body = nil
	request.ContentLength = 0
	if err := request.Write(upstream); err != nil {
		if build {
			self.buildTransportFault()
		}
		return
	}
	upReader := bufio.NewReader(upstream)
	response, err := http.ReadResponse(upReader, request)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		if build {
			self.buildTransportFault()
		}
		http.Error(w, "daemon upgrade not acknowledged", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		if build {
			self.buildTransportFault()
		}
		return
	}
	downstream, buffer, err := hijacker.Hijack()
	if err != nil {
		if build {
			self.buildTransportFault()
		}
		return
	}
	defer downstream.Close()
	_ = downstream.SetDeadline(time.Time{})
	if deadline, present := self.ownerCtx.Deadline(); present {
		_ = downstream.SetDeadline(deadline)
	}
	if err := response.Write(downstream); err != nil {
		if build {
			self.buildTransportFault()
		}
		return
	}
	releaseUp := self.trackSession(upstream, build)
	defer releaseUp()
	releaseDown := self.trackSession(downstream, build)
	defer releaseDown()
	if !build {
		_, _ = io.CopyBuffer(downstream, io.LimitReader(upReader, self.settings.MaximumResponseBytes+1), make([]byte, self.settings.CopyBufferBytes))
		return
	}
	done := make(chan error, 2)
	go func() {
		_, err := io.CopyBuffer(upstream, io.LimitReader(buffer.Reader, self.settings.MaximumResponseBytes+1), make([]byte, self.settings.CopyBufferBytes))
		done <- err
	}()
	go func() {
		_, err := io.CopyBuffer(downstream, io.LimitReader(upReader, self.settings.MaximumResponseBytes+1), make([]byte, self.settings.CopyBufferBytes))
		done <- err
	}()
	<-done
	_ = upstream.Close()
	_ = downstream.Close()
	<-done
}

// Each upgraded connection is counted independently until its owner joins it.
func (self *DockerProxy) trackSession(connection net.Conn, build bool) func() {
	self.stateLock.Lock()
	self.sessions[connection] = build
	self.inflight++
	self.notifyWithLock()
	self.stateLock.Unlock()
	return func() {
		self.stateLock.Lock()
		delete(self.sessions, connection)
		self.inflight--
		self.notifyWithLock()
		self.stateLock.Unlock()
	}
}

// Only independent build-session endpoints are closed after that CLI has joined.
func (self *DockerProxy) closeBuildSessions() {
	self.stateLock.Lock()
	connections := []net.Conn{}
	for connection, build := range self.sessions {
		if build {
			connections = append(connections, connection)
		}
	}
	self.stateLock.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

// Buffered upgrade bytes cannot be dropped between HTTP/1 and HTTP/2 parsing.
type dockerProxyBufferedConn struct {
	net.Conn
	reader *bufio.Reader
	done   chan struct{}
	once   sync.Once
}

// Only the buffered prefix differs from the original connection.
func (self *dockerProxyBufferedConn) Read(content []byte) (int, error) {
	return self.reader.Read(content)
}

// The listener learns exactly when the protocol server closes the one stream.
func (self *dockerProxyBufferedConn) Close() error {
	err := self.Conn.Close()
	self.once.Do(func() { close(self.done) })
	return err
}

// A hijacked connection is handed once to a native net/http h2c server.
type dockerProxySingleListener struct {
	connection net.Conn
	done       chan struct{}
	sent       bool
}

// The second accept waits for the first connection's actual close.
func (self *dockerProxySingleListener) Accept() (net.Conn, error) {
	if !self.sent {
		self.sent = true
		return self.connection, nil
	}
	<-self.done
	return nil, net.ErrClosed
}

// Closing the protocol owner also releases the listener accept.
func (self *dockerProxySingleListener) Close() error { return self.connection.Close() }

// Addresses are diagnostic only and never select an upstream.
func (self *dockerProxySingleListener) Addr() net.Addr { return self.connection.LocalAddr() }
