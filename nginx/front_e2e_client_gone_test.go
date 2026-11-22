package nginx

// End-to-end tests of a forwarded request whose client goes away: nginx
// terminates such a request with nothing sent, so the front sends nothing,
// and records the request with no status, never as a success or one of its
// own 502s.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/quic-go"
	"github.com/urnetwork/quic-go/http3"
)

// The front's request count of the api service with no block, by protocol
// and status class.
func e2eApiRequestCount(protocol string, statusClass string) float64 {
	return testutil.ToFloat64(defaultFrontMetrics.requests.WithLabelValues("api", protocol, statusClass, routeHttpNoLabel))
}

// An h1 client that closes its side with its declared body 90 bytes short
// gets no response, as nginx terminates the request whose connection closed
// under its body (c->error), where the front answered the 200 of a handler
// that wrote nothing. No block sees the request, the refusal is counted as
// canceled, and the request as answered with no status.
func TestFrontE2eClientGoneMidBodyGetsNoResponse(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	beforeNone := e2eApiRequestCount("h1", "none")
	before2xx := e2eApiRequestCount("h1", "2xx")
	beforeCanceled := bodyRefusalCount("api", bodyRefusalCanceled)

	ctx, cancel := context.WithTimeout(t.Context(), e2eWaitTimeout)
	defer cancel()
	tlsConn, err := e2e.dialTls(ctx, eth1.httpsAddr, "api.service.example", []string{"http/1.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer tlsConn.Close()
	tlsConn.SetDeadline(time.Now().Add(e2eWaitTimeout))
	if _, err := io.WriteString(tlsConn, "POST /half-closed-body HTTP/1.1\r\nHost: api.service.example\r\nContent-Length: 100\r\n\r\n0123456789"); err != nil {
		t.Fatal(err)
	}
	if err := tlsConn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if response, err := http.ReadResponse(bufio.NewReader(tlsConn), nil); err == nil {
		t.Errorf("a client that closed its side mid-body got %d, want no response", response.StatusCode)
	} else if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("the connection ended with %v, want its end", err)
	}
	eth1.httpsServed.await(t, "/half-closed-body")

	if count := e2eApiRequestCount("h1", "none") - beforeNone; count != 1 {
		t.Errorf("%v requests recorded with no status, want 1", count)
	}
	if count := e2eApiRequestCount("h1", "2xx") - before2xx; count != 0 {
		t.Errorf("%v requests recorded as 2xx, want none", count)
	}
	if count := bodyRefusalCount("api", bodyRefusalCanceled) - beforeCanceled; count != 1 {
		t.Errorf("%v canceled refusals, want 1", count)
	}
	if requests := e2e.serviceRequests("api"); len(requests) != 0 {
		t.Errorf("%d requests reached a block, want none", len(requests))
	}
}

// An h2 client that resets its stream while the front holds part of its
// body is recorded with no status, not as the 200 of a handler that wrote
// nothing. The barrier is the body's spill file: the front creates it once
// the body has passed its memory prefix.
func TestFrontE2eClientResetMidBodyIsRecordedWithNoStatus(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	store := e2e.store(t)
	createSpillFile := store.createSpillFile
	spilling := make(chan struct{}, 1)
	// set before any request reaches the store
	store.createSpillFile = func() (bodySpillFile, error) {
		select {
		case spilling <- struct{}{}:
		default:
		}
		return createSpillFile()
	}
	beforeNone := e2eApiRequestCount("h2", "none")
	before2xx := e2eApiRequestCount("h2", "2xx")
	beforeCanceled := bodyRefusalCount("api", bodyRefusalCanceled)

	clientConn, _ := e2e.newH2ClientConn(t, eth1.httpsAddr, "api.service.example")
	bodyReader, bodyWriter := io.Pipe()
	defer bodyWriter.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "POST", "https://api.service.example/reset-body", bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	prefixBytes := e2e.settings.Body.MemoryPrefixBytes
	request.ContentLength = 2 * prefixBytes
	roundTripErrs := make(chan error, 1)
	go func() {
		response, err := clientConn.RoundTrip(request)
		if err == nil {
			response.Body.Close()
		}
		roundTripErrs <- err
	}()
	go bodyWriter.Write(e2eBody(int(prefixBytes) + 10))
	receiveE2e(t, spilling, "the spill of the body's first bytes past its prefix")
	// RST_STREAM(CANCEL)
	cancel()
	if err := receiveE2e(t, roundTripErrs, "the round trip's end"); err == nil {
		t.Errorf("the reset request got a response")
	}
	eth1.httpsServed.await(t, "/reset-body")

	if count := e2eApiRequestCount("h2", "none") - beforeNone; count != 1 {
		t.Errorf("%v requests recorded with no status, want 1", count)
	}
	if count := e2eApiRequestCount("h2", "2xx") - before2xx; count != 0 {
		t.Errorf("%v requests recorded as 2xx, want none", count)
	}
	if count := bodyRefusalCount("api", bodyRefusalCanceled) - beforeCanceled; count != 1 {
		t.Errorf("%v canceled refusals, want 1", count)
	}
	if requests := e2e.serviceRequests("api"); len(requests) != 0 {
		t.Errorf("%d requests reached a block, want none", len(requests))
	}
}

// Regression test: an h3 client that resets its request's side while the
// front reads its body, the response's side left open, gets no response,
// as a client gone mid-body gets on h1 and h2. Its body is refused as
// canceled and the request recorded with no status, on a buffered route,
// which no block sees, and on a streamed one, whose block gets the body cut
// short. The quic-go fork's http3 fails the read with the client's reset and
// never ends the request's context for it, so the front answered a 400 and
// counted an incomplete body. The barriers: on the buffered route the body's
// spill file, which the front creates once the body has passed its memory
// prefix; on the streamed route the block's read of the first bytes.
func TestFrontE2eH3ClientResetMidBodyGetsNoResponse(t *testing.T) {
	// the api's /stream/ paths stream, as the api's streamable paths do
	e2e := newE2eFront(t, nil, func(routeTable *RouteTable) {
		for _, service := range routeTable.Services {
			if service.Name == "api" {
				service.StreamablePaths = []string{"/stream/.*"}
			}
		}
	})
	h3ApiServer := newE2eH3ApiServer(t, e2e)
	store := e2e.store(t)
	createSpillFile := store.createSpillFile
	spilling := make(chan struct{}, 1)
	// set before any request reaches the store
	store.createSpillFile = func() (bodySpillFile, error) {
		select {
		case spilling <- struct{}{}:
		default:
		}
		return createSpillFile()
	}
	// the blocks read a streamed body as it arrives, and keep its end
	blockReads := make(chan struct{}, 1)
	blockBodyErrs := make(chan error, 1)
	for _, block := range []string{"g1", "g2"} {
		e2e.upstream(t, "api", block).setStreamHandler(func(w http.ResponseWriter, r *http.Request) {
			buffer := make([]byte, 4096)
			first := true
			for {
				n, err := r.Body.Read(buffer)
				if 0 < n && first {
					first = false
					blockReads <- struct{}{}
				}
				if err != nil {
					blockBodyErrs <- err
					return
				}
			}
		})
	}

	prefixBytes := e2e.settings.Body.MemoryPrefixBytes
	for _, c := range []struct {
		path           string
		declaredLength int64
		sent           []byte
		// waits until the front is reading the body, past what was sent
		barrier  func()
		streamed bool
	}{
		{
			path:           "/reset-buffered",
			declaredLength: 2 * prefixBytes,
			sent:           e2eBody(int(prefixBytes) + 10),
			barrier: func() {
				receiveE2e(t, spilling, "the spill of the body's first bytes past its prefix")
			},
		},
		{
			path:           "/stream/reset",
			declaredLength: 100,
			sent:           []byte("0123456789"),
			barrier: func() {
				receiveE2e(t, blockReads, "the block's read of the body's first bytes")
			},
			streamed: true,
		},
	} {
		beforeCanceled := bodyRefusalCount("api", bodyRefusalCanceled)
		beforeIncomplete := bodyRefusalCount("api", bodyRefusalIncomplete)
		beforeNone := e2eApiRequestCount("h3", "none")
		before4xx := e2eApiRequestCount("h3", "4xx")
		beforeForwardErrors := forwardErrorCount("api", http.StatusBadGateway)
		requestCounts := e2e.requestCounts()

		str := h3ApiServer.openRequestStream(t, e2e)
		// bounds a broken test, never a passing one
		str.SetDeadline(time.Now().Add(e2eWaitTimeout))
		request := newE2eRequest(t, "POST", "https://api.service.example"+c.path, http.NoBody)
		// the declared length goes in the head; the body goes as DATA frames
		request.ContentLength = c.declaredLength
		if err := str.SendRequestHeader(request); err != nil {
			t.Fatal(err)
		}
		if _, err := str.Write(c.sent); err != nil {
			t.Fatal(err)
		}
		c.barrier()
		// RESET_STREAM for the request's side alone: no STOP_SENDING, so the
		// response's side, and the request's context, stay open
		str.CancelWrite(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
		if response, err := str.ReadResponse(); err == nil {
			t.Errorf("%s: a client that reset its request's side got %d, want no response", c.path, response.StatusCode)
		}
		h3ApiServer.servedLog.await(t, c.path)

		if count := bodyRefusalCount("api", bodyRefusalCanceled) - beforeCanceled; count != 1 {
			t.Errorf("%s: %v canceled refusals, want 1", c.path, count)
		}
		if count := bodyRefusalCount("api", bodyRefusalIncomplete) - beforeIncomplete; count != 0 {
			t.Errorf("%s: %v incomplete refusals, want none", c.path, count)
		}
		if count := e2eApiRequestCount("h3", "none") - beforeNone; count != 1 {
			t.Errorf("%s: %v requests recorded with no status, want 1", c.path, count)
		}
		if count := e2eApiRequestCount("h3", "4xx") - before4xx; count != 0 {
			t.Errorf("%s: %v requests recorded as 4xx, want none", c.path, count)
		}
		if count := forwardErrorCount("api", http.StatusBadGateway) - beforeForwardErrors; count != 0 {
			t.Errorf("%s: %v of the front's own 502s, want none", c.path, count)
		}
		requests := e2e.requestsSince(requestCounts)
		if !c.streamed && len(requests) != 0 {
			t.Errorf("%s: the reset body reached a block: %d requests", c.path, len(requests))
		}
		if c.streamed {
			if len(requests) != 1 {
				t.Errorf("%s: %d requests reached a block, want the streamed head once", c.path, len(requests))
			}
			if err := receiveE2e(t, blockBodyErrs, "the block's end of the body"); err == nil {
				t.Errorf("%s: the block read the reset body whole", c.path)
			}
		}
		if memoryBytes, spillBytes, spillFiles, bodies := e2e.bodyUsage(t); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
			t.Errorf("%s: the store holds %d bytes, %d spilled in %d files, for %d bodies, want nothing", c.path, memoryBytes, spillBytes, spillFiles, bodies)
		}
	}
}

// A client that goes away while its block holds the answer, nginx's 499,
// gets nothing and is recorded with no status, over h1, where it closes its
// side, and over h2, where it resets its stream; never as a 5xx, and never
// as one of the front's own 502s.
func TestFrontE2eClientGoneWhileTheBlockHoldsTheAnswerIsRecordedWithNoStatus(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	hold := newE2eHold(t)
	e2e.setServiceHandler(t, "api", func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
		hold.wait(r)
		io.WriteString(w, "late")
	})

	cases := []struct {
		protocol string
		path     string
		// sends the request, then goes away once a block has it; returns
		// whether a response came back
		send func(t *testing.T, path string, requestCount int) bool
	}{
		{
			protocol: "h1",
			path:     "/gone-while-held-h1",
			send: func(t *testing.T, path string, requestCount int) bool {
				ctx, cancel := context.WithTimeout(t.Context(), e2eWaitTimeout)
				defer cancel()
				tlsConn, err := e2e.dialTls(ctx, eth1.httpsAddr, "api.service.example", []string{"http/1.1"})
				if err != nil {
					t.Fatal(err)
				}
				defer tlsConn.Close()
				tlsConn.SetDeadline(time.Now().Add(e2eWaitTimeout))
				if _, err := io.WriteString(tlsConn, "GET "+path+" HTTP/1.1\r\nHost: api.service.example\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				e2e.awaitServiceRequests(t, "api", requestCount)
				tlsConn.CloseWrite()
				_, err = http.ReadResponse(bufio.NewReader(tlsConn), nil)
				return err == nil
			},
		},
		{
			protocol: "h2",
			path:     "/gone-while-held-h2",
			send: func(t *testing.T, path string, requestCount int) bool {
				clientConn, _ := e2e.newH2ClientConn(t, eth1.httpsAddr, "api.service.example")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, "GET", "https://api.service.example"+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				roundTripErrs := make(chan error, 1)
				go func() {
					response, err := clientConn.RoundTrip(request)
					if err == nil {
						response.Body.Close()
					}
					roundTripErrs <- err
				}()
				e2e.awaitServiceRequests(t, "api", requestCount)
				// RST_STREAM(CANCEL)
				cancel()
				return receiveE2e(t, roundTripErrs, "the round trip's end") == nil
			},
		},
	}
	for i, c := range cases {
		beforeNone := e2eApiRequestCount(c.protocol, "none")
		before5xx := e2eApiRequestCount(c.protocol, "5xx")
		beforeForwardErrors := forwardErrorCount("api", http.StatusBadGateway)
		if c.send(t, c.path, i+1) {
			t.Errorf("%s: a client gone while its block held the answer got a response", c.protocol)
		}
		eth1.httpsServed.await(t, c.path)
		if count := e2eApiRequestCount(c.protocol, "none") - beforeNone; count != 1 {
			t.Errorf("%s: %v requests recorded with no status, want 1", c.protocol, count)
		}
		if count := e2eApiRequestCount(c.protocol, "5xx") - before5xx; count != 0 {
			t.Errorf("%s: %v requests recorded as 5xx, want none", c.protocol, count)
		}
		if count := forwardErrorCount("api", http.StatusBadGateway) - beforeForwardErrors; count != 0 {
			t.Errorf("%s: %v of the front's own 502s counted, want none", c.protocol, count)
		}
	}
}
