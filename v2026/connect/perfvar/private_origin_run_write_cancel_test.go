// Real HTTP/1 and TLS pipe I/O separate joined download workers from an
// origin response still owned by a canceled retained run.
package perfvar

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Only synthetic origins install this hook. Preserve the previous hook's
// context, then retain the accepted raw endpoint rather than TLS.Close.
type syntheticFastOriginRawConnKey struct{}

func syntheticFastOriginOwnConnContext(server *http.Server) {
	previous := server.ConnContext
	server.ConnContext = func(ctx context.Context, connection net.Conn) context.Context {
		if previous != nil {
			ctx = previous(ctx, connection)
		}
		if secure, ok := connection.(*tls.Conn); ok {
			connection = secure.NetConn()
		}
		return context.WithValue(ctx, syntheticFastOriginRawConnKey{}, connection)
	}
}

// Observation never delays a write; only the unread real pipe peer blocks it.
type privateOriginRunWriteConn struct {
	net.Conn
	writeActive   atomic.Bool
	expiredWrites atomic.Int64
	closeCalls    atomic.Int64
}

// The real TLS writer remains inside its actual net.Conn call while active.
func (self *privateOriginRunWriteConn) Write(data []byte) (int, error) {
	self.writeActive.Store(true)
	defer self.writeActive.Store(false)
	return self.Conn.Write(data)
}

// Observe an actual raw close without adding any gate or delay.
func (self *privateOriginRunWriteConn) Close() error {
	self.closeCalls.Add(1)
	return self.Conn.Close()
}

// Count cancellation deadlines separately from normal zero deadline resets.
func (self *privateOriginRunWriteConn) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		self.expiredWrites.Add(1)
	}
	return self.Conn.SetWriteDeadline(deadline)
}

// The existing in-memory listener and TLS factories own all real endpoints.
type privateOriginRunWriteFixture struct {
	ctx               context.Context
	cancel            context.CancelFunc
	origin            *syntheticFastOrigin
	settings          nativeBrowsingRunSettings
	listener          *nativeBrowsingOverlapListener
	server            *http.Server
	serverDone        chan error
	stateLock         sync.Mutex
	connections       []net.Conn
	responses         []*http.Response
	transports        []*http.Transport
	proxyWorkers      sync.WaitGroup
	connectionWorkers sync.WaitGroup
	closeOnce         sync.Once
	closeErr          error
	previousConnCalls atomic.Int64
}

// No socket, DNS lookup, borrowed fixture, Go policy or external actor exists.
func newPrivateOriginRunWriteFixture(t *testing.T, finiteBytes int64) *privateOriginRunWriteFixture {
	t.Helper()
	serverTls, clientTls, err := nativeBrowsingTlsConfigs()
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultSyntheticFastSettings()
	settings.BytesPerStream = finiteBytes
	ctx, cancel := context.WithTimeout(context.Background(), settings.OverallTimeout)
	self := &privateOriginRunWriteFixture{ctx: ctx, cancel: cancel,
		origin:   &syntheticFastOrigin{ctx: ctx, settings: settings},
		settings: nativeBrowsingRunSettings{originServerTlsForTest: serverTls, originClientTlsForTest: clientTls},
		listener: &nativeBrowsingOverlapListener{connections: make(chan net.Conn), closed: make(chan struct{})}, serverDone: make(chan error, 1)}
	server, servingListener := newNativeBrowsingInnerServer(self.origin, self.listener, self.settings)
	self.server = server
	self.server.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		self.previousConnCalls.Add(1)
		return ctx
	}
	syntheticFastOriginOwnConnContext(self.server)
	self.server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			self.connectionWorkers.Add(1)
		}
		if state == http.StateClosed || state == http.StateHijacked {
			self.connectionWorkers.Done()
		}
	}
	go func() { self.serverDone <- self.server.Serve(servingListener) }()
	t.Cleanup(func() {
		if err := self.close(); err != nil {
			t.Error(err)
		}
	})
	return self
}

