// Inner website protocol is independent of the outer H1 carrier. The default
// branch preserves the existing H1 setup; h2 uses the standard Go implementation.
package perfvar

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
)

// One protocol setting changes no dial path, connection cap, budget or timer.
func nativeBrowsingInnerAlpn(http2 bool) string {
	if http2 {
		return "h2"
	}
	return "http/1.1"
}

// A real TLS listener is served by an explicitly HTTP/2-enabled standard
// Server. H1 remains accepted solely for the unchanged setup qualification.
func newPrivateSdkBrowsingInnerServer(handler http.Handler, listener net.Listener, settings nativeBrowsingRunSettings) (*http.Server, net.Listener) {
	server := &http.Server{Handler: handler}
	if settings.originServerTlsForTest == nil {
		return server, listener
	}
	config := settings.originServerTlsForTest.Clone()
	if settings.innerHttp2ForTest {
		config.NextProtos = []string{"h2", "http/1.1"}
		protocols := &http.Protocols{}
		protocols.SetHTTP1(true)
		protocols.SetHTTP2(true)
		server.Protocols = protocols
		server.TLSConfig = config
	}
	return server, tls.NewListener(listener, config)
}

// The custom dial still returns the actual TUN connection after authenticated
// TLS. An empty TLSNextProto retains default H1; h2 explicitly installs the
// standard implementation and accepts no negotiated H1 fallback.
func newPrivateSdkBrowsingInnerTransport(dialOrigin func(context.Context, string, string) (net.Conn, error), settings nativeBrowsingRunSettings) *http.Transport {
	transport := &http.Transport{MaxConnsPerHost: nativeBrowsingConnectionsPerOrigin,
		MaxIdleConns:        nativeBrowsingOriginCount * nativeBrowsingConnectionsPerOrigin,
		MaxIdleConnsPerHost: nativeBrowsingConnectionsPerOrigin, DialContext: dialOrigin}
	if settings.originClientTlsForTest != nil {
		transport.TLSClientConfig = settings.originClientTlsForTest.Clone()
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		if settings.innerHttp2ForTest {
			protocols := &http.Protocols{}
			protocols.SetHTTP2(true)
			transport.Protocols = protocols
			transport.ForceAttemptHTTP2 = true
			transport.TLSNextProto = nil
		}
		transport.DialTLSContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
			settings.innerHttpTraceForTest.beginDial()
			defer settings.innerHttpTraceForTest.endDial()
			raw, err := dialOrigin(dialCtx, network, address)
			if err != nil {
				return nil, err
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				raw.Close()
				return nil, err
			}
			config := settings.originClientTlsForTest.Clone()
			config.ServerName = host
			if settings.innerHttp2ForTest {
				config.NextProtos = []string{"h2"}
			}
			connection := tls.Client(raw, config)
			trace := httptrace.ContextClientTrace(dialCtx)
			if trace != nil && trace.TLSHandshakeStart != nil {
				trace.TLSHandshakeStart()
			}
			err = connection.HandshakeContext(dialCtx)
			state := connection.ConnectionState()
			settings.innerHttpTraceForTest.handshake(err)
			if trace != nil && trace.TLSHandshakeDone != nil {
				trace.TLSHandshakeDone(state, err)
			}
			if err != nil {
				connection.Close()
				return nil, err
			}
			if state.NegotiatedProtocol != nativeBrowsingInnerAlpn(settings.innerHttp2ForTest) || len(state.VerifiedChains) == 0 {
				connection.Close()
				return nil, errors.New("synthetic HTTPS origin did not verify its root/name and required inner ALPN")
			}
			return settings.innerHttpTraceForTest.own(connection), nil
		}
	}
	return transport
}

