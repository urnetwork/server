package nginx

// Streamed request bodies (owner: s-forward): on a streamed route the
// client's body is read once, straight into the upstream request, as nginx's
// `proxy_request_buffering off` reads it. Nothing is held, so the memory
// budget and the spill caps don't apply, but the body limit and
// client_body_timeout between reads do. A body that began going upstream
// can't be replayed, so it ends the request's retries (D7).

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// A streamed body. Read by one goroutine, the upstream request's writer;
// interrupt and the status methods are safe from any goroutine.
type forwardStreamedBody struct {
	ctx  context.Context
	body io.Reader
	// for a body of unknown length, BodySettings.MaxBodyBytes; non-positive
	// means no limit. A declared length is checked before forwarding.
	maxBytes        int64
	readTimeout     time.Duration
	setReadDeadline func(time.Time) error
	readBytes       int64
	beganForwarding atomic.Bool

	stateLock   sync.Mutex
	refusal     *bodyRefusal
	interrupted bool
}

func newForwardStreamedBody(ctx context.Context, body io.Reader, maxBytes int64, readTimeout time.Duration, setReadDeadline func(time.Time) error) *forwardStreamedBody {
	return &forwardStreamedBody{
		ctx:             ctx,
		body:            body,
		maxBytes:        maxBytes,
		readTimeout:     readTimeout,
		setReadDeadline: setReadDeadline,
	}
}

// Marks that the upstream request began: its head is on the wire and the
// body is about to be read, so the request can't be retried.
func (self *forwardStreamedBody) begin() {
	self.beganForwarding.Store(true)
}

// Whether the upstream request began.
func (self *forwardStreamedBody) began() bool {
	return self.beganForwarding.Load()
}

// Reads the client's body under its timeout and limit. A failure on the
// client's side is kept as the request's refusal; one caused by interrupt
// isn't.
func (self *forwardStreamedBody) Read(p []byte) (int, error) {
	interrupted := func() bool {
		// set the deadline under the lock, so interrupt's past deadline is
		// never overwritten by a later read's
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.interrupted && 0 < self.readTimeout && self.setReadDeadline != nil {
			self.setReadDeadline(time.Now().Add(self.readTimeout))
		}
		return self.interrupted
	}()
	if interrupted {
		return 0, errBodyInterrupted
	}
	n, err := self.body.Read(p)
	self.readBytes += int64(n)
	if 0 < self.maxBytes && self.maxBytes < self.readBytes {
		self.fail(&bodyRefusal{status: http.StatusRequestEntityTooLarge, reason: bodyRefusalTooLarge})
		return n, io.ErrUnexpectedEOF
	}
	if err == io.EOF {
		if 0 < self.readTimeout && self.setReadDeadline != nil {
			// nothing reads the client again until the next request
			self.setReadDeadline(time.Time{})
		}
		return n, io.EOF
	}
	if err != nil {
		self.fail(bodyReadRefusal(self.ctx, err))
	}
	return n, err
}

// Keeps the first client-side failure, unless the read was interrupted.
func (self *forwardStreamedBody) fail(refusal *bodyRefusal) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.interrupted || self.refusal != nil {
		return
	}
	self.refusal = refusal
}

// Whether the body failed on the client's side.
func (self *forwardStreamedBody) failed() bool {
	return self.failure() != nil
}

// The client-side failure, or nil.
func (self *forwardStreamedBody) failure() *bodyRefusal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.refusal
}

// Stops the body: a pending read ends at once, through a read deadline in
// the past, and later reads fail. The deadline stays, so the server closes
// the client connection rather than wait to discard what remains. Without
// read deadlines a pending read ends only with the client's next bytes or
// its connection.
func (self *forwardStreamedBody) interrupt() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.interrupted {
		return
	}
	self.interrupted = true
	if self.setReadDeadline != nil {
		self.setReadDeadline(time.Unix(1, 0))
	}
}