// Registration happens before any worker can use or close these pipe owners.
func (self *privateOriginRunWriteFixture) remember(connections ...net.Conn) {
	if deadline, ok := self.ctx.Deadline(); ok {
		for _, connection := range connections {
			_ = connection.SetDeadline(deadline)
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connections = append(self.connections, connections...)
}

// This is the same accepted HTTP/1 TLS server path as the retained fixture.
func (self *privateOriginRunWriteFixture) peer(t *testing.T) (*tls.Conn, *privateOriginRunWriteConn) {
	t.Helper()
	clientPipe, serverPipe := net.Pipe()
	observed := &privateOriginRunWriteConn{Conn: serverPipe}
	self.remember(clientPipe, observed)
	select {
	case self.listener.connections <- observed:
	case <-self.ctx.Done():
		t.Fatal(self.ctx.Err())
	}
	settings := self.settings.originClientTlsForTest.Clone()
	settings.ServerName = "origin0.test"
	client := tls.Client(clientPipe, settings)
	if err := client.HandshakeContext(self.ctx); err != nil {
		t.Fatal(err)
	}
	// Complete a real finite exchange before the bulk response reuses this
	// TLS connection; the held write is not handshake or SYN establishment.
	request, err := http.NewRequest(http.MethodGet, "https://origin0.test/latency", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Write(client); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatal("origin TLS warm exchange changed")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return client, observed
}

// Read headers, retain the actual body unread, and leave its TLS socket owned.
func (self *privateOriginRunWriteFixture) heldResponse(t *testing.T, peer *tls.Conn, ordinal int) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://origin0.test/bulk/0?run=%d", ordinal), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Write(peer); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(peer), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.ProtoMajor != 1 {
		t.Fatal("actual TLS origin did not return HTTP/1 bulk headers")
	}
	self.stateLock.Lock()
	self.responses = append(self.responses, response)
	self.stateLock.Unlock()
	return response
}

// A split TLS endpoint keeps the origin body unread after the real client
// worker cancels. No callback fabricates worker, handler or quiescence state.
func (self *privateOriginRunWriteFixture) blockedClient(t *testing.T, upstream *tls.Conn) (*http.Client, <-chan error) {
	t.Helper()
	clientPipe, proxyPipe := net.Pipe()
	self.remember(clientPipe, proxyPipe)
	closed := make(chan error, 1)
	self.proxyWorkers.Add(1)
	go func() {
		defer self.proxyWorkers.Done()
		proxy := tls.Server(proxyPipe, self.settings.originServerTlsForTest.Clone())
		request, err := http.ReadRequest(bufio.NewReader(proxy))
		if err != nil {
			closed <- err
			return
		}
		if err := request.Write(upstream); err != nil {
			closed <- err
			return
		}
		response, err := http.ReadResponse(bufio.NewReader(upstream), request)
		if err != nil {
			closed <- err
			return
		}
		self.stateLock.Lock()
		self.responses = append(self.responses, response)
		self.stateLock.Unlock()
		// Reading neither the origin body nor an application response makes
		// a real origin TLS write block while the real request owner cancels.
		_, err = io.Copy(io.Discard, proxy)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, net.ErrClosed) {
			closed <- err
			return
		}
		closed <- nil
	}()
	var dialed atomic.Bool
	transport := newNativeBrowsingInnerTransport(func(ctx context.Context, _, _ string) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !dialed.CompareAndSwap(false, true) {
			return nil, errors.New("diagnostic opened an undeclared second application pipe")
		}
		return clientPipe, nil
	}, self.settings)
	transport.ResponseHeaderTimeout = self.origin.settings.HeaderTimeout
	self.transports = append(self.transports, transport)
	return &http.Client{Transport: transport}, closed
}

// Every endpoint closes before any body drain or worker wait can retain it.
func (self *privateOriginRunWriteFixture) close() error {
	self.closeOnce.Do(func() {
		self.cancel()
		self.stateLock.Lock()
		connections := append([]net.Conn(nil), self.connections...)
		self.stateLock.Unlock()
		for _, connection := range connections {
			_ = connection.Close()
		}
		for _, transport := range self.transports {
			transport.CloseIdleConnections()
		}
		_ = self.listener.Close()
		self.closeErr = errors.Join(self.closeErr, self.server.Close())
		ctx, cancel := context.WithTimeout(context.Background(), self.origin.settings.FixtureDrainTimeout)
		defer cancel()
		select {
		case err := <-self.serverDone:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				self.closeErr = errors.Join(self.closeErr, err)
			}
		case <-ctx.Done():
			self.closeErr = errors.Join(self.closeErr, ctx.Err())
		}
		joined := make(chan struct{})
		go func() { self.proxyWorkers.Wait(); self.connectionWorkers.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-ctx.Done():
			self.closeErr = errors.Join(self.closeErr, ctx.Err())
		}
		self.stateLock.Lock()
		responses := append([]*http.Response(nil), self.responses...)
		self.stateLock.Unlock()
		for _, response := range responses {
			_ = response.Body.Close()
		}
		for index := range self.origin.runs {
			if run := self.origin.runs[index].Load(); run != nil {
				self.closeErr = errors.Join(self.closeErr, run.sealAndWait(ctx))
			}
		}
		if self.origin.started.Load() != self.origin.finished.Load() {
			self.closeErr = errors.Join(self.closeErr, errors.New("origin fixture cleanup retained a handler"))
		}
	})
	return self.closeErr
}

