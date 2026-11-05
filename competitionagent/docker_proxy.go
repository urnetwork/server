//go:build linux

// The finite Docker API boundary seals complete request bodies and a durable
// intent before issuing a mutation. It drains terminal responses independently
// of downstream disconnects, and never replays issued work after process loss.
package competitionagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Methods are safe concurrently. The independent systemd owner drives Serve;
// neither an evaluator HTTP context nor its process tree owns upstream work.
type DockerProxy struct {
	plan                          DockerProxyPlan
	settings                      DockerProxySettings
	ownerCtx                      context.Context
	cancelOwner                   context.CancelFunc
	stateLock                     sync.Mutex
	accepting                     bool
	barrierCommitted              bool
	inflight, operations, unknown int
	spoolBytes                    int64
	changed                       chan struct{}
	operationsByIds               map[string]*dockerProxyOperation
	build                         *dockerProxyBuild
	buildAdmissionsClosed         bool
	retirement                    *dockerBuildRetirement
	retirementWaiters             int
	buildRetirer                  *hostBuildRetirement
	journalLock                   *os.File
	protected                     bool
	transport                     *http.Transport
	grpcTransport                 *http.Transport
	dialDaemon                    func(context.Context) (net.Conn, error)
	buildRunner                   func(context.Context, *dockerProxyBuild) DockerBuildResult
	sessions                      map[net.Conn]bool
	beforeIssue                   func()
	beforeBuildIssue              func()
	beforeHandoffWait             func()
	fileHandoff                   bool
	hostFiles                     *hostFileBroker
	syncSpoolDirectory            func(string) error
}

// Only bounded hashes/statuses enter metadata; bodies stay private files.
type dockerProxyOperation struct {
	Schema         int    `json:"schema"`
	Id             string `json:"id"`
	RunId          string `json:"run_id"`
	Kind           string `json:"kind"`
	Method         string `json:"method"`
	Uri            string `json:"uri"`
	State          string `json:"state"`
	RequestSha256  string `json:"request_sha256"`
	RequestBytes   int64  `json:"request_bytes"`
	ResponseSha256 string `json:"response_sha256,omitempty"`
	ResponseBytes  int64  `json:"response_bytes,omitempty"`
	StatusCode     int    `json:"status_code,omitempty"`
	GrpcStatus     string `json:"grpc_status,omitempty"`
}

// Public construction validates fixed tools and protects the private journal.
func NewDockerProxy(plan DockerProxyPlan, settings DockerProxySettings) (*DockerProxy, error) {
	return newDockerProxy(plan, settings, true)
}