// Only scalar summaries and fixture-local ordinals are serialized. Stream
// concurrency means header-written request/body lifetimes sharing a connection;
// it is not a frame-level census or a browser-priority/flow-control equivalence.
type nativeBrowsingInnerHttpReport struct {
	LateTraceCallbacks       uint64 `json:"late_trace_callbacks"`
	Http2                    bool   `json:"http2"`
	DialStarted              uint64 `json:"dial_started"`
	DialFinished             uint64 `json:"dial_finished"`
	DialsInFlight            uint64 `json:"dials_in_flight"`
	HandshakeCount           uint64 `json:"handshake_count"`
	HandshakeErrors          uint64 `json:"handshake_errors"`
	OpenedConnections        uint64 `json:"opened_connections"`
	ClosedConnections        uint64 `json:"closed_connections"`
	LiveConnections          uint64 `json:"live_connections"`
	HighestConnectionOrdinal uint64 `json:"highest_connection_ordinal"`
	RequestsStarted          uint64 `json:"requests_started"`
	RequestsFinished         uint64 `json:"requests_finished"`
	LiveRequests             uint64 `json:"live_requests"`
	Acquisitions             uint64 `json:"acquisitions"`
	ReusedAcquisitions       uint64 `json:"reused_acquisitions"`
	RequestWrites            uint64 `json:"request_writes"`
	RequestWriteErrors       uint64 `json:"request_write_errors"`
	Responses                uint64 `json:"responses"`
	ServerRequests           uint64 `json:"server_requests"`
	MaximumConcurrentStreams uint64 `json:"maximum_concurrent_header_written_requests"`
	MultiplexedConnections   uint64 `json:"connections_with_overlapping_header_written_requests"`
	ProtocolErrors           uint64 `json:"protocol_errors"`
	UnknownConnections       uint64 `json:"unknown_connections"`
	ConnectionOverflow       uint64 `json:"connection_overflow"`
}

// The existing workload permits at most four live connections at four origins.
// Closed entries are removed immediately; active request objects retain only
// their own scalar state through their existing bounded worker lifetime.
type nativeBrowsingInnerHttpTrace struct {
	stateLock   sync.Mutex
	http2       bool
	connections map[*nativeBrowsingInnerConn]*nativeBrowsingInnerConnection
	changed     *clientconnect.Monitor
	report      nativeBrowsingInnerHttpReport
}

// Mutable fields are protected by the trace's one observation lock.
type nativeBrowsingInnerConnection struct {
	ordinal     uint64
	streams     uint64
	multiplexed bool
}

// The pinned Go1.27 transport dispatches ALPN through ConnectionState and passes
// net.Conn to its bundled h2 implementation. This wrapper relies on that path,
// not the older concrete-*tls.Conn TLSNextProto fallback. Close delegates first.
type nativeBrowsingInnerConn struct {
	*tls.Conn
	trace     *nativeBrowsingInnerHttpTrace
	closeOnce sync.Once
}

// A request is tracked only until its response-body close/deferred finish.
// Reacquisition moves that request between connections, never double-counting.
type nativeBrowsingInnerRequest struct {
	trace      *nativeBrowsingInnerHttpTrace
	connection *nativeBrowsingInnerConnection
	written    bool
	finished   bool
}

// Construction changes no runtime or process-global policy.
func newNativeBrowsingInnerHttpTrace(http2 bool) *nativeBrowsingInnerHttpTrace {
	return &nativeBrowsingInnerHttpTrace{
		http2:       http2,
		connections: map[*nativeBrowsingInnerConn]*nativeBrowsingInnerConnection{},
		changed:     clientconnect.NewMonitor(),
		report:      nativeBrowsingInnerHttpReport{Http2: http2},
	}
}

// Count actual dial ownership, including losing speculative dials.
func (self *nativeBrowsingInnerHttpTrace) beginDial() {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.report.DialStarted++
	self.report.DialsInFlight++
	self.changed.NotifyAll()
}

// A terminal dial is not necessarily a successfully registered connection.
func (self *nativeBrowsingInnerHttpTrace) endDial() {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.report.DialFinished++
	self.report.DialsInFlight--
	self.changed.NotifyAll()
}

// Handshake errors are retained, not all fatal: canceled losing dials remain
// possible. Every registered/used connection must nevertheless authenticate.
func (self *nativeBrowsingInnerHttpTrace) handshake(err error) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.report.HandshakeCount++
	if err != nil {
		self.report.HandshakeErrors++
	}
}

