// Native HTTP request bodies, not only io.Pipe, exercise read cancellation.
// The real router, JWT state and server transports remain in the call path.
package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// Bounded in-memory body controls expose the native capability explicitly.
// Real transport tests below forward it to the actual HTTP response writer.
type snAttemptUploadRecorder struct {
	*httptest.ResponseRecorder
	deadline func(time.Time) error
}

func snAttemptUploadTestRecorder() *snAttemptUploadRecorder {
	return &snAttemptUploadRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (self *snAttemptUploadRecorder) SetReadDeadline(deadline time.Time) error {
	if self.deadline != nil {
		return self.deadline(deadline)
	}
	return nil
}

// A conforming wrapper must preserve the original controller capability.
type snAttemptUploadUnwrapper struct {
	http.ResponseWriter
	next http.ResponseWriter
}

func (self *snAttemptUploadUnwrapper) Unwrap() http.ResponseWriter { return self.next }

// Count the actual native call, not a synthesized body-unblock callback.
type snAttemptUploadObservedWriter struct {
	http.ResponseWriter
	interrupts *atomic.Int32
}

func (self *snAttemptUploadObservedWriter) SetReadDeadline(deadline time.Time) error {
	self.interrupts.Add(1)
	return http.NewResponseController(self.ResponseWriter).SetReadDeadline(deadline)
}

// The witness runs inside the actual socket Read called while net/http's
// request body owns its mutex, after the partial buffered byte is exhausted.
type snAttemptUploadReadConn struct {
	net.Conn
	read func()
}

func (self *snAttemptUploadReadConn) Read(data []byte) (int, error) {
	self.read()
	return self.Conn.Read(data)
}

type snAttemptUploadReadListener struct {
	net.Listener
	read func()
}

func (self *snAttemptUploadReadListener) Accept() (net.Conn, error) {
	connection, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &snAttemptUploadReadConn{Conn: connection, read: self.read}, nil
}

// Cancellation must interrupt the real socket before Close takes the body's
// mutex. On original code, the Close-entry witness deterministically records
// zero native interrupts; only then is the peer closed so every owner joins.
func TestSnAttemptUploadRealHTTP1CancellationInterruptsBeforeClose(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		entered, closing := make(chan struct{}), make(chan struct{})
		owner := make(chan context.CancelFunc, 1)
		finished := make(chan struct{})
		slots := make(chan struct{}, 1)
		var interrupts, closes, stores atomic.Int32
		var readingBody atomic.Bool
		var readOnce sync.Once
		handler := func(w http.ResponseWriter, r *http.Request) {
			defer close(finished)
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			owner <- cancel
			original := r.Body
			var closeOnce sync.Once
			request := r.WithContext(ctx)
			request.Body = &snAttemptTestReadCloser{
				Reader: snAttemptTestReadFunc(func(value []byte) (int, error) { readingBody.Store(true); return original.Read(value) }),
				close:  func() error { closes.Add(1); closeOnce.Do(func() { close(closing) }); return original.Close() },
			}
			observed := &snAttemptUploadObservedWriter{ResponseWriter: w, interrupts: &interrupts}
			wrapped := &snAttemptUploadUnwrapper{ResponseWriter: observed, next: observed}
			serveSnUploadAttemptArtifact(wrapped, request,
				func() (server.BlobStore, bool) { stores.Add(1); return nil, false },
				func(context.Context, server.Id, uint64) error { return nil }, snAttemptTestBounds(), slots)
		}
		endpoint := httptest.NewUnstartedServer(router.NewRouter(tb.Context(), []*router.Route{router.NewRoute(http.MethodPost, "/sn/attempt-artifact", handler)}))
		endpoint.Listener = &snAttemptUploadReadListener{Listener: endpoint.Listener, read: func() {
			if readingBody.Load() {
				readOnce.Do(func() { close(entered) })
			}
		}}
		endpoint.Start()
		defer endpoint.Close()
		connection, err := (&net.Dialer{}).DialContext(tb.Context(), "tcp", endpoint.Listener.Addr().String())
		if err != nil {
			tb.Fatal(err)
		}
		defer connection.Close()
		request := snAttemptUploadTestRequest(tb, client.Sign(), "metadata", []byte("data"))
		// Only one of the four declared bytes is sent; the peer stays open.
		if _, err := fmt.Fprintf(connection, "POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: %s\r\nContent-Type: application/json\r\nContent-Length: 4\r\n\r\nd", request.URL.RequestURI(), endpoint.Listener.Addr().String(), request.Header.Get("Authorization")); err != nil {
			tb.Fatal(err)
		}
		cancel := <-owner
		defer cancel()
		select {
		case <-entered:
		case <-finished:
			select {
			case <-entered:
			default:
				tb.Fatal("real HTTP/1 upload ended before its native body read")
			}
		}
		cancel()
		select {
		case <-closing:
		case <-finished:
			select {
			case <-closing:
			default:
				tb.Fatal("real HTTP/1 upload returned without joining body Close")
			}
		}
		beforePeerClose := interrupts.Load()
		_ = connection.Close()
		<-finished
		if beforePeerClose != 1 {
			tb.Fatalf("canceled real HTTP/1 body reached Close without native interruption: %d", beforePeerClose)
		}
		if closes.Load() != 1 || stores.Load() != 0 || len(slots) != 0 {
			tb.Fatal("canceled real HTTP/1 upload retained body, store or slot ownership")
		}
	})
}

