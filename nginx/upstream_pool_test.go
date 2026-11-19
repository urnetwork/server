package nginx

// Tests of the keepalive pools: nginx's keepalive, keepalive_requests,
// keepalive_time and keepalive_timeout per block, the least recently used
// eviction, and the MSG_PEEK check that drops a connection the block
// closed before any request is written to it.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The connections a block's requests came on, in order.
func requestConns(upstream *recordingUpstream) []string {
	conns := []string{}
	for _, recorded := range upstream.recorded() {
		conns = append(conns, recorded.remoteAddr)
	}
	return conns
}

// Sends a request through the front and waits for serveHttp to return, so
// its connection is back in the pool.
func sendAndWait(t *testing.T, front *testForwardFront, path string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, front.url(path), nil)
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("%s: %v %v", path, response, err)
	}
	front.waitServed(t)
}

// A connection serves keepalive_requests requests, then closes.
func TestUpstreamPoolKeepaliveRequests(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	service := testForwardRouteService("pool-requests", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	})
	service.Keepalive.Requests = 2
	group, _ := newTestGroup(t, service, testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
	for range 3 {
		sendAndWait(t, front, "/")
	}
	conns := requestConns(upstream)
	if conns[0] != conns[1] || conns[1] == conns[2] {
		t.Errorf("connections %v, want two requests on the first and the third on a new one", conns)
	}
}

// No keepalive connections means a new connection for every request, as
// nginx's upstream without `keepalive`.
func TestUpstreamPoolWithoutKeepalive(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	service := testForwardRouteService("pool-none", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	})
	service.Keepalive.Connections = 0
	group, _ := newTestGroup(t, service, testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
	for range 3 {
		sendAndWait(t, front, "/")
	}
	conns := requestConns(upstream)
	if conns[0] == conns[1] || conns[1] == conns[2] || conns[0] == conns[2] {
		t.Errorf("connections %v, want a new one per request", conns)
	}
	if group.blocks[0].pool.idleCount() != 0 {
		t.Errorf("a pool with no keepalive kept a connection")
	}
}

// A full pool closes its least recently used connection to keep a new one,
// and the next request takes the one kept.
func TestUpstreamPoolEvictsLeastRecentlyUsed(t *testing.T) {
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	// held handlers end on any exit, so the servers' cleanup never waits
	releaseFirstOnce := sync.OnceFunc(func() { close(releaseFirst) })
	releaseSecondOnce := sync.OnceFunc(func() { close(releaseSecond) })
	defer releaseFirstOnce()
	defer releaseSecondOnce()
	arrivals := make(chan *recordedRequest)
	upstream := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		switch r.URL.Path {
		case "/held-1":
			<-releaseFirst
		case "/held-2":
			<-releaseSecond
		}
		io.WriteString(w, "g1")
	})
	upstream.arrivals = arrivals
	service := testForwardRouteService("pool-evict", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	})
	service.Keepalive.Connections = 1
	group, _ := newTestGroup(t, service, testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
	pool := group.blocks[0].pool
	send := func(path string) *recordedRequest {
		go func() {
			request, _ := http.NewRequest(http.MethodGet, front.url(path), nil)
			testDo(t, request)
		}()
		return receiveBounded(t, arrivals, "arrival")
	}
	pooledConn := func() string {
		return idleConnsOf(t, pool, 1)[0].conn.LocalAddr().String()
	}

	// two connections, each holding one request
	first := send("/held-1")
	second := send("/held-2")
	if first.remoteAddr == second.remoteAddr {
		t.Fatalf("held requests shared a connection")
	}
	releaseFirstOnce()
	front.waitServed(t)
	if pooledConn() != first.remoteAddr {
		t.Fatalf("the first connection was not pooled")
	}
	releaseSecondOnce()
	front.waitServed(t)
	if pooledConn() != second.remoteAddr {
		t.Fatalf("the full pool kept the least recently used connection")
	}
	if next := send("/next"); next.remoteAddr != second.remoteAddr {
		t.Errorf("the next request came on %s, not the pooled %s", next.remoteAddr, second.remoteAddr)
	}
	front.waitServed(t)
}

