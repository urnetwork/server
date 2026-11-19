package nginx

// End-to-end tests of forwarding through the assembled front: websocket
// tunnels, streamed responses, D7's retries, and D19 and D21's body storage,
// each checked at the client and at the recording blocks.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
)

// A websocket block: it upgrades every request, greets the client, and
// answers each message with "echo:" and the message, recording what it got.
// Safe for concurrent use.
type e2eWebsocketBlock struct {
	upgrader *websocket.Upgrader
	// each message the block read
	messages chan string
	// closed when the first tunnel's read loop has ended
	ended     chan struct{}
	endedOnce sync.Once
}

// A block whose tests open one tunnel through it.
func newE2eWebsocketBlock() *e2eWebsocketBlock {
	return &e2eWebsocketBlock{
		upgrader: &websocket.Upgrader{},
		messages: make(chan string, 16),
		ended:    make(chan struct{}),
	}
}

// The block's handler.
func (self *e2eWebsocketBlock) serveHttp(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
	conn, err := self.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer self.endedOnce.Do(func() {
		close(self.ended)
	})
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte("welcome")); err != nil {
		return
	}
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		self.messages <- string(message)
		if err := conn.WriteMessage(websocket.TextMessage, append([]byte("echo:"), message...)); err != nil {
			return
		}
	}
}

// Dials the gossip websocket through an interface socket, as a public
// client does.
func (self *e2eFront) dialE2eWebsocket(t *testing.T, addr string, path string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	dialer := &websocket.Dialer{
		NetDialTLSContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return self.dialTls(ctx, addr, "gossip.service.example", []string{"http/1.1"})
		},
		HandshakeTimeout: e2eWaitTimeout,
	}
	ctx, cancel := context.WithTimeout(t.Context(), e2eWaitTimeout)
	defer cancel()
	conn, response, err := dialer.DialContext(ctx, "wss://gossip.service.example"+path, nil)
	if conn != nil {
		t.Cleanup(func() {
			conn.Close()
		})
	}
	return conn, response, err
}

// Reads one message from a client websocket, within e2eWaitTimeout.
func readE2eWebsocketMessage(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(e2eWaitTimeout))
	_, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("reading the websocket: %v", err)
	}
	return string(message)
}

// Writes one message to a client websocket and checks the block's echo.
func exchangeE2eWebsocketMessage(t *testing.T, conn *websocket.Conn, block *e2eWebsocketBlock, message string) {
	t.Helper()
	conn.SetWriteDeadline(time.Now().Add(e2eWaitTimeout))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
		t.Fatalf("writing the websocket: %v", err)
	}
	if received := receiveE2e(t, block.messages, "message at the block"); received != message {
		t.Fatalf("the block read %q, want %q", received, message)
	}
	if echo := readE2eWebsocketMessage(t, conn); echo != "echo:"+message {
		t.Fatalf("the client read %q, want the block's echo", echo)
	}
}

