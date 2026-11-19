package nginx

// Tests of the keepalive pools: nginx's keepalive, keepalive_requests,
// keepalive_time and keepalive_timeout per block, the least recently used
// eviction, and the MSG_PEEK check that drops a connection the block
// closed before any request is written to it.

import (
	"io"
	"net"
	"net/http"
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
func sendAndWait(t *testing.T, front *testFront, path string) {
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
	service := testRouteService("pool-requests", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	})
	service.Keepalive.Requests = 2
	group, _ := newTestGroup(t, service, testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
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
	service := testRouteService("pool-none", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	})
	service.Keepalive.Connections = 0
	group, _ := newTestGroup(t, service, testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
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
	service := testRouteService("pool-evict", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	})
	service.Keepalive.Connections = 1
	group, _ := newTestGroup(t, service, testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
	pool := group.blocks[0].pool
	send := func(path string) *recordedRequest {
		go func() {
			request, _ := http.NewRequest(http.MethodGet, front.url(path), nil)
			testDo(t, request)
		}()
		return <-arrivals
	}
	pooledConn := func() string {
		pool.stateLock.Lock()
		defer pool.stateLock.Unlock()
		if len(pool.idleConns) != 1 {
			t.Fatalf("pool keeps %d connections, want 1", len(pool.idleConns))
		}
		return pool.idleConns[0].conn.LocalAddr().String()
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
	service := testRouteService("pool-time", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	})
	group, _ := newTestGroup(t, service, testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
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
	func() {
		pool.stateLock.Lock()
		defer pool.stateLock.Unlock()
		pool.idleConns[0].dialTime = time.Now().Add(-time.Duration(service.Keepalive.TimeSeconds+1) * time.Second)
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
	group, _ := newTestGroup(t, testRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
	pool := group.blocks[0].pool
	sendAndWait(t, front, "/first")
	pooledConn := func() net.Conn {
		pool.stateLock.Lock()
		defer pool.stateLock.Unlock()
		return pool.idleConns[0].conn
	}()
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
	service := testRouteService(testServiceName("stateful-walk"), testBlocks)
	service.Stateful = true
	group, _ := newTestGroup(t, service, testFrontSettings())
	front := newTestFront(t, group, func(r *http.Request) forwardOptions {
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
