package sidecart3

// A service origin's requests served in process (scenario_sidecar.go's T3Api, as the lb routes a service's host to its
// block). The handler runs on a goroutine of its own, as a server's connection runs it, on a server-side request built
// from the client's: its url the path and query alone, its host the origin's, its RemoteAddr the documentation address
// api's requests come from, no TLS state, since the lb ends TLS in front of a block, and its body the client's own,
// read as the handler reads it and closed once the handler returns.
//
// The request's context is the client's, ended too when the handler returns, as net/http ends a served request's, and
// when the client closes the answer's body before the handler is done, as a closed connection ends it. RoundTrip
// answers once the handler fixes its status: at WriteHeader, its first Write or Flush, or its return. The body then
// reads what the handler writes through a pipe, unbuffered, so a stream reaches its client as it is written, a handler
// writing faster than its client reads waits on it as on a connection, and nothing of an answer is kept here. A
// handler that panics before it fixes its status is RoundTrip's error; after, the body's read ends with it, as a
// dropped connection's does. A client whose context ends before the answer starts gets its context's error, and the
// handler's later writes fail.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
)

// What a client that closed its answer early leaves the handler's next write.
var errServiceAnswerClosed = errors.New("the client closed the answer")

// One request of a service origin's, served by handler in process: see the file's header.
func serveServiceRequest(handler http.Handler, request *http.Request, remoteAddr string) (*http.Response, error) {
	body := request.Body
	if body == nil {
		body = http.NoBody
	}
	handlerCtx, cancel := context.WithCancel(request.Context())
	served := httptest.NewRequestWithContext(handlerCtx, request.Method, request.URL.RequestURI(), body)
	served.Header = request.Header.Clone()
	served.Host = request.URL.Host
	if request.Host != "" {
		served.Host = request.Host
	}
	served.RemoteAddr = remoteAddr
	served.ContentLength = request.ContentLength
	if body == http.NoBody {
		served.ContentLength = 0
	}
	served.TLS = nil

	writer := newT3ServiceResponseWriter(request, cancel)
	go func() {
		var panicked any
		func() {
			defer func() {
				panicked = recover()
			}()
			handler.ServeHTTP(writer, served)
		}()
		body.Close()
		writer.finish(panicked)
		cancel()
	}()
	select {
	case <-writer.started:
		return writer.response, nil
	case <-writer.failed:
		return nil, writer.failure
	case <-request.Context().Done():
		// the handler's later writes fail rather than wait on a client that left
		writer.bodyReader.CloseWithError(request.Context().Err())
		return nil, request.Context().Err()
	}
}

// A service handler's http.ResponseWriter and http.Flusher, whose answer streams to the client through a pipe. The
// handler's goroutine alone writes to it, as net/http's writer requires.
type t3ServiceResponseWriter struct {
	request    *http.Request
	cancel     context.CancelFunc
	header     http.Header
	bodyReader *io.PipeReader
	bodyWriter *io.PipeWriter
	// closed once response is set
	started chan struct{}
	// closed once failure is set, before any status was fixed
	failed chan struct{}

	stateLock sync.Mutex
	response  *http.Response
	failure   error
}

var _ http.Flusher = (*t3ServiceResponseWriter)(nil)

func newT3ServiceResponseWriter(request *http.Request, cancel context.CancelFunc) *t3ServiceResponseWriter {
	bodyReader, bodyWriter := io.Pipe()
	return &t3ServiceResponseWriter{
		request:    request,
		cancel:     cancel,
		header:     http.Header{},
		bodyReader: bodyReader,
		bodyWriter: bodyWriter,
		started:    make(chan struct{}),
		failed:     make(chan struct{}),
	}
}

func (self *t3ServiceResponseWriter) Header() http.Header {
	return self.header
}