// HTTP/2 deadlines apply to one request stream, never a healthy sibling on
// the same connection. A partial upload cannot acknowledge or poison its peer.
func TestSnAttemptUploadRealHTTP2CancellationKeepsPeerConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, identity := snAttemptUploadTestIdentity(tb)
		entered, closing := make(chan struct{}), make(chan struct{})
		owner := make(chan context.CancelFunc, 1)
		finished := make(chan struct{})
		type observedRequest struct {
			protocol int
			remote   string
		}
		observed := make(chan observedRequest, 2)
		slots := make(chan struct{}, 1)
		var interrupts, closes, stores atomic.Int32
		upload := func(w http.ResponseWriter, r *http.Request) {
			defer close(finished)
			observed <- observedRequest{protocol: r.ProtoMajor, remote: r.RemoteAddr}
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			owner <- cancel
			var readOnce, closeOnce sync.Once
			original := r.Body
			request := r.WithContext(ctx)
			request.Body = &snAttemptTestReadCloser{
				Reader: snAttemptTestReadFunc(func(value []byte) (int, error) { readOnce.Do(func() { close(entered) }); return original.Read(value) }),
				close:  func() error { closes.Add(1); closeOnce.Do(func() { close(closing) }); return original.Close() },
			}
			serveSnUploadAttemptArtifact(&snAttemptUploadObservedWriter{ResponseWriter: w, interrupts: &interrupts}, request,
				func() (server.BlobStore, bool) { stores.Add(1); return nil, false },
				func(context.Context, server.Id, uint64) error { return nil }, snAttemptTestBounds(), slots)
		}
		healthy := func(w http.ResponseWriter, r *http.Request) {
			observed <- observedRequest{protocol: r.ProtoMajor, remote: r.RemoteAddr}
			w.WriteHeader(http.StatusNoContent)
		}
		endpoint := httptest.NewUnstartedServer(router.NewRouter(tb.Context(), []*router.Route{
			router.NewRoute(http.MethodPost, "/sn/attempt-artifact", upload),
			router.NewRoute(http.MethodGet, "/healthy", healthy),
		}))
		endpoint.EnableHTTP2 = true
		endpoint.StartTLS()
		defer endpoint.Close()
		client := endpoint.Client()
		defer client.CloseIdleConnections()
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		request := snAttemptUploadTestRequest(tb, identity.Sign(), "metadata", []byte("data"))
		request.URL.Scheme, request.URL.Host = "https", endpoint.Listener.Addr().String()
		request.RequestURI = ""
		request.Body = reader
		requestCtx, cancelRequest := context.WithCancel(tb.Context())
		defer cancelRequest()
		request = request.WithContext(requestCtx)
		type result struct {
			response *http.Response
			err      error
		}
		done := make(chan result, 1)
		go func() { response, err := client.Do(request); done <- result{response: response, err: err} }()
		var outcome result
		var joinOnce sync.Once
		joinRequest := func() {
			joinOnce.Do(func() {
				cancelRequest()
				_ = writer.CloseWithError(context.Canceled)
				outcome = <-done
				if outcome.response != nil && outcome.response.Body != nil {
					_ = outcome.response.Body.Close()
				}
			})
		}
		defer joinRequest()
		cancel := <-owner
		defer cancel()
		select {
		case <-entered:
		case <-finished:
			select {
			case <-entered:
			default:
				tb.Fatal("real HTTP/2 upload ended before its body read")
			}
		}
		cancel()
		select {
		case <-closing:
		case <-finished:
			select {
			case <-closing:
			default:
				tb.Fatal("real HTTP/2 upload returned without joining body Close")
			}
		}
		native := interrupts.Load()
		joinRequest()
		<-finished
		first := <-observed
		peer, err := client.Get(endpoint.URL + "/healthy")
		if err != nil {
			tb.Fatal(err)
		}
		closeErr := peer.Body.Close()
		second := <-observed
		if native != 1 || first.protocol != 2 || second.protocol != 2 || first.remote != second.remote || peer.StatusCode != http.StatusNoContent || closeErr != nil {
			tb.Fatalf("HTTP/2 cancellation did not retain native stream isolation: native%d protocol%d/%d same-connection%t status%d close%v", native, first.protocol, second.protocol, first.remote == second.remote, peer.StatusCode, closeErr)
		}
		if outcome.err == nil && outcome.response != nil && outcome.response.StatusCode == http.StatusNoContent {
			tb.Fatal("canceled HTTP/2 upload acknowledged success")
		}
		if closes.Load() != 1 || stores.Load() != 0 || len(slots) != 0 {
			tb.Fatal("canceled HTTP/2 upload retained ownership")
		}
	})
}