// Tests replace protected ownership only, never admission/durable transitions.
func newDockerProxy(plan DockerProxyPlan, settings DockerProxySettings, protected bool) (*DockerProxy, error) {
	plan.CgroupNames = append([]string(nil), plan.CgroupNames...)
	plan.ReadOnlyBindPaths = append([]string(nil), plan.ReadOnlyBindPaths...)
	if plan.HostFiles != nil {
		copy := *plan.HostFiles
		plan.HostFiles = &copy
	}
	if plan.BuildRetirement != nil {
		copy := *plan.BuildRetirement
		plan.BuildRetirement = &copy
	}
	if err := validateDockerProxyPlan(plan, settings, protected); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(plan.StateDirectory, "operations"), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(plan.ScratchDirectory, "operations"), 0700); err != nil {
		return nil, err
	}
	for _, path := range []string{plan.StateDirectory, plan.ScratchDirectory} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("Docker owner state and scratch must be private directories")
		}
		if protected {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != 0 {
				return nil, errors.New("Docker owner state and scratch must be root-owned")
			}
		}
	}
	lock, err := os.OpenFile(filepath.Join(plan.StateDirectory, "proxy.lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another durable Docker owner still holds this run")
	}
	ownerCtx, cancel := context.WithDeadline(context.Background(), plan.DeadlineAt.Add(settings.DrainAllowance))
	self := &DockerProxy{plan: plan, settings: settings, ownerCtx: ownerCtx, cancelOwner: cancel, accepting: true,
		changed: make(chan struct{}), operationsByIds: map[string]*dockerProxyOperation{}, journalLock: lock, protected: protected, sessions: map[net.Conn]bool{}}
	self.dialDaemon = func(ctx context.Context) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, "unix", plan.UpstreamSocketPath)
		if err != nil {
			return nil, err
		}
		if err := connection.SetWriteDeadline(plan.DeadlineAt); err != nil {
			connection.Close()
			return nil, err
		}
		return connection, nil
	}
	self.transport = &http.Transport{Proxy: nil, DisableCompression: true, MaxConnsPerHost: settings.MaximumConnections,
		MaxIdleConns: settings.MaximumConnections, MaxIdleConnsPerHost: settings.MaximumConnections,
		ResponseHeaderTimeout: settings.OperationTimeout,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return self.dialDaemon(ctx)
		}}
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	self.grpcTransport = &http.Transport{Proxy: nil, DisableCompression: true, Protocols: protocols,
		MaxConnsPerHost: settings.MaximumConnections, ResponseHeaderTimeout: settings.OperationTimeout,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return self.dialDaemon(ctx)
		}}
	self.buildRunner = self.runBuildCli
	managerBoundary := Pin{}
	if plan.HostFiles != nil {
		managerBoundary = plan.HostFiles.ResourceBoundary
	}
	self.buildRetirer = newHostBuildRetirement(managerBoundary, DefaultBuildRetirementSettings())
	self.syncSpoolDirectory = syncDirectory
	if plan.HostFiles != nil {
		self.hostFiles, err = newHostFileBroker(plan, DefaultHostFilesSettings())
		if err != nil {
			self.Close()
			return nil, err
		}
		self.hostFiles.proveMap = self.proveHostNamespace
		self.hostFiles.proveNetwork = self.proveHostNetwork
		self.hostFiles.beforePath = self.requireNoRunContainers
	}
	if err := self.recoverJournal(); err != nil {
		self.Close()
		return nil, err
	}
	if err := self.recoverBuild(); err != nil {
		self.Close()
		return nil, err
	}
	if err := self.recoverBuildRetirement(); err != nil {
		self.Close()
		return nil, err
	}
	if content, err := readBounded(filepath.Join(plan.StateDirectory, "quiesced.json"), settings.MaximumDocumentBytes); err == nil {
		var barrier struct {
			Schema int    `json:"schema"`
			RunId  string `json:"run_id"`
		}
		if decodeStrict(content, &barrier) != nil || barrier.Schema != Schema || barrier.RunId != plan.RunId {
			self.Close()
			return nil, errors.New("Docker quiesce barrier is corrupt")
		}
		self.accepting = false
		self.barrierCommitted = true
	} else if !errors.Is(err, os.ErrNotExist) {
		self.Close()
		return nil, err
	}
	if self.unknown != 0 {
		self.accepting = false
	}
	return self, nil
}

// Both listeners are manager-provisioned Unix sockets; no request can select a
// daemon. The root-only build socket additionally owns BuildKit's live session.
func (self *DockerProxy) Serve(ctx context.Context, workerListener, buildListener net.Listener) error {
	return self.serve(ctx, workerListener, buildListener, nil)
}

// The externally driven root lifecycle may freeze/kill its exact parents
// before any daemon join, including listener failure as well as cancellation.
func (self *DockerProxy) serve(ctx context.Context, workerListener, buildListener net.Listener, stopping func(context.Context) error) error {
	if workerListener == nil || buildListener == nil {
		return errors.New("Docker owner requires separate worker and build listeners")
	}
	worker := self.httpServer(http.HandlerFunc(self.workerHandler), false)
	build := self.httpServer(http.HandlerFunc(self.buildHandler), true)
	slots := make(chan struct{}, self.settings.MaximumConnections)
	workerListener = &dockerProxyLimitedListener{Listener: workerListener, slots: slots}
	buildListener = &dockerProxyLimitedListener{Listener: buildListener, slots: slots}
	done := make(chan error, 2)
	go func() { done <- worker.Serve(workerListener) }()
	go func() { done <- build.Serve(buildListener) }()
	completed := 0
	var serveErr error
	select {
	case err := <-done:
		completed++
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
	case <-self.ownerCtx.Done():
	}
	quiesceCtx, cancel := context.WithTimeout(context.Background(), self.settings.QuiesceTimeout)
	defer cancel()
	self.stateLock.Lock()
	self.accepting = false
	self.notifyWithLock()
	self.stateLock.Unlock()
	// Interrupt incomplete worker uploads, but retain the separate root build
	// session until its terminal daemon response has been drained or is unknown.
	_ = worker.Close()
	if stopping != nil {
		serveErr = errors.Join(serveErr, stopping(quiesceCtx))
	}
	err := self.Quiesce(quiesceCtx)
	if err != nil {
		self.cancelOwner()
	}
	_ = build.Close()
	for completed < 2 {
		serverErr := <-done
		completed++
		if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, serverErr)
		}
	}
	return errors.Join(serveErr, err)
}