// An idle connection past keepalive_timeout is closed by the sweep, and a
// connection past keepalive_time serves the request that takes it, then
// closes, as nginx checks keepalive_time after each request.
func TestUpstreamPoolIdleTimeoutAndKeepaliveTime(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	service := testForwardRouteService("pool-time", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	})
	group, _ := newTestGroup(t, service, testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
	pool := group.blocks[0].pool

	sendAndWait(t, front, "/first")
	pool.sweep(time.Now())
	if pool.idleCount() != 1 {
		t.Fatalf("the sweep closed a connection within its timeout")
	}
	pool.sweep(time.Now().Add(time.Duration(service.Keepalive.TimeoutSeconds) * time.Second))
	if pool.idleCount() != 0 {
		t.Fatalf("the sweep kept a connection past its timeout")
	}

	sendAndWait(t, front, "/second")
	// the pooled connection is past its keepalive time
	oldConn := idleConnsOf(t, pool, 1)[0]
	func() {
		pool.stateLock.Lock()
		defer pool.stateLock.Unlock()
		oldConn.dialTime = time.Now().Add(-time.Duration(service.Keepalive.TimeSeconds+1) * time.Second)
	}()
	sendAndWait(t, front, "/third")
	sendAndWait(t, front, "/fourth")
	conns := requestConns(upstream)
	if conns[1] != conns[2] || conns[2] == conns[3] {
		t.Errorf("connections %v: want the old connection to serve one more request, then close", conns)
	}
}

