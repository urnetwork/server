// The connect service's timer census sites (timer_census.go and the server's
// timer_census.go, CPUOPT7.md §11): each counts once per call that may move a
// runtime timer, in the bucket of the horizon it gives the timer, and each
// stop or clear as a stop. Each test runs a census of its own, every call
// counted, so its counts start at zero.
package connect

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	quic "github.com/urnetwork/quic-go/v2026"

	"github.com/urnetwork/server/v2026"
)

var timerCensusHorizons = []string{"lt_1ms", "1ms_10ms", "10ms_100ms", "100ms_1s", "1s_10s", "ge_10s", "stop"}

// Runs a census of the test's own, counting every call at the server's sites,
// until the test ends.
func startTestTimerCensus(t testing.TB) {
	t.Helper()
	t.Cleanup(server.Testing_StartTimerCensus())
}

// Every bucket's count at site in the running census.
func timerCensusCounts(site server.TimerCensusSite) map[string]float64 {
	counts := map[string]float64{}
	for _, horizon := range timerCensusHorizons {
		counts[horizon] = server.Testing_TimerCensusCount(site, horizon)
	}
	return counts
}

// Requires that since before, site counted the wanted count in each bucket,
// and nothing in a bucket wants leaves out.
func requireTimerCensusCounts(t testing.TB, what string, site server.TimerCensusSite, before map[string]float64, wants map[string]float64) {
	t.Helper()
	for horizon, count := range timerCensusCounts(site) {
		if delta := count - before[horizon]; delta != wants[horizon] {
			t.Errorf("%s: %s counted %v in %s, want %v", what, site, delta, horizon, wants[horizon])
		}
	}
}

// Every one of the connect library's and quic-go's sites maps to a server site
// of its own, under its library's prefix.
func TestTimerCensusMapsEveryLibrarySiteToItsOwn(t *testing.T) {
	seen := map[server.TimerCensusSite]bool{}
	for site, serverSite := range clientTimerCensusSites {
		if name := serverSite.String(); len(name) < 7 || name[:7] != "client_" || seen[serverSite] {
			t.Errorf("connect library site %d maps to %s", site, name)
		}
		seen[serverSite] = true
	}
	for site, serverSite := range quicTimerCensusSites {
		name := serverSite.String()
		if !(len(name) >= 5 && name[:5] == "quic_" || len(name) >= 6 && name[:6] == "http3_") || seen[serverSite] {
			t.Errorf("quic-go site %d maps to %s", site, name)
		}
		seen[serverSite] = true
	}
	if len(seen) != int(clientconnect.TimerCensusSiteCount)+int(quic.TimerCensusSiteCount) {
		t.Fatalf("%d library sites map to %d server sites", int(clientconnect.TimerCensusSiteCount)+int(quic.TimerCensusSiteCount), len(seen))
	}
}