// A forwarded websocket, on gossip, a websocket service, is relayed both
// ways through the front's TLS: the block gets the upgrade with nginx's
// contract, the client gets the block's 101 with the location's headers, and
// messages pass in each direction. The upgraded connection holds its
// concurrency slot (D8) until it closes: with net_connections 1, the
// source's next request on that interface is a 429 that reaches no block
// while the tunnel is open, and is served once the tunnel has closed; the
// other interface's quota was never touched.
func TestFrontE2eRelaysAWebsocketAndHoldsItsSlot(t *testing.T) {
	e2e := newE2eFront(t, nil, func(routeTable *RouteTable) {
		eth1, _ := routeTable.Interface("eth1")
		eth1.RateLimit.RequestsPerMinute = 0
		eth1.RateLimit.NetConnections = 1
	})
	eth1 := e2e.iface(t, "eth1")
	eth2 := e2e.iface(t, "eth2")
	websocketBlock := newE2eWebsocketBlock()
	gossip := e2e.upstream(t, "gossip", "g1")
	gossip.setHandler(websocketBlock.serveHttp)

	conn, response, err := e2e.dialE2eWebsocket(t, eth1.httpsAddr, "/ws?room=1")
	if err != nil {
		t.Fatalf("the websocket dial failed: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade answered %d", response.StatusCode)
	}
	if response.Header.Get("Strict-Transport-Security") == "" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("the 101 lacks the service name's security headers: %v", response.Header)
	}
	upgrade := gossip.awaitRequests(t, 1)[0]
	if upgrade.requestUri != "/ws?room=1" || upgrade.host != "gossip.service.example" {
		t.Errorf("the block got %q for host %q", upgrade.requestUri, upgrade.host)
	}
	if upgrade.header.Get("Upgrade") != "websocket" || upgrade.header.Get("Connection") != "upgrade" {
		t.Errorf("the block got Upgrade %q and Connection %q", upgrade.header.Get("Upgrade"), upgrade.header.Get("Connection"))
	}
	if forwardedFor := upgrade.header.Get("X-UR-Forwarded-For"); forwardedFor != conn.LocalAddr().String() {
		t.Errorf("the block got X-UR-Forwarded-For %q, want %q", forwardedFor, conn.LocalAddr().String())
	}
	if greeting := readE2eWebsocketMessage(t, conn); greeting != "welcome" {
		t.Fatalf("the client read %q first, want the block's greeting", greeting)
	}
	exchangeE2eWebsocketMessage(t, conn, websocketBlock, "first")
	exchangeE2eWebsocketMessage(t, conn, websocketBlock, "second")

	// the open tunnel holds eth1's one slot for this address
	requestCounts := e2e.requestCounts()
	eth1Client := e2e.newH1Client(t, eth1.httpsAddr, "api.service.example")
	response, _ = e2eDo(t, eth1Client, newE2eRequest(t, "GET", "https://api.service.example/while-open", nil))
	if response.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a request on eth1 while the tunnel is open = %d, want 429", response.StatusCode)
	}
	if requests := e2e.requestsSince(requestCounts); len(requests) != 0 {
		t.Errorf("a refused request reached %v", e2eRequestServices(requests))
	}
	eth2Client := e2e.newH1Client(t, eth2.httpsAddr, "api.service.example")
	response, _ = e2eDo(t, eth2Client, newE2eRequest(t, "GET", "https://api.service.example/other-interface", nil))
	if response.StatusCode != http.StatusOK {
		t.Errorf("a request on eth2 while eth1's tunnel is open = %d, want 200", response.StatusCode)
	}

	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(e2eWaitTimeout))
	conn.Close()
	waitE2eClosed(t, websocketBlock.ended, "the block's end of the tunnel")
	eth1.httpsServed.await(t, "/ws?room=1")
	response, body := e2eDo(t, eth1Client, newE2eRequest(t, "GET", "https://api.service.example/after-close", nil))
	if response.StatusCode != http.StatusOK || !bytes.HasPrefix(body, []byte("api/")) {
		t.Errorf("a request on eth1 after the tunnel closed = %d %q, want the block's 200", response.StatusCode, body)
	}
}