// The actual workload workers finish before the sequence cancels its origin
// owner. The old fixture then fails precisely at the unchanged five-second seal.
func TestPrivateOriginRunCanceledWriteQuiesces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newPrivateOriginRunWriteFixture(t, 0)
		upstream, observed := fixture.peer(t)
		client, proxyClosed := fixture.blockedClient(t, upstream)
		settings := defaultSyntheticFastSettings()
		settings.InitialStreams, settings.MaximumStreams, settings.Adaptive, settings.LoadedLatency = 1, 1, false, false
		if settings.RunDrainTimeout != 5*time.Second {
			t.Fatal("origin regression changed the actual drain policy")
		}
		var blocked atomic.Bool
		sequence, _ := measureSyntheticFastSequence(fixture.ctx, settings, 1, "owned-http1-tls-write",
			func(ctx context.Context, ordinal int) (syntheticFastReport, error) {
				_, err := syntheticFastCarrierBeginOriginRun(fixture.origin, ctx, ordinal, false, nil)
				if err != nil {
					return syntheticFastReport{}, err
				}
				return measureSyntheticFast(ctx, client, func(int) string { return "https://origin0.test/bulk/0?run=0" }, settings, func() {
					if observed.writeActive.Load() && fixture.origin.streamStarted[0].Load() > fixture.origin.streamFinished[0].Load() {
						blocked.Store(true)
					}
				})
			}, func(ctx context.Context, ordinal int) error {
				return fixture.origin.runs[ordinal].Load().sealAndWait(ctx)
			},
			func() (syntheticFastCumulative, any) { return syntheticFastCumulative{}, nil })
		row := sequence.Runs[0]
		if !blocked.Load() || !row.Workload.Joined || row.Workload.WorkerJoinState != "WORKERS_JOINED" || row.Workload.MeasurementBytes != 0 {
			t.Fatal("regression did not compose real blocked origin I/O with joined zero-payload workers")
		}
		if err := <-proxyClosed; err != nil {
			t.Fatal(err)
		}
		if row.Workload.Valid || sequence.Valid {
			t.Fatal("the deliberately withheld response became a valid performance result")
		}
		quiescent := row.RequestsQuiescent
		started, finished := fixture.origin.streamStarted[0].Load(), fixture.origin.streamFinished[0].Load()
		if started != 1 || quiescent && finished != 1 || !quiescent && (finished != 0 || !observed.writeActive.Load()) {
			t.Fatalf("origin seal state: started=%d finished=%d quiescent=%t write_active=%t raw_closes=%d expired_deadlines=%d", started, finished, quiescent, observed.writeActive.Load(), observed.closeCalls.Load(), observed.expiredWrites.Load())
		}
		closedByRun := observed.closeCalls.Load() != 0
		if err := fixture.close(); err != nil {
			t.Fatal(err)
		}
		if !quiescent {
			t.Fatalf("ORIGIN_RUN_CANCEL_IO_HELD: joined_workers=true origin_quiescent=false drain_ns=%d stop=%s", settings.RunDrainTimeout, sequence.Stop)
		}
		if sequence.Stop != "completed" || !closedByRun {
			t.Fatal("canceled origin did not close only its own raw I/O")
		}
		t.Log("[origin-write-cancel] real_http1_tls=true joined_workers=true zero_payload_invalid=true origin_quiescent=true cleanup_joined=true")
	})
}

