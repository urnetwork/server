package nginx

// Tests of the websocket relay: both directions, the idle bound, close
// cutting tunnels, and the caller's hold on the request for the tunnel's
// life.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A websocket test setup: a raw block that switches protocols, and a front
// for a websocket service whose tunnelOpened hook is counted.
type tunnelFixture struct {
	raw           *rawUpstream
	group         *upstreamGroup
	front         *testFront
	tunnelsOpened atomic.Int32
	serviceName   string
}

// A tunnel fixture for one run of a test, its service named after serviceName.
func newTunnelFixture(t *testing.T, serviceName string, settings *FrontSettings) *tunnelFixture {
	fixture := &tunnelFixture{
		raw:         newRawUpstream(t),
		serviceName: testServiceName(serviceName),
	}
	fixture.group, _ = newTestGroup(t, testRouteService(fixture.serviceName, []testBlock{
		{name: "g1", weight: 1, port: fixture.raw.port},
	}), settings)
	fixture.front = newTestFront(t, fixture.group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.websocket = true
		options.tunnelOpened = func() {
			fixture.tunnelsOpened.Add(1)
		}
		return options
	})
	return fixture
}

// Opens a tunnel: the client sends an upgrade request and early bytes, the
// block switches and sends its own first bytes. Returns the client's
// connection and reader past the 101, and the block's.
func (self *tunnelFixture) open(t *testing.T) (net.Conn, *bufio.Reader, net.Conn, *bufio.Reader) {
	clientConn, err := net.Dial("tcp", self.front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	t.Cleanup(func() { clientConn.Close() })
	io.WriteString(clientConn, "GET /socket HTTP/1.1\r\nHost: ws.service.example\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nclient-early")

	upstreamConn, upstreamReader, request := self.raw.accept(t)
	if request.Header.Get(upgradeHeader) != "websocket" || request.Header.Get(connectionHeader) != "upgrade" {
		t.Fatalf("upgrade request headers %v", request.Header)
	}
	io.WriteString(upstreamConn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Upstream: switched\r\nServer: hidden\r\n\r\nserver-first")

	clientReader := bufio.NewReader(clientConn)
	response, err := http.ReadResponse(clientReader, nil)
	if err != nil {
		t.Fatalf("read 101: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols ||
		response.Header.Get(upgradeHeader) != "websocket" ||
		response.Header.Get(connectionHeader) != "upgrade" ||
		response.Header.Get("X-Upstream") != "switched" ||
		response.Header.Get("Server") != "" {
		t.Fatalf("101 relayed as %d %v", response.StatusCode, response.Header)
	}
	readExactly(t, clientReader, "server-first")
	readExactly(t, upstreamReader, "client-early")
	return clientConn, clientReader, upstreamConn, upstreamReader
}

// Reads exactly want from reader.
func readExactly(t *testing.T, reader io.Reader, want string) {
	got := make([]byte, len(want))
	if _, err := io.ReadFull(reader, got); err != nil || string(got) != want {
		t.Fatalf("read %q %v, want %q", got, err, want)
	}
}

// Waits for a reader to reach its end, as a closed tunnel's side does.
func readEnd(t *testing.T, reader io.Reader) {
	if n, err := io.Copy(io.Discard, reader); err != nil && !isClosedError(err) {
		t.Fatalf("read to end: %d %v", n, err)
	}
}

// Whether a read error is the end of a connection the other side closed.
func isClosedError(err error) bool {
	return err == io.EOF || strings.Contains(err.Error(), "reset") || strings.Contains(err.Error(), "closed")
}

// Bytes flow both ways, those sent ahead of the switch included; the 101
// reaches the client with the block's headers except the hidden ones; the
// hook runs once; serveHttp holds the request, and its block, until the
// tunnel closes, then returns.
func TestTunnelRelaysBothWays(t *testing.T) {
	fixture := newTunnelFixture(t, "tunnel-relay", testFrontSettings())
	clientConn, clientReader, upstreamConn, upstreamReader := fixture.open(t)

	for _, message := range []string{"ping", "pong-longer", "x"} {
		io.WriteString(clientConn, message)
		readExactly(t, upstreamReader, message)
		io.WriteString(upstreamConn, "echo:"+message)
		readExactly(t, clientReader, "echo:"+message)
	}
	if fixture.tunnelsOpened.Load() != 1 {
		t.Errorf("tunnelOpened ran %d times, want once", fixture.tunnelsOpened.Load())
	}
	// the tunnel is open, so serveHttp hasn't returned and the block is
	// held
	select {
	case <-fixture.front.served:
		t.Fatalf("serveHttp returned while its tunnel was open")
	default:
	}
	if activeRequests := blockActiveRequests(fixture.group, 0); activeRequests != 1 {
		t.Errorf("block holds %d requests during the tunnel, want 1", activeRequests)
	}
	if tunnels := testutil.ToFloat64(forwardTunnelsGauge.WithLabelValues(fixture.serviceName)); tunnels != 1 {
		t.Errorf("tunnel gauge %v during the tunnel, want 1", tunnels)
	}

	// the client's end ends the tunnel, and the block sees its end
	clientConn.Close()
	readEnd(t, upstreamReader)
	fixture.front.waitServed(t)
	if activeRequests := blockActiveRequests(fixture.group, 0); activeRequests != 0 {
		t.Errorf("block holds %d requests after the tunnel, want 0", activeRequests)
	}
	if tunnels := testutil.ToFloat64(forwardTunnelsGauge.WithLabelValues(fixture.serviceName)); tunnels != 0 {
		t.Errorf("tunnel gauge %v after the tunnel, want 0", tunnels)
	}
}

// A block's balanced requests in flight.
func blockActiveRequests(group *upstreamGroup, blockIndex int) int {
	group.stateLock.Lock()
	defer group.stateLock.Unlock()
	return group.blocks[blockIndex].activeRequests
}

// The block's end ends the tunnel too.
func TestTunnelEndsWithTheBlock(t *testing.T) {
	fixture := newTunnelFixture(t, "tunnel-block-end", testFrontSettings())
	_, clientReader, upstreamConn, _ := fixture.open(t)
	io.WriteString(upstreamConn, "last")
	upstreamConn.Close()
	readExactly(t, clientReader, "last")
	readEnd(t, clientReader)
	fixture.front.waitServed(t)
}

// With no bytes either way for ForwardSettings.StreamTimeout the tunnel
// closes, as nginx's re-armed proxy_read_timeout closes it.
func TestTunnelIdleTimeout(t *testing.T) {
	settings := testFrontSettings()
	settings.Forward.StreamTimeout = 100 * time.Millisecond
	fixture := newTunnelFixture(t, "tunnel-idle", settings)
	start := time.Now()
	_, clientReader, _, upstreamReader := fixture.open(t)
	readEnd(t, clientReader)
	readEnd(t, upstreamReader)
	if elapsed := time.Since(start); elapsed < settings.Forward.StreamTimeout {
		t.Errorf("tunnel closed after %v, before its idle bound", elapsed)
	}
	fixture.front.waitServed(t)
}

// Bytes one way keep the tunnel open past the idle bound even when the
// other way is silent, since nginx re-arms its timer on every event.
func TestTunnelOneWayTrafficKeepsItOpen(t *testing.T) {
	settings := testFrontSettings()
	settings.Forward.StreamTimeout = 1 * time.Second
	fixture := newTunnelFixture(t, "tunnel-one-way", settings)
	_, clientReader, upstreamConn, _ := fixture.open(t)
	// twice the idle bound of block-to-client traffic, the client silent
	ticks := 20
	for i := range ticks {
		io.WriteString(upstreamConn, "t")
		readExactly(t, clientReader, "t")
		if i < ticks-1 {
			time.Sleep(settings.Forward.StreamTimeout / 10)
		}
	}
	select {
	case <-fixture.front.served:
		t.Fatalf("the tunnel closed while bytes flowed one way")
	default:
	}
	io.WriteString(upstreamConn, "still-open")
	readExactly(t, clientReader, "still-open")
}

// Closing the group cuts its tunnels: both sides end and serveHttp
// returns.
func TestTunnelCutByClose(t *testing.T) {
	fixture := newTunnelFixture(t, "tunnel-close", testFrontSettings())
	_, clientReader, _, upstreamReader := fixture.open(t)
	fixture.group.close()
	readEnd(t, clientReader)
	readEnd(t, upstreamReader)
	fixture.front.waitServed(t)
}

// A block that answers an upgrade request without switching is relayed as
// an ordinary response, and one that switches without being asked is a
// 502, never retried.
func TestTunnelSwitchOnlyWhenAsked(t *testing.T) {
	fixture := newTunnelFixture(t, "tunnel-unasked", testFrontSettings())
	responses := make(chan *http.Response, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodGet, fixture.front.url("/plain"), nil)
		response, _, _ := testDo(t, request)
		responses <- response
	}()
	upstreamConn, _, _ := fixture.raw.accept(t)
	io.WriteString(upstreamConn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	if response := receiveBounded(t, responses, "response"); response == nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("unasked switch: %v, want 502", response)
	}
	if attemptCount(fixture.serviceName, "g1", "invalid_header") != 1 {
		t.Errorf("an unasked switch was not counted as an invalid head")
	}

	go func() {
		request, _ := http.NewRequest(http.MethodGet, fixture.front.url("/socket"), nil)
		request.Header.Set(connectionHeader, "Upgrade")
		request.Header.Set(upgradeHeader, "websocket")
		response, _, _ := testDo(t, request)
		responses <- response
	}()
	upstreamConn, _, _ = fixture.raw.accept(t)
	io.WriteString(upstreamConn, "HTTP/1.1 426 Upgrade Required\r\nContent-Length: 0\r\n\r\n")
	if response := receiveBounded(t, responses, "response"); response == nil || response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("refused upgrade: %v, want the block's 426", response)
	}
	if fixture.tunnelsOpened.Load() != 0 {
		t.Errorf("a tunnel opened without a switch")
	}
}

// Writes 64 MiB, more than loopback buffers hold, to conn in a goroutine,
// and sends the write's end on the channel returned: nil when it all went,
// else the error that stopped it.
func writeFlood(conn net.Conn) chan error {
	flooded := make(chan error, 1)
	go func() {
		chunk := make([]byte, 64*1024)
		var err error
		for i := 0; err == nil && i < 1024; i += 1 {
			_, err = conn.Write(chunk)
		}
		flooded <- err
	}()
	return flooded
}

// HttpSettings.SendTimeout bounds each write to a tunnel's client, nginx's
// send_timeout on the client side: a client that reads keeps the tunnel
// relaying past it, and one that stops reading is cut once it passes
// without progress, while the block still sends and the idle bound is far
// off.
func TestTunnelSendTimeoutCutsStalledClient(t *testing.T) {
	settings := testFrontSettings()
	settings.Http.SendTimeout = 200 * time.Millisecond
	fixture := newTunnelFixture(t, "tunnel-client-stall", settings)
	_, clientReader, upstreamConn, _ := fixture.open(t)
	// twice the bound of traffic the client takes: every write gets its own
	// bound, none left from an earlier one
	for range 10 {
		time.Sleep(settings.Http.SendTimeout / 5)
		io.WriteString(upstreamConn, "t")
		readExactly(t, clientReader, "t")
	}
	// the block sends without end; the client reads none of it
	flooded := writeFlood(upstreamConn)
	fixture.front.waitServed(t)
	if err := receiveBounded(t, flooded, "end of the flood"); err == nil {
		t.Errorf("the block's bytes were all taken for a client that read none of them")
	}
}

// A tunnel whose block stops reading closes once
// ForwardSettings.StreamTimeout passes without progress on its writes,
// nginx's proxy_send_timeout on websocket routes, though bytes the other way
// keep the idle bound from passing.
func TestTunnelStreamTimeoutCutsStalledBlock(t *testing.T) {
	settings := testFrontSettings()
	settings.Forward.StreamTimeout = 1 * time.Second
	fixture := newTunnelFixture(t, "tunnel-block-stall", settings)
	clientConn, clientReader, upstreamConn, _ := fixture.open(t)
	// the block sends a byte at a time, well within the idle bound, and the
	// client takes them
	tickerDone := make(chan struct{})
	defer close(tickerDone)
	go func() {
		for {
			select {
			case <-tickerDone:
				return
			case <-time.After(settings.Forward.StreamTimeout / 100):
			}
			if _, err := upstreamConn.Write([]byte("t")); err != nil {
				return
			}
		}
	}()
	go io.Copy(io.Discard, clientReader)
	// the client sends without end; the block reads none of it
	flooded := writeFlood(clientConn)
	fixture.front.waitServed(t)
	if err := receiveBounded(t, flooded, "end of the flood"); err == nil {
		t.Errorf("the client's bytes were all taken for a block that read none of them")
	}
}
