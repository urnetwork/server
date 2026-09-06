package router

// Adjacent recovery controls use the actual router and HTTP transport. No
// service fixture, database, external endpoint or mutable global hook is used.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

// Static and capture routes preserve the exact sentinel even when the request
// was already canceled. Request cloning must not change that transport signal.
func TestRouterAbortHandlerCanceledCapturePreservesSentinel(t *testing.T) {
	for _, capture := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		requestContext, cancelRequest := context.WithCancel(ctx)
		cancelRequest()
		pattern := "/stream/leaf"
		if capture {
			pattern = "/stream/([a-z]+)"
		}
		entered := false
		router := NewRouter(ctx, []*Route{NewRoute(http.MethodGet, pattern, func(_ http.ResponseWriter, request *http.Request) {
			entered = true
			if !errors.Is(request.Context().Err(), context.Canceled) {
				t.Fatal("canceled request lost its context at route dispatch")
			}
			var want []string
			if capture {
				want = []string{"leaf"}
			}
			if !reflect.DeepEqual(GetPathValues(request), want) {
				t.Fatalf("actual capture values differ: capture=%t got=%v", capture, GetPathValues(request))
			}
			panic(http.ErrAbortHandler)
		})})
		writer := httptest.NewRecorder()
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/stream/leaf", nil).WithContext(requestContext))
		}()
		cancel()
		if !entered || recovered != http.ErrAbortHandler || writer.Body.Len() != 0 || len(writer.Header()) != 0 {
			t.Fatalf("canceled capture abort changed response ownership: capture=%t entered=%t recovered=%v body=%q", capture, entered, recovered, writer.Body.String())
		}
	}
}

// A hijacked connection belongs to the handler. The sentinel must escape
// without any attempted write, just as the unchanged Done control is consumed.
func TestRouterAbortHandlerHijackedResponsePreservesSentinel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	router := NewRouter(ctx, []*Route{NewRoute(http.MethodGet, "/stream", func(writer http.ResponseWriter, _ *http.Request) {
		if _, _, err := writer.(http.Hijacker).Hijack(); err != nil {
			t.Fatal(err)
		}
		panic(http.ErrAbortHandler)
	})})
	writer := newHijackedResponseWriter()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/stream", nil))
	}()
	if recovered != http.ErrAbortHandler || !writer.hijacked || writer.writesAfterHijack != 0 {
		t.Fatalf("hijacked abort ownership changed: recovered=%v hijacked=%t writes=%d", recovered, writer.hijacked, writer.writesAfterHijack)
	}
}

// net/http recognizes identity, not matching text or errors.Is. Preserve the
// existing generic failure response for every non-identical panic below.
func TestRouterAbortHandlerNearSentinelsRemainGenericFailures(t *testing.T) {
	for _, value := range []any{
		errors.New(http.ErrAbortHandler.Error()),
		fmt.Errorf("wrapped transport signal: %w", http.ErrAbortHandler),
		errors.Join(http.ErrAbortHandler, errors.New("separate failure")),
		http.ErrAbortHandler.Error(),
	} {
		ctx, cancel := context.WithCancel(t.Context())
		router := NewRouter(ctx, []*Route{NewRoute(http.MethodGet, "/stream", func(http.ResponseWriter, *http.Request) {
			panic(value)
		})})
		writer := httptest.NewRecorder()
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/stream", nil))
		}()
		cancel()
		if recovered != nil || writer.Code != http.StatusInternalServerError ||
			writer.Body.String() != "Error. Please email support@ur.io for help.\n" {
			t.Fatalf("non-identical abort panic changed generic recovery: type=%T recovered=%v status=%d body=%q", value, recovered, writer.Code, writer.Body.String())
		}
	}
}

// A writer may itself discover a transport failure while generic recovery is
// trying to send its error. The inner fallback must not consume that sentinel.
type routerAbortFaultWriter struct {
	header     http.Header
	stage      string
	calls      []string
	aborted    bool
	afterAbort int
}

// Every writer boundary records before raising the exact standard sentinel.
func (self *routerAbortFaultWriter) enter(stage string) {
	if self.aborted {
		self.afterAbort++
	}
	self.calls = append(self.calls, stage)
	if stage == self.stage {
		self.aborted = true
		panic(http.ErrAbortHandler)
	}
}

// No real connection is involved in the nested recovery boundary control.
func (self *routerAbortFaultWriter) Header() http.Header {
	self.enter("header")
	return self.header
}

// Status commitment is observable separately from accepting body bytes.
func (self *routerAbortFaultWriter) WriteHeader(int) {
	self.enter("status")
}

// The write boundary refuses before accepting any generic-error body.
func (self *routerAbortFaultWriter) Write(value []byte) (int, error) {
	self.enter("write")
	return len(value), nil
}