// Close is a terminal owner shutdown, never an evaluator-client cancellation.
// Any issued operation not yet durably acknowledged stays unknown on restart.
func (self *DockerProxy) Close() error {
	self.stateLock.Lock()
	self.accepting = false
	self.notifyWithLock()
	self.stateLock.Unlock()
	self.cancelOwner()
	self.transport.CloseIdleConnections()
	self.grpcTransport.CloseIdleConnections()
	self.stateLock.Lock()
	sessions := make([]net.Conn, 0, len(self.sessions))
	for session := range self.sessions {
		sessions = append(sessions, session)
	}
	self.stateLock.Unlock()
	for _, session := range sessions {
		_ = session.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), self.settings.QuiesceTimeout)
	defer cancel()
	for {
		self.stateLock.Lock()
		changed, inflight := self.changed, self.inflight
		var journal *os.File
		if inflight == 0 {
			journal, self.journalLock = self.journalLock, nil
		}
		self.stateLock.Unlock()
		if inflight == 0 {
			if journal != nil {
				return journal.Close()
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("Docker owner native work has not joined; journal custody retained")
		case <-changed:
		}
	}
}

// Admission closes atomically before its durable barrier. Existing admitted
// bodies/requests/builds keep their owner and must join before cleanup/census.
func (self *DockerProxy) Quiesce(ctx context.Context) error {
	self.stateLock.Lock()
	self.accepting = false
	readConnections := []net.Conn{}
	for connection, build := range self.sessions {
		if !build {
			readConnections = append(readConnections, connection)
		}
	}
	self.notifyWithLock()
	self.stateLock.Unlock()
	for _, connection := range readConnections {
		_ = connection.Close()
	}
	barrier, err := json.Marshal(struct {
		Schema int    `json:"schema"`
		RunId  string `json:"run_id"`
	}{Schema: Schema, RunId: self.plan.RunId})
	if err != nil {
		return err
	}
	if err := atomicFile(self.plan.StateDirectory, "quiesced.json", barrier); err != nil {
		return err
	}
	self.stateLock.Lock()
	self.barrierCommitted = true
	self.notifyWithLock()
	self.stateLock.Unlock()
	for {
		self.stateLock.Lock()
		changed := self.changed
		inflight, unknown := self.inflight, self.unknown
		self.stateLock.Unlock()
		if inflight == 0 {
			if unknown != 0 {
				return ErrDockerProxyUnknown
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// This projection never exposes mutable operation pointers or body content.
func (self *DockerProxy) Snapshot() DockerProxySnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	snapshot := DockerProxySnapshot{RunId: self.plan.RunId, AdmissionsClosed: !self.accepting, Quiesced: self.barrierCommitted && !self.accepting && self.inflight == 0 && self.unknown == 0, Inflight: self.inflight,
		Operations: self.operations, UnknownOperations: self.unknown}
	if self.build != nil {
		snapshot.BuildStarted = self.build.issued
		snapshot.BuildAcknowledged = self.build.result.Acknowledged
	}
	return snapshot
}

// A fixed root-only control operation is distinct from Docker API mutations.
func (self *DockerProxy) workerHandler(w http.ResponseWriter, r *http.Request) {
	if !self.authorized(r, false) {
		http.Error(w, "Docker role rejected", http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/_urnetwork/status":
		if r.Method != http.MethodGet || !self.authorized(r, true) {
			http.Error(w, "role rejected", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(self.Snapshot())
		return
	case "/_urnetwork/quiesce":
		if r.Method != http.MethodPost || !self.authorized(r, true) || r.URL.Query().Get("run_id") != self.plan.RunId {
			http.Error(w, "role rejected", http.StatusForbidden)
			return
		}
		if err := self.Quiesce(r.Context()); err != nil {
			http.Error(w, "Docker custody remains held", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case "/_urnetwork/build":
		self.buildRequestHandler(w, r)
		return
	case "/_urnetwork/host-files":
		self.hostFilesRequestHandler(w, r)
		return
	}
	path := dockerProxyApiPath(r.URL.Path)
	if path == "/grpc" || path == "/session" || path == "/build" || strings.HasPrefix(path, "/moby.buildkit.") || strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		http.Error(w, "BuildKit requires the independently owned build command", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		self.forwardRead(w, r)
		return
	}
	if strings.HasSuffix(path, "/attach") && strings.EqualFold(r.Header.Get("Connection"), "Upgrade") {
		self.attach(w, r)
		return
	}
	if !allowedDockerMutation(r.Method, path) {
		http.Error(w, "Docker mutation is outside the fixed helper role", http.StatusForbidden)
		return
	}
	self.forwardMutation(w, r, "http", false)
}

// Mutation admission is counted before reading a body, so quiesce also joins
// incomplete uploads rather than admitting a late request behind its barrier.
func (self *DockerProxy) admit(internalBuild bool) (func(), error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if ((!self.accepting || self.fileHandoff) && !(internalBuild && self.build != nil && self.build.issued && !self.build.finished)) ||
		self.ownerCtx.Err() != nil || !self.settings.Now().Before(self.plan.DeadlineAt) || self.operations >= self.settings.MaximumOperations || self.inflight >= self.settings.MaximumConnections {
		return nil, errors.New("Docker mutation admission is closed")
	}
	self.operations++
	self.inflight++
	self.notifyWithLock()
	return func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.inflight--; self.notifyWithLock() }, nil
}

// The notification is a one-shot edge paired with reads under the same lock.
func (self *DockerProxy) notifyWithLock() { close(self.changed); self.changed = make(chan struct{}) }

// A fixed operation timeout is subordinate to the absolute owner lifetime.
func (self *DockerProxy) operationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(self.ownerCtx, self.settings.OperationTimeout)
}

// The request file and issued metadata reach disk before the upstream can see a
// byte. Completion means EOF/trailers plus retained response, not 200 headers.
func (self *DockerProxy) forwardMutation(w http.ResponseWriter, r *http.Request, kind string, internalBuild bool) {
	release, err := self.admit(internalBuild)
	if err != nil {
		http.Error(w, "Docker admission closed", http.StatusServiceUnavailable)
		return
	}
	defer release()
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		http.Error(w, "Docker custody unavailable", http.StatusServiceUnavailable)
		return
	}
	id := hex.EncodeToString(idBytes)
	bodyPath := filepath.Join(self.bodyDirectory(), id+".request")
	uploadDeadline := self.settings.Now().Add(self.settings.UploadTimeout)
	if self.plan.DeadlineAt.Before(uploadDeadline) {
		uploadDeadline = self.plan.DeadlineAt
	}
	_ = http.NewResponseController(w).SetReadDeadline(uploadDeadline)
	body := io.Reader(http.NoBody)
	if r.Body != nil {
		body = r.Body
	}
	maximum := self.settings.MaximumRequestBytes
	if kind == "image-load" {
		maximum = self.settings.MaximumImageLoadBytes
	}
	bodyBytes, bodyDigest, err := self.sealBody(r.Context(), body, bodyPath, maximum)
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if err != nil || r.ContentLength >= 0 && r.ContentLength != bodyBytes {
		http.Error(w, "incomplete or oversized Docker request", http.StatusBadRequest)
		return
	}
	if kind == "http" {
		path, scopeErr := self.verifyMutationScope(r.Context(), r, bodyPath)
		if scopeErr != nil {
			http.Error(w, "Docker scope rejected", http.StatusForbidden)
			return
		}
		r = r.Clone(r.Context())
		r.URL.Path = path
		r.URL.RawPath = ""
	}
	if !self.settings.Now().Before(self.plan.DeadlineAt) {
		http.Error(w, "original Docker issue deadline elapsed", http.StatusRequestTimeout)
		return
	}
	operation := &dockerProxyOperation{Schema: Schema, Id: id, RunId: self.plan.RunId, Kind: kind, Method: r.Method,
		Uri: r.URL.RequestURI(), State: "issued", RequestSha256: bodyDigest, RequestBytes: bodyBytes}
	if err := self.storeOperation(operation); err != nil {
		http.Error(w, "Docker intent could not be committed", http.StatusServiceUnavailable)
		return
	}
	self.stateLock.Lock()
	self.operationsByIds[id] = operation
	self.stateLock.Unlock()
	if self.beforeIssue != nil {
		self.beforeIssue()
	}
	if !self.settings.Now().Before(self.plan.DeadlineAt) {
		operation.StatusCode = http.StatusRequestTimeout
		self.finishUnissuedOperation(operation)
		http.Error(w, "original Docker issue deadline elapsed after intent commit", http.StatusRequestTimeout)
		return
	}
	ctx, cancel := self.operationContext()
	defer cancel()
	request, err := self.upstreamRequest(ctx, r, bodyPath, bodyBytes)
	if err != nil {
		self.finishOperation(operation, false)
		http.Error(w, "Docker request failed", http.StatusBadGateway)
		return
	}
	transport := http.RoundTripper(self.transport)
	if kind == "grpc" {
		transport = self.grpcTransport
		request.Header.Set("Te", "trailers")
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		self.finishOperation(operation, false)
		http.Error(w, "Docker outcome unknown", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	operation.StatusCode = response.StatusCode
	responseFile, err := os.OpenFile(filepath.Join(self.bodyDirectory(), id+".response"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		self.finishOperation(operation, false)
		return
	}
	count, digest, copyErr := self.drainResponse(w, response, responseFile)
	syncErr := responseFile.Sync()
	closeErr := responseFile.Close()
	directoryErr := self.syncSpoolDirectory(self.bodyDirectory())
	operation.ResponseBytes, operation.ResponseSha256 = count, digest
	ack := copyErr == nil && syncErr == nil && closeErr == nil && directoryErr == nil && response.StatusCode < 500
	if kind == "grpc" {
		operation.GrpcStatus = response.Trailer.Get("Grpc-Status")
		if operation.GrpcStatus == "" {
			operation.GrpcStatus = response.Header.Get("Grpc-Status")
		}
		message := response.Trailer.Get("Grpc-Message")
		if message == "" {
			message = response.Header.Get("Grpc-Message")
		}
		ack = ack && definitiveDockerGrpcResponse(operation.GrpcStatus, message)
		self.observeSolve(operation.GrpcStatus, ack)
	}
	self.finishOperation(operation, ack)
}

// All downstream errors only disable downstream delivery, not upstream reads.
func (self *DockerProxy) drainResponse(w http.ResponseWriter, response *http.Response, file io.Writer) (int64, string, error) {
	declared := prepareDockerResponseHeaders(w.Header(), response)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(self.settings.Now().Add(self.settings.ClientWriteTimeout))
	w.WriteHeader(response.StatusCode)
	_ = controller.Flush()
	digest := sha256.New()
	buffer := make([]byte, self.settings.CopyBufferBytes)
	total := int64(0)
	deliver := true
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			if self.settings.MaximumResponseBytes-total < int64(count) || self.reserveSpool(int64(count)) != nil {
				return total, "", errors.New("Docker response exceeded its custody bound")
			}
			content := buffer[:count]
			if written, err := file.Write(content); err != nil || written != len(content) {
				if err == nil {
					err = io.ErrShortWrite
				}
				return total, "", err
			}
			_, _ = digest.Write(content)
			total += int64(count)
			if deliver {
				_ = controller.SetWriteDeadline(self.settings.Now().Add(self.settings.ClientWriteTimeout))
				if written, err := w.Write(content); err != nil || written != len(content) {
					deliver = false
				} else if err := controller.Flush(); err != nil {
					deliver = false
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return total, "", readErr
		}
	}
	if deliver {
		finishDockerResponseTrailers(w.Header(), response, declared)
	}
	return total, hex.EncodeToString(digest.Sum(nil)), nil
}

// Read-only requests cannot create future work and may follow caller lifetime.
func (self *DockerProxy) forwardRead(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), self.settings.OperationTimeout)
	defer cancel()
	request := r.Clone(ctx)
	request.RequestURI = ""
	request.URL.Scheme = "http"
	request.URL.Host = "docker"
	request.Host = "docker"
	response, err := self.transport.RoundTrip(request)
	if err != nil {
		http.Error(w, "Docker read failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyDockerHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.CopyBuffer(w, io.LimitReader(response.Body, self.settings.MaximumResponseBytes), make([]byte, self.settings.CopyBufferBytes))
}

// No peer-supplied daemon address or host header survives this rewrite.
func (self *DockerProxy) upstreamRequest(ctx context.Context, r *http.Request, path string, size int64) (*http.Request, error) {
	file, err := openRegularNoFollow(path)
	if err != nil {
		return nil, err
	}
	request := r.Clone(ctx)
	request.RequestURI = ""
	request.URL.Scheme, request.URL.Host = "http", "docker"
	request.Host = "docker"
	request.Header = r.Header.Clone()
	removeDockerHopHeaders(request.Header)
	request.Body = file
	request.GetBody = nil
	request.ContentLength = size
	request.TransferEncoding = nil
	return request, nil
}

// An issued operation is never removed; a failed acknowledgement write remains
// unknown even if the daemon response itself was definitively complete.
func (self *DockerProxy) finishOperation(operation *dockerProxyOperation, ack bool) {
	state := "unknown"
	if ack {
		state = "acknowledged"
	}
	self.finishOperationState(operation, state)
}

// The durable intent may outlive its issue deadline before any daemon call.
// Retain a distinct definite-not-issued receipt instead of inventing an RPC.
func (self *DockerProxy) finishUnissuedOperation(operation *dockerProxyOperation) {
	self.finishOperationState(operation, "not-issued")
}

func (self *DockerProxy) finishOperationState(operation *dockerProxyOperation, state string) {
	operation.State = state
	if err := self.storeOperation(operation); err != nil {
		operation.State = "unknown"
	}
	self.stateLock.Lock()
	if operation.State != "acknowledged" && operation.State != "not-issued" {
		self.unknown++
		self.accepting = false
	}
	self.notifyWithLock()
	self.stateLock.Unlock()
}

// Metadata is crash-safe and contains no hidden request/header content.
func (self *DockerProxy) storeOperation(operation *dockerProxyOperation) error {
	content, err := json.Marshal(operation)
	if err != nil || int64(len(content)) > self.settings.MaximumDocumentBytes {
		return errors.New("Docker custody metadata exceeds its bound")
	}
	return atomicFile(self.operationDirectory(), operation.Id+".json", content)
}

// Full bodies, including chunked uploads, are bounded and synced before intent.
func (self *DockerProxy) sealBody(ctx context.Context, reader io.Reader, path string, maximum int64) (int64, string, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	digest := sha256.New()
	buffer := make([]byte, self.settings.CopyBufferBytes)
	total := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return total, "", err
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if maximum-total < int64(count) || self.reserveSpool(int64(count)) != nil {
				return total, "", errors.New("Docker request exceeds its custody bound")
			}
			if written, err := file.Write(buffer[:count]); err != nil || written != count {
				if err == nil {
					err = io.ErrShortWrite
				}
				return total, "", err
			}
			_, _ = digest.Write(buffer[:count])
			total += int64(count)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return total, "", readErr
		}
	}
	if err := file.Sync(); err != nil {
		return total, "", err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return total, "", err
	}
	return total, hex.EncodeToString(digest.Sum(nil)), nil
}

// Disk bytes are charged across all simultaneously owned requests/contexts.
func (self *DockerProxy) reserveSpool(bytes int64) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if bytes < 0 || self.settings.MaximumSpoolBytes-self.spoolBytes < bytes {
		return errors.New("Docker custody disk budget exhausted")
	}
	self.spoolBytes += bytes
	return nil
}

// Recovery examines at most the configured finite per-run inventory. Issued or
// corrupt work is never forwarded again; it permanently holds this run.
func (self *DockerProxy) recoverJournal() error {
	stems := map[string]bool{}
	for _, metadata := range []bool{true, false} {
		path, maximum := self.operationDirectory(), self.settings.MaximumOperations
		if !metadata {
			path, maximum = self.bodyDirectory(), self.settings.MaximumOperations*2
		}
		directory, err := os.Open(path)
		if err != nil {
			return err
		}
		examined := 0
		err = func() error {
			defer directory.Close()
			for {
				entries, readErr := directory.ReadDir(self.settings.DirectoryBatch)
				for _, entry := range entries {
					examined++
					if examined > maximum {
						return errors.New("Docker custody inventory exceeds its bound")
					}
					info, err := entry.Info()
					if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
						return errors.New("Docker custody inventory is not regular")
					}
					stem, extension, ok := strings.Cut(entry.Name(), ".")
					if !ok || len(stem) != 32 || !isDockerProxyHex(stem) || metadata && extension != "json" || !metadata && extension != "request" && extension != "response" {
						return errors.New("Docker custody inventory contains an unknown file")
					}
					stems[stem] = true
					if len(stems) > self.settings.MaximumOperations {
						return errors.New("Docker custody operation count exceeds its bound")
					}
					if !metadata {
						if err := self.reserveSpool(info.Size()); err != nil {
							return err
						}
						continue
					}
					content, err := readBounded(filepath.Join(path, entry.Name()), self.settings.MaximumDocumentBytes)
					if err != nil {
						return err
					}
					operation := &dockerProxyOperation{}
					if decodeStrict(content, operation) != nil || operation.Schema != Schema || operation.Id != stem || operation.RunId != self.plan.RunId {
						return errors.New("Docker custody record is corrupt")
					}
					self.operationsByIds[stem] = operation
					if operation.State != "acknowledged" && operation.State != "not-issued" {
						self.unknown++
					}
				}
				if errors.Is(readErr, io.EOF) {
					return nil
				}
				if readErr != nil {
					return readErr
				}
			}
		}()
		if err != nil {
			return err
		}
	}
	self.operations = len(stems)
	return nil
}

// Path generation never uses a caller-controlled filename.
func (self *DockerProxy) operationDirectory() string {
	return filepath.Join(self.plan.StateDirectory, "operations")
}

// Large bodies and exports consume only the separately bounded candidate
// filesystem; an exhausted spool cannot consume journal/cleanup reserve.
func (self *DockerProxy) bodyDirectory() string {
	return filepath.Join(self.plan.ScratchDirectory, "operations")
}

// Hex decoding is bounded by the fixed operation-id length at each caller.
func isDockerProxyHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

// Connection and hop-by-hop metadata cannot change the fixed upstream protocol.
func removeDockerHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(key))
		}
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(key)
	}
}