// The service's census installs quic-go's hook with its own sampling, and its
// stop removes it.
func TestStartTimerCensusRecordsQuicSites(t *testing.T) {
	stop, err := startTimerCensus(&server.TimerCensusSettings{SamplePeriod: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	before := timerCensusCounts(server.TimerCensusSiteHttp3IdleTimer)
	quic.TimerCensusReset(quic.TimerCensusSiteHTTP3IdleTimer, 47*time.Second)
	quic.TimerCensusStop(quic.TimerCensusSiteHTTP3IdleTimer)
	requireTimerCensusCounts(t, "an idle timer's reset and stop", server.TimerCensusSiteHttp3IdleTimer, before, map[string]float64{"ge_10s": 1, "stop": 1})

	stop()
	quic.TimerCensusReset(quic.TimerCensusSiteHTTP3IdleTimer, 47*time.Second)
	if count := server.Testing_TimerCensusCount(server.TimerCensusSiteHttp3IdleTimer, "ge_10s"); count != 0 {
		t.Fatalf("a stopped census counted %v", count)
	}
}

// The service's census installs the connect library's hook: a route write
// that waits out its timeout twice resets its timer at the second wait, which
// the service counts at client_route_write_timer; and each library site
// records under its own server site.
func TestStartTimerCensusRecordsConnectLibrarySites(t *testing.T) {
	stop, err := startTimerCensus(&server.TimerCensusSettings{SamplePeriod: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selector := clientconnect.NewMultiRouteSelector(ctx, "synthetic-census", clientconnect.NewNoopLogger(), clientconnect.DestinationId(clientconnect.NewId()), false)
	defer selector.Close()
	message := clientconnect.MessagePoolGet(31)
	defer clientconnect.MessagePoolReturn(message)
	before := timerCensusCounts(server.TimerCensusSiteClientRouteWriteTimer)
	// the first wait creates the timer, and the second resets it
	for _, timeout := range []time.Duration{3 * time.Millisecond, 7 * time.Millisecond} {
		if err := selector.Write(ctx, message, timeout); err == nil {
			t.Fatal("a write with no route went out")
		}
	}
	requireTimerCensusCounts(t, "two waits for a route", server.TimerCensusSiteClientRouteWriteTimer, before, map[string]float64{"1ms_10ms": 1})

	for site := range clientconnect.TimerCensusSiteCount {
		serverSite := clientTimerCensusSites[site]
		before := timerCensusCounts(serverSite)
		server.RecordTimerChange(serverSite, 5*time.Millisecond, false, 64)
		requireTimerCensusCounts(t, "a sampled library change", serverSite, before, map[string]float64{"1ms_10ms": 64})
	}
}

// The census is off with a non-positive period, and a period that is not a
// power of two fails the start, which Run logs and serves on.
func TestStartTimerCensusOffAndRefused(t *testing.T) {
	stop, err := startTimerCensus(&server.TimerCensusSettings{SamplePeriod: 0})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	quic.TimerCensusReset(quic.TimerCensusSiteHTTP3IdleTimer, time.Second)
	if count := server.Testing_TimerCensusCount(server.TimerCensusSiteHttp3IdleTimer, "1s_10s"); count != 0 {
		t.Fatalf("an off census counted %v", count)
	}
	if _, err := startTimerCensus(&server.TimerCensusSettings{SamplePeriod: 100}); err == nil {
		t.Fatal("a sample period of 100 was taken")
	}
	if period := DefaultExchangeSettings().TimerCensus.SamplePeriod; period != 64 {
		t.Fatalf("the default sample period is %d, want 64", period)
	}
}

// A connected loopback tcp pair, closed with the test.
func newTimerCensusTcpPair(t testing.TB) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()
	dialed, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn := <-accepted
	if conn == nil {
		t.Fatal("the loopback accept failed")
	}
	t.Cleanup(func() {
		dialed.Close()
		conn.Close()
	})
	return dialed, conn
}

// Each exchange write and read refreshes its socket deadline once, a full
// WriteTimeout or ExchangeReadTimeout out.
func TestTimerCensusCountsTheExchangeDeadlines(t *testing.T) {
	startTestTimerCensus(t)
	settings := DefaultExchangeSettings()
	settings.WriteTimeout = 5 * time.Second
	settings.ExchangeReadTimeout = 50 * time.Second
	sendConn, receiveConn := newTimerCensusTcpPair(t)
	sendBuffer := NewDefaultExchangeBuffer(settings)
	receiveBuffer := NewDefaultExchangeBuffer(settings)

	before := timerCensusCounts(server.TimerCensusSiteExchangeWriteDeadline)
	if err := sendBuffer.WriteMessage(sendConn, clientconnect.MessagePoolGet(16)); err != nil {
		t.Fatal(err)
	}
	if err := sendBuffer.WriteMessages(sendConn, [][]byte{clientconnect.MessagePoolGet(16), clientconnect.MessagePoolGet(16)}); err != nil {
		t.Fatal(err)
	}
	requireTimerCensusCounts(t, "a write and a batch", server.TimerCensusSiteExchangeWriteDeadline, before, map[string]float64{"1s_10s": 2})

	before = timerCensusCounts(server.TimerCensusSiteExchangeReadDeadline)
	for range 3 {
		message, err := receiveBuffer.ReadMessage(receiveConn)
		if err != nil {
			t.Fatal(err)
		}
		clientconnect.MessagePoolReturn(message)
	}
	requireTimerCensusCounts(t, "three reads", server.TimerCensusSiteExchangeReadDeadline, before, map[string]float64{"ge_10s": 3})
}

// A send into a full queue resets the write timer once, for its timeout; a
// send the queue takes at once touches no timer.
func TestTimerCensusCountsTheForwardWriteTimer(t *testing.T) {
	startTestTimerCensus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	before := timerCensusCounts(server.TimerCensusSiteForwardWriteTimer)
	result := sendPooledMessage(ctx.Done(), nil, newMessageQueue(0), clientconnect.MessagePoolGet(16), timer, 3*time.Millisecond)
	if result != pooledMessageSendDropped {
		t.Fatalf("a send nothing receives = %d, want dropped", result)
	}
	requireTimerCensusCounts(t, "a send into a full queue", server.TimerCensusSiteForwardWriteTimer, before, map[string]float64{"1ms_10ms": 1})

	before = timerCensusCounts(server.TimerCensusSiteForwardWriteTimer)
	destination := newMessageQueue(1)
	defer returnQueuedPooledMessages(destination)
	if result := sendPooledMessage(ctx.Done(), nil, destination, clientconnect.MessagePoolGet(16), timer, time.Second); result != pooledMessageSendDelivered {
		t.Fatalf("a send into a free queue = %d, want delivered", result)
	}
	requireTimerCensusCounts(t, "a send into a free queue", server.TimerCensusSiteForwardWriteTimer, before, map[string]float64{})
}

// A public connection counts every deadline its owners set, and the bound of
// a write its owner left without one: set before the write, cleared after.
func TestTimerCensusCountsTheFrontDeadlines(t *testing.T) {
	startTestTimerCensus(t)
	serverConn, clientConn := newTimerCensusTcpPair(t)
	go io.Copy(io.Discard, clientConn)
	trackedConn := &frontTrackedConn{
		Conn:        serverConn,
		sendTimeout: 60 * time.Second,
	}

	before := timerCensusCounts(server.TimerCensusSiteFrontWriteBound)
	if _, err := trackedConn.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	requireTimerCensusCounts(t, "a write without a deadline", server.TimerCensusSiteFrontWriteBound, before, map[string]float64{"ge_10s": 1, "stop": 1})

	beforeBound := timerCensusCounts(server.TimerCensusSiteFrontWriteBound)
	before = timerCensusCounts(server.TimerCensusSiteFrontWriteDeadline)
	trackedConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := trackedConn.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	trackedConn.SetWriteDeadline(time.Time{})
	requireTimerCensusCounts(t, "an owner's deadline and clear", server.TimerCensusSiteFrontWriteDeadline, before, map[string]float64{"1s_10s": 1, "stop": 1})
	requireTimerCensusCounts(t, "a write under an owner's deadline", server.TimerCensusSiteFrontWriteBound, beforeBound, map[string]float64{})

	before = timerCensusCounts(server.TimerCensusSiteFrontReadDeadline)
	trackedConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	trackedConn.SetReadDeadline(time.Time{})
	requireTimerCensusCounts(t, "a read deadline and clear", server.TimerCensusSiteFrontReadDeadline, before, map[string]float64{"ge_10s": 1, "stop": 1})

	beforeRead := timerCensusCounts(server.TimerCensusSiteFrontReadDeadline)
	beforeWrite := timerCensusCounts(server.TimerCensusSiteFrontWriteDeadline)
	trackedConn.SetDeadline(time.Now().Add(50 * time.Millisecond))
	requireTimerCensusCounts(t, "both deadlines", server.TimerCensusSiteFrontReadDeadline, beforeRead, map[string]float64{"10ms_100ms": 1})
	requireTimerCensusCounts(t, "both deadlines", server.TimerCensusSiteFrontWriteDeadline, beforeWrite, map[string]float64{"10ms_100ms": 1})
}

// An owner's clear while a bounded write waits leaves the socket the bound,
// which is what the census counts for it; the write's end clears the bound.
func TestTimerCensusCountsTheFrontBoundAnOwnersClearLeaves(t *testing.T) {
	startTestTimerCensus(t)
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	rawConn := newTestFrontDeadlineConn(serverConn)
	trackedConn := &frontTrackedConn{
		Conn:        rawConn,
		sendTimeout: 60 * time.Second,
	}
	beforeBound := timerCensusCounts(server.TimerCensusSiteFrontWriteBound)
	beforeOwner := timerCensusCounts(server.TimerCensusSiteFrontWriteDeadline)
	written := make(chan error, 1)
	go func() {
		_, err := trackedConn.Write([]byte("a"))
		written <- err
	}()
	// the write waits for the peer to read, under its bound
	rawConn.nextWriteStart(t)
	trackedConn.SetWriteDeadline(time.Time{})
	var b [1]byte
	if _, err := io.ReadFull(clientConn, b[:]); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	requireTimerCensusCounts(t, "an owner's clear under a bound", server.TimerCensusSiteFrontWriteDeadline, beforeOwner, map[string]float64{"ge_10s": 1})
	requireTimerCensusCounts(t, "the bounded write", server.TimerCensusSiteFrontWriteBound, beforeBound, map[string]float64{"ge_10s": 1, "stop": 1})
}

// Each H1 read and write sets its deadline once, its timeout out.
func TestTimerCensusCountsTheH1Deadlines(t *testing.T) {
	startTestTimerCensus(t)
	ws := &transportDeadlineH1Conn{}

	before := timerCensusCounts(server.TimerCensusSiteH1ReadDeadline)
	if _, message, err := readConnectH1PooledWithDeadline(ws, 30*time.Second, 1024); err != nil {
		t.Fatal(err)
	} else {
		clientconnect.MessagePoolReturn(message)
	}
	if _, _, err := readConnectH1AuthWithDeadline(ws, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	requireTimerCensusCounts(t, "a read and an auth read", server.TimerCensusSiteH1ReadDeadline, before, map[string]float64{"ge_10s": 1, "1s_10s": 1})

	before = timerCensusCounts(server.TimerCensusSiteH1WriteDeadline)
	if err := echoConnectH1AuthWithDeadline(ws, 2*time.Second, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeConnectH1UserReadyBatch(context.Background(), ws, nil, newMessageQueue(0), clientconnect.MessagePoolGet(64), true, 15*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	requireTimerCensusCounts(t, "an auth echo and a batch", server.TimerCensusSiteH1WriteDeadline, before, map[string]float64{"1s_10s": 1, "ge_10s": 1})
}

// A writer's heartbeat reset counts once at its transport's site, in its
// interval's bucket, and arms the timer for the interval.
func TestTimerCensusCountsThePingTimerResets(t *testing.T) {
	startTestTimerCensus(t)
	pingTimer := time.NewTimer(time.Hour)
	defer pingTimer.Stop()

	beforeH1 := timerCensusCounts(server.TimerCensusSiteH1PingTimer)
	beforeH3 := timerCensusCounts(server.TimerCensusSiteH3PingTimer)
	resetConnectPingTimer(pingTimer, 3*time.Second, server.TimerCensusSiteH1PingTimer)
	resetConnectPingTimer(pingTimer, time.Hour, server.TimerCensusSiteH3PingTimer)
	requireTimerCensusCounts(t, "an h1 reset", server.TimerCensusSiteH1PingTimer, beforeH1, map[string]float64{"1s_10s": 1})
	requireTimerCensusCounts(t, "an h3 reset", server.TimerCensusSiteH3PingTimer, beforeH3, map[string]float64{"ge_10s": 1})
	if !pingTimer.Stop() {
		t.Fatal("the reset left the timer unarmed")
	}
}