// A response is relayed as it arrives (proxy_buffering off): the client
// reads the block's first write while the block still holds the rest, over
// h1 and over h2.
func TestFrontE2eStreamsAResponseAsItArrives(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	web := e2e.upstream(t, "web", "g1")
	h2ClientConn, _ := e2e.newH2ClientConn(t, eth1.httpsAddr, "web.service.example")
	h1Client := e2e.newH1Client(t, eth1.httpsAddr, "web.service.example")
	roundTrips := []struct {
		name      string
		roundTrip func(request *http.Request) (*http.Response, error)
	}{
		{name: "h1", roundTrip: h1Client.Do},
		{name: "h2", roundTrip: h2ClientConn.RoundTrip},
	}
	for _, roundTrip := range roundTrips {
		hold := newE2eHold(t)
		web.setHandler(func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "first\n")
			http.NewResponseController(w).Flush()
			hold.wait(r)
			io.WriteString(w, "second\n")
		})
		response, err := roundTrip.roundTrip(newE2eRequest(t, "GET", "https://web.service.example/stream", nil))
		if err != nil {
			t.Fatalf("%s: %v", roundTrip.name, err)
		}
		reader := bufio.NewReader(response.Body)
		first, err := reader.ReadString('\n')
		if err != nil || first != "first\n" {
			t.Fatalf("%s: read %q, %v before the block wrote the rest, want its first write", roundTrip.name, first, err)
		}
		hold.release()
		rest, err := io.ReadAll(reader)
		response.Body.Close()
		if err != nil || string(rest) != "second\n" {
			t.Errorf("%s: read %q, %v after the release", roundTrip.name, rest, err)
		}
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
			t.Errorf("%s: %d, Content-Type %q", roundTrip.name, response.StatusCode, response.Header.Get("Content-Type"))
		}
	}
}

// One attempt a block answered, in arrival order across the service's
// blocks.
type e2eAttempt struct {
	block  string
	status int
	body   []byte
	// the length the block was told
	contentLength int64
}

// The attempts every block of a service answers, each with the status
// statusOf gives for its place in the order, 1 first.
type e2eAttempts struct {
	stateLock sync.Mutex
	attempts  []*e2eAttempt
}

// A handler for every block of the service, answering the nth attempt
// across them with statusOf(n) and a body naming the block.
func (self *e2eAttempts) handler(statusOf func(attemptNumber int) int) func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
	return func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
		status := func() int {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			status := statusOf(len(self.attempts) + 1)
			self.attempts = append(self.attempts, &e2eAttempt{
				block:         request.block,
				status:        status,
				body:          request.body,
				contentLength: request.contentLength,
			})
			return status
		}()
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		io.WriteString(w, strconv.Itoa(status)+" from "+request.block)
	}
}

// The attempts so far.
func (self *e2eAttempts) recorded() []*e2eAttempt {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*e2eAttempt{}, self.attempts...)
}

// D7: an upstream 503 is retried once on the service's other block, with
// the buffered body replayed byte for byte, the part past the 64 KiB memory
// prefix from its spill file included, and the second block's answer is the
// client's, over h1 and h2. A second 503 is final. The attempts are counted
// where they landed, and the body's storage is released once the request
// ends.
func TestFrontE2eRetriesA503OnAnotherBlockWithTheBodyReplayed(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	h1Client := e2e.newH1Client(t, eth1.httpsAddr, "api.service.example")
	h2ClientConn, _ := e2e.newH2ClientConn(t, eth1.httpsAddr, "api.service.example")
	body := e2eBody(200 * 1024)
	cases := []struct {
		path      string
		roundTrip func(request *http.Request) (*http.Response, error)
		statusOf  func(attemptNumber int) int
		status    int
	}{
		{
			path:      "/retry",
			roundTrip: h1Client.Do,
			statusOf: func(attemptNumber int) int {
				if attemptNumber == 1 {
					return http.StatusServiceUnavailable
				}
				return http.StatusOK
			},
			status: http.StatusOK,
		},
		{
			path:      "/retry-h2",
			roundTrip: h2ClientConn.RoundTrip,
			statusOf: func(attemptNumber int) int {
				if attemptNumber == 1 {
					return http.StatusServiceUnavailable
				}
				return http.StatusOK
			},
			status: http.StatusOK,
		},
		{
			path:      "/retry-again",
			roundTrip: h1Client.Do,
			statusOf: func(attemptNumber int) int {
				return http.StatusServiceUnavailable
			},
			status: http.StatusServiceUnavailable,
		},
	}
	for _, c := range cases {
		attempts := &e2eAttempts{}
		e2e.setServiceHandler(t, "api", attempts.handler(c.statusOf))
		request := newE2eRequest(t, "POST", "https://api.service.example"+c.path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/octet-stream")
		response, err := c.roundTrip(request)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		responseBody, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatalf("%s: reading the body: %v", c.path, err)
		}

		recorded := attempts.recorded()
		if len(recorded) != 2 {
			t.Fatalf("%s: %d attempts reached the blocks, want 2", c.path, len(recorded))
		}
		if recorded[0].block == recorded[1].block {
			t.Errorf("%s: both attempts went to %s, want the retry on the other block", c.path, recorded[0].block)
		}
		for i, attempt := range recorded {
			if !bytes.Equal(attempt.body, body) || attempt.contentLength != int64(len(body)) {
				t.Errorf("%s: attempt %d at %s got %d bytes (Content-Length %d), equal %t, want the %d bytes sent", c.path, i+1, attempt.block, len(attempt.body), attempt.contentLength, bytes.Equal(attempt.body, body), len(body))
			}
		}
		if response.StatusCode != c.status || string(responseBody) != strconv.Itoa(c.status)+" from "+recorded[1].block {
			t.Errorf("%s: the client got %d %q, want the second block's %d", c.path, response.StatusCode, responseBody, c.status)
		}
		eth1.httpsServed.await(t, c.path)
		if memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
			t.Errorf("%s: the store holds %d bytes, %d spilled in %d files, for %d bodies, after the request ended", c.path, memoryBytes, spillBytes, spillFiles, bodies)
		}
	}
}

