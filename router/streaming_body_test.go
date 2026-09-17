package router

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These tests drive real sockets: the deadlines under test live on the
// connection, and a recorder has none. The client writes the body at a chosen
// pace so the server timeouts, sized for a body the lb already holds, are what
// fire when the router does nothing.

// countBody reads the whole body and answers with its length; a read error is
// the 408 a stalled streamed upload should surface as.
func countBody(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestTimeout)
		return
	}
	fmt.Fprintf(w, "%d", len(data))
}

func newStreamingTestServer(t *testing.T, routes []*Route, readTimeout time.Duration, writeTimeout time.Duration, defaults StreamingBody) *httptest.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	router := NewRouter(ctx, routes)
	router.SetStreamingBody(defaults)
	server := httptest.NewUnstartedServer(router)
	server.Config.ReadTimeout = readTimeout
	server.Config.WriteTimeout = writeTimeout
	server.Start()
	t.Cleanup(server.Close)
	return server
}

// a raw client so the test controls the pace of the body bytes on the wire
type streamingTestClient struct {
	t    *testing.T
	conn net.Conn
}

func dialStreamingTest(t *testing.T, server *httptest.Server) *streamingTestClient {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// registered after server.Close, so it runs first: a server goroutine
	// still draining sees EOF instead of holding Close for the drain timeout
	t.Cleanup(func() { conn.Close() })
	return &streamingTestClient{t: t, conn: conn}
}

func (self *streamingTestClient) head(path string, contentLength int, streamed bool) {
	self.t.Helper()
	head := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: test\r\nContent-Length: %d\r\n", path, contentLength)
	if streamed {
		head += RequestBufferingHeader + ": " + RequestBufferingOff + "\r\n"
	}
	head += "\r\n"
	if _, err := io.WriteString(self.conn, head); err != nil {
		self.t.Fatal(err)
	}
}

func (self *streamingTestClient) write(chunk string) error {
	_, err := io.WriteString(self.conn, chunk)
	return err
}

// writePaced writes the body in `count` chunks separated by `gap`
func (self *streamingTestClient) writePaced(chunk string, count int, gap time.Duration) {
	self.t.Helper()
	for i := 0; i < count; i++ {
		if 0 < i {
			time.Sleep(gap)
		}
		if err := self.write(chunk); err != nil {
			self.t.Fatalf("chunk %d: %v", i, err)
		}
	}
}

func (self *streamingTestClient) response() (*http.Response, error) {
	self.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	return http.ReadResponse(bufio.NewReader(self.conn), nil)
}