// Waits until a connection has bytes or its end to read, without reading
// them: the barrier for bytes or a close the peer sent.
func waitReadable(t *testing.T, conn net.Conn) {
	t.Helper()
	rawConn, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatalf("raw conn: %v", err)
	}
	var peekBuffer [1]byte
	err = rawConn.Read(func(fd uintptr) bool {
		_, _, peekErr := syscall.Recvfrom(int(fd), peekBuffer[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		// nothing yet: false waits for the descriptor to be readable
		return peekErr != syscall.EAGAIN && peekErr != syscall.EWOULDBLOCK
	})
	if err != nil {
		t.Fatalf("wait readable: %v", err)
	}
}

// A pooled connection the block closed is dropped by the MSG_PEEK check
// before any request is written to it: the next request dials, and no
// attempt fails or is redone.
func TestUpstreamPoolDropsClosedIdleConnection(t *testing.T) {
	serviceName := testServiceName("pool-peek")
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
	pool := group.blocks[0].pool
	sendAndWait(t, front, "/first")
	pooledConn := idleConnsOf(t, pool, 1)[0].conn
	if !upstreamConnAlive(pooledConn) {
		t.Fatalf("a live idle connection was taken for closed")
	}
	upstream.server.CloseClientConnections()
	waitReadable(t, pooledConn)
	sendAndWait(t, front, "/second")
	if attemptCount(serviceName, "g1", "response") != 2 || attemptCount(serviceName, "g1", "stale_redo") != 0 || attemptCount(serviceName, "g1", "error") != 0 {
		t.Errorf("attempts: response %v stale_redo %v error %v; want two clean responses",
			attemptCount(serviceName, "g1", "response"), attemptCount(serviceName, "g1", "stale_redo"), attemptCount(serviceName, "g1", "error"))
	}
	if conns := requestConns(upstream); conns[0] == conns[1] {
		t.Errorf("the closed connection was reused")
	}
}

// The MSG_PEEK check: a quiet connection is alive; one with bytes nobody
// asked for is not; a connection without a descriptor is assumed alive.
func TestUpstreamConnAlive(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer peer.Close()
	if !upstreamConnAlive(conn) {
		t.Errorf("a quiet connection was taken for closed")
	}
	io.WriteString(peer, "x")
	waitReadable(t, conn)
	if upstreamConnAlive(conn) {
		t.Errorf("a connection holding unasked bytes was taken for alive")
	}
	pipeConn, pipePeer := net.Pipe()
	defer pipeConn.Close()
	defer pipePeer.Close()
	if !upstreamConnAlive(pipeConn) {
		t.Errorf("a connection without a descriptor was taken for closed")
	}
}

// For a stateful service, a refused first block moves the request to the
// block nginx's walk reaches next, by the independent reference.
func TestUpstreamStatefulRetryWalksForward(t *testing.T) {
	table := referenceCrc32Table(t)
	listeners := []net.Listener{}
	peers := []*referencePeer{}
	testBlocks := []testBlock{}
	for i, weight := range []int{1, 24, 25} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		name := []string{"beta", "g1", "g2"}[i]
		listeners = append(listeners, listener)
		peers = append(peers, &referencePeer{name: name, server: "warpservices:" + strconv.Itoa(port), weight: weight})
		testBlocks = append(testBlocks, testBlock{name: name, weight: weight, port: port})
	}
	clientKey := "198.51.100.77"
	points := referenceRing(&table, peers)
	walk := &referenceWalk{hash: referenceFindPoint(points, referenceCrc32Update(&table, 0xffffffff, []byte(clientKey))^0xffffffff)}
	tried := make([]bool, len(peers))
	firstPeer, _ := referenceChashPeer(points, peers, walk, tried)
	tried[firstPeer] = true
	secondPeer, _ := referenceChashPeer(points, peers, walk, tried)

	// the first choice refuses; the others serve
	listeners[firstPeer].Close()
	upstreams := map[string]*recordingUpstream{}
	for i, listener := range listeners {
		if i != firstPeer {
			upstreams[peers[i].name] = newRecordingUpstreamOn(t, peers[i].name, listener, nil)
		}
	}
	service := testForwardRouteService(testServiceName("stateful-walk"), testBlocks)
	service.Stateful = true
	group, _ := newTestGroup(t, service, testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.clientAddrPort = netip.MustParseAddrPort(clientKey + ":5000")
		return options
	})
	request, _ := http.NewRequest(http.MethodGet, front.url("/mcp"), nil)
	response, body, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != peers[secondPeer].name {
		t.Fatalf("response %v %q %v, want the walk's next block %s", response, body, err, peers[secondPeer].name)
	}
	// the same client lands there again, a fresh walk passing the refused
	// block the same way
	request, _ = http.NewRequest(http.MethodGet, front.url("/mcp"), nil)
	if _, body, err := testDo(t, request); err != nil || string(body) != peers[secondPeer].name {
		t.Errorf("second request reached %q %v", body, err)
	}
}

// A dialed connection whose read deadline a test can make pass at once, as
// if its time had come: it keeps the read deadline last set, and
// passReadDeadline moves a set one into the past. Its raw descriptor is the
// real socket's, so the pool's MSG_PEEK check sees what a real connection
// would.
type deadlineConn struct {
	net.Conn

	stateLock    sync.Mutex
	readDeadline time.Time
}

// Keeps the read deadline, then sets both.
func (self *deadlineConn) SetDeadline(deadline time.Time) error {
	self.keepReadDeadline(deadline)
	return self.Conn.SetDeadline(deadline)
}

// Keeps the read deadline, then sets it.
func (self *deadlineConn) SetReadDeadline(deadline time.Time) error {
	self.keepReadDeadline(deadline)
	return self.Conn.SetReadDeadline(deadline)
}

// Records the read deadline last set.
func (self *deadlineConn) keepReadDeadline(deadline time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readDeadline = deadline
}

// Makes the read deadline last set pass now. With none set, nothing changes.
func (self *deadlineConn) passReadDeadline() {
	readDeadline := func() time.Time {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.readDeadline
	}()
	if !readDeadline.IsZero() {
		self.Conn.SetReadDeadline(time.Unix(1, 0))
	}
}

// The real socket's raw descriptor.
func (self *deadlineConn) SyscallConn() (syscall.RawConn, error) {
	return self.Conn.(syscall.Conn).SyscallConn()
}