// Opaque/cyclic wrappers refuse before quota, while native and Unwrap writers
// admit exact bytes without extending the server's earlier read deadline.
func TestSnAttemptUploadNativeDeadlineCapabilityPrecedesQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		for _, fault := range []string{"native", "unwrap", "opaque", "cycle", "depth"} {
			response := snAttemptUploadTestRecorder()
			deadlines, reads, reservations, stores := 0, 0, 0, 0
			response.deadline = func(time.Time) error { deadlines++; return nil }
			var owned http.ResponseWriter = response
			switch fault {
			case "unwrap":
				owned = &snAttemptUploadUnwrapper{ResponseWriter: response, next: response}
			case "opaque":
				owned = struct{ http.ResponseWriter }{ResponseWriter: response}
			case "cycle":
				cycle := &snAttemptUploadUnwrapper{ResponseWriter: response}
				cycle.next = cycle
				owned = cycle
			case "depth":
				for range 8 {
					owned = &snAttemptUploadUnwrapper{ResponseWriter: owned, next: owned}
				}
			}
			data := []byte("data")
			body := bytes.NewReader(data)
			request := snAttemptUploadTestRequest(tb, client.Sign(), "metadata", data)
			request.Body = &snAttemptTestReadCloser{Reader: snAttemptTestReadFunc(func(value []byte) (int, error) { reads++; return body.Read(value) })}
			store := server.NewLocalBlobStore(tb.TempDir(), "attempt-upload")
			serveSnUploadAttemptArtifact(owned, request,
				func() (server.BlobStore, bool) { stores++; return store, true },
				func(context.Context, server.Id, uint64) error { reservations++; return nil }, snAttemptTestBounds(), make(chan struct{}, 1))
			if fault == "native" || fault == "unwrap" {
				if response.Code != http.StatusNoContent || reads == 0 || reservations != 1 || stores != 1 || deadlines != 0 {
					tb.Fatalf("%s native ownership prerequisite differs", fault)
				}
			} else if response.Code != http.StatusServiceUnavailable || reads != 0 || reservations != 0 || stores != 0 || deadlines != 0 {
				tb.Fatalf("%s unsupported writer reached quota or body: status%d read%d quota%d store%d", fault, response.Code, reads, reservations, stores)
			}
		}
	})
}

// An interrupt error is retained, not flattened into cancellation or hidden
// behind an acquired body. Close still runs exactly once before slot release.
func TestSnAttemptUploadNativeDeadlineFailureStillJoinsClose(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		ctx, cancel := context.WithCancel(tb.Context())
		defer cancel()
		cause := errors.New("actual native upload read interruption failed")
		var deadlines, closes atomic.Int32
		response := snAttemptUploadTestRecorder()
		response.deadline = func(time.Time) error { deadlines.Add(1); return cause }
		request := snAttemptUploadTestRequest(tb, client.Sign(), "metadata", []byte("data")).WithContext(ctx)
		data := bytes.NewReader([]byte("data"))
		request.Body = &snAttemptTestReadCloser{
			Reader: snAttemptTestReadFunc(func(value []byte) (int, error) { cancel(); return data.Read(value) }),
			close:  func() error { closes.Add(1); return nil },
		}
		stores := 0
		slots := make(chan struct{}, 1)
		serveSnUploadAttemptArtifact(response, request,
			func() (server.BlobStore, bool) { stores++; return nil, false },
			func(context.Context, server.Id, uint64) error { return nil }, snAttemptTestBounds(), slots)
		if response.Code != http.StatusRequestTimeout || !strings.Contains(response.Body.String(), cause.Error()) || deadlines.Load() != 1 || closes.Load() != 1 || stores != 0 || len(slots) != 0 {
			tb.Fatalf("native interruption failure escaped joined Close: status%d deadline%d close%d store%d", response.Code, deadlines.Load(), closes.Load(), stores)
		}
	})
}