// D7: an application 500, 502 or 504, and a response cut short after its
// head reached the client, are never retried: the block's answer is the
// client's, a cut one aborted rather than completed, and exactly one attempt
// reaches a block. A retried 502 once ran a password reset twice.
func TestFrontE2eNeverRetriesOtherFailures(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	client := e2e.newH1Client(t, eth1.httpsAddr, "api.service.example")
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout} {
		attempts := &e2eAttempts{}
		e2e.setServiceHandler(t, "api", attempts.handler(func(attemptNumber int) int {
			return status
		}))
		path := "/fail-" + strconv.Itoa(status)
		response, responseBody := e2eDo(t, client, newE2eRequest(t, "POST", "https://api.service.example"+path, bytes.NewReader([]byte("reset the password"))))
		recorded := attempts.recorded()
		if len(recorded) != 1 {
			t.Fatalf("%d: %d attempts reached the blocks, want 1", status, len(recorded))
		}
		if response.StatusCode != status || string(responseBody) != strconv.Itoa(status)+" from "+recorded[0].block {
			t.Errorf("%d: the client got %d %q, want the block's answer", status, response.StatusCode, responseBody)
		}
	}

	requestCounts := e2e.requestCounts()
	e2e.setServiceHandler(t, "api", func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "0123456789")
		http.NewResponseController(w).Flush()
		// the block's connection ends 990 bytes short
		panic(http.ErrAbortHandler)
	})
	response, err := client.Do(newE2eRequest(t, "POST", "https://api.service.example/cut-short", bytes.NewReader([]byte("once"))))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a cut response: %d, %d bytes, %v, want the head and an aborted body", response.StatusCode, len(responseBody), err)
	}
	if requests := e2e.requestsSince(requestCounts); len(requests) != 1 {
		t.Errorf("a cut response reached %d blocks (%v), want 1", len(requests), e2eRequestServices(requests))
	}
}