// A pooled connection carries no deadline from its last exchange: idle past
// that exchange's read timeout, within keepalive_timeout, it still serves
// the next request, never dropped as dead by the MSG_PEEK check that takes
// it from the pool.
func TestUpstreamPoolKeepsConnectionPastItsReadDeadline(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("pool-read-deadline"), []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	dialedConns := make(chan *deadlineConn, 4)
	wrapGroupDials(group, func(conn net.Conn, address string) net.Conn {
		dialedConn := &deadlineConn{Conn: conn}
		dialedConns <- dialedConn
		return dialedConn
	})
	front := newTestForwardFront(t, group, testForwardOptions)

	sendAndWait(t, front, "/first")
	// the time of any read deadline the first exchange left comes
	receiveBounded(t, dialedConns, "dial").passReadDeadline()
	sendAndWait(t, front, "/second")
	if conns := requestConns(upstream); conns[0] != conns[1] {
		t.Errorf("connections %v: the idle connection was dropped once its last exchange's read deadline passed", conns)
	}
	if len(dialedConns) != 0 {
		t.Errorf("the second request dialed a new connection")
	}
}

// The group's own sweep, every ForwardSettings.KeepaliveSweepInterval,
// closes an idle connection past keepalive_timeout: the block sees it
// close, and the pool is left empty.
func TestUpstreamPoolSweepLoopClosesExpiredConnection(t *testing.T) {
	settings := testForwardFrontSettings()
	settings.Forward.KeepaliveSweepInterval = 10 * time.Millisecond
	raw := newRawUpstream(t)
	service := testForwardRouteService(testServiceName("pool-sweep-loop"), []testBlock{
		{name: "g1", weight: 1, port: raw.port},
	})
	group, _ := newTestGroup(t, service, settings)
	front := newTestForwardFront(t, group, testForwardOptions)
	go func() {
		request, _ := http.NewRequest(http.MethodGet, front.url("/"), nil)
		testDo(t, request)
	}()
	upstreamConn, upstreamReader, _ := raw.accept(t)
	io.WriteString(upstreamConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	front.waitServed(t)

	pool := group.blocks[0].pool
	idleConn := idleConnsOf(t, pool, 1)[0]
	// its keepalive timeout has passed
	func() {
		pool.stateLock.Lock()
		defer pool.stateLock.Unlock()
		idleConn.idleTime = time.Now().Add(-time.Duration(service.Keepalive.TimeoutSeconds+1) * time.Second)
	}()
	upstreamConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if n, err := upstreamReader.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the block read %d %v, want the sweep to close the expired connection", n, err)
	}
	if pool.idleCount() != 0 {
		t.Errorf("the pool kept the expired connection")
	}
}

// A response read whole before its request write reported doesn't cost the
// connection: the block has the whole request, so the exchange waits for the
// write to report, as Go's transport waits (wroteRequest), and pools the
// connection, rather than cut it.
func TestUpstreamFinishWaitsForLateWriteReport(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("finish-write-wait"), []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	dialedConns := make(chan *holdWriteConn, 1)
	wrapGroupDials(group, func(conn net.Conn, address string) net.Conn {
		holdConn := newHoldWriteConn(t, conn)
		dialedConns <- holdConn
		return holdConn
	})
	waitStarted := make(chan struct{})
	group.waitAfter = func(d time.Duration) <-chan time.Time {
		close(waitStarted)
		// never fires: only the write's report ends this wait
		return nil
	}
	front := newTestForwardFront(t, group, testForwardOptions)
	responses := make(chan string, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodGet, front.url("/late-report"), nil)
		response, body, err := testDo(t, request)
		if err != nil {
			responses <- err.Error()
			return
		}
		responses <- strconv.Itoa(response.StatusCode) + " " + string(body)
	}()

	holdConn := receiveBounded(t, dialedConns, "dial")
	// the request head is at the block; its write hasn't returned
	waitClosed(t, holdConn.held, "the held write")
	select {
	case <-waitStarted:
	case <-holdConn.closed:
		t.Fatalf("the exchange cut a connection whose request the block had whole")
	case <-time.After(30 * time.Second):
		t.Fatalf("the exchange neither waited for the write nor cut the connection")
	}
	holdConn.release()
	if response := receiveBounded(t, responses, "response"); response != "200 g1" {
		t.Fatalf("response %q, want 200 g1", response)
	}
	front.waitServed(t)
	if idleConnsOf(t, group.blocks[0].pool, 1)[0].conn != holdConn {
		t.Errorf("the pool kept another connection")
	}
}