// The existing inner defer recover() is not a direct deferred closure around
// this new writer panic. This control pins its actual behavior, not its comment.
func TestRouterAbortHandlerFromErrorWriterEscapes(t *testing.T) {
	for index, stage := range []string{"header", "status", "write"} {
		ctx, cancel := context.WithCancel(t.Context())
		router := NewRouter(ctx, []*Route{NewRoute(http.MethodGet, "/stream", func(http.ResponseWriter, *http.Request) {
			panic(errors.New("controlled generic route failure"))
		})})
		writer := &routerAbortFaultWriter{header: make(http.Header), stage: stage}
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/stream", nil))
		}()
		cancel()
		if recovered != http.ErrAbortHandler || !writer.aborted || writer.afterAbort != 0 ||
			!reflect.DeepEqual(writer.calls, []string{"header", "status", "write"}[:index+1]) {
			t.Fatalf("nested error-writer abort changed ownership: stage=%s recovered=%v calls=%v after=%d", stage, recovered, writer.calls, writer.afterAbort)
		}
	}
}

// A canceled request alone is not an abort panic. Ordinary handler responses
// and the static fast path remain unchanged by the sentinel-specific repair.
func TestRouterAbortHandlerNormalCanceledResponseRemainsUntouched(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	requestContext, cancelRequest := context.WithCancel(ctx)
	cancelRequest()
	router := NewRouter(ctx, []*Route{NewRoute(http.MethodGet, "/stream", func(writer http.ResponseWriter, request *http.Request) {
		if !errors.Is(request.Context().Err(), context.Canceled) {
			t.Fatal("normal canceled request lost its context")
		}
		writer.Header().Set("X-Owned", "normal")
		writer.WriteHeader(http.StatusAccepted)
		if _, err := io.WriteString(writer, "normal-owned-response"); err != nil {
			t.Fatal(err)
		}
	})})
	writer := httptest.NewRecorder()
	router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(requestContext))
	if writer.Code != http.StatusAccepted || writer.Header().Get("X-Owned") != "normal" || writer.Body.String() != "normal-owned-response" {
		t.Fatalf("ordinary canceled response changed: status=%d headers=%v body=%q", writer.Code, writer.Header(), writer.Body.String())
	}
}

// HTTP/2 has a stream reset instead of HTTP/1's incomplete chunked EOF. The
// exact prefix must arrive before the abort, and no generic tail may complete it.
func TestRouterAbortHandlerTerminatesFlushedHTTP2Response(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("HTTP/2 abort control requires the original test deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	const prefix = "authenticated-http2-prefix\n"
	abort, flushed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handlerErrors := make(chan error, 1)
	var abortOnce sync.Once
	release := func() { abortOnce.Do(func() { close(abort) }) }
	router := NewRouter(ctx, []*Route{NewRoute(http.MethodGet, "/stream", func(writer http.ResponseWriter, request *http.Request) {
		defer close(done)
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.WriteHeader(http.StatusOK)
		if count, err := io.WriteString(writer, prefix); err != nil || count != len(prefix) {
			handlerErrors <- errors.Join(err, io.ErrShortWrite)
			return
		}
		if err := http.NewResponseController(writer).Flush(); err != nil {
			handlerErrors <- err
			return
		}
		close(flushed)
		select {
		case <-abort:
		case <-request.Context().Done():
			handlerErrors <- request.Context().Err()
			return
		}
		panic(http.ErrAbortHandler)
	})})
	httpServer := httptest.NewUnstartedServer(router)
	httpServer.EnableHTTP2 = true
	httpServer.StartTLS()
	defer httpServer.Close()
	defer release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatalf("HTTP/2 abort failed before its prefix: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ProtoMajor != 2 {
		t.Fatalf("HTTP/2 stream was not negotiated: status=%d protocol=%s", response.StatusCode, response.Proto)
	}
	observed := make([]byte, len(prefix))
	if _, err := io.ReadFull(response.Body, observed); err != nil || !bytes.Equal(observed, []byte(prefix)) {
		t.Fatalf("HTTP/2 exact prefix did not arrive: prefix=%q error=%v", observed, err)
	}
	select {
	case <-flushed:
	case <-ctx.Done():
		t.Fatal("HTTP/2 stream did not reach its flush barrier")
	}
	release()
	tail, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("HTTP/2 abort handler did not join")
	}
	select {
	case handlerErr := <-handlerErrors:
		t.Fatalf("HTTP/2 handler failed before its exact sentinel: %v", handlerErr)
	default:
	}
	if readErr == nil || errors.Is(readErr, io.EOF) || ctx.Err() != nil || len(tail) != 0 {
		t.Fatalf("HTTP/2 abort completed or appended a response: error=%v context=%v tail=%q", readErr, ctx.Err(), tail)
	}
}