// Takes no new packet share; the HTTP transport retains the actual TLS socket.
func (self *nativeBrowsingInnerHttpTrace) own(connection *tls.Conn) net.Conn {
	if self == nil {
		return connection
	}
	owned := &nativeBrowsingInnerConn{Conn: connection, trace: self}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.report.OpenedConnections++
	if len(self.connections) >= nativeBrowsingOriginCount*nativeBrowsingConnectionsPerOrigin || self.report.HighestConnectionOrdinal == ^uint64(0) {
		self.report.ConnectionOverflow++
		return owned
	}
	self.report.HighestConnectionOrdinal++
	self.connections[owned] = &nativeBrowsingInnerConnection{ordinal: self.report.HighestConnectionOrdinal}
	self.report.LiveConnections = uint64(len(self.connections))
	self.changed.NotifyAll()
	return owned
}

// No observation lock is held during a native close or any transport operation.
func (self *nativeBrowsingInnerConn) Close() error {
	err := self.Conn.Close()
	self.closeOnce.Do(func() {
		self.trace.stateLock.Lock()
		defer self.trace.stateLock.Unlock()
		delete(self.trace.connections, self)
		self.trace.report.ClosedConnections++
		self.trace.report.LiveConnections = uint64(len(self.trace.connections))
		self.trace.changed.NotifyAll()
	})
	return err
}

// Admission is exactly one existing logical HTTP worker, not another queue.
func (self *nativeBrowsingInnerHttpTrace) newRequest() *nativeBrowsingInnerRequest {
	if self == nil {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.report.RequestsStarted++
	self.report.LiveRequests++
	return &nativeBrowsingInnerRequest{trace: self}
}

// Composes with existing HTML/asset traces. No callback retains application
// bytes or emits per-packet logs; the lock never spans network operations.
func (self *nativeBrowsingInnerRequest) clientTrace(navigation *nativeBrowsingHttpTrace) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			self.trace.stateLock.Lock()
			defer self.trace.stateLock.Unlock()
			if self.finished {
				self.trace.report.LateTraceCallbacks++
				return
			}
			if self.written {
				self.connection.streams--
				self.written = false
			}
			self.trace.report.Acquisitions++
			if info.Reused {
				self.trace.report.ReusedAcquisitions++
			}
			owned, ok := info.Conn.(*nativeBrowsingInnerConn)
			self.connection = self.trace.connections[owned]
			if !ok || self.connection == nil {
				self.trace.report.UnknownConnections++
				return
			}
			if navigation != nil {
				navigation.connectionOrdinal.Store(self.connection.ordinal)
				navigation.connectionReused.Store(info.Reused)
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			self.trace.stateLock.Lock()
			defer self.trace.stateLock.Unlock()
			if self.finished {
				self.trace.report.LateTraceCallbacks++
				return
			}
			if info.Err != nil {
				self.trace.report.RequestWriteErrors++
				return
			}
			self.trace.report.RequestWrites++
			if self.connection != nil && !self.written {
				self.connection.streams++
				self.written = true
				self.trace.report.MaximumConcurrentStreams = max(self.trace.report.MaximumConcurrentStreams, self.connection.streams)
				if self.connection.streams >= 2 && !self.connection.multiplexed {
					self.connection.multiplexed = true
					self.trace.report.MultiplexedConnections++
				}
			}
		},
	}
}

// Called after Body.Close. The state is not a live HTTP/2 internal stream
// accessor; it is the exact header-written request/body ownership interval.
func (self *nativeBrowsingInnerRequest) finish() {
	if self == nil {
		return
	}
	self.trace.stateLock.Lock()
	defer self.trace.stateLock.Unlock()
	if self.finished {
		return
	}
	self.finished = true
	if self.written {
		self.connection.streams--
		self.written = false
	}
	self.connection = nil
	self.trace.report.RequestsFinished++
	self.trace.report.LiveRequests--
	self.trace.changed.NotifyAll()
}

