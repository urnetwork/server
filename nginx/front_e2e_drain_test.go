package nginx

// End-to-end tests of the front's drain (D12): BeginDrain closes client
// connections after their current response while accepted work runs on.

import (
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
)

// After BeginDrain, every h1 response carries Connection: close, so the
// client's next request dials again, and every h2 response is followed by a
// GOAWAY, so the client's next stream goes to a new connection: clients
// re-dial and reach the new generation. A new h2 stream opened during the
// drain is served. Requests in flight when the drain began complete, on h1
// closing the connection after them, on h2 past the GOAWAY, and a websocket
// established before it keeps relaying both ways until it ends on its own. A
// new upgrade is refused with 503 instead of holding the old generation.
func TestFrontE2eDrainClosesConnectionsAndKeepsWebsockets(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	web := e2e.upstream(t, "web", "g1")
	hold := newE2eHold(t)
	web.setHandler(func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
		if request.requestUri == "/in-flight" || request.requestUri == "/h2-in-flight" {
			hold.wait(r)
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "web "+request.requestUri)
	})
	websocketBlock := newE2eWebsocketBlock()
	gossip := e2e.upstream(t, "gossip", "g1")
	gossip.setHandler(websocketBlock.serveHttp)

	// before the drain: a keepalive h1 connection, an h2 connection, a
	// websocket, and an h1 request and an h2 stream held at their block
	keepaliveClient := e2e.newH1Client(t, eth1.httpsAddr, "web.service.example")
	response, _ := e2eDo(t, keepaliveClient, newE2eRequest(t, "GET", "https://web.service.example/before", nil))
	if response.StatusCode != http.StatusOK || response.Close {
		t.Fatalf("before the drain: %d, close %t", response.StatusCode, response.Close)
	}
	h2Framer := e2e.dialH2Framer(t, eth1.httpsAddr, "web.service.example")
	status, body, goAwayFrame := h2Framer.await(t, h2Framer.get(t, "web.service.example", "/h2-before"), false)
	if status != "200" || string(body) != "web /h2-before" || goAwayFrame != nil {
		t.Fatalf("h2 before the drain: %s %q, goaway %v", status, body, goAwayFrame)
	}
	conn, _, err := e2e.dialE2eWebsocket(t, eth1.httpsAddr, "/ws")
	if err != nil {
		t.Fatalf("the websocket dial failed: %v", err)
	}
	if greeting := readE2eWebsocketMessage(t, conn); greeting != "welcome" {
		t.Fatalf("the client read %q first, want the block's greeting", greeting)
	}
	exchangeE2eWebsocketMessage(t, conn, websocketBlock, "before the drain")
	inFlightClient := e2e.newH1Client(t, eth1.httpsAddr, "web.service.example")
	type result struct {
		response *http.Response
		body     []byte
		err      error
	}
	inFlightResults := make(chan result, 1)
	go func() {
		response, err := inFlightClient.Do(newE2eRequest(t, "GET", "https://web.service.example/in-flight", nil))
		if err != nil {
			inFlightResults <- result{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		inFlightResults <- result{response: response, body: body, err: err}
	}()
	inFlightStreamId := h2Framer.get(t, "web.service.example", "/h2-in-flight")
	web.await(t, "the in-flight requests", func() bool {
		inFlightUris := map[string]bool{}
		for _, request := range web.requests {
			inFlightUris[request.requestUri] = true
		}
		return inFlightUris["/in-flight"] && inFlightUris["/h2-in-flight"]
	})

	e2e.front.BeginDrain()

	newStreamId := h2Framer.get(t, "web.service.example", "/h2-during")
	status, body, goAwayFrame = h2Framer.await(t, newStreamId, true)
	if status != "200" || string(body) != "web /h2-during" {
		t.Errorf("a new h2 stream during the drain: %s %q", status, body)
	}
	if goAwayFrame.ErrCode != http2.ErrCodeNo || goAwayFrame.LastStreamID < newStreamId {
		t.Errorf("h2 GOAWAY %v, last stream %d, want a graceful one that keeps stream %d", goAwayFrame.ErrCode, goAwayFrame.LastStreamID, newStreamId)
	}

	hold.release()
	inFlight := receiveE2e(t, inFlightResults, "in-flight response")
	if inFlight.err != nil || inFlight.response.StatusCode != http.StatusOK || string(inFlight.body) != "web /in-flight" || !inFlight.response.Close {
		t.Errorf("the in-flight h1 request: %v, %v %q, want the block's 200 with Connection: close", inFlight.err, inFlight.response, inFlight.body)
	}
	status, body, _ = h2Framer.await(t, inFlightStreamId, false)
	if status != "200" || string(body) != "web /h2-in-flight" {
		t.Errorf("the in-flight h2 stream: %s %q, want the block's 200", status, body)
	}

	reused := false
	request := newE2eRequest(t, "GET", "https://web.service.example/after", nil)
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		GotConn: func(gotConnInfo httptrace.GotConnInfo) {
			reused = gotConnInfo.Reused
		},
	}))
	response, body = e2eDo(t, keepaliveClient, request)
	if !reused {
		t.Errorf("the request after the drain began opened a new connection, want the keepalive one")
	}
	// net/http's client reads Connection: close into Close, and drops it
	// from the header map
	if response.StatusCode != http.StatusOK || string(body) != "web /after" || !response.Close {
		t.Errorf("h1 after the drain began: %d %q, close %t, want 200 with Connection: close", response.StatusCode, body, response.Close)
	}

	// the established websocket runs on, both ways
	exchangeE2eWebsocketMessage(t, conn, websocketBlock, "during the drain")
	requestCounts := e2e.requestCounts()
	_, response, err = e2e.dialE2eWebsocket(t, eth1.httpsAddr, "/ws-during-the-drain")
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable || !response.Close {
		t.Errorf("a new upgrade during the drain: %v, %v, want 503 with Connection: close", err, response)
	}
	if requests := e2e.requestsSince(requestCounts); len(requests) != 0 {
		t.Errorf("a new upgrade during the drain reached %v", e2eRequestServices(requests))
	}
	exchangeE2eWebsocketMessage(t, conn, websocketBlock, "still open")
	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(e2eWaitTimeout))
	conn.Close()
	waitE2eClosed(t, websocketBlock.ended, "the block's end of the tunnel")
	eth1.httpsServed.await(t, "/ws")
}

