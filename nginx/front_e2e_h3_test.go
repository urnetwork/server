package nginx

// End-to-end tests of the assembled front's api handler over a real http3
// server, for what depends on the vendored http3 (third_party/quic-go): it
// gives a request with no content-length a body of unknown length, ended by
// the stream's end, and ends a body short of its content-length as the
// stream's end too, without an error.

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// An h3 client of the front's api handler for eth1, served by an http3
// server on a loopback socket, as connect serves api names on QUIC. The
// server, its socket and the client close when the test ends.
func newE2eH3ApiClient(t *testing.T, e2e *e2eFront) *http.Client {
	t.Helper()
	// quic-go warns once per process when it can't raise the socket buffers
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	quicTransport := &quic.Transport{
		Conn: udpConn,
	}
	listener, err := quicTransport.ListenEarly(e2e.front.QuicTlsConfig(), &quic.Config{})
	if err != nil {
		udpConn.Close()
		t.Fatal(err)
	}
	h3Server := &http3.Server{
		Handler: e2e.front.H3ApiHandler("eth1"),
	}
	go h3Server.ServeListener(listener)
	h3Transport := &http3.Transport{
		TLSClientConfig: e2e.clientTlsConfig("api.service.example", nil),
		Dial: func(ctx context.Context, _ string, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
			return quic.DialAddr(ctx, udpConn.LocalAddr().String(), tlsConfig, quicConfig)
		},
	}
	t.Cleanup(func() {
		h3Transport.Close()
		h3Server.Close()
		listener.Close()
		quicTransport.Close()
		udpConn.Close()
	})
	return &http.Client{
		Transport: h3Transport,
		Timeout:   e2eWaitTimeout,
	}
}

// The api service with /stream/ paths streamed, as the api's streamable
// paths are.
func e2eStreamApiPaths(routeTable *RouteTable) {
	for _, service := range routeTable.Services {
		if service.Name == "api" {
			service.StreamablePaths = []string{"/stream/.*"}
		}
	}
}

// An h3 body whose stream ends 90 bytes short of its declared 100 is a 400,
// as nginx answers a stream that ends before its body ("client prematurely
// closed stream") and as the front answers on h2: on a buffered route it
// reaches no block, where the vendored http3's end of stream had it forwarded
// as a complete 10-byte body, and on a streamed route, whose head went
// upstream at once, it is a refusal, not one of the front's own 502s.
func TestFrontE2eH3BodyEndingShortOfItsLengthIs400(t *testing.T) {
	e2e := newE2eFront(t, nil, e2eStreamApiPaths)
	client := newE2eH3ApiClient(t, e2e)

	for _, c := range []struct {
		path     string
		streamed bool
	}{
		{path: "/short-buffered"},
		{path: "/stream/short", streamed: true},
	} {
		beforeIncomplete := bodyRefusalCount("api", bodyRefusalIncomplete)
		beforeForwardErrors := forwardErrorCount("api", http.StatusBadGateway)
		requestCounts := e2e.requestCounts()
		// the body isn't a type the client can measure, so it sends the
		// declared length and then the stream's end after the ten bytes
		request := newE2eRequest(t, "POST", "https://api.service.example"+c.path, io.NopCloser(strings.NewReader("0123456789")))
		request.ContentLength = 100
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		response.Body.Close()
		if response.ProtoMajor != 3 || response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: a body 90 bytes short of its length = HTTP/%d %d, want 400", c.path, response.ProtoMajor, response.StatusCode)
		}
		if count := bodyRefusalCount("api", bodyRefusalIncomplete) - beforeIncomplete; count != 1 {
			t.Errorf("%s: %v incomplete refusals, want 1", c.path, count)
		}
		if count := forwardErrorCount("api", http.StatusBadGateway) - beforeForwardErrors; count != 0 {
			t.Errorf("%s: %v of the front's own 502s, want none", c.path, count)
		}
		requests := e2e.requestsSince(requestCounts)
		if !c.streamed && len(requests) != 0 {
			t.Errorf("%s: the short body reached a block: %d requests", c.path, len(requests))
		}
		for _, upstreamRequest := range requests {
			if upstreamRequest.bodyErr == nil {
				t.Errorf("%s: block %s read a whole %d-byte body", c.path, upstreamRequest.block, len(upstreamRequest.body))
			}
		}
	}
}

// An h3 GET, which the vendored http3 gives a body of unknown length ended
// by its stream, goes to a buffered route's block with no framing, as the
// same GET does over h1 and h2 and as nginx's h3 sends a request whose stream
// ended with its head: no Content-Length and no Transfer-Encoding, and,
// while the block holds it, nothing reserved in the body store. An h3 POST of
// no declared length still goes with the length of what it sent.
func TestFrontE2eH3BodilessRequestHasNoFraming(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	client := newE2eH3ApiClient(t, e2e)
	hold := newE2eHold(t)
	e2e.setServiceHandler(t, "api", func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
		if r.URL.Path == "/held" {
			hold.wait(r)
		}
		io.WriteString(w, "answered")
	})

	responses := make(chan *http.Response, 1)
	go func() {
		response, err := client.Do(newE2eRequest(t, "GET", "https://api.service.example/held", nil))
		if err != nil {
			responses <- nil
			return
		}
		response.Body.Close()
		responses <- response
	}()
	held := e2e.awaitServiceRequests(t, "api", 1)[0]
	if memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
		t.Errorf("while the block holds the GET the store holds %d bytes, %d spilled in %d files, for %d bodies", memoryBytes, spillBytes, spillFiles, bodies)
	}
	hold.release()
	if response := receiveE2e(t, responses, "the GET's response"); response == nil || response.ProtoMajor != 3 || response.StatusCode != http.StatusOK {
		t.Fatalf("the h3 GET = %v, want the block's 200", response)
	}
	if contentLength, ok := held.header["Content-Length"]; ok || len(held.transferEncoding) != 0 {
		t.Errorf("the block got the h3 GET with Content-Length %v, Transfer-Encoding %v, want no framing", contentLength, held.transferEncoding)
	}

	// a body the client didn't measure, so it declares no length
	response, err := client.Do(newE2eRequest(t, "POST", "https://api.service.example/unmeasured", io.NopCloser(strings.NewReader("0123456789"))))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	posted := e2e.awaitServiceRequests(t, "api", 2)
	if last := posted[len(posted)-1]; last.header.Get("Content-Length") != "10" || string(last.body) != "0123456789" {
		t.Errorf("the h3 POST of no declared length reached the block with Content-Length %q and body %q", last.header.Get("Content-Length"), last.body)
	}
}