// Every measured response must match the configured authenticated protocol.
// Setup qualification remains H1 and is outside this observer entirely.
func (self *nativeBrowsingInnerHttpTrace) observeResponse(response *http.Response) error {
	if self == nil {
		return nil
	}
	valid := response != nil && response.ProtoMajor == 1
	if self.http2 {
		valid = response != nil && response.ProtoMajor == 2
	}
	valid = valid && response.TLS != nil && response.TLS.NegotiatedProtocol == nativeBrowsingInnerAlpn(self.http2) && len(response.TLS.VerifiedChains) != 0
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.report.Responses++
	if !valid {
		self.report.ProtocolErrors++
		return errors.New("measured inner HTTP response was not authenticated with its required protocol")
	}
	return nil
}

// Only measured synthetic workload paths are checked; unchanged H1 readiness
// GETs still use the same real listeners and are excluded from body accounting.
func (self *nativeBrowsingInnerHttpTrace) wrapHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/page/") || strings.HasPrefix(request.URL.Path, "/asset/") {
			wantMajor := 1
			if self.http2 {
				wantMajor = 2
			}
			valid := request.ProtoMajor == wantMajor && request.TLS != nil && request.TLS.NegotiatedProtocol == nativeBrowsingInnerAlpn(self.http2)
			self.stateLock.Lock()
			self.report.ServerRequests++
			if !valid {
				self.report.ProtocolErrors++
			}
			self.stateLock.Unlock()
			if !valid {
				http.Error(writer, "synthetic inner protocol mismatch", http.StatusBadRequest)
				return
			}
		}
		handler.ServeHTTP(writer, request)
	})
}

// The unchanged driver join deadline bounds these observer-owned dial/socket
// receipts too. Subscribe and inspect under the same lock; no polling or timer.
func (self *nativeBrowsingInnerHttpTrace) waitJoined(ctx context.Context) error {
	if self == nil {
		return nil
	}
	for {
		self.stateLock.Lock()
		changed := self.changed.NotifyChannel()
		joined := self.report.DialsInFlight == 0 && self.report.LiveRequests == 0 && len(self.connections) == 0
		self.stateLock.Unlock()
		if joined {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return errors.New("inner HTTP dial/request/socket owners did not join within the existing cleanup deadline")
		}
	}
}

// Copies a bounded scalar value, never connection objects or raw identities.
func (self *nativeBrowsingInnerHttpTrace) snapshot() nativeBrowsingInnerHttpReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.report
}

// A successful screen needs protocol and owner evidence, not just ALPN intent.
// This is not a whole Go-runtime or browser-engine memory qualification.
func (self *nativeBrowsingInnerHttpTrace) verdict() error {
	report := self.snapshot()
	if report.ProtocolErrors != 0 || report.UnknownConnections != 0 || report.ConnectionOverflow != 0 || report.LiveConnections != 0 || report.DialsInFlight != 0 || report.LiveRequests != 0 || report.DialStarted != report.DialFinished || report.OpenedConnections != report.ClosedConnections || report.RequestsStarted != report.RequestsFinished || report.Responses == 0 || report.ServerRequests == 0 {
		return fmt.Errorf("inner HTTP protocol/owner proof incomplete: protocol_errors=%d unknown=%d overflow=%d live_connections=%d live_dials=%d live_requests=%d", report.ProtocolErrors, report.UnknownConnections, report.ConnectionOverflow, report.LiveConnections, report.DialsInFlight, report.LiveRequests)
	}
	if self.http2 && (report.MultiplexedConnections == 0 || report.ReusedAcquisitions == 0) {
		return errors.New("inner HTTP/2 had no observed same-connection overlapping request/reuse premise")
	}
	return nil
}

// Small loopback controls reuse the exact standard server and transport
// constructors used by actual100; no SDK or database fixture is substituted.
type nativeBrowsingInnerControl struct {
	ctx         context.Context
	cancel      context.CancelFunc
	client      *http.Client
	transport   *http.Transport
	server      *http.Server
	serveResult chan error
	trace       *nativeBrowsingInnerHttpTrace
	closeOnce   sync.Once
	closeErr    error
}