// A request write that doesn't report within ForwardSettings.KeepaliveWriteWait
// costs only its connection: the response is relayed whole, the connection is
// closed rather than pooled, and nothing waits on the write beyond the bound.
func TestUpstreamFinishGivesUpOnWriteReportPastItsWait(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	settings := testForwardFrontSettings()
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("finish-write-give-up"), []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), settings)
	dialedConns := make(chan *holdWriteConn, 1)
	wrapGroupDials(group, func(conn net.Conn, address string) net.Conn {
		holdConn := newHoldWriteConn(t, conn)
		dialedConns <- holdConn
		return holdConn
	})
	waitDurations := make(chan time.Duration, 1)
	group.waitAfter = func(d time.Duration) <-chan time.Time {
		waitDurations <- d
		// the wait's bound passes at once
		passed := make(chan time.Time, 1)
		passed <- time.Now()
		return passed
	}
	front := newTestForwardFront(t, group, testForwardOptions)
	request, _ := http.NewRequest(http.MethodGet, front.url("/no-report"), nil)
	response, body, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "g1" {
		t.Fatalf("response %v %q %v, want 200 g1", response, body, err)
	}
	front.waitServed(t)
	if waitDuration := receiveBounded(t, waitDurations, "write wait"); waitDuration != settings.Forward.KeepaliveWriteWait {
		t.Errorf("waited %v for the write, want KeepaliveWriteWait %v", waitDuration, settings.Forward.KeepaliveWriteWait)
	}
	holdConn := receiveBounded(t, dialedConns, "dial")
	waitClosed(t, holdConn.closed, "closing the connection")
	if group.blocks[0].pool.idleCount() != 0 {
		t.Errorf("a connection whose write never reported was pooled")
	}
}

// A response writer whose client goes away the moment the response's last
// expected byte is written to it: the request is canceled, as Go's server
// cancels it when a client that read a response of known length closes its
// connection.
type clientGoneWriter struct {
	http.ResponseWriter
	remainingBytes int
	gone           context.CancelFunc
}

// Writes p, then cancels the request once the expected bytes are written.
func (self *clientGoneWriter) Write(p []byte) (int, error) {
	n, err := self.ResponseWriter.Write(p)
	self.remainingBytes -= n
	if self.remainingBytes <= 0 {
		self.gone()
	}
	return n, err
}

// The writer underneath, for http.ResponseController.
func (self *clientGoneWriter) Unwrap() http.ResponseWriter {
	return self.ResponseWriter
}

// A client that goes away once it has the block's whole response, before
// the handler returns, leaves the upstream connection pooled: the exchange
// stops watching for the client once the response was read whole.
func TestUpstreamPoolKeepsConnectionWhenClientLeavesAfterResponse(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("pool-client-gone"), []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	served := make(chan any, 1)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			served <- recover()
		}()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		goneWriter := &clientGoneWriter{
			ResponseWriter: w,
			remainingBytes: len("g1"),
			gone:           cancel,
		}
		group.serveHttp(goneWriter, r.WithContext(ctx), testForwardOptions(r))
	}))
	defer front.Close()

	request, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	response, body, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "g1" {
		t.Fatalf("response %v %q %v, want 200 g1", response, body, err)
	}
	if recovered := receiveBounded(t, served, "the end of serveHttp"); recovered != nil {
		t.Fatalf("serveHttp panicked: %v", recovered)
	}
	if group.blocks[0].pool.idleCount() != 1 {
		t.Errorf("the connection of a whole response was not pooled after its client left")
	}
}