// Completed handlers unregister cancellation before the same healthy TLS
// connection is reused by the next one of five distinct retained run owners.
func TestPrivateOriginRunHealthyFiveReusedConnections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newPrivateOriginRunWriteFixture(t, 32*1024)
		var dials atomic.Int64
		clientPipe, serverPipe := net.Pipe()
		observed := &privateOriginRunWriteConn{Conn: serverPipe}
		fixture.remember(clientPipe, observed)
		fixture.listener.connections <- observed
		transport := newNativeBrowsingInnerTransport(func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if dials.Add(1) != 1 {
				return nil, errors.New("healthy reuse opened an undeclared second pipe")
			}
			return clientPipe, nil
		}, fixture.settings)
		transport.ResponseHeaderTimeout = fixture.origin.settings.HeaderTimeout
		fixture.transports = append(fixture.transports, transport)
		client := &http.Client{Transport: transport}
		for ordinal := range 5 {
			ctx, cancel := context.WithCancel(fixture.ctx)
			run, err := syntheticFastCarrierBeginOriginRun(fixture.origin, ctx, ordinal, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://origin0.test/bulk/0?run=%d", ordinal), nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := nativeBrowsingVerifyTlsResponse(response, fixture.settings); err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil || !bytes.Equal(body, syntheticFastBlock(0)) || response.Trailer.Get("X-Synthetic-Complete") != "true" {
				t.Fatal("healthy real TLS response lost exact body ownership")
			}
			cancel()
			synctest.Wait()
			join, release := context.WithTimeout(fixture.ctx, fixture.origin.settings.RunDrainTimeout)
			if err := run.sealAndWait(join); err != nil {
				t.Fatal(err)
			}
			release()
		}
		if dials.Load() != 1 || fixture.previousConnCalls.Load() != 1 || observed.closeCalls.Load() != 0 {
			t.Fatal("healthy five-run HTTP/1 connection was closed or poisoned by an old owner")
		}
		if err := fixture.close(); err != nil {
			t.Fatal(err)
		}
		t.Log("[origin-write-reuse] runs=5 tls_connections=1 prior_owner_cancellation_isolated=true cleanup_joined=true")
	})
}

// Two real response writers block independently. Canceling one owner must
// neither interrupt the other writer nor consume its healthy body budget.
func TestPrivateOriginRunCancelPreservesUnrelatedWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newPrivateOriginRunWriteFixture(t, 32*1024)
		ctx0, cancel0 := context.WithCancel(fixture.ctx)
		run0, _ := syntheticFastCarrierBeginOriginRun(fixture.origin, ctx0, 0, false, nil)
		ctx1, cancel1 := context.WithCancel(fixture.ctx)
		defer cancel1()
		run1, _ := syntheticFastCarrierBeginOriginRun(fixture.origin, ctx1, 1, false, nil)
		peer0, writer0 := fixture.peer(t)
		peer1, writer1 := fixture.peer(t)
		_ = fixture.heldResponse(t, peer0, 0)
		response1 := fixture.heldResponse(t, peer1, 1)
		synctest.Wait()
		if !writer0.writeActive.Load() || !writer1.writeActive.Load() {
			t.Fatal("unrelated-writer control did not hold real response writes")
		}
		cancel0()
		join, release := context.WithTimeout(fixture.ctx, fixture.origin.settings.RunDrainTimeout)
		err0 := run0.sealAndWait(join)
		release()
		if err0 != nil {
			t.Fatalf("canceled response remained owned: error=%v started=%d finished=%d write_active=%t raw_closes=%d expired_deadlines=%d", err0, fixture.origin.streamStarted[0].Load(), fixture.origin.streamFinished[0].Load(), writer0.writeActive.Load(), writer0.closeCalls.Load(), writer0.expiredWrites.Load())
		}
		synctest.Wait()
		if !writer1.writeActive.Load() || writer1.expiredWrites.Load() != 0 || writer1.closeCalls.Load() != 0 {
			t.Fatal("one run canceled an unrelated real TLS writer")
		}
		body, err := io.ReadAll(response1.Body)
		if err != nil || !bytes.Equal(body, syntheticFastBlock(0)) {
			t.Fatal("healthy unrelated response lost its full body budget")
		}
		_ = response1.Body.Close()
		join, release = context.WithTimeout(fixture.ctx, fixture.origin.settings.RunDrainTimeout)
		if err := run1.sealAndWait(join); err != nil {
			t.Fatal(err)
		}
		release()
		if err := fixture.close(); err != nil {
			t.Fatal(err)
		}
		t.Log("[origin-write-sibling] canceled_owner_joined=true unrelated_body_exact=true cleanup_joined=true")
	})
}