// During the drain a new upgrade to a forwarded websocket service is a 503
// however its Connection header reads: the forwarder tunnels on the block's
// 101 to any h1 request with an Upgrade header, sending Connection: upgrade
// itself, as nginx does, so one without the client's own Connection token
// would otherwise open a tunnel that holds the draining generation. It never
// reaches the block. Before the drain, the same request tunnels.
func TestFrontE2eDrainRefusesAnUpgradeWithoutTheConnectionToken(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	websocketBlock := newE2eWebsocketBlock()
	gossip := e2e.upstream(t, "gossip", "g1")
	gossip.setHandler(websocketBlock.serveHttp)
	upgradeHead := func(path string) []string {
		return []string{
			"GET " + path + " HTTP/1.1",
			"Host: gossip.service.example",
			"Upgrade: websocket",
			"Sec-WebSocket-Version: 13",
			"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==",
		}
	}
	upgrade := func(path string) *http.Response {
		rawConn := e2e.dialRaw(t, eth1.httpsAddr, "gossip.service.example")
		rawConn.conn.SetDeadline(time.Now().Add(e2eWaitTimeout))
		if _, err := io.WriteString(rawConn.conn, strings.Join(upgradeHead(path), "\r\n")+"\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(rawConn.reader, nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return response
	}

	if response := upgrade("/ws-before-the-drain"); response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("an upgrade without the Connection token before the drain = %d, want the block's 101", response.StatusCode)
	}

	e2e.front.BeginDrain()
	requestCounts := e2e.requestCounts()
	if response := upgrade("/ws-during-the-drain"); response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("an upgrade without the Connection token during the drain = %d, want 503", response.StatusCode)
	}
	if requests := e2e.requestsSince(requestCounts); len(requests) != 0 {
		t.Errorf("the upgrade during the drain reached %v", e2eRequestServices(requests))
	}
}
