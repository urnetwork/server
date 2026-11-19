package nginx

// Streamed request bodies (owner: s-forward): on a streamed route the
// client's body is read once, straight into the upstream request, as nginx's
// `proxy_request_buffering off` reads it. Nothing is held, so the memory
// budget and the spill caps don't apply, but the body limit and
// client_body_timeout between reads do. A body that began going upstream
// can't be replayed, so it ends the request's retries (D7). A read that
// fails on the client's side ends the request, a response being relayed
// included, as nginx ends it.

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
	// the length the client declared, or -1 when unknown
	declaredLength int64
	// for a body of unknown length, BodySettings.MaxBodyBytes; non-positive
	// means no limit. A declared length is checked before forwarding.
	maxBytes        int64
	readTimeout     time.Duration
	setReadDeadline func(time.Time) error
	readBytes       int64
	beganForwarding atomic.Bool
	// set once the client's body was read whole, its declared length or its
	// end, before those last bytes go upstream
	whole atomic.Bool
	// set by interrupt before its past deadline, and checked by a read after
	// its own deadline, so neither deadline can hide the other
	interrupted atomic.Bool

	stateLock sync.Mutex
	refusal   *bodyRefusal
}

// A streamed body over the client's, of declaredLength or -1 when unknown,
// read under its timeout through setReadDeadline, and under maxBytes when
// its length is unknown.
func newForwardStreamedBody(ctx context.Context, body io.Reader, declaredLength int64, maxBytes int64, readTimeout time.Duration, setReadDeadline func(time.Time) error) *forwardStreamedBody {
	streamedBody := &forwardStreamedBody{
		ctx:             ctx,
		body:            body,
		declaredLength:  declaredLength,
		maxBytes:        maxBytes,
		readTimeout:     readTimeout,
		setReadDeadline: setReadDeadline,
	}
	// an empty body is whole before any read
	streamedBody.whole.Store(declaredLength == 0)
	return streamedBody
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

// Whether the client's body was read whole, so all that is left of the
// upstream request is its framing and the last flush.
func (self *forwardStreamedBody) isWhole() bool {
	return self.whole.Load()
}

// Reads the client's body under its timeout and limit. A failure on the
// client's side is kept as the request's refusal; one caused by interrupt
// isn't.
func (self *forwardStreamedBody) Read(p []byte) (int, error) {
	if self.interrupted.Load() {
		return 0, errBodyInterrupted
	}
	if 0 < self.readTimeout && self.setReadDeadline != nil {
		self.setReadDeadline(time.Now().Add(self.readTimeout))
		// an interrupt whose past deadline this one may have replaced has
		// set its flag already: restore its deadline and stop
		if self.interrupted.Load() {
			self.setReadDeadline(time.Unix(1, 0))
			return 0, errBodyInterrupted
		}
	}
	n, err := self.body.Read(p)
	self.readBytes += int64(n)
	if 0 < self.maxBytes && self.maxBytes < self.readBytes {
		self.fail(&bodyRefusal{status: http.StatusRequestEntityTooLarge, reason: bodyRefusalTooLarge})
		return n, io.ErrUnexpectedEOF
	}
	// a declared length counts as the end: the request's writer reads no
	// further than it, so it may never see the end itself
	if err == io.EOF || (err == nil && 0 <= self.declaredLength && self.declaredLength <= self.readBytes) {
		self.whole.Store(true)
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
	if self.interrupted.Load() {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.refusal == nil {
		self.refusal = refusal
	}
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
	if self.interrupted.Swap(true) {
		return
	}
	if self.setReadDeadline != nil {
		self.setReadDeadline(time.Unix(1, 0))
	}
}