// Trailers are announced separately by the response owner before streaming.
func copyDockerHeaders(destination, source http.Header) {
	copied := source.Clone()
	removeDockerHopHeaders(copied)
	for key, values := range copied {
		destination[key] = append([]string(nil), values...)
	}
}

// Native gRPC need not predeclare trailer names. Announce its fixed terminal
// fields ourselves, and move trailers-only status headers to a final trailer
// frame rather than emitting a premature status in nonterminal headers.
func prepareDockerResponseHeaders(destination http.Header, response *http.Response) map[string]bool {
	copyDockerHeaders(destination, response.Header)
	declared := map[string]bool{}
	for key := range response.Trailer {
		declared[http.CanonicalHeaderKey(key)] = true
	}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "application/grpc") {
		for _, key := range []string{"Grpc-Status", "Grpc-Message", "Grpc-Status-Details-Bin"} {
			declared[key] = true
			destination.Del(key)
		}
	}
	for key := range declared {
		destination.Add("Trailer", key)
	}
	return declared
}

// Unknown trailer fields use net/http's explicit late-trailer prefix.
func finishDockerResponseTrailers(destination http.Header, response *http.Response, declared map[string]bool) {
	trailers := response.Trailer.Clone()
	if trailers == nil {
		trailers = http.Header{}
	}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "application/grpc") {
		for _, key := range []string{"Grpc-Status", "Grpc-Message", "Grpc-Status-Details-Bin"} {
			if trailers.Get(key) == "" && response.Header.Get(key) != "" {
				trailers[key] = append([]string(nil), response.Header.Values(key)...)
			}
		}
	}
	for key, values := range trailers {
		if declared[http.CanonicalHeaderKey(key)] {
			destination[key] = append([]string(nil), values...)
		} else {
			destination[http.TrailerPrefix+key] = append([]string(nil), values...)
		}
	}
}

// Complete compile/validation responses acknowledge work; cancellation,
// timeout, internal/data-loss and unavailable outcomes retain uncertainty.
func definitiveDockerGrpcStatus(status string) bool {
	switch status {
	case "0", "3", "5", "6", "7", "9", "11", "12", "16":
		return true
	default:
		return false
	}
}

// Status Unknown is not blanket proof. The fixed Dockerfile's ordinary failed
// RUN is positively identified in the authenticated terminal server message;
// progress/body log strings never participate in acknowledgement.
func definitiveDockerGrpcResponse(status, message string) bool {
	if status != "2" {
		return definitiveDockerGrpcStatus(status)
	}
	decoded, err := url.PathUnescape(message)
	if err != nil {
		return false
	}
	for _, uncertain := range []string{"context canceled", "transport is closing", "connection reset", "unexpected eof", "unavailable", "deadline exceeded", "internal error"} {
		if strings.Contains(strings.ToLower(decoded), uncertain) {
			return false
		}
	}
	return dockerProxyCompileExitPattern.MatchString(decoded)
}
