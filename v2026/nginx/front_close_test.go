package nginx

// Tests of the front's close and drain contracts (front.go; LBALT-IMPL.md
// §2.8, s-connect's and s-forward's contract notes): Close closes every
// upstream group and the body store, cuts forwarded websockets both ways,
// is safe while handlers still run and doesn't let one blocked close hold
// back the rest; BeginDrain never blocks. The first tests use the fakes;
// the rest the front NewFront builds, with real upstream groups and body
// store, against blocks on loopback.

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A connection whose Close blocks until released, as a TLS close_notify to
// a client that stopped reading can.
type testBlockingCloseConn struct {
	net.Conn
	// closed when a close begins
	closing     chan struct{}
	closingOnce sync.Once
	release     chan struct{}
}

// One end of a pipe, its close blocking until release closes.
func newTestBlockingCloseConn(t *testing.T) *testBlockingCloseConn {
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
	})
	return &testBlockingCloseConn{
		Conn:    serverConn,
		closing: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// Reports the close, then waits for the release before closing.
func (self *testBlockingCloseConn) Close() error {
	self.closingOnce.Do(func() {
		close(self.closing)
	})
	<-self.release
	return self.Conn.Close()
}

// A front NewFront builds, the production parts throughout, from a table
// and its certificates. It closes when the test ends.
func newTestCloseFront(t *testing.T, routeTable *RouteTable, certificateStore *CertificateStore, settings *FrontSettings) *Front {
	t.Helper()
	front, err := NewFront(t.Context(), routeTable, "test-revision", certificateStore, &testConnectHandler{}, "1.2.3", settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		front.Close()
	})
	return front
}

// Whether a group refuses new work, as its close leaves it.
func testUpstreamGroupClosed(group *upstreamGroup) bool {
	group.stateLock.Lock()
	defer group.stateLock.Unlock()
	return group.closed
}

// Close cuts everything at once: a hijacked connection whose close blocks,
// as a TLS close_notify to a client that stopped reading can, holds back
// neither the forwarders' closes, which cut the upstream sides of forwarded
// websockets, nor the body store's. The cut completes once the blocked
// close returns.
func TestFrontCloseCutsEverythingAtOnce(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		w.(http.Hijacker).Hijack()
	}
	blockingConn := newTestBlockingCloseConn(t)
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(blockingConn.release)
		})
	}
	defer release()
	testFront.front.HttpsHandler("eth1").ServeHTTP(&testHijackResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             blockingConn,
	}, newTestRequest("GET", "connect.service.example", "/", true))

	closed := make(chan string, len(testFront.forwarders)+1)
	for name, forwarder := range testFront.forwarders {
		forwarder.closeHook = func() {
			closed <- name
		}
	}
	testFront.bodyStore.closeHook = func() {
		closed <- "body store"
	}
	closeErrs := make(chan error, 1)
	go func() {
		closeErrs <- testFront.front.Close()
	}()
	waitTestBarrier(t, blockingConn.closing)
	for range len(testFront.forwarders) + 1 {
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Fatal("a close waited on the blocked close of a hijacked connection")
		}
	}
	release()
	if err := <-closeErrs; err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

// BeginDrain never blocks: it returns while the cut its drain deadline
// started is stuck in a forwarder's close, and a second call returns too.
func TestFrontBeginDrainNeverBlocks(t *testing.T) {
	settings := testFrontSettings()
	settings.Http.DrainTimeout = time.Nanosecond
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	stuck := make(chan struct{})
	defer close(stuck)
	cutStuck := make(chan struct{})
	var cutStuckOnce sync.Once
	testFront.forwarders["api"].closeHook = func() {
		cutStuckOnce.Do(func() {
			close(cutStuck)
		})
		<-stuck
	}
	testFront.front.BeginDrain()
	waitTestBarrier(t, cutStuck)
	testFront.front.BeginDrain()
	if !testFront.front.Draining() {
		t.Fatal("the front is not draining")
	}
}