// D19 and D21: a buffered body keeps its first 64 KiB in memory and the
// rest in one spill file, a body of unknown length is stored the same way
// and reaches the block with its length, and a body over the limit is a 413,
// declared or crossed, that reaches no block. Each body's memory, bytes and
// file are released when its request ends, and no spill file is left.
func TestFrontE2eStoresBodiesAcrossTheMemoryPrefix(t *testing.T) {
	settings := e2eFrontSettings(t)
	settings.Body.MaxBodyBytes = 256 * 1024
	e2e := newE2eFront(t, settings, nil)
	eth1 := e2e.iface(t, "eth1")
	web := e2e.upstream(t, "web", "g1")
	client := e2e.newH1Client(t, eth1.httpsAddr, "web.service.example")
	prefixBytes := settings.Body.MemoryPrefixBytes
	cases := []struct {
		path          string
		length        int
		unknownLength bool
		memoryBytes   int64
		spillBytes    int64
		spillFiles    int
	}{
		{path: "/at-the-prefix", length: 64 * 1024, memoryBytes: prefixBytes},
		{path: "/past-the-prefix", length: 64*1024 + 1, memoryBytes: prefixBytes, spillBytes: 1, spillFiles: 1},
		{path: "/unknown-length", length: 100 * 1024, unknownLength: true, memoryBytes: prefixBytes, spillBytes: 36 * 1024, spillFiles: 1},
		{path: "/at-the-limit", length: 256 * 1024, memoryBytes: prefixBytes, spillBytes: 192 * 1024, spillFiles: 1},
	}
	for i, c := range cases {
		hold := newE2eHold(t)
		web.setHandler(func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
			hold.wait(r)
			io.WriteString(w, "stored")
		})
		body := e2eBody(c.length)
		var requestBody io.Reader = bytes.NewReader(body)
		if c.unknownLength {
			// a reader net/http can't size, so the body goes chunked
			requestBody = io.MultiReader(bytes.NewReader(body))
		}
		request := newE2eRequest(t, "POST", "https://web.service.example"+c.path, requestBody)
		if c.unknownLength {
			request.ContentLength = -1
		}
		type result struct {
			response *http.Response
			body     []byte
			err      error
		}
		results := make(chan result, 1)
		go func() {
			response, err := client.Do(request)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer response.Body.Close()
			responseBody, err := io.ReadAll(response.Body)
			results <- result{response: response, body: responseBody, err: err}
		}()

		// the block holds the request: the whole body is stored
		received := web.awaitRequests(t, i+1)[i]
		if !bytes.Equal(received.body, body) || received.contentLength != int64(c.length) || len(received.transferEncoding) != 0 {
			t.Errorf("%s: the block got %d bytes, equal %t, Content-Length %d, Transfer-Encoding %v, want the %d bytes with their length", c.path, len(received.body), bytes.Equal(received.body, body), received.contentLength, received.transferEncoding, c.length)
		}
		memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t)
		if memoryBytes != c.memoryBytes || spillBytes != c.spillBytes || spillFiles != c.spillFiles || bodies != 1 {
			t.Errorf("%s: the store holds %d bytes in memory and %d in %d spill files for %d bodies, want %d, %d in %d, for 1", c.path, memoryBytes, spillBytes, spillFiles, bodies, c.memoryBytes, c.spillBytes, c.spillFiles)
		}
		if names := e2e.spillDirNames(t); len(names) != 0 {
			t.Errorf("%s: the spill directory lists %v while the body is held, want the file unlinked", c.path, names)
		}
		hold.release()
		r := receiveE2e(t, results, "response")
		if r.err != nil || r.response.StatusCode != http.StatusOK || string(r.body) != "stored" {
			t.Fatalf("%s: %v, %v %q", c.path, r.err, r.response, r.body)
		}
		eth1.httpsServed.await(t, c.path)
		if memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
			t.Errorf("%s: the store holds %d bytes, %d spilled in %d files, for %d bodies, after the request ended", c.path, memoryBytes, spillBytes, spillFiles, bodies)
		}
	}

	requestCounts := e2e.requestCounts()
	// a declared length over the limit, refused before any body byte
	rawConn := e2e.dialRaw(t, eth1.httpsAddr, "web.service.example")
	response, _ := rawConn.exchange(t,
		"POST /declared-too-large HTTP/1.1",
		"Host: web.service.example",
		"Content-Length: "+strconv.Itoa(256*1024+1),
	)
	if response.StatusCode != http.StatusRequestEntityTooLarge || !response.Close {
		t.Errorf("a declared body over the limit = %d, close %t, want 413 and close", response.StatusCode, response.Close)
	}
	// a body of unknown length that crosses the limit
	request := newE2eRequest(t, "POST", "https://web.service.example/crosses-the-limit", io.MultiReader(bytes.NewReader(e2eBody(256*1024+16*1024))))
	request.ContentLength = -1
	response, _ = e2eDo(t, client, request)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a body of unknown length over the limit = %d, want 413", response.StatusCode)
	}
	eth1.httpsServed.await(t, "/crosses-the-limit")
	if requests := e2e.requestsSince(requestCounts); len(requests) != 0 {
		t.Errorf("a body over the limit reached %v", e2eRequestServices(requests))
	}
	if memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
		t.Errorf("the store holds %d bytes, %d spilled in %d files, for %d bodies, after the refusals", memoryBytes, spillBytes, spillFiles, bodies)
	}
	if names := e2e.spillDirNames(t); len(names) != 0 {
		t.Errorf("the spill directory lists %v after every request ended", names)
	}
}