// Server/client protocol disagreement is permitted only to test refusal.
// All dials terminate at this already-owned listener, even for a wrong-SAN test.
func newNativeBrowsingInnerControl(t *testing.T, serverHttp2, clientHttp2 bool, handler http.Handler, configureClient func(*tls.Config)) *nativeBrowsingInnerControl {
	t.Helper()
	serverTls, clientTls, err := privateSdkBrowsingTlsConfigs()
	if err != nil {
		t.Fatal("generate synthetic TLS control configs")
	}
	if configureClient != nil {
		configureClient(clientTls)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal("bind owned inner protocol control")
	}
	trace := newNativeBrowsingInnerHttpTrace(clientHttp2)
	settings := nativeBrowsingRunSettings{maxLivePages: 4, innerHttp2ForTest: serverHttp2, originServerTlsForTest: serverTls, originClientTlsForTest: clientTls}
	server, secureListener := newPrivateSdkBrowsingInnerServer(trace.wrapHandler(handler), listener, settings)
	server.ErrorLog = log.New(io.Discard, "", 0)
	settings.innerHttp2ForTest, settings.innerHttpTraceForTest = clientHttp2, trace
	transport := newPrivateSdkBrowsingInnerTransport(func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" || host != "origin0.test" && host != "unowned.example" {
			return nil, errors.New("unexpected synthetic control destination")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	}, settings)
	control := &nativeBrowsingInnerControl{ctx: ctx, cancel: cancel, client: &http.Client{Transport: transport}, transport: transport, server: server, serveResult: make(chan error, 1), trace: trace}
	go func() { control.serveResult <- server.Serve(secureListener) }()
	t.Cleanup(func() { control.close(t) })
	return control
}

// Joins serve, request/dial/socket receipts under a finite control-only bound.
// This does not claim access to standard HTTP/2's unexported worker census.
func (self *nativeBrowsingInnerControl) close(t *testing.T) {
	t.Helper()
	self.closeOnce.Do(func() {
		self.cancel()
		self.transport.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := self.server.Shutdown(ctx); err != nil {
			self.closeErr = errors.Join(self.closeErr, err, self.server.Close())
		}
		select {
		case err := <-self.serveResult:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				self.closeErr = errors.Join(self.closeErr, err)
			}
		case <-ctx.Done():
			self.closeErr = errors.Join(self.closeErr, errors.New("inner control Serve owner did not join"))
		}
		self.closeErr = errors.Join(self.closeErr, self.trace.waitJoined(ctx))
	})
	if self.closeErr != nil {
		t.Error("inner control cleanup did not join all owned receipts")
	}
}

// A successful call lends the response body to the caller, who closes it
// before finishing its request observer. No pooled application data is retained.
func (self *nativeBrowsingInnerControl) request(ctx context.Context, host, path string) (*http.Response, *nativeBrowsingInnerRequest, *nativeBrowsingHttpTrace, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	owner := self.trace.newRequest()
	navigation := &nativeBrowsingHttpTrace{}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), owner.clientTrace(navigation)))
	response, err := self.client.Do(request)
	if err != nil {
		owner.finish()
		return nil, nil, nil, err
	}
	if err := self.trace.observeResponse(response); err != nil {
		response.Body.Close()
		owner.finish()
		return nil, nil, nil, err
	}
	return response, owner, navigation, nil
}

// Reads one exact bounded positive response and returns only connection facts.
func (self *nativeBrowsingInnerControl) fetch(t *testing.T, path, expected string) (uint64, bool) {
	t.Helper()
	response, owner, navigation, err := self.request(self.ctx, "origin0.test", path)
	if err != nil {
		t.Fatal("synthetic inner HTTP request failed")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, int64(len(expected)+1)))
	closeErr := response.Body.Close()
	owner.finish()
	if response.StatusCode != http.StatusOK || string(body) != expected || readErr != nil || closeErr != nil {
		t.Fatal("synthetic inner HTTP exact status/body/close mismatch")
	}
	return navigation.connectionOrdinal.Load(), navigation.connectionReused.Load()
}