func (self *streamingTestClient) responseBody() (int, string) {
	self.t.Helper()
	response, err := self.response()
	if err != nil {
		self.t.Fatalf("response: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		self.t.Fatalf("response body: %v", err)
	}
	return response.StatusCode, string(body)
}

// The server ReadTimeout covers the whole body. A streamed upload that pauses
// between chunks for less than the idle window, but in total for longer than
// ReadTimeout, completes; the same upload without the lb's mark is cut, which
// is the pre-existing behavior for a buffered route.
func TestStreamingBodyOutlivesServerReadTimeout(t *testing.T) {
	server := newStreamingTestServer(
		t,
		[]*Route{NewRoute("POST", "/count", countBody)},
		500*time.Millisecond,
		0,
		StreamingBody{IdleTimeout: 3 * time.Second},
	)

	client := dialStreamingTest(t, server)
	client.head("/count", 30, true)
	client.writePaced(strings.Repeat("x", 10), 3, 400*time.Millisecond)
	status, body := client.responseBody()
	if status != http.StatusOK || body != "30" {
		t.Fatalf("streamed upload: status=%d body=%q, want 200 \"30\"", status, body)
	}

	control := dialStreamingTest(t, server)
	control.head("/count", 30, false)
	for i := 0; i < 3; i++ {
		if 0 < i {
			time.Sleep(400 * time.Millisecond)
		}
		// the server cuts the read at ReadTimeout; later writes may fail
		control.write(strings.Repeat("x", 10))
	}
	if response, err := control.response(); err == nil && response.StatusCode == http.StatusOK {
		t.Fatal("buffered route outlived the server ReadTimeout without the lb's mark")
	}
}

// A route's own policy applies without the lb's mark: the alt front serves
// the same routes with no lb in front.
func TestStreamingRoutePolicyAppliesWithoutHeader(t *testing.T) {
	server := newStreamingTestServer(
		t,
		[]*Route{NewStreamingRoute("POST", "/count", countBody, StreamingBody{IdleTimeout: 3 * time.Second})},
		500*time.Millisecond,
		0,
		StreamingBody{},
	)

	client := dialStreamingTest(t, server)
	client.head("/count", 30, false)
	client.writePaced(strings.Repeat("x", 10), 3, 400*time.Millisecond)
	status, body := client.responseBody()
	if status != http.StatusOK || body != "30" {
		t.Fatalf("route policy: status=%d body=%q, want 200 \"30\"", status, body)
	}
}

// A streamed upload that stalls for longer than the idle window is cut, and
// the handler's error response still reaches the client.
func TestStreamingBodyIdleTimeoutCutsStalledUpload(t *testing.T) {
	server := newStreamingTestServer(
		t,
		[]*Route{NewRoute("POST", "/count", countBody)},
		0,
		0,
		// the drain after the 408 ends at the client's close; keep its bound short
		StreamingBody{IdleTimeout: 300 * time.Millisecond, DrainTimeout: 500 * time.Millisecond},
	)

	client := dialStreamingTest(t, server)
	client.head("/count", 30, true)
	if err := client.write(strings.Repeat("x", 10)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	status, _ := client.responseBody()
	if status != http.StatusRequestTimeout {
		t.Fatalf("stalled upload: status=%d, want 408", status)
	}
}

// A handler that responds without reading the body must not close with the
// body unread. net/http drains at most 256 KiB itself and then closes, which
// resets the connection; the router drains the rest so the response is read
// cleanly. The client's blocking write of the whole body completing is the
// proof that the server consumed it.
func TestStreamingBodyDrainsUnreadBodyAfterEarlyResponse(t *testing.T) {
	early := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not authorized.", http.StatusUnauthorized)
	}
	server := newStreamingTestServer(
		t,
		[]*Route{NewRoute("POST", "/early", early)},
		0,
		0,
		StreamingBody{},
	)

	client := dialStreamingTest(t, server)
	size := 512 * 1024
	client.head("/early", size, true)
	if err := client.write(strings.Repeat("x", size)); err != nil {
		t.Fatalf("the server did not drain the unread body: %v", err)
	}
	status, body := client.responseBody()
	if status != http.StatusUnauthorized || strings.TrimSpace(body) != "Not authorized." {
		t.Fatalf("early response: status=%d body=%q, want 401", status, body)
	}
}

// The server WriteTimeout starts at the end of the headers, so a slow upload
// eats the response budget. Once the streamed body is in hand the router
// re-arms the write deadline; the buffered route's response is lost.
func TestStreamingBodyRearmsResponseDeadlineAfterUpload(t *testing.T) {
	server := newStreamingTestServer(
		t,
		[]*Route{NewRoute("POST", "/count", countBody)},
		0,
		500*time.Millisecond,
		StreamingBody{IdleTimeout: 3 * time.Second, ResponseTimeout: 2 * time.Second},
	)

	client := dialStreamingTest(t, server)
	client.head("/count", 30, true)
	client.writePaced(strings.Repeat("x", 10), 3, 300*time.Millisecond)
	status, body := client.responseBody()
	if status != http.StatusOK || body != "30" {
		t.Fatalf("streamed upload: status=%d body=%q, want 200 \"30\"", status, body)
	}

	control := dialStreamingTest(t, server)
	control.head("/count", 30, false)
	control.writePaced(strings.Repeat("x", 10), 3, 300*time.Millisecond)
	if response, err := control.response(); err == nil && response.StatusCode == http.StatusOK {
		t.Fatal("buffered route answered after its WriteTimeout had passed")
	}
}

// Policy precedence: a route's own policy, then the lb's mark selecting the
// router default, and nothing for a request with no body to stream.
func TestStreamingPolicyPrecedence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router := NewRouter(ctx, nil)
	router.SetStreamingBody(StreamingBody{IdleTimeout: time.Second})

	plain := NewRoute("POST", "/plain", countBody)
	own := NewStreamingRoute("POST", "/own", countBody, StreamingBody{IdleTimeout: time.Minute})

	marked := httptest.NewRequest(http.MethodPost, "/plain", strings.NewReader("body"))
	marked.Header.Set(RequestBufferingHeader, "OFF")
	if policy, ok := router.streamingPolicy(plain, marked); !ok || policy.IdleTimeout != time.Second {
		t.Fatalf("lb mark: policy=%+v ok=%t, want the router default", policy, ok)
	}

	unmarked := httptest.NewRequest(http.MethodPost, "/plain", strings.NewReader("body"))
	if _, ok := router.streamingPolicy(plain, unmarked); ok {
		t.Fatal("an unmarked request on a plain route is not streamed")
	}

	if policy, ok := router.streamingPolicy(own, unmarked); !ok || policy.IdleTimeout != time.Minute {
		t.Fatalf("route policy: policy=%+v ok=%t, want the route's own", policy, ok)
	}

	bodyless := httptest.NewRequest(http.MethodGet, "/own", nil)
	bodyless.Header.Set(RequestBufferingHeader, RequestBufferingOff)
	if _, ok := router.streamingPolicy(own, bodyless); ok {
		t.Fatal("a request with no body has nothing to stream")
	}
}