// Fixes the status, as net/http's does: an informational status but 101 is no answer, and a second call is ignored.
// A code outside 100-999 panics, as net/http's does.
func (self *t3ServiceResponseWriter) WriteHeader(statusCode int) {
	if statusCode < 100 || 999 < statusCode {
		panic(fmt.Sprintf("invalid WriteHeader code %v", statusCode))
	}
	if statusCode < 200 && statusCode != http.StatusSwitchingProtocols {
		return
	}
	self.start(statusCode)
}

// The answer, its status and a copy of its header as they stand now, handed to RoundTrip once.
func (self *t3ServiceResponseWriter) start(statusCode int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.response != nil || self.failure != nil {
		return
	}
	header := self.header.Clone()
	contentLength := int64(-1)
	if value := header.Get("Content-Length"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && 0 <= parsed {
			contentLength = parsed
		}
	}
	self.response = &http.Response{
		Status:        fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		StatusCode:    statusCode,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          newT3ServiceBody(self.request, self.bodyReader, self.cancel),
		ContentLength: contentLength,
		Request:       self.request,
	}
	close(self.started)
}

// The status fixed, or nil before.
func (self *t3ServiceResponseWriter) statusCode() (int, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.response == nil {
		return 0, false
	}
	return self.response.StatusCode, true
}

// Writes p to the client, fixing a 200 first as net/http does, its type sniffed from p when the handler set none. A
// HEAD's body is dropped and a 204's or 304's refused, as net/http drops and refuses them.
func (self *t3ServiceResponseWriter) Write(p []byte) (int, error) {
	statusCode, ok := self.statusCode()
	if !ok {
		if self.header.Get("Content-Type") == "" && 0 < len(p) {
			self.header.Set("Content-Type", http.DetectContentType(p))
		}
		self.start(http.StatusOK)
		statusCode = http.StatusOK
	}
	switch {
	case self.request.Method == http.MethodHead:
		return len(p), nil
	case statusCode == http.StatusNoContent || statusCode == http.StatusNotModified:
		return 0, http.ErrBodyNotAllowed
	}
	return self.bodyWriter.Write(p)
}

// Fixes a 200 when nothing else has: the pipe holds nothing to flush.
func (self *t3ServiceResponseWriter) Flush() {
	self.start(http.StatusOK)
}

// The handler returned, or panicked with panicked: the answer ends, a 200 with no body for a handler that wrote
// nothing, as net/http's; a panic before the status is RoundTrip's error, and after, the body's.
func (self *t3ServiceResponseWriter) finish(panicked any) {
	if panicked == nil {
		self.start(http.StatusOK)
		self.bodyWriter.Close()
		return
	}
	err := fmt.Errorf("the service's handler panicked: %v", panicked)
	if panicked == http.ErrAbortHandler {
		err = fmt.Errorf("the service's handler aborted the answer: %w", http.ErrAbortHandler)
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.response == nil {
			self.failure = err
			close(self.failed)
		}
	}()
	self.bodyWriter.CloseWithError(err)
}

// An answer's body: the pipe's reader, which the client's context ends while it reads, and whose close before the end
// ends the handler's request, as a closed connection does.
type t3ServiceBody struct {
	reader    *io.PipeReader
	cancel    context.CancelFunc
	stopWatch func() bool
	closeOnce sync.Once
}

func newT3ServiceBody(request *http.Request, reader *io.PipeReader, cancel context.CancelFunc) *t3ServiceBody {
	ctx := request.Context()
	return &t3ServiceBody{
		reader: reader,
		cancel: cancel,
		stopWatch: context.AfterFunc(ctx, func() {
			reader.CloseWithError(ctx.Err())
		}),
	}
}

func (self *t3ServiceBody) Read(p []byte) (int, error) {
	return self.reader.Read(p)
}

func (self *t3ServiceBody) Close() error {
	self.closeOnce.Do(func() {
		self.stopWatch()
		self.reader.CloseWithError(errServiceAnswerClosed)
		self.cancel()
	})
	return nil
}