// Real independent h2 streams share one authenticated TCP connection. Canceling
// the held child must reach its server context while healthy and later requests
// complete on that same connection, without releasing the held handler first.
func TestPrivateSdkInnerHttp2MultiplexesCancellationAndReuse(t *testing.T) {
	entered, canceled, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var serverLock sync.Mutex
	heldAddress := ""
	sameConnection := true
	control := newNativeBrowsingInnerControl(t, true, true, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/asset/0/2" {
			defer close(finished)
			serverLock.Lock()
			heldAddress = request.RemoteAddr
			serverLock.Unlock()
			if _, err := io.WriteString(writer, "synthetic prefix"); err != nil {
				return
			}
			writer.(http.Flusher).Flush()
			close(entered)
			select {
			case <-request.Context().Done():
				close(canceled)
			case <-release:
			}
			return
		}
		serverLock.Lock()
		sameConnection = sameConnection && heldAddress != "" && heldAddress == request.RemoteAddr
		serverLock.Unlock()
		_, _ = io.WriteString(writer, "healthy exact response")
	}), nil)
	childCtx, cancelChild := context.WithCancel(control.ctx)
	defer cancelChild()
	response, owner, navigation, err := control.request(childCtx, "origin0.test", "/asset/0/2")
	if err != nil {
		t.Fatal("held actual HTTP/2 request failed before its prefix")
	}
	defer owner.finish()
	defer response.Body.Close()
	prefix := make([]byte, len("synthetic prefix"))
	if _, err := io.ReadFull(response.Body, prefix); err != nil || string(prefix) != "synthetic prefix" {
		t.Fatal("held actual HTTP/2 prefix was not exact")
	}
	select {
	case <-entered:
	case <-control.ctx.Done():
		t.Fatal("held h2 handler did not publish its actual prefix")
	}
	ordinal, reused := control.fetch(t, "/page/0", "healthy exact response")
	if ordinal == 0 || ordinal != navigation.connectionOrdinal.Load() || !reused {
		t.Fatal("healthy second stream did not reuse the held stream's exact TCP connection")
	}
	cancelChild()
	_, readErr := io.ReadAll(io.LimitReader(response.Body, 1))
	closeErr := response.Body.Close()
	owner.finish()
	if !errors.Is(readErr, context.Canceled) || closeErr != nil {
		t.Fatal("child h2 cancellation did not preserve its expected read/close disposition")
	}
	select {
	case <-canceled:
	case <-control.ctx.Done():
		t.Fatal("child h2 cancellation did not reach its actual server handler")
	}
	select {
	case <-finished:
	case <-control.ctx.Done():
		t.Fatal("canceled actual h2 handler did not join")
	}
	nextOrdinal, nextReused := control.fetch(t, "/page/1", "healthy exact response")
	serverLock.Lock()
	actualSameConnection := sameConnection
	serverLock.Unlock()
	if nextOrdinal != ordinal || !nextReused || !actualSameConnection {
		t.Fatal("child cancellation closed or replaced the healthy shared TCP connection")
	}
	control.close(t)
	if err := control.trace.verdict(); err != nil {
		t.Fatal("actual h2 protocol/ownership proof was incomplete")
	}
	report := control.trace.snapshot()
	if report.OpenedConnections != 1 || report.ClosedConnections != 1 || report.Responses != 3 || report.ServerRequests != 3 || report.MultiplexedConnections != 1 || report.MaximumConcurrentStreams < 2 {
		t.Fatalf("actual h2 connection/stream facts changed: %+v", report)
	}
}