// NewFront builds a group per service, sharing one body store, and both TLS
// configs; Close closes every group and the body store, which then refuse
// new work.
func TestNewFrontCloseClosesEveryUpstreamGroupAndTheBodyStore(t *testing.T) {
	vault := newTestTlsVault(t)
	front := newTestCloseFront(t, vault.routeTable, vault.load(t), testFrontSettings())
	bodyStore, ok := front.bodyStore.(*bodyStore)
	if !ok {
		t.Fatalf("the body store is a %T", front.bodyStore)
	}
	if len(front.serviceForwarders) != len(vault.routeTable.Services) || front.TlsConfig() == nil || front.QuicTlsConfig() == nil {
		t.Fatalf("NewFront built %d forwarders for %d services", len(front.serviceForwarders), len(vault.routeTable.Services))
	}
	groups := []*upstreamGroup{}
	for _, service := range vault.routeTable.Services {
		group, ok := front.serviceForwarders[service.Name].(*upstreamGroup)
		if !ok || group.service != service || group.bodyStore != bodyStore {
			t.Fatalf("%s's forwarder is not its upstream group over the front's body store", service.Name)
		}
		groups = append(groups, group)
	}

	if err := front.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	for _, group := range groups {
		if !testUpstreamGroupClosed(group) {
			t.Errorf("%s's group is open after Close", group.service.Name)
		}
		if block := group.selectBlock("", make([]bool, len(group.blocks)), nil); block != nil {
			t.Errorf("%s's closed group picked block %s", group.service.Name, block.name)
		}
	}
	if _, refusal := bodyStore.open(0, true); refusal == nil || refusal.reason != bodyRefusalClosed {
		t.Errorf("the closed body store opened a body: %+v", refusal)
	}
}

// The fixture's table for NewFront tests against blocks on loopback: the
// upstream is loopback, and service's blocks are one, g1 at port.
func newTestCloseRouteTable(t *testing.T, vault *testTlsVault, serviceName string, port int) *RouteTable {
	t.Helper()
	routeTable := vault.routeTable
	routeTable.Upstream.Ipv4 = "127.0.0.1"
	testRouteService(t, routeTable, serviceName).Blocks = []*RouteBlock{
		{Name: "g1", Weight: 1, Port: port},
	}
	return routeTable
}

// Close cuts a forwarded websocket both ways: the client's side, which the
// front tracks from its hijack, and the block's, which the service's
// upstream group holds.
func TestNewFrontCloseCutsAForwardedWebsocket(t *testing.T) {
	raw := newRawUpstream(t)
	vault := newTestTlsVault(t)
	routeTable := newTestCloseRouteTable(t, vault, "mcp", raw.port)
	mcp := testRouteService(t, routeTable, "mcp")
	mcp.Websocket = true
	mcp.Streamable = false
	front := newTestCloseFront(t, routeTable, vault.load(t), testFrontSettings())
	server := newTestConfiguredServerOf(t, front, false)

	clientConn, err := tls.Dial("tcp", server.Listener.Addr().String(), &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"http/1.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	clientConn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
	fmt.Fprintf(clientConn, "GET /socket HTTP/1.1\r\nHost: mcp.service.example\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	upstreamConn, upstreamReader, _ := raw.accept(t)
	upstreamConn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
	io.WriteString(upstreamConn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	clientReader := bufio.NewReader(clientConn)
	response, err := http.ReadResponse(clientReader, nil)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade = %v, %v; want the block's 101", response, err)
	}

	if err := front.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	readEnd(t, clientReader)
	readEnd(t, upstreamReader)
}

// Close is safe while handlers still run: a forwarded request its block
// holds ends once Close cuts the block's connection, Close completes, and a
// request after Close is a 503.
func TestNewFrontCloseWhileAForwardedRequestRuns(t *testing.T) {
	raw := newRawUpstream(t)
	vault := newTestTlsVault(t)
	routeTable := newTestCloseRouteTable(t, vault, "api", raw.port)
	front := newTestCloseFront(t, routeTable, vault.load(t), testFrontSettings())
	server := newTestConfiguredServerOf(t, front, false)
	client := server.Client()
	get := func() (int, error) {
		response, err := client.Do(newTestServerRequest(t, server, "GET", "api.service.example", "/v1", nil))
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode, nil
	}

	type result struct {
		statusCode int
		err        error
	}
	results := make(chan result, 1)
	go func() {
		statusCode, err := get()
		results <- result{
			statusCode: statusCode,
			err:        err,
		}
	}()
	// the block has the request, and never answers
	raw.accept(t)
	if err := front.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	inFlight := receiveBounded(t, results, "the in-flight request's end")
	if inFlight.err == nil && inFlight.statusCode != http.StatusBadGateway {
		t.Fatalf("the cut request = %d, want 502 or a cut connection", inFlight.statusCode)
	}
	if statusCode, err := get(); err != nil || statusCode != http.StatusServiceUnavailable {
		t.Fatalf("a request after Close = %d, %v; want 503", statusCode, err)
	}
}
