package router

// These controls exercise actual Router.ServeHTTP recovery and net/http's
// already-flushed response boundary. No service, database or external endpoint
// is used; the only listener is the test-owned httptest loopback server.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The exact standard-library sentinel must reach net/http unchanged. An error
// with the same text, or an errors.Is wrapper, is not that transport signal.
func TestRouterAbortHandlerPreservesExactSentinel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := false
	router := NewRouter(ctx, []*Route{NewRoute(http.MethodGet, "/stream", func(http.ResponseWriter, *http.Request) {
		entered = true
		panic(http.ErrAbortHandler)
	})})
	writer := httptest.NewRecorder()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/stream", nil))
	}()
	if !entered {
		t.Fatal("abort fixture never reached its actual route")
	}
	if recovered != http.ErrAbortHandler {
		t.Fatalf("router swallowed exact http.ErrAbortHandler: recovered=%T status=%d body=%q",
			recovered, writer.Code, writer.Body.String())
	}
	if writer.Body.Len() != 0 || len(writer.Header()) != 0 {
		t.Fatalf("abort recovery mutated an unstarted response: headers=%v body=%q", writer.Header(), writer.Body.String())
	}
}

// A real client first receives the complete flushed prefix. Only then does a
// barrier release the panic. A clean EOF plus an appended generic error body
// is the causal defect, not a missing prefix, refused request or setup timeout.
func TestRouterAbortHandlerTerminatesFlushedResponse(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("flushed abort control requires the original test deadline")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	const prefix = "authenticated-stream-prefix\n"
	flushed := make(chan struct{})
	abort := make(chan struct{})
	done := make(chan struct{})
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
	httpServer := httptest.NewServer(router)
	defer httpServer.Close()
	defer release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatalf("flushed abort request failed before its prefix: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ProtoMajor != 1 {
		t.Fatalf("flushed abort fixture did not start its HTTP/1 stream: status=%d protocol=%s", response.StatusCode, response.Proto)
	}
	observed := make([]byte, len(prefix))
	if _, err := io.ReadFull(response.Body, observed); err != nil || !bytes.Equal(observed, []byte(prefix)) {
		t.Fatalf("flushed abort fixture did not deliver its exact prefix: prefix=%q error=%v", observed, err)
	}
	select {
	case <-flushed:
	case <-ctx.Done():
		t.Fatal("flushed abort fixture did not reach its explicit barrier")
	}
	release()
	tail, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("flushed abort handler did not join before the original deadline")
	}
	select {
	case handlerErr := <-handlerErrors:
		t.Fatalf("flushed abort fixture failed before its sentinel: %v", handlerErr)
	default:
	}
	if !errors.Is(readErr, io.ErrUnexpectedEOF) || len(tail) != 0 {
		t.Fatalf("router converted aborted flushed response into completed HTTP body: read_error=%v tail=%q", readErr, tail)
	}
}