// Default mode still pins the old verified H1 transport options and performs
// real sequential H1 reuse using the same construction helpers as actual100.
func TestPrivateSdkInnerHttpDefaultRemainsVerifiedHttp1(t *testing.T) {
	control := newNativeBrowsingInnerControl(t, false, false, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "default exact h1")
	}), nil)
	if control.transport.ForceAttemptHTTP2 || control.transport.Protocols != nil || control.transport.TLSNextProto == nil || len(control.transport.TLSNextProto) != 0 || control.transport.MaxConnsPerHost != nativeBrowsingConnectionsPerOrigin {
		t.Fatal("default verified H1 transport or connection cap changed")
	}
	first, _ := control.fetch(t, "/page/0", "default exact h1")
	second, reused := control.fetch(t, "/page/1", "default exact h1")
	if first == 0 || first != second || !reused {
		t.Fatal("default H1 exact connection reuse changed")
	}
	control.close(t)
	if err := control.trace.verdict(); err != nil {
		t.Fatal("default H1 ownership/protocol proof was incomplete")
	}
	if (nativeBrowsingRunSettings{maxLivePages: 4, innerHttp2ForTest: true}).validate() == nil {
		t.Fatal("inner h2 admitted a plaintext or unpaired TLS mode")
	}
	total := 0
	for page := range nativeBrowsingPageCount {
		html := []byte(strings.ReplaceAll(string(nativeBrowsingHTML(page)), "http://", "https://")[:nativeBrowsingHTMLBytes])
		pageBytes := len(html)
		for asset := range nativeBrowsingAssetCount {
			pageBytes += len(nativeBrowsingAsset(page, asset))
		}
		if len(html) != nativeBrowsingHTMLBytes || pageBytes != nativeBrowsingPageBytes(page) || pageBytes < 50*1024 || 300*1024 < pageBytes {
			t.Fatal("inner protocol variant changed the exact HTML/page/body plan")
		}
		total += pageBytes
	}
	if total != 17879040 {
		t.Fatal("inner protocol variant changed total offered payload")
	}
}

// Real TLS rejects an untrusted root, a wrong SAN and an H1-only server rather
// than falling back. Fixed value checks also reject a false response protocol
// or a claimed ALPN with no verified chain; errors do not log raw TLS text.
func TestPrivateSdkInnerHttp2RejectsTlsAndProtocolFallback(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		serverHttp2 bool
		wrongName   bool
		untrusted   bool
	}{
		{name: "untrusted-root", serverHttp2: true, untrusted: true},
		{name: "wrong-name", serverHttp2: true, wrongName: true},
		{name: "h1-only-origin", serverHttp2: false},
	} {
		func() {
			control := newNativeBrowsingInnerControl(t, testCase.serverHttp2, true, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.WriteString(writer, "must not be accepted")
			}), func(config *tls.Config) {
				if testCase.untrusted {
					config.RootCAs = x509.NewCertPool()
				}
			})
			defer control.close(t)
			host := "origin0.test"
			if testCase.wrongName {
				host = "unowned.example"
			}
			response, owner, _, err := control.request(control.ctx, host, "/page/0")
			if response != nil {
				response.Body.Close()
				owner.finish()
			}
			if err == nil {
				t.Fatalf("strict inner h2 refusal was bypassed: %s", testCase.name)
			}
			report := control.trace.snapshot()
			if report.ServerRequests != 0 || report.OpenedConnections != 0 || report.Responses != 0 {
				t.Fatalf("refused inner protocol was used: %s", testCase.name)
			}
		}()
	}
	for _, response := range []*http.Response{
		{ProtoMajor: 1, TLS: &tls.ConnectionState{NegotiatedProtocol: "h2", VerifiedChains: [][]*x509.Certificate{{{}}}}},
		{ProtoMajor: 2, TLS: &tls.ConnectionState{NegotiatedProtocol: "h2"}},
		{ProtoMajor: 2, TLS: &tls.ConnectionState{NegotiatedProtocol: "http/1.1", VerifiedChains: [][]*x509.Certificate{{{}}}}},
	} {
		trace := newNativeBrowsingInnerHttpTrace(true)
		if trace.observeResponse(response) == nil || trace.snapshot().ProtocolErrors != 1 {
			t.Fatal("false inner protocol/authentication receipt was accepted")
		}
	}
}

// Rich counters remain fixed scalars. All 100 navigation ordinal/reuse fields
// are included in the existing worst-case pressure-report bound as well.
func TestPrivateSdkInnerHttpReportIsBounded(t *testing.T) {
	report := nativeBrowsingInnerHttpReport{Http2: true}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal("marshal fixed inner HTTP report")
	}
	var columns map[string]any
	if err := json.Unmarshal(encoded, &columns); err != nil {
		t.Fatal("inspect fixed inner HTTP report columns")
	}
	for name, value := range columns {
		if _, ok := value.(float64); ok {
			columns[name] = ^uint64(0)
		}
	}
	encoded, err = json.Marshal(columns)
	if err != nil || len(encoded) >= 4*1024 {
		t.Fatal("fixed inner HTTP report exceeded its reserved 4-KiB allowance")
	}
}