// A streamable service's request body (mcp) is forwarded as it arrives,
// marked X-UR-Request-Buffering: off: the block reads the client's first
// part while the client still holds the rest, and the front stores none of
// it.
func TestFrontE2eStreamsARequestBodyAsItArrives(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	mcp := e2e.upstream(t, "mcp", "g1")
	firstParts := make(chan string, 1)
	mcp.setStreamHandler(func(w http.ResponseWriter, r *http.Request) {
		reader := bufio.NewReader(r.Body)
		firstPart, err := reader.ReadString('\n')
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		firstParts <- firstPart
		rest, err := io.ReadAll(reader)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		io.WriteString(w, "got "+string(rest))
	})

	bodyReader, bodyWriter := io.Pipe()
	t.Cleanup(func() {
		bodyWriter.Close()
	})
	request := newE2eRequest(t, "POST", "https://mcp.service.example/stream-up", bodyReader)
	request.ContentLength = -1
	type result struct {
		response *http.Response
		body     []byte
		err      error
	}
	results := make(chan result, 1)
	client := e2e.newH1Client(t, eth1.httpsAddr, "mcp.service.example")
	go func() {
		response, err := client.Do(request)
		if err != nil {
			results <- result{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		results <- result{response: response, body: body, err: err}
	}()

	if _, err := io.WriteString(bodyWriter, "part one\n"); err != nil {
		t.Fatal(err)
	}
	if firstPart := receiveE2e(t, firstParts, "first part at the block"); firstPart != "part one\n" {
		t.Fatalf("the block read %q first", firstPart)
	}
	if _, _, _, bodies := e2e.bodyUsage(t); bodies != 0 {
		t.Errorf("the store holds %d bodies for a streamed one, want none", bodies)
	}
	io.WriteString(bodyWriter, "part two")
	bodyWriter.Close()
	r := receiveE2e(t, results, "response")
	if r.err != nil || r.response.StatusCode != http.StatusOK || string(r.body) != "got part two" {
		t.Fatalf("%v, %v %q, want the block's answer to the whole body", r.err, r.response, r.body)
	}
	request0 := mcp.awaitRequests(t, 1)[0]
	if buffering := request0.header.Get("X-UR-Request-Buffering"); buffering != "off" {
		t.Errorf("the block got X-UR-Request-Buffering %q, want off", buffering)
	}
	if len(request0.transferEncoding) != 1 || request0.transferEncoding[0] != "chunked" || request0.contentLength != -1 {
		t.Errorf("the block got Transfer-Encoding %v and Content-Length %d, want a chunked body", request0.transferEncoding, request0.contentLength)
	}
}

// A large response arrives whole and in order, with the length the block
// declared, over h1 and h2, through the front's per-write flushes and h2's
// flow control.
func TestFrontE2eRelaysALargeResponseWhole(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	largeBody := e2eBody(4*1024*1024 + 17)
	e2e.upstream(t, "web", "g1").setHandler(func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(largeBody)))
		w.Write(largeBody)
	})
	h2ClientConn, _ := e2e.newH2ClientConn(t, eth1.httpsAddr, "web.service.example")
	h1Client := e2e.newH1Client(t, eth1.httpsAddr, "web.service.example")
	roundTrips := []struct {
		name      string
		roundTrip func(request *http.Request) (*http.Response, error)
	}{
		{name: "h1", roundTrip: h1Client.Do},
		{name: "h2", roundTrip: h2ClientConn.RoundTrip},
	}
	for _, roundTrip := range roundTrips {
		response, err := roundTrip.roundTrip(newE2eRequest(t, "GET", "https://web.service.example/large", nil))
		if err != nil {
			t.Fatalf("%s: %v", roundTrip.name, err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || !bytes.Equal(body, largeBody) || response.ContentLength != int64(len(largeBody)) {
			t.Errorf("%s: %d of %d bytes, equal %t, Content-Length %d, %v", roundTrip.name, len(body), len(largeBody), bytes.Equal(body, largeBody), response.ContentLength, err)
		}
	}
}

// A body that stalls past client_body_timeout gets no response through the
// front, as nginx terminates such a request: on h1 the connection closes
// with nothing written, and on h2 the stream is reset, with INTERNAL_ERROR
// where nginx's code is PROTOCOL_ERROR, and the connection serves the next
// stream. It holds for a buffered body (api), read before any block, and a
// streamed one (mcp), whose block has its start. The front's handler lets
// the abort through to Go's servers, which log no panic for it, and each
// refusal is counted.
func TestFrontE2eBodyTimeoutEndsTheRequestWithoutAResponse(t *testing.T) {
	settings := e2eFrontSettings(t)
	settings.Body.ClientBodyTimeout = 100 * time.Millisecond
	e2e := newE2eFront(t, settings, nil)
	eth1 := e2e.iface(t, "eth1")
	for _, service := range []string{"api", "mcp"} {
		name := service + ".service.example"
		timeouts := bodyRefusalCount(service, bodyRefusalTimeout)

		raw := e2e.dialRaw(t, eth1.httpsAddr, name)
		raw.conn.SetDeadline(time.Now().Add(e2eWaitTimeout))
		io.WriteString(raw.conn, "POST /stalled HTTP/1.1\r\nHost: "+name+"\r\nContent-Length: 100\r\n\r\nfirst-ten!")
		if answered, err := io.ReadAll(raw.reader); len(answered) != 0 || (err != nil && !isClosedError(err)) {
			t.Errorf("%s over h1: the stalled body was answered %q %v, want the connection closed with nothing written", service, answered, err)
		}
		eth1.httpsServed.await(t, "/stalled")

		clientConn, _ := e2e.newH2ClientConn(t, eth1.httpsAddr, name)
		bodyReader, bodyWriter := io.Pipe()
		t.Cleanup(func() {
			bodyWriter.Close()
		})
		go io.WriteString(bodyWriter, "first-ten!")
		response, err := clientConn.RoundTrip(newE2eRequest(t, "POST", "https://"+name+"/stalled-h2", bodyReader))
		var streamError http2.StreamError
		if !errors.As(err, &streamError) || streamError.Code != http2.ErrCodeInternal {
			if response != nil {
				response.Body.Close()
			}
			t.Errorf("%s over h2: %v %v, want the stream reset with INTERNAL_ERROR", service, response, err)
		}
		eth1.httpsServed.await(t, "/stalled-h2")
		next, body := e2eRoundTripH2(t, clientConn, newE2eRequest(t, "GET", "https://"+name+"/next", nil))
		if next.StatusCode != http.StatusOK || string(body) != service+"/g1" {
			t.Errorf("%s over h2: the next stream got %d %q, want its block's answer on the same connection", service, next.StatusCode, body)
		}

		if counted := bodyRefusalCount(service, bodyRefusalTimeout) - timeouts; counted != 2 {
			t.Errorf("%s: %v timeouts counted, want one per protocol", service, counted)
		}
	}
	if memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
		t.Errorf("the store holds %d bytes, %d spilled in %d files, for %d bodies, after the timeouts", memoryBytes, spillBytes, spillFiles, bodies)
	}
}
